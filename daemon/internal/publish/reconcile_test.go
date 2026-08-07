package publish_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

func newTestReconciler(t *testing.T, gh *fakeGitHub) *publish.Reconciler {
	t.Helper()
	srv := gh.server()
	return publish.NewReconciler(testTokenSource(), srv.Client(), srv.URL)
}

const testRepo = "freeside-ai/evidence-repo"

// conditionalRequests counts logged requests that carried
// If-None-Match.
func conditionalRequests(gh *fakeGitHub) int {
	n := 0
	for _, r := range gh.requestLog() {
		if strings.HasSuffix(r, " if-none-match") {
			n++
		}
	}
	return n
}

// TestReconcileRefConditional (issue #81 acceptance 3): the first poll
// is unconditional and establishes the validator; the second rides
// If-None-Match, is answered 304, and returns the cached observation
// unchanged; a moved ref invalidates the validator and is observed
// fresh.
func TestReconcileRefConditional(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	gh.refs["freeside/publish/abcd"] = testHeadSHA
	r := newTestReconciler(t, gh)

	first, err := r.ReconcileRef(context.Background(), testRepo, "freeside/publish/abcd")
	if err != nil {
		t.Fatalf("first ReconcileRef: %v", err)
	}
	if !first.Exists || first.SHA != testHeadSHA || first.NotModified {
		t.Errorf("first = %+v", first)
	}
	if conditionalRequests(gh) != 0 {
		t.Error("first poll was conditional; nothing was cached yet")
	}

	second, err := r.ReconcileRef(context.Background(), testRepo, "freeside/publish/abcd")
	if err != nil {
		t.Fatalf("second ReconcileRef: %v", err)
	}
	if !second.NotModified {
		t.Error("unchanged ref did not report NotModified")
	}
	if second.Exists != first.Exists || second.SHA != first.SHA {
		t.Errorf("304 churned the observation: %+v vs %+v", second, first)
	}
	if conditionalRequests(gh) != 1 {
		t.Errorf("second poll did not ride If-None-Match: %v", gh.requestLog())
	}

	gh.refs["freeside/publish/abcd"] = testOtherSHA
	third, err := r.ReconcileRef(context.Background(), testRepo, "freeside/publish/abcd")
	if err != nil {
		t.Fatalf("third ReconcileRef: %v", err)
	}
	if third.NotModified || third.SHA != testOtherSHA {
		t.Errorf("moved ref observed as %+v", third)
	}
}

// TestReconcileRefAbsent: ref absence is an observation, and with no
// validator the next poll stays unconditional.
func TestReconcileRefAbsent(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	r := newTestReconciler(t, gh)

	obs, err := r.ReconcileRef(context.Background(), testRepo, "freeside/publish/none")
	if err != nil {
		t.Fatalf("ReconcileRef: %v", err)
	}
	if obs.Exists || obs.NotModified {
		t.Errorf("absent ref observed as %+v", obs)
	}
	if _, err := r.ReconcileRef(context.Background(), testRepo, "freeside/publish/none"); err != nil {
		t.Fatal(err)
	}
	if conditionalRequests(gh) != 0 {
		t.Error("poll after a 404 was conditional; there was no validator")
	}
}

// TestReconcilePullConditional mirrors the ref cycle for a pull
// request: unconditional, then 304 without churn, then a fresh
// observation after the PR changes.
func TestReconcilePullConditional(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	gh.prs = append(gh.prs, fakePR{Number: 7, State: "open", Title: "t", Body: "b", HeadRef: "freeside/publish/abcd", HeadSHA: testHeadSHA})
	r := newTestReconciler(t, gh)

	first, err := r.ReconcilePull(context.Background(), testRepo, 7)
	if err != nil {
		t.Fatalf("first ReconcilePull: %v", err)
	}
	if first.NotModified || first.Number != 7 || first.State != "open" || first.Title != "t" {
		t.Errorf("first = %+v", first)
	}

	second, err := r.ReconcilePull(context.Background(), testRepo, 7)
	if err != nil {
		t.Fatalf("second ReconcilePull: %v", err)
	}
	if !second.NotModified {
		t.Error("unchanged PR did not report NotModified")
	}
	second.NotModified = false
	if second != first {
		t.Errorf("304 churned the observation: %+v vs %+v", second, first)
	}
	if conditionalRequests(gh) != 1 {
		t.Errorf("second poll did not ride If-None-Match: %v", gh.requestLog())
	}

	gh.prs[0].State = "closed"
	gh.prRevs[7]++
	third, err := r.ReconcilePull(context.Background(), testRepo, 7)
	if err != nil {
		t.Fatalf("third ReconcilePull: %v", err)
	}
	if third.NotModified || third.State != "closed" {
		t.Errorf("changed PR observed as %+v", third)
	}
}

