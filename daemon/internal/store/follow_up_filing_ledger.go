package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Follow-up filing ledger (plan §5.17 follow-up filing recovery, issue
// #1626). Daemon-internal: never synchronized, written on InternalTx.
//
// Every write is a transition the pure rule in domain permits from the
// intent's stored state: the store reads the intent, applies the transition,
// and persists exactly its delta under a guard that refuses a row another
// writer moved first. A write is therefore never repeated blindly. Recovery
// reads the intent and continues from the state it finds; only opening an
// intent is idempotent.
//
// Trust posture: no stored bit is believed on its own. An intent is rebuilt
// from its rows, validated as a history the rule could have produced, and
// re-gated against current rows: its instance must still be an approved
// follow-up filing whose derived target repository is the one the intent
// names. A filed-issue row must decode to its key columns, be backed by a
// ledgered intent whose attempts account for the issue number, and carry the
// origin the proposal's subject resolves to now. A row that fails any of
// that is an error, never an absent answer: the yes/no reads below return
// the error instead of "no", so a caller cannot mistake an unreadable ledger
// for a clean one. Every row the re-gate reads is write-once (the proposal
// instance, its decision, the deciding command, the item binding) or
// immutable where it matters (a project's repository identity, a run's
// pinned policy), so a read fails only for a real inconsistency and not
// because ordinary work moved on.

const (
	selectFilingIntentsSQL = `
SELECT instance_id, repository_id, opened_at, pre_dispatch_issue_numbers,
       pre_dispatch_bot_user_id, pre_dispatch_recorded_at, outcome,
       refusal_reason, resolved_at
FROM follow_up_filing_intents`
	selectFilingAttemptsSQL = `
SELECT ordinal, dispatch_started_at, response_class, response_issue_number,
       response_recorded_at
FROM follow_up_filing_attempts WHERE instance_id = ? ORDER BY ordinal`
	selectFiledFollowUpIssuesSQL = `
SELECT repository_id, issue_number, instance_id, body
FROM follow_up_filed_issues`
	filingIntentSQL            = selectFilingIntentsSQL + ` WHERE instance_id = ?`
	repositoryFilingIntentsSQL = selectFilingIntentsSQL + ` WHERE repository_id = ? ORDER BY opened_at, instance_id`
	filedFollowUpIssueSQL      = selectFiledFollowUpIssuesSQL + ` WHERE repository_id = ? AND issue_number = ?`
	repositoryFiledIssuesSQL   = selectFiledFollowUpIssuesSQL + ` WHERE repository_id = ? ORDER BY issue_number`
	instanceFiledIssueSQL      = selectFiledFollowUpIssuesSQL + ` WHERE instance_id = ?`
)

// OpenFollowUpFilingIntent records the durable creation intent for one
// approved follow-up filing proposal instance and returns it. A repeat for
// an instance that already has an intent returns that intent unchanged,
// whatever state it has reached, so a recovering caller opens first and then
// reads where it stands.
//
// The instance must be a follow-up filing with a recorded approve decision
// (ErrFollowUpFilingNotApproved otherwise, which covers another effect kind,
// an undecided proposal, and a declined one). The repository comes from the
// proposal's re-derived target, never from the caller. A repository holds
// one outstanding intent at a time: while another instance's intent there
// has no terminal outcome, opening is refused with
// ErrFollowUpFilingRepositoryBusy.
func (tx *InternalTx) OpenFollowUpFilingIntent(
	ctx context.Context, instanceID domain.ProposalInstanceID, openedAt time.Time,
) (domain.FollowUpFilingIntent, error) {
	fail := func(err error) (domain.FollowUpFilingIntent, error) {
		return domain.FollowUpFilingIntent{}, fmt.Errorf("open follow-up filing intent %q: %w", instanceID, err)
	}
	existing, err := tx.GetFollowUpFilingIntent(ctx, instanceID)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return fail(err)
	}
	repositoryID, _, err := tx.approvedFollowUpFiling(ctx, instanceID)
	if err != nil {
		return fail(err)
	}
	outstanding, err := tx.OutstandingFollowUpFilingIntent(ctx, repositoryID)
	if err != nil {
		return fail(err)
	}
	if outstanding != nil {
		return fail(fmt.Errorf("%w: instance %q is outstanding in repository %d",
			domain.ErrFollowUpFilingRepositoryBusy, outstanding.InstanceID, repositoryID))
	}
	intent, err := domain.NewFollowUpFilingIntent(instanceID, repositoryID, openedAt)
	if err != nil {
		return fail(err)
	}
	if _, err := tx.tx.ExecContext(ctx, `INSERT INTO follow_up_filing_intents
		(instance_id, repository_id, opened_at) VALUES (?, ?, ?)`,
		intent.InstanceID, intent.RepositoryID, formatTime(intent.OpenedAt)); err != nil {
		return fail(err)
	}
	return tx.GetFollowUpFilingIntent(ctx, instanceID)
}

