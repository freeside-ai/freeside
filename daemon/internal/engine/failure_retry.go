package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// reconcileFailureRetries turns each accepted Retry on a specification or
// implementation failure card into one more attempt on the same agent
// (#3440). The new attempt records the command (a specification iteration
// in its reason, a production attempt in its operator binding), so a pass
// replayed after a crash finds it and starts nothing more. An operator
// feedback card's Retry is reconcileOperatorFeedbackRetries' to handle.
func (e *Engine) reconcileFailureRetries(ctx context.Context) (int, error) {
	var selected []domain.Command
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		selected, err = tx.ListCommandsForActions(ctx, domain.ActionRetry)
		return err
	}); err != nil {
		return 0, err
	}
	created := 0
	var joined error
	for _, command := range selected {
		if !domain.HasExecutionFailureItemPrefix(command.ItemID) ||
			domain.IsOperatorFeedbackExecutionFailureItemID(command.ItemID) {
			continue
		}
		made, err := e.reconcileFailureRetry(ctx, command)
		// A closed or canceled task starts nothing, and its replay has
		// nothing left to start.
		if errors.Is(err, store.ErrTaskWorkflowClosed) || errors.Is(err, store.ErrTaskCancellationFenced) {
			continue
		}
		if err != nil {
			joined = errors.Join(joined, fmt.Errorf("command %q: %w", command.CommandID, err))
			continue
		}
		created += boolCount(made)
	}
	return created, joined
}

func (e *Engine) reconcileFailureRetry(ctx context.Context, command domain.Command) (bool, error) {
	var item domain.AttentionItem
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		item, err = tx.GetAttentionItemRecord(ctx, command.ItemID)
		return err
	}); err != nil {
		return false, err
	}
	if !retryCommandResolved(command, item) || item.Subject.RunID == nil {
		return false, fmt.Errorf("retry command %q did not resolve failure card %q: %w",
			command.CommandID, item.ID, domain.ErrParentKeyMismatch)
	}
	runID := *item.Subject.RunID
	switch stage := item.ExecutionFailure.Stage; stage {
	case domain.StageNameSpecification:
		retry, err := retrySpecificationRun(ctx, e.store, SpecificationRetrySpec{
			SpecificationRunID: runID, Reason: operatorSpecificationRetryReason(command.CommandID),
		}, command.CommandID)
		return retry.Created, err
	case domain.StageNameImplementation:
		retry, err := ReattemptProductionRun(ctx, e.store, ProductionReattemptSpec{
			ParentRunID:         runID,
			Reason:              "operator retry " + command.CommandID,
			OperatorCommandID:   command.CommandID,
			RetryOfInvocationID: item.ExecutionFailure.InvocationID,
		})
		return retry.Created, err
	case domain.StageNameReview, domain.StageNameVerification:
		return false, fmt.Errorf("retry command %q on a %s failure: %w",
			command.CommandID, stage, domain.ErrParentKeyMismatch)
	}
	return false, fmt.Errorf("retry command %q on stage %q: %w",
		command.CommandID, item.ExecutionFailure.Stage, domain.ErrParentKeyMismatch)
}
