package ward

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

// claude_review_test.go covers the Claude shadow review provider (#865): the
// value seam pins, the launch-spec topology (read-only workspace, a snapshot
// volume carrying exactly {token, CLAUDE.md}, provider-only Anthropic egress, no
// publication credential, no token in the environment), the in-container command
// shape, cross-provider auth-mode and lifecycle rejection, setup-token
// inspection, strict findings collection over the Claude JSON envelope, and
// provider-aware outcome validation.

const testClaudeSetupToken = "sk-ant-oat01-" + "AZaz09-_." + "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// testClaudeReviewSnapshot mirrors testCodexReviewSnapshot but drives the Claude
// provider's observer topology, so the seeded volume carries {CLAUDE.md, token}.
func testClaudeReviewSnapshot(
	t *testing.T,
	cfg CodexReviewConfig,
	runID, volume string,
	tokenBody, instructionBody []byte,
	observerFingerprint string,
) CodexReviewSnapshotObservation {
	t.Helper()
	owner := testOwnershipLabel()
	spec, err := buildReviewSnapshotObserverSpec(claudeReviewProvider{}, cfg, runID, volume, owner)
	if err != nil {
		t.Fatal(err)
	}
	volumeReport := VolumeSummary{
		Name: volume, Labels: slices.Clone(spec.Labels), LabelsObserved: true,
		CreationDate: "2026-08-03T12:00:02Z",
	}
	observerReport := InspectReport{
		ID: spec.Name, ImageReference: spec.Image, Command: slices.Clone(spec.Command),
		WorkingDirectory: "/", State: StateStopped, AllowlistFieldsObserved: true,
		Mounts: slices.Clone(spec.Mounts), Env: []string{fixedContainerPathEnv},
		NetworksObserved: true, Labels: slices.Clone(spec.Labels), LabelsObserved: true,
		CreationDate: observerFingerprint,
	}
	tokenSum := sha256.Sum256(tokenBody)
	instrSum := sha256.Sum256(instructionBody)
	// The proof is nonce-bound and reports the two content digests; the observer
	// script (Claude basenames) proved exactly {CLAUDE.md, token}. The proof keys
	// stay auth=/instr= regardless of the provider's basenames.
	proof := []byte(fmt.Sprintf(
		"nonce=%s\nvalid=valid\nauth=sha256:%x\ninstr=sha256:%x\n", owner.Value, tokenSum, instrSum,
	))
	observation, err := observeReviewSnapshot(
		claudeReviewProvider{}, cfg, runID, volume, owner, owner, volumeReport, observerReport, proof,
	)
	if err != nil {
		t.Fatal(err)
	}
	return observation
}

