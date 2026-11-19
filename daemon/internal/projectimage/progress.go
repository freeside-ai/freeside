package projectimage

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// maxStepLogBytes caps one step's log file. Past it the file gets a
	// closing line and the step's output goes only to the tail and the
	// failing-test excerpt.
	maxStepLogBytes = 256 << 20
	// timingFileName is the per-build timing record a later build reads for
	// its heartbeat estimates.
	timingFileName = "steps.json"
	// maxTimingFileBytes bounds the timing record a build reads back.
	maxTimingFileBytes = 1 << 20
	// buildDirStamp names a build's log directory by its start time, so the
	// newest earlier build sorts last. Fixed width keeps the sort exact.
	buildDirStamp = "20060102T150405.000000000Z"
)

var stepSlugRun = regexp.MustCompile(`[^a-z0-9]+`)

// buildProgress narrates one build: a line in the builder's log as each step
// starts, an optional heartbeat while it runs, and, when full logs are on, a
// file of each command step's whole output and a timing record of every step.
// Nothing it does can change a build's verdict: a log write that fails is
// recorded and reported, never returned to the command writing the output.
type buildProgress struct {
	log       io.Writer
	heartbeat time.Duration
	// dir is this build's log directory; empty when full logs are off.
	dir string
	// previous maps each step of the newest earlier recorded build of the
	// same image to its timing; nil when no earlier build is on record.
	previous map[string]stepTiming

	mu      sync.Mutex // guards writes to log, steps, and current
	steps   []stepTiming
	current *buildStep
}

// stepTiming is one step's entry in a build's timing record.
type stepTiming struct {
	Name       string `json:"name"`
	DurationMS int64  `json:"duration_ms"`
	Outcome    string `json:"outcome"`
}

type timingRecord struct {
	Steps []stepTiming `json:"steps"`
}

// startProgress creates the build's log directory under logDir, when set,
// and reads the newest earlier timing record of the same image. Failing to
// create the directory fails the build before any step runs; a missing or
// unreadable earlier record only drops the heartbeat's estimate. imageName
// has passed imageNamePattern, so it is a safe path element.
func startProgress(log io.Writer, logDir string, heartbeat time.Duration, imageName string) (*buildProgress, error) {
	progress := &buildProgress{log: log, heartbeat: heartbeat}
	if logDir == "" {
		return progress, nil
	}
	imageDir := filepath.Join(logDir, imageName)
	if err := os.MkdirAll(imageDir, 0o700); err != nil {
		return nil, fmt.Errorf("create project-image log directory: %w", err)
	}
	token, err := randomToken()
	if err != nil {
		return nil, fmt.Errorf("name project-image log directory: %w", err)
	}
	name := time.Now().UTC().Format(buildDirStamp) + "-" + token[:8]
	progress.dir = filepath.Join(imageDir, name)
	if err := os.Mkdir(progress.dir, 0o700); err != nil {
		return nil, fmt.Errorf("create project-image build log directory: %w", err)
	}
	previous, err := readPreviousTiming(imageDir, name)
	if err != nil {
		progress.logf("earlier build timing unreadable, so no estimates: %v", err)
	}
	progress.previous = previous
	progress.logf("logs for this build: %s", progress.dir)
	return progress, nil
}

// readPreviousTiming returns the steps of the newest build directory before
// current that holds a timing record, or nil when none does.
func readPreviousTiming(imageDir, current string) (map[string]stepTiming, error) {
	entries, err := os.ReadDir(imageDir)
	if err != nil {
		return nil, err
	}
	for _, entry := range slices.Backward(entries) {
		if !entry.IsDir() || entry.Name() >= current {
			continue
		}
		path := filepath.Join(imageDir, entry.Name(), timingFileName)
		file, err := os.Open(path) //nolint:gosec // G304: a fixed name under the builder's own log directory
		if errors.Is(err, os.ErrNotExist) {
			continue // a build that ended without a record, such as a killed one
		}
		if err != nil {
			return nil, err
		}
		body, readErr := io.ReadAll(io.LimitReader(file, maxTimingFileBytes+1))
		closeErr := file.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		if len(body) > maxTimingFileBytes {
			return nil, fmt.Errorf("%s exceeds %d bytes", path, maxTimingFileBytes)
		}
		var record timingRecord
		if err := json.Unmarshal(body, &record); err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}
		previous := map[string]stepTiming{}
		for _, step := range record.Steps {
			previous[step.Name] = step
		}
		return previous, nil
	}
	return nil, nil
}

