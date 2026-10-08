package publish

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Native review observation endpoints (plan §5.16, §7; issue #497): the
// active-resource reconciler observes a ready PR's native (forge-hosted)
// review activity as best-effort extra evidence. Three list resources are
// read conditionally — the PR's reviews, its inline review comments, and its
// description reactions (the reviewer's clean-pass signal is a reaction, not a
// review). This layer is pure transport: it decodes the raw forge fields and
// leaves reviewer-login filtering, badge normalization, and the trust-boundary
// bounds to the reconciler that builds the durable domain observation.

// listRead is a conditional list observation; see prRead. On a solicited 304
// it reports NotModified with no items, and the caller reuses its cached list.
type listRead[E any] struct {
	Items       []E
	ETag        string
	NotModified bool
}

// PullReview is one submitted native review on a pull request. AuthorID is
// the author's immutable forge account ID and AuthorLogin the login exactly as
// the forge returned it for this review; 0 means the forge named no account.
type PullReview struct {
	ID          int64
	AuthorID    int64
	AuthorLogin string
	State       string
	Body        string
	CommitID    string
	SubmittedAt time.Time
}

// PullReviewComment is one inline review comment on a pull request. ReviewID
// links it to the PullReview it belongs to (the forge's
// pull_request_review_id).
type PullReviewComment struct {
	ID       int64
	ReviewID int64
	// InReplyToID is the thread's first comment when this comment is a reply,
	// and 0 when this comment starts the thread.
	InReplyToID int64
	// AuthorID is the author's immutable forge account ID; see PullReview.
	AuthorID    int64
	AuthorLogin string
	Path        string
	// StartLine is the first line of a multi-line inline comment; 0 for a
	// single-line comment (the range is then just Line). Line is the last line.
	StartLine int
	Line      int
	Body      string
	// CommitID is the commit the forge currently anchors the comment to: it
	// moves to a newer head while the comment still applies there.
	// OriginalCommitID is the commit the comment was left on and never moves,
	// and a reply carries its thread's. OriginalStartLine and OriginalLine are
	// the comment's range on that commit, in the form of StartLine and Line.
	CommitID          string
	OriginalCommitID  string
	OriginalStartLine int
	OriginalLine      int
	CreatedAt         time.Time
}

// PullDescriptionReaction is one reaction on a pull request's description (the
// issues reactions surface). The reviewer's clean-pass signal is a "+1".
type PullDescriptionReaction struct {
	ID          int64
	AuthorLogin string
	Content     string
	CreatedAt   time.Time
}

type reviewResponse struct {
	ID   int64 `json:"id"`
	User struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	} `json:"user"`
	State       string `json:"state"`
	Body        string `json:"body"`
	CommitID    string `json:"commit_id"`
	SubmittedAt string `json:"submitted_at"`
}

type reviewCommentResponse struct {
	ID   int64 `json:"id"`
	User struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	} `json:"user"`
	ReviewID          int64  `json:"pull_request_review_id"`
	InReplyToID       int64  `json:"in_reply_to_id"`
	Path              string `json:"path"`
	StartLine         *int   `json:"start_line"`
	Line              *int   `json:"line"`
	OriginalStartLine *int   `json:"original_start_line"`
	OriginalLine      *int   `json:"original_line"`
	Body              string `json:"body"`
	CommitID          string `json:"commit_id"`
	OriginalCommitID  string `json:"original_commit_id"`
	CreatedAt         string `json:"created_at"`
}

// lineOrZero reads an optional forge line number; the forge sends null for a
// file-level comment and for a range the current diff no longer holds.
func lineOrZero(line *int) int {
	if line == nil {
		return 0
	}
	return *line
}

