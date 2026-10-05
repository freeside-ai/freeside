package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// The forge's review states whose body is not a finding. An approval is not
// one by issue #524's decision 1. A dismissed review is one a maintainer
// withdrew, and the forge no longer says whether it approved: counted, an
// approval would become a finding the moment someone dismissed it.
const (
	externalReviewApprovedState  = "APPROVED"
	externalReviewDismissedState = "DISMISSED"
)

// The thread ID forms an external finding carries (issue #524, decision 3).
// A reply joins its thread's first comment, so every comment of one thread
// shares one ID. #1636 addresses its replies by these IDs.
func externalReviewThreadID(reviewID int64) string {
	return fmt.Sprintf("review/%d", reviewID)
}

func externalReviewCommentThreadID(rootCommentID int64) string {
	return fmt.Sprintf("review_comment/%d", rootCommentID)
}

// buildExternalFindings normalizes one raw observation of a published pull
// request's review activity into external findings (plan §5.19, §7; issue
// #524): one for each submitted review that has a body and is neither an
// approval nor dismissed, and one for each inline comment with a body. The caller must have fetched obs for
// binding's own pull request: a finding names no repository or pull request,
// so the run is its only tie to one.
//
// Every identity's activity becomes a finding. Nothing here consults the
// allowlist, and a stored finding grants nothing: admission is decided at the
// trigger, against the trust profile active at that moment.
//
// The forge's fields are bound as returned, never repaired. The reviewer is
// the account ID and the login exactly as the forge sent them for that
// comment, because the allowlist matches both. The head is the commit the
// reviewer commented on: a review's own commit, and a comment's original
// commit and range, which the forge keeps when it re-anchors the comment to a
// newer head. Bound to the moving commit, an old comment would become a new
// finding on every later head. The timestamp is the forge's, so a later pass
// over the same activity rebuilds the same bytes.
//
// Activity that cannot be a valid finding is reported in skipped and stored
// nowhere: text is never truncated to fit, since the finding's digest names
// the reviewer's exact words.
func buildExternalFindings(
	obs publish.PullReviewObservation, binding domain.ReadyItemPRBinding,
) (findings []domain.Finding, skipped []error) {
	seen := map[domain.FindingID]bool{}
	add := func(what string, id int64, in domain.ExternalFindingInput) {
		in.RunID = binding.RunID
		in.Forge = domain.ExternalReviewForgeGitHub
		in.Severity = domain.FindingSeverity(nativeReviewBadge(in.RawText))
		in.Message = in.RawText
		finding, err := domain.NewExternalFinding(in)
		if err != nil {
			skipped = append(skipped, fmt.Errorf("skip %s %d on %s#%d: %w",
				what, id, binding.Repo, binding.PRNumber, err))
			return
		}
		// One reviewer repeating identical text in one thread on one head has
		// one ID. The first by forge time is the finding; a repeat says
		// nothing new.
		if seen[finding.ID] {
			return
		}
		seen[finding.ID] = true
		findings = append(findings, finding)
	}

	reviews := slices.Clone(obs.Reviews)
	slices.SortFunc(reviews, func(a, b publish.PullReview) int {
		return cmp.Or(a.SubmittedAt.Compare(b.SubmittedAt), cmp.Compare(a.ID, b.ID))
	})
	for _, rv := range reviews {
		if rv.State == externalReviewApprovedState || rv.State == externalReviewDismissedState ||
			strings.TrimSpace(rv.Body) == "" {
			continue
		}
		add("review", rv.ID, domain.ExternalFindingInput{
			ReviewerAccountID: rv.AuthorID, ReviewerLogin: rv.AuthorLogin,
			ThreadID: externalReviewThreadID(rv.ID), HeadSHA: rv.CommitID,
			RawText: rv.Body, CreatedAt: rv.SubmittedAt.UTC(),
		})
	}

	comments := slices.Clone(obs.Comments)
	slices.SortFunc(comments, func(a, b publish.PullReviewComment) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	for _, c := range comments {
		if strings.TrimSpace(c.Body) == "" {
			continue
		}
		root := c.ID
		if c.InReplyToID != 0 {
			root = c.InReplyToID
		}
		// A range that starts on the other side of the diff carries a start
		// line from the other file version, which can be past the end line.
		// The finding then names the comment's own line alone.
		startLine := c.OriginalStartLine
		if startLine > c.OriginalLine {
			startLine = 0
		}
		add("review comment", c.ID, domain.ExternalFindingInput{
			ReviewerAccountID: c.AuthorID, ReviewerLogin: c.AuthorLogin,
			ThreadID: externalReviewCommentThreadID(root), HeadSHA: c.OriginalCommitID,
			Location: reviewCommentLocation(c.Path, startLine, c.OriginalLine),
			RawText:  c.Body, CreatedAt: c.CreatedAt.UTC(),
		})
	}
	return findings, skipped
}

// commitExternalFindings stores the pass's external findings in their own
// transaction, so a failure here never blocks the pull and issue facts. Only
// findings with no stored row are written. That makes a later pass over the
// same activity a no-op without advancing the client-visible revision, and it
// is also how a repeat converges: a stored row with the same ID and other
// bytes is the reviewer's identical text in the same thread on the same head,
// seen earlier with another timestamp, and it stays as first stored (devlog
// 2026-10-02-1249).
func (r activeResourceReconciler) commitExternalFindings(
	ctx context.Context, observation activeResourceObservation,
) error {
	var fresh []domain.Finding
	if err := r.store.Read(ctx, func(tx *store.ReadTx) error {
		for _, finding := range observation.externalFindings {
			_, err := tx.GetFinding(ctx, finding.ID)
			switch {
			case errors.Is(err, store.ErrNotFound):
				fresh = append(fresh, finding)
			case err != nil:
				return err
			}
		}
		return nil
	}); err != nil || len(fresh) == 0 {
		return err
	}
	return r.store.Write(ctx, func(tx *store.WriteTx) error {
		// The findings were fetched for the pull request this binding named;
		// storing them under the run is sound only while it still does.
		binding, err := tx.GetReadyItemPRBinding(ctx, observation.itemID)
		if err != nil {
			return err
		}
		if binding != observation.binding {
			return fmt.Errorf("ready resource binding changed during reconciliation: %w", store.ErrImmutableConflict)
		}
		for _, finding := range fresh {
			if err := tx.PutExternalFinding(ctx, finding); err != nil {
				return err
			}
		}
		return nil
	})
}
