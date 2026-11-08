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

// TestAlternateAgentDecisionPayloadCarriesOnlyTheChoice: the decision's
// durable message is its choice's canonical message, a null model or effort
// left to the lineup, and every other content, a missing choice, or an
// invalid value is refused naming the field and never the text.
func TestAlternateAgentDecisionPayloadCarriesOnlyTheChoice(t *testing.T) {
	t.Parallel()
	model, effort := "claude-opus-5-5", "high"
	full := &AlternateAgentChoice{EnrollmentID: "anthropic-a/claude_code", ModelID: &model, Effort: &effort}
	for name, tc := range map[string]struct {
		choice *AlternateAgentChoice
		want   domain.AlternateAgentChoice
	}{
		"full": {full, domain.AlternateAgentChoice{
			EnrollmentID: "anthropic-a/claude_code", ModelID: model, Effort: effort,
		}},
		"enrollment alone": {
			&AlternateAgentChoice{EnrollmentID: "anthropic-a/claude_code"},
			domain.AlternateAgentChoice{EnrollmentID: "anthropic-a/claude_code"},
		},
	} {
		message, err := decisionMessage(DecisionPayload{
			Action: domain.ActionRetryWithAlternateAgent, AlternateAgentChoice: tc.choice,
		})
		if err != nil || message != tc.want.Message() {
			t.Errorf("%s: decisionMessage = %q, %v; want %q", name, message, err, tc.want.Message())
		}
	}
	digest := domain.Digest("sha256:" + "ab")
	empty, secret := "", "secret text"
	for name, tc := range map[string]struct {
		payload DecisionPayload
		field   string
	}{
		"no choice":        {DecisionPayload{Action: domain.ActionRetryWithAlternateAgent}, ""},
		"no enrollment":    {DecisionPayload{Action: domain.ActionRetryWithAlternateAgent, AlternateAgentChoice: &AlternateAgentChoice{}}, "enrollment_id"},
		"an empty model":   {DecisionPayload{Action: domain.ActionRetryWithAlternateAgent, AlternateAgentChoice: &AlternateAgentChoice{EnrollmentID: "anthropic-a/claude_code", ModelID: &empty}}, "model_id"},
		"an invalid model": {DecisionPayload{Action: domain.ActionRetryWithAlternateAgent, AlternateAgentChoice: &AlternateAgentChoice{EnrollmentID: "anthropic-a/claude_code", ModelID: &secret}}, "model_id"},
		"an empty effort":  {DecisionPayload{Action: domain.ActionRetryWithAlternateAgent, AlternateAgentChoice: &AlternateAgentChoice{EnrollmentID: "anthropic-a/claude_code", Effort: &empty}}, "effort"},
		"an invalid enrollment": {DecisionPayload{
			Action: domain.ActionRetryWithAlternateAgent, AlternateAgentChoice: &AlternateAgentChoice{EnrollmentID: domain.ClientEnrollmentID(secret)},
		}, "enrollment_id"},
		"with message": {DecisionPayload{
			Action: domain.ActionRetryWithAlternateAgent, AlternateAgentChoice: full, Message: "please",
		}, ""},
		"with manifest": {DecisionPayload{
			Action: domain.ActionRetryWithAlternateAgent, AlternateAgentChoice: full, CapabilityManifestDigest: &digest,
		}, ""},
		"choice on another action": {DecisionPayload{Action: domain.ActionStop, AlternateAgentChoice: full}, ""},
	} {
		_, err := decisionMessage(tc.payload)
		if !errors.Is(err, ErrInvalidAlternateAgentDecisionPayload) || !strings.Contains(err.Error(), tc.field) ||
			strings.Contains(err.Error(), secret) {
			t.Errorf("%s: decisionMessage error = %v, want invalid naming %q and not the text", name, err, tc.field)
		}
	}
}

// TestAlternateAgentOffersRenderAsEnrollments: the wire carries every
// enrollment offer and drops a set recorded before #2735, which names tree
// agents, with the retry action.
func TestAlternateAgentOffersRenderAsEnrollments(t *testing.T) {
	t.Parallel()
	a, b := enrollmentOffer("anthropic-a/claude_code"), enrollmentOffer("anthropic-b/claude_code")
	v1 := domain.AlternateAgentOffer{
		Agent: "claude-old", Pool: "identity:anthropic-a", Harness: domain.HarnessClientClaudeCode,
		Model: "claude-opus-5-5", Qualification: domain.AlternateAgentDifferentPool,
	}
	rendered := renderItem(alternateAgentItem(t, a, b))
	if !reflect.DeepEqual(rendered.AlternateAgents, []domain.AlternateAgentOffer{a, b}) ||
		!reflect.DeepEqual(rendered.RequestedDecision, []domain.Action{
			domain.ActionRetryWithAlternateAgent, domain.ActionDiscuss, domain.ActionStop,
		}) {
		t.Fatalf("rendered = %+v %v, want both offers and the action", rendered.AlternateAgents,
			rendered.RequestedDecision)
	}

	item := alternateAgentItem(t, v1)
	rendered = renderItem(item)
	if rendered.AlternateAgents != nil ||
		slices.Contains(rendered.RequestedDecision, domain.ActionRetryWithAlternateAgent) ||
		!slices.Contains(item.RequestedDecision, domain.ActionRetryWithAlternateAgent) {
		t.Fatalf("rendered v1 = %+v %v, want no offers or action and the stored item unchanged",
			rendered.AlternateAgents, rendered.RequestedDecision)
	}

	window := domain.QuotaWindowHold{AlternateAgents: []domain.AlternateAgentOffer{v1}}
	if hold := renderHold(&domain.RunHoldObservation{QuotaWindow: &window}); hold.QuotaWindow.AlternateAgents != nil ||
		window.AlternateAgents == nil {
		t.Fatalf("rendered v1 hold = %+v, want no offers and the stored hold unchanged", hold.QuotaWindow)
	}
}
