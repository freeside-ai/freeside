package publish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// filingForgeFake serves the three follow-up filing endpoints from canned
// values and records what it was asked.
type filingForgeFake struct {
	t *testing.T
	// createStatus and createBody answer the create request.
	createStatus int
	createBody   string
	created      map[string]any
	// pages are the issue listing's pages, in order.
	pages              []string
	listQuery          map[string]string
	malformedNextQuery string
	canonicalNext      int64
	namedNext          bool
	listRequests       int
	milestones         string
}

func newFilingForgeFake(t *testing.T) (*forge, *filingForgeFake, repoRef) {
	t.Helper()
	fake := &filingForgeFake{t: t}
	srv := httptest.NewServer(http.HandlerFunc(fake.handle))
	t.Cleanup(srv.Close)
	return newForge(draftTestTokenSource{}, srv.Client(), srv.URL+"/api/v3"), fake, repoRef{owner: "owner", name: "name"}
}

func (g *filingForgeFake) handle(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v3")
	render := func(body string) string {
		return strings.ReplaceAll(body, "$API_ROOT", "http://"+r.Host+"/api/v3")
	}
	switch {
	case r.Method == http.MethodPost && path == "/repos/owner/name/issues":
		body, err := io.ReadAll(r.Body)
		if err != nil || json.Unmarshal(body, &g.created) != nil {
			g.t.Errorf("create issue body %q: %v", body, err)
		}
		w.WriteHeader(g.createStatus)
		_, _ = io.WriteString(w, render(g.createBody))
	case r.Method == http.MethodGet && (path == "/repos/owner/name/issues" || path == "/repositories/42/issues"):
		g.listRequests++
		query := r.URL.Query()
		page := 1
		if raw := query.Get("page"); raw != "" {
			_, _ = fmt.Sscan(raw, &page)
		} else {
			g.listQuery = map[string]string{}
			for key := range query {
				g.listQuery[key] = query.Get(key)
			}
		}
		if page < len(g.pages) {
			query.Set("page", fmt.Sprint(page+1))
			nextPath := r.URL.Path
			if g.canonicalNext != 0 && page == 1 {
				nextPath = fmt.Sprintf("/api/v3/repositories/%d/issues", g.canonicalNext)
			}
			if g.namedNext {
				nextPath = "/api/v3/repos/owner/name/issues"
			}
			if g.malformedNextQuery != "" {
				w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?%s&%s>; rel="next"`, r.Host, nextPath, query.Encode(), g.malformedNextQuery))
				_, _ = io.WriteString(w, render(g.pages[page-1]))
				return
			}
			w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?%s>; rel="next"`, r.Host, nextPath, query.Encode()))
		}
		_, _ = io.WriteString(w, render(g.pages[page-1]))
	case r.Method == http.MethodGet && path == "/repos/owner/name/milestones":
		_, _ = io.WriteString(w, g.milestones)
	default:
		g.t.Errorf("unexpected request %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusNotImplemented)
	}
}

const filingIssueJSON = `{"number":12,"repository_url":"$API_ROOT/repos/owner/name","url":"$API_ROOT/repos/owner/name/issues/12","title":"SECRET-TITLE","user":{"id":7,"type":"Bot","login":"app[bot]"},"created_at":"2026-09-02T12:00:00Z"}`

