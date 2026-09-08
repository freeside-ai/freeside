package engine

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func (e *Engine) reconcileOperatorFeedbackRetries(ctx context.Context) (int, error) {
	var items []domain.AttentionItem
	var commands []domain.Command
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		runs, err := tx.ListRuns(ctx)
		if err != nil {
			return err
		}
		for _, run := range runs {
			records, err := tx.ListOpenAttentionItemRecordsForRun(ctx, run.Value.ID)
			if err != nil {
				return err
			}
			items = append(items, records...)
		}
		commands, err = tx.ListCommandsForActions(ctx, domain.ActionRetry)
		return err
	}); err != nil {
		return 0, err
	}
	for _, item := range items {
		if item.Status != domain.StatusOpen || item.Type != domain.AttentionExecutionFailure ||
			item.ExecutionFailure == nil || item.Offers(domain.ActionRetry) ||
			!strings.HasPrefix(string(item.ExecutionFailure.InvocationID), "inv-operator-feedback-") {
			continue
		}
		if err := e.offerOperatorFeedbackRetry(ctx, item.ID); err != nil {
			return 0, err
		}
	}
	created := 0
	for _, command := range commands {
		if !strings.HasPrefix(string(command.ItemID), "execution-failure-inv-operator-feedback-") {
			continue
		}
		made, err := e.enqueueOperatorFeedbackRetry(ctx, command)
		if err != nil {
			return created, err
		}
		created += boolCount(made)
	}
	return created, nil
}

func (e *Engine) offerOperatorFeedbackRetry(ctx context.Context, id domain.ItemID) error {
	return e.store.Write(ctx, func(tx *store.WriteTx) error {
		item, err := tx.GetAttentionItem(ctx, id)
		if err != nil {
			return err
		}
		if item.Status != domain.StatusOpen || item.Offers(domain.ActionRetry) {
			return nil
		}
		if _, err := tx.OperatorFeedbackRetryParent(ctx, item); err != nil {
			// A pre-publication question or a damaged lineage is not an offer.
			// Dispatch still diagnoses any invalid retained execution markers.
			if errors.Is(err, store.ErrNotFound) || errors.Is(err, domain.ErrParentKeyMismatch) || errors.Is(err, domain.ErrImmutableTransition) {
				return nil
			}
			return err
		}
		item.RequestedDecision = append([]domain.Action{domain.ActionRetry}, item.RequestedDecision...)
		item.ItemVersion++
		return tx.PutAttentionItem(ctx, item)
	})
}

