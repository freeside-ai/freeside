package ward

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

// This exercises the pinned CLI's own sandbox, not an outer-container Git
// check. The latter succeeds as privileged root even when sandbox root cannot
// traverse the host-owned 0700 checkout copied onto the volume.
func TestLiveCodexReviewSandboxWorkspaceAccess(t *testing.T) {
	if os.Getenv("FREESIDE_WARD_LIVE_TEST") != "1" {
		t.Skip("set FREESIDE_WARD_LIVE_TEST=1 and the exporter/Codex image pins")
	}
	image := os.Getenv("FREESIDE_WARD_CODEX_AGENT_IMAGE")
	if !digestPinnedImagePattern.MatchString(image) {
		t.Fatal("FREESIDE_WARD_CODEX_AGENT_IMAGE must be digest pinned")
	}
	bin, err := osexec.LookPath("container")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	checkout := initLiveSeedCheckout(t, root)
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := commitLiveSeedCheckout(t, checkout)
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("head\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	head := commitLiveSeedCheckout(t, checkout)
	ctx := context.Background()
	rt := NewCLIRuntime(bin)
	cfg := testConfig()
	cfg.ExporterImage = liveExporterImage(t)
	cfg.SeedRoot = root
	cfg.SeedTimeout = 2 * time.Minute
	cfg.PollInterval = 500 * time.Millisecond
	lc, err := NewCodexReviewLifecycle(rt, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("liveaccess-%d", time.Now().UnixNano())
	journal := &fakeCodexReviewJournal{}
	t.Cleanup(func() {
		if err := lc.CleanupCodexReviewWorkspace(context.Background(), journal, id); err != nil {
			t.Error(err)
		}
	})
	binding, err := lc.PrepareCodexReviewWorkspace(ctx, journal, id, checkout, head, 64)
	if err != nil {
		t.Fatal(err)
	}
	probe := "set -eu; id; pwd; cat /proc/self/uid_map; " +
		"stat -c '%a %u %g' /workspace/project; " +
		"test \"$(stat -c '%a %u %g' /workspace/project)\" = '700 0 0'; " +
		"test \"$(stat -c '%a' /workspace/project/README.md)\" = 600; " +
		"test \"$(pwd -P)\" = /workspace/project; " +
		"test \"$(git rev-parse HEAD)\" = " + shellQuote(head.BaseSHA) + "; " +
		"git cat-file -e " + shellQuote(base.BaseSHA+"^{commit}") + "; " +
		"git cat-file -e " + shellQuote(head.BaseSHA+"^{commit}") + "; " +
		"cat README.md; git --no-pager diff --no-ext-diff --no-textconv " +
		shellQuote(base.BaseSHA) + " " + shellQuote(head.BaseSHA) + " --"
	command := "codex --version; id; stat -c '%a %u %g' /workspace/project; " +
		"git -c safe.directory=/workspace/project -C /workspace/project rev-parse HEAD; " +
		"codex sandbox -P freeside-review " +
		"-c 'permissions.freeside-review.filesystem={\":root\"=\"read\"}' " +
		"-c permissions.freeside-review.network.enabled=false " +
		"-C /workspace/project -- sh -c " + shellQuote(probe)
	out, probeErr := osexec.CommandContext(ctx, bin, "run", "--rm", "--name", id+"-probe", //nolint:gosec // resolved CLI, pinned image, fixture-owned volume
		"--network", "none", "--volume", binding.Volume+":/workspace/project:ro",
		image, "sh", "-c", command).CombinedOutput()
	t.Logf("image=%s base=%s head=%s\n%s", image, base.BaseSHA, head.BaseSHA, out)
	if probeErr != nil || !strings.Contains(string(out), "+head") {
		t.Fatalf("private-workspace sandbox access failed: %v", probeErr)
	}
	for _, tc := range []struct {
		name, workspace, base, head string
		wantSuccess                 bool
	}{
		{"bound diff", "/workspace/project", base.BaseSHA, head.BaseSHA, true},
		{"wrong cwd", "/workspace", base.BaseSHA, head.BaseSHA, false},
		{"absent workspace", "/absent", base.BaseSHA, head.BaseSHA, false},
		{"absent base", "/workspace/project", strings.Repeat("f", 40), head.BaseSHA, false},
		{"absent head", "/workspace/project", base.BaseSHA, strings.Repeat("f", 40), false},
		{"wrong bound head", "/workspace/project", base.BaseSHA, base.BaseSHA, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := osexec.CommandContext(ctx, bin, "run", "--rm", "--name", id+"-gate", //nolint:gosec // resolved CLI, pinned image, fixture-owned volume
				"--network", "none", "--volume", binding.Volume+":/workspace/project:ro", image,
				"sh", "-c", codexReviewAccessCommand(tc.workspace, tc.base, tc.head)).CombinedOutput()
			if (err == nil) != tc.wantSuccess {
				t.Fatalf("sandbox access gate: %v\n%s", err, out)
			}
		})
	}
	if os.Getenv("FREESIDE_WARD_CODEX_REVIEW_PAID") == "1" {
		for _, missingBase := range []bool{true, false} {
			t.Run(fmt.Sprintf("collected-missing-base-%t", missingBase), func(t *testing.T) {
				liveCodexReviewAccessCollection(t, lc, image, cfg.ExporterImage, checkout, base, head, missingBase)
			})
		}
	}
}

// The paid opt-in uses the ordinary source, authenticated collection, and
// cleanup path. Auth is read only from an explicitly named private snapshot.
func liveCodexReviewAccessCollection(t *testing.T, lc *CodexReviewLifecycle, image, exporter, checkout string, base, head domain.BaseRevision, missingBase bool) {
	t.Helper()
	authPath := os.Getenv("FREESIDE_WARD_CODEX_REVIEW_AUTH")
	if authPath == "" {
		t.Fatal("paid live review requires FREESIDE_WARD_CODEX_REVIEW_AUTH")
	}
	auth, err := os.ReadFile(authPath) //nolint:gosec // explicit operator opt-in snapshot, never logged
	if err != nil {
		t.Fatal("read the supplied private auth snapshot")
	}
	cfg, spec := testCodexReview(t)
	cfg.ApprovedImage, cfg.ObserverImage = image, exporter
	cfg.Now = func() time.Time { return time.Now().UTC() }
	if model := os.Getenv("FREESIDE_WARD_CODEX_REVIEW_MODEL"); model != "" {
		cfg.Model = model
	}
	inputRoot, err := os.OpenRoot(cfg.InputRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(inputRoot.WriteFile("live-auth.json", auth, 0o600), inputRoot.Close()); err != nil {
		t.Fatal(err)
	}
	spec.AuthSnapshot = filepath.Join(cfg.InputRoot, "live-auth.json")
	journal := &fakeCodexReviewJournal{}
	sourceCfg := codexReviewSourceConfigForTest(t, lc, cfg, spec, journal)
	sourceCfg.Now = cfg.Now
	source, err := NewCodexReviewSource(sourceCfg)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID(fmt.Sprintf("livecollect-%d", time.Now().UnixNano()))
	req := exec.ReviewRequest{
		RunID: domain.RunID(id), Round: 1, Repo: head.Repo, RepositoryID: head.RepositoryID,
		BaseRef: base.BaseRef, BaseSHA: base.BaseSHA, HeadSHA: head.BaseSHA, Workspace: checkout,
		Verification: testReviewVerificationEvidence(), Instructions: spec.InstructionBinding, RequestedAt: time.Now().UTC(),
	}
	if missingBase {
		req.BaseSHA = strings.Repeat("f", 40)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	if err := source.RequestReview(ctx, id, req); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = lc.AbortCodexReview(context.Background(), sourceCfg.Review, string(id))
		source.mu.Lock()
		defer source.mu.Unlock()
		_ = source.launches[id].Close()
	})
	for {
		status, err := source.Inspect(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if status == exec.StatusCompleted || status == exec.StatusFailed {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Second):
		}
	}
	outcome, ready, err := journal.GetCodexReviewOutcome(ctx, string(id))
	if err != nil || !ready || outcome.Collection == nil {
		t.Fatalf("missing authenticated collection: ready=%t err=%v", ready, err)
	}
	if err := outcome.verifyCompletionEvidence(codexReviewProvider{}); err != nil {
		t.Fatal(err)
	}
	if evidenceDir := os.Getenv("FREESIDE_WARD_CODEX_REVIEW_EVIDENCE_DIR"); evidenceDir != "" {
		body, err := json.MarshalIndent(outcome, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		evidenceRoot, err := os.OpenRoot(evidenceDir)
		if err != nil {
			t.Fatal(err)
		}
		if err := errors.Join(evidenceRoot.WriteFile(string(id)+".json", body, 0o600), evidenceRoot.Close()); err != nil {
			t.Fatal(err)
		}
	}
	result, pollErr := source.Poll(ctx, id)
	if missingBase {
		if !errors.Is(pollErr, exec.ErrNoResult) || outcome.Result != nil || outcome.Collection.ExitStatus == 0 {
			t.Fatalf("missing base passed: %v", pollErr)
		}
	} else if pollErr != nil || len(result.Findings) != 0 || !strings.Contains(string(outcome.Collection.Events), "command_execution") {
		t.Fatalf("clean fixture did not complete an inspected zero-finding review: %v", pollErr)
	}
	restarted, err := NewCodexReviewSource(sourceCfg)
	if err != nil {
		t.Fatal(err)
	}
	_, replayErr := restarted.Poll(ctx, id)
	if (replayErr != nil) != (pollErr != nil) {
		t.Fatalf("restart changed review disposition: %v", replayErr)
	}
	t.Logf("invocation=%s base=%s head=%s exit=%d collection=%s events=%d bytes", id, req.BaseSHA, req.HeadSHA,
		outcome.Collection.ExitStatus, outcome.CollectionEvidence, len(outcome.Collection.Events))
}
