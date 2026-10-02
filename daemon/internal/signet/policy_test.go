package signet

import (
	"errors"
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestAllowedActionsByType is the independent fixture for all Phase 1
// attention types. It pins both halves of each allowed set: the listed
// actions pass together, and every other member of the domain union fails.
func TestAllowedActionsByType(t *testing.T) {
	fixtures := map[domain.AttentionType][]domain.Action{
		domain.AttentionSpecApproval: {
			domain.ActionApprove, domain.ActionRequestChanges, domain.ActionDiscuss, domain.ActionStop,
		},
		domain.AttentionReviewDiminishing: {
			domain.ActionFinishNow, domain.ActionApplyThenFinish,
			domain.ActionContinueUnderPolicy, domain.ActionConvertToPolicy,
		},
		domain.AttentionReviewDispute: {
			domain.ActionApprove, domain.ActionDiscuss, domain.ActionStop,
		},
		domain.AttentionReviewContradiction: {domain.ActionRecoverReview},
		domain.AttentionReviewConfiguration: {
			domain.ActionAdoptReviewConfiguration, domain.ActionDiscuss, domain.ActionStop,
		},
		domain.AttentionFindingAdjudication: {
			domain.ActionAcceptRecommendedRoute, domain.ActionChooseAlternativeRoute,
			domain.ActionDiscuss, domain.ActionStop,
		},
		domain.AttentionExecutionFailure: {
			domain.ActionRetry, domain.ActionRetryWithCapability, domain.ActionDiscuss, domain.ActionStop,
		},
		domain.AttentionAgentQuestion: {
			domain.ActionAnswerAndRetry, domain.ActionAnswerWithoutRetry, domain.ActionStop,
		},
		domain.AttentionPublishBlocked: {
			domain.ActionRerunTrustEvaluation,
			domain.ActionInspectTrustFailure, domain.ActionOpenPR, domain.ActionStop,
		},
		domain.AttentionReadyForFinalReview: {
			domain.ActionOpenPR, domain.ActionReturnToAgent, domain.ActionMarkSeen,
			domain.ActionDismiss, domain.ActionStop,
		},
		domain.AttentionTaskProposal: {
			domain.ActionStart, domain.ActionStartWithChanges, domain.ActionDecline, domain.ActionSnooze,
		},
		domain.AttentionEffectProposal: {
			domain.ActionApprove, domain.ActionApproveWithChanges,
			domain.ActionDecline, domain.ActionSnooze,
		},
		domain.AttentionSystemHealth: {
			domain.ActionAcknowledge, domain.ActionRunDoctor, domain.ActionStopUnattended,
			domain.ActionResumeUnattended, domain.ActionResolveReenrollment,
		},
		domain.AttentionBlocked: {},
	}

	if len(fixtures) != len(domain.AllAttentionTypes) || len(allowedActionsByType) != len(fixtures) {
		t.Fatalf("attention-type fixture/table sizes = %d/%d, want %d registered types",
			len(fixtures), len(allowedActionsByType), len(domain.AllAttentionTypes))
	}
	// The table is checked on an item that carries a reference wherever the
	// type permits one, so open_pr's reference condition does not hide a cell;
	// TestOpenPRNeedsAPRReference pins that condition by itself.
	offering := func(itemType domain.AttentionType, actions []domain.Action) domain.AttentionItem {
		item := domain.AttentionItem{Type: itemType, RequestedDecision: actions}
		if itemType == domain.AttentionReadyForFinalReview || itemType == domain.AttentionPublishBlocked {
			item.PRReference = &domain.PRReference{Repo: "owner/repo", Number: 123}
		}
		return item
	}
	for _, itemType := range domain.AllAttentionTypes {
		allowed, ok := fixtures[itemType]
		if !ok {
			t.Fatalf("missing fixture for attention type %q", itemType)
		}
		t.Run(string(itemType), func(t *testing.T) {
			if err := validateRequestedActions(offering(itemType, allowed)); err != nil {
				t.Fatalf("allowed set rejected: %v", err)
			}
			for _, action := range domain.AllActions {
				if slices.Contains(allowed, action) {
					continue
				}
				if err := validateRequestedActions(offering(itemType, []domain.Action{action})); !errors.Is(err, ErrActionNotAllowedForType) {
					t.Errorf("action %q error = %v, want ErrActionNotAllowedForType", action, err)
				}
			}
			if itemType != domain.AttentionBlocked {
				if err := validateRequestedActions(offering(itemType, nil)); !errors.Is(err, domain.ErrNoActions) {
					t.Errorf("empty set error = %v, want ErrNoActions", err)
				}
			}
		})
	}
}

// TestOpenPRNeedsAPRReference pins the conditional cell of the action table:
// a publish_blocked item offers open_pr only when it carries the pull request
// the action opens. The app's cross-language policy matrix leaves this cell to
// this test, because its seeded holds carry no reference.
func TestOpenPRNeedsAPRReference(t *testing.T) {
	hold := domain.AttentionItem{
		Type:              domain.AttentionPublishBlocked,
		RequestedDecision: []domain.Action{domain.ActionInspectTrustFailure, domain.ActionOpenPR},
	}
	if err := validateRequestedActions(hold); !errors.Is(err, ErrActionNotAllowedForType) {
		t.Fatalf("open_pr without a reference = %v, want ErrActionNotAllowedForType", err)
	}
	hold.PRReference = &domain.PRReference{Repo: "owner/repo", Number: 123}
	if err := validateRequestedActions(hold); err != nil {
		t.Fatalf("open_pr with a reference = %v, want accepted", err)
	}
	// A hold with a reference need not offer the action.
	hold.RequestedDecision = []domain.Action{domain.ActionInspectTrustFailure}
	if err := validateRequestedActions(hold); err != nil {
		t.Fatalf("reference without open_pr = %v, want accepted", err)
	}
}
