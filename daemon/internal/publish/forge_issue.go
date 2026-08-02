package publish

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Issue observation endpoints (plan §5.18, issue #443): the capture hooks'
// issue-closure criterion needs the issue's lifecycle state and the commit
// its latest `closed` event attributes the closure to — the explicit
// closed-by link, never "closed while a merge happened to land".

// issueState is the decoded observation of one issue.
type issueState struct {
	Number int
	State  string
}

// issueRead is a conditional issue observation; see prRead.
type issueRead struct {
	Issue       issueState
	ETag        string
	NotModified bool
}

// getIssue observes one issue. A 304 against etag reports NotModified with
// no other fields; a missing issue is an APIError. A number that names a
// pull request is refused: the issues API serves both, and a bound issue
// that is secretly a PR would let one resource impersonate the other.
func (f *forge) getIssue(ctx context.Context, repo repoRef, number int, etag string) (issueRead, error) {
	path := fmt.Sprintf("/repos/%s/issues/%d", repo.path(), number)
	resp, err := f.do(ctx, http.MethodGet, repo, path, etag, nil)
	if err != nil {
		return issueRead{}, fmt.Errorf("get issue: %w", err)
	}
	defer drainAndClose(resp.Body)
	switch resp.StatusCode {
	case http.StatusOK:
		var decoded struct {
			Number      int    `json:"number"`
			State       string `json:"state"`
			PullRequest *struct {
				URL string `json:"url"`
			} `json:"pull_request"`
		}
		if err := decodeResponse(resp.Body, &decoded); err != nil {
			return issueRead{}, fmt.Errorf("get issue: decode response: %w", err)
		}
		if decoded.Number != number {
			// Returned-object boundary: an observation of some other issue
			// must not update this resource's state.
			return issueRead{}, errors.New("get issue: response names a different issue number")
		}
		if decoded.PullRequest != nil {
			return issueRead{}, errors.New("get issue: response names a pull request")
		}
		if decoded.State == "" {
			return issueRead{}, errors.New("get issue: response carries no state")
		}
		return issueRead{
			Issue: issueState{Number: decoded.Number, State: decoded.State},
			ETag:  resp.Header.Get("ETag"),
		}, nil
	case http.StatusNotModified:
		return issueRead{NotModified: true, ETag: etag}, nil
	}
	return issueRead{}, fmt.Errorf("get issue: %w", &APIError{Status: resp.StatusCode, RequestPath: path})
}

// issueClosingCommit walks the issue's event list and returns the commit id
// of the latest `closed` event, or "" when the latest closure carries no
// commit attribution (a manual close). The walk is bounded: an issue whose
// event history exceeds the bound fails the observation rather than
// answering from a partial read.
func (f *forge) issueClosingCommit(ctx context.Context, repo repoRef, number int) (string, error) {
	path := fmt.Sprintf("/repos/%s/issues/%d/events?per_page=100", repo.path(), number)
	const maxPages = 10
	commit := ""
	for page := 0; ; page++ {
		if page == maxPages {
			return "", fmt.Errorf("issue events: history exceeds the %d-page read bound", maxPages)
		}
		resp, err := f.do(ctx, http.MethodGet, repo, path, "", nil)
		if err != nil {
			return "", fmt.Errorf("issue events: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			drainAndClose(resp.Body)
			return "", fmt.Errorf("issue events: %w", &APIError{Status: resp.StatusCode, RequestPath: path})
		}
		var decoded []struct {
			Event    string  `json:"event"`
			CommitID *string `json:"commit_id"`
		}
		err = decodeResponse(resp.Body, &decoded)
		next := nextPageLink(resp.Header.Get("Link"))
		drainAndClose(resp.Body)
		if err != nil {
			return "", fmt.Errorf("issue events: decode response: %w", err)
		}
		// JSON null decodes into a nil slice without error; an empty first
		// page is legitimate (a never-closed issue), null is not a list.
		if decoded == nil {
			return "", errors.New("issue events: response is not a list")
		}
		for _, ev := range decoded {
			if ev.Event != "closed" {
				continue
			}
			if ev.CommitID != nil {
				commit = *ev.CommitID
			} else {
				commit = ""
			}
		}
		if next == "" {
			return commit, nil
		}
		// The Link target is absolute; only this API root is followable, so
		// a redirect off-host cannot carry the authenticated request away.
		// The separator is part of the required prefix: a bare-prefix check
		// would accept a host that merely starts with the root's name.
		if !strings.HasPrefix(next, f.baseURL+"/") {
			return "", errors.New("issue events: next page is outside the API root")
		}
		path = strings.TrimPrefix(next, f.baseURL)
	}
}

// nextPageLink extracts the rel="next" target from a Link header, or ""
// when there is no next page.
func nextPageLink(header string) string {
	for _, part := range strings.Split(header, ",") {
		segments := strings.Split(part, ";")
		if len(segments) < 2 {
			continue
		}
		target := strings.Trim(strings.TrimSpace(segments[0]), "<>")
		for _, p := range segments[1:] {
			if strings.TrimSpace(p) == `rel="next"` {
				return target
			}
		}
	}
	return ""
}
