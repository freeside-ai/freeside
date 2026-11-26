package publish

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// reviewThreadResolutionResult classifies one attempt to find and resolve an
// inline pull request review thread on GitHub. The zero value "" is invalid.
type reviewThreadResolutionResult string

const (
	// reviewThreadResolved: the matching thread is now resolved (either already
	// resolved or confirmed resolved by resolveReviewThread).
	reviewThreadResolved reviewThreadResolutionResult = "resolved"
	// reviewThreadRetry: GitHub refused nothing (transport error, 5xx, 429, or
	// a rate-limited 403 / 200 GraphQL error); the caller may retry within its
	// attempt bound.
	reviewThreadRetry reviewThreadResolutionResult = "retry"
	// reviewThreadFailed: GitHub refused the call, the thread could not be
	// uniquely identified, or the returned object failed verification.
	reviewThreadFailed reviewThreadResolutionResult = "failed"
)

// allReviewThreadResolutionResults is the single registration point for the
// thread-resolution vocabulary; the zero value "" is invalid by design.
var allReviewThreadResolutionResults = []reviewThreadResolutionResult{
	reviewThreadResolved,
	reviewThreadRetry,
	reviewThreadFailed,
}

func (r reviewThreadResolutionResult) valid() bool {
	switch r {
	case reviewThreadResolved, reviewThreadRetry, reviewThreadFailed:
		return true
	default:
		return false
	}
}

func unhandledReviewThreadResolutionResult(r reviewThreadResolutionResult) error {
	if r.valid() {
		return fmt.Errorf("review thread resolution result %q is unhandled", r)
	}
	return fmt.Errorf("review thread resolution result %q is not one of %v", r, allReviewThreadResolutionResults)
}

const maxReviewThreadPages = 100

