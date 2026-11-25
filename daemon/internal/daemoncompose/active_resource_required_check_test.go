package daemoncompose

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// checkReconciler is an active-resource reconciler over activeReadyItem whose
// required-check read returns *check (or *checkErr) and counts its calls.
func checkReconciler(
	t *testing.T, pull *publish.PullObservation, check *publish.RequiredCheckObservation, checkErr *error, reads *int,
) (*ActiveResourceReconciler, domain.AttentionItem) {
	t.Helper()
	st := schedTestStore(t)
	item := activeReadyItem(t, st)
	return &ActiveResourceReconciler{
		Store: st,
		Pull: func(context.Context, string, int) (publish.PullObservation, error) {
			return *pull, nil
		},
		observeCheck: func(_ context.Context, repo, baseRef, head string) (publish.RequiredCheckObservation, error) {
			*reads++
			if repo != "owner/repo" || baseRef != "main" || head != "cafed00d" {
				t.Errorf("required check read %s %s %s, want the binding's", repo, baseRef, head)
			}
			return *check, *checkErr
		},
		Now: func() time.Time { return activeResourceTestTime },
	}, item
}

// TestActiveResourceRecordsRequiredCheckChanges: each changed reading is
// written at the item's next version, an unchanged one writes nothing, and a
// failed read is reported and changes nothing.
func TestActiveResourceRecordsRequiredCheckChanges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pull := exactPull("open", false)
	check := publish.RequiredCheckObservation{State: domain.RequiredCheckPending}
	var checkErr error
	reads := 0
	r, item := checkReconciler(t, &pull, &check, &checkErr, &reads)

	pass := func(wantVersion int, wantFailures int) domain.AttentionItem {
		t.Helper()
		failures, err := reconcileActiveResource(r, ctx)
		if err != nil || len(failures) != wantFailures {
			t.Fatalf("Reconcile = %v, %v; want %d failures", failures, err, wantFailures)
		}
		got := readActiveItem(t, r.Store, item.ID)
		if got.ItemVersion != wantVersion || got.Status != domain.StatusOpen {
			t.Fatalf("item version %d status %s, want %d open", got.ItemVersion, got.Status, wantVersion)
		}
		return got
	}

	got := pass(2, 0)
	want := &domain.RequiredCheck{State: domain.RequiredCheckPending, HeadSHA: "cafed00d", ObservedAt: activeResourceTestTime}
	if !reflect.DeepEqual(got.RequiredCheck, want) {
		t.Fatalf("pending reading = %+v, want %+v", got.RequiredCheck, want)
	}
	pass(2, 0) // unchanged: no write

	infrastructure := domain.CheckFailureInfrastructure
	check = publish.RequiredCheckObservation{
		State: domain.RequiredCheckFailed, FailureClass: &infrastructure,
		FailedJobs: []publish.FailedCheckJob{{ID: 11, RunID: 7, RunAttempt: 1, Name: "daemon / macos (test, darwin 1/1)"}},
		FailedRuns: []publish.CheckRun{{ID: 7, Attempt: 1}},
	}
	got = pass(3, 0)
	if got.RequiredCheck.State != domain.RequiredCheckFailed ||
		!reflect.DeepEqual(got.RequiredCheck.FailedJobs, []string{"daemon / macos (test, darwin 1/1)"}) {
		t.Fatalf("failed reading = %+v", got.RequiredCheck)
	}

	checkErr = errors.New("actions read refused")
	pass(3, 1)

	checkErr = nil
	check = publish.RequiredCheckObservation{State: domain.RequiredCheckPassed}
	if got = pass(4, 0); got.RequiredCheck.State != domain.RequiredCheckPassed || got.RequiredCheck.FailedJobs != nil {
		t.Fatalf("passed reading = %+v", got.RequiredCheck)
	}
	if reads != 5 {
		t.Errorf("required check reads = %d, want one per pass", reads)
	}
}

// TestActiveResourceReadsNoCheckOffTheBoundHead: a moved head invalidates the
// item and is never read; a base that requires no check writes nothing.
func TestActiveResourceReadsNoCheckOffTheBoundHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	pull := exactPull("open", false)
	none := publish.RequiredCheckObservation{None: true}
	var checkErr error
	reads := 0
	r, item := checkReconciler(t, &pull, &none, &checkErr, &reads)
	if failures, err := reconcileActiveResource(r, ctx); err != nil || len(failures) != 0 {
		t.Fatalf("Reconcile = %v, %v", failures, err)
	}
	if got := readActiveItem(t, r.Store, item.ID); got.RequiredCheck != nil || got.ItemVersion != item.ItemVersion {
		t.Fatalf("no required check wrote %+v at version %d", got.RequiredCheck, got.ItemVersion)
	}

	pull.HeadSHA = "beefcafe"
	reads = 0
	if _, err := reconcileActiveResource(r, ctx); err != nil {
		t.Fatal(err)
	}
	if reads != 0 {
		t.Errorf("moved head read the required check %d times, want none", reads)
	}
	if got := readActiveItem(t, r.Store, item.ID); got.RequiredCheck != nil {
		t.Fatalf("moved head wrote a reading %+v", got.RequiredCheck)
	}
}

