package domain

import (
	"fmt"
	"slices"
	"time"
)

// The issue comment ledger (plan §5.13): the posting intent that fences one
// approved comment's create and its recovery, one record per create attempt,
// and the immutable row that binds a posted comment to the proposal instance
// it came from. It mirrors the follow-up filing ledger
// (follow_up_filing_ledger.go) rather than generalizing it, so the filing
// ledger's tables and transitions stay as they are; the response classes,
// refusal reasons, outcomes, and terminal record are the filing ledger's own
// types.
//
// Two things differ from a filing. The created object is a comment, named by
// its forge comment ID. And the scope that serializes creates is the target
// issue, not the repository: the pre-dispatch set lists the App's comments on
// that issue, so an ambiguous comment on one issue blocks adoption only
// there.

// IssueCommentPreDispatchSet is the set of the App's comments on the target
// issue an intent saw before its first dispatch. A comment in it existed
// before the intent created anything, so the intent never adopts it. An empty
// set is a recorded observation of none and is distinct from a set not yet
// recorded, which is a nil pointer on the intent. BotUserID is the App bot
// account the set was listed under, and every create of the intent is sent as
// that account; recovery tells the intent's comment from a foreign one by
// authorship.
type IssueCommentPreDispatchSet struct {
	// CommentIDs is strictly ascending and never nil, so one set has one
	// encoding.
	CommentIDs []int64   `json:"comment_ids"`
	BotUserID  int64     `json:"bot_user_id"`
	RecordedAt time.Time `json:"recorded_at"`
}

func (s IssueCommentPreDispatchSet) validate() error {
	if s.BotUserID <= 0 {
		return fmt.Errorf("issue comment pre-dispatch bot_user_id %d: %w", s.BotUserID, ErrNonPositive)
	}
	if s.CommentIDs == nil {
		return fmt.Errorf("issue comment pre-dispatch comment_ids absent: %w", ErrIssueCommentLedgerInconsistent)
	}
	for i, id := range s.CommentIDs {
		if id <= 0 {
			return fmt.Errorf("issue comment pre-dispatch comment id %d: %w", id, ErrNonPositive)
		}
		if i > 0 && s.CommentIDs[i-1] >= id {
			return fmt.Errorf("issue comment pre-dispatch comment ids are not strictly ascending: %w",
				ErrIssueCommentLedgerInconsistent)
		}
	}
	return validateFilingLedgerTime("comment pre-dispatch recorded_at", s.RecordedAt)
}

// IssueCommentAttemptResponse is the response one create attempt recorded.
// CommentID is the comment a success response named, and nil for every other
// class.
type IssueCommentAttemptResponse struct {
	Class      FollowUpFilingResponseClass `json:"class"`
	CommentID  *int64                      `json:"comment_id"`
	RecordedAt time.Time                   `json:"recorded_at"`
}

func (r IssueCommentAttemptResponse) validate() error {
	if !r.Class.valid() {
		return fmt.Errorf("issue comment response class %q: %w", r.Class, ErrIssueCommentLedgerInconsistent)
	}
	if (r.Class == FollowUpFilingResponseSuccess) != (r.CommentID != nil) {
		return fmt.Errorf("issue comment %s response comment id: %w", r.Class, ErrIssueCommentLedgerInconsistent)
	}
	if r.CommentID != nil && *r.CommentID <= 0 {
		return fmt.Errorf("issue comment response comment id %d: %w", *r.CommentID, ErrNonPositive)
	}
	return validateFilingLedgerTime("comment response recorded_at", r.RecordedAt)
}

// IssueCommentAttempt is one create attempt. The record's existence is the
// dispatch-started marker written before the create request is sent.
// Response is nil until one is recorded, and an attempt with no recorded
// response reads as unproven.
type IssueCommentAttempt struct {
	// Ordinal counts the intent's attempts from 1.
	Ordinal           int                          `json:"ordinal"`
	DispatchStartedAt time.Time                    `json:"dispatch_started_at"`
	Response          *IssueCommentAttemptResponse `json:"response"`
}

