package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// forge is the package's minimal GitHub resource client: exactly the
// endpoints identity-convergent publication needs, hand-rolled on
// net/http like the minter so the package's credential discipline
// holds everywhere — tokens travel only at the request-write site,
// redirects are never followed, and response bodies never enter
// errors (only decoded, typed fields are retained).
type forge struct {
	tokens  TokenSource
	client  *http.Client
	baseURL string
}

func newForge(ts TokenSource, client *http.Client, baseURL string) *forge {
	return &forge{tokens: ts, client: noRedirect(client), baseURL: baseURL}
}

// repoRef is a parsed "owner/name" repository reference. The minter
// grants by name within the installation account; list filters need
// the owner prefix; URL paths need both.
type repoRef struct {
	owner string
	name  string
}

func parseRepo(s string) (repoRef, error) {
	owner, name, ok := strings.Cut(s, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return repoRef{}, fmt.Errorf("repository %q is not owner/name", s)
	}
	return repoRef{owner: owner, name: name}, nil
}

func (r repoRef) path() string { return r.owner + "/" + r.name }

// do issues one authenticated request. etag, when non-empty, rides
// If-None-Match so an unchanged resource answers 304 with no body.
// The caller owns status handling and must drain and close the body.
func (f *forge) do(ctx context.Context, method string, repo repoRef, path, etag string, body any) (*http.Response, error) {
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, f.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	tok, err := f.tokens.Token(ctx, repo.path())
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok.Token.Reveal())
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// refState is the decoded observation of one branch ref.
type refState struct {
	Exists      bool
	SHA         string
	ETag        string
	NotModified bool
}

// getRef observes refs/heads/<branch>. Absence (404) is an
// observation, not an error; a 304 against etag reports NotModified
// with no other fields.
func (f *forge) getRef(ctx context.Context, repo repoRef, branch, etag string) (refState, error) {
	path := "/repos/" + repo.path() + "/git/ref/heads/" + url.PathEscape(branch)
	resp, err := f.do(ctx, http.MethodGet, repo, path, etag, nil)
	if err != nil {
		return refState{}, fmt.Errorf("get ref: %w", err)
	}
	defer drainAndClose(resp.Body)
	switch resp.StatusCode {
	case http.StatusOK:
		var decoded struct {
			Ref    string `json:"ref"`
			Object struct {
				SHA string `json:"sha"`
			} `json:"object"`
		}
		if err := decodeResponse(resp.Body, &decoded); err != nil {
			return refState{}, fmt.Errorf("get ref: decode response: %w", err)
		}
		// Returned-object boundary: the observation must name the ref
		// that was asked for, or a wrong-ref response would attribute
		// some other branch's SHA to this one; and a 200 without a
		// usable SHA cannot drive a convergence decision.
		if decoded.Ref != "refs/heads/"+branch {
			return refState{}, errors.New("get ref: response names a different ref")
		}
		if decoded.Object.SHA == "" {
			return refState{}, errors.New("get ref: response carries no object sha")
		}
		return refState{Exists: true, SHA: decoded.Object.SHA, ETag: resp.Header.Get("ETag")}, nil
	case http.StatusNotModified:
		return refState{NotModified: true, ETag: etag}, nil
	case http.StatusNotFound:
		return refState{}, nil
	}
	return refState{}, fmt.Errorf("get ref: %w", &APIError{Status: resp.StatusCode, RequestPath: path})
}