func TestExactNextPageLink(t *testing.T) {
	t.Parallel()
	valid := "<https://api.example/issues?page=2>; rel=\"next\", <https://api.example/issues?page=3>; rel=\"last\""
	for name, headers := range map[string][]string{
		"next and last":      {valid},
		"spaced relations":   {"<https://api.example/issues?page=1>; rel=\"prev\", <https://api.example/issues?page=2>; rel=\"next\", <https://api.example/issues?page=3>; rel=\"last\""},
		"terminal relations": {"<https://api.example/issues?page=1>; rel=\"prev\", <https://api.example/issues?page=1>; rel=\"first\""},
	} {
		t.Run(name, func(t *testing.T) {
			next, err := exactNextPageLink(headers)
			if err != nil {
				t.Fatal(err)
			}
			want := "https://api.example/issues?page=2"
			if name == "terminal relations" {
				want = ""
			}
			if next != want {
				t.Fatalf("next = %q, want %q", next, want)
			}
		})
	}
	for name, headers := range map[string][]string{
		"empty":             {"<>; rel=\"next\""},
		"semicolon target":  {"<https://api.example/issues?x=1;bad=%ZZ>; rel=\"next\""},
		"malformed escape":  {"<https://api.example/issues?bad=%ZZ>; rel=\"next\""},
		"bad after next":    {"<https://api.example/issues?page=2>; rel=\"next\", broken"},
		"duplicate headers": {"<https://api.example/issues?page=2>; rel=\"next\"", "<https://api.example/issues?page=3>; rel=\"next\""},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := exactNextPageLink(headers)
			if err == nil {
				t.Fatal("accepted malformed link")
			}
		})
	}
}

func TestForgeCreateIssue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	send := func(t *testing.T, f *forge, repo repoRef, labels []string, milestone *int) issueCreateResult {
		t.Helper()
		req, err := f.newCreateIssueRequest(ctx, repo, "Title", "Body", labels, milestone)
		if err != nil {
			t.Fatalf("newCreateIssueRequest: %v", err)
		}
		result, err := f.sendCreateIssue(req, repo)
		if err != nil {
			t.Fatalf("sendCreateIssue: %v", err)
		}
		return result
	}
	t.Run("sends the target and decodes the issue identity", func(t *testing.T) {
		t.Parallel()
		f, fake, repo := newFilingForgeFake(t)
		fake.createStatus, fake.createBody = http.StatusCreated, filingIssueJSON
		milestone := 7
		result := send(t, f, repo, []string{"deferral", "lane:spine"}, &milestone)
		want := filedIssue{
			Number: 12, AuthorID: 7, AuthorType: "Bot", CreatedAt: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC),
		}
		if result.Status != http.StatusCreated || result.Issue == nil || *result.Issue != want {
			t.Fatalf("result = %+v (issue %+v), want %+v", result, result.Issue, want)
		}
		sent, err := json.Marshal(fake.created)
		if err != nil {
			t.Fatal(err)
		}
		if string(sent) != `{"body":"Body","labels":["deferral","lane:spine"],"milestone":7,"title":"Title"}` {
			t.Fatalf("create request body = %s", sent)
		}
	})
	t.Run("omits an empty label list and a missing milestone", func(t *testing.T) {
		t.Parallel()
		f, fake, repo := newFilingForgeFake(t)
		fake.createStatus, fake.createBody = http.StatusCreated, filingIssueJSON
		send(t, f, repo, nil, nil)
		if _, ok := fake.created["labels"]; ok || len(fake.created) != 2 {
			t.Fatalf("create request body = %+v, want only title and body", fake.created)
		}
	})
	// A response the create may have committed behind is never an error and
	// never an issue: the caller classifies it.
	for name, body := range map[string]string{
		"not JSON":               `<html>`,
		"no number":              strings.Replace(filingIssueJSON, `"number":12,`, "", 1),
		"zero number":            strings.Replace(filingIssueJSON, `"number":12`, `"number":0`, 1),
		"no author":              strings.Replace(filingIssueJSON, `"user":`, `"unused":`, 1),
		"no author id":           strings.Replace(filingIssueJSON, `"id":7,`, "", 1),
		"no author type":         strings.Replace(filingIssueJSON, `"type":"Bot",`, "", 1),
		"no creation time":       strings.Replace(filingIssueJSON, `"created_at":`, `"unused":`, 1),
		"missing repository URL": strings.Replace(filingIssueJSON, `"repository_url":`, `"unused":`, 1),
		"missing issue URL":      strings.Replace(filingIssueJSON, `"url":`, `"unused":`, 1),
		"an empty response":      ``,
	} {
		t.Run("201 with "+name, func(t *testing.T) {
			t.Parallel()
			f, fake, repo := newFilingForgeFake(t)
			fake.createStatus, fake.createBody = http.StatusCreated, body
			if result := send(t, f, repo, nil, nil); result.Status != http.StatusCreated || result.Issue != nil {
				t.Fatalf("result = %+v (issue %+v), want a 201 with no issue", result, result.Issue)
			}
		})
	}
	t.Run("returns a rejection with its headers and no issue", func(t *testing.T) {
		t.Parallel()
		f, fake, repo := newFilingForgeFake(t)
		fake.createStatus, fake.createBody = http.StatusUnprocessableEntity, filingIssueJSON
		result := send(t, f, repo, nil, nil)
		if result.Status != http.StatusUnprocessableEntity || result.Issue != nil || result.Header == nil {
			t.Fatalf("result = %+v", result)
		}
	})
}

