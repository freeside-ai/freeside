package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	execfake "github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	specifyfake "github.com/freeside-ai/freeside/daemon/internal/specify/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

type failedImplementationFixture struct {
	specificationFixture
	engine  *Engine
	driver  *execfake.StageDriver
	run     domain.Run
	attempt domain.Attempt
}

// newFailedImplementationFixture drives an auto-approved specification into an
// admitted, started implementation attempt whose fake driver ends with the
// given non-blocked outcome. It models the real stage driver's ordering: a
// caller pre-records the ExecutionOutcome (as recordOutcome/recordLost do)
// before reconciling, so a test can prove the engine still collects the
// terminal and raises the execution_failure item. It carries none of the
// blocked fixture's decision, blob, artifact, or claim seeding.
func newFailedImplementationFixture(t *testing.T, outcome execfake.Outcome, summary string) failedImplementationFixture {
	t.Helper()
	f := newSpecificationFixture(t, false, 4)
	driver := f.newDriver(t)
	if err := specifyfake.Script(driver, specificationInvocationID("specification-run", 1), 0, 0,
		specify.Output{Specification: &specify.Specification{
			Summary: "The implementation plan is ready.", Body: "# Specification\n\nImplement the bounded workflow.",
			Addressals: []specify.Addressal{},
		}}); err != nil {
		t.Fatal(err)
	}
	implementationID := productionInvocationID("implementation-run")
	driver.Script(implementationID, execfake.StageScript{
		Outcome: outcome,
		Result:  exec.StageResult{Summary: summary},
	})
	f.submit(t)
	engine := f.newEngine(t, driver)
	// Pass one accepts the auto-approved specification and submits the
	// implementation run, creating its invocation row and dispatch intent.
	if result, err := engine.Reconcile(t.Context()); err != nil || result.ResultsAccepted != 1 {
		t.Fatalf("specification reconcile = %+v, %v", result, err)
	}
	// An attended engine holds production dispatch, so admit, record, and start
	// the attempt the way dispatchIntent does; the caller's next pass collects
	// the terminal.
	var entry store.QueueEntry
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetOutbox(t.Context(), string(implementationID))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	request, err := decodeProductionRequest(entry)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := engine.loadProductionBinding(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	stage, ok := findProductionStage(binding.run)
	if !ok {
		t.Fatal("approved implementation has no production stage")
	}
	admission, admitted, err := engine.admitAttempt(t.Context(), binding, stage, implementationID)
	if err != nil || !admitted {
		t.Fatalf("admit implementation = admitted %t, %v", admitted, err)
	}
	_, effective, bound, err := engine.recordAttempt(
		t.Context(), binding.run.ID, stage.ID, implementationID, entry.Status, &admission,
	)
	if err != nil || !bound {
		t.Fatalf("record implementation attempt = bound %t, %v", bound, err)
	}
	if err := driver.Start(t.Context(), implementationID, exec.StartSpecFromAdmission(effective)); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		if err := tx.MarkOutboxDispatched(t.Context(), string(implementationID)); err != nil {
			return err
		}
		invocation := implementationID
		return tx.AppendRunMilestone(t.Context(), domain.RunMilestone{
			RunID: binding.run.ID, Kind: domain.MilestoneInvocationStarted,
			InvocationID: &invocation, RecordedAt: f.now.UTC(),
		})
	}); err != nil {
		t.Fatal(err)
	}
	run, err := f.run("implementation-run")
	if err != nil {
		t.Fatal(err)
	}
	stage, ok = findProductionStage(run)
	if !ok {
		t.Fatal("started implementation has no production stage")
	}
	if len(stage.Attempts) != 1 {
		t.Fatalf("implementation run stages = %+v", run.Stages)
	}
	return failedImplementationFixture{
		specificationFixture: f, engine: engine, driver: driver,
		run: run, attempt: stage.Attempts[0],
	}
}

func (f failedImplementationFixture) failureItemID() domain.ItemID {
	return domain.ItemID("execution-failure-" + string(f.attempt.InvocationID))
}

// preRecordOutcome writes the ExecutionOutcome the real driver records before
// the engine collects, keyed to the attempt's admission.
func (f failedImplementationFixture) preRecordOutcome(
	t *testing.T, status domain.ExecutionOutcomeStatus, summary string,
) domain.ExecutionOutcome {
	t.Helper()
	var outcome domain.ExecutionOutcome
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		admission, err := tx.GetExecutionAdmissionRecord(t.Context(), f.attempt.InvocationID)
		if err != nil {
			return err
		}
		outcome = domain.ExecutionOutcome{
			InvocationID: f.attempt.InvocationID, AdmissionID: admission.ID,
			Status: status, Summary: summary, RecordedAt: f.now.Add(time.Minute).UTC(),
		}
		return tx.RecordExecutionOutcome(t.Context(), outcome)
	}); err != nil {
		t.Fatal(err)
	}
	return outcome
}

