package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

func externalTestBinding() domain.ReadyItemPRBinding {
	return domain.ReadyItemPRBinding{
		ItemID: "item-ready-active", RunID: "run-active",
		Repo: "owner/repo", RepositoryID: 424242, PRNumber: 450,
		BaseRef: "main", HeadSHA: "cafed00d",
	}
}

// externalReviewActivity is review activity by two identities on the bound
// head "cafed00d": a maintainer's changes-requested review with one inline
// comment, and a bot's single comment.
func externalReviewActivity() publish.PullReviewObservation {
	return publish.PullReviewObservation{
		Reviews: []publish.PullReview{{
			ID: 900100, AuthorID: 4101, AuthorLogin: "maintainer", State: "CHANGES_REQUESTED",
			Body: "Two problems below.", CommitID: "cafed00d", SubmittedAt: activeResourceTestTime,
		}},
		Comments: []publish.PullReviewComment{
			{
				ID: 800200, ReviewID: 900100, AuthorID: 4101, AuthorLogin: "maintainer",
				Path: "daemon/main.go", Line: 42, OriginalLine: 42,
				Body:     "P1: this leaks the handle",
				CommitID: "cafed00d", OriginalCommitID: "cafed00d", CreatedAt: activeResourceTestTime,
			},
			{
				ID: 800201, ReviewID: 900101, AuthorID: 4102, AuthorLogin: "review-bot[bot]",
				Path: "daemon/main.go", Line: 7, OriginalLine: 7,
				Body:     "nit: rename",
				CommitID: "cafed00d", OriginalCommitID: "cafed00d", CreatedAt: activeResourceTestTime.Add(time.Second),
			},
		},
	}
}

