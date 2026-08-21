package ward

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// review_equivalence_test.go is the behavior-preserving proof for the #872
// provider-neutral extraction. The refactor routed the review runtime's
// vendor-varying decisions through reviewProvider / credentialStrategy; this
// harness reconstructs the pre-#872 pure implementations from base commit
// 6bf2c8b854d6470e77ae5b93e6113530e35c3d90 and measures, over a fuzzed corpus,
// that the Codex provider path is decision-for-decision identical. It covers
// the two trust-boundary surfaces the acceptance criteria name: (a) collection
// normalization / strict-JSON decode, and (b) launch-spec mount + command + env
// derivation. A diff-read asserts equivalence; this harness measures it.

// These literals are the exact hardcoded constants the base commit used at the
// points the refactor replaced with provider calls. codexReviewProvider must
// reproduce each one, or a production review's evidence/labels/topology would
// silently change.
const (
	baseSourceLabel               = "codex_local"
	baseProviderLabel             = "openai"
	baseTopologyVersion           = "codex_review_read_only_v3"
	baseCompletionEvidenceVersion = "codex-review-completion-v1"
	baseResultEvidenceVersion     = "codex-review-result-v3"
	baseConfigurationVersion      = "codex-review-configuration-v3"
	basePromptProtocol            = "codex-production-review-prompt-v3"
)

// TestReviewProviderConstantsMatchBase pins the Codex provider's value seam to
// the base commit's hardcoded constants. This is the pure-value half of the
// equivalence proof: the label, version-tag, and prompt-protocol substitutions
// are behavior-preserving iff each returns exactly the pre-#872 literal.
func TestReviewProviderConstantsMatchBase(t *testing.T) {
	p := codexReviewProvider{}
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"sourceLabel", p.sourceLabel(), baseSourceLabel},
		{"providerLabel", p.providerLabel(), baseProviderLabel},
		{"topologyVersion", p.topologyVersion(), baseTopologyVersion},
		{"completionEvidenceVersion", p.completionEvidenceVersion(), baseCompletionEvidenceVersion},
		{"resultEvidenceVersion", p.resultEvidenceVersion(), baseResultEvidenceVersion},
		{"configurationVersion", p.configurationVersion(), baseConfigurationVersion},
		{"promptProtocol", p.promptProtocol(), basePromptProtocol},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %q, base commit had %q", tc.name, tc.got, tc.want)
		}
	}
	// The topology-version constant the provider returns must also equal the
	// package constant the base binding stamped.
	if p.topologyVersion() != codexReviewTopologyVersion {
		t.Errorf("provider topologyVersion %q != package constant %q",
			p.topologyVersion(), codexReviewTopologyVersion)
	}
}

