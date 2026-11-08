package signet

import (
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestDecisionMessageRejectsPresentEmptyAlternativeChoices(t *testing.T) {
	t.Parallel()
	snoozeUntil := time.Date(2026, 8, 22, 1, 0, 0, 0, time.UTC)
	revision := &TaskProposalRevisionInput{
		Intent:            domain.TaskProposalIntentImplement,
		ExpectedCostUnits: 1,
		Scope: domain.TaskProposalScope{
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
			Action: domain.ActionStartWithChanges, TaskProposalRevision: revision,
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

// A move receipt stored before #2808 holds no choice, and one from before
// #2735 no retry selection; neither has a wire form, so rendering refuses it
// rather than send a record without the fields the schema requires.
func TestAMoveReceiptWithoutAChoiceAndSelectionHasNoWireForm(t *testing.T) {
	t.Parallel()
	selection := &domain.TaskLineSelection{EnrollmentID: "claude-b/claude_code", ModelID: "claude-opus", Effort: "high"}
	for name, move := range map[string]domain.ParkedTaskMoveReceipt{
		"agent and selection": {
			ParkedTaskMoveRequest: domain.ParkedTaskMoveRequest{TaskID: "task-1", Agent: "claude-b", Selection: selection},
			Retry:                 domain.AlternateAgentRetry{Selection: selection},
		},
		"choice on a v1 record": {
			ParkedTaskMoveRequest: domain.ParkedTaskMoveRequest{
				TaskID: "task-1", Choice: &domain.AlternateAgentChoice{EnrollmentID: "claude-b/claude_code"},
			},
			Retry: domain.AlternateAgentRetry{Agent: "claude-b"},
		},
	} {
		if _, err := kindedReceiptRecord(domain.CommandReceipt{ParkedTaskMove: &move}, nil); !errors.Is(err, domain.ErrCommandReceiptArms) {
			t.Errorf("%s: err = %v, want ErrCommandReceiptArms", name, err)
		}
	}
}