func TestBuildExternalFindings(t *testing.T) {
	t.Parallel()
	binding := externalTestBinding()
	at := activeResourceTestTime
	type want struct {
		accountID int64
		login     string
		threadID  string
		head      string
		location  string // "" is a nil location
		severity  domain.FindingSeverity
		rawText   string
		createdAt time.Time
	}
	cases := []struct {
		name        string
		obs         publish.PullReviewObservation
		want        []want
		wantSkipped int
	}{
		{
			name: "review body and inline comment",
			obs:  externalReviewActivity(),
			want: []want{
				{4101, "maintainer", "review/900100", "cafed00d", "", "", "Two problems below.", at},
				{4101, "maintainer", "review_comment/800200", "cafed00d", "daemon/main.go:42", "P1", "P1: this leaks the handle", at},
				{4102, "review-bot[bot]", "review_comment/800201", "cafed00d", "daemon/main.go:7", "", "nit: rename", at.Add(time.Second)},
			},
		},
		{
			// A dismissed review no longer says whether it approved, so the
			// same approval must not turn into a finding once it is dismissed.
			name: "approving and dismissed reviews and empty bodies are not findings",
			obs: publish.PullReviewObservation{
				Reviews: []publish.PullReview{
					{ID: 1, AuthorID: 4101, AuthorLogin: "maintainer", State: "APPROVED", Body: "ship it", CommitID: "cafed00d", SubmittedAt: at},
					{ID: 2, AuthorID: 4101, AuthorLogin: "maintainer", State: "COMMENTED", Body: "", CommitID: "cafed00d", SubmittedAt: at},
					{ID: 3, AuthorID: 4101, AuthorLogin: "maintainer", State: "COMMENTED", Body: " \n\t", CommitID: "cafed00d", SubmittedAt: at},
					{ID: 4, AuthorID: 4101, AuthorLogin: "maintainer", State: "COMMENTED", Body: "a real note", CommitID: "cafed00d", SubmittedAt: at},
					{ID: 5, AuthorID: 4101, AuthorLogin: "maintainer", State: "DISMISSED", Body: "ship it", CommitID: "cafed00d", SubmittedAt: at},
				},
				Comments: []publish.PullReviewComment{
					{ID: 6, AuthorID: 4101, AuthorLogin: "maintainer", Path: "a.go", OriginalLine: 3, Body: " \n", OriginalCommitID: "cafed00d", CreatedAt: at},
				},
			},
			want: []want{{4101, "maintainer", "review/4", "cafed00d", "", "", "a real note", at}},
		},
		{
			name: "a reply joins its thread's first comment",
			obs: publish.PullReviewObservation{Comments: []publish.PullReviewComment{
				{ID: 10, AuthorID: 4101, AuthorLogin: "maintainer", Path: "a.go", OriginalLine: 3, Body: "why?", OriginalCommitID: "cafed00d", CreatedAt: at},
				{ID: 11, InReplyToID: 10, AuthorID: 4102, AuthorLogin: "author", Path: "a.go", OriginalLine: 3, Body: "because", OriginalCommitID: "cafed00d", CreatedAt: at.Add(time.Minute)},
			}},
			want: []want{
				{4101, "maintainer", "review_comment/10", "cafed00d", "a.go:3", "", "why?", at},
				{4102, "author", "review_comment/10", "cafed00d", "a.go:3", "", "because", at.Add(time.Minute)},
			},
		},
		{
			// The forge moved the comment to the new head "feedface" and to a
			// new line there, or dropped its current range. The finding stays
			// on the commit and range the reviewer commented on.
			name: "a re-anchored comment keeps its original commit and range",
			obs: publish.PullReviewObservation{Comments: []publish.PullReviewComment{
				{ID: 20, AuthorID: 4101, AuthorLogin: "maintainer", Path: "a.go", StartLine: 50, Line: 52, OriginalStartLine: 40, OriginalLine: 42, Body: "moved", CommitID: "feedface", OriginalCommitID: "0ldc0de", CreatedAt: at},
				{ID: 21, AuthorID: 4101, AuthorLogin: "maintainer", Path: "a.go", OriginalLine: 9, Body: "outdated", CommitID: "feedface", OriginalCommitID: "0ldc0de", CreatedAt: at},
				{ID: 22, AuthorID: 4101, AuthorLogin: "maintainer", Path: "a.go", Body: "whole file", CommitID: "cafed00d", OriginalCommitID: "cafed00d", CreatedAt: at},
				// A range that starts on the other side of the diff: the start
				// line is from the old file and past the end line.
				{ID: 23, AuthorID: 4101, AuthorLogin: "maintainer", Path: "a.go", OriginalStartLine: 50, OriginalLine: 10, Body: "both sides", OriginalCommitID: "cafed00d", CreatedAt: at},
			}},
			want: []want{
				{4101, "maintainer", "review_comment/20", "0ldc0de", "a.go:40-42", "", "moved", at},
				{4101, "maintainer", "review_comment/21", "0ldc0de", "a.go:9", "", "outdated", at},
				{4101, "maintainer", "review_comment/22", "cafed00d", "a.go", "", "whole file", at},
				{4101, "maintainer", "review_comment/23", "cafed00d", "a.go:10", "", "both sides", at},
			},
		},
		{
			name: "identical text repeated in one thread is one finding, the earliest",
			obs: publish.PullReviewObservation{Comments: []publish.PullReviewComment{
				{ID: 32, InReplyToID: 30, AuthorID: 4101, AuthorLogin: "maintainer", Path: "a.go", OriginalLine: 3, Body: "ping", OriginalCommitID: "cafed00d", CreatedAt: at.Add(2 * time.Minute)},
				{ID: 31, InReplyToID: 30, AuthorID: 4101, AuthorLogin: "maintainer", Path: "a.go", OriginalLine: 3, Body: "ping", OriginalCommitID: "cafed00d", CreatedAt: at.Add(time.Minute)},
				// Another account's identical text is its own finding.
				{ID: 33, InReplyToID: 30, AuthorID: 4102, AuthorLogin: "author", Path: "a.go", OriginalLine: 3, Body: "ping", OriginalCommitID: "cafed00d", CreatedAt: at.Add(3 * time.Minute)},
			}},
			want: []want{
				{4101, "maintainer", "review_comment/30", "cafed00d", "a.go:3", "", "ping", at.Add(time.Minute)},
				{4102, "author", "review_comment/30", "cafed00d", "a.go:3", "", "ping", at.Add(3 * time.Minute)},
			},
		},
		{
			name: "activity that cannot be a valid finding is skipped",
			obs: publish.PullReviewObservation{
				Reviews: []publish.PullReview{
					{ID: 40, AuthorLogin: "no-account", State: "COMMENTED", Body: "no id", CommitID: "cafed00d", SubmittedAt: at},
					{ID: 41, AuthorID: 4101, State: "COMMENTED", Body: "no login", CommitID: "cafed00d", SubmittedAt: at},
					{ID: 42, AuthorID: 4101, AuthorLogin: "maintainer", State: "COMMENTED", Body: "no commit", SubmittedAt: at},
				},
				Comments: []publish.PullReviewComment{
					{ID: 43, AuthorID: 4101, AuthorLogin: "maintainer", Path: "a.go", Body: strings.Repeat("x", domain.MaxNativeReviewTextBytes+1), OriginalCommitID: "cafed00d", CreatedAt: at},
					{ID: 44, AuthorID: 4101, AuthorLogin: "maintainer", Path: "a.go", Body: "bad \xff bytes", OriginalCommitID: "cafed00d", CreatedAt: at},
					{ID: 45, AuthorID: 4101, AuthorLogin: "maintainer", Path: "a.go", Body: "no original commit", CommitID: "cafed00d", CreatedAt: at},
					{ID: 47, AuthorID: 4101, AuthorLogin: "maintainer", Path: "a.go", Body: "kept", OriginalCommitID: "cafed00d", CreatedAt: at},
				},
			},
			want:        []want{{4101, "maintainer", "review_comment/47", "cafed00d", "a.go", "", "kept", at}},
			wantSkipped: 6,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, skipped := buildExternalFindings(tc.obs, binding)
			if len(skipped) != tc.wantSkipped {
				t.Errorf("skipped %d, want %d: %v", len(skipped), tc.wantSkipped, skipped)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("built %d findings, want %d: %+v", len(got), len(tc.want), got)
			}
			for i, w := range tc.want {
				f := got[i]
				if err := f.Validate(); err != nil {
					t.Fatalf("finding %d invalid: %v", i, err)
				}
				location := ""
				if f.Location != nil {
					location = f.Location.String()
				}
				p := f.External
				if f.RunID != binding.RunID || p.Class != domain.FindingProvenanceExternalUntrusted ||
					p.Forge != domain.ExternalReviewForgeGitHub ||
					p.ReviewerAccountID != w.accountID || p.ReviewerLogin != w.login ||
					p.ThreadID != w.threadID || p.HeadSHA != w.head || location != w.location ||
					f.Severity != w.severity || f.RawText != w.rawText || f.Message != w.rawText ||
					!f.CreatedAt.Equal(w.createdAt) {
					t.Errorf("finding %d = %+v (external %+v, location %q), want %+v", i, f, *p, location, w)
				}
			}
		})
	}
}

