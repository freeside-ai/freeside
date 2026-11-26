package cordon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// A writer launch's inbox is ctl/messages/, where the daemon leaves an
// operator's message for the running agent (#3632). Like every ctl/ path,
// the launch can neither read nor write it, so a message says only what the
// daemon wrote. Each message is one file named by its ordinal, and its name
// is its state:
//
//   - <ordinal>.json is waiting. The daemon creates it, never replacing
//     one.
//   - <ordinal>.taken is taken: the supervisor renamed it, and then writes
//     it to the agent's input.
//   - <ordinal>.refused is withdrawn: the daemon renamed it once the run
//     ended, its input closed, or nobody took it in time.
//
// Both renames have the one waiting file as their source, so exactly one of
// them succeeds, and a message is written to the agent at most once.
//
// The supervisor takes messages while a turn runs, which the CLI folds into
// that turn, and when a turn ends. A turn that ends with none waiting closes
// the input: the supervisor writes ctl/input-closed.json and takes nothing
// after it, so a message the daemon leaves then is withdrawn. A launch from
// before the inbox has no ctl/messages/, which reads as not streamed.

// Why a message isn't delivered. SendMessage returns one of these, wrapped,
// or ErrNoRun for a run that has no directory.
var (
	// ErrRunEnded means the run has an end record or its cancellation was
	// requested.
	ErrRunEnded = errors.New("cordon: the run has ended")
	// ErrInputClosed means the agent's input is closed, so nothing more
	// reaches it.
	ErrInputClosed = errors.New("cordon: the run's input is closed")
	// ErrNotStreamed means the run has no inbox: it launched before the
	// inbox existed.
	ErrNotStreamed = errors.New("cordon: the run's input is not streamed")
	// ErrNotStarted means the run's directory exists but its launch hasn't
	// reached its supervisor yet: it is being probed or seeded.
	ErrNotStarted = errors.New("cordon: the run has not started its writer")
	// ErrNotTaken means the supervisor didn't take the message within
	// messageTakeTimeout, so the daemon withdrew it.
	ErrNotTaken = errors.New("cordon: the run did not take the message in time")
)

// MaxMessageBytes bounds a message's text. A message file is a record, which
// readRecord bounds at maxRecordBytes, and JSON can write one byte as six
// (\u003c for <), so the text is bounded at well under a sixth of that.
const MaxMessageBytes = 8 << 10

// messageTakeTimeout is how long SendMessage waits for the supervisor to
// take a message. The supervisor looks every inputPollInterval while a turn
// runs, so a message nobody takes by then is one nobody will.
const messageTakeTimeout = 10 * time.Second

const (
	messageVersion     = "freeside.cordon.message/v1"
	inputClosedVersion = "freeside.cordon.input-closed/v1"
)

// inboxMessage is one message file.
type inboxMessage struct {
	Version   string `json:"version"`
	RunID     string `json:"run_id"`
	Ordinal   int64  `json:"ordinal"`
	CommandID string `json:"command_id"`
	Text      string `json:"text"`
}

// inputClosedMarker is ctl/input-closed.json.
type inputClosedMarker struct {
	Version string `json:"version"`
	RunID   string `json:"run_id"`
}

// messageFiles are the three names one message can have.
type messageFiles struct{ waiting, taken, refused string }

const (
	waitingSuffix = ".json"
	takenSuffix   = ".taken"
	refusedSuffix = ".refused"
)

func messageName(ordinal int64) string { return fmt.Sprintf("%020d", ordinal) }

func filesFor(l layout, ordinal int64) messageFiles {
	base := filepath.Join(l.messages, messageName(ordinal))
	return messageFiles{waiting: base + waitingSuffix, taken: base + takenSuffix, refused: base + refusedSuffix}
}

// readMessage reads the message file at path, refusing one that isn't
// this run's ordinal.
func readMessage(path, runID string, ordinal int64) (inboxMessage, error) {
	var m inboxMessage
	if err := readRecord(path, &m); err != nil {
		return inboxMessage{}, err
	}
	if m.Version != messageVersion || m.RunID != runID || m.Ordinal != ordinal {
		return inboxMessage{}, fmt.Errorf("cordon: %s is not message %d of %s", filepath.Base(path), ordinal, runID)
	}
	return m, nil
}

