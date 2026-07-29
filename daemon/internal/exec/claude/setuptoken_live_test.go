package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
// for. The token is never logged.
func TestSetupTokenBuysInferenceOnly(t *testing.T) {
	token := liveToken(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	t.Run("inference completes", func(t *testing.T) {
		home := t.TempDir()
		configDir := filepath.Join(home, ".claude")
		if err := os.Mkdir(configDir, 0o700); err != nil {
			t.Fatalf("create isolated Claude config directory: %v", err)
		}
		baseEnv := isolatedClaudeEnv(home, configDir)

		status, err := readAuthStatus(ctx, baseEnv)
		if err != nil {
			t.Fatalf("read isolated negative-control auth status: %v", err)
		}
		if status.LoggedIn || status.AuthMethod != "none" {
			t.Fatalf("isolated negative control found ambient authentication method %q",
				status.AuthMethod)
		}

		isError, err := runInference(ctx, append(baseEnv, tokenEnv+"="+token))
		if err != nil {
			t.Fatalf("inference through the pinned CLI failed: %v", err)
		}
		if isError {
			t.Fatal("CLI reported an error result for setup-token inference")
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
			if !scopeAuthorizationRefused(resp.StatusCode) {
				t.Fatalf(
					"%s %s returned %d, want an authorization refusal (401 or 403); "+
						"availability, routing, and throttling failures are not scope evidence",
					tc.method, tc.path, resp.StatusCode)
			}
		})
	}
}

func scopeAuthorizationRefused(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

func TestScopeProbeRequiresAuthorizationRefusal(t *testing.T) {
	t.Parallel()
	for _, status := range []int{
		http.StatusOK,
		http.StatusNotFound,
		http.StatusMethodNotAllowed,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
	} {
		if scopeAuthorizationRefused(status) {
			t.Errorf("status %d was accepted as an authorization refusal", status)
		}
	}
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		if !scopeAuthorizationRefused(status) {
			t.Errorf("status %d was not accepted as an authorization refusal", status)
		}
	}
}

type inferenceResult struct {
	IsError *bool `json:"is_error"`
}

type authStatus struct {
	LoggedIn   bool   `json:"loggedIn"`
	AuthMethod string `json:"authMethod"`
}

func readAuthStatus(ctx context.Context, env []string) (authStatus, error) {
	cmd := exec.CommandContext(ctx, "claude", "auth", "status", "--json")
	cmd.Env = env
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &bytes.Buffer{}
	runErr := cmd.Run()
	var status authStatus
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
		if runErr != nil {
			return authStatus{}, fmt.Errorf("%w; decode CLI auth status: %v "+
				"(CLI stderr suppressed)", runErr, err)
		}
		return authStatus{}, fmt.Errorf("decode CLI auth status: %w", err)
	}
	return status, nil
}

func runInference(ctx context.Context, env []string) (bool, error) {
	cmd := exec.CommandContext(ctx, "claude", "-p",
		"Reply with exactly the word: ok", "--output-format", "json")
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("%w (CLI stderr suppressed)", err)
	}
	var result inferenceResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return false, fmt.Errorf("decode CLI JSON result: %w", err)
	}
	if result.IsError == nil {
		return false, fmt.Errorf("CLI JSON result omitted is_error")
	}
	return *result.IsError, nil
}

func isolatedClaudeEnv(home, configDir string) []string {
	return isolatedClaudeEnvFrom(os.Environ(), home, configDir)
}

func isolatedClaudeEnvFrom(source []string, home, configDir string) []string {
	env := make([]string, 0, len(source)+2)
	for _, value := range source {
		name, _, _ := strings.Cut(value, "=")
		switch name {
		case "HOME", "CLAUDE_CONFIG_DIR",
			"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
			tokenEnv, "CLAUDE_CODE_OAUTH_REFRESH_TOKEN", "CLAUDE_CODE_OAUTH_SCOPES",
			"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX",
			"CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_USE_MANTLE":
			continue
		}
		env = append(env, value)
	}
	return append(env, "HOME="+home, "CLAUDE_CONFIG_DIR="+configDir)
}

func TestIsolatedClaudeEnvDropsAmbientCredentials(t *testing.T) {
	source := []string{
		"PATH=/bin",
		"HOME=/real-home",
		"CLAUDE_CONFIG_DIR=/real-config",
		"ANTHROPIC_API_KEY=api-key",
		"ANTHROPIC_AUTH_TOKEN=auth-token",
		"ANTHROPIC_BASE_URL=https://proxy.invalid",
		tokenEnv + "=setup-token",
		"CLAUDE_CODE_OAUTH_REFRESH_TOKEN=refresh-token",
		"CLAUDE_CODE_OAUTH_SCOPES=user:inference",
		"CLAUDE_CODE_USE_BEDROCK=1",
		"KEEP=value",
	}
	got := isolatedClaudeEnvFrom(source, "/isolated-home", "/isolated-config")
	want := []string{
		"PATH=/bin",
		"KEEP=value",
		"HOME=/isolated-home",
		"CLAUDE_CONFIG_DIR=/isolated-config",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("isolated environment = %q, want %q", got, want)
	}
}
