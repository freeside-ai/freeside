package domain_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

// validator is implemented by every domain type; the golden fixtures below are
// each a valid value, so they double as validation-positive cases.
type validator interface{ Validate() error }

// TestGolden is acceptance criterion 9: golden-file coverage of the serialized
// shape of every exported type. Each fixture is a fixed, valid value; its
// json.MarshalIndent bytes are compared against testdata/<name>.golden.
// Regenerate with: go test ./internal/domain -run TestGolden -update.
func TestGolden(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	recipe := domain.Digest("sha256:recipe-approved")
	approved := map[domain.Digest]bool{recipe: true}

	provenance := domain.Provenance{
		ProducerClass:            domain.ProducerVerifier,
		ProducerInvocationID:     "inv-1",
		HeadBinding:              domain.HeadBound,
		SourceHeadSHA:            "cafebabe", // matches the item's pr_head_sha (evidence head-binding)
		VerificationRecipeDigest: &recipe,
		SensitivityClass:         domain.SensitivityNormal,
	}
	artifact, err := domain.NewArtifact(domain.ArtifactInput{
		ID: "art-1", Type: "verify_log", Digest: "sha256:log", Provenance: provenance,
	}, approved)
	if err != nil {
		t.Fatal(err)
	}

	// Head-independent provenance (plan §5.15 rule 2): evidence intentionally
	// decoupled from repository head carries no source_head_sha and survives a
	// remediation head. Both modes appear in the goldens so the api examples
	// lifted from them exercise both discriminator branches.
	indepProvenance := domain.Provenance{
		ProducerClass:            domain.ProducerVerifier,
		ProducerInvocationID:     "inv-1",
		HeadBinding:              domain.HeadIndependent,
		VerificationRecipeDigest: &recipe,
		SensitivityClass:         domain.SensitivityNormal,
	}
	indepArtifact, err := domain.NewArtifact(domain.ArtifactInput{
		ID: "art-3", Type: "license_scan", Digest: "sha256:lic", Provenance: indepProvenance,
	}, approved)
	if err != nil {
		t.Fatal(err)
	}

	acceptedAt := ts.Add(time.Minute)
	openedAt := ts.Add(5 * time.Minute)
	delivery := domain.AttentionDelivery{
		ItemID: "item-1", DeviceID: "device-1", Channel: "ntfy", Attempt: 1,
		SubmittedAt: ts, ChannelAcceptedAt: &acceptedAt, OpenedAt: &openedAt,
		Status: domain.DeliveryOpened,
	}
	timing := domain.TimingAggregates([]domain.AttentionDelivery{delivery})

	runID := domain.RunID("run-1")
	convID := domain.ConversationID("conv-1")
	expires := ts.Add(24 * time.Hour)
	agentClaim := domain.AgentClaim{Label: "screenshot", Artifact: "art-2", Digest: "sha256:img"}
	subject := domain.Subject{Type: domain.SubjectRun, ID: "run-1", RunID: &runID}

	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-1", ProjectID: "proj-1", Subject: subject,
		Type: domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason:            "checks are green and the diff is ready",
		RequestedDecision: []domain.Action{domain.ActionOpenPR, domain.ActionReturnToAgent, domain.ActionDismiss},
		EvidenceSnapshot:  []domain.Artifact{artifact},
		AgentClaims:       []domain.AgentClaim{agentClaim},
		PRHeadSHA:         "cafebabe", ItemVersion: 1,
		InterruptionClass: domain.InterruptionPlannedGate,
		ConversationID:    &convID, ExpiresWhen: &expires, Status: domain.StatusOpen,
	}, approved)
	if err != nil {
		t.Fatal(err)
	}
	item, err = item.WithTiming([]domain.AttentionDelivery{delivery})
	if err != nil {
		t.Fatal(err)
	}

	device := domain.Device{
		ID: "device-1", DisplayName: "Ben's iPhone",
		Status: domain.DeviceActive, PairedAt: ts,
	}
	// The credential fixture carries only verifier material (the digest of an
	// issued token), per the §5.14 no-reusable-plaintext contract.
	credential := domain.DeviceCredential{ //nolint:gosec // fixture digest of a fixture string, not a credential
		DeviceID: "device-1", Kind: domain.CredentialHash, Credential: "sha256:4d1566a1d7df42a8517456d60ea06ed284e535cfe4c956aa6ee172dbcdf945f7",
	}
	consumedAt := ts.Add(time.Minute)
	consumingDevice := domain.DeviceID("device-1")
	pairingCode := domain.PairingCode{
		CodeHash: "sha256:e5da4a1cdb3c241cc8b3f2a9d7ba70a679960729bd9d8700791d412b34feef97", CreatedAt: ts, ExpiresAt: ts.Add(10 * time.Minute),
		ConsumedAt: &consumedAt, DeviceID: &consumingDevice,
	}

	msg := domain.Message{
		ID: "msg-1", ConversationID: "conv-1", Sequence: 1,
		Author: domain.AuthorUser, Body: "please proceed", CreatedAt: ts,
	}
	conversation := domain.Conversation{ID: "conv-1", Messages: []domain.Message{msg}}
	invocation := domain.AgentInvocation{ID: "inv-1", InputIDs: []domain.ArtifactID{"art-1", "art-2"}}

	finding := domain.Finding{
		ID: "find-1", RunID: "run-1", Source: "codex_github",
		Location: "daemon/main.go:42", Message: "unchecked error", RawText: "err not handled", CreatedAt: ts,
	}
	classification := domain.Classification{
		FindingID: "find-1", Version: 1, Materiality: "medium", Confidence: "high", Note: "worth fixing",
	}

	policyKey := domain.PolicyKey{
		Key: "rein", Value: "tight",
		Provenance: domain.KeyProvenance{Source: domain.ProvenancePreset, Digest: "sha256:preset"},
	}
	resolvedPolicy, err := domain.NewResolvedPolicy("run-1", []domain.PolicyKey{policyKey})
	if err != nil {
		t.Fatal(err)
	}

	attempt := domain.Attempt{ID: "attempt-1", StageID: "stage-1", Number: 1, InvocationID: "inv-1"}
	stage := domain.Stage{ID: "stage-1", RunID: "run-1", Name: "implementation", Attempts: []domain.Attempt{attempt}}
	run := domain.Run{ID: "run-1", ProjectID: "proj-1", SpecDigest: "sha256:spec", PolicyDigest: resolvedPolicy.Digest, Stages: []domain.Stage{stage}}

	// The command binds the item above: its accepted version, head, and the
	// item's derived binding set (union of the evidence and claim digests). The
	// digests are passed out of order to exercise NewCommand's canonicalization.
	command, err := domain.NewCommand(domain.CommandInput{
		CommandID: "cmd-1", DeviceID: "device-1", ItemID: "item-1",
		ItemVersion: 1, PRHeadSHA: "cafebabe",
		ArtifactDigests: []domain.Digest{"sha256:log", "sha256:img"},
		Action:          domain.ActionOpenPR,
	})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		value any
	}{
		{"attention_item", item},
		{"command", command},
		{"subject", subject},
		{"agent_claim", agentClaim},
		{"attention_delivery", delivery},
		{"timing_summary", timing},
		{"artifact", artifact},
		{"provenance", provenance},
		{"head_independent_artifact", indepArtifact},
		{"head_independent_provenance", indepProvenance},
		{"device", device},
		{"device_credential", credential},
		{"pairing_code", pairingCode},
		{"finding", finding},
		{"classification", classification},
		{"conversation", conversation},
		{"message", msg},
		{"agent_invocation", invocation},
		{"resolved_policy", resolvedPolicy},
		{"policy_key", policyKey},
		{"key_provenance", policyKey.Provenance},
		{"run", run},
		{"stage", stage},
		{"attempt", attempt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if v, ok := tc.value.(validator); ok {
				if err := v.Validate(); err != nil {
					t.Fatalf("golden fixture %q is not valid: %v", tc.name, err)
				}
			}
			got, err := json.MarshalIndent(tc.value, "", "  ")
			if err != nil {
				t.Fatalf("marshal %q: %v", tc.name, err)
			}
			golden.Assert(t, tc.name, append(got, '\n'))
		})
	}
}