// oldNormalizeCollection is the base-commit (6bf2c8b8) body of
// CodexReviewSource.normalizeCollection, reconstructed verbatim with its inline
// literals, as the independent reference for the equivalence comparison. It
// calls the unchanged package helpers.
func oldNormalizeCollection(
	s *CodexReviewSource,
	id domain.InvocationID, req exec.ReviewRequest, collection CodexReviewCollection,
) CodexReviewSourceOutcome {
	evidenceBytes := fmt.Appendf(nil, "codex-review-completion-v1:%d:", len(collection.Events))
	evidenceBytes = append(evidenceBytes, collection.Events...)
	evidenceBytes = fmt.Appendf(evidenceBytes, ":%d:", len(collection.Result))
	evidenceBytes = append(evidenceBytes, collection.Result...)
	evidenceBytes = fmt.Appendf(evidenceBytes, ":%d", collection.ExitStatus)
	collectionEvidence := domain.Digest(contentaddr.Sum(evidenceBytes))
	if collection.ExitStatus != 0 {
		class, terminalMessage := classifyCodexTerminalFailure(collection.Events)
		failure := fmt.Sprintf("Codex review exited with status %d", collection.ExitStatus)
		if codexRefreshAttemptFailure([]byte(terminalMessage)) {
			failure = "Codex review attempted an in-container credential refresh"
		}
		return CodexReviewSourceOutcome{
			InvocationID: id,
			FailureClass: class,
			Failure:      failure,
		}
	}
	var raw struct {
		Findings *[]struct {
			Severity string `json:"severity"`
			Location *struct {
				Path      string `json:"path"`
				StartLine int    `json:"start_line"`
				EndLine   int    `json:"end_line"`
			} `json:"location"`
			Explanation string `json:"explanation"`
		} `json:"findings"`
	}
	if err := RejectDuplicateJSONKeys(collection.Result); err != nil {
		return CodexReviewSourceOutcome{
			InvocationID: id,
			FailureClass: domain.ReviewFailureContradiction,
			Failure:      "Codex review returned malformed structured output",
		}
	}
	if err := strictjson.Decode(
		collection.Result, &raw, strictjson.TolerateInvalidUTF8, strictjson.NoLimit,
	); err != nil {
		if errors.Is(err, strictjson.ErrTrailingData) {
			return CodexReviewSourceOutcome{
				InvocationID: id,
				FailureClass: domain.ReviewFailureContradiction,
				Failure:      "Codex review returned trailing structured output",
			}
		}
		return CodexReviewSourceOutcome{
			InvocationID: id,
			FailureClass: domain.ReviewFailureContradiction,
			Failure:      "Codex review returned malformed structured output",
		}
	}
	if raw.Findings == nil {
		return CodexReviewSourceOutcome{
			InvocationID: id,
			FailureClass: domain.ReviewFailureContradiction,
			Failure:      "Codex review omitted the required findings array",
		}
	}
	contradiction := func(failure string) CodexReviewSourceOutcome {
		return CodexReviewSourceOutcome{
			InvocationID: id, FailureClass: domain.ReviewFailureContradiction, Failure: failure,
		}
	}
	completedAt := s.cfg.Now().UTC()
	findings := make([]domain.Finding, len(*raw.Findings))
	for i, item := range *raw.Findings {
		severity := domain.FindingSeverity(item.Severity)
		if !slices.Contains(domain.AllFindingSeverities, severity) {
			return contradiction("Codex review returned an out-of-domain finding severity")
		}
		if item.Location == nil {
			return contradiction("Codex review omitted a required finding location")
		}
		location := domain.FindingLocation{
			Path: item.Location.Path, StartLine: item.Location.StartLine, EndLine: item.Location.EndLine,
		}
		if err := location.Validate(); err != nil || location.StartLine < 1 {
			return contradiction("Codex review returned an invalid finding location")
		}
		identity := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%d\x00%d\x00%s",
			id, req.BaseSHA, req.HeadSHA, severity,
			location.Path, location.StartLine, location.EndLine, item.Explanation)
		sum := sha256.Sum256([]byte(identity))
		findings[i] = domain.Finding{
			ID: domain.FindingID(fmt.Sprintf("review-%x", sum[:12])), RunID: req.RunID,
			Source: "codex_local", Severity: severity, Location: &location,
			Message: item.Explanation, RawText: item.Explanation, CreatedAt: completedAt,
		}
	}
	result := exec.ReviewResult{
		InvocationID: id, BaseSHA: req.BaseSHA, HeadSHA: req.HeadSHA,
		Provider: "openai", ModelConfiguration: s.cfg.Review.Model + "/" + s.cfg.Review.ReasoningEffort,
		ConfigurationDigest: s.cfg.ConfigurationDigest,
		InstructionDigest:   req.Instructions.ResultDigest,
		CostOwner:           s.cfg.CostOwner, CompletedAt: completedAt,
		Findings: findings,
	}
	// Base-commit inline result-evidence envelope (version "codex-review-result-v3").
	result.CompletionEvidence, _ = oldReviewResultEvidence(result, collectionEvidence)
	return CodexReviewSourceOutcome{
		InvocationID: id, Result: &result, CollectionEvidence: collectionEvidence,
	}
}

