package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func appendMilestone(t *testing.T, s *store.Store, m domain.RunMilestone) {
	t.Helper()
	if err := s.Write(context.Background(), func(tx *store.WriteTx) error {
		return tx.AppendRunMilestone(context.Background(), m)
	}); err != nil {
		t.Fatalf("append milestone %s: %v", m.Kind, err)
	}
}

func observeRun(t *testing.T, s *store.Store, runID domain.RunID) domain.RunObservation {
	t.Helper()
	var observation domain.RunObservation
	if err := s.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		observation, err = tx.ObserveRun(context.Background(), runID)
		return err
	}); err != nil {
		t.Fatalf("observe run %s: %v", runID, err)
	}
	return observation
}

// TestRunObservationRoundTrip: milestones, an invocation observation, and a
// hold come back validated and in append order through the ObserveRun read
// surface an operator client consumes.
func TestRunObservationRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAdmissionFixture(t, nil)
	s := openWithFixture(t, f, store.Options{AdmissionFloors: attendedFloors()})
	inv := domain.InvocationID("inv-1")

	appendMilestone(t, s, domain.RunMilestone{
		RunID: f.run.ID, Kind: domain.MilestoneRunSubmitted,
		InvocationID: &inv, RecordedAt: admissionEpoch,
	})
	appendMilestone(t, s, domain.RunMilestone{
		RunID: f.run.ID, Kind: domain.MilestoneInvocationStarted,
		InvocationID: &inv, RecordedAt: admissionEpoch.Add(time.Minute),
	})
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.RecordInvocationObservation(ctx, domain.InvocationObservation{
			InvocationID: inv, RunID: f.run.ID,
			Status: domain.ObservedStatusRunning, Live: true,
			ObservedAt: admissionEpoch.Add(2 * time.Minute),
		})
	}); err != nil {
		t.Fatalf("record observation: %v", err)
	}

	got := observeRun(t, s, f.run.ID)
	if len(got.Milestones) != 2 {
		t.Fatalf("milestones = %d, want 2", len(got.Milestones))
	}
	if got.Milestones[0].Kind != domain.MilestoneRunSubmitted ||
		got.Milestones[1].Kind != domain.MilestoneInvocationStarted {
		t.Errorf("milestone order = %s, %s", got.Milestones[0].Kind, got.Milestones[1].Kind)
	}
	if got.Hold != nil {
		t.Errorf("hold = %+v, want nil", got.Hold)
	}
	if len(got.Invocations) != 1 || !got.Invocations[0].Live ||
		got.Invocations[0].Status != domain.ObservedStatusRunning {
		t.Errorf("invocations = %+v", got.Invocations)
	}

	// An unknown run has an empty, valid observation: nothing observed is a
	// legitimate state, not an error.
	empty := observeRun(t, s, "run-unknown")
	if len(empty.Milestones) != 0 || empty.Hold != nil || len(empty.Invocations) != 0 {
		t.Errorf("unknown run observation = %+v, want empty", empty)
	}
}

// TestRunMilestoneFirstObservationWins: a replayed append with a different
// instant converges on the recorded row instead of duplicating or erroring.
func TestRunMilestoneFirstObservationWins(t *testing.T) {
	t.Parallel()
	f := newAdmissionFixture(t, nil)
	s := openWithFixture(t, f, store.Options{AdmissionFloors: attendedFloors()})
	inv := domain.InvocationID("inv-1")

	first := domain.RunMilestone{
		RunID: f.run.ID, Kind: domain.MilestoneRunSubmitted,
		InvocationID: &inv, RecordedAt: admissionEpoch,
	}
	appendMilestone(t, s, first)
	replay := first
	replay.RecordedAt = admissionEpoch.Add(time.Hour)
	appendMilestone(t, s, replay)

	got := observeRun(t, s, f.run.ID)
	if len(got.Milestones) != 1 {
		t.Fatalf("milestones = %d, want 1", len(got.Milestones))
	}
	if !got.Milestones[0].RecordedAt.Equal(admissionEpoch) {
		t.Errorf("recorded_at = %v, want the first observation %v",
			got.Milestones[0].RecordedAt, admissionEpoch)
	}
}

