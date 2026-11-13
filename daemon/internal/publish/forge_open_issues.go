package publish

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// The duplicate search's open-issue listing (plan §7 Finding Adjudication,
// issue #3146). It reads with the issue-read token source only: listing open
// issues needs no write scope, and the lister never holds a source that
// could mint one.

const (
	// MaxOpenIssueTitleBytes and MaxOpenIssueExcerptBytes bound the text each
	// listed issue carries. The title bound matches the duplicate-search
	// record's (domain.MaxDuplicateSearchTitleBytes); the excerpt is only
	// shown to the site and never stored.
	MaxOpenIssueTitleBytes   = 256
	MaxOpenIssueExcerptBytes = 1024
	// MaxOpenIssueListing is the largest page GitHub serves, and so the most
	// one listing returns.
	MaxOpenIssueListing = 100
)

// OpenIssue is one open issue as the duplicate search sees it. Title is cut
// to MaxOpenIssueTitleBytes with control characters replaced by spaces;
// BodyExcerpt is the body's start, cut to MaxOpenIssueExcerptBytes.
type OpenIssue struct {
	Number      int
	Title       string
	BodyExcerpt string
}

// OpenIssueListing is one listing's result. Truncated says the repository
// has more open issues than the listing returned.
type OpenIssueListing struct {
	Issues    []OpenIssue
	Truncated bool
}

// OpenIssueLister lists a repository's most recently updated open issues.
type OpenIssueLister struct {
	forge *forge
}

// NewOpenIssueLister builds a lister over tokens, which the caller sets to
// the issue-read source. The client follows no redirect.
func NewOpenIssueLister(tokens TokenSource, client *http.Client, baseURL string) *OpenIssueLister {
	return &OpenIssueLister{forge: newForge(tokens, client, baseURL)}
}

// openIssueWire is one list element. Pointers distinguish an absent field
// from a zero one, so an element that cannot be judged fails the read.
type openIssueWire struct {
	RepositoryURL string  `json:"repository_url"`
	URL           string  `json:"url"`
	Number        *int    `json:"number"`
	Title         *string `json:"title"`
	Body          *string `json:"body"`
	PullRequest   *struct {
		URL string `json:"url"`
	} `json:"pull_request"`
}

// ListOpenIssues makes one GET of the repository's open issues, most
// recently updated first, at most limit of them. It drops pull requests and
// the exclude number (zero excludes nothing), and never follows a next page:
// a next link sets Truncated instead.
//
// Every element must prove membership of repo, by its repository_url and
// url, or the whole listing fails: an element from another repository must
// never become a candidate a duplicate verdict can name. Issue text never
// enters an error.
func (l *OpenIssueLister) ListOpenIssues(
	ctx context.Context, repo string, repositoryID int64, exclude, limit int,
) (OpenIssueListing, error) {
	ref, err := parseRepo(repo)
	if err != nil {
		return OpenIssueListing{}, fmt.Errorf("list open issues: %w", err)
	}
	if repositoryID <= 0 {
		return OpenIssueListing{}, errors.New("list open issues: repository id is not positive")
	}
	if limit < 1 || limit > MaxOpenIssueListing {
		return OpenIssueListing{}, fmt.Errorf("list open issues: limit %d is outside 1..%d", limit, MaxOpenIssueListing)
	}
	query := url.Values{
		"state":     {"open"},
		"sort":      {"updated"},
		"direction": {"desc"},
		"per_page":  {strconv.Itoa(limit)},
	}
	path := fmt.Sprintf("/repositories/%d/issues?%s", repositoryID, query.Encode())
	resp, err := l.forge.do(ctx, http.MethodGet, ref, path, "", nil)
	if err != nil {
		return OpenIssueListing{}, fmt.Errorf("list open issues: %w", err)
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return OpenIssueListing{}, fmt.Errorf("list open issues: %w",
			&APIError{Status: resp.StatusCode, RequestPath: path})
	}
	var wire []openIssueWire
	err = strictjson.DecodeReaderAllowingUnknownFields(
		resp.Body, &wire, strictjson.RejectInvalidUTF8, strictjson.Limit(maxForgeResponseBytes))
	if err != nil {
		return OpenIssueListing{}, errors.New("list open issues: response does not decode")
	}
	if len(wire) > limit {
		return OpenIssueListing{}, errors.New("list open issues: response exceeds the requested page size")
	}
	repositoryURL := l.forge.baseURL + "/repos/" + ref.path()
	listing := OpenIssueListing{
		Issues:    make([]OpenIssue, 0, len(wire)),
		Truncated: nextPageLink(resp.Header.Get("Link")) != "",
	}
	seen := make(map[int]bool, len(wire))
	for _, element := range wire {
		if element.Number == nil || *element.Number <= 0 {
			return OpenIssueListing{}, errors.New("list open issues: element carries no positive number")
		}
		number := *element.Number
		if element.RepositoryURL != repositoryURL || element.URL != fmt.Sprintf("%s/issues/%d", repositoryURL, number) {
			return OpenIssueListing{}, errors.New("list open issues: element does not prove repository membership")
		}
		if seen[number] {
			return OpenIssueListing{}, errors.New("list open issues: response repeats an issue number")
		}
		seen[number] = true
		if element.PullRequest != nil || number == exclude {
			continue
		}
		if element.Title == nil {
			return OpenIssueListing{}, errors.New("list open issues: element carries no title")
		}
		var body string
		if element.Body != nil {
			body = *element.Body
		}
		listing.Issues = append(listing.Issues, OpenIssue{
			Number:      number,
			Title:       cutAtRune(strings.Map(controlToSpace, *element.Title), MaxOpenIssueTitleBytes),
			BodyExcerpt: cutAtRune(body, MaxOpenIssueExcerptBytes),
		})
	}
	return listing, nil
}

// controlToSpace replaces a control character with a space, so a title
// keeps its words but stays one line, as the duplicate-search record
// requires.
func controlToSpace(r rune) rune {
	if unicode.IsControl(r) {
		return ' '
	}
	return r
}

// cutAtRune returns s cut to at most limit bytes without splitting a rune.
// s is valid UTF-8: the decoder rejected anything else.
func cutAtRune(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