func (a IssueCommentAttempt) validate() error {
	if a.Ordinal < 1 {
		return fmt.Errorf("issue comment attempt ordinal %d: %w", a.Ordinal, ErrNonPositive)
	}
	if err := validateFilingLedgerTime("comment attempt dispatch_started_at", a.DispatchStartedAt); err != nil {
		return err
	}
	if a.Response == nil {
		return nil
	}
	return a.Response.validate()
}

func (a IssueCommentAttempt) class() FollowUpFilingResponseClass {
	if a.Response == nil {
		return FollowUpFilingResponseUnproven
	}
	return a.Response.Class
}

// IssueCommentIntent is the durable posting intent for one approved
// issue_comment instance, with its attempts. There is at most one per
// instance, ever, and at most one outstanding per (repository, issue). It is
// outstanding until Terminal is set, and a terminal outcome is permanent.
type IssueCommentIntent struct {
	InstanceID ProposalInstanceID `json:"instance_id"`
	// RepositoryID and IssueNumber are the comment's target. The store
	// derives them from the instance's re-gated proposal; no caller supplies
	// them.
	RepositoryID int64     `json:"repository_id"`
	IssueNumber  int       `json:"issue_number"`
	OpenedAt     time.Time `json:"opened_at"`
	// PreDispatch is nil until the set is recorded.
	PreDispatch *IssueCommentPreDispatchSet `json:"pre_dispatch"`
	// Attempts is in ordinal order and never nil.
	Attempts []IssueCommentAttempt   `json:"attempts"`
	Terminal *FollowUpFilingTerminal `json:"terminal"`
}

// NewIssueCommentIntent opens an intent with no pre-dispatch set, attempt, or
// outcome.
func NewIssueCommentIntent(
	instanceID ProposalInstanceID, repositoryID int64, issueNumber int, openedAt time.Time,
) (IssueCommentIntent, error) {
	intent := IssueCommentIntent{
		InstanceID: instanceID, RepositoryID: repositoryID, IssueNumber: issueNumber,
		OpenedAt: openedAt.UTC(), Attempts: []IssueCommentAttempt{},
	}
	return intent.validated()
}

// Validate reports whether the intent is a state the transitions below can
// reach. It is the reconstruction backstop: a rebuilt intent whose rows were
// changed outside the store fails here instead of being read as history.
func (i IssueCommentIntent) Validate() error {
	if i.InstanceID == "" {
		return fmt.Errorf("issue comment intent instance_id: %w", ErrEmptyID)
	}
	if i.RepositoryID <= 0 {
		return fmt.Errorf("issue comment intent repository_id %d: %w", i.RepositoryID, ErrNonPositive)
	}
	if i.IssueNumber <= 0 {
		return fmt.Errorf("issue comment intent issue_number %d: %w", i.IssueNumber, ErrNonPositive)
	}
	if err := validateFilingLedgerTime("comment intent opened_at", i.OpenedAt); err != nil {
		return err
	}
	if i.PreDispatch != nil {
		if err := i.PreDispatch.validate(); err != nil {
			return err
		}
	}
	if i.Attempts == nil {
		return fmt.Errorf("issue comment intent attempts absent: %w", ErrIssueCommentLedgerInconsistent)
	}
	if len(i.Attempts) > 0 && i.PreDispatch == nil {
		return fmt.Errorf("issue comment intent dispatched before its pre-dispatch set: %w",
			ErrIssueCommentLedgerInconsistent)
	}
	for index, attempt := range i.Attempts {
		if err := attempt.validate(); err != nil {
			return err
		}
		if attempt.Ordinal != index+1 {
			return fmt.Errorf("issue comment attempt ordinal %d at position %d: %w",
				attempt.Ordinal, index+1, ErrIssueCommentLedgerInconsistent)
		}
		// Only a transient rejection is evidence for a further create.
		if index < len(i.Attempts)-1 && attempt.class() != FollowUpFilingResponseTransientRejection {
			return fmt.Errorf("issue comment attempt %d follows a %s attempt: %w",
				attempt.Ordinal+1, attempt.class(), ErrIssueCommentLedgerInconsistent)
		}
		if attempt.class() != FollowUpFilingResponseSuccess {
			continue
		}
		if i.Terminal == nil || i.Terminal.Outcome != FollowUpFilingLedgered {
			return fmt.Errorf("issue comment success response without a ledgered outcome: %w",
				ErrIssueCommentLedgerInconsistent)
		}
		if i.sawBeforeDispatch(*attempt.Response.CommentID) {
			return fmt.Errorf("issue comment success response names pre-dispatch comment %d: %w",
				*attempt.Response.CommentID, ErrIssueCommentLedgerInconsistent)
		}
	}
	if i.Terminal == nil {
		return nil
	}
	if err := i.Terminal.validate(); err != nil {
		return err
	}
	if !i.outcomeSupported(*i.Terminal) {
		return fmt.Errorf("issue comment %s outcome is not supported by the intent's attempts: %w",
			i.Terminal.Outcome, ErrIssueCommentLedgerInconsistent)
	}
	return nil
}