func (e *Engine) enqueueOperatorFeedbackRetry(ctx context.Context, command domain.Command) (bool, error) {
	replayed := false
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		request, err := tx.OperatorFeedbackIntent(ctx, operatorFeedbackInvocationID(command.CommandID))
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if request.Retry == nil || request.Retry.CommandID != command.CommandID {
			return domain.ErrParentKeyMismatch
		}
		replayed = true
		return nil
	}); err != nil || replayed {
		return false, err
	}
	var request operatorFeedbackRequest
	var invocation domain.AgentInvocation
	var run domain.Run
	var artifact domain.Artifact
	var sourceItem domain.AttentionItem
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		item, err := tx.GetAttentionItemRecord(ctx, command.ItemID)
		if err != nil {
			return err
		}
		if item.Status != domain.StatusResolved || item.DecidedAt == nil ||
			command.ItemVersion+1 != item.ItemVersion || command.PRHeadSHA != item.PRHeadSHA ||
			!slices.Equal(command.ArtifactDigests, item.ArtifactDigests) ||
			command.Message != "" || len(command.Attachments) != 0 {
			return domain.ErrParentKeyMismatch
		}
		sourceItem = item
		// Reconstruct exactly the open card accepted by this command. The
		// recorded command and concluded card remain untouched.
		item.Status, item.DecidedAt = domain.StatusOpen, nil
		item.ItemVersion--
		parent, err := tx.OperatorFeedbackRetryParent(ctx, item)
		if err != nil {
			return err
		}
		request = parent
		request.Version = domain.OperatorFeedbackRetryIntentVersion
		request.InvocationID = operatorFeedbackInvocationID(command.CommandID)
		request.StageID = operatorFeedbackStageID(request.InvocationID)
		request.Retry = &domain.OperatorFeedbackRetry{
			CommandID: command.CommandID, ItemID: command.ItemID, FailedInvocationID: parent.InvocationID,
		}
		prior, err := tx.GetAgentInvocation(ctx, parent.InvocationID)
		if err != nil {
			return err
		}
		invocation, err = domain.NewAgentInvocation(request.InvocationID, prior.InputIDs, nil, 0)
		if err != nil {
			return err
		}
		run, err = tx.GetRun(ctx, request.RunID)
		if err != nil {
			return err
		}
		artifact, err = tx.GetArtifact(ctx, request.InputArtifactID)
		return err
	}); err != nil {
		if errors.Is(err, store.ErrPublicationCompleted) {
			return e.recordCompletedOperatorFeedbackRetry(ctx, sourceItem, command)
		}
		return false, err
	}
	if err := e.authenticateOperatorFeedbackInput(ctx, request); err != nil {
		return false, err
	}
	if recorded, err := e.operatorFeedbackUndeliverableRecorded(ctx, sourceItem, command); err != nil || recorded {
		return false, err
	}
	if err := e.validateOperatorFeedbackDelivery(ctx, run, invocation, artifact); err != nil {
		if errors.Is(err, ErrProductionInputUndeliverable) {
			return e.recordOperatorFeedbackDeliveryFailure(ctx, sourceItem, command)
		}
		return false, err
	}
	payload, err := encodeOperatorFeedbackRequest(request)
	if err != nil {
		return false, err
	}
	inserted := false
	if err := runDurableTransitionHook(e.productionPublication.transitionHook,
		DurableTransitionOperatorFeedback, DurableTransitionBefore); err != nil {
		return false, err
	}
	err = e.store.Write(ctx, func(tx *store.WriteTx) error {
		storedCommand, err := tx.GetCommand(ctx, command.CommandID)
		if err != nil || !reflect.DeepEqual(storedCommand, command) {
			return errors.Join(err, domain.ErrParentKeyMismatch)
		}
		if err := tx.RequireIncompletePublication(ctx, run.ID); err != nil {
			return err
		}
		current, err := tx.GetRun(ctx, run.ID)
		if err != nil {
			return err
		}
		if _, found := findOperatorFeedbackStage(current, request.InvocationID); !found {
			current.Stages = append(current.Stages, domain.Stage{
				ID: request.StageID, RunID: run.ID, Name: productionStageName,
			})
			if err := tx.PutRun(ctx, current); err != nil {
				return err
			}
		}
		if err := tx.PutAgentInvocation(ctx, invocation); err != nil {
			return err
		}
		entry, made, err := tx.EnqueueOutbox(ctx, string(request.InvocationID), KindOperatorFeedbackInvocationRequested, payload)
		if err != nil || entry.Kind != KindOperatorFeedbackInvocationRequested || !bytes.Equal(entry.Payload, payload) {
			return errors.Join(err, domain.ErrImmutableTransition)
		}
		if _, _, err := authenticateOperatorFeedbackTransition(ctx, &tx.ReadTx, entry, run.ID, request.StageID); err != nil {
			return err
		}
		inserted = made
		return nil
	})
	if err != nil {
		if errors.Is(err, store.ErrPublicationCompleted) {
			return e.recordCompletedOperatorFeedbackRetry(ctx, sourceItem, command)
		}
		return false, err
	}
	if err := runDurableTransitionHook(e.productionPublication.transitionHook,
		DurableTransitionOperatorFeedback, DurableTransitionAfter); err != nil {
		return false, err
	}
	return inserted, nil
}

func (e *Engine) recordCompletedOperatorFeedbackRetry(ctx context.Context, source domain.AttentionItem, command domain.Command) (bool, error) {
	return e.recordOperatorFeedbackFailure(ctx, source, command, domain.AttentionSystemHealth,
		"The accepted feedback Retry cannot start because the published work unit completed before the retry was queued.")
}
