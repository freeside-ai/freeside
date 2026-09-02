package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/export"
)

func TestAgentQuestionActionSetAuthentication(t *testing.T) {
	t.Run("write", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		st, item := authenticatedAgentQuestionFixture(t)
		item.RequestedDecision = []domain.Action{domain.ActionAnswerWithoutRetry, domain.ActionStop}

		err := st.Write(ctx, func(tx *WriteTx) error { return tx.PutAttentionItem(ctx, item) })
		if !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("put question with unauthenticated action set = %v, want ErrParentKeyMismatch", err)
		}
	})

	t.Run("reconstruction", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		st, item := authenticatedAgentQuestionFixture(t)
		putItem(t, ctx, st, item)

		item.RequestedDecision = []domain.Action{domain.ActionAnswerWithoutRetry, domain.ActionStop}
		surface, err := domain.NewDecisionSurface(item)
		if err != nil {
			t.Fatal(err)
		}
		item.DecisionSurface = domain.DecisionSurfaceRef{Epoch: surface.Epoch, Digest: surface.Digest}
		itemBody, err := encode(item)
		if err != nil {
			t.Fatal(err)
		}
		surfaceBody, err := encode(surface)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.ExecContext(ctx,
			`UPDATE attention_items SET body = ? WHERE id = ?`, itemBody, item.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.ExecContext(ctx,
			`UPDATE attention_decision_surfaces SET digest = ?, body = ? WHERE item_id = ?`,
			surface.Digest, surfaceBody, item.ID); err != nil {
			t.Fatal(err)
		}

		err = st.Read(ctx, func(tx *ReadTx) error {
			_, err := tx.GetAttentionItem(ctx, item.ID)
			return err
		})
		if !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("reconstruct question with unauthenticated action set = %v, want ErrParentKeyMismatch", err)
		}
	})
}

func authenticatedAgentQuestionFixture(t *testing.T) (*Store, domain.AttentionItem) {
	t.Helper()
	ctx := context.Background()
	st, admission := seedAdmission(t, nil)
	invocation, err := domain.NewAgentInvocation(admission.InvocationID, []domain.ArtifactID{"input-1"}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	kind := domain.BlockedKindOwnerDecision
	facts := &domain.AgentQuestionFacts{
		Stage: domain.StageNameImplementation, InvocationID: admission.InvocationID, Kind: &kind,
		Decisions: []domain.Decision{{
			Question: "Which deployment target should be used?", WhyBlocking: "The target is required.",
			Options: []domain.DecisionOption{
				{Label: "staging", Tradeoffs: "Safe validation."},
				{Label: "production", Tradeoffs: "Immediate delivery."},
			},
			Recommendation: "staging",
		}},
	}
	digest, err := facts.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	createdAt := time.Date(2026, 9, 2, 20, 0, 0, 0, time.UTC)
	claim := domain.AgentClaim{
		Label: export.BlockedEvidenceLabel, Artifact: "blocked-inv-1", Digest: digest,
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerAgent, ProducerInvocationID: admission.InvocationID,
			HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
		},
		Metadata: domain.EvidenceMetadata{
			MediaType: domain.EvidenceMediaApplicationJSON, SizeBytes: 1, CreatedAt: createdAt,
			Source: domain.EvidenceSourceClaim, Availability: domain.EvidenceAvailable,
		},
	}
	artifactMetadata := claim.Metadata
	artifactMetadata.Source = domain.EvidenceSourceRun
	artifact, err := domain.NewArtifact(domain.ArtifactInput{
		ID: claim.Artifact, Type: domain.ArtifactKindEvidence, Digest: claim.Digest,
		Provenance: claim.Provenance, Metadata: artifactMetadata,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	questionClaim := claim
	questionClaim.Label = domain.AgentQuestionClaimLabel
	runID := admission.RunID
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: domain.ItemID("question-" + string(admission.InvocationID)), ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionAgentQuestion, Priority: domain.PriorityNormal,
		Reason:            facts.Decisions[0].Question,
		RequestedDecision: []domain.Action{domain.ActionAnswerAndRetry, domain.ActionStop},
		AgentClaims:       []domain.AgentClaim{questionClaim}, AgentQuestion: facts,
		ItemVersion: 1, InterruptionClass: domain.InterruptionExceptional,
		CreatedAt: &createdAt, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := json.Marshal(agentQuestionProductionTerminal{
		InvocationID: admission.InvocationID, RunID: runID, StageID: admission.StageID,
		Status: exec.StatusBlocked, Artifacts: []domain.Digest{claim.Digest},
		Summary: facts.Decisions[0].Question,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutAgentInvocation(ctx, invocation); err != nil {
			return err
		}
		if err := tx.PutArtifact(ctx, artifact); err != nil {
			return err
		}
		if err := tx.PutAgentClaims(ctx, admission.InvocationID, []domain.AgentClaim{claim}); err != nil {
			return err
		}
		if err := tx.RecordExecutionOutcome(ctx, domain.ExecutionOutcome{
			InvocationID: admission.InvocationID, AdmissionID: admission.ID,
			Status: domain.ExecutionOutcomeBlocked, Summary: facts.Decisions[0].Question,
			RecordedAt: createdAt,
		}); err != nil {
			return err
		}
		_, _, err := tx.RecordInbox(ctx, string(admission.InvocationID),
			agentQuestionProductionTerminalKind, terminal)
		return err
	}); err != nil {
		t.Fatalf("seed authenticated question: %v", err)
	}
	return st, item
}
