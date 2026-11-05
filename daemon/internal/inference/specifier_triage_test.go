package inference_test

import (
	"bytes"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/inference/fake"
)

// The triager is a choose site with the task namer's limits, and a call sends
// exactly its five allowlisted fields, redacted, with the triager's prompt.
func TestTriagerSiteAndCall(t *testing.T) {
	site := inference.TriagerSite(testBudget(1))
	if site.ID != inference.TriagerSiteID || site.Authority != inference.AuthorityChoose ||
		site.MaxInputBytes != 64<<10 || site.Retention != 14*24*time.Hour ||
		site.Timeout != 30*time.Second || site.AuditEvery != 10 || site.MaxComputeUnits != 10_000 {
		t.Fatalf("triager contract = %+v", site)
	}
	if err := site.ValidateOutput([]byte(site.FailSafe)); err != nil {
		t.Fatalf("fail-safe %s is not a valid answer: %v", site.FailSafe, err)
	}
	driver := fake.New()
	driver.Script(site.ID, fake.Script{Response: inference.Response{
		Output: []byte(`{"tier":"escalated","reason":"Changes the API schema."}`), ComputeUnits: 1,
	}})
	client, _, _ := testClient(t, driver, 1)
	input := inference.TriagerInput{
		Project: "project-1", RootLineage: "spec-1", ProjectName: "freeside",
		IssueTitle: "Add a field", IssueBody: "Body; token-value", IssueLabels: "kind:contract\nwave-9",
	}
	answer, reason, err := client.TriageSpecifier(t.Context(), input)
	if err != nil || reason != "" || answer != (inference.TriageAnswer{Tier: domain.TriageTierEscalated, Reason: "Changes the API schema."}) {
		t.Fatalf("TriageSpecifier = %+v, %q, %v", answer, reason, err)
	}
	requests := driver.Requests()
	if len(requests) != 1 {
		t.Fatalf("requests = %d", len(requests))
	}
	want := map[string]string{
		"project_name": "freeside", "issue_title": "Add a field", "issue_body": "Body; [REDACTED]",
		"issue_labels": "kind:contract\nwave-9", "source_text": "",
	}
	if !maps.Equal(requests[0].Fields, want) {
		t.Fatalf("outbound fields = %v, want %v", requests[0].Fields, want)
	}
	prompt, _ := inference.CodeOwnedRolePrompt(domain.RoleTriager)
	if !bytes.Equal(requests[0].RolePrompt, prompt.Body) || requests[0].RolePromptDigest != prompt.Digest ||
		!strings.Contains(string(prompt.Body), "escalated") {
		t.Fatalf("role prompt = %q (%s)", requests[0].RolePrompt, requests[0].RolePromptDigest)
	}
	// The budget is one call: the next returns its fallback reason.
	if answer, reason, err := client.TriageSpecifier(t.Context(), input); err != nil ||
		reason != inference.FallbackBudgetExhausted || answer != (inference.TriageAnswer{}) {
		t.Fatalf("budget fallback = %+v, %q, %v", answer, reason, err)
	}
}

func TestTriagerValidation(t *testing.T) {
	for _, tc := range []struct {
		label, output string
		valid         bool
	}{
		{label: "standard", output: `{"tier":"standard","reason":"A small fix."}`, valid: true},
		{label: "escalated", output: `{"tier":"escalated","reason":"A plan change."}`, valid: true},
		{label: "longest reason", output: `{"tier":"standard","reason":"` + strings.Repeat("a", 1024) + `"}`, valid: true},
		{label: "unknown tier", output: `{"tier":"large","reason":"Big."}`},
		{label: "missing tier", output: `{"reason":"Big."}`},
		{label: "extra key", output: `{"tier":"standard","reason":"Small.","agent":"claude-escalated"}`},
		{label: "duplicate key", output: `{"tier":"standard","tier":"escalated","reason":"Small."}`},
		{label: "blank reason", output: `{"tier":"standard","reason":" \n"}`},
		{label: "missing reason", output: `{"tier":"standard"}`},
		{label: "oversized reason", output: `{"tier":"standard","reason":"` + strings.Repeat("a", 1025) + `"}`},
		{label: "invalid utf8", output: "{\"tier\":\"standard\",\"reason\":\"\xff\"}"},
		{label: "credential", output: `{"tier":"standard","reason":"ghp_` + strings.Repeat("x", 36) + `"}`},
	} {
		t.Run(tc.label, func(t *testing.T) {
			err := inference.TriagerSite(testBudget(1)).ValidateOutput([]byte(tc.output))
			if (err == nil) != tc.valid {
				t.Fatalf("ValidateOutput(%s) = %v", tc.output, err)
			}
		})
	}
	// A refused answer reaches the caller as the schema fallback, never as a
	// tier.
	driver := fake.New()
	driver.Script(inference.TriagerSiteID, fake.Script{Response: inference.Response{Output: []byte(`{"tier":"large","reason":"Big."}`)}})
	client, _, _ := testClient(t, driver, 10)
	answer, reason, err := client.TriageSpecifier(t.Context(), inference.TriagerInput{Project: "p", RootLineage: "r", SourceText: "Do it"})
	if err != nil || reason != inference.FallbackOutputSchemaRejected || answer != (inference.TriageAnswer{}) {
		t.Fatalf("refused answer = %+v, %q, %v", answer, reason, err)
	}
	driver.Script(inference.TriagerSiteID, fake.Script{Err: errors.New("unavailable")})
	if _, reason, err := client.TriageSpecifier(t.Context(), inference.TriagerInput{Project: "p", RootLineage: "r", SourceText: "Do it"}); err != nil || reason != inference.FallbackDriverFailed {
		t.Fatalf("driver failure = %q, %v", reason, err)
	}
}

// Every fallback reason maps to the outcome the fail-safe names.
func TestTriageFallbackOutcome(t *testing.T) {
	special := map[inference.FallbackReason]domain.TriageOutcome{
		inference.FallbackBudgetExhausted:          domain.TriageOutcomeBudgetSpent,
		inference.FallbackOutputSchemaRejected:     domain.TriageOutcomeInvalidAnswer,
		inference.FallbackResponseExceededContract: domain.TriageOutcomeInvalidAnswer,
		inference.FallbackRoleOff:                  domain.TriageOutcomeNoTriagerLine,
		inference.FallbackRoleSourceAbsent:         domain.TriageOutcomeNoTriagerLine,
	}
	for _, reason := range inference.AllFallbackReasons {
		want, ok := special[reason]
		if !ok {
			want = domain.TriageOutcomeCallFailed
		}
		if got := inference.TriageFallbackOutcome(reason); got != want {
			t.Errorf("TriageFallbackOutcome(%s) = %s, want %s", reason, got, want)
		}
	}
	for reason := range special {
		if !slices.Contains(inference.AllFallbackReasons, reason) {
			t.Errorf("%s is not a registered fallback reason", reason)
		}
	}
}
