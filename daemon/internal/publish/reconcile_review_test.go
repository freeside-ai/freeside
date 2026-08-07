package publish_test

import (
	"context"
	"testing"
)

func intptr(n int) *int { return &n }

// TestReconcilePullReviewActivityConditional proves the three review
// sub-resources are read, then ride If-None-Match with no churn while
// unchanged, and re-read a changed sub-resource while the unchanged siblings
// stay on their 304.
func TestReconcilePullReviewActivityConditional(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	gh.reviews[7] = []fakeReview{{
		ID: 900100, Login: "chatgpt-codex-connector", State: "COMMENTED",
		Body: "findings", CommitID: "cafebabe", SubmittedAt: "2026-01-02T05:04:05Z",
	}}
	gh.reviewComments[7] = []fakeReviewComment{{
		ID: 800200, ReviewID: 900100, Login: "chatgpt-codex-connector",
		Path: "daemon/main.go", Line: intptr(42), Body: "P2: unchecked error",
		CommitID: "cafebabe", CreatedAt: "2026-01-02T05:04:05Z",
	}}
	gh.reactions[7] = []fakeReaction{{
		ID: 700300, Login: "someone-else", Content: "heart", CreatedAt: "2026-01-02T05:00:00Z",
	}}
	r := newTestReconciler(t, gh)

	first, err := r.ReconcilePullReviewActivity(context.Background(), testRepo, 7)
	if err != nil {
		t.Fatalf("first ReconcilePullReviewActivity: %v", err)
	}
	if first.NotModified {
		t.Error("first observation reported NotModified")
	}
	if len(first.Reviews) != 1 || first.Reviews[0].ID != 900100 ||
		first.Reviews[0].CommitID != "cafebabe" || first.Reviews[0].SubmittedAt.IsZero() {
		t.Errorf("reviews = %+v", first.Reviews)
	}
	if len(first.Comments) != 1 || first.Comments[0].ReviewID != 900100 ||
		first.Comments[0].Line != 42 || first.Comments[0].Path != "daemon/main.go" {
		t.Errorf("comments = %+v", first.Comments)
	}
	if len(first.Reactions) != 1 || first.Reactions[0].Content != "heart" {
		t.Errorf("reactions = %+v", first.Reactions)
	}

	second, err := r.ReconcilePullReviewActivity(context.Background(), testRepo, 7)
	if err != nil {
		t.Fatalf("second ReconcilePullReviewActivity: %v", err)
	}
	if !second.NotModified {
		t.Error("unchanged review activity did not report NotModified")
	}
	second.NotModified = false
	if len(second.Reviews) != 1 || second.Reviews[0] != first.Reviews[0] ||
		len(second.Comments) != 1 || second.Comments[0] != first.Comments[0] ||
		len(second.Reactions) != 1 || second.Reactions[0] != first.Reactions[0] {
		t.Errorf("304 churned the observation: %+v vs %+v", second, first)
	}
	if conditionalRequests(gh) != 3 {
		t.Errorf("second poll did not ride If-None-Match on all three sub-resources: %v", gh.requestLog())
	}

	// A new reaction (the clean-pass signal) changes only that sub-resource.
	gh.reactions[7] = append(gh.reactions[7], fakeReaction{
		ID: 700400, Login: "chatgpt-codex-connector", Content: "+1", CreatedAt: "2026-01-02T06:00:00Z",
	})
	gh.reactionRevs[7]++
	third, err := r.ReconcilePullReviewActivity(context.Background(), testRepo, 7)
	if err != nil {
		t.Fatalf("third ReconcilePullReviewActivity: %v", err)
	}
	if third.NotModified {
		t.Error("a changed sub-resource still reported NotModified")
	}
	if len(third.Reactions) != 2 {
		t.Errorf("reactions after append = %+v", third.Reactions)
	}
	// The unchanged reviews and comments stayed on their 304 and kept their
	// cached values.
	if len(third.Reviews) != 1 || third.Reviews[0] != first.Reviews[0] ||
		len(third.Comments) != 1 || third.Comments[0] != first.Comments[0] {
		t.Errorf("unchanged siblings churned: reviews=%+v comments=%+v", third.Reviews, third.Comments)
	}
}

