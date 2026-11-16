package daemoncompose

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

var pauseEpoch = time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)

func openPauseStore(t *testing.T) *store.Store {
	t.Helper()
	return storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
}

func requirePauseState(t *testing.T, st *store.Store, want domain.ExecutionPauseState) {
	t.Helper()
	pause, err := ReadExecutionPause(t.Context(), st)
	if err != nil {
		t.Fatal(err)
	}
	if want == "" {
		if pause != nil {
			t.Fatalf("pause = %+v, want none", pause)
		}
		return
	}
	if pause == nil || pause.State != want {
		t.Fatalf("pause = %+v, want %s", pause, want)
	}
}

// pauseCards counts the advisory system_health cards whose IDs start with
// prefix and carry code, open or concluded.
func pauseCards(t *testing.T, st *store.Store, prefix, code string) (open, concluded int) {
	t.Helper()
	ctx := t.Context()
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		items, err := tx.ListAttentionItems(ctx)
		if err != nil {
			return err
		}
		for _, snapshot := range items {
			item := snapshot.Value
			if !strings.HasPrefix(string(item.ID), prefix) {
				continue
			}
			if item.HealthDiagnostic == nil || item.HealthDiagnostic.Code != code ||
				*item.Posture != domain.HealthPostureAdvisory {
				t.Errorf("pause card %s: diagnostic %+v, posture %s", item.ID, item.HealthDiagnostic, *item.Posture)
			}
			if item.Status == domain.StatusOpen {
				open++
			} else {
				concluded++
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return open, concluded
}

// TestExecutionPauseRequestAndResume: a request records the pause and one
// advisory card; a repeated request changes nothing; resume clears both and
// reports it did; a resume with nothing in force reports nothing; and a
// release never overwrites a resume that committed first.
func TestExecutionPauseRequestAndResume(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st := openPauseStore(t)
	for range 2 {
		if err := RequestExecutionPause(ctx, st, "cli:operator", pauseEpoch); err != nil {
			t.Fatal(err)
		}
		requirePauseState(t, st, domain.ExecutionPauseRequested)
		if open, concluded := pauseCards(t, st, executionPauseItemPrefix, executionPauseCode); open != 1 || concluded != 0 {
			t.Fatalf("cards: %d open, %d concluded; want 1 open", open, concluded)
		}
	}
	// A pause already released is kept as it is too.
	if released, err := releaseExecutionPause(ctx, st, pauseEpoch); err != nil || !released {
		t.Fatalf("release = %t, %v", released, err)
	}
	if err := RequestExecutionPause(ctx, st, "cli:operator", pauseEpoch); err != nil {
		t.Fatal(err)
	}
	requirePauseState(t, st, domain.ExecutionPauseReleased)

	if cleared, err := ResumeExecution(ctx, st, "cli:operator", pauseEpoch); err != nil || !cleared {
		t.Fatalf("resume = %t, %v; want cleared", cleared, err)
	}
	requirePauseState(t, st, "")
	if open, concluded := pauseCards(t, st, executionPauseItemPrefix, executionPauseCode); open != 0 || concluded != 1 {
		t.Fatalf("cards after resume: %d open, %d concluded; want 1 concluded", open, concluded)
	}
	if cleared, err := ResumeExecution(ctx, st, "cli:operator", pauseEpoch); err != nil || cleared {
		t.Fatalf("second resume = %t, %v; want nothing cleared", cleared, err)
	}
	if released, err := releaseExecutionPause(ctx, st, pauseEpoch); err != nil || released {
		t.Fatalf("release after resume = %t, %v; want none", released, err)
	}
	requirePauseState(t, st, "")
}

// TestExecutionPauseResumeRestartsOnlyADriverlessDaemon: resume clears a
// requested pause either way, but restarts only a daemon without a driver,
// so cancelling a pause never interrupts the work it was waiting on.
func TestExecutionPauseResumeRestartsOnlyADriverlessDaemon(t *testing.T) {
	t.Parallel()
	for _, executes := range []bool{true, false} {
		st := openPauseStore(t)
		if err := RequestExecutionPause(t.Context(), st, "cli:operator", pauseEpoch); err != nil {
			t.Fatal(err)
		}
		restarts := 0
		control := &ExecutionPauseControl{
			store: st,
			readiness: func(context.Context) (signet.RestartReadinessResponse, error) {
				return signet.RestartReadinessResponse{
					Blockers: []domain.RestartBlocker{{Kind: domain.AllRestartBlockerKinds[0], Count: 1}},
				}, nil
			},
			restart: func() { restarts++ },
			now:     func() time.Time { return pauseEpoch }, executes: executes,
		}
		status, err := control.Resume(t.Context(), "cli:operator")
		if err != nil {
			t.Fatal(err)
		}
		requirePauseState(t, st, "")
		wantRestarts := 1
		if executes {
			wantRestarts = 0
		}
		if status.Executes != executes || status.Restarting != !executes || restarts != wantRestarts {
			t.Fatalf("executes %t: status %+v, %d restarts; want %d", executes, status, restarts, wantRestarts)
		}
	}
}

// TestExecutionPauseWaiterReleasesOnceNothingRuns: the waiter ignores a store
// with no pause, holds a requested pause while any restart blocker kind is
// counted, and once none is records the pause released and asks for one
// restart, then waits for the restart's cancel.
func TestExecutionPauseWaiterReleasesOnceNothingRuns(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		st := openPauseStore(t)
		var mu sync.Mutex
		var blockers []domain.RestartBlocker
		setBlockers := func(b []domain.RestartBlocker) {
			mu.Lock()
			defer mu.Unlock()
			blockers = b
		}
		var restarts atomic.Int32
		waiter := executionPauseWaiter{
			store: st,
			readiness: func(context.Context) (signet.RestartReadinessResponse, error) {
				mu.Lock()
				defer mu.Unlock()
				return signet.RestartReadinessResponse{Blockers: blockers}, nil
			},
			restart:   func() { restarts.Add(1) },
			leaseRoot: t.TempDir(), now: time.Now, logger: slog.New(slog.DiscardHandler),
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- waiter.run(ctx) }()

		synctest.Sleep(executionPauseInterval + time.Second)
		requirePauseState(t, st, "")
		if err := RequestExecutionPause(ctx, st, "cli:operator", time.Now()); err != nil {
			t.Fatal(err)
		}
		for _, kind := range domain.AllRestartBlockerKinds {
			setBlockers([]domain.RestartBlocker{{Kind: kind, Count: 1}})
			synctest.Sleep(executionPauseInterval)
			requirePauseState(t, st, domain.ExecutionPauseRequested)
			if restarts.Load() != 0 {
				t.Fatalf("%s counted: restart requested", kind)
			}
		}
		setBlockers(nil)
		synctest.Sleep(executionPauseInterval)
		requirePauseState(t, st, domain.ExecutionPauseReleased)
		if got := restarts.Load(); got != 1 {
			t.Fatalf("restarts = %d, want 1", got)
		}
		synctest.Sleep(3 * executionPauseInterval)
		if got := restarts.Load(); got != 1 {
			t.Fatalf("restarts after the release = %d, want 1", got)
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("waiter = %v, want nil on cancel", err)
		}
	})
}

