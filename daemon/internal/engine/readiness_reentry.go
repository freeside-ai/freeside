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

// StartReadinessReentry starts the cycle that re-earns readiness for a ready
// item its caller has just superseded (plan §7, issue #502). It runs in the
// transaction that wrote the supersession, after that write: the store seals
// the authority only against the superseded item's invalidation fact, and one
// transaction means a restart never finds the item superseded with no cycle
// behind it. It reports whether it started one.
//
// Only a base advance or a head change re-enters. An item that is not one of
// those, a run whose work unit already completed or whose task is cancelled,
// and a run whose latest review does not cover the item's head start nothing:
// the item stays superseded for a person, as it did before this cycle
// existed. Each of those is decided from reads, before the first write.
//
// Any other failure is returned and must fail the caller's transaction. The
// supersession is then retried with the re-entry, never committed without it.
func StartReadinessReentry(
	ctx context.Context, tx *store.WriteTx, item domain.AttentionItem,
) (bool, error) {
	fact := item.ReadinessInvalidation
	if item.Type != domain.AttentionReadyForFinalReview ||
		item.Status != domain.StatusSuperseded || fact == nil ||
		item.Subject.Type != domain.SubjectRun || item.Subject.RunID == nil {
		return false, nil
	}
	switch fact.Reason {
	case domain.ReadinessInvalidationBaseAdvanced, domain.ReadinessInvalidationHeadChanged:
	case domain.ReadinessInvalidationRetargeted, domain.ReadinessInvalidationIdentityChanged:
		return false, nil
	}
	runID := *item.Subject.RunID
	if err := tx.RequireIncompletePublication(ctx, runID); err != nil {
		if errors.Is(err, store.ErrPublicationCompleted) {
			return false, nil
		}
		return false, err
	}
	if err := requireTaskExecutionOpen(ctx, &tx.ReadTx, runID); err != nil {
		if errors.Is(err, store.ErrTaskCancellationFenced) {
			return false, nil
		}
		return false, err
	}
	current, err := tx.CurrentProductionReadyItemID(ctx, runID)
	if err != nil {
		return false, err
	}
	if current != item.ID {
		// A newer cycle already owns the run. The second of two writers that
		// observed one invalidation lands here.
		return false, nil
	}
	binding, err := tx.GetReadyItemPRBinding(ctx, item.ID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	latest, err := tx.LatestReviewRecord(ctx, runID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if latest.HeadSHA != binding.HeadSHA {
		return false, nil
	}
	failure, err := tx.LatestReviewFailure(ctx, runID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return false, err
	}
	if err == nil && failure.Round > latest.Round {
		return false, nil
	}
	reentry := domain.PublicationSuccessorReentry{Reason: fact.Reason}
	switch fact.Reason {
	case domain.ReadinessInvalidationBaseAdvanced:
		reentry.BaseSHA, reentry.HeadSHA = fact.Observed, binding.HeadSHA
	case domain.ReadinessInvalidationHeadChanged:
		// The new head is reviewed against the base the last review used. A
		// record from before reviews carried a base has none to reuse.
		if fact.Bound != binding.HeadSHA || latest.BaseSHA == "" {
			return false, nil
		}
		reentry.BaseSHA, reentry.HeadSHA = latest.BaseSHA, fact.Observed
	case domain.ReadinessInvalidationRetargeted, domain.ReadinessInvalidationIdentityChanged:
		return false, nil
	}
	successor := domain.PublicationSuccessor{
		Version:                 domain.PublicationReentryVersion,
		RunID:                   runID,
		PredecessorItemID:       item.ID,
		PriorReviewInvocationID: latest.InvocationID,
		ReviewRound:             latest.Round + 1,
		Origin:                  domain.PublicationSuccessorReadinessInvalidation,
		Reentry:                 &reentry,
	}
	task := newReentryTask(item.ProjectID, successor)
	if err := task.validate(); err != nil {
		// Coordinates the lane could not decode again start nothing: a task
		// row it would quarantine on every pass is worse than no cycle.
		return false, nil
	}
	if err := tx.RecordPublicationSuccessor(ctx, successor); err != nil {
		return false, fmt.Errorf("record readiness re-entry authority: %w", err)
	}
	payload, err := json.Marshal(task)
	if err != nil {
		return false, err
	}
	entry, _, err := tx.EnqueueOutbox(
		ctx, successor.TaskKey(), KindProductionPublicationRequested, payload)
	if err != nil {
		return false, err
	}
	if entry.Kind != KindProductionPublicationRequested || !bytes.Equal(entry.Payload, payload) {
		return false, fmt.Errorf("readiness re-entry task disagrees with stored row: %w",
			domain.ErrImmutableTransition)
	}
	return true, nil
}
