package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

//nolint:gosec // G101 false positive: a fixture token, not a credential.
const authAddTestToken = "sk-ant-oat01-command-fixture-0123456789abcdef"

// unavailableRuntime lists no volumes and then fails the volume create, the
// first mutating call Claude enrollment makes; any other method would panic
// on the nil embedded interface, proving the command went no further.
type unavailableRuntime struct{ ward.Runtime }

func (unavailableRuntime) ListVolumes(context.Context) ([]ward.VolumeSummary, error) {
	return nil, nil
}

func (unavailableRuntime) CreateVolume(context.Context, string, int64, []ward.Label) error {
	return errors.New("runtime unavailable")
}

func authAddTestDeps() authAddDeps {
	return authAddDeps{
		codexRefresher: &commandCodexAuthRefresher{},
		runtime:        func(string) ward.Runtime { return unavailableRuntime{} },
	}
}

func claudeAuthAddArgs(dbPath string) []string {
	return []string{
		"-db", dbPath, "-client", "claude_code", "-auth-identity", "claude-main",
		"-route", "anthropic-subscription", "-cost-owner", "operator",
		"-account", "acct-fixture-0002", "-auth-volume", "freeside-claude-main-auth",
		"-exporter-image", "example.test/exporter@sha256:" + strings.Repeat("0", 64),
	}
}

func TestAuthAddCodexEnrollsAndListsTheEnrollment(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "freeside.db")
	inputRoot, storeRoot := filepath.Join(root, "input"), filepath.Join(root, "store")
	for _, dir := range []string{inputRoot, storeRoot} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	inputPath, storePath := filepath.Join(inputRoot, "auth.json"), filepath.Join(storeRoot, "auth.json")
	var input map[string]any
	if err := json.Unmarshal(commandCodexAuth("operator-refresh"), &input); err != nil {
		t.Fatal(err)
	}
	input["tokens"].(map[string]any)["account_id"] = "acct-fixture-0001"
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inputPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err = runAuthAddCommand(context.Background(), []string{
		"-db", dbPath, "-client", "codex_cli", "-auth-identity", "codex-primary",
		"-route", "openai-subscription", "-cost-owner", "operator", "-project", "project-1",
		"-input-root", inputRoot, "-input-file", inputPath,
		"-auth-store-root", storeRoot, "-auth-store", storePath,
	}, nil, &stdout, &stderr, authAddTestDeps())
	if err != nil {
		t.Fatalf("auth add: %v; stderr = %s", err, stderr.String())
	}
	var result ward.CodexAuthEnrollmentResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode %q: %v", stdout.String(), err)
	}
	if result.EnrollmentID != "codex-primary/codex_cli" || result.AttentionItemID == "" {
		t.Fatalf("result = %+v", result)
	}

	listed := runAuthListForTest(t, dbPath)
	if len(listed.Identities) != 1 {
		t.Fatalf("identities = %+v", listed.Identities)
	}
	identity := listed.Identities[0]
	if identity.ID != "codex-primary" || identity.Label != "****0001" || identity.CostOwner != "operator" ||
		!identity.Interim || len(identity.Enrollments) != 1 {
		t.Fatalf("listed identity = %+v", identity)
	}
	enrollment := identity.Enrollments[0]
	if enrollment.ID != "codex-primary/codex_cli" || enrollment.Route != "openai-subscription" ||
		enrollment.Generation == nil || *enrollment.Generation != 1 ||
		enrollment.TokenExpiry == nil || !enrollment.TokenExpiry.Equal(result.AccessTokenExpiresAt) {
		t.Fatalf("listed enrollment = %+v", enrollment)
	}
	raw := runAuthListRawForTest(t, dbPath)
	if strings.Contains(raw, "acct-fixture-0001") || strings.Contains(raw, "rotated-refresh") {
		t.Fatalf("auth list leaks the account binding or a token: %s", raw)
	}
}

// TestAuthAddClaudeFailureLeavesARetryableEnrollment drives the command up
// to the runtime: the identity and enrollment are recorded, no generation
// is, and neither output stream nor the error carries the token.
func TestAuthAddClaudeFailureLeavesARetryableEnrollment(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	var stdout, stderr bytes.Buffer
	err := runAuthAddCommand(context.Background(), claudeAuthAddArgs(dbPath),
		strings.NewReader(authAddTestToken+"\n"), &stdout, &stderr, authAddTestDeps())
	if err == nil || !strings.Contains(err.Error(), "runtime unavailable") {
		t.Fatalf("auth add = %v, want the runtime failure", err)
	}
	for _, text := range []string{err.Error(), stdout.String(), stderr.String()} {
		if strings.Contains(text, authAddTestToken) {
			t.Fatalf("token leaked into %q", text)
		}
	}
	listed := runAuthListForTest(t, dbPath)
	if len(listed.Identities) != 1 || len(listed.Identities[0].Enrollments) != 1 {
		t.Fatalf("listed = %+v", listed)
	}
	identity := listed.Identities[0]
	enrollment := identity.Enrollments[0]
	if identity.Provider != "claude" || identity.Label != "****0002" ||
		enrollment.AuthMethod != "setup_token" || enrollment.Generation != nil ||
		enrollment.TokenExpiry != nil || enrollment.RecordedAt != nil {
		t.Fatalf("listed identity = %+v, enrollment = %+v", identity, enrollment)
	}
}

