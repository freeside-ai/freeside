package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	execfake "github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
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
	return newFailedImplementationFixtureScript(t, execfake.StageScript{
		Outcome: outcome,
		Result:  exec.StageResult{Summary: summary},
	})
}

// newFailedImplementationFixtureScript is newFailedImplementationFixture with
// the implementation invocation's full driver script, so a caller can add
// RunningInspects to model the driver reporting running before the terminal.
func newFailedImplementationFixtureScript(t *testing.T, script execfake.StageScript) failedImplementationFixture {
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
	driver.Script(implementationID, script)
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

// activeExecutionCount reads the identity's active execution count.
func (f failedImplementationFixture) activeExecutionCount(t *testing.T, id domain.AuthIdentityID) int {
	t.Helper()
	var got int
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		got, err = tx.ActiveIdentityExecutionCount(t.Context(), id)
		return err
	}); err != nil {
		t.Fatalf("ActiveIdentityExecutionCount: %v", err)
	}
	return got
}

// invocationObservation reads the current mirrored observation for the run's
// implementation invocation. The mirror is upsert-per-invocation, so it holds
// the latest state, which is what the sequential timeline checks compare.
func (f failedImplementationFixture) invocationObservation(t *testing.T) domain.ObservedInvocationStatus {
	t.Helper()
	var status domain.ObservedInvocationStatus
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		observations, err := tx.ListInvocationObservations(t.Context(), f.run.ID)
		if err != nil {
			return err
		}
		for _, o := range observations {
			if o.InvocationID == f.attempt.InvocationID {
				status = o.Status
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("ListInvocationObservations: %v", err)
	}
	return status
}

// outboxDispatched reports whether the implementation invocation's outbox row
// is still dispatched.
func (f failedImplementationFixture) outboxDispatched(t *testing.T) bool {
	t.Helper()
	var dispatched bool
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		entry, err := tx.GetOutbox(t.Context(), string(f.attempt.InvocationID))
		if err != nil {
			return err
		}
		dispatched = entry.Dispatched()
		return nil
	}); err != nil {
		t.Fatalf("GetOutbox: %v", err)
	}
	return dispatched
}

