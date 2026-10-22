package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// unroutedFindingsNote tells the operator where a superseded dispute's
// findings went: nowhere. Filing them needs an adjudication entry, which an
// unrouted finding does not have, so the closed card and the task record are
// where they stay visible. "Any" keeps it true for a discuss-only dispute
// that lists none.
const unroutedFindingsNote = " Any review findings it lists were never routed; they stay on this card and in the task record."

// completedEpisode returns the task's current start and the completion that
// finished it, or nils while the current episode is unfinished. It mirrors
// the TaskFinished branch of domain.Task.DisplayLifecycle; it stays here
// rather than moving into domain because a domain change would make this fix
// a contract unit.
func completedEpisode(task domain.Task) (start, completion *domain.TaskLifecycleFact) {
	start = task.CurrentStart()
	if start == nil {
		return nil, nil
	}
	for i := range task.LifecycleFacts {
		fact := task.LifecycleFacts[i]
		if fact.Ordinal > start.Ordinal && fact.Kind == domain.TaskLifecycleCompleted &&
			sameCampaignID(fact.CampaignID, start.CampaignID) &&
			(fact.EpisodeOrdinal == 0 || fact.EpisodeOrdinal == start.Ordinal) {
			return start, &fact
		}
	}
	return nil, nil
}

// sameCampaignID copies domain's unexported sameCampaign for the same reason.
func sameCampaignID(a, b *domain.CampaignID) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// completionSupersedes reports whether a task's completion retires an open
// item of this type. The ready item's owner, the merge observer, resolves it
// itself, and a proposal asks for an effect completion does not perform, so
// it stays decidable.
func completionSupersedes(kind domain.AttentionType) bool {
	switch kind {
	case domain.AttentionReadyForFinalReview, domain.AttentionTaskProposal, domain.AttentionEffectProposal:
		return false
	case domain.AttentionSpecApproval, domain.AttentionExecutionFailure, domain.AttentionAgentQuestion,
		domain.AttentionReviewDiminishing, domain.AttentionReviewDispute, domain.AttentionReviewContradiction,
		domain.AttentionReviewConfiguration, domain.AttentionFindingAdjudication, domain.AttentionPublishBlocked,
		domain.AttentionSystemHealth, domain.AttentionBlocked:
		return true
	}
	return false
}

// supersededByCompletion is every type completionSupersedes accepts.
var supersededByCompletion = func() []domain.AttentionType {
	var kinds []domain.AttentionType
	for _, kind := range domain.AllAttentionTypes {
		if completionSupersedes(kind) {
			kinds = append(kinds, kind)
		}
	}
	return kinds
}()

// RetireCompletedTaskItems supersedes the open items owned by the task's
// completed current episode, stamped at the completion's recorded time. It
// retires nothing while the episode is unfinished, and it never touches an
// earlier episode's items. Supersession is a daemon close, not an operator
// decision, so decided_at stays unset.
func RetireCompletedTaskItems(ctx context.Context, tx *store.WriteTx, taskID domain.TaskID) ([]domain.ItemID, error) {
	task, err := tx.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	start, completion := completedEpisode(task)
	if completion == nil {
		return []domain.ItemID{}, nil
	}
	open, err := tx.ListOpenAttentionItemRecordsForTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	var owned []domain.AttentionItem
	for _, item := range open {
		if !completionSupersedes(item.Type) {
			continue
		}
		owner, episode, err := tx.AttentionItemWorkflowEpisode(ctx, item)
		if err != nil {
			return nil, err
		}
		if owner == taskID && episode == start.Ordinal {
			owned = append(owned, item)
		}
	}
	if len(owned) == 0 {
		return []domain.ItemID{}, nil
	}
	unit, err := tx.GetWorkUnitCompletion(ctx, *completion.BindingUnitID)
	if err != nil {
		return nil, err
	}
	reason := fmt.Sprintf("Superseded when the task completed: pull request #%d was merged.", unit.PRNumber)
	retired := []domain.ItemID{}
	for _, item := range owned {
		itemReason := reason
		if item.Type == domain.AttentionReviewDispute {
			itemReason += unroutedFindingsNote
		}
		item.ItemVersion++
		superseded, err := item.ConcludeByDaemon(domain.ConcludedBySupersession, completion.RecordedAt.UTC(), itemReason, nil)
		if err != nil {
			return nil, fmt.Errorf("supersede completed-task item %q: %w", item.ID, err)
		}
		if err := tx.PutAttentionItem(ctx, superseded); err != nil {
			return nil, fmt.Errorf("supersede completed-task item %q: %w", item.ID, err)
		}
		retired = append(retired, item.ID)
	}
	sort.Slice(retired, func(i, j int) bool { return retired[i] < retired[j] })
	return retired, nil
}

// reconcileCompletedTaskItems retires completed tasks' items on every pass,
// so it converges after a crash and also reaches tasks that completed before
// the sweep existed. The engine reconciles many times a second, so candidates
// are chosen in one read and only a completed task with a supersedable open
// item costs a write. A task whose items or completion fail their gates
// stays visible and is logged; the pass continues with the other tasks.
func (e *Engine) reconcileCompletedTaskItems(ctx context.Context) error {
	var candidates []domain.TaskID
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		taskIDs, err := tx.ListOpenAttentionItemTaskIDs(ctx, supersededByCompletion)
		if err != nil {
			return err
		}
		for _, taskID := range taskIDs {
			task, err := tx.GetTask(ctx, taskID)
			if err != nil {
				e.logger.Warn("completed task item cleanup remains incomplete", "task", taskID, "error", err)
				continue
			}
			if _, completion := completedEpisode(task); completion != nil {
				candidates = append(candidates, taskID)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	for _, taskID := range candidates {
		err := e.store.Write(ctx, func(tx *store.WriteTx) error {
			items, err := RetireCompletedTaskItems(ctx, tx, taskID)
			if err != nil {
				return err
			}
			if len(items) == 0 {
				return errReplay
			}
			return nil
		})
		if err != nil && !errors.Is(err, errReplay) {
			e.logger.Warn("completed task item cleanup remains incomplete", "task", taskID, "error", err)
		}
	}
	return nil
}