// TestBuildExternalFindingsIsOrderIndependent proves a later pass that reads
// the same activity in another order rebuilds the same findings, so the
// stored rows converge.
func TestBuildExternalFindingsIsOrderIndependent(t *testing.T) {
	t.Parallel()
	obs := externalReviewActivity()
	obs.Reviews = append(obs.Reviews, publish.PullReview{
		ID: 900101, AuthorID: 4102, AuthorLogin: "review-bot[bot]", State: "COMMENTED",
		Body: "One nit below.", CommitID: "cafed00d", SubmittedAt: activeResourceTestTime.Add(time.Second),
	})
	first, _ := buildExternalFindings(obs, externalTestBinding())
	obs.Reviews[0], obs.Reviews[1] = obs.Reviews[1], obs.Reviews[0]
	obs.Comments[0], obs.Comments[1] = obs.Comments[1], obs.Comments[0]
	second, _ := buildExternalFindings(obs, externalTestBinding())
	if len(first) != 4 || len(second) != len(first) {
		t.Fatalf("built %d then %d findings, want 4 both times", len(first), len(second))
	}
	for i := range first {
		if first[i].ID != second[i].ID {
			t.Errorf("finding %d: %s then %s", i, first[i].ID, second[i].ID)
		}
	}
}

func readStoredFinding(t *testing.T, st *store.Store, id domain.FindingID) (domain.Finding, error) {
	t.Helper()
	var finding domain.Finding
	err := st.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		finding, err = tx.GetFinding(context.Background(), id)
		return err
	})
	return finding, err
}

func readRevision(t *testing.T, st *store.Store) int64 {
	t.Helper()
	var state store.ServerState
	if err := st.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		state, err = tx.ServerState(context.Background())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return state.Revision
}

