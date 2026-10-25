package ward

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
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

const (
	lineupProbeTestAgentImage  = "registry.example/agent@sha256:" + "1111111111111111111111111111111111111111111111111111111111111111"
	lineupProbeTestReviewImage = "registry.example/review@sha256:" + "2222222222222222222222222222222222222222222222222222222222222222"
)

func lineupProbeTestToken(t *testing.T) SetupToken {
	t.Helper()
	token, err := NewSetupToken([]byte(claudeEnrollmentTestToken))
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func claudeWriterProbe(t *testing.T) LineupProbe {
	t.Helper()
	return LineupProbe{
		Harness: domain.HarnessClientClaudeCode, Image: lineupProbeTestAgentImage,
		Model: "claude-fable-5-1", NativeEffort: "high",
		Credential: SetupTokenProbeCredential(lineupProbeTestToken(t)),
	}
}

func codexReviewerProbe() LineupProbe {
	return LineupProbe{
		Harness: domain.HarnessClientCodexCLI, Image: lineupProbeTestReviewImage,
		Model: "gpt-6.1-sol", NativeEffort: "xhigh",
		Credential: LineupProbeCredential{mode: CodexAuthSubscription, body: []byte(`{"tokens":{}}`)},
	}
}

// TestLineupProbeCommandGolden pins both probe programs: the flags each CLI
// launches with, the credential's removal before any output is written, and
// the prompt as a positional argument.
func TestLineupProbeCommandGolden(t *testing.T) {
	claude := claudeWriterProbe(t)
	codex := codexReviewerProbe()
	commands := map[string][]string{
		"claude_code": lineupProbeCommand(claude, claudeReviewProvider{}.homeTarget(), claudeReviewProvider{}.configHomeTarget()),
		"codex_cli":   lineupProbeCommand(codex, codexReviewProvider{}.homeTarget(), codexReviewProvider{}.configHomeTarget()),
	}
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(commands); err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "lineup_probe_commands", body.Bytes())

	// A launch that sends no model or effort sends no option for it.
	claude.Model, claude.NativeEffort = "", ""
	script := lineupProbeCommand(claude, claudeReviewHome, claudeReviewConfigTarget)[2]
	if strings.Contains(script, "--model") || strings.Contains(script, "--effort") {
		t.Fatalf("empty launch values reached the command: %s", script)
	}
}

// The two refusals that motivated the probe (#1613), as each CLI printed
// them, are pinned here so a parser change that loses either fails.
const (
	claudeRefusedModelEnvelope = `{"type":"result","subtype":"success","is_error":true,"duration_ms":812,"num_turns":1,` +
		`"result":"API Error: 400 {\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",` +
		`\"message\":\"claude-fable-5-1 is not available in this version of Claude Code: version 2.1.251 or newer is required\"}}",` +
		`"total_cost_usd":0,"usage":{"input_tokens":0,"output_tokens":0},"modelUsage":{}}`
	codexRefusedModelEvents = `{"type":"thread.started","thread_id":"t"}` + "\n" +
		`{"type":"turn.started"}` + "\n" +
		`{"type":"error","message":"{\"detail\":\"The 'gpt-6.1-sol' model is not supported when using Codex with a ChatGPT account.\"}"}` + "\n" +
		`{"type":"turn.failed","error":{"message":"{\"detail\":\"The 'gpt-6.1-sol' model is not supported when using Codex with a ChatGPT account.\"}"}}` + "\n"
)

