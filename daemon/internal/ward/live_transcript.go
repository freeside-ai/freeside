package ward

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// The live transcript read lets the operator watch a running writer (plan
// §5.7, "Live view of a running writer"; devlog
// 2026-11-20-2253-live-transcript-read.md). Tier A: it runs a process as
// root inside the credential-bearing writer while it runs, and returns
// bytes that reach a client. It re-checks, on every read, that:
//
//   - the run's current Handoff registered the writer, between a successful
//     StartContainer and the end of the writer wait;
//   - a fresh Inspect of that container carries this handoff's ownership
//     evidence (classifyEvidence) and reports it running.
//
// It grants nothing: a read never touches the journal, the writer wait,
// teardown, or the absence proof, and deregistration only deletes a map
// entry and cancels the context reads derive from, so it never waits for a
// read. The bytes it returns are untrusted and unscanned; the stage driver
// scans them before any leave the daemon.

// WriterTranscriptPath is the writer's in-flight transcript, in the
// root-only launch directory outside every mount. It must equal the Claude
// launcher's launchTranscriptPath (internal/exec/claude/spec.go), which a
// test there pins: the launcher creates the directory root-owned and 0700
// before it drops privilege, so the agent can't create or replace any entry
// in it.
const WriterTranscriptPath = "/var/lib/freeside-launch/agent-transcript.jsonl"

// liveReadTimeout bounds one live read, inspect and exec together. A read
// that hangs is killed at the host; it never holds anything the writer's
// lifecycle waits on.
const liveReadTimeout = 5 * time.Second

// ErrNoLiveWriter means the run has no registered, running writer to read:
// it hasn't started, it has stopped, or the container no longer proves it is
// this run's. The caller serves nothing new.
var ErrNoLiveWriter = errors.New("no running writer is registered for this run")

// liveWriter is one running writer's registration: what a read checks a
// fresh Inspect against, and the context every read derives from.
type liveWriter struct {
	container string
	claim     objectClaim
	owner     Label
	ctx       context.Context
	cancel    context.CancelFunc
}

// liveWriters is the in-memory registry of running writers, keyed by run
// ID. It lives only as long as the daemon: a restart tears down a writer
// that hasn't completed (Recover), so no registration needs to survive one.
type liveWriters struct {
	mu      sync.Mutex
	writers map[string]*liveWriter
}

// register records a started writer and returns its removal. Removal is
// idempotent, removes only this registration, and never blocks on a read.
func (r *liveWriters) register(runID, container string, claim objectClaim, owner Label) func() {
	ctx, cancel := context.WithCancel(context.Background())
	w := &liveWriter{container: container, claim: claim, owner: owner, ctx: ctx, cancel: cancel}
	r.mu.Lock()
	if r.writers == nil {
		r.writers = map[string]*liveWriter{}
	}
	if old := r.writers[runID]; old != nil {
		old.cancel()
	}
	r.writers[runID] = w
	r.mu.Unlock()
	return func() {
		cancel()
		r.mu.Lock()
		if r.writers[runID] == w {
			delete(r.writers, runID)
		}
		r.mu.Unlock()
	}
}

func (r *liveWriters) lookup(runID string) *liveWriter {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.writers[runID]
}

// ReadLiveTranscript returns up to maxBytes of the running writer's
// transcript from byte offset. It returns ErrNoLiveWriter when the run has
// no registered writer, or the registered container is stopped or can't be
// shown to be the one this run's handoff started. The bytes are untrusted
// and unscanned: the caller must scan them before any leave the daemon.
func (b *Backend) ReadLiveTranscript(ctx context.Context, runID string, offset int64, maxBytes int) ([]byte, error) {
	if offset < 0 || maxBytes <= 0 {
		return nil, fmt.Errorf("live transcript read: offset %d, cap %d", offset, maxBytes)
	}
	w := b.live.lookup(runID)
	if w == nil {
		return nil, ErrNoLiveWriter
	}
	ctx, cancel := context.WithTimeout(ctx, liveReadTimeout)
	defer cancel()
	// Deregistration cancels the read: the writer wait has ended, so
	// nothing this read could return still describes a running writer.
	stop := context.AfterFunc(w.ctx, cancel)
	defer stop()
	ended := func(err error) error {
		if w.ctx.Err() != nil {
			return ErrNoLiveWriter
		}
		return err
	}
	rep, err := b.rt.Inspect(ctx, w.container)
	if err != nil {
		return nil, ended(fmt.Errorf("live transcript read: inspect writer: %w", err))
	}
	if rep.ID != w.container {
		return nil, ended(errors.New("live transcript read: inspect returned a report for the wrong container"))
	}
	if classifyEvidence(w.claim, w.owner, rep.CreationDate, rep.Labels, rep.LabelsObserved) != evidenceOurs {
		return nil, ErrNoLiveWriter
	}
	if rep.State != StateRunning {
		return nil, ErrNoLiveWriter
	}
	out, err := b.rt.ExecRead(ctx, w.container, liveTranscriptReadArgv(offset, maxBytes), maxBytes)
	if err != nil {
		return nil, ended(fmt.Errorf("live transcript read: %w", err))
	}
	return out, nil
}

// liveTranscriptReadScript reads one fixed file from a byte offset. It
// refuses a symbolic link or a non-regular file, reads a missing file (the
// agent not launched yet) as empty, and stops at end of file (no -f), so the
// process exits on its own. The directory is root-only, so the checks are a
// second line, not the defense. Every argument arrives positionally: the
// daemon formats the offset and cap, and nothing from the guest or a client
// reaches the script text.
const liveTranscriptReadScript = `p=$1
if [ -L "$p" ]; then exit 3; fi
if [ ! -e "$p" ]; then exit 0; fi
if [ ! -f "$p" ]; then exit 4; fi
tail -c +"$2" -- "$p" | head -c "$3"`

// liveTranscriptReadArgv is the exec argv for one read. tail -c +N starts at
// the Nth byte, counting from 1.
func liveTranscriptReadArgv(offset int64, maxBytes int) []string {
	return []string{
		"sh", "-c", liveTranscriptReadScript, "sh",
		WriterTranscriptPath, strconv.FormatInt(offset+1, 10), strconv.Itoa(maxBytes),
	}
}
