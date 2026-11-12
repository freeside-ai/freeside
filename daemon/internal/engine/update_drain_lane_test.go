package engine

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	execfake "github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// TestPublicationPassRunsUnderADrainOnlyForAHeldReview: with no drain a pass
// starts and counts as running publication work until it ends. Under a drain
// a pass starts only while a launched review holds its slot, since only a
// pass's cleanup frees that slot; with none held it starts nothing and
// leaves nothing counted.
func TestPublicationPassRunsUnderADrainOnlyForAHeldReview(t *testing.T) {
	t.Parallel()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	running := NewRunningWork()
	engine, err := New(st, signet.NewService(st), execfake.NewStageDriver(), WithRunningWork(running))
	if err != nil {
		t.Fatal(err)
	}
	begin := func(label string, wantRun bool) {
		t.Helper()
		end, run, err := engine.beginPublicationPass(t.Context())
		if err != nil || run != wantRun {
			t.Fatalf("%s: pass may run = %t, %v; want %t", label, run, err, wantRun)
		}
		if !run {
			if got := running.Counts(); len(got) != 0 {
				t.Fatalf("%s: a skipped pass left counts %v", label, got)
			}
			return
		}
		if got := running.Counts(); got[domain.RestartBlockerPublication] != 1 {
			t.Fatalf("%s: counts = %v, want one publication", label, got)
		}
		end()
		if got := running.Counts(); len(got) != 0 {
			t.Fatalf("%s: counts after the pass = %v, want none", label, got)
		}
	}

	begin("no drain", true)
	now := drainEpoch
	if _, err := newDrain(st, drainOldBuild, &now).Request(t.Context()); err != nil {
		t.Fatal(err)
	}
	begin("drained, no review held", false)
	seedHeldReviewSlot(t, st)
	begin("drained, a review held", true)
}

// seedHeldReviewSlot records a review admission with no outcome, which holds
// its pool's review slot.
func seedHeldReviewSlot(t *testing.T, st *store.Store) {
	t.Helper()
	digest := func(fill string) domain.Digest { return domain.Digest("sha256:" + strings.Repeat(fill, 64)) }
	admission, err := domain.NewReviewAdmission(domain.ReviewAdmission{
		InvocationID: "review-1", RunID: "run-review", Role: domain.RoleReviewer,
		AuthIdentityID: "auth-1", LineageGroup: "openai",
		AgentBinding: domain.AdmissionAgentBinding{
			AgentDigest: digest("a"), LaunchDigest: digest("b"), TreatmentDigest: digest("d"),
			PricingRevision: "pricing-2026-01", LineupRevision: digest("c"),
			EnrollmentID: "enroll-1", EnrollmentGeneration: 1, StoreManifestDigest: digest("9"),
			EffectiveEgress: []string{"chatgpt.com:443"},
			RouteModelID:    "gpt-5.2-codex", RequestedEffort: domain.EffortHigh, NativeEffort: "high",
			SelectionSource: domain.AgentSelectionSourceLineup,
		},
		AdmittedAt: drainEpoch,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		if err := tx.PutRun(t.Context(), domain.Run{
			ID: "run-review", ProjectID: "project-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
		}); err != nil {
			return err
		}
		if err := tx.RecordAuthIdentity(t.Context(), domain.AuthIdentity{
			ID: "auth-1", Provider: "claude", AuthStoreMutationLease: true,
			Interim: domain.InterimClientFacts{AuthStoreVolume: "volume-auth-1", RefreshStrategy: domain.RefreshOnDemand},
		}, drainEpoch); err != nil {
			return err
		}
		return tx.PutReviewAdmission(t.Context(), admission)
	}); err != nil {
		t.Fatal(err)
	}
	var held int
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		held, err = tx.ReviewLaunchesHeld(t.Context())
		return err
	}); err != nil || held != 1 {
		t.Fatalf("review launches held = %d, %v; want 1", held, err)
	}
}

// TestUpdateDrainHoldsAVerificationJob: a drain refuses a verification job
// before its first container, as a retryable wait recorded as update_drain,
// with nothing counted; after the lift the job reserves and counts as a
// running verification job until it ends.
func TestUpdateDrainHoldsAVerificationJob(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newSpecificationFixture(t, false, 4)
	f.submit(t)
	runID := domain.RunID("specification-run")
	run, err := f.run(runID)
	if err != nil {
		t.Fatal(err)
	}
	running := NewRunningWork()
	w := &productionPublicationWorkflow{
		store: f.store, attention: f.signet, now: func() time.Time { return *f.now }, running: running,
	}
	task := productionPublicationTask{RunID: runID, ProjectID: run.ProjectID, VerificationID: "inv-verification"}
	drain := UpdateDrain{Store: f.store, Build: drainOldBuild, Now: func() time.Time { return *f.now }}
	if _, err := drain.Request(ctx); err != nil {
		t.Fatal(err)
	}

	_, err = w.reserveVerification(ctx, task, productionBinding{})
	if !errors.Is(err, store.ErrUpdateDrainInForce) || !productionPublicationRetryableFailure(err) {
		t.Fatalf("reserve while draining = %v, want a retryable update drain refusal", err)
	}
	if got := running.Counts(); len(got) != 0 {
		t.Fatalf("counts after the refusal = %v, want none", got)
	}
	if err := w.recordRetryableHold(ctx, task, err); err != nil {
		t.Fatal(err)
	}
	if hold := storedRunHold(t, f.store, runID); hold == nil || hold.Reason != domain.HoldUpdateDrain {
		t.Fatalf("hold = %+v, want update_drain", hold)
	}

	if _, err := drain.Cancel(ctx); err != nil {
		t.Fatal(err)
	}
	if hold := storedRunHold(t, f.store, runID); hold != nil {
		t.Fatalf("hold after the lift = %+v, want none", hold)
	}
	end, err := w.reserveVerification(ctx, task, productionBinding{})
	if err != nil {
		t.Fatal(err)
	}
	if got := running.Counts(); got[domain.RestartBlockerVerificationJob] != 1 {
		t.Fatalf("counts = %v, want one verification job", got)
	}
	end()
	if got := running.Counts(); len(got) != 0 {
		t.Fatalf("counts after the job = %v, want none", got)
	}
}

