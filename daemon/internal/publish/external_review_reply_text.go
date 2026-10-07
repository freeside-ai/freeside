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

type externalReplyThread struct{ review, root int64 }

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
	}
	return externalReplyThread{}, errors.New("review reply: invalid thread")
}

const externalReplyReasonLimit = 8192

func externalReviewReplyText(d domain.ExternalFindingDisposition, thread externalReplyThread, binding domain.ReadyItemPRBinding, ruleset domain.IssueTextRuleset) (string, error) {
	// Even a fixed outcome is screened under its recorded ruleset. Its reason
	// is not printed because it can name an earlier, unpublished head.
	if err := publicationtext.ScreenIssueBody(ruleset, d.Reason, externalReplyReasonLimit); err != nil {
		return "", err
	}
	var body string
	switch d.Disposition {
	case domain.ReviewDispositionDeclined:
		body = "Freeside declined this finding.\n\nReason: " + boundedClaim(d.Reason, externalReplyReasonLimit) + "\n"
	case domain.ReviewDispositionDeferred:
		body = "Freeside deferred this finding.\n\nReason: " + boundedClaim(d.Reason, externalReplyReasonLimit) + "\n"
	case domain.ReviewDispositionFixed:
		body = "Freeside recorded this finding as fixed after reviewing a remediated head.\n\nPublished commit: " + dispositionCode(binding.HeadSHA) + ".\n\nFreeside has not proven the finding fixed.\n"
	}
	if body == "" {
		return "", errors.New("review reply: unsupported disposition")
	}
	if thread.review > 0 {
		// A root-relative link keeps the review on this forge's web host,
		// including Enterprise, without guessing from an API hostname.
		body = fmt.Sprintf("In response to [review %d](/%s/pull/%d#pullrequestreview-%d):\n\n", thread.review, binding.Repo, binding.PRNumber, thread.review) + body
	}
	return body, nil
}
