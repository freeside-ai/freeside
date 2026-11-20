package stage

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// capLossCheck is the F42 shape (#3440): export verification refused the
// handoff over the entry cap, which the content itself decides.
func capLossCheck() *domain.HandoffCheckFailure {
	return &domain.HandoffCheckFailure{
		Check:           domain.WardCheckExportVerification,
		Evidence:        "export has 10001 entries, over the 10000 entry cap",
		ContentEvidence: true,
	}
}

// saveRunningOrphan saves a running intent with no live pipeline, as a
// daemon restart leaves one behind.
func saveRunningOrphan(t *testing.T, d *Driver) intent {
	t.Helper()
	spec := testStartSpec()
	inputs := stageInputs(t, &spec)
	instructions, _ := ward.VendorInstructionsFromStageInputs(inputs)
	prompt, _ := renderPrompt(inputs)
	in := intent{
		InvocationID: testInvoke, RunID: testRunIDFor(testInvoke), Phase: phaseRunning,
		Spec: spec, Seed: filepath.Join(d.seedRoot, testRunIDFor(testInvoke)),
		Prompt: prompt, PromptDelivery: PromptFileV8, Inputs: durableInputsFrom(inputs),
		Instructions: instructions, RecordedAt: fixedNow, CommitDate: fixedNow,
	}
	if err := d.saveIntent(in); err != nil {
		t.Fatalf("save orphan intent: %v", err)
	}
	return in
}

func lossGate(check *domain.HandoffCheckFailure) *stubGate {
	return &stubGate{recoverFn: func(string, ward.HandoffSpec) (*ward.RecoveryResult, error) {
		return &ward.RecoveryResult{Outcome: ward.RecoveryLoss, LossCheck: check}, nil
	}}
}

// TestLiveHandoffCheckNamesTheLaterLoss covers F42 on the live path: a
// handoff check refuses the run while it is still running, which commits
// nothing, and the loss recovery later proves names that check.
func TestLiveHandoffCheckNamesTheLaterLoss(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	exports := newStubExports()
	d := newTestDriver(t, lossGate(nil), exports)
	saveRunningOrphan(t, d)

	d.mu.Lock()
	committed, err := d.commitResultLocked(testInvoke, exec.StageResult{
		Status: exec.StatusFailed, Summary: "handoff refused",
		Cause: domain.FailureCauseHandoffCheck, CheckFailure: capLossCheck(),
	})
	d.mu.Unlock()
	if err != nil || committed {
		t.Fatalf("commit running failure = %t, %v; want held for recovery", committed, err)
	}
	if _, ok := exports.outcomes[testInvoke]; ok {
		t.Fatal("a running handoff failure recorded an outcome before recovery")
	}

	if _, err := inspectRecovered(t, ctx, d, testInvoke); err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	reconcileClean(t, ctx, d)
	outcome, ok := exports.outcomes[testInvoke]
	if !ok || outcome.Status != domain.ExecutionOutcomeLost || !outcome.CheckFailure.Equal(capLossCheck()) {
		t.Fatalf("outcome = %+v (present=%t), want lost naming the cap check", outcome, ok)
	}
}

// TestRecoveryLossNamesWardCheck covers F42 on the recovery path: ward
// recovery proves the loss and names the check that forced it.
func TestRecoveryLossNamesWardCheck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	exports := newStubExports()
	d := newTestDriver(t, lossGate(capLossCheck()), exports)
	saveRunningOrphan(t, d)

	if _, err := inspectRecovered(t, ctx, d, testInvoke); err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	reconcileClean(t, ctx, d)
	outcome, ok := exports.outcomes[testInvoke]
	if !ok || outcome.Status != domain.ExecutionOutcomeLost || !outcome.CheckFailure.Equal(capLossCheck()) {
		t.Fatalf("outcome = %+v (present=%t), want lost naming the cap check", outcome, ok)
	}
}

