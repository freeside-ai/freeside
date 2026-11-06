package gitrun

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"

	"github.com/freeside-ai/freeside/daemon/internal/procbound"
)

// ObjectType is the type git reports for an object in a batch header.
type ObjectType string

const (
	ObjectBlob   ObjectType = "blob"
	ObjectTree   ObjectType = "tree"
	ObjectCommit ObjectType = "commit"
	ObjectTag    ObjectType = "tag"
)

// AllObjectTypes lists every valid ObjectType; it is the single place a new
// type is registered.
var AllObjectTypes = []ObjectType{ObjectBlob, ObjectTree, ObjectCommit, ObjectTag}

func (t ObjectType) valid() bool {
	switch t {
	case ObjectBlob, ObjectTree, ObjectCommit, ObjectTag:
		return true
	default:
		return false
	}
}

// ObjectInfo is what git reported for one object: the ID it echoed, which
// always equals the requested ID, and the object's type and size.
type ObjectInfo struct {
	ID   string
	Type ObjectType
	Size int64
}

// ErrObjectMissing marks a batch response saying the requested object is not
// in the pinned repository. The reader fails on it like any other error.
var ErrObjectMissing = errors.New("git object is missing")

var (
	// errBatchResponse marks a response that breaks the batch protocol.
	errBatchResponse = errors.New("malformed cat-file batch response")
	// errCallCancelled marks a call whose context, or the session, ended
	// after git answered: the process is being killed, so the reader cannot
	// go on.
	errCallCancelled = errors.New("call cancelled")
)

const (
	batchCheckMode = "--batch-check"
	batchMode      = "--batch"
	// batchStderrBytes bounds the stderr kept for a failure's detail.
	batchStderrBytes = 4096
)

// ObjectReader reads objects from the runner's pinned repository through
// two long-lived processes, `cat-file --batch-check` for Info and
// `cat-file --batch` for Open, each started on first use. Object bytes are
// untrusted: they come from agent workspaces and imported candidates and
// feed Tier A gates, so the reader checks every response against the
// request and trusts nothing it cannot check.
//
// The reader accepts only full lowercase hex object IDs of the pinned
// object format, never a revision or `<rev>:<path>` expression, so a request
// names exactly one object and git's echo of it is checkable.
//
// Any error is final: the reader stops both processes, and every later call
// returns the first error, so a desynchronized stream can never pair a
// response with the wrong object. That covers a malformed header, a missing
// object, an echo of another ID, a short body, a bad delimiter, the death of
// git, an ID refused before any write, and cancellation of the session
// context or of one call's context. Errors are *GitError values carrying the
// runner's Options.Class.
//
// An ObjectReader serves one goroutine at a time. Close stops both
// processes; no child outlives Close or an error.
type ObjectReader struct {
	runner  *Runner
	session context.Context
	idLen   int
	check   *batchProcess
	content *batchProcess
	// body is the last Open's content until the caller has read it and the
	// reader has checked the delimiter that follows it.
	body *objectBody
	err  error
}

// Objects returns a reader over the runner's pinned checkout. Its processes
// run under session, so cancelling session stops them; each call takes its
// own context as well. The runner must already be pinned: an unpinned
// runner's reader fails every call, because git would otherwise discover a
// repository from the runner's scratch directory.
func (r *Runner) Objects(session context.Context) *ObjectReader {
	var idLen int
	switch r.format {
	case "sha1":
		idLen = 40
	case "sha256":
		idLen = 64
	}
	return &ObjectReader{runner: r, session: session, idLen: idLen}
}

// Info reports an object's type and size without git streaming its content.
func (o *ObjectReader) Info(ctx context.Context, id string) (ObjectInfo, error) {
	p, err := o.begin(ctx, id, &o.check, batchCheckMode)
	if err != nil {
		return ObjectInfo{}, err
	}
	release := watch(ctx, p)
	info, err := o.exchange(p, id)
	if cancelled := !release() || o.session.Err() != nil; cancelled && err == nil {
		err = errCallCancelled
	}
	if err != nil {
		return ObjectInfo{}, o.fail(ctx, p, err)
	}
	return info, nil
}

