package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const commentBotUserID int64 = 4100

type commentStep func(domain.IssueCommentIntent) (domain.IssueCommentIntent, error)

func commentPreDispatch(ids ...int64) commentStep {
	return func(i domain.IssueCommentIntent) (domain.IssueCommentIntent, error) {
		return i.RecordPreDispatch(ids, commentBotUserID, filingLedgerAt.Add(time.Minute))
	}
}

func commentAttempt(i domain.IssueCommentIntent) (domain.IssueCommentIntent, error) {
	return i.StartAttempt(filingLedgerAt.Add(2 * time.Minute))
}

func commentResponse(class domain.FollowUpFilingResponseClass) commentStep {
	return func(i domain.IssueCommentIntent) (domain.IssueCommentIntent, error) {
		return i.RecordResponse(class, filingLedgerAt.Add(3*time.Minute))
	}
}

func commentLedgerSuccess(id int64) commentStep {
	return func(i domain.IssueCommentIntent) (domain.IssueCommentIntent, error) {
		return i.LedgerSuccess(id, filingLedgerAt.Add(4*time.Minute))
	}
}

func commentAdopt(id int64, issueAmbiguous bool) commentStep {
	return func(i domain.IssueCommentIntent) (domain.IssueCommentIntent, error) {
		return i.Adopt(id, issueAmbiguous, filingLedgerAt.Add(4*time.Minute))
	}
}

func commentRefuse(reason domain.FollowUpFilingRefusalReason) commentStep {
	return func(i domain.IssueCommentIntent) (domain.IssueCommentIntent, error) {
		return i.Refuse(reason, filingLedgerAt.Add(4*time.Minute))
	}
}

func commentAmbiguous(i domain.IssueCommentIntent) (domain.IssueCommentIntent, error) {
	return i.RecordAmbiguous(filingLedgerAt.Add(4 * time.Minute))
}

func commentIntentAfter(t *testing.T, steps ...commentStep) domain.IssueCommentIntent {
	t.Helper()
	intent, err := domain.NewIssueCommentIntent("instance-1", 123, 17, filingLedgerAt)
	if err != nil {
		t.Fatal(err)
	}
	for index, step := range steps {
		if intent, err = step(intent); err != nil {
			t.Fatalf("step %d: %v", index+1, err)
		}
		if err := intent.Validate(); err != nil {
			t.Fatalf("step %d left an invalid intent: %v", index+1, err)
		}
	}
	return intent
}

// TestIssueCommentAttemptRule pins the evidence rule the filing ledger uses:
// a create needs evidence that no earlier create for the instance committed.
func TestIssueCommentAttemptRule(t *testing.T) {
	t.Parallel()
	set := commentPreDispatch()
	cases := []struct {
		name  string
		steps []commentStep
		want  error
	}{
		{"first attempt", []commentStep{set}, nil},
		{"after a transient rejection", []commentStep{
			set, commentAttempt, commentResponse(domain.FollowUpFilingResponseTransientRejection),
		}, nil},
		{"after a definite rejection", []commentStep{
			set, commentAttempt, commentResponse(domain.FollowUpFilingResponseDefiniteRejection),
		}, domain.ErrIssueCommentCreateUnproven},
		{"after an unproven response", []commentStep{
			set, commentAttempt, commentResponse(domain.FollowUpFilingResponseUnproven),
		}, domain.ErrIssueCommentCreateUnproven},
		{"after a marker with no response", []commentStep{set, commentAttempt}, domain.ErrIssueCommentCreateUnproven},
		{"after a success", []commentStep{set, commentAttempt, commentLedgerSuccess(900)}, domain.ErrIssueCommentIntentResolved},
		{"before the pre-dispatch set", nil, domain.ErrIssueCommentPreDispatchMissing},
		{"after a refused outcome", []commentStep{
			set, commentRefuse(domain.FollowUpFilingRefusalPreconditionFailed),
		}, domain.ErrIssueCommentIntentResolved},
		{"after an ambiguous outcome", []commentStep{set, commentAttempt, commentAmbiguous}, domain.ErrIssueCommentIntentResolved},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			intent := commentIntentAfter(t, tc.steps...)
			next, err := intent.StartAttempt(filingLedgerAt.Add(time.Hour))
			if !errors.Is(err, tc.want) {
				t.Fatalf("StartAttempt error = %v, want %v", err, tc.want)
			}
			if err == nil && len(next.Attempts) != len(intent.Attempts)+1 {
				t.Fatalf("attempts = %d, want one more than %d", len(next.Attempts), len(intent.Attempts))
			}
		})
	}
}

