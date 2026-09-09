package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const publicationContinuationReason = "The rechecked publication needs code changes. Approve starts one remediation continuation with fresh verification and review on the same pull request."

var errPublicationContinuationOfferUnchanged = errors.New("publication continuation offer unchanged")

func (e *Engine) reconcilePublicationContinuations(ctx context.Context) (int, error) {
	if e.productionPublication == nil {
		return 0, nil
	}
	var commands []domain.Command
	var rechecks []domain.Command
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		rechecks, err = tx.ListCommandsForActions(ctx, domain.ActionRerunTrustEvaluation)
		if err != nil {
			return err
		}
		commands, err = tx.ListCommandsForActions(ctx, domain.ActionApprove)
		return err
	}); err != nil {
		return 0, err
	}
	for _, recheck := range rechecks {
		if err := e.offerPublicationContinuation(ctx, recheck); err != nil {
			return 0, err
		}
	}
	created := 0
	for _, command := range commands {
		if !strings.HasPrefix(string(command.ItemID), domain.PublicationContinuationItemPrefix) {
			continue
		}
		made, err := e.enqueuePublicationContinuation(ctx, command)
		if err != nil {
			return created, err
		}
		created += boolCount(made)
	}
	return created, nil
}

// Completed rechecks are absent from the pending task scan. Upgrade only their
// still-open legacy disputes, without replaying the task or accepting a command.
func (e *Engine) offerPublicationContinuation(ctx context.Context, recheck domain.Command) error {
	err := e.store.Write(ctx, func(tx *store.WriteTx) error {
		item, err := tx.GetAttentionItem(ctx, domain.ItemID(domain.PublicationContinuationItemPrefix+recheck.CommandID))
		if err != nil {
			return err
		}
		if item.Status != domain.StatusOpen || item.Type != domain.AttentionReviewDispute || item.ReviewDispute != nil {
			return errPublicationContinuationOfferUnchanged
		}
		if item.Subject.RunID == nil {
			return domain.ErrParentKeyMismatch
		}
		run, err := tx.GetRun(ctx, *item.Subject.RunID)
		if err != nil {
			return err
		}
		if err := tx.RequireIncompletePublication(ctx, run.ID); err != nil {
			return err
		}
		entry, err := tx.GetOutbox(ctx, signet.PublicationReevaluationKey(run.ID, recheck.CommandID))
		if err != nil {
			return err
		}
		request, err := signet.DecodePublicationReevaluationRequest(entry.Payload)
		if err != nil {
			return errors.Join(err, domain.ErrParentKeyMismatch)
		}
		completion, found, err := signet.AuthenticatePublicationReevaluationCompletion(ctx, &tx.ReadTx, run, request)
		if err != nil || !found || completion.Outcome != signet.PublicationReevaluationReviewEscalated {
			return errors.Join(err, domain.ErrParentKeyMismatch)
		}
		source, err := productionTaskForBlockedItem(ctx, &tx.ReadTx, run.ID, recheck.ItemID)
		if err != nil {
			return err
		}
		task, err := decodeProductionPublicationTask(source)
		if err != nil {
			return errors.Join(err, domain.ErrParentKeyMismatch)
		}
		review, err := tx.LatestReviewRecord(ctx, run.ID)
		if err != nil {
			return err
		}
		if !source.Dispatched() || task.RunID != run.ID || task.ProjectID != item.ProjectID ||
			task.HeadSHA != request.PRHeadSHA || item.PRHeadSHA != request.PRHeadSHA ||
			request.CommandID != recheck.CommandID || review.HeadSHA != request.PRHeadSHA ||
			review.Round < request.ReviewRound || review.Outcome != domain.ReviewFindings {
			return domain.ErrParentKeyMismatch
		}
		artifact, err := tx.GetFindingAdjudicationForRound(ctx, run.ID, review.Round)
		if err != nil {
			return err
		}
		routes, err := effectiveFindingRoutesTx(ctx, &tx.ReadTx, artifact)
		if err != nil {
			return err
		}
		if len(remediationFindingIDs(artifact, routes)) == 0 {
			return domain.ErrParentKeyMismatch
		}
		item.RequestedDecision = []domain.Action{domain.ActionApprove, domain.ActionDiscuss, domain.ActionStop}
		item.ReviewDispute = &domain.ReviewDisputeBinding{RunID: review.RunID, Round: review.Round, FindingIDs: slices.Clone(review.FindingIDs), CompletionEvidence: review.CompletionEvidence}
		item.Reason = publicationContinuationReason
		item.ItemVersion++
		return tx.PutAttentionItem(ctx, item)
	})
	// Missing or damaged retained evidence cannot grant a new action. Existing
	// command/task reconciliation owns diagnosis; a repair is only an offer.
	if errors.Is(err, errPublicationContinuationOfferUnchanged) || errors.Is(err, store.ErrNotFound) || errors.Is(err, domain.ErrParentKeyMismatch) || errors.Is(err, domain.ErrImmutableTransition) {
		return nil
	}
	return err
}