// TestRunHoldObservationLifecycle: same cause advances only the span's end,
// a changed cause restarts the span, forward progress clears the hold, and
// an explicit clear removes it.
func TestRunHoldObservationLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAdmissionFixture(t, nil)
	s := openWithFixture(t, f, store.Options{AdmissionFloors: attendedFloors()})
	inv := domain.InvocationID("inv-1")

	recordHold := func(reason domain.RunHoldReason, at time.Time) {
		t.Helper()
		if err := s.Write(ctx, func(tx *store.WriteTx) error {
			return tx.RecordRunHold(ctx, domain.RunHoldObservation{
				RunID: f.run.ID, InvocationID: &inv, Reason: reason,
				FirstObservedAt: at, LastObservedAt: at,
			})
		}); err != nil {
			t.Fatalf("record hold: %v", err)
		}
	}

	recordHold(domain.HoldOperationStopped, admissionEpoch)
	recordHold(domain.HoldOperationStopped, admissionEpoch.Add(time.Minute))
	got := observeRun(t, s, f.run.ID)
	if got.Hold == nil {
		t.Fatal("hold missing after record")
	}
	if !got.Hold.FirstObservedAt.Equal(admissionEpoch) ||
		!got.Hold.LastObservedAt.Equal(admissionEpoch.Add(time.Minute)) {
		t.Errorf("same-cause span = %v..%v, want %v..%v",
			got.Hold.FirstObservedAt, got.Hold.LastObservedAt,
			admissionEpoch, admissionEpoch.Add(time.Minute))
	}

	recordHold(domain.HoldBlockingSystemHealth, admissionEpoch.Add(2*time.Minute))
	got = observeRun(t, s, f.run.ID)
	if got.Hold == nil || got.Hold.Reason != domain.HoldBlockingSystemHealth ||
		!got.Hold.FirstObservedAt.Equal(admissionEpoch.Add(2*time.Minute)) {
		t.Errorf("changed cause did not restart the span: %+v", got.Hold)
	}

	appendMilestone(t, s, domain.RunMilestone{
		RunID: f.run.ID, Kind: domain.MilestoneInvocationStarted,
		InvocationID: &inv, RecordedAt: admissionEpoch.Add(3 * time.Minute),
	})
	got = observeRun(t, s, f.run.ID)
	if got.Hold != nil {
		t.Errorf("forward progress left the hold in place: %+v", got.Hold)
	}

	// A converged milestone replay is not forward progress: replaying the
	// already-recorded milestone must leave a standing hold alone.
	recordHold(domain.HoldOperationStopped, admissionEpoch.Add(4*time.Minute))
	appendMilestone(t, s, domain.RunMilestone{
		RunID: f.run.ID, Kind: domain.MilestoneInvocationStarted,
		InvocationID: &inv, RecordedAt: admissionEpoch.Add(5 * time.Minute),
	})
	if got = observeRun(t, s, f.run.ID); got.Hold == nil {
		t.Error("a converged milestone replay cleared a standing hold")
	}

	// A clock stepped back below the recorded first instant restarts the
	// span by overwrite instead of failing the pass that carries the write.
	recordHold(domain.HoldOperationStopped, admissionEpoch.Add(2*time.Minute))
	if got = observeRun(t, s, f.run.ID); got.Hold == nil ||
		!got.Hold.FirstObservedAt.Equal(admissionEpoch.Add(2*time.Minute)) {
		t.Errorf("stepped-back re-record = %+v, want an overwritten span", got.Hold)
	}

	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.ClearRunHold(ctx, f.run.ID)
	}); err != nil {
		t.Fatalf("clear hold: %v", err)
	}
	if got = observeRun(t, s, f.run.ID); got.Hold != nil {
		t.Errorf("explicit clear left the hold in place: %+v", got.Hold)
	}
}

// TestClearRunHoldCause: the cause is a delete predicate, so a lane that
// ends one cause leaves a hold another cause is keeping exactly as it
// stands — span included.
func TestClearRunHoldCause(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAdmissionFixture(t, nil)
	s := openWithFixture(t, f, store.Options{AdmissionFloors: attendedFloors()})

	inv := domain.InvocationID("inv-observed")
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.RecordRunHold(ctx, domain.RunHoldObservation{
			RunID: f.run.ID, InvocationID: &inv,
			Reason:          domain.HoldVerificationFindings,
			FirstObservedAt: admissionEpoch, LastObservedAt: admissionEpoch,
		})
	}); err != nil {
		t.Fatalf("record hold: %v", err)
	}
	clearCause := func(reason domain.RunHoldReason) error {
		return s.Write(ctx, func(tx *store.WriteTx) error {
			return tx.ClearRunHoldCause(ctx, f.run.ID, reason)
		})
	}
	if err := clearCause(domain.HoldAttendedModeActive); err != nil {
		t.Fatalf("clear other cause: %v", err)
	}
	got := observeRun(t, s, f.run.ID)
	if got.Hold == nil || got.Hold.Reason != domain.HoldVerificationFindings ||
		!got.Hold.FirstObservedAt.Equal(admissionEpoch) {
		t.Errorf("clearing another cause disturbed the hold: %+v", got.Hold)
	}
	if err := clearCause(domain.HoldVerificationFindings); err != nil {
		t.Fatalf("clear matching cause: %v", err)
	}
	if got = observeRun(t, s, f.run.ID); got.Hold != nil {
		t.Errorf("matching cause left the hold in place: %+v", got.Hold)
	}
	// The closed vocabulary binds the predicate too: an unknown cause is a
	// contract violation, not a delete that silently matches nothing.
	if err := clearCause("vibes"); !errors.Is(err, domain.ErrInvalidRunHoldReason) {
		t.Errorf("unknown cause: ClearRunHoldCause() = %v, want %v",
			err, domain.ErrInvalidRunHoldReason)
	}
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.ClearRunHoldCause(ctx, "", domain.HoldAttendedModeActive)
	}); !errors.Is(err, domain.ErrEmptyID) {
		t.Errorf("empty run: ClearRunHoldCause() = %v, want %v", err, domain.ErrEmptyID)
	}
}

