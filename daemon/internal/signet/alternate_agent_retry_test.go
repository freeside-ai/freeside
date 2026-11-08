package signet

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// alternateAgentItem is an implementation quota failure offering offers.
func alternateAgentItem(t *testing.T, offers ...domain.AlternateAgentOffer) domain.AttentionItem {
	t.Helper()
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
		AlternateAgents: offers,
		ItemVersion:     1, InterruptionClass: domain.InterruptionExceptional,
		Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return item
}

// enrollmentOffer is an offer of enrollment under the catalog fixture
// tree's implementer lineup model.
func enrollmentOffer(enrollment domain.ClientEnrollmentID) domain.AlternateAgentOffer {
	return domain.AlternateAgentOffer{
		Enrollment: enrollment, Pool: domain.UsagePoolID("identity:" + strings.TrimSuffix(string(enrollment), "/claude_code")),
		Harness: domain.HarnessClientClaudeCode, Model: "claude-opus-5-5",
		Qualification: domain.AlternateAgentDifferentPool,
	}
}

func TestAlternateAgentRetryAcceptsOnlyAnOfferedEnrollment(t *testing.T) {
	t.Parallel()
	item := alternateAgentItem(t, enrollmentOffer("anthropic-a/claude_code"))
	decide := func(message string) domain.Command {
		command, err := domain.NewCommand(domain.CommandInput{
			CommandID: "command-alternate-agent", DeviceID: "device-1",
			ItemID: item.ID, ItemVersion: item.ItemVersion,
			Action: domain.ActionRetryWithAlternateAgent, Message: message,
		})
		if err != nil {
			t.Fatal(err)
		}
		return command
	}
	offered := decide(domain.AlternateAgentChoice{EnrollmentID: "anthropic-a/claude_code"}.Message())
	if err := validateAlternateAgentRetryDecision(offered, item); err != nil {
		t.Fatalf("offered enrollment rejected: %v", err)
	}
	if err := (&Service{}).validateCommandContent(offered); err != nil {
		t.Fatalf("choice rejected by shared content gate: %v", err)
	}
	unoffered := decide(domain.AlternateAgentChoice{EnrollmentID: "anthropic-b/claude_code"}.Message())
	if err := validateAlternateAgentRetryDecision(unoffered, item); !errors.Is(err, ErrAlternateAgentNotOffered) {
		t.Fatalf("unoffered enrollment error = %v", err)
	}
	// A v1 card offers agents, which no choice of an enrollment names.
	v1 := alternateAgentItem(t, domain.AlternateAgentOffer{
		Agent: "claude-a", Pool: "identity:anthropic-a", Harness: domain.HarnessClientClaudeCode,
		Model: "claude-opus-5-5", Qualification: domain.AlternateAgentDifferentPool,
	})
	if err := validateAlternateAgentRetryDecision(offered, v1); !errors.Is(err, ErrAlternateAgentNotOffered) {
		t.Fatalf("a choice on a v1 card error = %v", err)
	}
	for name, message := range map[string]string{
		"empty":         "",
		"an agent name": "claude-a",
	} {
		if err := validateAlternateAgentRetryDecision(decide(message), item); !errors.Is(err, ErrInvalidAlternateAgentDecisionPayload) {
			t.Fatalf("%s message error = %v", name, err)
		}
	}
}

func TestAlternateAgentDecisionPayloadCarriesOnlyTheAgent(t *testing.T) {
	t.Parallel()
	tree := catalogFixtureTree(t)
	names := agentNames{tree: &tree}
	agent := "claude-a"
	message, err := decisionMessage(DecisionPayload{
		Action: domain.ActionRetryWithAlternateAgent, AlternateAgent: &agent,
	}, names)
	want := domain.AlternateAgentChoice{
		EnrollmentID: "anthropic-a/claude_code", ModelID: "claude-opus-5-5", Effort: "high",
	}.Message()
	if err != nil || message != want {
		t.Fatalf("decisionMessage = %q, %v; want the choice %q", message, err, want)
	}
	unknown := "claude-unknown"
	for name, names := range map[string]agentNames{"an unknown agent": names, "no tree": {}} {
		_, err := decisionMessage(DecisionPayload{
			Action: domain.ActionRetryWithAlternateAgent, AlternateAgent: &unknown,
		}, names)
		if !errors.Is(err, ErrAlternateAgentNotOffered) || strings.Contains(err.Error(), unknown) {
			t.Errorf("%s: decisionMessage error = %v, want not offered without the name", name, err)
		}
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
		if _, err := decisionMessage(payload, names); !errors.Is(err, ErrInvalidAlternateAgentDecisionPayload) {
			t.Errorf("%s: decisionMessage error = %v", name, err)
		}
	}
}

// TestAlternateAgentOffersRenderAsAgents: the wire names each enrollment
// offer by the tree agent that runs the implementer's lineup model and effort
// on it, drops one no agent maps to, and drops the retry action with the
// last offer. A v1 offer already names its agent.
func TestAlternateAgentOffersRenderAsAgents(t *testing.T) {
	t.Parallel()
	tree := catalogFixtureTree(t)
	names := agentNames{tree: &tree}
	a, b := enrollmentOffer("anthropic-a/claude_code"), enrollmentOffer("anthropic-b/claude_code")
	rendered := names.renderItem(alternateAgentItem(t, a, b))
	wantOffer := a
	wantOffer.Agent, wantOffer.Enrollment = "claude-a", ""
	if !reflect.DeepEqual(rendered.AlternateAgents, []domain.AlternateAgentOffer{wantOffer}) ||
		!reflect.DeepEqual(rendered.RequestedDecision, []domain.Action{
			domain.ActionRetryWithAlternateAgent, domain.ActionDiscuss, domain.ActionStop,
		}) {
		t.Fatalf("rendered = %+v %v, want only %+v and the action", rendered.AlternateAgents,
			rendered.RequestedDecision, wantOffer)
	}

	item := alternateAgentItem(t, b)
	rendered = names.renderItem(item)
	if rendered.AlternateAgents != nil ||
		slices.Contains(rendered.RequestedDecision, domain.ActionRetryWithAlternateAgent) ||
		!slices.Contains(item.RequestedDecision, domain.ActionRetryWithAlternateAgent) {
		t.Fatalf("rendered with no agent = %+v %v, want no offers or action and the stored item unchanged",
			rendered.AlternateAgents, rendered.RequestedDecision)
	}
	if rendered := (agentNames{}).renderItem(alternateAgentItem(t, a)); rendered.AlternateAgents != nil {
		t.Fatalf("rendered with no tree = %+v, want no offers", rendered.AlternateAgents)
	}

	v1 := domain.AlternateAgentOffer{
		Agent: "claude-old", Pool: "identity:anthropic-a", Harness: domain.HarnessClientClaudeCode,
		Model: "claude-opus-5-5", Qualification: domain.AlternateAgentDifferentPool,
	}
	if rendered := names.renderItem(alternateAgentItem(t, v1)); !reflect.DeepEqual(rendered.AlternateAgents, []domain.AlternateAgentOffer{v1}) {
		t.Fatalf("rendered v1 = %+v, want it unchanged", rendered.AlternateAgents)
	}
}
