package ward

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

func testReviewVerificationEvidence() exec.ReviewVerificationEvidence {
	return exec.ReviewVerificationEvidence{
		Outcome:                domain.VerificationPassed,
		RecipeDigest:           domain.Digest("sha256:" + strings.Repeat("c", 64)),
		EvidenceSnapshotDigest: domain.Digest("sha256:" + strings.Repeat("d", 64)),
	}
}

func codexReviewSourceConfigForTest(
	t *testing.T,
	backend *Backend,
	cfg CodexReviewConfig,
	request CodexReviewSpec,
	journal CodexReviewJournal,
) CodexReviewSourceConfig {
	t.Helper()
	leaser, err := NewRuntimeCodexReviewVolumeLeaser(backend.rt)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Journal = journal
	cfg.ProxyURL = ""
	cfg.VolumeLifecycleLeaser = leaser
	sourceConfig := CodexReviewSourceConfig{
		Backend: backend, Review: cfg, Journal: journal, WorkspaceSizeMB: 64,
		AuthMode: request.AuthMode, AuthIdentityID: request.AuthIdentityID,
		AuthSnapshot: request.AuthSnapshot, Instructions: request.Instructions,
		InstructionFile: request.InstructionFile,
		CostOwner:       "subscription:owner", Now: func() time.Time { return codexReviewEpoch },
	}
	sourceConfig.ConfigurationDigest, err = CodexReviewConfigurationDigest(
		cfg, sourceConfig.WorkspaceSizeMB, sourceConfig.AuthMode, sourceConfig.AuthIdentityID,
		sourceConfig.Instructions.Digest, sourceConfig.CostOwner,
	)
	if err != nil {
		t.Fatal(err)
	}
	return sourceConfig
}

