package seedfixture

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// itemSpec is the per-type part of an attention item; itemInput fills the
// fields every fixture item shares.
type itemSpec struct {
	id        domain.ItemID
	project   project
	runID     domain.RunID
	itemType  domain.AttentionType
	priority  domain.Priority
	reason    string
	actions   []domain.Action
	createdAt time.Time
}

func (spec itemSpec) input() domain.AttentionItemInput {
	runID := spec.runID
	createdAt := spec.createdAt
	return domain.AttentionItemInput{
		ID: spec.id, ProjectID: spec.project.id,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    spec.itemType, Priority: spec.priority, Reason: spec.reason,
		RequestedDecision: spec.actions, PRHeadSHA: headSHA,
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate,
		CreatedAt: &createdAt, Status: domain.StatusOpen,
	}
}

// putItem runs signet's intake policy (per-type actions, no caller-set
// decision instant) before the store's own gates, as generic intake does.
func putItem(s seeder, in domain.AttentionItemInput) error {
	item, err := domain.NewAttentionItem(in, nil)
	if err != nil {
		return err
	}
	if err := signet.ValidateItemIntake(item); err != nil {
		return err
	}
	return s.write(func(tx *store.WriteTx) error { return tx.PutAttentionItem(s.ctx, item) })
}

// seedItems opens one item per attention type the generic store path admits.
// spec_approval and ready_for_final_review come from the runs that own them;
// task_proposal and effect_proposal need their dedicated openers and are not
// seeded (#1503 follow-up).
func seedItems(s seeder, failed, active, verification, awaiting domain.Run, approval domain.AttentionItem) error {
	p := freesideProject
	at := base.Add(2 * time.Hour)
	var inputs []domain.AttentionItemInput

	executionFailure := itemSpec{
		id: "execution-failure-" + domain.ItemID(failed.ID), project: p, runID: failed.ID,
		itemType: domain.AttentionExecutionFailure, priority: domain.PriorityHigh,
		reason:  "The implementation attempt failed before producing a candidate.",
		actions: []domain.Action{domain.ActionRetry, domain.ActionDiscuss, domain.ActionStop}, createdAt: at,
	}.input()
	inputs = append(inputs, executionFailure)

	diminishing := itemSpec{
		id: "review-diminishing-" + domain.ItemID(active.ID), project: p, runID: active.ID,
		itemType: domain.AttentionReviewDiminishing, priority: domain.PriorityNormal,
		reason: "Review rounds are finding less each time.",
		actions: []domain.Action{
			domain.ActionFinishNow, domain.ActionApplyThenFinish, domain.ActionContinueUnderPolicy,
		},
		createdAt: at.Add(time.Minute),
	}.input()
	inputs = append(inputs, diminishing)

	// ReviewDispute stays nil, so the dispute binding gate has nothing to
	// authenticate; the card renders from the item alone.
	dispute := itemSpec{
		id: "review-dispute-" + domain.ItemID(active.ID), project: p, runID: active.ID,
		itemType: domain.AttentionReviewDispute, priority: domain.PriorityNormal,
		reason:    "The agent disputes a review finding.",
		actions:   []domain.Action{domain.ActionApprove, domain.ActionDiscuss, domain.ActionStop},
		createdAt: at.Add(2 * time.Minute),
	}.input()
	inputs = append(inputs, dispute)

	contradiction := itemSpec{
		id: "review-contradiction-" + domain.ItemID(active.ID), project: p, runID: active.ID,
		itemType: domain.AttentionReviewContradiction, priority: domain.PriorityHigh,
		reason:    "The reviewer reported findings it then contradicted.",
		actions:   []domain.Action{domain.ActionRecoverReview},
		createdAt: at.Add(3 * time.Minute),
	}.input()
	contradiction.ReviewRecoveryBinding = &domain.ReviewRecoveryBinding{
		RunID: active.ID, InvocationID: domain.InvocationID("review-contradiction-" + string(active.ID)), Round: 1,
		BaseSHA: baseSHA, HeadSHA: headSHA, FailureDigest: digest("failure-contradiction"),
	}
	inputs = append(inputs, contradiction)

	configuration := itemSpec{
		id: "review-configuration-" + domain.ItemID(active.ID), project: p, runID: active.ID,
		itemType: domain.AttentionReviewConfiguration, priority: domain.PriorityNormal,
		reason: "The review configuration changed since this run was admitted.",
		actions: []domain.Action{
			domain.ActionAdoptReviewConfiguration, domain.ActionDiscuss, domain.ActionStop,
		},
		createdAt: at.Add(4 * time.Minute),
	}.input()
	configuration.ReviewConfigurationRecovery = &domain.ReviewConfigurationRecoveryBinding{
		RunID: active.ID, InvocationID: domain.InvocationID("review-configuration-" + string(active.ID)), Round: 2,
		BaseSHA: baseSHA, HeadSHA: headSHA, FailureDigest: digest("failure-configuration"),
		Repo: p.repo, RepositoryID: p.repositoryID, SupersededProfileDigest: digest("review-profile"),
	}
	inputs = append(inputs, configuration)

	blocked := itemSpec{
		id: "publish-blocked-" + domain.ItemID(verification.ID), project: orioleProject, runID: verification.ID,
		itemType: domain.AttentionPublishBlocked, priority: domain.PriorityHigh,
		reason:    domain.PublicationBlockVerification,
		actions:   []domain.Action{domain.ActionInspectTrustFailure, domain.ActionStop},
		createdAt: at.Add(5 * time.Minute),
	}.input()
	inputs = append(inputs, blocked)

	posture := domain.HealthPostureBlocking
	health := itemSpec{
		id: "system-health-seed-fixture", itemType: domain.AttentionSystemHealth,
		priority:  domain.PriorityHigh,
		reason:    "The local backup has not completed a checkpoint.",
		actions:   []domain.Action{domain.ActionAcknowledge, domain.ActionRunDoctor},
		createdAt: at.Add(6 * time.Minute),
	}.input()
	health.ProjectID = "project-system"
	health.Subject = domain.Subject{Type: domain.SubjectSystem, ID: "daemon"}
	health.PRHeadSHA = ""
	health.InterruptionClass = domain.InterruptionExceptional
	health.Posture = &posture
	inputs = append(inputs, health)

	for _, in := range inputs {
		if err := putItem(s, in); err != nil {
			return fmt.Errorf("item %s: %w", in.ID, err)
		}
	}
	if err := seedFindingAdjudication(s, active, at.Add(7*time.Minute)); err != nil {
		return err
	}
	if err := seedAgentQuestion(s, at.Add(8*time.Minute)); err != nil {
		return err
	}

	// blocked names the spec_approval it waits on, so it is written last; its
	// wait starts when that approval was created.
	waiting := itemSpec{
		id: "blocked-" + domain.ItemID(awaiting.ID), project: p, runID: awaiting.ID,
		itemType: domain.AttentionBlocked, priority: domain.PriorityLow,
		reason: "Waiting on specification approval.", createdAt: at.Add(9 * time.Minute),
	}.input()
	waiting.PRHeadSHA = ""
	waiting.RequestedDecision = nil
	waiting.BlockedOn = &domain.BlockedWait{
		Kind: domain.BlockedWaitSpecApproval, Since: *approval.CreatedAt, ItemID: &approval.ID,
	}
	if err := putItem(s, waiting); err != nil {
		return fmt.Errorf("item %s: %w", waiting.ID, err)
	}
	return nil
}

