package daemoncompose

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

func TestActiveResourceRunWakesForNewReadyItem(t *testing.T) {
	t.Parallel()
	st := schedTestStore(t)
	observed := make(chan struct{}, 1)
	reconciler := ActiveResourceReconciler{
		Store: st,
		Pull: func(context.Context, string, int) (publish.PullObservation, error) {
			select {
			case observed <- struct{}{}:
			default:
			}
			return exactPull("open", false), nil
		},
		Now: func() time.Time { return time.Now().UTC() },
	}

	const (
		// A wide idle interval keeps the wake-versus-idle discriminator robust
		// under -race: the ReadyItemCreated wake fires an immediate pass, so the
		// observation arrives in well under a second, while the idle timer would
		// not fire for ten. The select below allows half that interval, clearing
		// the wake comfortably while still failing if the wake were lost. -race
		// slows a pass's CPU work, not the wall-clock timer, so the separation
		// holds; a tight interval (a few hundred ms) would sit close enough to a
		// pass's own work time for jitter to erase, reddening the race job.
		defaultInterval  = 10 * time.Second
		operatorInterval = 10 * time.Millisecond
	)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- reconciler.Run(ctx, defaultInterval, operatorInterval, slog.New(slog.DiscardHandler))
	}()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("Run did not stop after cancellation")
		}
	}()

	// Let the startup pass enter its idle wait before committing the item. The
	// observation must arrive well before that idle timer would fire.
	time.Sleep(20 * time.Millisecond)
	capturedRunWithCriterion(t, st, "run-wake-ready", "item-wake-ready", domain.CompletionBoundPRMerged, nil)
	select {
	case <-observed:
	case <-time.After(defaultInterval / 2):
		t.Fatal("new ready item waited for the idle cadence")
	}
}