func TestJudgeLineupProbe(t *testing.T) {
	claudePass := `{"type":"result","subtype":"success","is_error":false,"num_turns":1,"result":"OK",` +
		`"total_cost_usd":0.0213,"usage":{"input_tokens":9,"output_tokens":4},` +
		`"modelUsage":{"claude-fable-5-1[1m]":{"outputTokens":4}}}`
	codexPass := `{"type":"turn.started"}` + "\n" + `{"type":"turn.completed","usage":{"input_tokens":3100,"output_tokens":5}}`
	for _, tc := range []struct {
		name        string
		harness     domain.HarnessClientKind
		model       string
		out         LineupProbeOutput
		wantPass    bool
		wantRefusal string
	}{
		{
			name: "claude pass", harness: domain.HarnessClientClaudeCode, model: "claude-fable-5-1",
			out: LineupProbeOutput{Version: []byte("2.1.282 (Claude Code)\n"), Stdout: []byte(claudePass)}, wantPass: true,
		},
		{
			name: "claude refused model", harness: domain.HarnessClientClaudeCode, model: "claude-fable-5-1",
			out:         LineupProbeOutput{Version: []byte("2.1.220 (Claude Code)\n"), Stdout: []byte(claudeRefusedModelEnvelope), ExitStatus: 1},
			wantRefusal: "version 2.1.251 or newer is required",
		},
		{
			name: "claude refused effort", harness: domain.HarnessClientClaudeCode, model: "claude-fable-5-1",
			out: LineupProbeOutput{
				Stdout: []byte(`{"type":"result","is_error":true,"result":"API Error: 400 effort 'max' is not supported by this model"}`), ExitStatus: 1,
			},
			wantRefusal: "effort 'max' is not supported",
		},
		{
			name: "claude substituted model", harness: domain.HarnessClientClaudeCode, model: "claude-opus-5-5",
			out:         LineupProbeOutput{Stdout: []byte(claudePass)},
			wantRefusal: `answered with ["claude-fable-5-1[1m]"], not "claude-opus-5-5"`,
		},
		{
			name: "claude unstructured refusal on stderr", harness: domain.HarnessClientClaudeCode, model: "claude-fable-5-1",
			out:         LineupProbeOutput{Stderr: []byte("error: unknown option '--safe-mode'\n"), ExitStatus: 1},
			wantRefusal: "unknown option '--safe-mode'",
		},
		{
			name: "claude nonzero exit with a clean envelope", harness: domain.HarnessClientClaudeCode, model: "claude-fable-5-1",
			out:         LineupProbeOutput{Stdout: []byte(claudePass), ExitStatus: 2},
			wantRefusal: "exited 2",
		},
		{
			name: "codex pass", harness: domain.HarnessClientCodexCLI, model: "gpt-6.1-sol",
			out: LineupProbeOutput{Version: []byte("codex-cli 0.162.1\n"), Stdout: []byte(codexPass)}, wantPass: true,
		},
		{
			name: "codex refused model", harness: domain.HarnessClientCodexCLI, model: "gpt-6.1-sol",
			out:         LineupProbeOutput{Version: []byte("codex-cli 0.147.0\n"), Stdout: []byte(codexRefusedModelEvents), ExitStatus: 1},
			wantRefusal: "not supported when using Codex with a ChatGPT account",
		},
		{
			name: "codex error event without a terminal event", harness: domain.HarnessClientCodexCLI, model: "gpt-6.1-sol",
			out: LineupProbeOutput{
				Stdout: []byte(`{"type":"turn.started"}` + "\n" + `{"type":"error","message":"Unsupported value: 'xhigh' for reasoning.effort"}`), ExitStatus: 1,
			},
			wantRefusal: "Unsupported value: 'xhigh'",
		},
		{
			name: "codex completed then nonzero exit", harness: domain.HarnessClientCodexCLI, model: "gpt-6.1-sol",
			out:         LineupProbeOutput{Stdout: []byte(codexPass), Stderr: []byte("panic: late\n"), ExitStatus: 101},
			wantRefusal: "panic: late",
		},
		{
			name: "no output at all", harness: domain.HarnessClientCodexCLI, model: "gpt-6.1-sol",
			out: LineupProbeOutput{ExitStatus: 127}, wantRefusal: "exited 127 with no error message",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := JudgeLineupProbe(tc.harness, tc.model, tc.out)
			if got.Passed != tc.wantPass {
				t.Fatalf("passed = %v, want %v (refusal %q)", got.Passed, tc.wantPass, got.Refusal)
			}
			if tc.wantPass && got.Refusal != "" {
				t.Fatalf("a pass carries a refusal: %q", got.Refusal)
			}
			if !strings.Contains(got.Refusal, tc.wantRefusal) {
				t.Fatalf("refusal = %q, want it to contain %q", got.Refusal, tc.wantRefusal)
			}
			if got.CLIBuild != firstLine(tc.out.Version) {
				t.Fatalf("build = %q", got.CLIBuild)
			}
		})
	}
}

