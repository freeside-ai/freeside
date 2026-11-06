package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// stoppedTaskItemReason closes a card that a confirmed Stop made moot.
const stoppedTaskItemReason = "Expired when the task's Stop was confirmed: this execution was canceled by that Stop."

// errStopItemRetirement marks a failure to retire a stopped task's cards
// inside the confirming write, so the confirmation can commit without them.
var errStopItemRetirement = errors.New("retire stopped task items")

// stopCanceledCandidate reports whether item could exist only because the
// Stop c canceled its invocation: an execution-failure card whose facts say
// canceled, on one of the runs the Stop targeted. It decides from the card
// alone; canceledByStop finishes the rule from the store's answers. A card
// without execution-failure facts (an interpretation failure or a
// quarantine notice) is never a candidate.
func stopCanceledCandidate(c domain.TaskCancellation, item domain.AttentionItem) bool {
	facts := item.ExecutionFailure
	if item.Type != domain.AttentionExecutionFailure || facts == nil ||
		facts.Outcome != domain.ExecutionOutcomeCanceled || item.Subject.RunID == nil {
		return false
	}
	return slices.ContainsFunc(c.Target.Runs, func(r domain.TaskCancellationRun) bool {
		return r.RunID == *item.Subject.RunID
	})
}

// canceledByStop reports whether a candidate card belongs to the Stop's own
// task and episode and its invocation's recorded outcome is a cancellation
// recorded no earlier than the Stop was requested. RecordedAt is when the
// daemon recorded the outcome, normally when it collected the result, so a
// cancellation collected after the Stop was requested counts as the Stop's
// even if the driver ended it earlier.
func canceledByStop(c domain.TaskCancellation, owner domain.TaskID, episode int, outcome domain.ExecutionOutcome) bool {
	return owner == c.Target.TaskID && episode == c.Target.EpisodeOrdinal &&
		outcome.Status == domain.ExecutionOutcomeCanceled && !outcome.RecordedAt.Before(c.RequestedAt)
}

// stoppedTaskItems selects, in ID order, the open cards the task's confirmed
// Stop retires, with that Stop. It selects nothing unless the Stop is
// confirmed, and it writes nothing.
func stoppedTaskItems(ctx context.Context, tx *store.ReadTx, taskID domain.TaskID) (*domain.TaskCancellation, []domain.AttentionItem, error) {
	task, err := tx.GetTask(ctx, taskID)
	if err != nil {
		return nil, nil, err
	}
	c := task.Cancellation
	if c == nil || c.State != domain.TaskCancellationConfirmed {
		return c, nil, nil
	}
	open, err := tx.ListOpenAttentionItemRecordsForTask(ctx, taskID)
	if err != nil {
		return nil, nil, err
	}
	var selected []domain.AttentionItem
	for _, item := range open {
		retires, err := retiredByStop(ctx, tx, *c, item)
		if err != nil {
			return nil, nil, err
		}
		if retires {
			selected = append(selected, item)
		}
	}
	return c, selected, nil
}

// retiredByStop applies the whole retirement rule to one open card of the
// task whose confirmed Stop is c: the card-only half first, so a card that
// cannot qualify costs no store read.
func retiredByStop(ctx context.Context, tx *store.ReadTx, c domain.TaskCancellation, item domain.AttentionItem) (bool, error) {
	if !stopCanceledCandidate(c, item) {
		return false, nil
	}
	owner, episode, err := tx.AttentionItemWorkflowEpisode(ctx, item)
	if err != nil {
		return false, err
	}
	outcome, err := tx.GetExecutionOutcomeRecord(ctx, item.ExecutionFailure.InvocationID)
	if err != nil {
		return false, err
	}
	return canceledByStop(c, owner, episode, outcome), nil
}