func TestEvictResourceCachesForceUnconditionalRefetch(t *testing.T) {
	t.Run("ref", func(t *testing.T) {
		t.Parallel()
		gh := newFakeGitHub(t)
		gh.refs["main"] = testHeadSHA
		r := newTestReconciler(t, gh)

		if _, err := r.ReconcileRef(t.Context(), testRepo, "main"); err != nil {
			t.Fatal(err)
		}
		if obs, err := r.ReconcileRef(t.Context(), testRepo, "main"); err != nil || !obs.NotModified {
			t.Fatalf("cached ReconcileRef = %+v, %v", obs, err)
		}
		conditionalBefore := conditionalRequests(gh)
		r.EvictRef(testRepo, "main")
		if obs, err := r.ReconcileRef(t.Context(), testRepo, "main"); err != nil || obs.NotModified {
			t.Fatalf("post-eviction ReconcileRef = %+v, %v", obs, err)
		}
		if got := conditionalRequests(gh); got != conditionalBefore {
			t.Fatalf("post-eviction ref sent %d conditional requests, want 0", got-conditionalBefore)
		}
		if obs, err := r.ReconcileRef(t.Context(), testRepo, "main"); err != nil || !obs.NotModified {
			t.Fatalf("repopulated ReconcileRef = %+v, %v", obs, err)
		}
	})

	t.Run("pull", func(t *testing.T) {
		t.Parallel()
		gh := newFakeGitHub(t)
		gh.prs = append(gh.prs, fakePR{
			Number: 7, State: "open", HeadRef: "branch", HeadSHA: testHeadSHA,
		})
		r := newTestReconciler(t, gh)

		if _, err := r.ReconcilePull(t.Context(), testRepo, 7); err != nil {
			t.Fatal(err)
		}
		if obs, err := r.ReconcilePull(t.Context(), testRepo, 7); err != nil || !obs.NotModified {
			t.Fatalf("cached ReconcilePull = %+v, %v", obs, err)
		}
		conditionalBefore := conditionalRequests(gh)
		r.EvictPull(testRepo, 7)
		if obs, err := r.ReconcilePull(t.Context(), testRepo, 7); err != nil || obs.NotModified {
			t.Fatalf("post-eviction ReconcilePull = %+v, %v", obs, err)
		}
		if got := conditionalRequests(gh); got != conditionalBefore {
			t.Fatalf("post-eviction pull sent %d conditional requests, want 0", got-conditionalBefore)
		}
		if obs, err := r.ReconcilePull(t.Context(), testRepo, 7); err != nil || !obs.NotModified {
			t.Fatalf("repopulated ReconcilePull = %+v, %v", obs, err)
		}
	})

	t.Run("issue", func(t *testing.T) {
		t.Parallel()
		gh := newFakeGitHub(t)
		gh.issues[443] = fakeIssue{State: "open"}
		r := newTestReconciler(t, gh)

		if _, err := r.ReconcileIssue(t.Context(), testRepo, 443); err != nil {
			t.Fatal(err)
		}
		if obs, err := r.ReconcileIssue(t.Context(), testRepo, 443); err != nil || !obs.NotModified {
			t.Fatalf("cached ReconcileIssue = %+v, %v", obs, err)
		}
		conditionalBefore := conditionalRequests(gh)
		r.EvictIssue(testRepo, 443)
		if obs, err := r.ReconcileIssue(t.Context(), testRepo, 443); err != nil || obs.NotModified {
			t.Fatalf("post-eviction ReconcileIssue = %+v, %v", obs, err)
		}
		if got := conditionalRequests(gh); got != conditionalBefore {
			t.Fatalf("post-eviction issue sent %d conditional requests, want 0", got-conditionalBefore)
		}
		if obs, err := r.ReconcileIssue(t.Context(), testRepo, 443); err != nil || !obs.NotModified {
			t.Fatalf("repopulated ReconcileIssue = %+v, %v", obs, err)
		}
	})
}

