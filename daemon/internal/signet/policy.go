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
		domain.ActionAdjudicate, domain.ActionDiscuss, domain.ActionStop,
	),
	domain.AttentionExecutionFailure: actionSet(
		domain.ActionRetry, domain.ActionRetryWithCapability, domain.ActionDiscuss, domain.ActionStop,
	),
	domain.AttentionAgentQuestion: actionSet(
		domain.ActionAnswerAndRetry, domain.ActionAnswerWithoutRetry, domain.ActionStop,
	),
	domain.AttentionPublishBlocked: actionSet(
		domain.ActionRerunTrustEvaluation, domain.ActionChooseAlternate,
		domain.ActionInspectTrustFailure, domain.ActionStop,
	),
	domain.AttentionReadyForFinalReview: actionSet(
		domain.ActionOpenPR, domain.ActionReturnToAgent, domain.ActionMarkSeen,
		domain.ActionDismiss, domain.ActionStop,
	),
	domain.AttentionRunProposal: actionSet(
		domain.ActionStart, domain.ActionStartWithChanges, domain.ActionDecline, domain.ActionSnooze,
	),
	domain.AttentionSystemHealth: actionSet(
		domain.ActionAcknowledge, domain.ActionRunDoctor, domain.ActionStopUnattended,
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
// offer none.
func validateRequestedActions(itemType domain.AttentionType, requested []domain.Action) error {
	allowed, known := allowedActionsByType[itemType]
	if !known {
		return fmt.Errorf("attention type %q: %w", itemType, domain.ErrUnknownAttentionType)
	}
	if len(requested) == 0 {
		if itemType == domain.AttentionBlocked {
			return nil
		}
		return fmt.Errorf("attention type %q: %w", itemType, domain.ErrNoActions)
	}
	for _, action := range requested {
		if _, ok := allowed[action]; !ok {
			return fmt.Errorf("action %q is not allowed for attention type %q: %w",
				action, itemType, ErrActionNotAllowedForType)
		}
	}
	return nil
}
