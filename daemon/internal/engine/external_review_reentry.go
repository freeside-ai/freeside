package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// externalReviewReentryPlan is the cycle one trigger pass decided to start,
// derived from reads alone.
type externalReviewReentryPlan struct {
	item      domain.AttentionItem
	successor domain.PublicationSuccessor
	task      productionPublicationTask
}

// planExternalReviewReentry decides, from reads, whether an admitted external
// finding should start a cycle for a ready item, and returns nil when none
// should. It is the whole decision: the read-only probe and the writer both
// run it, so the writer never acts on a decision made in another transaction.
//
// A cycle starts only when the item is the run's open, current ready item,
// its latest review covers the published head, that head is one Freeside
// pushed, and a stored external finding on it is one the repository's active
// trust profile admits and no earlier cycle answered. One finding starts one
// cycle, the earliest first.
func planExternalReviewReentry(
	ctx context.Context, tx *store.ReadTx, itemID domain.ItemID,
) (*externalReviewReentryPlan, error) {
	item, err := tx.GetAttentionItem(ctx, itemID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if item.Type != domain.AttentionReadyForFinalReview || item.Status != domain.StatusOpen ||
		item.Subject.Type != domain.SubjectRun || item.Subject.RunID == nil {
		return nil, nil
	}
	runID := *item.Subject.RunID
	binding, err := tx.GetReadyItemPRBinding(ctx, item.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// Most passes end here: nothing was left on the published head.
	findings, err := tx.ListExternalFindings(ctx, runID)
	if err != nil {
		return nil, err
	}
	findings = slices.DeleteFunc(findings, func(finding domain.Finding) bool {
		return finding.External.HeadSHA != binding.HeadSHA
	})
	if len(findings) == 0 {
		return nil, nil
	}
	profile, err := tx.LatestTrustProfile(ctx, binding.Repo)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// Anyone can comment on a pull request, so the gated admission read below
	// runs only for reviewers the profile lists. It still decides.
	findings = slices.DeleteFunc(findings, func(finding domain.Finding) bool {
		_, listed := profile.ExternalReviewAuthorityFor(
			finding.External.Forge, finding.External.ReviewerAccountID, finding.External.ReviewerLogin)
		return !listed
	})
	if len(findings) == 0 {
		return nil, nil
	}
	if err := tx.RequireIncompletePublication(ctx, runID); err != nil {
		if errors.Is(err, store.ErrPublicationCompleted) {
			return nil, nil
		}
		return nil, err
	}
	if err := requireTaskExecutionOpen(ctx, tx, runID); err != nil {
		if errors.Is(err, store.ErrTaskCancellationFenced) {
			return nil, nil
		}
		return nil, err
	}
	chain, err := tx.PublicationSuccessorChain(ctx, runID)
	if err != nil {
		return nil, err
	}
	current := domain.ProductionReadyItemID(runID)
	answered := make(map[domain.FindingID]bool)
	for _, sealed := range chain {
		current = sealed.ReadyItemID()
		if sealed.ExternalFindingID != "" {
			answered[sealed.ExternalFindingID] = true
		}
	}
	if current != item.ID {
		return nil, nil
	}
	latest, err := tx.LatestReviewRecord(ctx, runID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// The cycle reviews the published head against the base its last review
	// used. A record from before reviews carried a base has none to reuse.
	if latest.HeadSHA != binding.HeadSHA || latest.BaseSHA == "" {
		return nil, nil
	}
	failure, err := tx.LatestReviewFailure(ctx, runID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if err == nil && failure.Round > latest.Round {
		return nil, nil
	}
	// The store refuses this authority on a head someone else pushed. Decided
	// here, the refusal leaves the ready item open.
	foreign, err := tx.ReadyHeadIsForeign(ctx, item.ID)
	if err != nil || foreign {
		return nil, err
	}
	var trigger *domain.Finding
	for _, finding := range findings {
		if answered[finding.ID] {
			continue
		}
		admitted, err := tx.GetAdmittedExternalFinding(ctx, finding.ID)
		if errors.Is(err, domain.ErrExternalReviewNotAdmitted) {
			continue
		}
		if err != nil {
			return nil, err
		}
		trigger = &admitted
		break
	}
	if trigger == nil {
		return nil, nil
	}
	successor := domain.PublicationSuccessor{
		Version:                 domain.PublicationExternalReviewVersion,
		RunID:                   runID,
		PredecessorItemID:       item.ID,
		PriorReviewInvocationID: latest.InvocationID,
		ReviewRound:             latest.Round + 1,
		Origin:                  domain.PublicationSuccessorExternalReview,
		Reentry: &domain.PublicationSuccessorReentry{
			BaseSHA: latest.BaseSHA, HeadSHA: binding.HeadSHA,
		},
		ExternalFindingID:      trigger.ID,
		AdmittingProfileDigest: profile.ProfileDigest,
	}
	task := newReentryTask(item.ProjectID, successor)
	if err := task.validate(); err != nil {
		// Coordinates the lane could not decode again start nothing: a task
		// row it would quarantine on every pass is worse than no cycle.
		return nil, nil
	}
	return &externalReviewReentryPlan{item: item, successor: successor, task: task}, nil
}

// ExternalReviewReentryDue reports whether StartExternalReviewReentry would
// start a cycle for the item now. A write transaction advances the
// client-visible revision even when it writes nothing, so a caller that runs
// on every pass asks here first and opens one only when a cycle is due.
func ExternalReviewReentryDue(
	ctx context.Context, tx *store.ReadTx, itemID domain.ItemID,
) (bool, error) {
	plan, err := planExternalReviewReentry(ctx, tx, itemID)
	return plan != nil, err
}

// StartExternalReviewReentry starts the cycle that answers an external
// reviewer's finding on a published pull request (plan §5.19, §7; issue
// #524), and reports whether it started one. Unlike StartReadinessReentry
// nothing has invalidated the ready item: this function supersedes it, seals
// the authority, and queues the cycle's task, all in the caller's
// transaction, so readiness is never withdrawn without a cycle behind it.
//
// Whether to start is decided again here, from this transaction's reads and
// before its first write, so a pass with nothing to start leaves the item
// untouched. Any failure after the first write is returned and must fail the
// caller's transaction.
func StartExternalReviewReentry(
	ctx context.Context, tx *store.WriteTx, itemID domain.ItemID,
) (bool, error) {
	plan, err := planExternalReviewReentry(ctx, &tx.ReadTx, itemID)
	if err != nil || plan == nil {
		return false, err
	}
	item := plan.item
	item.Status = domain.StatusSuperseded
	item.ItemVersion++
	if err := tx.PutAttentionItem(ctx, item); err != nil {
		return false, fmt.Errorf("withdraw ready item for external review: %w", err)
	}
	if err := tx.RecordPublicationSuccessor(ctx, plan.successor); err != nil {
		return false, fmt.Errorf("record external review re-entry authority: %w", err)
	}
	payload, err := json.Marshal(plan.task)
	if err != nil {
		return false, err
	}
	entry, _, err := tx.EnqueueOutbox(
		ctx, plan.successor.TaskKey(), KindProductionPublicationRequested, payload)
	if err != nil {
		return false, err
	}
	if entry.Kind != KindProductionPublicationRequested || !bytes.Equal(entry.Payload, payload) {
		return false, fmt.Errorf("external review re-entry task disagrees with stored row: %w",
			domain.ErrImmutableTransition)
	}
	return true, nil
}
