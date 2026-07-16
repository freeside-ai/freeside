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

// TestReconcilePerResourceValidators (issue #81 acceptance 3, the "no
// global cursor" half): each resource carries its own validator, so
// one resource changing does not disturb another's 304 cycle.
func TestReconcilePerResourceValidators(t *testing.T) {
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
