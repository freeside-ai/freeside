package main

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const testReviewerLogin = "chatgpt-codex-connector"

func testReviewers() map[string]bool { return map[string]bool{testReviewerLogin: true} }

func openExactPull(context.Context, string, int) (publish.PullObservation, error) {
	return exactPull("open", false), nil
}

func staticReview(obs publish.PullReviewObservation) nativeReviewObserver {
	return func(context.Context, string, int) (publish.PullReviewObservation, error) {
		return obs, nil
	}
}

func readNativeObservations(t *testing.T, st *store.Store) []domain.NativeReviewObservation {
	t.Helper()
	var got []domain.NativeReviewObservation
	if err := st.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		got, err = tx.ListNativeReviewObservations(context.Background(), 424242, 450)
		return err
	}); err != nil {
		t.Fatalf("list native observations: %v", err)
	}
	return got
}

// findingsReviewActivity is a native findings review (one badge comment) plus a
// clean-pass reaction, both by the configured reviewer and bound to the ready
// item's head "cafed00d".
func findingsReviewActivity() publish.PullReviewObservation {
	return publish.PullReviewObservation{
		Reviews: []publish.PullReview{{
			ID: 900100, AuthorLogin: testReviewerLogin, State: "COMMENTED",
			Body: "review", CommitID: "cafed00d", SubmittedAt: activeResourceTestTime,
		}},
		Comments: []publish.PullReviewComment{{
			ID: 800200, ReviewID: 900100, AuthorLogin: testReviewerLogin,
			Path: "daemon/main.go", Line: 42, Body: "P2: unchecked error return",
			CommitID: "cafed00d", CreatedAt: activeResourceTestTime,
		}},
		Reactions: []publish.PullDescriptionReaction{{
			ID: 700300, AuthorLogin: testReviewerLogin, Content: "+1", CreatedAt: activeResourceTestTime,
		}},
	}
}

func TestActiveResourceRecordsNativeFindingsAndCleanPass(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := activeReadyItem(t, st)
	armActiveTestSchedules(t, st, item)
	reconciler := activeResourceReconciler{
		store: st, pull: openExactPull, review: staticReview(findingsReviewActivity()),
		reviewers: testReviewers(),
		now:       func() time.Time { return activeResourceTestTime },
	}
	failures, err := reconcileActiveResource(&reconciler, ctx)
	if err != nil || len(failures) != 0 {
		t.Fatalf("Reconcile = %v, %v", failures, err)
	}

	got := readNativeObservations(t, st)
	if len(got) != 2 {
		t.Fatalf("recorded %d native observations, want 2 (findings + clean pass)", len(got))
	}
	var findings, clean *domain.NativeReviewObservation
	for i := range got {
		switch got[i].Kind {
		case domain.NativeReviewFindings:
			findings = &got[i]
		case domain.NativeReviewCleanPass:
			clean = &got[i]
		}
	}
	if findings == nil || clean == nil {
		t.Fatalf("kinds recorded = %+v, want one findings_review and one clean_pass_signal", got)
	}
	if len(findings.Findings) != 1 || findings.Findings[0].Severity != "P2" ||
		findings.Findings[0].Location != "daemon/main.go:42" ||
		findings.Findings[0].RunID != *item.Subject.RunID {
		t.Fatalf("normalized finding = %+v", findings.Findings)
	}
	if clean.NativeID != 700300 || len(clean.Findings) != 0 || clean.ReviewCommitSHA != "" {
		t.Fatalf("clean pass = %+v", clean)
	}

	// The item stays open and gains no readiness from native activity: readiness
	// remains gated on the Freeside-invoked pass.
	if got := readActiveItem(t, st, item.ID); got.Status != domain.StatusOpen {
		t.Fatalf("native activity changed item status to %s", got.Status)
	}
}

