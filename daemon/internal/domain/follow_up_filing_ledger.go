package domain

import (
	"fmt"
	"slices"
	"time"
)

// The follow-up filing ledger (plan §5.17): the creation intent that fences a
// filing's create and its recovery, one record per create attempt, and the
// immutable row that binds a filed issue to the proposal instance it came
// from. An issue is identified the way the daemon identifies issues
// everywhere else: the canonical repository id plus the issue number.
//
// The intent's methods are the §5.17 recovery rule as pure transitions. Each
// returns the next intent or refuses with a sentinel; the store persists the
// change a transition makes and never decides the rule itself. Every write is
// a transition from specific states, so a repeat of a write already made is
// refused like any other: recovery reads the intent and continues from its
// state instead of replaying a step.

// FollowUpFilingPreDispatchSet is the set of candidate issues an intent saw
// before its first dispatch (plan §5.17). An issue in it existed before the
// intent created anything, so the intent never adopts it. An empty set is a
// recorded observation of no candidates and is distinct from a set not yet
// recorded, which is a nil pointer on the intent.
//
// The set belongs to one GitHub App: it lists the issues that App's bot
// account had authored, and every create of the intent is sent as that
// account. Recovery tells the intent's own issue from a foreign one by
// authorship, so a candidate search under any other identity proves nothing
// about this intent (issue #1777).
type FollowUpFilingPreDispatchSet struct {
	// IssueNumbers is strictly ascending and never nil, so one set has one
	// encoding.
	IssueNumbers []int `json:"issue_numbers"`
	// BotUserID is the numeric user ID of the App bot account the set was
	// listed under. It is nil only on a set recorded before the identity was
	// (migration 0093). Such an intent can prove no authorship: it starts no
	// attempt, and its caller adopts nothing for it.
	BotUserID  *int64    `json:"bot_user_id"`
	RecordedAt time.Time `json:"recorded_at"`
}

func (s FollowUpFilingPreDispatchSet) validate() error {
	if s.BotUserID != nil && *s.BotUserID <= 0 {
		return fmt.Errorf("follow-up filing pre-dispatch bot_user_id %d: %w", *s.BotUserID, ErrNonPositive)
	}
	if s.IssueNumbers == nil {
		return fmt.Errorf("follow-up filing pre-dispatch issue_numbers absent: %w", ErrFollowUpFilingLedgerInconsistent)
	}
	for i, number := range s.IssueNumbers {
		if number <= 0 {
			return fmt.Errorf("follow-up filing pre-dispatch issue number %d: %w", number, ErrNonPositive)
		}
		if i > 0 && s.IssueNumbers[i-1] >= number {
			return fmt.Errorf("follow-up filing pre-dispatch issue numbers are not strictly ascending: %w",
				ErrFollowUpFilingLedgerInconsistent)
		}
	}
	return validateFilingLedgerTime("pre-dispatch recorded_at", s.RecordedAt)
}

// FollowUpFilingAttemptResponse is the response one create attempt recorded.
// IssueNumber is the issue a success response named, and nil for every other
// class.
type FollowUpFilingAttemptResponse struct {
	Class       FollowUpFilingResponseClass `json:"class"`
	IssueNumber *int                        `json:"issue_number"`
	RecordedAt  time.Time                   `json:"recorded_at"`
}

func (r FollowUpFilingAttemptResponse) validate() error {
	if !r.Class.valid() {
		return fmt.Errorf("follow-up filing response class %q: %w", r.Class, ErrFollowUpFilingLedgerInconsistent)
	}
	if (r.Class == FollowUpFilingResponseSuccess) != (r.IssueNumber != nil) {
		return fmt.Errorf("follow-up filing %s response issue number: %w", r.Class, ErrFollowUpFilingLedgerInconsistent)
	}
	if r.IssueNumber != nil && *r.IssueNumber <= 0 {
		return fmt.Errorf("follow-up filing response issue number %d: %w", *r.IssueNumber, ErrNonPositive)
	}
	return validateFilingLedgerTime("response recorded_at", r.RecordedAt)
}

// FollowUpFilingAttempt is one create attempt. The record's existence is the
// dispatch-started marker §5.17 requires before the create request is sent.
// Response is nil until one is recorded, and an attempt with no recorded
// response reads as unproven.
type FollowUpFilingAttempt struct {
	// Ordinal counts the intent's attempts from 1.
	Ordinal           int                            `json:"ordinal"`
	DispatchStartedAt time.Time                      `json:"dispatch_started_at"`
	Response          *FollowUpFilingAttemptResponse `json:"response"`
}