// TestStrandedFailureConvergesWithVisibleTimeline is the engine half of the
// stranded-dispatch fix (issue #1181): once the driver reports running and then
// a failed terminal (what the stage driver's bounded recovery grace produces),
// the engine raises the execution_failure card offering discuss and stop,
// frees the identity's execution slot, and leaves a coherent timeline. The
// observation mirror is upsert-per-invocation, so the running-then-failed
// timeline is proved by reading it between passes rather than as a stored list.
func TestStrandedFailureConvergesWithVisibleTimeline(t *testing.T) {
	f := newFailedImplementationFixtureScript(t, execfake.StageScript{
		RunningInspects: 1,
		Outcome:         execfake.OutcomeFail,
		Result:          exec.StageResult{Summary: "recovery of the released export failed after grace"},
	})
	identity := domain.AuthIdentityID("auth-1")

	// While the driver still reports running the slot is held and no card exists.
	if _, err := f.engine.Reconcile(t.Context()); err != nil {
		t.Fatalf("running-pass reconcile: %v", err)
	}
	if got := f.invocationObservation(t); got != domain.ObservedStatusRunning {
		t.Fatalf("observation during running pass = %q, want running", got)
	}
	runningCount := f.activeExecutionCount(t, identity)
	if runningCount < 1 {
		t.Fatalf("active execution count while running = %d, want the attempt counted", runningCount)
	}
	if !f.outboxDispatched(t) {
		t.Fatal("outbox row is not dispatched during the running pass")
	}
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		if _, err := tx.GetAttentionItem(t.Context(), f.failureItemID()); !errors.Is(err, store.ErrNotFound) {
			return errors.New("execution_failure card raised before the terminal")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// The failed terminal converges: the card is raised, the slot is freed, the
	// outbox row is untouched, and the observation mirror shows the failure.
	if _, err := f.engine.Reconcile(t.Context()); err != nil {
		t.Fatalf("terminal-pass reconcile: %v", err)
	}
	item, _ := f.item(t, f.failureItemID())
	if item.Type != domain.AttentionExecutionFailure || item.Status != domain.StatusOpen {
		t.Fatalf("execution_failure item = %#v", item)
	}
	if !slices.Contains(item.RequestedDecision, domain.ActionDiscuss) ||
		!slices.Contains(item.RequestedDecision, domain.ActionStop) {
		t.Fatalf("requested decision = %v, want discuss and stop", item.RequestedDecision)
	}
	// The failed outcome frees exactly this attempt's slot. The count is a
	// delta because the specification invocation on the same identity holds a
	// slot of its own; the store test pins the absolute 1-to-0 transition.
	if got := f.activeExecutionCount(t, identity); got != runningCount-1 {
		t.Fatalf("active execution count after failure = %d, want %d (one slot freed)", got, runningCount-1)
	}
	if got := f.invocationObservation(t); got != domain.ObservedStatusFailed {
		t.Fatalf("observation after failure = %q, want failed", got)
	}
	if !f.outboxDispatched(t) {
		t.Fatal("failure convergence changed the outbox row away from dispatched")
	}
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		milestones, err := tx.ListRunMilestones(t.Context(), f.run.ID)
		if err != nil {
			return err
		}
		// invocation_admitted is a defined-but-unrecorded milestone kind in the
		// current engine, so the timeline's recorded start marker is
		// invocation_started (the issue contract's admitted marker has no writer
		// yet; recorded here as the mismatch surfaced in the PR).
		started := false
		for _, m := range milestones {
			if m.Kind == domain.MilestoneInvocationStarted && m.InvocationID != nil &&
				*m.InvocationID == f.attempt.InvocationID {
				started = true
			}
		}
		if !started {
			return fmt.Errorf("milestones %+v lack invocation_started for the attempt", milestones)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// inspectErrorDriver is a StageDriver whose Inspect always fails; only Inspect
// is exercised on the acceptProductionAttempt refusal path.
type inspectErrorDriver struct {
	exec.StageDriver
	err error
}

func (d inspectErrorDriver) Inspect(context.Context, domain.InvocationID) (exec.Inspection, error) {
	return exec.Inspection{}, d.err
}

// TestPolicyRefusalRecordsRunHold covers the second silent path the fix closes
// (issue #1181): when acceptProductionAttempt skips an attempt on a mutable
// admission-policy refusal, it now records a run hold with the classified
// reason instead of skipping silently.
func TestPolicyRefusalRecordsRunHold(t *testing.T) {
	f := newFailedImplementationFixture(t, execfake.OutcomeFail, "unused")
	driver := inspectErrorDriver{
		err: fmt.Errorf("inspect: %w", store.ErrBackendNotConformant),
	}
	engine := f.newEngine(t, driver)

	accepted, err := engine.acceptProductionAttempt(t.Context(), f.run, f.attempt)
	if err != nil || accepted {
		t.Fatalf("acceptProductionAttempt on a policy refusal = accepted %t, %v; want false, nil", accepted, err)
	}
	var (
		hold  domain.RunHoldObservation
		found bool
	)
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		hold, found, err = tx.GetRunHold(t.Context(), f.run.ID)
		return err
	}); err != nil {
		t.Fatalf("GetRunHold: %v", err)
	}
	if !found || hold.Reason != domain.HoldBackendNotConformant {
		t.Fatalf("run hold = (%+v, found %t), want HoldBackendNotConformant", hold, found)
	}
}

// switchableInspectDriver is an inspectErrorDriver whose Inspect error can be
// switched off between passes on one engine: with an error set it refuses the
// way a mutable-policy refusal surfaces from collectTerminal; once cleared it
// reports the attempt still running, the healthy inspection acceptProductionAttempt
// clears a stale refusal hold on. The tests drive it single-threaded, so it
// needs no lock.
type switchableInspectDriver struct {
	exec.StageDriver
	err error
}

func (d *switchableInspectDriver) Inspect(context.Context, domain.InvocationID) (exec.Inspection, error) {
	if d.err != nil {
		return exec.Inspection{}, d.err
	}
	return exec.Inspection{Status: exec.StatusRunning, Live: true}, nil
}

// runHold reads the run's current hold observation.
func (f failedImplementationFixture) runHold(t *testing.T) (domain.RunHoldObservation, bool) {
	t.Helper()
	var (
		hold  domain.RunHoldObservation
		found bool
	)
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		hold, found, err = tx.GetRunHold(t.Context(), f.run.ID)
		return err
	}); err != nil {
		t.Fatalf("GetRunHold: %v", err)
	}
	return hold, found
}

