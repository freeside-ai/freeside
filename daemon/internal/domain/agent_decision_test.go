package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

func TestValidateDecisionsLimits(t *testing.T) {
	base := validDecisions()[0]
	withOptions := func(n int) domain.Decision {
		d := base
		d.Options = nil
		for i := 0; i < n; i++ {
			d.Options = append(d.Options, domain.DecisionOption{
				Label: strings.Repeat("x", i+1), Tradeoffs: "tradeoffs",
			})
		}
		d.Recommendation = "x"
		return d
	}
	repeat := func(n int) []domain.Decision {
		out := make([]domain.Decision, n)
		for i := range out {
			out[i] = base
		}
		return out
	}

	valid := [][]domain.Decision{
		repeat(1),
		repeat(domain.MaxDecisionsPerResult),
		{withOptions(domain.MinDecisionOptions)},
		{withOptions(domain.MaxDecisionOptions)},
	}
	for _, decisions := range valid {
		if err := domain.ValidateDecisions(decisions); err != nil {
			t.Errorf("ValidateDecisions(%d decisions, %d options) = %v",
				len(decisions), len(decisions[0].Options), err)
		}
	}

	over := base
	over.Question = strings.Repeat("q", domain.MaxDecisionTextBytes+1)
	untrimmed := base
	untrimmed.WhyBlocking = " leading space"
	noMatch := base
	noMatch.Recommendation = "neither"
	duplicate := withOptions(2)
	duplicate.Options[1].Label = duplicate.Options[0].Label
	duplicate.Recommendation = duplicate.Options[0].Label
	invalidUTF8 := base
	invalidUTF8.Options = []domain.DecisionOption{
		{Label: "a", Tradeoffs: "ok"}, {Label: "b", Tradeoffs: "bad \xff byte"},
	}
	invalidUTF8.Recommendation = "a"

	invalid := []struct {
		name      string
		decisions []domain.Decision
		want      error
	}{
		{"none", nil, domain.ErrDecisionInvalid},
		{"empty", []domain.Decision{}, domain.ErrDecisionInvalid},
		{"nine", repeat(domain.MaxDecisionsPerResult + 1), domain.ErrDecisionInvalid},
		{"zero options", []domain.Decision{withOptions(0)}, domain.ErrDecisionInvalid},
		{"one option", []domain.Decision{withOptions(1)}, domain.ErrDecisionInvalid},
		{"seven options", []domain.Decision{withOptions(domain.MaxDecisionOptions + 1)}, domain.ErrDecisionInvalid},
		{"recommendation matches no label", []domain.Decision{noMatch}, domain.ErrDecisionInvalid},
		{"duplicate option label", []domain.Decision{duplicate}, domain.ErrDuplicate},
		{"text over limit", []domain.Decision{over}, domain.ErrDecisionInvalid},
		{"untrimmed text", []domain.Decision{untrimmed}, domain.ErrDecisionInvalid},
		{"invalid utf-8", []domain.Decision{invalidUTF8}, domain.ErrDecisionInvalid},
	}
	for _, test := range invalid {
		if err := domain.ValidateDecisions(test.decisions); !errors.Is(err, test.want) {
			t.Errorf("%s: ValidateDecisions() = %v, want %v", test.name, err, test.want)
		}
	}
}