// oldReviewResultEvidence reconstructs the base-commit CodexReviewResultEvidence
// with its inline "codex-review-result-v3" literal.
func oldReviewResultEvidence(
	result exec.ReviewResult, collectionEvidence domain.Digest,
) (domain.Digest, error) {
	result.CompletionEvidence = ""
	body, err := json.Marshal(struct {
		Version            string            `json:"version"`
		CollectionEvidence domain.Digest     `json:"collection_evidence"`
		Result             exec.ReviewResult `json:"result"`
	}{"codex-review-result-v3", collectionEvidence, result})
	if err != nil {
		return "", err
	}
	return domain.Digest(contentaddr.Sum(body)), nil
}

// newEquivalenceReviewSource builds a Codex source shell sufficient to drive
// normalizeCollection deterministically. It goes through the accessor default,
// so cfg.provider is the Codex provider.
func newEquivalenceReviewSource() *CodexReviewSource {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	return &CodexReviewSource{cfg: CodexReviewSourceConfig{
		Review:              CodexReviewConfig{Model: "gpt-codex", ReasoningEffort: "high"},
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		CostOwner:           "subscription:owner", Now: func() time.Time { return now },
	}}
}

// FuzzReviewNormalizeCollectionEquivalence measures surface (a): the new
// provider-driven normalizeCollection is decision-for-decision identical to the
// base-commit reconstruction across a fuzzed corpus of exit statuses, structured
// output shapes (valid, malformed, trailing, duplicate-key, missing, out-of-domain
// severity, whole-file and inverted line ranges), and request identity fields.
func FuzzReviewNormalizeCollectionEquivalence(f *testing.F) {
	seeds := []struct {
		exit    int
		result  string
		events  string
		baseSHA string
		headSHA string
		runID   string
	}{
		{0, `{"findings":[{"severity":"P2","location":{"path":"a.go","start_line":12,"end_line":12},"explanation":"x"}]}`, "ev\n", strings.Repeat("a", 40), strings.Repeat("b", 40), "run-1"},
		{0, `{"findings":[]}`, "", strings.Repeat("a", 40), strings.Repeat("b", 40), "run-1"},
		{0, `{"findings":[{"severity":"P9","location":{"path":"a.go","start_line":1,"end_line":1},"explanation":"x"}]}`, "", "", "", "r"},
		{0, `{"findings":[{"severity":"P0","location":{"path":"a.go","start_line":0,"end_line":0},"explanation":"x"}]}`, "", "", "", "r"},
		{0, `{"findings":[{"severity":"P1","location":{"path":"a.go","start_line":5,"end_line":2},"explanation":"x"}]}`, "", "", "", "r"},
		{0, `{"findings":[{"severity":"P3","explanation":"x"}]}`, "", "", "", "r"},
		{0, `{"findings":[{"severity":"P2","location":{"path":"a.go","start_line":1,"end_line":1},"explanation":"x"}]}extra`, "", "", "", "r"},
		{0, `{"findings":[{"severity":"P2","location":{"path":"a.go","start_line":1,"end_line":1},"explanation":"x"},"findings":[]}`, "", "", "", "r"},
		{0, `{"nope":1}`, "", "", "", "r"},
		{0, `not json`, "", "", "", "r"},
		{0, ``, "", "", "", "r"},
		{1, `{"findings":[]}`, "some terminal output\n", strings.Repeat("a", 40), strings.Repeat("b", 40), "run-1"},
		{7, ``, "boom\n", "", "", "r"},
		{255, ``, "credential refresh attempted\n", "", "", "r"},
	}
	for _, s := range seeds {
		f.Add(s.exit, s.result, s.events, s.baseSHA, s.headSHA, s.runID)
	}
	f.Fuzz(func(t *testing.T, exit int, result, events, baseSHA, headSHA, runID string) {
		source := newEquivalenceReviewSource()
		collection := CodexReviewCollection{
			ExitStatus: exit, Result: []byte(result), Events: []byte(events),
		}
		req := exec.ReviewRequest{RunID: domain.RunID(runID), BaseSHA: baseSHA, HeadSHA: headSHA}
		id := domain.InvocationID("review-" + runID)
		got := source.normalizeCollection(id, req, collection)
		want := oldNormalizeCollection(source, id, req, collection)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("normalizeCollection diverged from base reconstruction\n new: %+v\n base: %+v", got, want)
		}
	})
}

