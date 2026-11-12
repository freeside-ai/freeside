package daemoncompose

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// conversationTestBotID is the App bot's account in these tests: its
// conversation comments are its own disposition replies.
const conversationTestBotID = 9900

type fakeAppBotIdentity struct {
	id  int64
	err error
}

func (f *fakeAppBotIdentity) Resolve(context.Context, string) (publish.AppBotIdentity, error) {
	return publish.AppBotIdentity{AppSlug: "freeside", BotUserID: f.id}, f.err
}

func conversationComment(id, author int64, login, body string, at time.Time) publish.PullConversationComment {
	return publish.PullConversationComment{ID: id, AuthorID: author, AuthorLogin: login, Body: body, CreatedAt: at}
}

// conversationActivity is a conversation on the bound pull request: the
// maintainer's comment, the App bot's reply to an earlier finding, and a
// blank comment.
func conversationActivity(maintainerBody string) publish.PullReviewObservation {
	return publish.PullReviewObservation{ConversationComments: []publish.PullConversationComment{
		conversationComment(600100, externalTestMaintainerID, externalTestMaintainerLogin, maintainerBody, activeResourceTestTime),
		conversationComment(600101, conversationTestBotID, "freeside[bot]", "Freeside declined this finding.", activeResourceTestTime.Add(time.Minute)),
		conversationComment(600102, externalTestMaintainerID, externalTestMaintainerLogin, "  \n", activeResourceTestTime.Add(2*time.Minute)),
	}}
}

func conversationReconciler(st *store.Store, review NativeReviewObserver, identity appBotIdentitySource) ActiveResourceReconciler {
	reconciler := externalReentryReconciler(st, review)
	reconciler.appBotIdentity = identity
	return reconciler
}

