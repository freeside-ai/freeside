package ward

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/contract"
)

type reviewSourceContractHarness struct {
	runtime   *fakeRuntime
	journal   *fakeCodexReviewJournal
	config    CodexReviewSourceConfig
	source    *CodexReviewSource
	scenario  contract.Scenario
	provider  reviewProvider
	fixture   func(*testing.T) (CodexReviewConfig, CodexReviewSpec)
	configure func(
		*testing.T, *CodexReviewLifecycle, CodexReviewConfig, CodexReviewSpec, CodexReviewJournal,
	) CodexReviewSourceConfig
	construct func(CodexReviewSourceConfig) (*CodexReviewSource, error)
}

func newCodexReviewContractHarness(t *testing.T) contract.ReviewSourceHarness {
	t.Helper()
	return &reviewSourceContractHarness{
		provider: codexReviewProvider{}, fixture: testCodexReview,
		configure: codexReviewSourceConfigForTest, construct: NewCodexReviewSource,
	}
}

func newClaudeReviewContractHarness(t *testing.T) contract.ReviewSourceHarness {
	t.Helper()
	return &reviewSourceContractHarness{
		provider: claudeReviewProvider{}, fixture: testClaudeReview,
		configure: claudeReviewSourceConfigForTest, construct: NewClaudeReviewSource,
	}
}

func (h *reviewSourceContractHarness) Prepare(
	t *testing.T, id domain.InvocationID, scenario contract.Scenario,
) exec.ReviewRequest {
	t.Helper()
	h.scenario = scenario
	cfg, request := h.fixture(t)
	lifecycle, runtime, cfg, launch, journal := testReviewLifecycle(t, h.provider, cfg, request)
	retargetCodexReviewLifecycle(t, runtime, &launch, journal, string(id))
	requestSpec := CodexReviewSpec{
		AuthMode: launch.AuthMode, AuthIdentityID: launch.AuthIdentityID,
		AuthSnapshot: launch.AuthSnapshot, Instructions: launch.Instructions,
		InstructionBinding: launch.InstructionBinding,
	}
	sourceConfig := h.configure(t, lifecycle, cfg, requestSpec, journal)
	source, err := h.construct(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	h.runtime = runtime
	h.journal = journal
	h.config = sourceConfig
	h.source = source
	return exec.ReviewRequest{
		RunID: "contract-run", Round: 1, Repo: "freeside-ai/candidate", RepositoryID: 42,
		BaseRef: "refs/heads/main", BaseSHA: strings.Repeat("a", 40),
		HeadSHA: testCodexReviewHead, Workspace: t.TempDir(),
		Verification: testReviewVerificationEvidence(), Instructions: launch.InstructionBinding,
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
}

func (h *reviewSourceContractHarness) AuthorityRejectionComplete(
	t *testing.T, id domain.InvocationID,
) error {
	t.Helper()
	outcome, ready, err := h.journal.GetCodexReviewOutcome(t.Context(), string(id))
	if err != nil {
		return fmt.Errorf("get authority rejection outcome: %w", err)
	}
	if !ready || !outcome.AbortRequired {
		return fmt.Errorf("authority rejection outcome = %#v, ready=%v; want ready abort", outcome, ready)
	}
	containers, err := h.runtime.ListContainers(t.Context())
	if err != nil {
		return fmt.Errorf("list containers after authority rejection: %w", err)
	}
	volumes, err := h.runtime.ListVolumes(t.Context())
	if err != nil {
		return fmt.Errorf("list volumes after authority rejection: %w", err)
	}
	networks, err := h.runtime.ListNetworks(t.Context())
	if err != nil {
		return fmt.Errorf("list networks after authority rejection: %w", err)
	}
	if len(containers) != 0 || len(volumes) != 0 || len(networks) != 0 {
		return fmt.Errorf("authority rejection leaked topology: containers=%v volumes=%v networks=%v", containers, volumes, networks)
	}
	return nil
}

func (h *reviewSourceContractHarness) Source() exec.ReviewSource { return h.source }

func (*reviewSourceContractHarness) AwaitReady(*testing.T, domain.InvocationID) {}

func (h *reviewSourceContractHarness) Finish(t *testing.T, id domain.InvocationID) {
	t.Helper()
	if h.scenario.Outcome == contract.OutcomeCrashBeforeResult {
		return
	}
	entries := []tarEntry{
		{name: strings.TrimPrefix(codexReviewStatusPath, "/"), body: []byte("0\n")},
		{name: strings.TrimPrefix(codexReviewEventsPath, "/"), body: []byte("terminal\n")},
		{name: strings.TrimPrefix(codexReviewResultPath, "/"), body: []byte(`{"findings":[]}`)},
	}
	if h.scenario.Outcome == contract.OutcomeFail {
		entries = []tarEntry{
			{name: strings.TrimPrefix(codexReviewStatusPath, "/"), body: []byte("1\n")},
			{name: strings.TrimPrefix(codexReviewEventsPath, "/"), body: []byte("terminal\n")},
		}
	}
	h.runtime.exportTarPath = buildTar(t, entries)
	for range 8 {
		status, err := h.source.Inspect(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if status.Terminal() || status == exec.StatusGone {
			return
		}
	}
	t.Fatal("review source contract scenario did not finish")
}

func (h *reviewSourceContractHarness) Restart(t *testing.T) exec.ReviewSource {
	t.Helper()
	source, err := h.construct(h.config)
	if err != nil {
		t.Fatal(err)
	}
	h.source = source
	return source
}

func TestCodexReviewSourceContract(t *testing.T) {
	contract.RunReviewSourceContract(t, contract.ReviewSourceFactory{
		New:              newCodexReviewContractHarness,
		KnownDivergences: reviewSourceKnownDivergences(),
	})
}

func TestClaudeReviewSourceContract(t *testing.T) {
	contract.RunReviewSourceContract(t, contract.ReviewSourceFactory{
		New:              newClaudeReviewContractHarness,
		KnownDivergences: reviewSourceKnownDivergences(),
	})
}

func reviewSourceKnownDivergences() []contract.KnownDivergence {
	return []contract.KnownDivergence{
		{Case: contract.ReviewCaseCrashBeforeResult, Issue: 663, Failure: "Inspect after crash-before-result = \"failed\", want StatusGone"},
		{Case: contract.ReviewCaseCrashAfterResult, Issue: 664, Failure: "Inspect after crash-after-result = \"completed\", want StatusGone"},
	}
}

var _ contract.ReviewSourceHarness = (*reviewSourceContractHarness)(nil)
