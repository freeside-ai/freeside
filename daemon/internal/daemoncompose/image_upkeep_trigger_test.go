package daemoncompose

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// closedUpkeepStore is a store every read of fails at once, so a tip pass
// fails before it fetches or builds anything.
func closedUpkeepStore(t *testing.T) *store.Store {
	t.Helper()
	st := storetest.Open(t, t.TempDir()+"/freesided.db", store.Options{})
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return st
}

// failingUpkeep is an upkeep whose start pass fails reading the tip and
// whose tip pass fails reading the store; starts counts its start passes.
func failingUpkeep(st *store.Store, repositoryID int64, starts *int) *engine.ImageUpkeep {
	return &engine.ImageUpkeep{
		Store: st, RepositoryID: repositoryID,
		ReadTip: func(context.Context) (string, error) {
			*starts++
			return "", errors.New("tip unreadable")
		},
	}
}

// runTrigger runs the trigger's workers until the bubble is idle, then
// stops them and returns their logs.
func runTrigger(t *testing.T, trigger *imageUpkeepTrigger) string {
	t.Helper()
	var logs lockedBuffer
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		trigger.Run(ctx, slog.New(slog.NewTextHandler(&logs, nil)))
	}()
	synctest.Wait()
	cancel()
	<-done
	return logs.String()
}

// The reconciler's handoff never blocks and keeps only the newest tip of
// each repository; a worker runs the start pass first, then the handed-off
// tip, and the failure of either is logged rather than ending it. A tip of
// a repository no upkeep serves is refused.
func TestImageUpkeepTriggerRunsTheStartPassThenTheNewestTip(t *testing.T) {
	t.Parallel()
	st := closedUpkeepStore(t)
	synctest.Test(t, func(t *testing.T) {
		starts := 0
		trigger := newImageUpkeepTrigger(failingUpkeep(st, 42, &starts))
		for _, tip := range []string{"older", "newer"} {
			if err := trigger.tipAdvanced(t.Context(), domain.RepositoryTipObservation{
				RepositoryID: 42, TipSHA: tip,
			}); err != nil {
				t.Fatal(err)
			}
		}
		if err := trigger.tipAdvanced(t.Context(), domain.RepositoryTipObservation{RepositoryID: 7, TipSHA: "x"}); !errors.Is(
			err, domain.ErrRepositoryIdentityMismatch) {
			t.Fatalf("a tip of an unserved repository = %v, want refused", err)
		}
		got := runTrigger(t, trigger)
		if starts != 1 || !strings.Contains(got, "image upkeep start pass failed") ||
			strings.Count(got, "image upkeep tip pass failed") != 1 || !strings.Contains(got, "tip_sha=newer") {
			t.Fatalf("starts = %d, logs:\n%s\nwant one start pass, then one pass at the newer tip", starts, got)
		}
	})
}

// A wake runs one more start pass for every repository, and wakes a worker
// has not taken yet collapse into one.
func TestImageUpkeepTriggerRunsAPassPerWake(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var first, second int
		trigger := newImageUpkeepTrigger(failingUpkeep(nil, 42, &first), failingUpkeep(nil, 43, &second))
		trigger.Wake()
		trigger.Wake()
		got := runTrigger(t, trigger)
		if first != 2 || second != 2 || strings.Count(got, "image upkeep adoption pass failed") != 2 {
			t.Fatalf("starts = %d and %d, logs:\n%s\nwant each repository's start pass and one pass for the wakes",
				first, second, got)
		}
	})
}

// One repository's pass never delays another's: while the first
// repository's start pass is stuck, the second's start and tip passes run.
func TestImageUpkeepTriggerRunsEachRepositoryApart(t *testing.T) {
	t.Parallel()
	st := closedUpkeepStore(t)
	synctest.Test(t, func(t *testing.T) {
		stuck := &engine.ImageUpkeep{
			Store: st, RepositoryID: 42,
			ReadTip: func(ctx context.Context) (string, error) {
				<-ctx.Done()
				return "", ctx.Err()
			},
		}
		starts := 0
		trigger := newImageUpkeepTrigger(stuck, failingUpkeep(st, 43, &starts))
		if err := trigger.tipAdvanced(t.Context(), domain.RepositoryTipObservation{RepositoryID: 43, TipSHA: "tip"}); err != nil {
			t.Fatal(err)
		}
		got := runTrigger(t, trigger)
		if starts != 1 || !strings.Contains(got, "repository_id=43") ||
			strings.Count(got, "image upkeep tip pass failed") != 1 || strings.Contains(got, "repository_id=42") {
			t.Fatalf("starts = %d, logs:\n%s\nwant repository 43's passes while 42's is stuck", starts, got)
		}
	})
}