// TestPolicyRefusalRecoveryClearsRunHold: a transient mutable-policy refusal
// records a hold; the next acceptance pass that inspects a still-running
// attempt (the policy having recovered) clears it, so the operator sees no hold
// instead of the stale reason (issue #1194). One engine sees both passes, so
// the healthy pass really clears what the refusing pass recorded.
func TestPolicyRefusalRecoveryClearsRunHold(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		reason domain.RunHoldReason
	}{
		{"backend not conformant", store.ErrBackendNotConformant, domain.HoldBackendNotConformant},
		{"admission policy refused", domain.ErrCapabilityBelowFloor, domain.HoldAdmissionPolicyRefused},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFailedImplementationFixture(t, execfake.OutcomeFail, "unused")
			driver := &switchableInspectDriver{err: fmt.Errorf("inspect: %w", tc.err)}
			engine := f.newEngine(t, driver)

			// Pass one refuses and records the classified hold.
			accepted, err := engine.acceptProductionAttempt(t.Context(), f.run, f.attempt)
			if err != nil || accepted {
				t.Fatalf("refusing pass = accepted %t, %v; want false, nil", accepted, err)
			}
			if hold, found := f.runHold(t); !found || hold.Reason != tc.reason {
				t.Fatalf("hold after refusal = (%+v, found %t), want %s", hold, found, tc.reason)
			}

			// Pass two inspects a still-running attempt (the policy recovered)
			// and clears the hold.
			driver.err = nil
			accepted, err = engine.acceptProductionAttempt(t.Context(), f.run, f.attempt)
			if err != nil || accepted {
				t.Fatalf("healthy pass = accepted %t, %v; want false, nil", accepted, err)
			}
			if hold, found := f.runHold(t); found {
				t.Fatalf("hold after recovery = %+v, want no hold", hold)
			}
		})
	}
}

// TestPolicyRefusalRecoveryPreservesOtherHold: the class-scoped clear leaves a
// hold outside the refusal class untouched, keeping its reason and its span. A
// dispatch capacity hold recorded between the refusal and the healthy pass
// survives with its first-observed instant.
func TestPolicyRefusalRecoveryPreservesOtherHold(t *testing.T) {
	f := newFailedImplementationFixture(t, execfake.OutcomeFail, "unused")
	driver := &switchableInspectDriver{err: fmt.Errorf("inspect: %w", store.ErrBackendNotConformant)}
	engine := f.newEngine(t, driver)

	if accepted, err := engine.acceptProductionAttempt(t.Context(), f.run, f.attempt); err != nil || accepted {
		t.Fatalf("refusing pass = accepted %t, %v; want false, nil", accepted, err)
	}

	// A different lane replaces the hold with a capacity hold outside the
	// refusal class. Record it directly with a distinct first-observed instant.
	otherFirst := f.now.Add(2 * time.Minute).UTC()
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.RecordRunHold(t.Context(), domain.RunHoldObservation{
			RunID: f.run.ID, Reason: domain.HoldIdentityParallelism,
			FirstObservedAt: otherFirst, LastObservedAt: otherFirst,
		})
	}); err != nil {
		t.Fatalf("record other hold: %v", err)
	}

	driver.err = nil
	if accepted, err := engine.acceptProductionAttempt(t.Context(), f.run, f.attempt); err != nil || accepted {
		t.Fatalf("healthy pass = accepted %t, %v; want false, nil", accepted, err)
	}

	hold, found := f.runHold(t)
	if !found || hold.Reason != domain.HoldIdentityParallelism || !hold.FirstObservedAt.Equal(otherFirst) {
		t.Fatalf("hold after recovery = (%+v, found %t), want HoldIdentityParallelism first-observed %s",
			hold, found, otherFirst)
	}
}

// TestPolicyRefusalAfterRecoveryIsRecorded: a refusal that returns right after
// a recovery is re-recorded on the same engine. The sentinel pace state the
// clear stamps on the hold key must not suppress the next real reason.
func TestPolicyRefusalAfterRecoveryIsRecorded(t *testing.T) {
	f := newFailedImplementationFixture(t, execfake.OutcomeFail, "unused")
	driver := &switchableInspectDriver{err: fmt.Errorf("inspect: %w", store.ErrBackendNotConformant)}
	engine := f.newEngine(t, driver)

	// Refuse, recover, refuse again.
	if _, err := engine.acceptProductionAttempt(t.Context(), f.run, f.attempt); err != nil {
		t.Fatalf("refusing pass: %v", err)
	}
	driver.err = nil
	if _, err := engine.acceptProductionAttempt(t.Context(), f.run, f.attempt); err != nil {
		t.Fatalf("healthy pass: %v", err)
	}
	if _, found := f.runHold(t); found {
		t.Fatal("hold not cleared on the healthy pass")
	}
	driver.err = fmt.Errorf("inspect: %w", store.ErrBackendNotConformant)
	if _, err := engine.acceptProductionAttempt(t.Context(), f.run, f.attempt); err != nil {
		t.Fatalf("second refusing pass: %v", err)
	}
	if hold, found := f.runHold(t); !found || hold.Reason != domain.HoldBackendNotConformant {
		t.Fatalf("hold after second refusal = (%+v, found %t), want HoldBackendNotConformant", hold, found)
	}
}

