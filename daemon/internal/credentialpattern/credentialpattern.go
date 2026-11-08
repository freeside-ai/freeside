package credentialpattern

import "regexp"

// Rule is one credential class.
type Rule struct {
	// ID names the class in import findings, for example "github_token".
	// It is persisted in findings, so it never changes once shipped.
	ID string
	// Name describes the class to an operator without echoing a match, for
	// example "a GitHub token". The export scan's refusal message carries it.
	Name string
	// Pattern matches the token's own structure.
	Pattern *regexp.Regexp
}

// rules are compiled once. The order is the importer's historical order;
// the export scan reports the first rule that matches, so the order only
// chooses which Name a refusal carries when one chunk holds two classes.
var rules = []Rule{
	{"anthropic_token", "an Anthropic key or token", regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{16,}`)},
	// Personal, OAuth, user-to-server, server-to-server, and refresh tokens.
	// No word boundaries: a token glued to a preceding word character
	// (GH_TOKEN_ghp_...) or followed by an underscore is still a token.
	{"github_token", "a GitHub token", regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{16,}`)},
	// Fine-grained personal access tokens.
	{"github_pat", "a GitHub token", regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{22,}\b`)},
	{"aws_access_key_id", "an AWS access key id", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	// App, bot, user, refresh, and service tokens.
	{"slack_token", "a Slack token", regexp.MustCompile(`\bxox[abprs]-[0-9A-Za-z-]{10,}\b`)},
	// PEM private keys of any flavour.
	{"pem_private_key", "a PEM private key", regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY-----`)},
	{"gcp_service_account_key", "a GCP service-account key id", regexp.MustCompile(`"private_key_id"\s*:\s*"[0-9a-f]{40}"`)},
}

// All returns every rule. The slice is a copy, so a caller can't add, drop,
// or reorder the rules another scanner reads; the compiled patterns are
// shared and safe for concurrent use.
func All() []Rule {
	return append([]Rule(nil), rules...)
}
