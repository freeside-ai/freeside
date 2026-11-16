package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	specifyfake "github.com/freeside-ai/freeside/daemon/internal/specify/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// stall holds every call that reaches it until release closes or the call's
// context ends, and closes reached on the first.
type stall struct {
	reached, release chan struct{}
	once             sync.Once
}

func newStall() *stall {
	return &stall{reached: make(chan struct{}), release: make(chan struct{})}
}

func (s *stall) hold(ctx context.Context) error {
	s.once.Do(func() { close(s.reached) })
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// returnsWhileStalled runs pass until the stalled call is in flight and
// requires the pass that started it to return while it still hangs. The
// engine's one pass dispatches every project's work, so a pass that returns
// is a pass that reaches every other project (#3215).
func returnsWhileStalled(t *testing.T, s *stall, pass func() error) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		returned := make(chan error, 1)
		go func() { returned <- pass() }()
		select {
		case err := <-returned:
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-s.reached:
				return
			default:
			}
			continue
		case <-deadline:
			t.Fatal("the pass waited on the stalled call")
		}
	}
}

// TestTriageCallRunsOutsideThePass: admission holds the specifier while its
// triage call hangs, rather than waiting for the call, and admits once the
// call's record lands.
func TestTriageCallRunsOutsideThePass(t *testing.T) {
	t.Parallel()
	f := newAgentAdmissionFixture(t)
	f.addTriagerLine(t)
	f.addEscalatedSpecifierOn(t, true, agentTestEscalatedModel)
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.RegisterProject(t.Context(), domain.Project{ID: f.run.ProjectID, Repo: "owner/repo", RepositoryID: 1})
	}); err != nil {
		t.Fatal(err)
	}
	s := newStall()
	f.engine.inference = triageClient(t, taskNamingDriverFunc(
		func(ctx context.Context, _ inference.Request, _ inference.Secret) (inference.Response, error) {
			if err := s.hold(ctx); err != nil {
				return inference.Response{}, err
			}
			return inference.Response{Output: []byte(`{"tier":"standard","reason":"A small fix."}`)}, nil
		}), 10)
	f.engine.logger = slog.New(slog.DiscardHandler)
	returnsWhileStalled(t, s, func() error {
		_, _, err := f.admitSpecifierOnce(t, "inv-specification-1")
		if !errors.Is(err, errSpecifierTriagePending) {
			return err
		}
		return nil
	})
	close(s.release)
	if _, err := f.admitSpecifier(t, "inv-specification-1"); err != nil {
		t.Fatal(err)
	}
	if record, _ := f.triageState(t); record == nil || record.Outcome != domain.TriageOutcomeAnswered {
		t.Fatalf("triage record = %+v", record)
	}
}

// TestDiscussionReplyRunsOutsideThePass: a pass returns while a discussion's
// model call hangs, and a later pass reports the reply the call produced.
func TestDiscussionReplyRunsOutsideThePass(t *testing.T) {
	t.Parallel()
	s := newStall()
	f := newAttentionDiscussionFixture(t, domain.AttentionExecutionFailure, taskNamingDriverFunc(
		func(ctx context.Context, _ inference.Request, _ inference.Secret) (inference.Response, error) {
			if err := s.hold(ctx); err != nil {
				return inference.Response{}, err
			}
			return inference.Response{Output: []byte(`{"reply":"The decision set is unchanged."}`), ComputeUnits: 1}, nil
		}))
	f.submit(t)
	e := f.engine(t, true)
	returnsWhileStalled(t, s, func() error {
		_, err := e.Reconcile(t.Context())
		return err
	})
	close(s.release)
	if result, err := reconcileSettled(t, e); err != nil || result.ResultsAccepted != 1 {
		t.Fatalf("Reconcile = %+v, %v", result, err)
	}
	f.assertReply(t, "The decision set is unchanged.", []domain.Action{domain.ActionDiscuss, domain.ActionStop})
}

// TestResearchFetchRunsOutsideThePass: a pass returns while a specification
// attempt's research fetch hangs, and the pass that takes the fetched
// research accepts it without fetching again.
func TestResearchFetchRunsOutsideThePass(t *testing.T) {
	t.Parallel()
	f := newSpecificationFixture(t, true, 2)
	s := newStall()
	f.researchTransport = specificationRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		f.fetchCalls.Add(1)
		if err := s.hold(request.Context()); err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/plain"}},
			Body:       io.NopCloser(strings.NewReader("authoritative research")), Request: request,
		}, nil
	})
	driver := f.newDriver(t)
	if err := specifyfake.Script(driver, specificationInvocationID("specification-run", 1), 0, 0, specify.Output{
		FetchRequests: []specify.FetchRequest{{URL: "https://docs.example/contracts", Purpose: "verify the contract"}},
	}); err != nil {
		t.Fatal(err)
	}
	f.submit(t)
	e := f.newEngine(t, driver)
	returnsWhileStalled(t, s, func() error {
		_, err := e.Reconcile(t.Context())
		return err
	})
	close(s.release)
	if result, err := reconcileSettled(t, e); err != nil || result.ResultsAccepted != 1 {
		t.Fatalf("Reconcile = %+v, %v", result, err)
	}
	if got := f.fetchCalls.Load(); got != 1 {
		t.Fatalf("research fetches = %d, want 1", got)
	}
}