func (e *Engine) enqueuePublicationContinuation(ctx context.Context, command domain.Command) (bool, error) {
	var item domain.AttentionItem
	var r domain.PublicationContinuationIntent
	replayed := false
	err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		item, err = tx.GetAttentionItemRecord(ctx, command.ItemID)
		if err != nil {
			return err
		}
		if item.Subject.RunID == nil {
			return domain.ErrParentKeyMismatch
		}
		runID := *item.Subject.RunID
		r.Successor.RunID, r.Successor.CommandID = runID, command.CommandID
		if _, err := tx.GetOutbox(ctx, r.Key()); err == nil {
			// Pending payloads are authenticated and quarantined by the task
			// scan. Do not turn a damaged row into a lane-fatal command replay.
			replayed = true
			return nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err := tx.RequireIncompletePublication(ctx, runID); err != nil {
			return err
		}
		rerunID := strings.TrimPrefix(string(item.ID), domain.PublicationContinuationItemPrefix)
		rerun, err := tx.GetCommand(ctx, rerunID)
		if err != nil {
			return err
		}
		source, err := productionTaskForBlockedItem(ctx, tx, runID, rerun.ItemID)
		if err != nil {
			return err
		}
		task, err := decodeProductionPublicationTask(source)
		if err != nil {
			return err
		}
		prior, err := tx.LatestReviewRecord(ctx, runID)
		if err != nil {
			return err
		}
		adjudication, err := tx.GetFindingAdjudicationForRound(ctx, runID, prior.Round)
		if err != nil {
			return err
		}
		r = domain.PublicationContinuationIntent{
			Version: domain.PublicationContinuationIntentVersion,
			Successor: domain.PublicationSuccessor{
				Version: domain.PublicationContinuationVersion, Origin: domain.PublicationSuccessorRemediation,
				RunID: runID, CommandID: command.CommandID, ReevaluationCommandID: rerunID,
				PredecessorItemID: task.readyItemID(), PriorReviewInvocationID: prior.InvocationID, ReviewRound: prior.Round + 1,
			},
			SourceTaskKey: source.IdempotencyKey, SourceTaskDigest: digestProductionBytes(source.Payload), AdjudicationDigest: adjudication.Digest,
		}
		return tx.AuthenticatePublicationContinuation(ctx, r)
	})
	if errors.Is(err, store.ErrPublicationCompleted) {
		return e.recordOperatorFeedbackFailure(ctx, item, command, domain.AttentionSystemHealth,
			"The accepted remediation continuation cannot start because the published work unit completed before remediation was queued.")
	}
	if errors.Is(err, store.ErrNoPublishedContinuationTarget) {
		return e.recordOperatorFeedbackFailure(ctx, item, command, domain.AttentionSystemHealth,
			"The accepted remediation continuation cannot start because this run has no published pull request to update. A continuation does not authorize creating the first pull request.")
	}
	if err != nil || replayed {
		return false, err
	}
	if recorded, err := e.operatorFeedbackUndeliverableRecorded(ctx, item, command); err != nil || recorded {
		return false, err
	}
	payload, err := json.Marshal(r)
	if err != nil {
		return false, err
	}
	// Reconstruct the completed recheck before creating any new authority.
	if _, err := e.productionPublication.reconstructPublicationContinuationTask(ctx, store.QueueEntry{IdempotencyKey: r.Key(), Kind: domain.PublicationContinuationRequestedKind, Payload: payload}); err != nil {
		return false, err
	}
	created := false
	err = e.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.RequireIncompletePublication(ctx, r.Successor.RunID); err != nil {
			return err
		}
		if err := tx.AuthenticatePublicationContinuation(ctx, r); err != nil {
			return err
		}
		entry, made, err := tx.EnqueueOutbox(ctx, r.Key(), domain.PublicationContinuationRequestedKind, payload)
		if err != nil || entry.Kind != domain.PublicationContinuationRequestedKind || !bytes.Equal(entry.Payload, payload) {
			return errors.Join(err, domain.ErrImmutableTransition)
		}
		created = made
		return nil
	})
	if errors.Is(err, store.ErrPublicationCompleted) {
		return e.recordOperatorFeedbackFailure(ctx, item, command, domain.AttentionSystemHealth,
			"The accepted remediation continuation cannot start because the published work unit completed before remediation was queued.")
	}
	return created, err
}

