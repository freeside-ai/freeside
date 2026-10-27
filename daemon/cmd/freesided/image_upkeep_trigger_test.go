package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
)

// The reconciler's handoff never blocks and keeps only the newest tip; the
// worker runs the start pass first, then the handed-off tip, and the failure
// of either is logged rather than ending it.
func TestImageUpkeepTriggerRunsTheStartPassThenTheNewestTip(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		trigger := newImageUpkeepTrigger()
		for _, tip := range []string{"older", "newer"} {
			if err := trigger.tipAdvanced(t.Context(), domain.RepositoryTipObservation{
				RepositoryID: 7, TipSHA: tip,
			}); err != nil {
				t.Fatal(err)
			}
		}
		starts := 0
		// Neither pass reaches the store: the start pass fails reading the
		// tip, and the tip pass names a repository upkeep does not serve.
		upkeep := &engine.ImageUpkeep{
			RepositoryID: 42,
			ReadTip: func(context.Context) (string, error) {
				starts++
				return "", errors.New("tip unreadable")
			},
		}
		var logs lockedBuffer
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			trigger.run(ctx, upkeep, slog.New(slog.NewTextHandler(&logs, nil)))
		}()
		synctest.Wait()
		got := logs.String()
		if starts != 1 || !strings.Contains(got, "image upkeep start pass failed") ||
			strings.Count(got, "image upkeep tip pass failed") != 1 || !strings.Contains(got, "tip_sha=newer") {
			t.Fatalf("starts = %d, logs:\n%s\nwant one start pass, then one pass at the newer tip", starts, got)
		}
		cancel()
		<-done
	})
}
