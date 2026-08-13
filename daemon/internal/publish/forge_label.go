package publish

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Label-initiator intake observation (plan §5.11, §5.12, issue #659): the
// intake reconciliation loop observes the open issues carrying the configured
// initiator label. Like the native-review layer this is pure transport — it
// decodes only the bounded fields intake needs and leaves occurrence,
// admission, and start decisions to the loop that consumes the observation.

// LabelIssue is the bounded observation of one issue returned by the labeled
// open-issue query. It carries the issue number, its lifecycle state, and
// whether the requested label is present — never the title, body, author, or
// any other content, so §5.13's "no event bodies, target identities, or
// authority" holds structurally, not by later filtering.
type LabelIssue struct {
	Number   int
	State    string
	HasLabel bool
}

// labelIssueResponse decodes only the fields above from an issues-list entry.
// The GitHub issues API serves pull requests too, so a labeled PR carrying the
// initiator label would appear here; PullRequest lets the decode identify and
// refuse those entries rather than admit a PR as an intake subject.
type labelIssueResponse struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	PullRequest *struct {
		URL string `json:"url"`
	} `json:"pull_request"`
}

// getLabelIssues observes the open issues carrying label conditionally. Entries
// the issues API serves that are actually pull requests are refused (dropped
// from the observation): a PR carrying the initiator label is not an intake
// subject, and admitting one would let a PR impersonate an issue on the intake
// path. The label is compared case-insensitively because the forge's ?labels=
// filter is case-insensitive; an entry that somehow lacks the requested label is
// reported with HasLabel false rather than silently kept.
//
// The labeled-open set is correctness-critical for intake: a missed later-page
// departure would leave a proposal actionable indefinitely, and a missed
// later-page arrival would stay undiscovered. A first-page ETag only validates a
// single-page list, so when the list spans pages the returned validator is
// dropped, forcing the next poll to re-read every page unconditionally rather
// than trust a first-page 304.
func (f *forge) getLabelIssues(ctx context.Context, repo repoRef, label, etag string) (listRead[LabelIssue], error) {
	if label == "" {
		return listRead[LabelIssue]{}, errors.New("get label issues: empty label")
	}
	path := fmt.Sprintf("/repos/%s/issues?labels=%s&state=open&per_page=100",
		repo.path(), url.QueryEscape(label))
	raw, resultETag, notModified, multiPage, err := fetchConditionalList[labelIssueResponse](ctx, f, repo, path, etag)
	if err != nil {
		return listRead[LabelIssue]{}, fmt.Errorf("get label issues: %w", err)
	}
	if notModified {
		return listRead[LabelIssue]{NotModified: true, ETag: etag}, nil
	}
	issues := make([]LabelIssue, 0, len(raw))
	for _, item := range raw {
		if item.PullRequest != nil {
			continue
		}
		if item.Number <= 0 {
			return listRead[LabelIssue]{}, errors.New("get label issues: entry carries no issue number")
		}
		if item.State == "" {
			return listRead[LabelIssue]{}, errors.New("get label issues: entry carries no state")
		}
		hasLabel := false
		for _, l := range item.Labels {
			if strings.EqualFold(l.Name, label) {
				hasLabel = true
				break
			}
		}
		issues = append(issues, LabelIssue{Number: item.Number, State: item.State, HasLabel: hasLabel})
	}
	if multiPage {
		// Drop the first-page validator so the next poll re-reads every page: a
		// first-page 304 cannot prove a multi-page labeled set unchanged.
		resultETag = ""
	}
	return listRead[LabelIssue]{Items: issues, ETag: resultETag}, nil
}
