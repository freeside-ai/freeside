package ward_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
	"github.com/freeside-ai/freeside/daemon/internal/wardstore"
)

type failAfterBindingJournal struct {
	ward.CodexReviewJournal
	afterBinding bool
	failed       bool
	beforeFail   func()
}

func (j *failAfterBindingJournal) GetCodexReviewBinding(
	ctx context.Context, runID string,
) (ward.CodexReviewJournalBinding, error) {
	binding, err := j.CodexReviewJournal.GetCodexReviewBinding(ctx, runID)
	if err == nil {
		j.afterBinding = true
	}
	return binding, err
}

func (j *failAfterBindingJournal) MarkCodexReviewIntentResource(
	ctx context.Context, runID string, resource ward.CodexReviewIntentResource,
) error {
	if err := j.CodexReviewJournal.MarkCodexReviewIntentResource(ctx, runID, resource); err != nil {
		return err
	}
	if j.afterBinding && !j.failed {
		j.failed = true
		j.beforeFail()
		return errors.New("simulated journal response loss after durable resource update")
	}
	return nil
}

type codexReviewAdapterFixture struct {
	path     string
	store    *store.Store
	Adapters *wardstore.Adapters
}

func openCodexReviewAdapter(t *testing.T) *codexReviewAdapterFixture {
	t.Helper()
	fixture := &codexReviewAdapterFixture{path: filepath.Join(t.TempDir(), "freeside.db")}
	fixture.reopen(t)
	t.Cleanup(func() {
		if fixture.store != nil {
			_ = fixture.store.Close()
		}
	})
	return fixture
}

func (f *codexReviewAdapterFixture) reopen(t *testing.T) {
	t.Helper()
	if f.store != nil {
		if err := f.store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(context.Background(), f.path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	adapters, err := wardstore.New(st)
	if err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	f.store = st
	f.Adapters = adapters
}

func TestCodexReviewLifecycleUsesRealJournalThroughStarted(t *testing.T) {
	ctx := context.Background()
	adapterFixture := openCodexReviewAdapter(t)
	adapters := adapterFixture.Adapters
	fixture := ward.NewCodexReviewLifecycleTestFixture(t, adapters.Journal)

	launch, err := fixture.Lifecycle.CodexReview(ctx, fixture.Config, fixture.Launch)
	if err != nil {
		t.Fatalf("CodexReview with real journal: %v", err)
	}
	t.Cleanup(func() { _ = launch.Close() })
	intent, err := adapters.Journal.GetCodexReviewIntent(ctx, fixture.Launch.RunID)
	if err != nil || intent.State != ward.CodexReviewIntentStarted {
		t.Fatalf("final intent = %+v, %v; want started", intent, err)
	}
	if err := fixture.Lifecycle.AbortCodexReview(ctx, fixture.Config, fixture.Launch.RunID); err != nil {
		t.Fatalf("abort started fixture: %v", err)
	}

	preparedID := "prepared-resource-guard"
	prepared := ward.CodexReviewLaunchIntent{
		RunID: preparedID, SpecDigest: strings.Repeat("a", 64), OwnershipToken: strings.Repeat("b", 32),
		ShadowVolume: "shadow", Network: "network", ReviewContainer: "review",
		Resources: []ward.CodexReviewIntentResource{{Name: "shadow", OwnershipToken: strings.Repeat("b", 32)}},
		State:     ward.CodexReviewIntentPreparing,
	}
	if err := adapters.Journal.BeginCodexReviewIntent(ctx, prepared); err != nil {
		t.Fatal(err)
	}
	if err := adapters.Journal.MarkCodexReviewIntentPrepared(ctx, preparedID); err != nil {
		t.Fatal(err)
	}
	err = adapters.Journal.MarkCodexReviewIntentResource(ctx, preparedID,
		ward.CodexReviewIntentResource{Name: "shadow", OwnershipToken: strings.Repeat("b", 32), Fingerprint: "late"})
	if !errors.Is(err, ward.ErrConformance) || !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("prepared resource mutation = %v, want conformance immutable conflict", err)
	}
}

func TestCodexReviewPreparedTransitionRejectsDurableOutcome(t *testing.T) {
	ctx := context.Background()
	adapters := openCodexReviewAdapter(t).Adapters
	runID := "rejected-before-prepared"
	_, instructions, err := exec.ComposeCodexReviewInstructions(exec.ReviewHostInstructionInput{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := exec.ReviewRequest{
		RunID: "source-run", Round: 1, Repo: "owner/repo", RepositoryID: 42,
		BaseRef: "main", BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40),
		Workspace: "/seed/candidate", Verification: exec.ReviewVerificationEvidence{
			Outcome:                domain.VerificationPassed,
			RecipeDigest:           domain.Digest("sha256:" + strings.Repeat("c", 64)),
			EvidenceSnapshotDigest: domain.Digest("sha256:" + strings.Repeat("d", 64)),
		}, Instructions: instructions, RequestedAt: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC),
	}
	if err := adapters.Journal.PutCodexReviewRequest(ctx, runID, request); err != nil {
		t.Fatal(err)
	}
	intent := ward.CodexReviewLaunchIntent{
		RunID: runID, SpecDigest: strings.Repeat("a", 64), OwnershipToken: strings.Repeat("b", 32),
		ShadowVolume: "shadow", Network: "network", ReviewContainer: "review",
		Resources: []ward.CodexReviewIntentResource{{Name: "shadow", OwnershipToken: strings.Repeat("b", 32)}},
		State:     ward.CodexReviewIntentPreparing,
	}
	if err := adapters.Journal.BeginCodexReviewIntent(ctx, intent); err != nil {
		t.Fatal(err)
	}
	if err := adapters.Journal.PutCodexReviewOutcome(ctx, runID, ward.CodexReviewSourceOutcome{
		InvocationID: domain.InvocationID(runID),
		FailureClass: domain.ReviewFailureContradiction,
		Failure:      "request rejected",
	}); err != nil {
		t.Fatal(err)
	}
	if ids, err := adapters.Journal.ListCodexReviewOutcomeIDs(ctx); err != nil ||
		!slices.Equal(ids, []string{runID}) {
		t.Fatalf("outcome ids = %#v, %v", ids, err)
	}
	err = adapters.Journal.MarkCodexReviewIntentPrepared(ctx, runID)
	if !errors.Is(err, ward.ErrConformance) || !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("prepare after durable rejection = %v, want conformance immutable conflict", err)
	}
	got, err := adapters.Journal.GetCodexReviewIntent(ctx, runID)
	if err != nil || got.State != ward.CodexReviewIntentPreparing {
		t.Fatalf("intent after rejected preparation = %+v, %v; want preparing", got, err)
	}
}

