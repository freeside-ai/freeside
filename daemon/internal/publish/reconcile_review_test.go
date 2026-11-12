package publish_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

func intptr(n int) *int { return &n }

// TestReconcilePullReviewActivityConditional proves the four review
// sub-resources are read, then ride If-None-Match with no churn while
// unchanged, and re-read a changed sub-resource while the unchanged siblings
// stay on their 304.
func TestReconcilePullReviewActivityConditional(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	gh.reviews[7] = []fakeReview{{
		ID: 900100, Login: "chatgpt-codex-connector", State: "COMMENTED",
		Body: "findings", CommitID: "cafebabe", SubmittedAt: "2026-01-02T05:04:05Z",
	}}
	gh.reviewComments[7] = []fakeReviewComment{{
		ID: 800200, ReviewID: 900100, Login: "chatgpt-codex-connector",
		Path: "daemon/main.go", Line: intptr(42), Body: "P2: unchecked error",
		CommitID: "cafebabe", CreatedAt: "2026-01-02T05:04:05Z",
	}}
	gh.reactions[7] = []fakeReaction{{
		ID: 700300, Login: "someone-else", Content: "heart", CreatedAt: "2026-01-02T05:00:00Z",
	}}
	gh.conversation[7] = []fakeConversationComment{{
		ID: 600100, AccountID: 4101, Login: "maintainer", Body: "please also check the docs",
		CreatedAt: "2026-01-02T05:06:05Z",
	}}
	r := newTestReconciler(t, gh)

	first, err := r.ReconcilePullReviewActivity(context.Background(), testRepo, 7)
	if err != nil {
		t.Fatalf("first ReconcilePullReviewActivity: %v", err)
	}
	if first.NotModified {
		t.Error("first observation reported NotModified")
	}
	if len(first.Reviews) != 1 || first.Reviews[0].ID != 900100 ||
		first.Reviews[0].CommitID != "cafebabe" || first.Reviews[0].SubmittedAt.IsZero() {
		t.Errorf("reviews = %+v", first.Reviews)
	}
	if len(first.Comments) != 1 || first.Comments[0].ReviewID != 900100 ||
		first.Comments[0].Line != 42 || first.Comments[0].Path != "daemon/main.go" {
		t.Errorf("comments = %+v", first.Comments)
	}
	if len(first.Reactions) != 1 || first.Reactions[0].Content != "heart" {
		t.Errorf("reactions = %+v", first.Reactions)
	}
	if len(first.ConversationComments) != 1 || first.ConversationComments[0] != (publish.PullConversationComment{
		ID: 600100, AuthorID: 4101, AuthorLogin: "maintainer", Body: "please also check the docs",
		CreatedAt: time.Date(2026, 1, 2, 5, 6, 5, 0, time.UTC),
	}) {
		t.Errorf("conversation comments = %+v", first.ConversationComments)
	}

	second, err := r.ReconcilePullReviewActivity(context.Background(), testRepo, 7)
	if err != nil {
		t.Fatalf("second ReconcilePullReviewActivity: %v", err)
	}
	if !second.NotModified {
		t.Error("unchanged review activity did not report NotModified")
	}
	second.NotModified = false
	if len(second.Reviews) != 1 || second.Reviews[0] != first.Reviews[0] ||
		len(second.Comments) != 1 || second.Comments[0] != first.Comments[0] ||
		len(second.Reactions) != 1 || second.Reactions[0] != first.Reactions[0] ||
		len(second.ConversationComments) != 1 || second.ConversationComments[0] != first.ConversationComments[0] {
		t.Errorf("304 churned the observation: %+v vs %+v", second, first)
	}
	if conditionalRequests(gh) != 4 {
		t.Errorf("second poll did not ride If-None-Match on all four sub-resources: %v", gh.requestLog())
	}

	// A new reaction (the clean-pass signal) changes only that sub-resource.
	gh.reactions[7] = append(gh.reactions[7], fakeReaction{
		ID: 700400, Login: "chatgpt-codex-connector", Content: "+1", CreatedAt: "2026-01-02T06:00:00Z",
	})
	gh.reactionRevs[7]++
	third, err := r.ReconcilePullReviewActivity(context.Background(), testRepo, 7)
	if err != nil {
		t.Fatalf("third ReconcilePullReviewActivity: %v", err)
	}
	if third.NotModified {
		t.Error("a changed sub-resource still reported NotModified")
	}
	if len(third.Reactions) != 2 {
		t.Errorf("reactions after append = %+v", third.Reactions)
	}
	// The unchanged reviews and comments stayed on their 304 and kept their
	// cached values.
	if len(third.Reviews) != 1 || third.Reviews[0] != first.Reviews[0] ||
		len(third.Comments) != 1 || third.Comments[0] != first.Comments[0] ||
		len(third.ConversationComments) != 1 || third.ConversationComments[0] != first.ConversationComments[0] {
		t.Errorf("unchanged siblings churned: reviews=%+v comments=%+v conversation=%+v",
			third.Reviews, third.Comments, third.ConversationComments)
	}

	// A new conversation comment changes only that sub-resource, and alone
	// makes the observation a change.
	gh.mu.Lock()
	gh.conversation[7] = append(gh.conversation[7], fakeConversationComment{
		ID: 600101, AccountID: 4101, Login: "maintainer", Body: "and the README", CreatedAt: "2026-01-02T07:00:00Z",
	})
	gh.conversationRevs[7]++
	gh.mu.Unlock()
	fourth, err := r.ReconcilePullReviewActivity(context.Background(), testRepo, 7)
	if err != nil {
		t.Fatalf("fourth ReconcilePullReviewActivity: %v", err)
	}
	if fourth.NotModified || len(fourth.ConversationComments) != 2 || fourth.ConversationComments[1].ID != 600101 {
		t.Errorf("new conversation comment not observed: %+v", fourth)
	}
}