// assertFailure checks the collected terminal, the execution_failure item and
// its facts, and that the pre-recorded outcome record is untouched.
func (f failedImplementationFixture) assertFailure(
	t *testing.T, wantTerminal exec.Status, wantOutcome domain.ExecutionOutcomeStatus, pre domain.ExecutionOutcome,
) {
	t.Helper()
	ctx := t.Context()
	item, _ := f.item(t, f.failureItemID())
	facts := item.ExecutionFailure
	if item.Type != domain.AttentionExecutionFailure || item.Subject.RunID == nil || *item.Subject.RunID != f.run.ID ||
		item.Priority != domain.PriorityHigh || item.Status != domain.StatusOpen ||
		facts == nil || facts.Outcome != wantOutcome || facts.Stage != domain.StageNameImplementation ||
		facts.InvocationID != f.attempt.InvocationID {
		t.Fatalf("execution_failure item = %#v", item)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		entry, err := tx.GetInbox(ctx, string(f.attempt.InvocationID))
		if err != nil {
			return err
		}
		var terminal productionTerminalRecord
		if err := json.Unmarshal(entry.Payload, &terminal); err != nil {
			return err
		}
		if terminal.Status != wantTerminal {
			return fmt.Errorf("terminal status = %q, want %q", terminal.Status, wantTerminal)
		}
		outcome, err := tx.GetExecutionOutcomeRecord(ctx, f.attempt.InvocationID)
		if err != nil {
			return err
		}
		if outcome.Status != pre.Status || outcome.Summary != pre.Summary ||
			outcome.AdmissionID != pre.AdmissionID || !outcome.RecordedAt.Equal(pre.RecordedAt) {
			return fmt.Errorf("outcome record = %#v, want unchanged status/summary/admission/recorded-at %#v", outcome, pre)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestProductionFailedOutcomeConverges: a driver-recorded failed outcome no
// longer suppresses collection. The engine records the terminal, raises the
// execution_failure item, and converges on the pre-recorded outcome. A replay
// and a restart change nothing.
func TestProductionFailedOutcomeConverges(t *testing.T) {
	f := newFailedImplementationFixture(t, execfake.OutcomeFail, "the implementation could not be produced")
	pre := f.preRecordOutcome(t, domain.ExecutionOutcomeFailed, "driver-recorded failure")
	if _, err := f.engine.Reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	f.assertFailure(t, exec.StatusFailed, domain.ExecutionOutcomeFailed, pre)

	// A replayed pass and a restarted engine converge without a second item or
	// a changed one; every collected terminal is re-authenticated through the
	// recorded path on each pass.
	_, before := f.item(t, f.failureItemID())
	if _, err := f.engine.Reconcile(t.Context()); err != nil {
		t.Fatalf("replayed reconcile: %v", err)
	}
	reopened := f.reopen(t)
	if _, err := reopened.newEngine(t, reopened.newDriver(t)).Reconcile(t.Context()); err != nil {
		t.Fatalf("restarted reconcile: %v", err)
	}
	f.specificationFixture = reopened
	if _, after := f.item(t, f.failureItemID()); after.EntityVersion != before.EntityVersion {
		t.Fatalf("replay advanced the item to version %d (was %d)", after.EntityVersion, before.EntityVersion)
	}
}

// TestProductionLostOutcomeConverges: a session lost before any result, with a
// pre-recorded lost outcome, records a gone terminal and a lost failure item.
func TestProductionLostOutcomeConverges(t *testing.T) {
	f := newFailedImplementationFixture(t, execfake.OutcomeCrashBeforeResult, "")
	pre := f.preRecordOutcome(t, domain.ExecutionOutcomeLost, "")
	if _, err := f.engine.Reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	f.assertFailure(t, exec.StatusGone, domain.ExecutionOutcomeLost, pre)
}

// TestProductionCanceledOutcomeConverges: a canceled stage (the daemon's own
// shutdown), with a pre-recorded canceled outcome, records a canceled terminal
// and a canceled failure item.
func TestProductionCanceledOutcomeConverges(t *testing.T) {
	f := newFailedImplementationFixture(t, execfake.OutcomeCancel, "the daemon shut down mid-stage")
	pre := f.preRecordOutcome(t, domain.ExecutionOutcomeCanceled, "driver-recorded cancellation")
	if _, err := f.engine.Reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	f.assertFailure(t, exec.StatusCanceled, domain.ExecutionOutcomeCanceled, pre)
}

// TestProductionOutcomeStatusMismatchFailsClosed: a pre-recorded canceled
// outcome under a driver that fails makes the collected terminal disagree with
// the stored outcome, so the pass fails closed and raises no item.
func TestProductionOutcomeStatusMismatchFailsClosed(t *testing.T) {
	f := newFailedImplementationFixture(t, execfake.OutcomeFail, "the implementation could not be produced")
	f.preRecordOutcome(t, domain.ExecutionOutcomeCanceled, "disagreeing pre-record")
	if _, err := f.engine.Reconcile(t.Context()); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("reconcile with a mismatched outcome = %v, want ErrParentKeyMismatch", err)
	}
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		if _, err := tx.GetAttentionItem(t.Context(), f.failureItemID()); !errors.Is(err, store.ErrNotFound) {
			return errors.New("mismatched outcome created an execution_failure item")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestProductionDeliveryRefusalReplayStaysInert: the #842 delivery refusal
// records a failed outcome and its execution_failure item without a terminal
// row. acceptProductionAttempt must skip on that pair without calling the
// driver, so a driver that knows nothing about the invocation still yields
// (false, nil) rather than an ErrUnknownInvocation collection error.
func TestProductionDeliveryRefusalReplayStaysInert(t *testing.T) {
	f := newFailedImplementationFixture(t, execfake.OutcomeFail, "unused")
	stage, ok := findProductionStage(f.run)
	if !ok {
		t.Fatal("run has no production stage")
	}
	if err := f.engine.recordProductionDeliveryRefusal(
		t.Context(), f.run, stage, f.attempt.InvocationID, "rendered prompt exceeds limit",
	); err != nil {
		t.Fatal(err)
	}
	engine := f.newEngine(t, execfake.NewStageDriver())
	accepted, err := engine.acceptProductionAttempt(t.Context(), f.run, f.attempt)
	if err != nil || accepted {
		t.Fatalf("acceptProductionAttempt after delivery refusal = accepted %t, %v; want false, nil", accepted, err)
	}
}
