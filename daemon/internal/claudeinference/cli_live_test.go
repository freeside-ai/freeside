package claudeinference

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// This opt-in protocol probe uses no real credential or provider. It executes
// the configured native CLI against a localhost synthetic Messages endpoint,
// using the production argument/environment builders without exposing an
// endpoint override in production configuration.
func TestPinnedClaudeCLIProtocol(t *testing.T) {
	bin := os.Getenv("FREESIDE_JUDGMENT_CLI")
	if bin == "" {
		t.Skip("set FREESIDE_JUDGMENT_CLI and FREESIDE_JUDGMENT_CLI_SHA256 for the local mock-provider probe")
	}
	const model = "claude-sonnet-4-6"
	driver, err := New(Config{Binary: bin, SHA256: os.Getenv("FREESIDE_JUDGMENT_CLI_SHA256"), Model: model})
	if err != nil {
		t.Fatal(err)
	}
	req := classifierRequest()
	req.MaxComputeUnits = 1024
	prompt, site, err := promptFor(req)
	if err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(req.Fields)
	var mu sync.Mutex
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		calls++
		b, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		var request struct {
			Model     string `json:"model"`
			Tools     []any  `json:"tools"`
			MaxTokens int64  `json:"max_tokens"`
			Thinking  struct {
				Type string `json:"type"`
			} `json:"thinking"`
			Messages []struct {
				Role    string `json:"role"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"messages"`
		}
		if err = json.Unmarshal(b, &request); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if request.Model != model || len(request.Tools) != 0 || request.MaxTokens != req.MaxComputeUnits || request.Thinking.Type != "disabled" {
			t.Error("CLI changed model/tool/compute contract")
		}
		found := false
		for _, m := range request.Messages {
			for _, c := range m.Content {
				if c.Text == string(input) {
					found = true
				}
			}
		}
		if !found || strings.Contains(string(b), "repository-canary") {
			t.Error("CLI input containment failed")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		event := func(kind string, data map[string]any) {
			data["type"] = kind
			encoded, _ := json.Marshal(data)
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, encoded); err != nil {
				t.Error(err)
			}
		}
		event("message_start", map[string]any{"message": map[string]any{"id": "msg_test", "type": "message", "role": "assistant", "model": model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 1, "output_tokens": 0}}})
		event("content_block_start", map[string]any{"index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		event("content_block_delta", map[string]any{"index": 0, "delta": map[string]any{"type": "text_delta", "text": completion()["result"]}})
		event("content_block_stop", map[string]any{"index": 0})
		event("message_delta", map[string]any{"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 12}})
		event("message_stop", map[string]any{})
	}))
	defer server.Close()
	root := t.TempDir()
	copyPath := root + "/claude"
	f, err := os.OpenFile(copyPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o500) //nolint:gosec // G302: exercise the production private executable-copy protocol.
	if err != nil {
		t.Fatal(err)
	}
	copyErr := driver.copyBinary(f)
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		t.Fatal("private executable copy failed")
	}
	if err := os.WriteFile(root+"/CLAUDE.md", []byte("repository-canary must not be read"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), copyPath, commandArgs(model, prompt)...) //nolint:gosec // G204: private content-verified native CLI in an opt-in synthetic probe.
	cmd.Dir = root
	cmd.Env = append(commandEnv(root, "synthetic-test-only", req, site), "ANTHROPIC_BASE_URL="+server.URL)
	cmd.Stdin = bytes.NewReader(input)
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("pinned CLI protocol failed: %v", err)
	}
	if _, err := decodeCompletion(output, model, req, site); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("made %d model calls, want one", calls)
	}
}
