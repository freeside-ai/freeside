package publish

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestReviewReplyForgeCalls(t *testing.T) {
	for _, test := range []struct {
		name string
		repo repoRef
		root int64
	}{
		{name: "conversation", repo: repoRef{"owner", "repo"}},
		{name: "inline", repo: repoRef{"owner", "repo"}, root: 42},
		{name: "inline repository named issues", repo: repoRef{"owner", "issues"}, root: 42},
		{name: "inline owner named issues", repo: repoRef{"issues", "repo"}, root: 42},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := fmt.Sprintf("/repos/%s/issues/7/comments", test.repo.path())
			if test.root != 0 {
				path = fmt.Sprintf("/repos/%s/pulls/7/comments/%d/replies", test.repo.path(), test.root)
			}
			sent := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != path {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				var payload struct {
					Body string `json:"body"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.Body != "reply" {
					t.Errorf("payload = %+v, %v", payload, err)
				}
				sent++
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": 99, "user": map[string]any{"id": 123, "type": "Bot"},
					"in_reply_to_id": test.root, "created_at": "2026-10-06T12:00:00Z",
					"issue_url": "http://" + r.Host + "/repos/" + test.repo.path() + "/issues/7",
				})
			}))
			defer server.Close()
			f := newForge(draftTestTokenSource{}, server.Client(), server.URL)
			req, err := f.newReviewReplyRequest(context.Background(), test.repo, 7, test.root, "reply")
			if err != nil || sent != 0 {
				t.Fatalf("build: sent %d, %v", sent, err)
			}
			result, err := f.sendReviewReply(req, test.repo, 7, test.root)
			if err != nil || result.Comment == nil || result.Comment.ID != 99 || result.Comment.InReplyToID != test.root || sent != 1 {
				t.Fatalf("send: %+v, %v", result, err)
			}
		})
	}
}

func TestReviewReplyRejectsIncompleteCommentIdentities(t *testing.T) {
	for _, body := range []string{`{}`, `{"id":1}`, `{"id":1,"user":{"id":2,"type":"Bot"}}`, `{"id":1,"user":{"id":2,"type":"Bot"},"created_at":"invalid"}`} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_, _ = fmt.Fprintf(w, "[%s]", body)
					return
				}
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()
			f := newForge(draftTestTokenSource{}, server.Client(), server.URL)
			repo := repoRef{"owner", "repo"}
			if _, err := f.listConversationComments(context.Background(), repo, 7); err == nil {
				t.Fatal("incomplete listing accepted")
			}
			req, err := f.newReviewReplyRequest(context.Background(), repo, 7, 0, "reply")
			if err != nil {
				t.Fatal(err)
			}
			result, err := f.sendReviewReply(req, repo, 7, 0)
			if got := classifyForgeCreate(result.Status, result.Header, result.Comment != nil, err); got != domain.FollowUpFilingResponseUnproven {
				t.Fatalf("class = %s", got)
			}
		})
	}
}

func TestReviewReplyRejectsPaginationOntoAnotherPullRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/owner/repo/issues/7/comments" {
			w.Header().Set("Link", fmt.Sprintf(`<http://%s/repos/owner/repo/issues/8/comments>; rel="next"`, r.Host))
			_, _ = w.Write([]byte("[]"))
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"id": 99, "user": map[string]any{"id": 123, "type": "Bot"},
			"created_at": "2026-10-06T12:00:00Z",
			"issue_url":  "http://" + r.Host + "/repos/owner/repo/issues/8",
		}})
	}))
	defer server.Close()
	f := newForge(draftTestTokenSource{}, server.Client(), server.URL)
	if _, err := f.listConversationComments(context.Background(), repoRef{"owner", "repo"}, 7); err == nil {
		t.Fatal("wrong-resource next page accepted")
	}
}

// Compare the extracted classifier against the old status table over every
// HTTP status, body presence, transport outcome, and rate-limit header form.
func TestSharedCreateClassifierPreservesFilingDecisions(t *testing.T) {
	for status := 100; status < 600; status++ {
		for _, decoded := range []bool{false, true} {
			for _, header := range []http.Header{nil, {"X-Ratelimit-Remaining": {"0"}}, {"Retry-After": {"1"}}, {"X-Ratelimit-Remaining": {"1"}}} {
				want := domain.FollowUpFilingResponseUnproven
				switch status {
				case 201:
					if decoded {
						want = domain.FollowUpFilingResponseSuccess
					}
				case 400, 401, 404, 410, 422:
					want = domain.FollowUpFilingResponseDefiniteRejection
				case 429:
					want = domain.FollowUpFilingResponseTransientRejection
				case 403:
					want = domain.FollowUpFilingResponseDefiniteRejection
					if header.Get("X-RateLimit-Remaining") == "0" || header.Get("Retry-After") != "" {
						want = domain.FollowUpFilingResponseTransientRejection
					}
				}
				if got := classifyForgeCreate(status, header, decoded, nil); got != want {
					t.Fatalf("status %d: %s != %s", status, got, want)
				}
				if got := classifyForgeCreate(status, header, decoded, context.Canceled); got != domain.FollowUpFilingResponseUnproven {
					t.Fatalf("transport failure: %s", got)
				}
			}
		}
	}
}
