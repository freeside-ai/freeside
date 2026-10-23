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
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

const (
	// The refusals the pinned CLIs printed when the lineup named a model they
	// don't serve (#2208): a Claude Code build too old for the model, and a
	// Codex model a ChatGPT-account login can't use.
	probeClaudeRefusedModel = `{"type":"result","is_error":true,"result":"API Error: 400 {\"type\":\"error\",` +
		`\"error\":{\"type\":\"invalid_request_error\",\"message\":\"claude-fable-5-1 is not available in this ` +
		`version of Claude Code: version 2.1.251 or newer is required\"}}","modelUsage":{}}`
	probeCodexRefusedModel = `{"type":"turn.started"}` + "\n" +
		`{"type":"turn.failed","error":{"message":"{\"detail\":\"The 'gpt-6.1-sol' model is not supported ` +
		`when using Codex with a ChatGPT account.\"}"}}` + "\n"
	probeClaudeRefusedEffort = `{"type":"result","is_error":true,` +
		`"result":"API Error: 400 effort 'max' is not supported by this model","modelUsage":{}}`
)

// probeClaudePass is a Claude envelope that the named model answered.
func probeClaudePass(model string) string {
	return `{"type":"result","is_error":false,"result":"OK","total_cost_usd":0.0213,` +
		`"usage":{"input_tokens":9,"output_tokens":4},"modelUsage":{"` + model + `":{"outputTokens":4}}}`
}

const probeCodexPass = `{"type":"turn.started"}` + "\n" +
	`{"type":"turn.completed","usage":{"input_tokens":3100,"output_tokens":5}}` + "\n"

// fakeLineupProber answers each call by its surface and model, and records
// the calls it was asked to make.
type fakeLineupProber struct {
	outputs map[string]ward.LineupProbeOutput
	errs    map[string]error
	calls   []lineupCall
}

func fakeProbeKey(surface lineupSurface, model string) string { return string(surface) + "/" + model }

func (f *fakeLineupProber) probe(_ context.Context, call lineupCall) (ward.LineupProbeOutput, error) {
	f.calls = append(f.calls, call)
	key := fakeProbeKey(call.Surface, call.Model)
	if err, ok := f.errs[key]; ok {
		return ward.LineupProbeOutput{}, err
	}
	out, ok := f.outputs[key]
	if !ok {
		return ward.LineupProbeOutput{}, errors.New("fake prober has no output for " + key)
	}
	return out, nil
}