func storedExternalFindings(t *testing.T, st *store.Store, runID domain.RunID) []domain.Finding {
	t.Helper()
	var findings []domain.Finding
	if err := st.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		findings, err = tx.ListExternalFindings(context.Background(), runID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return findings
}

// TestBuildExternalFindingsConversation proves a conversation comment with a
// body becomes one finding on the ready item's head with no location, and
// that the App bot's own comments, blank comments, and every comment when the
// bot is unknown become none (#3127).
func TestBuildExternalFindingsConversation(t *testing.T) {
	t.Parallel()
	binding := externalTestBinding()
	findings, skipped := buildExternalFindings(conversationActivity("Please also check the docs."), binding, conversationTestBotID)
	if len(skipped) != 0 || len(findings) != 1 {
		t.Fatalf("findings = %+v, skipped = %v; want the maintainer's comment alone", findings, skipped)
	}
	got := findings[0]
	if got.RunID != binding.RunID || got.External.ThreadID != "issue_comment/600100" ||
		got.External.HeadSHA != binding.HeadSHA || got.Location != nil ||
		got.External.ReviewerAccountID != externalTestMaintainerID || got.External.ReviewerLogin != externalTestMaintainerLogin ||
		got.RawText != "Please also check the docs." || !got.CreatedAt.Equal(activeResourceTestTime) {
		t.Fatalf("conversation finding = %+v (%+v)", got, got.External)
	}

	if unknown, _ := buildExternalFindings(conversationActivity("Please also check the docs."), binding, 0); len(unknown) != 0 {
		t.Fatalf("with no known bot, conversation findings = %+v; want none", unknown)
	}
}

// TestActiveResourceConversationCommentStartsCycle proves an admitted
// account's conversation comment on an open ready pull request is stored and
// starts an external review cycle naming it, and that the App bot's own
// comment is not stored at all (#3127).
func TestActiveResourceConversationCommentStartsCycle(t *testing.T) {
	t.Parallel()
	st := schedTestStore(t)
	item := reentryReadyItem(t, st, "run-conversation-start")
	armActiveTestSchedules(t, st, item)
	profile := activateExternalTestProfile(t, st, activeResourceTestTime.Add(-time.Hour), externalTestMaintainer())
	reconciler := conversationReconciler(st, staticReview(conversationActivity("Please also check the docs.")),
		&fakeAppBotIdentity{id: conversationTestBotID})

	reconcileClean(t, &reconciler, "first pass")
	stored := storedExternalFindings(t, st, *item.Subject.RunID)
	if len(stored) != 1 || stored[0].External.ThreadID != "issue_comment/600100" || stored[0].External.HeadSHA != reentryTestHead {
		t.Fatalf("stored findings = %+v; want only the maintainer's comment on the ready head", stored)
	}
	assertExternalReviewStarted(t, st, item, profile, stored[0].ID)
}

// TestActiveResourceConversationCommentFromOthersDrivesNothing proves a
// conversation comment from an account the profile doesn't admit is stored as
// history and starts nothing.
func TestActiveResourceConversationCommentFromOthersDrivesNothing(t *testing.T) {
	t.Parallel()
	st := schedTestStore(t)
	item := reentryReadyItem(t, st, "run-conversation-other")
	armActiveTestSchedules(t, st, item)
	activateExternalTestProfile(t, st, activeResourceTestTime.Add(-time.Hour), externalTestMaintainer())
	activity := publish.PullReviewObservation{ConversationComments: []publish.PullConversationComment{
		conversationComment(600200, 5000, "someone-else", "Looks wrong to me.", activeResourceTestTime),
	}}
	reconciler := conversationReconciler(st, staticReview(activity), &fakeAppBotIdentity{id: conversationTestBotID})

	reconcileClean(t, &reconciler, "pass")
	if stored := storedExternalFindings(t, st, *item.Subject.RunID); len(stored) != 1 ||
		stored[0].External.ReviewerAccountID != 5000 {
		t.Fatalf("stored findings = %+v; want the other account's comment as history", stored)
	}
	assertNoExternalReview(t, st, item)
}

// TestActiveResourceConversationCommentIsFinal proves the first stored
// finding for a conversation comment is final in its run (#3127): an edit
// stores nothing and leaves the client-visible revision alone, the same
// comment read under a later head stores nothing, and listing the author
// afterwards starts a cycle from the first finding.
func TestActiveResourceConversationCommentIsFinal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := schedTestStore(t)
	item := reentryReadyItem(t, st, "run-conversation-final")
	armActiveTestSchedules(t, st, item)
	runID := *item.Subject.RunID
	activity := conversationActivity("Please also check the docs.")
	reconciler := conversationReconciler(st,
		func(context.Context, string, int) (publish.PullReviewObservation, error) { return activity, nil },
		&fakeAppBotIdentity{id: conversationTestBotID})

	reconcileClean(t, &reconciler, "unlisted pass")
	first := storedExternalFindings(t, st, runID)
	if len(first) != 1 {
		t.Fatalf("stored findings = %+v; want one", first)
	}
	revision := readRevision(t, st)

	activity = conversationActivity("Please also check the docs and the README.")
	reconcileClean(t, &reconciler, "edited pass")
	if got := storedExternalFindings(t, st, runID); len(got) != 1 || got[0].ID != first[0].ID {
		t.Fatalf("after an edit, stored findings = %+v; want the first alone", got)
	}
	if got := readRevision(t, st); got != revision {
		t.Fatalf("an edit moved the revision %d to %d", revision, got)
	}

	// The same comment under a later head: the intake binds a conversation
	// comment to the ready head it reads under.
	var binding domain.ReadyItemPRBinding
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		binding, err = tx.GetReadyItemPRBinding(ctx, item.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	later := binding
	later.HeadSHA = strings.Repeat("b", 40)
	moved, _ := buildExternalFindings(conversationActivity("Please also check the docs."), later, conversationTestBotID)
	if len(moved) != 1 || moved[0].ID == first[0].ID {
		t.Fatalf("later-head build = %+v; want one finding with a new ID", moved)
	}
	if err := reconciler.commitExternalFindings(ctx, activeResourceObservation{
		itemID: item.ID, binding: binding, externalFindings: moved,
	}); err != nil {
		t.Fatal(err)
	}
	if got := storedExternalFindings(t, st, runID); len(got) != 1 || got[0].ID != first[0].ID {
		t.Fatalf("after a later head, stored findings = %+v; want the first alone", got)
	}

	profile := activateExternalTestProfile(t, st, activeResourceTestTime, externalTestMaintainer())
	reconcileClean(t, &reconciler, "listed pass")
	assertExternalReviewStarted(t, st, item, profile, first[0].ID)
}

// TestActiveResourceConversationWaitsForBotIdentity proves conversation
// intake fails closed without the App bot's identity: the failure is
// reported, the review and inline findings are still stored, no conversation
// comment is, and the evicted cache lets the next pass store it.
func TestActiveResourceConversationWaitsForBotIdentity(t *testing.T) {
	t.Parallel()
	st := schedTestStore(t)
	item := reentryReadyItem(t, st, "run-conversation-identity")
	armActiveTestSchedules(t, st, item)
	runID := *item.Subject.RunID
	activity := externalReentryActivity()
	activity.ConversationComments = conversationActivity("Please also check the docs.").ConversationComments
	identity := &fakeAppBotIdentity{err: errors.New("installation token unavailable")}
	evicted := 0
	reconciler := conversationReconciler(st, staticReview(activity), identity)
	reconciler.reviewInvalidate = func(string, int) { evicted++ }

	result, err := reconciler.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile hard error: %v", err)
	}
	if len(result.Failures) != 1 || !strings.Contains(result.Failures[0].Error(), "resolve App bot identity") {
		t.Fatalf("failures = %v; want the identity failure alone", result.Failures)
	}
	if evicted != 1 {
		t.Fatalf("review cache evictions = %d, want 1", evicted)
	}
	stored := storedExternalFindings(t, st, runID)
	for _, finding := range stored {
		if publish.IsExternalReviewConversationThread(finding.External.ThreadID) {
			t.Fatalf("stored a conversation finding without the bot's identity: %+v", finding)
		}
	}
	if len(stored) != 3 {
		t.Fatalf("stored findings = %d, want the three inline comments", len(stored))
	}

	*identity = fakeAppBotIdentity{id: conversationTestBotID}
	reconcileClean(t, &reconciler, "retry")
	if got := storedExternalFindings(t, st, runID); len(got) != 4 {
		t.Fatalf("after the retry, stored findings = %d, want the conversation comment added", len(got))
	}
}