// TestReconcilePullReviewActivityConversationFailsClosed proves a
// conversation listing that names another resource, or that GitHub refuses,
// fails the whole observation: a conversation comment can start a round
// (#3127), so it is never read from a foreign list or treated as absent the
// way a refused reactions read is.
func TestReconcilePullReviewActivityConversationFailsClosed(t *testing.T) {
	t.Parallel()
	t.Run("foreign issue", func(t *testing.T) {
		t.Parallel()
		gh := newFakeGitHub(t)
		seedReviewActivity(gh)
		gh.conversation[7] = []fakeConversationComment{{
			ID: 600100, AccountID: 4101, Login: "maintainer", Body: "elsewhere",
			CreatedAt: "2026-01-02T05:06:05Z", IssueURL: "https://api.github.com/repos/other/repo/issues/7",
		}}
		r := newTestReconciler(t, gh)
		if _, err := r.ReconcilePullReviewActivity(context.Background(), testRepo, 7); err == nil ||
			!strings.Contains(err.Error(), "another resource") {
			t.Fatalf("foreign conversation comment = %v, want a refused observation", err)
		}
	})
	t.Run("refused", func(t *testing.T) {
		t.Parallel()
		gh := newFakeGitHub(t)
		seedReviewActivity(gh)
		gh.conversationRefusal[7] = fakeRefusal{Status: http.StatusForbidden}
		r := newTestReconciler(t, gh)
		if _, err := r.ReconcilePullReviewActivity(context.Background(), testRepo, 7); err == nil {
			t.Fatal("refused conversation read did not fail the observation")
		}
	})
}

