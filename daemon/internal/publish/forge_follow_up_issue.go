package publish

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Follow-up filing endpoints (plan §5.17, issue #1634): create one issue,
// list the issues an account created, and resolve a milestone title. Every
// returned field is untrusted. Issue text is never decoded here, so it can
// neither match a candidate nor reach an error.

// errMilestoneUnresolved reports that a milestone title names no single
// milestone in the repository. It is a definite precondition failure: no
// retry changes it.
var errMilestoneUnresolved = errors.New("milestone title does not resolve to one milestone")

// filedIssue is the decoded identity of one issue: its number, the account
// that authored it, and when the forge says it was created.
type filedIssue struct {
	Number      int
	AuthorID    int64
	AuthorType  string
	CreatedAt   time.Time
	PullRequest bool
}

// filedIssueWire is the response shape shared by the create response and
// each list element. Pointers distinguish an absent field from a zero one,
// so an element that cannot be judged fails the read instead of being
// dropped from a candidate count.
type filedIssueWire struct {
	Number *int `json:"number"`
	User   *struct {
		ID   *int64 `json:"id"`
		Type string `json:"type"`
	} `json:"user"`
	CreatedAt   *time.Time `json:"created_at"`
	PullRequest *struct {
		URL string `json:"url"`
	} `json:"pull_request"`
}

func (w filedIssueWire) issue() (filedIssue, error) {
	if w.Number == nil || *w.Number <= 0 {
		return filedIssue{}, errors.New("issue carries no positive number")
	}
	if w.User == nil || w.User.ID == nil || *w.User.ID <= 0 || w.User.Type == "" {
		return filedIssue{}, errors.New("issue carries no author identity")
	}
	if w.CreatedAt == nil || w.CreatedAt.IsZero() {
		return filedIssue{}, errors.New("issue carries no creation time")
	}
	return filedIssue{
		Number: *w.Number, AuthorID: *w.User.ID, AuthorType: w.User.Type,
		CreatedAt: w.CreatedAt.UTC(), PullRequest: w.PullRequest != nil,
	}, nil
}

// issueCreateResult is what one create request observed. Issue is set only
// for a 201 whose body decoded to an issue; the classifier reads nothing
// else.
type issueCreateResult struct {
	Status int
	Header http.Header
	Issue  *filedIssue
}

// newCreateIssueRequest builds the create request without sending it, so the
// caller can record its dispatch-started marker with nothing fallible left
// but the send. A nil milestone and an empty label list are omitted.
func (f *forge) newCreateIssueRequest(
	ctx context.Context, repo repoRef, title, body string, labels []string, milestone *int,
) (*http.Request, error) {
	payload := struct {
		Title     string   `json:"title"`
		Body      string   `json:"body"`
		Labels    []string `json:"labels,omitempty"`
		Milestone *int     `json:"milestone,omitempty"`
	}{Title: title, Body: body, Labels: labels, Milestone: milestone}
	req, err := f.newRequest(ctx, http.MethodPost, repo, "/repos/"+repo.path()+"/issues", "", payload)
	if err != nil {
		return nil, fmt.Errorf("create issue: %w", err)
	}
	return req, nil
}

// sendCreateIssue sends a built create request. The error is a transport
// failure only: every response, whatever its status, is returned for
// classification. A 201 whose body does not decode to an issue returns a nil
// Issue, never an error, because the create may still have committed.
func (f *forge) sendCreateIssue(req *http.Request) (issueCreateResult, error) {
	resp, err := f.client.Do(req)
	if err != nil {
		return issueCreateResult{}, fmt.Errorf("create issue: %w", err)
	}
	defer drainAndClose(resp.Body)
	result := issueCreateResult{Status: resp.StatusCode, Header: resp.Header}
	if resp.StatusCode != http.StatusCreated {
		return result, nil
	}
	var decoded filedIssueWire
	if err := decodeResponse(resp.Body, &decoded); err != nil {
		return result, nil
	}
	issue, err := decoded.issue()
	if err != nil {
		return result, nil
	}
	result.Issue = &issue
	return result, nil
}

// listIssuesCreatedBy lists every issue and pull request the account created
// that changed at or after since, in any state. The creator filter takes a
// login and since compares the update time, so the caller still judges each
// element by numeric author identity and creation time. Any failure,
// including an element that cannot be judged, fails the whole read: a partial
// listing would undercount candidates.
func (f *forge) listIssuesCreatedBy(
	ctx context.Context, repo repoRef, login string, since time.Time,
) ([]filedIssue, error) {
	query := url.Values{
		"state":    {"all"},
		"creator":  {login},
		"since":    {since.UTC().Format(time.RFC3339)},
		"per_page": {"100"},
	}
	path := "/repos/" + repo.path() + "/issues?" + query.Encode()
	wire, _, _, _, err := fetchConditionalList[filedIssueWire](ctx, f, repo, path, "")
	if err != nil {
		return nil, fmt.Errorf("list issues: %w", err)
	}
	issues := make([]filedIssue, 0, len(wire))
	for _, element := range wire {
		issue, err := element.issue()
		if err != nil {
			return nil, fmt.Errorf("list issues: %w", err)
		}
		issues = append(issues, issue)
	}
	return issues, nil
}

// getMilestoneNumber resolves a milestone title to its number by exact
// match over the repository's milestones, open and closed. A title that
// matches none, or more than one, is errMilestoneUnresolved.
func (f *forge) getMilestoneNumber(ctx context.Context, repo repoRef, title string) (int, error) {
	path := "/repos/" + repo.path() + "/milestones?state=all&per_page=100"
	milestones, _, _, _, err := fetchConditionalList[struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
	}](ctx, f, repo, path, "")
	if err != nil {
		return 0, fmt.Errorf("list milestones: %w", err)
	}
	number, matches := 0, 0
	for _, milestone := range milestones {
		if milestone.Title == title {
			number = milestone.Number
			matches++
		}
	}
	if matches != 1 || number <= 0 {
		return 0, fmt.Errorf("list milestones: %w", errMilestoneUnresolved)
	}
	return number, nil
}
