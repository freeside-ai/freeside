package main

import (
	"context"
	"log/slog"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
)

// imageUpkeepTrigger runs the start and tip-advance triggers of image upkeep
// (plan §5.7, Project-image upkeep, Triggers) on one worker. The
// active-resource reconciler's pass must not wait for a build that can take
// an hour, so its tip-advance handoff keeps only the newest unhandled tip
// and returns; a newer tip supersedes an older one, because upkeep keeps the
// image current at the tip, not at every commit it passed through. One
// worker also keeps the start pass and a tip pass from fetching and
// measuring the same tip side by side.
//
// An approved image-policy adoption wakes the worker for another start pass
// (wake), so upkeep builds under the adopted policy without waiting for the
// next tip.
type imageUpkeepTrigger struct {
	// pending holds at most the newest tip the worker has not taken yet.
	pending chan domain.RepositoryTipObservation
	// rerun holds at most one wake the worker has not taken yet.
	rerun chan struct{}
}

func newImageUpkeepTrigger() *imageUpkeepTrigger {
	return &imageUpkeepTrigger{
		pending: make(chan domain.RepositoryTipObservation, 1),
		rerun:   make(chan struct{}, 1),
	}
}

// wake is signet's image-upkeep wake. It never blocks: a wake already
// pending covers this one, because the pass it runs reads the current
// adoptions. The wake is a latency hint; the start pass and the doctor's
// schedule cover a missed one.
func (t *imageUpkeepTrigger) wake() {
	select {
	case t.rerun <- struct{}{}:
	default:
	}
}

// tipAdvanced is the reconciler's upkeepTipAdvance entry point. It never
// blocks: a tip still pending is replaced. The reconciler is its one caller,
// so the replacement cannot race another sender.
func (t *imageUpkeepTrigger) tipAdvanced(_ context.Context, observation domain.RepositoryTipObservation) error {
	for {
		select {
		case t.pending <- observation:
			return nil
		default:
		}
		select {
		case <-t.pending:
		default:
		}
	}
}

// run is the worker: the start pass, then one pass per handed-off tip or
// wake, until ctx ends. A failed pass is not fatal; the doctor's schedule retries. A tip
// handed off but not yet run when the daemon stops is covered by the next
// start pass, which reads the tip itself.
func (t *imageUpkeepTrigger) run(ctx context.Context, upkeep *engine.ImageUpkeep, logger *slog.Logger) {
	if err := upkeep.RunAll(ctx); err != nil && ctx.Err() == nil {
		logger.Warn("image upkeep start pass failed", "error", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case observation := <-t.pending:
			if err := upkeep.TipAdvanced(ctx, observation.RepositoryID, observation.TipSHA); err != nil &&
				ctx.Err() == nil {
				logger.Warn("image upkeep tip pass failed", "tip_sha", observation.TipSHA, "error", err)
			}
		case <-t.rerun:
			if err := upkeep.RunAll(ctx); err != nil && ctx.Err() == nil {
				logger.Warn("image upkeep adoption pass failed", "error", err)
			}
		}
	}
}