// TestReconcilePullReviewActivityReadsAuthorAndOrigin proves the fields
// external review intake binds a finding with are read as the forge returned
// them: the author's account ID, a reply's thread root, and the commit and
// range a comment was left on, which stay fixed after the forge re-anchors
// the comment to a newer head. A response without them decodes to zero values.
func TestReconcilePullReviewActivityReadsAuthorAndOrigin(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	gh.reviews[7] = []fakeReview{
		{
			ID: 900100, AccountID: 4101, Login: "maintainer", State: "CHANGES_REQUESTED",
			Body: "please fix", CommitID: "cafebabe", SubmittedAt: "2026-01-02T05:04:05Z",
		},
		{
			ID: 900101, Login: "no-account", State: "COMMENTED",
			Body: "note", CommitID: "cafebabe", SubmittedAt: "2026-01-02T05:05:05Z",
		},
	}
	gh.reviewComments[7] = []fakeReviewComment{
		{
			ID: 800200, ReviewID: 900100, AccountID: 4101, Login: "maintainer",
			Path: "daemon/main.go", Line: intptr(48), Body: "re-anchored",
			CommitID: "feedface", CreatedAt: "2026-01-02T05:04:05Z",
			OriginalCommitID: "cafebabe", OriginalStartLine: intptr(40), OriginalLine: intptr(42),
		},
		{
			ID: 800201, ReviewID: 900102, AccountID: 4102, Login: "other[bot]",
			Path: "daemon/main.go", Body: "outdated reply",
			CommitID: "feedface", CreatedAt: "2026-01-02T05:06:05Z",
			InReplyToID: 800200, OriginalCommitID: "cafebabe", OriginalLine: intptr(42),
		},
		{
			ID: 800202, ReviewID: 900101, Login: "no-account",
			Path: "daemon/main.go", Line: intptr(7), Body: "old shape",
			CommitID: "cafebabe", CreatedAt: "2026-01-02T05:07:05Z",
		},
	}
	r := newTestReconciler(t, gh)

	obs, err := r.ReconcilePullReviewActivity(context.Background(), testRepo, 7)
	if err != nil {
		t.Fatalf("ReconcilePullReviewActivity: %v", err)
	}
	if len(obs.Reviews) != 2 || len(obs.Comments) != 3 {
		t.Fatalf("observation = %+v", obs)
	}
	if got := obs.Reviews[0]; got.AuthorID != 4101 || got.AuthorLogin != "maintainer" {
		t.Errorf("review author = %d %q", got.AuthorID, got.AuthorLogin)
	}
	if got := obs.Reviews[1]; got.AuthorID != 0 {
		t.Errorf("review without a user id decoded account %d", got.AuthorID)
	}
	moved := obs.Comments[0]
	if moved.AuthorID != 4101 || moved.InReplyToID != 0 ||
		moved.CommitID != "feedface" || moved.OriginalCommitID != "cafebabe" ||
		moved.Line != 48 || moved.OriginalStartLine != 40 || moved.OriginalLine != 42 {
		t.Errorf("re-anchored comment = %+v", moved)
	}
	reply := obs.Comments[1]
	if reply.AuthorID != 4102 || reply.AuthorLogin != "other[bot]" || reply.InReplyToID != 800200 ||
		reply.Line != 0 || reply.OriginalStartLine != 0 || reply.OriginalLine != 42 ||
		reply.OriginalCommitID != "cafebabe" {
		t.Errorf("reply = %+v", reply)
	}
	old := obs.Comments[2]
	if old.AuthorID != 0 || old.InReplyToID != 0 || old.OriginalCommitID != "" ||
		old.OriginalLine != 0 || old.Line != 7 || old.CommitID != "cafebabe" {
		t.Errorf("comment without the new fields = %+v", old)
	}
}

// TestEvictPullReviewActivityForcesUnconditionalRefetch proves eviction defeats
// the 304 suppression that would otherwise strand un-persisted observations:
// after the active-resource reconciler evicts a PR's review-activity cache
// (its durable append failed), the next observation re-fetches every
// sub-resource unconditionally and rebuilds the lists rather than riding a
// NotModified (issue #497).
func TestEvictPullReviewActivityForcesUnconditionalRefetch(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	gh.reviews[7] = []fakeReview{{
		ID: 900100, Login: "chatgpt-codex-connector", State: "COMMENTED",
		Body: "findings", CommitID: "cafebabe", SubmittedAt: "2026-01-02T05:04:05Z",
	}}
	gh.reviewComments[7] = []fakeReviewComment{{
		ID: 800200, ReviewID: 900100, Login: "chatgpt-codex-connector",
		Path: "daemon/main.go", Line: intptr(42), Body: "P2: unchecked error",
		CommitID: "cafebabe", CreatedAt: "2026-01-02T05:04:05Z",
	}}
	r := newTestReconciler(t, gh)

	if _, err := r.ReconcilePullReviewActivity(context.Background(), testRepo, 7); err != nil {
		t.Fatalf("first ReconcilePullReviewActivity: %v", err)
	}
	// A second poll without eviction rides If-None-Match and 304.
	second, err := r.ReconcilePullReviewActivity(context.Background(), testRepo, 7)
	if err != nil {
		t.Fatalf("second ReconcilePullReviewActivity: %v", err)
	}
	if !second.NotModified {
		t.Fatal("unchanged activity did not ride NotModified before eviction")
	}
	condAfterSecond := conditionalRequests(gh)

	// Eviction drops the validators, so the next poll re-fetches unconditionally
	// and rebuilds the full lists rather than reporting NotModified.
	r.EvictPullReviewActivity(testRepo, 7)
	third, err := r.ReconcilePullReviewActivity(context.Background(), testRepo, 7)
	if err != nil {
		t.Fatalf("third ReconcilePullReviewActivity: %v", err)
	}
	if third.NotModified {
		t.Fatal("post-eviction poll rode NotModified instead of re-fetching")
	}
	if len(third.Reviews) != 1 || third.Reviews[0].ID != 900100 ||
		len(third.Comments) != 1 || third.Comments[0].ID != 800200 {
		t.Fatalf("post-eviction poll did not rebuild the lists: %+v", third)
	}
	if got := conditionalRequests(gh); got != condAfterSecond {
		t.Fatalf("post-eviction poll issued %d new conditional requests, want 0 (unconditional re-fetch)",
			got-condAfterSecond)
	}
}

