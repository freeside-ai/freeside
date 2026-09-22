package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// Under the source-issue-closure human gate the publisher holds a closable
// source's pull request as a draft until the effect_proposal item is decided.
// The decision lands through signet, which never touches the forge, so the
// engine must revisit the published pull request afterwards to write Closes or
// Refs and mark it ready (issue #1419 Part E). No existing pass does: the task's
// outbox row is retired once the pull request is published, and the
// active-resource reconciler only observes.
//
// The task row is still retired on publish, so operator feedback, publication
// continuations, and run-conclusion projections, which all require a
// dispatched row, behave as for any published run. Beside it the engine keeps a
// closure-wait row naming the task. While the closure item and the ready item
// are both open, a pass costs two store reads per wait. Once the decision lands
// (the item leaves the open state) or the ready item concludes (the pull request
// merged, closed, or was invalidated), the pass re-lists the retired task row,
// which re-enters reconcileTask through the published-recovery path. That path
// reconverges the pull request from current store state, and
// completePublishedTask retires the wait.
//
// Only a production_publication_requested row (a run's first publication or a
// successor) arms a wait. A reevaluation or continuation intent authenticates
// against state that holds only while it is pending, so it cannot be re-entered
// once retired.

// KindProductionClosureWait is the engine-private outbox kind for a published
// task whose pull request waits on a human-gate closure decision.
const KindProductionClosureWait = "production_publication_closure_wait"

const (
	productionClosureWaitVersion   = "freeside.production-closure-wait/v1"
	productionClosureWaitKeyPrefix = "production-closure-wait/"
)

type productionClosureWait struct {
	Version     string                    `json:"version"`
	TaskKey     string                    `json:"task_key"`
	ReadyItemID domain.ItemID             `json:"ready_item_id"`
	InstanceID  domain.ProposalInstanceID `json:"instance_id"`
}

func productionClosureWaitKey(taskKey string) string {
	return productionClosureWaitKeyPrefix + taskKey
}

// decodeProductionClosureWait authenticates a wait row's shape against its key.
// The row is engine-written but crosses the reconstruction boundary, so a
// mismatch fails closed. Its fields only choose when to re-enter a task; the
// re-entered task re-derives every authority from its own row and the store.
func decodeProductionClosureWait(entry store.QueueEntry) (productionClosureWait, error) {
	var wait productionClosureWait
	if entry.Kind != KindProductionClosureWait {
		return wait, domain.ErrParentKeyMismatch
	}
	if err := json.Unmarshal(entry.Payload, &wait); err != nil {
		return wait, err
	}
	if wait.Version != productionClosureWaitVersion || wait.TaskKey == "" ||
		wait.ReadyItemID == "" || wait.InstanceID == "" ||
		entry.IdempotencyKey != productionClosureWaitKey(wait.TaskKey) {
		return wait, fmt.Errorf("closure wait %q: %w", entry.IdempotencyKey, domain.ErrParentKeyMismatch)
	}
	return wait, nil
}

// ProductionClosureWaitBackupPayloadDigests validates a closure-wait row for
// backup. The row names a task and two store ids and references no artifact.
func ProductionClosureWaitBackupPayloadDigests(entry store.QueueEntry) ([]domain.Digest, error) {
	if _, err := decodeProductionClosureWait(entry); err != nil {
		return nil, err
	}
	return nil, nil
}

// closureHoldInstance reports the closure instance whose open decision still
// holds this published task's pull request as a draft. Only an authored recipe
// under the human gate with an admitted closure proposal can hold. The instance
// id is read from a decoded checkpoint; it only chooses whether to wait, never
// what the publisher writes, which re-derives the reference from the store.
func (w *productionPublicationWorkflow) closureHoldInstance(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
) (domain.ProposalInstanceID, bool, error) {
	if !authoredPublicationRecipe(task.Publication.Recipe) ||
		!parseSourceIssueClosureHumanGate(binding.resolvedPolicy) {
		return "", false, nil
	}
	key := productionClosureCheckpointKey(task.RunID, task.PublicationID)
	want := productionClosureCheckpoint{
		Version: productionClosureCheckpointVersion, RunID: task.RunID, PublicationID: task.PublicationID,
	}
	closure, found, err := w.loadClosureCheckpoint(ctx, key, want)
	if err != nil || !found || !closure.HasProposal {
		return "", false, err
	}
	var state closureWaitState
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		state, err = readClosureWaitState(ctx, tx, task.readyItemID(), closure.InstanceID)
		return err
	}); err != nil || state != closureWaitHeld {
		return "", false, err
	}
	return closure.InstanceID, true, nil
}

// closureWaitState is where a published task's closure wait stands.
type closureWaitState int

const (
	// closureWaitHeld: the ready item and the closure item are both open.
	closureWaitHeld closureWaitState = iota + 1
	// closureWaitDecided: the closure item left the open state while the pull
	// request is still live, so the task re-enters to release the hold.
	closureWaitDecided
	// closureWaitConcluded: the ready item concluded (the pull request merged,
	// closed, or was invalidated), so there is no hold left to release and the
	// wait retires without re-entering the task.
	closureWaitConcluded
)

