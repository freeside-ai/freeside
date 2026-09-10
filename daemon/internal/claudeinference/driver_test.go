package claudeinference

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/inference"
)

func classifierRequest() inference.Request {
	s := inference.ClassifierSite(inference.Budget{})
	f := map[string]string{}
	for _, field := range s.Fields {
		f[field.Name] = "synthetic"
	}
	return inference.Request{SiteID: s.ID, Fields: f, MaxOutput: s.MaxOutputBytes, MaxComputeUnits: s.MaxComputeUnits}
}

func completion() map[string]any {
	return map[string]any{
		"type": "result", "subtype": "success", "is_error": false, "num_turns": 1, "stop_reason": "end_turn",
		"result":             `{"materiality":"high","confidence":"high","note":"Concrete reachable failure"}`,
		"permission_denials": []any{}, "usage": map[string]any{"output_tokens": 12, "server_tool_use": map[string]any{"web_search_requests": 0}},
		"modelUsage": map[string]any{"test-model": map[string]any{"outputTokens": 12, "webSearchRequests": 0}},
	}
}

func TestCompletionRefusesUnavailableOrContradictoryEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"error", func(m map[string]any) { m["is_error"] = true }},
		{"missing status", func(m map[string]any) { delete(m, "is_error") }},
		{"truncated", func(m map[string]any) { m["stop_reason"] = "max_tokens" }},
		{"extra turn", func(m map[string]any) { m["num_turns"] = 2 }},
		{"no usage", func(m map[string]any) { delete(m, "usage") }},
		{"missing server tool usage", func(m map[string]any) { delete(m["usage"].(map[string]any), "server_tool_use") }},
		{"missing model tool usage", func(m map[string]any) {
			delete(m["modelUsage"].(map[string]any)["test-model"].(map[string]any), "webSearchRequests")
		}},
		{"negative usage", func(m map[string]any) { m["usage"].(map[string]any)["output_tokens"] = -1 }},
		{"excess usage", func(m map[string]any) { m["usage"].(map[string]any)["output_tokens"] = 10001 }},
		{"tool use", func(m map[string]any) {
			m["usage"].(map[string]any)["server_tool_use"] = map[string]int{"web_search_requests": 1}
		}},
		{"substituted model", func(m map[string]any) { m["modelUsage"] = map[string]any{"other": map[string]int{"outputTokens": 12}} }},
		{"invalid proposal", func(m map[string]any) { m["result"] = `{"materiality":"low","confidence":"certain","note":"x"}` }},
		{"oversize proposal", func(m map[string]any) { m["result"] = strings.Repeat("x", 9000) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := completion()
			tc.change(m)
			b, _ := json.Marshal(m)
			if _, err := decodeCompletion(b, "test-model", classifierRequest(), inference.ClassifierSite(inference.Budget{})); err == nil {
				t.Fatal("accepted invalid completion")
			}
		})
	}
	b, _ := json.Marshal(completion())
	got, err := decodeCompletion(b, "test-model", classifierRequest(), inference.ClassifierSite(inference.Budget{}))
	if err != nil || got.ComputeUnits != 12 {
		t.Fatalf("valid completion: %v, %v", got, err)
	}
	if _, err := decodeCompletion(append(b, []byte(` {}`)...), "test-model", classifierRequest(), inference.ClassifierSite(inference.Budget{})); err == nil {
		t.Fatal("accepted trailing object")
	}
}