// TestReconcilePullReviewActivitySkipsPending proves a pending (never
// submitted) review is not observed.
func TestReconcilePullReviewActivitySkipsPending(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	gh.reviews[7] = []fakeReview{
		{ID: 1, Login: "chatgpt-codex-connector", State: "PENDING", SubmittedAt: ""},
		{ID: 2, Login: "chatgpt-codex-connector", State: "COMMENTED", CommitID: "cafebabe", SubmittedAt: "2026-01-02T05:04:05Z"},
	}
	r := newTestReconciler(t, gh)
	obs, err := r.ReconcilePullReviewActivity(context.Background(), testRepo, 7)
	if err != nil {
		t.Fatalf("ReconcilePullReviewActivity: %v", err)
	}
	if len(obs.Reviews) != 1 || obs.Reviews[0].ID != 2 {
		t.Errorf("pending review was not skipped: %+v", obs.Reviews)
	}
}

// TestReconcilePullReviewActivityValidation fails fast on a bad repo or number.
func TestReconcilePullReviewActivityValidation(t *testing.T) {
	t.Parallel()
	r := newTestReconciler(t, newFakeGitHub(t))
	if _, err := r.ReconcilePullReviewActivity(context.Background(), "no-slash", 7); err == nil {
		t.Error("bad repo did not error")
	}
	if _, err := r.ReconcilePullReviewActivity(context.Background(), testRepo, 0); err == nil {
		t.Error("non-positive number did not error")
	}
}

// TestReconcilePullReviewActivityMultiPageDropsValidator proves reviews,
// review comments, and conversation comments that span pages never ride a
// first-page 304: each can start a review round (issues #524 and #3127), and
// a first-page validator cannot see one appended to a later page. Reactions
// keep theirs.
func TestReconcilePullReviewActivityMultiPageDropsValidator(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	gh.reviewActivityPageSize = 2
	review := func(id int64) fakeReview {
		return fakeReview{
			ID: id, AccountID: 4101, Login: "maintainer", State: "COMMENTED",
			Body: "note", CommitID: "cafebabe", SubmittedAt: "2026-01-02T05:04:05Z",
		}
	}
	comment := func(id int64) fakeReviewComment {
		return fakeReviewComment{
			ID: id, ReviewID: 900100, AccountID: 4101, Login: "maintainer",
			Path: "daemon/main.go", Line: intptr(7), Body: "inline",
			CommitID: "cafebabe", CreatedAt: "2026-01-02T05:04:05Z",
		}
	}
	conversation := func(id int64) fakeConversationComment {
		return fakeConversationComment{
			ID: id, AccountID: 4101, Login: "maintainer", Body: "note", CreatedAt: "2026-01-02T05:04:05Z",
		}
	}
	gh.reviews[7] = []fakeReview{review(900100), review(900101), review(900102)}
	gh.reviewComments[7] = []fakeReviewComment{comment(800200), comment(800201), comment(800202)}
	gh.conversation[7] = []fakeConversationComment{conversation(600100), conversation(600101), conversation(600102)}
	r := newTestReconciler(t, gh)

	first, err := r.ReconcilePullReviewActivity(context.Background(), testRepo, 7)
	if err != nil {
		t.Fatalf("first ReconcilePullReviewActivity: %v", err)
	}
	if first.NotModified || len(first.Reviews) != 3 || len(first.Comments) != 3 || len(first.ConversationComments) != 3 {
		t.Fatalf("first multi-page observation = %+v", first)
	}

	// A review and a comment land on the later pages. Neither list's revision
	// is bumped, so each first page would still answer its validator with 304.
	gh.mu.Lock()
	gh.reviews[7] = append(gh.reviews[7], review(900103))
	gh.reviewComments[7] = append(gh.reviewComments[7], comment(800203))
	gh.conversation[7] = append(gh.conversation[7], conversation(600103))
	gh.mu.Unlock()
	before := len(gh.requestLog())
	second, err := r.ReconcilePullReviewActivity(context.Background(), testRepo, 7)
	if err != nil {
		t.Fatalf("second ReconcilePullReviewActivity: %v", err)
	}
	if second.NotModified || len(second.Reviews) != 4 || len(second.Comments) != 4 || len(second.ConversationComments) != 4 {
		t.Fatalf("later-page activity not observed: %d reviews, %d comments, %d conversation comments, not modified %t",
			len(second.Reviews), len(second.Comments), len(second.ConversationComments), second.NotModified)
	}
	conditional := 0
	for _, request := range gh.requestLog()[before:] {
		if strings.HasSuffix(request, " if-none-match") {
			conditional++
			if !strings.Contains(request, "/reactions") {
				t.Errorf("multi-page list sent a first-page validator: %s", request)
			}
		}
	}
	if conditional != 1 {
		t.Errorf("conditional requests = %d, want the reactions list only", conditional)
	}
}