// TestActiveResourceRecordsLateAndForeignFiltered proves a late review (arriving
// on an already-ready item) is recorded while activity from a non-reviewer
// login is filtered out.
func TestActiveResourceRecordsLateAndForeignFiltered(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := activeReadyItem(t, st)
	armActiveTestSchedules(t, st, item)
	activity := findingsReviewActivity()
	activity.Reviews = append(activity.Reviews, publish.PullReview{
		ID: 900999, AuthorLogin: "random-contributor", State: "COMMENTED",
		CommitID: "cafed00d", SubmittedAt: activeResourceTestTime,
	})
	activity.Reactions = append(activity.Reactions, publish.PullDescriptionReaction{
		ID: 700999, AuthorLogin: "random-contributor", Content: "+1", CreatedAt: activeResourceTestTime,
	})
	reconciler := activeResourceReconciler{
		store: st, pull: openExactPull, review: staticReview(activity),
		reviewers: testReviewers(),
		now:       func() time.Time { return activeResourceTestTime },
	}
	if failures, err := reconcileActiveResource(&reconciler, ctx); err != nil || len(failures) != 0 {
		t.Fatalf("Reconcile = %v, %v", failures, err)
	}
	got := readNativeObservations(t, st)
	if len(got) != 2 {
		t.Fatalf("recorded %d native observations, want 2 (foreign login filtered out)", len(got))
	}
	for _, o := range got {
		if o.AuthorLogin != testReviewerLogin {
			t.Fatalf("recorded a foreign login: %+v", o)
		}
	}
}

// TestActiveResourceNormalizesBotLogin proves the reviewer filter matches the
// GitHub App "[bot]" login form the REST endpoints actually return, so a
// reviewer configured by its canonical login still has its review, inline
// comment, and clean-pass reaction observed and recorded under the canonical
// login. Without normalization the exact-login filter drops everything.
func TestActiveResourceNormalizesBotLogin(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := activeReadyItem(t, st)
	armActiveTestSchedules(t, st, item)
	activity := findingsReviewActivity()
	botLogin := testReviewerLogin + "[bot]"
	activity.Reviews[0].AuthorLogin = botLogin
	activity.Comments[0].AuthorLogin = botLogin
	activity.Reactions[0].AuthorLogin = botLogin
	reconciler := activeResourceReconciler{
		store: st, pull: openExactPull, review: staticReview(activity),
		reviewers: testReviewers(),
		now:       func() time.Time { return activeResourceTestTime },
	}
	if failures, err := reconcileActiveResource(&reconciler, ctx); err != nil || len(failures) != 0 {
		t.Fatalf("Reconcile = %v, %v", failures, err)
	}
	got := readNativeObservations(t, st)
	if len(got) != 2 {
		t.Fatalf("recorded %d native observations, want 2 (bot-form login normalized)", len(got))
	}
	for _, o := range got {
		if o.AuthorLogin != testReviewerLogin {
			t.Fatalf("stored login = %q, want the canonical %q", o.AuthorLogin, testReviewerLogin)
		}
	}
}

// TestActiveResourceRejectsStaleCleanPassReaction proves a clean-pass reaction
// created before the current head's binding (a leftover +1 from an earlier
// head) is not re-recorded as a clean pass for the current head; only the
// findings review, which binds by commit_id, is recorded.
func TestActiveResourceRejectsStaleCleanPassReaction(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := activeReadyItem(t, st)
	armActiveTestSchedules(t, st, item)
	activity := findingsReviewActivity()
	// The binding's RecordedAt is activeResourceTestTime.Add(-time.Minute); this
	// reaction predates it, so it belongs to an earlier head.
	activity.Reactions[0].CreatedAt = activeResourceTestTime.Add(-2 * time.Minute)
	reconciler := activeResourceReconciler{
		store: st, pull: openExactPull, review: staticReview(activity),
		reviewers: testReviewers(),
		now:       func() time.Time { return activeResourceTestTime },
	}
	if failures, err := reconcileActiveResource(&reconciler, ctx); err != nil || len(failures) != 0 {
		t.Fatalf("Reconcile = %v, %v", failures, err)
	}
	got := readNativeObservations(t, st)
	if len(got) != 1 {
		t.Fatalf("recorded %d native observations, want 1 (stale clean-pass rejected)", len(got))
	}
	if got[0].Kind != domain.NativeReviewFindings {
		t.Fatalf("recorded kind = %s, want only the findings review", got[0].Kind)
	}
}