// RecordFollowUpFilingPreDispatch records the issue numbers a candidate
// search found before the first create attempt, with botUserID, the App bot
// account the search listed under and every create of the intent is sent as.
// The set is written once, before any attempt, and an empty set is a recorded
// fact distinct from no set: a second set, and any set after an attempt, is
// refused with ErrFollowUpFilingPreDispatchFixed.
func (tx *InternalTx) RecordFollowUpFilingPreDispatch(
	ctx context.Context, instanceID domain.ProposalInstanceID, issueNumbers []int, botUserID int64, at time.Time,
) (domain.FollowUpFilingIntent, error) {
	return tx.advanceFollowUpFiling(ctx, "record follow-up filing pre-dispatch set", instanceID,
		func(intent domain.FollowUpFilingIntent) (domain.FollowUpFilingIntent, error) {
			return intent.RecordPreDispatch(issueNumbers, botUserID, at)
		},
		func(next domain.FollowUpFilingIntent) error {
			numbers, err := json.Marshal(next.PreDispatch.IssueNumbers)
			if err != nil {
				return err
			}
			return tx.execOneFilingRow(ctx, `UPDATE follow_up_filing_intents
				SET pre_dispatch_issue_numbers = ?, pre_dispatch_bot_user_id = ?, pre_dispatch_recorded_at = ?
				WHERE instance_id = ? AND pre_dispatch_recorded_at IS NULL AND outcome IS NULL`,
				string(numbers), *next.PreDispatch.BotUserID, formatTime(next.PreDispatch.RecordedAt), instanceID)
		})
}

// StartFollowUpFilingAttempt records the dispatch-started marker for the
// next create attempt, which the caller commits before it sends the create.
// It is the §5.17 evidence rule: a create is allowed only when no earlier
// create for this instance can have committed, so the attempt is refused
// with ErrFollowUpFilingCreateUnproven unless there is no earlier attempt or
// the last one recorded a transient rejection.
func (tx *InternalTx) StartFollowUpFilingAttempt(
	ctx context.Context, instanceID domain.ProposalInstanceID, at time.Time,
) (domain.FollowUpFilingIntent, error) {
	return tx.advanceFollowUpFiling(ctx, "start follow-up filing attempt", instanceID,
		func(intent domain.FollowUpFilingIntent) (domain.FollowUpFilingIntent, error) {
			return intent.StartAttempt(at)
		},
		func(next domain.FollowUpFilingIntent) error {
			attempt := next.Attempts[len(next.Attempts)-1]
			_, err := tx.tx.ExecContext(ctx, `INSERT INTO follow_up_filing_attempts
				(instance_id, ordinal, dispatch_started_at) VALUES (?, ?, ?)`,
				instanceID, attempt.Ordinal, formatTime(attempt.DispatchStartedAt))
			return err
		})
}

// RecordFollowUpFilingResponse records how the dispatched attempt's create
// was answered when it did not return an issue: a transient rejection, a
// definite rejection, or unproven (an answer that leaves it unknown whether
// the issue exists). A success is recorded by LedgerFollowUpFilingSuccess,
// with its issue. A response is written once.
func (tx *InternalTx) RecordFollowUpFilingResponse(
	ctx context.Context, instanceID domain.ProposalInstanceID,
	class domain.FollowUpFilingResponseClass, at time.Time,
) (domain.FollowUpFilingIntent, error) {
	return tx.advanceFollowUpFiling(ctx, "record follow-up filing response", instanceID,
		func(intent domain.FollowUpFilingIntent) (domain.FollowUpFilingIntent, error) {
			return intent.RecordResponse(class, at)
		},
		func(next domain.FollowUpFilingIntent) error {
			return tx.writeFilingResponse(ctx, next)
		})
}

// LedgerFollowUpFilingSuccess records that the dispatched attempt's create
// returned an issue: the success response, the ledgered outcome, and the
// filed-issue row, in the caller's transaction.
func (tx *InternalTx) LedgerFollowUpFilingSuccess(
	ctx context.Context, instanceID domain.ProposalInstanceID, issueNumber int, at time.Time,
) (domain.FiledFollowUpIssue, error) {
	return tx.ledgerFollowUpFiling(ctx, "ledger follow-up filing success", instanceID, issueNumber, at,
		func(intent domain.FollowUpFilingIntent) (domain.FollowUpFilingIntent, error) {
			return intent.LedgerSuccess(issueNumber, at)
		}, true)
}

