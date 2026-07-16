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

// AllOutcomes lists every valid Outcome; the single place a new outcome is
// registered, and the driver of table-driven tests. The zero value "" is
// invalid by design (the daemon enum convention).
var AllOutcomes = []Outcome{
	OutcomeComplete,
	OutcomeFail,
	OutcomeCrashBeforeResult,
	OutcomeCrashAfterResult,
}

// valid reports whether o is a registered outcome. Being a validity predicate,
// it uses default; the behaviour-dispatch switches over Outcome omit default so
// the exhaustive linter forces a new member to be handled.
func (o Outcome) valid() bool {
	switch o {
	case OutcomeComplete, OutcomeFail, OutcomeCrashBeforeResult, OutcomeCrashAfterResult:
		return true
	default:
		return false
	}
}

// StageScript is one scripted stage scenario, keyed to an invocation id via
// Script. Progression is call-step-counted, never timed: PendingInspects
// then RunningInspects are consumed one per Inspect, and the outcome applies
// on the first Inspect after both are exhausted.
type StageScript struct {
	// PendingInspects is how many Inspect calls observe StatusPending.
	PendingInspects int `json:"pending_inspects"`
	// RunningInspects is how many Inspect calls observe StatusRunning after
	// the pending ones ("delay" is more of these, not wall time).
	RunningInspects int `json:"running_inspects"`
	// Outcome is how the invocation ends once the inspect steps are spent.
	Outcome Outcome `json:"outcome"`
	// Result carries the outcome's HeadSHA, Artifacts, and Summary for the
	// outcomes that commit one; the fake stamps InvocationID and Status, so
	// a script cannot commit a result under a foreign id or a status that
	// disagrees with its outcome.
	Result exec.StageResult `json:"result"`
	// Transcript is what Stream serves; durably recorded, so it stays
	// readable after a crash.
	Transcript []byte `json:"transcript,omitempty"`
}

// stageSession is the crash-destructible half of an invocation's state.
type stageSession struct {
	script   StageScript
	pending  int
	running  int
	lost     bool // crash observed; session state is gone
	finished bool // outcome applied
}

// StageDriver is the permanent scripted fake of exec.StageDriver. With a
// persistence dir (NewStageDriverAt) the durable facets survive a real
// process restart; without one (NewStageDriver) it is a fast in-memory
// fixture. Either way the sessions map is transient: it is the provider
// session a restart loses.
type StageDriver struct {
	mu        sync.Mutex
	dir       string // persistence dir; "" means in-memory only
	scripts   map[domain.InvocationID]StageScript
	sessions  map[domain.InvocationID]*stageSession
	committed map[domain.InvocationID]exec.StageResult
	intents   map[domain.InvocationID]exec.StartSpec
}

// NewStageDriver returns an empty in-memory fake; register scenarios with
// Script before Start. State does not survive the value being discarded; use
// NewStageDriverAt for restart-recovery fixtures.
func NewStageDriver() *StageDriver {
	return &StageDriver{
		scripts:   make(map[domain.InvocationID]StageScript),
		sessions:  make(map[domain.InvocationID]*stageSession),
		committed: make(map[domain.InvocationID]exec.StageResult),
		intents:   make(map[domain.InvocationID]exec.StartSpec),
	}
}

// NewStageDriverAt returns a fake backed by dir, loading any state left there
// by a prior instance (load-or-create, like store.Open). Reconstruction is
// the restart boundary: the durable facets (scripts, committed intents,
// committed results) reload, but no live session does, so every intent that
// had not committed a result reads as a lost session (StatusGone,
// ErrNoResult) while a committed result stays collectable by id (§5.3). The
// same call both creates a fresh fake and reconstructs one after a kill.
func NewStageDriverAt(dir string) (*StageDriver, error) {
	st, err := loadStageState(dir)
	if err != nil {
		return nil, err
	}
	d := &StageDriver{
		dir:       dir,
		scripts:   st.Scripts,
		sessions:  make(map[domain.InvocationID]*stageSession),
		committed: st.Committed,
		intents:   st.Intents,
	}
	// Every committed intent whose result did not survive is a lost session:
	// the provider session is gone, but a committed result (if any) is still
	// collectable by id. Seeding here keeps Start idempotent across restart
	// (a re-Start of a known intent returns ErrDuplicateStart).
	for id := range d.intents {
		d.sessions[id] = &stageSession{script: d.scripts[id], lost: true}
	}
	return d, nil
}