// reactionsReadConditional reports whether any logged reactions read for the
// fake's PR 7 rode If-None-Match, counting from the log entry at index from.
func reactionsReadConditional(gh *fakeGitHub, from int) bool {
	for _, request := range gh.requestLog()[from:] {
		if strings.Contains(request, "/issues/7/reactions") && strings.HasSuffix(request, " if-none-match") {
			return true
		}
	}
	return false
}

// seedReviewActivity gives PR 7 one review and one inline comment.
func seedReviewActivity(gh *fakeGitHub) {
	gh.reviews[7] = []fakeReview{{
		ID: 900100, Login: "chatgpt-codex-connector", State: "COMMENTED",
		Body: "findings", CommitID: "cafebabe", SubmittedAt: "2026-01-02T05:04:05Z",
	}}
	gh.reviewComments[7] = []fakeReviewComment{{
		ID: 800200, ReviewID: 900100, Login: "chatgpt-codex-connector",
		Path: "daemon/main.go", Line: intptr(42), Body: "P2: unchecked error",
		CommitID: "cafebabe", CreatedAt: "2026-01-02T05:04:05Z",
	}}
}

// TestReconcilePullReviewActivityReactionsRefused proves a reactions read
// GitHub refuses with a plain 403 still returns the reviews and comments,
// marks the reactions unavailable rather than empty, rides NotModified once
// the refusal repeats over unchanged activity, and reads the reactions
// unconditionally once the refusal lifts (#2717).
func TestReconcilePullReviewActivityReactionsRefused(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	seedReviewActivity(gh)
	gh.reactions[7] = []fakeReaction{{
		ID: 700400, Login: "chatgpt-codex-connector", Content: "+1", CreatedAt: "2026-01-02T06:00:00Z",
	}}
	gh.reactionsRefusal[7] = fakeRefusal{Status: 403}
	r := newTestReconciler(t, gh)
	ctx := context.Background()

	first, err := r.ReconcilePullReviewActivity(ctx, testRepo, 7)
	if err != nil {
		t.Fatalf("refused pass: %v", err)
	}
	if len(first.Reviews) != 1 || first.Reviews[0].ID != 900100 ||
		len(first.Comments) != 1 || first.Comments[0].ID != 800200 {
		t.Fatalf("refused pass lost the reviews or comments: %+v", first)
	}
	if !first.ReactionsUnavailable || first.Reactions != nil {
		t.Fatalf("refused reactions = %+v (unavailable %v), want nil and unavailable",
			first.Reactions, first.ReactionsUnavailable)
	}
	if first.NotModified {
		t.Fatal("first refused pass reported NotModified")
	}

	// Still refused, reviews and comments unchanged: the whole activity is
	// unchanged, so the caller doesn't rebuild it every pass.
	second, err := r.ReconcilePullReviewActivity(ctx, testRepo, 7)
	if err != nil {
		t.Fatalf("second refused pass: %v", err)
	}
	if !second.NotModified || !second.ReactionsUnavailable || second.Reactions != nil {
		t.Fatalf("second refused pass = NotModified %v, unavailable %v, reactions %+v; want true, true, nil",
			second.NotModified, second.ReactionsUnavailable, second.Reactions)
	}

	// The refusal lifts: the reactions are read without a validator and the
	// observation is a change.
	delete(gh.reactionsRefusal, 7)
	logged := len(gh.requestLog())
	third, err := r.ReconcilePullReviewActivity(ctx, testRepo, 7)
	if err != nil {
		t.Fatalf("recovered pass: %v", err)
	}
	if reactionsReadConditional(gh, logged) {
		t.Fatalf("recovered reactions read rode If-None-Match: %v", gh.requestLog()[logged:])
	}
	if third.ReactionsUnavailable || third.NotModified ||
		len(third.Reactions) != 1 || third.Reactions[0].ID != 700400 {
		t.Fatalf("recovered pass = %+v, want the reaction, available, and modified", third)
	}
	if len(third.Reviews) != 1 || len(third.Comments) != 1 {
		t.Fatalf("recovered pass lost the cached reviews or comments: %+v", third)
	}
}

