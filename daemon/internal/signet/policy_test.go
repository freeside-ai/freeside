package signet

import (
	"errors"
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestAllowedActionsByType is the independent plan §4 fixture for all ten
// Phase 1 attention types. It pins both halves of each allowed set: the listed
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
			domain.ActionAdjudicate, domain.ActionDiscuss, domain.ActionStop,
		},
		domain.AttentionExecutionFailure: {
			domain.ActionRetry, domain.ActionRetryWithCapability, domain.ActionDiscuss, domain.ActionStop,
		},
		domain.AttentionAgentQuestion: {
			domain.ActionAnswerAndRetry, domain.ActionAnswerWithoutRetry, domain.ActionStop,
		},
		domain.AttentionPublishBlocked: {
			domain.ActionRerunTrustEvaluation, domain.ActionChooseAlternate,
			domain.ActionInspectTrustFailure, domain.ActionStop,
		},
		domain.AttentionReadyForFinalReview: {
			domain.ActionOpenPR, domain.ActionReturnToAgent, domain.ActionMarkSeen,
			domain.ActionDismiss, domain.ActionStop,
		},
		domain.AttentionRunProposal: {
			domain.ActionStart, domain.ActionStartWithChanges, domain.ActionDecline, domain.ActionSnooze,
		},
		domain.AttentionSystemHealth: {
			domain.ActionAcknowledge, domain.ActionRunDoctor, domain.ActionStopUnattended,
		},
		domain.AttentionBlocked: {},
	}

	if len(fixtures) != len(domain.AllAttentionTypes) || len(allowedActionsByType) != len(fixtures) {
		t.Fatalf("attention-type fixture/table sizes = %d/%d, want %d registered types",
			len(fixtures), len(allowedActionsByType), len(domain.AllAttentionTypes))
	}
	for _, itemType := range domain.AllAttentionTypes {
		allowed, ok := fixtures[itemType]
		if !ok {
			t.Fatalf("missing fixture for attention type %q", itemType)
		}
		t.Run(string(itemType), func(t *testing.T) {
			if err := validateRequestedActions(itemType, allowed); err != nil {
				t.Fatalf("allowed set rejected: %v", err)
			}
			for _, action := range domain.AllActions {
				if slices.Contains(allowed, action) {
					continue
				}
				if err := validateRequestedActions(itemType, []domain.Action{action}); !errors.Is(err, ErrActionNotAllowedForType) {
					t.Errorf("action %q error = %v, want ErrActionNotAllowedForType", action, err)
				}
			}
			if itemType != domain.AttentionBlocked {
				if err := validateRequestedActions(itemType, nil); !errors.Is(err, domain.ErrNoActions) {
					t.Errorf("empty set error = %v, want ErrNoActions", err)
				}
			}
		})
	}
}