// SendMessage offers text to runID's agent as message ordinal, and returns
// nil once the supervisor has taken it, which means it is written, or being
// written, to the agent's input. The caller owns the ordinal and the
// command ID: an ordinal offered before settles from its files, so a
// daemon that restarted mid-send can offer it again, and it is an error for
// another command ID to reuse it. Canceling ctx leaves a waiting message
// waiting, for the next offer.
func (b *Backend) SendMessage(ctx context.Context, runID string, ordinal int64, commandID, text string) error {
	if ordinal < 1 || commandID == "" {
		return fmt.Errorf("cordon: message %d needs a positive ordinal and its command id", ordinal)
	}
	if text == "" || len(text) > MaxMessageBytes || !utf8.ValidString(text) {
		return fmt.Errorf("cordon: message %d must be UTF-8 text of 1 to %d bytes", ordinal, MaxMessageBytes)
	}
	l, err := b.existingRun(ctx, runID)
	if err != nil {
		return err
	}
	f := filesFor(l, ordinal)
	state, err := offeredState(runID, ordinal, commandID, f)
	switch {
	case err != nil:
		return err
	case state == takenSuffix:
		return nil
	case state == refusedSuffix:
		return refusedReason(l, runID)
	case state == waitingSuffix:
		return awaitTake(ctx, l, runID, f)
	}
	if err := openReason(l, runID); err != nil {
		return err
	}
	err = writeRecord(f.waiting, inboxMessage{Version: messageVersion, RunID: runID, Ordinal: ordinal, CommandID: commandID, Text: text})
	if err != nil {
		return err
	}
	return awaitTake(ctx, l, runID, f)
}

// offeredState returns the suffix of the file ordinal already has, or ""
// when it has none. A file that holds another command's message is an
// error.
//
// It reads the waiting file first. A message only ever moves out of
// waiting, so a take or withdraw that lands between two reads leaves the
// later read to find it; read the other way round, a take between them
// would read as no file at all, and the caller would write the message
// again for the supervisor to take twice.
func offeredState(runID string, ordinal int64, commandID string, f messageFiles) (string, error) {
	for _, file := range []struct{ path, suffix string }{
		{f.waiting, waitingSuffix}, {f.taken, takenSuffix}, {f.refused, refusedSuffix},
	} {
		m, err := readMessage(file.path, runID, ordinal)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if m.CommandID != commandID {
			return "", fmt.Errorf("cordon: message %d of %s belongs to another command", ordinal, runID)
		}
		return file.suffix, nil
	}
	return "", nil
}

// openReason returns why nothing more reaches runID's agent, or nil while
// a message still can: ErrRunEnded, ErrNotStarted or ErrNotStreamed, or
// ErrInputClosed, in that order. Launch creates the inbox before the
// supervisor's request, so a run with neither hasn't started, and one with
// a request and no inbox launched before the inbox existed.
func openReason(l layout, runID string) error {
	if err := endedReason(l, runID); err != nil {
		return err
	}
	if _, err := os.Lstat(l.messages); errors.Is(err, os.ErrNotExist) {
		if _, err := os.Lstat(l.request); errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrNotStarted, runID)
		}
		return fmt.Errorf("%w: %s", ErrNotStreamed, runID)
	} else if err != nil {
		return fmt.Errorf("cordon: inspect the inbox: %w", err)
	}
	return closedReason(l, runID)
}

// endedReason returns ErrRunEnded once runID has an end record or a
// requested cancellation.
func endedReason(l layout, runID string) error {
	if _, err := os.Lstat(l.ended); err == nil {
		return fmt.Errorf("%w: %s", ErrRunEnded, runID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("cordon: inspect the end record: %w", err)
	}
	canceled, err := cancellationRequested(l, runID)
	if err != nil {
		return err
	}
	if canceled {
		return fmt.Errorf("%w: %s", ErrRunEnded, runID)
	}
	return nil
}

// closedReason returns ErrInputClosed once runID's supervisor has written
// its marker.
func closedReason(l layout, runID string) error {
	var m inputClosedMarker
	err := readRecord(l.inputClosed, &m)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if m.Version != inputClosedVersion || m.RunID != runID {
		return fmt.Errorf("cordon: the input-closed marker is not %s's", runID)
	}
	return fmt.Errorf("%w: %s", ErrInputClosed, runID)
}

// refusedReason says why a withdrawn message was withdrawn, from what the
// run's files say now: the run ended, its input closed, or neither, so
// nobody took it in time.
func refusedReason(l layout, runID string) error {
	if err := endedReason(l, runID); err != nil {
		return err
	}
	if err := closedReason(l, runID); err != nil {
		return err
	}
	return fmt.Errorf("%w: %s", ErrNotTaken, runID)
}

// awaitTake waits for the supervisor to take f's waiting message, and
// withdraws it once the run ends, its input closes, or messageTakeTimeout
// passes.
func awaitTake(ctx context.Context, l layout, runID string, f messageFiles) error {
	deadline := time.NewTimer(messageTakeTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(supervisorPollInterval)
	defer tick.Stop()
	for {
		if _, err := os.Lstat(f.taken); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("cordon: inspect the message: %w", err)
		}
		reason := endedReason(l, runID)
		if reason == nil {
			reason = closedReason(l, runID)
		}
		if reason != nil {
			return withdraw(f, reason)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return withdraw(f, fmt.Errorf("%w: %s", ErrNotTaken, runID))
		case <-tick.C:
		}
	}
}

// withdraw refuses f's waiting message for reason. A waiting file already
// gone means the supervisor took it first.
func withdraw(f messageFiles, reason error) error {
	err := os.Rename(f.waiting, f.refused)
	if err == nil {
		return reason
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("cordon: withdraw the message: %w", err)
	}
	if _, err := os.Lstat(f.taken); err != nil {
		return fmt.Errorf("cordon: the message is neither waiting nor taken: %w", err)
	}
	return nil
}

// takeWaiting takes every waiting message in ordinal order and returns
// each one's text. A message the daemon withdrew first is skipped.
func takeWaiting(l layout, runID string) ([]string, error) {
	entries, err := os.ReadDir(l.messages)
	if err != nil {
		return nil, fmt.Errorf("cordon: read the inbox: %w", err)
	}
	var texts []string
	// ReadDir sorts by name, and an ordinal's name is fixed-width.
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), waitingSuffix)
		if !ok {
			continue
		}
		ordinal, err := strconv.ParseInt(name, 10, 64)
		if err != nil || ordinal < 1 || messageName(ordinal) != name {
			return texts, fmt.Errorf("cordon: the inbox holds %s, which names no ordinal", e.Name())
		}
		f := filesFor(l, ordinal)
		if err := os.Rename(f.waiting, f.taken); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return texts, fmt.Errorf("cordon: take message %d: %w", ordinal, err)
		}
		m, err := readMessage(f.taken, runID, ordinal)
		if err != nil {
			return texts, err
		}
		texts = append(texts, m.Text)
	}
	return texts, nil
}