func (a FollowUpFilingAttempt) validate() error {
	if a.Ordinal < 1 {
		return fmt.Errorf("follow-up filing attempt ordinal %d: %w", a.Ordinal, ErrNonPositive)
	}
	if err := validateFilingLedgerTime("attempt dispatch_started_at", a.DispatchStartedAt); err != nil {
		return err
	}
	if a.Response == nil {
		return nil
	}
	return a.Response.validate()
}

// class is the response class the attempt reads as.
func (a FollowUpFilingAttempt) class() FollowUpFilingResponseClass {
	if a.Response == nil {
		return FollowUpFilingResponseUnproven
	}
	return a.Response.Class
}

// FollowUpFilingTerminal is an intent's one terminal outcome. Reason is set
// exactly when the outcome is refused.
type FollowUpFilingTerminal struct {
	Outcome    FollowUpFilingOutcome        `json:"outcome"`
	Reason     *FollowUpFilingRefusalReason `json:"reason"`
	ResolvedAt time.Time                    `json:"resolved_at"`
}

func (t FollowUpFilingTerminal) validate() error {
	if !t.Outcome.valid() {
		return fmt.Errorf("follow-up filing outcome %q: %w", t.Outcome, ErrFollowUpFilingLedgerInconsistent)
	}
	if (t.Outcome == FollowUpFilingRefused) != (t.Reason != nil) {
		return fmt.Errorf("follow-up filing %s outcome reason: %w", t.Outcome, ErrFollowUpFilingLedgerInconsistent)
	}
	if t.Reason != nil && !t.Reason.valid() {
		return fmt.Errorf("follow-up filing refusal reason %q: %w", *t.Reason, ErrFollowUpFilingLedgerInconsistent)
	}
	return validateFilingLedgerTime("outcome resolved_at", t.ResolvedAt)
}

// FollowUpFilingIntent is the durable creation intent for one proposal
// instance, with its attempts (plan §5.17). There is at most one per
// instance, ever, and at most one outstanding per repository. It is
// outstanding until Terminal is set, and a terminal outcome is permanent.
type FollowUpFilingIntent struct {
	InstanceID ProposalInstanceID `json:"instance_id"`
	// RepositoryID is the repository the filing targets. The store derives it
	// from the proposal's current target; no caller supplies it.
	RepositoryID int64     `json:"repository_id"`
	OpenedAt     time.Time `json:"opened_at"`
	// PreDispatch is nil until the set is recorded.
	PreDispatch *FollowUpFilingPreDispatchSet `json:"pre_dispatch"`
	// Attempts is in ordinal order and never nil.
	Attempts []FollowUpFilingAttempt `json:"attempts"`
	Terminal *FollowUpFilingTerminal `json:"terminal"`
}

// NewFollowUpFilingIntent opens an intent with no pre-dispatch set, attempt,
// or outcome.
func NewFollowUpFilingIntent(
	instanceID ProposalInstanceID, repositoryID int64, openedAt time.Time,
) (FollowUpFilingIntent, error) {
	intent := FollowUpFilingIntent{
		InstanceID: instanceID, RepositoryID: repositoryID, OpenedAt: openedAt.UTC(),
		Attempts: []FollowUpFilingAttempt{},
	}
	if err := intent.Validate(); err != nil {
		return FollowUpFilingIntent{}, err
	}
	return intent, nil
}