func TestCodexReviewRecoveryConvergesPreparingCrashAfterBinding(t *testing.T) {
	ctx := context.Background()
	adapterFixture := openCodexReviewAdapter(t)
	adapters := adapterFixture.Adapters
	fixture := ward.NewCodexReviewLifecycleTestFixture(t, adapters.Journal)
	crashing := &failAfterBindingJournal{
		CodexReviewJournal: adapters.Journal,
		beforeFail:         fixture.BlockRuntimeCleanup,
	}
	crashConfig := fixture.Config
	crashConfig.Journal = crashing

	if launch, err := fixture.Lifecycle.CodexReview(ctx, crashConfig, fixture.Launch); err == nil || launch != nil {
		t.Fatalf("CodexReview crash fixture = (%v, %v), want failure", launch, err)
	} else if !errors.Is(err, ward.ErrCodexReviewOperational) {
		t.Fatalf("CodexReview crash fixture = %v, want operational journal response loss", err)
	}
	if !crashing.failed {
		t.Fatal("crash fixture failed before the post-binding reconstruction update")
	}
	intent, err := adapters.Journal.GetCodexReviewIntent(ctx, fixture.Launch.RunID)
	if err != nil || intent.State != ward.CodexReviewIntentPreparing {
		t.Fatalf("crashed intent = %+v, %v; want preparing", intent, err)
	}
	if _, err := adapters.Journal.GetCodexReviewBinding(ctx, fixture.Launch.RunID); err != nil {
		t.Fatalf("crash window did not persist binding: %v", err)
	}
	stage := fixture.SeedSnapshotStageResidue(t)
	fixture.UnblockRuntimeCleanup()
	adapterFixture.reopen(t)
	adapters = adapterFixture.Adapters
	fixture.Config.Journal = adapters.Journal
	fixture.RestartVolumeLifecycleLeaser(t)
	if err := fixture.Lifecycle.RecoverCodexReview(ctx, fixture.Config, fixture.Launch); err != nil {
		t.Fatalf("recover preparing crash: %v", err)
	}
	intent, err = adapters.Journal.GetCodexReviewIntent(ctx, fixture.Launch.RunID)
	if err != nil || intent.State != ward.CodexReviewIntentClosed {
		t.Fatalf("recovered intent = %+v, %v; want closed", intent, err)
	}
	if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot stage residue stat = %v, want absent", err)
	}
	fixture.AssertNoLaunchRuntimeResidue(t)
	if err := fixture.Lifecycle.CleanupCodexReviewWorkspace(ctx, adapters.Journal, fixture.Launch.RunID); err != nil {
		t.Fatalf("cleanup preserved retry workspace: %v", err)
	}
}

func TestCodexReviewClassifiesPostBindingJournalResponseLossOperational(t *testing.T) {
	ctx := context.Background()
	adapterFixture := openCodexReviewAdapter(t)
	adapters := adapterFixture.Adapters
	fixture := ward.NewCodexReviewLifecycleTestFixture(t, adapters.Journal)
	responseLoss := &failAfterBindingJournal{
		CodexReviewJournal: adapters.Journal,
		beforeFail:         func() {},
	}
	cfg := fixture.Config
	cfg.Journal = responseLoss
	if launch, err := fixture.Lifecycle.CodexReview(ctx, cfg, fixture.Launch); err == nil || launch != nil {
		t.Fatalf("CodexReview response-loss fixture = (%v, %v), want failure", launch, err)
	} else if !errors.Is(err, ward.ErrCodexReviewOperational) {
		t.Fatalf("CodexReview response-loss fixture = %v, want operational classification", err)
	}
	if !responseLoss.failed {
		t.Fatal("response-loss fixture failed before the post-binding resource update")
	}
	if err := fixture.Lifecycle.RecoverCodexReview(ctx, fixture.Config, fixture.Launch); err != nil {
		t.Fatalf("recover response-loss fixture: %v", err)
	}
	if err := fixture.Lifecycle.CleanupCodexReviewWorkspace(ctx, adapters.Journal, fixture.Launch.RunID); err != nil {
		t.Fatalf("cleanup response-loss workspace: %v", err)
	}
}