func TestIssueCommentLedgeringRule(t *testing.T) {
	t.Parallel()
	set := commentPreDispatch(700, 500)
	open := commentIntentAfter(t, set)
	if open.PreDispatch.CommentIDs[0] != 500 || open.PreDispatch.BotUserID != commentBotUserID {
		t.Fatalf("pre-dispatch = %#v, want sorted ids under the bot", open.PreDispatch)
	}
	if _, err := set(open); !errors.Is(err, domain.ErrIssueCommentPreDispatchFixed) {
		t.Fatalf("second pre-dispatch = %v", err)
	}
	if _, err := open.RecordPreDispatch(nil, commentBotUserID, filingLedgerAt); !errors.Is(err, domain.ErrIssueCommentPreDispatchFixed) {
		t.Fatalf("rewrite = %v", err)
	}
	if _, err := commentIntentAfter(t).RecordPreDispatch(nil, 0, filingLedgerAt); !errors.Is(err, domain.ErrNonPositive) {
		t.Fatalf("no bot identity = %v", err)
	}
	dispatched := commentIntentAfter(t, set, commentAttempt)
	cases := []struct {
		name string
		step commentStep
		want error
	}{
		{"success on a new comment", commentLedgerSuccess(900), nil},
		{"success naming a pre-dispatch comment", commentLedgerSuccess(500), domain.ErrIssueCommentLedgerInconsistent},
		{"adopt a new comment", commentAdopt(901, false), nil},
		{"adopt a pre-dispatch comment", commentAdopt(700, false), domain.ErrIssueCommentAdoptionRefused},
		{"adopt on an ambiguous issue", commentAdopt(901, true), domain.ErrIssueCommentAdoptionRefused},
		{"record ambiguity", commentAmbiguous, nil},
		{"refuse while unproven", commentRefuse(domain.FollowUpFilingRefusalPreconditionFailed), domain.ErrIssueCommentOutcomeRefused},
	}
	for _, tc := range cases {
		if _, err := tc.step(dispatched); !errors.Is(err, tc.want) {
			t.Errorf("%s: error = %v, want %v", tc.name, err, tc.want)
		}
	}
	if _, err := commentAdopt(901, false)(open); !errors.Is(err, domain.ErrIssueCommentAdoptionRefused) {
		t.Fatalf("adopt before any dispatch = %v", err)
	}
	if _, err := commentAmbiguous(open); !errors.Is(err, domain.ErrIssueCommentOutcomeRefused) {
		t.Fatalf("ambiguous before any dispatch = %v", err)
	}
	responded := commentIntentAfter(t, set, commentAttempt, commentResponse(domain.FollowUpFilingResponseDefiniteRejection))
	if _, err := commentResponse(domain.FollowUpFilingResponseUnproven)(responded); !errors.Is(err, domain.ErrIssueCommentResponseConflict) {
		t.Fatalf("second response = %v", err)
	}
	if _, err := commentRefuse(domain.FollowUpFilingRefusalDefiniteRejection)(responded); err != nil {
		t.Fatalf("refuse after a definite rejection: %v", err)
	}
}

