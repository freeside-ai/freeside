package daemoncompose

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
)

// imageUpkeepTrigger runs the start and tip-advance triggers of image upkeep
// (plan §5.7, Project-image upkeep, Triggers) on one worker per repository
// (#3210), so one repository's hour-long build never delays another's
// pass. The active-resource reconciler's pass must not wait for a build
// either, so its tip-advance handoff keeps only each repository's newest
// unhandled tip and returns; a newer tip supersedes an older one, because
// upkeep keeps the image current at the tip, not at every commit it passed
// through. One worker per repository also keeps the start pass and a tip
// pass from fetching and measuring the same tip side by side.
//
// An approved image-policy adoption wakes every worker for another start
// pass (wake), so upkeep builds under the adopted policy without waiting
// for the next tip.
type imageUpkeepTrigger struct {
	// workers are the repositories' workers, keyed by repository ID. The
	// map is fixed at construction, so reading it needs no lock.
	workers map[int64]*imageUpkeepWorker
}

// imageUpkeepWorker is one repository's upkeep and the handoff to its
// worker.
type imageUpkeepWorker struct {
	upkeep *engine.ImageUpkeep
	// pending holds at most the newest tip the worker has not taken yet.
	pending chan domain.RepositoryTipObservation
	// rerun holds at most one wake the worker has not taken yet.
	rerun chan struct{}
}

// newImageUpkeepTrigger has a worker for each upkeep's repository.
func newImageUpkeepTrigger(upkeeps ...*engine.ImageUpkeep) *imageUpkeepTrigger {
	t := &imageUpkeepTrigger{workers: make(map[int64]*imageUpkeepWorker, len(upkeeps))}
	for _, upkeep := range upkeeps {
		t.workers[upkeep.RepositoryID] = &imageUpkeepWorker{
			upkeep:  upkeep,
			pending: make(chan domain.RepositoryTipObservation, 1),
			rerun:   make(chan struct{}, 1),
		}
	}
	return t
}

// Wake is signet's image-upkeep wake, for every repository. It never
// blocks: a wake already pending covers this one, because the pass it runs
// reads the current adoptions. The wake is a latency hint; the start pass
// and the doctor's schedule cover a missed one.
func (t *imageUpkeepTrigger) Wake() {
	for _, worker := range t.workers {
		select {
		case worker.rerun <- struct{}{}:
		default:
		}
	}
}

// tipAdvanced is the reconciler's upkeepTipAdvance entry point. It hands
// the tip to its repository's worker and never blocks: a tip still pending
// for that repository is replaced. The reconciler is its one caller, so the
// replacement cannot race another sender. A repository with no worker is
// refused, since nothing would keep its image.
func (t *imageUpkeepTrigger) tipAdvanced(_ context.Context, observation domain.RepositoryTipObservation) error {
	worker, ok := t.workers[observation.RepositoryID]
	if !ok {
		return fmt.Errorf("image upkeep serves no repository %d: %w",
			observation.RepositoryID, domain.ErrRepositoryIdentityMismatch)
	}
	for {
		select {
		case worker.pending <- observation:
			return nil
		default:
		}
		select {
		case <-worker.pending:
		default:
		}
	}
}

// Run runs every repository's worker until ctx ends, and returns once all
// have stopped.
func (t *imageUpkeepTrigger) Run(ctx context.Context, logger *slog.Logger) {
	var workers sync.WaitGroup
	for _, worker := range t.workers {
		workers.Go(func() {
			worker.run(ctx, logger.With("repository_id", worker.upkeep.RepositoryID))
		})
	}
	workers.Wait()
}

// run is one repository's worker: the start pass, then one pass per
// handed-off tip or wake, until ctx ends. A failed pass is not fatal; the
// doctor's schedule retries. A tip handed off but not yet run when the
// daemon stops is covered by the next start pass, which reads the tip
// itself.
func (w *imageUpkeepWorker) run(ctx context.Context, logger *slog.Logger) {
	if err := w.upkeep.RunAll(ctx); err != nil && ctx.Err() == nil {
		logger.Warn("image upkeep start pass failed", "error", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case observation := <-w.pending:
			if err := w.upkeep.TipAdvanced(ctx, observation.RepositoryID, observation.TipSHA); err != nil &&
				ctx.Err() == nil {
				logger.Warn("image upkeep tip pass failed", "tip_sha", observation.TipSHA, "error", err)
			}
		case <-w.rerun:
			if err := w.upkeep.RunAll(ctx); err != nil && ctx.Err() == nil {
				logger.Warn("image upkeep adoption pass failed", "error", err)
			}
		}
	}
}