func TestJudgeLineupProbeReportsUsage(t *testing.T) {
	got := JudgeLineupProbe(domain.HarnessClientClaudeCode, "m", LineupProbeOutput{Stdout: []byte(
		`{"type":"result","is_error":false,"total_cost_usd":0.0213,"usage":{"input_tokens":9,"output_tokens":4},"modelUsage":{"m":{}}}`,
	)})
	if !got.Passed || got.Usage.InputTokens == nil || *got.Usage.InputTokens != 9 ||
		got.Usage.OutputTokens == nil || *got.Usage.OutputTokens != 4 || got.Usage.CostUSD == nil || *got.Usage.CostUSD != "0.0213" {
		t.Fatalf("claude usage = %+v", got)
	}
	got = JudgeLineupProbe(domain.HarnessClientCodexCLI, "m", LineupProbeOutput{Stdout: []byte(
		`{"type":"turn.completed","usage":{"input_tokens":3100,"output_tokens":5}}`,
	)})
	if !got.Passed || *got.Usage.InputTokens != 3100 || *got.Usage.OutputTokens != 5 || got.Usage.CostUSD != nil {
		t.Fatalf("codex usage = %+v", got)
	}
}

func TestJudgeLineupProbeBoundsRefusalText(t *testing.T) {
	long := strings.Repeat("é", lineupProbeRefusalCap)
	got := JudgeLineupProbe(domain.HarnessClientCodexCLI, "m", LineupProbeOutput{Stderr: []byte(long), ExitStatus: 1})
	if len(got.Refusal) > lineupProbeRefusalCap || !strings.HasPrefix(got.Refusal, "é") || strings.ContainsRune(got.Refusal, '�') {
		t.Fatalf("refusal is %d bytes: %q...", len(got.Refusal), got.Refusal[:8])
	}
}

// lineupProbeFakeRuntime is the fake runtime with the probe container's
// export replaced by the files its command would have written.
type lineupProbeFakeRuntime struct {
	*fakeRuntime
	files []tarFile
}

func (r *lineupProbeFakeRuntime) ExportRootFS(ctx context.Context, id string, dest io.Writer, _ int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record("export %s", id)
	if err := r.checkCtx(ctx); err != nil {
		return err
	}
	if c, ok := r.ctrs[id]; !ok || r.state(c, id) != StateStopped {
		return errors.New("export of a container that is not stopped")
	}
	return writeFilesTar(dest, r.files...)
}

func newLineupProbeFakeRuntime(t *testing.T, files ...tarFile) *lineupProbeFakeRuntime {
	t.Helper()
	return &lineupProbeFakeRuntime{fakeRuntime: newFakeRuntime(t), files: files}
}

func lineupProbeOutputFiles(version, stdout, stderr, status string) []tarFile {
	return []tarFile{
		{lineupProbeVersion, []byte(version)},
		{lineupProbeStdout, []byte(stdout)},
		{lineupProbeStderr, []byte(stderr)},
		{lineupProbeStatus, []byte(status)},
		{"/var/lib/freeside/lineup-probe/ready/ready", []byte("ready\n")},
	}
}