// oldBuildReviewAgentSpec is the base-commit (6bf2c8b8) body of
// BuildCodexReviewAgentSpec, reconstructed verbatim with its inline
// codexReviewCommand / codexReviewTopologyVersion calls, as the independent
// reference for the launch-spec equivalence comparison.
func oldBuildReviewAgentSpec(
	cfg CodexReviewConfig,
	req CodexReviewSpec,
) (ContainerSpec, CodexReviewJournalBinding, error) {
	if err := validateCodexReviewRequest(cfg, req); err != nil {
		return ContainerSpec{}, CodexReviewJournalBinding{}, err
	}
	authPath, hostAuthBody, err := readCodexReviewInput(cfg.InputRoot, req.AuthSnapshot, maxCodexAuthSnapshotBytes)
	if err != nil {
		return ContainerSpec{}, CodexReviewJournalBinding{}, fmt.Errorf("%w: auth snapshot: %w", ErrInvalidCodexReviewSpec, err)
	}
	authBody, accessExpiry, err := codexReviewAgentAuthSnapshot(req.AuthMode, hostAuthBody)
	if err != nil {
		return ContainerSpec{}, CodexReviewJournalBinding{}, fmt.Errorf("%w: auth snapshot: %w", ErrInvalidCodexReviewSpec, err)
	}
	now := cfg.Now()
	if accessExpiry != nil && accessExpiry.Sub(now) < cfg.AccessTokenLifetimeFloor {
		return ContainerSpec{}, CodexReviewJournalBinding{}, fmt.Errorf(
			"%w: identity %q access token has %s remaining, floor %s",
			ErrInvalidCodexReviewSpec, req.AuthIdentityID,
			accessExpiry.Sub(now), cfg.AccessTokenLifetimeFloor,
		)
	}
	instructionPath, instructionBody, err := readCodexReviewInput(
		cfg.InputRoot, req.InstructionFile, domain.MaxVendorInstructionBytes,
	)
	if err != nil {
		return ContainerSpec{}, CodexReviewJournalBinding{}, fmt.Errorf("%w: instruction snapshot: %w", ErrInvalidCodexReviewSpec, err)
	}
	if !bytes.Equal(instructionBody, req.Instructions.Body) {
		return ContainerSpec{}, CodexReviewJournalBinding{}, fmt.Errorf(
			"%w: instruction snapshot does not match its admitted digest-bound body",
			ErrInvalidCodexReviewSpec,
		)
	}
	if authPath == instructionPath {
		return ContainerSpec{}, CodexReviewJournalBinding{}, fmt.Errorf(
			"%w: auth and instruction snapshots must be distinct files",
			ErrInvalidCodexReviewSpec,
		)
	}
	authSum := sha256.Sum256(authBody)
	wantAuthDigest := contentaddr.Format(authSum[:])
	instructionSum := sha256.Sum256(instructionBody)
	wantInstructionDigest := contentaddr.Format(instructionSum[:])
	if req.Snapshot.authDigest != wantAuthDigest || req.Snapshot.instructionDigest != wantInstructionDigest {
		return ContainerSpec{}, CodexReviewJournalBinding{}, failf(
			CheckCredentialSeparation, "Codex review snapshot volume does not hold the admitted credential and instruction bytes",
		)
	}
	shadowTargets := codexAgentsShadowTargets(cfg.WorkspaceTarget, req.Workspace.agentsEntry)
	env := append([]string{
		"HOME=" + CodexContainerHomeTarget,
		"CODEX_HOME=" + CodexHomeTarget,
	}, proxyEnvironment(cfg.ProxyURL)...)
	command := codexReviewCommand(cfg.WorkspaceTarget, cfg.Model, cfg.ReasoningEffort, req.Prompt)
	mounts := []Mount{
		{Type: MountVolume, Source: req.WorkspaceVolume, Target: cfg.WorkspaceTarget, ReadOnly: true},
		{Type: MountVolume, Source: req.Snapshot.volume, Target: codexReviewSnapshotTarget, ReadOnly: true},
	}
	for _, target := range shadowTargets {
		mounts = append(mounts, Mount{
			Type: MountVolume, Source: req.AgentsShadow.volume, Target: target, ReadOnly: true,
		})
	}
	spec := ContainerSpec{
		Name:    codexReviewContainerName(req.RunID),
		Image:   req.Image,
		Command: command,
		Env:     env,
		Mounts:  mounts,
		Labels:  runLabels(req.RunID),
		Network: codexReviewNetworkName(req.RunID),
	}
	binding := CodexReviewJournalBinding{
		TopologyVersion:                 codexReviewTopologyVersion,
		RunID:                           req.RunID,
		Boundary:                        req.Boundary,
		WorkspaceSourceRunID:            req.WorkspaceSourceRunID,
		WorkspaceVolume:                 req.WorkspaceVolume,
		WorkspaceFingerprint:            req.Workspace.fingerprint,
		WorkspaceHead:                   req.Workspace.head,
		WorkspaceTreeDigest:             req.Workspace.treeDigest,
		WorkspaceAgentsEntry:            req.Workspace.agentsEntry,
		WorkspaceObserverImage:          req.Workspace.observerImage,
		WorkspaceObserverFingerprint:    req.Workspace.observerFingerprint,
		WorkspaceTarget:                 cfg.WorkspaceTarget,
		WorkspaceReadOnly:               true,
		HomeTarget:                      CodexContainerHomeTarget,
		CodexHomeTarget:                 CodexHomeTarget,
		FreshContext:                    true,
		ContinuityMounted:               false,
		AuthMode:                        req.AuthMode,
		AuthIdentityID:                  req.AuthIdentityID,
		AuthSnapshotDigest:              wantAuthDigest,
		AccessTokenExpiresAt:            accessExpiry,
		AuthReadOnly:                    true,
		AuthStoreMutationLeaseRequired:  true,
		InstructionDigest:               req.Instructions.Digest,
		InstructionCompositionVersion:   req.InstructionBinding.CompositionVersion,
		HostInstructionDigest:           cloneOptionalDigest(req.InstructionBinding.HostDigest),
		RepositoryInstructionSources:    slices.Clone(req.InstructionBinding.RepositorySources),
		InstructionReadOnly:             true,
		SnapshotVolume:                  req.Snapshot.volume,
		SnapshotTarget:                  codexReviewSnapshotTarget,
		SnapshotFingerprint:             req.Snapshot.fingerprint,
		SnapshotObserverImage:           req.Snapshot.observerImage,
		SnapshotObserverFingerprint:     req.Snapshot.observerFingerprint,
		SnapshotReadOnly:                true,
		AgentsShadowVolume:              req.AgentsShadow.volume,
		AgentsShadowFingerprint:         req.AgentsShadow.fingerprint,
		AgentsShadowDigest:              req.AgentsShadow.digest,
		AgentsShadowObserverImage:       req.AgentsShadow.observerImage,
		AgentsShadowObserverFingerprint: req.AgentsShadow.observerFingerprint,
		AgentsShadowTargets:             slices.Clone(shadowTargets),
		AgentsShadowReadOnly:            true,
		ProviderEndpoints:               slices.Clone(cfg.ProviderEndpoints),
		ProviderNetwork:                 req.Network.name,
		ProviderNetworkFingerprint:      req.Network.fingerprint,
		ProviderNetworkHostOnly:         true,
		ProviderNetworkGateway:          req.Network.gateway,
		ProviderNetworkSubnet:           req.Network.subnet,
		ProviderProxyAuthority:          req.Network.proxyAuthority,
		RefreshEndpointReachable:        false,
		PublicationCredentials:          false,
		LauncherEnvironmentDigest:       digestEnvironment(env),
		CommandDigest:                   digestStrings(command),
	}
	if err := validateCodexReviewAgentSpec(cfg, req, spec, binding); err != nil {
		return ContainerSpec{}, CodexReviewJournalBinding{}, err
	}
	return spec, binding, nil
}

