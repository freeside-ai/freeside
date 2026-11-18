package ward

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

// releaseRecorder is a RuntimeResourceReleaser that records every name set it
// is asked to release.
type releaseRecorder struct {
	mu    sync.Mutex
	calls []RuntimeResourceNames
	err   error
}

func (r *releaseRecorder) release(_ context.Context, names RuntimeResourceNames) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, names)
	return r.err
}

func (r *releaseRecorder) released() RuntimeResourceNames {
	r.mu.Lock()
	defer r.mu.Unlock()
	return concatRuntimeResourceNames(r.calls...)
}

func (r *releaseRecorder) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// requireReleased fails unless every name in want was released.
func requireReleased(t *testing.T, got, want RuntimeResourceNames) {
	t.Helper()
	for kind, pair := range map[string][2][]string{
		"container": {got.Containers, want.Containers},
		"volume":    {got.Volumes, want.Volumes},
		"network":   {got.Networks, want.Networks},
	} {
		for _, name := range pair[1] {
			if !slices.Contains(pair[0], name) {
				t.Errorf("%s %q was not released; released %v", kind, name, pair[0])
			}
		}
	}
}

func TestReleaseAbsentRuntimeResourcesKeepsNamesWithObjects(t *testing.T) {
	t.Parallel()
	rt := newFakeRuntime(t)
	ctx := context.Background()
	present := namesFor("c" + "0123456789abcdef0123456789abcde")
	gone := namesFor("c" + "fedcba9876543210fedcba987654321")
	if err := rt.CreateNetwork(ctx, present.Network, nil); err != nil {
		t.Fatal(err)
	}
	if err := rt.CreateVolume(ctx, present.Workspace, 1, nil); err != nil {
		t.Fatal(err)
	}
	if err := rt.CreateContainer(ctx, ContainerSpec{Size: testContainerSize, Name: present.Agent, Image: "agent"}); err != nil {
		t.Fatal(err)
	}
	var recorder releaseRecorder
	if err := ReleaseAbsentRuntimeResources(ctx, rt, recorder.release, RuntimeResourceNames{
		Containers: []string{present.Agent, gone.Agent},
		Volumes:    []string{present.Workspace, gone.Workspace},
		Networks:   []string{present.Network, gone.Network},
	}); err != nil {
		t.Fatal(err)
	}
	got := recorder.released()
	want := RuntimeResourceNames{
		Containers: []string{gone.Agent}, Volumes: []string{gone.Workspace}, Networks: []string{gone.Network},
	}
	if !slices.Equal(got.Containers, want.Containers) || !slices.Equal(got.Volumes, want.Volumes) ||
		!slices.Equal(got.Networks, want.Networks) {
		t.Fatalf("released %+v, want only the absent names %+v", got, want)
	}
}

func TestReleaseAbsentRuntimeResourcesReleasesNothingWhenAListFails(t *testing.T) {
	t.Parallel()
	names := namesFor("c" + "0123456789abcdef0123456789abcde")
	resources := RuntimeResourceNames{
		Containers: []string{names.Agent}, Volumes: []string{names.Workspace}, Networks: []string{names.Network},
	}
	for _, kind := range []string{"containers", "volumes", "networks"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			rt := newFakeRuntime(t)
			listErr := errors.New("fixture: list failed")
			switch kind {
			case "containers":
				rt.onListContainers = func([]ContainerSummary) ([]ContainerSummary, error) { return nil, listErr }
			case "volumes":
				rt.onListVolumes = func([]VolumeSummary) ([]VolumeSummary, error) { return nil, listErr }
			case "networks":
				rt.onListNetworks = func([]NetworkSummary) ([]NetworkSummary, error) { return nil, listErr }
			}
			var recorder releaseRecorder
			err := ReleaseAbsentRuntimeResources(context.Background(), rt, recorder.release, resources)
			if !errors.Is(err, listErr) {
				t.Fatalf("release error = %v, want the list failure", err)
			}
			if recorder.callCount() != 0 {
				t.Fatalf("released %+v after a failed %s list", recorder.released(), kind)
			}
		})
	}
}