func TestForgeListIssuesCreatedBy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	since := time.Date(2026, 9, 2, 12, 0, 0, 0, time.FixedZone("", 3600))
	t.Run("accepts the trusted canonical repository collection", func(t *testing.T) {
		t.Parallel()
		f, fake, repo := newFilingForgeFake(t)
		fake.pages = []string{`[` + filingIssueJSON + `]`, `[]`}
		fake.canonicalNext = 42
		issues, err := f.listIssuesCreatedBy(ctx, repo, 42, "app[bot]", since)
		if err != nil || len(issues) != 1 {
			t.Fatalf("issues = %+v, err = %v", issues, err)
		}
	})
	t.Run("accepts a named pagination alias under the configured API root", func(t *testing.T) {
		t.Parallel()
		f, fake, repo := newFilingForgeFake(t)
		fake.pages = []string{`[` + filingIssueJSON + `]`, `[]`}
		fake.namedNext = true
		issues, err := f.listIssuesCreatedBy(ctx, repo, 42, "app[bot]", since)
		if err != nil || len(issues) != 1 || fake.listRequests != 2 {
			t.Fatalf("issues = %+v, err = %v, requests = %d", issues, err, fake.listRequests)
		}
	})
	t.Run("rejects another numeric repository collection", func(t *testing.T) {
		t.Parallel()
		f, fake, repo := newFilingForgeFake(t)
		fake.pages = []string{`[` + filingIssueJSON + `]`, `[]`}
		fake.canonicalNext = 43
		issues, err := f.listIssuesCreatedBy(ctx, repo, 42, "app[bot]", since)
		if err == nil || issues != nil || fake.listRequests != 1 {
			t.Fatalf("issues = %+v, err = %v, requests = %d", issues, err, fake.listRequests)
		}
	})
	t.Run("preserves a partial body decode failure with a valid next link", func(t *testing.T) {
		t.Parallel()
		f, fake, repo := newFilingForgeFake(t)
		malformed := strings.TrimSuffix(filingIssueJSON, "}") + `,"pull_request":{"url":123}}`
		fake.pages = []string{`[` + filingIssueJSON + `,` + malformed + `]`, `[]`}
		issues, err := f.listIssuesCreatedBy(ctx, repo, 42, "app[bot]", since)
		if err == nil || issues != nil || fake.listRequests != 1 {
			t.Fatalf("issues = %+v, err = %v, requests = %d", issues, err, fake.listRequests)
		}
	})
	t.Run("reads every page and marks pull requests", func(t *testing.T) {
		t.Parallel()
		f, fake, repo := newFilingForgeFake(t)
		fake.pages = []string{
			`[` + filingIssueJSON + `]`,
			`[{"number":13,"repository_url":"$API_ROOT/repos/owner/name","url":"$API_ROOT/repos/owner/name/issues/13","user":{"id":8,"type":"User"},"created_at":"2026-09-02T13:00:00+01:00","pull_request":{"url":"x"}}]`,
		}
		issues, err := f.listIssuesCreatedBy(ctx, repo, 42, "app[bot]", since)
		if err != nil {
			t.Fatalf("listIssuesCreatedBy: %v", err)
		}
		want := []filedIssue{
			{Number: 12, AuthorID: 7, AuthorType: "Bot", CreatedAt: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)},
			{Number: 13, AuthorID: 8, AuthorType: "User", CreatedAt: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC), PullRequest: true},
		}
		if !slices.Equal(issues, want) {
			t.Fatalf("issues = %+v, want %+v", issues, want)
		}
		wantQuery := map[string]string{
			"state": "all", "creator": "app[bot]", "since": "2026-09-02T11:00:00Z", "per_page": "100",
		}
		if fmt.Sprint(fake.listQuery) != fmt.Sprint(wantQuery) {
			t.Fatalf("listing query = %v, want %v", fake.listQuery, wantQuery)
		}
	})
	t.Run("rejects malformed next query before following it", func(t *testing.T) {
		t.Parallel()
		f, fake, repo := newFilingForgeFake(t)
		fake.pages = []string{`[` + filingIssueJSON + `]`, `[]`}
		fake.malformedNextQuery = "state=evil%ZZ"
		issues, err := f.listIssuesCreatedBy(ctx, repo, 42, "app[bot]", since)
		if err == nil || issues != nil {
			t.Fatalf("issues = %+v, err = %v, want a failed read", issues, err)
		}
	})
	t.Run("fails whole on an element it cannot judge", func(t *testing.T) {
		t.Parallel()
		f, fake, repo := newFilingForgeFake(t)
		fake.pages = []string{`[` + filingIssueJSON + `,` + strings.Replace(filingIssueJSON, `"user":`, `"unused":`, 1) + `]`}
		issues, err := f.listIssuesCreatedBy(ctx, repo, 42, "app[bot]", since)
		if err == nil || issues != nil || strings.Contains(err.Error(), "SECRET-TITLE") {
			t.Fatalf("issues = %+v, err = %v, want a failed read that quotes no issue text", issues, err)
		}
	})
	t.Run("fails whole on a page that is not a list", func(t *testing.T) {
		t.Parallel()
		f, fake, repo := newFilingForgeFake(t)
		fake.pages = []string{`[` + filingIssueJSON + `]`, `{"message":"Server Error"}`}
		if issues, err := f.listIssuesCreatedBy(ctx, repo, 42, "app[bot]", since); err == nil || issues != nil {
			t.Fatalf("issues = %+v, err = %v, want a failed read", issues, err)
		}
	})
}

func TestForgeGetMilestoneNumber(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		milestones string
		want       int
	}{
		"exact title":     {`[{"number":3,"title":"1A"},{"number":7,"title":"1B"}]`, 7},
		"no such title":   {`[{"number":3,"title":"1A"}]`, 0},
		"case differs":    {`[{"number":7,"title":"1b"}]`, 0},
		"duplicate title": {`[{"number":7,"title":"1B"},{"number":8,"title":"1B"}]`, 0},
		"no number":       {`[{"title":"1B"}]`, 0},
		"no milestones":   {`[]`, 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, fake, repo := newFilingForgeFake(t)
			fake.milestones = tc.milestones
			number, err := f.getMilestoneNumber(context.Background(), repo, "1B")
			if tc.want != 0 {
				if err != nil || number != tc.want {
					t.Fatalf("milestone = %d, %v, want %d", number, err, tc.want)
				}
				return
			}
			if !errors.Is(err, errMilestoneUnresolved) || number != 0 {
				t.Fatalf("milestone = %d, %v, want errMilestoneUnresolved", number, err)
			}
		})
	}
}