// TestOffPassTriageErrorStopsTheNextPass: a triage call that fails outside
// the pass stops the next pass with its error, as it stopped the pass that
// made the call when the call ran inside it, and is reported once.
func TestOffPassTriageErrorStopsTheNextPass(t *testing.T) {
	t.Parallel()
	f := newAttentionDiscussionFixture(t, domain.AttentionExecutionFailure, nil)
	e := f.engine(t, false)
	failed := errors.New("record specifier triage: store refused")
	e.triageOutcomes.record(false, failed)
	if _, err := e.Reconcile(t.Context()); !errors.Is(err, failed) {
		t.Fatalf("Reconcile after a failed triage = %v, want %v", err, failed)
	}
	if _, err := e.Reconcile(t.Context()); err != nil {
		t.Fatalf("the next Reconcile reported the triage failure again: %v", err)
	}
}

// TestFailureDiagnosisRunsOutsideThePass: recording a failed run returns
// while the run's advisory diagnosis call hangs.
func TestFailureDiagnosisRunsOutsideThePass(t *testing.T) {
	t.Parallel()
	s := newStall()
	workflow, run, terminal, _ := newDiagnosticFixture(t, taskNamingDriverFunc(
		func(ctx context.Context, _ inference.Request, _ inference.Secret) (inference.Response, error) {
			if err := s.hold(ctx); err != nil {
				return inference.Response{}, err
			}
			return inference.Response{Output: []byte(`{"probable_cause":"tool failure","explanation":"failed"}`)}, nil
		}))
	returnsWhileStalled(t, s, func() error {
		_, err := workflow.recordProductionTerminalWithAuthority(t.Context(), run, terminal, false)
		return err
	})
	close(s.release)
	workflow.waitOffPass()
}

// countingStart counts the driver's Start calls and, with err set, refuses
// every one, as the stage driver's own start-authority check would.
type countingStart struct {
	exec.StageDriver
	calls atomic.Int32
	err   error
}

func (d *countingStart) Start(ctx context.Context, id domain.InvocationID, spec exec.StartSpec) error {
	d.calls.Add(1)
	if d.err != nil {
		return d.err
	}
	return d.StageDriver.Start(ctx, id, spec)
}

