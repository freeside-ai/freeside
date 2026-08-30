package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

func usageMeasurementFixture() exec.UsageMeasurement {
	return exec.UsageMeasurement{
		Source:     domain.UsageSourceAdapterTranscript,
		Kind:       domain.UsageMeasurementReportedUsage,
		Metric:     "input_tokens",
		Unit:       "tokens",
		Quantity:   123,
		Sequence:   1,
		ObservedAt: time.Date(2026, 1, 2, 4, 5, 6, 0, time.UTC),
	}
}

func seedUsageAdmission(t *testing.T) (*Store, domain.ExecutionAdmission) {
	t.Helper()
	s := openAgentAdmissionStore(t)
	generation := seedAgentClosure(t, s)
	admission := agentBoundAdmission(t, generation)
	if err := s.Write(context.Background(), func(tx *WriteTx) error {
		return tx.RecordExecutionAdmission(context.Background(), admission)
	}); err != nil {
		t.Fatalf("record admission: %v", err)
	}
	return s, admission
}

func listRunUsage(t *testing.T, s *Store, runID domain.RunID) []domain.UsageObservation {
	t.Helper()
	var observations []domain.UsageObservation
	if err := s.ReadUsage(context.Background(), func(tx *UsageReadTx) error {
		var err error
		observations, err = tx.ListRunUsageObservations(context.Background(), runID)
		return err
	}); err != nil {
		t.Fatalf("read usage: %v", err)
	}
	return observations
}

func TestAppendUsageObservationsRoundTripsAndReplays(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, admission := seedUsageAdmission(t)
	measurement := usageMeasurementFixture()

	for attempt, wantInserted := range []int{1, 0} {
		var inserted int
		if err := s.Write(ctx, func(tx *WriteTx) error {
			var err error
			inserted, err = tx.AppendUsageObservations(ctx, admission.InvocationID,
				[]exec.UsageMeasurement{measurement})
			return err
		}); err != nil {
			t.Fatalf("append attempt %d: %v", attempt+1, err)
		}
		if inserted != wantInserted {
			t.Fatalf("append attempt %d inserted %d, want %d", attempt+1, inserted, wantInserted)
		}
	}

	want := domain.UsageObservation{
		InvocationID:    string(admission.InvocationID),
		RunID:           string(admission.RunID),
		AgentDigest:     admission.AgentBinding.AgentDigest,
		LaunchDigest:    admission.AgentBinding.LaunchDigest,
		TreatmentDigest: admission.AgentBinding.TreatmentDigest,
		PricingRevision: admission.AgentBinding.PricingRevision,
		Source:          measurement.Source, Kind: measurement.Kind,
		Metric: measurement.Metric, Unit: measurement.Unit,
		Quantity: measurement.Quantity, Sequence: measurement.Sequence,
		ObservedAt: measurement.ObservedAt,
	}
	if got := listRunUsage(t, s, admission.RunID); !reflect.DeepEqual(got, []domain.UsageObservation{want}) {
		t.Fatalf("observations = %#v, want %#v", got, []domain.UsageObservation{want})
	}
}

func TestAppendUsageObservationsConflictingReplayFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, admission := seedUsageAdmission(t)
	measurement := usageMeasurementFixture()
	if err := s.Write(ctx, func(tx *WriteTx) error {
		_, err := tx.AppendUsageObservations(ctx, admission.InvocationID,
			[]exec.UsageMeasurement{measurement})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	conflict := measurement
	conflict.Quantity++
	err := s.Write(ctx, func(tx *WriteTx) error {
		_, err := tx.AppendUsageObservations(ctx, admission.InvocationID,
			[]exec.UsageMeasurement{conflict})
		return err
	})
	if !errors.Is(err, domain.ErrUsageObservationConflict) {
		t.Fatalf("conflicting append = %v, want %v", err, domain.ErrUsageObservationConflict)
	}
	if got := listRunUsage(t, s, admission.RunID); len(got) != 1 || got[0].Quantity != measurement.Quantity {
		t.Fatalf("conflicting append changed row: %#v", got)
	}
}

func TestAppendUsageObservationsWithoutBindingWritesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openAgentAdmissionStore(t)
	generation := seedAgentClosure(t, s)
	admission := agentBoundAdmission(t, generation)
	admission.AgentBinding = nil
	id, err := admission.ComputeID()
	if err != nil {
		t.Fatal(err)
	}
	admission.ID = id
	if err := admission.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(ctx, func(tx *WriteTx) error {
		return tx.RecordExecutionAdmission(ctx, admission)
	}); err != nil {
		t.Fatalf("record legacy admission: %v", err)
	}
	var inserted int
	if err := s.Write(ctx, func(tx *WriteTx) error {
		var err error
		inserted, err = tx.AppendUsageObservations(ctx, admission.InvocationID,
			[]exec.UsageMeasurement{usageMeasurementFixture()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if inserted != 0 || len(listRunUsage(t, s, admission.RunID)) != 0 {
		t.Fatalf("unattributed append inserted %d rows", inserted)
	}
}

func TestAppendUsageObservationsUsesHistoricalAdmissionAttribution(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, admission := seedUsageAdmission(t)
	// Terminal observations describe work already admitted. Tightening the
	// live capability floor can block another admission read without changing
	// the immutable attribution that the completed invocation ran under.
	tightenedFloor := domain.NewCapabilitySnapshot(domain.AllRunnerCapabilities...)
	s.admissionPolicy.Floors[domain.ModeAttendedDev] = tightenedFloor
	if err := s.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetExecutionAdmission(ctx, admission.InvocationID)
		return err
	}); !errors.Is(err, domain.ErrCapabilityBelowFloor) {
		t.Fatalf("current-policy admission read = %v, want %v",
			err, domain.ErrCapabilityBelowFloor)
	}
	if err := s.Write(ctx, func(tx *WriteTx) error {
		_, err := tx.AppendUsageObservations(ctx, admission.InvocationID,
			[]exec.UsageMeasurement{usageMeasurementFixture()})
		return err
	}); err != nil {
		t.Fatalf("append under tightened admission policy: %v", err)
	}
	if got := listRunUsage(t, s, admission.RunID); len(got) != 1 ||
		got[0].TreatmentDigest != admission.AgentBinding.TreatmentDigest {
		t.Fatalf("historically attributed observations = %#v", got)
	}
}

func TestUsageObservationsAreStructurallyAppendOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, admission := seedUsageAdmission(t)
	if err := s.Write(ctx, func(tx *WriteTx) error {
		_, err := tx.AppendUsageObservations(ctx, admission.InvocationID,
			[]exec.UsageMeasurement{usageMeasurementFixture()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for name, statement := range map[string]string{
		"update": `UPDATE usage_observations SET quantity = 9 WHERE invocation_id = 'inv-1'`,
		"delete": `DELETE FROM usage_observations WHERE invocation_id = 'inv-1'`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.db.ExecContext(ctx, statement); err == nil {
				t.Fatalf("%s succeeded on append-only table", name)
			}
		})
	}
}

func TestUsageObservationMayArriveAfterConclusion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, admission := seedUsageAdmission(t)
	outcome := domain.ExecutionOutcome{
		InvocationID: admission.InvocationID,
		AdmissionID:  admission.ID,
		Status:       domain.ExecutionOutcomeFailed,
		Summary:      "provider failed after reporting usage",
		RecordedAt:   admission.AdmittedAt.Add(time.Hour),
	}
	if err := s.Write(ctx, func(tx *WriteTx) error {
		return tx.RecordExecutionOutcome(ctx, outcome)
	}); err != nil {
		t.Fatalf("record outcome: %v", err)
	}
	if err := s.Write(ctx, func(tx *WriteTx) error {
		_, err := tx.AppendUsageObservations(ctx, admission.InvocationID,
			[]exec.UsageMeasurement{usageMeasurementFixture()})
		return err
	}); err != nil {
		t.Fatalf("late append: %v", err)
	}
	if got := listRunUsage(t, s, admission.RunID); len(got) != 1 {
		t.Fatalf("late observations = %#v, want one", got)
	}
}

func TestPlaintextRestoreReplacesUsageAndReinstatesAppendOnlyGuard(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, admission := seedUsageAdmission(t)
	first := usageMeasurementFixture()
	if err := s.Write(ctx, func(tx *WriteTx) error {
		_, err := tx.AppendUsageObservations(ctx, admission.InvocationID, []exec.UsageMeasurement{first})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	checkpoint := filepath.Join(t.TempDir(), "usage-checkpoint.db")
	if err := s.Checkpoint(ctx, checkpoint); err != nil {
		t.Fatal(err)
	}
	appendLaterUsage(t, ctx, s, admission)

	if _, err := s.Restore(ctx, checkpoint); err != nil {
		t.Fatalf("restore usage checkpoint: %v", err)
	}
	assertRestoredUsageGuard(t, ctx, s, admission, first)
}

func TestEncryptedRestoreReplacesUsageAndReinstatesAppendOnlyGuard(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, admission := seedUsageAdmission(t)
	first := usageMeasurementFixture()
	if err := s.Write(ctx, func(tx *WriteTx) error {
		_, err := tx.AppendUsageObservations(ctx, admission.InvocationID, []exec.UsageMeasurement{first})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	plaintext, err := serializeStoreCheckpoint(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	sourceDB, source, err := openDeserializedBackupDatabase(ctx, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDeserializedBackupDatabase(sourceDB, source)
	appendLaterUsage(t, ctx, s, admission)

	if _, err := s.restoreFromDatabase(ctx, source); err != nil {
		t.Fatalf("restore encrypted usage checkpoint: %v", err)
	}
	assertRestoredUsageGuard(t, ctx, s, admission, first)
}

func appendLaterUsage(
	t *testing.T, ctx context.Context, s *Store, admission domain.ExecutionAdmission,
) {
	t.Helper()
	later := usageMeasurementFixture()
	later.Sequence = 2
	later.Quantity++
	later.ObservedAt = later.ObservedAt.Add(time.Minute)
	if err := s.Write(ctx, func(tx *WriteTx) error {
		_, err := tx.AppendUsageObservations(ctx, admission.InvocationID, []exec.UsageMeasurement{later})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func assertRestoredUsageGuard(
	t *testing.T,
	ctx context.Context,
	s *Store,
	admission domain.ExecutionAdmission,
	want exec.UsageMeasurement,
) {
	t.Helper()
	rows := listRunUsage(t, s, admission.RunID)
	if len(rows) != 1 || rows[0].Sequence != want.Sequence || rows[0].Quantity != want.Quantity {
		t.Fatalf("restored usage = %#v, want only checkpoint measurement %#v", rows, want)
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM usage_observations WHERE invocation_id = ?`, admission.InvocationID,
	); err == nil {
		t.Fatal("restored usage delete guard accepted a delete")
	}
}