// Validate reports whether the intent is a state the transitions below can
// reach. It is the reconstruction backstop: a rebuilt intent whose rows were
// changed outside the store (an attempt added without evidence, an outcome
// its attempts do not support) fails here instead of being read as history.
func (i FollowUpFilingIntent) Validate() error {
	if i.InstanceID == "" {
		return fmt.Errorf("follow-up filing intent instance_id: %w", ErrEmptyID)
	}
	if i.RepositoryID <= 0 {
		return fmt.Errorf("follow-up filing intent repository_id %d: %w", i.RepositoryID, ErrNonPositive)
	}
	if err := validateFilingLedgerTime("intent opened_at", i.OpenedAt); err != nil {
		return err
	}
	if i.PreDispatch != nil {
		if err := i.PreDispatch.validate(); err != nil {
			return err
		}
	}
	if i.Attempts == nil {
		return fmt.Errorf("follow-up filing intent attempts absent: %w", ErrFollowUpFilingLedgerInconsistent)
	}
	if len(i.Attempts) > 0 && i.PreDispatch == nil {
		return fmt.Errorf("follow-up filing intent dispatched before its pre-dispatch set: %w",
			ErrFollowUpFilingLedgerInconsistent)
	}
	for index, attempt := range i.Attempts {
		if err := attempt.validate(); err != nil {
			return err
		}
		if attempt.Ordinal != index+1 {
			return fmt.Errorf("follow-up filing attempt ordinal %d at position %d: %w",
				attempt.Ordinal, index+1, ErrFollowUpFilingLedgerInconsistent)
		}
		// Only a transient rejection is evidence for a further create, so
		// every attempt but the last must have recorded one.
		if index < len(i.Attempts)-1 && attempt.class() != FollowUpFilingResponseTransientRejection {
			return fmt.Errorf("follow-up filing attempt %d follows a %s attempt: %w",
				attempt.Ordinal+1, attempt.class(), ErrFollowUpFilingLedgerInconsistent)
		}
		if attempt.class() != FollowUpFilingResponseSuccess {
			continue
		}
		// A success response is recorded only in the step that ledgers its
		// issue, and that issue was created after the pre-dispatch set.
		if i.Terminal == nil || i.Terminal.Outcome != FollowUpFilingLedgered {
			return fmt.Errorf("follow-up filing success response without a ledgered outcome: %w",
				ErrFollowUpFilingLedgerInconsistent)
		}
		if i.sawBeforeDispatch(*attempt.Response.IssueNumber) {
			return fmt.Errorf("follow-up filing success response names pre-dispatch issue %d: %w",
				*attempt.Response.IssueNumber, ErrFollowUpFilingLedgerInconsistent)
		}
	}
	if i.Terminal == nil {
		return nil
	}
	if err := i.Terminal.validate(); err != nil {
		return err
	}
	if !i.outcomeSupported(*i.Terminal) {
		return fmt.Errorf("follow-up filing %s outcome is not supported by the intent's attempts: %w",
			i.Terminal.Outcome, ErrFollowUpFilingLedgerInconsistent)
	}
	return nil
}

// Outstanding reports whether the intent still holds its repository.
func (i FollowUpFilingIntent) Outstanding() bool { return i.Terminal == nil }

// MayHaveCreated reports whether a create under this intent may have
// committed: its issue is ledgered, its outcome is ambiguous, or an attempt
// was dispatched and not recorded as rejected. An ambiguous create can commit
// after its outcome is recorded and leave an issue the ledger does not name,
// which is why intake asks this and not only whether a ledger row exists.
func (i FollowUpFilingIntent) MayHaveCreated() bool {
	if i.Terminal != nil && i.Terminal.Outcome != FollowUpFilingRefused {
		return true
	}
	return slices.ContainsFunc(i.Attempts, func(attempt FollowUpFilingAttempt) bool {
		return !rejectedBeforeCreation(attempt.class())
	})
}

// RecordPreDispatch records the candidate issues seen before the first
// dispatch, and botUserID, the App bot account they were listed under. The
// set is written once, before any attempt, so the identity that listed it is
// the one every create of the intent is held to.
func (i FollowUpFilingIntent) RecordPreDispatch(
	issueNumbers []int, botUserID int64, at time.Time,
) (FollowUpFilingIntent, error) {
	if i.Terminal != nil {
		return FollowUpFilingIntent{}, ErrFollowUpFilingIntentResolved
	}
	if i.PreDispatch != nil || len(i.Attempts) > 0 {
		return FollowUpFilingIntent{}, ErrFollowUpFilingPreDispatchFixed
	}
	// A fresh non-nil slice: the canonical encoding of no candidates is [],
	// and the copy detaches the intent from the caller's backing array.
	numbers := append([]int{}, issueNumbers...)
	slices.Sort(numbers)
	next := i
	next.PreDispatch = &FollowUpFilingPreDispatchSet{
		IssueNumbers: slices.Compact(numbers), BotUserID: &botUserID, RecordedAt: at.UTC(),
	}
	return next.validated()
}