// testClaudeReview builds a valid Claude review config and spec fixture, mirroring
// testCodexReview but for the setup-token strategy and the Anthropic profile.
func testClaudeReview(t *testing.T) (CodexReviewConfig, CodexReviewSpec) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil { //nolint:gosec // fixture is a private directory, not a file
		t.Fatal(err)
	}
	token := []byte(testClaudeSetupToken)
	instructionBody, instructionBinding, err := exec.ComposeCodexReviewInstructions(
		exec.ReviewHostInstructionInput{}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	digest := digestBody(instructionBody)
	cfg := CodexReviewConfig{
		Model: "claude-opus-4-8", ReasoningEffort: "high",
		InputRoot:         root,
		WorkspaceTarget:   "/workspace/project",
		ProviderEndpoints: []string{"api.anthropic.com:443"},
		ProxyURL:          "http://127.0.0.1:43123",
		ApprovedImage:     "example.test/claude@sha256:" + strings.Repeat("a", 64),
		ObserverImage:     "example.test/exporter@sha256:" + strings.Repeat("c", 64),
		// A setup token has no lifetime floor; the Claude provider does not require
		// one, so leaving it zero exercises the no-expiry configuration path.
		Now: func() time.Time { return codexReviewEpoch },
	}
	shadow := testCodexReviewShadow(
		t, cfg, "review-1", "freeside-review-review-1-agents", "2026-08-03T12:00:01Z",
	)
	snapshot := testClaudeReviewSnapshot(
		t, cfg, "review-1", codexReviewSnapshotVolumeName("review-1"), token, instructionBody, "2026-08-03T12:00:02Z",
	)
	workspace := testCodexReviewWorkspace(
		t, cfg, "review-1", namesFor("review-1").Workspace, "2026-08-03T12:00:03Z",
	)
	req := CodexReviewSpec{
		RunID:                "review-1",
		Image:                "example.test/claude@sha256:" + strings.Repeat("a", 64),
		WorkspaceSourceRunID: "review-1",
		WorkspaceVolume:      namesFor("review-1").Workspace,
		Workspace:            workspace,
		Network:              testCodexReviewNetwork(t, cfg, "review-1"),
		Prompt:               "Review the exact candidate head.",
		Boundary:             CodexReviewFreshStart,
		AuthMode:             CodexAuthSetupToken,
		AuthIdentityID:       "claude-reviewer",
		AuthSnapshot:         writeCodexReviewFile(t, root, "token", token),
		Instructions: VendorInstructions{
			Vendor: domain.AgentVendorClaude, Delivery: domain.VendorInstructionDeliveryAppendFile,
			Present: true, Digest: digest, Body: instructionBody,
		},
		InstructionFile:    writeCodexReviewFile(t, root, "CLAUDE.md", instructionBody),
		InstructionBinding: instructionBinding,
		AgentsShadow:       shadow,
		Snapshot:           snapshot,
	}
	return cfg, req
}

func claudeReviewSourceConfigForTest(
	t *testing.T,
	lifecycle *CodexReviewLifecycle,
	cfg CodexReviewConfig,
	request CodexReviewSpec,
	journal CodexReviewJournal,
) CodexReviewSourceConfig {
	t.Helper()
	leaser, err := NewRuntimeCodexReviewVolumeLeaser(lifecycle.rt)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Journal = journal
	cfg.ProxyURL = ""
	cfg.VolumeLifecycleLeaser = leaser
	cfg.AuthStoreLeaser = &fakeLeaser{volume: request.AuthSnapshot}
	cfg.AuthRefresher = &fakeCodexAuthRefresher{}
	cfg.AuthState = &fakeCodexAuthState{}
	sourceConfig := CodexReviewSourceConfig{
		Lifecycle: lifecycle, Review: cfg, Journal: journal, WorkspaceSizeMB: 64,
		AuthMode: request.AuthMode, AuthIdentityID: request.AuthIdentityID,
		AuthSnapshot: request.AuthSnapshot,
		InstructionArtifacts: testReviewInstructionArtifacts{
			request.InstructionBinding.ResultDigest: request.Instructions.Body,
		},
		CostOwner: "subscription:owner", Now: func() time.Time { return codexReviewEpoch },
	}
	sourceConfig.ConfigurationDigest, err = ClaudeReviewConfigurationDigest(
		cfg, sourceConfig.WorkspaceSizeMB, sourceConfig.AuthMode,
		sourceConfig.AuthIdentityID, sourceConfig.CostOwner,
	)
	if err != nil {
		t.Fatal(err)
	}
	return sourceConfig
}