// TestImageUpkeepSkipsARebuildWhileDraining: under a drain an upkeep pass
// starts no build and settles nothing, so the first pass after the lift
// rebuilds at the tip and counts as a running image build while it does.
func TestImageUpkeepSkipsARebuildWhileDraining(t *testing.T) {
	t.Parallel()
	repo := newUpkeepRepo(t)
	built := repo.npmBase()
	tip := repo.commit(map[string]string{"src/index.js": "export const a = 1;\n"})
	st := openUpkeepStore(t)
	h := newUpkeepHarness(t, st, repo, "")
	running := NewRunningWork()
	h.upkeep.Running = running
	h.seed(upkeepImage(t, built, nil))
	h.observeTip(tip)
	now := drainEpoch
	drain := newDrain(st, drainOldBuild, &now)
	if _, err := drain.Request(t.Context()); err != nil {
		t.Fatal(err)
	}

	if err := h.upkeep.RunAll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(h.builder.requests) != 0 || h.pointer().CommitSHA != built || len(h.refusals()) != 0 {
		t.Fatalf("drained pass: builds = %d, pointer at %s, refusals = %v; want nothing done",
			len(h.builder.requests), h.pointer().CommitSHA, h.refusals())
	}
	if got := h.upkeepTip(); got == tip {
		t.Fatal("a drained pass recorded the tip as handled")
	}

	if _, err := drain.Cancel(t.Context()); err != nil {
		t.Fatal(err)
	}
	var countedDuringBuild map[domain.RestartBlockerKind]int
	h.builder.during = func() { countedDuringBuild = running.Counts() }
	if err := h.upkeep.RunAll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(h.builder.requests) != 1 || h.pointer().CommitSHA != tip {
		t.Fatalf("pass after the lift: builds = %d, pointer at %s; want one build at %s",
			len(h.builder.requests), h.pointer().CommitSHA, tip)
	}
	if countedDuringBuild[domain.RestartBlockerImageBuild] != 1 {
		t.Fatalf("counts during the build = %v, want one image build", countedDuringBuild)
	}
	if got := running.Counts(); len(got) != 0 {
		t.Fatalf("counts after the pass = %v, want none", got)
	}
}

// TestUpdateDrainHoldsAReviewAdmission: a drain refuses a review admission
// before its row is written, so no slot is held for a review that can't
// launch. The routed and shadow arms both wait as update_drain holds, the
// shadow one within its window as a capacity wait does, and the lift lets
// the admission through.
func TestUpdateDrainHoldsAReviewAdmission(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	runID := domain.RunID("run-review-drain")
	run := domain.Run{ID: runID, ProjectID: "project-shadow", SpecDigest: shadowTestDigest("3"), PolicyDigest: shadowTestDigest("2")}
	if err := st.Write(ctx, func(tx *store.WriteTx) error { return tx.PutRun(ctx, run) }); err != nil {
		t.Fatal(err)
	}
	recordFakeReviewIdentities(t, st)
	now := drainEpoch
	w := &productionPublicationWorkflow{store: st, reviewAdmitter: execfake.AdmitReview, now: func() time.Time { return now }}
	drain := newDrain(st, drainOldBuild, &now)
	if _, err := drain.Request(ctx); err != nil {
		t.Fatal(err)
	}
	requireDrainHold := func(label string) {
		t.Helper()
		if hold := storedRunHold(t, st, runID); hold == nil || hold.Reason != domain.HoldUpdateDrain {
			t.Fatalf("%s: hold = %+v, want update_drain", label, hold)
		}
	}

	id := ProductionReviewInvocationID(runID, 1)
	_, err := w.ensureReviewAdmission(ctx, domain.RoleReviewer, runID, id)
	if !errors.Is(err, store.ErrUpdateDrainInForce) || !reviewCapacityRefusal(err) {
		t.Fatalf("admission while draining = %v, want an update drain wait", err)
	}
	var held int
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		held, err = tx.ReviewLaunchesHeld(ctx)
		return err
	}); err != nil || held != 0 {
		t.Fatalf("review slots held = %d, %v; want none", held, err)
	}
	if err := w.holdReviewForCapacity(ctx, runID, id, err); err != nil {
		t.Fatal(err)
	}
	requireDrainHold("routed")

	shadowID := ProductionReviewInvocationID(runID, 2)
	shadowTask := productionPublicationTask{RunID: runID, ProjectID: run.ProjectID}
	routed := domain.ReviewRecord{Round: 1, CompletedAt: now}
	drained := fmt.Errorf("admit: %w", store.ErrUpdateDrainInForce)
	abandoned, err := w.refuseShadowReviewAdmission(ctx, shadowTask, routed, shadowID, drained)
	if err != nil || abandoned {
		t.Fatalf("shadow refusal = abandoned %t, %v; want a wait", abandoned, err)
	}
	requireDrainHold("shadow")

	if _, err := drain.Cancel(ctx); err != nil {
		t.Fatal(err)
	}
	if hold := storedRunHold(t, st, runID); hold != nil {
		t.Fatalf("hold after the lift = %+v, want none", hold)
	}
	if _, err := w.ensureReviewAdmission(ctx, domain.RoleReviewer, runID, id); err != nil {
		t.Fatalf("admission after the lift: %v", err)
	}
}