// TestReviewBuildAgentSpecEquivalence measures surface (b): the new
// provider-driven BuildCodexReviewAgentSpec produces a container spec and
// journal binding decision-for-decision identical to the base-commit
// reconstruction, across variants of the command-affecting inputs (workspace
// target, model, reasoning effort, prompt) that flow into the review command,
// its digest, and the mount targets.
func TestReviewBuildAgentSpecEquivalence(t *testing.T) {
	variants := []struct {
		name      string
		workspace string
		model     string
		effort    string
		prompt    string
	}{
		{"baseline", "", "", "", ""},
		{"other-model", "", "gpt-5.2-codex-mini", "low", "Different prompt."},
		{"shell-metachars", "", "m$o'd\"e l", "hi;gh", "Review \"$1\" & `echo x`"},
		{"deep-workspace", "/workspace/nested/project", "", "", ""},
		{"empty-prompt", "", "", "", ""},
		{"unicode", "", "modèl", "éffort", "review the café changes"},
	}
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			cfg, req := testCodexReview(t)
			if v.workspace != "" {
				cfg.WorkspaceTarget = v.workspace
			}
			if v.model != "" {
				cfg.Model = v.model
			}
			if v.effort != "" {
				cfg.ReasoningEffort = v.effort
			}
			if v.prompt != "" {
				req.Prompt = v.prompt
			}
			gotSpec, gotBinding, gotErr := BuildCodexReviewAgentSpec(cfg, req)
			wantSpec, wantBinding, wantErr := oldBuildReviewAgentSpec(cfg, req)
			if !sameErr(gotErr, wantErr) {
				t.Fatalf("build error diverged: new=%v base=%v", gotErr, wantErr)
			}
			if !reflect.DeepEqual(gotSpec, wantSpec) {
				t.Errorf("container spec diverged from base reconstruction\n new: %+v\n base: %+v", gotSpec, wantSpec)
			}
			if !reflect.DeepEqual(gotBinding, wantBinding) {
				t.Errorf("journal binding diverged from base reconstruction\n new: %+v\n base: %+v", gotBinding, wantBinding)
			}
		})
	}
}

// FuzzReviewCommandEquivalence measures that the provider's review command is
// byte-identical to the base-commit codexReviewCommand for arbitrary workspace
// target, model, reasoning effort, and prompt (including shell metacharacters),
// so the launch command and its digest never drift through the seam.
func FuzzReviewCommandEquivalence(f *testing.F) {
	f.Add("/workspace/project", "gpt-5.2-codex", "high", "Review the exact candidate head.")
	f.Add("", "", "", "")
	f.Add("/w", "m$o'd\"e l", "hi;gh", "Review \"$1\" & `echo x`")
	f.Fuzz(func(t *testing.T, workspace, model, effort, prompt string) {
		got := codexReviewProvider{}.reviewCommand(workspace, model, effort, prompt)
		want := codexReviewCommand(workspace, model, effort, prompt)
		if !slices.Equal(got, want) {
			t.Fatalf("provider review command diverged from base\n new: %q\n base: %q", got, want)
		}
	})
}

func sameErr(a, b error) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if a == nil {
		return true
	}
	return a.Error() == b.Error()
}
