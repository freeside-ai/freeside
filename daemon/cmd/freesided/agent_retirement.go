package main

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// agentRetirementDevice is the device a retirement Stop is recorded under.
// No paired client commanded it, so it names the command.
const agentRetirementDevice = domain.DeviceID("auth-adopt-retirement")

// legacyOpenTasks groups the open tasks by the flag-era identity that owns
// them. A task is open until a task-level decision (completed, stopped,
// abandoned) closes it. Its owner is read from its newest admission: a
// legacy one (an identity and no agent binding) names the owner, and an
// agent-bound one means the task already runs through the lineup and has no
// flag-era owner. Review records carry no identity until #898, so ownership
// is read from admissions alone.
func legacyOpenTasks(ctx context.Context, tx *store.ReadTx) (map[domain.AuthIdentityID][]domain.Task, error) {
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

// stopRetiredTasks records a Stop (plan §5.7) for each open task the retired
// identity owns and returns the tasks it found. It runs under auth adopt,
// with no daemon on the store, so nothing launches beside it; the daemon's
// cancellation loop tears the work down and confirms it after its next
// start. A task whose current episode already has a cancellation is left
// alone, and the command id is derived from the identity, the task, and the
// episode, so a rerun records no second Stop.
func stopRetiredTasks(
	ctx context.Context, st *store.Store, identity domain.AuthIdentityID, now time.Time,
) ([]domain.TaskID, error) {
	var tasks []domain.Task
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		owned, err := legacyOpenTasks(ctx, tx)
		tasks = owned[identity]
		return err
	}); err != nil {
		return nil, err
	}
	ids := make([]domain.TaskID, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, task.ID)
		episode := 0
		if start := task.CurrentStart(); start != nil {
			episode = start.Ordinal
		}
		if task.Cancellation != nil && task.Cancellation.Target.EpisodeOrdinal == episode {
			continue
		}
		commandID := "agent-retirement-" + contentaddr.Sum(fmt.Appendf(nil, "%s\n%s\n%d", identity, task.ID, episode))
		if err := st.Write(ctx, func(tx *store.WriteTx) error {
			state, err := tx.ServerState(ctx)
			if err != nil {
				return err
			}
			_, _, err = tx.StopTask(ctx, domain.StopTaskRequest{
				CommandID: commandID, DeviceID: agentRetirementDevice,
				TaskID: task.ID, ProjectID: task.ProjectID,
				ExpectedSyncEpoch: state.SyncEpoch, ExpectedEntityVersion: state.Revision,
			}, now)
			return err
		}); err != nil {
			return ids, fmt.Errorf("stop task %s: %w", task.ID, err)
		}
	}
	return ids, nil
}