// TestActiveResourceRecordsStaleHeadReview proves a review that bound to a head
// other than the item's live binding head is recorded with the divergence
// visible rather than dropped.
func TestActiveResourceRecordsStaleHeadReview(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := activeReadyItem(t, st)
	armActiveTestSchedules(t, st, item)
	activity := findingsReviewActivity()
	activity.Reviews[0].CommitID = "0ldc0de" // review bound to an earlier head
	activity.Comments[0].CommitID = "0ldc0de"
	reconciler := activeResourceReconciler{
		store: st, pull: openExactPull, review: staticReview(activity),
		reviewers: testReviewers(),
		now:       func() time.Time { return activeResourceTestTime },
	}
	if failures, err := reconcileActiveResource(&reconciler, ctx); err != nil || len(failures) != 0 {
		t.Fatalf("Reconcile = %v, %v", failures, err)
	}
	for _, o := range readNativeObservations(t, st) {
		if o.Kind == domain.NativeReviewFindings {
			if o.ReviewCommitSHA != "0ldc0de" || o.BindingHeadSHA != "cafed00d" {
				t.Fatalf("stale-head divergence not preserved: %+v", o)
			}
			return
		}
	}
	t.Fatal("stale-head findings review was not recorded")
}

// TestActiveResourceNativeObserveFailureIsolated proves a native-observe failure
// is reported but never blocks the pull fact, and the next tick retries and
// records without churning the pull timeline.
func TestActiveResourceNativeObserveFailureIsolated(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := activeReadyItem(t, st)
	armActiveTestSchedules(t, st, item)

	failing := func(context.Context, string, int) (publish.PullReviewObservation, error) {
		return publish.PullReviewObservation{}, errors.New("boom")
	}
	reconciler := activeResourceReconciler{
		store: st, pull: openExactPull, review: failing,
		reviewers: testReviewers(),
		now:       func() time.Time { return activeResourceTestTime },
	}
	failures, err := reconcileActiveResource(&reconciler, ctx)
	if err != nil {
		t.Fatalf("Reconcile hard error: %v", err)
	}
	if len(failures) != 1 {
		t.Fatalf("failures = %v, want one isolated native failure", failures)
	}
	// The pull fact still committed despite the native failure.
	if pulls := readPullTimeline(t, st); len(pulls) != 1 {
		t.Fatalf("pull timeline = %d, want the pull fact committed despite native failure", len(pulls))
	}
	if got := readNativeObservations(t, st); len(got) != 0 {
		t.Fatalf("recorded native observations on a failed observe: %+v", got)
	}

	// The next tick retries with a working observer and records, without
	// re-appending the unchanged pull fact.
	reconciler.review = staticReview(findingsReviewActivity())
	if failures, err := reconcileActiveResource(&reconciler, ctx); err != nil || len(failures) != 0 {
		t.Fatalf("retry Reconcile = %v, %v", failures, err)
	}
	if got := readNativeObservations(t, st); len(got) != 2 {
		t.Fatalf("retry recorded %d native observations, want 2", len(got))
	}
	if pulls := readPullTimeline(t, st); len(pulls) != 1 {
		t.Fatalf("pull timeline churned on retry = %d, want 1", len(pulls))
	}
}

// TestActiveResourceNativeObservationConverges proves a repeated observation of
// unchanged native activity appends no new rows.
func TestActiveResourceNativeObservationConverges(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := activeReadyItem(t, st)
	armActiveTestSchedules(t, st, item)
	reconciler := activeResourceReconciler{
		store: st, pull: openExactPull, review: staticReview(findingsReviewActivity()),
		reviewers: testReviewers(),
		now:       func() time.Time { return activeResourceTestTime },
	}
	for i := 0; i < 3; i++ {
		if failures, err := reconcileActiveResource(&reconciler, ctx); err != nil || len(failures) != 0 {
			t.Fatalf("Reconcile %d = %v, %v", i, failures, err)
		}
	}
	if got := readNativeObservations(t, st); len(got) != 2 {
		t.Fatalf("recorded %d native observations across three ticks, want 2 (converged)", len(got))
	}
}

