package engine

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	execfake "github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// stillRunning is a root invocation that stays running until the daemon ends
// it: a restart reloads it as lost, and a cancellation commits it canceled.
var stillRunning = execfake.StageScript{
	Outcome: execfake.OutcomeComplete, RunningInspects: 1 << 20,
	Result: exec.StageResult{Summary: "the implementation finished"},
}

// newRunningCampaignFixture is the campaign fixture with its first attempt's
// root implementation invocation still running.
func newRunningCampaignFixture(t *testing.T) failedImplementationFixture {
	t.Helper()
	return newFailedImplementationCampaignFixtureScript(t, stillRunning)
}

// restart models a daemon restart: the fake driver reloads from its
// directory, so every session that had not committed a result reads as lost,
// and a new engine runs over the same store.
func (f failedImplementationFixture) restart(t *testing.T) failedImplementationFixture {
	t.Helper()
	f.driver = f.newDriver(t)
	f.engine = f.newEngine(t, f.driver)
	return f
}

// shutDown models a graceful shutdown: the stage driver cancels the running
// session and commits it canceled, then the daemon restarts.
func (f failedImplementationFixture) shutDown(t *testing.T, invocationID domain.InvocationID) failedImplementationFixture {
	t.Helper()
	if err := f.driver.Cancel(t.Context(), invocationID); err != nil {
		t.Fatal(err)
	}
	return f.restart(t)
}

