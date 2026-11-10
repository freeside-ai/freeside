package daemoncompose

import (
	"context"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
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
	publication, err := tx.PublishedPublicationInvocationID(ctx, runID)
	if err != nil {
		return err
	}
	published, err := signet.PublicationCycleObservation(ctx, &tx.ReadTx, observation, publication)
	if err != nil {
		return err
	}
	if !domain.PublicationReadyStands(published) {
		return nil
	}
	milestone, err := CurrentWorkUnitCompletedMilestone(ctx, &tx.ReadTx, runID, completion)
	if err != nil {
		return err
	}
	return tx.AppendRunMilestone(ctx, milestone)
}

func CurrentWorkUnitCompletedMilestone(ctx context.Context, tx *store.ReadTx, runID domain.RunID, completion domain.WorkUnitCompletion) (domain.RunMilestone, error) {
	invocation, err := tx.PublishedPublicationInvocationID(ctx, runID)
	if err != nil {
		return domain.RunMilestone{}, err
	}
	milestone := workUnitCompletedMilestone(runID, completion)
	milestone.InvocationID = &invocation
	return milestone, nil
}

// workUnitCompletedMilestone is the observation mirror of a recorded
// completion (#1134): invocation-only, on the run's publication invocation,
// at the record's instant. The record stays the single authority for the
// PR, merge commit, and bound issue; the sync boundary re-binds the
// milestone to it on every read. Writers go through
// appendWorkUnitCompletedMilestone, which applies the boundary's
// precondition.
func workUnitCompletedMilestone(runID domain.RunID, completion domain.WorkUnitCompletion) domain.RunMilestone {
	invocation := domain.ProductionPublicationInvocationID(runID)
	return domain.RunMilestone{
		RunID: runID, Kind: domain.MilestoneWorkUnitCompleted,
		InvocationID: &invocation, RecordedAt: completion.RecordedAt,
	}
}
