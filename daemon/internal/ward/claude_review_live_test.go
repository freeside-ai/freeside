package ward

// Host-gated live regression for the Claude shadow review runtime (#865). It
// proves on the reference runtime (Apple container 1.1.0) what the scripted fake
// cannot: that the setup token and the review instruction are delivered on one
// read-only named snapshot volume as exactly {CLAUDE.md, token}, that the
// networkless observer proves those two files, that the review container is the
// pinned agent-claude image with a read-only workspace, provider-only Anthropic
// egress, and no publication credential, and that the durable binding carries
// the Claude topology and the -claude container name across the reconstruction
// start boundary. The setup token is exercised end-to-end through the credential
// seeding and observation path.
//
// Opt-in and CI-blind, following live_test.go: it needs macOS, the `container`
// CLI, `container system start`, the pinned exporter/observer image, the pinned
// agent-claude image, and a real setup token. It is skipped by default like
// every other FREESIDE_WARD_LIVE_TEST suite.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

func TestLiveClaudeReviewLifecycleCrossesReconstructionStartBoundary(t *testing.T) {
	if os.Getenv("FREESIDE_WARD_LIVE_TEST") != "1" {
		t.Skip("live claude-review lifecycle test skipped: set FREESIDE_WARD_LIVE_TEST=1 (requires macOS, Apple container 1.1.0, `container system start`, FREESIDE_WARD_EXPORTER_IMAGE, FREESIDE_WARD_CLAUDE_AGENT_IMAGE, and CLAUDE_CODE_OAUTH_TOKEN)")
	}
	reviewImage := os.Getenv("FREESIDE_WARD_CLAUDE_AGENT_IMAGE")
	if reviewImage == "" {
		t.Skip("live claude-review lifecycle test skipped: set FREESIDE_WARD_CLAUDE_AGENT_IMAGE to the digest-pinned agent-claude image")
	}
	setupToken := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN")
	if setupToken == "" {
		t.Skip("live claude-review lifecycle test skipped: set CLAUDE_CODE_OAUTH_TOKEN to a valid setup token (keychain hint: freeside-claude-setup-token)")
	}
	bin, err := osexec.LookPath("container")
	if err != nil {
		t.Fatalf("container CLI not on PATH: %v", err)
	}
	if out, pullErr := osexec.Command(bin, "image", "pull", liveImage).CombinedOutput(); pullErr != nil { //nolint:gosec // fixed args, resolved CLI path
		t.Logf("image pull (continuing; may be cached): %v: %s", pullErr, out)
	}
	exporterImage := liveExporterImage(t)
	requireExporterGit(t, bin, exporterImage)

	ctx := context.Background()
	rt := NewCLIRuntime(bin)
	runID := fmt.Sprintf("liveclaudereview-%d", time.Now().Unix())
	names := reviewNames(claudeReviewProvider{}, runID)
	workspace := namesFor(runID).Workspace
	t.Cleanup(func() {
		for _, name := range []string{
			names.workspaceObserver, names.shadowInitializer, names.shadowObserver,
			names.snapshotSeeder, names.snapshotObserver, names.reviewContainer,
		} {
			_ = rt.StopContainer(ctx, name)
			_ = rt.DeleteContainer(ctx, name)
		}
		_ = rt.DeleteNetwork(ctx, names.network)
		_ = rt.DeleteVolume(ctx, names.shadowVolume)
		_ = rt.DeleteVolume(ctx, names.snapshotVolume)
		_ = rt.DeleteVolume(ctx, workspace)
	})

	root := t.TempDir()
	checkout := initLiveSeedCheckout(t, root)
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("review fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate := commitLiveSeedCheckout(t, checkout)
	journal := &fakeCodexReviewJournal{}
	backendConfig := testConfig()
	backendConfig.ExporterImage = exporterImage
	backendConfig.SeedRoot = root
	backendConfig.PollInterval = 500 * time.Millisecond
	backendConfig.SeedTimeout = 2 * time.Minute
	backend, err := NewClaudeReviewLifecycle(rt, backendConfig, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.PrepareCodexReviewWorkspace(
		ctx, journal, runID, checkout, candidate, 64,
	); err != nil {
		t.Fatalf("PrepareCodexReviewWorkspace: %v", err)
	}

	// The credential input root is a private directory holding the raw setup
	// token and the review instruction; the runtime seeds them onto the read-only
	// snapshot volume as {token, CLAUDE.md}.
	inputRoot := t.TempDir()
	if err := os.Chmod(inputRoot, 0o700); err != nil { //nolint:gosec // fixture is a private directory
		t.Fatal(err)
	}
	instructionBody, instructionBinding, err := exec.ComposeCodexReviewInstructions(exec.ReviewHostInstructionInput{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The token is written directly (not through the shared test helper) so the
	// environment-sourced value never taints that helper's path sink.
	tokenPath := filepath.Join(inputRoot, "token")
	if err := os.WriteFile(tokenPath, []byte(setupToken), 0o400); err != nil { //nolint:gosec // fixed basename under a private t.TempDir() input root
		t.Fatal(err)
	}
	instructionPath := writeCodexReviewFile(t, inputRoot, "CLAUDE.md", instructionBody)

	reviewConfig := CodexReviewConfig{
		Model: "claude-opus-4-8", ReasoningEffort: "high",
		InputRoot:         inputRoot,
		WorkspaceTarget:   "/workspace/project",
		ProviderEndpoints: []string{"api.anthropic.com:443"},
		ProxyURL:          "",
		ApprovedImage:     reviewImage,
		ObserverImage:     exporterImage,
		Journal:           journal,
		Now:               func() time.Time { return time.Now().UTC() },
	}
	reviewConfig.VolumeLifecycleLeaser, err = NewRuntimeCodexReviewVolumeLeaser(rt)
	if err != nil {
		t.Fatal(err)
	}
	launchSpec := CodexReviewLaunchSpec{
		RunID: runID, WorkflowRunID: domain.RunID(runID),
		Image: reviewImage, WorkspaceSourceRunID: runID,
		WorkspaceVolume: workspace, ExpectedHead: candidate.BaseSHA,
		Prompt: "Review the exact candidate head.", Boundary: CodexReviewFreshStart,
		AuthMode: CodexAuthSetupToken, AuthIdentityID: "claude-reviewer",
		AuthSnapshot: tokenPath, InstructionFile: instructionPath,
		Instructions: VendorInstructions{
			Vendor: domain.AgentVendorClaude, Delivery: domain.VendorInstructionDeliveryAppendFile,
			Present: true, Digest: digestBody(instructionBody), Body: instructionBody,
		},
		InstructionBinding: instructionBinding,
	}
	launch, err := backend.CodexReview(ctx, reviewConfig, launchSpec)
	if err != nil {
		t.Fatalf("CodexReview through final reconstruction and Start: %v", err)
	}
	if journal.intent == nil || journal.intent.State != CodexReviewIntentStarted {
		t.Fatalf("intent = %+v, want started handoff", journal.intent)
	}
	if launch.Binding.TopologyVersion != claudeReviewTopologyVersion {
		t.Errorf("durable binding topology = %q, want %q", launch.Binding.TopologyVersion, claudeReviewTopologyVersion)
	}
	if launch.Binding.ReviewContainer != names.reviewContainer {
		t.Errorf("durable binding review container = %q, want %q", launch.Binding.ReviewContainer, names.reviewContainer)
	}
	if launch.Binding.PublicationCredentials || launch.Binding.RefreshEndpointReachable {
		t.Errorf("durable binding relaxed a credential-isolation invariant: %+v", launch.Binding)
	}
	if !slices.Equal(launch.Binding.ProviderEndpoints, []string{"api.anthropic.com:443"}) {
		t.Errorf("durable binding endpoints = %v, want the Anthropic endpoint", launch.Binding.ProviderEndpoints)
	}
	if launch.Binding.AccessTokenExpiresAt != nil {
		t.Errorf("durable binding carries an access-token expiry, want nil for setup-token")
	}
	if err := launch.Close(); err != nil {
		t.Fatalf("close review proxy: %v", err)
	}

	pollCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		state, inspectErr := backend.InspectCodexReview(pollCtx, reviewConfig, runID)
		if inspectErr != nil {
			t.Fatalf("InspectCodexReview: %v", inspectErr)
		}
		if state == StateStopped {
			break
		}
		if state != StateRunning {
			t.Fatalf("Claude review state = %q, want running or stopped", state)
		}
		select {
		case <-pollCtx.Done():
			t.Fatalf("Claude review did not stop within five minutes: %v", pollCtx.Err())
		case <-ticker.C:
		}
	}
	collection, err := backend.CollectCodexReview(ctx, reviewConfig, runID)
	if err != nil {
		t.Fatalf("CollectCodexReview: %v", err)
	}
	if collection.ExitStatus != 0 {
		t.Fatalf(
			"Claude review exited with status %d; collected %d event bytes",
			collection.ExitStatus, len(collection.Events),
		)
	}
	if len(bytes.TrimSpace(collection.Result)) == 0 {
		t.Fatal("Claude review returned an empty structured-output result")
	}
	outcome := testClaudeReviewSourceForCollection().normalizeCollection(
		domain.InvocationID(runID),
		exec.ReviewRequest{
			RunID: domain.RunID(runID), BaseSHA: candidate.BaseSHA, HeadSHA: candidate.BaseSHA,
			Instructions: instructionBinding,
		},
		collection,
	)
	if outcome.Result == nil {
		t.Fatalf("Claude structured-output envelope did not normalize: %+v", outcome)
	}
	if err := outcome.Validate(); err != nil {
		t.Fatalf("Claude structured-output envelope failed production validation: %v", err)
	}
	if err := outcome.verifyCompletionEvidence(claudeReviewProvider{}); err != nil {
		t.Fatalf("Claude structured-output envelope failed provider evidence validation: %v", err)
	}
	for _, finding := range outcome.Result.Findings {
		if finding.Source != "claude_local" || !slices.Contains(domain.AllFindingSeverities, finding.Severity) {
			t.Errorf("normalized finding = %+v, want claude_local severity in P0-P3", finding)
		}
	}

	if err := backend.AbortCodexReview(ctx, reviewConfig, runID); err != nil {
		t.Fatalf("AbortCodexReview: %v", err)
	}
	if journal.intent.State != CodexReviewIntentClosed {
		t.Fatalf("intent state after teardown = %q, want closed", journal.intent.State)
	}
	containers, err := rt.ListContainers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, container := range containers {
		if container.ID == names.reviewContainer || container.ID == names.snapshotObserver {
			t.Errorf("container %q survived live Claude review teardown", container.ID)
		}
	}
}