func (w *productionPublicationWorkflow) reconstructPublicationContinuationTask(ctx context.Context, entry store.QueueEntry) (productionPublicationTask, error) {
	r, err := domain.DecodePublicationContinuationIntent(entry.Payload)
	if err != nil || entry.Kind != domain.PublicationContinuationRequestedKind || entry.IdempotencyKey != r.Key() {
		return productionPublicationTask{}, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	var reevaluation store.QueueEntry
	err = w.store.Read(ctx, func(tx *store.ReadTx) error {
		if err := tx.AuthenticatePublicationContinuation(ctx, r); err != nil {
			return err
		}
		var err error
		reevaluation, err = tx.GetOutbox(ctx, signet.PublicationReevaluationKey(r.Successor.RunID, r.Successor.ReevaluationCommandID))
		if err != nil {
			return err
		}
		request, err := signet.DecodePublicationReevaluationRequest(reevaluation.Payload)
		if err != nil {
			return err
		}
		run, err := tx.GetRun(ctx, request.RunID)
		if err != nil {
			return err
		}
		completion, found, err := signet.AuthenticatePublicationReevaluationCompletion(ctx, tx, run, request)
		if err != nil || !found || completion.Outcome != signet.PublicationReevaluationReviewEscalated {
			return errors.Join(err, domain.ErrParentKeyMismatch)
		}
		return nil
	})
	if err != nil {
		return productionPublicationTask{}, err
	}
	task, err := w.reconstructProductionReevaluationTask(ctx, reevaluation)
	if err != nil {
		return task, err
	}
	source := task
	source.reevaluation = nil
	body, err := json.Marshal(source)
	if err != nil || source.intentKey() != r.SourceTaskKey || digestProductionBytes(body) != r.SourceTaskDigest {
		return task, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	task.continuation = &r
	return task, nil
}

func (w *productionPublicationWorkflow) startPublicationContinuation(ctx context.Context, source productionPublicationTask, root string) (productionTaskOutcome, error) {
	r := *source.continuation
	var record domain.ReviewRecord
	var artifact domain.FindingAdjudication
	var routes map[domain.FindingID]domain.AdjudicationRoute
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		record, err = tx.GetReviewRecord(ctx, r.Successor.PriorReviewInvocationID)
		if err != nil {
			return err
		}
		artifact, err = tx.GetFindingAdjudication(ctx, r.AdjudicationDigest)
		if err != nil {
			return err
		}
		routes, err = effectiveFindingRoutesTx(ctx, tx, artifact)
		return err
	}); err != nil {
		return productionTaskOutcome{}, err
	}
	task := source
	task.Successor, task.PublicationID, task.reevaluation = &r.Successor, r.Successor.PublicationID(), nil
	intent, err := w.prepareRemediationIntent(ctx, task, record, artifact, routes, root)
	if errors.Is(err, ErrRemediationInputUndeliverable) {
		return w.refusePublicationContinuation(ctx, source, "The accepted remediation continuation cannot start because its input exceeds the delivery limit.")
	}
	if err != nil {
		return productionTaskOutcome{}, err
	}
	if intent == nil {
		return productionTaskOutcome{}, domain.ErrParentKeyMismatch
	}
	err = w.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := intent.persist(ctx, tx); err != nil {
			return err
		}
		return persistFindingRouteDispositions(ctx, tx, artifact, routes, artifact.CreatedAt)
	})
	if errors.Is(err, store.ErrPublicationCompleted) {
		return w.refuseCompletedPublicationContinuation(ctx, source)
	}
	return productionTaskOutcome{completed: err == nil}, err
}