// TestReconcilePullReviewActivityRefusalAfterReactionsIsChange proves a
// refusal that follows a successful reactions read drops the cached
// reactions and reports a change, even with the reviews and comments on 304:
// the reactions the caller last saw are no longer known.
func TestReconcilePullReviewActivityRefusalAfterReactionsIsChange(t *testing.T) {
	t.Parallel()
	gh := newFakeGitHub(t)
	seedReviewActivity(gh)
	gh.reactions[7] = []fakeReaction{{
		ID: 700400, Login: "chatgpt-codex-connector", Content: "+1", CreatedAt: "2026-01-02T06:00:00Z",
	}}
	r := newTestReconciler(t, gh)
	ctx := context.Background()

	if first, err := r.ReconcilePullReviewActivity(ctx, testRepo, 7); err != nil || len(first.Reactions) != 1 {
		t.Fatalf("first pass = %+v, %v", first, err)
	}
	gh.reactionsRefusal[7] = fakeRefusal{Status: 403}
	second, err := r.ReconcilePullReviewActivity(ctx, testRepo, 7)
	if err != nil {
		t.Fatalf("refused pass: %v", err)
	}
	if second.NotModified || !second.ReactionsUnavailable || second.Reactions != nil {
		t.Fatalf("refusal after reactions = NotModified %v, unavailable %v, reactions %+v; want false, true, nil",
			second.NotModified, second.ReactionsUnavailable, second.Reactions)
	}
	if len(second.Reviews) != 1 || len(second.Comments) != 1 {
		t.Fatalf("refused pass lost the cached reviews or comments: %+v", second)
	}
}

// TestReconcilePullReviewActivityReactionsFailuresStillFatal proves only a
// plain 403 counts as a refusal: a rate-limited 403 (either signal) and a
// 5xx fail the whole observation as before, and leave the cache alone so the
// next pass retries from the last good state.
func TestReconcilePullReviewActivityReactionsFailuresStillFatal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		refusal fakeRefusal
	}{
		{"retry-after 403", fakeRefusal{Status: 403, Header: map[string]string{"Retry-After": "60"}}},
		{"exhausted 403", fakeRefusal{Status: 403, Header: map[string]string{"X-RateLimit-Remaining": "0"}}},
		{"500", fakeRefusal{Status: 500}},
		{"404", fakeRefusal{Status: 404}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gh := newFakeGitHub(t)
			seedReviewActivity(gh)
			r := newTestReconciler(t, gh)
			ctx := context.Background()
			if _, err := r.ReconcilePullReviewActivity(ctx, testRepo, 7); err != nil {
				t.Fatalf("first pass: %v", err)
			}

			gh.reactionsRefusal[7] = tc.refusal
			if obs, err := r.ReconcilePullReviewActivity(ctx, testRepo, 7); err == nil {
				t.Fatalf("%s returned %+v, want the whole observation to fail", tc.name, obs)
			}

			// The failed pass wrote no cache entry: once the error clears, the
			// unchanged activity still rides its validators.
			delete(gh.reactionsRefusal, 7)
			obs, err := r.ReconcilePullReviewActivity(ctx, testRepo, 7)
			if err != nil {
				t.Fatalf("retry: %v", err)
			}
			if !obs.NotModified || obs.ReactionsUnavailable {
				t.Fatalf("retry = NotModified %v, unavailable %v; want true, false",
					obs.NotModified, obs.ReactionsUnavailable)
			}
		})
	}
}
