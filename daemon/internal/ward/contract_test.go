package ward

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
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
	// credential, when set, is the source's Credential.
	credential func(context.Context, domain.ReviewAdmission) (ReviewCredential, error)
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
	sourceConfig.Credential = h.credential
	source, err := h.construct(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	h.runtime = runtime
	h.journal = journal
	h.config = sourceConfig
	h.source = source
	reviewRequest := exec.ReviewRequest{
		Admission: testSourceReviewAdmission("contract-run", sourceConfig),
		RunID:     "contract-run", Round: 1, Repo: "freeside-ai/candidate", RepositoryID: 42,
		BaseRef: "refs/heads/main", BaseSHA: strings.Repeat("a", 40),
		HeadSHA: testCodexReviewHead, Workspace: t.TempDir(),
		Verification: testReviewVerificationEvidence(), Instructions: launch.InstructionBinding,
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	if scenario.EvaluatedMerge {
		reviewRequest.EvaluatedSHA = testCodexReviewEvaluated
		// The fixture volume holds the head; the runtime observes whatever
		// commit the volume holds, so only a re-seeded volume is at the merge.
		if !scenario.WorkspaceAtHead {
			runtime.volBase[launch.WorkspaceVolume] = testCodexReviewEvaluated
		}
	}
	return reviewRequest
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

// TestClaudeReviewLaunchesUnderAReadCredential: a request admitted under an
// identity the source was not composed with launches end to end on the
// credential read for it (#1981). The host snapshot is gone, so only the read
// body can reach the snapshot volume; the binding records that body's digest
// and the request's identity, and the host stage holding the plaintext is
// wiped once the launch starts.
func TestClaudeReviewLaunchesUnderAReadCredential(t *testing.T) {
	read := []byte("sk-ant-oat01-" + "readFromTheEnrollmentVolume0123456789")
	h := newClaudeReviewContractHarness(t).(*reviewSourceContractHarness)
	var admitted []domain.ReviewAdmission
	h.credential = func(_ context.Context, a domain.ReviewAdmission) (ReviewCredential, error) {
		admitted = append(admitted, a)
		return NewReviewCredential(read), nil
	}
	id := domain.InvocationID("review-" + strings.Repeat("c", 24))
	request := h.Prepare(t, id, contract.Scenario{})
	request.Admission.AuthIdentityID = "claude-second"
	if err := os.Remove(h.config.AuthSnapshot); err != nil {
		t.Fatal(err)
	}
	if err := h.source.RequestReview(t.Context(), id, request); err != nil {
		t.Fatalf("RequestReview under a read credential: %v", err)
	}
	if len(admitted) != 1 || admitted[0].AuthIdentityID != "claude-second" {
		t.Fatalf("the credential reader saw %+v, want the request's admission once", admitted)
	}
	sum := sha256.Sum256(read)
	if h.journal.binding.AuthSnapshotDigest != contentaddr.Format(sum[:]) {
		t.Errorf("binding auth digest = %s, want the read credential's", h.journal.binding.AuthSnapshotDigest)
	}
	if h.journal.intent == nil || h.journal.intent.State != CodexReviewIntentStarted {
		t.Fatalf("launch intent = %+v, want a started launch", h.journal.intent)
	}
	stage := codexReviewSnapshotStagePath(h.config.Lifecycle.cfg.ExportRoot, h.journal.intent.RunID)
	if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the host stage holding the credential survived the launch: %v", err)
	}
}
