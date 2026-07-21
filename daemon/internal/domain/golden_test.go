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
	agentClaim := domain.AgentClaim{
		Label: "screenshot", Artifact: "art-2", Digest: "sha256:img",
		Provenance: domain.Provenance{
			ProducerClass:        domain.ProducerAgent,
			ProducerInvocationID: "inv-2",
			HeadBinding:          domain.HeadBound,
			SourceHeadSHA:        "cafebabe",
			SensitivityClass:     domain.SensitivityNormal,
		},
	}
	// A text claim's digest is computed, never hand-written: Validate
	// recomputes it over the content bytes, so a placeholder would make the
	// fixture validation-negative.
	claimText := domain.ClaimText{
		MediaType: domain.MediaTypeTextMarkdown,
		Content:   "All checks green; the diff touches only docs.",
	}
	textClaim := domain.AgentClaim{
		Label: "change summary", Artifact: "art-3", Digest: claimText.ComputeDigest(),
		Text: &claimText,
		Provenance: domain.Provenance{
			ProducerClass:        domain.ProducerAgent,
			ProducerInvocationID: "inv-2",
			HeadBinding:          domain.HeadBound,
			SourceHeadSHA:        "cafebabe",
			SensitivityClass:     domain.SensitivityNormal,
		},
	}
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

	// The read-only blocked type offers no action (plan §4; relaxed by #96):
	// this fixture pins the actionless shape, with every collection rendering
	// as the required non-null empty array the wire contract declares.
	blockedItem, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-2", ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: "run-1", RunID: &runID},
		Type:    domain.AttentionBlocked, Priority: domain.PriorityNormal,
		Reason:            "waiting on an external dependency",
		RequestedDecision: []domain.Action{},
		EvidenceSnapshot:  []domain.Artifact{},
		AgentClaims:       []domain.AgentClaim{},
		ItemVersion:       1,
		InterruptionClass: domain.InterruptionPlannedGate,
		Status:            domain.StatusOpen,
	}, approved)
	if err != nil {
		t.Fatal(err)
	}

	// The decided fixture pins the present render of decided_at (issue #171):
	// the item above, concluded by its offered dismiss decision, stamped at a
	// UTC-fixed instant. The base fixture keeps the explicit-null render.
	decidedItem := item
	decidedItem.ItemVersion = 2
	decidedItem.Status = domain.StatusDismissed
	decidedItem, err = decidedItem.WithDecidedAt(ts.Add(2 * time.Hour))
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
		Author: domain.AuthorUser, Body: "please proceed",
		Attachments: []domain.Digest{"sha256:img"}, CreatedAt: ts,
	}
	conversation := domain.Conversation{ID: "conv-1", Status: domain.ConversationAwaitingAgent, Messages: []domain.Message{msg}}
	// The invocation fixture binds both immutable input classes: artifact IDs
	// and a conversation prefix (the discuss shape, §5.14).
	invocationConv := domain.ConversationID("conv-1")
	invocation, err := domain.NewAgentInvocation("inv-1", []domain.ArtifactID{"art-1", "art-2"}, &invocationConv, 1)
	if err != nil {
		t.Fatal(err)
	}
	// An artifact-bound invocation renders the conversation binding's explicit
	// null (pointer-for-optional), pinning the pre-discuss shape.
	artifactInvocation, err := domain.NewAgentInvocation("inv-2", []domain.ArtifactID{"art-1"}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}

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

	// The protected-path extras are passed unsorted with a duplicate to
	// exercise NewAutomationTrustProfile's canonicalization.
	trustProfile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo:                       "freeside-ai/demo",
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        "sha256:workflow-audit",
		Review: domain.ReviewSettings{
			Mode: domain.ReviewAuto, ConfigDigest: "sha256:review-config",
		},
		ProtectedPaths: domain.ProtectedPathConfig{
			ExtraAutomationControlPatterns:   []string{"deploy/**", "ci/*.sh", "deploy/**"},
			ExtraVerificationControlPatterns: []string{"Makefile"},
			ExtraPromptsAndPolicyPatterns:    []string{"prompts/**", "policy/**", "prompts/**"},
			ExtraMaterialityRulesPatterns:    []string{"docs/plan.md"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	workflowAudit := domain.WorkflowAudit{
		Repo:                "freeside-ai/demo",
		AuditedCommitSHA:    "cafebabe",
		AuditedAt:           ts,
		WorkflowAuditDigest: "sha256:workflow-audit",
		EffectiveTokenPerms: domain.TokenPermissionsReadOnly,
		OIDCAvailable:       false,
		PullRequestTarget:   true,
		ReusableWorkflows:   true,
		ReviewDecisionRef:   "decision-1",
	}

	// The findings are passed out of canonical order to exercise
	// NewCandidateAuthorization's canonicalization; the waived
	// repo-change-policy finding carries the full waiver shape, and the
	// authorizing variant below shows the computed bit flipping with the
	// finding set.
	controlPlaneCategory := domain.ControlPlaneReviewerInstructions
	blockedAuthorization, err := domain.NewCandidateAuthorization(domain.CandidateAuthorizationInput{
		Repo: "freeside-ai/demo", BaseSHA: "beefcafe", HeadSHA: "cafebabe",
		ImportResultDigest:       "sha256:import-result",
		VerificationRecipeDigest: recipe,
		VerificationOutcome:      domain.VerificationPassed,
		Findings: []domain.CandidateFinding{
			{
				Class: domain.FindingClassSecret, Origin: domain.FindingOriginImport,
				Kind: "secret", Path: "config/app.env", Detail: "rule aws-key line 3",
				Disposition: domain.DispositionBlocking,
			},
			{
				Class: domain.FindingClassControlPlane, Category: &controlPlaneCategory,
				Origin: domain.FindingOriginImport, Kind: "reviewer_instruction_path",
				Path: "AGENTS.md", Disposition: domain.DispositionBlocking,
			},
			{
				Class: domain.FindingClassImportIntegrity, Origin: domain.FindingOriginImport,
				Kind: "non_regular_change", Path: "bin/tool",
				Disposition: domain.DispositionBlocking,
			},
		},
		TrustProfileDigest: trustProfile.ProfileDigest,
		InvocationID:       "inv-1",
		CreatedAt:          ts,
	})
	if err != nil {
		t.Fatal(err)
	}
	authorization, err := domain.NewCandidateAuthorization(domain.CandidateAuthorizationInput{
		Repo: "freeside-ai/demo", BaseSHA: "beefcafe", HeadSHA: "cafebabe",
		ImportResultDigest:       "sha256:import-result",
		VerificationRecipeDigest: recipe,
		VerificationOutcome:      domain.VerificationPassed,
		Findings: []domain.CandidateFinding{
			{
				Class: domain.FindingClassRepoChangePolicy, Origin: domain.FindingOriginImport,
				Kind: "size_violation", Path: "assets/big.bin",
				Disposition: domain.DispositionWaived,
				Waiver: &domain.WaiverRecord{
					DecisionID: "decision-2", DecidedBy: domain.AuthorUser, DecidedAt: ts,
					Justification:  "generated fixture, reviewed",
					DecisionDigest: "sha256:decision",
				},
			},
		},
		TrustProfileDigest: trustProfile.ProfileDigest,
		InvocationID:       "inv-1",
		CreatedAt:          ts,
	})
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
	// The discuss shape carries conversation content: the message body and its
	// attachment digests, which stay in authored order (no canonicalization).
	discussCommand, err := domain.NewCommand(domain.CommandInput{
		CommandID: "cmd-2", DeviceID: "device-1", ItemID: "item-1",
		ItemVersion: 1, PRHeadSHA: "cafebabe",
		ArtifactDigests: []domain.Digest{"sha256:log", "sha256:img"},
		Action:          domain.ActionDiscuss,
		Message:         "why does the retry loop back off twice?",
		Attachments:     []domain.Digest{"sha256:screen2", "sha256:screen1"},
	})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		value any
	}{
		{"attention_item", item},
		{"attention_item_blocked", blockedItem},
		{"attention_item_decided", decidedItem},
		{"command", command},
		{"subject", subject},
		{"agent_claim", agentClaim},
		{"agent_claim_text", textClaim},
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
		{"command_discuss", discussCommand},
		{"conversation", conversation},
		{"message", msg},
		{"agent_invocation", invocation},
		{"agent_invocation_artifact_bound", artifactInvocation},
		{"resolved_policy", resolvedPolicy},
		{"policy_key", policyKey},
		{"key_provenance", policyKey.Provenance},
		{"trust_profile", trustProfile},
		{"workflow_audit", workflowAudit},
		{"candidate_authorization", authorization},
		{"candidate_authorization_blocked", blockedAuthorization},
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