// AdoptFollowUpFilingCandidate ledgers an issue found after an attempt whose
// create may have committed without an answer. The store enforces the
// adoption conditions it can see and refuses with
// ErrFollowUpFilingAdoptionRefused when the intent has no dispatched
// attempt, when the issue was already present before the first dispatch, and
// when the repository holds an ambiguous outcome; an issue another instance
// already ledgered is refused with ErrFollowUpFilingIssueLedgered. Whether
// the candidate is otherwise the daemon's (its author, its creation window)
// is the caller's to establish before it calls.
func (tx *InternalTx) AdoptFollowUpFilingCandidate(
	ctx context.Context, instanceID domain.ProposalInstanceID, issueNumber int, at time.Time,
) (domain.FiledFollowUpIssue, error) {
	return tx.ledgerFollowUpFiling(ctx, "adopt follow-up filing candidate", instanceID, issueNumber, at,
		func(intent domain.FollowUpFilingIntent) (domain.FollowUpFilingIntent, error) {
			ambiguous, err := tx.FollowUpFilingRepositoryAmbiguous(ctx, intent.RepositoryID)
			if err != nil {
				return domain.FollowUpFilingIntent{}, err
			}
			return intent.Adopt(issueNumber, ambiguous, at)
		}, false)
}

// RefuseFollowUpFiling resolves the intent as refused: no issue was created
// and none will be. The reason must be one the last attempt supports, and no
// refusal is available while that attempt is unproven
// (ErrFollowUpFilingOutcomeRefused).
func (tx *InternalTx) RefuseFollowUpFiling(
	ctx context.Context, instanceID domain.ProposalInstanceID,
	reason domain.FollowUpFilingRefusalReason, at time.Time,
) (domain.FollowUpFilingIntent, error) {
	return tx.advanceFollowUpFiling(ctx, "refuse follow-up filing", instanceID,
		func(intent domain.FollowUpFilingIntent) (domain.FollowUpFilingIntent, error) {
			return intent.Refuse(reason, at)
		},
		func(next domain.FollowUpFilingIntent) error {
			return tx.writeFilingTerminal(ctx, next)
		})
}

// RecordFollowUpFilingAmbiguous resolves the intent as ambiguous: an attempt
// was dispatched and nothing establishes whether its issue exists. From then
// on no intent in the repository adopts.
func (tx *InternalTx) RecordFollowUpFilingAmbiguous(
	ctx context.Context, instanceID domain.ProposalInstanceID, at time.Time,
) (domain.FollowUpFilingIntent, error) {
	return tx.advanceFollowUpFiling(ctx, "record ambiguous follow-up filing", instanceID,
		func(intent domain.FollowUpFilingIntent) (domain.FollowUpFilingIntent, error) {
			return intent.RecordAmbiguous(at)
		},
		func(next domain.FollowUpFilingIntent) error {
			return tx.writeFilingTerminal(ctx, next)
		})
}

// advanceFollowUpFiling is the single write path for an existing intent:
// read and re-gate it, apply one pure transition, persist that transition's
// delta, and return the intent as the store now reconstructs it.
func (tx *InternalTx) advanceFollowUpFiling(
	ctx context.Context, operation string, instanceID domain.ProposalInstanceID,
	transition func(domain.FollowUpFilingIntent) (domain.FollowUpFilingIntent, error),
	persist func(next domain.FollowUpFilingIntent) error,
) (domain.FollowUpFilingIntent, error) {
	fail := func(err error) (domain.FollowUpFilingIntent, error) {
		return domain.FollowUpFilingIntent{}, fmt.Errorf("%s %q: %w", operation, instanceID, err)
	}
	intent, err := tx.GetFollowUpFilingIntent(ctx, instanceID)
	if err != nil {
		return fail(err)
	}
	next, err := transition(intent)
	if err != nil {
		return fail(err)
	}
	if err := persist(next); err != nil {
		return fail(err)
	}
	stored, err := tx.GetFollowUpFilingIntent(ctx, instanceID)
	if err != nil {
		return fail(err)
	}
	return stored, nil
}