// Outstanding reports whether the intent still holds its issue.
func (i IssueCommentIntent) Outstanding() bool { return i.Terminal == nil }

// MayHaveCreated reports whether a create under this intent may have
// committed: its comment is ledgered, its outcome is ambiguous, or an attempt
// was dispatched and not recorded as rejected. The per-run cap counts such
// comments with the run's filings.
func (i IssueCommentIntent) MayHaveCreated() bool {
	if i.Terminal != nil && i.Terminal.Outcome != FollowUpFilingRefused {
		return true
	}
	return slices.ContainsFunc(i.Attempts, func(attempt IssueCommentAttempt) bool {
		return !rejectedBeforeCreation(attempt.class())
	})
}

// RecordPreDispatch records the App's comments on the target issue seen
// before the first dispatch, and botUserID, the account they were listed
// under. The set is written once, before any attempt.
func (i IssueCommentIntent) RecordPreDispatch(
	commentIDs []int64, botUserID int64, at time.Time,
) (IssueCommentIntent, error) {
	if i.Terminal != nil {
		return IssueCommentIntent{}, ErrIssueCommentIntentResolved
	}
	if i.PreDispatch != nil || len(i.Attempts) > 0 {
		return IssueCommentIntent{}, ErrIssueCommentPreDispatchFixed
	}
	ids := append([]int64{}, commentIDs...)
	slices.Sort(ids)
	next := i
	next.PreDispatch = &IssueCommentPreDispatchSet{
		CommentIDs: slices.Compact(ids), BotUserID: botUserID, RecordedAt: at.UTC(),
	}
	return next.validated()
}

// StartAttempt appends the dispatch-started marker for a create. A create is
// allowed only with evidence that no earlier one committed: no attempt was
// dispatched, or the last attempt recorded a transient rejection.
func (i IssueCommentIntent) StartAttempt(at time.Time) (IssueCommentIntent, error) {
	if i.Terminal != nil {
		return IssueCommentIntent{}, ErrIssueCommentIntentResolved
	}
	if i.PreDispatch == nil {
		return IssueCommentIntent{}, ErrIssueCommentPreDispatchMissing
	}
	if last := i.lastAttempt(); last != nil && last.class() != FollowUpFilingResponseTransientRejection {
		return IssueCommentIntent{}, fmt.Errorf("last attempt is %s: %w", last.class(), ErrIssueCommentCreateUnproven)
	}
	next := i
	next.Attempts = append(slices.Clone(i.Attempts), IssueCommentAttempt{
		Ordinal: len(i.Attempts) + 1, DispatchStartedAt: at.UTC(),
	})
	return next.validated()
}

// RecordResponse records a rejection or an unproven response on the
// dispatched attempt. A success response is recorded by LedgerSuccess.
func (i IssueCommentIntent) RecordResponse(
	class FollowUpFilingResponseClass, at time.Time,
) (IssueCommentIntent, error) {
	if !class.valid() || class == FollowUpFilingResponseSuccess {
		return IssueCommentIntent{}, fmt.Errorf("issue comment response class %q: %w",
			class, ErrIssueCommentLedgerInconsistent)
	}
	responded, err := i.respond(IssueCommentAttemptResponse{Class: class, RecordedAt: at.UTC()})
	if err != nil {
		return IssueCommentIntent{}, err
	}
	return responded.validated()
}