func TestClaudeReviewProviderConstants(t *testing.T) {
	p := claudeReviewProvider{}
	strs := []struct{ name, got, want string }{
		{"sourceLabel", p.sourceLabel(), "claude_local"},
		{"providerLabel", p.providerLabel(), "anthropic"},
		{"topologyVersion", p.topologyVersion(), "claude_review_read_only_v1"},
		{"completionEvidenceVersion", p.completionEvidenceVersion(), "claude-review-completion-v1"},
		{"resultEvidenceVersion", p.resultEvidenceVersion(), "claude-review-result-v1"},
		{"configurationVersion", p.configurationVersion(), "claude-review-configuration-v1"},
		{"promptProtocol", p.promptProtocol(), "claude-production-review-prompt-v1"},
		{"reviewContainerSuffix", p.reviewContainerSuffix(), "-claude"},
		{"homeTarget", p.homeTarget(), claudeReviewHome},
		{"configHomeTarget", p.configHomeTarget(), claudeReviewConfigTarget},
		{"snapshotCredentialName", p.snapshotCredentialName(), "token"},
		{"snapshotInstructionName", p.snapshotInstructionName(), "CLAUDE.md"},
	}
	for _, tc := range strs {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
	if p.vendor() != domain.AgentVendorClaude {
		t.Errorf("vendor = %q, want claude", p.vendor())
	}
	if p.legacyTopologyVersions() != nil {
		t.Errorf("legacyTopologyVersions = %v, want nil", p.legacyTopologyVersions())
	}
	if p.requiresExpiringCredential() {
		t.Error("requiresExpiringCredential = true, want false for the setup-token strategy")
	}
	if !p.acceptsAuthMode(CodexAuthSetupToken) {
		t.Error("acceptsAuthMode(setup_token) = false, want true")
	}
	if p.acceptsAuthMode(CodexAuthSubscription) || p.acceptsAuthMode(CodexAuthAPIKey) {
		t.Error("Claude provider accepted a Codex auth mode")
	}
	// The Claude topology and evidence namespaces must be disjoint from Codex so a
	// binding or result can never be validated against the wrong provider.
	c := codexReviewProvider{}
	if p.topologyVersion() == c.topologyVersion() || p.resultEvidenceVersion() == c.resultEvidenceVersion() ||
		p.reviewContainerSuffix() == c.reviewContainerSuffix() || p.providerLabel() == c.providerLabel() {
		t.Error("Claude provider shares a namespace value with Codex")
	}
}

func TestClaudeReviewBuildAgentSpecTopology(t *testing.T) {
	cfg, req := testClaudeReview(t)
	spec, binding, err := buildReviewAgentSpec(claudeReviewProvider{}, cfg, req)
	if err != nil {
		t.Fatalf("buildReviewAgentSpec: %v", err)
	}
	if spec.Name != "freeside-review-review-1-claude" {
		t.Errorf("review container = %q, want the -claude suffix", spec.Name)
	}
	if len(spec.Mounts) < 2 {
		t.Fatalf("spec has %d mounts, want at least workspace + snapshot", len(spec.Mounts))
	}
	if !spec.Mounts[0].ReadOnly || spec.Mounts[0].Target != cfg.WorkspaceTarget {
		t.Errorf("workspace mount = %+v, want read-only at %q", spec.Mounts[0], cfg.WorkspaceTarget)
	}
	if !spec.Mounts[1].ReadOnly || spec.Mounts[1].Target != codexReviewSnapshotTarget {
		t.Errorf("snapshot mount = %+v, want read-only at %q", spec.Mounts[1], codexReviewSnapshotTarget)
	}
	if binding.TopologyVersion != claudeReviewTopologyVersion {
		t.Errorf("topology = %q, want %q", binding.TopologyVersion, claudeReviewTopologyVersion)
	}
	if binding.PublicationCredentials {
		t.Error("binding carries publication credentials")
	}
	if binding.RefreshEndpointReachable {
		t.Error("binding marks the refresh endpoint reachable")
	}
	if !slices.Equal(binding.ProviderEndpoints, []string{"api.anthropic.com:443"}) {
		t.Errorf("provider endpoints = %v, want the Anthropic endpoint", binding.ProviderEndpoints)
	}
	if binding.HomeTarget != claudeReviewHome || binding.CodexHomeTarget != claudeReviewConfigTarget {
		t.Errorf("home targets = (%q, %q), want Claude review targets", binding.HomeTarget, binding.CodexHomeTarget)
	}
	if binding.AccessTokenExpiresAt != nil {
		t.Errorf("binding carries an access-token expiry %v, want nil for setup-token", binding.AccessTokenExpiresAt)
	}
	// The setup token must never appear in the persisted, digested launcher
	// environment: it reaches the CLI only from the read-only snapshot file.
	for _, e := range spec.Env {
		if strings.Contains(e, testClaudeSetupToken) || strings.HasPrefix(e, "CLAUDE_CODE_OAUTH_TOKEN=") {
			t.Errorf("launcher environment leaks the token: %q", e)
		}
	}
	// The final reconstruction accepts the exact spec it built.
	if err := validateReviewAgentSpec(claudeReviewProvider{}, cfg, req, spec, binding); err != nil {
		t.Fatalf("validateReviewAgentSpec rejected a self-built Claude spec: %v", err)
	}
}

func TestClaudeReviewContainerNameIsSingleSourced(t *testing.T) {
	cfg, req := testClaudeReview(t)
	spec, _, err := buildReviewAgentSpec(claudeReviewProvider{}, cfg, req)
	if err != nil {
		t.Fatalf("buildReviewAgentSpec: %v", err)
	}
	want := "freeside-review-review-1-claude"
	// The spec name (which the launch copies into binding.ReviewContainer), the
	// naming helper, and the runtime resource enumeration must all agree on the
	// provider-specific container name. The lifecycle builds its intent and
	// cleanup targets from reviewContainerName; if any diverged, a Claude launch
	// would journal a -codex intent against a -claude container
	// (ErrParentKeyMismatch) and leak the credential container on cleanup.
	for name, got := range map[string]string{
		"spec.Name":                     spec.Name,
		"reviewContainerName":           reviewContainerName(claudeReviewProvider{}, "review-1"),
		"reviewNames().reviewContainer": reviewNames(claudeReviewProvider{}, "review-1").reviewContainer,
	} {
		if got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if !slices.Contains(reviewRuntimeResourceNamesFor(claudeReviewProvider{}, "review-1").Containers, want) {
		t.Errorf("runtime resource enumeration omits the Claude review container %q", want)
	}
	if slices.Contains(reviewRuntimeResourceNamesFor(claudeReviewProvider{}, "review-1").Containers, "freeside-review-review-1-codex") {
		t.Error("Claude runtime resource enumeration still names the Codex container")
	}
}

func TestClaudeReviewCommandShape(t *testing.T) {
	cmd := claudeReviewCommand("/workspace/project", "claude-opus-4-8", "high", "the prompt")
	if len(cmd) != 5 || cmd[0] != "sh" || cmd[1] != "-c" {
		t.Fatalf("command shell wrapper = %v", cmd[:min(len(cmd), 2)])
	}
	// The prompt is a positional argument, never interpolated into the program.
	if cmd[4] != "the prompt" {
		t.Errorf("prompt positional = %q, want the raw prompt", cmd[4])
	}
	if strings.Contains(cmd[2], "the prompt") {
		t.Error("prompt was interpolated into the shell program")
	}
	program := cmd[2]
	for _, want := range []string{
		"--output-format json",
		"--json-schema",
		"--effort",
		"--model",
		"--no-session-persistence",
		"--append-system-prompt-file",
		"--safe-mode",
		"--allowedTools Read Grep Glob",
		"claude -p \"$1\"",
		"CLAUDE_CODE_OAUTH_TOKEN=\"$token\"",
		"cat " + shellQuote(claudeReviewSnapshotTokenSource),
		claudeReviewSnapshotInstrSource,
		reviewFindingsJSONSchema,
		codexReviewResultPath,
		codexReviewEventsPath,
		codexReviewStatusPath,
	} {
		if !strings.Contains(program, want) {
			t.Errorf("command missing %q", want)
		}
	}
	// The findings schema is the shared provider-neutral literal, identical to the
	// Codex command's.
	if !strings.Contains(codexReviewCommand("/w", "m", "e", "p")[2], reviewFindingsJSONSchema) {
		t.Error("Codex and Claude do not share the findings schema literal")
	}
}

func TestClaudeReviewRejectsCrossProviderAuthMode(t *testing.T) {
	// A Codex subscription request against the Claude provider is refused.
	codexCfg, codexReq := testCodexReview(t)
	if _, _, err := buildReviewAgentSpec(claudeReviewProvider{}, codexCfg, codexReq); !errors.Is(err, ErrInvalidCodexReviewSpec) {
		t.Errorf("Claude provider accepted a Codex subscription request: %v", err)
	}
	// A Claude setup-token request against the Codex provider is refused.
	claudeCfg, claudeReq := testClaudeReview(t)
	if _, _, err := buildReviewAgentSpec(codexReviewProvider{}, claudeCfg, claudeReq); !errors.Is(err, ErrInvalidCodexReviewSpec) {
		t.Errorf("Codex provider accepted a Claude setup-token request: %v", err)
	}
}

func TestReviewSourceRejectsLifecycleProviderMismatch(t *testing.T) {
	fx := newHandoffFixture(t)
	codexLifecycle := fx.codexReviewLifecycle(t)
	claudeLifecycle, err := NewClaudeReviewLifecycle(fx.rt, fx.cfg, nil)
	if err != nil {
		t.Fatal(err)
	}

	claudeCfg, claudeReq := testClaudeReview(t)
	journal := &fakeCodexReviewJournal{}
	claudeSourceCfg := claudeReviewSourceConfigForTest(
		t, codexLifecycle, claudeCfg, claudeReq, journal,
	)
	if _, err := NewClaudeReviewSource(claudeSourceCfg); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("NewClaudeReviewSource(Codex lifecycle) = %v, want ErrInvalidConfig", err)
	}

	codexCfg, codexReq := testCodexReview(t)
	codexSourceCfg := codexReviewSourceConfigForTest(
		t, claudeLifecycle, codexCfg, codexReq, &fakeCodexReviewJournal{},
	)
	if _, err := NewCodexReviewSource(codexSourceCfg); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("NewCodexReviewSource(Claude lifecycle) = %v, want ErrInvalidConfig", err)
	}

	claudeSourceCfg.Lifecycle = claudeLifecycle
	if _, err := NewClaudeReviewSource(claudeSourceCfg); err != nil {
		t.Fatalf("NewClaudeReviewSource(Claude lifecycle) = %v, want nil", err)
	}
}