func readClosureWaitState(
	ctx context.Context, tx *store.ReadTx, readyItemID domain.ItemID, instance domain.ProposalInstanceID,
) (closureWaitState, error) {
	ready, err := tx.GetAttentionItemRecord(ctx, readyItemID)
	if errors.Is(err, store.ErrNotFound) {
		return closureWaitConcluded, nil
	}
	if err != nil {
		return 0, err
	}
	if ready.Status != domain.StatusOpen {
		return closureWaitConcluded, nil
	}
	_, _, open, err := tx.OpenEffectItemForInstance(ctx, instance)
	if err != nil {
		return 0, err
	}
	if open {
		return closureWaitHeld, nil
	}
	return closureWaitDecided, nil
}

// finishPublishedTask retires the published task row and, in the same
// transaction, arms or retires its closure wait: a held task records the wait,
// and a task no longer held (or never held) retires any wait it had, which is a
// no-op when there is none. One transaction keeps a pending task row and an
// armed wait from ever coexisting. With two writes, a crash between them would
// leave the task pending beside its wait; a decision landing later would then
// reach the task as an ordinary pending row, which the reconcile pass's
// fail-once rule for released waits never retires, so a pull request merged in
// the meantime would fail the task on every pass.
func (w *productionPublicationWorkflow) finishPublishedTask(
	ctx context.Context, task productionPublicationTask, binding productionBinding,
) error {
	taskKey := task.intentKey()
	instance, held, err := w.closureHoldInstance(ctx, task, binding)
	if err != nil {
		return err
	}
	if !held || task.reevaluation != nil || task.continuation != nil {
		return w.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
			if err := tx.MarkOutboxDispatched(ctx, productionClosureWaitKey(taskKey)); err != nil {
				return err
			}
			return tx.MarkOutboxDispatched(ctx, taskKey)
		})
	}
	payload, err := json.Marshal(productionClosureWait{
		Version: productionClosureWaitVersion, TaskKey: taskKey,
		ReadyItemID: task.readyItemID(), InstanceID: instance,
	})
	if err != nil {
		return err
	}
	return w.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		entry, _, err := tx.EnqueueOutbox(ctx, productionClosureWaitKey(taskKey), KindProductionClosureWait, payload)
		if err != nil {
			return err
		}
		if entry.Kind != KindProductionClosureWait || !bytes.Equal(entry.Payload, payload) {
			return fmt.Errorf("closure wait %q disagrees with stored row: %w",
				entry.IdempotencyKey, domain.ErrImmutableTransition)
		}
		return tx.MarkOutboxDispatched(ctx, taskKey)
	})
}

// retireClosureWaits marks the named tasks' waits dispatched. Marking a missing
// or already-dispatched wait is a no-op.
func (w *productionPublicationWorkflow) retireClosureWaits(ctx context.Context, taskKeys []string) error {
	if len(taskKeys) == 0 {
		return nil
	}
	return w.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		for _, taskKey := range taskKeys {
			if err := tx.MarkOutboxDispatched(ctx, productionClosureWaitKey(taskKey)); err != nil {
				return err
			}
		}
		return nil
	})
}

// releasedClosureWaitTasks sorts the pending closure waits for one reconcile
// pass. It returns the retired task rows whose decision has landed, for the
// pass to re-enter beside its pending rows (a row already pending is not listed
// twice), and the task keys whose ready item concluded, whose waits retire
// without re-entry. A held wait is skipped.
//
// A wait row this daemon cannot use (an unreadable or newer-version payload, or
// one naming a missing or foreign task row) is skipped and left pending for a
// daemon that can read it, never an error: one bad row must not stop the
// publication lane, and a skipped wait only leaves its pull request a draft,
// which withholds Closes and merging rather than granting them. Only a store
// read failure propagates.
func releasedClosureWaitTasks(
	ctx context.Context, tx *store.ReadTx, pending []store.QueueEntry,
) (released []store.QueueEntry, concluded []string, err error) {
	waits, err := tx.ListPendingOutbox(ctx, KindProductionClosureWait)
	if err != nil {
		return nil, nil, err
	}
	listed := make(map[string]bool, len(pending))
	for _, entry := range pending {
		listed[entry.IdempotencyKey] = true
	}
	for _, entry := range waits {
		wait, err := decodeProductionClosureWait(entry)
		if err != nil {
			continue
		}
		state, err := readClosureWaitState(ctx, tx, wait.ReadyItemID, wait.InstanceID)
		if err != nil {
			return nil, nil, err
		}
		switch state {
		case closureWaitHeld:
			continue
		case closureWaitConcluded:
			concluded = append(concluded, wait.TaskKey)
			continue
		case closureWaitDecided:
		}
		if listed[wait.TaskKey] {
			continue
		}
		task, err := tx.GetOutbox(ctx, wait.TaskKey)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		if task.Kind != KindProductionPublicationRequested {
			continue
		}
		listed[wait.TaskKey] = true
		released = append(released, task)
	}
	return released, concluded, nil
}