func (w *productionPublicationWorkflow) refuseCompletedPublicationContinuation(ctx context.Context, task productionPublicationTask) (productionTaskOutcome, error) {
	return w.refusePublicationContinuation(ctx, task, "The accepted remediation continuation cannot start because the published work unit completed before remediation was queued.")
}

func (w *productionPublicationWorkflow) refusePublicationContinuation(ctx context.Context, task productionPublicationTask, reason string) (productionTaskOutcome, error) {
	var item domain.AttentionItem
	var command domain.Command
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		command, err = tx.GetCommand(ctx, task.continuation.Successor.CommandID)
		if err != nil {
			return err
		}
		item, err = tx.GetAttentionItemRecord(ctx, command.ItemID)
		return err
	}); err != nil {
		return productionTaskOutcome{}, err
	}
	e := &Engine{store: w.store, productionPublication: w}
	if _, err := e.recordOperatorFeedbackFailure(ctx, item, command, domain.AttentionSystemHealth, reason); err != nil {
		return productionTaskOutcome{}, err
	}
	err := w.store.Write(ctx, func(tx *store.WriteTx) error { return tx.MarkOutboxDispatched(ctx, task.continuation.Key()) })
	return productionTaskOutcome{completed: err == nil}, err
}

func PublicationContinuationBackupPayloadDigests(entry store.QueueEntry) ([]domain.Digest, error) {
	r, err := domain.DecodePublicationContinuationIntent(entry.Payload)
	if err != nil || entry.Kind != domain.PublicationContinuationRequestedKind || entry.IdempotencyKey != r.Key() {
		return nil, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	return nil, nil // Source replay and adjudication blobs are rooted by their own records.
}

func persistContinuationAuthority(ctx context.Context, tx *store.WriteTx, r domain.PublicationContinuationIntent) error {
	stored, entry, err := tx.PublicationContinuationIntent(ctx, r.Successor.RunID, r.Successor.CommandID)
	if err != nil || !reflect.DeepEqual(stored, r) || entry.Dispatched() {
		return errors.Join(err, domain.ErrImmutableTransition)
	}
	if err := tx.RequireIncompletePublication(ctx, r.Successor.RunID); err != nil {
		return err
	}
	if _, err := tx.GetOutbox(ctx, r.Successor.TaskKey()); !errors.Is(err, store.ErrNotFound) {
		return errors.Join(err, domain.ErrImmutableTransition)
	}
	if err := tx.MarkOutboxDispatched(ctx, r.Key()); err != nil {
		return err
	}
	return tx.RecordPublicationSuccessor(ctx, r.Successor)
}
