package domain_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

var filingLedgerAt = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

// filingBotUserID is the App bot account every test history lists under.
const filingBotUserID int64 = 4100

// filingStep is one ledger transition, so a test names the history it needs.
type filingStep func(domain.FollowUpFilingIntent) (domain.FollowUpFilingIntent, error)

func filingPreDispatch(numbers ...int) filingStep {
	return func(i domain.FollowUpFilingIntent) (domain.FollowUpFilingIntent, error) {
		return i.RecordPreDispatch(numbers, filingBotUserID, filingLedgerAt.Add(time.Minute))
	}
}

// filingForgetIdentity leaves the intent as a row written before the
// dispatching identity was recorded reads back: its set names no account.
func filingForgetIdentity(i domain.FollowUpFilingIntent) (domain.FollowUpFilingIntent, error) {
	set := *i.PreDispatch
	set.BotUserID = nil
	i.PreDispatch = &set
	return i, nil
}

func filingAttempt(i domain.FollowUpFilingIntent) (domain.FollowUpFilingIntent, error) {
	return i.StartAttempt(filingLedgerAt.Add(2 * time.Minute))
}

func filingResponse(class domain.FollowUpFilingResponseClass) filingStep {
	return func(i domain.FollowUpFilingIntent) (domain.FollowUpFilingIntent, error) {
		return i.RecordResponse(class, filingLedgerAt.Add(3*time.Minute))
	}
}

func filingLedgerSuccess(issue int) filingStep {
	return func(i domain.FollowUpFilingIntent) (domain.FollowUpFilingIntent, error) {
		return i.LedgerSuccess(issue, filingLedgerAt.Add(4*time.Minute))
	}
}

func filingAdopt(issue int, repositoryAmbiguous bool) filingStep {
	return func(i domain.FollowUpFilingIntent) (domain.FollowUpFilingIntent, error) {
		return i.Adopt(issue, repositoryAmbiguous, filingLedgerAt.Add(4*time.Minute))
	}
}

func filingRefuse(reason domain.FollowUpFilingRefusalReason) filingStep {
	return func(i domain.FollowUpFilingIntent) (domain.FollowUpFilingIntent, error) {
		return i.Refuse(reason, filingLedgerAt.Add(4*time.Minute))
	}
}

func filingAmbiguous(i domain.FollowUpFilingIntent) (domain.FollowUpFilingIntent, error) {
	return i.RecordAmbiguous(filingLedgerAt.Add(4 * time.Minute))
}

