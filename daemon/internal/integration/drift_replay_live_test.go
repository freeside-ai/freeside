package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/claudeinference"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
)

const (
	driftReplayLiveEnv   = "FREESIDE_DRIFT_REPLAY_LIVE_TEST"
	driftReplayRecordEnv = "FREESIDE_DRIFT_REPLAY_RECORD"
	driftReplayModelEnv  = "FREESIDE_DRIFT_REPLAY_MODEL"
	driftReplayCLIEnv    = "FREESIDE_JUDGMENT_CLI"
	driftReplayCLIPinEnv = "FREESIDE_JUDGMENT_CLI_SHA256"
	driftReplayTokenEnv  = "CLAUDE_CODE_OAUTH_TOKEN" //nolint:gosec // G101: the variable's name, not a credential
)

// driftReplayRecorder keeps the driver's raw answer, which AuditDrift turns
// into an artifact and does not return.
type driftReplayRecorder struct {
	inner    inference.Driver
	response inference.Response
}

func (r *driftReplayRecorder) Complete(
	ctx context.Context, request inference.Request, credential inference.Secret,
) (inference.Response, error) {
	response, err := r.inner.Complete(ctx, request, credential)
	if err == nil {
		r.response = response
	}
	return response, err
}

// TestDriftReplayLive audits each fixture with the real provider through the
// production driver. It is opt-in and CI-blind: it spends the operator's
// subscription and its answer varies between runs, so it asserts only that an
// artifact came back. With the record variable set it writes the raw answer
// beside the fixture, and refuses to replace one already there: a kept answer
// is the first one, never the best of several.
//
// The token reaches only the driver's child process. It is never logged, and
// an answer that contains it is not written.
func TestDriftReplayLive(t *testing.T) {
	if os.Getenv(driftReplayLiveEnv) != "1" {
		t.Skip("live drift replay is opt-in: set " + driftReplayLiveEnv + "=1, " +
			driftReplayCLIEnv + " (absolute path to the native Claude CLI), " +
			driftReplayCLIPinEnv + " (its SHA-256), " + driftReplayModelEnv + " (the judgment model), and " +
			driftReplayTokenEnv + " (the operator's setup token); set " + driftReplayRecordEnv +
			"=1 to write each fixture's recorded_response.json; select one pull request with " +
			"-run 'TestDriftReplayLive/pr-<N>'")
	}
	token := os.Getenv(driftReplayTokenEnv)
	model := os.Getenv(driftReplayModelEnv)
	if token == "" || model == "" {
		t.Fatal(driftReplayTokenEnv + " and " + driftReplayModelEnv + " are required when " + driftReplayLiveEnv + "=1")
	}
	driver, err := claudeinference.New(claudeinference.Config{
		Binary: os.Getenv(driftReplayCLIEnv), SHA256: os.Getenv(driftReplayCLIPinEnv), Model: model,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range loadDriftReplayFixtures(t) {
		t.Run(filepath.Base(fixture.Dir), func(t *testing.T) {
			recorder := &driftReplayRecorder{inner: driver}
			client := driftReplayClient(t, inference.Binding{
				Provider: claudeinference.Protocol, Model: model,
				Credential: inference.Secret(token), Driver: recorder,
			}, time.Now)
			input := fixture.auditInput(t)
			audit, err := client.AuditDrift(context.Background(), driftReplayProject, string(input.RunID), input)
			if err != nil {
				// A fail-safe result is evidence too: the error names the reason.
				t.Fatal(err)
			}
			if bytes.Contains(recorder.response.Output, []byte(token)) {
				t.Fatal("the answer contains the credential; nothing was logged or written")
			}
			t.Logf("verdict %s, confidence %s, %d reversals", audit.Verdict, audit.Confidence, len(audit.Reversals))
			for _, reversal := range audit.Reversals {
				t.Logf("reversal %s: %s", reversal.FindingID, reversal.Undo)
			}
			if os.Getenv(driftReplayRecordEnv) != "1" {
				return
			}
			writeDriftReplayRecording(t, fixture.Dir, driftReplayRecording{
				Model: model, RecordedAt: audit.CreatedAt, DaemonCommit: driftReplayDaemonCommit(t),
				ComputeUnits: recorder.response.ComputeUnits, Output: recorder.response.Output,
			})
		})
	}
}

// driftReplayDaemonCommit names the daemon revision whose site and auditor
// instruction made the call. The fixture and this test may be uncommitted
// when a recording is made; they are not what the commit vouches for.
func driftReplayDaemonCommit(t *testing.T) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "git", "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func writeDriftReplayRecording(t *testing.T, dir string, recording driftReplayRecording) {
	t.Helper()
	path := filepath.Join(dir, "recorded_response.json")
	// 0o600 with O_EXCL: a committed, non-sensitive fixture that a second run
	// must not replace.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // G304: fixed basename in a fixture directory the test listed.
	if err != nil {
		t.Fatalf("%s: %v (delete it deliberately to record again, and say so in the manifest)", path, err)
	}
	encoder := json.NewEncoder(file)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(recording); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