// begin starts a step with no output of its own.
func (p *buildProgress) begin(name, detail string) *buildStep {
	return p.start(name, detail, false)
}

// beginCommand starts a step that runs one command. Its writer, given to the
// command as its Output, keeps the failing Go tests and, when full logs are
// on, the whole output in the step's log file.
func (p *buildProgress) beginCommand(name, detail string) *buildStep {
	return p.start(name, detail, true)
}

func (p *buildProgress) start(name, detail string, command bool) *buildStep {
	step := &buildStep{progress: p, name: name, started: time.Now()}
	line := name + " started"
	if detail != "" {
		line += ": " + detail
	}
	if command {
		step.output = &stepWriter{}
		if p.dir != "" {
			step.output.file = openStepLog(p.dir, len(p.steps)+1, name)
			if step.output.file.path != "" {
				line += "; output in " + step.output.file.path
			} else {
				line += "; output not kept: " + step.output.file.err.Error()
			}
		}
	}
	p.mu.Lock()
	p.current = step
	p.mu.Unlock()
	p.logf("%s", line)
	if p.heartbeat > 0 && p.log != nil {
		step.stop = make(chan struct{})
		step.done = make(chan struct{})
		go step.beat(p.heartbeat)
	}
	return step
}

// finish ends a step left open and writes the build's timing record. It runs
// once, after every step, whether the build passed or failed.
func (p *buildProgress) finish() {
	p.mu.Lock()
	current := p.current
	p.mu.Unlock()
	if current != nil {
		current.end(errors.New("build ended"))
	}
	if p.dir == "" {
		return
	}
	p.mu.Lock()
	body, err := json.MarshalIndent(timingRecord{Steps: p.steps}, "", "  ")
	p.mu.Unlock()
	if err == nil {
		err = writeNewFile(filepath.Join(p.dir, timingFileName), append(body, '\n'))
	}
	if err != nil {
		p.logf("build timing not recorded: %v", err)
	}
}

func (p *buildProgress) logf(format string, args ...any) {
	if p.log == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, _ = fmt.Fprintf(p.log, "project-image: "+format+"\n", args...)
}

// estimate describes how long the step took in the last recorded build.
func (p *buildProgress) estimate(name string) string {
	if p.previous == nil {
		return "no earlier build on record"
	}
	step, ok := p.previous[name]
	if !ok {
		return "the last recorded build has no such step"
	}
	took := (time.Duration(step.DurationMS) * time.Millisecond).Round(time.Second)
	if step.Outcome != outcomePassed {
		return fmt.Sprintf("it took %s in the last recorded build (%s)", took, step.Outcome)
	}
	return fmt.Sprintf("it took %s in the last recorded build", took)
}

const (
	outcomePassed = "passed"
	outcomeFailed = "failed"
)

// buildStep is one running step. Every begun step is ended exactly once.
type buildStep struct {
	progress *buildProgress
	name     string
	started  time.Time
	// output is the command's writer; nil for a step with no command.
	output *stepWriter
	stop   chan struct{}
	done   chan struct{}
	ended  bool
}

func (s *buildStep) beat(interval time.Duration) {
	defer close(s.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			elapsed := time.Since(s.started).Round(time.Second)
			s.progress.logf("still %s: %s elapsed; %s", s.name, elapsed, s.progress.estimate(s.name))
		}
	}
}

// end stops the step's heartbeat, closes its log file, and records its
// timing. A nil err records it as passed. A step's outcome is what the step
// observed; it never feeds back into the build's verdict.
func (s *buildStep) end(err error) {
	if s.ended {
		return
	}
	s.ended = true
	if s.stop != nil {
		close(s.stop)
		<-s.done
	}
	if s.output != nil {
		s.output.close()
	}
	outcome := outcomePassed
	if err != nil {
		outcome = outcomeFailed
	}
	p := s.progress
	p.mu.Lock()
	defer p.mu.Unlock()
	p.steps = append(p.steps, stepTiming{
		Name: s.name, DurationMS: time.Since(s.started).Milliseconds(), Outcome: outcome,
	})
	if p.current == s {
		p.current = nil
	}
}

