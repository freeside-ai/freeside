package engine

import (
	"context"
	"errors"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

type reviewDiminishingRoute struct {
	action domain.Action
	item   *domain.AttentionItem
}

func productionReviewDiminishingItemID(runID domain.RunID, round int) domain.ItemID {
	return store.ReviewDiminishingItemID(runID, round)
}

func (w *productionPublicationWorkflow) reconcileReviewDiminishing(
	ctx context.Context,
	task productionPublicationTask,
	record domain.ReviewRecord,
	artifact domain.FindingAdjudication,
) (reviewDiminishingRoute, productionReviewGateState, bool, error) {
	itemID := productionReviewDiminishingItemID(task.RunID, record.Round)
	existing, state, handled, found, err := w.reconcileExistingReviewDiminishing(ctx, itemID)
	if err != nil || found {
		return existing, state, handled, err
	}

	convergence, err := w.reviewConvergenceState(ctx, record)
	if err != nil {
		return reviewDiminishingRoute{}, productionReviewPending, true, err
	}
	cause, stop, err := store.EvaluateReviewConvergence(convergence, record)
	if err != nil {
		return reviewDiminishingRoute{}, productionReviewPending, true, err
	}
	if !stop {
		return reviewDiminishingRoute{}, productionReviewPending, false, nil
	}
	binding := store.ReviewDiminishingBinding{
		ItemID: itemID, RunID: task.RunID, Round: record.Round, HeadSHA: record.HeadSHA,
		FindingIDs:         append([]domain.FindingID(nil), record.FindingIDs...),
		AdjudicationDigest: artifact.Digest, FindingBatchDigest: artifact.FindingBatchDigest,
		PolicyDigest: convergence.Policy.Digest, ContinueWhile: convergence.Policy.ContinueWhile,
		LowValueStreakBeforeAttention: convergence.Policy.LowValueStreakBeforeAttention,
		HardRoundLimit:                convergence.Policy.HardRoundLimit,
		Cause:                         cause,
	}
	reason, err := store.ReviewDiminishingReason(binding)
	if err != nil {
		return reviewDiminishingRoute{}, productionReviewPending, true, err
	}
	runID := task.RunID
	createdAt := w.attentionCreatedAt()
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: itemID, ProjectID: task.ProjectID,
		Subject: domain.Subject{
			Type: domain.SubjectRun, ID: domain.SubjectID(task.RunID), RunID: &runID,
		},
		Type: domain.AttentionReviewDiminishing, Priority: domain.PriorityNormal,
		Reason: reason,
		RequestedDecision: store.ReviewDiminishingRequestedActions(
			record.Round, convergence.Policy.HardRoundLimit),
		PRHeadSHA: record.HeadSHA, YieldHistory: &convergence.History,
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate,
		CreatedAt: &createdAt, Status: domain.StatusOpen,
	}, w.approvedRecipes)
	if err != nil {
		return reviewDiminishingRoute{}, productionReviewPending, true, err
	}
	if err := w.attention.PutItem(ctx, item); err != nil {
		return reviewDiminishingRoute{}, productionReviewPending, true, err
	}
	return reviewDiminishingRoute{}, productionReviewPending, true, nil
}

func (w *productionPublicationWorkflow) reconcileExistingReviewDiminishing(
	ctx context.Context,
	itemID domain.ItemID,
) (reviewDiminishingRoute, productionReviewGateState, bool, bool, error) {
	var existing store.ReviewDiminishingDecision
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		existing, err = tx.ReviewDiminishingDecision(ctx, itemID)
		return err
	})
	if err == nil {
		if existing.Command == nil {
			return reviewDiminishingRoute{}, productionReviewPending, true, true, nil
		}
		if existing.Command.Action == domain.ActionFinishNow {
			if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
				return tx.FinishReviewDiminishing(ctx, itemID)
			}); err != nil {
				return reviewDiminishingRoute{}, productionReviewPending, true, true, err
			}
			return reviewDiminishingRoute{}, productionReviewPassed, true, true, nil
		}
		if existing.Command.Action == domain.ActionApplyThenFinish ||
			existing.Command.Action == domain.ActionContinueUnderPolicy {
			item := existing.Item
			return reviewDiminishingRoute{
				action: existing.Command.Action, item: &item,
			}, productionReviewPending, false, true, nil
		}
		return reviewDiminishingRoute{}, productionReviewPending, true, true,
			domain.ErrTransitionCommandMismatch
	}
	if !errors.Is(err, store.ErrNotFound) {
		return reviewDiminishingRoute{}, productionReviewPending, true, false, err
	}
	return reviewDiminishingRoute{}, productionReviewPending, false, false, nil
}

func (w *productionPublicationWorkflow) reviewConvergenceState(
	ctx context.Context, current domain.ReviewRecord,
) (store.ReviewConvergenceState, error) {
	var state store.ReviewConvergenceState
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		state, err = tx.ReviewConvergenceStateAtDecision(ctx, current)
		return err
	})
	if err != nil {
		return store.ReviewConvergenceState{}, err
	}
	return state, nil
}
