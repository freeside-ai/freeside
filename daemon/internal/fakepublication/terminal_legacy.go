package fakepublication

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// These shapes freeze the deployed JSON field order and vocabulary. Task
// migration verifies the original commitment before replacing derived labels.
type terminalSubjectBeforeTasks struct {
	Type  domain.SubjectType `json:"subject_type"`
	ID    domain.SubjectID   `json:"subject_id"`
	RunID *domain.RunID      `json:"run_id"`
}

func terminalLegacySubject(subject domain.Subject) terminalSubjectBeforeTasks {
	return terminalSubjectBeforeTasks{Type: subject.Type, ID: subject.ID, RunID: subject.RunID}
}

type terminalNamesBeforeTasks struct {
	Project  domain.DisplayName `json:"project"`
	WorkUnit domain.DisplayName `json:"work_unit"`
}

type terminalItemBeforeTasks struct {
	ID                               domain.ItemID                              `json:"id"`
	ProjectID                        domain.ProjectID                           `json:"project_id"`
	Subject                          terminalSubjectBeforeTasks                 `json:"subject"`
	Type                             domain.AttentionType                       `json:"type"`
	Priority                         domain.Priority                            `json:"priority"`
	Reason                           string                                     `json:"reason"`
	RequestedDecision                []domain.Action                            `json:"requested_decision"`
	Recommendation                   *domain.Recommendation                     `json:"recommendation"`
	DecisionSurface                  domain.DecisionSurfaceRef                  `json:"decision_surface"`
	EvidenceSnapshot                 []domain.Artifact                          `json:"evidence_snapshot"`
	AgentClaims                      []domain.AgentClaim                        `json:"agent_claims"`
	ArtifactDigests                  []domain.Digest                            `json:"artifact_digests"`
	PRHeadSHA                        string                                     `json:"pr_head_sha"`
	PRReference                      *domain.PRReference                        `json:"pr_reference"`
	Readiness                        *domain.ReadinessSummary                   `json:"readiness"`
	ReadinessDetail                  *domain.ReadinessDetail                    `json:"readiness_detail"`
	YieldHistory                     *domain.ReviewYieldHistory                 `json:"yield_history"`
	CommitPlanNotice                 *domain.CommitPlanNoticeReason             `json:"commit_plan_notice"`
	ScopeDecision                    *domain.ScopeDecisionFacts                 `json:"scope_decision"`
	BaseFreshness                    *domain.BaseFreshness                      `json:"base_freshness"`
	ReadinessInvalidation            *domain.ReadinessInvalidation              `json:"readiness_invalidation"`
	ReviewRecoveryBinding            *domain.ReviewRecoveryBinding              `json:"review_recovery_binding"`
	CodexReenrollmentRecoveryBinding *domain.CodexReenrollmentRecoveryBinding   `json:"codex_reenrollment_recovery_binding"`
	ReviewConfigurationRecovery      *domain.ReviewConfigurationRecoveryBinding `json:"review_configuration_recovery"`
	FindingAdjudication              *domain.FindingAdjudicationBinding         `json:"finding_adjudication"`
	DisplayNames                     *terminalNamesBeforeTasks                  `json:"display_names"`
	BillableCostSoFar                *domain.CostSoFar                          `json:"billable_cost_so_far"`
	ExecutionFailure                 *domain.ExecutionFailureFacts              `json:"execution_failure"`
	PublishBlock                     *domain.PublishBlockFacts                  `json:"publish_block"`
	DiffStats                        *domain.DiffStats                          `json:"diff_stats"`
	BlockedOn                        *domain.BlockedWait                        `json:"blocked_on"`
	HealthDiagnostic                 *domain.HealthDiagnostic                   `json:"health_diagnostic"`
	ReviewDispute                    *domain.ReviewDisputeBinding               `json:"review_dispute"`
	SpecRevision                     *domain.SpecRevisionFacts                  `json:"spec_revision"`
	AgentQuestion                    *domain.AgentQuestionFacts                 `json:"agent_question"`
	ItemVersion                      int                                        `json:"item_version"`
	InterruptionClass                domain.InterruptionClass                   `json:"interruption_class"`
	ConversationID                   *domain.ConversationID                     `json:"conversation_id"`
	Timing                           domain.TimingSummary                       `json:"timing"`
	CreatedAt                        *time.Time                                 `json:"created_at"`
	ExpiresWhen                      *time.Time                                 `json:"expires_when"`
	DecidedAt                        *time.Time                                 `json:"decided_at"`
	Posture                          *domain.HealthPosture                      `json:"posture"`
	BlockingSupersession             *domain.BlockingSupersession               `json:"blocking_supersession"`
	Status                           domain.ItemStatus                          `json:"status"`
}