// StartAttempt appends the dispatch-started marker for a create. §5.17 allows
// a create only with evidence that no earlier one for this instance
// committed: no attempt was dispatched, or the last attempt recorded a
// transient rejection. A definite rejection is evidence too, but it ends the
// intent as refused instead of allowing another create.
//
// The set must also name the identity it was listed under: a create sent
// with none recorded could never be told from another App's issue. Whether
// that identity is still the repository's is live state, and the caller's to
// compare before it calls.
func (i FollowUpFilingIntent) StartAttempt(at time.Time) (FollowUpFilingIntent, error) {
	if i.Terminal != nil {
		return FollowUpFilingIntent{}, ErrFollowUpFilingIntentResolved
	}
	if i.PreDispatch == nil {
		return FollowUpFilingIntent{}, ErrFollowUpFilingPreDispatchMissing
	}
	if i.PreDispatch.BotUserID == nil {
		return FollowUpFilingIntent{}, ErrFollowUpFilingIdentityMissing
	}
	if last := i.lastAttempt(); last != nil && last.class() != FollowUpFilingResponseTransientRejection {
		return FollowUpFilingIntent{}, fmt.Errorf("last attempt is %s: %w", last.class(), ErrFollowUpFilingCreateUnproven)
	}
	next := i
	next.Attempts = append(slices.Clone(i.Attempts), FollowUpFilingAttempt{
		Ordinal: len(i.Attempts) + 1, DispatchStartedAt: at.UTC(),
	})
	return next.validated()
}

// RecordResponse records a rejection or an unproven response on the
// dispatched attempt. A success response is recorded by LedgerSuccess, in the
// step that ledgers its issue.
func (i FollowUpFilingIntent) RecordResponse(
	class FollowUpFilingResponseClass, at time.Time,
) (FollowUpFilingIntent, error) {
	if !class.valid() || class == FollowUpFilingResponseSuccess {
		return FollowUpFilingIntent{}, fmt.Errorf("follow-up filing response class %q: %w",
			class, ErrFollowUpFilingLedgerInconsistent)
	}
	responded, err := i.respond(FollowUpFilingAttemptResponse{Class: class, RecordedAt: at.UTC()})
	if err != nil {
		return FollowUpFilingIntent{}, err
	}
	return responded.validated()
}

// LedgerSuccess records a success response naming issueNumber on the
// dispatched attempt and ends the intent as ledgered.
func (i FollowUpFilingIntent) LedgerSuccess(issueNumber int, at time.Time) (FollowUpFilingIntent, error) {
	responded, err := i.respond(FollowUpFilingAttemptResponse{
		Class: FollowUpFilingResponseSuccess, IssueNumber: &issueNumber, RecordedAt: at.UTC(),
	})
	if err != nil {
		return FollowUpFilingIntent{}, err
	}
	// The issue a create made cannot be one seen before the first dispatch.
	if i.sawBeforeDispatch(issueNumber) {
		return FollowUpFilingIntent{}, fmt.Errorf("success response names pre-dispatch issue %d: %w",
			issueNumber, ErrFollowUpFilingLedgerInconsistent)
	}
	return responded.resolve(FollowUpFilingTerminal{Outcome: FollowUpFilingLedgered, ResolvedAt: at.UTC()})
}

// Adopt ends the intent as ledgered by adopting a discovered candidate.
// It checks the §5.17 conditions an intent can see: the intent has a
// dispatched attempt, because before its first dispatch it has created
// nothing and any candidate is foreign; the issue is outside the pre-dispatch
// set; and the repository holds no ambiguous outcome, because a stray issue
// from that create would pass as this intent's candidate. Candidate
// validation (repository, App authorship, the intent window) is the caller's.
func (i FollowUpFilingIntent) Adopt(
	issueNumber int, repositoryAmbiguous bool, at time.Time,
) (FollowUpFilingIntent, error) {
	if i.Terminal != nil {
		return FollowUpFilingIntent{}, ErrFollowUpFilingIntentResolved
	}
	if issueNumber <= 0 {
		return FollowUpFilingIntent{}, fmt.Errorf("adopted issue number %d: %w", issueNumber, ErrNonPositive)
	}
	if len(i.Attempts) == 0 {
		return FollowUpFilingIntent{}, fmt.Errorf("no attempt was dispatched: %w", ErrFollowUpFilingAdoptionRefused)
	}
	if i.sawBeforeDispatch(issueNumber) {
		return FollowUpFilingIntent{}, fmt.Errorf("issue %d is in the pre-dispatch set: %w",
			issueNumber, ErrFollowUpFilingAdoptionRefused)
	}
	if repositoryAmbiguous {
		return FollowUpFilingIntent{}, fmt.Errorf("repository %d holds an ambiguous filing outcome: %w",
			i.RepositoryID, ErrFollowUpFilingAdoptionRefused)
	}
	return i.resolve(FollowUpFilingTerminal{Outcome: FollowUpFilingLedgered, ResolvedAt: at.UTC()})
}