// LedgerSuccess records a success response naming commentID on the
// dispatched attempt and ends the intent as ledgered.
func (i IssueCommentIntent) LedgerSuccess(commentID int64, at time.Time) (IssueCommentIntent, error) {
	responded, err := i.respond(IssueCommentAttemptResponse{
		Class: FollowUpFilingResponseSuccess, CommentID: &commentID, RecordedAt: at.UTC(),
	})
	if err != nil {
		return IssueCommentIntent{}, err
	}
	if i.sawBeforeDispatch(commentID) {
		return IssueCommentIntent{}, fmt.Errorf("success response names pre-dispatch comment %d: %w",
			commentID, ErrIssueCommentLedgerInconsistent)
	}
	return responded.resolve(FollowUpFilingTerminal{Outcome: FollowUpFilingLedgered, ResolvedAt: at.UTC()})
}

// Adopt ends the intent as ledgered by adopting a discovered comment. The
// intent must have dispatched an attempt, the comment must be outside the
// pre-dispatch set, and the issue must hold no ambiguous comment outcome,
// because a stray comment from that create would pass as this intent's.
// Candidate validation (issue, App authorship, the intent window) is the
// caller's.
func (i IssueCommentIntent) Adopt(
	commentID int64, issueAmbiguous bool, at time.Time,
) (IssueCommentIntent, error) {
	if i.Terminal != nil {
		return IssueCommentIntent{}, ErrIssueCommentIntentResolved
	}
	if commentID <= 0 {
		return IssueCommentIntent{}, fmt.Errorf("adopted comment id %d: %w", commentID, ErrNonPositive)
	}
	if len(i.Attempts) == 0 {
		return IssueCommentIntent{}, fmt.Errorf("no attempt was dispatched: %w", ErrIssueCommentAdoptionRefused)
	}
	if i.sawBeforeDispatch(commentID) {
		return IssueCommentIntent{}, fmt.Errorf("comment %d is in the pre-dispatch set: %w",
			commentID, ErrIssueCommentAdoptionRefused)
	}
	if issueAmbiguous {
		return IssueCommentIntent{}, fmt.Errorf("issue %d holds an ambiguous comment outcome: %w",
			i.IssueNumber, ErrIssueCommentAdoptionRefused)
	}
	return i.resolve(FollowUpFilingTerminal{Outcome: FollowUpFilingLedgered, ResolvedAt: at.UTC()})
}

// Refuse ends the intent as refused. The reason must match the last attempt,
// and no reason is available while that attempt is unproven.
func (i IssueCommentIntent) Refuse(reason FollowUpFilingRefusalReason, at time.Time) (IssueCommentIntent, error) {
	return i.resolve(FollowUpFilingTerminal{Outcome: FollowUpFilingRefused, Reason: &reason, ResolvedAt: at.UTC()})
}

// RecordAmbiguous ends the intent as ambiguous: what its create did cannot be
// settled. It needs a dispatched attempt.
func (i IssueCommentIntent) RecordAmbiguous(at time.Time) (IssueCommentIntent, error) {
	return i.resolve(FollowUpFilingTerminal{Outcome: FollowUpFilingAmbiguous, ResolvedAt: at.UTC()})
}

func (i IssueCommentIntent) respond(response IssueCommentAttemptResponse) (IssueCommentIntent, error) {
	if i.Terminal != nil {
		return IssueCommentIntent{}, ErrIssueCommentIntentResolved
	}
	last := i.lastAttempt()
	if last == nil || last.Response != nil {
		return IssueCommentIntent{}, ErrIssueCommentResponseConflict
	}
	if err := response.validate(); err != nil {
		return IssueCommentIntent{}, err
	}
	next := i
	next.Attempts = slices.Clone(i.Attempts)
	next.Attempts[len(next.Attempts)-1].Response = &response
	return next, nil
}

