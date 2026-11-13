package publish

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publicationtext"
)

// ExternalReviewThreadID names a review body. GitHub has no threaded reply
// endpoint for it; responses are conversation comments linking that review.
func ExternalReviewThreadID(reviewID int64) string { return fmt.Sprintf("review/%d", reviewID) }

// ExternalReviewCommentThreadID names the first comment of an inline thread.
func ExternalReviewCommentThreadID(rootCommentID int64) string {
	return fmt.Sprintf("review_comment/%d", rootCommentID)
}

// ExternalReviewConversationThreadID names one comment in a pull request's
// main conversation (#3127). Like a review body it has no threaded reply
// endpoint; responses are conversation comments linking it.
func ExternalReviewConversationThreadID(commentID int64) string {
	return fmt.Sprintf("issue_comment/%d", commentID)
}

// IsExternalReviewConversationThread reports whether thread names a
// conversation comment. Such a thread's first stored finding in a run is
// final (#3127): the comment has no commit, so an edit or a later head would
// otherwise make it a new finding and start another round.
func IsExternalReviewConversationThread(thread string) bool {
	return domain.IsExternalConversationThread(thread)
}

// externalReplyThread holds exactly one of a review body, an inline thread's
// root comment, or a conversation comment. Only an inline thread has a root:
// the other two reply in the conversation.
type externalReplyThread struct{ review, root, comment int64 }

func parseExternalReplyThread(thread string) (externalReplyThread, error) {
	kind, value, ok := strings.Cut(thread, "/")
	id, err := strconv.ParseInt(value, 10, 64)
	if !ok || err != nil || id <= 0 || strconv.FormatInt(id, 10) != value {
		return externalReplyThread{}, errors.New("review reply: invalid thread")
	}
	switch kind {
	case "review":
		return externalReplyThread{review: id}, nil
	case "review_comment":
		return externalReplyThread{root: id}, nil
	case "issue_comment":
		return externalReplyThread{comment: id}, nil
	}
	return externalReplyThread{}, errors.New("review reply: invalid thread")
}

const externalReplyReasonLimit = 8192

// externalReviewReplyText writes a reply in the target repository's terms
// (#2349). A declined or deferred outcome needs the adjudication entry that
// decided it; adjudicatedReplyText says why.
func externalReviewReplyText(d domain.ExternalFindingDisposition, entry *domain.FindingAdjudicationEntry, thread externalReplyThread, binding domain.ReadyItemPRBinding, ruleset domain.IssueTextRuleset) (string, error) {
	// Even a fixed outcome is screened under its recorded ruleset. Its reason
	// is not printed because it can name an earlier, unpublished head.
	if err := publicationtext.ScreenIssueBody(ruleset, d.Reason, externalReplyReasonLimit); err != nil {
		return "", err
	}
	var body string
	var err error
	switch d.Disposition {
	case domain.ReviewDispositionDeclined:
		body, err = adjudicatedReplyText("declined", d.FindingID, entry, ruleset)
	case domain.ReviewDispositionDeferred:
		body, err = adjudicatedReplyText("deferred", d.FindingID, entry, ruleset)
	case domain.ReviewDispositionFixed:
		body = "Freeside recorded this finding as fixed after reviewing a remediated head.\n\nPublished commit: " + dispositionCode(binding.HeadSHA) + ".\n\nFreeside has not proven the finding fixed.\n"
	}
	if err != nil {
		return "", err
	}
	if body == "" {
		return "", errors.New("review reply: unsupported disposition")
	}
	// A root-relative link keeps the source on this forge's web host,
	// including Enterprise, without guessing from an API hostname.
	switch {
	case thread.review > 0:
		body = fmt.Sprintf("In response to [review %d](/%s/pull/%d#pullrequestreview-%d):\n\n", thread.review, binding.Repo, binding.PRNumber, thread.review) + body
	case thread.comment > 0:
		body = fmt.Sprintf("In response to [comment %d](/%s/pull/%d#issuecomment-%d):\n\n", thread.comment, binding.Repo, binding.PRNumber, thread.comment) + body
	}
	return body, nil
}

// adjudicatedReplyText words a declined or deferred outcome from the entry
// that decided it. A person's decision names a maintainer and prints the
// reason the person gave (#2352); a decision made before a reason could be
// given carries a placeholder rationale, so its reply prints no reason. Any
// other producer's rationale is printed alone, without the internal
// adjudication digest the disposition's Reason appends. Every printed reason
// is screened because it is the text the reply posts.
func adjudicatedReplyText(verb string, id domain.FindingID, entry *domain.FindingAdjudicationEntry, ruleset domain.IssueTextRuleset) (string, error) {
	if entry == nil || entry.FindingID != id {
		return "", errors.New("review reply: adjudication entry missing")
	}
	if entry.Producer == domain.AdjudicationProducerHuman {
		reason := entry.HumanReason()
		if reason == "" {
			return "A maintainer " + verb + " this finding.\n", nil
		}
		if err := publicationtext.ScreenIssueBody(ruleset, reason, externalReplyReasonLimit); err != nil {
			return "", err
		}
		return "A maintainer " + verb + " this finding.\n\nReason: " + reason + "\n", nil
	}
	reason := strings.TrimSpace(entry.Rationale)
	if err := publicationtext.ScreenIssueBody(ruleset, reason, externalReplyReasonLimit); err != nil {
		return "", err
	}
	return "Freeside " + verb + " this finding.\n\nReason: " + boundedClaim(reason, externalReplyReasonLimit) + "\n", nil
}
