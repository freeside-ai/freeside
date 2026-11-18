package engine

import (
	"context"
	"errors"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// HandBackWait is a handed-back pull request whose head waits for an
// admitted account's conversation comment before a hand-back cycle can start
// (issue #3192). Commenters are the sorted, distinct logins of the accounts
// that commented on the head and whom the trust profile's external-reviewer
// allowlist does not admit to drive a round; empty means nobody commented.
// The logins are untrusted forge text, fit only for display.
type HandBackWait struct {
	Repo       string
	PRNumber   int
	HeadSHA    string
	Commenters []string
}

// HandBackCommentGap reports why a pull request the caller observed under
// the hand-back label on head waits, or nil when it does not wait on a
// comment. It only reads, and it decides nothing about whether a cycle
// starts: planHandBackReentry stays the trigger, and this explains one of
// the ways it declines.
//
// It returns nil when the run has no published pull request open and
// unmerged on head and its bound target, its publication is complete, an
// admitted account commented on head, a hand-back cycle started on head or
// the run's current cycle is still running, or the label is the one a
// hand-back cycle earned on head and Freeside has not yet recorded taking it
// off. Other reasons a cycle cannot start, such as an earlier hand-back's
// open ending, a review failure, a stopped task, or a project that is not
// ready, have their own items or refusals and are not checked here.
func HandBackCommentGap(
	ctx context.Context, tx *store.ReadTx, runID domain.RunID, head string,
) (*HandBackWait, error) {
	if head == "" {
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
	findings, err := tx.ListExternalFindings(ctx, runID)
	if err != nil {
		return nil, err
	}
	var commenters []string
	for _, finding := range findings {
		if finding.External == nil || finding.External.HeadSHA != head || !finding.External.OnConversationThread() {
			continue
		}
		_, err := tx.GetAdmittedExternalFinding(ctx, finding.ID)
		if errors.Is(err, domain.ErrExternalReviewNotAdmitted) {
			commenters = append(commenters, finding.External.ReviewerLogin)
			continue
		}
		if err != nil {
			return nil, err
		}
		// An admitted comment either started a cycle or waits on something
		// other than a comment.
		return nil, nil
	}
	// Walk the chain as planHandBackReentry does: the last authority names
	// the task that acts for the run until it ends.
	chain, err := tx.PublicationSuccessorChain(ctx, runID)
	if err != nil {
		return nil, err
	}
	taskKey := productionPublicationTaskKey(runID)
	for _, sealed := range chain {
		if sealed.HandsBack() && sealed.Reentry != nil && sealed.Reentry.HeadSHA == head {
			return nil, nil
		}
		taskKey = sealed.TaskKey()
	}
	owner, err := tx.GetOutbox(ctx, taskKey)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if err == nil && !owner.Dispatched() {
		return nil, nil
	}
	// A hand-back cycle that made the run ready again on head leaves the
	// label for the remover to take off; until it records doing so, the
	// label is Freeside's, not a new hand-back.
	ready, earned, err := tx.HandBackReadyBinding(ctx, runID)
	if err != nil {
		return nil, err
	}
	if earned && ready.HeadSHA == head {
		_, err := tx.GetOutbox(ctx, publish.HandBackLabelRemovalKey(ready.ItemID))
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
	}
	slices.Sort(commenters)
	return &HandBackWait{
		Repo: binding.Repo, PRNumber: binding.PRNumber, HeadSHA: head, Commenters: slices.Compact(commenters),
	}, nil
}