// attemptRun reads the campaign's nth attempt run, if it was allocated.
func (f failedImplementationFixture) attemptRun(t *testing.T, n int) (domain.Run, bool) {
	t.Helper()
	runID, err := ProductionAttemptRunID(f.run.CampaignID, n)
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.specificationFixture.run(runID)
	if errors.Is(err, store.ErrNotFound) {
		return domain.Run{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return run, true
}

// mustAttemptRun is attemptRun for an attempt the test requires.
func (f failedImplementationFixture) mustAttemptRun(t *testing.T, n int) domain.Run {
	t.Helper()
	run, found := f.attemptRun(t, n)
	if !found {
		t.Fatalf("campaign has no attempt %d", n)
	}
	return run
}

// startAttempt dispatches an allocated attempt with script on its root
// invocation and returns the run and attempt the dispatch recorded.
func (f failedImplementationFixture) startAttempt(
	t *testing.T, run domain.Run, script execfake.StageScript,
) (domain.Run, domain.Attempt) {
	t.Helper()
	f.driver.Script(productionInvocationID(run.ID), script)
	if err := dispatchParked(t, f, run); err != nil {
		t.Fatalf("dispatch attempt %s = %v", run.ID, err)
	}
	started, err := f.specificationFixture.run(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	stage, ok := findProductionStage(started)
	if !ok || len(stage.Attempts) != 1 {
		t.Fatalf("started attempt %s stages = %+v", run.ID, started.Stages)
	}
	return started, stage.Attempts[0]
}

// collect records the attempt's terminal the way the engine's pass does.
func (f failedImplementationFixture) collect(t *testing.T, run domain.Run, attempt domain.Attempt) {
	t.Helper()
	if accepted, err := f.engine.acceptProductionAttempt(t.Context(), run, attempt); err != nil || accepted {
		t.Fatalf("acceptProductionAttempt(%s) = %t, %v; want false, nil", attempt.InvocationID, accepted, err)
	}
}

func (f failedImplementationFixture) hasCard(t *testing.T, invocationID domain.InvocationID) bool {
	t.Helper()
	err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		_, err := tx.GetAttentionItem(t.Context(), domain.ExecutionFailureItemID(invocationID))
		return err
	})
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

// wantRerun checks the campaign's nth attempt is a daemon re-run of parent
// under reason, and that the parent's terminal raised no card.
func (f failedImplementationFixture) wantRerun(t *testing.T, n int, parent domain.Run, reason string) domain.Run {
	t.Helper()
	next, found := f.attemptRun(t, n)
	if !found || next.AttemptReason != reason || next.ParentRunID != parent.ID || next.TaskID != parent.TaskID {
		t.Fatalf("attempt %d = %+v (found %t), want a re-run of %s under %q", n, next, found, parent.ID, reason)
	}
	if f.hasCard(t, productionInvocationID(parent.ID)) {
		t.Fatalf("the re-run of %s raised its execution_failure card", parent.ID)
	}
	return next
}

// TestDaemonRerunAfterARestartLoss: a restart during the writer loses the
// root invocation. Its gone terminal records the lost outcome, raises no
// card, and allocates exactly one next attempt under the loss reason; a
// replayed pass allocates nothing more.
func TestDaemonRerunAfterARestartLoss(t *testing.T) {
	t.Parallel()
	f := newRunningCampaignFixture(t).restart(t)
	f.collect(t, f.run, f.attempt)
	if outcome := f.outcome(t); outcome.Status != domain.ExecutionOutcomeLost {
		t.Fatalf("outcome = %+v, want lost", outcome)
	}
	f.wantRerun(t, 2, f.run, daemonLossRerunReason)
	f.collect(t, f.run, f.attempt)
	if _, found := f.attemptRun(t, 3); found {
		t.Fatal("a replayed loss allocated a second re-run")
	}
}

// TestDaemonRerunKeepsTheCardForALiveLoss: ward also closes a live handoff
// as a loss when the writer outlives its stop budget or refuses to start, and
// the process that started the writer then sees it gone. That is the work
// failing, not a restart, so it keeps its card. If the process dies before
// recording it, the next process sees an ordinary restart loss and re-runs.
func TestDaemonRerunKeepsTheCardForALiveLoss(t *testing.T) {
	t.Parallel()
	crash := execfake.StageScript{Outcome: execfake.OutcomeCrashBeforeResult}
	for _, tc := range []struct {
		name    string
		restart bool
	}{
		{"seen live", false},
		{"unrecorded at a restart", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newRunningCampaignFixture(t).restart(t)
			f.collect(t, f.run, f.attempt)
			second := f.wantRerun(t, 2, f.run, daemonLossRerunReason)
			second, attempt := f.startAttempt(t, second, crash)
			if tc.restart {
				f = f.restart(t)
			}
			f.collect(t, second, attempt)
			if !tc.restart {
				if _, found := f.attemptRun(t, 3); found || !f.hasCard(t, attempt.InvocationID) {
					t.Fatalf("a live loss re-ran (%t) or kept no card", found)
				}
				return
			}
			f.wantRerun(t, 3, second, daemonLossRerunReason)
		})
	}
}

// TestDaemonRerunAfterAShutdown: a graceful shutdown cancels the writer.
// After the restart its canceled terminal gets exactly one next attempt
// under the cancel reason and raises no card.
func TestDaemonRerunAfterAShutdown(t *testing.T) {
	t.Parallel()
	f := newRunningCampaignFixture(t)
	f = f.shutDown(t, f.attempt.InvocationID)
	f.collect(t, f.run, f.attempt)
	if outcome := f.outcome(t); outcome.Status != domain.ExecutionOutcomeCanceled {
		t.Fatalf("outcome = %+v, want canceled", outcome)
	}
	f.wantRerun(t, 2, f.run, daemonCancelRerunReason)
	if _, found := f.attemptRun(t, 3); found {
		t.Fatal("a shutdown allocated more than one re-run")
	}
}

// TestDaemonRerunCompletedWriterIsNotRerun: a writer that committed its
// result before the restart is adopted with that result, so nothing re-runs.
func TestDaemonRerunCompletedWriterIsNotRerun(t *testing.T) {
	t.Parallel()
	f := newFailedImplementationCampaignFixtureScript(t, execfake.StageScript{
		Outcome: execfake.OutcomeCrashAfterResult,
		Result:  exec.StageResult{Summary: "the implementation finished"},
	})
	// The first inspection commits the result and then loses the session.
	if _, err := f.driver.Inspect(t.Context(), f.attempt.InvocationID); err != nil {
		t.Fatal(err)
	}
	f = f.restart(t)
	if _, err := f.engine.acceptProductionAttempt(t.Context(), f.run, f.attempt); err != nil {
		t.Fatalf("acceptProductionAttempt = %v", err)
	}
	if _, found := f.attemptRun(t, 2); found {
		t.Fatal("a completed writer was re-run")
	}
}

// TestDaemonRerunKeepsTheCardForATaskStop: a task Stop records the task's
// cancellation before it cancels work, so its canceled terminal raises the
// card and allocates nothing.
func TestDaemonRerunKeepsTheCardForATaskStop(t *testing.T) {
	t.Parallel()
	f := newRunningCampaignFixture(t)
	stopRunTask(t, f.store, f.run.ID)
	if err := f.driver.Cancel(t.Context(), f.attempt.InvocationID); err != nil {
		t.Fatal(err)
	}
	f.collect(t, f.run, f.attempt)
	if !f.hasFailureItem(t) {
		t.Fatal("a stopped task's canceled terminal raised no card")
	}
	if _, found := f.attemptRun(t, 2); found {
		t.Fatal("a stopped task was re-run")
	}
}

// TestDaemonRerunLimitsLossesPerChain: the first and second losses on a
// chain re-run and the third raises the card. A cancel re-run between them
// neither counts nor breaks the chain, and an operator retry after the card
// starts a new chain.
func TestDaemonRerunLimitsLossesPerChain(t *testing.T) {
	t.Parallel()
	f := newRunningCampaignFixture(t).restart(t)

	// Loss one.
	f.collect(t, f.run, f.attempt)
	second := f.wantRerun(t, 2, f.run, daemonLossRerunReason)

	// A shutdown: uncounted.
	second, attempt := f.startAttempt(t, second, stillRunning)
	f = f.shutDown(t, attempt.InvocationID)
	f.collect(t, second, attempt)
	third := f.wantRerun(t, 3, second, daemonCancelRerunReason)

	// Loss two.
	third, attempt = f.startAttempt(t, third, stillRunning)
	f = f.restart(t)
	f.collect(t, third, attempt)
	fourth := f.wantRerun(t, 4, third, daemonLossRerunReason)

	// Loss three: the card, and no further attempt.
	fourth, attempt = f.startAttempt(t, fourth, stillRunning)
	f = f.restart(t)
	f.collect(t, fourth, attempt)
	if !f.hasCard(t, attempt.InvocationID) {
		t.Fatal("the third loss on a chain raised no card")
	}
	if _, found := f.attemptRun(t, 5); found {
		t.Fatal("the third loss on a chain re-ran")
	}

	// An operator retry starts a new chain, whose first loss re-runs.
	retry, err := ReattemptProductionRun(t.Context(), f.store, ProductionReattemptSpec{
		ParentRunID: fourth.ID, Reason: "Operator retry after repeated losses.",
	})
	if err != nil {
		t.Fatal(err)
	}
	fifth, attempt := f.startAttempt(t, retry.Run.Run, stillRunning)
	f = f.restart(t)
	f.collect(t, fifth, attempt)
	f.wantRerun(t, 6, fifth, daemonLossRerunReason)
}

// TestDaemonRerunOnlyAnswersTheLatestRootAttempt: a terminal keeps the card
// when no next attempt can be allocated for it, or when it isn't the run's
// root implementation invocation: a remediation round or operator feedback
// would lose its reviewed work to a fresh attempt.
func TestDaemonRerunOnlyAnswersTheLatestRootAttempt(t *testing.T) {
	t.Parallel()
	f := newRunningCampaignFixture(t).restart(t)
	root := productionInvocationID(f.run.ID)
	for _, tc := range []struct {
		name     string
		terminal productionTerminalRecord
	}{
		{"failed root", productionTerminalRecord{InvocationID: root, RunID: f.run.ID, Status: exec.StatusFailed}},
		{"lost operator feedback", productionTerminalRecord{
			InvocationID: "inv-operator-feedback-1", RunID: f.run.ID, Status: exec.StatusGone,
		}},
		{"canceled remediation round", productionTerminalRecord{
			InvocationID: remediationInvocationID(f.run.ID, 1), RunID: f.run.ID, Status: exec.StatusCanceled,
		}},
	} {
		if reason := f.rerunReason(t, f.run, tc.terminal); reason != "" {
			t.Errorf("%s: re-run reason = %q, want none", tc.name, reason)
		}
	}
	lost := productionTerminalRecord{InvocationID: root, RunID: f.run.ID, Status: exec.StatusGone}
	if reason := f.rerunReason(t, f.run, lost); reason != daemonLossRerunReason {
		t.Fatalf("latest root loss re-run reason = %q, want the loss reason", reason)
	}

	// Once the next attempt exists the run is no longer its campaign's latest.
	f.collect(t, f.run, f.attempt)
	if reason := f.rerunReason(t, f.run, lost); reason != "" {
		t.Errorf("superseded attempt: re-run reason = %q, want none", reason)
	}

	// A closed workflow allocates nothing either.
	second := f.mustAttemptRun(t, 2)
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		task, err := tx.GetTask(t.Context(), second.TaskID)
		if err != nil {
			return err
		}
		if _, err := tx.AbandonTask(t.Context(), task.ID, *f.now); err != nil {
			return err
		}
		_, _, err = tx.CloseTaskWorkflow(t.Context(), task.ID, task.CurrentStart().Ordinal, *f.now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	lostSecond := productionTerminalRecord{
		InvocationID: productionInvocationID(second.ID), RunID: second.ID, Status: exec.StatusGone,
	}
	if reason := f.rerunReason(t, second, lostSecond); reason != "" {
		t.Errorf("closed workflow: re-run reason = %q, want none", reason)
	}
}

func (f failedImplementationFixture) rerunReason(
	t *testing.T, run domain.Run, terminal productionTerminalRecord,
) string {
	t.Helper()
	var reason string
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		var err error
		reason, err = daemonRerunReason(t.Context(), tx, run, terminal, false)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return reason
}

// TestDaemonRerunPassesTheAdmissionReGate: the re-run attempt has no
// admission of its own until dispatch admits it under current policy, like
// any other attempt. An engine whose backend falls below the capability
// floor refuses it and starts nothing; once admitted, a tampered admission
// row fails the attempt's collection closed instead of yielding a terminal
// or another re-run.
func TestDaemonRerunPassesTheAdmissionReGate(t *testing.T) {
	t.Parallel()
	f := newRunningCampaignFixture(t).restart(t)
	f.collect(t, f.run, f.attempt)
	second := f.wantRerun(t, 2, f.run, daemonLossRerunReason)
	invocationID := productionInvocationID(second.ID)
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		_, found, err := tx.LookupExecutionAdmission(t.Context(), invocationID)
		if err == nil && found {
			return errors.New("the re-run inherited an admission before dispatch")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	floor := f.specificationFixture
	floor.admissionFloor = []exec.Capability{exec.CapEnforcedProviderEgress}
	refusing := f
	refusing.engine = floor.newEngine(t, f.driver)
	f.driver.Script(invocationID, stillRunning)
	if err := dispatchParked(t, refusing, second); !errors.Is(err, exec.ErrCapabilityRefused) {
		t.Fatalf("dispatch below the floor = %v, want ErrCapabilityRefused", err)
	}
	if inspection, err := f.driver.Inspect(t.Context(), invocationID); !errors.Is(err, exec.ErrUnknownInvocation) {
		t.Fatalf("refused re-run inspection = %+v, %v; want it never started", inspection, err)
	}

	second, attempt := f.startAttempt(t, second, stillRunning)
	db, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(t.Context(),
		`UPDATE execution_admissions SET body = json_set(body, '$.egress_profile', 'provider_web_read') WHERE invocation_id = ?`,
		string(invocationID)); err != nil {
		t.Fatal(err)
	}
	f = f.restart(t)
	if _, err := f.engine.acceptProductionAttempt(t.Context(), second, attempt); !errors.Is(err, domain.ErrAdmissionInconsistent) {
		t.Fatalf("collection over a tampered re-run admission = %v, want ErrAdmissionInconsistent", err)
	}
	if _, found := f.attemptRun(t, 3); found {
		t.Fatal("a tampered re-run admission yielded another re-run")
	}
}

// chainLosses reads the losses already re-run on run's chain.
func (f failedImplementationFixture) chainLosses(t *testing.T, run domain.Run) int {
	t.Helper()
	var losses int
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		losses, err = daemonRerunLosses(t.Context(), tx, run)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return losses
}

// TestDaemonRerunSurvivesACrashOnEitherSide models process loss at the
// daemon re-run's seams. Before the write the next attempt doesn't exist and
// the repeat allocates it; after it the attempt exists and the repeat
// allocates nothing more. Either way exactly one re-run is allocated.
func TestDaemonRerunSurvivesACrashOnEitherSide(t *testing.T) {
	t.Parallel()
	for _, side := range AllDurableTransitionSides {
		t.Run(string(side), func(t *testing.T) {
			t.Parallel()
			f := newRunningCampaignFixture(t).restart(t)
			injected := errors.New("injected process loss")
			crashing := f.newEngineWithTransitionHook(t, f.driver,
				func(transition DurableTransition, at DurableTransitionSide) error {
					if transition == DurableTransitionDaemonRerun && at == side {
						return injected
					}
					return nil
				})
			if _, err := crashing.acceptProductionAttempt(t.Context(), f.run, f.attempt); !errors.Is(err, injected) {
				t.Fatalf("collection with a crash %s = %v, want the injected loss", side, err)
			}
			_, allocated := f.attemptRun(t, 2)
			if allocated != (side == DurableTransitionAfter) {
				t.Fatalf("re-run allocated at the crash %s = %t", side, allocated)
			}
			f = f.restart(t)
			f.collect(t, f.run, f.attempt)
			f.wantRerun(t, 2, f.run, daemonLossRerunReason)
			if _, found := f.attemptRun(t, 3); found {
				t.Fatal("the repeat allocated a second re-run")
			}
		})
	}
}
