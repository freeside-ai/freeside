package publish_test

import (
	"context"
	"testing"
)

// TestReconcileLabelIssuesConditional proves the labeled open-issue list is
// read, rides If-None-Match with no churn while unchanged, and re-reads when
// the labeled set changes.
func TestReconcileLabelIssuesConditional(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	gh.issues[7] = fakeIssue{State: "open", Labels: []string{"freeside", "bug"}}
	gh.issues[9] = fakeIssue{State: "open", Labels: []string{"freeside"}}
	gh.issues[11] = fakeIssue{State: "open", Labels: []string{"docs"}}       // wrong label
	gh.issues[13] = fakeIssue{State: "closed", Labels: []string{"freeside"}} // closed
	r := newTestReconciler(t, gh)

	first, err := r.ReconcileLabelIssues(context.Background(), testRepo, "freeside")
	if err != nil {
		t.Fatalf("first ReconcileLabelIssues: %v", err)
	}
	if first.NotModified {
		t.Error("first observation reported NotModified")
	}
	if len(first.Issues) != 2 {
		t.Fatalf("labeled open issues = %+v, want issues 7 and 9", first.Issues)
	}
	if first.Issues[0].Number != 7 || !first.Issues[0].HasLabel || first.Issues[0].State != "open" {
		t.Errorf("issue[0] = %+v", first.Issues[0])
	}
	if first.Issues[1].Number != 9 || !first.Issues[1].HasLabel {
		t.Errorf("issue[1] = %+v", first.Issues[1])
	}
	if first.RepositoryID != testRepoID {
		t.Errorf("observed repository id = %d, want %d", first.RepositoryID, testRepoID)
	}

	second, err := r.ReconcileLabelIssues(context.Background(), testRepo, "freeside")
	if err != nil {
		t.Fatalf("second ReconcileLabelIssues: %v", err)
	}
	if !second.NotModified {
		t.Error("unchanged labeled set did not report NotModified")
	}
	if len(second.Issues) != 2 || second.Issues[0] != first.Issues[0] || second.Issues[1] != first.Issues[1] {
		t.Errorf("304 churned the observation: %+v vs %+v", second.Issues, first.Issues)
	}
	if conditionalRequests(gh) != 1 {
		t.Errorf("second poll did not ride If-None-Match: %v", gh.requestLog())
	}

	// The label is removed from issue 7 and added to a new issue 15.
	gh.issues[7] = fakeIssue{State: "open", Labels: []string{"bug"}}
	gh.issues[15] = fakeIssue{State: "open", Labels: []string{"freeside"}}
	gh.labelIssueRevs["freeside"]++
	third, err := r.ReconcileLabelIssues(context.Background(), testRepo, "freeside")
	if err != nil {
		t.Fatalf("third ReconcileLabelIssues: %v", err)
	}
	if third.NotModified {
		t.Error("a changed labeled set still reported NotModified")
	}
	if len(third.Issues) != 2 || third.Issues[0].Number != 9 || third.Issues[1].Number != 15 {
		t.Errorf("labeled set after relabel = %+v, want issues 9 and 15", third.Issues)
	}
}

