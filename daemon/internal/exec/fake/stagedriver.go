package fake

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

// Compile-time contract assertions (the exec package convention): a
// signature drift in the interfaces fails this build, not a test.
var (
	_ exec.StageDriver  = (*StageDriver)(nil)
	_ exec.ReviewSource = (*ReviewSource)(nil)
)

// ErrUnscripted marks a fixture bug: an invocation id was started without a
// script registered for it.
var ErrUnscripted = errors.New("fake: no script registered for invocation id")

// Outcome is how a scripted stage invocation ends.
type Outcome string

const (
	// OutcomeComplete commits the scripted result with StatusCompleted.
	OutcomeComplete Outcome = "complete"
	// OutcomeFail commits the scripted result with StatusFailed.
	OutcomeFail Outcome = "fail"
	// OutcomeCrashBeforeResult loses the session before any result is
	// committed: Inspect reports StatusGone, Collect returns ErrNoResult.
	OutcomeCrashBeforeResult Outcome = "crash_before_result"
	// OutcomeCrashAfterResult commits the scripted result and then loses
	// the session: Inspect reports StatusGone, but the result stays
	// recoverable by invocation id (§5.3 reconciliation).
	OutcomeCrashAfterResult Outcome = "crash_after_result"
)

// StageScript is one scripted stage scenario, keyed to an invocation id via
// Script. Progression is call-step-counted, never timed: PendingInspects
// then RunningInspects are consumed one per Inspect, and the outcome applies
// on the first Inspect after both are exhausted.
type StageScript struct {
	// PendingInspects is how many Inspect calls observe StatusPending.
	PendingInspects int
	// RunningInspects is how many Inspect calls observe StatusRunning after
	// the pending ones ("delay" is more of these, not wall time).
	RunningInspects int
	// Outcome is how the invocation ends once the inspect steps are spent.
	Outcome Outcome
	// Result carries the outcome's HeadSHA, Artifacts, and Summary for the
	// outcomes that commit one; the fake stamps InvocationID and Status, so
	// a script cannot commit a result under a foreign id or a status that
	// disagrees with its outcome.
	Result exec.StageResult
	// Transcript is what Stream serves; durably recorded, so it stays
	// readable after a crash.
	Transcript []byte
}

// stageSession is the crash-destructible half of an invocation's state.
type stageSession struct {
	script   StageScript
	pending  int
	running  int
	lost     bool // crash observed; session state is gone
	finished bool // outcome applied
}

// StageDriver is the permanent scripted fake of exec.StageDriver.
type StageDriver struct {
	mu        sync.Mutex
	scripts   map[domain.InvocationID]StageScript
	sessions  map[domain.InvocationID]*stageSession
	committed map[domain.InvocationID]exec.StageResult
}

// NewStageDriver returns an empty fake; register scenarios with Script
// before Start.
func NewStageDriver() *StageDriver {
	return &StageDriver{
		scripts:   make(map[domain.InvocationID]StageScript),
		sessions:  make(map[domain.InvocationID]*stageSession),
		committed: make(map[domain.InvocationID]exec.StageResult),
	}
}

// Script registers the scenario for an invocation id. Scripts are keyed by
// id, not ordered, so a test's call order can never bind a scenario to the
// wrong invocation.
func (d *StageDriver) Script(id domain.InvocationID, s StageScript) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.scripts[id] = s
}

// Start commits the invocation intent. A second Start with the same id
// returns exec.ErrDuplicateStart; an id with no registered script returns
// ErrUnscripted (a fixture bug, loud by design).
func (d *StageDriver) Start(_ context.Context, id domain.InvocationID, _ exec.StartSpec) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.sessions[id]; ok {
		return fmt.Errorf("fake stage driver start %s: %w", id, exec.ErrDuplicateStart)
	}
	if _, ok := d.committed[id]; ok {
		return fmt.Errorf("fake stage driver start %s: %w", id, exec.ErrDuplicateStart)
	}
	script, ok := d.scripts[id]
	if !ok {
		return fmt.Errorf("fake stage driver start %s: %w", id, ErrUnscripted)
	}
	d.sessions[id] = &stageSession{
		script:  script,
		pending: script.PendingInspects,
		running: script.RunningInspects,
	}
	return nil
}

