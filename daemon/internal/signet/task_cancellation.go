package signet

import (
	"context"
	"errors"
	"reflect"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

type StopTaskPayload struct {
	TaskID            domain.TaskID    `json:"task_id"`
	ProjectID         domain.ProjectID `json:"project_id"`
	ExpectedSyncEpoch string           `json:"expected_sync_epoch"`
}

var ErrInvalidStopTaskPayload = errors.New("invalid stop_task payload")

// StaleTaskError returns the current public task projection and epoch, so the
// client can ask for a fresh decision without retargeting a prepared Stop.
type StaleTaskError struct {
	ReplacementTask TaskSnapshot `json:"replacement_task"`
	SyncEpoch       string       `json:"sync_epoch"`
}

func (*StaleTaskError) Error() string { return "task cancellation binding changed" }

func (s *Service) stopTask(ctx context.Context, in ClientCommand) (CommandResult, error) {
	request := domain.StopTaskRequest{CommandID: in.CommandID, DeviceID: in.DeviceID, TaskID: in.StopTask.TaskID, ProjectID: in.StopTask.ProjectID, ExpectedSyncEpoch: in.StopTask.ExpectedSyncEpoch, ExpectedEntityVersion: in.ExpectedEntityVersion}
	if request.Validate() != nil || in.SubmitTask != (SubmitTaskPayload{}) || !reflect.ValueOf(in.Payload).IsZero() {
		return CommandResult{}, ErrInvalidStopTaskPayload
	}
	var result CommandResult
	err := s.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := gateActiveDevice(ctx, tx, in.DeviceID); err != nil {
			return err
		}
		if old, snap, err := tx.GetStopTaskReceipt(ctx, in.CommandID); err == nil {
			if old.StopTaskRequest != request {
				return store.ErrImmutableConflict
			}
			result = CommandResult{Stop: &old, Revision: snap.AsOfRevision}
			return errReplay
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		task, err := tx.GetTask(ctx, request.TaskID)
		if err != nil {
			return err
		}
		if task.ProjectID != request.ProjectID {
			return store.ErrNotFound
		}
		receipt, snap, err := tx.StopTask(ctx, request, s.now().UTC())
		if errors.Is(err, store.ErrCancellationBinding) {
			state, err := tx.ServerState(ctx)
			if err != nil {
				return err
			}
			snapshot, err := currentTaskSnapshot(ctx, &tx.ReadTx, state, task)
			if err != nil {
				return err
			}
			return &StaleTaskError{ReplacementTask: snapshot, SyncEpoch: state.SyncEpoch}
		}
		if err != nil {
			return err
		}
		result = CommandResult{Stop: &receipt, Revision: snap.AsOfRevision}
		return nil
	})
	if err != nil && !errors.Is(err, errReplay) {
		return CommandResult{}, err
	}
	return result, nil
}

func currentTaskSnapshot(ctx context.Context, tx *store.ReadTx, state store.ServerState, task domain.Task) (TaskSnapshot, error) {
	ids, err := tx.TaskRunIDs(ctx, task.ID)
	if err != nil {
		return TaskSnapshot{}, err
	}
	items, err := tx.ListAttentionItems(ctx)
	if err != nil {
		return TaskSnapshot{}, err
	}
	runs := make(map[domain.RunID]Run, len(ids))
	for _, id := range ids {
		run, err := tx.GetRunSnapshot(ctx, id)
		if err != nil {
			return TaskSnapshot{}, err
		}
		projected, err := projectRunSnapshot(ctx, tx, state, run.Value, run.Snapshot, items)
		if err != nil {
			return TaskSnapshot{}, err
		}
		runs[id] = projected.Run
	}
	return projectTaskSnapshot(ctx, tx, state, task, runs)
}
