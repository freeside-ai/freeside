package daemoncompose

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

var checkFailureTestToken = "ghp_" + strings.Repeat("A", 36)

// codeFailure is a code failure of the required check with jobs failed jobs,
// job i having ID 100+i in workflow run 7, attempt 1.
func codeFailure(jobs int) publish.RequiredCheckObservation {
	code := domain.CheckFailureCode
	observation := publish.RequiredCheckObservation{State: domain.RequiredCheckFailed, FailureClass: &code}
	for i := range jobs {
		observation.FailedJobs = append(observation.FailedJobs, publish.FailedCheckJob{
			ID: int64(100 + i), RunID: 7, RunAttempt: 1, Name: "daemon / job " + string(rune('a'+i)),
		})
	}
	return observation
}

// checkFailureReconciler reconciles the open pull request reentryReadyItem
// binds, reading check as its required check and logs[job ID] (or
// logErrs[job ID]) as each job's log. It returns the job IDs each read asked
// for, in order.
func checkFailureReconciler(
	t *testing.T, st *store.Store, check *publish.RequiredCheckObservation,
	logs map[int64]string, logErrs map[int64]error,
) (*ActiveResourceReconciler, *signet.BlobStore, *[]int64) {
	t.Helper()
	blobs, err := signet.NewBlobStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	reconciler := externalReentryReconciler(st, staticReview(publish.PullReviewObservation{}))
	reconciler.observeCheck = func(context.Context, string, string, string) (publish.RequiredCheckObservation, error) {
		return *check, nil
	}
	var reads []int64
	reconciler.readJobLog = func(_ context.Context, repo string, jobID int64) ([]byte, error) {
		if repo != "owner/repo" {
			t.Errorf("job log read in %s, want the binding's repository", repo)
		}
		reads = append(reads, jobID)
		if err := logErrs[jobID]; err != nil {
			return nil, err
		}
		return []byte(logs[jobID]), nil
	}
	reconciler.excerpts = blobs
	return &reconciler, blobs, &reads
}

func readExcerpt(t *testing.T, blobs *signet.BlobStore, digest domain.Digest) string {
	t.Helper()
	body, err := blobs.Open(digest)
	if err != nil {
		t.Fatal(err)
	}
	excerpt, err := io.ReadAll(body)
	if err := errors.Join(err, body.Close()); err != nil {
		t.Fatal(err)
	}
	return string(excerpt)
}

// TestActiveResourceStartsCheckFailureCycle proves a code failure of the
// published head's required check supersedes the ready item, records the
// first MaxCheckFailureJobs failed jobs with screened log excerpts, seals a
// check_failure authority over that record, and concludes the item's
// watches. A log GitHub no longer keeps, or one too long to read, is named
// as such, and a later pass reads no log and writes nothing (issue #3598).
func TestActiveResourceStartsCheckFailureCycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := schedTestStore(t)
	item := reentryReadyItem(t, st, "run-check-failure-start")
	armActiveTestSchedules(t, st, item)
	check := codeFailure(domain.MaxCheckFailureJobs + 1)
	reconciler, blobs, reads := checkFailureReconciler(t, st, &check,
		map[int64]string{100: "--- FAIL: TestThing\nexport GITHUB_TOKEN=" + checkFailureTestToken + "\n"},
		map[int64]error{101: publish.ErrJobLogTooLong, 102: publish.ErrJobLogUnavailable})

	reconcileClean(t, reconciler, "first pass")
	if got := *reads; len(got) != domain.MaxCheckFailureJobs || got[0] != 100 || got[2] != 102 {
		t.Fatalf("log reads = %v, want the first %d jobs in order", got, domain.MaxCheckFailureJobs)
	}
	if got := readActiveItem(t, st, item.ID); got.Status != domain.StatusSuperseded {
		t.Fatalf("ready item = status %s, want superseded", got.Status)
	}
	successor, queued := readReentry(t, st, *item.Subject.RunID)
	if successor == nil || !queued || successor.Origin != domain.PublicationSuccessorCheckFailure ||
		successor.Reentry == nil || successor.Reentry.BaseSHA != reentryTestBase ||
		successor.Reentry.HeadSHA != reentryTestHead || successor.ReviewRound != 2 {
		t.Fatalf("cycle = authority %+v, task queued %t; want a check_failure cycle on the head", successor, queued)
	}
	var failure domain.RequiredCheckFailure
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		failure, err = tx.GetRequiredCheckFailure(ctx, successor.RunID, successor.CheckFailureDigest)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(failure.Jobs) != domain.MaxCheckFailureJobs || failure.HeadSHA != reentryTestHead || failure.PRNumber != 450 {
		t.Fatalf("failure record = %+v, want the first %d jobs on the head", failure, domain.MaxCheckFailureJobs)
	}
	for i, job := range failure.Jobs {
		want := check.FailedJobs[i]
		if job.Name != want.Name || job.JobID != want.ID || job.WorkflowRunID != 7 || job.Attempt != 1 {
			t.Errorf("job %d = %+v, want %+v", i, job, want)
		}
	}
	screened := readExcerpt(t, blobs, failure.Jobs[0].ExcerptDigest)
	if strings.Contains(screened, checkFailureTestToken) || !strings.Contains(screened, "--- FAIL: TestThing") {
		t.Errorf("first excerpt = %q, want the failure kept and the token screened out", screened)
	}
	if got := readExcerpt(t, blobs, failure.Jobs[1].ExcerptDigest); got != tooLongJobLog {
		t.Errorf("oversized log's excerpt = %q, want %q", got, tooLongJobLog)
	}
	if got := readExcerpt(t, blobs, failure.Jobs[2].ExcerptDigest); got != unavailableJobLog {
		t.Errorf("expired log's excerpt = %q, want %q", got, unavailableJobLog)
	}
	for _, id := range publicationScheduleIDs(item.ID) {
		if schedule := readScheduleRow(t, st, id); !schedule.Status.Terminal() {
			t.Errorf("schedule %s not concluded: %s", id, schedule.Status)
		}
	}

	started := readRevision(t, st)
	reconcileClean(t, reconciler, "second pass")
	if len(*reads) != domain.MaxCheckFailureJobs || readRevision(t, st) != started {
		t.Fatalf("a pass after the cycle started read logs %v or moved the revision", *reads)
	}
}