func testDriver(t *testing.T, script string) *Driver {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cli")
	body := []byte("#!/bin/sh\n" + script)
	if err := os.WriteFile(p, body, 0o700); err != nil { //nolint:gosec // G306: executable synthetic CLI fixture in a private test directory.
		t.Fatal(err)
	}
	h := sha256.Sum256(body)
	d, err := New(Config{Binary: p, SHA256: hex.EncodeToString(h[:]), Model: "test-model"})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestCompleteIsolatesCredentialAndInput(t *testing.T) {
	b, _ := json.Marshal(completion())
	script := `test "$CLAUDE_CODE_OAUTH_TOKEN" = 'synthetic-credential' || exit 2
test -z "$ANTHROPIC_API_KEY$ANTHROPIC_BASE_URL$CODEX_HOME" || exit 3
test "$HOME" -ef "$PWD" && test "$CLAUDE_CONFIG_DIR" -ef "$PWD" || exit 4
test "$CLAUDE_CODE_MAX_OUTPUT_TOKENS" = 10000 || exit 5
test "$MAX_THINKING_TOKENS" = 0 || exit 6
case "$*" in *synthetic-credential*|*synthetic-input*) exit 7;; esac
input=$(cat)
case "$input" in *synthetic-input*) ;; *) exit 8;; esac
printf '%s' '` + string(b) + `'
`
	d := testDriver(t, script)
	t.Setenv("ANTHROPIC_API_KEY", "must-not-inherit")
	t.Setenv("ANTHROPIC_BASE_URL", "must-not-inherit")
	t.Setenv("CODEX_HOME", "must-not-inherit")
	r := classifierRequest()
	r.Fields["message"] = "synthetic-input"
	if _, err := d.Complete(t.Context(), r, inference.Secret("synthetic-credential")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.config.Binary, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil { //nolint:gosec // G306: changed executable fixture must fail the content pin.
		t.Fatal(err)
	}
	if _, err := d.Complete(t.Context(), r, inference.Secret("synthetic-credential")); err == nil {
		t.Fatal("accepted changed CLI pin")
	}
}

func TestCompleteRejectsUnsupportedSitesAndCancellation(t *testing.T) {
	d := testDriver(t, "exit 99\n")
	r := classifierRequest()
	r.SiteID = inference.DiagnosticSiteID
	if _, err := d.Complete(t.Context(), r, "token"); err == nil {
		t.Fatal("accepted unsupported site")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := d.Complete(ctx, classifierRequest(), "token"); err == nil {
		t.Fatal("accepted canceled call")
	}
}

func TestBoundedOutputCancelsOverflow(t *testing.T) {
	canceled := false
	b := &boundedOutput{limit: 5, cancel: func() { canceled = true }}
	if _, err := b.Write([]byte("12345")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write([]byte("6")); err == nil || !canceled || b.Len() != 5 {
		t.Fatal("output exceeded bound")
	}
}

func TestCompleteSerializesDifferentSites(t *testing.T) {
	root := t.TempDir()
	ready := filepath.Join(root, "ready")
	release := filepath.Join(root, "release")
	for _, p := range []string{ready, release} {
		if err := syscall.Mkfifo(p, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	b, _ := json.Marshal(completion())
	d := testDriver(t, "printf ready > "+quote(ready)+"\nread -r done < "+quote(release)+"\nprintf '%s' "+quote(string(b))+"\n")
	finished := make(chan error, 1)
	go func() { _, err := d.Complete(t.Context(), classifierRequest(), "token"); finished <- err }()
	if _, err := os.ReadFile(ready); err != nil { //nolint:gosec // G304: fixed FIFO inside this test's private directory.
		t.Fatal(err)
	}
	// The first CLI is blocked on the release FIFO, so a different site's call
	// must refuse without launching another process or waiting in a queue.
	site := inference.AdjudicatorSite(inference.Budget{})
	fields := map[string]string{}
	for _, f := range site.Fields {
		fields[f.Name] = "synthetic"
	}
	req := inference.Request{SiteID: site.ID, Fields: fields, MaxOutput: site.MaxOutputBytes, MaxComputeUnits: site.MaxComputeUnits}
	if _, err := d.Complete(t.Context(), req, "token"); err == nil {
		t.Error("concurrent site was accepted")
	}
	if err := os.WriteFile(release, []byte("done\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}
