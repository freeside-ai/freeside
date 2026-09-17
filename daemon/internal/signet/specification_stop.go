package signet

import (
	"context"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// submitDecision orders a task-bound card Stop with runtime launch. The
// accepting transaction authenticates the command again; this lookup grants
// no authority and cannot turn a rejected decision into a cancellation.
func (s *Service) submitDecision(ctx context.Context, in ClientCommand) (CommandResult, error) {
	if in.Payload.Action != domain.ActionStop || s.taskStopGuard == nil {
		return s.submitDecisionTransaction(ctx, in)
	}
	var taskID domain.TaskID
	if err := s.store.Read(ctx, func(tx *store.ReadTx) error {
		item, err := tx.GetAttentionItemRecord(ctx, in.Payload.ItemID)
		if err != nil || item.Subject.RunID == nil {
			return err
		}
		run, err := tx.GetRun(ctx, *item.Subject.RunID)
		taskID = run.TaskID
		return err
	}); err != nil || taskID == "" {
		return s.submitDecisionTransaction(ctx, in)
	}
	var result CommandResult
	err := s.taskStopGuard(ctx, taskID, func() error {
		var err error
		result, err = s.submitDecisionTransaction(ctx, in)
		return err
	})
	return result, err
}

// applySpecificationStop shares the command's accepting transaction. A
// replay never enters this method, so a historical decision cannot stop a
// later episode or acquire authority in a new sync epoch.
func (s *Service) applySpecificationStop(ctx context.Context, tx *store.WriteTx, command domain.Command, item domain.AttentionItem) error {
	if command.Action != domain.ActionStop || item.Subject.RunID == nil ||
		(item.Type != domain.AttentionSpecApproval && item.Type != domain.AttentionExecutionFailure && item.Type != domain.AttentionAgentQuestion) {
		return nil
	}
	run, err := tx.GetRun(ctx, *item.Subject.RunID)
	if err != nil {
		return err
	}
	specification := false
	for _, stage := range run.Stages {
		specification = specification || stage.Name == "specification"
	}
	if !specification {
		return nil
	}
	task, err := tx.GetTask(ctx, run.TaskID)
	if err != nil {
		return err
	}
	start := task.CurrentStart()
	if start == nil || start.RunID != run.ID || run.ProjectID != item.ProjectID {
		return fmt.Errorf("specification Stop no longer names the current episode: %w", store.ErrCancellationBinding)
	}
	state, err := tx.ServerState(ctx)
	if err != nil {
		return err
	}
	// The original command keeps its public receipt. A distinct stable key
	// binds the internal adapter without reusing a command ID across kinds.
	derived := "specification-stop-" + contentaddr.Sum([]byte(command.CommandID))
	_, _, err = tx.StopTask(ctx, domain.StopTaskRequest{
		CommandID: derived, DeviceID: command.DeviceID, TaskID: task.ID, ProjectID: task.ProjectID,
		ExpectedSyncEpoch: state.SyncEpoch, ExpectedEntityVersion: state.Revision,
	}, s.now().UTC())
	if errors.Is(err, store.ErrImmutableConflict) {
		return fmt.Errorf("specification Stop receipt identity: %w", err)
	}
	return err
}
