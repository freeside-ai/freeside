package publish

import (
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

// TestExternalReviewReplyTextGolden pins each reply form. A model's decline
// or defer prints the entry's rationale without the disposition reason's
// adjudication digest; a person's prints neither (#2349).
func TestExternalReviewReplyTextGolden(t *testing.T) {
	const rationale = "The <input> is already checked by `Validate`."
	producers := map[string]domain.AdjudicationProducer{"": domain.AdjudicationProducerModel, "human-": domain.AdjudicationProducerHuman}
	for _, form := range []string{ExternalReviewThreadID(42), ExternalReviewCommentThreadID(43)} {
		for prefix, producer := range producers {
			for _, disposition := range []domain.ReviewDisposition{domain.ReviewDispositionDeclined, domain.ReviewDispositionDeferred, domain.ReviewDispositionFixed} {
				if producer == domain.AdjudicationProducerHuman && disposition == domain.ReviewDispositionFixed {
					continue
				}
				thread, err := parseExternalReplyThread(form)
				if err != nil {
					t.Fatal(err)
				}
				d := domain.ExternalFindingDisposition{FindingID: "finding-1", Disposition: disposition, Reason: rationale + " (finding adjudication sha256:3f7b)"}
				var entry *domain.FindingAdjudicationEntry
				if disposition != domain.ReviewDispositionFixed {
					entry = &domain.FindingAdjudicationEntry{FindingID: d.FindingID, Producer: producer, Rationale: rationale}
					if producer == domain.AdjudicationProducerHuman {
						entry.Rationale = "Human decision recorded."
					}
				}
				binding := domain.ReadyItemPRBinding{Repo: "owner/repo", PRNumber: 7, HeadSHA: strings.Repeat("a", 40)}
				body, err := externalReviewReplyText(d, entry, thread, binding, domain.IssueTextRulesetGitHubIssue1)
				if err != nil {
					t.Fatal(err)
				}
				for _, internal := range []string{"finding adjudication", "sha256:", "Human decision recorded"} {
					if strings.Contains(body, internal) {
						t.Errorf("%s %s%s reply prints %q:\n%s", form, prefix, disposition, internal, body)
					}
				}
				golden.Assert(t, "external-reply-"+strings.Split(form, "/")[0]+"-"+prefix+string(disposition), []byte(body))
			}
		}
	}
}

func TestExternalReviewReplyTextRefusesUnsafeReasonsAndThreads(t *testing.T) {
	for _, reason := range []string{"@codex review", "Closes #42", "/run deploy", "<!-- freeside:test -->", strings.Repeat("x", externalReplyReasonLimit+1)} {
		_, err := externalReviewReplyText(domain.ExternalFindingDisposition{Disposition: domain.ReviewDispositionDeclined, Reason: reason}, nil, externalReplyThread{root: 42}, domain.ReadyItemPRBinding{}, domain.IssueTextRulesetGitHubIssue1)
		if err == nil || strings.Contains(err.Error(), reason) {
			t.Errorf("screen must refuse without echoing reason: %v", err)
		}
	}
	entry := &domain.FindingAdjudicationEntry{FindingID: "finding-1", Producer: domain.AdjudicationProducerModel, Rationale: "@codex review"}
	if _, err := externalReviewReplyText(domain.ExternalFindingDisposition{FindingID: "finding-1", Disposition: domain.ReviewDispositionDeclined, Reason: "Already checked."}, entry, externalReplyThread{root: 42}, domain.ReadyItemPRBinding{}, domain.IssueTextRulesetGitHubIssue1); err == nil {
		t.Error("screen must refuse an unsafe printed rationale")
	}
	for name, entry := range map[string]*domain.FindingAdjudicationEntry{
		"no entry":    nil,
		"other entry": {FindingID: "finding-2", Producer: domain.AdjudicationProducerHuman},
	} {
		d := domain.ExternalFindingDisposition{FindingID: "finding-1", Disposition: domain.ReviewDispositionDeferred, Reason: "Tracked separately."}
		if _, err := externalReviewReplyText(d, entry, externalReplyThread{root: 42}, domain.ReadyItemPRBinding{}, domain.IssueTextRulesetGitHubIssue1); err == nil {
			t.Errorf("%s: a deferral replied without the entry that decided it", name)
		}
	}
	for _, thread := range []string{"", "review/0", "review/-1", "review/01", "review/+1", "review/1/2", "other/1"} {
		if _, err := parseExternalReplyThread(thread); err == nil {
			t.Errorf("accepted %q", thread)
		}
	}
}