// TestPolicyRefusalRecoveryClearsRunHoldOnTerminal: a stale refusal hold is
// cleared when the recovering attempt reaches a terminal, not only when it is
// still running. The terminal's milestone clears the run hold inside the
// recording transaction, so acceptProductionAttempt deliberately skips its own
// clearRefusalHold write on this path; recording the terminal must still leave
// the run with no hold (issue #1194). Skipping that redundant write is what
// keeps a transient clear failure from surfacing as an error that would strand
// a completion the recording transaction already committed.
func TestPolicyRefusalRecoveryClearsRunHoldOnTerminal(t *testing.T) {
	f := newFailedImplementationFixture(t, execfake.OutcomeFail, "the implementation could not be produced")

	// An earlier transient refusal left a classified hold on the run.
	first := f.now.Add(time.Minute).UTC()
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.RecordRunHold(t.Context(), domain.RunHoldObservation{
			RunID: f.run.ID, Reason: domain.HoldBackendNotConformant,
			FirstObservedAt: first, LastObservedAt: first,
		})
	}); err != nil {
		t.Fatalf("seed refusal hold: %v", err)
	}

	// The attempt recovers and reaches a failed terminal; recording it clears
	// the hold through the terminal milestone.
	f.preRecordOutcome(t, domain.ExecutionOutcomeFailed, "driver-recorded failure")
	if _, err := f.engine.Reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if hold, found := f.runHold(t); found {
		t.Fatalf("hold after terminal = %+v, want no hold", hold)
	}
}

// TestProductionRefusalHoldReasonsPinned pins productionRefusalHoldReasons to
// the two functions it summarizes: for every sentinel MutableAdmissionPolicyRefusal
// accepts, dispatchHoldReason maps it to a reason in the set, and every reason
// in the set is reached by at least one sentinel. It fails when either function
// gains a member the other, or this list, does not know.
func TestProductionRefusalHoldReasonsPinned(t *testing.T) {
	// Every sentinel MutableAdmissionPolicyRefusal accepts (invocation.go).
	sentinels := []error{
		store.ErrBackendNotConformant,
		domain.ErrConformanceConfigurationUnbound,
		domain.ErrAdmissionConfigurationMismatch,
		domain.ErrAdmissionExceedsConformance,
		domain.ErrUnknownAdmissionFloor,
		domain.ErrCapabilityBelowFloor,
		domain.ErrCredentialModeNotApproved,
		domain.ErrWaiverNotConfigured,
		domain.ErrBackupHealthUnavailable,
		domain.ErrCheckpointNotEncrypted,
		domain.ErrCheckpointNotCurrent,
		domain.ErrArtifactClosureIncomplete,
		domain.ErrRestoreTestStale,
		domain.ErrInvalidBackupHealthStatus,
		store.ErrRepositoryUntrusted,
		publish.ErrJanitorInactive,
		domain.ErrRepositoryIdentityMismatch,
		domain.ErrPathBoundaryMismatch,
		domain.ErrTrustProfileSuperseded,
		domain.ErrReviewConfigurationUnapproved,
	}
	reached := make(map[domain.RunHoldReason]bool)
	for _, sentinel := range sentinels {
		if !MutableAdmissionPolicyRefusal(sentinel) {
			t.Errorf("sentinel %v is no longer a mutable admission-policy refusal", sentinel)
			continue
		}
		reason, ok := dispatchHoldReason(sentinel)
		if !ok {
			t.Errorf("sentinel %v classifies onto no hold reason", sentinel)
			continue
		}
		if !slices.Contains(productionRefusalHoldReasons, reason) {
			t.Errorf("sentinel %v maps to %s, outside productionRefusalHoldReasons", sentinel, reason)
		}
		reached[reason] = true
	}
	for _, reason := range productionRefusalHoldReasons {
		if !reached[reason] {
			t.Errorf("productionRefusalHoldReasons has %s, which no listed sentinel reaches", reason)
		}
	}
}