// closeInput writes runID's input-closed marker.
func closeInput(l layout, runID string) error {
	return writeRecord(l.inputClosed, inputClosedMarker{Version: inputClosedVersion, RunID: runID})
}

// userFrame is one line of the agent's streamed input: text as a user
// message. text must be UTF-8, since json.Marshal would replace an invalid
// byte rather than refuse it.
func userFrame(text string) []byte {
	type message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	body, err := json.Marshal(struct {
		Type    string  `json:"type"`
		Message message `json:"message"`
	}{"user", message{"user", text}})
	if err != nil {
		panic(fmt.Sprintf("marshal a user frame: %v", err))
	}
	return append(body, '\n')
}

// inputPollInterval paces the supervisor's look for waiting messages while
// a turn runs.
const inputPollInterval = 250 * time.Millisecond

// inputFeed is a streamed launch's input, which the supervisor owns: the
// prompt's message first, then each message it takes from the inbox. It
// closes the input when a turn ends with no message waiting, or when the
// agent exits, writing the input-closed marker first. The CLI runs every
// message it read before its input closed (devlog
// 2026-11-26-1700-steer-cordoned-writer.md), so a message taken just before
// a turn ends still runs.
type inputFeed struct {
	l      layout
	runID  string
	prompt []byte
	// reader is the agent's stdin, and w its write end, which only run
	// writes; stop closes it to end a write the exited agent never reads.
	reader *io.PipeReader
	w      *io.PipeWriter
	// turnEnds watches the agent's stdout, and ended carries what it saw.
	turnEnds *turnEnds
	ended    chan struct{}
	exited   chan struct{}
	done     chan error
}

func newInputFeed(l layout, runID string, prompt []byte) *inputFeed {
	r, w := io.Pipe()
	f := &inputFeed{
		l: l, runID: runID, prompt: prompt, reader: r, w: w,
		ended: make(chan struct{}, 1), exited: make(chan struct{}), done: make(chan error, 1),
	}
	f.turnEnds = newTurnEnds(func() {
		select {
		case f.ended <- struct{}{}:
		default:
		}
	})
	return f
}

// start runs the feed once the agent holds its stdin.
func (f *inputFeed) start() { go func() { f.done <- f.run() }() }

// stop ends the feed once the agent has exited and returns why it failed,
// if it did. The agent's exit already decided how the launch ends, so a
// failure here is only reported.
func (f *inputFeed) stop() error {
	close(f.exited)
	_ = f.w.Close()
	return <-f.done
}

func (f *inputFeed) run() error {
	err := f.feed()
	// The marker comes first, so a message the daemon leaves from here on
	// is withdrawn instead of waiting for a take that never comes.
	err = errors.Join(err, closeInput(f.l, f.runID))
	_ = f.w.Close()
	return err
}

func (f *inputFeed) feed() error {
	if !f.write(userFrame(string(f.prompt))) {
		return nil
	}
	tick := time.NewTicker(inputPollInterval)
	defer tick.Stop()
	for {
		select {
		case <-f.exited:
			return nil
		case <-f.ended:
			n, open, err := f.deliver()
			if err != nil || !open || n == 0 {
				return err
			}
		case <-tick.C:
			if _, open, err := f.deliver(); err != nil || !open {
				return err
			}
		}
	}
}

// deliver writes every waiting message to the agent and returns how many
// it took, and whether the input is still open.
func (f *inputFeed) deliver() (int, bool, error) {
	texts, err := takeWaiting(f.l, f.runID)
	for _, text := range texts {
		if !f.write(userFrame(text)) {
			return len(texts), false, err
		}
	}
	return len(texts), true, err
}

// write reports whether frame reached the agent's input; it fails only
// once stop has closed it.
func (f *inputFeed) write(frame []byte) bool {
	_, err := f.w.Write(frame)
	return err == nil
}