// Inspect consumes one scripted step and reports the resulting status.
func (d *StageDriver) Inspect(_ context.Context, id domain.InvocationID) (exec.Status, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.sessions[id]
	if !ok {
		return "", fmt.Errorf("fake stage driver inspect %s: %w", id, exec.ErrUnknownInvocation)
	}
	switch {
	case s.lost:
		return exec.StatusGone, nil
	case s.finished:
		return d.committed[id].Status, nil
	case s.pending > 0:
		s.pending--
		return exec.StatusPending, nil
	case s.running > 0:
		s.running--
		return exec.StatusRunning, nil
	}

	// Steps are spent: apply the outcome on this observing call.
	switch s.script.Outcome {
	case OutcomeComplete:
		s.finished = true
		d.commit(id, s.script.Result, exec.StatusCompleted)
		return exec.StatusCompleted, nil
	case OutcomeFail:
		s.finished = true
		d.commit(id, s.script.Result, exec.StatusFailed)
		return exec.StatusFailed, nil
	case OutcomeCrashBeforeResult:
		s.lost = true
		return exec.StatusGone, nil
	case OutcomeCrashAfterResult:
		s.lost = true
		d.commit(id, s.script.Result, exec.StatusCompleted)
		return exec.StatusGone, nil
	}
	return "", fmt.Errorf("fake stage driver inspect %s: unknown outcome %q", id, s.script.Outcome)
}

// commit stamps identity and status onto the scripted result and records it.
// Callers hold d.mu.
func (d *StageDriver) commit(id domain.InvocationID, r exec.StageResult, status exec.Status) {
	r.InvocationID = id
	r.Status = status
	d.committed[id] = r
}

// Stream returns a reader over the scripted transcript. Each call reads from
// the beginning (the transcript is durably recorded, §5.3), so it works
// before, during, and after a crash.
func (d *StageDriver) Stream(_ context.Context, id domain.InvocationID) (io.ReadCloser, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.sessions[id]; !ok {
		return nil, fmt.Errorf("fake stage driver stream %s: %w", id, exec.ErrUnknownInvocation)
	}
	return io.NopCloser(bytes.NewReader(d.scripts[id].Transcript)), nil
}

// Cancel stops a live invocation and commits a canceled result carrying the
// script's transcript-side fields. Canceling an invocation with a committed
// result is a no-op (the result stands); canceling a lost session is a no-op
// too (there is nothing left to stop, and nothing to commit).
func (d *StageDriver) Cancel(_ context.Context, id domain.InvocationID) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.sessions[id]
	if !ok {
		return fmt.Errorf("fake stage driver cancel %s: %w", id, exec.ErrUnknownInvocation)
	}
	if _, ok := d.committed[id]; ok || s.lost {
		return nil
	}
	s.finished = true
	d.commit(id, s.script.Result, exec.StatusCanceled)
	return nil
}

// Collect returns the committed result, identically on every call: duplicate
// delivery is inherent, acceptance is the caller's job. Before any result is
// committed it returns exec.ErrResultNotReady; after a crash that beat the
// result, exec.ErrNoResult.
func (d *StageDriver) Collect(_ context.Context, id domain.InvocationID) (exec.StageResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.sessions[id]
	if !ok {
		return exec.StageResult{}, fmt.Errorf("fake stage driver collect %s: %w", id, exec.ErrUnknownInvocation)
	}
	if r, ok := d.committed[id]; ok {
		return r, nil
	}
	if s.lost {
		return exec.StageResult{}, fmt.Errorf("fake stage driver collect %s: %w", id, exec.ErrNoResult)
	}
	return exec.StageResult{}, fmt.Errorf("fake stage driver collect %s: %w", id, exec.ErrResultNotReady)
}