func TestBlockedOutcomeDecode(t *testing.T) {
	for _, kind := range domain.AllBlockedKinds {
		body, err := domain.EncodeBlockedOutcome(domain.BlockedOutcome{
			Version: domain.BlockedOutcomeEncodingVersion, Kind: kind, Decisions: validDecisions(),
		})
		if err != nil {
			t.Fatalf("encode %s: %v", kind, err)
		}
		decoded, err := domain.DecodeBlockedOutcome(body)
		if err != nil {
			t.Fatalf("decode %s: %v", kind, err)
		}
		if decoded.Kind != kind || len(decoded.Decisions) != 1 {
			t.Fatalf("decode %s = %+v", kind, decoded)
		}
	}

	invalid := []struct {
		name string
		body string
		want error
	}{
		{"unknown kind", `{"version":"freeside.blocked-outcome/v1","kind":"tired","decisions":[{"question":"q","why_blocking":"w","options":[{"label":"a","tradeoffs":"t"},{"label":"b","tradeoffs":"t"}],"recommendation":"a"}]}`, domain.ErrInvalidBlockedKind},
		{"wrong version", `{"version":"freeside.blocked-outcome/v2","kind":"owner_decision","decisions":[{"question":"q","why_blocking":"w","options":[{"label":"a","tradeoffs":"t"},{"label":"b","tradeoffs":"t"}],"recommendation":"a"}]}`, domain.ErrBlockedOutcomeInvalid},
		{"no decisions", `{"version":"freeside.blocked-outcome/v1","kind":"owner_decision","decisions":[]}`, domain.ErrDecisionInvalid},
		{"unknown field", `{"version":"freeside.blocked-outcome/v1","kind":"owner_decision","decisions":[],"extra":1}`, nil},
	}
	for _, test := range invalid {
		_, err := domain.DecodeBlockedOutcome([]byte(test.body))
		if err == nil || (test.want != nil && !errors.Is(err, test.want)) {
			t.Errorf("%s: DecodeBlockedOutcome() = %v, want %v", test.name, err, test.want)
		}
	}
}

func TestBlockedOutcomeEncodeRejectsCanonicalBodyOverLimit(t *testing.T) {
	decisions := validDecisions()
	decisions[0].Options = []domain.DecisionOption{
		{Label: "a", Tradeoffs: strings.Repeat("&", domain.MaxDecisionTextBytes)},
		{Label: "b", Tradeoffs: strings.Repeat("&", domain.MaxDecisionTextBytes)},
		{Label: "c", Tradeoffs: strings.Repeat("&", domain.MaxDecisionTextBytes)},
	}
	decisions[0].Recommendation = "a"
	_, err := domain.EncodeBlockedOutcome(domain.BlockedOutcome{
		Version: domain.BlockedOutcomeEncodingVersion,
		Kind:    domain.BlockedKindOwnerDecision, Decisions: decisions,
	})
	if !errors.Is(err, strictjson.ErrLimitExceeded) {
		t.Fatalf("EncodeBlockedOutcome() = %v, want %v", err, strictjson.ErrLimitExceeded)
	}
}