func TestReleaseAbsentRuntimeResourcesWithoutRigListsNothing(t *testing.T) {
	t.Parallel()
	rt := newFakeRuntime(t)
	names := namesFor("c" + "0123456789abcdef0123456789abcde")
	if err := ReleaseAbsentRuntimeResources(context.Background(), rt, nil, RuntimeResourceNames{
		Containers: []string{names.Agent},
	}); err != nil {
		t.Fatal(err)
	}
	if calls := rt.calls; len(calls) != 0 {
		t.Fatalf("runtime calls without a releaser = %v, want none", calls)
	}
}

func TestHandoffCloseReleasesItsNamespace(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		fail func(*handoffFixture, HandoffSpec)
		want HandoffJournalOutcome
	}{
		{name: "completed", want: HandoffCompleted},
		{
			name: "loss",
			fail: func(fx *handoffFixture, hs HandoffSpec) {
				fx.rt.onStart = func(id string) error {
					if id == namesFor(hs.RunID).Agent {
						return errors.New("fixture: start refused")
					}
					return nil
				}
			},
			want: HandoffLoss,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fx := newHandoffFixture(t)
			j := fx.journalled()
			var recorder releaseRecorder
			fx.cfg.ReleaseRuntimeResources = recorder.release
			hs := testHandoffSpec()
			if test.fail != nil {
				test.fail(fx, hs)
			}
			_, _ = fx.runSpec(t, hs)
			if rec := j.snapshot(hs.RunID); rec == nil || rec.Outcome == nil || *rec.Outcome != test.want {
				t.Fatalf("record = %+v, want closed %s", rec, test.want)
			}
			requireReleased(t, recorder.released(), RuntimeResourceNamesFor(hs.RunID))
		})
	}
}

func TestHandoffKeepsNamespaceBoundWhileRecordOpen(t *testing.T) {
	t.Parallel()
	t.Run("close fails", func(t *testing.T) {
		t.Parallel()
		fx := newHandoffFixture(t)
		j := fx.journalled()
		j.failClose = errors.New("fixture: journal close failed")
		var recorder releaseRecorder
		fx.cfg.ReleaseRuntimeResources = recorder.release
		if _, err := fx.run(t); err == nil {
			t.Fatal("Handoff succeeded with a failing journal close")
		}
		if recorder.callCount() != 0 {
			t.Fatalf("released %+v without a closed record", recorder.released())
		}
	})
	t.Run("teardown fails", func(t *testing.T) {
		t.Parallel()
		fx := newHandoffFixture(t)
		j := fx.journalled()
		var recorder releaseRecorder
		fx.cfg.ReleaseRuntimeResources = recorder.release
		hs := testHandoffSpec()
		fx.rt.onDeleteVolume = func(name string) (bool, error) {
			if name == namesFor(hs.RunID).Workspace {
				return true, errors.New("fixture: delete refused")
			}
			return false, nil
		}
		if _, err := fx.runSpec(t, hs); err == nil {
			t.Fatal("Handoff succeeded with a failing teardown")
		}
		if rec := j.snapshot(hs.RunID); rec == nil || rec.Outcome != nil {
			t.Fatalf("record = %+v, want it left open for recovery", rec)
		}
		if recorder.callCount() != 0 {
			t.Fatalf("released %+v while the record is open", recorder.released())
		}
	})
}

func TestHandoffReleaseFailureKeepsOutcome(t *testing.T) {
	t.Parallel()
	fx := newHandoffFixture(t)
	fx.journalled()
	recorder := releaseRecorder{err: errors.New("fixture: rig unbind failed")}
	fx.cfg.ReleaseRuntimeResources = recorder.release
	res, err := fx.run(t)
	if err != nil || res == nil {
		t.Fatalf("Handoff = %v, %v; want the export despite the failed release", res, err)
	}
	if recorder.callCount() == 0 {
		t.Fatal("release was never attempted")
	}
}

func TestRecoverCloseReleasesItsNamespace(t *testing.T) {
	t.Parallel()
	fx := newRecoveryFixture(t)
	var recorder releaseRecorder
	fx.cfg.ReleaseRuntimeResources = recorder.release
	hs := testHandoffSpec()
	fx.openRecord(t, hs)
	names := namesFor(hs.RunID)
	fx.worldVolume(t, names.Workspace, fx.runLabels(hs.RunID))
	if _, err := fx.recover(t, hs.RunID, hs); err != nil {
		t.Fatalf("Recover = %v, want committed loss", err)
	}
	fx.wantClosed(t, hs.RunID, HandoffLoss)
	requireReleased(t, recorder.released(), RuntimeResourceNamesFor(hs.RunID))
}

