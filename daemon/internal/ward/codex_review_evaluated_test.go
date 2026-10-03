package ward

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

// evaluatedCodexReview is the review spec of a base-advance re-entry: the
// fixture's observed workspace commit stands for the unpushed merge, and the
// pull request head is a different commit.
func evaluatedCodexReview(t *testing.T) (CodexReviewConfig, CodexReviewSpec) {
	t.Helper()
	cfg, req := testCodexReview(t)
	req.EvaluatedSHA = testCodexReviewHead
	req.HeadSHA = strings.Repeat("b", 40)
	return cfg, req
}

func TestBuildCodexReviewAgentSpecReviewsTheEvaluatedCommit(t *testing.T) {
	cfg, req := evaluatedCodexReview(t)
	spec, binding, err := BuildCodexReviewAgentSpec(cfg, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCodexReviewAgentSpec(cfg, req, spec, binding); err != nil {
		t.Fatalf("generated topology fails its conformance check: %v", err)
	}
	if binding.WorkspaceHead != req.EvaluatedSHA {
		t.Fatalf("journal binding workspace head = %q, want the evaluated commit", binding.WorkspaceHead)
	}
	program := spec.Command[2]
	for _, want := range []string{
		`test "$(git rev-parse HEAD)" = '\''` + req.EvaluatedSHA,
		`git cat-file -e '\''` + req.BaseSHA + `^{commit}`,
		`git cat-file -e '\''` + req.HeadSHA + `^{commit}`,
		`git cat-file -e '\''` + req.EvaluatedSHA + `^{commit}`,
		`--no-textconv '\''` + req.BaseSHA + `'\'' '\''` + req.EvaluatedSHA + `'\'' --`,
		"freeside-review-access-v2 base=%s head=%s evaluated=%s cwd=%s",
	} {
		if !strings.Contains(program, want) {
			t.Errorf("evaluated-commit review command omits %q:\n%s", want, program)
		}
	}
	got, err := json.MarshalIndent(struct {
		Spec    ContainerSpec             `json:"spec"`
		Binding CodexReviewJournalBinding `json:"binding"`
	}{cloneContainerSpec(spec), binding}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "codex-review-spec-prospective-merge", append(got, '\n'))

	// The command is re-derived from the request at the pre-start check, so a
	// request that drops the evaluated commit no longer matches the container
	// built for it.
	dropped := req
	dropped.EvaluatedSHA, dropped.HeadSHA = "", ""
	if err := validateCodexReviewAgentSpec(cfg, dropped, spec, binding); !errors.Is(err, ErrConformance) {
		t.Fatalf("spec built for the merge passed as a head review: %v", err)
	}
}

// An evaluated commit is trusted only as the commit the runtime observed in
// the workspace. A caller-supplied value that the observation does not back,
// or that is not distinct from the head and base, is refused before a
// credential-bearing container is built.
func TestBuildCodexReviewAgentSpecRejectsUnboundEvaluatedCommit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*CodexReviewSpec)
	}{
		{"workspace holds another commit", func(r *CodexReviewSpec) { r.EvaluatedSHA = strings.Repeat("e", 40) }},
		{"no head", func(r *CodexReviewSpec) { r.HeadSHA = "" }},
		{"head without an evaluated commit", func(r *CodexReviewSpec) { r.EvaluatedSHA = "" }},
		{"evaluated commit is the head", func(r *CodexReviewSpec) { r.HeadSHA = r.EvaluatedSHA }},
		{"evaluated commit is the base", func(r *CodexReviewSpec) { r.BaseSHA = r.EvaluatedSHA }},
		{"head is the base", func(r *CodexReviewSpec) { r.HeadSHA = r.BaseSHA }},
		{"malformed head", func(r *CodexReviewSpec) { r.HeadSHA = "HEAD" }},
		{"malformed evaluated commit", func(r *CodexReviewSpec) { r.EvaluatedSHA = "HEAD" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, req := evaluatedCodexReview(t)
			tc.mutate(&req)
			if _, _, err := BuildCodexReviewAgentSpec(cfg, req); !errors.Is(err, ErrInvalidCodexReviewSpec) {
				t.Fatalf("BuildCodexReviewAgentSpec = %v, want ErrInvalidCodexReviewSpec", err)
			}
		})
	}
}

func TestCodexReviewLaunchShapeValidatesExpectedEvaluated(t *testing.T) {
	_, _, cfg, launch, _ := testCodexReviewLifecycle(t)
	if got := launch.workspaceCommit(); got != launch.ExpectedHead {
		t.Fatalf("workspace commit without an evaluated commit = %q, want the head", got)
	}
	launch.ExpectedEvaluated = testCodexReviewEvaluated
	if err := validateCodexReviewLaunchShape(codexReviewProvider{}, cfg, launch); err != nil {
		t.Fatalf("launch naming a distinct evaluated commit: %v", err)
	}
	if got := launch.workspaceCommit(); got != testCodexReviewEvaluated {
		t.Fatalf("workspace commit = %q, want the evaluated commit", got)
	}
	for name, evaluated := range map[string]string{
		"the head": launch.ExpectedHead, "the base": launch.ExpectedBase, "malformed": "HEAD",
	} {
		refused := launch
		refused.ExpectedEvaluated = evaluated
		if err := validateCodexReviewLaunchShape(codexReviewProvider{}, cfg, refused); !errors.Is(err, ErrInvalidCodexReviewSpec) {
			t.Fatalf("ExpectedEvaluated %s = %v, want ErrInvalidCodexReviewSpec", name, err)
		}
	}
}

