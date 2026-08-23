package signet

import (
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestDecisionMessageRejectsPresentEmptyAlternativeChoices(t *testing.T) {
	snoozeUntil := time.Date(2026, 8, 22, 1, 0, 0, 0, time.UTC)
	revision := &RunProposalRevisionInput{
		Intent:            domain.RunProposalIntentImplement,
		ExpectedCostUnits: 1,
		Scope: domain.RunProposalScope{
			ComponentCount: 1, DeclaredPathCount: 1,
		},
	}
	tests := []struct {
		name    string
		payload DecisionPayload
		want    error
	}{
		{"accept recommendation", DecisionPayload{
			Action: domain.ActionAcceptRecommendedRoute, AlternativeChoices: []AlternativeChoice{},
		}, ErrInvalidFindingAdjudicationDecisionPayload},
		{"start with changes", DecisionPayload{
			Action: domain.ActionStartWithChanges, RunProposalRevision: revision,
			AlternativeChoices: []AlternativeChoice{},
		}, ErrInvalidProposalDecisionPayload},
		{"snooze", DecisionPayload{
			Action: domain.ActionSnooze, SnoozeUntil: &snoozeUntil,
			AlternativeChoices: []AlternativeChoice{},
		}, ErrInvalidProposalDecisionPayload},
		{"unrelated action", DecisionPayload{
			Action: domain.ActionStop, AlternativeChoices: []AlternativeChoice{},
		}, ErrInvalidProposalDecisionPayload},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decisionMessage(test.payload); !errors.Is(err, test.want) {
				t.Fatalf("decisionMessage() = %v, want %v", err, test.want)
			}
		})
	}
}