func TestCodexReviewSourceRunsWardLifecycleAndCleansBeforePoll(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal)
	source, err := NewCodexReviewSource(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-run-1-1")
	request := exec.ReviewRequest{
		RunID: "run-1", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(),
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	if err := source.RequestReview(ctx, id, request); err != nil {
		t.Fatal(err)
	}
	resultArchive := buildTar(t, []tarEntry{
		{name: strings.TrimPrefix(codexReviewStatusPath, "/"), body: []byte("0\n")},
		{name: strings.TrimPrefix(codexReviewEventsPath, "/"), body: []byte("terminal\n")},
		{name: strings.TrimPrefix(codexReviewResultPath, "/"), body: []byte(`{"findings":[]}`)},
	})
	fx.rt.exportTarPath = resultArchive
	state, err := backend.InspectCodexReview(ctx, sourceConfig.Review, string(id))
	if err != nil {
		t.Fatal(err)
	}
	if state == StateRunning {
		state, err = backend.InspectCodexReview(ctx, sourceConfig.Review, string(id))
	}
	if err != nil || state != StateStopped {
		t.Fatalf("review runtime state = %q, %v", state, err)
	}
	collection, err := backend.CollectCodexReview(ctx, sourceConfig.Review, string(id))
	if err != nil {
		t.Fatal(err)
	}
	outcome := source.normalizeCollection(id, request, collection)
	if err := journal.PutCodexReviewOutcome(ctx, string(id), outcome); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after durable collection but before cleanup. The new
	// source has no live launch handle and must finish teardown from journals.
	source.mu.Lock()
	_ = source.launches[id].Close()
	delete(source.launches, id)
	source.mu.Unlock()
	// Refute the destructive cleanup boundary too: model a first cleanup that
	// deleted the container and shadow volume, then crashed before the
	// workspace, network, journal transition, or lease release.
	binding, err := journal.GetCodexReviewBinding(ctx, string(id))
	if err != nil {
		t.Fatal(err)
	}
	intent, err := journal.GetCodexReviewIntent(ctx, string(id))
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.rt.DeleteContainer(ctx, binding.ReviewContainer); err != nil {
		t.Fatal(err)
	}
	if err := fx.rt.DeleteVolume(ctx, intent.ShadowVolume); err != nil {
		t.Fatal(err)
	}
	restartedConfig := source.cfg
	restartedConfig.Review.VolumeLifecycleLeaser, err = NewRuntimeCodexReviewVolumeLeaser(fx.rt)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := NewCodexReviewSource(restartedConfig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Poll(ctx, id); !errors.Is(err, exec.ErrResultNotReady) {
		t.Fatalf("poll before cleanup = %v", err)
	}
	cleanupFailure := errors.New("runtime cleanup temporarily unavailable")
	failedCleanup := false
	fx.rt.onDeleteVolume = func(name string) (bool, error) {
		if name == binding.WorkspaceVolume && !failedCleanup {
			failedCleanup = true
			return true, cleanupFailure
		}
		return false, nil
	}
	status, err := restarted.Inspect(ctx, id)
	if err != nil || status != exec.StatusPending {
		t.Fatalf("transient cleanup status = %q, %v", status, err)
	}
	if _, err := restarted.Poll(ctx, id); !errors.Is(err, exec.ErrResultNotReady) {
		t.Fatalf("poll after transient cleanup = %v", err)
	}
	fx.rt.onDeleteVolume = nil
	status, err = restarted.Inspect(ctx, id)
	if err != nil || status != exec.StatusCompleted {
		t.Fatalf("restart cleanup status = %q, %v", status, err)
	}
	result, err := restarted.Poll(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if result.BaseSHA != request.BaseSHA || result.HeadSHA != request.HeadSHA || len(result.Findings) != 0 {
		t.Fatalf("review result = %#v", result)
	}
	if err := restarted.Verify(ctx, id, request.BaseSHA, request.HeadSHA); err != nil {
		t.Fatal(err)
	}
	containers, _ := fx.rt.ListContainers(ctx)
	volumes, _ := fx.rt.ListVolumes(ctx)
	networks, _ := fx.rt.ListNetworks(ctx)
	if len(containers) != 0 || len(volumes) != 0 || len(networks) != 0 {
		t.Fatalf("review topology leaked: containers=%v volumes=%v networks=%v", containers, volumes, networks)
	}
	if _, err := os.Stat(resultArchive); err != nil {
		t.Fatal(err)
	}
}

func TestCodexReviewSourcePersistsInvalidCollectedResultAndCleans(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal)
	source, err := NewCodexReviewSource(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-run-1-1")
	request := exec.ReviewRequest{
		RunID: "run-1", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(),
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	if err := source.RequestReview(ctx, id, request); err != nil {
		t.Fatal(err)
	}
	// Schema-valid but semantically invalid: two identical findings normalize
	// to a duplicated finding identity, which the result contract rejects. The
	// contradiction must still persist durably and finish authenticated
	// cleanup instead of terminalizing around a leaked topology.
	duplicated := `{"findings":[` +
		`{"severity":"P1","location":"daemon/main.go:12","explanation":"unsafe transition"},` +
		`{"severity":"P1","location":"daemon/main.go:12","explanation":"unsafe transition"}]}`
	resultArchive := buildTar(t, []tarEntry{
		{name: strings.TrimPrefix(codexReviewStatusPath, "/"), body: []byte("0\n")},
		{name: strings.TrimPrefix(codexReviewEventsPath, "/"), body: []byte("terminal\n")},
		{name: strings.TrimPrefix(codexReviewResultPath, "/"), body: []byte(duplicated)},
	})
	fx.rt.exportTarPath = resultArchive
	status, err := source.Inspect(ctx, id)
	for err == nil && status == exec.StatusRunning {
		status, err = source.Inspect(ctx, id)
	}
	if err != nil || status != exec.StatusFailed {
		t.Fatalf("invalid collected result status = %q, %v", status, err)
	}
	outcome, ready, err := journal.GetCodexReviewOutcome(ctx, string(id))
	if err != nil || !ready || outcome.Result != nil ||
		outcome.FailureClass != domain.ReviewFailureContradiction ||
		!strings.Contains(outcome.Failure, "invalid collected result") {
		t.Fatalf("persisted outcome = %#v, ready=%v, %v", outcome, ready, err)
	}
	var failure *exec.ReviewSourceFailure
	if _, err := source.Poll(ctx, id); !errors.As(err, &failure) ||
		failure.Class != domain.ReviewFailureContradiction {
		t.Fatalf("poll after invalid collection = %v", err)
	}
	containers, _ := fx.rt.ListContainers(ctx)
	volumes, _ := fx.rt.ListVolumes(ctx)
	networks, _ := fx.rt.ListNetworks(ctx)
	if len(containers) != 0 || len(volumes) != 0 || len(networks) != 0 {
		t.Fatalf("invalid-result topology leaked: containers=%v volumes=%v networks=%v",
			containers, volumes, networks)
	}
}

func TestCodexReviewSourcePersistsMalformedRawOutputAndCleans(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []tarEntry
	}{
		{
			name: "missing status",
			entries: []tarEntry{
				{name: strings.TrimPrefix(codexReviewEventsPath, "/"), body: []byte("terminal\n")},
			},
		},
		{
			name: "invalid status",
			entries: []tarEntry{
				{name: strings.TrimPrefix(codexReviewStatusPath, "/"), body: []byte("invalid\n")},
				{name: strings.TrimPrefix(codexReviewEventsPath, "/"), body: []byte("terminal\n")},
			},
		},
		{
			name: "missing events",
			entries: []tarEntry{
				{name: strings.TrimPrefix(codexReviewStatusPath, "/"), body: []byte("0\n")},
				{name: strings.TrimPrefix(codexReviewResultPath, "/"), body: []byte(`{"findings":[]}`)},
			},
		},
		{
			name: "missing result",
			entries: []tarEntry{
				{name: strings.TrimPrefix(codexReviewStatusPath, "/"), body: []byte("0\n")},
				{name: strings.TrimPrefix(codexReviewEventsPath, "/"), body: []byte("terminal\n")},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			fx := newHandoffFixture(t)
			seedSpec := fx.seed(t)
			backend := fx.backend(t)
			cfg, requestSpec := testCodexReview(t)
			journal := &fakeCodexReviewJournal{}
			sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal)
			source, err := NewCodexReviewSource(sourceConfig)
			if err != nil {
				t.Fatal(err)
			}
			id := domain.InvocationID("review-run-raw-1")
			request := exec.ReviewRequest{
				RunID: "run-raw", Round: 1, Repo: seedSpec.Seed.Base.Repo,
				RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
				BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
				Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(),
				RequestedAt: codexReviewEpoch.Add(-time.Minute),
			}
			if err := source.RequestReview(ctx, id, request); err != nil {
				t.Fatal(err)
			}
			fx.rt.exportTarPath = buildTar(t, tc.entries)
			binding, err := journal.GetCodexReviewBinding(ctx, string(id))
			if err != nil {
				t.Fatal(err)
			}
			failedCleanup := false
			fx.rt.onDeleteVolume = func(name string) (bool, error) {
				if name == binding.WorkspaceVolume && !failedCleanup {
					failedCleanup = true
					return true, errors.New("runtime cleanup temporarily unavailable")
				}
				return false, nil
			}
			status, err := source.Inspect(ctx, id)
			for attempts := 0; err == nil && status != exec.StatusFailed && attempts < 5; attempts++ {
				status, err = source.Inspect(ctx, id)
			}
			if err != nil || status != exec.StatusFailed {
				t.Fatalf("malformed raw output status = %q, %v", status, err)
			}
			outcome, ready, err := journal.GetCodexReviewOutcome(ctx, string(id))
			if err != nil || !ready || outcome.Result != nil ||
				outcome.FailureClass != domain.ReviewFailureContradiction ||
				!strings.Contains(outcome.Failure, "invalid raw output") {
				t.Fatalf("persisted malformed-output outcome = %#v, ready=%v, %v", outcome, ready, err)
			}
			containers, _ := fx.rt.ListContainers(ctx)
			volumes, _ := fx.rt.ListVolumes(ctx)
			networks, _ := fx.rt.ListNetworks(ctx)
			if len(containers) != 0 || len(volumes) != 0 || len(networks) != 0 {
				t.Fatalf("malformed-output topology leaked: containers=%v volumes=%v networks=%v",
					containers, volumes, networks)
			}
		})
	}
}

func TestCodexReviewSourceRetriesOperationalBindingRead(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal)
	source, err := NewCodexReviewSource(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-run-binding-read-1")
	request := exec.ReviewRequest{
		RunID: "run-binding-read", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(),
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	if err := source.RequestReview(ctx, id, request); err != nil {
		t.Fatal(err)
	}
	journal.failGetBinding = errors.New("SQLite temporarily unavailable")
	var failure *exec.ReviewSourceFailure
	if _, err := source.Inspect(ctx, id); !errors.As(err, &failure) ||
		failure.Class != domain.ReviewFailureTransient ||
		!errors.Is(failure.Err, ErrCodexReviewOperational) {
		t.Fatalf("operational binding read = %v", err)
	}
	if len(fx.rt.ctrs) == 0 {
		t.Fatal("operational binding read tore down the retryable review topology")
	}
	journal.failGetBinding = errors.Join(ErrConformance, errors.New("decoded binding is invalid"))
	if _, err := source.Inspect(ctx, id); !errors.As(err, &failure) ||
		failure.Class != domain.ReviewFailureContradiction ||
		errors.Is(failure.Err, ErrCodexReviewOperational) {
		t.Fatalf("rejected binding row = %v", err)
	}
	if len(fx.rt.ctrs) == 0 {
		t.Fatal("rejected binding row tore down unauthenticated topology")
	}
	journal.failGetBinding = nil
	if _, err := source.Inspect(ctx, id); err != nil {
		t.Fatalf("binding read retry = %v", err)
	}
}

func TestCodexReviewSourceCleansBeforeRejectingMalformedOutcome(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal)
	source, err := NewCodexReviewSource(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-run-outcome-row-1")
	request := exec.ReviewRequest{
		RunID: "run-outcome-row", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(),
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	if err := source.RequestReview(ctx, id, request); err != nil {
		t.Fatal(err)
	}
	if err := journal.PutCodexReviewOutcome(ctx, string(id), CodexReviewSourceOutcome{
		InvocationID: id, FailureClass: domain.ReviewFailureContradiction,
		Failure: "collected outcome was corrupted before cleanup", AbortRequired: true,
	}); err != nil {
		t.Fatal(err)
	}
	binding, err := journal.GetCodexReviewBinding(ctx, string(id))
	if err != nil {
		t.Fatal(err)
	}
	failedCleanup := false
	fx.rt.onDeleteVolume = func(name string) (bool, error) {
		if name == binding.WorkspaceVolume && !failedCleanup {
			failedCleanup = true
			return true, errors.New("runtime cleanup temporarily unavailable")
		}
		return false, nil
	}
	journal.failGetOutcome = errors.Join(
		ErrCodexReviewOutcomeRejected, errors.New("decode persisted outcome"))
	var failure *exec.ReviewSourceFailure
	if status, err := source.Inspect(ctx, id); err != nil || status != exec.StatusPending {
		t.Fatalf("malformed outcome cleanup retry = %q, %v", status, err)
	}
	if _, err := source.Inspect(ctx, id); !errors.As(err, &failure) ||
		failure.Class != domain.ReviewFailureContradiction ||
		!errors.Is(failure.Err, ErrCodexReviewOutcomeRejected) {
		t.Fatalf("malformed persisted outcome = %v", err)
	}
	if !journal.ready[string(id)] {
		t.Fatal("malformed persisted outcome was not marked ready after cleanup")
	}
	containers, _ := fx.rt.ListContainers(ctx)
	volumes, _ := fx.rt.ListVolumes(ctx)
	networks, _ := fx.rt.ListNetworks(ctx)
	if len(containers) != 0 || len(volumes) != 0 || len(networks) != 0 {
		t.Fatalf("malformed-outcome topology leaked: containers=%v volumes=%v networks=%v",
			containers, volumes, networks)
	}
}

func TestCodexReviewSourceAbortsStartedInvocationForRejectedRequest(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal)
	source, err := NewCodexReviewSource(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-run-rejected-1")
	request := exec.ReviewRequest{
		RunID: "run-rejected", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(),
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	if err := source.RequestReview(ctx, id, request); err != nil {
		t.Fatal(err)
	}
	authority, err := request.AuthorityDigest()
	if err != nil {
		t.Fatal(err)
	}
	if err := source.VerifyRequestAuthority(ctx, id, authority); err != nil {
		t.Fatalf("authentic request authority = %v", err)
	}
	// Rewrite the persisted request to a still-valid body for another head:
	// exactly the tamper the engine's pre-Inspect gate rejects. The rejection
	// must abort the review the original request already started instead of
	// stranding the credential-bearing topology behind the terminal
	// contradiction.
	rewritten := request
	rewritten.HeadSHA = strings.Repeat("b", 40)
	if err := journal.PutCodexReviewRequest(ctx, string(id), rewritten); err != nil {
		t.Fatal(err)
	}
	intent, err := journal.GetCodexReviewIntent(ctx, string(id))
	if err != nil {
		t.Fatal(err)
	}
	// The first rejection meets a failing runtime: the abort stays durable and
	// transient, then converges on the retry instead of leaking.
	failedCleanup := false
	fx.rt.onDeleteContainer = func(container string) (bool, error) {
		if container == intent.ReviewContainer && !failedCleanup {
			failedCleanup = true
			return true, errors.New("runtime cleanup temporarily unavailable")
		}
		return false, nil
	}
	var failure *exec.ReviewSourceFailure
	if err := source.VerifyRequestAuthority(ctx, id, authority); !errors.As(err, &failure) ||
		failure.Class != domain.ReviewFailureTransient {
		t.Fatalf("rejection with failing teardown = %v", err)
	}
	outcome, ready, err := journal.GetCodexReviewOutcome(ctx, string(id))
	if err != nil || ready || !outcome.AbortRequired ||
		outcome.FailureClass != domain.ReviewFailureContradiction ||
		!strings.Contains(outcome.Failure, "rejected after launch") {
		t.Fatalf("persisted rejection outcome = %#v, ready=%v, %v", outcome, ready, err)
	}
	fx.rt.onDeleteContainer = nil
	if err := source.VerifyRequestAuthority(ctx, id, authority); !errors.As(err, &failure) ||
		failure.Class != domain.ReviewFailureContradiction ||
		!errors.Is(failure.Err, domain.ErrParentKeyMismatch) {
		t.Fatalf("rejection after teardown = %v", err)
	}
	if _, ready, err = journal.GetCodexReviewOutcome(ctx, string(id)); err != nil || !ready {
		t.Fatalf("rejection outcome ready = %v, %v", ready, err)
	}
	containers, _ := fx.rt.ListContainers(ctx)
	volumes, _ := fx.rt.ListVolumes(ctx)
	networks, _ := fx.rt.ListNetworks(ctx)
	if len(containers) != 0 || len(volumes) != 0 || len(networks) != 0 {
		t.Fatalf("rejected topology leaked: containers=%v volumes=%v networks=%v",
			containers, volumes, networks)
	}
}

func TestCodexReviewSourceAbortsPreHandoffLaunchForRejectedRequest(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal)
	source, err := NewCodexReviewSource(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-run-rejected-3")
	request := exec.ReviewRequest{
		RunID: "run-rejected", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(),
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	if err := source.RequestReview(ctx, id, request); err != nil {
		t.Fatal(err)
	}
	authority, err := request.AuthorityDigest()
	if err != nil {
		t.Fatal(err)
	}
	rewritten := request
	rewritten.HeadSHA = strings.Repeat("b", 40)
	if err := journal.PutCodexReviewRequest(ctx, string(id), rewritten); err != nil {
		t.Fatal(err)
	}
	// A crash between container start and the started handoff record leaves
	// the intent in `starting` with the durable binding already persisted.
	// The rejection must still abort through that binding rather than
	// stranding the running review.
	journal.intent.State = CodexReviewIntentStarting
	var failure *exec.ReviewSourceFailure
	if err := source.VerifyRequestAuthority(ctx, id, authority); !errors.As(err, &failure) ||
		failure.Class != domain.ReviewFailureContradiction ||
		!errors.Is(failure.Err, domain.ErrParentKeyMismatch) {
		t.Fatalf("pre-handoff rejection = %v", err)
	}
	outcome, ready, err := journal.GetCodexReviewOutcome(ctx, string(id))
	if err != nil || !ready || !outcome.AbortRequired ||
		outcome.FailureClass != domain.ReviewFailureContradiction {
		t.Fatalf("pre-handoff rejection outcome = %#v, ready=%v, %v", outcome, ready, err)
	}
	containers, _ := fx.rt.ListContainers(ctx)
	volumes, _ := fx.rt.ListVolumes(ctx)
	networks, _ := fx.rt.ListNetworks(ctx)
	if len(containers) != 0 || len(volumes) != 0 || len(networks) != 0 {
		t.Fatalf("pre-handoff rejected topology leaked: containers=%v volumes=%v networks=%v",
			containers, volumes, networks)
	}
}

func TestCodexReviewSourceRejectedRequestStaysLoudBeforeBinding(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal)
	source, err := NewCodexReviewSource(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-run-rejected-4")
	request := exec.ReviewRequest{
		RunID: "run-rejected", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(),
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	if err := source.RequestReview(ctx, id, request); err != nil {
		t.Fatal(err)
	}
	authority, err := request.AuthorityDigest()
	if err != nil {
		t.Fatal(err)
	}
	rewritten := request
	rewritten.HeadSHA = strings.Repeat("b", 40)
	if err := journal.PutCodexReviewRequest(ctx, string(id), rewritten); err != nil {
		t.Fatal(err)
	}
	// Before the durable binding exists nothing can authenticate a teardown,
	// so the rejection stays loud without one: the recorded
	// topology-contradiction boundary.
	journal.intent.State = CodexReviewIntentPreparing
	var failure *exec.ReviewSourceFailure
	err = source.VerifyRequestAuthority(ctx, id, authority)
	if !errors.As(err, &failure) || failure.Class != domain.ReviewFailureContradiction ||
		!strings.Contains(failure.Err.Error(), "pre-binding") {
		t.Fatalf("pre-binding rejection = %v", err)
	}
	if _, _, err := journal.GetCodexReviewOutcome(ctx, string(id)); !errors.Is(err, ErrCodexReviewOutcomeNotFound) {
		t.Fatalf("pre-binding rejection outcome = %v", err)
	}
	containers, _ := fx.rt.ListContainers(ctx)
	if len(containers) == 0 {
		t.Fatal("pre-binding rejection must not tear down an unauthenticatable topology")
	}
}

func TestCodexReviewSourceInspectAbortsInvocationForInvalidPersistedRequest(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal)
	source, err := NewCodexReviewSource(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-run-rejected-5")
	request := exec.ReviewRequest{
		RunID: "run-rejected", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(),
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	if err := source.RequestReview(ctx, id, request); err != nil {
		t.Fatal(err)
	}
	// Model the production adapter rejecting a corrupt decoded row before it can
	// return a ReviewRequest. Inspect must route that sentinel through the same
	// authenticated teardown used for a validly decoded authority mismatch.
	journal.failGetRequest = errors.Join(
		ErrCodexReviewRequestRejected, errors.New("decode persisted request"))
	var failure *exec.ReviewSourceFailure
	if _, err := source.Inspect(ctx, id); !errors.As(err, &failure) ||
		failure.Class != domain.ReviewFailureContradiction {
		t.Fatalf("inspect of invalid persisted request = %v", err)
	}
	outcome, ready, err := journal.GetCodexReviewOutcome(ctx, string(id))
	if err != nil || !ready || !outcome.AbortRequired ||
		outcome.FailureClass != domain.ReviewFailureContradiction {
		t.Fatalf("inspect rejection outcome = %#v, ready=%v, %v", outcome, ready, err)
	}
	containers, _ := fx.rt.ListContainers(ctx)
	volumes, _ := fx.rt.ListVolumes(ctx)
	networks, _ := fx.rt.ListNetworks(ctx)
	if len(containers) != 0 || len(volumes) != 0 || len(networks) != 0 {
		t.Fatalf("inspect-rejected topology leaked: containers=%v volumes=%v networks=%v",
			containers, volumes, networks)
	}
}

func TestCodexReviewSourceReapsPreparedWorkspaceForRejectedUnstartedRequest(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal)
	source, err := NewCodexReviewSource(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-run-rejected-2")
	request := exec.ReviewRequest{
		RunID: "run-rejected", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(),
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	// Persist and prepare without ever launching: the window between workspace
	// preparation and container start. A rejection here reaps the prepared
	// volume and needs no durable outcome, because nothing credential-bearing
	// ever existed.
	if err := journal.PutCodexReviewRequest(ctx, string(id), request); err != nil {
		t.Fatal(err)
	}
	candidate := domain.BaseRevision{
		Repo: request.Repo, RepositoryID: request.RepositoryID,
		BaseRef: request.BaseRef, BaseSHA: request.HeadSHA,
	}
	if _, err := backend.PrepareCodexReviewWorkspace(
		ctx, journal, string(id), request.Workspace, candidate, sourceConfig.WorkspaceSizeMB,
	); err != nil {
		t.Fatal(err)
	}
	var failure *exec.ReviewSourceFailure
	err = source.VerifyRequestAuthority(ctx, id, domain.Digest("sha256:"+strings.Repeat("e", 64)))
	if !errors.As(err, &failure) || failure.Class != domain.ReviewFailureContradiction ||
		!errors.Is(failure.Err, domain.ErrParentKeyMismatch) {
		t.Fatalf("unstarted rejection = %v", err)
	}
	if _, _, err := journal.GetCodexReviewOutcome(ctx, string(id)); !errors.Is(err, ErrCodexReviewOutcomeNotFound) {
		t.Fatalf("unstarted rejection outcome = %v", err)
	}
	volumes, _ := fx.rt.ListVolumes(ctx)
	if len(volumes) != 0 {
		t.Fatalf("prepared workspace leaked: %v", volumes)
	}
	// A rejected request with no workspace at all tolerates the absence and
	// still reports the rejection.
	bare := domain.InvocationID("review-run-rejected-bare")
	if err := journal.PutCodexReviewRequest(ctx, string(bare), request); err != nil {
		t.Fatal(err)
	}
	err = source.VerifyRequestAuthority(ctx, bare, domain.Digest("sha256:"+strings.Repeat("e", 64)))
	if !errors.As(err, &failure) || failure.Class != domain.ReviewFailureContradiction ||
		!errors.Is(failure.Err, domain.ErrParentKeyMismatch) {
		t.Fatalf("bare rejection = %v", err)
	}
}

func TestCodexReviewCleanupRefusesRedirectedWorkspaceBinding(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal)
	source, err := NewCodexReviewSource(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-run-redirect-1")
	request := exec.ReviewRequest{
		RunID: "run-redirect", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(),
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	if err := source.RequestReview(ctx, id, request); err != nil {
		t.Fatal(err)
	}
	// Rewrite the durable binding so its workspace fields identify a sibling
	// invocation's prepared volume, complete with the sibling's own valid
	// ownership evidence. Cleanup must refuse the redirection instead of
	// deleting the sibling's volume.
	siblingVolume := namesFor("review-run-sibling").Workspace
	siblingOwner := testOwnershipLabel()
	fx.rt.vols[siblingVolume] = &fakeVol{
		labels: append(runLabels("review-run-sibling"), siblingOwner), created: "sibling-created",
	}
	binding, err := journal.GetCodexReviewBinding(ctx, string(id))
	if err != nil {
		t.Fatal(err)
	}
	binding.WorkspaceSourceRunID = "review-run-sibling"
	binding.WorkspaceVolume = siblingVolume
	journal.binding = binding
	journal.workspaceBinding = CodexReviewWorkspaceBinding{
		SourceRunID: "review-run-sibling", Volume: siblingVolume,
		OwnershipToken: siblingOwner.Value, CreationFingerprint: "sibling-created",
	}
	if err := backend.AbortCodexReview(ctx, sourceConfig.Review, string(id)); !errors.Is(err, ErrConformance) {
		t.Fatalf("abort with redirected workspace binding = %v", err)
	}
	if _, ok := fx.rt.vols[siblingVolume]; !ok {
		t.Fatal("redirected cleanup deleted the sibling invocation's workspace volume")
	}
}

func TestCodexReviewCleanupRefusesSubstitutedIntentResources(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal)
	source, err := NewCodexReviewSource(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-run-redirect-2")
	request := exec.ReviewRequest{
		RunID: "run-redirect", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(),
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	if err := source.RequestReview(ctx, id, request); err != nil {
		t.Fatal(err)
	}
	// Rewrite the intent so its shadow volume and network carry unused
	// CLI-safe substitute names. Cleanup must refuse rather than treat the
	// substitutes as already absent and close the intent while the real
	// resources stay live.
	realShadow := journal.intent.ShadowVolume
	realNetwork := journal.intent.Network
	journal.intent.ShadowVolume = "freeside-review-substitute-agents"
	journal.intent.Network = "freeside-review-substitute-egress"
	if err := backend.AbortCodexReview(ctx, sourceConfig.Review, string(id)); !errors.Is(err, ErrConformance) {
		t.Fatalf("abort with substituted intent resources = %v", err)
	}
	if _, ok := fx.rt.vols[realShadow]; !ok {
		t.Fatal("substituted-intent cleanup lost the real shadow volume")
	}
	if _, ok := fx.rt.nets[realNetwork]; !ok {
		t.Fatal("substituted-intent cleanup lost the real network")
	}
}

func TestCodexReviewCleanupRefusesMissingIntentAuthority(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal)
	source, err := NewCodexReviewSource(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-run-intent-authority-1")
	request := exec.ReviewRequest{
		RunID: "run-intent-authority", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(),
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	if err := source.RequestReview(ctx, id, request); err != nil {
		t.Fatal(err)
	}
	journal.failGetIntent = errors.Join(ErrConformance, errors.New("intent body authority mismatch"))
	deleteCalls := 0
	fx.rt.onDeleteContainer = func(string) (bool, error) { deleteCalls++; return false, nil }
	fx.rt.onDeleteVolume = func(string) (bool, error) { deleteCalls++; return false, nil }
	fx.rt.onDeleteNetwork = func(string) (bool, error) { deleteCalls++; return false, nil }
	if err := backend.AbortCodexReview(ctx, sourceConfig.Review, string(id)); !errors.Is(err, ErrConformance) || errors.Is(err, ErrCodexReviewOperational) {
		t.Fatalf("abort without intent authority = %v", err)
	}
	if deleteCalls != 0 {
		t.Fatalf("cleanup issued %d deletes without intent authority", deleteCalls)
	}
}

func TestCodexReviewCleanupSurfacesForeignLeaseAsContradiction(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal)
	source, err := NewCodexReviewSource(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-run-lease-1")
	request := exec.ReviewRequest{
		RunID: "run-lease", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(),
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	if err := source.RequestReview(ctx, id, request); err != nil {
		t.Fatal(err)
	}
	// An authenticated lease refusal at terminal cleanup is a contradiction,
	// not operational I/O: wrapping it transient would retry silently forever.
	tampered := sourceConfig.Review
	tampered.VolumeLifecycleLeaser = &fakeCodexReviewVolumeLeaser{
		rt: fx.rt, recoverErr: ErrCodexReviewVolumeLeaseForeignOwner,
	}
	if err := backend.AbortCodexReview(ctx, tampered, string(id)); !errors.Is(err, ErrConformance) {
		t.Fatalf("abort with foreign terminal lease = %v", err)
	}
	tampered.VolumeLifecycleLeaser = &fakeCodexReviewVolumeLeaser{
		rt: fx.rt, recoverErr: ErrCodexReviewVolumeLeaseTransferred,
	}
	if err := backend.AbortCodexReview(ctx, tampered, string(id)); !errors.Is(err, ErrConformance) {
		t.Fatalf("abort with still-transferred terminal lease = %v", err)
	}
	// A genuine operational refusal still stays retryable.
	tampered.VolumeLifecycleLeaser = &fakeCodexReviewVolumeLeaser{
		rt: fx.rt, recoverErr: errors.New("runtime lease bookkeeping unavailable"),
	}
	err = backend.AbortCodexReview(ctx, tampered, string(id))
	if !errors.Is(err, ErrCodexReviewOperational) || errors.Is(err, ErrConformance) {
		t.Fatalf("abort with operational lease failure = %v", err)
	}
}

func TestCodexReviewRecoveryAbortsRunningInvocationWithLostProxy(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal)
	source, err := NewCodexReviewSource(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-run-restart-1")
	request := exec.ReviewRequest{
		RunID: "run-restart", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(),
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	if err := source.RequestReview(ctx, id, request); err != nil {
		t.Fatal(err)
	}
	source.mu.Lock()
	_ = source.launches[id].Close()
	delete(source.launches, id)
	source.mu.Unlock()
	volumeLeaser, err := NewRuntimeCodexReviewVolumeLeaser(fx.rt)
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := NewCodexReviewRecovery(backend, journal, volumeLeaser)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := journal.GetCodexReviewIntent(ctx, string(id))
	if err != nil {
		t.Fatal(err)
	}
	failedCleanup := false
	fx.rt.onDeleteContainer = func(container string) (bool, error) {
		if container == intent.ReviewContainer && !failedCleanup {
			failedCleanup = true
			return true, errors.New("runtime cleanup temporarily unavailable")
		}
		return false, nil
	}
	if err := recovery.Reconcile(ctx); !errors.Is(err, ErrCodexReviewOperational) {
		t.Fatalf("restart transient abort = %v", err)
	}
	if _, ready, err := journal.GetCodexReviewOutcome(ctx, string(id)); err != nil || ready {
		t.Fatalf("outcome after transient abort = ready %v, %v", ready, err)
	}
	fx.rt.onDeleteContainer = nil
	if err := recovery.Reconcile(ctx); err != nil {
		t.Fatalf("restart recovery retry = %v", err)
	}
	outcome, ready, err := journal.GetCodexReviewOutcome(ctx, string(id))
	if err != nil || !ready || outcome.FailureClass != domain.ReviewFailureTransient || !outcome.AbortRequired {
		t.Fatalf("restart outcome = %#v, ready %v, %v", outcome, ready, err)
	}
	containers, _ := fx.rt.ListContainers(ctx)
	volumes, _ := fx.rt.ListVolumes(ctx)
	networks, _ := fx.rt.ListNetworks(ctx)
	if len(containers) != 0 || len(volumes) != 0 || len(networks) != 0 {
		t.Fatalf("aborted topology leaked: containers=%v volumes=%v networks=%v", containers, volumes, networks)
	}
}

func TestCodexReviewSourceRestartAbortsRunningInvocationWithLostProxy(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	sourceConfig := codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal)
	source, err := NewCodexReviewSource(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-run-restart-source")
	request := exec.ReviewRequest{
		RunID: "run-restart-source", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(),
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	if err := source.RequestReview(ctx, id, request); err != nil {
		t.Fatal(err)
	}
	source.mu.Lock()
	_ = source.launches[id].Close()
	delete(source.launches, id)
	source.mu.Unlock()
	restartedConfig := source.cfg
	restartedConfig.Review.VolumeLifecycleLeaser, err = NewRuntimeCodexReviewVolumeLeaser(fx.rt)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := NewCodexReviewSource(restartedConfig)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := journal.GetCodexReviewIntent(ctx, string(id))
	if err != nil {
		t.Fatal(err)
	}
	failedCleanup := false
	fx.rt.onDeleteContainer = func(container string) (bool, error) {
		if container == intent.ReviewContainer && !failedCleanup {
			failedCleanup = true
			return true, errors.New("runtime cleanup temporarily unavailable")
		}
		return false, nil
	}
	status, err := restarted.Inspect(ctx, id)
	if err != nil || status != exec.StatusPending {
		t.Fatalf("restart transient abort = %q, %v", status, err)
	}
	if _, err := restarted.Poll(ctx, id); !errors.Is(err, exec.ErrResultNotReady) {
		t.Fatalf("poll after transient abort = %v", err)
	}
	fx.rt.onDeleteContainer = nil
	status, err = restarted.Inspect(ctx, id)
	if err != nil || status != exec.StatusFailed {
		t.Fatalf("restart inspect = %q, %v", status, err)
	}
	_, err = restarted.Poll(ctx, id)
	var failure *exec.ReviewSourceFailure
	if !errors.As(err, &failure) || failure.Class != domain.ReviewFailureTransient {
		t.Fatalf("restart poll failure = %v", err)
	}
}

func TestCodexReviewRecoveryCleansBeforeReportingRejectedOutcome(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	source, err := NewCodexReviewSource(
		codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal),
	)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-run-rejected-outcome")
	request := exec.ReviewRequest{
		RunID: "run-rejected", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(),
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	if err := source.RequestReview(ctx, id, request); err != nil {
		t.Fatal(err)
	}
	source.mu.Lock()
	_ = source.launches[id].Close()
	delete(source.launches, id)
	source.mu.Unlock()
	journal.failGetOutcome = ErrCodexReviewOutcomeRejected
	volumeLeaser, err := NewRuntimeCodexReviewVolumeLeaser(fx.rt)
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := NewCodexReviewRecovery(backend, journal, volumeLeaser)
	if err != nil {
		t.Fatal(err)
	}
	if err := recovery.Reconcile(ctx); !errors.Is(err, ErrCodexReviewOutcomeRejected) {
		t.Fatalf("rejected outcome recovery = %v", err)
	}
	if journal.intent.State != CodexReviewIntentClosed {
		t.Fatalf("rejected outcome left intent %q, want closed", journal.intent.State)
	}
	containers, _ := fx.rt.ListContainers(ctx)
	volumes, _ := fx.rt.ListVolumes(ctx)
	networks, _ := fx.rt.ListNetworks(ctx)
	if len(containers) != 0 || len(volumes) != 0 || len(networks) != 0 {
		t.Fatalf("rejected outcome leaked topology: containers=%v volumes=%v networks=%v",
			containers, volumes, networks)
	}
}

func TestCodexReviewSourceNormalizesStructuredFindings(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	source := &CodexReviewSource{cfg: CodexReviewSourceConfig{
		Review:              CodexReviewConfig{Model: "gpt-codex", ReasoningEffort: "high"},
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		CostOwner:           "subscription:owner", Now: func() time.Time { return now },
	}}
	request := exec.ReviewRequest{
		RunID: "run-1", Round: 1, Repo: "owner/repo", RepositoryID: 42,
		BaseRef: "main", BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40),
		Workspace: "/seed/candidate", Verification: testReviewVerificationEvidence(),
		RequestedAt: now.Add(-time.Minute),
	}
	collection := CodexReviewCollection{
		Result: []byte(`{"findings":[{"severity":"P2","location":"daemon/main.go:12","explanation":"unchecked error"}]}`),
		Events: []byte("terminal event\n"),
	}
	first := source.normalizeCollection("review-run-1-1", request, collection)
	second := source.normalizeCollection("review-run-1-1", request, collection)
	if err := first.Validate(); err != nil {
		t.Fatal(err)
	}
	if first.Result == nil || len(first.Result.Findings) != 1 {
		t.Fatalf("normalized outcome = %#v", first)
	}
	finding := first.Result.Findings[0]
	if finding.ID != second.Result.Findings[0].ID || finding.RunID != request.RunID ||
		finding.Source != "codex_local" || finding.Severity != "P2" ||
		first.Result.BaseSHA != request.BaseSHA || first.Result.HeadSHA != request.HeadSHA ||
		first.Result.Provider != "openai" || first.Result.ModelConfiguration != "gpt-codex/high" ||
		first.Result.CompletionEvidence == "" {
		t.Fatalf("normalized result = %#v", first.Result)
	}
}

func TestCodexReviewSourceRejectsInvalidFindingsEnvelope(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	source := &CodexReviewSource{cfg: CodexReviewSourceConfig{
		Review:              CodexReviewConfig{Model: "gpt-codex", ReasoningEffort: "high"},
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		CostOwner:           "subscription:owner", Now: func() time.Time { return now },
	}}
	request := exec.ReviewRequest{
		RunID: "run-1", Round: 1, Repo: "owner/repo", RepositoryID: 42,
		BaseRef: "main", BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40),
		Workspace: "/seed/candidate", Verification: testReviewVerificationEvidence(),
		RequestedAt: now.Add(-time.Minute),
	}
	for _, tc := range []struct {
		name, result, failure string
	}{
		{"missing", `{}`, "required findings array"},
		{"null", `{"findings":null}`, "required findings array"},
		{
			"duplicate",
			`{"findings":[{"severity":"P1","location":"main.go:1","explanation":"unsafe"}],"findings":[]}`,
			"malformed structured output",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outcome := source.normalizeCollection("review-run-1-1", request, CodexReviewCollection{
				Result: []byte(tc.result), Events: []byte("terminal event\n"),
			})
			if outcome.Result != nil || outcome.FailureClass != domain.ReviewFailureContradiction ||
				!strings.Contains(outcome.Failure, tc.failure) {
				t.Fatalf("result %s normalized as %#v", tc.result, outcome)
			}
		})
	}
	clean := source.normalizeCollection("review-run-1-1", request, CodexReviewCollection{
		Result: []byte(`{"findings":[]}`), Events: []byte("terminal event\n"),
	})
	if clean.Result == nil || len(clean.Result.Findings) != 0 || clean.FailureClass != "" {
		t.Fatalf("empty findings array = %#v", clean)
	}
}

func TestCodexReviewSourceFindingIdentityIsInvocationScoped(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	source := &CodexReviewSource{cfg: CodexReviewSourceConfig{
		Review:              CodexReviewConfig{Model: "gpt-codex", ReasoningEffort: "high"},
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		CostOwner:           "owner", Now: func() time.Time { return now },
	}}
	request := exec.ReviewRequest{
		RunID: "run-1", Round: 1, Repo: "owner/repo", RepositoryID: 42, BaseRef: "main",
		BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), Workspace: "/candidate",
		Verification: testReviewVerificationEvidence(), RequestedAt: now,
	}
	collection := CodexReviewCollection{Result: []byte(
		`{"findings":[{"severity":"P2","location":"daemon/main.go:12","explanation":"unchecked error"}]}`,
	)}
	first := source.normalizeCollection("review-invocation-1", request, collection)
	request.RunID = "run-2"
	second := source.normalizeCollection("review-invocation-2", request, collection)
	if first.Result.Findings[0].ID == second.Result.Findings[0].ID {
		t.Fatalf("finding identity crossed invocations: %q", first.Result.Findings[0].ID)
	}
}

func TestCodexReviewSourceClassifiesTerminalFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events string
		want   domain.ReviewFailureClass
	}{
		{"quota", "rate limit exceeded", domain.ReviewFailureQuota},
		{"configuration", "authentication failed", domain.ReviewFailureConfiguration},
		{"transient", "connection reset", domain.ReviewFailureTransient},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyCodexTerminalFailure([]byte(tc.events)); got != tc.want {
				t.Fatalf("class = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCodexReviewSourceClassifiesOutcomeWritesAndCleanup(t *testing.T) {
	journalErr := errors.New("journal temporarily unavailable")
	var failure *exec.ReviewSourceFailure
	if err := codexReviewOutcomeWriteFailure(journalErr); !errors.As(err, &failure) ||
		failure.Class != domain.ReviewFailureTransient || !errors.Is(err, journalErr) {
		t.Fatalf("outcome write failure = %v", err)
	}

	status, err := codexReviewCleanupStatus(errors.New("runtime temporarily unavailable"))
	if err != nil || status != exec.StatusPending {
		t.Fatalf("operational cleanup = %q, %v", status, err)
	}
	conformanceErr := fmt.Errorf("foreign cleanup topology: %w", ErrConformance)
	status, err = codexReviewCleanupStatus(conformanceErr)
	failure = nil
	if status != "" || !errors.As(err, &failure) ||
		failure.Class != domain.ReviewFailureContradiction || !errors.Is(err, conformanceErr) {
		t.Fatalf("contradictory cleanup = %q, %v", status, err)
	}
}

func TestCodexReviewLaunchCleanupFailureDoesNotTerminalizeUnreapedWorkspace(t *testing.T) {
	launchErr := fmt.Errorf("invalid review launch: %w", ErrInvalidCodexReviewSpec)
	transient := codexReviewLaunchCleanupFailure(launchErr, errors.New("runtime temporarily unavailable"))
	if exec.ClassifyReviewSourceFailure(transient) != domain.ReviewFailureTransient ||
		!errors.Is(transient, launchErr) {
		t.Fatalf("transient cleanup classification = %v", transient)
	}
	contradiction := codexReviewLaunchCleanupFailure(launchErr, fmt.Errorf("foreign volume: %w", ErrConformance))
	if exec.ClassifyReviewSourceFailure(contradiction) != domain.ReviewFailureContradiction ||
		!errors.Is(contradiction, launchErr) {
		t.Fatalf("contradictory cleanup classification = %v", contradiction)
	}
}

func TestCodexReviewSourceJournalReadsKeepResultPending(t *testing.T) {
	id := domain.InvocationID("review-journal-read-1")
	request := exec.ReviewRequest{
		RunID: "run-journal-read", Round: 1, Repo: "owner/repo", RepositoryID: 42,
		BaseRef: "main", BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40),
		Workspace: "/seed/candidate", Verification: testReviewVerificationEvidence(),
		RequestedAt: codexReviewEpoch,
	}
	journal := &fakeCodexReviewJournal{requests: map[string]exec.ReviewRequest{string(id): request}}
	source := &CodexReviewSource{cfg: CodexReviewSourceConfig{Journal: journal}}
	readErr := errors.New("journal temporarily unavailable")
	journal.failGetOutcome = readErr
	if _, err := source.Poll(context.Background(), id); !errors.Is(err, exec.ErrResultNotReady) ||
		exec.ClassifyReviewSourceFailure(err) != domain.ReviewFailureTransient || !errors.Is(err, readErr) {
		t.Fatalf("poll journal failure = %v", err)
	}
	journal.failGetOutcome = nil
	journal.failGetRequest = readErr
	err := source.Verify(context.Background(), id, request.BaseSHA, request.HeadSHA)
	if exec.ClassifyReviewSourceFailure(err) != domain.ReviewFailureTransient || !errors.Is(err, readErr) {
		t.Fatalf("verify journal failure = %v", err)
	}
}

func TestCodexReviewSourceRecoversBeforeLaunchIntent(t *testing.T) {
	for _, preparation := range []string{"absent", "pending", "finalized"} {
		t.Run(preparation, func(t *testing.T) {
			ctx := context.Background()
			fx := newHandoffFixture(t)
			seedSpec := fx.seed(t)
			backend := fx.backend(t)
			cfg, requestSpec := testCodexReview(t)
			journal := &fakeCodexReviewJournal{}
			id := domain.InvocationID("review-recover-" + preparation)
			request := exec.ReviewRequest{
				RunID: "run-recover", Round: 1, Repo: seedSpec.Seed.Base.Repo,
				RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
				BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
				Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(),
				RequestedAt: codexReviewEpoch.Add(-time.Minute),
			}
			if err := journal.PutCodexReviewRequest(ctx, string(id), request); err != nil {
				t.Fatal(err)
			}
			var priorFingerprint string
			switch preparation {
			case "pending":
				owner := testOwnershipLabel()
				binding := CodexReviewWorkspaceBinding{
					SourceRunID: string(id), Volume: namesFor(string(id)).Workspace,
					OwnershipToken: owner.Value,
				}
				if err := journal.PutCodexReviewWorkspaceBinding(ctx, binding); err != nil {
					t.Fatal(err)
				}
				if err := fx.rt.CreateVolume(ctx, binding.Volume, 64,
					append(runLabels(string(id)), owner)); err != nil {
					t.Fatal(err)
				}
				view, err := fx.rt.InspectVolume(ctx, binding.Volume)
				if err != nil {
					t.Fatal(err)
				}
				priorFingerprint = view.CreationDate
			case "finalized":
				binding, err := backend.PrepareCodexReviewWorkspace(
					ctx, journal, string(id), request.Workspace,
					domain.BaseRevision{
						Repo: request.Repo, RepositoryID: request.RepositoryID,
						BaseRef: request.BaseRef, BaseSHA: request.HeadSHA,
					}, 64,
				)
				if err != nil {
					t.Fatal(err)
				}
				priorFingerprint = binding.CreationFingerprint
			}
			source, err := NewCodexReviewSource(
				codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal),
			)
			if err != nil {
				t.Fatal(err)
			}
			status, err := source.Inspect(ctx, id)
			if err != nil || status != exec.StatusRunning {
				t.Fatalf("recovered status = %q, %v", status, err)
			}
			workspace, err := journal.GetCodexReviewWorkspaceBinding(ctx, string(id))
			if err != nil || workspace.CreationFingerprint == "" {
				t.Fatalf("recovered workspace = %#v, %v", workspace, err)
			}
			if preparation == "pending" && workspace.CreationFingerprint == priorFingerprint {
				t.Fatal("pending partial workspace was adopted instead of reconstructed")
			}
			if preparation == "finalized" && workspace.CreationFingerprint != priorFingerprint {
				t.Fatal("finalized workspace was unnecessarily reconstructed")
			}
			intent, err := journal.GetCodexReviewIntent(ctx, string(id))
			if err != nil || intent.State != CodexReviewIntentStarted {
				t.Fatalf("recovered intent = %#v, %v", intent, err)
			}
			source.mu.Lock()
			launch := source.launches[id]
			delete(source.launches, id)
			source.mu.Unlock()
			if launch != nil {
				_ = launch.Close()
			}
			if err := backend.AbortCodexReview(ctx, source.cfg.Review, string(id)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCodexReviewSourceRetriesTransientPreparationUnderSameInvocation(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	source, err := NewCodexReviewSource(
		codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal),
	)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-prep-retry")
	request := exec.ReviewRequest{
		RunID: "run-retry", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(),
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	fx.rt.onCreateVolume = func(name string) error {
		if name == namesFor(string(id)).Workspace {
			return errors.New("runtime temporarily unavailable")
		}
		return nil
	}
	if err := source.RequestReview(ctx, id, request); err == nil ||
		exec.ClassifyReviewSourceFailure(err) != domain.ReviewFailureTransient {
		t.Fatalf("transient preparation = %v", err)
	}
	fx.rt.onCreateVolume = nil
	status, err := source.Inspect(ctx, id)
	if err != nil || status != exec.StatusRunning {
		t.Fatalf("retried preparation = %q, %v", status, err)
	}
	source.mu.Lock()
	launch := source.launches[id]
	delete(source.launches, id)
	source.mu.Unlock()
	if launch != nil {
		_ = launch.Close()
	}
	if err := backend.AbortCodexReview(ctx, source.cfg.Review, string(id)); err != nil {
		t.Fatal(err)
	}
}

func TestCodexReviewSourceRetriesTransientLaunchUnderSameInvocation(t *testing.T) {
	ctx := context.Background()
	fx := newHandoffFixture(t)
	seedSpec := fx.seed(t)
	backend := fx.backend(t)
	cfg, requestSpec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	source, err := NewCodexReviewSource(
		codexReviewSourceConfigForTest(t, backend, cfg, requestSpec, journal),
	)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-launch-retry")
	request := exec.ReviewRequest{
		RunID: "run-retry", Round: 1, Repo: seedSpec.Seed.Base.Repo,
		RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
		BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
		Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(),
		RequestedAt: codexReviewEpoch.Add(-time.Minute),
	}
	fx.rt.onCreateVolume = func(name string) error {
		if name == codexReviewShadowVolumeName(string(id)) {
			return errors.New("runtime temporarily unavailable")
		}
		return nil
	}
	if err := source.RequestReview(ctx, id, request); err == nil ||
		exec.ClassifyReviewSourceFailure(err) != domain.ReviewFailureTransient {
		t.Fatalf("transient launch = %v", err)
	}
	workspace, err := journal.GetCodexReviewWorkspaceBinding(ctx, string(id))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fx.rt.InspectVolume(ctx, workspace.Volume); err != nil {
		t.Fatalf("transient launch removed retry workspace: %v", err)
	}
	fx.rt.onCreateVolume = nil
	status, err := source.Inspect(ctx, id)
	if err != nil || status != exec.StatusRunning {
		t.Fatalf("retried launch = %q, %v", status, err)
	}
	source.mu.Lock()
	launch := source.launches[id]
	delete(source.launches, id)
	source.mu.Unlock()
	if launch != nil {
		_ = launch.Close()
	}
	if err := backend.AbortCodexReview(ctx, source.cfg.Review, string(id)); err != nil {
		t.Fatal(err)
	}
}

func TestCodexReviewSourceVerifyRejectsSwappedInvocation(t *testing.T) {
	id := domain.InvocationID("review-run-1-1")
	request := exec.ReviewRequest{
		RunID: "run-1", Round: 1, Repo: "owner/repo", RepositoryID: 42, BaseRef: "main",
		BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), Workspace: "/candidate",
		Verification: testReviewVerificationEvidence(), RequestedAt: codexReviewEpoch,
	}
	result := exec.ReviewResult{
		InvocationID: "review-run-1-2", BaseSHA: request.BaseSHA, HeadSHA: request.HeadSHA,
		Provider: "openai", ModelConfiguration: "gpt-codex/high",
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		CostOwner:           "owner", CompletedAt: codexReviewEpoch,
	}
	collectionEvidence := domain.Digest("sha256:" + strings.Repeat("e", 64))
	result.CompletionEvidence, _ = CodexReviewResultEvidence(result, collectionEvidence)
	journal := &fakeCodexReviewJournal{
		requests: map[string]exec.ReviewRequest{string(id): request},
		outcomes: map[string]CodexReviewSourceOutcome{
			string(id): {InvocationID: id, Result: &result, CollectionEvidence: collectionEvidence},
		},
		ready: map[string]bool{string(id): true},
	}
	source := &CodexReviewSource{cfg: CodexReviewSourceConfig{Journal: journal}}
	if err := source.Verify(t.Context(), id, request.BaseSHA, request.HeadSHA); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("swapped invocation verification = %v", err)
	}
}

func TestCodexReviewSourcePollRejectsSwappedFailureOutcome(t *testing.T) {
	id := domain.InvocationID("review-run-failure-1")
	foreign := domain.InvocationID("review-run-failure-2")
	journal := &fakeCodexReviewJournal{
		outcomes: map[string]CodexReviewSourceOutcome{
			string(id): {
				InvocationID: foreign, FailureClass: domain.ReviewFailureConfiguration,
				Failure: "foreign review configuration failure",
			},
		},
		ready: map[string]bool{string(id): true},
	}
	source := &CodexReviewSource{cfg: CodexReviewSourceConfig{Journal: journal}}
	if _, err := source.Poll(t.Context(), id); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("swapped failure outcome poll = %v", err)
	}
}

func TestCodexReviewSourcePollMarksPersistedFailureTerminal(t *testing.T) {
	for _, class := range []domain.ReviewFailureClass{
		domain.ReviewFailureTransient,
		domain.ReviewFailureConfiguration,
		domain.ReviewFailureQuota,
		domain.ReviewFailureContradiction,
	} {
		t.Run(string(class), func(t *testing.T) {
			id := domain.InvocationID("review-run-terminal-failure-" + string(class))
			journal := &fakeCodexReviewJournal{
				outcomes: map[string]CodexReviewSourceOutcome{
					string(id): {
						InvocationID: id, FailureClass: class,
						Failure: "terminal source failure",
					},
				},
				ready: map[string]bool{string(id): true},
			}
			source := &CodexReviewSource{cfg: CodexReviewSourceConfig{Journal: journal}}
			for range 2 {
				_, err := source.Poll(t.Context(), id)
				if !errors.Is(err, exec.ErrNoResult) ||
					exec.ClassifyReviewSourceFailure(err) != class {
					t.Fatalf("terminal %s outcome = %v", class, err)
				}
			}
		})
	}
}

func TestCodexReviewSourceVerifyRejectsSwappedRequestAuthority(t *testing.T) {
	id := domain.InvocationID("review-run-authority-1")
	request := exec.ReviewRequest{
		RunID: "run-1", Round: 1, Repo: "owner/repo", RepositoryID: 42, BaseRef: "main",
		BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), Workspace: "/candidate",
		Verification: testReviewVerificationEvidence(), RequestedAt: codexReviewEpoch,
	}
	expected, err := request.AuthorityDigest()
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*exec.ReviewRequest){
		"ownership": func(swapped *exec.ReviewRequest) {
			swapped.RunID = "run-2"
			swapped.Round = 2
		},
		"workspace": func(swapped *exec.ReviewRequest) {
			swapped.Workspace = "/swapped-candidate"
		},
		"verification": func(swapped *exec.ReviewRequest) {
			swapped.Verification.EvidenceSnapshotDigest = domain.Digest("sha256:" + strings.Repeat("f", 64))
		},
	} {
		t.Run(name, func(t *testing.T) {
			swapped := request
			mutate(&swapped)
			source := &CodexReviewSource{cfg: CodexReviewSourceConfig{Journal: &fakeCodexReviewJournal{
				requests: map[string]exec.ReviewRequest{string(id): swapped},
			}}}
			if err := source.VerifyRequestAuthority(t.Context(), id, expected); !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("swapped request authority verification = %v", err)
			}
		})
	}
}

func TestCodexReviewSourceOutcomeRejectsFindingCorruption(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	source := &CodexReviewSource{cfg: CodexReviewSourceConfig{
		Review:              CodexReviewConfig{Model: "gpt-codex", ReasoningEffort: "high"},
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		CostOwner:           "owner", Now: func() time.Time { return now },
	}}
	request := exec.ReviewRequest{
		RunID: "run-1", Round: 1, Repo: "owner/repo", RepositoryID: 42, BaseRef: "main",
		BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), Workspace: "/candidate",
		Verification: testReviewVerificationEvidence(), RequestedAt: now,
	}
	outcome := source.normalizeCollection("review-invocation-1", request, CodexReviewCollection{
		Result: []byte(`{"findings":[{"severity":"P1","location":"main.go:1","explanation":"unsafe"}]}`),
	})
	if err := outcome.Validate(); err != nil {
		t.Fatal(err)
	}
	outcome.Result.Findings = nil
	if err := outcome.Validate(); !errors.Is(err, domain.ErrInvalidReviewCompletionEvidence) {
		t.Fatalf("corrupted outcome validation = %v", err)
	}
}

func TestCodexReviewConfigurationDigestBindsEffectiveInputs(t *testing.T) {
	cfg, request := testCodexReview(t)
	digest := func(
		config CodexReviewConfig,
		size int64,
		authMode CodexAuthMode,
		identity domain.AuthIdentityID,
		instructions domain.Digest,
		costOwner string,
	) domain.Digest {
		t.Helper()
		got, err := CodexReviewConfigurationDigest(
			config, size, authMode, identity, instructions, costOwner,
		)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	base := digest(cfg, 64, request.AuthMode, request.AuthIdentityID,
		request.Instructions.Digest, "subscription:owner")
	reordered := cfg
	reordered.ProviderEndpoints = slices.Clone(cfg.ProviderEndpoints)
	slices.Reverse(reordered.ProviderEndpoints)
	if got := digest(reordered, 64, request.AuthMode, request.AuthIdentityID,
		request.Instructions.Digest, "subscription:owner"); got != base {
		t.Fatalf("endpoint order changed digest: %q != %q", got, base)
	}
	mutated := cfg
	mutated.Model += "-different"
	if got := digest(mutated, 64, request.AuthMode, request.AuthIdentityID,
		request.Instructions.Digest, "subscription:owner"); got == base {
		t.Fatal("model change did not change configuration digest")
	}
	if got := digest(cfg, 65, request.AuthMode, request.AuthIdentityID,
		request.Instructions.Digest, "subscription:owner"); got == base {
		t.Fatal("workspace size change did not change configuration digest")
	}
	if got := digest(cfg, 64, request.AuthMode, request.AuthIdentityID,
		domain.Digest("sha256:"+strings.Repeat("d", 64)), "subscription:owner"); got == base {
		t.Fatal("instruction change did not change configuration digest")
	}
	if got := digest(cfg, 64, request.AuthMode, request.AuthIdentityID,
		request.Instructions.Digest, "different-owner"); got == base {
		t.Fatal("cost owner change did not change configuration digest")
	}
}

func TestClassifyCodexLaunchFailureKeepsRuntimePreparationRetryable(t *testing.T) {
	if got := classifyCodexLaunchFailure(errors.New("runtime unavailable")); got != domain.ReviewFailureTransient {
		t.Fatalf("runtime failure class = %q", got)
	}
	if got := classifyCodexLaunchFailure(ErrInvalidCodexReviewSpec); got != domain.ReviewFailureConfiguration {
		t.Fatalf("invalid spec class = %q", got)
	}
	if got := classifyCodexLaunchFailure(fmt.Errorf("%w: create volume", ErrCodexReviewOperational)); got != domain.ReviewFailureTransient {
		t.Fatalf("operational preparation class = %q", got)
	}
	operationalConformance := codexReviewOperationalCheckf(
		CheckControlPlaneIsolation, "create volume: %v", errors.New("runtime unavailable"),
	)
	if !errors.Is(operationalConformance, ErrConformance) {
		t.Fatal("operational failure lost its check context")
	}
	if got := classifyCodexLaunchFailure(operationalConformance); got != domain.ReviewFailureTransient {
		t.Fatalf("operational check class = %q", got)
	}
	if got := classifyCodexObservationFailure(errors.New("runtime inspect failed")); got != domain.ReviewFailureTransient {
		t.Fatalf("operational observation class = %q", got)
	}
	if got := classifyCodexObservationFailure(ErrConformance); got != domain.ReviewFailureContradiction {
		t.Fatalf("authenticated observation contradiction class = %q", got)
	}
}

func TestRuntimeCodexReviewVolumeLeaseTransfersAtomically(t *testing.T) {
	ctx := context.Background()
	runtime := newFakeRuntime(t)
	for _, volume := range []string{"workspace", "shadow"} {
		if err := runtime.CreateVolume(ctx, volume, 2, nil); err != nil {
			t.Fatal(err)
		}
	}
	leaser, err := NewRuntimeCodexReviewVolumeLeaser(runtime)
	if err != nil {
		t.Fatal(err)
	}
	volumes := []string{"workspace", "shadow"}
	lease, err := leaser.AcquireCodexReviewVolumeLease(ctx, "owner", volumes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaser.AcquireCodexReviewVolumeLease(ctx, "foreign", volumes); !errors.Is(err, ErrCodexReviewVolumeLeaseForeignOwner) {
		t.Fatalf("foreign acquire = %v", err)
	}
	if err := runtime.CreateContainer(ctx, ContainerSpec{
		Name: "review", Image: "image", Command: []string{"true"},
		Labels: []Label{{Key: ownershipLabelKey, Value: "owner"}},
		Mounts: []Mount{
			{Type: MountVolume, Source: "workspace", Target: "/workspace", ReadOnly: true},
			{Type: MountVolume, Source: "shadow", Target: "/.agents", ReadOnly: true},
		},
	}); err != nil {
		t.Fatal(err)
	}
	createdRestart, err := NewRuntimeCodexReviewVolumeLeaser(runtime)
	if err != nil {
		t.Fatal(err)
	}
	if _, transfer, err := createdRestart.RecoverCodexReviewVolumeLease(
		ctx, "owner", volumes,
	); !errors.Is(err, ErrCodexReviewVolumeLeaseTransferred) || transfer.Container != "review" {
		t.Fatalf("created attachment recovery = %#v, %v", transfer, err)
	}
	if err := lease.StartCodexReviewContainer(ctx, "review"); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewRuntimeCodexReviewVolumeLeaser(runtime)
	if err != nil {
		t.Fatal(err)
	}
	if _, transfer, err := restarted.RecoverCodexReviewVolumeLease(ctx, "owner", volumes); !errors.Is(err, ErrCodexReviewVolumeLeaseTransferred) || transfer.Container != "review" {
		t.Fatalf("transferred recovery = %#v, %v", transfer, err)
	}
	runtime.onInspect = func(id string, report InspectReport) (InspectReport, error) {
		if id == "review" {
			report.Labels = []Label{{Key: ownershipLabelKey, Value: "foreign"}}
		}
		return report, nil
	}
	if _, _, err := restarted.RecoverCodexReviewVolumeLease(ctx, "owner", volumes); !errors.Is(err, ErrCodexReviewVolumeLeaseForeignOwner) {
		t.Fatalf("cached transfer accepted foreign replacement: %v", err)
	}
	runtime.onInspect = nil
	_, _ = runtime.Inspect(ctx, "review")
	_, _ = runtime.Inspect(ctx, "review")
	if err := runtime.DeleteContainer(ctx, "review"); err != nil {
		t.Fatal(err)
	}
	recovered, _, err := restarted.RecoverCodexReviewVolumeLease(ctx, "owner", volumes)
	if err != nil {
		t.Fatal(err)
	}
	if err := recovered.ReleaseCodexReviewVolumeLease(ctx); err != nil {
		t.Fatal(err)
	}
}