// Refuse ends the intent as refused. The reason must match the last attempt,
// and no reason is available while that attempt is unproven.
func (i FollowUpFilingIntent) Refuse(reason FollowUpFilingRefusalReason, at time.Time) (FollowUpFilingIntent, error) {
	return i.resolve(FollowUpFilingTerminal{Outcome: FollowUpFilingRefused, Reason: &reason, ResolvedAt: at.UTC()})
}

// RecordAmbiguous ends the intent as ambiguous: what its create did cannot be
// settled. It needs a dispatched attempt, because an intent that dispatched
// nothing has nothing to be ambiguous about.
func (i FollowUpFilingIntent) RecordAmbiguous(at time.Time) (FollowUpFilingIntent, error) {
	return i.resolve(FollowUpFilingTerminal{Outcome: FollowUpFilingAmbiguous, ResolvedAt: at.UTC()})
}

// respond records response on the dispatched attempt, which must be the last
// one and must not have a response yet. The result is not validated as a
// whole: a success response is valid only with the ledgered outcome its
// caller sets next.
func (i FollowUpFilingIntent) respond(response FollowUpFilingAttemptResponse) (FollowUpFilingIntent, error) {
	if i.Terminal != nil {
		return FollowUpFilingIntent{}, ErrFollowUpFilingIntentResolved
	}
	last := i.lastAttempt()
	if last == nil || last.Response != nil {
		return FollowUpFilingIntent{}, ErrFollowUpFilingResponseConflict
	}
	next := i
	next.Attempts = slices.Clone(i.Attempts)
	next.Attempts[len(next.Attempts)-1].Response = &response
	if err := response.validate(); err != nil {
		return FollowUpFilingIntent{}, err
	}
	return next, nil
}

// resolve sets the intent's one terminal outcome.
func (i FollowUpFilingIntent) resolve(terminal FollowUpFilingTerminal) (FollowUpFilingIntent, error) {
	if i.Terminal != nil {
		return FollowUpFilingIntent{}, ErrFollowUpFilingIntentResolved
	}
	if err := terminal.validate(); err != nil {
		return FollowUpFilingIntent{}, err
	}
	if !i.outcomeSupported(terminal) {
		return FollowUpFilingIntent{}, fmt.Errorf("%s outcome: %w", terminal.Outcome, ErrFollowUpFilingOutcomeRefused)
	}
	next := i
	next.Terminal = &terminal
	return next.validated()
}

// outcomeSupported reports whether the intent's attempts support a terminal
// outcome. It assumes terminal passed validate. The switches dispatch
// behaviour and so omit default; the trailing returns guard an unregistered
// member.
func (i FollowUpFilingIntent) outcomeSupported(terminal FollowUpFilingTerminal) bool {
	last := i.lastAttempt()
	switch terminal.Outcome {
	case FollowUpFilingLedgered, FollowUpFilingAmbiguous:
		return last != nil
	case FollowUpFilingRefused:
		switch *terminal.Reason {
		case FollowUpFilingRefusalDefiniteRejection:
			return last != nil && last.class() == FollowUpFilingResponseDefiniteRejection
		case FollowUpFilingRefusalRetryBoundSpent:
			return last != nil && last.class() == FollowUpFilingResponseTransientRejection
		case FollowUpFilingRefusalPreconditionFailed:
			return last == nil || rejectedBeforeCreation(last.class())
		}
	}
	return false
}

func (i FollowUpFilingIntent) lastAttempt() *FollowUpFilingAttempt {
	if len(i.Attempts) == 0 {
		return nil
	}
	return &i.Attempts[len(i.Attempts)-1]
}

func (i FollowUpFilingIntent) sawBeforeDispatch(issueNumber int) bool {
	return i.PreDispatch != nil && slices.Contains(i.PreDispatch.IssueNumbers, issueNumber)
}

