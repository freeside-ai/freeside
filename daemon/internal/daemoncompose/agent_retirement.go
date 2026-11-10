package daemoncompose

import (
	"context"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// LegacyOpenTasks groups the open tasks by the flag-era identity that owns
// them. A task is open until a task-level decision (completed, stopped,
// abandoned) closes it. Its owner is read from its newest admission: a
// legacy one (an identity and no agent binding) names the owner, and an
// agent-bound one means the task already runs through the lineup and has no
// flag-era owner. Ownership is read from admissions alone: a review record's
// admission snapshot names only an identity the lineup selected, never a
// flag-era one, and a legacy review record names none.
func LegacyOpenTasks(ctx context.Context, tx *store.ReadTx) (map[domain.AuthIdentityID][]domain.Task, error) {
	tasks, err := tx.ListTasks(ctx)
	if err != nil {
		return nil, err
	}
	owned := map[domain.AuthIdentityID][]domain.Task{}
	for _, task := range tasks {
		if task.Value.DisplayLifecycle(nil) != nil {
			continue
		}
		// Runs come back in task order and admissions in recorded order, so
		// the last admission of the last admitted run is the newest.
		runs, err := tx.TaskRunIDs(ctx, task.Value.ID)
		if err != nil {
			return nil, err
		}
		for _, run := range slices.Backward(runs) {
			admissions, err := tx.ListRunExecutionAdmissionRecords(ctx, run)
			if err != nil {
				return nil, err
			}
			if len(admissions) == 0 {
				continue
			}
			newest := admissions[len(admissions)-1]
			if newest.AgentBinding == nil && newest.AuthIdentityID != nil {
				owned[*newest.AuthIdentityID] = append(owned[*newest.AuthIdentityID], task.Value)
			}
			break
		}
	}
	return owned, nil
}