func TestCodexReviewRecoveryDispatchesStartedClaudeIntent(t *testing.T) {
	ctx := t.Context()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	claudeLifecycle, err := NewClaudeReviewLifecycle(fx.rt, fx.cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg, requestSpec := testClaudeReview(t)
	journal := &fakeCodexReviewJournal{}
	sourceConfig := claudeReviewSourceConfigForTest(
		t, claudeLifecycle, cfg, requestSpec, journal,
	)
	source, err := NewClaudeReviewSource(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-claude-restart-1")
	request := exec.ReviewRequest{
		RunID: "run-claude-restart", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(),
		Instructions: testReviewInstructionBinding(), RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	if err := source.RequestReview(ctx, id, request); err != nil {
		t.Fatal(err)
	}
	source.mu.Lock()
	if err := source.launches[id].Close(); err != nil {
		source.mu.Unlock()
		t.Fatal(err)
	}
	delete(source.launches, id)
	source.mu.Unlock()

	codexLifecycle := fx.codexReviewLifecycle(t)
	recovery, err := NewCodexReviewRecovery(
		codexLifecycle, journal, sourceConfig.Review.VolumeLifecycleLeaser, cfg.InputRoot,
	)
	if err != nil {
		t.Fatal(err)
	}
	trustedBinding := journal.binding
	journal.binding.TopologyVersion = codexReviewTopologyVersion
	if err := recovery.Reconcile(ctx); !errors.Is(err, ErrConformance) {
		t.Fatalf("recovery with a provider-rewritten binding = %v, want conformance refusal", err)
	}
	if _, exists := fx.rt.ctrs[reviewContainerName(claudeReviewProvider{}, string(id))]; !exists {
		t.Fatal("provider-rewritten binding redirected cleanup at the Claude container")
	}
	if _, _, err := journal.GetCodexReviewOutcome(ctx, string(id)); !errors.Is(err, ErrCodexReviewOutcomeNotFound) {
		t.Fatalf("provider-rewritten binding persisted an outcome before provider authentication: %v", err)
	}
	journal.binding = trustedBinding
	if err := recovery.Reconcile(ctx); err != nil {
		t.Fatalf("Codex-composed recovery of a started Claude review = %v", err)
	}
	if journal.intent == nil || journal.intent.State != CodexReviewIntentClosed {
		t.Fatalf("recovered Claude intent = %+v, want closed", journal.intent)
	}
	if _, ready, err := journal.GetCodexReviewOutcome(ctx, string(id)); err != nil || !ready {
		t.Fatalf("recovered Claude outcome = ready %v, %v", ready, err)
	}
	if _, exists := fx.rt.ctrs[reviewContainerName(claudeReviewProvider{}, string(id))]; exists {
		t.Fatal("Codex-composed recovery left the credential-bearing Claude container behind")
	}
}

func TestClaudeReviewInspectSetupToken(t *testing.T) {
	p := claudeReviewProvider{}
	// A trailing newline is trimmed; the container body is the bare token.
	body, expires, err := p.inspectAgentAuthSnapshot(CodexAuthSetupToken, []byte(testClaudeSetupToken+"\n"))
	if err != nil {
		t.Fatalf("inspectAgentAuthSnapshot: %v", err)
	}
	if expires != nil {
		t.Errorf("expires = %v, want nil for a setup token", expires)
	}
	if string(body) != testClaudeSetupToken {
		t.Errorf("token = %q, want the trimmed value", body)
	}
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"empty", []byte("")},
		{"only newline", []byte("\n")},
		{"control character", []byte("abc\tdef")},
		{"embedded newline", []byte("abc\ndef")},
		{"del character", []byte("abc\x7f")},
	} {
		if _, _, err := p.inspectAgentAuthSnapshot(CodexAuthSetupToken, tc.body); err == nil {
			t.Errorf("%s: inspectAgentAuthSnapshot accepted an invalid token", tc.name)
		}
	}
	// The wrong auth mode is refused.
	if _, _, err := p.inspectAgentAuthSnapshot(CodexAuthSubscription, []byte(testClaudeSetupToken)); err == nil {
		t.Error("Claude provider inspected a subscription auth mode")
	}
}

// TestClaudeReviewSourceMaterializesClaudeVendor covers the source's
// RequestReview materialization path, which the topology tests bypass: the
// reconstructed instructions must carry the source's own vendor, or a Claude
// source's every request is rejected at the launch-shape vendor gate.
func TestClaudeReviewSourceMaterializesClaudeVendor(t *testing.T) {
	host := []byte("operator review rules\n")
	bundle, binding, err := exec.ComposeCodexReviewInstructions(
		exec.ReviewHostInstructionInput{Present: true, Body: host}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil { //nolint:gosec // fixture is a private directory
		t.Fatal(err)
	}
	source := &CodexReviewSource{cfg: CodexReviewSourceConfig{
		provider:             claudeReviewProvider{},
		Review:               CodexReviewConfig{InputRoot: root},
		InstructionArtifacts: testReviewInstructionArtifacts{*binding.HostDigest: host, binding.ResultDigest: bundle},
	}}
	instructions, _, err := source.materializeReviewInstructions(t.Context(), "review-1", binding)
	if err != nil {
		t.Fatalf("materializeReviewInstructions: %v", err)
	}
	if instructions.Vendor != domain.AgentVendorClaude {
		t.Errorf("materialized instruction vendor = %q, want claude (Codex-hardcoded vendor rejects every Claude request)", instructions.Vendor)
	}
	if !instructions.Present || !bytes.Equal(instructions.Body, bundle) {
		t.Errorf("materialized instructions lost the reconstructed bundle")
	}
}

func testClaudeReviewSourceForCollection() *CodexReviewSource {
	return &CodexReviewSource{cfg: CodexReviewSourceConfig{
		provider:            claudeReviewProvider{},
		Now:                 func() time.Time { return codexReviewEpoch },
		Review:              CodexReviewConfig{Model: "claude-opus-4-8", ReasoningEffort: "high"},
		ConfigurationDigest: domain.Digest(contentaddr.Sum([]byte("claude-config"))),
		CostOwner:           "claude-reviewer",
	}}
}

func TestClaudeReviewCollectionEnvelope(t *testing.T) {
	source := testClaudeReviewSourceForCollection()
	id := domain.InvocationID("review-run-1-1")
	req := exec.ReviewRequest{RunID: "run-1", BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40)}

	valid := source.normalizeCollection(id, req, CodexReviewCollection{
		Result: []byte(`{"findings":[{"severity":"P1","location":{"path":"a.go","start_line":3,"end_line":5},"explanation":"x"}]}`),
	})
	if valid.Result == nil {
		t.Fatalf("valid Claude envelope produced no result: %+v", valid)
	}
	if valid.Result.Provider != "anthropic" {
		t.Errorf("result provider = %q, want anthropic", valid.Result.Provider)
	}
	if len(valid.Result.Findings) != 1 || valid.Result.Findings[0].Source != "claude_local" ||
		valid.Result.Findings[0].Severity != domain.FindingSeverity("P1") {
		t.Errorf("finding = %+v, want one claude_local P1", valid.Result.Findings)
	}

	for _, tc := range []struct {
		name   string
		result string
	}{
		{"out-of-domain severity", `{"findings":[{"severity":"P9","location":{"path":"a.go","start_line":1,"end_line":1},"explanation":"x"}]}`},
		{"non-concrete range", `{"findings":[{"severity":"P1","location":{"path":"a.go","start_line":0,"end_line":0},"explanation":"x"}]}`},
		{"malformed json", `{"findings":`},
	} {
		outcome := source.normalizeCollection(id, req, CodexReviewCollection{Result: []byte(tc.result)})
		if outcome.Result != nil || outcome.FailureClass != domain.ReviewFailureContradiction {
			t.Errorf("%s: outcome = %+v, want a contradiction failure", tc.name, outcome)
		}
	}

	for _, tc := range []struct {
		name   string
		events string
		want   domain.ReviewFailureClass
	}{
		{
			"quota", `{"type":"result","is_error":true,"result":"rate limit exceeded"}`,
			domain.ReviewFailureQuota,
		},
		{
			"authentication", `{"type":"result","is_error":true,"result":"authentication failed"}`,
			domain.ReviewFailureConfiguration,
		},
		{
			"non-error envelope is not classification authority",
			`{"type":"result","is_error":false,"result":"quota exceeded"}`,
			domain.ReviewFailureTransient,
		},
	} {
		t.Run("terminal "+tc.name, func(t *testing.T) {
			failed := source.normalizeCollection(id, req, CodexReviewCollection{
				Events: []byte(tc.events + "\n"), ExitStatus: 1,
			})
			if failed.Result != nil || failed.FailureClass != tc.want {
				t.Errorf("terminal envelope = %+v, want %s failure", failed, tc.want)
			}
		})
	}
}

func TestClaudeReviewOutcomeValidationProviderAware(t *testing.T) {
	source := testClaudeReviewSourceForCollection()
	id := domain.InvocationID("review-run-1-1")
	req := exec.ReviewRequest{
		RunID: "run-1", BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40),
		Instructions: exec.ReviewInstructionBinding{ResultDigest: domain.Digest(contentaddr.Sum([]byte("instr")))},
	}
	outcome := source.normalizeCollection(id, req, CodexReviewCollection{
		Result: []byte(`{"findings":[{"severity":"P2","location":{"path":"a.go","start_line":1,"end_line":1},"explanation":"y"}]}`),
	})
	if outcome.Result == nil {
		t.Fatalf("expected a Claude result: %+v", outcome)
	}
	// Shape validation is provider-agnostic and passes.
	if err := outcome.Validate(); err != nil {
		t.Fatalf("Claude outcome failed shape validation: %v", err)
	}
	// The provider-aware gate accepts the Claude result under the trusted Claude
	// provider (matching label + Claude-version evidence).
	if err := outcome.verifyCompletionEvidence(claudeReviewProvider{}); err != nil {
		t.Fatalf("Claude outcome failed provider-aware validation: %v", err)
	}
	// The trusted provider, not the decoded label, selects the validator: the same
	// Claude row re-gated under the Codex provider is rejected on the label match.
	if err := outcome.verifyCompletionEvidence(codexReviewProvider{}); !errors.Is(err, domain.ErrInvalidReviewCompletionEvidence) {
		t.Errorf("Claude result accepted under the Codex trusted provider: %v", err)
	}
	// A rewritten row that flips the label to Codex cannot self-validate against
	// the trusted Claude provider.
	asCodex := outcome
	relabeled := *outcome.Result
	relabeled.Provider = "openai"
	asCodex.Result = &relabeled
	if err := asCodex.verifyCompletionEvidence(claudeReviewProvider{}); !errors.Is(err, domain.ErrInvalidReviewCompletionEvidence) {
		t.Errorf("relabeled row self-validated: %v", err)
	}
	// An unknown provider label fails closed under any trusted provider.
	asUnknown := outcome
	unknown := *outcome.Result
	unknown.Provider = "mystery"
	asUnknown.Result = &unknown
	if err := asUnknown.verifyCompletionEvidence(claudeReviewProvider{}); !errors.Is(err, domain.ErrInvalidReviewCompletionEvidence) {
		t.Errorf("unknown provider label validated: %v", err)
	}
}

