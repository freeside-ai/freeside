package engine

import (
	"context"
	"errors"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// A maintenance task project-image upkeep submits for a repair-class refusal
// runs at the refused tip, on the image the refusal was measured from (plan
// §5.7, "A maintenance task runs on the image it fixes"). That is the one
// bounded exception to the image-admissibility checks, and repairTaskRefusal
// is the only way a run qualifies for it.

// repairTaskRefusal returns the refusal a run repairs, or nil for every run
// that is not a repair task's. A run qualifies only when all of these hold,
// each re-read from the store rather than taken from the run:
//
//   - Its task's source artifact digest names a recorded refusal. One path
//     covers the specification run and every implementation run.
//   - The refusal is repair-class and names the run's project's repository.
//   - The submission recorded under that refusal's daemon identity is
//     daemon-origin and created this run's task. A device task whose source
//     bytes happen to equal a refusal's preimage has the same digest but
//     another task, so it gets the ordinary checks.
//
// The caller still checks the run's base and image against the refusal.
func repairTaskRefusal(ctx context.Context, tx *store.ReadTx, run domain.Run) (*domain.ImageUpkeepRefusal, error) {
	if run.TaskID == "" {
		return nil, nil
	}
	task, err := tx.GetTask(ctx, run.TaskID)
	if err != nil {
		return nil, err
	}
	if task.Source == nil || task.Source.Kind != domain.SpecificationSourceWorkItemArtifact {
		return nil, nil
	}
	source, err := tx.GetArtifact(ctx, task.Source.WorkItemArtifactID)
	if err != nil {
		return nil, err
	}
	refusal, err := tx.GetImageUpkeepRefusal(ctx, source.Digest)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	project, err := tx.GetProject(ctx, run.ProjectID)
	if err != nil {
		return nil, err
	}
	if refusal.Class != domain.ImageUpkeepRefusalRepair || refusal.RepositoryID != project.RepositoryID {
		return nil, nil
	}
	submission, err := tx.GetTaskSubmission(ctx, domain.ImageUpkeepSubmissionIdentity(refusal.ID))
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if submission.Origin != domain.TaskSubmissionOriginDaemon || submission.TaskID != run.TaskID {
		return nil, nil
	}
	return &refusal, nil
}

// readRepairTaskRefusal reads repairTaskRefusal in its own transaction.
func readRepairTaskRefusal(ctx context.Context, st *store.Store, run domain.Run) (*domain.ImageUpkeepRefusal, error) {
	var refusal *domain.ImageUpkeepRefusal
	err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		refusal, err = repairTaskRefusal(ctx, tx, run)
		return err
	})
	return refusal, err
}