// RetireStoppedTaskItems expires, and returns in ID order, the open
// execution-failure cards that exist only because the task's confirmed Stop
// canceled their invocations, so the Inbox stops asking the operator to
// recover work they deliberately ended. It retires nothing unless the Stop is
// confirmed; stopCanceledCandidate and canceledByStop state the rest of the
// rule. Expiry is a daemon close, not an operator decision, so decided_at
// stays unset; the terminal result and execution outcome stay as history.
func RetireStoppedTaskItems(ctx context.Context, tx *store.WriteTx, taskID domain.TaskID) ([]domain.ItemID, error) {
	c, items, err := stoppedTaskItems(ctx, &tx.ReadTx, taskID)
	if err != nil {
		return nil, err
	}
	retired := []domain.ItemID{}
	for _, item := range items {
		// A card raised after confirmation must not close before it opened.
		at := c.Acknowledgement.RecordedAt.UTC()
		if item.CreatedAt != nil && item.CreatedAt.After(at) {
			at = item.CreatedAt.UTC()
		}
		item.ItemVersion++
		expired, err := item.ConcludeByDaemon(domain.ConcludedByExpiry, at, stoppedTaskItemReason, nil)
		if err != nil {
			return nil, fmt.Errorf("expire stopped-task item %q: %w", item.ID, err)
		}
		if err := tx.PutAttentionItem(ctx, expired); err != nil {
			return nil, fmt.Errorf("expire stopped-task item %q: %w", item.ID, err)
		}
		retired = append(retired, item.ID)
	}
	return retired, nil
}

// reconcileStoppedTaskItems retires stopped tasks' cards on every pass. The
// confirming write retires what was open then; this sweep catches a canceled
// result collected after confirmation and cards left open by daemons that
// predate the retirement.
//
// Selection runs in one read and stays cheap on every pass, because a
// stopped task may keep a genuine failure card open indefinitely. Column
// lookups and task point reads find the confirmed-stopped tasks first, so a
// pass with none reads no card. Otherwise one type-scoped listing of open
// execution-failure cards is matched against the rule in memory; the
// task-scoped read, which decodes every stored card to guard its columns,
// runs only in the write for a task with a card to retire. A task or card
// whose reads fail stays visible and is logged; the pass continues.
func (e *Engine) reconcileStoppedTaskItems(ctx context.Context) error {
	var candidates []domain.TaskID
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		candidates, err = e.stoppedTaskCandidates(ctx, tx)
		return err
	}); err != nil {
		return err
	}
	for _, taskID := range candidates {
		err := e.store.Write(ctx, func(tx *store.WriteTx) error {
			items, err := RetireStoppedTaskItems(ctx, tx, taskID)
			if err != nil {
				return err
			}
			if len(items) == 0 {
				return errReplay
			}
			return nil
		})
		if err != nil && !errors.Is(err, errReplay) {
			e.logger.Warn("stopped task item cleanup remains incomplete", "task", taskID, "error", err)
		}
	}
	return nil
}

// stoppedTaskCandidates returns, in ID order, the confirmed-stopped tasks
// with at least one open card their Stop retires. The write re-selects
// through RetireStoppedTaskItems, so this read only decides which tasks get
// one.
func (e *Engine) stoppedTaskCandidates(ctx context.Context, tx *store.ReadTx) ([]domain.TaskID, error) {
	taskIDs, err := tx.ListOpenAttentionItemTaskIDs(ctx, []domain.AttentionType{domain.AttentionExecutionFailure})
	if err != nil {
		return nil, err
	}
	stops := map[domain.TaskID]domain.TaskCancellation{}
	for _, taskID := range taskIDs {
		task, err := tx.GetTask(ctx, taskID)
		if err != nil {
			e.logger.Warn("stopped task item cleanup remains incomplete", "task", taskID, "error", err)
			continue
		}
		if c := task.Cancellation; c != nil && c.State == domain.TaskCancellationConfirmed {
			stops[taskID] = *c
		}
	}
	if len(stops) == 0 {
		return nil, nil
	}
	open, err := tx.ListOpenAttentionItems(ctx, domain.AttentionExecutionFailure)
	if err != nil {
		// One unreadable card fails the store-wide listing; leave every
		// card open and say so rather than fail the whole pass.
		e.logger.Warn("stopped task item cleanup remains incomplete", "error", err)
		return nil, nil
	}
	selected := map[domain.TaskID]bool{}
	for _, item := range open {
		if item.Subject.TaskID == nil || selected[*item.Subject.TaskID] {
			continue
		}
		taskID := *item.Subject.TaskID
		c, stopped := stops[taskID]
		if !stopped {
			continue
		}
		retires, err := retiredByStop(ctx, tx, c, item)
		if err != nil {
			e.logger.Warn("stopped task item cleanup remains incomplete", "task", taskID, "item", item.ID, "error", err)
			continue
		}
		if retires {
			selected[taskID] = true
		}
	}
	var candidates []domain.TaskID
	for _, taskID := range taskIDs {
		if selected[taskID] {
			candidates = append(candidates, taskID)
		}
	}
	return candidates, nil
}
