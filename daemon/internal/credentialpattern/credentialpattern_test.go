package credentialpattern_test

import (
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/credentialpattern"
)

// ruleCases are structurally valid but non-live tokens: the rules key on
// structure, so these exercise every rule without being real credentials.
// Every rule must appear here (TestEveryRuleHasCases), so a new rule can't
// land untested.
var ruleCases = map[string]struct {
	match   []string
	noMatch []string
}{
	"anthropic_token": {
		match: []string{
			"sk-ant-api03-" + strings.Repeat("A", 40),
			"prefixsk-ant-oat01-" + strings.Repeat("C", 40),
		},
		noMatch: []string{"sk-ant-oat01-...", "sk_ant_oat01_" + strings.Repeat("C", 40)},
	},
	"github_token": {
		match: []string{
			"ghp_" + strings.Repeat("A", 36),
			"GH_TOKEN_ghp_" + strings.Repeat("A", 36),
			"ghs_" + strings.Repeat("B", 36) + "_x",
			"gho_" + strings.Repeat("C", 16),
		},
		noMatch: []string{"ghp_" + strings.Repeat("D", 15), "ghp_", "ghx_" + strings.Repeat("E", 36)},
	},
	"github_pat": {
		match:   []string{"github_pat_" + strings.Repeat("B", 30)},
		noMatch: []string{"github_pat_" + strings.Repeat("B", 21), "xgithub_pat_" + strings.Repeat("B", 30)},
	},
	"aws_access_key_id": {
		match:   []string{"AKIA" + strings.Repeat("Q", 16), "ASIA" + strings.Repeat("7", 16)},
		noMatch: []string{"AKIA" + strings.Repeat("Q", 15), "AKIA" + strings.Repeat("Q", 17)},
	},
	"slack_token": {
		match:   []string{"xoxb-" + strings.Repeat("1", 20)},
		noMatch: []string{"xoxb-123", "xoxz-" + strings.Repeat("1", 20)},
	},
	"pem_private_key": {
		match:   []string{"-----BEGIN OPENSSH PRIVATE KEY-----", "-----BEGIN PRIVATE KEY-----"},
		noMatch: []string{"-----BEGIN PUBLIC KEY-----", "BEGIN PRIVATE KEY"},
	},
	"gcp_service_account_key": {
		match:   []string{`"private_key_id": "` + strings.Repeat("a", 40) + `"`},
		noMatch: []string{`"private_key_id": "` + strings.Repeat("a", 39) + `"`, `private_key_id = "abc"`},
	},
}

func TestRules(t *testing.T) {
	t.Parallel()
	for _, rule := range credentialpattern.All() {
		cases := ruleCases[rule.ID]
		t.Run(rule.ID, func(t *testing.T) {
			t.Parallel()
			for _, s := range cases.match {
				if !rule.Pattern.MatchString(s) {
					t.Errorf("rule %s must match %q", rule.ID, s)
				}
			}
			for _, s := range cases.noMatch {
				if rule.Pattern.MatchString(s) {
					t.Errorf("rule %s must not match %q", rule.ID, s)
				}
			}
		})
	}
}

func TestEveryRuleHasCases(t *testing.T) {
	t.Parallel()
	seen := make(map[string]bool)
	for _, rule := range credentialpattern.All() {
		if seen[rule.ID] {
			t.Errorf("duplicate rule id %q", rule.ID)
		}
		seen[rule.ID] = true
		if rule.Name == "" {
			t.Errorf("rule %q has no name", rule.ID)
		}
		cases, ok := ruleCases[rule.ID]
		if !ok || len(cases.match) == 0 || len(cases.noMatch) == 0 {
			t.Errorf("rule %q needs at least one matching and one non-matching case", rule.ID)
		}
	}
	for id := range ruleCases {
		if !seen[id] {
			t.Errorf("cases name rule %q, which does not exist", id)
		}
	}
}

// TestRuleNames pins each rule's name: the export scan puts it in the refusal
// an operator reads, so a rename is a visible change, never a side effect.
func TestRuleNames(t *testing.T) {
	t.Parallel()
	want := map[string]string{ //nolint:gosec // G101: rule ids and refusal names, not credentials
		"anthropic_token":         "an Anthropic key or token",
		"github_token":            "a GitHub token",
		"github_pat":              "a GitHub token",
		"aws_access_key_id":       "an AWS access key id",
		"slack_token":             "a Slack token",
		"pem_private_key":         "a PEM private key",
		"gcp_service_account_key": "a GCP service-account key id",
	}
	got := make(map[string]string)
	for _, rule := range credentialpattern.All() {
		got[rule.ID] = rule.Name
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rules, want %d", len(got), len(want))
	}
	for id, name := range want {
		if got[id] != name {
			t.Errorf("rule %s name = %q, want %q", id, got[id], name)
		}
	}
}

func TestAllReturnsACopy(t *testing.T) {
	t.Parallel()
	first := credentialpattern.All()
	first[0] = credentialpattern.Rule{}
	if credentialpattern.All()[0].ID == "" {
		t.Fatal("editing All's result changed the shared list")
	}
}
