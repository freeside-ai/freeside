package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// The issue-#394 scenario fixtures: an operator client following an
// unattended run through the ObserveRun read surface sees typed milestones,
// typed holds, and derivable liveness, while the observation path never
// reads live writer output and never becomes workflow authority.

func observeProductionRun(t *testing.T, st *store.Store, runID domain.RunID) domain.RunObservation {
	t.Helper()
	var observation domain.RunObservation
	if err := st.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		observation, err = tx.ObserveRun(context.Background(), runID)
		return err
	}); err != nil {
		t.Fatalf("observe run %s: %v", runID, err)
	}
	return observation
}

func milestoneKinds(o domain.RunObservation) []domain.RunMilestoneKind {
	kinds := make([]domain.RunMilestoneKind, 0, len(o.Milestones))
	for _, m := range o.Milestones {
		kinds = append(kinds, m.Kind)
	}
	return kinds
}

func assertMilestoneKinds(t *testing.T, o domain.RunObservation, want ...domain.RunMilestoneKind) {
	t.Helper()
	got := milestoneKinds(o)
	if len(got) != len(want) {
		t.Fatalf("milestone kinds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("milestone kinds = %v, want %v", got, want)
		}
	}
}

// streamRefusingDriver proves the containment boundary mechanically: the
// engine's observation flow must never read the writer's transcript, so a
// Stream call anywhere in the observed scenarios is a test failure.
type streamRefusingDriver struct {
	exec.StageDriver
	t *testing.T
}

func (d streamRefusingDriver) Stream(context.Context, domain.InvocationID) (io.ReadCloser, error) {
	d.t.Fatal("the observation path read the writer's stream")
	return nil, errors.New("unreachable")
}

// TestRunObservationTimelineForAPublishedRun is the normal-run scenario: the
// timeline crosses submission, admission, start, export, publication, and
// terminal recording in order, the final liveness is terminal, and elapsed
// time and last observation derive from the model.
func TestRunObservationTimelineForAPublishedRun(t *testing.T) {
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	p.assertReady(t)

	observation := observeProductionRun(t, p.store, p.runID)
	assertMilestoneKinds(t, observation,
		domain.MilestoneRunSubmitted,
		domain.MilestoneInvocationAdmitted,
		domain.MilestoneInvocationStarted,
		domain.MilestoneExecutionExportRecorded,
		domain.MilestonePublicationReady,
		domain.MilestoneTerminalRecorded,
	)
	if observation.Hold != nil {
		t.Errorf("published run still holds: %+v", observation.Hold)
	}
	if _, ok := observation.SubmittedAt(); !ok {
		t.Error("submission instant is not derivable")
	}
	if _, ok := observation.ConcludedAt(); !ok {
		t.Error("conclusion instant is not derivable")
	}
	if elapsed, ok := observation.Elapsed(time.Now().UTC()); !ok || elapsed < 0 {
		t.Errorf("elapsed = %v, %v; want a non-negative conclusion-frozen span", elapsed, ok)
	}
	if _, ok := observation.LastObservedAt(); !ok {
		t.Error("last observation instant is not derivable")
	}

	// A converged replay adds nothing to the timeline.
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	replayed := observeProductionRun(t, p.store, p.runID)
	if len(replayed.Milestones) != len(observation.Milestones) {
		t.Errorf("replay grew the timeline: %v", milestoneKinds(replayed))
	}
}

