package publish_test

import (
	"context"
	"fmt"
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

const (
	testRepo              = "freeside-ai/evidence-repo"
	testClosedEventNodeID = "issue-event-443-0"
)

func graphQLIssueClosureResponse(issueNumber int, eventNodeID, closer string) string {
	return fmt.Sprintf(
		`{"data":{"repository":{"issue":{"number":%d,"timelineItems":{"nodes":[{"__typename":"ClosedEvent","id":%q,"closer":%s}]}}}}}`,
		issueNumber, eventNodeID, closer,
	)
}

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

// TestReconcileIssuePRBodyClosureAttribution observes the merge commit of the
// PR that actually closed an issue through a body keyword, then retains the
// attributed observation across the issue resource's normal 304 path.
func TestReconcileIssuePRBodyClosureAttribution(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	gh.issues[443] = fakeIssue{State: "closed", Events: []fakeIssueEvent{{Event: "closed", NodeID: testClosedEventNodeID}}}
	gh.graphqlIssueResponses[443] = fakeGraphQLResponse{Body: graphQLIssueClosureResponse(
		443,
		testClosedEventNodeID,
		`{"__typename":"PullRequest","number":96,"merged":true,"mergeCommit":{"oid":"`+testOtherSHA+`"}}`,
	)}
	r := newTestReconciler(t, gh)

	first, err := r.ReconcileIssue(t.Context(), testRepo, 443)
	if err != nil {
		t.Fatalf("first ReconcileIssue: %v", err)
	}
	if first.State != "closed" || first.ClosedByCommitSHA != testOtherSHA || first.NotModified {
		t.Errorf("first = %+v", first)
	}
	second, err := r.ReconcileIssue(t.Context(), testRepo, 443)
	if err != nil {
		t.Fatalf("second ReconcileIssue: %v", err)
	}
	if !second.NotModified || second.ClosedByCommitSHA != testOtherSHA {
		t.Errorf("second = %+v", second)
	}
	if got := gh.requestLog(); strings.Count(strings.Join(got, "\n"), "POST /graphql") != 1 {
		t.Errorf("GraphQL request count != 1: %v", got)
	}
}

// TestReconcileIssueManualClosureIsNotCached proves a definitive manual close
// cannot pin an empty attribution behind the issue ETag. Each poll retries the
// full observation unconditionally, while the fact itself remains empty.
func TestReconcileIssueManualClosureIsNotCached(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	gh.issues[443] = fakeIssue{State: "closed", Events: []fakeIssueEvent{{Event: "closed"}}}
	r := newTestReconciler(t, gh)

	for i := 0; i < 2; i++ {
		obs, err := r.ReconcileIssue(t.Context(), testRepo, 443)
		if err != nil {
			t.Fatalf("ReconcileIssue poll %d: %v", i+1, err)
		}
		if obs.State != "closed" || obs.ClosedByCommitSHA != "" || obs.NotModified {
			t.Errorf("poll %d = %+v", i+1, obs)
		}
	}
	requests := gh.requestLog()
	counts := map[string]int{}
	for _, request := range requests {
		counts[request]++
	}
	if got := counts["GET "+testRepoPath+"/issues/443"]; got != 2 {
		t.Errorf("issue request count = %d, want 2: %v", got, requests)
	}
	if got := counts["GET "+testRepoPath+"/issues/443/events"]; got != 2 {
		t.Errorf("event request count = %d, want 2: %v", got, requests)
	}
	if got := counts["POST /graphql"]; got != 2 {
		t.Errorf("GraphQL request count = %d, want 2: %v", got, requests)
	}
	if conditionalRequests(gh) != 0 {
		t.Errorf("manual closure poll used a cached validator: %v", gh.requestLog())
	}
}

// TestReconcileIssueManualClosureRetriesAttribution proves the uncached empty
// result can adopt a later-attributable closer even when the issue ETag does
// not change, then resumes the normal attributed 304 path.
func TestReconcileIssueManualClosureRetriesAttribution(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	gh.issues[443] = fakeIssue{State: "closed", Events: []fakeIssueEvent{{Event: "closed", NodeID: testClosedEventNodeID}}}
	r := newTestReconciler(t, gh)

	first, err := r.ReconcileIssue(t.Context(), testRepo, 443)
	if err != nil {
		t.Fatalf("first ReconcileIssue: %v", err)
	}
	if first.ClosedByCommitSHA != "" || first.NotModified {
		t.Errorf("first = %+v", first)
	}
	gh.mu.Lock()
	gh.graphqlIssueResponses[443] = fakeGraphQLResponse{Body: graphQLIssueClosureResponse(
		443,
		testClosedEventNodeID,
		`{"__typename":"PullRequest","number":96,"merged":true,"mergeCommit":{"oid":"`+testOtherSHA+`"}}`,
	)}
	gh.mu.Unlock()
	second, err := r.ReconcileIssue(t.Context(), testRepo, 443)
	if err != nil {
		t.Fatalf("second ReconcileIssue: %v", err)
	}
	if second.ClosedByCommitSHA != testOtherSHA || second.NotModified {
		t.Errorf("second = %+v", second)
	}
	third, err := r.ReconcileIssue(t.Context(), testRepo, 443)
	if err != nil {
		t.Fatalf("third ReconcileIssue: %v", err)
	}
	if third.ClosedByCommitSHA != testOtherSHA || !third.NotModified {
		t.Errorf("third = %+v", third)
	}
}

// TestReconcileIssueCommitCloserAttribution covers GraphQL's direct Commit
// closer variant when REST omitted the commit id for that same closed event.
func TestReconcileIssueCommitCloserAttribution(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	gh.issues[443] = fakeIssue{State: "closed", Events: []fakeIssueEvent{{Event: "closed", NodeID: testClosedEventNodeID}}}
	gh.graphqlIssueResponses[443] = fakeGraphQLResponse{Body: graphQLIssueClosureResponse(
		443, testClosedEventNodeID, `{"__typename":"Commit","oid":"`+testOtherSHA+`"}`,
	)}
	r := newTestReconciler(t, gh)

	obs, err := r.ReconcileIssue(t.Context(), testRepo, 443)
	if err != nil {
		t.Fatalf("ReconcileIssue: %v", err)
	}
	if obs.ClosedByCommitSHA != testOtherSHA {
		t.Errorf("ClosedByCommitSHA = %q, want %q", obs.ClosedByCommitSHA, testOtherSHA)
	}
}

// TestReconcileIssueClosureAttributionRejectsUntrustedResponses refutes the
// new returned-object boundary: ambiguous or malformed closer data must fail
// the observation instead of becoming an empty or mismatched completion fact.
func TestReconcileIssueClosureAttributionRejectsUntrustedResponses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		response fakeGraphQLResponse
	}{
		{name: "non-200", response: fakeGraphQLResponse{Status: http.StatusBadGateway}},
		{name: "malformed JSON", response: fakeGraphQLResponse{Body: `{"data":`}},
		{name: "GraphQL errors", response: fakeGraphQLResponse{Body: `{"errors":[{"message":"denied"}],"data":{"repository":null}}`}},
		{name: "null repository", response: fakeGraphQLResponse{Body: `{"data":{"repository":null}}`}},
		{name: "null issue", response: fakeGraphQLResponse{Body: `{"data":{"repository":{"issue":null}}}`}},
		{name: "different issue", response: fakeGraphQLResponse{Body: graphQLIssueClosureResponse(444, testClosedEventNodeID, `null`)}},
		{name: "empty nodes", response: fakeGraphQLResponse{Body: `{"data":{"repository":{"issue":{"number":443,"timelineItems":{"nodes":[]}}}}}`}},
		{name: "null node", response: fakeGraphQLResponse{Body: `{"data":{"repository":{"issue":{"number":443,"timelineItems":{"nodes":[null]}}}}}`}},
		{name: "multiple nodes", response: fakeGraphQLResponse{Body: `{"data":{"repository":{"issue":{"number":443,"timelineItems":{"nodes":[{"id":"first","closer":null},{"id":"second","closer":null}]}}}}}`}},
		{name: "unexpected event type", response: fakeGraphQLResponse{Body: `{"data":{"repository":{"issue":{"number":443,"timelineItems":{"nodes":[{"__typename":"ReopenedEvent","id":"` + testClosedEventNodeID + `","closer":null}]}}}}}`}},
		{name: "different closed event", response: fakeGraphQLResponse{Body: graphQLIssueClosureResponse(443, "older-event", `{"__typename":"PullRequest","number":96,"merged":true,"mergeCommit":{"oid":"`+testOtherSHA+`"}}`)}},
		{name: "omitted closer", response: fakeGraphQLResponse{Body: `{"data":{"repository":{"issue":{"number":443,"timelineItems":{"nodes":[{"__typename":"ClosedEvent","id":"` + testClosedEventNodeID + `"}]}}}}}`}},
		{name: "unexpected closer", response: fakeGraphQLResponse{Body: graphQLIssueClosureResponse(443, testClosedEventNodeID, `{"__typename":"Milestone"}`)}},
		{name: "commit without oid", response: fakeGraphQLResponse{Body: graphQLIssueClosureResponse(443, testClosedEventNodeID, `{"__typename":"Commit"}`)}},
		{name: "pull without number", response: fakeGraphQLResponse{Body: graphQLIssueClosureResponse(443, testClosedEventNodeID, `{"__typename":"PullRequest","merged":true,"mergeCommit":{"oid":"`+testOtherSHA+`"}}`)}},
		{name: "unmerged pull", response: fakeGraphQLResponse{Body: graphQLIssueClosureResponse(443, testClosedEventNodeID, `{"__typename":"PullRequest","number":96,"merged":false,"mergeCommit":{"oid":"`+testOtherSHA+`"}}`)}},
		{name: "pull without merge commit", response: fakeGraphQLResponse{Body: graphQLIssueClosureResponse(443, testClosedEventNodeID, `{"__typename":"PullRequest","number":96,"merged":true,"mergeCommit":null}`)}},
		{name: "pull with empty merge oid", response: fakeGraphQLResponse{Body: graphQLIssueClosureResponse(443, testClosedEventNodeID, `{"__typename":"PullRequest","number":96,"merged":true,"mergeCommit":{"oid":""}}`)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gh := newFakeGitHub(t)
			gh.issues[443] = fakeIssue{State: "closed", Events: []fakeIssueEvent{{Event: "closed", NodeID: testClosedEventNodeID}}}
			gh.graphqlIssueResponses[443] = tt.response
			r := newTestReconciler(t, gh)

			if obs, err := r.ReconcileIssue(t.Context(), testRepo, 443); err == nil {
				t.Errorf("ReconcileIssue = %+v, want returned-object error", obs)
			}
		})
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
	if got := strings.Count(strings.Join(gh.requestLog(), "\n"), "POST /graphql"); got != 0 {
		t.Errorf("direct REST attribution issued %d GraphQL requests: %v", got, gh.requestLog())
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