func TestRecoverKeepsNamespaceBoundWhenCloseFails(t *testing.T) {
	t.Parallel()
	fx := newRecoveryFixture(t)
	var recorder releaseRecorder
	fx.cfg.ReleaseRuntimeResources = recorder.release
	hs := testHandoffSpec()
	fx.openRecord(t, hs)
	fx.worldVolume(t, namesFor(hs.RunID).Workspace, fx.runLabels(hs.RunID))
	fx.j.failClose = errors.New("fixture: journal close failed")
	if _, err := fx.recover(t, hs.RunID, hs); err == nil {
		t.Fatal("Recover succeeded with a failing journal close")
	}
	if recorder.callCount() != 0 {
		t.Fatalf("released %+v without a closed record", recorder.released())
	}
}

func TestReviewCleanupKeepsNamespaceBoundWhenCloseFails(t *testing.T) {
	t.Parallel()
	lifecycle, _, cfg, launchSpec, journal := testCodexReviewLifecycle(t)
	var recorder releaseRecorder
	lifecycle.cfg.ReleaseRuntime = recorder.release
	launch, err := lifecycle.CodexReview(context.Background(), cfg, launchSpec)
	if err != nil {
		t.Fatal(err)
	}
	_ = launch.Close()
	journal.failClose = errors.New("fixture: intent close failed")
	if err := lifecycle.AbortCodexReview(t.Context(), cfg, launchSpec.RunID); err == nil {
		t.Fatal("AbortCodexReview succeeded with a failing intent close")
	}
	if journal.intent.State == CodexReviewIntentClosed {
		t.Fatal("intent closed despite the failing close")
	}
	if recorder.callCount() != 0 {
		t.Fatalf("released %+v without a closed intent", recorder.released())
	}
}

func TestReviewCleanupCloseReleasesItsNamespace(t *testing.T) {
	t.Parallel()
	lifecycle, _, cfg, launchSpec, journal := testCodexReviewLifecycle(t)
	var recorder releaseRecorder
	lifecycle.cfg.ReleaseRuntime = recorder.release
	launch, err := lifecycle.CodexReview(context.Background(), cfg, launchSpec)
	if err != nil {
		t.Fatal(err)
	}
	_ = launch.Close()
	if recorder.callCount() != 0 {
		t.Fatalf("launch released %+v before cleanup", recorder.released())
	}
	if err := lifecycle.AbortCodexReview(t.Context(), cfg, launchSpec.RunID); err != nil {
		t.Fatal(err)
	}
	if journal.intent.State != CodexReviewIntentClosed {
		t.Fatalf("intent state = %q, want closed", journal.intent.State)
	}
	requireReleased(t, recorder.released(),
		reviewRuntimeResourceNamesFor(lifecycle.reviewProvider(), launchSpec.RunID))
}

// getFailJournal fails the journal read of one run.
type getFailJournal struct {
	*fakeJournal
	failRun string
}

func (j getFailJournal) Get(ctx context.Context, runID string) (HandoffJournalRecord, error) {
	if runID == j.failRun {
		return HandoffJournalRecord{}, errors.New("fixture: journal read failed")
	}
	return j.fakeJournal.Get(ctx, runID)
}

// requireNotReleased fails if any name in names was released.
func requireNotReleased(t *testing.T, got, names RuntimeResourceNames) {
	t.Helper()
	for _, pair := range [][2][]string{
		{got.Containers, names.Containers}, {got.Volumes, names.Volumes}, {got.Networks, names.Networks},
	} {
		for _, name := range pair[1] {
			if slices.Contains(pair[0], name) {
				t.Errorf("%q was released", name)
			}
		}
	}
}