// TestInvocationObservationUpsert: last write wins, including the run
// binding — the stored row is projection, so a divergent binding is
// repaired by overwrite, never trusted enough to refuse the write (the
// refute-pass wedge).
func TestInvocationObservationUpsert(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAdmissionFixture(t, nil)
	s := openWithFixture(t, f, store.Options{AdmissionFloors: attendedFloors()})

	record := func(o domain.InvocationObservation) error {
		return s.Write(ctx, func(tx *store.WriteTx) error {
			return tx.RecordInvocationObservation(ctx, o)
		})
	}
	base := domain.InvocationObservation{
		InvocationID: "inv-1", RunID: f.run.ID,
		Status: domain.ObservedStatusPending, ObservedAt: admissionEpoch,
	}
	if err := record(base); err != nil {
		t.Fatalf("record: %v", err)
	}
	next := base
	next.Status = domain.ObservedStatusRunning
	next.Live = true
	next.ObservedAt = admissionEpoch.Add(time.Minute)
	if err := record(next); err != nil {
		t.Fatalf("update: %v", err)
	}
	got := observeRun(t, s, f.run.ID)
	if len(got.Invocations) != 1 || got.Invocations[0].Status != domain.ObservedStatusRunning ||
		!got.Invocations[0].ObservedAt.Equal(next.ObservedAt) {
		t.Errorf("last write did not win: %+v", got.Invocations)
	}

	repaired := next
	repaired.RunID = "run-2"
	repaired.ObservedAt = next.ObservedAt.Add(time.Minute)
	if err := record(repaired); err != nil {
		t.Fatalf("repairing rebind = %v, want overwrite", err)
	}
	if got := observeRun(t, s, "run-2"); len(got.Invocations) != 1 {
		t.Errorf("repaired binding not visible under its run: %+v", got.Invocations)
	}
	if got := observeRun(t, s, f.run.ID); len(got.Invocations) != 0 {
		t.Errorf("stale binding survived the repair: %+v", got.Invocations)
	}
}

// TestExecutionRecordsProjectMilestones: the admission, export, and outcome
// authorities carry their observation milestones atomically, whatever lane
// commits them, and the outcome's summary text never crosses into the
// projection.
func TestExecutionRecordsProjectMilestones(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAdmissionFixture(t, nil)
	s := openWithFixture(t, f, store.Options{AdmissionFloors: attendedFloors()})
	if err := recordAdmission(t, s, f.admission); err != nil {
		t.Fatalf("record admission: %v", err)
	}

	got := observeRun(t, s, f.run.ID)
	if len(got.Milestones) != 1 || got.Milestones[0].Kind != domain.MilestoneInvocationAdmitted {
		t.Fatalf("milestones after admission = %+v", got.Milestones)
	}
	if !got.Milestones[0].RecordedAt.Equal(f.admission.AdmittedAt) {
		t.Errorf("admitted milestone instant = %v, want the admission's own %v",
			got.Milestones[0].RecordedAt, f.admission.AdmittedAt)
	}

	outcome := domain.ExecutionOutcome{
		InvocationID: f.admission.InvocationID, AdmissionID: f.admission.ID,
		Status: domain.ExecutionOutcomeFailed, Summary: "provider stderr: secret-ish detail",
		RecordedAt: f.admission.AdmittedAt.Add(time.Hour),
	}
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.RecordExecutionOutcome(ctx, outcome)
	}); err != nil {
		t.Fatalf("record outcome: %v", err)
	}
	got = observeRun(t, s, f.run.ID)
	if len(got.Milestones) != 2 {
		t.Fatalf("milestones after outcome = %+v", got.Milestones)
	}
	last := got.Milestones[1]
	if last.Kind != domain.MilestoneExecutionOutcomeRecorded ||
		last.Outcome == nil || *last.Outcome != domain.ExecutionOutcomeFailed {
		t.Errorf("outcome milestone = %+v", last)
	}

	// The export path is exclusive with the outcome, so exercise it on a
	// fresh store.
	f2 := newAdmissionFixture(t, nil)
	s2 := openWithFixture(t, f2, store.Options{AdmissionFloors: attendedFloors()})
	if err := recordAdmission(t, s2, f2.admission); err != nil {
		t.Fatalf("record admission: %v", err)
	}
	export, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: f2.admission.InvocationID, AdmissionID: f2.admission.ID,
		ObservedBaseSHA: "deadbeef", HeadSHA: "cafebabe",
		ManifestDigest: "sha256:manifest",
		RecordedAt:     f2.admission.AdmittedAt.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("NewExecutionExport: %v", err)
	}
	if err := s2.Write(ctx, func(tx *store.WriteTx) error {
		return tx.RecordExecutionExport(ctx, export)
	}); err != nil {
		t.Fatalf("record export: %v", err)
	}
	got = observeRun(t, s2, f2.run.ID)
	if len(got.Milestones) != 2 ||
		got.Milestones[1].Kind != domain.MilestoneExecutionExportRecorded {
		t.Errorf("milestones after export = %+v", got.Milestones)
	}
}
