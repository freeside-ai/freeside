package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

type activeResourceCacheTokenSource struct{}

func (activeResourceCacheTokenSource) Token(
	context.Context, string,
) (publish.InstallationToken, error) {
	return publish.InstallationToken{
		Token: publish.Secret("fixture-token"), Repo: "repo",
		ExpiresAt: time.Now().Add(time.Hour),
	}, nil
}

type activeResourceCacheForge struct {
	mu                  sync.Mutex
	pullState           string
	pullRevision        int
	conditionalRequests int
}

func (f *activeResourceCacheForge) serveHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("If-None-Match") != "" {
		f.conditionalRequests++
	}
	etag := `"cache-v1"`
	if r.URL.Path == "/repos/owner/repo/pulls/450" {
		etag = `"pull-v` + strconv.Itoa(f.pullRevision) + `"`
	}
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Type", "application/json")
	var body any
	switch r.URL.Path {
	case "/repos/owner/repo/git/ref/heads/main":
		body = map[string]any{
			"ref": "refs/heads/main", "object": map[string]any{"sha": "cafebabe"},
		}
	case "/repos/owner/repo/pulls/450":
		body = map[string]any{
			"number": 450, "state": f.pullState, "title": "ready", "body": "body",
			"head": map[string]any{
				"ref": "freeside/publish/cache", "sha": "cafed00d",
				"repo": map[string]any{"full_name": "owner/repo"},
			},
			"base": map[string]any{
				"ref": "main", "repo": map[string]any{"id": 424242, "full_name": "owner/repo"},
			},
		}
	case "/repos/owner/repo/issues/443":
		body = map[string]any{"number": 443, "state": "open"}
	case "/repos/owner/repo/pulls/450/reviews",
		"/repos/owner/repo/pulls/450/comments",
		"/repos/owner/repo/issues/450/reactions":
		body = []any{}
	default:
		http.NotFound(w, r)
		return
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		panic(err)
	}
}

func (f *activeResourceCacheForge) reopenPull() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pullState = "open"
	f.pullRevision++
}

func (f *activeResourceCacheForge) closePull() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pullState = "closed"
	f.pullRevision++
}

func (f *activeResourceCacheForge) conditionalCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conditionalRequests
}

func TestActiveResourceReconcileEvictsConcludedCachesAndAllowsResumption(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := capturedRun(t, st)
	forge := &activeResourceCacheForge{pullState: "open", pullRevision: 1}
	server := httptest.NewServer(http.HandlerFunc(forge.serveHTTP))
	t.Cleanup(server.Close)
	cache := publish.NewReconciler(activeResourceCacheTokenSource{}, server.Client(), server.URL)

	if _, err := cache.ReconcileRef(ctx, "owner/repo", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.ReconcilePull(ctx, "owner/repo", 450); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.ReconcileIssue(ctx, "owner/repo", 443); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.ReconcilePullReviewActivity(ctx, "owner/repo", 450); err != nil {
		t.Fatal(err)
	}

	evictions := 0
	reconciler := activeResourceReconciler{
		store: st, pull: cache.ReconcilePull, issue: cache.ReconcileIssue,
		review: cache.ReconcilePullReviewActivity,
		evictConcluded: func(binding domain.ReadyItemPRBinding, boundIssue *int) {
			evictions++
			cache.EvictRef(binding.Repo, binding.BaseRef)
			cache.EvictPull(binding.Repo, binding.PRNumber)
			cache.EvictPullReviewActivity(binding.Repo, binding.PRNumber)
			if boundIssue != nil {
				cache.EvictIssue(binding.Repo, *boundIssue)
			}
		},
		now: func() time.Time { return activeResourceTestTime },
	}
	if failures, err := reconcileActiveResource(&reconciler, ctx); err != nil || len(failures) != 0 {
		t.Fatalf("open Reconcile = %v, %v", failures, err)
	}
	if evictions != 0 {
		t.Fatalf("open resource evictions = %d, want 0", evictions)
	}

	forge.closePull()
	if failures, err := reconcileActiveResource(&reconciler, ctx); err != nil || len(failures) != 0 {
		t.Fatalf("concluding Reconcile = %v, %v", failures, err)
	}
	if evictions != 1 {
		t.Fatalf("concluding resource evictions = %d, want 1", evictions)
	}
	if got := readActiveItem(t, st, item.ID); got.Status != domain.StatusResolved {
		t.Fatalf("concluded item status = %s, want resolved", got.Status)
	}

	forge.reopenPull()
	conditionalBefore := forge.conditionalCount()
	ref, refErr := cache.ReconcileRef(ctx, "owner/repo", "main")
	pull, pullErr := cache.ReconcilePull(ctx, "owner/repo", 450)
	issue, issueErr := cache.ReconcileIssue(ctx, "owner/repo", 443)
	review, reviewErr := cache.ReconcilePullReviewActivity(ctx, "owner/repo", 450)
	if refErr != nil || pullErr != nil || issueErr != nil || reviewErr != nil {
		t.Fatalf("post-eviction reconcile errors = ref %v pull %v issue %v review %v",
			refErr, pullErr, issueErr, reviewErr)
	}
	if ref.NotModified || pull.NotModified || issue.NotModified || review.NotModified {
		t.Fatalf("post-eviction resources rode cache: ref=%+v pull=%+v issue=%+v review=%+v",
			ref, pull, issue, review)
	}
	if pull.State != "open" {
		t.Fatalf("reopened pull observed as %q", pull.State)
	}
	if got := forge.conditionalCount(); got != conditionalBefore {
		t.Fatalf("post-eviction reconcile sent %d conditional requests, want 0", got-conditionalBefore)
	}

	if failures, err := reconcileActiveResource(&reconciler, ctx); err != nil || len(failures) != 0 {
		t.Fatalf("repeated terminal Reconcile = %v, %v", failures, err)
	}
	if evictions != 1 {
		t.Fatalf("repeated terminal resource evictions = %d, want 1", evictions)
	}
	ref, refErr = cache.ReconcileRef(ctx, "owner/repo", "main")
	pull, pullErr = cache.ReconcilePull(ctx, "owner/repo", 450)
	issue, issueErr = cache.ReconcileIssue(ctx, "owner/repo", 443)
	review, reviewErr = cache.ReconcilePullReviewActivity(ctx, "owner/repo", 450)
	if refErr != nil || pullErr != nil || issueErr != nil || reviewErr != nil {
		t.Fatalf("repopulated reconcile errors = ref %v pull %v issue %v review %v",
			refErr, pullErr, issueErr, reviewErr)
	}
	if !ref.NotModified || !pull.NotModified || !issue.NotModified || !review.NotModified {
		t.Fatalf("repopulated resources did not ride cache: ref=%+v pull=%+v issue=%+v review=%+v",
			ref, pull, issue, review)
	}
}