func TestProbeLineupImageRunsOneContainedCallAndRemovesEverything(t *testing.T) {
	for _, p := range []LineupProbe{claudeWriterProbe(t), codexReviewerProbe()} {
		t.Run(string(p.Harness), func(t *testing.T) {
			rt := newLineupProbeFakeRuntime(t, lineupProbeOutputFiles("2.1.282 (Claude Code)\n", `{"type":"result"}`, "warn\n", "1\n")...)
			var copied []string
			var credential []byte
			rt.onCopyIntoContainer = func(_, hostDir, targetDir string) error {
				copied = append(copied, targetDir)
				if targetDir == lineupProbeStageDir {
					body, err := os.ReadFile(filepath.Join(hostDir, lineupProbeCredName)) //nolint:gosec // G304: the stage directory the probe under test hands the fake runtime
					if err != nil {
						return err
					}
					credential = body
				}
				return nil
			}
			var spec ContainerSpec
			rt.onCreateContainer = func(s ContainerSpec) error { spec = s; return nil }
			var granted RuntimeResourceNames
			authorize := func(_ context.Context, names RuntimeResourceNames) error { granted = names; return nil }

			out, err := ProbeLineupImage(t.Context(), rt, p, authorize)
			if err != nil {
				t.Fatal(err)
			}
			if want := (LineupProbeOutput{
				Version: []byte("2.1.282 (Claude Code)\n"), Stdout: []byte(`{"type":"result"}`), Stderr: []byte("warn\n"), ExitStatus: 1,
			}); !bytes.Equal(out.Version, want.Version) || !bytes.Equal(out.Stdout, want.Stdout) ||
				!bytes.Equal(out.Stderr, want.Stderr) || out.ExitStatus != want.ExitStatus {
				t.Fatalf("output = %+v", out)
			}
			if !slices.Equal(copied, []string{lineupProbeStageDir, lineupProbeReadyDir}) {
				t.Fatalf("copies = %v, want the credential then the ready sentinel", copied)
			}
			if !bytes.Equal(credential, p.Credential.body) {
				t.Fatal("the container received a different credential")
			}
			if len(granted.Containers) != 1 || granted.Containers[0] != spec.Name ||
				!slices.Equal(granted.Networks, []string{spec.Network}) {
				t.Fatalf("authorized %+v for container %q on %q", granted, spec.Name, spec.Network)
			}
			if spec.Image != p.Image || len(spec.Mounts) != 0 || spec.NetworkDisabled ||
				!strings.HasPrefix(spec.Name, "freeside-lineup-probe-") || spec.Network != spec.Name+"-egress" {
				t.Fatalf("spec = %+v", spec)
			}
			for _, env := range spec.Env {
				if strings.Contains(env, string(p.Credential.body)) {
					t.Fatal("the credential reached the container environment")
				}
			}
			if !slices.Contains(spec.Env, "NO_PROXY=") || !slices.ContainsFunc(spec.Env, func(e string) bool {
				return strings.HasPrefix(e, "HTTPS_PROXY=http://127.0.0.1:")
			}) {
				t.Fatalf("the container is not routed through the proxy: %v", spec.Env)
			}
			rt.mu.Lock()
			defer rt.mu.Unlock()
			if len(rt.ctrs) != 0 || len(rt.nets) != 0 {
				t.Fatalf("left %d containers and %d networks behind", len(rt.ctrs), len(rt.nets))
			}
		})
	}
}

