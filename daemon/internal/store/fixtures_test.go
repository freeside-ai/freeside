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
	command      domain.Command
	device       domain.Device
	credential   domain.DeviceCredential
	pairingCode  domain.PairingCode
}

// fixtureRecipe is the verification-recipe digest the evidence-bearing fixtures
// (the artifact and the item's snapshot) are built under; approvedFixtureRecipes
// is the approved set a store must carry to persist and reconstruct them. The
// fail-closed regression tests deliberately open with a bare store.Options{}
// (nothing approved) instead.
const fixtureRecipe = domain.Digest("sha256:recipe-approved")

func approvedFixtureRecipes() map[domain.Digest]bool {
	return map[domain.Digest]bool{fixtureRecipe: true}
}

func newFixtures(t *testing.T) fixtures {
	t.Helper()
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	recipe := fixtureRecipe
	approved := approvedFixtureRecipes()

	artifact, err := domain.NewArtifact(domain.ArtifactInput{
		ID: "art-1", Type: "verify_log", Digest: "sha256:log",
		Provenance: domain.Provenance{
			ProducerClass:            domain.ProducerVerifier,
			ProducerInvocationID:     "inv-1",
			HeadBinding:              domain.HeadBound,
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
	claimText := domain.ClaimText{
		MediaType: domain.MediaTypeTextMarkdown,
		Content:   "All checks green; the diff touches only docs.",
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-1", ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: "run-1", RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason:            "checks are green and the diff is ready",
		RequestedDecision: []domain.Action{domain.ActionOpenPR, domain.ActionReturnToAgent, domain.ActionDismiss},
		EvidenceSnapshot:  []domain.Artifact{artifact},
		AgentClaims: []domain.AgentClaim{{
			Label: "screenshot", Artifact: "art-2", Digest: "sha256:img",
			Provenance: domain.Provenance{
				ProducerClass:        domain.ProducerAgent,
				ProducerInvocationID: "inv-2",
				HeadBinding:          domain.HeadBound,
				SourceHeadSHA:        "cafebabe",
				SensitivityClass:     domain.SensitivityNormal,
			},
		}, {
			// A text claim (#217): the digest is computed over the content, so
			// the persisted item exercises the decode-side re-validation of
			// the binding rule, not just the marshalled shape.
			Label: "change summary", Artifact: "art-3", Digest: claimText.ComputeDigest(),
			Text: &claimText,
			Provenance: domain.Provenance{
				ProducerClass:        domain.ProducerAgent,
				ProducerInvocationID: "inv-2",
				HeadBinding:          domain.HeadBound,
				SourceHeadSHA:        "cafebabe",
				SensitivityClass:     domain.SensitivityNormal,
			},
		}},
		PRHeadSHA: "cafebabe", ItemVersion: 1,
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

	// The command records an accepted decision on the item, binding its version,
	// head, and derived digest set (the union of the evidence and claim digests).
	command, err := domain.NewCommand(domain.CommandInput{
		CommandID: "cmd-1", DeviceID: "device-1", ItemID: item.ID,
		ItemVersion: item.ItemVersion, PRHeadSHA: item.PRHeadSHA,
		ArtifactDigests: item.ArtifactDigests, Action: domain.ActionOpenPR,
	})
	if err != nil {
		t.Fatalf("NewCommand: %v", err)
	}

	// The digest is computed from the keys, and the run binds its policy by that
	// digest, so both must reference the same computed value (not a label).
	policy, err := domain.NewResolvedPolicy("run-1", []domain.PolicyKey{{
		Key: "rein", Value: "tight",
		Provenance: domain.KeyProvenance{Source: domain.ProvenancePreset, Digest: "sha256:preset"},
	}})
	if err != nil {
		t.Fatalf("NewResolvedPolicy: %v", err)
	}

	return fixtures{
		run: domain.Run{
			ID: "run-1", ProjectID: "proj-1",
			SpecDigest: "sha256:spec", PolicyDigest: policy.Digest,
			Stages: []domain.Stage{{
				ID: "stage-1", RunID: "run-1", Name: "implementation",
				Attempts: []domain.Attempt{{ID: "attempt-1", StageID: "stage-1", Number: 1, InvocationID: "inv-1"}},
			}},
		},
		conversation: domain.Conversation{ID: "conv-1", Status: domain.ConversationIdle, Messages: []domain.Message{{
			ID: "msg-1", ConversationID: "conv-1", Sequence: 1,
			Author: domain.AuthorUser, Body: "please proceed",
			Attachments: []domain.Digest{}, CreatedAt: ts,
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
		policy:  policy,
		command: command,
		// The device matches the device_id the delivery and command fixtures
		// already carry; its credential holds only verifier material (§5.14
		// no-reusable-plaintext). The pairing code is minted unconsumed;
		// consumption is exercised through ConsumePairingCode.
		device: domain.Device{
			ID: "device-1", DisplayName: "Ben's iPhone",
			Status: domain.DeviceActive, PairedAt: ts,
		},
		credential: domain.DeviceCredential{ //nolint:gosec // fixture digest of a fixture string, not a credential
			DeviceID: "device-1", Kind: domain.CredentialHash, Credential: "sha256:4d1566a1d7df42a8517456d60ea06ed284e535cfe4c956aa6ee172dbcdf945f7",
		},
		pairingCode: domain.PairingCode{
			CodeHash: "sha256:e5da4a1cdb3c241cc8b3f2a9d7ba70a679960729bd9d8700791d412b34feef97", CreatedAt: ts, ExpiresAt: ts.Add(10 * time.Minute),
		},
	}
}