// TestActiveResourceStoresExternalFindings proves a pass over a ready pull
// request stores one external finding per counted review body and inline
// comment, whoever wrote it, and that storing them changes nothing else: no
// profile lists either reviewer here, so the ready item stays open, no
// authority is sealed, and no attention item appears.
func TestActiveResourceStoresExternalFindings(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := activeReadyItem(t, st)
	armActiveTestSchedules(t, st, item)
	reconciler := activeResourceReconciler{
		store: st, pull: openExactPull, review: staticReview(externalReviewActivity()),
		reviewers: testReviewers(),
		now:       func() time.Time { return activeResourceTestTime },
	}
	result, err := reconciler.Reconcile(ctx)
	if err != nil || len(result.failures) != 0 || len(result.skipped) != 0 {
		t.Fatalf("Reconcile = %+v, %v", result, err)
	}

	want, _ := buildExternalFindings(externalReviewActivity(), externalTestBinding())
	if len(want) != 3 {
		t.Fatalf("fixture builds %d findings, want 3", len(want))
	}
	for _, w := range want {
		got, err := readStoredFinding(t, st, w.ID)
		if err != nil {
			t.Fatalf("finding %s (%s): %v", w.ID, w.External.ThreadID, err)
		}
		if got.RunID != *item.Subject.RunID || got.External == nil || *got.External != *w.External ||
			got.RawText != w.RawText || !got.CreatedAt.Equal(w.CreatedAt) {
			t.Errorf("stored finding = %+v, want %+v", got, w)
		}
	}

	if got := readActiveItem(t, st, item.ID); got.Status != domain.StatusOpen || got.ItemVersion != item.ItemVersion {
		t.Fatalf("external findings changed the ready item to %s v%d", got.Status, got.ItemVersion)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		chain, err := tx.PublicationSuccessorChain(ctx, *item.Subject.RunID)
		if err != nil {
			return err
		}
		if len(chain) != 0 {
			t.Errorf("unadmitted findings sealed %d successors", len(chain))
		}
		items, err := tx.ListAttentionItems(ctx)
		if err != nil {
			return err
		}
		if len(items) != 1 {
			t.Errorf("attention items = %d, want only the ready item", len(items))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestActiveResourceExternalFindingsConverge proves a later pass over the same
// activity stores nothing, fails nothing, and leaves the client-visible
// revision alone, and that the reviewer's identical text re-read with another
// timestamp is a duplicate of the stored finding, not an error.
func TestActiveResourceExternalFindingsConverge(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := activeReadyItem(t, st)
	armActiveTestSchedules(t, st, item)
	activity := externalReviewActivity()
	reconciler := activeResourceReconciler{
		store: st, pull: openExactPull,
		review: func(context.Context, string, int) (publish.PullReviewObservation, error) {
			return activity, nil
		},
		reviewers: testReviewers(),
		now:       func() time.Time { return activeResourceTestTime },
	}
	reconcile := func(step string) {
		t.Helper()
		result, err := reconciler.Reconcile(ctx)
		if err != nil || len(result.failures) != 0 || len(result.skipped) != 0 {
			t.Fatalf("%s: Reconcile = %+v, %v", step, result, err)
		}
	}
	reconcile("first pass")
	stored := readRevision(t, st)
	reconcile("unchanged pass")
	if got := readRevision(t, st); got != stored {
		t.Fatalf("an unchanged pass moved the revision %d to %d", stored, got)
	}

	// The reviewer deletes the comment and posts the same words again: same
	// thread, head, and text, so the same finding ID with a later timestamp.
	first, _ := buildExternalFindings(activity, externalTestBinding())
	activity.Comments[0].CreatedAt = activity.Comments[0].CreatedAt.Add(time.Hour)
	reconcile("reposted comment")
	if got := readRevision(t, st); got != stored {
		t.Fatalf("a repeated comment moved the revision %d to %d", stored, got)
	}
	var comment domain.Finding
	for _, f := range first {
		if f.External.ThreadID == "review_comment/800200" {
			comment = f
		}
	}
	got, err := readStoredFinding(t, st, comment.ID)
	if err != nil || !got.CreatedAt.Equal(comment.CreatedAt) {
		t.Fatalf("stored finding = %+v, %v; want it as first stored at %s", got, err, comment.CreatedAt)
	}

	// New activity is stored, and only it.
	activity.Comments = append(activity.Comments, publish.PullReviewComment{
		ID: 800300, AuthorID: 4101, AuthorLogin: "maintainer", Path: "daemon/main.go", OriginalLine: 9,
		Body: "one more", OriginalCommitID: "cafed00d", CreatedAt: activeResourceTestTime.Add(2 * time.Hour),
	})
	reconcile("new comment")
	if got := readRevision(t, st); got != stored+1 {
		t.Fatalf("a new comment moved the revision %d to %d, want one write", stored, got)
	}
}

// TestActiveResourceExternalIntakeSkipsUnusableActivity proves activity that
// cannot be a valid finding is reported and stored nowhere, while the rest of
// the same pass is stored and nothing fails.
func TestActiveResourceExternalIntakeSkipsUnusableActivity(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := activeReadyItem(t, st)
	armActiveTestSchedules(t, st, item)
	activity := externalReviewActivity()
	activity.Comments[1].Body = strings.Repeat("x", domain.MaxNativeReviewTextBytes+1)
	reconciler := activeResourceReconciler{
		store: st, pull: openExactPull, review: staticReview(activity),
		reviewers: testReviewers(),
		now:       func() time.Time { return activeResourceTestTime },
	}
	result, err := reconciler.Reconcile(ctx)
	if err != nil || len(result.failures) != 0 {
		t.Fatalf("Reconcile = %+v, %v", result, err)
	}
	if len(result.skipped) != 1 || !errors.Is(result.skipped[0], domain.ErrNativeReviewTextTooLarge) {
		t.Fatalf("skipped = %v, want the oversized comment", result.skipped)
	}
	kept, _ := buildExternalFindings(activity, externalTestBinding())
	if len(kept) != 2 {
		t.Fatalf("fixture builds %d findings, want 2", len(kept))
	}
	for _, f := range kept {
		if _, err := readStoredFinding(t, st, f.ID); err != nil {
			t.Fatalf("finding %s beside a skipped one: %v", f.ID, err)
		}
	}
}

// externalIntakeStore opens a store at a path the test can also reach through
// a second connection, with one open ready item on the bound pull request.
func externalIntakeStore(t *testing.T) (*store.Store, string, domain.AttentionItem) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "store.db")
	st := storetest.Open(t, dbPath, store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		},
	})
	item := activeReadyItem(t, st)
	armActiveTestSchedules(t, st, item)
	return st, dbPath, item
}