// resolveReviewThread finds the single review thread on repo/number whose first
// comment has REST ID root and resolves it when it is still unresolved. It
// returns a Go error only when nothing was sent (invalid input, canceled
// context, or request construction / token mint failure), and never echoes a
// token or response body.
//
// Tier A returned-object gate: every query page must carry no GraphQL errors,
// name repo.path() and pull number, include pageInfo.hasNextPage and a non-nil
// nodes list, give every thread a non-empty id, an explicit isResolved bool,
// and a valid fullDatabaseId on any first comment, and advance with a non-empty
// unseen cursor. The resolveReviewThread mutation must carry no GraphQL errors
// and return the exact thread id with isResolved present and true.
func (f *forge) resolveReviewThread(ctx context.Context, repo repoRef, number int, root int64) (reviewThreadResolutionResult, error) {
	if repo.owner == "" || repo.name == "" || number <= 0 || root <= 0 {
		return "", errors.New("resolve review thread: invalid target")
	}
	const listQuery = `query($owner: String!, $name: String!, $number: Int!, $after: String) {
  repository(owner: $owner, name: $name) {
    nameWithOwner
    pullRequest(number: $number) {
      number
      reviewThreads(first: 100, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          isResolved
          comments(first: 1) {
            nodes { fullDatabaseId }
          }
        }
      }
    }
  }
}`
	type listData struct {
		Repository *struct {
			NameWithOwner string `json:"nameWithOwner"`
			PullRequest   *struct {
				Number        int `json:"number"`
				ReviewThreads *struct {
					PageInfo *struct {
						HasNextPage *bool  `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []*struct {
						ID         string `json:"id"`
						IsResolved *bool  `json:"isResolved"`
						Comments   *struct {
							Nodes []*struct {
								FullDatabaseID string `json:"fullDatabaseId"`
							} `json:"nodes"`
						} `json:"comments"`
					} `json:"nodes"`
				} `json:"reviewThreads"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}

	var after *string
	seenCursors := map[string]bool{}
	var matchID string
	var matchResolved bool
	matches := 0

	for page := 0; ; page++ {
		if page == maxReviewThreadPages {
			return reviewThreadFailed, nil
		}
		body := struct {
			Query     string `json:"query"`
			Variables struct {
				Owner  string  `json:"owner"`
				Name   string  `json:"name"`
				Number int     `json:"number"`
				After  *string `json:"after"`
			} `json:"variables"`
		}{Query: listQuery}
		body.Variables.Owner = repo.owner
		body.Variables.Name = repo.name
		body.Variables.Number = number
		body.Variables.After = after

		data, outcome, err := postReviewThreadGraphQL[listData](ctx, f, repo, body)
		if err != nil || outcome != "" {
			return outcome, err
		}
		if data.Repository == nil || data.Repository.NameWithOwner != repo.path() {
			return reviewThreadFailed, nil
		}
		pr := data.Repository.PullRequest
		if pr == nil || pr.Number != number || pr.ReviewThreads == nil ||
			pr.ReviewThreads.PageInfo == nil || pr.ReviewThreads.PageInfo.HasNextPage == nil ||
			pr.ReviewThreads.Nodes == nil {
			return reviewThreadFailed, nil
		}
		for _, thread := range pr.ReviewThreads.Nodes {
			if thread == nil || thread.ID == "" || thread.IsResolved == nil ||
				thread.Comments == nil || thread.Comments.Nodes == nil || len(thread.Comments.Nodes) > 1 {
				return reviewThreadFailed, nil
			}
			if len(thread.Comments.Nodes) == 0 {
				continue
			}
			first := thread.Comments.Nodes[0]
			if first == nil {
				return reviewThreadFailed, nil
			}
			commentID, err := parseFullDatabaseID(first.FullDatabaseID)
			if err != nil {
				return reviewThreadFailed, nil
			}
			if commentID == root {
				matches++
				matchID = thread.ID
				matchResolved = *thread.IsResolved
			}
		}
		if matches > 1 {
			return reviewThreadFailed, nil
		}
		if matches == 1 {
			break
		}
		if !*pr.ReviewThreads.PageInfo.HasNextPage {
			return reviewThreadFailed, nil
		}
		next := pr.ReviewThreads.PageInfo.EndCursor
		if next == "" || seenCursors[next] {
			return reviewThreadFailed, nil
		}
		seenCursors[next] = true
		cursor := next
		after = &cursor
	}

	if matchResolved {
		return reviewThreadResolved, nil
	}

	const resolveMutation = `mutation($id: ID!) {
  resolveReviewThread(input: {threadId: $id}) {
    thread { id isResolved }
  }
}`
	type resolveData struct {
		ResolveReviewThread *struct {
			Thread *struct {
				ID         string `json:"id"`
				IsResolved *bool  `json:"isResolved"`
			} `json:"thread"`
		} `json:"resolveReviewThread"`
	}
	mutBody := struct {
		Query     string `json:"query"`
		Variables struct {
			ID string `json:"id"`
		} `json:"variables"`
	}{Query: resolveMutation}
	mutBody.Variables.ID = matchID

	data, outcome, err := postReviewThreadGraphQL[resolveData](ctx, f, repo, mutBody)
	if err != nil || outcome != "" {
		return outcome, err
	}
	if data.ResolveReviewThread == nil || data.ResolveReviewThread.Thread == nil ||
		data.ResolveReviewThread.Thread.ID != matchID ||
		data.ResolveReviewThread.Thread.IsResolved == nil ||
		!*data.ResolveReviewThread.Thread.IsResolved {
		return reviewThreadFailed, nil
	}
	return reviewThreadResolved, nil
}

// postReviewThreadGraphQL sends one GraphQL request and classifies HTTP,
// transport, and GraphQL-error responses without leaking tokens or bodies.
// A non-nil error means nothing was sent or ctx was canceled; an empty outcome
// with nil error means HTTP 200 decoded with no GraphQL errors.
func postReviewThreadGraphQL[T any](ctx context.Context, f *forge, repo repoRef, body any) (T, reviewThreadResolutionResult, error) {
	var zero T
	req, err := f.newRequest(ctx, http.MethodPost, repo, "/graphql", "", body)
	if err != nil {
		if ctx.Err() != nil {
			return zero, "", ctx.Err()
		}
		// Never return raw token-source errors across this boundary.
		return zero, "", errors.New("resolve review thread: request unavailable")
	}
	resp, err := f.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return zero, "", ctx.Err()
		}
		// Transport errors may echo credentials; map to a retry without text.
		return zero, reviewThreadRetry, nil
	}
	defer drainAndClose(resp.Body)

	switch {
	case resp.StatusCode >= http.StatusInternalServerError && resp.StatusCode <= 599:
		return zero, reviewThreadRetry, nil
	case resp.StatusCode == http.StatusTooManyRequests:
		return zero, reviewThreadRetry, nil
	case resp.StatusCode == http.StatusForbidden:
		if forbiddenByRateLimit(resp.Header) {
			return zero, reviewThreadRetry, nil
		}
		return zero, reviewThreadFailed, nil
	case resp.StatusCode != http.StatusOK:
		return zero, reviewThreadFailed, nil
	}

	respBody := &readErrorRecorder{r: resp.Body}
	var envelope struct {
		Errors []struct{} `json:"errors"`
		Data   T          `json:"data"`
	}
	if err := decodeResponse(respBody, &envelope); err != nil {
		if ctx.Err() != nil {
			return zero, "", ctx.Err()
		}
		if respBody.err != nil {
			// A mid-body read error is a transport failure; retry without echoing text.
			return zero, reviewThreadRetry, nil
		}
		return zero, reviewThreadFailed, nil
	}
	if len(envelope.Errors) != 0 {
		if forbiddenByRateLimit(resp.Header) {
			return zero, reviewThreadRetry, nil
		}
		return zero, reviewThreadFailed, nil
	}
	return envelope.Data, "", nil
}

// readErrorRecorder lets the caller tell a dropped connection (GitHub refused
// nothing, so retry) from a body that arrived whole but doesn't decode (fail).
type readErrorRecorder struct {
	r   io.Reader
	err error
}

func (r *readErrorRecorder) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && r.err == nil {
		r.err = err
	}
	return n, err
}

// parseFullDatabaseID strictly parses GitHub's GraphQL BigInt comment ID
// string, matching parseExternalReplyThread's canonical positive-integer check.
func parseFullDatabaseID(raw string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != raw {
		return 0, errors.New("resolve review thread: invalid fullDatabaseId")
	}
	return id, nil
}