// ledgerFollowUpFiling resolves an intent as ledgered and appends its
// filed-issue row. respond says whether the transition also recorded the
// success response on the dispatched attempt.
func (tx *InternalTx) ledgerFollowUpFiling(
	ctx context.Context, operation string, instanceID domain.ProposalInstanceID, issueNumber int, at time.Time,
	transition func(domain.FollowUpFilingIntent) (domain.FollowUpFilingIntent, error),
	respond bool,
) (domain.FiledFollowUpIssue, error) {
	fail := func(err error) (domain.FiledFollowUpIssue, error) {
		return domain.FiledFollowUpIssue{}, fmt.Errorf("%s %q: %w", operation, instanceID, err)
	}
	_, err := tx.advanceFollowUpFiling(ctx, operation, instanceID, transition,
		func(next domain.FollowUpFilingIntent) error {
			_, origin, err := tx.approvedFollowUpFiling(ctx, instanceID)
			if err != nil {
				return err
			}
			filed := domain.FiledFollowUpIssue{
				InstanceID: instanceID, RepositoryID: next.RepositoryID, IssueNumber: issueNumber,
				Origin: origin, LedgeredAt: at.UTC(),
			}
			if err := filed.BackedBy(next); err != nil {
				return err
			}
			body, err := encode(filed)
			if err != nil {
				return err
			}
			ledgered, err := tx.FiledFollowUpIssue(ctx, next.RepositoryID, issueNumber)
			if err != nil {
				return err
			}
			if ledgered != nil {
				return fmt.Errorf("%w: issue %d in repository %d belongs to instance %q",
					domain.ErrFollowUpFilingIssueLedgered, issueNumber, next.RepositoryID, ledgered.InstanceID)
			}
			if respond {
				if err := tx.writeFilingResponse(ctx, next); err != nil {
					return err
				}
			}
			if _, err := tx.tx.ExecContext(ctx, `INSERT INTO follow_up_filed_issues
				(repository_id, issue_number, instance_id, body) VALUES (?, ?, ?, ?)`,
				filed.RepositoryID, filed.IssueNumber, filed.InstanceID, body); err != nil {
				return err
			}
			return tx.writeFilingTerminal(ctx, next)
		})
	if err != nil {
		// advanceFollowUpFiling already named the operation and instance.
		return domain.FiledFollowUpIssue{}, err
	}
	filed, err := tx.FiledFollowUpIssueForInstance(ctx, instanceID)
	if err != nil {
		return fail(err)
	}
	if filed == nil {
		return fail(errRowInconsistent)
	}
	return *filed, nil
}

// writeFilingResponse persists the response the transition recorded on the
// intent's last attempt, guarded so a response is written once.
func (tx *InternalTx) writeFilingResponse(ctx context.Context, next domain.FollowUpFilingIntent) error {
	attempt := next.Attempts[len(next.Attempts)-1]
	var issueNumber any
	if attempt.Response.IssueNumber != nil {
		issueNumber = *attempt.Response.IssueNumber
	}
	return tx.execOneFilingRow(ctx, `UPDATE follow_up_filing_attempts
		SET response_class = ?, response_issue_number = ?, response_recorded_at = ?
		WHERE instance_id = ? AND ordinal = ? AND response_class IS NULL`,
		attempt.Response.Class, issueNumber, formatTime(attempt.Response.RecordedAt),
		next.InstanceID, attempt.Ordinal)
}

// writeFilingTerminal persists the terminal outcome the transition recorded,
// guarded so an outcome is written once.
func (tx *InternalTx) writeFilingTerminal(ctx context.Context, next domain.FollowUpFilingIntent) error {
	var reason any
	if next.Terminal.Reason != nil {
		reason = *next.Terminal.Reason
	}
	return tx.execOneFilingRow(ctx, `UPDATE follow_up_filing_intents
		SET outcome = ?, refusal_reason = ?, resolved_at = ?
		WHERE instance_id = ? AND outcome IS NULL`,
		next.Terminal.Outcome, reason, formatTime(next.Terminal.ResolvedAt), next.InstanceID)
}

// execOneFilingRow runs a guarded ledger update and requires it to change
// exactly one row. The transition was computed from a read in this
// transaction, so a guard that matches nothing means the stored row is not
// the one that was read.
func (tx *InternalTx) execOneFilingRow(ctx context.Context, query string, args ...any) error {
	result, err := tx.tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return errRowInconsistent
	}
	return nil
}

// GetFollowUpFilingIntent reconstructs one instance's intent with its
// pre-dispatch set, attempts, and terminal outcome. ErrNotFound means the
// instance has no intent; an intent whose rows or backing proposal no longer
// hold together is an error, never ErrNotFound.
func (tx *ReadTx) GetFollowUpFilingIntent(
	ctx context.Context, instanceID domain.ProposalInstanceID,
) (domain.FollowUpFilingIntent, error) {
	row := tx.tx.QueryRowContext(ctx, filingIntentSQL, instanceID)
	stored, err := scanFilingIntentRow(row)
	if err != nil {
		return domain.FollowUpFilingIntent{}, fmt.Errorf("get follow-up filing intent %q: %w", instanceID, notFoundOr(err))
	}
	intent, err := tx.reconstructFilingIntent(ctx, stored)
	if err != nil {
		return domain.FollowUpFilingIntent{}, fmt.Errorf("get follow-up filing intent %q: %w", instanceID, err)
	}
	return intent, nil
}