// TestRunObservationHeldRunShowsTypedReason is the held-run scenario: an
// operator stop surfaces as the operation_stopped code with an observation
// span, never free text, and the resume that dispatches the run clears the
// hold through forward progress.
func TestRunObservationHeldRunShowsTypedReason(t *testing.T) {
	ctx := context.Background()
	f := openUnattendedFixture(t)
	// The stop decision arrives through signet, which requires the deciding
	// device to exist and be active.
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutDevice(ctx, domain.Device{
			ID: deviceA, DisplayName: string(deviceA), Status: domain.DeviceActive,
			PairedAt: time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC),
		})
	}); err != nil {
		t.Fatalf("seed device: %v", err)
	}
	stopOperations(t, f, "stop-observe")

	spec, policy, resolved := registerSubmissionArtifacts(t, f.store, "run-prod-observe-hold")
	submitted, err := engine.SubmitProductionRun(ctx, f.store, engine.ProductionRunSpec{
		RunID: "run-prod-observe-hold", ProjectID: "proj-prod",
		SpecArtifactID: spec.ID, PolicyArtifactID: policy.ID,
		ResolvedPolicy: resolved, Publication: productionPublicationMetadata(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Keep the run executing after the resume: acceptance of a completed
	// production result needs the publication workflow, which is out of this
	// scenario's scope.
	f.driver.Script(submitted.InvocationID, fake.StageScript{
		RunningInspects: 8, Outcome: fake.OutcomeComplete,
		Result: exec.StageResult{Summary: "still running when the scenario ends"},
	})

	if _, err := f.engine.Reconcile(ctx); err != nil {
		t.Fatalf("held reconcile: %v", err)
	}
	observation := observeProductionRun(t, f.store, submitted.Run.ID)
	assertMilestoneKinds(t, observation, domain.MilestoneRunSubmitted)
	if observation.Hold == nil {
		t.Fatal("stopped run shows no hold")
	}
	if observation.Hold.Reason != domain.HoldOperationStopped {
		t.Errorf("hold reason = %s, want %s", observation.Hold.Reason, domain.HoldOperationStopped)
	}
	if observation.Hold.FirstObservedAt.IsZero() ||
		observation.Hold.LastObservedAt.Before(observation.Hold.FirstObservedAt) {
		t.Errorf("hold span = %+v", observation.Hold)
	}

	resumeOperations(t, f, "resume-observe")
	if _, err := f.engine.Reconcile(ctx); err != nil {
		t.Fatalf("resumed reconcile: %v", err)
	}
	resumed := observeProductionRun(t, f.store, submitted.Run.ID)
	if resumed.Hold != nil {
		t.Errorf("resumed run still holds: %+v", resumed.Hold)
	}
	assertMilestoneKinds(t, resumed,
		domain.MilestoneRunSubmitted,
		domain.MilestoneInvocationAdmitted,
		domain.MilestoneInvocationStarted,
	)
}

// TestRunObservationTerminalFailure is the terminal-failure scenario: a
// failed stage projects the terminal milestone with its closed status class,
// and the last observation derives as terminal liveness.
func TestRunObservationTerminalFailure(t *testing.T) {
	ctx := context.Background()
	f := openUnattendedFixture(t)
	// The containment boundary rides the whole scenario: observation must
	// never read the writer's stream.
	guarded, err := engine.New(f.store, f.signet,
		streamRefusingDriver{StageDriver: f.driver, t: t},
		unattendedProductionOptions(t)...,
	)
	if err != nil {
		t.Fatal(err)
	}

	spec, policy, resolved := registerSubmissionArtifacts(t, f.store, "run-prod-observe-fail")
	submitted, err := engine.SubmitProductionRun(ctx, f.store, engine.ProductionRunSpec{
		RunID: "run-prod-observe-fail", ProjectID: "proj-prod",
		SpecArtifactID: spec.ID, PolicyArtifactID: policy.ID,
		ResolvedPolicy: resolved, Publication: productionPublicationMetadata(),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.driver.Script(submitted.InvocationID, fake.StageScript{
		Outcome: fake.OutcomeFail,
		Result:  exec.StageResult{Summary: "The stage went sideways."},
		// A transcript is scripted so a Stream call would have bytes to leak;
		// the guard driver proves none is made.
		Transcript: []byte("provider transcript that must never be observed"),
	})
	if _, err := guarded.Reconcile(ctx); err != nil {
		t.Fatalf("dispatch reconcile: %v", err)
	}
	if _, err := guarded.Reconcile(ctx); err != nil {
		t.Fatalf("terminal reconcile: %v", err)
	}

	observation := observeProductionRun(t, f.store, submitted.Run.ID)
	assertMilestoneKinds(t, observation,
		domain.MilestoneRunSubmitted,
		domain.MilestoneInvocationAdmitted,
		domain.MilestoneInvocationStarted,
		domain.MilestoneTerminalRecorded,
	)
	terminal := observation.Milestones[len(observation.Milestones)-1]
	if terminal.Terminal == nil || *terminal.Terminal != domain.ObservedStatusFailed {
		t.Errorf("terminal milestone = %+v, want failed class", terminal)
	}
	if len(observation.Invocations) != 1 {
		t.Fatalf("invocation observations = %+v", observation.Invocations)
	}
	liveness := domain.DeriveInvocationLiveness(
		&observation.Invocations[0], time.Now().UTC(),
		domain.DefaultObservationFreshnessWindow,
	)
	if liveness != domain.LivenessTerminal {
		t.Errorf("liveness = %s, want %s", liveness, domain.LivenessTerminal)
	}
}

// TestRunObservationSurvivesRestartWithDerivableGap is the reconnect
// scenario: a daemon restart preserves the already-observed timeline, the
// stale pre-restart observation derives as an observation gap rather than a
// stale live verdict, and the next pass re-observes.
func TestRunObservationSurvivesRestartWithDerivableGap(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	f := openUnattendedFixtureAt(t, root, true)

	spec, policy, resolved := registerSubmissionArtifacts(t, f.store, "run-prod-observe-restart")
	submitted, err := engine.SubmitProductionRun(ctx, f.store, engine.ProductionRunSpec{
		RunID: "run-prod-observe-restart", ProjectID: "proj-prod",
		SpecArtifactID: spec.ID, PolicyArtifactID: policy.ID,
		ResolvedPolicy: resolved, Publication: productionPublicationMetadata(),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.driver.Script(submitted.InvocationID, fake.StageScript{
		RunningInspects: 8, Outcome: fake.OutcomeComplete,
		Result: exec.StageResult{Summary: "slow completion the restart interrupts"},
	})
	if _, err := f.engine.Reconcile(ctx); err != nil {
		t.Fatalf("dispatch reconcile: %v", err)
	}
	if _, err := f.engine.Reconcile(ctx); err != nil {
		t.Fatalf("running reconcile: %v", err)
	}

	before := observeProductionRun(t, f.store, submitted.Run.ID)
	if len(before.Invocations) != 1 || !before.Invocations[0].Live {
		t.Fatalf("pre-restart observation = %+v, want a live running one", before.Invocations)
	}
	preRestart := before.Invocations[0]
	liveness := domain.DeriveInvocationLiveness(&preRestart, preRestart.ObservedAt,
		domain.DefaultObservationFreshnessWindow)
	if liveness != domain.LivenessLive {
		t.Fatalf("pre-restart liveness = %s, want %s", liveness, domain.LivenessLive)
	}

	// Kill the daemon: reopen the store, driver, and engine over the same
	// root. The fake driver's sessions are transient, so the invocation is a
	// lost provider session, exactly like a crashed runtime.
	f.close(t)
	f = openUnattendedFixtureAt(t, root, false)

	after := observeProductionRun(t, f.store, submitted.Run.ID)
	if len(after.Milestones) != len(before.Milestones) {
		t.Errorf("restart changed the timeline: %v vs %v",
			milestoneKinds(after), milestoneKinds(before))
	}
	if len(after.Invocations) != 1 ||
		!after.Invocations[0].ObservedAt.Equal(preRestart.ObservedAt) {
		t.Fatalf("restart lost the last observation: %+v", after.Invocations)
	}
	// The preserved observation is stale knowledge, not a live verdict: past
	// the freshness window it derives as a gap.
	gapAsOf := preRestart.ObservedAt.Add(domain.DefaultObservationFreshnessWindow + time.Second)
	if got := domain.DeriveInvocationLiveness(&after.Invocations[0], gapAsOf,
		domain.DefaultObservationFreshnessWindow); got != domain.LivenessGap {
		t.Errorf("post-restart liveness = %s, want %s", got, domain.LivenessGap)
	}

	// The next pass re-observes: the restarted fake reports the session gone,
	// and the fresh observation replaces the stale one.
	if _, err := f.engine.Reconcile(ctx); err != nil {
		t.Fatalf("post-restart reconcile: %v", err)
	}
	reobserved := observeProductionRun(t, f.store, submitted.Run.ID)
	if len(reobserved.Invocations) != 1 {
		t.Fatalf("re-observed invocations = %+v", reobserved.Invocations)
	}
	obs := reobserved.Invocations[0]
	if obs.Live || !obs.ObservedAt.After(preRestart.ObservedAt) {
		t.Errorf("re-observation = %+v, want a fresh non-live one", obs)
	}
}

// TestAttendedHoldObservesEveryQueuedRun: with several production intents
// pending under an attended composition, the pass that stops on the oldest
// entry still records the typed hold for every queued run, not only the one
// that surfaced the condition.
func TestAttendedHoldObservesEveryQueuedRun(t *testing.T) {
	ctx := context.Background()
	f := openProductionFixture(t)
	for _, runID := range []string{"run-prod-attended-a", "run-prod-attended-b"} {
		spec, policy, resolved := registerSubmissionArtifacts(t, f.store, runID)
		if _, err := engine.SubmitProductionRun(ctx, f.store, engine.ProductionRunSpec{
			RunID: domain.RunID(runID), ProjectID: "proj-prod",
			SpecArtifactID: spec.ID, PolicyArtifactID: policy.ID,
			ResolvedPolicy: resolved, Publication: productionPublicationMetadata(),
		}); err != nil {
			t.Fatalf("submit %s: %v", runID, err)
		}
	}
	if _, err := f.engine.Reconcile(ctx); err != nil {
		t.Fatalf("attended reconcile: %v", err)
	}
	for _, runID := range []domain.RunID{"run-prod-attended-a", "run-prod-attended-b"} {
		observation := observeProductionRun(t, f.store, runID)
		if observation.Hold == nil || observation.Hold.Reason != domain.HoldAttendedModeActive {
			t.Errorf("run %s hold = %+v, want %s",
				runID, observation.Hold, domain.HoldAttendedModeActive)
		}
	}
}

// TestPublicationEnvironmentHoldCoversLockSetup: every environmental pause of
// a publication task states the same typed cause, not only the retryable
// reconcile failure. A work directory the daemon cannot prepare pauses the
// run exactly as a failed fetch does, so the read surface must name it
// instead of showing a run paused for no observable reason.
func TestPublicationEnvironmentHoldCoversLockSetup(t *testing.T) {
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	workDir := filepath.Join(p.workDir, "production-publication")
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A file where the lock directory belongs: MkdirAll cannot prepare it.
	lockDir := filepath.Join(workDir, "task-locks")
	if err := os.WriteFile(lockDir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := p.reconcileLanes()
	if err != nil || result != (engine.ReconcileResult{}) {
		t.Fatalf("unpreparable lock directory reconcile = %#v, %v", result, err)
	}
	observation := observeProductionRun(t, p.store, p.runID)
	if observation.Hold == nil || observation.Hold.Reason != domain.HoldPublicationEnvironment {
		t.Fatalf("lock setup hold = %+v, want %s",
			observation.Hold, domain.HoldPublicationEnvironment)
	}
	if err := os.Remove(lockDir); err != nil {
		t.Fatal(err)
	}
	p.now = p.now.Add(time.Minute)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatalf("recover publication after lock setup failure: %v", err)
	}
	p.assertReady(t)
	if observation := observeProductionRun(t, p.store, p.runID); observation.Hold != nil {
		t.Errorf("repaired publication left the hold standing: %+v", observation.Hold)
	}
}

// TestAcceptedPublicationClearsTheAttendedHold: the hold-only composition's
// pause ends when an active composition accepts the queued task, so the read
// surface must not keep reporting attended_mode_active through a whole fetch,
// verification, and publication attempt. The attempt is stopped at
// verification here, before any milestone exists, so acceptance is the only
// thing that can have cleared the hold.
func TestAcceptedPublicationClearsTheAttendedHold(t *testing.T) {
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	p.workflow = p.newEngineForMode(
		t, productionCrashSeams{}, true, nil, domain.ModeAttendedDev, true,
	)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatalf("attended publication hold: %v", err)
	}
	if observation := observeProductionRun(t, p.store, p.runID); observation.Hold == nil ||
		observation.Hold.Reason != domain.HoldAttendedModeActive {
		t.Fatalf("attended hold = %+v, want %s",
			observation.Hold, domain.HoldAttendedModeActive)
	}
	p.workflow = p.newEngine(t, productionCrashSeams{
		afterVerification: func() error { return errors.New("stop after checkpoint") },
	}, true)
	if _, err := p.reconcileLanes(); err == nil {
		t.Fatal("verification seam did not stop the accepted attempt")
	}
	observation := observeProductionRun(t, p.store, p.runID)
	if observation.Hold != nil {
		t.Fatalf("accepted publication left the attended hold standing: %+v", observation.Hold)
	}
	for _, kind := range milestoneKinds(observation) {
		if kind == domain.MilestonePublicationReady || kind == domain.MilestonePublicationBlocked {
			t.Fatalf("stopped attempt appended %s: the clear is not attributable", kind)
		}
	}
}

// TestAcceptedPublicationKeepsOtherHoldCauses: the clear at acceptance is
// cause-scoped. A hold the attempt itself will decide (an environmental
// back-off, a definitive block) keeps its row and its recorded span while the
// attempt runs, so acceptance cannot blink a live cause out or restart its
// "held since".
func TestAcceptedPublicationKeepsOtherHoldCauses(t *testing.T) {
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	p.transport.failFetch(&net.DNSError{Err: "temporary", Name: "github.com"})
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatalf("transient publication reconcile: %v", err)
	}
	held := observeProductionRun(t, p.store, p.runID)
	if held.Hold == nil || held.Hold.Reason != domain.HoldPublicationEnvironment {
		t.Fatalf("transient hold = %+v, want %s",
			held.Hold, domain.HoldPublicationEnvironment)
	}
	p.transport.failFetch(nil)
	p.now = p.now.Add(time.Minute)
	p.workflow = p.newEngine(t, productionCrashSeams{
		afterVerification: func() error { return errors.New("stop after checkpoint") },
	}, true)
	if _, err := p.reconcileLanes(); err == nil {
		t.Fatal("verification seam did not stop the accepted attempt")
	}
	observation := observeProductionRun(t, p.store, p.runID)
	if observation.Hold == nil || observation.Hold.Reason != domain.HoldPublicationEnvironment ||
		!observation.Hold.FirstObservedAt.Equal(held.Hold.FirstObservedAt) {
		t.Fatalf("accepted publication disturbed another cause: %+v, want %+v",
			observation.Hold, held.Hold)
	}
}

// TestSubmissionReplayDoesNotBackfillTheMilestone pins migration 0024's
// no-backfill rule at the one write that could violate it: a replayed
// idempotent submission against a run persisted before the migration (here
// simulated by deleting the creation's milestone) must not mint a
// run_submitted instant that was never observed — SubmittedAt and Elapsed
// would silently describe the replay instead of the run.
func TestSubmissionReplayDoesNotBackfillTheMilestone(t *testing.T) {
	ctx := context.Background()
	f := openUnattendedFixture(t)
	spec, policy, resolved := registerSubmissionArtifacts(t, f.store, "run-prod-observe-backfill")
	runSpec := engine.ProductionRunSpec{
		RunID: "run-prod-observe-backfill", ProjectID: "proj-prod",
		SpecArtifactID: spec.ID, PolicyArtifactID: policy.ID,
		ResolvedPolicy: resolved, Publication: productionPublicationMetadata(),
	}
	submitted, err := engine.SubmitProductionRun(ctx, f.store, runSpec)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate the pre-0024 shape: the run exists with no observed
	// submission instant. The store package registers the driver.
	db, err := sql.Open("sqlite", filepath.Join(f.root, "freeside.db"))
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM run_milestones WHERE run_id = ?`, string(submitted.Run.ID)); err != nil {
		t.Fatalf("erase milestone: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	if _, err := engine.SubmitProductionRun(ctx, f.store, runSpec); err != nil {
		t.Fatalf("replay submission: %v", err)
	}
	observation := observeProductionRun(t, f.store, submitted.Run.ID)
	for _, m := range observation.Milestones {
		if m.Kind == domain.MilestoneRunSubmitted {
			t.Errorf("replay backfilled a submission instant: %+v", m)
		}
	}
}

// malformedInspectionDriver claims a lost session is live: the impossible
// pair a buggy or hostile driver could hand back.
type malformedInspectionDriver struct {
	exec.StageDriver
}

func (d malformedInspectionDriver) Inspect(context.Context, domain.InvocationID) (exec.Inspection, error) {
	return exec.Inspection{Status: exec.StatusGone, Live: true}, nil
}

// TestMalformedInspectionFailsClosed: the inspection is a returned object,
// so a driver reporting a lost session as live fails the boundary loudly
// instead of projecting observed_live for a session nothing observes.
func TestMalformedInspectionFailsClosed(t *testing.T) {
	ctx := context.Background()
	f := openUnattendedFixture(t)
	spec, policy, resolved := registerSubmissionArtifacts(t, f.store, "run-prod-observe-malformed")
	submitted, err := engine.SubmitProductionRun(ctx, f.store, engine.ProductionRunSpec{
		RunID: "run-prod-observe-malformed", ProjectID: "proj-prod",
		SpecArtifactID: spec.ID, PolicyArtifactID: policy.ID,
		ResolvedPolicy: resolved, Publication: productionPublicationMetadata(),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.driver.Script(submitted.InvocationID, fake.StageScript{
		RunningInspects: 8, Outcome: fake.OutcomeComplete,
		Result: exec.StageResult{Summary: "never reaches acceptance"},
	})
	if _, err := f.engine.Reconcile(ctx); err != nil {
		t.Fatalf("dispatch reconcile: %v", err)
	}

	lying, err := engine.New(f.store, f.signet,
		malformedInspectionDriver{StageDriver: f.driver},
		unattendedProductionOptions(t)...,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lying.Reconcile(ctx); !errors.Is(err, exec.ErrInvalidStatus) {
		t.Fatalf("malformed inspection reconcile = %v, want %v", err, exec.ErrInvalidStatus)
	}
	// The impossible pair was never projected: the honest pass's running
	// observation stands, and nothing recorded the lying driver's
	// gone-but-live answer.
	observation := observeProductionRun(t, f.store, submitted.Run.ID)
	for _, obs := range observation.Invocations {
		if obs.Status == domain.ObservedStatusGone {
			t.Errorf("malformed inspection was projected: %+v", obs)
		}
	}
}

// TestForgedMilestonesDriveNoWorkflowDecision is the trust boundary: the
// observation projection is never workflow authority. Milestones claiming
// the run exported, published, and concluded change nothing about what the
// engine does next — recovery re-observes the driver and the durable
// records, and the run still dispatches, executes, and records its real
// terminal exactly as without the forgery.
func TestForgedMilestonesDriveNoWorkflowDecision(t *testing.T) {
	ctx := context.Background()
	f := openUnattendedFixture(t)

	spec, policy, resolved := registerSubmissionArtifacts(t, f.store, "run-prod-observe-forged")
	submitted, err := engine.SubmitProductionRun(ctx, f.store, engine.ProductionRunSpec{
		RunID: "run-prod-observe-forged", ProjectID: "proj-prod",
		SpecArtifactID: spec.ID, PolicyArtifactID: policy.ID,
		ResolvedPolicy: resolved, Publication: productionPublicationMetadata(),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.driver.Script(submitted.InvocationID, fake.StageScript{
		Outcome: fake.OutcomeFail,
		Result:  exec.StageResult{Summary: "the real outcome"},
	})

	// Forge a fully successful timeline before anything ran, plus an
	// observation binding the invocation to a foreign run — the refute pass
	// demonstrated that trusting the stored binding wedged the reconcile
	// loop, so this row must be repaired, never believed.
	invocation := submitted.InvocationID
	if err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		for _, kind := range []domain.RunMilestoneKind{
			domain.MilestoneInvocationAdmitted,
			domain.MilestoneInvocationStarted,
			domain.MilestoneExecutionExportRecorded,
			domain.MilestonePublicationReady,
		} {
			if err := tx.AppendRunMilestone(ctx, domain.RunMilestone{
				RunID: submitted.Run.ID, Kind: kind,
				InvocationID: &invocation,
				RecordedAt:   time.Now().UTC(),
			}); err != nil {
				return err
			}
		}
		return tx.RecordInvocationObservation(ctx, domain.InvocationObservation{
			InvocationID: submitted.InvocationID, RunID: "run-someone-else",
			Status: domain.ObservedStatusRunning, Live: true,
			ObservedAt: time.Now().UTC(),
		})
	}); err != nil {
		t.Fatalf("forge observation rows: %v", err)
	}

	// The engine still dispatches and collects the real failed outcome; the
	// forged "publication_ready" neither skips work nor publishes anything.
	if _, err := f.engine.Reconcile(ctx); err != nil {
		t.Fatalf("dispatch reconcile: %v", err)
	}
	if _, err := f.engine.Reconcile(ctx); err != nil {
		t.Fatalf("terminal reconcile: %v", err)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetExecutionExport(ctx, submitted.InvocationID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("forged export milestone conjured an export read: %v", err)
		}
		if _, err := tx.GetAttentionItem(ctx,
			domain.ItemID("production-ready-"+string(submitted.Run.ID))); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("forged ready milestone conjured a ready item: %v", err)
		}
		item, err := tx.GetAttentionItem(ctx,
			domain.ItemID("execution-failure-"+string(submitted.InvocationID)))
		if err != nil {
			return err
		}
		if item.Type != domain.AttentionExecutionFailure {
			t.Errorf("real failure item = %q", item.Type)
		}
		// The engine's own observation repaired the forged foreign binding.
		observation, err := tx.ObserveRun(ctx, submitted.Run.ID)
		if err != nil {
			return err
		}
		if len(observation.Invocations) != 1 ||
			observation.Invocations[0].RunID != submitted.Run.ID {
			t.Errorf("forged binding was not repaired: %+v", observation.Invocations)
		}
		return nil
	}); err != nil {
		t.Fatalf("read real outcome: %v", err)
	}
}