// TestReconcilePerResourceValidators (issue #81 acceptance 3, the "no
// global cursor" half): each resource carries its own validator, so
// one resource changing does not disturb another's 304 cycle.
func TestReconcilePerResourceValidators(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	gh.refs["freeside/publish/aaaa"] = testHeadSHA
	gh.refs["freeside/publish/bbbb"] = testHeadSHA
	r := newTestReconciler(t, gh)

	for _, branch := range []string{"freeside/publish/aaaa", "freeside/publish/bbbb"} {
		if _, err := r.ReconcileRef(context.Background(), testRepo, branch); err != nil {
			t.Fatal(err)
		}
	}

	// One resource moves; the other's cached validator must still 304.
	gh.refs["freeside/publish/aaaa"] = testOtherSHA
	moved, err := r.ReconcileRef(context.Background(), testRepo, "freeside/publish/aaaa")
	if err != nil {
		t.Fatal(err)
	}
	if moved.NotModified || moved.SHA != testOtherSHA {
		t.Errorf("moved resource observed as %+v", moved)
	}
	steady, err := r.ReconcileRef(context.Background(), testRepo, "freeside/publish/bbbb")
	if err != nil {
		t.Fatal(err)
	}
	if !steady.NotModified {
		t.Error("independent resource lost its validator when a sibling changed")
	}
}

// TestReconcilePullSurfacesRetarget: the observation carries every
// identity-bound coordinate, so an external retarget is visible in the
// next fresh observation instead of being cached behind a new
// validator and then confirmed as "unchanged" by every later 304.
func TestReconcilePullSurfacesRetarget(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	gh.prs = append(gh.prs, fakePR{Number: 7, State: "open", Title: "t", Body: "b", HeadRef: "freeside/publish/abcd", HeadSHA: testHeadSHA})
	r := newTestReconciler(t, gh)

	first, err := r.ReconcilePull(context.Background(), testRepo, 7)
	if err != nil {
		t.Fatal(err)
	}
	if first.BaseRef != "main" || first.BaseRepo != testRepo || first.HeadRef != "freeside/publish/abcd" || first.HeadRepo != testRepo {
		t.Errorf("first observation missing coordinates: %+v", first)
	}

	gh.prs[0].BaseRef = "release"
	gh.prRevs[7]++
	second, err := r.ReconcilePull(context.Background(), testRepo, 7)
	if err != nil {
		t.Fatal(err)
	}
	if second.NotModified || second.BaseRef != "release" {
		t.Errorf("retarget not surfaced: %+v", second)
	}

	third, err := r.ReconcilePull(context.Background(), testRepo, 7)
	if err != nil {
		t.Fatal(err)
	}
	if !third.NotModified || third.BaseRef != "release" {
		t.Errorf("post-retarget 304 lost the coordinates: %+v", third)
	}
}

// TestReconcileRefRejectsWrongRefName: a ref observation naming a
// different ref must not be attributed to the requested branch.
func TestReconcileRefRejectsWrongRefName(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	gh.refs["freeside/publish/abcd"] = testHeadSHA
	gh.mangleRefName = true
	r := newTestReconciler(t, gh)

	if _, err := r.ReconcileRef(context.Background(), testRepo, "freeside/publish/abcd"); err == nil {
		t.Error("wrong-ref response accepted, want error")
	}
}

// TestReconcileRefusesUnsolicited304: a 304 answers the validator we
// sent; a server answering 304 to an unconditional request must not
// have the reconciler fabricate a "confirmed" observation out of an
// empty cache.
func TestReconcileRefusesUnsolicited304(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	t.Cleanup(srv.Close)
	r := publish.NewReconciler(testTokenSource(), srv.Client(), srv.URL)

	if _, err := r.ReconcileRef(context.Background(), testRepo, "freeside/publish/abcd"); err == nil {
		t.Error("unsolicited ref 304 accepted, want error")
	}
	if _, err := r.ReconcilePull(context.Background(), testRepo, 7); err == nil {
		t.Error("unsolicited pull 304 accepted, want error")
	}
}

// TestReconcileValidation covers fail-fast argument checks.
func TestReconcileValidation(t *testing.T) {
	t.Parallel()
	r := newTestReconciler(t, newFakeGitHub(t))
	if _, err := r.ReconcileRef(context.Background(), "not-a-repo", "branch"); err == nil {
		t.Error("bad repo accepted, want error")
	}
	if _, err := r.ReconcileRef(context.Background(), testRepo, ""); err == nil {
		t.Error("empty branch accepted, want error")
	}
	if _, err := r.ReconcilePull(context.Background(), testRepo, 0); err == nil {
		t.Error("pull number 0 accepted, want error")
	}
}