// probeLineupSettings loads the deputy settings fixture with the writers and
// reviewer the refusal fixtures name, and judgment on when judgment is set.
func probeLineupSettings(t *testing.T, judgment bool) loadedDeputySettings {
	t.Helper()
	settings := deputySettingsFixture(t)
	settings["writers"] = map[string]any{
		"specifier":   map[string]any{"model": "claude-fable-5-1", "effort": "high"},
		"implementer": map[string]any{"model": "claude-opus-5-5", "effort": "max"},
		"remediator":  map[string]any{"model": "claude-opus-5-5", "effort": "max"},
	}
	settings["review"].(map[string]any)["model"] = "gpt-6.1-sol"
	settings["review"].(map[string]any)["effort"] = "max"
	if judgment {
		settings["judgment"] = map[string]any{
			"claude_bin": "/opt/claude/bin/claude", "claude_sha256": strings.Repeat("e", 64),
			"model": "claude-sonnet-5-5", "effort": "medium", "publication_author_prompt": "/prompts/author.md",
		}
	}
	path, _ := writeDeputySettings(t, settings, 0o600)
	loaded, err := loadDeputySettings(path)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

var probeLineupTime = time.Date(2026, 10, 23, 18, 30, 0, 0, time.UTC)

// passingLineupProber answers every call the settings fixture names.
func passingLineupProber() *fakeLineupProber {
	claudeVersion := []byte("2.1.282 (Claude Code)\n")
	return &fakeLineupProber{outputs: map[string]ward.LineupProbeOutput{
		fakeProbeKey(lineupSurfaceAgentImage, "claude-fable-5-1"): {
			Version: claudeVersion, Stdout: []byte(probeClaudePass("claude-fable-5-1[1m]")),
		},
		fakeProbeKey(lineupSurfaceAgentImage, "claude-opus-5-5"): {
			Version: claudeVersion, Stdout: []byte(probeClaudePass("claude-opus-5-5")),
		},
		fakeProbeKey(lineupSurfaceReviewImage, "gpt-6.1-sol"): {
			Version: []byte("codex-cli 0.162.1\n"), Stdout: []byte(probeCodexPass),
		},
		fakeProbeKey(lineupSurfaceJudgmentBinary, "claude-sonnet-5-5"): {
			Version: []byte("2.1.267 (Claude Code)\n"), Stdout: []byte(probeClaudePass("claude-sonnet-5-5")),
		},
	}}
}

func TestLineupCallsListEachDistinctLaunchOnce(t *testing.T) {
	t.Parallel()
	settings := probeLineupSettings(t, true)
	calls, err := lineupCalls(&settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 4 {
		t.Fatalf("calls = %+v; want the specifier, the shared implementer and remediator, the reviewer, and judgment", calls)
	}
	shared := calls[1]
	if len(shared.Roles) != 2 || shared.Roles[0] != domain.RoleImplementer || shared.Roles[1] != domain.RoleRemediator {
		t.Fatalf("shared writer call roles = %v", shared.Roles)
	}
	if calls[2].Harness != domain.HarnessClientCodexCLI || calls[2].NativeEffort != "xhigh" {
		t.Fatalf("reviewer call = %+v; want codex at its native max", calls[2])
	}
	if calls[3].Surface != lineupSurfaceJudgmentBinary || len(calls[3].Roles) != len(judgmentProbeRoles()) {
		t.Fatalf("judgment call = %+v; want every judgment role on one call", calls[3])
	}

	settings = probeLineupSettings(t, false)
	calls, err = lineupCalls(&settings)
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range calls {
		if call.Surface == lineupSurfaceJudgmentBinary {
			t.Fatalf("judgment off still probes the judgment binary: %+v", call)
		}
	}
}

func TestProbeLineupRecordGolden(t *testing.T) {
	t.Parallel()
	settings := probeLineupSettings(t, true)
	settings.Digest = domain.Digest("sha256:" + strings.Repeat("f", 64)) // The fixture's digest moves with its bytes.
	calls, err := lineupCalls(&settings)
	if err != nil {
		t.Fatal(err)
	}
	prober := passingLineupProber()
	record := probeLineup(t.Context(), calls, prober, settings.Digest, "freesided 0.0.0-test", probeLineupTime)
	if !record.Passed {
		t.Fatalf("record failed: %+v", record)
	}
	for _, call := range record.Calls {
		if !call.Surface.valid() || !call.Verdict.valid() {
			t.Fatalf("call carries an invalid enum: %+v", call)
		}
	}
	body, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "lineup_probe_record", append(body, '\n'))
}

func TestProbeLineupFailsAndNamesEachRefusal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		key          string
		out          ward.LineupProbeOutput
		wantVerdict  lineupVerdict
		wantFailLine string
	}{
		{
			name: "refused claude writer model", key: fakeProbeKey(lineupSurfaceAgentImage, "claude-fable-5-1"),
			out: ward.LineupProbeOutput{
				Version: []byte("2.1.240 (Claude Code)\n"), Stdout: []byte(probeClaudeRefusedModel), ExitStatus: 1,
			},
			wantVerdict: lineupVerdictRefused,
			wantFailLine: `refused [specifier]: model "claude-fable-5-1" at effort "high" (sent "high") on 2.1.240 (Claude Code): ` +
				"API Error: 400",
		},
		{
			name: "refused codex reviewer model", key: fakeProbeKey(lineupSurfaceReviewImage, "gpt-6.1-sol"),
			out: ward.LineupProbeOutput{
				Version: []byte("codex-cli 0.162.1\n"), Stdout: []byte(probeCodexRefusedModel), ExitStatus: 1,
			},
			wantVerdict: lineupVerdictRefused,
			wantFailLine: `refused [reviewer]: model "gpt-6.1-sol" at effort "max" (sent "xhigh") on codex-cli 0.162.1: ` +
				"{\"detail\":\"The 'gpt-6.1-sol' model is not supported when using Codex with a ChatGPT account.\"}",
		},
		{
			name: "refused effort", key: fakeProbeKey(lineupSurfaceAgentImage, "claude-opus-5-5"),
			out: ward.LineupProbeOutput{
				Version: []byte("2.1.282 (Claude Code)\n"), Stdout: []byte(probeClaudeRefusedEffort), ExitStatus: 1,
			},
			wantVerdict: lineupVerdictRefused,
			wantFailLine: `refused [implementer remediator]: model "claude-opus-5-5" at effort "max" (sent "max") ` +
				"on 2.1.282 (Claude Code): API Error: 400 effort 'max' is not supported by this model",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			settings := probeLineupSettings(t, true)
			calls, err := lineupCalls(&settings)
			if err != nil {
				t.Fatal(err)
			}
			prober := passingLineupProber()
			prober.outputs[tc.key] = tc.out
			record := probeLineup(t.Context(), calls, prober, settings.Digest, "test", probeLineupTime)
			if record.Passed {
				t.Fatal("record passed with a refused call")
			}
			if len(prober.calls) != len(calls) {
				t.Fatalf("probed %d of %d calls; a refusal must not stop the rest", len(prober.calls), len(calls))
			}

			stateRoot := t.TempDir()
			var stdout, stderr bytes.Buffer
			err = reportLineupProbe(stateRoot, record, &stdout, &stderr)
			if err == nil || !strings.Contains(err.Error(), "1 of 4 lineup calls failed") {
				t.Fatalf("err = %v; want one failed call named", err)
			}
			if !strings.Contains(stderr.String(), tc.wantFailLine) {
				t.Fatalf("stderr = %q\nwant a line containing %q", stderr.String(), tc.wantFailLine)
			}
			written := readLineupProbeRecord(t, stateRoot)
			if !bytes.Equal(written, stdout.Bytes()) {
				t.Fatal("the record printed differs from the record written")
			}
			var decoded lineupProbeRecord
			if err := json.Unmarshal(written, &decoded); err != nil {
				t.Fatal(err)
			}
			failed := 0
			for _, call := range decoded.Calls {
				if call.Verdict != lineupVerdictPass {
					failed++
					if call.Verdict != tc.wantVerdict || call.Refusal == nil || call.ObservedBuild == nil {
						t.Fatalf("failed call = %+v", call)
					}
				}
			}
			if failed != 1 || decoded.Passed {
				t.Fatalf("decoded record = %+v", decoded)
			}
		})
	}
}

