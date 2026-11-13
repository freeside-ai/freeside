package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// handBackReentryPlan is the hand-back cycle one trigger pass decided to
// start, derived from reads alone. item is the open ready item the cycle
// supersedes, or nil when the run has none. successor names no base: the
// base is the head's merge base with the target, a forge read the caller
// makes only once a cycle is due.
type handBackReentryPlan struct {
	item      *domain.AttentionItem
	successor domain.PublicationSuccessor
	projectID domain.ProjectID
}

// planHandBackReentry decides, from reads, whether a handed-back pull
// request starts a cycle that adopts head, the head the caller observed on
// it under the hand-back label (issue #3160), and returns nil when none
// should. The probe and the writer both run it.
//
// The label is the caller's observation; everything else is read here and
// proved again by the store when it seals the authority. A cycle starts only
// when the run's current chain item is its open ready item or one that no
// longer stands, the task that wrote it has ended, the run's last recorded
// pull fact is open and unmerged on head, the run has a review and no later
// review failure, and an admitted account's conversation
// comment on head is one no sealed cycle named and no disposition answered.
// One comment starts one cycle, the earliest first.
func planHandBackReentry(
	ctx context.Context, tx *store.ReadTx, runID domain.RunID, head string,
) (*handBackReentryPlan, error) {
	if head == "" {
		return nil, nil
	}
	// Most passes end here: nothing was said on the handed-back head.
	findings, err := tx.ListExternalFindings(ctx, runID)
	if err != nil {
		return nil, err
	}
	findings = slices.DeleteFunc(findings, func(finding domain.Finding) bool {
		return finding.External == nil || finding.External.HeadSHA != head || !finding.External.OnConversationThread()
	})
	if len(findings) == 0 {
		return nil, nil
	}
	published, err := tx.PublishedProductionReadyItemID(ctx, runID)
	if err != nil {
		return nil, err
	}
	binding, err := tx.GetReadyItemPRBinding(ctx, published)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	pull, err := tx.LatestPullMergeFact(ctx, binding.RepositoryID, binding.PRNumber)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if pull.State != domain.PullRequestOpen || pull.Merged || pull.HeadSHA != head || pull.BaseRef != binding.BaseRef {
		return nil, nil
	}
	if err := tx.RequireIncompletePublication(ctx, runID); err != nil {
		if errors.Is(err, store.ErrPublicationCompleted) {
			return nil, nil
		}
		return nil, err
	}
	if err := requireTaskExecutionOpen(ctx, tx, runID); err != nil {
		if workflowAdvancementFenced(err) {
			return nil, nil
		}
		return nil, err
	}
	chain, err := tx.PublicationSuccessorChain(ctx, runID)
	if err != nil {
		return nil, err
	}
	current := domain.ProductionReadyItemID(runID)
	taskKey := productionPublicationTaskKey(runID)
	answered := make(map[domain.FindingID]bool)
	handedBack := false
	for _, sealed := range chain {
		current, taskKey = sealed.ReadyItemID(), sealed.TaskKey()
		if sealed.ExternalFindingID != "" {
			answered[sealed.ExternalFindingID] = true
		}
		handedBack = handedBack || sealed.HandsBack()
	}
	// The task that wrote the current item, or ended without one, still acts
	// for the run until it ends (planExternalReviewReentry).
	owner, err := tx.GetOutbox(ctx, taskKey)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if err == nil && !owner.Dispatched() {
		return nil, nil
	}
	var open *domain.AttentionItem
	item, err := tx.GetAttentionItem(ctx, current)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return nil, err
	case item.Type != domain.AttentionReadyForFinalReview:
		return nil, nil
	case item.Status == domain.StatusOpen:
		open = &item
	}
	// An earlier hand-back that ended on a person leaves that person's item
	// open, and the store refuses a new cycle over it. Decided here, the
	// refusal costs no write transaction on every pass.
	if handedBack {
		items, err := tx.ListOpenAttentionItemRecordsForRun(ctx, runID)
		if err != nil {
			return nil, err
		}
		if slices.ContainsFunc(items, handBackEnding) {
			return nil, nil
		}
	}
	dispositions, err := tx.ListExternalFindingDispositions(ctx, runID)
	if err != nil {
		return nil, err
	}
	for _, disposition := range dispositions {
		answered[disposition.FindingID] = true
	}
	latest, err := tx.LatestReviewRecord(ctx, runID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	failure, err := tx.LatestReviewFailure(ctx, runID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if err == nil && failure.Round > latest.Round {
		return nil, nil
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
	profile, err := tx.LatestTrustProfile(ctx, binding.Repo)
	if err != nil {
		return nil, err
	}
	successor := domain.PublicationSuccessor{
		Version:                 domain.PublicationHandBackVersion,
		RunID:                   runID,
		PredecessorItemID:       current,
		PriorReviewInvocationID: latest.InvocationID,
		ReviewRound:             latest.Round + 1,
		Origin:                  domain.PublicationSuccessorHandBack,
		Reentry:                 &domain.PublicationSuccessorReentry{HeadSHA: head},
		ExternalFindingID:       trigger.ID,
		AdmittingProfileDigest:  profile.ProfileDigest,
	}
	run, err := tx.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	return &handBackReentryPlan{item: open, successor: successor, projectID: run.ProjectID}, nil
}

// handBackEnding reports an open item a hand-back cycle ends on when it
// cannot re-earn readiness.
func handBackEnding(item domain.AttentionItem) bool {
	return item.Type == domain.AttentionPublishBlocked || item.Type == domain.AttentionReviewDispute ||
		item.Type == domain.AttentionReviewDiminishing
}

// HandBackReentryDue reports whether StartHandBackReentry would start a
// cycle for the run on head now, so a caller that runs on every pass opens a
// write transaction only when one is due.
func HandBackReentryDue(ctx context.Context, tx *store.ReadTx, runID domain.RunID, head string) (bool, error) {
	plan, err := planHandBackReentry(ctx, tx, runID, head)
	return plan != nil, err
}

// StartHandBackReentry starts the cycle that reworks a handed-back pull
// request on its current head (issue #3160): it supersedes the run's open
// ready item when it has one, seals the hand_back authority, and queues the
// cycle's task, all in the caller's transaction. It returns the superseded
// item's ID, empty when nothing was superseded, and whether a cycle started.
// The decision is made again from this transaction's reads before its first
// write; any failure after that write must fail the caller's transaction.
//
// base is the head's merge base with the target branch as the forge reports
// it. The cycle reviews the head against it, so the review covers every
// change the pull request makes, and the cycle proves it before reviewing:
// the base must be reachable from the target branch and the head must
// descend from it.
func StartHandBackReentry(
	ctx context.Context, tx *store.WriteTx, runID domain.RunID, head, base string,
) (domain.ItemID, bool, error) {
	plan, err := planHandBackReentry(ctx, &tx.ReadTx, runID, head)
	if err != nil || plan == nil {
		return "", false, err
	}
	plan.successor.Reentry.BaseSHA = base
	task := newReentryTask(plan.projectID, plan.successor)
	if err := task.validate(); err != nil {
		return "", false, fmt.Errorf("hand-back task: %w", err)
	}
	var superseded domain.ItemID
	if plan.item != nil {
		item := *plan.item
		item.ItemVersion++
		item, err = item.ConcludeByDaemon(domain.ConcludedBySupersession, time.Now().UTC(),
			"Superseded when the pull request was handed back for rework.", nil)
		if err != nil {
			return "", false, fmt.Errorf("withdraw ready item for hand-back: %w", err)
		}
		if err := tx.PutAttentionItem(ctx, item); err != nil {
			return "", false, fmt.Errorf("withdraw ready item for hand-back: %w", err)
		}
		superseded = item.ID
	}
	if err := tx.RecordPublicationSuccessor(ctx, plan.successor); err != nil {
		return "", false, fmt.Errorf("record hand-back authority: %w", err)
	}
	payload, err := json.Marshal(task)
	if err != nil {
		return "", false, err
	}
	entry, _, err := tx.EnqueueOutbox(ctx, plan.successor.TaskKey(), KindProductionPublicationRequested, payload)
	if err != nil {
		return "", false, err
	}
	if entry.Kind != KindProductionPublicationRequested || !bytes.Equal(entry.Payload, payload) {
		return "", false, fmt.Errorf("hand-back task disagrees with stored row: %w", domain.ErrImmutableTransition)
	}
	return superseded, true, nil
}
