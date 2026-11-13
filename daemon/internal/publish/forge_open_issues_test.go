package publish_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

// listingTokens hands out one fixed token and counts its calls.
type listingTokens struct {
	token string
	calls atomic.Int32
}

func (s *listingTokens) Token(context.Context, string) (publish.InstallationToken, error) {
	s.calls.Add(1)
	return publish.InstallationToken{Token: publish.Secret(s.token)}, nil
}

// openIssueServer serves one open-issue listing at /repositories/42/issues.
// respond writes the response; $ROOT in a body becomes the API root.
type openIssueServer struct {
	t       *testing.T
	srv     *httptest.Server
	auth    string
	query   string
	respond func(w http.ResponseWriter, root string)
}

func newOpenIssueServer(t *testing.T, respond func(w http.ResponseWriter, root string)) *openIssueServer {
	t.Helper()
	s := &openIssueServer{t: t, respond: respond}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			t.Errorf("the lister followed a redirect")
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/repositories/42/issues" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		s.auth, s.query = r.Header.Get("Authorization"), r.URL.RawQuery
		s.respond(w, s.srv.URL)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func writeListing(body string) func(http.ResponseWriter, string) {
	return func(w http.ResponseWriter, root string) {
		_, _ = io.WriteString(w, strings.ReplaceAll(body, "$ROOT", root))
	}
}

func openIssueElement(number int, title, body string) string {
	return fmt.Sprintf(`{"repository_url":"$ROOT/repos/owner/name","url":"$ROOT/repos/owner/name/issues/%d",`+
		`"number":%d,"title":%q,"body":%q}`, number, number, title, body)
}

// TestListOpenIssuesReadsWithTheIssueReadSource: the request carries the
// token of the source the lister was built over, once, and asks for open
// issues most recently updated first.
func TestListOpenIssuesReadsWithTheIssueReadSource(t *testing.T) {
	t.Parallel()
	server := newOpenIssueServer(t, writeListing(`[`+openIssueElement(5, "Bound retries", "Body.")+`]`))
	issueRead := &listingTokens{token: "issue-read-token"}
	lister := publish.NewOpenIssueLister(issueRead, server.srv.Client(), server.srv.URL)
	listing, err := lister.ListOpenIssues(context.Background(), "owner/name", 42, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if server.auth != "Bearer issue-read-token" || issueRead.calls.Load() != 1 {
		t.Fatalf("authorization = %q after %d token calls", server.auth, issueRead.calls.Load())
	}
	if server.query != "direction=desc&per_page=100&sort=updated&state=open" {
		t.Fatalf("query = %q", server.query)
	}
	if len(listing.Issues) != 1 || listing.Issues[0] != (publish.OpenIssue{Number: 5, Title: "Bound retries", BodyExcerpt: "Body."}) ||
		listing.Truncated {
		t.Fatalf("listing = %#v", listing)
	}
}

// TestListOpenIssuesDropsPullRequestsAndTheExcludedIssue, and cuts text at
// its bounds without splitting a rune.
func TestListOpenIssuesDropsPullRequestsAndTheExcludedIssue(t *testing.T) {
	t.Parallel()
	longTitle := strings.Repeat("a", publish.MaxOpenIssueTitleBytes-1) + "é"
	longBody := strings.Repeat("b", publish.MaxOpenIssueExcerptBytes+10)
	pull := `{"repository_url":"$ROOT/repos/owner/name","url":"$ROOT/repos/owner/name/issues/8",` +
		`"number":8,"title":"A pull request","body":"","pull_request":{"url":"$ROOT/repos/owner/name/pulls/8"}}`
	server := newOpenIssueServer(t, writeListing(`[`+openIssueElement(9, "The bound issue", "")+`,`+pull+`,`+
		openIssueElement(3, longTitle, longBody)+`,`+openIssueElement(2, "Tab\there", "")+`,`+
		`{"repository_url":"$ROOT/repos/owner/name","url":"$ROOT/repos/owner/name/issues/1","number":1,"title":"No body","body":null}]`))
	lister := publish.NewOpenIssueLister(&listingTokens{token: "t"}, server.srv.Client(), server.srv.URL)
	listing, err := lister.ListOpenIssues(context.Background(), "owner/name", 42, 9, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Issues) != 3 {
		t.Fatalf("listing = %#v", listing)
	}
	if got := listing.Issues[0]; got.Number != 3 || got.Title != strings.Repeat("a", publish.MaxOpenIssueTitleBytes-1) ||
		got.BodyExcerpt != longBody[:publish.MaxOpenIssueExcerptBytes] {
		t.Fatalf("cut issue = %d %q (%d bytes)", got.Number, got.Title, len(got.BodyExcerpt))
	}
	if listing.Issues[1].Title != "Tab here" || listing.Issues[2] != (publish.OpenIssue{Number: 1, Title: "No body"}) {
		t.Fatalf("listing = %#v", listing.Issues[1:])
	}
}

func TestListOpenIssuesMarksANextPageTruncatedWithoutFollowingIt(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	server := newOpenIssueServer(t, func(w http.ResponseWriter, root string) {
		requests.Add(1)
		w.Header().Set("Link", fmt.Sprintf(`<%s/repositories/42/issues?page=2>; rel="next"`, root))
		_, _ = io.WriteString(w, strings.ReplaceAll(`[`+openIssueElement(4, "Only page", "")+`]`, "$ROOT", root))
	})
	lister := publish.NewOpenIssueLister(&listingTokens{token: "t"}, server.srv.Client(), server.srv.URL)
	listing, err := lister.ListOpenIssues(context.Background(), "owner/name", 42, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !listing.Truncated || len(listing.Issues) != 1 || requests.Load() != 1 {
		t.Fatalf("listing = %#v after %d requests", listing, requests.Load())
	}
}

// TestListOpenIssuesFailsClosed: a redirect is not followed, and an element
// that cannot prove membership fails the listing without its text reaching
// the error.
func TestListOpenIssuesFailsClosed(t *testing.T) {
	t.Parallel()
	const secretTitle = "issue text that must not reach an error"
	cases := map[string]func(http.ResponseWriter, string){
		"redirect": func(w http.ResponseWriter, root string) {
			w.Header().Set("Location", root+"/elsewhere")
			w.WriteHeader(http.StatusFound)
		},
		"another repository": writeListing(`[{"repository_url":"$ROOT/repos/other/name",` +
			`"url":"$ROOT/repos/other/name/issues/5","number":5,"title":"` + secretTitle + `","body":""}]`),
		"url names another issue": writeListing(`[{"repository_url":"$ROOT/repos/owner/name",` +
			`"url":"$ROOT/repos/owner/name/issues/6","number":5,"title":"` + secretTitle + `","body":""}]`),
		"no number":       writeListing(`[{"repository_url":"$ROOT/repos/owner/name","title":"` + secretTitle + `"}]`),
		"no title":        writeListing(`[` + strings.Replace(openIssueElement(5, "", ""), `"title":"",`, "", 1) + `]`),
		"repeated number": writeListing(`[` + openIssueElement(5, secretTitle, "") + `,` + openIssueElement(5, "x", "") + `]`),
		"over the page":   writeListing(`[` + openIssueElement(5, secretTitle, "") + `,` + openIssueElement(6, "x", "") + `]`),
		"invalid UTF-8":   writeListing("[" + openIssueElement(5, "bad", "") + "]\n\xff"),
		"server error": func(w http.ResponseWriter, _ string) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, secretTitle)
		},
	}
	for name, respond := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := newOpenIssueServer(t, respond)
			lister := publish.NewOpenIssueLister(&listingTokens{token: "t"}, server.srv.Client(), server.srv.URL)
			limit := 100
			if name == "over the page" {
				limit = 1
			}
			_, err := lister.ListOpenIssues(context.Background(), "owner/name", 42, 0, limit)
			if err == nil {
				t.Fatal("listing succeeded")
			}
			if strings.Contains(err.Error(), secretTitle) {
				t.Fatalf("error carries issue text: %v", err)
			}
			var apiErr *publish.APIError
			if name == "redirect" && (!errors.As(err, &apiErr) || apiErr.Status != http.StatusFound) {
				t.Fatalf("redirect error = %v, want the 302 as an APIError", err)
			}
		})
	}
}

func TestListOpenIssuesRefusesBadArguments(t *testing.T) {
	t.Parallel()
	lister := publish.NewOpenIssueLister(&listingTokens{token: "t"}, http.DefaultClient, "http://127.0.0.1:1")
	for name, call := range map[string]func() error{
		"repo": func() error { _, err := lister.ListOpenIssues(context.Background(), "owner", 42, 0, 10); return err },
		"id": func() error {
			_, err := lister.ListOpenIssues(context.Background(), "owner/name", 0, 0, 10)
			return err
		},
		"limit 0": func() error {
			_, err := lister.ListOpenIssues(context.Background(), "owner/name", 42, 0, 0)
			return err
		},
		"over 100": func() error {
			_, err := lister.ListOpenIssues(context.Background(), "owner/name", 42, 0, 101)
			return err
		},
	} {
		if call() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