// seedFindingAdjudication records a review round's finding and its
// adjudication artifact on the active run, then the item whose binding
// projects them, as the store re-gate requires.
func seedFindingAdjudication(s seeder, run domain.Run, at time.Time) error {
	const round = 3
	findingID := domain.FindingID("finding-" + string(run.ID))
	instructionDigest := digest("review-instructions")
	finding := domain.Finding{
		ID: findingID, RunID: run.ID, Source: "codex_local",
		Location: &domain.FindingLocation{Path: "daemon/internal/engine/engine.go", StartLine: 42, EndLine: 42},
		Message:  "The retry path drops the operator's reason.",
		RawText:  "The retry path drops the operator's reason.", CreatedAt: at,
	}
	record, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: domain.InvocationID(fmt.Sprintf("review-%s-%d", run.ID, round)), RunID: run.ID, Round: round,
		Provider: "openai", ModelConfiguration: "gpt-codex/high",
		ConfigurationDigest: digest("review-configuration"),
		InstructionDigest:   instructionDigest, CostOwner: "owner",
		BaseSHA: baseSHA, HeadSHA: headSHA, CompletedAt: at,
		CompletionEvidence: digest("review-completion"),
		Outcome:            domain.ReviewFindings, FindingIDs: []domain.FindingID{findingID},
	})
	if err != nil {
		return err
	}
	entry, err := domain.NewModelAdjudicationEntry(
		findingID, domain.GoalContradictory, nil, domain.RouteDecline,
		domain.ConfidenceHigh, "The finding contradicts the approved work unit.",
		nil, []string{"AGENTS.md"}, []string{"The work contract is current."}, nil, nil,
	)
	if err != nil {
		return err
	}
	artifact, err := domain.NewFindingAdjudication(
		run.ID, round, run.SpecDigest, instructionDigest, run.PolicyDigest,
		[]domain.FindingAdjudicationEntry{entry}, "", at,
	)
	if err != nil {
		return err
	}
	if err := s.write(func(tx *store.WriteTx) error {
		if err := tx.PutReviewRecord(s.ctx, record, []domain.Finding{finding}); err != nil {
			return err
		}
		return tx.PutFindingAdjudication(s.ctx, artifact)
	}); err != nil {
		return fmt.Errorf("finding adjudication authority: %w", err)
	}
	in := itemSpec{
		id: "finding-adjudication-" + domain.ItemID(run.ID), project: freesideProject, runID: run.ID,
		itemType: domain.AttentionFindingAdjudication, priority: domain.PriorityNormal,
		reason: "A review finding needs a routing decision.",
		actions: []domain.Action{
			domain.ActionAcceptRecommendedRoute, domain.ActionChooseAlternativeRoute,
			domain.ActionDiscuss, domain.ActionStop,
		},
		createdAt: at,
	}.input()
	// The proposal projects the seeded finding's coordinates and the artifact
	// entry's evidence and alternatives; the store re-gate refuses any other.
	in.FindingAdjudication = &domain.FindingAdjudicationBinding{
		RunID: run.ID, Round: round, AdjudicationDigest: artifact.Digest,
		Proposals: []domain.FindingAdjudicationProposal{{
			FindingID:        findingID,
			FindingMessage:   domain.NormalizeFindingMessage(finding.Message),
			FindingLocation:  finding.Location,
			Producer:         entry.Producer,
			GoalRelationship: entry.GoalRelationship, Compatibility: entry.Compatibility,
			Route: entry.Route, Rationale: entry.Rationale, Evidence: slices.Clone(entry.Evidence),
			CitedRules: entry.CitedRules, Assumptions: entry.Assumptions,
			OpenQuestions: entry.OpenQuestions, Confidence: entry.Confidence,
			OfferedAlternatives: slices.Clone(entry.OfferedAlternatives),
		}},
	}
	if err := putItem(s, in); err != nil {
		return fmt.Errorf("item %s: %w", in.ID, err)
	}
	return nil
}

