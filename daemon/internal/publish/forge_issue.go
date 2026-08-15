package publish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// Issue observation endpoints (plan §5.18, issue #443): the capture hooks'
// issue-closure criterion needs the issue's lifecycle state and the commit
// its latest `closed` event attributes the closure to, directly or through
// the closing pull request — the explicit closed-by link, never "closed
// while a merge happened to land".

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
// of the latest `closed` event, or "" when REST carries no direct commit
// attribution. The walk is bounded: an issue whose event history exceeds the
// bound fails the observation rather than answering from a partial read.
func (f *forge) issueClosingCommit(ctx context.Context, repo repoRef, number int) (commit, nodeID string, err error) {
	path := fmt.Sprintf("/repos/%s/issues/%d/events?per_page=100", repo.path(), number)
	const maxPages = 10
	for page := 0; ; page++ {
		if page == maxPages {
			return "", "", fmt.Errorf("issue events: history exceeds the %d-page read bound", maxPages)
		}
		resp, err := f.do(ctx, http.MethodGet, repo, path, "", nil)
		if err != nil {
			return "", "", fmt.Errorf("issue events: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			drainAndClose(resp.Body)
			return "", "", fmt.Errorf("issue events: %w", &APIError{Status: resp.StatusCode, RequestPath: path})
		}
		var decoded []struct {
			Event    string  `json:"event"`
			CommitID *string `json:"commit_id"`
			NodeID   string  `json:"node_id"`
		}
		err = decodeResponse(resp.Body, &decoded)
		next := nextPageLink(resp.Header.Get("Link"))
		drainAndClose(resp.Body)
		if err != nil {
			return "", "", fmt.Errorf("issue events: decode response: %w", err)
		}
		// JSON null decodes into a nil slice without error; an empty first
		// page is legitimate (a never-closed issue), null is not a list.
		if decoded == nil {
			return "", "", errors.New("issue events: response is not a list")
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
			nodeID = ev.NodeID
		}
		if next == "" {
			return commit, nodeID, nil
		}
		// The Link target is absolute; only this API root is followable, so
		// a redirect off-host cannot carry the authenticated request away.
		// The separator is part of the required prefix: a bare-prefix check
		// would accept a host that merely starts with the root's name.
		if !strings.HasPrefix(next, f.baseURL+"/") {
			return "", "", errors.New("issue events: next page is outside the API root")
		}
		path = strings.TrimPrefix(next, f.baseURL)
	}
}

// issueClosureAttribution resolves a latest closure that REST did not
// attribute directly. GitHub's GraphQL timeline retains the closer for a
// PR-body keyword closure, so a merged pull request maps to its merge commit;
// a null closer is the definitive manual-close result. Every other incomplete
// or unexpected returned shape fails closed because this SHA feeds the §5.18
// completion gate.
func (f *forge) issueClosureAttribution(ctx context.Context, repo repoRef, number int, eventNodeID string) (string, error) {
	if eventNodeID == "" {
		return "", errors.New("issue closure attribution: REST closed event carries no node id")
	}
	const path = "/graphql"
	const query = `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    issue(number: $number) {
      number
      timelineItems(last: 1, itemTypes: [CLOSED_EVENT]) {
        nodes { __typename ... on ClosedEvent { id closer {
          __typename
          ... on Commit { oid }
          ... on PullRequest { number merged mergeCommit { oid } }
        } } }
      }
    }
  }
}`
	body := struct {
		Query     string `json:"query"`
		Variables struct {
			Owner  string `json:"owner"`
			Name   string `json:"name"`
			Number int    `json:"number"`
		} `json:"variables"`
	}{Query: query}
	body.Variables.Owner = repo.owner
	body.Variables.Name = repo.name
	body.Variables.Number = number

	resp, err := f.do(ctx, http.MethodPost, repo, path, "", body)
	if err != nil {
		return "", fmt.Errorf("issue closure attribution: request: %w", err)
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("issue closure attribution: %w", &APIError{Status: resp.StatusCode, RequestPath: path})
	}

	type closer struct {
		TypeName    string `json:"__typename"`
		OID         string `json:"oid"`
		Number      int    `json:"number"`
		Merged      bool   `json:"merged"`
		MergeCommit *struct {
			OID string `json:"oid"`
		} `json:"mergeCommit"`
	}
	type node struct {
		TypeName string          `json:"__typename"`
		ID       string          `json:"id"`
		Closer   json.RawMessage `json:"closer"`
	}
	var decoded struct {
		Errors []struct{} `json:"errors"`
		Data   struct {
			Repository *struct {
				Issue *struct {
					Number        int `json:"number"`
					TimelineItems struct {
						Nodes []*node `json:"nodes"`
					} `json:"timelineItems"`
				} `json:"issue"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := decodeResponse(resp.Body, &decoded); err != nil {
		return "", fmt.Errorf("issue closure attribution: decode response: %w", err)
	}
	if len(decoded.Errors) != 0 {
		return "", errors.New("issue closure attribution: GraphQL response carries errors")
	}
	if decoded.Data.Repository == nil {
		return "", errors.New("issue closure attribution: response carries no repository")
	}
	issue := decoded.Data.Repository.Issue
	if issue == nil {
		return "", errors.New("issue closure attribution: response carries no issue")
	}
	if issue.Number != number {
		return "", errors.New("issue closure attribution: response names a different issue number")
	}
	if len(issue.TimelineItems.Nodes) != 1 || issue.TimelineItems.Nodes[0] == nil {
		return "", errors.New("issue closure attribution: response does not carry exactly one closed event")
	}
	event := issue.TimelineItems.Nodes[0]
	if event.TypeName != "ClosedEvent" {
		return "", errors.New("issue closure attribution: response carries an unexpected event type")
	}
	if event.ID != eventNodeID {
		return "", errors.New("issue closure attribution: response names a different closed event")
	}
	if len(event.Closer) == 0 {
		return "", errors.New("issue closure attribution: closed event carries no closer field")
	}
	if strings.TrimSpace(string(event.Closer)) == "null" {
		return "", nil
	}
	var attribution closer
	if err := strictjson.DecodeAllowingUnknownFields(
		event.Closer, &attribution, strictjson.TolerateInvalidUTF8, strictjson.NoLimit,
	); err != nil {
		return "", errors.New("issue closure attribution: closer is malformed")
	}
	switch attribution.TypeName {
	case "Commit":
		if attribution.OID == "" {
			return "", errors.New("issue closure attribution: commit closer carries no oid")
		}
		return attribution.OID, nil
	case "PullRequest":
		if attribution.Number <= 0 {
			return "", errors.New("issue closure attribution: pull request closer carries no number")
		}
		if !attribution.Merged {
			return "", errors.New("issue closure attribution: pull request closer is not merged")
		}
		if attribution.MergeCommit == nil || attribution.MergeCommit.OID == "" {
			return "", errors.New("issue closure attribution: pull request closer carries no merge commit")
		}
		return attribution.MergeCommit.OID, nil
	}
	return "", errors.New("issue closure attribution: response carries an unexpected closer type")
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