// TestReconcilePullMergeState (§5.18 capture, issue #443): a merged PR's
// observation carries the merged bit, its merge commit, and the observed
// base-repository identity; an unmerged PR never surfaces GitHub's
// test-merge commit as a fact.
func TestReconcilePullMergeState(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	gh.prs = []fakePR{
		{
			Number: 7, State: "closed", HeadRef: "feat/x", HeadSHA: testHeadSHA,
			BaseRepoID: 84958515,
			MergedAt:   "2026-01-02T03:04:05Z", MergeCommitSHA: testOtherSHA,
		},
		{
			Number: 8, State: "open", HeadRef: "feat/y", HeadSHA: testHeadSHA,
			MergeCommitSHA: testOtherSHA,
		}, // GitHub's test-merge commit
	}
	r := newTestReconciler(t, gh)

	merged, err := r.ReconcilePull(context.Background(), testRepo, 7)
	if err != nil {
		t.Fatalf("ReconcilePull(7): %v", err)
	}
	if !merged.Merged || merged.MergeCommitSHA != testOtherSHA || merged.BaseRepoID != 84958515 {
		t.Errorf("merged PR observed as %+v", merged)
	}

	unmerged, err := r.ReconcilePull(context.Background(), testRepo, 8)
	if err != nil {
		t.Fatalf("ReconcilePull(8): %v", err)
	}
	if unmerged.Merged || unmerged.MergeCommitSHA != "" {
		t.Errorf("unmerged PR surfaced a merge commit: %+v", unmerged)
	}
}

// TestReconcileIssueConditional: the issue observation is conditional like
// the pull's, and a closed issue's observation carries the closing commit
// of its latest closed event ("" for a manual close).
func TestReconcileIssueConditional(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	gh.issues[443] = fakeIssue{State: "open"}
	r := newTestReconciler(t, gh)

	first, err := r.ReconcileIssue(context.Background(), testRepo, 443)
	if err != nil {
		t.Fatalf("first ReconcileIssue: %v", err)
	}
	if first.State != "open" || first.ClosedByCommitSHA != "" || first.NotModified {
		t.Errorf("first = %+v", first)
	}

	second, err := r.ReconcileIssue(context.Background(), testRepo, 443)
	if err != nil {
		t.Fatalf("second ReconcileIssue: %v", err)
	}
	if !second.NotModified || second.State != "open" {
		t.Errorf("second = %+v", second)
	}

	commit := testOtherSHA
	gh.mu.Lock()
	gh.issues[443] = fakeIssue{State: "closed", Events: []fakeIssueEvent{
		{Event: "labeled"},
		{Event: "closed"}, // reopened later, no attribution
		{Event: "reopened"},
		{Event: "closed", CommitID: &commit}, // the operative closure
	}}
	gh.issueRevs[443]++
	gh.mu.Unlock()

	third, err := r.ReconcileIssue(context.Background(), testRepo, 443)
	if err != nil {
		t.Fatalf("third ReconcileIssue: %v", err)
	}
	if third.State != "closed" || third.ClosedByCommitSHA != commit {
		t.Errorf("third = %+v", third)
	}

	gh.mu.Lock()
	gh.issues[444] = fakeIssue{State: "closed", Events: []fakeIssueEvent{{Event: "closed"}}}
	gh.mu.Unlock()
	manual, err := r.ReconcileIssue(context.Background(), testRepo, 444)
	if err != nil {
		t.Fatalf("ReconcileIssue(444): %v", err)
	}
	if manual.State != "closed" || manual.ClosedByCommitSHA != "" {
		t.Errorf("manually closed issue observed as %+v", manual)
	}
}

// TestReconcileIssueEventPagination: the closing-commit walk follows
// rel="next" pages, so the latest closed event on a later page wins.
func TestReconcileIssueEventPagination(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	stale := testHeadSHA
	commit := testOtherSHA
	gh.issues[443] = fakeIssue{State: "closed", Events: []fakeIssueEvent{
		{Event: "closed", CommitID: &stale},
		{Event: "reopened"},
		{Event: "labeled"},
		{Event: "closed", CommitID: &commit},
	}}
	gh.issueEventsPageSize = 2
	r := newTestReconciler(t, gh)

	obs, err := r.ReconcileIssue(context.Background(), testRepo, 443)
	if err != nil {
		t.Fatalf("ReconcileIssue: %v", err)
	}
	if obs.ClosedByCommitSHA != commit {
		t.Errorf("ClosedByCommitSHA = %q, want the last page's closed event %q", obs.ClosedByCommitSHA, commit)
	}
}

// TestReconcileIssueRejectsPullRequest: a bound issue number that is
// secretly a pull request fails the observation, so one resource cannot
// impersonate the other.
func TestReconcileIssueRejectsPullRequest(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	gh.issues[450] = fakeIssue{State: "open", IsPR: true}
	r := newTestReconciler(t, gh)

	if _, err := r.ReconcileIssue(context.Background(), testRepo, 450); err == nil {
		t.Error("a PR observed through the issue surface, want error")
	}
}