// seedAgentQuestion records a specification run whose invocation stopped to
// ask a question: the run, its agent invocation, the decisions artifact, the
// execution admission, and the specification terminal naming the item, then
// the item itself.
func seedAgentQuestion(s seeder, at time.Time) error {
	const iteration = 1
	p := freesideProject
	runID := questionRunID
	invocation := domain.SpecificationInvocationID(runID, iteration)
	specStage := domain.SpecificationStageID(runID)
	itemID := domain.ItemID("question-" + string(invocation))
	facts := &domain.AgentQuestionFacts{
		Stage: domain.StageNameSpecification, InvocationID: invocation,
		Decisions: []domain.Decision{{
			Question:    "Which compatibility target applies?",
			WhyBlocking: "The specification cannot fix the API surface without it.",
			Options: []domain.DecisionOption{
				{Label: "Current and previous", Tradeoffs: "Wider support, more adapters."},
				{Label: "Current only", Tradeoffs: "Less code, drops older clients."},
			},
			Recommendation: "Current and previous",
		}},
	}
	factsDigest, err := facts.ComputeDigest()
	if err != nil {
		return err
	}
	body, err := json.Marshal(facts.Decisions)
	if err != nil {
		return err
	}
	claim := domain.AgentClaim{
		Label: domain.AgentQuestionClaimLabel, Artifact: domain.ArtifactID("decisions-" + string(runID)),
		Digest: factsDigest,
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerAgent, ProducerInvocationID: invocation,
			HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
		},
		Metadata: domain.EvidenceMetadata{
			MediaType: domain.EvidenceMediaApplicationJSON, SizeBytes: int64(len(body)),
			CreatedAt: at, Source: domain.EvidenceSourceClaim, Availability: domain.EvidenceAvailable,
		},
	}
	run := domain.Run{
		ID: runID, ProjectID: p.id,
		SpecDigest:   digest("spec-" + string(runID)),
		PolicyDigest: digest("policy-" + string(runID)),
		Stages: []domain.Stage{{
			ID: specStage, RunID: runID, Name: "specification",
			Attempts: []domain.Attempt{{
				ID: attemptID(invocation), StageID: specStage, Number: iteration, InvocationID: invocation,
			}},
		}},
	}
	agentInvocation, err := domain.NewAgentInvocation(
		invocation, []domain.ArtifactID{domain.ArtifactID("input-" + string(invocation))}, nil, 0,
	)
	if err != nil {
		return err
	}
	inputDigest, err := agentInvocation.ComputeInputDigest()
	if err != nil {
		return err
	}
	admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: invocation, RunID: runID, StageID: specStage, AttemptID: attemptID(invocation),
		Backend:       "fresh_vm_read_only_volume_handoff",
		Capabilities:  AdmissionCapabilities,
		OperatingMode: domain.ModeAttendedDev, CredentialMode: domain.CredentialLocalTrusted,
		EgressProfile: domain.EgressCleanVerification,
		ImageRef:      domain.ImageRef("ghcr.io/example/agent@" + string(digest("review-image"))),
		SpecDigest:    run.SpecDigest, PolicyDigest: run.PolicyDigest, InputDigest: inputDigest,
		Base: domain.BaseRevision{
			Repo: p.repo, RepositoryID: p.repositoryID, BaseRef: baseRef, BaseSHA: baseSHA,
		},
		Workspace: "workspace-" + string(runID), AdmittedAt: at,
	})
	if err != nil {
		return err
	}
	artifactMetadata := claim.Metadata
	artifactMetadata.Source = domain.EvidenceSourceRun
	artifact, err := domain.NewArtifact(domain.ArtifactInput{
		ID: claim.Artifact, Type: domain.ArtifactKindEvidence,
		Digest: claim.Digest, Provenance: claim.Provenance, Metadata: artifactMetadata,
	}, nil)
	if err != nil {
		return err
	}
	terminal, err := json.Marshal(struct {
		InvocationID        domain.InvocationID `json:"invocation_id"`
		Iteration           int                 `json:"iteration"`
		Status              string              `json:"status"`
		ResearchArtifactIDs []domain.ArtifactID `json:"research_artifact_ids"`
		DecisionArtifactID  *domain.ArtifactID  `json:"decision_artifact_id,omitempty"`
		QuestionItemID      *domain.ItemID      `json:"question_item_id,omitempty"`
	}{
		InvocationID: invocation, Iteration: iteration, Status: "completed",
		ResearchArtifactIDs: []domain.ArtifactID{}, DecisionArtifactID: &claim.Artifact, QuestionItemID: &itemID,
	})
	if err != nil {
		return err
	}
	if err := s.write(func(tx *store.WriteTx) error {
		if err := tx.PutRun(s.ctx, run); err != nil {
			return err
		}
		if err := tx.PutAgentInvocation(s.ctx, agentInvocation); err != nil {
			return err
		}
		if err := tx.PutArtifact(s.ctx, artifact); err != nil {
			return err
		}
		if err := tx.RecordExecutionAdmission(s.ctx, admission); err != nil {
			return err
		}
		if _, _, err := tx.RecordInbox(s.ctx, string(invocation), "specification_stage_terminal", terminal); err != nil {
			return err
		}
		return s.name(tx, runID, domain.DisplayName{
			Text: "Pick the API compatibility window", Source: domain.DisplayNameSourceAgent,
		}, at)
	}); err != nil {
		return fmt.Errorf("agent question authority: %w", err)
	}
	in := itemSpec{
		id: itemID, project: p, runID: runID,
		itemType: domain.AttentionAgentQuestion, priority: domain.PriorityNormal,
		reason:    "The specification agent has a question.",
		actions:   []domain.Action{domain.ActionAnswerAndRetry, domain.ActionStop},
		createdAt: at,
	}.input()
	in.PRHeadSHA = ""
	in.AgentClaims = []domain.AgentClaim{claim}
	in.AgentQuestion = facts
	if err := putItem(s, in); err != nil {
		return fmt.Errorf("item %s: %w", in.ID, err)
	}
	return nil
}
