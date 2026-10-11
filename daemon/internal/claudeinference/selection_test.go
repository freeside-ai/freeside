package claudeinference

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// recordingDriver is a driver over a stub CLI that writes its arguments to
// the returned path and answers as the model `reported`. The driver's
// configured model is "test-model".
func recordingDriver(t *testing.T, reported string) (*Driver, string) {
	t.Helper()
	answer := completion()
	answer["modelUsage"] = map[string]any{reported: map[string]any{"outputTokens": 12, "webSearchRequests": 0}}
	b, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	argsPath := filepath.Join(t.TempDir(), "args")
	return testDriver(t, `printf '%s' "$*" > '`+argsPath+`'
cat > /dev/null
printf '%s' '`+string(b)+`'
`), argsPath
}

// A launch that sends no effort passes the argument list the 2026-09-09
// audit covered, element for element. The list is written out here so a
// change to it is a change to this test.
func TestCommandArgsWithoutAnEffortAreTheAuditedList(t *testing.T) {
	want := []string{
		"-p", "--output-format", "json", "--model", "some-model", "--safe-mode", "--tools", "",
		"--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--no-session-persistence", "--max-turns", "1", "--system-prompt", "the prompt",
	}
	if got := commandArgs("some-model", "", "the prompt"); !slices.Equal(got, want) {
		t.Fatalf("commandArgs = %q, want %q", got, want)
	}
	withEffort := slices.Insert(slices.Clone(want), 5, "--effort", "medium")
	if got := commandArgs("some-model", "medium", "the prompt"); !slices.Equal(got, withEffort) {
		t.Fatalf("commandArgs = %q, want %q", got, withEffort)
	}
}

func TestCompletePassesTheRequestsModelAndEffort(t *testing.T) {
	t.Run("an explicit selection is passed as written", func(t *testing.T) {
		d, argsPath := recordingDriver(t, "claude-explicit-1")
		r := classifierRequest()
		r.Model, r.NativeEffort = "claude-explicit-1", "medium"
		got, err := d.Complete(t.Context(), r, "token")
		if err != nil || got.Observed.ModelID != "claude-explicit-1" {
			t.Fatalf("Complete = %+v, %v", got, err)
		}
		args, err := os.ReadFile(argsPath) //nolint:gosec // G304: test-owned path
		if err != nil || !strings.HasPrefix(string(args), "-p --output-format json --model claude-explicit-1 --effort medium --safe-mode ") {
			t.Fatalf("launch arguments = %q, %v", args, err)
		}
	})
	t.Run("an empty selection launches with the configured model and no effort", func(t *testing.T) {
		d, argsPath := recordingDriver(t, "test-model")
		got, err := d.Complete(t.Context(), classifierRequest(), "token")
		if err != nil || got.Observed.ModelID != "test-model" {
			t.Fatalf("Complete = %+v, %v", got, err)
		}
		args, err := os.ReadFile(argsPath) //nolint:gosec // G304: test-owned path
		if err != nil || !strings.HasPrefix(string(args), "-p --output-format json --model test-model --safe-mode ") ||
			strings.Contains(string(args), "--effort") {
			t.Fatalf("launch arguments = %q, %v", args, err)
		}
	})
	// The exact-match check follows the model the launch passed: a request
	// with its own model is refused when the configured model answers, and
	// the configured model's launch is refused when another answers.
	t.Run("a completion from another model than the launch passed is refused", func(t *testing.T) {
		d, _ := recordingDriver(t, "test-model")
		r := classifierRequest()
		r.Model = "claude-explicit-1"
		if _, err := d.Complete(t.Context(), r, "token"); err == nil {
			t.Fatal("accepted the configured model's answer to a request that named another model")
		}
		d, _ = recordingDriver(t, "claude-explicit-1")
		if _, err := d.Complete(t.Context(), classifierRequest(), "token"); err == nil {
			t.Fatal("accepted another model's answer to the configured model's launch")
		}
	})
}

// A stored offer only has to name a non-empty model, so the driver refuses a
// value the CLI could read as anything but one option value, and it does so
// before it starts a process.
func TestCompleteStartsNoProcessForAnUnsafeSelection(t *testing.T) {
	for _, tc := range []struct{ name, model, effort string }{
		{"model with a space", "claude explicit", ""},
		{"model with a newline", "claude\nexplicit", ""},
		{"model with a control character", "claude\x00explicit", ""},
		{"model with a format character", "claude\u202eexplicit", ""},
		{"model that is an option", "--dangerously-skip-permissions", ""},
		{"model that is not UTF-8", "claude\xff", ""},
		{"effort with a tab", "", "me\tdium"},
		{"effort that is an option", "", "-high"},
		{"effort with a trailing space", "claude-explicit-1", "high "},
		{"model longer than an observed identifier", strings.Repeat("m", 257), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, argsPath := recordingDriver(t, "claude-explicit-1")
			r := classifierRequest()
			r.Model, r.NativeEffort = tc.model, tc.effort
			_, quiescent, err := d.CompleteAndConfirm(t.Context(), r, "token")
			if err == nil || !quiescent {
				t.Fatalf("CompleteAndConfirm = quiescent %v, %v; want a refusal with no process", quiescent, err)
			}
			if _, statErr := os.Stat(argsPath); !os.IsNotExist(statErr) {
				t.Fatalf("the CLI started for an unsafe selection: %v", statErr)
			}
		})
	}
}
