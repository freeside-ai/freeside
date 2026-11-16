package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	execfake "github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	inferencefake "github.com/freeside-ai/freeside/daemon/internal/inference/fake"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// The tests here pin that an execution pause closes each kind of new work
// the update drain closes, the same way, so prod's running work reaches zero
// before it releases the App authority. Where the drain records update_drain
// the pause records blocking_system_health, the hold its card names.

// setExecutionPause appends an execution_pause revision in state, or one that
// clears the section when state is empty.
func setExecutionPause(t *testing.T, st *store.Store, state domain.ExecutionPauseState) {
	t.Helper()
	value := json.RawMessage("")
	if state != "" {
		value = json.RawMessage(`{"state":"` + string(state) + `"}`)
	}
	if err := st.WriteInternal(t.Context(), func(tx *store.InternalTx) error {
		_, err := tx.AppendInstanceSettingsRevision(t.Context(), domain.InstanceSettingsInput{
			Section: domain.InstanceSectionExecutionPause, Value: value, SetBy: "cli:operator",
		}, drainEpoch)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func requirePauseHold(t *testing.T, st *store.Store, runID domain.RunID, label string) {
	t.Helper()
	if hold := storedRunHold(t, st, runID); hold == nil || hold.Reason != domain.HoldBlockingSystemHealth {
		t.Fatalf("%s: hold = %+v, want blocking_system_health", label, hold)
	}
}

// TestExecutionPauseHoldsANewWriterLaunch: in both operating modes and both
// pause states a pause holds a writer attempt not yet recorded, and records
// no attempt. Once the pause clears the next attended pass starts it, and
// the start clears the hold.
func TestExecutionPauseHoldsANewWriterLaunch(t *testing.T) {
	t.Parallel()
	for _, mode := range []domain.OperatingMode{domain.ModeAttendedDev, domain.ModeUnattended} {
		for _, state := range domain.AllExecutionPauseStates {
			t.Run(string(mode)+"/"+string(state), func(t *testing.T) {
				t.Parallel()
				h := newDrainDispatchFixture(t, nil)
				h.engine.admission.environment.OperatingMode = mode
				setExecutionPause(t, h.fixture.store, state)
				h.fixture.submit(t)
				if started := h.dispatch(t); started != 0 {
					t.Fatalf("started %d while paused", started)
				}
				requirePauseHold(t, h.fixture.store, "specification-run", "paused")
				if h.attemptRecorded(t) {
					t.Fatal("a paused dispatch recorded an attempt")
				}
				if mode == domain.ModeUnattended {
					// The unattended gate has its own conditions.
					return
				}
				setExecutionPause(t, h.fixture.store, "")
				if started := h.dispatch(t); started != 1 {
					t.Fatalf("started %d after the pause cleared, want 1", started)
				}
				if hold := storedRunHold(t, h.fixture.store, "specification-run"); hold != nil {
					t.Fatalf("hold after the start = %+v, want none", hold)
				}
			})
		}
	}
}

// TestExecutionPauseStartsAnAttemptRecordedBeforeIt: an attempt recorded
// before the pause is running work, so a pass under the pause still starts
// it, while a new attempt's write is refused in its own transaction.
func TestExecutionPauseStartsAnAttemptRecordedBeforeIt(t *testing.T) {
	t.Parallel()
	h := newDrainDispatchFixture(t, func(driver exec.StageDriver) exec.StageDriver {
		return &failFirstStart{StageDriver: driver}
	})
	h.fixture.submit(t)
	if _, err := h.engine.dispatchPendingInvocations(t.Context()); err == nil {
		t.Fatal("the injected start failure did not surface")
	}
	if !h.attemptRecorded(t) {
		t.Fatal("the failed start left no recorded attempt")
	}
	setExecutionPause(t, h.fixture.store, domain.ExecutionPauseRequested)
	if started := h.dispatch(t); started != 1 {
		t.Fatalf("started %d under the pause, want the recorded attempt to start", started)
	}
	if hold := storedRunHold(t, h.fixture.store, "specification-run"); hold != nil {
		t.Fatalf("hold = %+v, want none", hold)
	}
}

func TestExecutionPauseBackstopsTheAttemptWrite(t *testing.T) {
	t.Parallel()
	h := newDrainDispatchFixture(t, nil)
	h.fixture.submit(t)
	run, err := h.fixture.run("specification-run")
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Stages) == 0 {
		t.Fatal("the submitted run has no stage")
	}
	setExecutionPause(t, h.fixture.store, domain.ExecutionPauseRequested)
	_, _, _, err = h.engine.recordAttempt(
		t.Context(), run.ID, run.Stages[0].ID, h.invocationID, nil, "pending", nil, domain.BaseRevision{})
	if !errors.Is(err, store.ErrExecutionPauseInForce) {
		t.Fatalf("attempt write under a pause = %v, want the pause refusal", err)
	}
	if h.attemptRecorded(t) {
		t.Fatal("the attempt write appended an attempt under a pause")
	}
}

// TestPublicationPassRunsUnderAPauseOnlyForAHeldReview: as under a drain, a
// pass starts under a pause only while a launched review holds its slot.
func TestPublicationPassRunsUnderAPauseOnlyForAHeldReview(t *testing.T) {
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
		if run {
			end()
		}
		if got := running.Counts(); len(got) != 0 {
			t.Fatalf("%s: counts after the pass = %v, want none", label, got)
		}
	}
	setExecutionPause(t, st, domain.ExecutionPauseRequested)
	begin("paused, no review held", false)
	seedHeldReviewSlot(t, st)
	begin("paused, a review held", true)
}

// TestExecutionPauseHoldsAVerificationJob: a pause refuses a verification
// job as a retryable wait, with nothing counted, and the job reserves once
// the pause clears.
func TestExecutionPauseHoldsAVerificationJob(t *testing.T) {
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
	setExecutionPause(t, f.store, domain.ExecutionPauseRequested)

	_, err = w.reserveVerification(ctx, task, productionBinding{})
	if !errors.Is(err, store.ErrExecutionPauseInForce) || !productionPublicationRetryableFailure(err) {
		t.Fatalf("reserve while paused = %v, want a retryable pause refusal", err)
	}
	if got := running.Counts(); len(got) != 0 {
		t.Fatalf("counts after the refusal = %v, want none", got)
	}
	if err := w.recordRetryableHold(ctx, task, err); err != nil {
		t.Fatal(err)
	}
	requirePauseHold(t, f.store, runID, "verification")

	setExecutionPause(t, f.store, "")
	end, err := w.reserveVerification(ctx, task, productionBinding{})
	if err != nil {
		t.Fatal(err)
	}
	end()
}

// TestImageUpkeepSkipsARebuildWhilePaused: under a pause an upkeep pass
// starts no build and settles nothing.
func TestImageUpkeepSkipsARebuildWhilePaused(t *testing.T) {
	t.Parallel()
	repo := newUpkeepRepo(t)
	built := repo.npmBase()
	tip := repo.commit(map[string]string{"src/index.js": "export const a = 1;\n"})
	st := openUpkeepStore(t)
	h := newUpkeepHarness(t, st, repo, "")
	h.upkeep.Running = NewRunningWork()
	h.seed(upkeepImage(t, built, nil))
	h.observeTip(tip)
	setExecutionPause(t, st, domain.ExecutionPauseRequested)

	if err := h.upkeep.RunAll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(h.builder.requests) != 0 || h.pointer().CommitSHA != built || len(h.refusals()) != 0 {
		t.Fatalf("paused pass: builds = %d, pointer at %s, refusals = %v; want nothing done",
			len(h.builder.requests), h.pointer().CommitSHA, h.refusals())
	}
	if got := h.upkeepTip(); got == tip {
		t.Fatal("a paused pass recorded the tip as handled")
	}
}

// TestExecutionPauseHoldsAReviewAdmission: a pause refuses a review admission
// before its row is written. The routed and shadow arms both wait, and the
// admission written once the pause clears ends the wait's hold.
func TestExecutionPauseHoldsAReviewAdmission(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	runID := domain.RunID("run-review-pause")
	run := domain.Run{ID: runID, ProjectID: "project-shadow", SpecDigest: shadowTestDigest("3"), PolicyDigest: shadowTestDigest("2")}
	if err := st.Write(ctx, func(tx *store.WriteTx) error { return tx.PutRun(ctx, run) }); err != nil {
		t.Fatal(err)
	}
	recordFakeReviewIdentities(t, st)
	now := drainEpoch
	w := &productionPublicationWorkflow{store: st, reviewAdmitter: execfake.AdmitReview, now: func() time.Time { return now }}
	setExecutionPause(t, st, domain.ExecutionPauseRequested)

	id := ProductionReviewInvocationID(runID, 1)
	_, err := w.ensureReviewAdmission(ctx, domain.RoleReviewer, runID, id)
	if !errors.Is(err, store.ErrExecutionPauseInForce) || !reviewCapacityRefusal(err) {
		t.Fatalf("admission while paused = %v, want a pause wait", err)
	}
	if err := w.holdReviewForCapacity(ctx, runID, id, err); err != nil {
		t.Fatal(err)
	}
	requirePauseHold(t, st, runID, "routed")

	shadowID := ProductionReviewInvocationID(runID, 2)
	shadowTask := productionPublicationTask{RunID: runID, ProjectID: run.ProjectID}
	routed := domain.ReviewRecord{Round: 1, CompletedAt: now}
	paused := fmt.Errorf("admit: %w", store.ErrExecutionPauseInForce)
	abandoned, err := w.refuseShadowReviewAdmission(ctx, shadowTask, routed, shadowID, paused)
	if err != nil || abandoned {
		t.Fatalf("shadow refusal = abandoned %t, %v; want a wait", abandoned, err)
	}
	requirePauseHold(t, st, runID, "shadow")

	setExecutionPause(t, st, "")
	if _, err := w.ensureReviewAdmission(ctx, domain.RoleReviewer, runID, id); err != nil {
		t.Fatalf("admission after the pause cleared: %v", err)
	}
	if hold := storedRunHold(t, st, runID); hold != nil {
		t.Fatalf("hold after the admission = %+v, want none", hold)
	}
}

// TestAttentionDiscussionWaitsOutAPause: a pause holds a new reply before any
// provider call, in either operating mode.
func TestAttentionDiscussionWaitsOutAPause(t *testing.T) {
	t.Parallel()
	for _, mode := range []domain.OperatingMode{domain.ModeAttendedDev, domain.ModeUnattended} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			driver := inferencefake.New()
			driver.Script(inference.AttentionDiscussionSiteID, inferencefake.Script{Response: inference.Response{
				Output: []byte(`{"reply":"after the pause"}`), ComputeUnits: 1,
			}})
			f := newAttentionDiscussionFixture(t, domain.AttentionExecutionFailure, driver)
			f.submit(t)
			setExecutionPause(t, f.store, domain.ExecutionPauseRequested)
			engine := f.engine(t, true)
			engine.admission = &admitter{environment: AdmissionEnvironment{OperatingMode: mode}}
			if result, err := engine.Reconcile(t.Context()); err != nil || result.ResultsAccepted != 0 {
				t.Fatalf("paused Reconcile = %#v, %v", result, err)
			}
			if requests := driver.Requests(); len(requests) != 0 {
				t.Fatalf("paused discussion reached the provider: %#v", requests)
			}
			setExecutionPause(t, f.store, "")
			held, err := engine.attentionDiscussionHeld(t.Context())
			if err != nil || held {
				t.Fatalf("held after the pause cleared = %t, %v; want false", held, err)
			}
		})
	}
}