// filingIntentAfter opens an intent and applies steps, each of which must
// succeed and leave a valid intent.
func filingIntentAfter(t *testing.T, steps ...filingStep) domain.FollowUpFilingIntent {
	t.Helper()
	intent, err := domain.NewFollowUpFilingIntent("instance-1", 123, filingLedgerAt)
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

// TestFollowUpFilingAttemptRule pins the §5.17 evidence rule: a create needs
// evidence that no earlier create for the instance committed.
func TestFollowUpFilingAttemptRule(t *testing.T) {
	t.Parallel()
	set := filingPreDispatch()
	cases := []struct {
		name  string
		steps []filingStep
		want  error
	}{
		{"first attempt", []filingStep{set}, nil},
		{"after a transient rejection", []filingStep{
			set, filingAttempt, filingResponse(domain.FollowUpFilingResponseTransientRejection),
		}, nil},
		{"after a definite rejection", []filingStep{
			set, filingAttempt, filingResponse(domain.FollowUpFilingResponseDefiniteRejection),
		}, domain.ErrFollowUpFilingCreateUnproven},
		{"after an unproven response", []filingStep{
			set, filingAttempt, filingResponse(domain.FollowUpFilingResponseUnproven),
		}, domain.ErrFollowUpFilingCreateUnproven},
		{"after a marker with no response", []filingStep{set, filingAttempt}, domain.ErrFollowUpFilingCreateUnproven},
		{"after a success", []filingStep{set, filingAttempt, filingLedgerSuccess(40)}, domain.ErrFollowUpFilingIntentResolved},
		{"before the pre-dispatch set", nil, domain.ErrFollowUpFilingPreDispatchMissing},
		{"under a set with no recorded identity", []filingStep{set, filingForgetIdentity}, domain.ErrFollowUpFilingIdentityMissing},
		{"after a transient rejection with no recorded identity", []filingStep{
			set, filingAttempt, filingResponse(domain.FollowUpFilingResponseTransientRejection), filingForgetIdentity,
		}, domain.ErrFollowUpFilingIdentityMissing},
		{"after a refused outcome", []filingStep{
			set, filingRefuse(domain.FollowUpFilingRefusalPreconditionFailed),
		}, domain.ErrFollowUpFilingIntentResolved},
		{"after an ambiguous outcome", []filingStep{set, filingAttempt, filingAmbiguous}, domain.ErrFollowUpFilingIntentResolved},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			intent := filingIntentAfter(t, tc.steps...)
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

func TestFollowUpFilingPreDispatchIsWrittenOnce(t *testing.T) {
	t.Parallel()
	open := filingIntentAfter(t)
	recorded, err := open.RecordPreDispatch([]int{9, 7, 9}, filingBotUserID, filingLedgerAt)
	if err != nil {
		t.Fatal(err)
	}
	if got := recorded.PreDispatch.IssueNumbers; !slices.Equal(got, []int{7, 9}) {
		t.Fatalf("pre-dispatch set = %v, want it sorted and duplicate-free", got)
	}
	if got := recorded.PreDispatch.BotUserID; got == nil || *got != filingBotUserID {
		t.Fatalf("pre-dispatch identity = %v, want bot user %d", got, filingBotUserID)
	}
	if open.PreDispatch != nil {
		t.Fatal("recording the set changed the intent it was applied to")
	}
	empty := filingIntentAfter(t, filingPreDispatch())
	if empty.PreDispatch == nil || empty.PreDispatch.IssueNumbers == nil || len(empty.PreDispatch.IssueNumbers) != 0 {
		t.Fatalf("empty pre-dispatch set = %+v, want a recorded empty set", empty.PreDispatch)
	}
	for name, intent := range map[string]domain.FollowUpFilingIntent{
		"a second set":              recorded,
		"a set after an attempt":    filingIntentAfter(t, filingPreDispatch(), filingAttempt),
		"a set on an empty set":     empty,
		"a set on a refused intent": filingIntentAfter(t, filingRefuse(domain.FollowUpFilingRefusalPreconditionFailed)),
	} {
		_, err := intent.RecordPreDispatch([]int{12}, filingBotUserID, filingLedgerAt)
		want := domain.ErrFollowUpFilingPreDispatchFixed
		if intent.Terminal != nil {
			want = domain.ErrFollowUpFilingIntentResolved
		}
		if !errors.Is(err, want) {
			t.Errorf("%s: error = %v, want %v", name, err, want)
		}
	}
	if _, err := open.RecordPreDispatch([]int{0}, filingBotUserID, filingLedgerAt); !errors.Is(err, domain.ErrNonPositive) {
		t.Errorf("issue number 0: error = %v, want %v", err, domain.ErrNonPositive)
	}
	for _, botUserID := range []int64{0, -1} {
		if _, err := open.RecordPreDispatch(nil, botUserID, filingLedgerAt); !errors.Is(err, domain.ErrNonPositive) {
			t.Errorf("bot user %d: error = %v, want %v", botUserID, err, domain.ErrNonPositive)
		}
	}
}

func TestFollowUpFilingResponseIsRecordedOnce(t *testing.T) {
	t.Parallel()
	dispatched := filingIntentAfter(t, filingPreDispatch(), filingAttempt)
	cases := []struct {
		name   string
		intent domain.FollowUpFilingIntent
		class  domain.FollowUpFilingResponseClass
		want   error
	}{
		{"unproven on the marker", dispatched, domain.FollowUpFilingResponseUnproven, nil},
		{
			"no attempt", filingIntentAfter(t, filingPreDispatch()),
			domain.FollowUpFilingResponseUnproven, domain.ErrFollowUpFilingResponseConflict,
		},
		{
			"a second response", filingIntentAfter(t, filingPreDispatch(), filingAttempt,
				filingResponse(domain.FollowUpFilingResponseUnproven)),
			domain.FollowUpFilingResponseDefiniteRejection, domain.ErrFollowUpFilingResponseConflict,
		},
		{
			"success without its issue", dispatched,
			domain.FollowUpFilingResponseSuccess, domain.ErrFollowUpFilingLedgerInconsistent,
		},
		{"an unregistered class", dispatched, "accepted", domain.ErrFollowUpFilingLedgerInconsistent},
		{
			"a resolved intent", filingIntentAfter(t, filingPreDispatch(), filingAttempt, filingAmbiguous),
			domain.FollowUpFilingResponseUnproven, domain.ErrFollowUpFilingIntentResolved,
		},
	}
	for _, tc := range cases {
		if _, err := tc.intent.RecordResponse(tc.class, filingLedgerAt); !errors.Is(err, tc.want) {
			t.Errorf("%s: error = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestFollowUpFilingLedgeringRule(t *testing.T) {
	t.Parallel()
	set := filingPreDispatch(7)
	unproven := filingIntentAfter(t, set, filingAttempt, filingResponse(domain.FollowUpFilingResponseUnproven))
	cases := []struct {
		name   string
		intent domain.FollowUpFilingIntent
		step   filingStep
		want   error
	}{
		{"success on the dispatched attempt", filingIntentAfter(t, set, filingAttempt), filingLedgerSuccess(40), nil},
		{"success with no attempt", filingIntentAfter(t, set), filingLedgerSuccess(40), domain.ErrFollowUpFilingResponseConflict},
		{"success over a recorded response", unproven, filingLedgerSuccess(40), domain.ErrFollowUpFilingResponseConflict},
		{
			"success naming a pre-dispatch issue", filingIntentAfter(t, set, filingAttempt),
			filingLedgerSuccess(7), domain.ErrFollowUpFilingLedgerInconsistent,
		},
		{"adopt after an unproven attempt", unproven, filingAdopt(40, false), nil},
		{"adopt on a marker with no response", filingIntentAfter(t, set, filingAttempt), filingAdopt(40, false), nil},
		{"adopt with no dispatched attempt", filingIntentAfter(t, set), filingAdopt(40, false), domain.ErrFollowUpFilingAdoptionRefused},
		{"adopt a pre-dispatch issue", unproven, filingAdopt(7, false), domain.ErrFollowUpFilingAdoptionRefused},
		{"adopt in an ambiguous repository", unproven, filingAdopt(40, true), domain.ErrFollowUpFilingAdoptionRefused},
		{"adopt issue number 0", unproven, filingAdopt(0, false), domain.ErrNonPositive},
		{
			"adopt on a resolved intent", filingIntentAfter(t, set, filingAttempt, filingAmbiguous),
			filingAdopt(40, false), domain.ErrFollowUpFilingIntentResolved,
		},
	}
	for _, tc := range cases {
		next, err := tc.step(tc.intent)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: error = %v, want %v", tc.name, err, tc.want)
			continue
		}
		if err == nil && (next.Terminal == nil || next.Terminal.Outcome != domain.FollowUpFilingLedgered) {
			t.Errorf("%s: terminal = %+v, want ledgered", tc.name, next.Terminal)
		}
	}
}

// TestFollowUpFilingOutcomeRule pins which terminal outcomes each history
// supports. A refusal reason must match the last attempt, no refusal is
// available while that attempt is unproven, and ambiguous needs a dispatch.
func TestFollowUpFilingOutcomeRule(t *testing.T) {
	t.Parallel()
	set := filingPreDispatch()
	histories := map[string]domain.FollowUpFilingIntent{
		"open":      filingIntentAfter(t),
		"set":       filingIntentAfter(t, set),
		"marker":    filingIntentAfter(t, set, filingAttempt),
		"unproven":  filingIntentAfter(t, set, filingAttempt, filingResponse(domain.FollowUpFilingResponseUnproven)),
		"transient": filingIntentAfter(t, set, filingAttempt, filingResponse(domain.FollowUpFilingResponseTransientRejection)),
		"definite":  filingIntentAfter(t, set, filingAttempt, filingResponse(domain.FollowUpFilingResponseDefiniteRejection)),
	}
	supported := map[string][]string{
		"definite_rejection":  {"definite"},
		"retry_bound_spent":   {"transient"},
		"precondition_failed": {"open", "set", "transient", "definite"},
		"ambiguous":           {"marker", "unproven", "transient", "definite"},
	}
	steps := map[string]filingStep{
		"definite_rejection":  filingRefuse(domain.FollowUpFilingRefusalDefiniteRejection),
		"retry_bound_spent":   filingRefuse(domain.FollowUpFilingRefusalRetryBoundSpent),
		"precondition_failed": filingRefuse(domain.FollowUpFilingRefusalPreconditionFailed),
		"ambiguous":           filingAmbiguous,
	}
	for outcome, step := range steps {
		for history, intent := range histories {
			var want error
			if !slices.Contains(supported[outcome], history) {
				want = domain.ErrFollowUpFilingOutcomeRefused
			}
			next, err := step(intent)
			if !errors.Is(err, want) {
				t.Errorf("%s after %s: error = %v, want %v", outcome, history, err, want)
				continue
			}
			if err != nil {
				continue
			}
			// A terminal outcome is permanent: every later write is refused.
			for name, later := range map[string]filingStep{
				"ambiguous": filingAmbiguous, "attempt": filingAttempt, "adopt": filingAdopt(40, false),
				"refuse": filingRefuse(domain.FollowUpFilingRefusalPreconditionFailed),
			} {
				if _, err := later(next); !errors.Is(err, domain.ErrFollowUpFilingIntentResolved) {
					t.Errorf("%s after a terminal %s: error = %v, want %v",
						name, outcome, err, domain.ErrFollowUpFilingIntentResolved)
				}
			}
		}
	}
	unregistered := domain.FollowUpFilingRefusalReason("operator_cancelled")
	if _, err := histories["open"].Refuse(unregistered, filingLedgerAt); !errors.Is(err, domain.ErrFollowUpFilingLedgerInconsistent) {
		t.Errorf("unregistered refusal reason: error = %v, want %v", err, domain.ErrFollowUpFilingLedgerInconsistent)
	}
}

func TestFollowUpFilingMayHaveCreated(t *testing.T) {
	t.Parallel()
	set := filingPreDispatch()
	transient := filingResponse(domain.FollowUpFilingResponseTransientRejection)
	cases := []struct {
		name  string
		steps []filingStep
		want  bool
	}{
		{"open", nil, false},
		{"set and no attempt", []filingStep{set}, false},
		{"only rejected attempts", []filingStep{
			set, filingAttempt, transient, filingAttempt, filingResponse(domain.FollowUpFilingResponseDefiniteRejection),
		}, false},
		{"refused", []filingStep{
			set, filingAttempt, transient, filingRefuse(domain.FollowUpFilingRefusalRetryBoundSpent),
		}, false},
		{"marker with no response", []filingStep{set, filingAttempt}, true},
		{"unproven attempt", []filingStep{set, filingAttempt, filingResponse(domain.FollowUpFilingResponseUnproven)}, true},
		{"ambiguous after a rejection", []filingStep{set, filingAttempt, transient, filingAmbiguous}, true},
		{"ledgered by success", []filingStep{set, filingAttempt, filingLedgerSuccess(40)}, true},
		{"ledgered by adoption after a rejection", []filingStep{set, filingAttempt, transient, filingAdopt(40, false)}, true},
	}
	for _, tc := range cases {
		if got := filingIntentAfter(t, tc.steps...).MayHaveCreated(); got != tc.want {
			t.Errorf("%s: MayHaveCreated = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestFollowUpFilingIntentValidateRejectsUnreachableStates pins the
// reconstruction backstop: an intent no transition sequence produces fails
// validation, so rows changed outside the store are never read as history.
func TestFollowUpFilingIntentValidateRejectsUnreachableStates(t *testing.T) {
	t.Parallel()
	set := filingPreDispatch(7)
	transient := filingResponse(domain.FollowUpFilingResponseTransientRejection)
	issue := 40
	definite := domain.FollowUpFilingRefusalDefiniteRejection
	cases := []struct {
		name   string
		base   domain.FollowUpFilingIntent
		mutate func(*domain.FollowUpFilingIntent)
		want   error
	}{
		{
			"nil attempts", filingIntentAfter(t), func(i *domain.FollowUpFilingIntent) { i.Attempts = nil },
			domain.ErrFollowUpFilingLedgerInconsistent,
		},
		{
			"attempt without a pre-dispatch set", filingIntentAfter(t, set, filingAttempt),
			func(i *domain.FollowUpFilingIntent) { i.PreDispatch = nil }, domain.ErrFollowUpFilingLedgerInconsistent,
		},
		{
			"unsorted pre-dispatch set", filingIntentAfter(t, set),
			func(i *domain.FollowUpFilingIntent) { i.PreDispatch.IssueNumbers = []int{9, 7} },
			domain.ErrFollowUpFilingLedgerInconsistent,
		},
		{
			"non-positive dispatching identity", filingIntentAfter(t, set),
			func(i *domain.FollowUpFilingIntent) { *i.PreDispatch.BotUserID = 0 }, domain.ErrNonPositive,
		},
		{
			"ordinal gap", filingIntentAfter(t, set, filingAttempt),
			func(i *domain.FollowUpFilingIntent) { i.Attempts[0].Ordinal = 2 }, domain.ErrFollowUpFilingLedgerInconsistent,
		},
		{
			"attempt after an unproven attempt", filingIntentAfter(t, set, filingAttempt, transient, filingAttempt),
			func(i *domain.FollowUpFilingIntent) { i.Attempts[0].Response = nil },
			domain.ErrFollowUpFilingLedgerInconsistent,
		},
		{
			"success without a ledgered outcome", filingIntentAfter(t, set, filingAttempt, filingLedgerSuccess(40)),
			func(i *domain.FollowUpFilingIntent) { i.Terminal = nil }, domain.ErrFollowUpFilingLedgerInconsistent,
		},
		{
			"success naming a pre-dispatch issue", filingIntentAfter(t, set, filingAttempt, filingLedgerSuccess(40)),
			func(i *domain.FollowUpFilingIntent) { *i.Attempts[0].Response.IssueNumber = 7 },
			domain.ErrFollowUpFilingLedgerInconsistent,
		},
		{
			"rejection carrying an issue number", filingIntentAfter(t, set, filingAttempt, transient),
			func(i *domain.FollowUpFilingIntent) { i.Attempts[0].Response.IssueNumber = &issue },
			domain.ErrFollowUpFilingLedgerInconsistent,
		},
		{
			"ledgered with no attempt", filingIntentAfter(t, set, filingAttempt, filingAdopt(40, false)),
			func(i *domain.FollowUpFilingIntent) { i.Attempts = []domain.FollowUpFilingAttempt{} },
			domain.ErrFollowUpFilingLedgerInconsistent,
		},
		{
			"refusal reason its attempt does not support",
			filingIntentAfter(t, set, filingAttempt, transient, filingRefuse(domain.FollowUpFilingRefusalRetryBoundSpent)),
			func(i *domain.FollowUpFilingIntent) { i.Terminal.Reason = &definite },
			domain.ErrFollowUpFilingLedgerInconsistent,
		},
		{
			"reason on an ambiguous outcome", filingIntentAfter(t, set, filingAttempt, filingAmbiguous),
			func(i *domain.FollowUpFilingIntent) { i.Terminal.Reason = &definite },
			domain.ErrFollowUpFilingLedgerInconsistent,
		},
		{
			"local time", filingIntentAfter(t),
			func(i *domain.FollowUpFilingIntent) { i.OpenedAt = i.OpenedAt.In(time.FixedZone("x", 3600)) },
			domain.ErrTimestampNotUTC,
		},
		{
			"no repository", filingIntentAfter(t), func(i *domain.FollowUpFilingIntent) { i.RepositoryID = 0 },
			domain.ErrNonPositive,
		},
	}
	for _, tc := range cases {
		intent := tc.base
		tc.mutate(&intent)
		if err := intent.Validate(); !errors.Is(err, tc.want) {
			t.Errorf("%s: Validate = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestFiledFollowUpIssueBackedBy(t *testing.T) {
	t.Parallel()
	set := filingPreDispatch(7)
	bySuccess := filingIntentAfter(t, set, filingAttempt, filingLedgerSuccess(40))
	byAdoption := filingIntentAfter(t, set, filingAttempt, filingAdopt(41, false))
	row := func(issue int) domain.FiledFollowUpIssue {
		return domain.FiledFollowUpIssue{
			InstanceID: "instance-1", RepositoryID: 123, IssueNumber: issue,
			Origin:     domain.FollowUpFilingOrigin{ProjectID: "project-1", RunID: "run-1"},
			LedgeredAt: filingLedgerAt,
		}
	}
	otherInstance, otherRepository := row(40), row(40)
	otherInstance.InstanceID, otherRepository.RepositoryID = "instance-2", 999
	cases := []struct {
		name   string
		row    domain.FiledFollowUpIssue
		intent domain.FollowUpFilingIntent
		want   error
	}{
		{"the issue the success named", row(40), bySuccess, nil},
		{"an adopted issue", row(41), byAdoption, nil},
		{"not the issue the success named", row(41), bySuccess, domain.ErrFollowUpFilingLedgerInconsistent},
		{"an adopted pre-dispatch issue", row(7), byAdoption, domain.ErrFollowUpFilingLedgerInconsistent},
		{"another instance", otherInstance, bySuccess, domain.ErrFollowUpFilingLedgerInconsistent},
		{"another repository", otherRepository, bySuccess, domain.ErrFollowUpFilingLedgerInconsistent},
		{"an outstanding intent", row(40), filingIntentAfter(t, set, filingAttempt), domain.ErrFollowUpFilingLedgerInconsistent},
		{
			"an ambiguous intent", row(40), filingIntentAfter(t, set, filingAttempt, filingAmbiguous),
			domain.ErrFollowUpFilingLedgerInconsistent,
		},
	}
	for _, tc := range cases {
		if err := tc.row.Validate(); err != nil {
			t.Fatalf("%s: row invalid: %v", tc.name, err)
		}
		if err := tc.row.BackedBy(tc.intent); !errors.Is(err, tc.want) {
			t.Errorf("%s: BackedBy = %v, want %v", tc.name, err, tc.want)
		}
	}
	for name, mutate := range map[string]func(*domain.FiledFollowUpIssue){
		"no instance":   func(f *domain.FiledFollowUpIssue) { f.InstanceID = "" },
		"no issue":      func(f *domain.FiledFollowUpIssue) { f.IssueNumber = 0 },
		"no repository": func(f *domain.FiledFollowUpIssue) { f.RepositoryID = 0 },
		"no origin run": func(f *domain.FiledFollowUpIssue) { f.Origin.RunID = "" },
		"no time":       func(f *domain.FiledFollowUpIssue) { f.LedgeredAt = time.Time{} },
	} {
		filed := row(40)
		mutate(&filed)
		if err := filed.Validate(); err == nil {
			t.Errorf("%s: Validate accepted the row", name)
		}
	}
}

// filingLedgerGoldens builds one fixture per ledger state the store persists
// or returns, each reached through the transitions so it is a valid history.
func filingLedgerGoldens(t *testing.T) []struct {
	name  string
	value any
} {
	t.Helper()
	set := filingPreDispatch(7, 31)
	transient := filingResponse(domain.FollowUpFilingResponseTransientRejection)
	retried := []filingStep{set, filingAttempt, transient, filingAttempt}
	with := func(steps ...filingStep) domain.FollowUpFilingIntent {
		return filingIntentAfter(t, append(slices.Clone(retried), steps...)...)
	}
	return []struct {
		name  string
		value any
	}{
		{"follow_up_filing_intent_open", filingIntentAfter(t)},
		{"follow_up_filing_intent_attempts", with()},
		{"follow_up_filing_intent_ledgered", with(filingLedgerSuccess(40))},
		{"follow_up_filing_intent_refused", with(transient, filingRefuse(domain.FollowUpFilingRefusalRetryBoundSpent))},
		{"follow_up_filing_intent_ambiguous", with(filingResponse(domain.FollowUpFilingResponseUnproven), filingAmbiguous)},
		{"filed_follow_up_issue", domain.FiledFollowUpIssue{
			InstanceID: "instance-1", RepositoryID: 123, IssueNumber: 40,
			Origin:     domain.FollowUpFilingOrigin{ProjectID: "proj-1", RunID: "run-1"},
			LedgeredAt: filingLedgerAt.Add(4 * time.Minute),
		}},
	}
}