func (i IssueCommentIntent) resolve(terminal FollowUpFilingTerminal) (IssueCommentIntent, error) {
	if i.Terminal != nil {
		return IssueCommentIntent{}, ErrIssueCommentIntentResolved
	}
	if err := terminal.validate(); err != nil {
		return IssueCommentIntent{}, err
	}
	if !i.outcomeSupported(terminal) {
		return IssueCommentIntent{}, fmt.Errorf("%s outcome: %w", terminal.Outcome, ErrIssueCommentOutcomeRefused)
	}
	next := i
	next.Terminal = &terminal
	return next.validated()
}

// outcomeSupported reports whether the intent's attempts support a terminal
// outcome, by the filing ledger's rule. It assumes terminal passed validate.
// The switches dispatch behaviour and so omit default.
func (i IssueCommentIntent) outcomeSupported(terminal FollowUpFilingTerminal) bool {
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

func (i IssueCommentIntent) lastAttempt() *IssueCommentAttempt {
	if len(i.Attempts) == 0 {
		return nil
	}
	return &i.Attempts[len(i.Attempts)-1]
}

func (i IssueCommentIntent) sawBeforeDispatch(commentID int64) bool {
	return i.PreDispatch != nil && slices.Contains(i.PreDispatch.CommentIDs, commentID)
}

func (i IssueCommentIntent) validated() (IssueCommentIntent, error) {
	if err := i.Validate(); err != nil {
		return IssueCommentIntent{}, err
	}
	return i, nil
}

// PostedIssueComment is the immutable ledger row for one posted comment: it
// binds the comment, by repository id, issue number, and comment ID, to the
// proposal instance that posted it. The row is a claim until the store
// rebuilds it against its ledgered intent (BackedBy).
type PostedIssueComment struct {
	InstanceID   ProposalInstanceID `json:"instance_id"`
	RepositoryID int64              `json:"repository_id"`
	IssueNumber  int                `json:"issue_number"`
	CommentID    int64              `json:"comment_id"`
	LedgeredAt   time.Time          `json:"ledgered_at"`
}

// Validate reports whether the row is well-formed.
func (p PostedIssueComment) Validate() error {
	if p.InstanceID == "" {
		return fmt.Errorf("posted issue comment instance_id: %w", ErrEmptyID)
	}
	if p.RepositoryID <= 0 || p.IssueNumber <= 0 || p.CommentID <= 0 {
		return fmt.Errorf("posted issue comment repository %d issue %d comment %d: %w",
			p.RepositoryID, p.IssueNumber, p.CommentID, ErrNonPositive)
	}
	return validateFilingLedgerTime("posted comment ledgered_at", p.LedgeredAt)
}

// BackedBy reports whether intent is the ledgered intent this row came from.
// The row's comment must be the one a recorded success response named or,
// when the intent ledgered by adoption, a comment outside the pre-dispatch
// set. intent must have passed Validate.
func (p PostedIssueComment) BackedBy(intent IssueCommentIntent) error {
	if intent.InstanceID != p.InstanceID || intent.RepositoryID != p.RepositoryID ||
		intent.IssueNumber != p.IssueNumber {
		return fmt.Errorf("posted issue comment %d names another intent: %w",
			p.CommentID, ErrIssueCommentLedgerInconsistent)
	}
	if intent.Terminal == nil || intent.Terminal.Outcome != FollowUpFilingLedgered {
		return fmt.Errorf("posted issue comment %d has no ledgered intent: %w",
			p.CommentID, ErrIssueCommentLedgerInconsistent)
	}
	last := intent.lastAttempt()
	if last == nil {
		return fmt.Errorf("posted issue comment %d has no attempt: %w", p.CommentID, ErrIssueCommentLedgerInconsistent)
	}
	if last.class() == FollowUpFilingResponseSuccess {
		if *last.Response.CommentID != p.CommentID {
			return fmt.Errorf("posted issue comment %d is not the comment the success response named: %w",
				p.CommentID, ErrIssueCommentLedgerInconsistent)
		}
		return nil
	}
	if intent.sawBeforeDispatch(p.CommentID) {
		return fmt.Errorf("posted issue comment %d is in the pre-dispatch set: %w",
			p.CommentID, ErrIssueCommentLedgerInconsistent)
	}
	return nil
}