// TestActiveResourceExternalIntakeFailureIsolated proves a failed external
// intake never blocks the pass's pull fact: the failure is collected, and the
// review cache is evicted so the next pass fetches the activity again and
// stores the findings instead of riding a 304 past them.
func TestActiveResourceExternalIntakeFailureIsolated(t *testing.T) {
	ctx := context.Background()
	st, dbPath, item := externalIntakeStore(t)
	// The pre-read succeeds and the write fails, which is the failure the
	// write's own transaction has to contain.
	execStoreSQL(t, dbPath, `CREATE TRIGGER refuse_findings BEFORE INSERT ON findings
		BEGIN SELECT RAISE(ABORT, 'findings are refused'); END`)

	// The observer answers "not modified" until its cache is evicted, as the
	// real one does once a fetch has advanced its validators.
	fetched, evicted := false, 0
	reconciler := activeResourceReconciler{
		store: st, pull: openExactPull,
		review: func(context.Context, string, int) (publish.PullReviewObservation, error) {
			if fetched {
				return publish.PullReviewObservation{NotModified: true}, nil
			}
			fetched = true
			return externalReviewActivity(), nil
		},
		reviewers: testReviewers(),
		reviewInvalidate: func(repo string, number int) {
			if repo != "owner/repo" || number != 450 {
				t.Errorf("reviewInvalidate(%q, %d), want owner/repo#450", repo, number)
			}
			fetched = false
			evicted++
		},
		now: func() time.Time { return activeResourceTestTime },
	}
	result, err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile hard error: %v", err)
	}
	if len(result.failures) != 1 || !strings.Contains(result.failures[0].Error(), "record external review") {
		t.Fatalf("failures = %v, want one isolated external-intake failure", result.failures)
	}
	if evicted != 1 {
		t.Fatalf("reviewInvalidate calls = %d, want 1", evicted)
	}
	if pulls := readPullTimeline(t, st); len(pulls) != 1 {
		t.Fatalf("pull timeline = %d, want the pull fact committed despite the failure", len(pulls))
	}
	if got := readActiveItem(t, st, item.ID); got.Status != domain.StatusOpen {
		t.Fatalf("a failed intake changed the ready item to %s", got.Status)
	}
	want, _ := buildExternalFindings(externalReviewActivity(), externalTestBinding())
	if _, err := readStoredFinding(t, st, want[0].ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a failed intake stored a finding: %v", err)
	}

	execStoreSQL(t, dbPath, "DROP TRIGGER refuse_findings")
	result, err = reconciler.Reconcile(ctx)
	if err != nil || len(result.failures) != 0 {
		t.Fatalf("retry Reconcile = %+v, %v", result, err)
	}
	for _, w := range want {
		if _, err := readStoredFinding(t, st, w.ID); err != nil {
			t.Fatalf("retry did not store finding %s: %v", w.ID, err)
		}
	}
}