func TestProbeLineupRecordsACallThatCouldNotRun(t *testing.T) {
	t.Parallel()
	settings := probeLineupSettings(t, false)
	calls, err := lineupCalls(&settings)
	if err != nil {
		t.Fatal(err)
	}
	prober := passingLineupProber()
	prober.errs = map[string]error{
		fakeProbeKey(lineupSurfaceReviewImage, "gpt-6.1-sol"): errors.New("codex credential expires before the probe floor"),
	}
	record := probeLineup(t.Context(), calls, prober, settings.Digest, "test", probeLineupTime)
	stateRoot := t.TempDir()
	var stdout, stderr bytes.Buffer
	if err := reportLineupProbe(stateRoot, record, &stdout, &stderr); err == nil {
		t.Fatal("a call that could not run passed the probe")
	}
	if want := `error [reviewer]: model "gpt-6.1-sol" at effort "max" (sent "xhigh") on unknown: ` +
		"codex credential expires before the probe floor"; !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr = %q\nwant a line containing %q", stderr.String(), want)
	}
	readLineupProbeRecord(t, stateRoot)
}

func TestReportLineupProbeWritesAPassingRecordAndExitsZero(t *testing.T) {
	t.Parallel()
	settings := probeLineupSettings(t, true)
	calls, err := lineupCalls(&settings)
	if err != nil {
		t.Fatal(err)
	}
	record := probeLineup(t.Context(), calls, passingLineupProber(), settings.Digest, "test", probeLineupTime)
	stateRoot := t.TempDir()
	// A record left by an earlier run is replaced, not appended to.
	if err := os.WriteFile(filepath.Join(stateRoot, lineupProbeRecordName), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := reportLineupProbe(stateRoot, record, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("a passing probe printed failures: %q", stderr.String())
	}
	if written := readLineupProbeRecord(t, stateRoot); !bytes.Equal(written, stdout.Bytes()) {
		t.Fatal("the record printed differs from the record written")
	}
}

// readLineupProbeRecord returns lineup-probe.json after checking that it is
// owner-only, which every write must leave it.
func readLineupProbeRecord(t *testing.T, stateRoot string) []byte {
	t.Helper()
	path := filepath.Join(stateRoot, lineupProbeRecordName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("%s has mode %04o; want 0600", lineupProbeRecordName, perm)
	}
	body, err := os.ReadFile(path) //nolint:gosec // G304: the test's own temp directory.
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestParseProbeLineupConfig(t *testing.T) {
	t.Parallel()
	if _, err := parseProbeLineupConfig([]string{"-root", "relative"}, &bytes.Buffer{}); err == nil {
		t.Fatal("accepted a relative -root")
	}
	if _, err := parseProbeLineupConfig([]string{"-root", "/r", "extra"}, &bytes.Buffer{}); err == nil {
		t.Fatal("accepted a positional argument")
	}
	cfg, err := parseProbeLineupConfig([]string{"-root", "/r"}, &bytes.Buffer{})
	if err != nil || cfg.ContainerBin != "container" || cfg.RigTokenFile != "" {
		t.Fatalf("cfg = %+v, err = %v", cfg, err)
	}
}

func TestLineupProbeEnumsRegisterEveryMember(t *testing.T) {
	t.Parallel()
	for _, surface := range allLineupSurfaces {
		if !surface.valid() {
			t.Errorf("surface %q is registered but invalid", surface)
		}
	}
	for _, verdict := range allLineupVerdicts {
		if !verdict.valid() {
			t.Errorf("verdict %q is registered but invalid", verdict)
		}
	}
	if lineupSurface("").valid() || lineupVerdict("").valid() {
		t.Error("the zero value is valid")
	}
}

// TestLineupProbeRecordLeavesTheManifestUnchanged pins that the probe's
// record is evidence beside the composition manifest, never an input to it:
// a launch preflight composes the same bytes with or without it (#2208).
func TestLineupProbeRecordLeavesTheManifestUnchanged(t *testing.T) {
	args, environment := preflightFixture(t)
	stateRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rigPath := args[slices.Index(args, "-rig-token-file")+1]
	var rig rigHoldOutput
	body, err := os.ReadFile(rigPath) //nolint:gosec // G304: the fixture's own temp file.
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &rig); err != nil {
		t.Fatal(err)
	}
	rig.Manifest.Resources.StateRoot = stateRoot
	rig.Manifest.Resources.DatabasePath = filepath.Join(stateRoot, "freeside.db")
	if body, err = json.Marshal(rig); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rigPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := parsePreflightConfig(args, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	cfg.TaskPath, cfg.PolicyPath, cfg.PublicationPath = "", "", ""
	cfg.ProjectID = "proj-launch"
	cfg.Launch = true
	cfg.SettingsDigest = domain.Digest("sha256:" + strings.Repeat("5", 64))
	compose := func() []byte {
		t.Helper()
		manifest, err := composePreflight(t.Context(), cfg, environment,
			time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC), "2dce6570ee23")
		if err != nil {
			t.Fatal(err)
		}
		if manifest.Status != compositionPassed {
			t.Fatalf("launch manifest status %s: %+v", manifest.Status, manifest.Checks)
		}
		body, err := json.MarshalIndent(manifest, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	without := compose()

	settings := probeLineupSettings(t, true)
	calls, err := lineupCalls(&settings)
	if err != nil {
		t.Fatal(err)
	}
	prober := passingLineupProber()
	prober.outputs[fakeProbeKey(lineupSurfaceReviewImage, "gpt-6.1-sol")] = ward.LineupProbeOutput{
		Stdout: []byte(probeCodexRefusedModel), ExitStatus: 1,
	}
	record := probeLineup(t.Context(), calls, prober, settings.Digest, "test", probeLineupTime)
	if err := reportLineupProbe(stateRoot, record, io.Discard, io.Discard); err == nil {
		t.Fatal("a refused reviewer passed the probe")
	}
	readLineupProbeRecord(t, stateRoot)

	if with := compose(); !bytes.Equal(without, with) {
		t.Fatalf("a failed lineup-probe.json changed the launch manifest:\nwithout:\n%s\nwith:\n%s", without, with)
	}
}

func TestProbeLineupStopsStartingCallsOnceCancelled(t *testing.T) {
	t.Parallel()
	settings := probeLineupSettings(t, true)
	calls, err := lineupCalls(&settings)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	prober := passingLineupProber()
	record := probeLineup(ctx, calls, prober, settings.Digest, "test", probeLineupTime)
	if len(prober.calls) != 0 {
		t.Fatalf("started %d calls after cancellation", len(prober.calls))
	}
	if record.Passed || len(record.Calls) != len(calls) {
		t.Fatalf("record = %+v; want every call recorded as an error", record)
	}
	for _, call := range record.Calls {
		if call.Verdict != lineupVerdictError {
			t.Fatalf("call = %+v; want an error verdict", call)
		}
	}
}

func TestProbeLineupRefusesWhileADaemonHoldsTheRoot(t *testing.T) {
	t.Parallel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(root, "daemon")
	if err := os.Mkdir(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	_, body := writeDeputySettings(t, deputySettingsFixture(t), 0o600)
	paths := newDeputyPaths(stateRoot)
	if err := os.WriteFile(paths.Settings, body, 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := daemonlock.Acquire(paths.DB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	err = runProbeLineupCommand(t.Context(), []string{"-root", root, "-container-bin", "/nonexistent/container"},
		io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "a daemon holds this root") {
		t.Fatalf("err = %v; want the held-lock refusal", err)
	}
	if _, err := os.Stat(filepath.Join(stateRoot, lineupProbeRecordName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused probe wrote %s: %v", lineupProbeRecordName, err)
	}
}