func TestAttentionItemAgentQuestionFacts(t *testing.T) {
	kind := domain.BlockedKindOwnerDecision
	implementation := validItemInput(domain.AttentionAgentQuestion)
	implementation.AgentQuestion = &domain.AgentQuestionFacts{
		Stage: domain.StageNameImplementation, InvocationID: "inv-implementation-1",
		Kind: &kind, Decisions: validDecisions(),
	}
	implementation.AgentClaims = []domain.AgentClaim{agentQuestionClaim(implementation.AgentQuestion)}
	for _, in := range []domain.AttentionItemInput{validItemInput(domain.AttentionAgentQuestion), implementation} {
		if _, err := domain.NewAttentionItem(in, nil); err != nil {
			t.Errorf("valid %s agent question rejected: %v", in.AgentQuestion.Stage, err)
		}
	}

	missing := validItemInput(domain.AttentionAgentQuestion)
	missing.AgentQuestion = nil
	elsewhere := validItemInput(domain.AttentionSpecApproval)
	elsewhere.AgentQuestion = validItemInput(domain.AttentionAgentQuestion).AgentQuestion
	specKind := validItemInput(domain.AttentionAgentQuestion)
	specKind.AgentQuestion.Kind = &kind
	implNoKind := implementation
	implNoKind.AgentQuestion = &domain.AgentQuestionFacts{
		Stage: domain.StageNameImplementation, InvocationID: "inv-implementation-1",
		Decisions: validDecisions(),
	}
	reviewStage := validItemInput(domain.AttentionAgentQuestion)
	reviewStage.AgentQuestion.Stage = domain.StageNameReview
	noClaim := validItemInput(domain.AttentionAgentQuestion)
	noClaim.AgentClaims = nil
	otherInvocation := validItemInput(domain.AttentionAgentQuestion)
	otherFacts := *otherInvocation.AgentQuestion
	otherFacts.InvocationID = "inv-other"
	otherInvocation.AgentClaims = []domain.AgentClaim{agentQuestionClaim(&otherFacts)}
	twoClaims := validItemInput(domain.AttentionAgentQuestion)
	twoClaims.AgentClaims = append(twoClaims.AgentClaims, agentQuestionClaim(twoClaims.AgentQuestion))
	twoClaims.AgentClaims[1].Artifact = "decisions-other"
	noRun := validItemInput(domain.AttentionAgentQuestion)
	noRun.Subject.RunID = nil
	badDecision := validItemInput(domain.AttentionAgentQuestion)
	badDecision.AgentQuestion.Decisions = nil
	tamperedDecisions := validItemInput(domain.AttentionAgentQuestion)
	tamperedDecisions.AgentQuestion.Decisions[0].Recommendation = "1 year"
	tamperedKind := implementation
	otherKind := domain.BlockedKindCapabilityUnavailable
	tamperedKind.AgentQuestion = &domain.AgentQuestionFacts{
		Stage: domain.StageNameImplementation, InvocationID: "inv-implementation-1",
		Kind: &otherKind, Decisions: validDecisions(),
	}

	invalid := []struct {
		name string
		in   domain.AttentionItemInput
		want error
	}{
		{"facts missing", missing, domain.ErrCardFactInconsistent},
		{"facts on another type", elsewhere, domain.ErrCardFactOutsideItem},
		{"kind on specification stage", specKind, domain.ErrCardFactInconsistent},
		{"no kind on implementation stage", implNoKind, domain.ErrCardFactInconsistent},
		{"review stage", reviewStage, domain.ErrCardFactInconsistent},
		{"no question claim", noClaim, domain.ErrCardFactInconsistent},
		{"claim from another invocation", otherInvocation, domain.ErrCardFactInconsistent},
		{"two question claims", twoClaims, domain.ErrCardFactInconsistent},
		{"no run subject", noRun, domain.ErrCardFactInconsistent},
		{"invalid decisions", badDecision, domain.ErrDecisionInvalid},
		{"decisions disagree with claim digest", tamperedDecisions, domain.ErrCardFactInconsistent},
		{"blocked kind disagrees with claim digest", tamperedKind, domain.ErrCardFactInconsistent},
	}
	for _, test := range invalid {
		if _, err := domain.NewAttentionItem(test.in, nil); !errors.Is(err, test.want) {
			t.Errorf("%s: NewAttentionItem() = %v, want %v", test.name, err, test.want)
		}
	}

	// The facts are immutable across versions and detached from caller input.
	first := mustItem(t, validItemInput(domain.AttentionAgentQuestion))
	changedInput := validItemInput(domain.AttentionAgentQuestion)
	changedInput.ItemVersion = 2
	changedInput.AgentQuestion.Decisions[0].Recommendation = "1 year"
	changedInput.AgentClaims = []domain.AgentClaim{agentQuestionClaim(changedInput.AgentQuestion)}
	changed := mustItem(t, changedInput)
	if err := domain.ValidateAttentionItemTransition(first, changed); !errors.Is(err, domain.ErrImmutableTransition) {
		t.Fatalf("changing agent question facts = %v, want ErrImmutableTransition", err)
	}
	detachedInput := validItemInput(domain.AttentionAgentQuestion)
	detached := mustItem(t, detachedInput)
	detachedInput.AgentQuestion.Decisions[0].Options[0].Label = "mutated"
	if detached.AgentQuestion.Decisions[0].Options[0].Label == "mutated" {
		t.Fatal("agent question decisions remained caller-owned")
	}
}
