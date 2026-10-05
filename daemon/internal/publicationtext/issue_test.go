package publicationtext

import (
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const issueBodyMax = 16 << 10

func TestScreenIssueBodyGitHubIssue1Refusals(t *testing.T) {
	for _, tc := range []struct {
		name, text, category string
	}{
		{"close directive", "Closes #1", "message_rules"},
		{"secret", "token ghp_" + strings.Repeat("x", 36), "secret_detection"},
		{"reserved section", "## Verification", "candidate_body_size_or_reserved_section"},
		{"freeside marker", "see freeside:unknown", "publisher_marker"},
		{"mention", "ask @octocat to look", "mention"},
		{"team mention", "ask @acme/reviewers", "mention"},
		{"mention at start", "@octocat", "mention"},
		{"email", "write to dev@example.com", "mention"},
		{"entity mention", "ask &#64;octocat", "mention"},
		{"emphasis mention", "ask @**octocat**", "mention"},
		{"linked mention", "ask @[octocat](https://example.com)", "mention"},
		{"slash command", "/close", "slash_command"},
		{"slash command on a later line", "Summary\n\n/label bug", "slash_command"},
		{"indented slash command", "Summary\n  /assign me", "slash_command"},
		{"escaped slash command", `\/close`, "slash_command"},
		{"entity slash command", "&#47;close", "slash_command"},
		{"slash command after a line separator", "Context\u2028/assign me", "line_separator"},
		{"slash command after a paragraph separator", "Context\u2029/label bug", "line_separator"},
		{"oversized", strings.Repeat("x", issueBodyMax+1), "encoding_or_size"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ScreenIssueBody(domain.IssueTextRulesetGitHubIssue1, tc.text, issueBodyMax)
			if err == nil || err.Error() != "issue body: "+tc.category {
				t.Fatalf("error = %v, want category %s", err, tc.category)
			}
			if tc.text != "" && len(tc.text) < 64 && strings.Contains(err.Error(), tc.text) {
				t.Fatalf("refusal echoes the refused text: %v", err)
			}
		})
	}
}

func TestScreenIssueTextGitHubIssue1Accepts(t *testing.T) {
	body := "## Problem\n\nThe retry loop ignores `ctx.Done()`.\n\n" +
		"- See internal/engine/retry.go and https://example.com/a/b.\n" +
		"- A lone @ sign, a trailing @, and a path like a/b/c are fine.\n" +
		"// a comment line is not a command\n"
	if err := ScreenIssueBody(domain.IssueTextRulesetGitHubIssue1, body, issueBodyMax); err != nil {
		t.Fatalf("body: %v", err)
	}
	if err := ScreenIssueTitle(domain.IssueTextRulesetGitHubIssue1, "Honor cancellation in the retry loop", 256); err != nil {
		t.Fatalf("title: %v", err)
	}
}

func TestScreenIssueTitleRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, text, want string
	}{
		{"two lines", "First\nSecond", "issue title: not_one_line"},
		{"carriage return", "First\rSecond", "issue title: not_one_line"},
		{"line separator", "First\u2028/close", "issue title: line_separator"},
		{"mention", "Ping @octocat", "issue title: mention"},
		{"slash command", "/close this", "issue title: slash_command"},
		{"empty", "", "issue title: message_rules"},
		{"oversized", strings.Repeat("x", 257), "issue title: encoding_or_size"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ScreenIssueTitle(domain.IssueTextRulesetGitHubIssue1, tc.text, 256)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("error = %v, want %s", err, tc.want)
			}
		})
	}
}

// An identifier outside the registry fails closed, whatever the text.
func TestScreenIssueTextUnknownRulesetFailsClosed(t *testing.T) {
	for _, ruleset := range []domain.IssueTextRuleset{"", "github-issue/2", "github/1"} {
		if err := ScreenIssueBody(ruleset, "plain text", issueBodyMax); err == nil ||
			err.Error() != "issue body: unknown_ruleset" {
			t.Fatalf("ruleset %q body error = %v", ruleset, err)
		}
		if err := ScreenIssueTitle(ruleset, "plain text", 256); err == nil ||
			err.Error() != "issue title: unknown_ruleset" {
			t.Fatalf("ruleset %q title error = %v", ruleset, err)
		}
	}
}

// Every registered ruleset has screening rules: a registry member the
// dispatch does not handle would otherwise refuse all text silently.
func TestEveryRegisteredIssueRulesetScreens(t *testing.T) {
	for _, ruleset := range domain.AllIssueTextRulesets {
		if err := ScreenIssueBody(ruleset, "plain text", issueBodyMax); err != nil {
			t.Fatalf("ruleset %q: %v", ruleset, err)
		}
	}
}