// TestEvictPullReviewActivityForcesUnconditionalRefetch proves eviction defeats
// the 304 suppression that would otherwise strand un-persisted observations:
// after the active-resource reconciler evicts a PR's review-activity cache
// (its durable append failed), the next observation re-fetches every
// sub-resource unconditionally and rebuilds the lists rather than riding a
// NotModified (issue #497).
func TestEvictPullReviewActivityForcesUnconditionalRefetch(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	gh.reviews[7] = []fakeReview{{
		ID: 900100, Login: "chatgpt-codex-connector", State: "COMMENTED",
		Body: "findings", CommitID: "cafebabe", SubmittedAt: "2026-01-02T05:04:05Z",
	}}
	gh.reviewComments[7] = []fakeReviewComment{{
		ID: 800200, ReviewID: 900100, Login: "chatgpt-codex-connector",
		Path: "daemon/main.go", Line: intptr(42), Body: "P2: unchecked error",
		CommitID: "cafebabe", CreatedAt: "2026-01-02T05:04:05Z",
	}}
	r := newTestReconciler(t, gh)

	if _, err := r.ReconcilePullReviewActivity(context.Background(), testRepo, 7); err != nil {
		t.Fatalf("first ReconcilePullReviewActivity: %v", err)
	}
	// A second poll without eviction rides If-None-Match and 304.
	second, err := r.ReconcilePullReviewActivity(context.Background(), testRepo, 7)
	if err != nil {
		t.Fatalf("second ReconcilePullReviewActivity: %v", err)
	}
	if !second.NotModified {
		t.Fatal("unchanged activity did not ride NotModified before eviction")
	}
	condAfterSecond := conditionalRequests(gh)

	// Eviction drops the validators, so the next poll re-fetches unconditionally
	// and rebuilds the full lists rather than reporting NotModified.
	r.EvictPullReviewActivity(testRepo, 7)
	third, err := r.ReconcilePullReviewActivity(context.Background(), testRepo, 7)
	if err != nil {
		t.Fatalf("third ReconcilePullReviewActivity: %v", err)
	}
	if third.NotModified {
		t.Fatal("post-eviction poll rode NotModified instead of re-fetching")
	}
	if len(third.Reviews) != 1 || third.Reviews[0].ID != 900100 ||
		len(third.Comments) != 1 || third.Comments[0].ID != 800200 {
		t.Fatalf("post-eviction poll did not rebuild the lists: %+v", third)
	}
	if got := conditionalRequests(gh); got != condAfterSecond {
		t.Fatalf("post-eviction poll issued %d new conditional requests, want 0 (unconditional re-fetch)",
			got-condAfterSecond)
	}
}

// TestReconcilePullReviewActivitySkipsPending proves a pending (never
// submitted) review is not observed.
func TestReconcilePullReviewActivitySkipsPending(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	gh.reviews[7] = []fakeReview{
		{ID: 1, Login: "chatgpt-codex-connector", State: "PENDING", SubmittedAt: ""},
		{ID: 2, Login: "chatgpt-codex-connector", State: "COMMENTED", CommitID: "cafebabe", SubmittedAt: "2026-01-02T05:04:05Z"},
	}
	r := newTestReconciler(t, gh)
	obs, err := r.ReconcilePullReviewActivity(context.Background(), testRepo, 7)
	if err != nil {
		t.Fatalf("ReconcilePullReviewActivity: %v", err)
	}
	if len(obs.Reviews) != 1 || obs.Reviews[0].ID != 2 {
		t.Errorf("pending review was not skipped: %+v", obs.Reviews)
	}
}

// TestReconcilePullReviewActivityValidation fails fast on a bad repo or number.
func TestReconcilePullReviewActivityValidation(t *testing.T) {
	t.Parallel()
	r := newTestReconciler(t, newFakeGitHub(t))
	if _, err := r.ReconcilePullReviewActivity(context.Background(), "no-slash", 7); err == nil {
		t.Error("bad repo did not error")
	}
	if _, err := r.ReconcilePullReviewActivity(context.Background(), testRepo, 0); err == nil {
		t.Error("non-positive number did not error")
	}
}
