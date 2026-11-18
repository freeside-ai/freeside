package daemoncompose

import (
	"context"
	"errors"
	"fmt"
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

// handBackNotices returns every hand_back_comment_missing notice, in any
// status.
func handBackNotices(t *testing.T, st *store.Store) []domain.AttentionItem {
	t.Helper()
	var snapshots []store.Snapshotted[domain.AttentionItem]
	if err := st.Read(context.Background(), func(tx *store.ReadTx) (err error) {
		snapshots, err = tx.ListAttentionItems(context.Background())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var notices []domain.AttentionItem
	for _, snapshot := range snapshots {
		if item := snapshot.Value; item.HealthDiagnostic != nil && item.HealthDiagnostic.Code == handBackCommentMissingCode {
			notices = append(notices, item)
		}
	}
	return notices
}

// assertHandBackNotice requires exactly one notice and that it is open on
// head, observed through ready, with a reason that names the pull request,
// the head, and what is missing.
func assertHandBackNotice(t *testing.T, st *store.Store, ready domain.ItemID, head, missing string) {
	t.Helper()
	notices := handBackNotices(t, st)
	if len(notices) != 1 {
		t.Fatalf("hand-back notices = %#v, want one", notices)
	}
	got := notices[0]
	if got.ID != handBackWaitItemID(ready, head) || got.Status != domain.StatusOpen ||
		got.Type != domain.AttentionSystemHealth || got.Subject.Type != domain.SubjectSystem ||
		got.Posture == nil || *got.Posture != domain.HealthPostureAdvisory ||
		got.HealthDiagnostic.Impairs != domain.ImpairedCapabilityNone {
		t.Fatalf("hand-back notice = %#v, want an open advisory system notice for %s on %s", got, ready, head)
	}
	for _, want := range []string{"owner/repo#450", head, missing} {
		if !strings.Contains(got.Reason, want) {
			t.Fatalf("hand-back notice reason = %q, want it to name %q", got.Reason, want)
		}
	}
}

func assertHandBackNoticeResolved(t *testing.T, st *store.Store, id domain.ItemID) {
	t.Helper()
	if got := readActiveItem(t, st, id); got.Status != domain.StatusResolved {
		t.Fatalf("hand-back notice %s = status %s, want resolved", id, got.Status)
	}
}

// TestActiveResourceHandBackRaisesCommentNotice proves a labeled pull
// request whose moved head no one commented on raises one advisory notice
// on the second pass that sees it wait, through the concluded ready item,
// raises nothing more while it still waits, and resolves it when the label
// comes off (issue #3192).
func TestActiveResourceHandBackRaisesCommentNotice(t *testing.T) {
	t.Parallel()
	st := schedTestStore(t)
	item := reentryReadyItem(t, st, "run-hand-back-notice")
	armActiveTestSchedules(t, st, item)
	activateExternalTestProfile(t, st, activeResourceTestTime.Add(-time.Hour), externalTestMaintainer())
	reconciler, _ := handBackReconciler(st, []string{handBackTestLabel}, false)

	reconcileClean(t, reconciler, "pass that supersedes")
	reconcileClean(t, reconciler, "first waiting pass")
	if notices := handBackNotices(t, st); len(notices) != 0 {
		t.Fatalf("first waiting pass raised %#v, want nothing yet", notices)
	}
	reconcileClean(t, reconciler, "second waiting pass")
	assertHandBackNotice(t, st, item.ID, reentryTestNewHead, "No one has commented on this head.")
	raised := readRevision(t, st)
	reconcileClean(t, reconciler, "third waiting pass")
	if got := readRevision(t, st); got != raised || len(handBackNotices(t, st)) != 1 {
		t.Fatalf("a pass that still waits moved the revision %d to %d or raised another notice", raised, got)
	}
	assertNoReentry(t, st, *item.Subject.RunID)

	reconciler.Pull = func(context.Context, string, int) (publish.PullObservation, error) {
		observed := exactPull("open", false)
		observed.HeadSHA = reentryTestNewHead
		return observed, nil
	}
	reconcileClean(t, reconciler, "pass with the label off")
	assertHandBackNoticeResolved(t, st, handBackWaitItemID(item.ID, reentryTestNewHead))
	assertNoReentry(t, st, *item.Subject.RunID)

	// One notice per ready item and head, in any status: the label put back
	// on the same head raises no second one.
	relabeled, _ := handBackReconciler(st, []string{handBackTestLabel}, false)
	relabeled.handBackGapSeen = reconciler.handBackGapSeen
	reconcileClean(t, relabeled, "first pass with the label back")
	reconcileClean(t, relabeled, "second pass with the label back")
	if notices := handBackNotices(t, st); len(notices) != 1 || notices[0].Status != domain.StatusResolved {
		t.Fatalf("hand-back notices = %#v, want the one resolved notice", notices)
	}
}

// TestActiveResourceHandBackNoNoticeWhileACycleRuns proves a labeled head
// with no comment raises no notice while another cycle's task still acts
// for the run: here an external review cycle the maintainer's inline
// comments on the published head started before the lander pushed.
func TestActiveResourceHandBackNoNoticeWhileACycleRuns(t *testing.T) {
	t.Parallel()
	st := schedTestStore(t)
	item := reentryReadyItem(t, st, "run-hand-back-cycle-running")
	armActiveTestSchedules(t, st, item)
	activateExternalTestProfile(t, st, activeResourceTestTime.Add(-time.Hour), externalTestMaintainer())
	review := externalReentryReconciler(st, staticReview(externalReentryActivity()))
	reconcileClean(t, &review, "pass that starts the external review cycle")
	if successor, queued := readReentry(t, st, *item.Subject.RunID); successor == nil || !queued ||
		successor.HandsBack() {
		t.Fatalf("cycle = authority %+v, task queued %t; want a queued external review cycle", successor, queued)
	}

	reconciler, _ := handBackReconciler(st, []string{handBackTestLabel}, false)
	for _, step := range []string{"first labeled pass", "second labeled pass", "third labeled pass"} {
		reconcileClean(t, reconciler, step)
	}
	if notices := handBackNotices(t, st); len(notices) != 0 {
		t.Fatalf("hand-back notices = %#v, want none while a cycle runs", notices)
	}
}

// TestActiveResourceHandBackNoticeNamesUnlistedCommenters proves the notice
// names an account that commented but is not on the allowlist, and that
// the admitted maintainer's comment, which starts the cycle, resolves it.
func TestActiveResourceHandBackNoticeNamesUnlistedCommenters(t *testing.T) {
	t.Parallel()
	st := schedTestStore(t)
	item := reentryReadyItem(t, st, "run-hand-back-unlisted")
	armActiveTestSchedules(t, st, item)
	activateExternalTestProfile(t, st, activeResourceTestTime.Add(-time.Hour), externalTestMaintainer())
	reconciler, _ := handBackReconciler(st, []string{handBackTestLabel}, false)
	stranger := conversationComment(600300, 4999, "lander-bot", "Handed back: CI is red.", activeResourceTestTime)
	reconciler.Review = staticReview(publish.PullReviewObservation{
		ConversationComments: []publish.PullConversationComment{stranger},
	})

	for _, step := range []string{"pass that supersedes", "first waiting pass", "second waiting pass"} {
		reconcileClean(t, reconciler, step)
	}
	assertHandBackNotice(t, st, item.ID, reentryTestNewHead,
		"Comments from lander-bot are from accounts the trust profile's external-reviewer allowlist does not admit to drive a round.")
	assertNoReentry(t, st, *item.Subject.RunID)

	reconciler.Review = staticReview(publish.PullReviewObservation{
		ConversationComments: []publish.PullConversationComment{stranger, conversationComment(
			600301, externalTestMaintainerID, externalTestMaintainerLogin,
			"Handed back: the rebase broke the build.", activeResourceTestTime.Add(time.Minute))},
	})
	reconcileClean(t, reconciler, "pass with the maintainer's comment")
	if successor, queued := readReentry(t, st, *item.Subject.RunID); successor == nil || !queued ||
		!successor.HandsBack() {
		t.Fatalf("cycle = authority %+v, task queued %t; want a hand_back cycle", successor, queued)
	}
	assertHandBackNoticeResolved(t, st, handBackWaitItemID(item.ID, reentryTestNewHead))
}

// TestActiveResourceHandBackNoticeOnTheReadyHead proves a labeled pull
// request whose head did not move raises the notice through its open ready
// item, that the head moving on resolves it, and that the pull request
// closing resolves the new head's notice.
func TestActiveResourceHandBackNoticeOnTheReadyHead(t *testing.T) {
	t.Parallel()
	st := schedTestStore(t)
	item := reentryReadyItem(t, st, "run-hand-back-ready-head")
	armActiveTestSchedules(t, st, item)
	activateExternalTestProfile(t, st, activeResourceTestTime.Add(-time.Hour), externalTestMaintainer())
	reconciler, _ := handBackReconciler(st, []string{handBackTestLabel}, false)
	pullOn := func(state, head string) {
		reconciler.Pull = func(context.Context, string, int) (publish.PullObservation, error) {
			observed := exactPull(state, false)
			observed.HeadSHA = head
			observed.Labels = []string{handBackTestLabel}
			return observed, nil
		}
	}

	pullOn("open", reentryTestHead)
	reconcileClean(t, reconciler, "first waiting pass")
	reconcileClean(t, reconciler, "second waiting pass")
	if got := readActiveItem(t, st, item.ID); got.Status != domain.StatusOpen {
		t.Fatalf("ready item = status %s, want open on its own head", got.Status)
	}
	assertHandBackNotice(t, st, item.ID, reentryTestHead, "No one has commented on this head.")

	pullOn("open", reentryTestNewHead)
	reconcileClean(t, reconciler, "pass that sees the head move")
	assertHandBackNoticeResolved(t, st, handBackWaitItemID(item.ID, reentryTestHead))
	reconcileClean(t, reconciler, "first pass waiting on the new head")
	reconcileClean(t, reconciler, "second pass waiting on the new head")
	newHead := handBackWaitItemID(item.ID, reentryTestNewHead)
	if got := readActiveItem(t, st, newHead); got.Status != domain.StatusOpen {
		t.Fatalf("new head's notice = status %s, want open", got.Status)
	}

	pullOn("closed", reentryTestNewHead)
	reconcileClean(t, reconciler, "pass that sees the pull request closed")
	assertHandBackNoticeResolved(t, st, newHead)
}

// TestActiveResourceHandBackAdmittedCommentRaisesNoNotice proves an admitted
// comment on the head raises no notice though the cycle it makes due cannot
// start: the comment is there, and the refusal is reported on its own.
func TestActiveResourceHandBackAdmittedCommentRaisesNoNotice(t *testing.T) {
	t.Parallel()
	st := schedTestStore(t)
	item := reentryReadyItem(t, st, "run-hand-back-not-ready")
	armActiveTestSchedules(t, st, item)
	activateExternalTestProfile(t, st, activeResourceTestTime.Add(-time.Hour), externalTestMaintainer())
	reconciler, _ := handBackReconciler(st, []string{handBackTestLabel}, true)
	reconciler.readinessGate = func(context.Context, *store.ReadTx, domain.ProjectID) error {
		return fmt.Errorf("fixture: %w", domain.ErrProjectNotReady)
	}

	reconcileClean(t, reconciler, "pass that supersedes")
	for _, step := range []string{"first refused pass", "second refused pass", "third refused pass"} {
		result, err := reconciler.Reconcile(context.Background())
		if err != nil || len(result.Failures) != 1 || !errors.Is(result.Failures[0], domain.ErrProjectNotReady) {
			t.Fatalf("%s: Reconcile = %+v, %v; want the readiness refusal alone", step, result, err)
		}
	}
	if notices := handBackNotices(t, st); len(notices) != 0 {
		t.Fatalf("hand-back notices = %#v, want none with an admitted comment", notices)
	}
	assertNoReentry(t, st, *item.Subject.RunID)
}
