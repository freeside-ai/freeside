package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// RetireAbandonedTaskItems expires open approval and execution-recovery cards
// for the current abandoned episode. Other kinds still require decisions in
// their workflow readers, and abandonment does not stop those workflows.
// Call this after committing the slot release so a stale item's policy failure
// cannot undo abandonment. A later explicit admission makes this a no-op.
func RetireAbandonedTaskItems(ctx context.Context, tx *store.WriteTx, taskID domain.TaskID) ([]domain.ItemID, error) {
	retired := []domain.ItemID{}
	task, err := tx.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if lifecycle := task.DisplayLifecycle(nil); lifecycle == nil || *lifecycle != domain.TaskAbandoned {
		return retired, nil
	}
	items, err := tx.ListOpenAttentionItemRecordsForTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if item.Type != domain.AttentionSpecApproval &&
			(item.Type != domain.AttentionExecutionFailure ||
				!strings.HasPrefix(string(item.ID), productionExecutionFailureCardPrefix)) {
			continue
		}
		item.Status = domain.StatusExpired
		item.ItemVersion++
		item.Reason += " The task was abandoned; no decision is needed."
		if err := tx.PutAttentionItem(ctx, item); err != nil {
			return nil, fmt.Errorf("expire abandoned-task item %q: %w", item.ID, err)
		}
		retired = append(retired, item.ID)
	}
	return retired, nil
}