// An intent opened before this field existed must re-derive to the same
// digest, or a review in flight across the upgrade fails its recovery. The
// shape below is the pre-upgrade one, written out so the pin does not follow
// the production struct.
func TestCodexReviewIntentDigestKeepsOpenIntentsAndBindsTheEvaluatedCommit(t *testing.T) {
	_, _, cfg, launch, _ := testCodexReviewLifecycle(t)
	legacy, err := json.Marshal(struct {
		ExpectedBase                                                          string `json:"ExpectedBase,omitempty"`
		RunID, Image, WorkspaceSourceRunID, WorkspaceVolume, ExpectedHead     string
		Boundary                                                              CodexReviewBoundary
		AuthMode                                                              CodexAuthMode
		AuthIdentityID                                                        domain.AuthIdentityID
		InstructionBinding                                                    exec.ReviewInstructionBinding
		ApprovedImage, ObserverImage, WorkspaceTarget, Model, ReasoningEffort string
		ProviderEndpoints                                                     []string
	}{
		ExpectedBase: launch.ExpectedBase,
		RunID:        launch.RunID, Image: launch.Image, WorkspaceSourceRunID: launch.WorkspaceSourceRunID,
		WorkspaceVolume: launch.WorkspaceVolume, ExpectedHead: launch.ExpectedHead,
		Boundary: launch.Boundary, AuthMode: launch.AuthMode, AuthIdentityID: launch.AuthIdentityID,
		InstructionBinding: launch.InstructionBinding,
		ApprovedImage:      cfg.ApprovedImage, ObserverImage: cfg.ObserverImage,
		WorkspaceTarget: cfg.WorkspaceTarget, Model: cfg.Model, ReasoningEffort: cfg.ReasoningEffort,
		ProviderEndpoints: slices.Clone(cfg.ProviderEndpoints),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%x", sha256.Sum256(legacy))
	got, err := codexReviewIntentDigest(cfg, launch)
	if err != nil || got != want {
		t.Fatalf("intent digest without an evaluated commit = %q, %v; want the pre-upgrade %q", got, err, want)
	}
	launch.ExpectedEvaluated = testCodexReviewEvaluated
	merged, err := codexReviewIntentDigest(cfg, launch)
	if err != nil || merged == want {
		t.Fatalf("intent digest ignored the evaluated commit: %q, %v", merged, err)
	}
}

func TestCodexProductionReviewPromptNamesTheProspectiveMerge(t *testing.T) {
	base, head, merge := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("e", 40)
	request := exec.ReviewRequest{
		BaseSHA: base, HeadSHA: head, Verification: testReviewVerificationEvidence(),
	}
	plain := codexProductionReviewPrompt(request)
	request.EvaluatedSHA = merge
	merged := codexProductionReviewPrompt(request)
	for _, want := range []string{
		"Review the prospective merge " + merge + ": pull request head " + head + " merged into base " + base + ".",
		"The merge was built locally and never pushed, so the pull request's head is still " + head,
		"your workspace holds the merge, and the reviewed diff runs from the base to the merge",
		"The change from the base to the prospective merge introduced it.",
		`"recipe_digest":"sha256:` + strings.Repeat("c", 64) + `"`,
	} {
		if !strings.Contains(merged, want) {
			t.Errorf("prospective-merge prompt omits %q:\n%s", want, merged)
		}
	}
	evidence, err := json.Marshal(request.Verification)
	if err != nil {
		t.Fatal(err)
	}
	if want := "Review the exact candidate at head " + head + " against base " + base +
		". The preceding verification evidence is " + string(evidence) + ".\n\nApply a precision-first admission test"; !strings.HasPrefix(plain, want) {
		t.Fatalf("head review prompt no longer opens as it did before the evaluated commit existed:\n%s", plain)
	}
	for _, absent := range []string{"Review the exact candidate at head", "head-versus-base change introduced it"} {
		if strings.Contains(merged, absent) {
			t.Errorf("prospective-merge prompt still carries %q", absent)
		}
	}
	// Both shapes share everything after the first admission bullet: the
	// admission bar, location rules, and daemon-owned rules do not fork.
	const shared = "- It has a demonstrable failure path."
	plainTail, mergedTail := plain[strings.Index(plain, shared):], merged[strings.Index(merged, shared):]
	if plainTail != mergedTail {
		t.Fatal("the two prompt shapes diverge beyond the candidate sentence and the introduction bullet")
	}
	if strings.Contains(plain, merge) || strings.Contains(plain, "prospective merge") {
		t.Fatalf("head review prompt mentions a merge:\n%s", plain)
	}
	// Claude shares the prompt text; only the protocol label is its own.
	if got := (claudeReviewProvider{}).reviewPrompt(request); got != merged {
		t.Fatal("Claude and Codex prompts differ for a prospective merge")
	}
}

// Both sources seed the review workspace from the commit the request asks to
// have reviewed. With an evaluated commit that is the merge, not the head, and
// a checkout that does not hold the merge never reaches a reviewer.
func TestReviewSourcesSeedTheEvaluatedCommit(t *testing.T) {
	for _, provider := range []struct {
		name      string
		lifecycle func(Runtime, Config, RuntimeResourceAuthorizer) (*CodexReviewLifecycle, error)
		fixture   func(*testing.T) (CodexReviewConfig, CodexReviewSpec)
		configure func(
			*testing.T, *CodexReviewLifecycle, CodexReviewConfig, CodexReviewSpec, CodexReviewJournal,
		) CodexReviewSourceConfig
		construct func(CodexReviewSourceConfig) (*CodexReviewSource, error)
	}{
		{"codex", NewCodexReviewLifecycle, testCodexReview, codexReviewSourceConfigForTest, NewCodexReviewSource},
		{"claude", NewClaudeReviewLifecycle, testClaudeReview, claudeReviewSourceConfigForTest, NewClaudeReviewSource},
	} {
		for _, tc := range []struct {
			name      string
			evaluated func(checkout string) string
			wantClass domain.ReviewFailureClass
		}{
			{"checkout holds the merge", func(checkout string) string { return checkout }, ""},
			{
				"checkout holds another commit",
				func(string) string { return testCodexReviewEvaluated },
				domain.ReviewFailureContradiction,
			},
		} {
			t.Run(provider.name+"/"+tc.name, func(t *testing.T) {
				fx := newHandoffFixture(t)
				seedSpec := fx.seed(t)
				lifecycle, err := provider.lifecycle(fx.rt, fx.cfg, nil)
				if err != nil {
					t.Fatal(err)
				}
				cfg, requestSpec := provider.fixture(t)
				journal := &fakeCodexReviewJournal{}
				source, err := provider.construct(provider.configure(t, lifecycle, cfg, requestSpec, journal))
				if err != nil {
					t.Fatal(err)
				}
				id := domain.InvocationID("review-" + strings.Repeat("e", 24))
				request := exec.ReviewRequest{
					RunID: "run-1", Round: 1, Repo: seedSpec.Seed.Base.Repo,
					RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
					BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40),
					EvaluatedSHA: tc.evaluated(seedSpec.Seed.Base.BaseSHA),
					Workspace:    seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(),
					Instructions: testReviewInstructionBinding(), RequestedAt: codexReviewEpoch,
				}
				err = source.RequestReview(t.Context(), id, request)
				t.Cleanup(func() {
					source.mu.Lock()
					defer source.mu.Unlock()
					if launch := source.launches[id]; launch != nil {
						_ = launch.Close()
					}
				})
				if tc.wantClass != "" {
					var failure *exec.ReviewSourceFailure
					if !errors.As(err, &failure) || failure.Class != tc.wantClass {
						t.Fatalf("RequestReview = %v, want a %s failure", err, tc.wantClass)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if got := fx.rt.volBase[namesFor(string(id)).Workspace]; got != request.EvaluatedSHA {
					t.Fatalf("seeded workspace holds %q, want the evaluated commit %q", got, request.EvaluatedSHA)
				}
				if journal.binding.WorkspaceHead != request.EvaluatedSHA {
					t.Fatalf("journal binding workspace head = %q, want the evaluated commit", journal.binding.WorkspaceHead)
				}
			})
		}
	}
}

// The approved configuration must cover the command every head review runs as
// well as the prospective-merge one, so an edit to either moves the digest.
func TestCodexReviewConfigurationCoversBothCommandShapes(t *testing.T) {
	cfg, request := testCodexReview(t)
	provider := codexReviewProvider{}
	envelope, err := newCodexReviewConfigurationEnvelope(
		provider, cfg, 64, request.AuthMode, request.AuthIdentityID, "subscription:owner")
	if err != nil {
		t.Fatal(err)
	}
	shape := func(evaluated string) []string {
		return provider.reviewCommand(
			cfg.WorkspaceTarget, cfg.Model, cfg.ReasoningEffort, "<runtime-review-prompt>",
			"<bound-base-sha>", "<bound-head-sha>", evaluated)
	}
	head, merge := shape(""), shape("<bound-evaluated-sha>")
	if slices.Equal(head, merge) {
		t.Fatal("the head and prospective-merge commands do not differ")
	}
	for name, partial := range map[string][]string{"head": head, "prospective-merge": merge} {
		if envelope.CommandTemplateDigest == digestStrings(partial) {
			t.Errorf("command template digest covers the %s command alone", name)
		}
	}
	if want := digestStrings(append(slices.Clone(head), merge...)); envelope.CommandTemplateDigest != want {
		t.Errorf("command template digest = %s, want both shapes %s", envelope.CommandTemplateDigest, want)
	}
}