// Open reports an object's type and size and returns a reader over exactly
// that many content bytes. ctx governs the whole read, header and content.
// The caller must read the content to its end before the next call; a call
// made with content left unread fails the reader. Reading to io.EOF also
// checks the delimiter git writes after the content, and a short or
// misdelimited body fails the reader.
func (o *ObjectReader) Open(ctx context.Context, id string) (ObjectInfo, io.Reader, error) {
	p, err := o.begin(ctx, id, &o.content, batchMode)
	if err != nil {
		return ObjectInfo{}, nil, err
	}
	release := watch(ctx, p)
	info, err := o.exchange(p, id)
	if err != nil {
		release()
		return ObjectInfo{}, nil, o.fail(ctx, p, err)
	}
	o.body = &objectBody{reader: o, process: p, ctx: ctx, release: release, id: id, remaining: info.Size}
	return info, o.body, nil
}

// Close stops both processes. It does not wait for git to exit on its own:
// every response the reader returned was already checked, so git's exit
// status adds nothing. Calls after Close fail.
func (o *ObjectReader) Close() error {
	o.stop()
	if o.err == nil {
		o.err = &GitError{Args: []string{"cat-file"}, Class: o.runner.class, Err: errors.New("object reader closed")}
	}
	return nil
}

// begin runs every check that precedes a request, then starts the mode's
// process when this is its first use.
func (o *ObjectReader) begin(ctx context.Context, id string, slot **batchProcess, mode string) (*batchProcess, error) {
	if o.err != nil {
		return nil, o.err
	}
	if err := o.finishBody(); err != nil {
		return nil, err
	}
	if o.idLen == 0 {
		return nil, o.fail(ctx, nil, fmt.Errorf("runner is not pinned to a sha1 or sha256 checkout (format %q)", o.runner.format))
	}
	if !o.validID(id) {
		return nil, o.fail(ctx, nil, fmt.Errorf("object ID %q is not a full %d-character lowercase hex name", id, o.idLen))
	}
	if err := ctx.Err(); err != nil {
		return nil, o.fail(ctx, nil, err)
	}
	if *slot == nil {
		p, err := o.start(mode)
		if err != nil {
			return nil, o.fail(ctx, nil, err)
		}
		*slot = p
	}
	return *slot, nil
}

