package signet

import (
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestAlternateAgentRetryAcceptsOnlyAnOfferedAgent(t *testing.T) {
	t.Parallel()
	runID := domain.RunID("run-alternate-agent")
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-alternate-agent", ProjectID: "proj-1",
		Subject: domain.Subject{
			Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID,
		},
		Type: domain.AttentionExecutionFailure, Priority: domain.PriorityHigh,
		Reason: "implementation hit its quota",
		RequestedDecision: []domain.Action{
			domain.ActionRetryWithAlternateAgent, domain.ActionDiscuss, domain.ActionStop,
		},
		ExecutionFailure: &domain.ExecutionFailureFacts{
			Outcome: domain.ExecutionOutcomeFailed, Cause: domain.FailureCauseQuota,
			Stage: domain.StageNameImplementation, InvocationID: "inv-implement-run-alternate-agent",
		},
		AlternateAgents: []domain.AlternateAgentOffer{{
			Agent: "claude-second", Pool: "identity:claude-second",
			Harness: domain.HarnessClientClaudeCode, Model: "claude-test",
			Qualification: domain.AlternateAgentDifferentPool,
		}},
		ItemVersion: 1, InterruptionClass: domain.InterruptionExceptional,
		Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	command, err := domain.NewCommand(domain.CommandInput{
		CommandID: "command-alternate-agent", DeviceID: "device-1",
		ItemID: item.ID, ItemVersion: item.ItemVersion,
		Action: domain.ActionRetryWithAlternateAgent, Message: "claude-second",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateAlternateAgentRetryDecision(command, item); err != nil {
		t.Fatalf("offered agent rejected: %v", err)
	}
	if err := (&Service{}).validateCommandContent(command); err != nil {
		t.Fatalf("agent name rejected by shared content gate: %v", err)
	}

	command.Message = "claude-third"
	if err := validateAlternateAgentRetryDecision(command, item); !errors.Is(err, ErrAlternateAgentNotOffered) {
		t.Fatalf("unoffered agent error = %v", err)
	}
	command.Message = ""
	if err := validateAlternateAgentRetryDecision(command, item); !errors.Is(err, ErrInvalidAlternateAgentDecisionPayload) {
		t.Fatalf("empty agent error = %v", err)
	}
}

func TestAlternateAgentDecisionPayloadCarriesOnlyTheAgent(t *testing.T) {
	t.Parallel()
	agent := "claude-second"
	message, err := decisionMessage(DecisionPayload{
		Action: domain.ActionRetryWithAlternateAgent, AlternateAgent: &agent,
	})
	if err != nil || message != agent {
		t.Fatalf("decisionMessage = %q, %v; want the agent", message, err)
	}
	digest := domain.Digest("sha256:" + "ab")
	empty := ""
	for name, payload := range map[string]DecisionPayload{
		"no agent":    {Action: domain.ActionRetryWithAlternateAgent},
		"empty agent": {Action: domain.ActionRetryWithAlternateAgent, AlternateAgent: &empty},
		"with message": {
			Action: domain.ActionRetryWithAlternateAgent, AlternateAgent: &agent, Message: "please",
		},
		"with manifest": {
			Action: domain.ActionRetryWithAlternateAgent, AlternateAgent: &agent,
			CapabilityManifestDigest: &digest,
		},
		"agent on another action": {Action: domain.ActionStop, AlternateAgent: &agent},
	} {
		if _, err := decisionMessage(payload); !errors.Is(err, ErrInvalidAlternateAgentDecisionPayload) {
			t.Errorf("%s: decisionMessage error = %v", name, err)
		}
	}
}