func TestReleaseFinishedHandoffsReleasesOnlyAbsentOrClosedRecords(t *testing.T) {
	t.Parallel()
	fx := newRecoveryFixture(t)
	var recorder releaseRecorder
	fx.cfg.ReleaseRuntimeResources = recorder.release
	specFor := func(runID string) HandoffSpec {
		hs := testHandoffSpec()
		hs.RunID = runID
		return hs
	}
	const absent, closed, open, unreadable = "run-absent", "run-closed", "run-open", "run-unreadable"
	fx.openRecord(t, specFor(closed))
	if err := fx.j.Close(context.Background(), closed, HandoffLoss); err != nil {
		t.Fatal(err)
	}
	fx.openRecord(t, specFor(open))
	fx.openRecord(t, specFor(unreadable))
	fx.cfg.Journal = getFailJournal{fakeJournal: fx.j, failRun: unreadable}

	fx.backend(t).ReleaseFinishedHandoffs(context.Background(), []string{absent, closed, open, unreadable})
	if calls := recorder.callCount(); calls != 1 {
		t.Fatalf("release calls = %d, want one batch", calls)
	}
	got := recorder.released()
	requireReleased(t, got, concatRuntimeResourceNames(RuntimeResourceNamesFor(absent), RuntimeResourceNamesFor(closed)))
	requireNotReleased(t, got, concatRuntimeResourceNames(RuntimeResourceNamesFor(open), RuntimeResourceNamesFor(unreadable)))
}

func TestReleaseFinishedHandoffsWithoutRigReadsNothing(t *testing.T) {
	t.Parallel()
	fx := newRecoveryFixture(t)
	fx.backend(t).ReleaseFinishedHandoffs(context.Background(), []string{"run-absent"})
	if calls := fx.j.calls; len(calls) != 0 {
		t.Fatalf("journal calls without a releaser = %v, want none", calls)
	}
	if calls := fx.rt.calls; len(calls) != 0 {
		t.Fatalf("runtime calls without a releaser = %v, want none", calls)
	}
}

// Review recovery releases a closed intent's names once a stored outcome
// fences any relaunch of its run, and keeps them while none is stored.
func TestReviewRecoveryReleasesClosedIntentOnlyBehindAnOutcome(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		outcome bool
	}{
		{name: "outcome stored", outcome: true},
		{name: "no outcome"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			lifecycle, rt, cfg, launchSpec, journal := testCodexReviewLifecycle(t)
			launch, err := lifecycle.CodexReview(ctx, cfg, launchSpec)
			if err != nil {
				t.Fatal(err)
			}
			_ = launch.Close()
			if err := lifecycle.AbortCodexReview(ctx, cfg, launchSpec.RunID); err != nil {
				t.Fatal(err)
			}
			if journal.intent.State != CodexReviewIntentClosed {
				t.Fatalf("intent state = %q, want closed", journal.intent.State)
			}
			if test.outcome {
				journal.outcomes = map[string]CodexReviewSourceOutcome{launchSpec.RunID: {
					InvocationID: domain.InvocationID(launchSpec.RunID),
					FailureClass: domain.ReviewFailureTransient, Failure: "fixture: review aborted",
				}}
			}
			// A crash before cleanup's own release left these names bound.
			var recorder releaseRecorder
			lifecycle.cfg.ReleaseRuntime = recorder.release
			leaser, err := NewRuntimeCodexReviewVolumeLeaser(rt)
			if err != nil {
				t.Fatal(err)
			}
			recovery, err := NewCodexReviewRecovery(lifecycle, journal, leaser, "")
			if err != nil {
				t.Fatal(err)
			}
			if err := recovery.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			names := reviewRuntimeResourceNamesFor(lifecycle.reviewProvider(), launchSpec.RunID)
			if !test.outcome {
				if recorder.callCount() != 0 {
					t.Fatalf("released %+v with no outcome fence", recorder.released())
				}
				return
			}
			if recorder.callCount() != 1 {
				t.Fatalf("release calls = %d, want one batch", recorder.callCount())
			}
			requireReleased(t, recorder.released(), names)
		})
	}
}