// The probe's names must pass the production rig manifest's namespace check,
// or every probe under a held lease fails before it creates anything.
func TestProbeLineupImageNamesBindUnderAHeldRigLease(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	lease, err := daemonlock.AcquireRig(daemonlock.RigAcquireConfig{
		Owner:     daemonlock.RigOwner{User: "operator", Host: "rig-host", PID: os.Getpid(), Note: "acceptance"},
		StateRoot: state, DatabasePath: filepath.Join(state, "freeside.db"),
		ListenAddress: "127.0.0.1:8677", SeedRoot: filepath.Join(root, "seed"),
		LeaseRoot: filepath.Join(root, "rig-locks"),
		Now:       func() time.Time { return time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })

	rt := newLineupProbeFakeRuntime(t, lineupProbeOutputFiles("2.1.282 (Claude Code)\n", `{"type":"result"}`, "", "0\n")...)
	var spec ContainerSpec
	rt.onCreateContainer = func(s ContainerSpec) error { spec = s; return nil }
	var manifest daemonlock.RigManifest
	authorize := func(_ context.Context, names RuntimeResourceNames) error {
		bound, err := daemonlock.BindRigRuntimeResources(state, lease.Token(), names.Containers, names.Volumes, names.Networks)
		manifest = bound
		return err
	}

	out, err := ProbeLineupImage(t.Context(), rt, claudeWriterProbe(t), authorize)
	if err != nil {
		t.Fatal(err)
	}
	if string(out.Stdout) != `{"type":"result"}` || out.ExitStatus != 0 {
		t.Fatalf("output = %+v", out)
	}
	if !slices.Equal(manifest.Resources.Containers, []string{spec.Name}) ||
		!slices.Equal(manifest.Resources.Networks, []string{spec.Network}) ||
		len(manifest.Resources.Volumes) != 0 {
		t.Fatalf("bound %+v for container %q on %q", manifest.Resources, spec.Name, spec.Network)
	}
}

func TestProbeLineupImageRefusesAnExportStillHoldingTheCredential(t *testing.T) {
	for name, held := range map[string]tarFile{
		"staged credential": {lineupProbeStageDir + "/" + lineupProbeCredName, []byte("token")},
		"codex auth":        {CodexAuthFileTarget, []byte(`{"tokens":{}}`)},
	} {
		t.Run(name, func(t *testing.T) {
			files := append(lineupProbeOutputFiles("v\n", "", "", "0\n"), held)
			rt := newLineupProbeFakeRuntime(t, files...)
			_, err := ProbeLineupImage(t.Context(), rt, claudeWriterProbe(t), nil)
			if err == nil || !strings.Contains(err.Error(), "still holds the credential") {
				t.Fatalf("err = %v", err)
			}
			rt.mu.Lock()
			defer rt.mu.Unlock()
			if len(rt.ctrs) != 0 || len(rt.nets) != 0 {
				t.Fatalf("left %d containers and %d networks behind", len(rt.ctrs), len(rt.nets))
			}
		})
	}
}

func TestProbeLineupImageRefusesAnExportWithoutAStatus(t *testing.T) {
	rt := newLineupProbeFakeRuntime(t, tarFile{lineupProbeStdout, []byte("x")})
	if _, err := ProbeLineupImage(t.Context(), rt, claudeWriterProbe(t), nil); err == nil ||
		!strings.Contains(err.Error(), "no exit status") {
		t.Fatalf("err = %v", err)
	}
}

func TestProbeLineupImageRefusesBeforeCreatingAnything(t *testing.T) {
	for name, change := range map[string]func(*LineupProbe){
		"tag-only image":        func(p *LineupProbe) { p.Image = "registry.example/agent:latest" },
		"option-shaped model":   func(p *LineupProbe) { p.Model = "--dangerously-skip-permissions" },
		"spaced effort":         func(p *LineupProbe) { p.NativeEffort = "high --x" },
		"codex credential":      func(p *LineupProbe) { p.Credential = codexReviewerProbe().Credential },
		"no credential":         func(p *LineupProbe) { p.Credential = LineupProbeCredential{} },
		"unknown harness":       func(p *LineupProbe) { p.Harness = "gemini_cli" },
		"codex without a model": func(p *LineupProbe) { *p = codexReviewerProbe(); p.Model = "" },
	} {
		t.Run(name, func(t *testing.T) {
			p := claudeWriterProbe(t)
			change(&p)
			rt := newLineupProbeFakeRuntime(t)
			authorized := false
			if _, err := ProbeLineupImage(t.Context(), rt, p, func(context.Context, RuntimeResourceNames) error {
				authorized = true
				return nil
			}); err == nil {
				t.Fatal("probe accepted an invalid launch")
			}
			if authorized || len(rt.calls) != 0 {
				t.Fatalf("refusal reached the runtime: authorized=%v calls=%v", authorized, rt.calls)
			}
		})
	}
}

func TestReviewSnapshotProbeCredential(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil { //nolint:gosec // fixture is a private directory
		t.Fatal(err)
	}
	write := func(name string, body []byte) string {
		p := filepath.Join(root, name)
		if err := os.WriteFile(p, body, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	fresh := write("fresh.json", codexHostAuthBody(t, "refresh-secret", now.Add(time.Hour), now))
	cred, err := ReviewSnapshotProbeCredential(domain.HarnessClientCodexCLI, CodexAuthSubscription, root, fresh, now)
	if err != nil {
		t.Fatal(err)
	}
	if cred.mode != CodexAuthSubscription || bytes.Contains(cred.body, []byte("refresh-secret")) {
		t.Fatal("the probe credential carries the refresh token")
	}

	stale := write("stale.json", codexHostAuthBody(t, "refresh-secret", now.Add(5*time.Minute), now))
	if _, err := ReviewSnapshotProbeCredential(domain.HarnessClientCodexCLI, CodexAuthSubscription, root, stale, now); err == nil ||
		!strings.Contains(err.Error(), "renew-codex") {
		t.Fatalf("near-expiry token: err = %v", err)
	}

	token := write("token", []byte(claudeEnrollmentTestToken+"\n"))
	cred, err = ReviewSnapshotProbeCredential(domain.HarnessClientClaudeCode, CodexAuthSetupToken, root, token, now)
	if err != nil || string(cred.body) != claudeEnrollmentTestToken {
		t.Fatalf("claude snapshot: %q, %v", cred.body, err)
	}
	if _, err := ReviewSnapshotProbeCredential(domain.HarnessClientClaudeCode, CodexAuthSubscription, root, token, now); err == nil {
		t.Fatal("a Claude reviewer accepted a Codex auth mode")
	}
}
