package signet

import (
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// allowedActionsByType is the authoritative signet policy for the actions an
// item of each Phase 1 attention type may offer (plan §4). The domain owns the
// Action union only; which members are legitimate for a type stays here at the
// attention-service boundary.
var allowedActionsByType = map[domain.AttentionType]map[domain.Action]struct{}{
	domain.AttentionSpecApproval: actionSet(
		domain.ActionApprove, domain.ActionRequestChanges, domain.ActionDiscuss, domain.ActionStop,
	),
	domain.AttentionReviewDiminishing: actionSet(
		domain.ActionFinishNow, domain.ActionApplyThenFinish,
		domain.ActionContinueUnderPolicy, domain.ActionConvertToPolicy,
	),
	domain.AttentionReviewDispute: actionSet(
		domain.ActionApprove, domain.ActionDiscuss, domain.ActionStop,
	),
	domain.AttentionReviewContradiction: actionSet(domain.ActionRecoverReview),
	domain.AttentionReviewConfiguration: actionSet(
		domain.ActionAdoptReviewConfiguration, domain.ActionDiscuss, domain.ActionStop,
	),
	domain.AttentionFindingAdjudication: actionSet(
		domain.ActionAcceptRecommendedRoute, domain.ActionChooseAlternativeRoute,
		domain.ActionDiscuss, domain.ActionStop,
	),
	domain.AttentionExecutionFailure: actionSet(
		domain.ActionRetry, domain.ActionRetryWithCapability, domain.ActionDiscuss, domain.ActionStop,
	),
	domain.AttentionAgentQuestion: actionSet(
		domain.ActionAnswerAndRetry, domain.ActionAnswerWithoutRetry, domain.ActionStop,
	),
	// open_pr on a hold is conditional: validateRequestedActions admits it only
	// when the item carries the pull request it would open.
	domain.AttentionPublishBlocked: actionSet(
		domain.ActionRerunTrustEvaluation,
		domain.ActionInspectTrustFailure, domain.ActionOpenPR, domain.ActionStop,
	),
	domain.AttentionReadyForFinalReview: actionSet(
		domain.ActionOpenPR, domain.ActionReturnToAgent, domain.ActionMarkSeen,
		domain.ActionDismiss, domain.ActionStop,
	),
	domain.AttentionTaskProposal: actionSet(
		domain.ActionStart, domain.ActionStartWithChanges, domain.ActionDecline, domain.ActionSnooze,
	),
	// effect_proposal decides an agent-requested real-world effect (plan §4).
	// approve_with_changes is offered only when the effect can be revised; the
	// open call drops it for a daemon_fallback closure. approve concludes with a
	// stored approval binding, unlike approve on other types.
	domain.AttentionEffectProposal: actionSet(
		domain.ActionApprove, domain.ActionApproveWithChanges, domain.ActionDecline, domain.ActionSnooze,
	),
	domain.AttentionSystemHealth: actionSet(
		domain.ActionAcknowledge, domain.ActionRunDoctor, domain.ActionStopUnattended,
		domain.ActionResumeUnattended, domain.ActionResolveReenrollment,
	),
	// blocked is a read-only consolidation of external waits (§5.12). The plan
	// assigns it no action; the shared domain/API contracts permit the empty
	// set (#96), and this table keeps the per-type cardinality rule here.
	domain.AttentionBlocked: actionSet(),
}

func actionSet(actions ...domain.Action) map[domain.Action]struct{} {
	set := make(map[domain.Action]struct{}, len(actions))
	for _, action := range actions {
		set[action] = struct{}{}
	}
	return set
}

// validateRequestedActions rejects an item whose offered actions are not a
// subset of the plan-defined set for its type. Every actionable type must
// offer at least one decision; blocked is the sole read-only type and must
// offer none. open_pr is navigation to the item's pull request, so an item
// with no reference cannot offer it: a ready item always carries one (domain
// validation), a publish_blocked item only once its run has published.
func validateRequestedActions(item domain.AttentionItem) error {
	allowed, known := allowedActionsByType[item.Type]
	if !known {
		return fmt.Errorf("attention type %q: %w", item.Type, domain.ErrUnknownAttentionType)
	}
	if len(item.RequestedDecision) == 0 {
		if item.Type == domain.AttentionBlocked {
			return nil
		}
		return fmt.Errorf("attention type %q: %w", item.Type, domain.ErrNoActions)
	}
	for _, action := range item.RequestedDecision {
		if _, ok := allowed[action]; !ok {
			return fmt.Errorf("action %q is not allowed for attention type %q: %w",
				action, item.Type, ErrActionNotAllowedForType)
		}
		if action == domain.ActionOpenPR && item.PRReference == nil {
			return fmt.Errorf("action %q needs a pr reference on attention type %q: %w",
				action, item.Type, ErrActionNotAllowedForType)
		}
	}
	return nil
}