type reactionResponse struct {
	ID   int64 `json:"id"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
}

// getPullReviews observes a pull request's submitted reviews conditionally.
// Pending (never-submitted) reviews are not observations and are skipped.
func (f *forge) getPullReviews(ctx context.Context, repo repoRef, number int, etag string) (listRead[PullReview], error) {
	path := fmt.Sprintf("/repos/%s/pulls/%d/reviews?per_page=100", repo.path(), number)
	raw, resultETag, notModified, multiPage, err := fetchConditionalList[reviewResponse](ctx, f, repo, path, etag)
	if err != nil {
		return listRead[PullReview]{}, fmt.Errorf("get pull reviews: %w", err)
	}
	if notModified {
		return listRead[PullReview]{NotModified: true, ETag: etag}, nil
	}
	reviews := make([]PullReview, 0, len(raw))
	for _, rv := range raw {
		if rv.SubmittedAt == "" {
			continue
		}
		submitted, err := time.Parse(time.RFC3339, rv.SubmittedAt)
		if err != nil {
			return listRead[PullReview]{}, fmt.Errorf("get pull reviews: parse submitted_at: %w", err)
		}
		reviews = append(reviews, PullReview{
			ID: rv.ID, AuthorID: rv.User.ID, AuthorLogin: rv.User.Login, State: rv.State,
			Body: rv.Body, CommitID: rv.CommitID, SubmittedAt: submitted.UTC(),
		})
	}
	if multiPage {
		// A review on a later page can start a round (issue #524), and a
		// first-page 304 cannot prove a multi-page list unchanged, so the
		// validator is dropped and the next poll re-reads every page.
		resultETag = ""
	}
	return listRead[PullReview]{Items: reviews, ETag: resultETag}, nil
}

// getPullReviewComments observes a pull request's inline review comments
// conditionally.
func (f *forge) getPullReviewComments(ctx context.Context, repo repoRef, number int, etag string) (listRead[PullReviewComment], error) {
	path := fmt.Sprintf("/repos/%s/pulls/%d/comments?per_page=100", repo.path(), number)
	raw, resultETag, notModified, multiPage, err := fetchConditionalList[reviewCommentResponse](ctx, f, repo, path, etag)
	if err != nil {
		return listRead[PullReviewComment]{}, fmt.Errorf("get pull review comments: %w", err)
	}
	if notModified {
		return listRead[PullReviewComment]{NotModified: true, ETag: etag}, nil
	}
	comments := make([]PullReviewComment, 0, len(raw))
	for _, c := range raw {
		created, err := time.Parse(time.RFC3339, c.CreatedAt)
		if err != nil {
			return listRead[PullReviewComment]{}, fmt.Errorf("get pull review comments: parse created_at: %w", err)
		}
		comments = append(comments, PullReviewComment{
			ID: c.ID, ReviewID: c.ReviewID, InReplyToID: c.InReplyToID,
			AuthorID: c.User.ID, AuthorLogin: c.User.Login,
			Path: c.Path, StartLine: lineOrZero(c.StartLine), Line: lineOrZero(c.Line), Body: c.Body,
			CommitID: c.CommitID, OriginalCommitID: c.OriginalCommitID,
			OriginalStartLine: lineOrZero(c.OriginalStartLine), OriginalLine: lineOrZero(c.OriginalLine),
			CreatedAt: created.UTC(),
		})
	}
	if multiPage {
		// As for reviews: a comment on a later page must not hide behind an
		// unchanged first page.
		resultETag = ""
	}
	return listRead[PullReviewComment]{Items: comments, ETag: resultETag}, nil
}

// getIssueReactions observes the reactions on a pull request's description
// conditionally (the reactions surface is served under the issues path).
func (f *forge) getIssueReactions(ctx context.Context, repo repoRef, number int, etag string) (listRead[PullDescriptionReaction], error) {
	path := fmt.Sprintf("/repos/%s/issues/%d/reactions?per_page=100", repo.path(), number)
	// Reactions are best-effort native review evidence and start nothing: a
	// later-page change under a first-page 304 is an accepted completeness gap
	// (see fetchConditionalList), so multiPage is ignored here.
	raw, resultETag, notModified, _, err := fetchConditionalList[reactionResponse](ctx, f, repo, path, etag)
	if err != nil {
		return listRead[PullDescriptionReaction]{}, fmt.Errorf("get issue reactions: %w", err)
	}
	if notModified {
		return listRead[PullDescriptionReaction]{NotModified: true, ETag: etag}, nil
	}
	reactions := make([]PullDescriptionReaction, 0, len(raw))
	for _, rn := range raw {
		created, err := time.Parse(time.RFC3339, rn.CreatedAt)
		if err != nil {
			return listRead[PullDescriptionReaction]{}, fmt.Errorf("get issue reactions: parse created_at: %w", err)
		}
		reactions = append(reactions, PullDescriptionReaction{
			ID: rn.ID, AuthorLogin: rn.User.Login, Content: rn.Content, CreatedAt: created.UTC(),
		})
	}
	return listRead[PullDescriptionReaction]{Items: reactions, ETag: resultETag}, nil
}

// fetchConditionalList issues a conditional GET for the first page of a list
// resource and walks any further pages unconditionally, decoding each page
// into []E and concatenating. The first-page ETag is the returned validator:
// for a single-page list it is the whole-list validator, so a 304 confirms the
// list unchanged. The known limitation is a list that spans pages: an item
// appended to (or removed from) a later page while the first page is unchanged
// answers 304 and is observed only once the first page changes. It reports
// multiPage so a caller whose correctness needs the whole list (label intake,
// and the reviews and review comments that can start a round) can drop the
// validator and force an unconditional re-read; the reactions caller accepts
// the degradation as best-effort (readiness-inert) completeness, never a
// safety property. The page bound fails closed on an unbounded history
// rather than answering from a partial read.
func fetchConditionalList[E any](
	ctx context.Context, f *forge, repo repoRef, basePath, etag string,
) (items []E, resultETag string, notModified, multiPage bool, err error) {
	return fetchConditionalListScoped[E](ctx, f, repo, basePath, etag, false, "")
}

// exactCollection confines next links to the initial collection before any
// request is sent. Filing needs this even when a foreign page would be empty.
// Other readers retain their existing API-root pagination scope.
func fetchConditionalListScoped[E any](
	ctx context.Context, f *forge, repo repoRef, basePath, etag string, exactCollection bool, alternateCollectionPath string,
) (items []E, resultETag string, notModified, multiPage bool, err error) {
	const maxPages = 10
	var collection *url.URL
	if exactCollection {
		collection, err = url.Parse(f.baseURL + basePath)
		if err != nil {
			return nil, "", false, false, errors.New("invalid list collection URL")
		}
	}
	path := basePath
	first := true
	for pageNum := 0; ; pageNum++ {
		if pageNum == maxPages {
			return nil, "", false, false, fmt.Errorf("list history exceeds the %d-page read bound", maxPages)
		}
		reqETag := ""
		if first {
			reqETag = etag
		}
		resp, err := f.do(ctx, http.MethodGet, repo, path, reqETag, nil)
		if err != nil {
			return nil, "", false, false, err
		}
		if first && resp.StatusCode == http.StatusNotModified {
			drainAndClose(resp.Body)
			return nil, etag, true, false, nil
		}
		if resp.StatusCode != http.StatusOK {
			drainAndClose(resp.Body)
			return nil, "", false, false, &APIError{Status: resp.StatusCode, RequestPath: path}
		}
		var batch []E
		decodeErr := decodeResponse(resp.Body, &batch)
		next := nextPageLink(resp.Header.Get("Link"))
		pageETag := resp.Header.Get("ETag")
		drainAndClose(resp.Body)
		if decodeErr != nil {
			return nil, "", false, false, fmt.Errorf("decode response: %w", decodeErr)
		}
		if exactCollection {
			next, err = exactNextPageLink(resp.Header.Values("Link"))
			if err != nil {
				return nil, "", false, false, err
			}
		}
		// JSON null decodes into a nil slice without error; an empty page is a
		// legitimate list, null is not.
		if batch == nil {
			return nil, "", false, false, errors.New("response is not a list")
		}
		if first {
			resultETag = pageETag
			first = false
		}
		items = append(items, batch...)
		if next == "" {
			return items, resultETag, false, pageNum > 0, nil
		}
		// The Link target is absolute; only this API root is followable, so a
		// redirect off-host cannot carry the authenticated request away.
		if !strings.HasPrefix(next, f.baseURL+"/") {
			return nil, "", false, false, errors.New("next page is outside the API root")
		}
		if exactCollection {
			target, parseErr := url.Parse(next)
			if parseErr != nil || target.Scheme != collection.Scheme || target.Host != collection.Host ||
				target.User != nil || target.Fragment != "" || (target.EscapedPath() != collection.EscapedPath() && target.EscapedPath() != alternateCollectionPath) ||
				!nextCollectionPage(collection, target, pageNum+2) {
				return nil, "", false, false, errors.New("next page is outside the list collection")
			}
			// Follow validated aliases through the initial stable collection so
			// a repository-name rebind cannot redirect a later page.
			path = strings.SplitN(basePath, "?", 2)[0] + "?" + target.RawQuery
			continue
		}
		path = strings.TrimPrefix(next, f.baseURL)
	}
}

// exactNextPageLink accepts the GitHub Link form filing supports: complete
// <target>; rel="next|prev|first|last" entries. Any other form fails closed.
func exactNextPageLink(headers []string) (string, error) {
	next := ""
	for _, header := range headers {
		for _, entry := range strings.Split(header, ",") {
			entry = strings.TrimSpace(entry)
			if entry == "" || !strings.HasPrefix(entry, "<") {
				return "", errors.New("malformed next page link")
			}
			end := strings.IndexByte(entry, '>')
			if end < 0 {
				return "", errors.New("malformed next page link")
			}
			target, params := entry[1:end], strings.TrimSpace(entry[end+1:])
			if target == "" || !strings.HasPrefix(params, ";") {
				return "", errors.New("malformed next page link")
			}
			parsedTarget, targetErr := url.Parse(target)
			if targetErr != nil {
				return "", errors.New("malformed next page link")
			}
			if _, queryErr := url.ParseQuery(parsedTarget.RawQuery); queryErr != nil {
				return "", errors.New("malformed next page link")
			}
			params = strings.TrimSpace(strings.TrimPrefix(params, ";"))
			if !strings.HasPrefix(params, `rel="`) {
				return "", errors.New("malformed next page link")
			}
			params = strings.TrimPrefix(params, `rel="`)
			quote := strings.IndexByte(params, '"')
			if quote < 0 {
				return "", errors.New("malformed next page link")
			}
			relation, trailing := params[:quote], strings.TrimSpace(params[quote+1:])
			if relation != "next" && relation != "prev" && relation != "first" && relation != "last" {
				return "", errors.New("malformed next page link")
			}
			if trailing != "" {
				return "", errors.New("malformed next page link")
			}
			if relation == "next" {
				if next != "" {
					return "", errors.New("malformed next page link")
				}
				next = target
			}
		}
	}
	return next, nil
}

// nextCollectionPage preserves the filing reader's filters while allowing
// only the next numbered page in the configured issues collection.
func nextCollectionPage(collection, target *url.URL, wantPage int) bool {
	collectionQuery, collectionErr := url.ParseQuery(collection.RawQuery)
	targetQuery, targetErr := url.ParseQuery(target.RawQuery)
	if collectionErr != nil || targetErr != nil {
		return false
	}
	pages := targetQuery["page"]
	if len(pages) != 1 || pages[0] != strconv.Itoa(wantPage) {
		return false
	}
	delete(targetQuery, "page")
	return targetQuery.Encode() == collectionQuery.Encode()
}