// TestActiveResourceDiscardedObservationEvictsReviewCache proves a pass that
// fetches review activity and then discards its observation evicts the review
// cache: the fetch advanced the observer's validators, so the next pass would
// otherwise be answered "not modified" and never store what this one read.
func TestActiveResourceDiscardedObservationEvictsReviewCache(t *testing.T) {
	ctx := context.Background()
	st, dbPath, _ := externalIntakeStore(t)
	evicted := 0
	reconciler := activeResourceReconciler{
		store: st, pull: openExactPull,
		review: func(context.Context, string, int) (publish.PullReviewObservation, error) {
			// The store breaks after the fetch and before the pass reads its
			// latest pull fact.
			dropStoreTable(t, dbPath, "pull_merge_facts")
			return externalReviewActivity(), nil
		},
		reviewers:        testReviewers(),
		reviewInvalidate: func(string, int) { evicted++ },
		now:              func() time.Time { return activeResourceTestTime },
	}
	result, err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile hard error: %v", err)
	}
	if len(result.failures) != 1 || !strings.Contains(result.failures[0].Error(), "reconcile ready resource") {
		t.Fatalf("failures = %v, want the discarded observation", result.failures)
	}
	if evicted != 1 {
		t.Fatalf("reviewInvalidate calls = %d, want 1", evicted)
	}
}

// TestActiveResourceExternalIntakeNeedsTheBoundOpenPull proves review activity
// is stored only while the pull request is open and still the bound one. A
// finding names no pull request, so activity read for any other state of it
// is never fetched.
func TestActiveResourceExternalIntakeNeedsTheBoundOpenPull(t *testing.T) {
	for name, mutate := range map[string]func(*publish.PullObservation){
		"closed":       func(p *publish.PullObservation) { p.State = "closed" },
		"head changed": func(p *publish.PullObservation) { p.HeadSHA = "feedface" },
		"retargeted":   func(p *publish.PullObservation) { p.BaseRef = "release" },
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			st := schedTestStore(t)
			item := activeReadyItem(t, st)
			armActiveTestSchedules(t, st, item)
			fetches := 0
			reconciler := activeResourceReconciler{
				store: st,
				pull: func(context.Context, string, int) (publish.PullObservation, error) {
					observed := exactPull("open", false)
					mutate(&observed)
					return observed, nil
				},
				review: func(context.Context, string, int) (publish.PullReviewObservation, error) {
					fetches++
					return externalReviewActivity(), nil
				},
				reviewers: testReviewers(),
				now:       func() time.Time { return activeResourceTestTime },
			}
			if _, err := reconciler.Reconcile(ctx); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if fetches != 0 {
				t.Errorf("review activity fetched %d times", fetches)
			}
			var stored []domain.Finding
			if err := st.Read(ctx, func(tx *store.ReadTx) error {
				var err error
				stored, err = tx.ListExternalFindings(ctx, *item.Subject.RunID)
				return err
			}); err != nil || len(stored) != 0 {
				t.Fatalf("stored %d external findings, %v", len(stored), err)
			}
		})
	}
}

// TestCommitExternalFindingsRefusesChangedBinding proves findings fetched for
// one binding are never stored once the item's binding reads differently: the
// run is a finding's only tie to a pull request.
func TestCommitExternalFindingsRefusesChangedBinding(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := activeReadyItem(t, st)
	reconciler := activeResourceReconciler{
		store: st, pull: openExactPull, review: staticReview(externalReviewActivity()),
		now: func() time.Time { return activeResourceTestTime },
	}
	observation, err := reconciler.observe(ctx, item, activeResourceTestTime)
	if err != nil || len(observation.externalFindings) != 3 {
		t.Fatalf("observe built %d findings, %v", len(observation.externalFindings), err)
	}
	observation.binding.PRNumber++
	if err := reconciler.commitExternalFindings(ctx, observation); !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("commit under a changed binding = %v, want an immutable conflict", err)
	}
	if _, err := readStoredFinding(t, st, observation.externalFindings[0].ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a refused write stored a finding: %v", err)
	}
}