// OutstandingFollowUpFilingIntent returns the repository's intent that has
// no terminal outcome, or nil when every intent there is resolved. Every
// intent in the repository must reconstruct for the answer to be nil.
func (tx *ReadTx) OutstandingFollowUpFilingIntent(
	ctx context.Context, repositoryID int64,
) (*domain.FollowUpFilingIntent, error) {
	fail := func(err error) (*domain.FollowUpFilingIntent, error) {
		return nil, fmt.Errorf("outstanding follow-up filing intent in repository %d: %w", repositoryID, err)
	}
	intents, err := tx.repositoryFilingIntents(ctx, repositoryID)
	if err != nil {
		return fail(err)
	}
	var outstanding *domain.FollowUpFilingIntent
	for index := range intents {
		if !intents[index].Outstanding() {
			continue
		}
		if outstanding != nil {
			return fail(errRowInconsistent)
		}
		outstanding = &intents[index]
	}
	return outstanding, nil
}

// FollowUpFilingRepositoryAmbiguous reports whether any intent in the
// repository resolved as ambiguous, the state in which no later intent may
// adopt.
func (tx *ReadTx) FollowUpFilingRepositoryAmbiguous(ctx context.Context, repositoryID int64) (bool, error) {
	intents, err := tx.repositoryFilingIntents(ctx, repositoryID)
	if err != nil {
		return false, fmt.Errorf("follow-up filing ambiguity in repository %d: %w", repositoryID, err)
	}
	for _, intent := range intents {
		if intent.Terminal != nil && intent.Terminal.Outcome == domain.FollowUpFilingAmbiguous {
			return true, nil
		}
	}
	return false, nil
}

// FollowUpFilingMayHaveCreated reports whether the daemon may have created
// an issue in the repository: true when any intent there has an attempt not
// recorded as rejected, resolved as ledgered or ambiguous, or when the
// ledger holds a row for the repository. False is a positive statement that
// every create the daemon dispatched there is recorded as rejected, so it is
// returned only when every intent and ledger row for the repository
// reconstructs; anything unreadable is an error.
func (tx *ReadTx) FollowUpFilingMayHaveCreated(ctx context.Context, repositoryID int64) (bool, error) {
	fail := func(err error) (bool, error) {
		return false, fmt.Errorf("follow-up filing may have created in repository %d: %w", repositoryID, err)
	}
	intents, err := tx.repositoryFilingIntents(ctx, repositoryID)
	if err != nil {
		return fail(err)
	}
	filed, err := tx.filedFollowUpIssues(ctx, repositoryFiledIssuesSQL, repositoryID)
	if err != nil {
		return fail(err)
	}
	if len(filed) > 0 {
		return true, nil
	}
	for _, intent := range intents {
		if intent.MayHaveCreated() {
			return true, nil
		}
	}
	return false, nil
}

// FiledFollowUpIssue returns the ledger row for one issue, or nil when the
// daemon did not file it. A row that does not reconstruct is an error, never
// nil.
func (tx *ReadTx) FiledFollowUpIssue(
	ctx context.Context, repositoryID int64, issueNumber int,
) (*domain.FiledFollowUpIssue, error) {
	filed, err := tx.filedFollowUpIssues(ctx, filedFollowUpIssueSQL, repositoryID, issueNumber)
	if err != nil {
		return nil, fmt.Errorf("filed follow-up issue %d in repository %d: %w", issueNumber, repositoryID, err)
	}
	if len(filed) == 0 {
		return nil, nil
	}
	return &filed[0], nil
}

// FiledFollowUpIssueForInstance returns the issue ledgered for one proposal
// instance, or nil when it has none. With no row the answer comes from the
// instance's intent: a ledgered intent whose row is gone fails to
// reconstruct, so that state is an error here too, never nil.
func (tx *ReadTx) FiledFollowUpIssueForInstance(
	ctx context.Context, instanceID domain.ProposalInstanceID,
) (*domain.FiledFollowUpIssue, error) {
	filed, err := tx.filedFollowUpIssues(ctx, instanceFiledIssueSQL, instanceID)
	if err != nil {
		return nil, fmt.Errorf("filed follow-up issue for instance %q: %w", instanceID, err)
	}
	if len(filed) > 0 {
		return &filed[0], nil
	}
	if _, err := tx.GetFollowUpFilingIntent(ctx, instanceID); err != nil && !errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("filed follow-up issue for instance %q: %w", instanceID, err)
	}
	return nil, nil
}

