package store_test

import (
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// fixtures is one valid instance of every persisted aggregate root, built the
// same way as the domain package's golden fixtures so the two stay
// recognizably the same shapes. Referential order matters for the foreign
// keys: run and conversation before their dependents.
type fixtures struct {
	run          domain.Run
	conversation domain.Conversation
	invocation   domain.AgentInvocation
	artifact     domain.Artifact
	item         domain.AttentionItem
	delivery     domain.AttentionDelivery
	finding      domain.Finding
	class        domain.Classification
	policy       domain.ResolvedPolicy
}

func newFixtures(t *testing.T) fixtures {
	t.Helper()
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	recipe := domain.Digest("sha256:recipe-approved")
	approved := map[domain.Digest]bool{recipe: true}

	artifact, err := domain.NewArtifact(domain.ArtifactInput{
		ID: "art-1", Type: "verify_log", Digest: "sha256:log",
		Provenance: domain.Provenance{
			ProducerClass:            domain.ProducerVerifier,
			ProducerInvocationID:     "inv-1",
			SourceHeadSHA:            "cafebabe",
			VerificationRecipeDigest: &recipe,
			SensitivityClass:         domain.SensitivityNormal,
		},
	}, approved)
	if err != nil {
		t.Fatalf("NewArtifact: %v", err)
	}

	acceptedAt := ts.Add(time.Minute)
	openedAt := ts.Add(5 * time.Minute)
	delivery := domain.AttentionDelivery{
		ItemID: "item-1", DeviceID: "device-1", Channel: "ntfy", Attempt: 1,
		SubmittedAt: ts, ChannelAcceptedAt: &acceptedAt, OpenedAt: &openedAt,
		Status: domain.DeliveryOpened,
	}

	runID := domain.RunID("run-1")
	convID := domain.ConversationID("conv-1")
	expires := ts.Add(24 * time.Hour)
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-1", ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: "run-1", RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason:            "checks are green and the diff is ready",
		RequestedDecision: []domain.Action{domain.ActionOpenPR, domain.ActionReturnToAgent, domain.ActionDismiss},
		EvidenceSnapshot:  []domain.Artifact{artifact},
		AgentClaims:       []domain.AgentClaim{{Label: "screenshot", Artifact: "art-2", Digest: "sha256:img"}},
		ArtifactDigests:   []domain.Digest{"sha256:log"},
		PRHeadSHA:         "cafebabe", ItemVersion: 1,
		InterruptionClass: domain.InterruptionPlannedGate,
		ConversationID:    &convID, ExpiresWhen: &expires, Status: domain.StatusOpen,
	}, approved)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	item, err = item.WithTiming([]domain.AttentionDelivery{delivery})
	if err != nil {
		t.Fatalf("WithTiming: %v", err)
	}

	return fixtures{
		run: domain.Run{
			ID: "run-1", ProjectID: "proj-1",
			SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
			Stages: []domain.Stage{{
				ID: "stage-1", RunID: "run-1", Name: "implementation",
				Attempts: []domain.Attempt{{ID: "attempt-1", StageID: "stage-1", Number: 1, InvocationID: "inv-1"}},
			}},
		},
		conversation: domain.Conversation{ID: "conv-1", Messages: []domain.Message{{
			ID: "msg-1", ConversationID: "conv-1", Sequence: 1,
			Author: domain.AuthorUser, Body: "please proceed", CreatedAt: ts,
		}}},
		invocation: domain.AgentInvocation{ID: "inv-1", InputIDs: []domain.ArtifactID{"art-1", "art-2"}},
		artifact:   artifact,
		item:       item,
		delivery:   delivery,
		finding: domain.Finding{
			ID: "find-1", RunID: "run-1", Source: "codex_github",
			Location: "daemon/main.go:42", Message: "unchecked error",
			RawText: "err not handled", CreatedAt: ts,
		},
		class: domain.Classification{
			FindingID: "find-1", Version: 1, Materiality: "medium", Confidence: "high", Note: "worth fixing",
		},
		policy: domain.ResolvedPolicy{
			RunID: "run-1", Digest: "sha256:policy",
			Keys: []domain.PolicyKey{{
				Key: "rein", Value: "tight",
				Provenance: domain.KeyProvenance{Source: domain.ProvenancePreset, Digest: "sha256:preset"},
			}},
		},
	}
}
