package daemoncompose

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

// appBotIdentitySource resolves the App bot a repository's replies post as.
type appBotIdentitySource interface {
	Resolve(ctx context.Context, repo string) (publish.AppBotIdentity, error)
}

// conversationBotUserID resolves the App bot's account ID when the activity
// holds a conversation comment to tell apart from the bot's replies. It
// returns 0 when there is none or no identity source is wired, which keeps
// every conversation comment out of the findings (buildExternalFindings).
func (r ActiveResourceReconciler) conversationBotUserID(
	ctx context.Context, repo string, obs publish.PullReviewObservation,
) (int64, error) {
	if len(obs.ConversationComments) == 0 || r.appBotIdentity == nil {
		return 0, nil
	}
	identity, err := r.appBotIdentity.Resolve(ctx, repo)
	if err != nil {
		return 0, fmt.Errorf("resolve App bot identity: %w", err)
	}
	if identity.BotUserID <= 0 {
		return 0, errors.New("resolve App bot identity: no bot account")
	}
	return identity.BotUserID, nil
}

// buildExternalFindings normalizes one raw observation of a published pull
// request's review activity into external findings (plan §5.19, §7; issue
// #524): one for each submitted review that has a body and is neither an
// approval nor dismissed, one for each inline comment with a body, and one
// for each conversation comment with a body that the App bot did not post
// (#3127). The caller must have fetched obs for binding's own pull request: a
// finding names no repository or pull request, so the run is its only tie to
// one.
//
// botUserID is the App bot's forge account ID. The bot's conversation
// comments are its own disposition replies, never review. With no known bot
// (botUserID <= 0) no conversation comment becomes a finding, so a reply can
// never be read back as one.
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
// finding on every later head. A conversation comment has no commit, so it
// binds to conversationHead, the head this pass observed: the ready item's
// own head while it is bound, and the head a hand-back adopts after
// readiness ended (issue #3160). commitExternalFindings keeps it from
// becoming a new finding under a later head. The timestamp is the
// forge's, so a later pass over the same activity rebuilds the same bytes.
//
// Activity that cannot be a valid finding is reported in skipped and stored
// nowhere: text is never truncated to fit, since the finding's digest names
// the reviewer's exact words.
func buildExternalFindings(
	obs publish.PullReviewObservation, binding domain.ReadyItemPRBinding, conversationHead string, botUserID int64,
) (findings []domain.Finding, skipped []error) {
	seen := map[domain.FindingID]bool{}
	add := func(what string, id int64, in domain.ExternalFindingInput) bool {
		in.RunID = binding.RunID
		in.Forge = domain.ExternalReviewForgeGitHub
		in.Severity = domain.FindingSeverity(nativeReviewBadge(in.RawText))
		in.Message = in.RawText
		finding, err := domain.NewExternalFinding(in)
		if err != nil {
			skipped = append(skipped, fmt.Errorf("skip %s %d on %s#%d: %w",
				what, id, binding.Repo, binding.PRNumber, err))
			return false
		}
		// One reviewer repeating identical text in one thread on one head has
		// one ID. The first by forge time is the finding; a repeat says
		// nothing new.
		if seen[finding.ID] {
			return true
		}
		seen[finding.ID] = true
		findings = append(findings, finding)
		return true
	}
	commentedReviews := map[int64]bool{}
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
		if add("review comment", c.ID, domain.ExternalFindingInput{
			ReviewerAccountID: c.AuthorID, ReviewerLogin: c.AuthorLogin,
			ThreadID: publish.ExternalReviewCommentThreadID(root), HeadSHA: c.OriginalCommitID,
			Location: reviewCommentLocation(c.Path, startLine, c.OriginalLine),
			RawText:  c.Body, CreatedAt: c.CreatedAt.UTC(),
		}) {
			commentedReviews[c.ReviewID] = true
		}
	}
	// A review's body summarizes its usable inline findings. If none could
	// be stored, retain the body so the reviewer's only usable text survives.
	reviews := slices.Clone(obs.Reviews)
	slices.SortFunc(reviews, func(a, b publish.PullReview) int {
		return cmp.Or(a.SubmittedAt.Compare(b.SubmittedAt), cmp.Compare(a.ID, b.ID))
	})
	for _, rv := range reviews {
		if commentedReviews[rv.ID] || rv.State == externalReviewApprovedState || rv.State == externalReviewDismissedState ||
			strings.TrimSpace(rv.Body) == "" {
			continue
		}
		add("review", rv.ID, domain.ExternalFindingInput{
			ReviewerAccountID: rv.AuthorID, ReviewerLogin: rv.AuthorLogin,
			ThreadID: publish.ExternalReviewThreadID(rv.ID), HeadSHA: rv.CommitID,
			RawText: rv.Body, CreatedAt: rv.SubmittedAt.UTC(),
		})
	}
	if botUserID <= 0 {
		return findings, skipped
	}
	conversation := slices.Clone(obs.ConversationComments)
	slices.SortFunc(conversation, func(a, b publish.PullConversationComment) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	for _, c := range conversation {
		if c.AuthorID == botUserID || strings.TrimSpace(c.Body) == "" {
			continue
		}
		add("conversation comment", c.ID, domain.ExternalFindingInput{
			ReviewerAccountID: c.AuthorID, ReviewerLogin: c.AuthorLogin,
			ThreadID: publish.ExternalReviewConversationThreadID(c.ID), HeadSHA: conversationHead,
			RawText: c.Body, CreatedAt: c.CreatedAt.UTC(),
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
//
// A conversation comment is final at its first stored finding in the run
// (#3127): a finding for a conversation thread the run already has is not
// written, so neither an edit nor a read under a later head stores a second
// finding or starts a second round. Checking it before the write suffices:
// the reconciler's passes run one at a time and this is the only writer of
// external findings.
func (r ActiveResourceReconciler) commitExternalFindings(
	ctx context.Context, observation activeResourceObservation,
) error {
	var fresh []domain.Finding
	if err := r.Store.Read(ctx, func(tx *store.ReadTx) error {
		final, err := storedConversationThreads(ctx, tx, observation.binding.RunID)
		if err != nil {
			return err
		}
		for _, finding := range observation.externalFindings {
			if final[finding.External.ThreadID] {
				continue
			}
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
	return r.Store.Write(ctx, func(tx *store.WriteTx) error {
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

// storedConversationThreads returns the conversation threads the run already
// has a finding for, each of which is final (commitExternalFindings).
func storedConversationThreads(ctx context.Context, tx *store.ReadTx, runID domain.RunID) (map[string]bool, error) {
	stored, err := tx.ListExternalFindings(ctx, runID)
	if err != nil {
		return nil, err
	}
	final := map[string]bool{}
	for _, finding := range stored {
		if publish.IsExternalReviewConversationThread(finding.External.ThreadID) {
			final[finding.External.ThreadID] = true
		}
	}
	return final, nil
}