// repositoryFilingIntents reconstructs every intent the repository's key
// column names, in opening order.
func (tx *ReadTx) repositoryFilingIntents(
	ctx context.Context, repositoryID int64,
) ([]domain.FollowUpFilingIntent, error) {
	rows, err := tx.tx.QueryContext(ctx, repositoryFilingIntentsSQL, repositoryID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var stored []filingIntentRow
	for rows.Next() {
		row, err := scanFilingIntentRow(rows)
		if err != nil {
			return nil, err
		}
		stored = append(stored, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Reconstruction runs its own queries, so the cursor is drained first.
	if err := rows.Close(); err != nil {
		return nil, err
	}
	intents := make([]domain.FollowUpFilingIntent, 0, len(stored))
	for _, row := range stored {
		intent, err := tx.reconstructFilingIntent(ctx, row)
		if err != nil {
			return nil, fmt.Errorf("intent %q: %w", row.instanceID, err)
		}
		intents = append(intents, intent)
	}
	return intents, nil
}

// filingIntentRow is one scanned intent row, before its attempts are read
// and before anything in it is trusted.
type filingIntentRow struct {
	instanceID          string
	repositoryID        int64
	openedAt            string
	preDispatchNumbers  sql.NullString
	preDispatchBot      sql.NullInt64
	preDispatchRecorded sql.NullString
	outcome             sql.NullString
	refusalReason       sql.NullString
	resolvedAt          sql.NullString
}

func scanFilingIntentRow(row scanner) (filingIntentRow, error) {
	var stored filingIntentRow
	err := row.Scan(&stored.instanceID, &stored.repositoryID, &stored.openedAt,
		&stored.preDispatchNumbers, &stored.preDispatchBot, &stored.preDispatchRecorded,
		&stored.outcome, &stored.refusalReason, &stored.resolvedAt)
	return stored, err
}

// reconstructFilingIntent is the single reconstruction path for an intent:
// rebuild it from its row and attempt rows, validate it as a history the
// §5.17 rule could have produced, then re-gate it against current rows. The
// instance must still be an approved follow-up filing whose derived target
// repository is the intent's, and the filed-issue row must be present
// exactly when the outcome is ledgered.
func (tx *ReadTx) reconstructFilingIntent(
	ctx context.Context, stored filingIntentRow,
) (domain.FollowUpFilingIntent, error) {
	fail := func(err error) (domain.FollowUpFilingIntent, error) {
		return domain.FollowUpFilingIntent{}, err
	}
	inconsistent := func(err error) (domain.FollowUpFilingIntent, error) {
		return fail(fmt.Errorf("%w: %w", errRowInconsistent, err))
	}
	openedAt, err := parseTime(stored.openedAt)
	if err != nil {
		return inconsistent(err)
	}
	intent := domain.FollowUpFilingIntent{
		InstanceID:   domain.ProposalInstanceID(stored.instanceID),
		RepositoryID: stored.repositoryID,
		OpenedAt:     openedAt,
	}
	if intent.PreDispatch, err = decodeFilingPreDispatch(stored); err != nil {
		return inconsistent(err)
	}
	if intent.Terminal, err = decodeFilingTerminal(stored); err != nil {
		return inconsistent(err)
	}
	if intent.Attempts, err = tx.filingAttempts(ctx, intent.InstanceID); err != nil {
		return fail(err)
	}
	if err := intent.Validate(); err != nil {
		return inconsistent(err)
	}
	repositoryID, _, err := tx.approvedFollowUpFiling(ctx, intent.InstanceID)
	if err != nil {
		return fail(filingBackingError(err))
	}
	if repositoryID != intent.RepositoryID {
		return fail(fmt.Errorf("%w: intent names repository %d, its proposal targets %d",
			errRowInconsistent, intent.RepositoryID, repositoryID))
	}
	var ledgerRows int
	if err := tx.tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM follow_up_filed_issues WHERE instance_id = ?`,
		intent.InstanceID).Scan(&ledgerRows); err != nil {
		return fail(err)
	}
	ledgered := intent.Terminal != nil && intent.Terminal.Outcome == domain.FollowUpFilingLedgered
	if ledgered != (ledgerRows == 1) || ledgerRows > 1 {
		return fail(fmt.Errorf("%w: ledgered outcome and filed-issue row disagree", errRowInconsistent))
	}
	return intent, nil
}

// decodeFilingPreDispatch reads the recorded pre-dispatch set. The numbers
// are stored as a canonical JSON array, so a column that does not re-encode
// to itself was not written by the store. The dispatching identity is
// written with the set and is absent only on a set that predates migration
// 0093; it is never trusted from here, because the filer compares it with the
// repository's live App identity before every step that relies on it.
func decodeFilingPreDispatch(stored filingIntentRow) (*domain.FollowUpFilingPreDispatchSet, error) {
	if !stored.preDispatchNumbers.Valid && !stored.preDispatchRecorded.Valid {
		if stored.preDispatchBot.Valid {
			return nil, errors.New("pre-dispatch identity is recorded without its set")
		}
		return nil, nil
	}
	if !stored.preDispatchNumbers.Valid || !stored.preDispatchRecorded.Valid {
		return nil, errors.New("pre-dispatch set is half recorded")
	}
	var numbers []int
	if err := json.Unmarshal([]byte(stored.preDispatchNumbers.String), &numbers); err != nil {
		return nil, fmt.Errorf("pre-dispatch issue numbers: %w", err)
	}
	canonical, err := json.Marshal(numbers)
	if err != nil {
		return nil, err
	}
	if numbers == nil || string(canonical) != stored.preDispatchNumbers.String {
		return nil, errors.New("pre-dispatch issue numbers are not canonical")
	}
	recordedAt, err := parseTime(stored.preDispatchRecorded.String)
	if err != nil {
		return nil, err
	}
	set := &domain.FollowUpFilingPreDispatchSet{IssueNumbers: numbers, RecordedAt: recordedAt}
	if stored.preDispatchBot.Valid {
		set.BotUserID = &stored.preDispatchBot.Int64
	}
	return set, nil
}

func decodeFilingTerminal(stored filingIntentRow) (*domain.FollowUpFilingTerminal, error) {
	if !stored.outcome.Valid && !stored.refusalReason.Valid && !stored.resolvedAt.Valid {
		return nil, nil
	}
	if !stored.outcome.Valid || !stored.resolvedAt.Valid {
		return nil, errors.New("terminal outcome is half recorded")
	}
	resolvedAt, err := parseTime(stored.resolvedAt.String)
	if err != nil {
		return nil, err
	}
	terminal := &domain.FollowUpFilingTerminal{
		Outcome:    domain.FollowUpFilingOutcome(stored.outcome.String),
		ResolvedAt: resolvedAt,
	}
	if stored.refusalReason.Valid {
		reason := domain.FollowUpFilingRefusalReason(stored.refusalReason.String)
		terminal.Reason = &reason
	}
	return terminal, nil
}

// filingAttempts reads an intent's attempt rows in ordinal order. It returns
// an empty, non-nil list for an intent with none, the shape the domain type
// requires.
func (tx *ReadTx) filingAttempts(
	ctx context.Context, instanceID domain.ProposalInstanceID,
) ([]domain.FollowUpFilingAttempt, error) {
	rows, err := tx.tx.QueryContext(ctx, selectFilingAttemptsSQL, instanceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	attempts := []domain.FollowUpFilingAttempt{}
	for rows.Next() {
		var (
			attempt     domain.FollowUpFilingAttempt
			startedAt   string
			class       sql.NullString
			issueNumber sql.NullInt64
			recordedAt  sql.NullString
		)
		if err := rows.Scan(&attempt.Ordinal, &startedAt, &class, &issueNumber, &recordedAt); err != nil {
			return nil, err
		}
		if attempt.DispatchStartedAt, err = parseTime(startedAt); err != nil {
			return nil, fmt.Errorf("%w: %w", errRowInconsistent, err)
		}
		if class.Valid != recordedAt.Valid || (issueNumber.Valid && !class.Valid) {
			return nil, fmt.Errorf("%w: attempt %d response is half recorded", errRowInconsistent, attempt.Ordinal)
		}
		if class.Valid {
			response := domain.FollowUpFilingAttemptResponse{Class: domain.FollowUpFilingResponseClass(class.String)}
			if response.RecordedAt, err = parseTime(recordedAt.String); err != nil {
				return nil, fmt.Errorf("%w: %w", errRowInconsistent, err)
			}
			if issueNumber.Valid {
				number := int(issueNumber.Int64)
				response.IssueNumber = &number
			}
			attempt.Response = &response
		}
		attempts = append(attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return attempts, nil
}

// filedFollowUpIssues reconstructs the ledger rows one of this file's
// key-column queries selects.
func (tx *ReadTx) filedFollowUpIssues(
	ctx context.Context, query string, args ...any,
) ([]domain.FiledFollowUpIssue, error) {
	rows, err := tx.tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var decoded []domain.FiledFollowUpIssue
	for rows.Next() {
		filed, err := scanFiledFollowUpIssue(rows)
		if err != nil {
			return nil, err
		}
		decoded = append(decoded, filed)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The backing checks run their own queries, so the cursor is drained first.
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for _, filed := range decoded {
		if err := tx.gateFiledFollowUpIssue(ctx, filed); err != nil {
			return nil, fmt.Errorf("issue %d in repository %d: %w", filed.IssueNumber, filed.RepositoryID, err)
		}
	}
	return decoded, nil
}

// scanFiledFollowUpIssue decodes and validates a ledger row's body, the
// authority, and cross-checks every key column against it.
func scanFiledFollowUpIssue(row scanner) (domain.FiledFollowUpIssue, error) {
	var (
		repositoryID int64
		issueNumber  int
		instanceID   string
		body         string
	)
	if err := row.Scan(&repositoryID, &issueNumber, &instanceID, &body); err != nil {
		return domain.FiledFollowUpIssue{}, err
	}
	filed, err := decode[domain.FiledFollowUpIssue]([]byte(body))
	if err != nil {
		return domain.FiledFollowUpIssue{}, fmt.Errorf("%w: %w", errRowInconsistent, err)
	}
	if filed.RepositoryID != repositoryID || filed.IssueNumber != issueNumber ||
		string(filed.InstanceID) != instanceID {
		return domain.FiledFollowUpIssue{}, errRowInconsistent
	}
	return filed, nil
}

// gateFiledFollowUpIssue re-derives everything a ledger row claims: its
// intent must reconstruct (which re-gates the approved proposal and its
// target repository), be ledgered with an attempt that accounts for the
// issue number, and the row's origin must be the project and run the
// proposal's subject resolves to now.
func (tx *ReadTx) gateFiledFollowUpIssue(ctx context.Context, filed domain.FiledFollowUpIssue) error {
	intent, err := tx.GetFollowUpFilingIntent(ctx, filed.InstanceID)
	if err != nil {
		return filingBackingError(err)
	}
	if err := filed.BackedBy(intent); err != nil {
		return fmt.Errorf("%w: %w", errRowInconsistent, err)
	}
	_, origin, err := tx.approvedFollowUpFiling(ctx, filed.InstanceID)
	if err != nil {
		return filingBackingError(err)
	}
	if origin != filed.Origin {
		return fmt.Errorf("%w: ledger row origin differs from the proposal subject", errRowInconsistent)
	}
	return nil
}

// approvedFollowUpFiling re-derives, from current rows, what an approved
// follow-up filing instance authorizes: the repository it files into and the
// project and run it originates from. GetProposalInstance re-runs the
// registry gate; the target comes from the project row and the run's pinned
// policy, not the decoded proposal body; and the approval is re-tied to its
// authoring command and the rendered digest that command decided, as
// RecordProposalDecision checked on write. A decision row whose action was
// changed to approve still names its original command and fails here.
//
// ErrFollowUpFilingNotApproved covers an instance of another effect kind, an
// undecided one, and a declined one.
func (tx *ReadTx) approvedFollowUpFiling(
	ctx context.Context, instanceID domain.ProposalInstanceID,
) (int64, domain.FollowUpFilingOrigin, error) {
	fail := func(err error) (int64, domain.FollowUpFilingOrigin, error) {
		return 0, domain.FollowUpFilingOrigin{}, err
	}
	instance, err := tx.GetProposalInstance(ctx, instanceID)
	if err != nil {
		return fail(err)
	}
	filing := instance.Proposal.FilingProposal
	if instance.Proposal.Kind != domain.EffectFollowUpFiling || filing == nil {
		return fail(fmt.Errorf("%w: instance is a %q proposal", domain.ErrFollowUpFilingNotApproved, instance.Proposal.Kind))
	}
	declaration, policy, err := tx.ResolveProposalSubject(ctx, filing.SubjectHandle)
	if err != nil {
		return fail(err)
	}
	target, err := tx.followUpFilingTarget(ctx, declaration, policy)
	if err != nil {
		return fail(err)
	}
	var commandID, action string
	var selectedDigest sql.NullString
	err = tx.tx.QueryRowContext(ctx, `SELECT command_id, action, selected_digest
		FROM effect_proposal_decisions WHERE instance_id = ?`,
		instanceID).Scan(&commandID, &action, &selectedDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return fail(fmt.Errorf("%w: no decision is recorded", domain.ErrFollowUpFilingNotApproved))
	}
	if err != nil {
		return fail(err)
	}
	if domain.Action(action) != domain.ActionApprove {
		return fail(fmt.Errorf("%w: the recorded decision is %q", domain.ErrFollowUpFilingNotApproved, action))
	}
	command, err := tx.GetCommand(ctx, commandID)
	if err != nil {
		return fail(err)
	}
	boundInstance, boundDigest, _, err := tx.proposalItemBinding(ctx, command.ItemID)
	if err != nil {
		return fail(err)
	}
	if command.Action != domain.ActionApprove || boundInstance != instanceID ||
		!selectedDigest.Valid || domain.Digest(selectedDigest.String) != boundDigest {
		return fail(fmt.Errorf("%w: approval does not bind its command and digest", errRowInconsistent))
	}
	return target.RepositoryID, domain.FollowUpFilingOrigin{
		ProjectID: declaration.ProjectID, RunID: declaration.RunID,
	}, nil
}

// filingBackingError keeps a missing or unapproved backing row from reading
// as an absent ledger fact. A stored intent or ledger row whose proposal is
// gone or no longer approved is inconsistent; the cause is reported as text
// so ErrNotFound and ErrFollowUpFilingNotApproved cannot leak to a caller
// that treats them as "nothing recorded".
func filingBackingError(err error) error {
	if errors.Is(err, ErrNotFound) || errors.Is(err, domain.ErrFollowUpFilingNotApproved) {
		return fmt.Errorf("%w: %v", errRowInconsistent, err) //nolint:errorlint // deliberately unwrapped, see above
	}
	return err
}