// TestActiveResourceNativeConvergesWithInvalidUTF8 proves a comment path or
// review commit_id carrying invalid UTF-8 (git stores paths as raw bytes) is
// sanitized to a byte-stable form, so repeated observation coalesces instead of
// churning the append timeline forever (the issue #180 round-trip trap).
func TestActiveResourceNativeConvergesWithInvalidUTF8(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := activeReadyItem(t, st)
	armActiveTestSchedules(t, st, item)
	activity := findingsReviewActivity()
	activity.Comments[0].Path = "dir/na\xffme"
	activity.Reviews[0].CommitID = "c0mm\xffit"
	reconciler := activeResourceReconciler{
		store: st, pull: openExactPull, review: staticReview(activity),
		reviewers: testReviewers(),
		now:       func() time.Time { return activeResourceTestTime },
	}
	var first int
	for i := 0; i < 3; i++ {
		if failures, err := reconcileActiveResource(&reconciler, ctx); err != nil || len(failures) != 0 {
			t.Fatalf("Reconcile %d = %v, %v", i, failures, err)
		}
		got := len(readNativeObservations(t, st))
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("invalid-UTF-8 native activity churned: tick %d has %d rows, want %d", i, got, first)
		}
	}
	if first == 0 {
		t.Fatal("no native observations recorded for the invalid-UTF-8 activity")
	}
}

// TestActiveResourceNativeNotModifiedRecordsNothing proves a fully-unchanged
// (304) native observation yields no store write.
func TestActiveResourceNativeNotModifiedRecordsNothing(t *testing.T) {
	ctx := context.Background()
	st := schedTestStore(t)
	item := activeReadyItem(t, st)
	armActiveTestSchedules(t, st, item)
	reconciler := activeResourceReconciler{
		store: st, pull: openExactPull,
		review:    staticReview(publish.PullReviewObservation{NotModified: true}),
		reviewers: testReviewers(),
		now:       func() time.Time { return activeResourceTestTime },
	}
	if failures, err := reconcileActiveResource(&reconciler, ctx); err != nil || len(failures) != 0 {
		t.Fatalf("Reconcile = %v, %v", failures, err)
	}
	if got := readNativeObservations(t, st); len(got) != 0 {
		t.Fatalf("recorded %d native observations on a 304, want 0", len(got))
	}
	_ = item
}

// TestActiveResourceNativeCommitFailureEvictsCache proves a failed durable
// append does not permanently drop native evidence: the reconciler evicts the
// observer cache (so the next tick re-fetches unconditionally instead of riding
// a 304) and the failure stays isolated from the pull fact (issue #497).
func TestActiveResourceNativeCommitFailureEvictsCache(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "store.db")
	st, err := store.Open(ctx, dbPath, store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	item := activeReadyItem(t, st)
	armActiveTestSchedules(t, st, item)

	// Drop the native table so the pull commit still succeeds but the native
	// append fails deterministically ("no such table"), isolating the failure to
	// commitNativeReview without disturbing observe or the pull commit.
	dropNativeReviewObservations(t, dbPath)

	var evicted [][2]any
	reconciler := activeResourceReconciler{
		store: st, pull: openExactPull, review: staticReview(findingsReviewActivity()),
		reviewers: testReviewers(),
		reviewInvalidate: func(repo string, number int) {
			evicted = append(evicted, [2]any{repo, number})
		},
		now: func() time.Time { return activeResourceTestTime },
	}
	failures, err := reconcileActiveResource(&reconciler, ctx)
	if err != nil {
		t.Fatalf("Reconcile hard error: %v", err)
	}
	if len(failures) != 1 {
		t.Fatalf("failures = %v, want one isolated native-commit failure", failures)
	}
	// The failed native commit evicts the observer cache for this exact PR so
	// the next tick re-fetches and retries rather than stranding the rows.
	if len(evicted) != 1 || evicted[0][0] != "owner/repo" || evicted[0][1] != 450 {
		t.Fatalf("reviewInvalidate calls = %v, want one for owner/repo#450", evicted)
	}
	// The pull fact still committed despite the native failure (isolation).
	if pulls := readPullTimeline(t, st); len(pulls) != 1 {
		t.Fatalf("pull timeline = %d, want the pull fact committed despite the native failure", len(pulls))
	}
}

// dropNativeReviewObservations deletes the native-review table through a second
// connection to the store's file, so a subsequent native append fails while the
// other tables stay intact.
func dropNativeReviewObservations(t *testing.T, dbPath string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open store db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("DROP TABLE native_review_observations"); err != nil {
		t.Fatalf("drop native table: %v", err)
	}
}

func readPullTimeline(t *testing.T, st *store.Store) []domain.PullMergeFact {
	t.Helper()
	var pulls []domain.PullMergeFact
	if err := st.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		pulls, err = tx.ListPullMergeFacts(context.Background(), 424242, 450)
		return err
	}); err != nil {
		t.Fatalf("list pull facts: %v", err)
	}
	return pulls
}
