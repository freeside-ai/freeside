package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	execfake "github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/keyedrun"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// newDispatchLane is a lane with only what its dispatcher and an empty pass
// read: a store with no pending task and a restart readiness counter.
func newDispatchLane(t *testing.T) *productionPublicationWorkflow {
	t.Helper()
	return &productionPublicationWorkflow{
		store:   storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{}),
		running: NewRunningWork(),
		now:     time.Now,
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// dispatchParts partitions tasks named run/label by run, as a pass does.
func dispatchParts(keys ...string) []keyedrun.Part[domain.RunID, pendingPublicationTask] {
	tasks := make([]pendingPublicationTask, 0, len(keys))
	for _, key := range keys {
		run, _, _ := strings.Cut(key, "/")
		runID := domain.RunID(run)
		tasks = append(tasks, pendingPublicationTask{
			entry: store.QueueEntry{IdempotencyKey: key}, runID: runID,
		})
	}
	return keyedrun.Partition(tasks, func(p pendingPublicationTask) domain.RunID { return p.runID })
}

func publicationCount(w *productionPublicationWorkflow) int {
	return w.running.Counts()[domain.RestartBlockerPublication]
}

// TestALaneRunsItsTasksInOrderBesideOtherRuns: invariant 1 (#3214). While
// the first task of run a is still working, run b's task starts and
// finishes, and a's second task starts only after its first ends. Each run's
// goroutine counts as running publication work until it ends. The bubble
// makes the check exact: once every goroutine is blocked, a's first task is
// parked, b's goroutine has ended, and a second task of a started beside the
// first would already have run.
func TestALaneRunsItsTasksInOrderBesideOtherRuns(t *testing.T) {
	t.Parallel()
	w := newDispatchLane(t)
	synctest.Test(t, func(t *testing.T) {
		firstBlocks := make(chan struct{})
		// A failed check must still release a's first task, or the bubble
		// ends with it blocked and aborts the test binary.
		release := sync.OnceFunc(func() { close(firstBlocks) })
		defer release()
		var (
			mu       sync.Mutex
			order    []string
			inFlight = map[domain.RunID]int{}
		)
		advance := func(_ context.Context, p pendingPublicationTask) (productionPublicationResult, error) {
			key := p.entry.IdempotencyKey
			mu.Lock()
			inFlight[p.runID]++
			if inFlight[p.runID] > 1 {
				t.Errorf("task %s started while another task of run %s ran", key, p.runID)
			}
			order = append(order, "start "+key)
			mu.Unlock()
			if key == "a/1" {
				<-firstBlocks
			}
			mu.Lock()
			inFlight[p.runID]--
			order = append(order, "end "+key)
			mu.Unlock()
			return productionPublicationResult{completed: 1}, nil
		}

		started := w.startRuns(t.Context(), dispatchParts("a/1", "b/1", "a/2"), nil, time.Now(), advance)
		if len(started) != 2 {
			t.Fatalf("started %d run goroutines, want 2", len(started))
		}
		synctest.Wait()
		mu.Lock()
		parked := slices.Clone(order)
		mu.Unlock()
		if want := []string{"start a/1", "start b/1", "end b/1"}; !slices.Equal(sortedSteps(parked), sortedSteps(want)) {
			t.Fatalf("while run a's first task works the lane ran %v, want %v", parked, want)
		}
		if got := publicationCount(w); got != 1 {
			t.Fatalf("publication work counted %d while only run a works, want 1", got)
		}
		release()
		for _, done := range started {
			<-done
		}

		mu.Lock()
		defer mu.Unlock()
		aOrder := slices.DeleteFunc(slices.Clone(order), func(step string) bool { return strings.HasSuffix(step, " b/1") })
		if want := []string{"start a/1", "end a/1", "start a/2", "end a/2"}; !slices.Equal(aOrder, want) {
			t.Fatalf("run a ran %v, want %v", aOrder, want)
		}
		if result, err := w.outcomes.collect(); err != nil || result.completed != 3 {
			t.Fatalf("collected %+v, %v; want 3 completed", result, err)
		}
		if got := publicationCount(w); got != 0 {
			t.Fatalf("publication work counted %d after the goroutines ended", got)
		}
	})
}

func sortedSteps(steps []string) []string {
	return slices.Sorted(slices.Values(steps))
}

// TestALaneSkipsARunStillRunning: a pass that finds a run's goroutine from
// an earlier pass still working starts nothing for that run, leaves its
// tasks for the next pass, and counts nothing for it.
func TestALaneSkipsARunStillRunning(t *testing.T) {
	t.Parallel()
	w := newDispatchLane(t)
	block := make(chan struct{})
	busy := w.runs.Go("a", func() { <-block })
	defer func() { close(block); <-busy }()

	var advanced []string
	started := w.startRuns(t.Context(), dispatchParts("a/1", "a/2"), nil, time.Now(),
		func(_ context.Context, p pendingPublicationTask) (productionPublicationResult, error) {
			advanced = append(advanced, p.entry.IdempotencyKey)
			return productionPublicationResult{}, nil
		})
	if len(started) != 0 || len(advanced) != 0 {
		t.Fatalf("started %d goroutines and advanced %v for a busy run, want none", len(started), advanced)
	}
	if got := publicationCount(w); got != 0 {
		t.Fatalf("publication work counted %d for a skipped run", got)
	}
	if _, held := w.launchWaits.earliestSince(); held {
		t.Fatal("a skipped run still keeps the launch waits")
	}
}

// TestALaneStopsAtCancelAndLeavesTheRestPending: invariant 9. Canceling the
// lane's context ends a run's goroutine before its next task, so joining the
// lane's runs returns and the later task is never advanced.
func TestALaneStopsAtCancelAndLeavesTheRestPending(t *testing.T) {
	t.Parallel()
	w := newDispatchLane(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	working := make(chan struct{})
	var advanced []string
	w.startRuns(ctx, dispatchParts("a/1", "a/2"), nil, time.Now(),
		func(ctx context.Context, p pendingPublicationTask) (productionPublicationResult, error) {
			advanced = append(advanced, p.entry.IdempotencyKey)
			close(working)
			<-ctx.Done()
			return productionPublicationResult{}, ctx.Err()
		})
	<-working
	cancel()
	w.runs.Wait()
	if !slices.Equal(advanced, []string{"a/1"}) {
		t.Fatalf("advanced %v after cancel, want only a/1", advanced)
	}
	if got := publicationCount(w); got != 0 {
		t.Fatalf("publication work counted %d after shutdown", got)
	}
}

// TestALaneTaskErrorReachesTheNextPass: invariant 8. A task goroutine's
// error is returned by the next pass, waiting or not, so a correctness error
// still stops the lane's loop; it is returned once.
func TestALaneTaskErrorReachesTheNextPass(t *testing.T) {
	t.Parallel()
	w := newDispatchLane(t)
	taskErr := fmt.Errorf("task %q: %w", "a/1", domain.ErrParentKeyMismatch)
	started := w.startRuns(t.Context(), dispatchParts("a/1"), nil, time.Now(),
		func(context.Context, pendingPublicationTask) (productionPublicationResult, error) {
			return productionPublicationResult{blocked: 1}, taskErr
		})
	for _, done := range started {
		<-done
	}
	result, err := w.reconcile(t.Context(), false)
	if !errors.Is(err, domain.ErrParentKeyMismatch) || result.blocked != 1 {
		t.Fatalf("next pass = %+v, %v; want the task's error and count", result, err)
	}
	if _, err := w.reconcile(t.Context(), false); err != nil {
		t.Fatalf("the pass after = %v, want the error returned once", err)
	}
}

// TestStartupReviewRecoveryRunsOnceUnderThePassLock: invariant 3. Recovery
// runs on the first pass, holding the pass lock every task goroutine's start
// needs, retries on the next pass when it fails, and never runs again once
// it succeeds.
func TestStartupReviewRecoveryRunsOnceUnderThePassLock(t *testing.T) {
	t.Parallel()
	w := newDispatchLane(t)
	calls := 0
	failure := errors.New("review source unreachable")
	w.reviewRecoveryPending = true
	w.reviewRecovery = func(context.Context) error {
		calls++
		if w.passMu.TryLock() {
			w.passMu.Unlock()
			t.Error("recovery ran without the pass lock")
		}
		if calls == 1 {
			return failure
		}
		return nil
	}
	if _, err := w.reconcile(t.Context(), true); !errors.Is(err, failure) {
		t.Fatalf("first pass = %v, want the recovery failure", err)
	}
	for pass := range 3 {
		if _, err := w.reconcile(t.Context(), true); err != nil {
			t.Fatalf("pass %d: %v", pass+2, err)
		}
	}
	if calls != 2 {
		t.Fatalf("recovery ran %d times, want 2: the failure and its retry", calls)
	}
}

// TestPacingStateIsSafeAcrossRuns: invariant 2. Tasks of different runs read
// and write the retry, lag, and review-deadline maps at once; under -race an
// unguarded map fails this test.
func TestPacingStateIsSafeAcrossRuns(t *testing.T) {
	t.Parallel()
	w := &productionPublicationWorkflow{
		now: time.Now, holdRetryInterval: time.Millisecond,
	}
	var wg sync.WaitGroup
	for i := range 8 {
		runID := domain.RunID(fmt.Sprintf("run-%d", i))
		task := productionPublicationTask{RunID: runID, PublicationID: domain.InvocationID(fmt.Sprintf("publication-%d", i))}
		wg.Go(func() {
			for range 50 {
				w.deferHeldTask(task)
				w.heldTaskDue(task)
				w.countLaggingSuccessorObservation(task)
				w.endLaggingSuccessorObservation(task)
				w.setReviewRetryDeadline(runID, time.Now())
				w.reviewRetryDeadline(runID)
				w.clearReviewRetryDeadline(runID)
			}
		})
	}
	wg.Go(func() {
		for range 50 {
			w.pruneHeldTaskRetries(nil)
		}
	})
	wg.Wait()
}

// TestALaneSkipsARunBusyWhenThePassRead: a run busy when the pass read its
// pending tasks is skipped even when its goroutine has ended by the time the
// pass starts runs. That read may name a task the goroutine finished, and
// advancing it again would repeat the task's work.
func TestALaneSkipsARunBusyWhenThePassRead(t *testing.T) {
	t.Parallel()
	w := newDispatchLane(t)
	var advanced []string
	started := w.startRuns(t.Context(), dispatchParts("a/1", "b/1"), map[domain.RunID]bool{"a": true}, time.Now(),
		func(_ context.Context, p pendingPublicationTask) (productionPublicationResult, error) {
			advanced = append(advanced, p.entry.IdempotencyKey)
			return productionPublicationResult{}, nil
		})
	for _, done := range started {
		<-done
	}
	if !slices.Equal(advanced, []string{"b/1"}) {
		t.Fatalf("advanced %v, want only b/1: run a was busy at the read", advanced)
	}
}

// TestLaneRunsTracksBusyRunsAndRefusesAfterWait: a run is busy from the
// moment Go starts it until its goroutine ends, a second Go for a busy run
// starts nothing, and once Wait begins no run starts.
func TestLaneRunsTracksBusyRunsAndRefusesAfterWait(t *testing.T) {
	t.Parallel()
	var runs laneRuns
	release := make(chan struct{})
	done := runs.Go("a", func() { <-release })
	if done == nil || !runs.Busy()["a"] {
		t.Fatal("run a is not busy after Go started it")
	}
	if again := runs.Go("a", func() { t.Error("a second goroutine ran for a busy run") }); again != nil {
		t.Fatal("Go started a second goroutine for a busy run")
	}
	close(release)
	<-done
	if runs.Busy()["a"] {
		t.Fatal("run a is still busy after its goroutine ended")
	}
	runs.Wait()
	if late := runs.Go("b", func() { t.Error("a run started after Wait") }); late != nil {
		t.Fatal("Go started a run after Wait")
	}
}

// TestPublicationLoopStopsOnATaskErrorAfterJoiningItsRuns: invariants 8
// and 9 through the daemon's loop. An error a task goroutine recorded stops
// RunProductionPublications on its next pass, and the loop returns only
// after every run goroutine still working has ended.
func TestPublicationLoopStopsOnATaskErrorAfterJoiningItsRuns(t *testing.T) {
	t.Parallel()
	w := newDispatchLane(t)
	e, err := New(w.store, signet.NewService(w.store), execfake.NewStageDriver())
	if err != nil {
		t.Fatal(err)
	}
	e.productionPublication = w
	w.outcomes.record(productionPublicationResult{}, fmt.Errorf("task %q: %w", "a/1", domain.ErrParentKeyMismatch))
	release := make(chan struct{})
	var ended atomic.Bool
	if w.runs.Go("b", func() { <-release; ended.Store(true) }) == nil {
		t.Fatal("run b did not start")
	}

	stopped := make(chan error, 1)
	go func() { stopped <- e.RunProductionPublications(t.Context(), time.Hour) }()
	// The loop's first pass returns the error; its shutdown then closes the
	// lane and joins run b.
	for !laneClosed(&w.runs) {
		select {
		case err := <-stopped:
			t.Fatalf("loop returned %v before joining run b", err)
		case <-t.Context().Done():
			t.Fatal("loop never began its shutdown")
		default:
			runtime.Gosched()
		}
	}
	close(release)
	if err := <-stopped; !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("loop returned %v, want the task's error", err)
	}
	if !ended.Load() {
		t.Fatal("loop returned before run b ended")
	}
}

func laneClosed(l *laneRuns) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}