// TestIssueCommentMayHaveCreated pins which intents the per-run cap counts:
// any whose create may have committed, never one recorded only as rejected.
func TestIssueCommentMayHaveCreated(t *testing.T) {
	set := commentPreDispatch()
	transient := commentResponse(domain.FollowUpFilingResponseTransientRejection)
	cases := []struct {
		name  string
		steps []commentStep
		want  bool
	}{
		{"open", nil, false},
		{"set and no attempt", []commentStep{set}, false},
		{"only rejected attempts", []commentStep{
			set, commentAttempt, transient, commentAttempt, commentResponse(domain.FollowUpFilingResponseDefiniteRejection),
		}, false},
		{"refused", []commentStep{
			set, commentAttempt, transient, commentRefuse(domain.FollowUpFilingRefusalRetryBoundSpent),
		}, false},
		{"marker with no response", []commentStep{set, commentAttempt}, true},
		{"unproven attempt", []commentStep{set, commentAttempt, commentResponse(domain.FollowUpFilingResponseUnproven)}, true},
		{"ambiguous after a rejection", []commentStep{set, commentAttempt, transient, commentAmbiguous}, true},
		{"ledgered by success", []commentStep{set, commentAttempt, commentLedgerSuccess(900)}, true},
		{"ledgered by adoption", []commentStep{set, commentAttempt, transient, commentAdopt(900, false)}, true},
	}
	for _, tc := range cases {
		if got := commentIntentAfter(t, tc.steps...).MayHaveCreated(); got != tc.want {
			t.Errorf("%s: MayHaveCreated = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestIssueCommentIntentValidateRejectsUnreachableStates(t *testing.T) {
	t.Parallel()
	set := commentPreDispatch(7)
	transient := commentResponse(domain.FollowUpFilingResponseTransientRejection)
	var comment int64 = 40
	cases := []struct {
		name   string
		base   domain.IssueCommentIntent
		mutate func(*domain.IssueCommentIntent)
		want   error
	}{
		{
			"nil attempts", commentIntentAfter(t), func(i *domain.IssueCommentIntent) { i.Attempts = nil },
			domain.ErrIssueCommentLedgerInconsistent,
		},
		{
			"attempt without a pre-dispatch set", commentIntentAfter(t, set, commentAttempt),
			func(i *domain.IssueCommentIntent) { i.PreDispatch = nil }, domain.ErrIssueCommentLedgerInconsistent,
		},
		{
			"unsorted pre-dispatch set", commentIntentAfter(t, set),
			func(i *domain.IssueCommentIntent) { i.PreDispatch.CommentIDs = []int64{9, 7} },
			domain.ErrIssueCommentLedgerInconsistent,
		},
		{
			"ordinal gap", commentIntentAfter(t, set, commentAttempt),
			func(i *domain.IssueCommentIntent) { i.Attempts[0].Ordinal = 2 }, domain.ErrIssueCommentLedgerInconsistent,
		},
		{
			"attempt after an unproven attempt", commentIntentAfter(t, set, commentAttempt, transient, commentAttempt),
			func(i *domain.IssueCommentIntent) { i.Attempts[0].Response = nil }, domain.ErrIssueCommentLedgerInconsistent,
		},
		{
			"success without a ledgered outcome", commentIntentAfter(t, set, commentAttempt, commentLedgerSuccess(40)),
			func(i *domain.IssueCommentIntent) { i.Terminal = nil }, domain.ErrIssueCommentLedgerInconsistent,
		},
		{
			"success naming a pre-dispatch comment", commentIntentAfter(t, set, commentAttempt, commentLedgerSuccess(40)),
			func(i *domain.IssueCommentIntent) { *i.Attempts[0].Response.CommentID = 7 },
			domain.ErrIssueCommentLedgerInconsistent,
		},
		{
			"rejection carrying a comment id", commentIntentAfter(t, set, commentAttempt, transient),
			func(i *domain.IssueCommentIntent) { i.Attempts[0].Response.CommentID = &comment },
			domain.ErrIssueCommentLedgerInconsistent,
		},
		{
			"ledgered with no attempt", commentIntentAfter(t, set, commentAttempt, commentAdopt(40, false)),
			func(i *domain.IssueCommentIntent) { i.Attempts = []domain.IssueCommentAttempt{} },
			domain.ErrIssueCommentLedgerInconsistent,
		},
		{"no issue", commentIntentAfter(t), func(i *domain.IssueCommentIntent) { i.IssueNumber = 0 }, domain.ErrNonPositive},
		{"no repository", commentIntentAfter(t), func(i *domain.IssueCommentIntent) { i.RepositoryID = 0 }, domain.ErrNonPositive},
	}
	for _, tc := range cases {
		intent := tc.base
		tc.mutate(&intent)
		if err := intent.Validate(); !errors.Is(err, tc.want) {
			t.Errorf("%s: Validate = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestPostedIssueCommentBackedBy(t *testing.T) {
	t.Parallel()
	set := commentPreDispatch(7)
	bySuccess := commentIntentAfter(t, set, commentAttempt, commentLedgerSuccess(40))
	byAdoption := commentIntentAfter(t, set, commentAttempt, commentAdopt(41, false))
	row := func(id int64) domain.PostedIssueComment {
		return domain.PostedIssueComment{
			InstanceID: "instance-1", RepositoryID: 123, IssueNumber: 17, CommentID: id, LedgeredAt: filingLedgerAt,
		}
	}
	otherIssue := row(40)
	otherIssue.IssueNumber = 18
	cases := []struct {
		name   string
		row    domain.PostedIssueComment
		intent domain.IssueCommentIntent
		want   error
	}{
		{"the comment the success named", row(40), bySuccess, nil},
		{"an adopted comment", row(41), byAdoption, nil},
		{"not the comment the success named", row(41), bySuccess, domain.ErrIssueCommentLedgerInconsistent},
		{"an adopted pre-dispatch comment", row(7), byAdoption, domain.ErrIssueCommentLedgerInconsistent},
		{"another issue", otherIssue, bySuccess, domain.ErrIssueCommentLedgerInconsistent},
		{"an outstanding intent", row(40), commentIntentAfter(t, set, commentAttempt), domain.ErrIssueCommentLedgerInconsistent},
	}
	for _, tc := range cases {
		if err := tc.row.Validate(); err != nil {
			t.Fatalf("%s: row invalid: %v", tc.name, err)
		}
		if err := tc.row.BackedBy(tc.intent); !errors.Is(err, tc.want) {
			t.Errorf("%s: BackedBy = %v, want %v", tc.name, err, tc.want)
		}
	}
}
