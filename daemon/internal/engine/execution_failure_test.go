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
			Cause:      domain.FailureCauseUnknown,
			RecordedAt: recordedAt,
		})
	}); err != nil {
		t.Fatal(err)
	}

	var facts *domain.ExecutionFailureFacts
	if err := engine.store.Write(ctx, func(tx *store.WriteTx) error {
		var err error
		facts, err = executionFailureFacts(
			ctx, tx, attempt.InvocationID, exec.StatusFailed, "driver failure", "",
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
			ctx, tx, attempt.InvocationID, exec.StatusFailed, "invalid completed output", "",
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

// TestProductionFailureRecordsTerminalCause pins that the cause a failed
// terminal carries reaches the outcome the engine records, and that a
// terminal without one records unknown.
func TestProductionFailureRecordsTerminalCause(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		cause domain.FailureCause
		want  domain.FailureCause
	}{
		{"with a cause", domain.FailureCauseQuota, domain.FailureCauseQuota},
		{"without a cause", "", domain.FailureCauseUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			e, run, attempt := usageEngineFixture(t, true)
			terminal := productionTerminalRecord{
				InvocationID: attempt.InvocationID, RunID: run.ID,
				StageID: attempt.StageID, Status: exec.StatusFailed,
				Summary: "Writer exited with status 1.", Cause: tc.cause,
			}
			if _, err := e.recordProductionTerminalWithAuthority(ctx, run, terminal, false); err != nil {
				t.Fatal(err)
			}
			if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
				outcome, err := tx.GetExecutionOutcomeRecord(ctx, attempt.InvocationID)
				if err == nil && outcome.Cause != tc.want {
					t.Errorf("recorded cause = %q, want %q", outcome.Cause, tc.want)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestSameProductionTerminalComparesCanonicalCause covers a failed terminal
// stored before causes existed: the driver restores the same failure with
// cause unknown, and the two must still authenticate.
func TestSameProductionTerminalComparesCanonicalCause(t *testing.T) {
	t.Parallel()
	terminal := func(status exec.Status, cause domain.FailureCause) productionTerminalRecord {
		return productionTerminalRecord{
			InvocationID: "inv-1", RunID: "run-1", StageID: "stage-1",
			Status: status, Summary: "Writer exited with status 1.", Cause: cause,
		}
	}
	for _, tc := range []struct {
		name             string
		recorded, actual productionTerminalRecord
		want             bool
	}{
		{
			"causeless failed against unknown",
			terminal(exec.StatusFailed, ""), terminal(exec.StatusFailed, domain.FailureCauseUnknown), true,
		},
		{
			"same positive cause",
			terminal(exec.StatusFailed, domain.FailureCauseQuota), terminal(exec.StatusFailed, domain.FailureCauseQuota), true,
		},
		{
			"positive cause against unknown",
			terminal(exec.StatusFailed, domain.FailureCauseQuota), terminal(exec.StatusFailed, domain.FailureCauseUnknown), false,
		},
		{
			"canceled without a cause against unknown",
			terminal(exec.StatusCanceled, ""), terminal(exec.StatusCanceled, domain.FailureCauseUnknown), false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := sameProductionTerminal(tc.recorded, tc.actual); got != tc.want {
				t.Fatalf("sameProductionTerminal = %v, want %v", got, tc.want)
			}
		})
	}
}