// TestReconcileLabelIssuesMultiPageDropsValidator proves a labeled-open set that
// spans pages is never trusted through a first-page 304: the validator is
// dropped so every poll re-reads all pages unconditionally. A first-page ETag
// cannot detect a change on a later page (a departed or newly-labeled older
// issue), so for the correctness-critical intake scan the conditional shortcut
// is disabled once the list paginates.
func TestReconcileLabelIssuesMultiPageDropsValidator(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	gh.labelIssuesPageSize = 2
	gh.issues[7] = fakeIssue{State: "open", Labels: []string{"freeside"}}
	gh.issues[9] = fakeIssue{State: "open", Labels: []string{"freeside"}}
	gh.issues[11] = fakeIssue{State: "open", Labels: []string{"freeside"}}
	r := newTestReconciler(t, gh)

	first, err := r.ReconcileLabelIssues(context.Background(), testRepo, "freeside")
	if err != nil {
		t.Fatalf("first ReconcileLabelIssues: %v", err)
	}
	if first.NotModified || len(first.Issues) != 3 {
		t.Fatalf("first multi-page observation = %+v", first)
	}

	second, err := r.ReconcileLabelIssues(context.Background(), testRepo, "freeside")
	if err != nil {
		t.Fatalf("second ReconcileLabelIssues: %v", err)
	}
	if second.NotModified {
		t.Error("a multi-page labeled set must not ride a first-page 304")
	}
	if len(second.Issues) != 3 {
		t.Fatalf("second multi-page observation = %+v", second.Issues)
	}
	if conditionalRequests(gh) != 0 {
		t.Errorf("multi-page poll sent a first-page validator: %v", gh.requestLog())
	}

	// A later-page departure (issue 11 loses the label) is observed on the next
	// poll even though the first page (7, 9) is unchanged and its rev is not
	// bumped -- exactly the change a first-page 304 would have hidden.
	gh.issues[11] = fakeIssue{State: "open", Labels: []string{"bug"}}
	third, err := r.ReconcileLabelIssues(context.Background(), testRepo, "freeside")
	if err != nil {
		t.Fatalf("third ReconcileLabelIssues: %v", err)
	}
	if third.NotModified || len(third.Issues) != 2 ||
		third.Issues[0].Number != 7 || third.Issues[1].Number != 9 {
		t.Fatalf("later-page departure not observed: %+v", third.Issues)
	}
}

// TestReconcileLabelIssuesRefusesPullRequests proves a pull request carrying
// the initiator label is dropped from the observation: the issues API serves
// both, and admitting a PR as an intake subject would let a PR impersonate an
// issue on the intake path (§5.13).
func TestReconcileLabelIssuesRefusesPullRequests(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	gh.issues[7] = fakeIssue{State: "open", Labels: []string{"freeside"}}
	gh.issues[8] = fakeIssue{State: "open", Labels: []string{"freeside"}, IsPR: true}
	r := newTestReconciler(t, gh)

	obs, err := r.ReconcileLabelIssues(context.Background(), testRepo, "freeside")
	if err != nil {
		t.Fatalf("ReconcileLabelIssues: %v", err)
	}
	if len(obs.Issues) != 1 || obs.Issues[0].Number != 7 {
		t.Errorf("PR was not refused from the labeled set: %+v", obs.Issues)
	}
}

// TestReconcileLabelIssuesEmpty proves a label with no matching open issues is
// a legitimate empty observation, not an error.
func TestReconcileLabelIssuesEmpty(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	r := newTestReconciler(t, gh)

	obs, err := r.ReconcileLabelIssues(context.Background(), testRepo, "freeside")
	if err != nil {
		t.Fatalf("ReconcileLabelIssues: %v", err)
	}
	if obs.NotModified || len(obs.Issues) != 0 {
		t.Errorf("empty labeled set = %+v", obs)
	}
}

// TestEvictLabelIssuesForcesUnconditionalRefetch proves eviction defeats the
// 304 suppression that would otherwise strand an un-persisted observation after
// a durable intake write fails.
func TestEvictLabelIssuesForcesUnconditionalRefetch(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	gh.issues[7] = fakeIssue{State: "open", Labels: []string{"freeside"}}
	r := newTestReconciler(t, gh)

	if _, err := r.ReconcileLabelIssues(context.Background(), testRepo, "freeside"); err != nil {
		t.Fatalf("first ReconcileLabelIssues: %v", err)
	}
	second, err := r.ReconcileLabelIssues(context.Background(), testRepo, "freeside")
	if err != nil {
		t.Fatalf("second ReconcileLabelIssues: %v", err)
	}
	if !second.NotModified {
		t.Fatal("unchanged labeled set did not ride NotModified before eviction")
	}
	r.EvictLabelIssues(testRepo, "freeside")
	third, err := r.ReconcileLabelIssues(context.Background(), testRepo, "freeside")
	if err != nil {
		t.Fatalf("third ReconcileLabelIssues: %v", err)
	}
	if third.NotModified {
		t.Error("eviction did not force an unconditional refetch")
	}
	if len(third.Issues) != 1 || third.Issues[0].Number != 7 {
		t.Errorf("refetched labeled set = %+v", third.Issues)
	}
}