// TestLossReplayConvergesWithoutItsCheck: a crash after the lost outcome
// committed but before the intent advanced replays recovery against a
// journal ward already closed, which names no check. The replay converges
// on the stored outcome instead of wedging the invocation.
func TestLossReplayConvergesWithoutItsCheck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	exports := newStubExports()
	d := newTestDriver(t, lossGate(nil), exports)
	in := saveRunningOrphan(t, d)
	stored := domain.ExecutionOutcome{
		InvocationID: testInvoke, AdmissionID: in.Spec.AdmissionID,
		Status: domain.ExecutionOutcomeLost, CheckFailure: capLossCheck(), RecordedAt: fixedNow,
	}
	exports.outcomes[testInvoke] = stored

	if _, err := inspectRecovered(t, ctx, d, testInvoke); err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	reconcileClean(t, ctx, d)
	if outcome := exports.outcomes[testInvoke]; !outcome.CheckFailure.Equal(stored.CheckFailure) {
		t.Fatalf("replayed loss rewrote the stored check: %+v", outcome.CheckFailure)
	}
	reloaded, err := d.loadIntent(ctx, testInvoke)
	if err != nil || reloaded.Phase != phaseLost {
		t.Fatalf("intent after replay = %q, %v; want lost", reloaded.Phase, err)
	}
}

// handoffCheckFailedOutcome is a stored failed outcome whose cause names a
// handoff check, as recovery records one.
func handoffCheckFailedOutcome(admission domain.Digest) domain.ExecutionOutcome {
	return domain.ExecutionOutcome{
		InvocationID: testInvoke, AdmissionID: admission,
		Status: domain.ExecutionOutcomeFailed, Summary: "handoff refused",
		Cause: domain.FailureCauseHandoffCheck, CheckFailure: capLossCheck(), RecordedAt: fixedNow,
	}
}

// TestRestoredHandoffCheckFailureKeepsItsCheck: a crash after a failed
// handoff_check outcome committed but before the intent advanced rebuilds
// the result from the stored outcome. The rebuilt result carries the check,
// which a handoff_check cause requires, so the intent saves and the
// invocation does not wedge.
func TestRestoredHandoffCheckFailureKeepsItsCheck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	exports := newStubExports()
	d := newTestDriver(t, &stubGate{}, exports)
	in := saveRunningOrphan(t, d)
	exports.outcomes[testInvoke] = handoffCheckFailedOutcome(in.Spec.AdmissionID)

	restored, ok, err := d.restoreDurableOutcome(ctx, in)
	if err != nil || !ok || restored.Result == nil {
		t.Fatalf("restore = %+v, %t, %v", restored, ok, err)
	}
	if !restored.Result.CheckFailure.Equal(capLossCheck()) {
		t.Fatalf("restored check = %+v, want the stored one", restored.Result.CheckFailure)
	}
	if err := d.saveIntent(restored); err != nil {
		t.Fatalf("save restored intent: %v", err)
	}
}

// TestAdoptedHandoffCheckFailureKeepsItsCheck is the export-recovery twin:
// adopting an already-written handoff_check failure commits a result that
// still names the check.
func TestAdoptedHandoffCheckFailureKeepsItsCheck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newTestDriver(t, &stubGate{}, newStubExports())
	in := orphan(t, d, phaseExported, &releasedExport{
		Dir: filepath.Join(os.TempDir(),
			"freeside-handoff-"+testRunIDFor(testInvoke)+"-out-gone-adopt-check"),
		Manifest:        export.Manifest{Version: export.ManifestVersion, Entries: []export.Entry{}},
		ObservedBaseSHA: testBase.BaseSHA,
	})
	if err := d.adoptRecordedOutcome(ctx, in, handoffCheckFailedOutcome(in.Spec.AdmissionID)); err != nil {
		t.Fatalf("adoptRecordedOutcome: %v", err)
	}
	adopted, err := d.loadIntent(ctx, testInvoke)
	if err != nil || adopted.Result == nil || !adopted.Result.CheckFailure.Equal(capLossCheck()) {
		t.Fatalf("adopted intent = %+v (err=%v), want the check kept", adopted, err)
	}
}