func TestAuthAddClaudeRefusesABadTokenBeforeTouchingTheStore(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	apiKey := "sk-ant-api03-" + strings.Repeat("k", 40)
	err := runAuthAddCommand(context.Background(), claudeAuthAddArgs(dbPath),
		strings.NewReader(apiKey), io.Discard, io.Discard, authAddTestDeps())
	if err == nil || strings.Contains(err.Error(), apiKey) {
		t.Fatalf("auth add = %v, want a refusal that does not echo the input", err)
	}
	if _, statErr := os.Stat(dbPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("refused token still created the store: %v", statErr)
	}
}

func TestAuthAddRequiresEachClientsFlags(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	without := func(args []string, flag string) []string {
		i := slices.Index(args, flag)
		return append(append([]string{}, args[:i]...), args[i+2:]...)
	}
	claude := claudeAuthAddArgs(dbPath)
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no client":      {without(claude, "-client"), "-client is required"},
		"unknown client": {append(without(claude, "-client"), "-client", "gemini"), `"gemini" is not`},
		"no route":       {without(claude, "-route"), "-route is required"},
		"claude account": {without(claude, "-account"), "-account is required"},
		"claude volume":  {without(claude, "-auth-volume"), "-auth-volume is required"},
		"codex input": {
			[]string{"-db", dbPath, "-client", "codex_cli", "-auth-identity", "a", "-route", "r"},
			"-project is required",
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := runAuthAddCommand(context.Background(), tc.args, strings.NewReader(authAddTestToken),
				io.Discard, io.Discard, authAddTestDeps())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("auth add = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestReadSetupTokenFromAPipe(t *testing.T) {
	for name, input := range map[string]string{
		"bare": authAddTestToken, "newline": authAddTestToken + "\n", "crlf": authAddTestToken + "\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			token, err := readSetupToken(strings.NewReader(input), io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if rendered := token.String(); strings.Contains(rendered, authAddTestToken) {
				t.Fatalf("token renders its value: %q", rendered)
			}
		})
	}
	for name, input := range map[string]string{
		"two lines": authAddTestToken + "\n" + authAddTestToken + "\n",
		"oversize":  "sk-ant-oat01-" + strings.Repeat("z", ward.MaxSetupTokenBytes+8),
		"empty":     "",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := readSetupToken(strings.NewReader(input), io.Discard); err == nil {
				t.Fatal("malformed input accepted")
			}
		})
	}
	if _, err := readSetupToken(nil, io.Discard); err == nil {
		t.Fatal("missing stdin accepted")
	}
}

func TestAuthCommandDispatch(t *testing.T) {
	if err := runAuthCommand(context.Background(), nil, nil, io.Discard, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "usage") {
		t.Fatalf("no subcommand = %v", err)
	}
	if err := runAuthCommand(context.Background(), []string{"remove"}, nil, io.Discard, io.Discard); err == nil ||
		!strings.Contains(err.Error(), `unknown auth command "remove"`) {
		t.Fatalf("unknown subcommand = %v", err)
	}
}

func TestMaskAccountBinding(t *testing.T) {
	for binding, want := range map[string]string{
		"": "", "short": "****", "12345678": "****", "123456789": "****6789",
		"acct-fixture-0001": "****0001",
	} {
		if got := maskAccountBinding(binding); got != want {
			t.Fatalf("maskAccountBinding(%q) = %q, want %q", binding, got, want)
		}
	}
}

func TestAuthListOnAnEmptyStore(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	st, _, err := openStoreWithTopicKey(context.Background(), dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if raw := runAuthListRawForTest(t, dbPath); strings.TrimSpace(raw) != `{"identities":[]}` {
		t.Fatalf("empty list = %s", raw)
	}
	if err := runAuthListCommand(context.Background(), []string{"-db", filepath.Join(t.TempDir(), "missing.db")},
		io.Discard, io.Discard); err == nil {
		t.Fatal("auth list created or read a missing store")
	}
}

func runAuthListRawForTest(t *testing.T, dbPath string) string {
	t.Helper()
	var stdout bytes.Buffer
	if err := runAuthListCommand(context.Background(), []string{"-db", dbPath}, &stdout, io.Discard); err != nil {
		t.Fatalf("auth list: %v", err)
	}
	return stdout.String()
}

func runAuthListForTest(t *testing.T, dbPath string) authListOutput {
	t.Helper()
	var listed authListOutput
	if err := json.Unmarshal([]byte(runAuthListRawForTest(t, dbPath)), &listed); err != nil {
		t.Fatal(err)
	}
	return listed
}