func (i FollowUpFilingIntent) validated() (FollowUpFilingIntent, error) {
	if err := i.Validate(); err != nil {
		return FollowUpFilingIntent{}, err
	}
	return i, nil
}

// rejectedBeforeCreation reports whether a response class is a recorded
// rejection, the only responses that show a create committed nothing.
func rejectedBeforeCreation(class FollowUpFilingResponseClass) bool {
	return class == FollowUpFilingResponseTransientRejection || class == FollowUpFilingResponseDefiniteRejection
}

// FollowUpFilingOrigin is the Freeside origin of a filed issue: the project
// and run its proposal's subject resolves to.
type FollowUpFilingOrigin struct {
	ProjectID ProjectID `json:"project_id"`
	RunID     RunID     `json:"run_id"`
}

// FiledFollowUpIssue is the immutable ledger row for one filed issue (plan
// §5.17): it binds the issue, by repository id and issue number, to the
// proposal instance that filed it and to that instance's origin. It is the
// lineage intake proof reads. The row is a claim until the store rebuilds it:
// no field here is trusted on a read without being re-derived or checked
// against the intent.
type FiledFollowUpIssue struct {
	InstanceID   ProposalInstanceID   `json:"instance_id"`
	RepositoryID int64                `json:"repository_id"`
	IssueNumber  int                  `json:"issue_number"`
	Origin       FollowUpFilingOrigin `json:"origin"`
	LedgeredAt   time.Time            `json:"ledgered_at"`
}

// Validate reports whether the row is well-formed.
func (f FiledFollowUpIssue) Validate() error {
	if f.InstanceID == "" {
		return fmt.Errorf("filed follow-up issue instance_id: %w", ErrEmptyID)
	}
	if f.RepositoryID <= 0 {
		return fmt.Errorf("filed follow-up issue repository_id %d: %w", f.RepositoryID, ErrNonPositive)
	}
	if f.IssueNumber <= 0 {
		return fmt.Errorf("filed follow-up issue issue_number %d: %w", f.IssueNumber, ErrNonPositive)
	}
	if f.Origin.ProjectID == "" || f.Origin.RunID == "" {
		return fmt.Errorf("filed follow-up issue origin: %w", ErrEmptyID)
	}
	return validateFilingLedgerTime("filed issue ledgered_at", f.LedgeredAt)
}

// BackedBy reports whether intent is the ledgered intent this row came from.
// The row's issue must be the one a recorded success response named or, when
// the intent ledgered by adoption, an issue outside the pre-dispatch set.
// intent must have passed Validate.
func (f FiledFollowUpIssue) BackedBy(intent FollowUpFilingIntent) error {
	if intent.InstanceID != f.InstanceID || intent.RepositoryID != f.RepositoryID {
		return fmt.Errorf("filed follow-up issue %d names another intent: %w",
			f.IssueNumber, ErrFollowUpFilingLedgerInconsistent)
	}
	if intent.Terminal == nil || intent.Terminal.Outcome != FollowUpFilingLedgered {
		return fmt.Errorf("filed follow-up issue %d has no ledgered intent: %w",
			f.IssueNumber, ErrFollowUpFilingLedgerInconsistent)
	}
	last := intent.lastAttempt()
	if last == nil {
		return fmt.Errorf("filed follow-up issue %d has no attempt: %w",
			f.IssueNumber, ErrFollowUpFilingLedgerInconsistent)
	}
	if last.class() == FollowUpFilingResponseSuccess {
		if *last.Response.IssueNumber != f.IssueNumber {
			return fmt.Errorf("filed follow-up issue %d is not the issue the success response named: %w",
				f.IssueNumber, ErrFollowUpFilingLedgerInconsistent)
		}
		return nil
	}
	if intent.sawBeforeDispatch(f.IssueNumber) {
		return fmt.Errorf("filed follow-up issue %d is in the pre-dispatch set: %w",
			f.IssueNumber, ErrFollowUpFilingLedgerInconsistent)
	}
	return nil
}

func validateFilingLedgerTime(field string, t time.Time) error {
	if t.IsZero() {
		return fmt.Errorf("follow-up filing %s: %w", field, ErrMissingTimestamp)
	}
	if t.Location() != time.UTC {
		return fmt.Errorf("follow-up filing %s: %w", field, ErrTimestampNotUTC)
	}
	return nil
}
