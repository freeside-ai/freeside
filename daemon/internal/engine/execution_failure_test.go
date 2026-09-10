package engine

import (
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestFailedWriterCardLinksSensitiveTranscriptAndReplays(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e, run, attempt := usageEngineFixture(t, true)
	claim := domain.AgentClaim{
		Label: "agent-transcript", Artifact: "artifact-failed-transcript-test",
		Digest: domain.Digest(contentaddr.Sum([]byte("diagnostic"))),
		Provenance: domain.Provenance{
			ProducerClass:        domain.ProducerAgent,
			ProducerInvocationID: attempt.InvocationID, HeadBinding: domain.HeadIndependent,
			SensitivityClass: domain.SensitivitySensitive,
		},
		Metadata: domain.EvidenceMetadata{
			MediaType: domain.EvidenceMediaTextPlain, SizeBytes: 10,
			CreatedAt: time.Date(2026, 1, 2, 5, 0, 0, 0, time.UTC),
			Source:    domain.EvidenceSourceClaim, Availability: domain.EvidenceAvailable,
		},
	}
	if err := e.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutAgentInvocation(ctx, domain.AgentInvocation{ID: attempt.InvocationID, InputIDs: []domain.ArtifactID{"input-failure-test"}}); err != nil {
			return err
		}
		return tx.PutAgentClaims(ctx, attempt.InvocationID, []domain.AgentClaim{claim})
	}); err != nil {
		t.Fatal(err)
	}
	terminal := productionTerminalRecord{
		InvocationID: attempt.InvocationID, RunID: run.ID,
		StageID: attempt.StageID, Status: exec.StatusFailed, Summary: "Writer exited with status 1.",
	}
	for range 2 {
		if _, err := e.recordProductionTerminalWithAuthority(ctx, run, terminal, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		item, err := tx.GetAttentionItem(ctx, domain.ItemID("execution-failure-"+string(attempt.InvocationID)))
		if err != nil {
			return err
		}
		if len(item.AgentClaims) != 1 || item.AgentClaims[0].Digest != claim.Digest || item.AgentClaims[0].Text != nil || item.AgentClaims[0].Provenance.SensitivityClass != domain.SensitivitySensitive || len(item.ArtifactDigests) != 1 || item.ArtifactDigests[0] != claim.Digest {
			t.Fatalf("failure card evidence: %+v", item.AgentClaims)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

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
	if facts == nil || !reflect.DeepEqual(facts, want) {
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
			exportedAt.Add(time.Minute), domain.StageNameSpecification,
		)
		return err
	}); err != nil {
		t.Fatalf("completed export classification: %v", err)
	}
	if facts != nil {
		t.Fatalf("completed export facts = %#v, want nil", facts)
	}
}