func (o *ObjectReader) validID(id string) bool {
	if len(id) != o.idLen {
		return false
	}
	for i := range len(id) {
		if c := id[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// exchange writes one request and reads its header. The request is written
// only after the previous response was read in full, so neither pipe can
// fill while the other waits.
func (o *ObjectReader) exchange(p *batchProcess, id string) (ObjectInfo, error) {
	if _, err := io.WriteString(p.stdin, id+"\n"); err != nil {
		return ObjectInfo{}, fmt.Errorf("write request: %w", err)
	}
	// ReadSlice bounds the header by the bufio buffer, so a runaway line
	// fails instead of growing without limit.
	line, err := p.stdout.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return ObjectInfo{}, fmt.Errorf("read header for %s: %w", id, err)
	}
	header := line[:len(line)-1]
	if string(header) == id+" missing" {
		return ObjectInfo{}, fmt.Errorf("%s: %w", id, ErrObjectMissing)
	}
	fields := bytes.Split(header, []byte{' '})
	if len(fields) != 3 || string(fields[0]) != id {
		return ObjectInfo{}, fmt.Errorf("header %q for %s: %w", header, id, errBatchResponse)
	}
	info := ObjectInfo{ID: id, Type: ObjectType(fields[1])}
	size, ok := parseSize(fields[2])
	if !info.Type.valid() || !ok {
		return ObjectInfo{}, fmt.Errorf("header %q for %s: %w", header, id, errBatchResponse)
	}
	info.Size = size
	return info, nil
}

// parseSize accepts only plain decimal digits; strconv alone would accept a
// sign.
func parseSize(field []byte) (int64, bool) {
	if len(field) == 0 {
		return 0, false
	}
	for _, c := range field {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	size, err := strconv.ParseInt(string(field), 10, 64)
	return size, err == nil
}

// finishBody checks the delimiter after the last Open's content. Content
// left unread fails the reader rather than being skipped: the caller was
// told the size before any content and chose to open it.
func (o *ObjectReader) finishBody() error {
	b := o.body
	if b == nil {
		return nil
	}
	if b.remaining != 0 {
		return o.fail(b.ctx, b.process, fmt.Errorf("content of %s left with %d bytes unread", b.id, b.remaining))
	}
	delimiter, err := b.process.stdout.ReadByte()
	cancelled := !b.release() || o.session.Err() != nil
	o.body = nil
	switch {
	case err != nil:
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return o.fail(b.ctx, b.process, fmt.Errorf("read delimiter after %s: %w", b.id, err))
	case delimiter != '\n':
		return o.fail(b.ctx, b.process, fmt.Errorf("delimiter %q after %s: %w", delimiter, b.id, errBatchResponse))
	case cancelled:
		return o.fail(b.ctx, b.process, errCallCancelled)
	}
	b.finished = true
	return nil
}

// fail records the reader's final error and stops both processes. A
// cancelled call or session is named in the error, since a killed process
// otherwise reports only a short read.
func (o *ObjectReader) fail(ctx context.Context, p *batchProcess, cause error) error {
	if o.err != nil {
		return o.err
	}
	for _, c := range []context.Context{ctx, o.session} {
		if c != nil && c.Err() != nil && !errors.Is(cause, c.Err()) {
			cause = fmt.Errorf("%w: %w", cause, c.Err())
		}
	}
	o.stop()
	gitErr := &GitError{Args: []string{"cat-file"}, Class: o.runner.class, Err: cause}
	if p != nil {
		gitErr.Args = p.args
		// The process has been waited on, so its stderr copier is done.
		gitErr.Stderr = string(p.stderr.Bytes())
	}
	o.err = gitErr
	return o.err
}

// stop ends the pending content watch, then kills and waits on each
// started process.
func (o *ObjectReader) stop() {
	if o.body != nil {
		o.body.release()
		o.body = nil
	}
	for _, p := range []*batchProcess{o.check, o.content} {
		if p != nil {
			p.stop()
		}
	}
}

// watch kills p's process group if ctx ends before the returned release
// runs. release reports whether the call finished uncancelled; when it was
// cancelled, release first waits for the kill so it never lands after the
// process has been reaped.
func watch(ctx context.Context, p *batchProcess) func() bool {
	killed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(killed)
		_ = p.cmd.Cancel()
	})
	return func() bool {
		if stop() {
			return true
		}
		<-killed
		return false
	}
}

type batchProcess struct {
	args    []string
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  *bufio.Reader
	stderr  *procbound.TailBuffer
	stopped bool
}

// start launches one batch process under the session context and the
// runner's pinned argv and environment, exactly as RunTo would.
func (o *ObjectReader) start(mode string) (*batchProcess, error) {
	if err := o.session.Err(); err != nil {
		return nil, err
	}
	args := []string{"cat-file", mode}
	r := o.runner
	cmd := exec.CommandContext(o.session, r.gitPath, r.argv(args)...) //nolint:gosec // G204: fixed plumbing argv; validated object IDs travel over stdin
	cmd.Dir = r.dir
	cmd.Env = r.env
	procbound.Bind(cmd, procbound.DefaultWaitDelay)
	stderr := procbound.NewTailBuffer(batchStderrBytes)
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &batchProcess{args: args, cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout), stderr: stderr}, nil
}

// stop closes stdin, kills the process group, waits, and reaps whatever
// remains, in the order procbound requires.
func (p *batchProcess) stop() {
	if p.stopped {
		return
	}
	p.stopped = true
	_ = p.stdin.Close()
	_ = p.cmd.Cancel()
	_ = p.cmd.Wait()
	procbound.Reap(p.cmd)
}

// objectBody streams one object's content and fails the reader on a short
// read. It never reads past the object, so git's stream stays aligned.
type objectBody struct {
	reader    *ObjectReader
	process   *batchProcess
	ctx       context.Context
	release   func() bool
	id        string
	remaining int64
	finished  bool
}

func (b *objectBody) Read(buf []byte) (int, error) {
	if b.reader.body != b {
		if b.finished {
			return 0, io.EOF
		}
		return 0, b.reader.err
	}
	if b.remaining == 0 {
		if err := b.reader.finishBody(); err != nil {
			return 0, err
		}
		return 0, io.EOF
	}
	if int64(len(buf)) > b.remaining {
		buf = buf[:b.remaining]
	}
	n, err := b.process.stdout.Read(buf)
	b.remaining -= int64(n)
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return n, b.reader.fail(b.ctx, b.process, fmt.Errorf("read content of %s: %w", b.id, err))
	}
	return n, nil
}