// TestActiveResourceCheckFailureStartsNothing proves a failure that is not on
// code, a code failure on a handed-back pull request, and one in a project
// that is not ready read no log and start no cycle, and that a failed log
// read starts none and is retried on the next pass.
func TestActiveResourceCheckFailureStartsNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		class    domain.CheckFailureClass
		handBack bool
	}{
		{name: "infrastructure", class: domain.CheckFailureInfrastructure},
		{name: "other", class: domain.CheckFailureOther},
		{name: "handed back", class: domain.CheckFailureCode, handBack: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st := schedTestStore(t)
			item := reentryReadyItem(t, st, domain.RunID("run-check-failure-"+strings.ReplaceAll(tc.name, " ", "-")))
			check := codeFailure(1)
			check.FailureClass = &tc.class
			reconciler, _, reads := checkFailureReconciler(t, st, &check, map[int64]string{100: "FAIL"}, nil)
			if tc.handBack {
				reconciler.handBackLabels = staticHandBackLabel(handBackTestLabel)
				reconciler.mergeBase = func(context.Context, string, string, string) (string, error) {
					return handBackTestMergeBase, nil
				}
				reconciler.Pull = func(context.Context, string, int) (publish.PullObservation, error) {
					observed := exactPull("open", false)
					observed.HeadSHA = reentryTestHead
					observed.Labels = []string{handBackTestLabel}
					return observed, nil
				}
			}
			reconcileClean(t, reconciler, "pass")
			if len(*reads) != 0 {
				t.Errorf("log reads = %v, want none", *reads)
			}
			if got := readActiveItem(t, st, item.ID); got.Status != domain.StatusOpen {
				t.Errorf("ready item = status %s, want open", got.Status)
			}
			assertNoReentry(t, st, *item.Subject.RunID)
		})
	}

	t.Run("project not ready", func(t *testing.T) {
		t.Parallel()
		st := schedTestStore(t)
		item := reentryReadyItem(t, st, "run-check-failure-not-ready")
		check := codeFailure(1)
		reconciler, _, reads := checkFailureReconciler(t, st, &check, map[int64]string{100: "FAIL"}, nil)
		reconciler.readinessGate = func(context.Context, *store.ReadTx, domain.ProjectID) error {
			return fmt.Errorf("fixture: %w", domain.ErrProjectNotReady)
		}
		result, err := reconciler.Reconcile(context.Background())
		if err != nil || len(result.Failures) != 1 || !errors.Is(result.Failures[0], domain.ErrProjectNotReady) {
			t.Fatalf("Reconcile = %+v, %v; want the readiness refusal alone", result, err)
		}
		if len(*reads) != 0 {
			t.Errorf("log reads = %v, want none before the project is ready", *reads)
		}
		if got := readActiveItem(t, st, item.ID); got.Status != domain.StatusOpen {
			t.Errorf("ready item = status %s, want open", got.Status)
		}
		assertNoReentry(t, st, *item.Subject.RunID)
	})

	t.Run("failed log read", func(t *testing.T) {
		t.Parallel()
		st := schedTestStore(t)
		item := reentryReadyItem(t, st, "run-check-failure-read-error")
		check := codeFailure(1)
		logErrs := map[int64]error{100: errors.New("connection reset")}
		reconciler, _, reads := checkFailureReconciler(t, st, &check, map[int64]string{100: "FAIL"}, logErrs)
		result, err := reconciler.Reconcile(context.Background())
		if err != nil || len(result.Failures) != 1 {
			t.Fatalf("Reconcile = %+v, %v; want the failed read reported", result, err)
		}
		if got := readActiveItem(t, st, item.ID); got.Status != domain.StatusOpen {
			t.Fatalf("ready item = status %s, want open", got.Status)
		}
		assertNoReentry(t, st, *item.Subject.RunID)

		delete(logErrs, 100)
		reconcileClean(t, reconciler, "retry")
		if len(*reads) != 2 {
			t.Fatalf("log reads = %v, want the retry to read again", *reads)
		}
		if successor, _ := readReentry(t, st, *item.Subject.RunID); successor == nil ||
			successor.Origin != domain.PublicationSuccessorCheckFailure {
			t.Fatalf("authority after the retry = %+v, want a check_failure cycle", successor)
		}
	})
}