// A review launch that fails with no open intent releases the names it bound,
// because the next launch binds the same set again before it creates
// anything. An open or unreadable intent keeps them for recovery.
func TestReviewLaunchFailureReleasesNamesWithoutAnOpenIntent(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		intent  func(id string) *CodexReviewLaunchIntent
		readErr bool
		release bool
	}{
		{name: "intent absent", release: true},
		{
			name: "intent closed",
			intent: func(id string) *CodexReviewLaunchIntent {
				return &CodexReviewLaunchIntent{RunID: id, State: CodexReviewIntentClosed}
			},
			release: true,
		},
		{
			name: "intent open",
			intent: func(id string) *CodexReviewLaunchIntent {
				return &CodexReviewLaunchIntent{RunID: id, State: CodexReviewIntentStarted}
			},
		},
		{name: "intent unreadable", readErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fx := newHandoffFixture(t)
			seedSpec := fx.seed(t)
			lifecycle := fx.codexReviewLifecycle(t)
			var recorder releaseRecorder
			lifecycle.cfg.ReleaseRuntime = recorder.release
			var bindMu sync.Mutex
			binds := 0
			lifecycle.authorizeRuntimeResources = func(context.Context, RuntimeResourceNames) error {
				bindMu.Lock()
				defer bindMu.Unlock()
				binds++
				return nil
			}
			cfg, requestSpec := testCodexReview(t)
			journal := &fakeCodexReviewJournal{}
			source, err := NewCodexReviewSource(
				codexReviewSourceConfigForTest(t, lifecycle, cfg, requestSpec, journal),
			)
			if err != nil {
				t.Fatal(err)
			}
			id := domain.InvocationID("review-release-1")
			if test.intent != nil {
				journal.extraIntents = map[string]*CodexReviewLaunchIntent{string(id): test.intent(string(id))}
			}
			if test.readErr {
				journal.failIntentByID = map[string]error{string(id): errors.New("fixture: intent read failed")}
			}
			request := exec.ReviewRequest{
				Admission: testReviewAdmission("run-release"),
				RunID:     "run-release", Round: 1, Repo: seedSpec.Seed.Base.Repo,
				RepositoryID: seedSpec.Seed.Base.RepositoryID, BaseRef: seedSpec.Seed.Base.BaseRef,
				BaseSHA: strings.Repeat("a", 40), HeadSHA: seedSpec.Seed.Base.BaseSHA,
				Workspace: seedSpec.Seed.SourceDir, Verification: testReviewVerificationEvidence(),
				Instructions: testReviewInstructionBinding(),
				RequestedAt:  codexReviewEpoch.Add(-time.Minute),
			}
			// A transient workspace failure stores no outcome and leaves no
			// workspace object behind.
			fx.rt.onCreateVolume = func(name string) error {
				if name == namesFor(string(id)).Workspace {
					return errors.New("fixture: runtime temporarily unavailable")
				}
				return nil
			}
			if err := source.RequestReview(ctx, id, request); err == nil ||
				exec.ClassifyReviewSourceFailure(err) != domain.ReviewFailureTransient {
				t.Fatalf("RequestReview = %v, want a transient failure", err)
			}
			names := reviewRuntimeResourceNamesFor(source.reviewProvider(), string(id))
			if !test.release {
				if recorder.callCount() != 0 {
					t.Fatalf("released %+v while recovery may own the intent", recorder.released())
				}
				return
			}
			requireReleased(t, recorder.released(), names)
			if _, stored, err := journal.GetCodexReviewOutcome(ctx, string(id)); err == nil || stored {
				t.Fatalf("outcome stored after a transient failure; the test needs none")
			}
			if test.intent != nil {
				return
			}
			// The retry binds the names again before it acts, and its success
			// releases nothing.
			bindMu.Lock()
			before := binds
			bindMu.Unlock()
			released := recorder.callCount()
			fx.rt.onCreateVolume = nil
			if status, err := source.Inspect(ctx, id); err != nil || status != exec.StatusRunning {
				t.Fatalf("retried launch = %q, %v", status, err)
			}
			bindMu.Lock()
			after := binds
			bindMu.Unlock()
			if after <= before {
				t.Fatal("the retried launch did not bind its names again")
			}
			if recorder.callCount() != released {
				t.Fatalf("a successful launch released %+v", recorder.released())
			}
			source.mu.Lock()
			launch := source.launches[id]
			delete(source.launches, id)
			source.mu.Unlock()
			if launch != nil {
				_ = launch.Close()
			}
			if err := lifecycle.AbortCodexReview(ctx, source.cfg.Review, string(id)); err != nil {
				t.Fatal(err)
			}
		})
	}
}
