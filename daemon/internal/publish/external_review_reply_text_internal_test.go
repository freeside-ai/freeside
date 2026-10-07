package publish

import (
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

func TestExternalReviewReplyTextGolden(t *testing.T) {
	for _, form := range []string{ExternalReviewThreadID(42), ExternalReviewCommentThreadID(43)} {
		for _, disposition := range []domain.ReviewDisposition{domain.ReviewDispositionDeclined, domain.ReviewDispositionDeferred, domain.ReviewDispositionFixed} {
			thread, err := parseExternalReplyThread(form)
			if err != nil {
				t.Fatal(err)
			}
			d := domain.ExternalFindingDisposition{Disposition: disposition, Reason: "The <input> is already checked by `Validate`."}
			binding := domain.ReadyItemPRBinding{Repo: "owner/repo", PRNumber: 7, HeadSHA: strings.Repeat("a", 40)}
			body, err := externalReviewReplyText(d, thread, binding, domain.IssueTextRulesetGitHubIssue1)
			if err != nil {
				t.Fatal(err)
			}
			golden.Assert(t, "external-reply-"+strings.Split(form, "/")[0]+"-"+string(disposition), []byte(body))
		}
	}
}

func TestExternalReviewReplyTextRefusesUnsafeReasonsAndThreads(t *testing.T) {
	for _, reason := range []string{"@codex review", "Closes #42", "/run deploy", "<!-- freeside:test -->", strings.Repeat("x", externalReplyReasonLimit+1)} {
		_, err := externalReviewReplyText(domain.ExternalFindingDisposition{Disposition: domain.ReviewDispositionDeclined, Reason: reason}, externalReplyThread{root: 42}, domain.ReadyItemPRBinding{}, domain.IssueTextRulesetGitHubIssue1)
		if err == nil || strings.Contains(err.Error(), reason) {
			t.Errorf("screen must refuse without echoing reason: %v", err)
		}
	}
	for _, thread := range []string{"", "review/0", "review/-1", "review/01", "review/+1", "review/1/2", "other/1"} {
		if _, err := parseExternalReplyThread(thread); err == nil {
			t.Errorf("accepted %q", thread)
		}
	}
}
