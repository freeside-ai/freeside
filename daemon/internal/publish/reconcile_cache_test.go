package publish

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type cacheTestTokenSource struct{}

func (cacheTestTokenSource) Token(context.Context, string) (InstallationToken, error) {
	return InstallationToken{Token: Secret("fixture-token")}, nil
}

type cacheTestRoundTripFunc func(*http.Request) (*http.Response, error)

func (f cacheTestRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestReconcilerEvictorsDropOnlyTheSelectedResource(t *testing.T) {
	const (
		repo    = "owner/repo"
		sibling = "owner/sibling"
	)
	r := &Reconciler{
		refs: map[string]refCacheEntry{
			refKey(repo, "main"):       {},
			refKey(sibling, "release"): {},
		},
		pulls: map[string]pullCacheEntry{
			pullKey(repo, 7):    {},
			pullKey(sibling, 8): {},
		},
		issues: map[string]issueCacheEntry{
			issueKey(repo, 7):    {},
			issueKey(sibling, 8): {},
		},
		reviewActivity: map[string]reviewActivityCacheEntry{
			reviewActivityKey(repo, 7):    {},
			reviewActivityKey(sibling, 8): {},
		},
	}

	r.EvictRef(repo, "main")
	r.EvictPull(repo, 7)
	r.EvictIssue(repo, 7)
	r.EvictPullReviewActivity(repo, 7)

	if len(r.refs) != 1 || len(r.pulls) != 1 || len(r.issues) != 1 || len(r.reviewActivity) != 1 {
		t.Fatalf("cache sizes after eviction = refs %d pulls %d issues %d review %d, want one sibling each",
			len(r.refs), len(r.pulls), len(r.issues), len(r.reviewActivity))
	}
	if _, ok := r.refs[refKey(sibling, "release")]; !ok {
		t.Error("ref sibling was evicted")
	}
	if _, ok := r.pulls[pullKey(sibling, 8)]; !ok {
		t.Error("pull sibling was evicted")
	}
	if _, ok := r.issues[issueKey(sibling, 8)]; !ok {
		t.Error("issue sibling was evicted")
	}
	if _, ok := r.reviewActivity[reviewActivityKey(sibling, 8)]; !ok {
		t.Error("review-activity sibling was evicted")
	}
}

func TestReconcilerEvictionWinsOverInFlightFill(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	client := &http.Client{Transport: cacheTestRoundTripFunc(func(*http.Request) (*http.Response, error) {
		close(started)
		<-release
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"ETag": []string{`"pull-v1"`}},
			Body: io.NopCloser(strings.NewReader(`{
				"number":7,"state":"open","title":"title","body":"body",
				"head":{"ref":"branch","sha":"cafed00d","repo":{"full_name":"owner/repo"}},
				"base":{"ref":"main","repo":{"id":42,"full_name":"owner/repo"}}
			}`)),
		}, nil
	})}
	r := NewReconciler(cacheTestTokenSource{}, client, "https://example.invalid")
	done := make(chan error, 1)
	go func() {
		_, err := r.ReconcilePull(t.Context(), "owner/repo", 7)
		done <- err
	}()

	<-started
	r.EvictPull("owner/repo", 7)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.pulls[pullKey("owner/repo", 7)]; ok {
		t.Fatal("in-flight response refilled the evicted pull cache")
	}
}
