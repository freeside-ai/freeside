package signet

import (
	"context"
	"errors"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// SettledRunConclusion applies domain.SettleRunConclusion to an authenticated
// conclusion, reading the run's ending facts in the caller's transaction. It
// serves the read surfaces (the run API, follow and resume, and inspect
// task); the engine and the Tier A gates read the unsettled conclusion
// (#3043). A conclusion that is already final needs no facts, so it is
// returned without reading any.
func SettledRunConclusion(
	ctx context.Context,
	tx *store.ReadTx,
	run domain.Run,
	observation domain.RunObservation,
	conclusion domain.RunConclusion,
) (domain.RunConclusion, error) {
	if conclusion.Final {
		return conclusion, nil
	}
	ending, err := readRunEnding(ctx, tx, run)
	if err != nil {
		return domain.RunConclusion{}, err
	}
	return domain.SettleRunConclusion(run, conclusion, observation, ending), nil
}

// readRunEnding gathers the facts that end a run no milestone concludes:
// the specification hand-off (boundImplementationRun), the task's confirmed
// cancellation naming the run, and its closed workflow episode. It reads them
// in precedence order and stops at the first that holds, since a lower one
// can't change the settlement; a handed-off run then skips the closure scan
// on every listing.
func readRunEnding(ctx context.Context, tx *store.ReadTx, run domain.Run) (domain.RunEnding, error) {
	var ending domain.RunEnding
	if run.CampaignID != "" {
		attempt, err := tx.GetProductionAttempt(ctx, run.CampaignID, run.AttemptNumber)
		if err != nil {
			return domain.RunEnding{}, err
		}
		if _, ending.HandedOff = boundImplementationRun(run, attempt); ending.HandedOff {
			return ending, nil
		}
	}
	task, err := tx.GetTask(ctx, run.TaskID)
	if err != nil {
		return domain.RunEnding{}, err
	}
	if cancellation := task.Cancellation; cancellation != nil &&
		cancellation.State == domain.TaskCancellationConfirmed {
		ending.Stopped = slices.ContainsFunc(cancellation.Target.Runs, func(target domain.TaskCancellationRun) bool {
			return target.RunID == run.ID
		})
		if ending.Stopped {
			return ending, nil
		}
	}
	switch err := tx.RequireRunWorkflowOpen(ctx, run.ID); {
	case errors.Is(err, store.ErrTaskWorkflowClosed):
		ending.Abandoned = true
	case err != nil:
		return domain.RunEnding{}, err
	}
	return ending, nil
}