// TestExecutionPauseWaiterRaisesTheUnusedCard: with the pause released, the
// waiter raises the unused-pause card once no rig lease manifest has existed
// for an hour, concludes it when a manifest appears, counts the hour again
// from the manifest's last sighting, and leaves no card once resume clears
// the pause. It never asks for a restart.
func TestExecutionPauseWaiterRaisesTheUnusedCard(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		st := openPauseStore(t)
		if err := RequestExecutionPause(ctx, st, "cli:operator", time.Now()); err != nil {
			t.Fatal(err)
		}
		if released, err := releaseExecutionPause(ctx, st, time.Now()); err != nil || !released {
			t.Fatalf("release = %t, %v", released, err)
		}
		leaseRoot := t.TempDir()
		manifest := filepath.Join(leaseRoot, "production-rig.json")
		var restarts atomic.Int32
		waiter := executionPauseWaiter{
			store: st,
			readiness: func(context.Context) (signet.RestartReadinessResponse, error) {
				return signet.RestartReadinessResponse{}, nil
			},
			restart:   func() { restarts.Add(1) },
			leaseRoot: leaseRoot, now: time.Now, logger: slog.New(slog.DiscardHandler),
		}
		done := make(chan error, 1)
		go func() { done <- waiter.run(ctx) }()
		requireUnused := func(step string, wantOpen, wantConcluded int) {
			t.Helper()
			synctest.Wait()
			open, concluded := pauseCards(t, st, unusedExecutionPauseItemPrefix, unusedExecutionPauseCode)
			if open != wantOpen || concluded != wantConcluded {
				t.Fatalf("%s: %d open, %d concluded; want %d open, %d concluded",
					step, open, concluded, wantOpen, wantConcluded)
			}
		}

		synctest.Sleep(executionPauseUnusedAfter - time.Second)
		requireUnused("just short of the hour", 0, 0)
		synctest.Sleep(executionPauseInterval)
		requireUnused("at the hour", 1, 0)

		if err := os.WriteFile(manifest, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		synctest.Sleep(executionPauseInterval)
		requireUnused("with a lease", 0, 1)
		if err := os.Remove(manifest); err != nil {
			t.Fatal(err)
		}
		synctest.Sleep(executionPauseUnusedAfter - executionPauseInterval)
		requireUnused("an hour after the lease was last seen, less a tick", 0, 1)
		synctest.Sleep(executionPauseInterval)
		requireUnused("an hour after the lease was last seen", 1, 1)

		if _, err := ResumeExecution(ctx, st, "cli:operator", time.Now()); err != nil {
			t.Fatal(err)
		}
		synctest.Sleep(executionPauseUnusedAfter + executionPauseInterval)
		requireUnused("after resume", 0, 2)
		if got := restarts.Load(); got != 0 {
			t.Fatalf("restarts = %d, want none", got)
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("waiter = %v, want nil on cancel", err)
		}
	})
}
