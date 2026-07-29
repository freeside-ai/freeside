package claude

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The Claude setup token's inference-only scope, contract-tested against the
// pinned CLI (plan §5.3, #237 acceptance).
//
// Why it is a contract test and not a comment: subscription_contained mounts
// this credential into an agent VM that runs untrusted model output. The
// containment argument assumes the credential can buy inference and nothing
// else, so "nothing else" has to be a checked property of the token the
// operator actually minted, re-checked whenever the vendor's scopes move.
//
// Opt-in and CI-blind by design: it needs the operator's real token and real
// network. CI records it as Not run.

const (
	tokenLiveEnv     = "FREESIDE_CLAUDE_TOKEN_LIVE_TEST"
	tokenEnv         = "CLAUDE_CODE_OAUTH_TOKEN" //nolint:gosec // G101: the variable's name, not a credential
	pinnedCLIVersion = "2.1.220"
	anthropicAPIBase = "https://api.anthropic.com"
)

func liveToken(t *testing.T) string {
	t.Helper()
	if os.Getenv(tokenLiveEnv) != "1" {
		t.Skip("live Claude token contract is opt-in: set " + tokenLiveEnv + "=1 and " +
			tokenEnv + " to the operator's setup token (for example from the login " +
			"keychain: security find-generic-password -s freeside-claude-setup-token -w)")
	}
	token := os.Getenv(tokenEnv)
	if token == "" {
		t.Fatal(tokenEnv + " is required when " + tokenLiveEnv + "=1")
	}
	return token
}

// TestPinnedCLIVersionMatchesTheImagePin fails when the host CLI has drifted
// from the version the agent image pins, which would make every other
// assertion here a statement about a different program.
func TestPinnedCLIVersionMatchesTheImagePin(t *testing.T) {
	liveToken(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "claude", "--version").Output()
	if err != nil {
		t.Fatalf("claude --version: %v", err)
	}
	if !strings.Contains(string(out), pinnedCLIVersion) {
		t.Fatalf("host CLI reports %q, image pins %s", strings.TrimSpace(string(out)), pinnedCLIVersion)
	}
}

// TestSetupTokenBuysInferenceOnly is the scope contract: the token completes
// a real inference through the pinned CLI, and is refused by the
// non-inference API surfaces an escaped credential would be worth stealing
// for. Only status codes are asserted, and the token is never logged.
func TestSetupTokenBuysInferenceOnly(t *testing.T) {
	token := liveToken(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	t.Run("inference completes", func(t *testing.T) {
		cmd := exec.CommandContext(ctx, "claude", "-p",
			"Reply with exactly the word: ok", "--output-format", "json")
		cmd.Env = append(os.Environ(), tokenEnv+"="+token)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("inference through the pinned CLI failed: %v (stderr: %s)",
				err, strings.TrimSpace(stderr.String()))
		}
		if !strings.Contains(stdout.String(), `"is_error":false`) &&
			strings.Contains(stdout.String(), `"is_error":true`) {
			t.Fatalf("CLI reported an error result: %s", strings.TrimSpace(stdout.String()))
		}
	})

	// Each case is a surface an API key would reach and a setup token must
	// not. A 2xx here is the finding: the credential mounted into the agent
	// VM would buy more than inference.
	scopes := []struct {
		name   string
		method string
		path   string
		header string
	}{
		{"messages as api key", http.MethodPost, "/v1/messages", "x-api-key"},
		{"api key administration", http.MethodGet, "/v1/organizations/api_keys", "authorization"},
		{"organization members", http.MethodGet, "/v1/organizations/users", "authorization"},
		{"usage report", http.MethodGet, "/v1/organizations/usage_report/messages", "authorization"},
	}
	client := &http.Client{Timeout: 30 * time.Second}
	for _, tc := range scopes {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.NewReader(`{"model":"claude-sonnet-4-5","max_tokens":1,` +
				`"messages":[{"role":"user","content":"hi"}]}`)
			req, err := http.NewRequestWithContext(ctx, tc.method, anthropicAPIBase+tc.path, body)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			if tc.header == "authorization" {
				req.Header.Set("authorization", "Bearer "+token)
			} else {
				req.Header.Set(tc.header, token)
			}
			req.Header.Set("anthropic-version", "2023-06-01")
			req.Header.Set("content-type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("call %s: %v", tc.path, err)
			}
			defer func() { _ = resp.Body.Close() }()
			// Status only: a response body from an authorization surface can
			// carry organization detail this test has no reason to record.
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				t.Fatalf("%s %s returned %d: the setup token reaches a non-inference surface",
					tc.method, tc.path, resp.StatusCode)
			}
		})
	}
}