// persistLocked writes the durable facets to disk, excluding the transient
// session progress. It is a no-op for an in-memory fake (empty dir). Callers
// hold d.mu.
func (d *StageDriver) persistLocked() error {
	if d.dir == "" {
		return nil
	}
	return atomicWrite(d.dir, stageStateFile, stageState{
		Scripts:   d.scripts,
		Committed: d.committed,
		Intents:   d.intents,
	})
}

// mustPersistLocked persists the durable facets or panics. Every mutator
// commits its in-memory change and the durable write as one step: a
// persistence failure is a broken test environment (unwritable dir, full
// disk), not a scripted scenario, so the fake fails loud rather than leave
// in-memory state diverged from what a restart would reload. Panicking (not
// returning) keeps that atomic: there is no half-committed state for a caller
// to observe or retry against. Callers hold d.mu.
func (d *StageDriver) mustPersistLocked(op string, id domain.InvocationID) {
	if err := d.persistLocked(); err != nil {
		panic(fmt.Errorf("fake stage driver %s %s: %w", op, id, err))
	}
}

// Script registers the scenario for an invocation id. Scripts are keyed by
// id, not ordered, so a test's call order can never bind a scenario to the
// wrong invocation.
func (d *StageDriver) Script(id domain.InvocationID, s StageScript) {
	d.mu.Lock()
	defer d.mu.Unlock()
	// Clone the scripted result so a later mutation of the caller's input
	// slice cannot reach the registered scenario (issue #35).
	s.Result = cloneStageResult(s.Result)
	d.scripts[id] = s
	d.mustPersistLocked("script", id)
}

// Start commits the invocation intent. A second Start with the same id
// returns exec.ErrDuplicateStart; an id with no registered script returns
// ErrUnscripted (a fixture bug, loud by design).
func (d *StageDriver) Start(_ context.Context, id domain.InvocationID, spec exec.StartSpec) error {
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
	// Record the committed intent durably (the outbox record): a restart
	// reconciles a started id by Inspect/Collect, never by starting again.
	d.intents[id] = spec
	d.mustPersistLocked("start", id)
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

	// Steps are spent: apply the outcome on this observing call. The three
	// committing branches fall through to persist the new committed result;
	// crash-before-result changes only transient session state and returns
	// early.
	var status exec.Status
	switch s.script.Outcome {
	case OutcomeComplete:
		s.finished = true
		d.commit(id, s.script.Result, exec.StatusCompleted)
		status = exec.StatusCompleted
	case OutcomeFail:
		s.finished = true
		d.commit(id, s.script.Result, exec.StatusFailed)
		status = exec.StatusFailed
	case OutcomeCrashBeforeResult:
		s.lost = true
		return exec.StatusGone, nil
	case OutcomeCrashAfterResult:
		s.lost = true
		d.commit(id, s.script.Result, exec.StatusCompleted)
		status = exec.StatusGone
	}
	// The switch dispatches behaviour, so it omits default (exhaustive forces a
	// new member to be handled). The invalid zero value and any unknown
	// deserialized outcome fall through here: fail loud, never silently pass.
	if !s.script.Outcome.valid() {
		return "", fmt.Errorf("fake stage driver inspect %s: unknown outcome %q", id, s.script.Outcome)
	}
	d.mustPersistLocked("inspect", id)
	return status, nil
}

// commit stamps identity and status onto the scripted result and records it.
// Callers hold d.mu.
func (d *StageDriver) commit(id domain.InvocationID, r exec.StageResult, status exec.Status) {
	r.InvocationID = id
	r.Status = status
	// Store an immutable snapshot: the committed result must not alias the
	// script (or any caller slice) so redelivery is value-identical (#35).
	d.committed[id] = cloneStageResult(r)
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
	d.mustPersistLocked("cancel", id)
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
		// Return a clone so a caller mutating the delivered slice cannot
		// alter the committed snapshot (#35).
		return cloneStageResult(r), nil
	}
	if s.lost {
		return exec.StageResult{}, fmt.Errorf("fake stage driver collect %s: %w", id, exec.ErrNoResult)
	}
	return exec.StageResult{}, fmt.Errorf("fake stage driver collect %s: %w", id, exec.ErrResultNotReady)
}
