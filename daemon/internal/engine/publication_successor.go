package engine

import (
	"context"
	"errors"
	"reflect"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func preparePublicationSuccessor(
	ctx context.Context, tx *store.ReadTx, feedback domain.InvocationID,
) (*domain.PublicationSuccessor, error) {
	returned, ready, found, err := tx.FeedbackPublicationParent(ctx, feedback)
	if err != nil || !found {
		return nil, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	successor := domain.PublicationSuccessor{
		Version: domain.PublicationSuccessorVersion, RunID: returned.RunID,
		CommandID: returned.CommandID, FeedbackInvocationID: feedback,
		PredecessorItemID: ready.ItemID,
	}
	existing, err := tx.GetPublicationSuccessor(ctx, successor.RunID, successor.PublicationID())
	if err == nil {
		if existing.RunID != successor.RunID || existing.CommandID != successor.CommandID ||
			existing.FeedbackInvocationID != feedback || existing.PredecessorItemID != ready.ItemID {
			return nil, domain.ErrParentKeyMismatch
		}
		return &existing, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	current, err := tx.CurrentProductionReadyItemID(ctx, returned.RunID)
	if err != nil || current != ready.ItemID {
		return nil, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	prior, err := tx.LatestReviewRecord(ctx, returned.RunID)
	if err != nil || prior.HeadSHA != ready.HeadSHA {
		return nil, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	successor.PriorReviewInvocationID = prior.InvocationID
	successor.ReviewRound = prior.Round + 1
	return &successor, successor.Validate()
}

func currentProductionTaskEntry(ctx context.Context, tx *store.ReadTx, runID domain.RunID) (store.QueueEntry, error) {
	successor, err := tx.CurrentPublicationSuccessor(ctx, runID)
	if err != nil {
		return store.QueueEntry{}, err
	}
	if successor != nil {
		return tx.GetOutbox(ctx, successor.TaskKey())
	}
	return tx.GetOutbox(ctx, productionPublicationTaskKey(runID))
}

func productionTaskForInvocation(ctx context.Context, tx *store.ReadTx, runID domain.RunID, invocation domain.InvocationID) (store.QueueEntry, error) {
	var publication domain.InvocationID
	if _, ok := remediationRoundForInvocation(runID, invocation); ok {
		entry, err := tx.GetOutbox(ctx, string(invocation))
		if err != nil {
			return store.QueueEntry{}, err
		}
		request, err := decodeRemediationRequest(entry)
		if err != nil {
			return store.QueueEntry{}, err
		}
		publication = request.SuccessorPublicationID
	} else if invocation != productionInvocationID(runID) {
		returned, _, found, err := tx.FeedbackPublicationParent(ctx, invocation)
		if err != nil {
			return store.QueueEntry{}, err
		}
		if found {
			publication = (domain.PublicationSuccessor{CommandID: returned.CommandID}).PublicationID()
		}
	}
	if publication == "" {
		return tx.GetOutbox(ctx, productionPublicationTaskKey(runID))
	}
	successor, err := tx.GetPublicationSuccessor(ctx, runID, publication)
	if err != nil {
		return store.QueueEntry{}, err
	}
	return tx.GetOutbox(ctx, successor.TaskKey())
}

func productionTaskForReadyItem(ctx context.Context, tx *store.ReadTx, runID domain.RunID, itemID domain.ItemID) (store.QueueEntry, error) {
	if itemID == productionReadyItemID(runID) {
		return tx.GetOutbox(ctx, productionPublicationTaskKey(runID))
	}
	ready, err := tx.GetReadyItemPRBinding(ctx, itemID)
	if err != nil || ready.RunID != runID {
		return store.QueueEntry{}, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	successor, err := tx.GetPublicationSuccessor(ctx, runID, ready.PublicationInvocationID)
	if err != nil || successor.ReadyItemID() != itemID {
		return store.QueueEntry{}, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	return tx.GetOutbox(ctx, successor.TaskKey())
}

func productionTaskForBlockedItem(ctx context.Context, tx *store.ReadTx, runID domain.RunID, itemID domain.ItemID) (store.QueueEntry, error) {
	seen := make(map[domain.ItemID]bool)
	for {
		if seen[itemID] {
			return store.QueueEntry{}, domain.ErrParentKeyMismatch
		}
		seen[itemID] = true
		if itemID == domain.ProductionBlockedItemID(runID) {
			return tx.GetOutbox(ctx, productionPublicationTaskKey(runID))
		}
		successor, err := tx.PublicationSuccessorForBlockedItem(ctx, runID, itemID)
		if err != nil {
			return store.QueueEntry{}, err
		}
		if successor != nil {
			return tx.GetOutbox(ctx, successor.TaskKey())
		}
		carrierRun, commandID, ok := signet.ReevaluatedBlockedItemCoordinates(itemID)
		if !ok || carrierRun != runID {
			return store.QueueEntry{}, domain.ErrParentKeyMismatch
		}
		command, err := tx.GetCommand(ctx, commandID)
		if err != nil {
			return store.QueueEntry{}, err
		}
		if command.Action != domain.ActionRerunTrustEvaluation {
			return store.QueueEntry{}, domain.ErrParentKeyMismatch
		}
		itemID = command.ItemID
	}
}

func authenticateTaskSuccessor(ctx context.Context, tx *store.ReadTx, task productionPublicationTask) error {
	if task.Successor == nil {
		return nil
	}
	successor, err := tx.GetPublicationSuccessor(ctx, task.RunID, task.PublicationID)
	if err != nil || !reflect.DeepEqual(successor, *task.Successor) {
		return errors.Join(err, domain.ErrParentKeyMismatch)
	}
	if task.ProducingInvocationID == successor.FeedbackInvocationID {
		entry, err := tx.GetOutbox(ctx, string(task.ProducingInvocationID))
		if err != nil {
			return err
		}
		_, publication, err := authenticateOperatorFeedbackTransition(
			ctx, tx, entry, task.RunID, operatorFeedbackStageID(task.ProducingInvocationID))
		if err != nil || !reflect.DeepEqual(publication, task.Publication) {
			return errors.Join(err, domain.ErrParentKeyMismatch)
		}
	} else {
		entry, err := tx.GetOutbox(ctx, string(task.ProducingInvocationID))
		if err != nil {
			return err
		}
		request, err := decodeRemediationRequest(entry)
		if err != nil || request.SuccessorPublicationID != task.PublicationID {
			return errors.Join(err, domain.ErrParentKeyMismatch)
		}
	}
	return nil
}

func (w *productionPublicationWorkflow) bindSuccessorCandidate(ctx context.Context, task productionPublicationTask, candidate *publish.Candidate) error {
	if task.Successor == nil {
		return nil
	}
	return w.store.Read(ctx, func(tx *store.ReadTx) error {
		target, err := tx.PublicationSuccessorTarget(ctx, task.RunID, task.PublicationID)
		if err != nil {
			return err
		}
		candidate.Successor = &target
		candidate.Branch = target.Branch
		return nil
	})
}

func (w *productionPublicationWorkflow) publishCandidate(ctx context.Context, task productionPublicationTask, candidate publish.Candidate, checkout PublicationCheckout) (publish.Result, error) {
	execution := publish.ExecutionCandidate{Candidate: candidate, ProducingInvocationID: task.ProducingInvocationID}
	if task.Successor != nil {
		transport, ok := w.transport.(interface {
			UpdateHead(context.Context, PublicationCheckout, publish.GatedUpdate) (publish.PushResult, error)
		})
		if !ok {
			return publish.Result{}, errors.New("publication transport does not support authenticated successor updates")
		}
		return w.publisher.PublishSuccessorAfterGateAndFinalize(ctx, execution, w.approvedRecipes,
			func(ctx context.Context, gated publish.GatedUpdate) error {
				_, err := transport.UpdateHead(ctx, checkout, gated)
				if err != nil && productionPublicationRetryableFailure(err) {
					return productionPublicationRetryableError(err)
				}
				return err
			})
	}
	return w.publisher.PublishExecutionAfterGateAndFinalize(ctx, execution, w.approvedRecipes,
		func(ctx context.Context, gated publish.GatedHead) error {
			_, err := w.transport.PushHead(ctx, checkout, gated)
			if err != nil && productionPublicationRetryableFailure(err) {
				return productionPublicationRetryableError(err)
			}
			return err
		})
}

func PublicationSuccessorBackupPayloadDigests(entry store.QueueEntry) ([]domain.Digest, error) {
	successor, err := domain.DecodePublicationSuccessor(entry.Payload)
	if err != nil || entry.Kind != domain.PublicationSuccessorKind || entry.IdempotencyKey != successor.Key() {
		return nil, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	return nil, nil
}
