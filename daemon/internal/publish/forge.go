package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
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
	tok, err := f.tokens.Token(ctx, repo.name)
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
	path := "/repos/" + repo.path() + "/git/ref/heads/" + branch
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
}

// prResponse is the wire shape a pull-request read decodes.
type prResponse struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Head   struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref  string `json:"ref"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
}

func (r prResponse) state() prState {
	return prState{
		Number:   r.Number,
		State:    r.State,
		Title:    r.Title,
		Body:     r.Body,
		HeadRef:  r.Head.Ref,
		HeadSHA:  r.Head.SHA,
		HeadRepo: r.Head.Repo.FullName,
		BaseRef:  r.Base.Ref,
		BaseRepo: r.Base.Repo.FullName,
	}
}

// listPRsByHead lists pull requests (any state) whose head is
// owner:branch.
func (f *forge) listPRsByHead(ctx context.Context, repo repoRef, branch string) ([]prState, error) {
	path := "/repos/" + repo.path() + "/pulls?head=" + repo.owner + ":" + branch + "&state=all&per_page=100"
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
		if pr.Number <= 0 || pr.State == "" || pr.Head.SHA == "" {
			return nil, errors.New("list pulls: response row is malformed")
		}
		states = append(states, pr.state())
	}
	return states, nil
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
		return prRead{PR: decoded.state(), ETag: resp.Header.Get("ETag")}, nil
	case http.StatusNotModified:
		return prRead{NotModified: true, ETag: etag}, nil
	}
	return prRead{}, fmt.Errorf("get pull: %w", &APIError{Status: resp.StatusCode, RequestPath: path})
}

// createPR opens a pull request from branch onto base.
func (f *forge) createPR(ctx context.Context, repo repoRef, branch, base, title, body string) (prState, error) {
	path := "/repos/" + repo.path() + "/pulls"
	resp, err := f.do(ctx, http.MethodPost, repo, path, "", map[string]string{
		"title": title,
		"head":  branch,
		"base":  base,
		"body":  body,
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
	return decoded.state(), nil
}

// decodeResponse decodes exactly one JSON document from an API
// response body. Unknown fields are expected (GitHub responses carry
// far more than this package reads), but trailing data after the
// document fails closed: a response this package cannot fully delimit
// must not drive convergence decisions.
func decodeResponse(r io.Reader, v any) error {
	dec := json.NewDecoder(r)
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the response document")
	}
	return nil
}
