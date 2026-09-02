package domain_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
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
		ID: "art-1", Type: domain.ArtifactKindVerifyLog, Digest: "sha256:log", Provenance: provenance,
		Metadata: domain.EvidenceMetadata{
			MediaType: domain.EvidenceMediaApplicationJSON, SizeBytes: 512, CreatedAt: ts,
			Source: domain.EvidenceSourceRun, Availability: domain.EvidenceAvailable,
		},
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
		ID: "art-3", Type: domain.ArtifactKindLicenseScan, Digest: "sha256:lic", Provenance: indepProvenance,
		// Availability bytes_absent so a standalone golden pins that member: the
		// digest is held but its bytes are not currently in the blob store.
		Metadata: domain.EvidenceMetadata{
			MediaType: domain.EvidenceMediaApplicationJSON, SizeBytes: 128, CreatedAt: ts,
			Source: domain.EvidenceSourceRun, Availability: domain.EvidenceBytesAbsent,
		},
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
		Metadata: domain.EvidenceMetadata{
			MediaType: domain.EvidenceMediaImagePNG, SizeBytes: 4096, CreatedAt: ts,
			Source: domain.EvidenceSourceClaim, Availability: domain.EvidenceAvailable,
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
		Label: "freeside.summary", Artifact: "art-3", Digest: claimText.ComputeDigest(),
		Text: &claimText,
		Provenance: domain.Provenance{
			ProducerClass:        domain.ProducerAgent,
			ProducerInvocationID: "inv-2",
			HeadBinding:          domain.HeadBound,
			SourceHeadSHA:        "cafebabe",
			SensitivityClass:     domain.SensitivityNormal,
		},
		// The metadata media type must equal the inline text's (text/markdown).
		Metadata: domain.EvidenceMetadata{
			MediaType: domain.EvidenceMediaTextMarkdown, SizeBytes: int64(len(claimText.Content)), CreatedAt: ts,
			Source: domain.EvidenceSourceClaim, Availability: domain.EvidenceAvailable,
		},
	}
	subject := domain.Subject{Type: domain.SubjectRun, ID: "run-1", RunID: &runID}

	// The base item carries a present commit-plan notice so the goldens pin
	// both renders: present here (and on the decided fixture derived from
	// it), explicit null on the blocked fixture.
	noticeReason := domain.CommitPlanNoticePresentButNotHonored
	readiness := domain.ReadinessSummary{
		Class: domain.ReadinessReadyClean, EvaluationSetDigest: "sha256:evaluation-clean",
	}
	// The detail behind the summary (issue #982): the production set's two
	// required checks, both passed against the published head and admitted
	// base, so the card can list every requirement and its bound coordinates.
	verificationRecipe := domain.Digest("sha256:recipe")
	reviewRecipe := domain.Digest("sha256:review-config")
	readinessDetail := domain.ReadinessDetail{
		EvaluationSetDigest: readiness.EvaluationSetDigest, CandidateHead: "cafebabe",
		Base: domain.ReadinessBoundBase{BaseRef: "main", BaseSHA: "deadbeef"},
		Requirements: []domain.ReadinessRequirement{
			{
				RequirementKey: "clean-verification", CheckClass: domain.CheckClassCleanVerification,
				Kind: domain.RequirementRequired, State: domain.ReadinessRequirementPassed,
				ProofRecipeDigest: &verificationRecipe,
			},
			{
				RequirementKey: "independent-review", CheckClass: domain.CheckClassIndependentReview,
				Kind: domain.RequirementRequired, State: domain.ReadinessRequirementPassed,
				ProofRecipeDigest: &reviewRecipe,
			},
		},
	}
	yieldHistory := domain.ReviewYieldHistory{
		Rounds: []domain.ReviewYieldRound{
			{
				Round: 1, FindingsIngested: 2, NewFindings: 2, Fixed: 1, Deferred: 1,
				Outcome: domain.ReviewFindings,
			},
			{
				Round: 2, FindingsIngested: 2, NewFindings: 1, RecurringFindings: 1,
				Fixed: 1, Declined: 1, Outcome: domain.ReviewFindings,
			},
			{Round: 3, Outcome: domain.ReviewClean},
		},
		TerminalOutcome: domain.ReviewClean,
	}
	displayNames := domain.DisplayNames{
		Project:  domain.DisplayName{Text: "owner/repo", Source: domain.DisplayNameSourceName},
		WorkUnit: domain.DisplayName{Text: "#724", Source: domain.DisplayNameSourceName},
	}
	diffStats := domain.DiffStats{
		FilesChanged: 12, Additions: 240, Deletions: 31,
		BaseSHA: "deadbeef", HeadSHA: "cafebabe",
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-1", ProjectID: "proj-1", Subject: subject,
		Type: domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason:            "checks are green and the diff is ready",
		RequestedDecision: []domain.Action{domain.ActionOpenPR, domain.ActionReturnToAgent, domain.ActionDismiss},
		EvidenceSnapshot:  []domain.Artifact{artifact},
		AgentClaims:       []domain.AgentClaim{agentClaim},
		PRHeadSHA:         "cafebabe",
		PRReference:       &domain.PRReference{Repo: "owner/repo", Number: 123},
		Readiness:         &readiness,
		ReadinessDetail:   &readinessDetail,
		YieldHistory:      &yieldHistory,
		CommitPlanNotice:  &noticeReason,
		DisplayNames:      &displayNames,
		DiffStats:         &diffStats,
		ItemVersion:       1,
		InterruptionClass: domain.InterruptionPlannedGate,
		ConversationID:    &convID, CreatedAt: &ts, ExpiresWhen: &expires, Status: domain.StatusOpen,
	}, approved)
	if err != nil {
		t.Fatal(err)
	}
	// The decision-surface identity of the base item at creation (plan §4):
	// the persisted record the store derives, kept off the item body until
	// #917 projects {epoch, digest} onto the wire.
	decisionSurface, err := domain.NewDecisionSurface(item)
	if err != nil {
		t.Fatal(err)
	}
	diminishingItem, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-diminishing-yield", ProjectID: "proj-1", Subject: subject,
		Type: domain.AttentionReviewDiminishing, Priority: domain.PriorityNormal,
		Reason: "review rounds are surfacing only marginal findings",
		RequestedDecision: []domain.Action{
			domain.ActionFinishNow, domain.ActionApplyThenFinish,
		},
		EvidenceSnapshot:  []domain.Artifact{},
		AgentClaims:       []domain.AgentClaim{},
		YieldHistory:      &yieldHistory,
		BillableCostSoFar: &domain.CostSoFar{Currency: "USD", Amount: "42.75", Invocations: 6, Complete: false},
		ItemVersion:       1,
		InterruptionClass: domain.InterruptionPlannedGate,
		CreatedAt:         &ts,
		Status:            domain.StatusOpen,
	}, approved)
	if err != nil {
		t.Fatal(err)
	}
	degradedItem := item
	degradedItem.Readiness = &domain.ReadinessSummary{
		Class: domain.ReadinessReadyDegraded, EvaluationSetDigest: "sha256:evaluation-degraded",
	}
	// Degraded: the same two passes plus a waived required policy failure and
	// an optional check that never ran, so the goldens pin the waiver's
	// identity and granting authority beside the advisory entry.
	degradedDetail := readinessDetail
	degradedDetail.EvaluationSetDigest = degradedItem.Readiness.EvaluationSetDigest
	degradedDetail.Requirements = append(slices.Clone(readinessDetail.Requirements),
		domain.ReadinessRequirement{
			RequirementKey: "license-headers", CheckClass: domain.CheckClassRepoChangePolicy,
			Kind: domain.RequirementOptional, State: domain.ReadinessRequirementNotRun,
		},
		domain.ReadinessRequirement{
			RequirementKey: "repo-change-policy", CheckClass: domain.CheckClassRepoChangePolicy,
			Kind: domain.RequirementRequired, State: domain.ReadinessRequirementFailed,
			Waiver: &domain.ReadinessWaiver{
				ID: "waiver-1", Dimension: "repo_change_policy",
				Authority: domain.WaiverAuthorityHumanApproval, GrantedAt: ts.Add(-time.Hour),
			},
		},
	)
	degradedItem.ReadinessDetail = &degradedDetail
	if err := degradedItem.Validate(); err != nil {
		t.Fatal(err)
	}
	item, err = item.WithTiming([]domain.AttentionDelivery{delivery})
	if err != nil {
		t.Fatal(err)
	}

	// The read-only blocked type offers no action (plan §4; relaxed by #96):
	// this fixture pins the actionless shape, with every collection rendering
	// as the required non-null empty array the wire contract declares.
	blockedOnItemID := domain.ItemID("item-spec-approval")
	blockedItem, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-2", ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: "run-1", RunID: &runID},
		Type:    domain.AttentionBlocked, Priority: domain.PriorityNormal,
		Reason:            "waiting on an external dependency",
		RequestedDecision: []domain.Action{},
		EvidenceSnapshot:  []domain.Artifact{},
		AgentClaims:       []domain.AgentClaim{},
		BlockedOn: &domain.BlockedWait{
			Kind: domain.BlockedWaitSpecApproval, Since: ts, ItemID: &blockedOnItemID,
		},
		ItemVersion:       1,
		InterruptionClass: domain.InterruptionPlannedGate,
		CreatedAt:         &ts,
		Status:            domain.StatusOpen,
	}, approved)
	if err != nil {
		t.Fatal(err)
	}

	executionFailureItem, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-execution-failure", ProjectID: "proj-1", Subject: subject,
		Type: domain.AttentionExecutionFailure, Priority: domain.PriorityUrgent,
		Reason:            "the implementation stage failed",
		RequestedDecision: []domain.Action{domain.ActionRetry, domain.ActionStop},
		ExecutionFailure: &domain.ExecutionFailureFacts{
			Outcome: domain.ExecutionOutcomeFailed, Stage: domain.StageNameImplementation,
			InvocationID: "inv-implementation-1",
		},
		ItemVersion: 1, InterruptionClass: domain.InterruptionExceptional,
		CreatedAt: &ts, Status: domain.StatusOpen,
	}, approved)
	if err != nil {
		t.Fatal(err)
	}

	questionProvenance := domain.Provenance{
		ProducerClass: domain.ProducerAgent, ProducerInvocationID: "inv-specify-1",
		HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
	}
	agentQuestionFacts := &domain.AgentQuestionFacts{
		Stage: domain.StageNameSpecification, InvocationID: "inv-specify-1",
		Decisions: []domain.Decision{{
			Question:    "Which retention period applies to exported logs?",
			WhyBlocking: "The specification cannot fix the schema without it.",
			Options: []domain.DecisionOption{
				{Label: "30 days", Tradeoffs: "Cheaper storage, shorter audit window."},
				{Label: "1 year", Tradeoffs: "Longer audit window, higher storage cost."},
			},
			Recommendation: "30 days",
		}},
	}
	questionDigest, err := agentQuestionFacts.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	agentQuestionItem, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-agent-question", ProjectID: "proj-1", Subject: subject,
		Type: domain.AttentionAgentQuestion, Priority: domain.PriorityNormal,
		Reason:            "the specifier needs an owner decision",
		RequestedDecision: []domain.Action{domain.ActionAnswerAndRetry, domain.ActionStop},
		AgentClaims: []domain.AgentClaim{{
			Label: domain.AgentQuestionClaimLabel, Artifact: "decisions-inv-specify-1",
			Digest: questionDigest, Provenance: questionProvenance,
			Metadata: domain.EvidenceMetadata{
				MediaType: domain.EvidenceMediaApplicationJSON, SizeBytes: 256, CreatedAt: ts,
				Source: domain.EvidenceSourceClaim, Availability: domain.EvidenceAvailable,
			},
		}},
		AgentQuestion: agentQuestionFacts,
		ItemVersion:   1, InterruptionClass: domain.InterruptionExceptional,
		CreatedAt: &ts, Status: domain.StatusOpen,
	}, approved)
	if err != nil {
		t.Fatal(err)
	}
	blockedKind := domain.BlockedKindOwnerDecision
	blockedOutcome := domain.BlockedOutcome{
		Version: domain.BlockedOutcomeEncodingVersion, Kind: blockedKind,
		Decisions: agentQuestionItem.AgentQuestion.Decisions,
	}

	trustRule := domain.TrustRuleTrustProfileDrift
	publishBlockedItem, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-publish-blocked", ProjectID: "proj-1", Subject: subject,
		Type: domain.AttentionPublishBlocked, Priority: domain.PriorityHigh,
		Reason:            "current trust state blocked publication",
		RequestedDecision: []domain.Action{domain.ActionInspectTrustFailure, domain.ActionStop},
		PublishBlock:      &domain.PublishBlockFacts{TrustRule: &trustRule},
		ItemVersion:       1, InterruptionClass: domain.InterruptionExceptional,
		CreatedAt: &ts, Status: domain.StatusOpen,
	}, approved)
	if err != nil {
		t.Fatal(err)
	}

	reviewDisputeItem, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-review-dispute", ProjectID: "proj-1", Subject: subject,
		Type: domain.AttentionReviewDispute, Priority: domain.PriorityHigh,
		Reason:            "the review finding conflicts with the work contract",
		RequestedDecision: []domain.Action{domain.ActionApprove, domain.ActionDiscuss, domain.ActionStop},
		ReviewDispute: &domain.ReviewDisputeBinding{
			RunID: "run-1", Round: 2, FindingIDs: []domain.FindingID{"finding-1", "finding-2"},
			CompletionEvidence: "sha256:review-completion",
		},
		ItemVersion: 1, InterruptionClass: domain.InterruptionExceptional,
		CreatedAt: &ts, Status: domain.StatusOpen,
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

	// The waived-posture shape (issues #319/#321): a system_health notice
	// carrying the typed condition that supersedes its blocking effect, so the
	// goldens pin the present render of blocking_supersession beside the
	// explicit null the fixtures above keep.
	blockingPosture := domain.HealthPostureBlocking
	supersededItem, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-3", ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: "run-1", RunID: &runID},
		Type:    domain.AttentionSystemHealth, Priority: domain.PriorityNormal,
		Reason:            "unattended execution admitted under the backup-encryption waiver for repository 424242",
		RequestedDecision: []domain.Action{domain.ActionAcknowledge, domain.ActionStopUnattended},
		EvidenceSnapshot:  []domain.Artifact{},
		AgentClaims:       []domain.AgentClaim{},
		ItemVersion:       1,
		InterruptionClass: domain.InterruptionExceptional,
		Posture:           &blockingPosture,
		BlockingSupersession: &domain.BlockingSupersession{
			Kind: domain.SupersessionBackupEncryptionWaiver, RepositoryID: 424242,
		},
		Status: domain.StatusOpen,
	}, approved)
	if err != nil {
		t.Fatal(err)
	}

	advisoryPosture := domain.HealthPostureAdvisory
	advisoryItem, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-4", ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectSystem, ID: "daemon"},
		Type:    domain.AttentionSystemHealth, Priority: domain.PriorityNormal,
		Reason:            "active-resource observation is temporarily unavailable",
		RequestedDecision: []domain.Action{domain.ActionAcknowledge, domain.ActionRunDoctor},
		EvidenceSnapshot:  []domain.Artifact{},
		AgentClaims:       []domain.AgentClaim{},
		ItemVersion:       1,
		InterruptionClass: domain.InterruptionExceptional,
		Posture:           &advisoryPosture,
		HealthDiagnostic: &domain.HealthDiagnostic{
			Code: "run_projection.unavailable", Impairs: domain.ImpairedCapabilityRunVisibility,
		},
		Status: domain.StatusOpen,
	}, approved)
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
		Severity: "P2", Location: &domain.FindingLocation{Path: "daemon/main.go", StartLine: 42, EndLine: 42}, Message: "unchecked error", RawText: "err not handled", CreatedAt: ts,
	}
	reviewRecord, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-run-1-1", RunID: "run-1", Round: 1,
		Provider: "openai", ModelConfiguration: "gpt-5.2-codex/high",
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		InstructionDigest:   domain.Digest("sha256:" + strings.Repeat("d", 64)),
		CostOwner:           "subscription:owner", BaseSHA: "beefcafe", HeadSHA: "cafebabe",
		CompletedAt: ts, CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("e", 64)),
		Outcome: domain.ReviewFindings, FindingIDs: []domain.FindingID{finding.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	shadowReviewRecord, err := domain.NewShadowReviewRecord(domain.ShadowReviewRecord{
		InvocationID: "shadow-review-run-1-1", RunID: "run-1", ShadowedRound: 1,
		Source: domain.ShadowReviewClaudeLocal, Provider: "anthropic",
		ModelConfiguration:  "claude-opus/high",
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("f", 64)),
		InstructionDigest:   domain.Digest("sha256:" + strings.Repeat("d", 64)),
		CostOwner:           "subscription:owner", BaseSHA: "beefcafe", HeadSHA: "cafebabe",
		CompletedAt: ts, CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("a", 64)),
		Outcome: domain.ReviewFindings, FindingIDs: []domain.FindingID{"shadow-find-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	classifierAccuracySample := domain.ClassifierAccuracySample{
		RunID: "run-1", FindingID: "shadow-find-1", ClassificationVersion: 1,
		ShadowInvocationID: shadowReviewRecord.InvocationID,
		Assessment:         domain.ClassifierAssessmentAccurate,
		RecordedAt:         ts.Add(time.Minute),
	}
	reviewDisposition := domain.ReviewDispositionRecord{
		FindingID: finding.ID, RunID: finding.RunID, Round: 1,
		Disposition:        domain.ReviewDispositionDeferred,
		Reason:             "requires a separate hardening unit",
		AdjudicationDigest: domain.Digest("sha256:" + strings.Repeat("a", 64)),
		CreatedAt:          ts,
	}
	initiator := domain.InitiatorConfig{
		Type: domain.InitiatorTypeLabel, Label: "freeside", Mode: domain.InitiatorModePropose,
	}
	manualInitiator := domain.InitiatorConfig{Type: domain.InitiatorTypeManual}
	reviewFailure := domain.ReviewFailure{
		InvocationID: "review-run-1-2", RunID: "run-1", Round: 2,
		BaseSHA: "beefcafe", HeadSHA: "cafebabe", Class: domain.ReviewFailureQuota,
		Reason: "provider quota exhausted", ObservedAt: ts,
	}
	commandID := "command-recover-review-1"
	reviewRecovery := domain.ReviewRecoveryTransition{
		RunID: "run-1", InvocationID: "review-run-1-2", Round: 2,
		BaseSHA: "beefcafe", HeadSHA: "cafebabe",
		FailureDigest: "sha256:review-failure-body", CommandID: &commandID,
		Reason: "operator authorized recovery of the displayed contradiction", OccurredAt: ts,
	}
	configCommandID := "command-adopt-review-configuration-1"
	configRecovery := domain.ReviewConfigurationRecoveryTransition{
		RunID: "run-1", InvocationID: "review-run-1-2", Round: 2,
		BaseSHA: "beefcafe", HeadSHA: "cafebabe",
		FailureDigest: "sha256:review-failure-body",
		Repo:          "owner/repo", RepositoryID: 84958515,
		SupersededProfileDigest:  "sha256:profile-superseded",
		SupersedingProfileDigest: "sha256:profile-superseding",
		CommandID:                &configCommandID,
		Reason:                   "operator adopted the superseding review configuration",
		OccurredAt:               ts,
	}
	recoveryRunID := domain.RunID("run-1")
	recoveryItem, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-review-recovery", ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: "run-1", RunID: &recoveryRunID},
		Type:    domain.AttentionReviewContradiction, Priority: domain.PriorityHigh,
		Reason:            "review contradicted its execution contract",
		RequestedDecision: []domain.Action{domain.ActionRecoverReview},
		PRHeadSHA:         "cafebabe", ReviewRecoveryBinding: &domain.ReviewRecoveryBinding{
			RunID: "run-1", InvocationID: "review-run-1-2", Round: 2,
			BaseSHA: "beefcafe", HeadSHA: "cafebabe", FailureDigest: "sha256:review-failure-body",
		},
		ItemVersion: 1, InterruptionClass: domain.InterruptionExceptional, Status: domain.StatusOpen,
	}, approved)
	if err != nil {
		t.Fatal(err)
	}
	configRecoveryItem, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-review-configuration", ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: "run-1", RunID: &recoveryRunID},
		Type:    domain.AttentionReviewConfiguration, Priority: domain.PriorityHigh,
		Reason:            "review parked on a configuration the trust profile no longer approves",
		RequestedDecision: []domain.Action{domain.ActionAdoptReviewConfiguration, domain.ActionDiscuss, domain.ActionStop},
		PRHeadSHA:         "cafebabe", ReviewConfigurationRecovery: &domain.ReviewConfigurationRecoveryBinding{
			RunID: "run-1", InvocationID: "review-run-1-2", Round: 2,
			BaseSHA: "beefcafe", HeadSHA: "cafebabe", FailureDigest: "sha256:review-failure-body",
			Repo: "owner/repo", RepositoryID: 84958515,
			SupersededProfileDigest: "sha256:profile-superseded",
		},
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
	}, approved)
	if err != nil {
		t.Fatal(err)
	}
	adjudicationConfidence := domain.ConfidenceHigh
	findingAdjudicationItem, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-finding-adjudication", ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: "run-1", RunID: &recoveryRunID},
		Type:    domain.AttentionFindingAdjudication, Priority: domain.PriorityHigh,
		Reason: "a review finding needs an adjudicated route",
		RequestedDecision: []domain.Action{
			domain.ActionAcceptRecommendedRoute, domain.ActionChooseAlternativeRoute,
			domain.ActionDiscuss, domain.ActionStop,
		},
		FindingAdjudication: &domain.FindingAdjudicationBinding{
			RunID: "run-1", Round: 2,
			AdjudicationDigest: domain.Digest("sha256:" + strings.Repeat("d", 64)),
			Proposals: []domain.FindingAdjudicationProposal{{
				FindingID:        "finding-1",
				FindingMessage:   "the finding contradicts the approved work unit",
				FindingLocation:  &domain.FindingLocation{Path: "daemon/example.go", StartLine: 12, EndLine: 12},
				Producer:         domain.AdjudicationProducerModel,
				GoalRelationship: domain.GoalContradictory, Route: domain.RouteDecline,
				Rationale:     "the finding contradicts the approved work unit",
				Evidence:      []string{"the reported change lies outside the declared work-unit paths"},
				CitedRules:    []string{"AGENTS.md: stay focused"},
				Assumptions:   []string{"the reported path is accurate"},
				OpenQuestions: []string{"Should a follow-up issue be filed?"},
				Confidence:    &adjudicationConfidence,
				OfferedAlternatives: []domain.OfferedAlternative{{
					Route:       domain.RouteDispute,
					Consequence: "ask a human to resolve the contract conflict",
				}},
			}},
		},
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
	}, approved)
	if err != nil {
		t.Fatal(err)
	}
	reenrollmentBinding := domain.CodexReenrollmentRecoveryBinding{
		AuthIdentityID: "codex-primary", LeaseFence: 4,
		AuthStoreDigest:      "sha256:replacement-store",
		AccessTokenExpiresAt: ts.Add(24 * time.Hour),
	}
	reenrollmentItem, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-codex-reenrollment", ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectSystem, ID: "daemon"},
		Type:    domain.AttentionSystemHealth, Priority: domain.PriorityHigh,
		Reason:                           "Codex identity requires verified re-enrollment",
		RequestedDecision:                []domain.Action{domain.ActionAcknowledge, domain.ActionResolveReenrollment},
		CodexReenrollmentRecoveryBinding: &reenrollmentBinding,
		ItemVersion:                      2, InterruptionClass: domain.InterruptionExceptional,
		Posture: &advisoryPosture, Status: domain.StatusOpen,
	}, approved)
	if err != nil {
		t.Fatal(err)
	}
	reenrollmentCommandID := "command-resolve-reenrollment-1"
	reenrollmentTransition := domain.CodexReenrollmentRecoveryTransition{
		AuthIdentityID:       reenrollmentBinding.AuthIdentityID,
		LeaseFence:           reenrollmentBinding.LeaseFence,
		AuthStoreDigest:      reenrollmentBinding.AuthStoreDigest,
		AccessTokenExpiresAt: reenrollmentBinding.AccessTokenExpiresAt,
		CommandID:            &reenrollmentCommandID,
		Reason:               "operator resolved the verified Codex re-enrollment",
		OccurredAt:           ts,
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
		RepositoryID:               123456789,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        "sha256:workflow-audit",
		Review: domain.ReviewSettings{
			Mode: domain.ReviewFreesideInvoked, ConfigDigest: "sha256:review-config",
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
		EvidenceSnapshotDigest:   "sha256:evidence-snapshot",
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
		EvidenceSnapshotDigest:   "sha256:evidence-snapshot",
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

	// The advisory variant carries the one advisory category (reviewer
	// instructions, plan §5.8 revision 42): surfaced, never waived, and
	// authorizing on its own.
	advisoryAuthorization, err := domain.NewCandidateAuthorization(domain.CandidateAuthorizationInput{
		Repo: "freeside-ai/demo", BaseSHA: "beefcafe", HeadSHA: "cafebabe",
		ImportResultDigest:       "sha256:import-result",
		VerificationRecipeDigest: recipe,
		EvidenceSnapshotDigest:   "sha256:evidence-snapshot",
		VerificationOutcome:      domain.VerificationPassed,
		Findings: []domain.CandidateFinding{
			{
				Class: domain.FindingClassControlPlane, Category: &controlPlaneCategory,
				Origin: domain.FindingOriginImport, Kind: "reviewer_instruction_path",
				Path: "AGENTS.md", Disposition: domain.DispositionAdvisory,
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
	run := domain.Run{
		ID: "run-1", ProjectID: "proj-1", SpecDigest: "sha256:spec", PolicyDigest: resolvedPolicy.Digest,
		CampaignID: "campaign-1", AttemptNumber: 2,
		AttemptReason: "Retry after repairing the acceptance rig", ParentRunID: "run-0",
		Stages: []domain.Stage{stage},
	}
	productionAttempt := domain.ProductionAttempt{
		CampaignID: "campaign-1", AttemptNumber: 2, Kind: domain.ProductionAttemptRetry,
		Reason: "Retry after repairing the acceptance rig", ParentRunID: "run-0",
		SourceDigest: "sha256:source", ApprovedSpecDigest: "sha256:spec",
		SpecificationRunID: "run-specification-1", ImplementationRunID: "run-1",
	}

	// The provider identity the stage below runs under, and a live lease on
	// its auth store. The identity carries the narrowed §5.4 shape: account
	// and operator fields on the identity, the interim client facts under
	// Interim until the #867 adoption moves them onto an enrollment.
	identity := domain.AuthIdentity{
		ID: "auth-claude-owner", Provider: "claude",
		AccountBinding: "acct-claude-owner-7f3a", UsagePool: "claude-owner-subscription",
		Budget: 500_000, AuthStoreMutationLease: true,
		MaxParallelExecutions: 1, Enabled: true, CostOwner: "owner",
		Interim: domain.InterimClientFacts{AuthStoreVolume: "claude-owner-credentials", RefreshStrategy: domain.RefreshOnDemand},
	}
	mutationLease := domain.AuthStoreMutationLease{
		AuthIdentityID: identity.ID, Holder: "inv-1", Fence: 1,
		AcquiredAt: ts, ExpiresAt: ts.Add(5 * time.Minute),
	}

	// One enrolled harness client on that identity, with the newest entry of
	// its append-only store history. The Claude setup token observes no
	// expiry, so the generation's token_expiry is the explicit null §5.4
	// admits by design; the lease above carries the binding its fence guards.
	enrollment := domain.ClientEnrollment{
		ID: "enroll-claude-owner-cli", AuthIdentityID: identity.ID,
		HarnessClient: domain.HarnessClientClaudeCode, Route: "anthropic_claude_subscription",
		AuthMethod:      domain.AuthMethodSetupToken,
		CredentialMode:  domain.CredentialSubscriptionContained,
		RefreshStrategy: domain.RefreshOnDemand, SupportsReadOnlyAuthSnapshot: true,
		AccountBinding: identity.AccountBinding,
	}
	enrollmentGeneration := domain.EnrollmentGeneration{
		EnrollmentID: enrollment.ID, Ordinal: 3,
		AuthStoreVolume:     "claude-owner-cli-store",
		StoreManifestDigest: stageDigest("9"),
		LeaseFence:          1,
		AccountBinding:      identity.AccountBinding,
		RecordedAt:          ts,
	}
	boundLease := mutationLease
	boundLease.GenerationBinding = &domain.LeaseGenerationBinding{
		EnrollmentID: enrollment.ID, Generation: enrollmentGeneration.Ordinal,
		AuthStoreVolume:     enrollmentGeneration.AuthStoreVolume,
		StoreManifestDigest: enrollmentGeneration.StoreManifestDigest,
	}

	// The admitted-agent configuration objects (§5.4): fragments, the
	// stage-owned launch, and a resolved agent. The digests inside these
	// goldens pin the canonical encodings — a field-order or encoding change
	// moves them visibly.
	goldenRoute := routeFragment(t)
	goldenAdapter := adapterFragment(t)
	goldenOffer := offerFragment(t)
	goldenLaunch := launchSpec(t)
	agentIdentity := domain.AuthIdentity{
		ID: "auth-openai-A", Provider: "openai", AccountBinding: "acct-openai-a",
		AuthStoreMutationLease: true, MaxParallelExecutions: 1, Enabled: true,
	}
	agentEnrollment := domain.ClientEnrollment{
		ID: "enroll-openai-A-codex", AuthIdentityID: agentIdentity.ID,
		HarnessClient: domain.HarnessClientCodexCLI, Route: "openai_chatgpt_codex",
		AuthMethod:      domain.AuthMethodOAuth,
		CredentialMode:  domain.CredentialSubscriptionContained,
		RefreshStrategy: domain.RefreshOnDemand, SupportsReadOnlyAuthSnapshot: true,
		AccountBinding: agentIdentity.AccountBinding,
	}
	goldenAgent, err := domain.ResolveAgentDefinition(domain.AgentResolutionInput{
		Source: domain.AgentSource{
			Name: "sol-via-codex", Enrollment: "openai-chatgpt-A/codex",
			Route: "openai_chatgpt_codex", Adapter: "codex_proto_v1",
			Offer: "gpt-5.6-sol", Effort: domain.EffortMax,
		},
		Identity: agentIdentity, Enrollment: agentEnrollment,
		Route: goldenRoute, Adapter: goldenAdapter, Offer: goldenOffer,
		OfferRoute: "openai_chatgpt_codex",
	})
	if err != nil {
		t.Fatal(err)
	}

	// The durable execution record for that attempt. Capabilities are passed
	// unsorted to exercise the constructor's canonicalization.
	authIdentity := identity.ID
	conversationDigest := stageDigest("7")
	vendorDigest := stageDigest("8")
	stageInputs, err := domain.NewStageInputSnapshot(domain.StageInputSnapshotInput{
		InputDigest:         stageDigest("1"),
		SpecificationDigest: stageDigest("2"),
		PromptPackageDigest: stageDigest("3"),
		PolicyDigest:        resolvedPolicy.Digest,
		VendorInstructions: &domain.VendorInstructionSnapshot{
			Vendor:   domain.AgentVendorClaude,
			Delivery: domain.VendorInstructionDeliveryAppendFile,
			Digest:   &vendorDigest,
		},
		ConversationDigest:   &conversationDigest,
		PriorArtifactDigests: []domain.Digest{stageDigest("4"), stageDigest("5")},
		ImageInputDigests:    []domain.Digest{stageDigest("6")},
	})
	if err != nil {
		t.Fatal(err)
	}
	codexStageInput := stageInputs
	codexVendorInstructions := *stageInputs.VendorInstructions
	codexVendorInstructions.Vendor = domain.AgentVendorCodex
	codexStageInput.VendorInstructions = &codexVendorInstructions
	codexStageInput.ID, err = codexStageInput.ComputeID()
	if err != nil {
		t.Fatal(err)
	}
	if err := codexStageInput.Validate(); err != nil {
		t.Fatal(err)
	}
	admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: "inv-1", RunID: "run-1", StageID: "stage-1", AttemptID: "attempt-1",
		Backend: "fresh_vm_read_only_volume_handoff",
		Capabilities: domain.CapabilitySnapshot{
			domain.CapPostExitExport, domain.CapDetachableWorkspace, domain.CapReadOnlyRemount,
		},
		OperatingMode:  domain.ModeAttendedDev,
		CredentialMode: domain.CredentialSubscriptionContained,
		EgressProfile:  domain.EgressProviderOnly,
		ImageRef:       domain.ImageRef("ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("ab", 32)),
		SpecDigest:     stageDigest("2"), PolicyDigest: resolvedPolicy.Digest, InputDigest: stageDigest("1"),
		StageInputs:    &stageInputs,
		Base:           domain.BaseRevision{Repo: "owner/repo", RepositoryID: 424242, BaseRef: "refs/heads/main", BaseSHA: "deadbeef"},
		Workspace:      "freeside-handoff-run-1-ws",
		AuthIdentityID: &authIdentity,
		AdmittedAt:     ts,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The agent-bound (v4) variant: the §5.4 admission step 5 snapshot rides
	// beside the existing fields, and its presence selects the new encoding
	// version, which this golden pins through the changed content address.
	agentIdentityID := agentIdentity.ID
	agentAdmission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: "inv-3", RunID: "run-1", StageID: "stage-1", AttemptID: "attempt-3",
		Backend: "fresh_vm_read_only_volume_handoff",
		Capabilities: domain.CapabilitySnapshot{
			domain.CapPostExitExport, domain.CapDetachableWorkspace,
		},
		OperatingMode:  domain.ModeAttendedDev,
		CredentialMode: domain.CredentialSubscriptionContained,
		EgressProfile:  domain.EgressProviderOnly,
		ImageRef:       domain.ImageRef("ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("ab", 32)),
		SpecDigest:     stageDigest("2"), PolicyDigest: resolvedPolicy.Digest, InputDigest: stageDigest("1"),
		Base:           domain.BaseRevision{Repo: "owner/repo", RepositoryID: 424242, BaseRef: "refs/heads/main", BaseSHA: "deadbeef"},
		Workspace:      "freeside-handoff-run-1-ws",
		StageInputs:    &codexStageInput,
		AuthIdentityID: &agentIdentityID,
		AgentBinding: &domain.AdmissionAgentBinding{
			AgentDigest:          goldenAgent.Digest,
			LaunchDigest:         goldenLaunch.Digest,
			TreatmentDigest:      stageDigest("d"),
			PricingRevision:      goldenOffer.PricingRevision,
			LineupRevision:       stageDigest("c"),
			EnrollmentID:         agentEnrollment.ID,
			EnrollmentGeneration: 2,
			StoreManifestDigest:  stageDigest("9"),
			EffectiveEgress:      goldenRoute.InferenceAuthorities,
			Attended:             true,
		},
		AdmittedAt: ts,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapterConformance := adapterConformanceRecord(t)

	waivedProfileDigest := domain.Digest("sha256:trust-profile-v1")
	// The waived, unattended variant: both pointer-for-optional branches
	// (waiver present, auth identity absent under clean verification) render
	// explicitly, so neither discriminator goes unpinned.
	waivedAdmission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: "inv-2", RunID: "run-1", StageID: "stage-1", AttemptID: "attempt-2",
		Backend:                    "fresh_vm_read_only_volume_handoff",
		BackendConfigurationDigest: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		Capabilities:               domain.CapabilitySnapshot(domain.AllRunnerCapabilities),
		OperatingMode:              domain.ModeUnattended,
		CredentialMode:             domain.CredentialSubscriptionContained,
		EgressProfile:              domain.EgressCleanVerification,
		ImageRef:                   domain.ImageRef("ghcr.io/freeside-ai/verifier@sha256:" + strings.Repeat("cd", 32)),
		SpecDigest:                 stageDigest("2"), PolicyDigest: resolvedPolicy.Digest, InputDigest: stageDigest("1"),
		StageInputs:        &stageInputs,
		Base:               domain.BaseRevision{Repo: "owner/repo", RepositoryID: 424242, BaseRef: "refs/heads/main", BaseSHA: "deadbeef"},
		Workspace:          "freeside-handoff-run-1-ws",
		TrustProfileDigest: &waivedProfileDigest,
		BackupEncryptionWaiver: &domain.BackupEncryptionWaiver{
			RepositoryID: 424242, Reason: "phase 1a.2 supervised runs (plan §5.7)",
		},
		AdmittedAt: ts,
	})
	if err != nil {
		t.Fatal(err)
	}

	evidenceManifest := domain.Digest("sha256:evidence-manifest")
	export, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: "inv-1", AdmissionID: admission.ID,
		ObservedBaseSHA: "deadbeef", HeadSHA: "cafebabe",
		ManifestDigest:         "sha256:change-manifest",
		EvidenceManifestDigest: &evidenceManifest,
		CommitPlanPresent:      true,
		RecordedAt:             ts.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	currentImportStart := domain.CurrentImportStart{
		InvocationID: admission.InvocationID,
		AdmissionID:  admission.ID,
	}

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
	projectImage, err := domain.NewProjectImage(domain.ProjectImageInput{
		Repository: "freeasinbird/gh-imgup", RepositoryID: 1278475858,
		CommitSHA:          "6ab4e3dff2be53f74bde9b8b3150290775152f9f",
		RecipeDigest:       domain.Digest("sha256:" + strings.Repeat("ef", 32)),
		PreparationCommand: []string{"/usr/local/bin/freeside-project-prepare"},
		BaseImageRef: domain.ImageRef("ghcr.io/freeside-ai/agent-claude@sha256:" +
			strings.Repeat("ab", 32)),
		ImageRef: domain.ImageRef("127.0.0.1:5100/freeside-project-freeasinbird-gh-imgup@sha256:" +
			strings.Repeat("cd", 32)),
	})
	if err != nil {
		t.Fatal(err)
	}

	project, err := domain.NewProject("project-alpha", "freeasinbird/gh-imgup", 1278475858)
	if err != nil {
		t.Fatal(err)
	}

	conformanceCeiling, ok := domain.ProvableCapabilities(domain.BackendFreshVMReadOnlyVolumeHandoff)
	if !ok {
		t.Fatal("fresh-vm class has no registered ceiling")
	}
	backendConformance, err := domain.NewBackendConformance(domain.BackendConformanceInput{
		Backend:             domain.BackendFreshVMReadOnlyVolumeHandoff,
		Outcome:             domain.ConformancePassed,
		ConfigurationDigest: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		Capabilities:        conformanceCeiling,
		ProvedAt:            time.Date(2026, 7, 27, 8, 30, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	// The store stamps the generation at append; the golden pins the
	// reconstructed render.
	backendConformance.Generation = 7
	failedConformance, err := domain.NewBackendConformance(domain.BackendConformanceInput{
		Backend:             domain.BackendFreshVMReadOnlyVolumeHandoff,
		Outcome:             domain.ConformanceFailed,
		ConfigurationDigest: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		ProvedAt:            time.Date(2026, 7, 27, 9, 15, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	failedConformance.Generation = 8
	supersededConformance, err := domain.NewBackendConformance(domain.BackendConformanceInput{
		Backend:             domain.BackendFreshVMReadOnlyVolumeHandoff,
		Outcome:             domain.ConformanceSuperseded,
		ConfigurationDigest: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		ProvedAt:            time.Date(2026, 7, 27, 9, 20, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	supersededConformance.Generation = 9
	restoreTestedAt := ts.Add(2 * time.Hour)
	backupCheckpoint := domain.BackupCheckpoint{
		CheckpointID:           "0123456789abcdef0123456789abcdef",
		SyncEpoch:              "abcdef0123456789abcdef0123456789",
		ServerRevision:         42,
		SQLiteSnapshotDigest:   "sha256:snapshot",
		ArtifactManifestDigest: "sha256:artifact-manifest",
		CreatedAt:              ts,
		CompletedAt:            ts.Add(time.Hour),
		RestoreTestedAt:        &restoreTestedAt,
	}

	// Run observation fixtures (#394): the milestone timeline, hold, and
	// liveness shapes an operator client consumes. The blocked milestone and
	// the hold observation pin the closed reason-code render; the plain
	// milestone pins the explicit-null render of every kind-scoped detail
	// pointer.
	obsInvocationID := domain.InvocationID("inv-1")
	submittedMilestone := domain.RunMilestone{
		RunID: "run-1", Kind: domain.MilestoneRunSubmitted,
		InvocationID: &obsInvocationID, RecordedAt: ts,
	}
	observedTerminal := domain.ObservedStatusFailed
	terminalMilestone := domain.RunMilestone{
		RunID: "run-1", Kind: domain.MilestoneTerminalRecorded,
		InvocationID: &obsInvocationID, Terminal: &observedTerminal,
		RecordedAt: ts.Add(10 * time.Minute),
	}
	observedOutcome := domain.ExecutionOutcomeFailed
	outcomeMilestone := domain.RunMilestone{
		RunID: "run-1", Kind: domain.MilestoneExecutionOutcomeRecorded,
		InvocationID: &obsInvocationID, Outcome: &observedOutcome,
		RecordedAt: ts.Add(9 * time.Minute),
	}
	blockedReason := domain.HoldBaseAdvanced
	blockedMilestone := domain.RunMilestone{
		RunID: "run-1", Kind: domain.MilestonePublicationBlocked,
		InvocationID: &obsInvocationID, Reason: &blockedReason,
		RecordedAt: ts.Add(11 * time.Minute),
	}
	invocationObservation := domain.InvocationObservation{
		InvocationID: "inv-1", RunID: "run-1",
		Status: domain.ObservedStatusRunning, Live: true,
		ObservedAt: ts.Add(3 * time.Minute),
	}
	holdObservation := domain.RunHoldObservation{
		RunID: "run-1", InvocationID: &obsInvocationID,
		Reason:          domain.HoldOperationStopped,
		FirstObservedAt: ts.Add(time.Minute),
		LastObservedAt:  ts.Add(2 * time.Minute),
	}
	runObservation := domain.RunObservation{
		RunID:      "run-1",
		Milestones: []domain.RunMilestone{submittedMilestone, outcomeMilestone, terminalMilestone},
		Hold:       &holdObservation,
		Invocations: []domain.InvocationObservation{
			invocationObservation,
		},
	}

	// A ready item carrying the base-advance watch's maintained fact
	// (§5.16): the api AttentionItem example's base_freshness branch.
	freshItem := item
	freshItem.ItemVersion = item.ItemVersion + 1
	freshItem.BaseFreshness = &domain.BaseFreshness{
		BaseRef: "main", AdmittedBaseSHA: "cafebabe", ObservedBaseSHA: "deadbeef",
		Advanced: true, ObservedAt: ts.Add(45 * time.Minute),
	}
	if err := freshItem.Validate(); err != nil {
		t.Fatal(err)
	}

	// A ready item superseded because its earned-against head moved (§7, issue
	// #496): the daemon records the readiness_invalidation fact in the same
	// transition that supersedes it, so the api example's
	// readiness_invalidation branch is pinned beside the superseded status.
	invalidatedItem := item
	invalidatedItem.ItemVersion = item.ItemVersion + 1
	invalidatedItem.Status = domain.StatusSuperseded
	invalidatedItem.ReadinessInvalidation = &domain.ReadinessInvalidation{
		Reason: domain.ReadinessInvalidationHeadChanged,
		Bound:  "cafebabe", Observed: "feedface",
		ObservedAt: ts.Add(50 * time.Minute),
	}
	if err := invalidatedItem.Validate(); err != nil {
		t.Fatal(err)
	}
	identityInvalidatedItem := item
	identityInvalidatedItem.ItemVersion = item.ItemVersion + 1
	identityInvalidatedItem.Status = domain.StatusSuperseded
	identityInvalidatedItem.ReadinessInvalidation = &domain.ReadinessInvalidation{
		Reason: domain.ReadinessInvalidationIdentityChanged,
		Bound:  "84958515#450", Observed: "84958516#451",
		ObservedAt: ts.Add(51 * time.Minute),
	}
	if err := identityInvalidatedItem.Validate(); err != nil {
		t.Fatal(err)
	}

	// Durable-scheduler fixtures (§5.16, #442): one schedule per shape class
	// (a one-shot deadline, the base-advance watch with its kind-scoped
	// detail, the expiring installation poll, a permanent trusted-config job,
	// and a concluded deadline), plus an occurrence pair and the trusted
	// event. The api/openapi.yaml examples are lifted from these.
	itemID := domain.ItemID("item-1")
	itemVersion := 1
	scheduleRunID := domain.RunID("run-1")
	schedulePolicyDigest := resolvedPolicy.Digest
	deadlineAt := ts.Add(30 * time.Minute)
	deadlineSchedule, err := domain.NewSchedule(domain.ScheduleInput{
		ID: "schedule-pr_checks_deadline-item-1", ProjectID: "project-1",
		Kind: domain.SchedulePRChecksDeadline,
		Subject: domain.ScheduleSubject{
			Type: domain.ScheduleSubjectAttentionItem, ItemID: &itemID, ItemVersion: &itemVersion,
		},
		RunID: &scheduleRunID, PolicyDigest: &schedulePolicyDigest,
		CreatedAt: ts, FireAt: &deadlineAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	watchInterval := int64(900)
	watchSchedule, err := domain.NewSchedule(domain.ScheduleInput{
		ID: "schedule-base_advance_watch-item-1", ProjectID: "project-1",
		Kind: domain.ScheduleBaseAdvanceWatch,
		Subject: domain.ScheduleSubject{
			Type: domain.ScheduleSubjectAttentionItem, ItemID: &itemID, ItemVersion: &itemVersion,
		},
		RunID: &scheduleRunID, PolicyDigest: &schedulePolicyDigest,
		CreatedAt: ts, IntervalSeconds: &watchInterval,
		BaseWatch: &domain.ScheduleBaseWatch{
			Repo: "owner/repo", BaseRef: "main", AdmittedBaseSHA: "cafebabe",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	registrationID := int64(4385298)
	activeEpoch := int64(1)
	intentRevision := int64(2)
	pollInterval := int64(2)
	pollExpiry := ts.Add(10 * time.Minute)
	pollSchedule, err := domain.NewSchedule(domain.ScheduleInput{
		ID: "schedule-installation_poll-4385298", ProjectID: "project-system",
		Kind: domain.ScheduleInstallationPoll,
		Subject: domain.ScheduleSubject{
			Type:           domain.ScheduleSubjectInstallationIntent,
			RegistrationID: &registrationID, ActiveEpoch: &activeEpoch,
			DurableIntentRevision: &intentRevision,
		},
		CreatedAt: ts, IntervalSeconds: &pollInterval, ExpiresAt: &pollExpiry,
	})
	if err != nil {
		t.Fatal(err)
	}
	janitorInterval := int64(30)
	janitorSchedule, err := domain.NewSchedule(domain.ScheduleInput{
		ID: "schedule-janitor", ProjectID: "project-system",
		Kind:      domain.ScheduleJanitor,
		Subject:   domain.ScheduleSubject{Type: domain.ScheduleSubjectTrustedConfig},
		CreatedAt: ts, IntervalSeconds: &janitorInterval,
	})
	if err != nil {
		t.Fatal(err)
	}
	firedSchedule, err := deadlineSchedule.Concluded(
		domain.ScheduleFired, domain.ResolutionDeadlineElapsed, ts.Add(31*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	pendingOccurrence := domain.ScheduleOccurrence{
		ScheduleID: deadlineSchedule.ID, Generation: deadlineSchedule.Generation,
		NominalFireAt: deadlineAt, Status: domain.OccurrencePending,
		CreatedAt: ts.Add(31 * time.Minute),
		Gap: &domain.ScheduleFireGap{
			MissedOccurrences: 1, EarliestMissedAt: deadlineAt.Add(-time.Minute),
		},
	}
	occurrenceConsumedAt := ts.Add(32 * time.Minute)
	consumedOutcome := domain.OutcomeHandled
	consumedOccurrence := pendingOccurrence
	consumedOccurrence.Status = domain.OccurrenceConsumed
	consumedOccurrence.ConsumedAt = &occurrenceConsumedAt
	consumedOccurrence.Outcome = &consumedOutcome
	scheduleEvent, err := domain.NewScheduleEvent(
		deadlineSchedule, pendingOccurrence, ts.Add(31*time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	boundIssue := 443
	unitDeclaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
		CompletionCriterion: domain.CompletionBoundIssueClosedByMergedPR,
		BoundIssue:          &boundIssue,
		DependsOnIssues:     []int{440, 442},
		DeclaredPaths:       []string{"daemon/", "devlog/"},
		ContractSerialized:  true,
	}, "run-1", "project-1", ts)
	if err != nil {
		t.Fatal(err)
	}
	unitPRBinding := domain.WorkUnitPRBinding{
		UnitID: unitDeclaration.ID, Repo: "owner/repo", RepositoryID: 84958515,
		PRNumber: 450, BaseRef: "main", HeadSHA: "cafebabe",
		RecordedAt: ts.Add(time.Hour),
	}
	readyItemPRBinding := domain.ReadyItemPRBinding{
		ItemID: "item-ready-1", RunID: "run-1", ProducingInvocationID: "inv-1",
		PublicationInvocationID: "publish-production-run-1",
		PublicationIdentity:     "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Repo:                    "owner/repo",
		RepositoryID:            84958515, PRNumber: 450, BaseRef: "main",
		HeadSHA: "cafebabe", RecordedAt: ts.Add(time.Hour),
	}
	pullMergeFact := domain.PullMergeFact{
		Repo: "owner/repo", RepositoryID: 84958515, PRNumber: 450,
		State: domain.PullRequestClosed, Merged: true,
		MergeCommitSHA: "deadbeef", BaseRef: "main", HeadSHA: "cafebabe",
		ObservedAt: ts.Add(2 * time.Hour),
	}
	issueStateFact := domain.IssueStateFact{
		Repo: "owner/repo", RepositoryID: 84958515, IssueNumber: 443,
		State: domain.IssueClosed, ClosedByCommitSHA: "deadbeef",
		ObservedAt: ts.Add(2 * time.Hour),
	}
	nativeReviewObservation := domain.NativeReviewObservation{
		Repo: "owner/repo", RepositoryID: 84958515, PRNumber: 450,
		Provider: domain.NativeReviewCodexGitHub, Kind: domain.NativeReviewFindings,
		NativeID: 900100, AuthorLogin: "chatgpt-codex-connector",
		ReviewCommitSHA: "cafebabe", ReviewState: "COMMENTED", BindingHeadSHA: "cafebabe",
		SubmittedAt: ts.Add(2 * time.Hour), ObservedAt: ts.Add(3 * time.Hour),
		Findings: []domain.Finding{{
			ID: "native-comment-800200", RunID: "run-1", Source: "codex_github",
			Severity: "P2", Location: &domain.FindingLocation{Path: "daemon/main.go", StartLine: 42, EndLine: 42},
			Message: "unchecked error", RawText: "P2: the error return is dropped",
			CreatedAt: ts.Add(2 * time.Hour),
		}},
	}
	nativeReviewCleanPass := domain.NativeReviewObservation{
		Repo: "owner/repo", RepositoryID: 84958515, PRNumber: 450,
		Provider: domain.NativeReviewCodexGitHub, Kind: domain.NativeReviewCleanPass,
		NativeID: 700300, AuthorLogin: "chatgpt-codex-connector",
		BindingHeadSHA: "cafebabe",
		SubmittedAt:    ts.Add(2 * time.Hour), ObservedAt: ts.Add(3 * time.Hour),
	}
	unitCompletion, completed := domain.EvaluateWorkUnitCompletion(
		unitDeclaration, unitPRBinding, pullMergeFact, &issueStateFact)
	if !completed {
		t.Fatal("golden completion fixture did not evaluate as completed")
	}

	// Label-intake occurrence fixtures (issue #720). The admitted occurrence
	// derives its admission key from its own coordinates, so a valid fixture
	// sets it from ProposalAdmissionKey rather than a hand-written literal.
	intakeFreshOccurrence, err := domain.NewIntakeOccurrence("owner/repo", 42, 7, "freeside", 1, ts)
	if err != nil {
		t.Fatal(err)
	}
	intakeIssueSource := domain.SpecificationSource{
		Kind:         domain.SpecificationSourceIssueSubject,
		IssueSubject: &domain.IssueSubjectRef{Repo: "owner/repo", RepositoryID: 42, IssueNumber: 7},
	}
	intakeSpecSource := domain.SpecificationSource{
		Kind: domain.SpecificationSourceWorkItemArtifact, WorkItemArtifactID: "spec-art-1",
	}
	intakeAdmittedOccurrence := domain.IntakeOccurrence{
		Repo: "owner/repo", RepositoryID: 42, IssueNumber: 7, Label: "freeside",
		Ordinal: 2, State: domain.IntakeOccurrenceAbsent,
		Admission: &domain.IntakeAdmission{
			ProposalInstanceID: "proposal-1",
			ProposalDigest:     domain.Digest(contentaddr.Sum([]byte("intake-proposal"))),
			Subject: domain.IntakeSubjectBinding{
				ProjectID: "proj-1", SpecificationRunID: "run-spec-1",
				WorkUnitID:           domain.WorkUnitIDForRun("run-spec-1"),
				PolicyArtifactID:     "policy-art-1",
				PolicyArtifactDigest: domain.Digest(contentaddr.Sum([]byte("intake-resolved-policy"))),
				ResolvedPolicyDigest: domain.Digest(contentaddr.Sum([]byte("intake-resolved-policy"))),
				Source:               intakeIssueSource,
			},
		},
		Refusal:      &domain.IntakeStartRefusal{Reason: domain.IntakeRefusalWIPCapExhausted, RecordedAt: ts},
		Supersession: &domain.IntakeSupersession{Reason: domain.IntakeSupersededLabelRemoved, RecordedAt: ts},
		RecordedAt:   ts,
	}
	intakeAdmittedOccurrence.Admission.AdmissionKey = intakeAdmittedOccurrence.ProposalAdmissionKey()
	findingSurface, err := domain.NewDecisionSurface(findingAdjudicationItem)
	if err != nil {
		t.Fatal(err)
	}
	recommendationSource, err := domain.NewRecommendationSourceRecord(domain.RecommendationSourceRecord{
		ItemID: findingAdjudicationItem.ID, Source: domain.RecommendationAgentJudgment,
		Provenance: domain.RecommendationProvenance{AgentJudgment: &domain.AgentJudgmentRecommendationProvenance{
			JudgmentSite:   domain.JudgmentSiteFindingAdjudicator,
			InvocationID:   "review-run-1-2",
			ArtifactDigest: findingAdjudicationItem.FindingAdjudication.AdjudicationDigest,
		}},
		Action:                domain.ActionAcceptRecommendedRoute,
		Reason:                domain.FindingAdjudicatorRecommendationReason,
		DecisionSurfaceDigest: findingSurface.Digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	specRevisionItem := mustItem(t, specRevisionInput(t))

	cases := []struct {
		name  string
		value any
	}{
		{"intake_occurrence", intakeFreshOccurrence},
		{"intake_occurrence_admitted", intakeAdmittedOccurrence},
		{"specification_source_work_item", intakeSpecSource},
		{"specification_source_issue", intakeIssueSource},
		{"attention_item_base_freshness", freshItem},
		{"attention_item_readiness_invalidation", invalidatedItem},
		{"attention_item_identity_changed", identityInvalidatedItem},
		{"schedule_deadline", deadlineSchedule},
		{"schedule_base_advance_watch", watchSchedule},
		{"schedule_installation_poll", pollSchedule},
		{"schedule_janitor", janitorSchedule},
		{"schedule_fired", firedSchedule},
		{"schedule_occurrence_pending", pendingOccurrence},
		{"schedule_occurrence_consumed", consumedOccurrence},
		{"schedule_event", scheduleEvent},
		{"attention_item", item},
		{"decision_surface", decisionSurface},
		{"recommendation_source_record", recommendationSource},
		{"attention_item_review_diminishing_yield", diminishingItem},
		{"attention_item_readiness_degraded", degradedItem},
		{"attention_item_blocked", blockedItem},
		{"attention_item_execution_failure", executionFailureItem},
		{"attention_item_publish_blocked", publishBlockedItem},
		{"attention_item_review_dispute", reviewDisputeItem},
		{"attention_item_spec_revision", specRevisionItem},
		{"attention_item_agent_question", agentQuestionItem},
		{"blocked_outcome", blockedOutcome},
		{"attention_item_decided", decidedItem},
		{"attention_item_superseded", supersededItem},
		{"attention_item_advisory", advisoryItem},
		{"attention_item_review_recovery", recoveryItem},
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
		{"review_record", reviewRecord},
		{"shadow_review_record", shadowReviewRecord},
		{"classifier_accuracy_sample", classifierAccuracySample},
		{"review_disposition_record", reviewDisposition},
		{"review_failure", reviewFailure},
		{"review_recovery_transition", reviewRecovery},
		{"review_configuration_recovery_transition", configRecovery},
		{"attention_item_review_configuration", configRecoveryItem},
		{"attention_item_finding_adjudication", findingAdjudicationItem},
		{"attention_item_codex_reenrollment", reenrollmentItem},
		{"review_yield_history", yieldHistory},
		{"codex_reenrollment_recovery_transition", reenrollmentTransition},
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
		{"candidate_authorization_advisory", advisoryAuthorization},
		{"run", run},
		{"production_attempt", productionAttempt},
		{"initiator_config", initiator},
		{"initiator_config_manual", manualInitiator},
		{"stage", stage},
		{"attempt", attempt},
		{"auth_identity", identity},
		{"auth_store_mutation_lease", mutationLease},
		{"auth_store_mutation_lease_bound", boundLease},
		{"client_enrollment", enrollment},
		{"enrollment_generation", enrollmentGeneration},
		{"route_fragment", goldenRoute},
		{"adapter_fragment", goldenAdapter},
		{"offer_fragment", goldenOffer},
		{"launch_spec", goldenLaunch},
		{"agent_definition", goldenAgent},
		{"stage_input_snapshot", stageInputs},
		{"stage_input_snapshot_codex", codexStageInput},
		{"execution_admission", admission},
		{"execution_admission_waived", waivedAdmission},
		{"execution_admission_agent", agentAdmission},
		{"adapter_conformance", adapterConformance},
		{"execution_export", export},
		{"current_import_start", currentImportStart},
		{"project_image", projectImage},
		{"project", project},
		{"backend_conformance", backendConformance},
		{"backend_conformance_failed", failedConformance},
		{"backend_conformance_superseded", supersededConformance},
		{"backup_checkpoint", backupCheckpoint},
		{"run_milestone", submittedMilestone},
		{"run_milestone_terminal", terminalMilestone},
		{"run_milestone_outcome", outcomeMilestone},
		{"run_milestone_blocked", blockedMilestone},
		{"invocation_observation", invocationObservation},
		{"run_hold_observation", holdObservation},
		{"run_observation", runObservation},
		{"work_unit_declaration", unitDeclaration},
		{"work_unit_pr_binding", unitPRBinding},
		{"ready_item_pr_binding", readyItemPRBinding},
		{"pull_merge_fact", pullMergeFact},
		{"issue_state_fact", issueStateFact},
		{"native_review_observation", nativeReviewObservation},
		{"native_review_clean_pass", nativeReviewCleanPass},
		{"work_unit_completion", unitCompletion},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value := tc.value
			if item, ok := value.(domain.AttentionItem); ok {
				surface, err := domain.NewDecisionSurface(item)
				if err != nil {
					t.Fatalf("derive %q decision surface: %v", tc.name, err)
				}
				item.DecisionSurface = domain.DecisionSurfaceRef{Epoch: surface.Epoch, Digest: surface.Digest}
				if item.Type == domain.AttentionFindingAdjudication {
					item.Recommendation = &domain.Recommendation{
						Action:     domain.ActionAcceptRecommendedRoute,
						Reason:     domain.FindingAdjudicatorRecommendationReason,
						Source:     domain.RecommendationAgentJudgment,
						Provenance: recommendationSource.Provenance,
					}
				}
				value = item
			}
			if v, ok := value.(validator); ok {
				if err := v.Validate(); err != nil {
					t.Fatalf("golden fixture %q is not valid: %v", tc.name, err)
				}
			}
			got, err := json.MarshalIndent(value, "", "  ")
			if err != nil {
				t.Fatalf("marshal %q: %v", tc.name, err)
			}
			golden.Assert(t, tc.name, append(got, '\n'))
		})
	}
}

func TestReadinessGoldenContracts(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 11, 1, 2, 3, 0, time.UTC)
	resolution, err := domain.NewRequirementResolution(domain.RequirementResolutionInput{
		RequirementKey: "repo-change-policy", CheckClass: domain.CheckClassRepoChangePolicy,
		Kind: domain.RequirementRequired, Applicable: true,
		RequirementSetDigest:    "sha256:requirement-set",
		FloorRegistryGeneration: domain.CurrentVerificationFloorRegistryGeneration,
		ResolvedPolicyDigest:    "sha256:policy",
	})
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := domain.NewWaiverLifecycleEvent("waiver-1", 1, domain.WaiverLifecycleGranted, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	waiver, err := domain.NewValidatedDegradedWaiver(resolution, "waiver-1", "repo_change_policy",
		domain.WaiverAuthorityHumanApproval, "sha256:grant", lifecycle, now)
	if err != nil {
		t.Fatal(err)
	}
	state, err := domain.NewNonPassingCheckState(resolution, domain.AdvisoryFailed, &waiver)
	if err != nil {
		t.Fatal(err)
	}
	verdict, err := domain.EvaluateReadiness(domain.EvaluationTarget{CandidateHead: "head-sha"}, []domain.RequirementResolution{resolution}, []domain.CheckState{state}, func(r domain.RequirementResolution, w domain.ValidatedDegradedWaiver) error {
		return domain.ValidateDegradedWaiver(r, lifecycle, w)
	})
	if err != nil {
		t.Fatal(err)
	}
	summary := domain.ReadinessSummary{
		Class: verdict.Class, EvaluationSetDigest: verdict.EvaluationSetDigest,
	}
	proofResolution, err := domain.NewRequirementResolution(domain.RequirementResolutionInput{
		RequirementKey: "review", CheckClass: domain.CheckClassIndependentReview,
		Kind: domain.RequirementRequired, Applicable: true, BaseDependent: true,
		RequirementSetDigest:    "sha256:requirement-set",
		FloorRegistryGeneration: domain.CurrentVerificationFloorRegistryGeneration,
		ResolvedPolicyDigest:    "sha256:policy",
	})
	if err != nil {
		t.Fatal(err)
	}
	base := domain.BaseRevision{
		Repo: "freeside-ai/freeside", RepositoryID: 1, BaseRef: "refs/heads/main", BaseSHA: "base-sha",
	}
	proof, err := domain.NewCheckProof(proofResolution, "head-sha", &base, "sha256:review-config")
	if err != nil {
		t.Fatal(err)
	}
	// The card-facing detail (issue #982) projected from the same target,
	// verdict, and states as the verdict itself: clean from the passed review
	// alone, degraded from the review beside the waived policy failure.
	passedState, err := domain.NewPassedCheckState(proofResolution, proof)
	if err != nil {
		t.Fatal(err)
	}
	target := domain.EvaluationTarget{CandidateHead: "head-sha", Base: &base}
	cleanVerdict, err := domain.EvaluateReadiness(target, []domain.RequirementResolution{proofResolution}, []domain.CheckState{passedState}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cleanDetail, err := domain.NewReadinessDetail(target, cleanVerdict, []domain.CheckState{passedState})
	if err != nil {
		t.Fatal(err)
	}
	degradedVerdict, err := domain.EvaluateReadiness(target, []domain.RequirementResolution{resolution, proofResolution}, []domain.CheckState{passedState, state}, func(r domain.RequirementResolution, w domain.ValidatedDegradedWaiver) error {
		return domain.ValidateDegradedWaiver(r, lifecycle, w)
	})
	if err != nil {
		t.Fatal(err)
	}
	degradedDetail, err := domain.NewReadinessDetail(target, degradedVerdict, []domain.CheckState{state, passedState})
	if err != nil {
		t.Fatal(err)
	}
	// Blocked is representable in the detail shape (a required non-pass with
	// no waiver) but never on a ready item, which the summary's ready-only
	// classes enforce; the golden pins the shape and the item pins the refusal.
	blockedDetail := cleanDetail
	blockedDetail.Requirements = append(slices.Clone(cleanDetail.Requirements), domain.ReadinessRequirement{
		RequirementKey: "unwaived-policy", CheckClass: domain.CheckClassRepoChangePolicy,
		Kind: domain.RequirementRequired, State: domain.ReadinessRequirementFailed,
	})
	if err := blockedDetail.Validate(); err != nil {
		t.Fatal(err)
	}
	if blockedDetail.Class() != domain.ReadinessBlocked {
		t.Fatalf("blocked detail class = %q", blockedDetail.Class())
	}
	blockedInput := validItemInput(domain.AttentionReadyForFinalReview)
	blockedInput.PRHeadSHA = "head-sha"
	blockedInput.Readiness = &domain.ReadinessSummary{
		Class: domain.ReadinessReadyClean, EvaluationSetDigest: blockedDetail.EvaluationSetDigest,
	}
	blockedInput.ReadinessDetail = &blockedDetail
	if _, err := domain.NewAttentionItem(blockedInput, nil); !errors.Is(err, domain.ErrReadinessDetailInconsistent) {
		t.Fatalf("ready item with a blocked detail error = %v, want ErrReadinessDetailInconsistent", err)
	}
	fixtures := []struct {
		name  string
		value any
	}{
		{"requirement_resolution", resolution},
		{"check_proof", proof},
		{"waiver_lifecycle_event", lifecycle},
		{"validated_degraded_waiver", waiver},
		{"check_state", state},
		{"readiness_summary", summary},
		{"readiness_verdict", verdict},
		{"readiness_detail_clean", cleanDetail},
		{"readiness_detail_degraded", degradedDetail},
		{"readiness_detail_blocked", blockedDetail},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			body, err := json.MarshalIndent(fixture.value, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			golden.Assert(t, fixture.name, append(body, '\n'))
		})
	}
}
