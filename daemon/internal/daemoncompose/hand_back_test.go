package daemoncompose

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const handBackTestLabel = "needs rework"

// handBackTestMergeBase is the merge base the fake forge reports for the
// handed-back head: not the base the last review used, as after a rebase.
var handBackTestMergeBase = strings.Repeat("e", 40)

// handBackReconciler observes reentryReadyItem's pull request open on the
// head the lander pushed, with labels, and the maintainer's conversation
// comment on that head when comment is set.
func handBackReconciler(st *store.Store, labels []string, comment bool) (*ActiveResourceReconciler, *[]string) {
	activity := publish.PullReviewObservation{}
	if comment {
		activity.ConversationComments = []publish.PullConversationComment{
			conversationComment(600200, externalTestMaintainerID, externalTestMaintainerLogin,
				"Handed back: the rebase broke the build.", activeResourceTestTime),
		}
	}
	reconciler := conversationReconciler(st, staticReview(activity), &fakeAppBotIdentity{id: conversationTestBotID})
	reconciler.Pull = func(context.Context, string, int) (publish.PullObservation, error) {
		observed := exactPull("open", false)
		observed.HeadSHA = reentryTestNewHead
		observed.Labels = labels
		return observed, nil
	}
	reconciler.handBackLabel = handBackTestLabel
	var reads []string
	reconciler.mergeBase = func(_ context.Context, repo, baseRef, head string) (string, error) {
		reads = append(reads, repo+" "+baseRef+"..."+head)
		return handBackTestMergeBase, nil
	}
	return &reconciler, &reads
}

// TestActiveResourceStartsHandBackCycle proves a labeled pull request's new
// head supersedes the ready item without starting a head_changed re-entry,
// and the maintainer's comment on that head, read on the next pass, seals a
// hand_back authority that adopts the head at its merge base with the target,
// read from the forge once, and queues the cycle's task. A later pass starts
// no second cycle (issue #3160).
func TestActiveResourceStartsHandBackCycle(t *testing.T) {
	t.Parallel()
	st := schedTestStore(t)
	item := reentryReadyItem(t, st, "run-hand-back-start")
	armActiveTestSchedules(t, st, item)
	profile := activateExternalTestProfile(t, st, activeResourceTestTime.Add(-time.Hour), externalTestMaintainer())
	reconciler, reads := handBackReconciler(st, []string{"bug", handBackTestLabel}, true)

	reconcileClean(t, reconciler, "first pass")
	got := readActiveItem(t, st, item.ID)
	if got.Status != domain.StatusSuperseded || got.ReadinessInvalidation == nil ||
		got.ReadinessInvalidation.Reason != domain.ReadinessInvalidationHeadChanged {
		t.Fatalf("ready item = status %s invalidation %+v; want superseded on the head change",
			got.Status, got.ReadinessInvalidation)
	}
	assertNoReentry(t, st, *item.Subject.RunID)
	reconcileClean(t, reconciler, "second pass")
	successor, queued := readReentry(t, st, *item.Subject.RunID)
	if successor == nil || !queued {
		t.Fatalf("cycle = authority %+v, task queued %t; want both", successor, queued)
	}
	if successor.Version != domain.PublicationHandBackVersion || !successor.HandsBack() ||
		successor.PredecessorItemID != item.ID || successor.PriorReviewInvocationID != reentryTestReviewID ||
		successor.Reentry.BaseSHA != handBackTestMergeBase || successor.Reentry.HeadSHA != reentryTestNewHead ||
		successor.AdmittingProfileDigest != profile.ProfileDigest {
		t.Fatalf("authority = %+v (re-entry %+v); want a hand_back cycle on the new head at its merge base",
			successor, successor.Reentry)
	}
	want := "owner/repo main..." + reentryTestNewHead
	if len(*reads) != 1 || (*reads)[0] != want {
		t.Fatalf("merge-base reads = %q, want one of %q", *reads, want)
	}

	started := readRevision(t, st)
	reconcileClean(t, reconciler, "third pass")
	if got := readRevision(t, st); got != started {
		t.Fatalf("a pass after the cycle started moved the revision %d to %d", started, got)
	}
	if len(*reads) != 1 {
		t.Fatalf("a pass with no cycle due read the merge base again: %q", *reads)
	}
}

// TestActiveResourceHandBackWaitsForAComment proves a labeled pull request
// whose head moved starts nothing until an admitted account comments: the
// head change supersedes the ready item, as any head change does, but starts
// no head_changed re-entry, which the hand-back cycle would replace.
func TestActiveResourceHandBackWaitsForAComment(t *testing.T) {
	t.Parallel()
	st := schedTestStore(t)
	item := reentryReadyItem(t, st, "run-hand-back-wait")
	armActiveTestSchedules(t, st, item)
	activateExternalTestProfile(t, st, activeResourceTestTime.Add(-time.Hour), externalTestMaintainer())
	reconciler, reads := handBackReconciler(st, []string{handBackTestLabel}, false)

	reconcileClean(t, reconciler, "first pass")
	reconcileClean(t, reconciler, "second pass")
	if got := readActiveItem(t, st, item.ID); got.Status != domain.StatusSuperseded {
		t.Fatalf("ready item = status %s; want superseded on the head change", got.Status)
	}
	assertNoReentry(t, st, *item.Subject.RunID)
	if len(*reads) != 0 {
		t.Fatalf("merge base read with no cycle due: %q", *reads)
	}
}

// TestActiveResourceHandBackNeedsTheLabel proves the same comment on an
// unlabeled pull request starts no hand-back cycle: the moved head starts
// the ordinary head_changed re-entry instead.
func TestActiveResourceHandBackNeedsTheLabel(t *testing.T) {
	t.Parallel()
	st := schedTestStore(t)
	item := reentryReadyItem(t, st, "run-hand-back-unlabeled")
	armActiveTestSchedules(t, st, item)
	activateExternalTestProfile(t, st, activeResourceTestTime.Add(-time.Hour), externalTestMaintainer())
	reconciler, reads := handBackReconciler(st, []string{"bug"}, true)

	if _, err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertReentryStarted(t, st, item, domain.ReadinessInvalidationHeadChanged, reentryTestBase, reentryTestNewHead)
	if len(*reads) != 0 {
		t.Fatalf("merge base read for an unlabeled pull request: %q", *reads)
	}
}
