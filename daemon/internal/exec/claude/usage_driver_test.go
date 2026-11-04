package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeCLIStdout is what testdata/usage_driver_fake_cli.cjs prints over a
// whole turn, which the driver must pass through unchanged.
const fakeCLIStdout = `{"type":"control_response","response":{"subtype":"success","request_id":"usage-init","response":{}}}
{"type":"system","subtype":"init","note":"café ☕"}
{"type":"assistant","message":{"content":[{"type":"text","text":"done"}]}}
{"type":"result","subtype":"success","is_error":false}
{"type":"control_response","response":{"subtype":"success","request_id":"usage-read","response":{"rate_limits":{}}}}
`

type usageDriverRun struct {
	stdout   string
	stdin    []map[string]any // what the fake CLI read, one decoded line each
	exitCode int
	elapsed  time.Duration
}

// runUsageDriver runs the embedded driver under node with the fake CLI as
// its child. Its timeouts are shortened so a silent CLI is quick to test;
// nothing else in the script changes.
func runUsageDriver(t *testing.T, mode, prompt string) usageDriverRun {
	t.Helper()
	node, err := osexec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH")
	}
	shortened := usageDriverScript
	for _, timeout := range []struct{ old, new string }{
		{"const ANSWER_TIMEOUT_MS = 15000;", "const ANSWER_TIMEOUT_MS = 300;"},
		{"const INIT_TIMEOUT_MS = 120000;", "const INIT_TIMEOUT_MS = 300;"},
		{"const EXIT_GRACE_MS = 2000;", "const EXIT_GRACE_MS = 300;"},
	} {
		if strings.Count(shortened, timeout.old) != 1 {
			t.Fatalf("driver constant %q moved", timeout.old)
		}
		shortened = strings.Replace(shortened, timeout.old, timeout.new, 1)
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "usage-driver.cjs")
	if err := os.WriteFile(script, []byte(shortened), 0o600); err != nil {
		t.Fatal(err)
	}
	fake, err := filepath.Abs("testdata/usage_driver_fake_cli.cjs")
	if err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(dir, "stdin.jsonl")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	command := osexec.CommandContext(ctx, node, script, node, fake, mode, record) //nolint:gosec // fixed test fixtures
	command.Stdin = strings.NewReader(prompt)
	var stdout bytes.Buffer
	command.Stdout = &stdout
	started := time.Now()
	err = command.Run()
	run := usageDriverRun{stdout: stdout.String(), elapsed: time.Since(started)}
	if ctx.Err() != nil {
		t.Fatal("driver did not exit: the CLI's stdin was never closed")
	}
	var exitErr *osexec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		run.exitCode = exitErr.ExitCode()
	default:
		t.Fatalf("run driver: %v", err)
	}
	recorded, err := os.ReadFile(record) //nolint:gosec // test temp dir
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(strings.TrimSuffix(string(recorded), "\n"), "\n") {
		var message map[string]any
		if err := json.Unmarshal([]byte(line), &message); err != nil {
			t.Fatalf("CLI stdin line %q: %v", line, err)
		}
		run.stdin = append(run.stdin, message)
	}
	return run
}

// stdinKinds names each line the CLI read: a control request by its
// subtype and request id, anything else by its type.
func stdinKinds(stdin []map[string]any) []string {
	kinds := make([]string, 0, len(stdin))
	for _, message := range stdin {
		kind, _ := message["type"].(string)
		if request, ok := message["request"].(map[string]any); ok {
			subtype, _ := request["subtype"].(string)
			id, _ := message["request_id"].(string)
			kind = subtype + ":" + id
		}
		kinds = append(kinds, kind)
	}
	return kinds
}