// pendingReruns lists the rerun intents' keys in insertion order.
func pendingReruns(t *testing.T, st *store.Store) []string {
	t.Helper()
	var keys []string
	if err := st.Read(context.Background(), func(tx *store.ReadTx) error {
		entries, err := tx.ListPendingOutbox(context.Background(), publish.CheckRerunKind)
		for _, entry := range entries {
			keys = append(keys, entry.IdempotencyKey)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return keys
}

func infrastructureFailure(attempt int) publish.RequiredCheckObservation {
	infrastructure := domain.CheckFailureInfrastructure
	return publish.RequiredCheckObservation{
		State: domain.RequiredCheckFailed, FailureClass: &infrastructure,
		FailedJobs: []publish.FailedCheckJob{{ID: int64(10 + attempt), RunID: 7, RunAttempt: attempt, Name: "daemon / macos (test, darwin 1/1)"}},
		FailedRuns: []publish.CheckRun{{ID: 7, Attempt: attempt}},
	}
}

// TestActiveResourceRerunsInfrastructureTwice: each infrastructure failure
// on a new attempt enqueues one rerun and raises the card's count, a
// repeated reading of the same attempt enqueues nothing, and a third failure
// is left on the card.
func TestActiveResourceRerunsInfrastructureTwice(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pull := exactPull("open", false)
	check := infrastructureFailure(1)
	var checkErr error
	reads := 0
	r, item := checkReconciler(t, &pull, &check, &checkErr, &reads)
	key := func(attempt int) string { return publish.CheckRerunKey(item.ID, 7, attempt) }

	for _, step := range []struct {
		attempt     int
		wantReruns  int
		wantIntents []string
	}{
		{attempt: 1, wantReruns: 1, wantIntents: []string{key(1)}},
		{attempt: 1, wantReruns: 1, wantIntents: []string{key(1)}},
		{attempt: 2, wantReruns: 2, wantIntents: []string{key(1), key(2)}},
		{attempt: 3, wantReruns: 2, wantIntents: []string{key(1), key(2)}},
	} {
		check = infrastructureFailure(step.attempt)
		if failures, err := reconcileActiveResource(r, ctx); err != nil || len(failures) != 0 {
			t.Fatalf("attempt %d: Reconcile = %v, %v", step.attempt, failures, err)
		}
		got := readActiveItem(t, r.Store, item.ID)
		if got.RequiredCheck == nil || got.RequiredCheck.Reruns != step.wantReruns {
			t.Fatalf("attempt %d: reading %+v, want %d reruns", step.attempt, got.RequiredCheck, step.wantReruns)
		}
		if intents := pendingReruns(t, r.Store); !reflect.DeepEqual(intents, step.wantIntents) {
			t.Fatalf("attempt %d: intents %q, want %q", step.attempt, intents, step.wantIntents)
		}
	}
}

// TestActiveResourceRerunsNothingElse: a code failure, and an infrastructure
// failure on a handed-back pull request, enqueue no rerun.
func TestActiveResourceRerunsNothingElse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	code := domain.CheckFailureCode
	codeFailure := infrastructureFailure(1)
	codeFailure.FailureClass = &code

	for _, tc := range []struct {
		name     string
		check    publish.RequiredCheckObservation
		handBack bool
	}{
		{name: "code failure", check: codeFailure},
		{name: "handed back", check: infrastructureFailure(1), handBack: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st := schedTestStore(t)
			item := activeReadyItem(t, st)
			r := &ActiveResourceReconciler{Store: st}
			binding := domain.ReadyItemPRBinding{Repo: "owner/repo", PRNumber: 450, BaseRef: "main", HeadSHA: item.PRHeadSHA}
			if err := r.convergeRequiredCheck(ctx, item.ID, binding, tc.check, tc.handBack, activeResourceTestTime); err != nil {
				t.Fatal(err)
			}
			got := readActiveItem(t, st, item.ID)
			if got.RequiredCheck == nil || got.RequiredCheck.Reruns != 0 || len(pendingReruns(t, st)) != 0 {
				t.Fatalf("reading %+v with intents %q, want a failed card and no rerun", got.RequiredCheck, pendingReruns(t, st))
			}
		})
	}
}