// newStartCheckFixture is a drain dispatch fixture whose driver counts its
// starts and whose engine holds the daemon lock, as production's does: the
// fixture's admission is credentialed, so a pass that replays its reserved
// intent needs the lock.
func newStartCheckFixture(t *testing.T, startErr error) (*drainDispatchFixture, *countingStart) {
	t.Helper()
	var driver *countingStart
	h := newDrainDispatchFixture(t, func(stage exec.StageDriver) exec.StageDriver {
		driver = &countingStart{StageDriver: stage, err: startErr}
		return driver
	})
	lock, err := daemonlock.Acquire(h.fixture.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	h.engine.databaseLock = lock
	return h, driver
}

func (h *drainDispatchFixture) outbox(t *testing.T) store.QueueEntry {
	t.Helper()
	var entry store.QueueEntry
	if err := h.fixture.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetOutbox(t.Context(), string(h.invocationID))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return entry
}

// TestStartAuthorityCheckRunsOutsideThePass: a pass returns while an
// invocation's start-authority check hangs, holds only that invocation with
// no recorded reason, keeps its dispatch reservation, and never calls Start;
// a later pass starts it once the check has passed (#3224).
func TestStartAuthorityCheckRunsOutsideThePass(t *testing.T) {
	t.Parallel()
	h, driver := newStartCheckFixture(t, nil)
	s := newStall()
	var checks atomic.Int32
	h.engine.startAuthority = func(ctx context.Context, _ domain.InvocationID, _ exec.StartSpec) error {
		checks.Add(1)
		return s.hold(ctx)
	}
	h.fixture.submit(t)
	returnsWhileStalled(t, s, func() error {
		started, err := h.engine.dispatchPendingInvocations(t.Context())
		if err == nil && started != 0 {
			return fmt.Errorf("started %d while the check hangs", started)
		}
		return err
	})
	if started := h.dispatch(t); started != 0 {
		t.Fatalf("a second pass started %d while the check hangs", started)
	}
	if got := checks.Load(); got != 1 {
		t.Fatalf("checks = %d while the first hangs, want 1", got)
	}
	if got := driver.calls.Load(); got != 0 {
		t.Fatalf("Start called %d times while the check hangs", got)
	}
	if hold := storedRunHold(t, h.fixture.store, "specification-run"); hold != nil {
		t.Fatalf("hold = %+v, want none recorded", hold)
	}
	if !h.attemptRecorded(t) {
		t.Fatal("the pending start recorded no attempt")
	}
	if status := h.outbox(t).Status; status != "dispatching" {
		t.Fatalf("outbox status while the check hangs = %q, want the reservation kept", status)
	}

	close(s.release)
	h.engine.waitOffPass()
	if started := h.dispatch(t); started != 1 {
		t.Fatalf("started %d after the check passed, want 1", started)
	}
	if got := driver.calls.Load(); got != 1 {
		t.Fatalf("Start called %d times, want 1", got)
	}
	if got := checks.Load(); got != 1 {
		t.Fatalf("checks = %d, want the passed check's answer used", got)
	}
	if !h.outbox(t).Dispatched() {
		t.Fatal("the started intent was not marked dispatched")
	}
}

// TestRefusedStartAuthorityCheckRefusesLikeAFailedStart: a check that
// refuses fails the start with the same error and leaves the same stored
// state as a driver Start that refuses with it, and the engine never calls
// Start for it (#3224).
func TestRefusedStartAuthorityCheckRefusesLikeAFailedStart(t *testing.T) {
	t.Parallel()
	refusal := errors.New("commit author is not the App bot")

	byStart, startDriver := newStartCheckFixture(t, refusal)
	byStart.fixture.submit(t)
	started, startErr := byStart.engine.dispatchPendingInvocations(t.Context())
	if started != 0 || !errors.Is(startErr, refusal) {
		t.Fatalf("refused Start: started %d, err %v; want 0 and the refusal", started, startErr)
	}

	byCheck, checkDriver := newStartCheckFixture(t, nil)
	byCheck.engine.startAuthority = func(context.Context, domain.InvocationID, exec.StartSpec) error {
		return refusal
	}
	byCheck.fixture.submit(t)
	if started := byCheck.dispatch(t); started != 0 {
		t.Fatalf("the pass that started the check started %d", started)
	}
	byCheck.engine.waitOffPass()
	started, checkErr := byCheck.engine.dispatchPendingInvocations(t.Context())
	if started != 0 || !errors.Is(checkErr, refusal) {
		t.Fatalf("refused check: started %d, err %v; want 0 and the refusal", started, checkErr)
	}
	if got := checkDriver.calls.Load(); got != 0 {
		t.Fatalf("Start called %d times after a refused check", got)
	}
	if got := startDriver.calls.Load(); got != 1 {
		t.Fatalf("refusing driver Start called %d times, want 1", got)
	}

	for name, h := range map[string]*drainDispatchFixture{"refused Start": byStart, "refused check": byCheck} {
		entry := h.outbox(t)
		if entry.Status != "pending" {
			t.Errorf("%s: outbox status %q, want the reservation released to pending", name, entry.Status)
		}
		if !h.attemptRecorded(t) {
			t.Errorf("%s: no attempt recorded", name)
		}
		if hold := storedRunHold(t, h.fixture.store, "specification-run"); hold != nil {
			t.Errorf("%s: hold = %+v, want none", name, hold)
		}
	}
}

// TestRunJoinsAStartAuthorityCheck: Run returns only after an in-flight
// start-authority check has returned, and a check that returns after
// shutdown canceled its context keeps no answer, so the next pass checks
// again (#3224).
func TestRunJoinsAStartAuthorityCheck(t *testing.T) {
	t.Parallel()
	h, driver := newStartCheckFixture(t, nil)
	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var checks atomic.Int32
	// The check ignores its context until release closes, so Run can return
	// before it only by not joining it.
	h.engine.startAuthority = func(context.Context, domain.InvocationID, exec.StartSpec) error {
		checks.Add(1)
		once.Do(func() { close(reached) })
		<-release
		return nil
	}
	h.fixture.submit(t)
	ctx, cancel := context.WithCancel(t.Context())
	ran := make(chan error, 1)
	go func() { ran <- h.engine.Run(ctx, time.Millisecond) }()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("Run never started the check")
	}
	cancel()
	select {
	case err := <-ran:
		t.Fatalf("Run returned before the check it started: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-ran:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the check did")
	}
	h.engine.startChecks.mu.Lock()
	kept := len(h.engine.startChecks.done)
	h.engine.startChecks.mu.Unlock()
	if kept != 0 {
		t.Fatalf("a check shutdown canceled kept %d answers", kept)
	}

	if started := h.dispatch(t); started != 0 {
		t.Fatalf("started %d without a fresh check", started)
	}
	h.engine.waitOffPass()
	if started := h.dispatch(t); started != 1 {
		t.Fatalf("started %d after the fresh check, want 1", started)
	}
	if got := checks.Load(); got != 2 {
		t.Fatalf("checks = %d, want the canceled one and a fresh one", got)
	}
	if got := driver.calls.Load(); got != 1 {
		t.Fatalf("Start called %d times, want 1", got)
	}
}