func TestClaudeReviewBindingRefusesV2LegacyTopology(t *testing.T) {
	cfg, req := testClaudeReview(t)
	_, binding, err := buildReviewAgentSpec(claudeReviewProvider{}, cfg, req)
	if err != nil {
		t.Fatalf("buildReviewAgentSpec: %v", err)
	}
	// The teardown path enables legacy-topology tolerance (allowLegacy=true). A
	// well-formed Claude binding validates under it against the current Claude
	// topology; requirePreStart=false skips the launch-time container ownership
	// evidence a prepared binding does not yet carry.
	if err := binding.validate(claudeReviewProvider{}, false, true); err != nil {
		t.Fatalf("a well-formed Claude binding failed teardown-shape validation: %v", err)
	}
	// The Codex v2 legacy topology is never tolerated for a Claude binding: the
	// Claude provider carries no legacy versions, so even the teardown path
	// refuses it.
	v2 := binding
	v2.TopologyVersion = codexReviewTopologyVersionV2
	if err := v2.validate(claudeReviewProvider{}, false, true); err == nil {
		t.Error("Claude binding tolerated the Codex v2 legacy topology under teardown")
	}
	// Sanity: the same v2 tolerance the Claude provider refuses is granted to the
	// Codex provider for its own bindings, so the refusal is provider-scoped, not
	// a blanket rejection of the constant.
	if !slices.Contains(codexReviewProvider{}.legacyTopologyVersions(), codexReviewTopologyVersionV2) {
		t.Error("Codex provider no longer lists its v2 legacy topology")
	}
}