// writer is the io.Writer a command step hands its command.
func (s *buildStep) writer() io.Writer {
	if s.output == nil {
		return nil
	}
	return s.output
}

// logNote names where the step's whole output is, for an error from the
// step; "" when full logs are off or the step ran no command.
func (s *buildStep) logNote() string {
	if s.output == nil || s.output.file == nil {
		return ""
	}
	file := s.output.file
	switch {
	case file.path == "":
		return "full output not kept: " + file.err.Error()
	case file.err != nil:
		return fmt.Sprintf("full output incomplete: %s: %v", file.path, file.err)
	case file.capped:
		return fmt.Sprintf("full output, cut at %d bytes: %s", file.limit, file.path)
	}
	return "full output: " + file.path
}

// noteSuffix is logNote as a parenthesized suffix for an error's action.
func (s *buildStep) noteSuffix() string {
	if note := s.logNote(); note != "" {
		return " (" + note + ")"
	}
	return ""
}

// proofOutput describes a failed proof command's output: the failing Go
// tests from its whole stream, when there are any, then its bounded tail,
// then where its whole output is. The tail alone often starts past the
// failing test (#3442).
func (s *buildStep) proofOutput(output []byte) string {
	described := boundedOutput(output)
	if summary := s.output.failingTests(); summary != "" {
		described = "failing Go tests:\n" + summary + "\noutput tail:\n" + described
	}
	if note := s.logNote(); note != "" {
		described += "\n" + note
	}
	return described
}

// stepWriter receives a command's whole output. It always reports full
// consumption and no error (see commandSpec.Output).
type stepWriter struct {
	mu    sync.Mutex
	tests failingTests
	file  *stepLog
}

func (w *stepWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, _ = w.tests.Write(p)
	if w.file != nil {
		w.file.write(p)
	}
	return len(p), nil
}

// failingTests summarizes the failing Go tests seen so far. It takes the
// lock because a runtime past its wait delay may still be writing.
func (w *stepWriter) failingTests() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.tests.summary()
}

func (w *stepWriter) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		w.file.close()
	}
}

// stepLog is one step's log file. It records its first failure instead of
// returning it, and stops writing after that failure or past its limit.
type stepLog struct {
	file    *os.File
	path    string
	limit   int64
	written int64
	capped  bool
	err     error
}

// openStepLog creates the step's file; on failure the returned log has no
// path and holds the error, so the step runs without a full log.
func openStepLog(dir string, index int, name string) *stepLog {
	path := filepath.Join(dir, fmt.Sprintf("%02d-%s.log", index, stepSlug(name)))
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: the builder's own log directory plus a slug of a fixed step name
	if err != nil {
		return &stepLog{err: err}
	}
	return &stepLog{file: file, path: path, limit: maxStepLogBytes}
}

func (l *stepLog) write(p []byte) {
	if l.file == nil || l.err != nil || l.capped {
		return
	}
	room := l.limit - l.written
	chunk := p
	if int64(len(chunk)) > room {
		chunk = chunk[:room]
		l.capped = true
	}
	n, err := l.file.Write(chunk)
	l.written += int64(n)
	if err != nil {
		l.err = err
		return
	}
	if l.capped {
		if _, err := fmt.Fprintf(l.file,
			"\n[freeside: log stopped at %d bytes; the step's output went on]\n", l.limit); err != nil {
			l.err = err
		}
	}
}

func (l *stepLog) close() {
	if l.file == nil {
		return
	}
	if err := l.file.Close(); err != nil && l.err == nil {
		l.err = err
	}
	l.file = nil
}

func stepSlug(name string) string {
	return strings.Trim(stepSlugRun.ReplaceAllString(strings.ToLower(name), "-"), "-")
}

func writeNewFile(path string, body []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: a fixed name under the builder's own log directory
	if err != nil {
		return err
	}
	_, writeErr := file.Write(body)
	return errors.Join(writeErr, file.Close())
}