// TerminalDigestBeforeTasks reproduces the commitment written before tasks
// added a subject coordinate and replaced display_names.work_unit.
func TerminalDigestBeforeTasks(task Task, item domain.AttentionItem) (domain.Digest, error) {
	item.ItemVersion = 1
	item.Status = domain.StatusOpen
	item.DecidedAt = nil
	item.Timing = domain.TimingSummary{}
	item.Recommendation = nil
	item.DecisionSurface = domain.DecisionSurfaceRef{}
	item.CreatedAt = nil
	var names *terminalNamesBeforeTasks
	if item.DisplayNames != nil {
		names = &terminalNamesBeforeTasks{Project: item.DisplayNames.Project, WorkUnit: item.DisplayNames.Task}
	}
	legacy := terminalItemBeforeTasks{
		ID:                               item.ID,
		ProjectID:                        item.ProjectID,
		Subject:                          terminalLegacySubject(item.Subject),
		Type:                             item.Type,
		Priority:                         item.Priority,
		Reason:                           item.Reason,
		RequestedDecision:                item.RequestedDecision,
		Recommendation:                   item.Recommendation,
		DecisionSurface:                  item.DecisionSurface,
		EvidenceSnapshot:                 item.EvidenceSnapshot,
		AgentClaims:                      item.AgentClaims,
		ArtifactDigests:                  item.ArtifactDigests,
		PRHeadSHA:                        item.PRHeadSHA,
		PRReference:                      item.PRReference,
		Readiness:                        item.Readiness,
		ReadinessDetail:                  item.ReadinessDetail,
		YieldHistory:                     item.YieldHistory,
		CommitPlanNotice:                 item.CommitPlanNotice,
		ScopeDecision:                    item.ScopeDecision,
		BaseFreshness:                    item.BaseFreshness,
		ReadinessInvalidation:            item.ReadinessInvalidation,
		ReviewRecoveryBinding:            item.ReviewRecoveryBinding,
		CodexReenrollmentRecoveryBinding: item.CodexReenrollmentRecoveryBinding,
		ReviewConfigurationRecovery:      item.ReviewConfigurationRecovery,
		FindingAdjudication:              item.FindingAdjudication,
		DisplayNames:                     names,
		BillableCostSoFar:                item.BillableCostSoFar,
		ExecutionFailure:                 item.ExecutionFailure,
		PublishBlock:                     item.PublishBlock,
		DiffStats:                        item.DiffStats,
		BlockedOn:                        item.BlockedOn,
		HealthDiagnostic:                 item.HealthDiagnostic,
		ReviewDispute:                    item.ReviewDispute,
		SpecRevision:                     item.SpecRevision,
		AgentQuestion:                    item.AgentQuestion,
		ItemVersion:                      item.ItemVersion,
		InterruptionClass:                item.InterruptionClass,
		ConversationID:                   item.ConversationID,
		Timing:                           item.Timing,
		CreatedAt:                        item.CreatedAt,
		ExpiresWhen:                      item.ExpiresWhen,
		DecidedAt:                        item.DecidedAt,
		Posture:                          item.Posture,
		BlockingSupersession:             item.BlockingSupersession,
		Status:                           item.Status,
	}
	payload, err := json.Marshal(struct {
		Task Task                    `json:"task"`
		Item terminalItemBeforeTasks `json:"item"`
	}{Task: task, Item: legacy})
	if err != nil {
		return "", fmt.Errorf("encode pre-task terminal binding: %w", err)
	}
	sum := sha256.Sum256(payload)
	return domain.Digest(contentaddr.Format(sum[:])), nil
}
