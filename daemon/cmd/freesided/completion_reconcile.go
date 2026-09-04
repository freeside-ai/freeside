package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// appendWorkUnitCompletedMilestone mirrors a recorded completion as the
// run's work_unit_completed milestone, at the record's instant so a replay
// converges on the first-observed row. The milestone is append-only and the
// sync boundary serves it only over a timeline whose publication_ready
// authority stands (domain.PublicationReadyStands), so the mirror is written
// only under that same precondition: a milestone the boundary would refuse
// would exclude the run from every listing for good, whereas a completion
// row with no milestone merely keeps the run at its publication outcome.
func appendWorkUnitCompletedMilestone(
	ctx context.Context, tx *store.WriteTx, runID domain.RunID, completion domain.WorkUnitCompletion,
) error {
	observation, err := tx.ObserveRun(ctx, runID)
	if err != nil {
		return err
	}
	if !domain.PublicationReadyStands(observation) {
		return nil
	}
	return tx.AppendRunMilestone(ctx, workUnitCompletedMilestone(runID, completion))
}

// reconcileWorkUnitCompletionMilestones is the one-time start-up pass that
// gives a completion recorded before migration 0066 its work_unit_completed
// milestone (#1134), so a run that finished under an older daemon lists as
// completed after the upgrade instead of staying "ready" forever. It reads
// every re-gated completion row, and appends the milestone only for a run
// whose timeline lacks it and whose publication_ready authority stands; a
// store already carrying the milestones is a no-op that bumps no sync
// revision. A row the re-gate refuses, a run whose declaration or timeline
// cannot be read, or a timeline whose ready authority does not stand is
// logged and skipped, never mirrored and never fatal: the same per-run
// tolerance the listing reads apply, so one damaged run cannot keep the
// daemon from starting. Skipping is reserved for the store's own per-row
// verdicts (store.IsRowVerdict): an infrastructure failure, whether listing
// the rows or reading one of them, is fatal, because a cancelled context or
// a database error is not evidence that a run has no completion and would
// otherwise leave a healthy run at its publication outcome until the next
// restart.
func reconcileWorkUnitCompletionMilestones(ctx context.Context, st *store.Store, logger *slog.Logger) error {
	var pending []domain.RunMilestone
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		completions, unsupported, err := tx.ListWorkUnitCompletions(ctx)
		if err != nil {
			return err
		}
		for _, unitID := range unsupported {
			logger.Warn("work unit completion row is not supported by its evidence; no milestone appended",
				"unit", unitID)
		}
		for _, completion := range completions {
			declaration, err := tx.GetWorkUnitDeclaration(ctx, completion.UnitID)
			if store.IsRowVerdict(err) {
				logger.Warn("work unit completion has no readable declaration; no milestone appended",
					"unit", completion.UnitID, "error", err)
				continue
			}
			if err != nil {
				return err
			}
			observation, err := tx.ObserveRun(ctx, declaration.RunID)
			if store.IsRowVerdict(err) {
				logger.Warn("work unit completion run has no readable timeline; no milestone appended",
					"unit", completion.UnitID, "run", declaration.RunID, "error", err)
				continue
			}
			if err != nil {
				return err
			}
			if hasMilestone(observation, domain.MilestoneWorkUnitCompleted) {
				continue
			}
			if !domain.PublicationReadyStands(observation) {
				logger.Warn("work unit completion run has no standing publication_ready; no milestone appended",
					"unit", completion.UnitID, "run", declaration.RunID)
				continue
			}
			pending = append(pending, workUnitCompletedMilestone(declaration.RunID, completion))
		}
		return nil
	}); err != nil {
		return fmt.Errorf("reconcile work unit completion milestones: %w", err)
	}
	if len(pending) == 0 {
		return nil
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		for _, milestone := range pending {
			if err := tx.AppendRunMilestone(ctx, milestone); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("reconcile work unit completion milestones: %w", err)
	}
	logger.Info("appended work_unit_completed milestones for pre-existing completions", "runs", len(pending))
	return nil
}

func hasMilestone(observation domain.RunObservation, kind domain.RunMilestoneKind) bool {
	for _, milestone := range observation.Milestones {
		if milestone.Kind == kind {
			return true
		}
	}
	return false
}