// TestUsageDriverRunsOneTurnAndOneUsageRead pins the driver's whole
// protocol: initialize, the prompt exactly once as one user message, and
// exactly one get_usage after the result, with the CLI's stdout passed
// through byte for byte and its exit status kept.
func TestUsageDriverRunsOneTurnAndOneUsageRead(t *testing.T) {
	t.Parallel()
	prompt := "Fix the bug.\n\nIt's in \"main.go\" — line 2.\n"
	for _, tc := range []struct {
		mode     string
		exitCode int
	}{{"answer", 0}, {"fail", 2}} {
		t.Run(tc.mode, func(t *testing.T) {
			t.Parallel()
			run := runUsageDriver(t, tc.mode, prompt)
			if run.stdout != fakeCLIStdout {
				t.Errorf("stdout differs from the CLI's:\n got %q\nwant %q", run.stdout, fakeCLIStdout)
			}
			if run.exitCode != tc.exitCode {
				t.Errorf("exit code = %d, want the CLI's %d", run.exitCode, tc.exitCode)
			}
			if got, want := strings.Join(stdinKinds(run.stdin), ","), "initialize:usage-init,user,get_usage:usage-read"; got != want {
				t.Fatalf("CLI stdin = %s, want %s", got, want)
			}
			user := run.stdin[1]["message"].(map[string]any) //nolint:forcetypeassert // shape checked above
			if user["role"] != "user" || user["content"] != prompt {
				t.Errorf("user message = %v, want the prompt verbatim", user)
			}
			if run.elapsed > 15*time.Second {
				t.Errorf("driver took %s: it waited past the usage answer", run.elapsed)
			}
		})
	}
}

// TestUsageDriverClosesStdinWithoutAnAnswer proves usage is optional: a CLI
// that never answers get_usage still gets EOF after the answer timeout and
// exits with its own status.
func TestUsageDriverClosesStdinWithoutAnAnswer(t *testing.T) {
	t.Parallel()
	run := runUsageDriver(t, "silent", "prompt")
	if run.exitCode != 0 {
		t.Errorf("exit code = %d, want 0", run.exitCode)
	}
	if got, want := strings.Join(stdinKinds(run.stdin), ","), "initialize:usage-init,user,get_usage:usage-read"; got != want {
		t.Errorf("CLI stdin = %s, want %s", got, want)
	}
	if want := strings.TrimSuffix(fakeCLIStdout, `{"type":"control_response","response":{"subtype":"success","request_id":"usage-read","response":{"rate_limits":{}}}}`+"\n"); run.stdout != want {
		t.Errorf("stdout = %q, want %q", run.stdout, want)
	}
}

// TestUsageDriverKeepsAnEarlyExit proves a CLI that ends before any result
// gets no usage request and its status is the driver's: a plain exit code
// as is, a signal as the shell reports one, 128 plus its number.
func TestUsageDriverKeepsAnEarlyExit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		mode     string
		exitCode int
	}{{"early", 3}, {"signal", 128 + 15}} {
		t.Run(tc.mode, func(t *testing.T) {
			t.Parallel()
			run := runUsageDriver(t, tc.mode, "prompt")
			if run.exitCode != tc.exitCode {
				t.Errorf("exit code = %d, want %d", run.exitCode, tc.exitCode)
			}
			for _, kind := range stdinKinds(run.stdin) {
				if strings.HasPrefix(kind, "get_usage") {
					t.Errorf("CLI read %s after exiting before a result", kind)
				}
			}
		})
	}
}

// TestUsageDriverClosesStdinWithoutAnInitAnswer proves a CLI that never
// answers initialize gets EOF and no prompt, as a file_v5 CLI that read its
// stdin would, instead of waiting for input until the stage deadline.
func TestUsageDriverClosesStdinWithoutAnInitAnswer(t *testing.T) {
	t.Parallel()
	run := runUsageDriver(t, "mute", "prompt")
	if run.exitCode != 0 {
		t.Errorf("exit code = %d, want the CLI's 0", run.exitCode)
	}
	if got, want := strings.Join(stdinKinds(run.stdin), ","), "initialize:usage-init"; got != want {
		t.Errorf("CLI stdin = %s, want %s", got, want)
	}
	if run.elapsed > 15*time.Second {
		t.Errorf("driver took %s", run.elapsed)
	}
}

// TestUsageDriverDoesNotWaitForAPipeHolder proves the CLI's exit ends the
// driver even while a descendant still holds the CLI's stdout.
func TestUsageDriverDoesNotWaitForAPipeHolder(t *testing.T) {
	t.Parallel()
	run := runUsageDriver(t, "linger", "prompt")
	if run.exitCode != 4 {
		t.Errorf("exit code = %d, want the CLI's 4", run.exitCode)
	}
	if run.elapsed > 15*time.Second {
		t.Errorf("driver took %s: it waited for the descendant holding stdout", run.elapsed)
	}
}