// createRef creates refs/heads/<branch> at sha and verifies the
// response echoes exactly the requested ref and commit: the create is
// an external effect, so its returned object is verified like any
// other before the publication proceeds on top of it.
func (f *forge) createRef(ctx context.Context, repo repoRef, branch, sha string) error {
	path := "/repos/" + repo.path() + "/git/refs"
	resp, err := f.do(ctx, http.MethodPost, repo, path, "", map[string]string{
		"ref": "refs/heads/" + branch,
		"sha": sha,
	})
	if err != nil {
		return fmt.Errorf("create ref: %w", err)
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("create ref: %w", &APIError{Status: resp.StatusCode, RequestPath: path})
	}
	var decoded struct {
		Ref    string `json:"ref"`
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := decodeResponse(resp.Body, &decoded); err != nil {
		return fmt.Errorf("create ref: decode response: %w", err)
	}
	if decoded.Ref != "refs/heads/"+branch || decoded.Object.SHA != sha {
		return errors.New("create ref: response does not echo the requested ref and commit")
	}
	return nil
}

// prState is the decoded observation of one pull request. Title and
// Body are retained for marker parsing and drift comparison; they are
// returned content and must never enter an error message. The head
// and base coordinates are the complete set of PR fields the
// publication identity binds (head ref/SHA and its repository, base
// ref and its repository); the publisher verifies all of them at its
// decision points, so a PR whose head lives in a fork or that was
// retargeted to another base never converges as ours.
type prState struct {
	Number   int
	State    string
	Title    string
	Body     string
	HeadRef  string
	HeadSHA  string
	HeadRepo string
	BaseRef  string
	BaseRepo string
	// BaseRepoID is the base repository's canonical numeric identity. The
	// capture hooks key completion on it (plan §5.18): a repository name
	// re-bound to a different repository is only detectable through the
	// observed id, never the name the request was addressed by.
	BaseRepoID int64
	// Merged derives from merged_at, which both the single-PR and list
	// responses carry (the single-PR-only `merged` bool would make the two
	// read paths disagree). MergeCommitSHA is retained only when merged: on
	// an unmerged PR the field names a test-merge commit, which is not a
	// fact about the PR (plan §5.18 capture).
	Merged         bool
	MergeCommitSHA string
	// Draft is the observed draft state. recipe v2 holds a closable-source
	// publication as a draft until its closure proposal resolves, because the
	// merge happens on GitHub and no internal signal can stop a direct merge
	// (issue #1419). NodeID is the pull request's GraphQL global id, the input
	// the mark-ready/convert-to-draft mutations bind: REST cannot toggle draft.
	Draft  bool
	NodeID string
}

// prResponse is the wire shape a pull-request read decodes.
type prResponse struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	// Draft is a pointer so an absent or null field is distinguishable from an
	// explicit false: the response is a returned-object trust boundary, and the
	// draft bit drives the hold decision (issue #1419). A partial body must fail
	// closed at each decode site, not read as "not a draft" and report a hold
	// released that GitHub never confirmed, exactly as setPRDraft guards isDraft.
	Draft          *bool   `json:"draft"`
	NodeID         string  `json:"node_id"`
	MergedAt       *string `json:"merged_at"`
	MergeCommitSHA string  `json:"merge_commit_sha"`
	Head           struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref  string `json:"ref"`
		Repo struct {
			ID       int64  `json:"id"`
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
}

// state maps the wire shape to the domain observation. Every caller rejects a
// nil Draft at its decode site before calling state, so the dereference below is
// safe: a response missing the draft bit never reaches a hold decision.
func (r prResponse) state() prState {
	merged := r.MergedAt != nil
	mergeCommit := ""
	if merged {
		mergeCommit = r.MergeCommitSHA
	}
	return prState{
		Number:         r.Number,
		State:          r.State,
		Title:          r.Title,
		Body:           r.Body,
		HeadRef:        r.Head.Ref,
		HeadSHA:        r.Head.SHA,
		HeadRepo:       r.Head.Repo.FullName,
		BaseRef:        r.Base.Ref,
		BaseRepo:       r.Base.Repo.FullName,
		BaseRepoID:     r.Base.Repo.ID,
		Merged:         merged,
		MergeCommitSHA: mergeCommit,
		Draft:          *r.Draft,
		NodeID:         r.NodeID,
	}
}

// listPRsByHead lists pull requests (any state) whose head is
// owner:branch.
func (f *forge) listPRsByHead(ctx context.Context, repo repoRef, branch string) ([]prState, error) {
	path := "/repos/" + repo.path() + "/pulls?head=" + url.QueryEscape(repo.owner+":"+branch) + "&state=all&per_page=100"
	resp, err := f.do(ctx, http.MethodGet, repo, path, "", nil)
	if err != nil {
		return nil, fmt.Errorf("list pulls: %w", err)
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list pulls: %w", &APIError{Status: resp.StatusCode, RequestPath: path})
	}
	var decoded []prResponse
	if err := decodeResponse(resp.Body, &decoded); err != nil {
		return nil, fmt.Errorf("list pulls: decode response: %w", err)
	}
	// JSON null decodes into a nil slice without error; treating it as
	// an authoritative empty list would drive a create. Only an actual
	// list is an observation.
	if decoded == nil {
		return nil, errors.New("list pulls: response is not a list")
	}
	states := make([]prState, 0, len(decoded))
	for _, pr := range decoded {
		// The head filter is a query parameter the server is trusted to
		// have applied; re-check it locally — branch name AND head
		// repository — so a broader-than-asked response cannot smuggle
		// an unrelated PR into convergence. A fork PR whose fork branch
		// shares our name (markers are public and copyable) is skipped:
		// it does not occupy this repository's head ref.
		if pr.Head.Ref != branch || pr.Head.Repo.FullName != repo.path() {
			continue
		}
		// Rows that survive the filter drive convergence decisions, so a
		// malformed one fails the read rather than flowing onward (a
		// number of zero would otherwise become a "successful" PR #0).
		if pr.Number <= 0 || pr.State == "" || pr.Head.SHA == "" || pr.Draft == nil {
			return nil, errors.New("list pulls: response row is malformed")
		}
		states = append(states, pr.state())
	}
	return states, nil
}

// repoIDResponse decodes only a repository's canonical numeric identity.
type repoIDResponse struct {
	ID int64 `json:"id"`
}

// getRepositoryID resolves a repository's canonical numeric identity from the
// name a request is addressed by. Label intake compares it against the
// configured RepositoryID and fails closed on a mismatch (§5.18): a repository
// name rebound to a different repository is only detectable through the observed
// id, never the name, so intake must never record occurrence, project, or
// work-unit authority under a name that now resolves elsewhere.
func (f *forge) getRepositoryID(ctx context.Context, repo repoRef) (int64, error) {
	path := "/repos/" + repo.path()
	resp, err := f.do(ctx, http.MethodGet, repo, path, "", nil)
	if err != nil {
		return 0, fmt.Errorf("get repository id: %w", err)
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("get repository id: %w", &APIError{Status: resp.StatusCode, RequestPath: path})
	}
	var decoded repoIDResponse
	if err := decodeResponse(resp.Body, &decoded); err != nil {
		return 0, fmt.Errorf("get repository id: decode response: %w", err)
	}
	if decoded.ID <= 0 {
		return 0, errors.New("get repository id: response carries no positive repository id")
	}
	return decoded.ID, nil
}

// repoVisibilityResponse decodes a repository's visibility. GitHub returns the
// modern "visibility" (public|private|internal) and the legacy "private" bool;
// the caller maps them to a sensitivity class. Private is a pointer so an absent
// field is distinguishable from an explicit false: a partial or empty response
// must fail closed, not default to public.
type repoVisibilityResponse struct {
	Visibility string `json:"visibility"`
	Private    *bool  `json:"private"`
}

// getRepositoryVisibility reads the target repository's current visibility from
// the same GET /repos/{owner}/{repo} endpoint getRepositoryID uses. recipe v2
// derives the authored artifact's sensitivity class from it and re-reads it
// before every render, so a repository that has become more open than the
// stored class allows falls back to v1 (issue #1419).
func (f *forge) getRepositoryVisibility(ctx context.Context, repo repoRef) (repoVisibilityResponse, error) {
	path := "/repos/" + repo.path()
	resp, err := f.do(ctx, http.MethodGet, repo, path, "", nil)
	if err != nil {
		return repoVisibilityResponse{}, fmt.Errorf("get repository visibility: %w", err)
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return repoVisibilityResponse{}, fmt.Errorf("get repository visibility: %w", &APIError{Status: resp.StatusCode, RequestPath: path})
	}
	var decoded repoVisibilityResponse
	if err := decodeResponse(resp.Body, &decoded); err != nil {
		return repoVisibilityResponse{}, fmt.Errorf("get repository visibility: decode response: %w", err)
	}
	return decoded, nil
}

// prRead is a conditional pull-request observation: the decoded state
// plus the validator for the next conditional request, or NotModified
// when the server confirmed the cached state still holds.
type prRead struct {
	PR          prState
	ETag        string
	NotModified bool
}

// getPR observes one pull request. A 304 against etag reports
// NotModified with no other fields; a missing PR is an APIError (the
// reconciler only watches resources this package created).
func (f *forge) getPR(ctx context.Context, repo repoRef, number int, etag string) (prRead, error) {
	path := fmt.Sprintf("/repos/%s/pulls/%d", repo.path(), number)
	resp, err := f.do(ctx, http.MethodGet, repo, path, etag, nil)
	if err != nil {
		return prRead{}, fmt.Errorf("get pull: %w", err)
	}
	defer drainAndClose(resp.Body)
	switch resp.StatusCode {
	case http.StatusOK:
		var decoded prResponse
		if err := decodeResponse(resp.Body, &decoded); err != nil {
			return prRead{}, fmt.Errorf("get pull: decode response: %w", err)
		}
		if decoded.Number != number {
			// Returned-object boundary: an observation of some other PR
			// must not update this resource's state.
			return prRead{}, errors.New("get pull: response names a different pull number")
		}
		if decoded.Draft == nil {
			return prRead{}, errors.New("get pull: response carries no draft state")
		}
		return prRead{PR: decoded.state(), ETag: resp.Header.Get("ETag")}, nil
	case http.StatusNotModified:
		return prRead{NotModified: true, ETag: etag}, nil
	}
	return prRead{}, fmt.Errorf("get pull: %w", &APIError{Status: resp.StatusCode, RequestPath: path})
}

// createPR opens a pull request from branch onto base. draft opens it as a
// draft: a draft cannot be merged on GitHub, which is how recipe v2 holds a
// closable-source publication until its closure proposal resolves (issue #1419).
func (f *forge) createPR(ctx context.Context, repo repoRef, branch, base, title, body string, draft bool) (prState, error) {
	path := "/repos/" + repo.path() + "/pulls"
	resp, err := f.do(ctx, http.MethodPost, repo, path, "", map[string]any{
		"title": title,
		"head":  branch,
		"base":  base,
		"body":  body,
		"draft": draft,
	})
	if err != nil {
		return prState{}, fmt.Errorf("create pull: %w", err)
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return prState{}, fmt.Errorf("create pull: %w", &APIError{Status: resp.StatusCode, RequestPath: path})
	}
	var decoded prResponse
	if err := decodeResponse(resp.Body, &decoded); err != nil {
		return prState{}, fmt.Errorf("create pull: decode response: %w", err)
	}
	if decoded.Number <= 0 {
		return prState{}, errors.New("create pull: response carries no pull number")
	}
	if decoded.Draft == nil {
		return prState{}, errors.New("create pull: response carries no draft state")
	}
	return decoded.state(), nil
}

// updatePR patches a pull request's title and body and returns the
// patched state: the PATCH is an external effect, so its returned
// object is verified by the caller like any other, not discarded.
func (f *forge) updatePR(ctx context.Context, repo repoRef, number int, title, body string) (prState, error) {
	path := fmt.Sprintf("/repos/%s/pulls/%d", repo.path(), number)
	resp, err := f.do(ctx, http.MethodPatch, repo, path, "", map[string]string{
		"title": title,
		"body":  body,
	})
	if err != nil {
		return prState{}, fmt.Errorf("update pull: %w", err)
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return prState{}, fmt.Errorf("update pull: %w", &APIError{Status: resp.StatusCode, RequestPath: path})
	}
	var decoded prResponse
	if err := decodeResponse(resp.Body, &decoded); err != nil {
		return prState{}, fmt.Errorf("update pull: decode response: %w", err)
	}
	if decoded.Number != number {
		return prState{}, errors.New("update pull: response names a different pull number")
	}
	if decoded.Draft == nil {
		return prState{}, errors.New("update pull: response carries no draft state")
	}
	return decoded.state(), nil
}

// setPRDraft marks a pull request ready for review (draft=false) or converts it
// back to a draft (draft=true). REST cannot toggle draft state, so this uses the
// GraphQL markPullRequestReadyForReview / convertPullRequestToDraft mutations,
// bound to the pull request's GraphQL node id. The mutation is an external
// effect, so its returned object is verified like any other: the response must
// name this pull number and report the exact draft state requested, or the call
// fails closed rather than reporting a hold it did not establish.
func (f *forge) setPRDraft(ctx context.Context, repo repoRef, number int, nodeID string, draft bool) error {
	if nodeID == "" {
		return errors.New("set pull draft: pull request carries no node id")
	}
	const path = "/graphql"
	// Both mutations return { pullRequest { number isDraft } }; the field name
	// differs by mutation, so the response decodes both and the caller selects
	// the one the request asked for.
	mutation := `mutation($id: ID!) {
  markPullRequestReadyForReview(input: {pullRequestId: $id}) {
    pullRequest { number isDraft }
  }
}`
	if draft {
		mutation = `mutation($id: ID!) {
  convertPullRequestToDraft(input: {pullRequestId: $id}) {
    pullRequest { number isDraft }
  }
}`
	}
	body := struct {
		Query     string `json:"query"`
		Variables struct {
			ID string `json:"id"`
		} `json:"variables"`
	}{Query: mutation}
	body.Variables.ID = nodeID

	resp, err := f.do(ctx, http.MethodPost, repo, path, "", body)
	if err != nil {
		return fmt.Errorf("set pull draft: request: %w", err)
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("set pull draft: %w", &APIError{Status: resp.StatusCode, RequestPath: path})
	}

	// isDraft is a pointer so an absent field is distinguishable from an
	// explicit false: the response is a returned-object trust boundary, so a
	// partial body must fail closed rather than read as "not a draft" and
	// report a hold released that the mutation never confirmed.
	type pullResult struct {
		Number  int   `json:"number"`
		IsDraft *bool `json:"isDraft"`
	}
	var decoded struct {
		Errors []struct{} `json:"errors"`
		Data   struct {
			MarkReady *struct {
				PullRequest *pullResult `json:"pullRequest"`
			} `json:"markPullRequestReadyForReview"`
			ConvertToDraft *struct {
				PullRequest *pullResult `json:"pullRequest"`
			} `json:"convertPullRequestToDraft"`
		} `json:"data"`
	}
	if err := decodeResponse(resp.Body, &decoded); err != nil {
		return fmt.Errorf("set pull draft: decode response: %w", err)
	}
	if len(decoded.Errors) != 0 {
		return errors.New("set pull draft: GraphQL response carries errors")
	}
	var result *pullResult
	if draft {
		if decoded.Data.ConvertToDraft != nil {
			result = decoded.Data.ConvertToDraft.PullRequest
		}
	} else {
		if decoded.Data.MarkReady != nil {
			result = decoded.Data.MarkReady.PullRequest
		}
	}
	if result == nil {
		return errors.New("set pull draft: response carries no pull request for the requested mutation")
	}
	if result.Number != number {
		return errors.New("set pull draft: response names a different pull number")
	}
	if result.IsDraft == nil {
		return errors.New("set pull draft: response carries no draft state")
	}
	if *result.IsDraft != draft {
		return fmt.Errorf("set pull draft: pull request draft state did not converge to %t: %w", draft, ErrPublicationConflict)
	}
	return nil
}

// decodeResponse decodes exactly one JSON document from an API
// response body. Unknown fields are expected (GitHub responses carry
// far more than this package reads), but trailing data after the
// document and bodies over the shared resource bound fail closed: a
// response this package cannot fully delimit must not drive convergence
// decisions.
const maxForgeResponseBytes = 16 << 20

func decodeResponse(r io.Reader, v any) error {
	err := strictjson.DecodeReaderAllowingUnknownFields(
		r, v, strictjson.TolerateInvalidUTF8, strictjson.Limit(maxForgeResponseBytes),
	)
	if errors.Is(err, strictjson.ErrLimitExceeded) {
		return errors.New("response exceeds the 16777216-byte bound")
	}
	if errors.Is(err, strictjson.ErrTrailingData) {
		return errors.New("trailing data after the response document")
	}
	return err
}
