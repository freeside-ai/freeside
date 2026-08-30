package engine

import (
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestExecutionFailureFactsReuseDriverOutcome(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	engine, _, attempt := usageEngineFixture(t, true)
	recordedAt := time.Date(2026, 1, 2, 5, 0, 0, 0, time.UTC)
	var admission domain.ExecutionAdmission
	if err := engine.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		admission, err = tx.GetExecutionAdmissionRecord(ctx, attempt.InvocationID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := engine.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.RecordExecutionOutcome(ctx, domain.ExecutionOutcome{
			InvocationID: attempt.InvocationID, AdmissionID: admission.ID,
			Status: domain.ExecutionOutcomeFailed, Summary: "driver failure",
			RecordedAt: recordedAt,
		})
	}); err != nil {
		t.Fatal(err)
	}

	var facts *domain.ExecutionFailureFacts
	if err := engine.store.Write(ctx, func(tx *store.WriteTx) error {
		var err error
		facts, err = executionFailureFacts(
			ctx, tx, attempt.InvocationID, exec.StatusFailed, "driver failure",
			recordedAt.Add(time.Minute), domain.StageNameImplementation,
		)
		return err
	}); err != nil {
		t.Fatalf("reuse driver outcome: %v", err)
	}
	want := &domain.ExecutionFailureFacts{
		Outcome: domain.ExecutionOutcomeFailed, Stage: domain.StageNameImplementation,
		InvocationID: attempt.InvocationID,
	}
	if facts == nil || *facts != *want {
		t.Fatalf("facts = %#v, want %#v", facts, want)
	}
}

func TestExecutionFailureFactsLeaveCompletedExportUnclassified(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	engine, _, attempt := usageEngineFixture(t, true)
	var admission domain.ExecutionAdmission
	if err := engine.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		admission, err = tx.GetExecutionAdmissionRecord(ctx, attempt.InvocationID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	exportedAt := admission.AdmittedAt.Add(time.Minute)
	export, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: attempt.InvocationID, AdmissionID: admission.ID,
		ObservedBaseSHA: admission.Base.BaseSHA, HeadSHA: "cafebabe",
		ManifestDigest: "sha256:manifest", RecordedAt: exportedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.RecordExecutionExport(ctx, export)
	}); err != nil {
		t.Fatal(err)
	}
	var facts *domain.ExecutionFailureFacts
	if err := engine.store.Write(ctx, func(tx *store.WriteTx) error {
		var err error
		facts, err = executionFailureFacts(
			ctx, tx, attempt.InvocationID, exec.StatusFailed, "invalid completed output",
			exportedAt.Add(time.Minute), domain.StageNameElaboration,
		)
		return err
	}); err != nil {
		t.Fatalf("completed export classification: %v", err)
	}
	if facts != nil {
		t.Fatalf("completed export facts = %#v, want nil", facts)
	}
}
