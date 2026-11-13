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

// Issue comment ledger (plan §5.13, issue #3147). Daemon-internal: never
// synchronized, written on InternalTx. It mirrors the follow-up filing
// ledger (follow_up_filing_ledger.go) and keeps its write discipline: every
// write is a transition the pure rule in domain permits from the intent's
// stored state, persisted as exactly its delta under a guard that refuses a
// row another writer moved first. Only opening an intent is idempotent.
//
// The scope that serializes creates is the target issue: one outstanding
// intent per (repository, issue), and an ambiguous outcome blocks adoption
// on that issue only.
//
// Trust posture: no stored bit is believed on its own. An intent is rebuilt
// from its rows, validated as a history the rule could have produced, and
// re-gated against current rows: its instance must still be an approved
// issue comment whose re-derived repository and issue are the ones the
// intent names. A posted-comment row must decode to its key columns and be
// backed by a ledgered intent whose attempts account for the comment. A row
// that fails any of that is an error, never an absent answer. Every row the
// re-gate reads is write-once (the proposal instance, its decision or policy
// approval, the deciding command, the item binding, the duplicate search
// record) or immutable where it matters (a project's repository identity, a
// run's pinned policy).

const (
	selectCommentIntentsSQL = `
SELECT instance_id, repository_id, issue_number, opened_at, pre_dispatch_comment_ids,
       pre_dispatch_bot_user_id, pre_dispatch_recorded_at, outcome,
       refusal_reason, resolved_at
FROM issue_comment_intents`
	selectCommentAttemptsSQL = `
SELECT ordinal, dispatch_started_at, response_class, response_comment_id,
       response_recorded_at
FROM issue_comment_attempts WHERE instance_id = ? ORDER BY ordinal`
	selectPostedCommentsSQL = `
SELECT repository_id, comment_id, issue_number, instance_id, body
FROM posted_issue_comments`
	commentIntentSQL       = selectCommentIntentsSQL + ` WHERE instance_id = ?`
	issueCommentIntentsSQL = selectCommentIntentsSQL +
		` WHERE repository_id = ? AND issue_number = ? ORDER BY opened_at, instance_id`
	postedCommentSQL         = selectPostedCommentsSQL + ` WHERE repository_id = ? AND comment_id = ?`
	instancePostedCommentSQL = selectPostedCommentsSQL + ` WHERE instance_id = ?`
)

// OpenIssueCommentIntent records the durable posting intent for one approved
// issue comment proposal instance and returns it. A repeat for an instance
// that already has an intent returns that intent unchanged, whatever state it
// has reached.
//
// The instance must be an issue comment with a recorded approve decision or
// policy approval (ErrIssueCommentNotApproved otherwise). The repository and
// issue come from the proposal's re-gated target, never from the caller. An
// issue holds one outstanding intent at a time: while another instance's
// intent there has no terminal outcome, opening is refused with
// ErrIssueCommentIssueBusy.
func (tx *InternalTx) OpenIssueCommentIntent(
	ctx context.Context, instanceID domain.ProposalInstanceID, openedAt time.Time,
) (domain.IssueCommentIntent, error) {
	fail := func(err error) (domain.IssueCommentIntent, error) {
		return domain.IssueCommentIntent{}, fmt.Errorf("open issue comment intent %q: %w", instanceID, err)
	}
	existing, err := tx.GetIssueCommentIntent(ctx, instanceID)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return fail(err)
	}
	if err := tx.RequireProposalWorkflowOpen(ctx, instanceID); err != nil {
		return fail(err)
	}
	approved, err := tx.approvedIssueComment(ctx, instanceID)
	if err != nil {
		return fail(err)
	}
	outstanding, err := tx.OutstandingIssueCommentIntent(ctx, approved.RepositoryID, approved.IssueNumber)
	if err != nil {
		return fail(err)
	}
	if outstanding != nil {
		return fail(fmt.Errorf("%w: instance %q is outstanding on issue %d in repository %d",
			domain.ErrIssueCommentIssueBusy, outstanding.InstanceID, approved.IssueNumber, approved.RepositoryID))
	}
	intent, err := domain.NewIssueCommentIntent(instanceID, approved.RepositoryID, approved.IssueNumber, openedAt)
	if err != nil {
		return fail(err)
	}
	if _, err := tx.tx.ExecContext(ctx, `INSERT INTO issue_comment_intents
		(instance_id, repository_id, issue_number, opened_at) VALUES (?, ?, ?, ?)`,
		intent.InstanceID, intent.RepositoryID, intent.IssueNumber, formatTime(intent.OpenedAt)); err != nil {
		return fail(err)
	}
	return tx.GetIssueCommentIntent(ctx, instanceID)
}

// RecordIssueCommentPreDispatch records the App's comments on the target
// issue seen before the first attempt, with botUserID, the account they were
// listed under and every create of the intent is sent as. The set is written
// once, before any attempt (ErrIssueCommentPreDispatchFixed otherwise).
func (tx *InternalTx) RecordIssueCommentPreDispatch(
	ctx context.Context, instanceID domain.ProposalInstanceID, commentIDs []int64, botUserID int64, at time.Time,
) (domain.IssueCommentIntent, error) {
	return tx.advanceIssueComment(ctx, "record issue comment pre-dispatch set", instanceID,
		func(intent domain.IssueCommentIntent) (domain.IssueCommentIntent, error) {
			return intent.RecordPreDispatch(commentIDs, botUserID, at)
		},
		func(next domain.IssueCommentIntent) error {
			ids, err := json.Marshal(next.PreDispatch.CommentIDs)
			if err != nil {
				return err
			}
			return tx.execOneFilingRow(ctx, `UPDATE issue_comment_intents
				SET pre_dispatch_comment_ids = ?, pre_dispatch_bot_user_id = ?, pre_dispatch_recorded_at = ?
				WHERE instance_id = ? AND pre_dispatch_recorded_at IS NULL AND outcome IS NULL`,
				string(ids), next.PreDispatch.BotUserID, formatTime(next.PreDispatch.RecordedAt), instanceID)
		})
}

// StartIssueCommentAttempt records the dispatch-started marker for the next
// create, which the caller commits before it sends the create. It is refused
// with ErrIssueCommentCreateUnproven unless no earlier attempt exists or the
// last one recorded a transient rejection.
func (tx *InternalTx) StartIssueCommentAttempt(
	ctx context.Context, instanceID domain.ProposalInstanceID, at time.Time,
) (domain.IssueCommentIntent, error) {
	if err := tx.RequireProposalWorkflowOpen(ctx, instanceID); err != nil {
		return domain.IssueCommentIntent{}, err
	}
	return tx.advanceIssueComment(ctx, "start issue comment attempt", instanceID,
		func(intent domain.IssueCommentIntent) (domain.IssueCommentIntent, error) {
			return intent.StartAttempt(at)
		},
		func(next domain.IssueCommentIntent) error {
			attempt := next.Attempts[len(next.Attempts)-1]
			_, err := tx.tx.ExecContext(ctx, `INSERT INTO issue_comment_attempts
				(instance_id, ordinal, dispatch_started_at) VALUES (?, ?, ?)`,
				instanceID, attempt.Ordinal, formatTime(attempt.DispatchStartedAt))
			return err
		})
}

// RecordIssueCommentResponse records how the dispatched attempt's create was
// answered when it did not return a comment: a transient rejection, a
// definite rejection, or unproven. A success is recorded by
// LedgerIssueCommentSuccess.
func (tx *InternalTx) RecordIssueCommentResponse(
	ctx context.Context, instanceID domain.ProposalInstanceID,
	class domain.FollowUpFilingResponseClass, at time.Time,
) (domain.IssueCommentIntent, error) {
	return tx.advanceIssueComment(ctx, "record issue comment response", instanceID,
		func(intent domain.IssueCommentIntent) (domain.IssueCommentIntent, error) {
			return intent.RecordResponse(class, at)
		},
		func(next domain.IssueCommentIntent) error {
			return tx.writeCommentResponse(ctx, next)
		})
}

// LedgerIssueCommentSuccess records that the dispatched attempt's create
// returned a comment: the success response, the ledgered outcome, and the
// posted-comment row, in the caller's transaction.
func (tx *InternalTx) LedgerIssueCommentSuccess(
	ctx context.Context, instanceID domain.ProposalInstanceID, commentID int64, at time.Time,
) (domain.PostedIssueComment, error) {
	return tx.ledgerIssueComment(ctx, "ledger issue comment success", instanceID, commentID, at,
		func(intent domain.IssueCommentIntent) (domain.IssueCommentIntent, error) {
			return intent.LedgerSuccess(commentID, at)
		}, true)
}

// AdoptIssueCommentCandidate ledgers a comment found after an attempt whose
// create may have committed without an answer. The store refuses with
// ErrIssueCommentAdoptionRefused when the intent has no dispatched attempt,
// when the comment was present before the first dispatch, and when the issue
// holds an ambiguous outcome; a comment another instance already ledgered is
// refused with ErrIssueCommentLedgered. Whether the candidate is otherwise
// the daemon's (its issue, its author, its creation window) is the caller's
// to establish.
func (tx *InternalTx) AdoptIssueCommentCandidate(
	ctx context.Context, instanceID domain.ProposalInstanceID, commentID int64, at time.Time,
) (domain.PostedIssueComment, error) {
	return tx.ledgerIssueComment(ctx, "adopt issue comment candidate", instanceID, commentID, at,
		func(intent domain.IssueCommentIntent) (domain.IssueCommentIntent, error) {
			ambiguous, err := tx.IssueCommentIssueAmbiguous(ctx, intent.RepositoryID, intent.IssueNumber)
			if err != nil {
				return domain.IssueCommentIntent{}, err
			}
			return intent.Adopt(commentID, ambiguous, at)
		}, false)
}

// RefuseIssueComment resolves the intent as refused: no comment was posted
// and none will be. The reason must be one the last attempt supports
// (ErrIssueCommentOutcomeRefused otherwise).
func (tx *InternalTx) RefuseIssueComment(
	ctx context.Context, instanceID domain.ProposalInstanceID,
	reason domain.FollowUpFilingRefusalReason, at time.Time,
) (domain.IssueCommentIntent, error) {
	return tx.advanceIssueComment(ctx, "refuse issue comment", instanceID,
		func(intent domain.IssueCommentIntent) (domain.IssueCommentIntent, error) {
			return intent.Refuse(reason, at)
		},
		func(next domain.IssueCommentIntent) error {
			return tx.writeCommentTerminal(ctx, next)
		})
}

// RecordIssueCommentAmbiguous resolves the intent as ambiguous: an attempt
// was dispatched and nothing establishes whether its comment exists. From
// then on no intent on the issue adopts.
func (tx *InternalTx) RecordIssueCommentAmbiguous(
	ctx context.Context, instanceID domain.ProposalInstanceID, at time.Time,
) (domain.IssueCommentIntent, error) {
	return tx.advanceIssueComment(ctx, "record ambiguous issue comment", instanceID,
		func(intent domain.IssueCommentIntent) (domain.IssueCommentIntent, error) {
			return intent.RecordAmbiguous(at)
		},
		func(next domain.IssueCommentIntent) error {
			return tx.writeCommentTerminal(ctx, next)
		})
}

// advanceIssueComment is the single write path for an existing intent: read
// and re-gate it, apply one pure transition, persist its delta, and return
// the intent as the store now reconstructs it.
func (tx *InternalTx) advanceIssueComment(
	ctx context.Context, operation string, instanceID domain.ProposalInstanceID,
	transition func(domain.IssueCommentIntent) (domain.IssueCommentIntent, error),
	persist func(next domain.IssueCommentIntent) error,
) (domain.IssueCommentIntent, error) {
	fail := func(err error) (domain.IssueCommentIntent, error) {
		return domain.IssueCommentIntent{}, fmt.Errorf("%s %q: %w", operation, instanceID, err)
	}
	intent, err := tx.GetIssueCommentIntent(ctx, instanceID)
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
	stored, err := tx.GetIssueCommentIntent(ctx, instanceID)
	if err != nil {
		return fail(err)
	}
	return stored, nil
}

// ledgerIssueComment resolves an intent as ledgered and appends its
// posted-comment row. respond says whether the transition also recorded the
// success response on the dispatched attempt.
func (tx *InternalTx) ledgerIssueComment(
	ctx context.Context, operation string, instanceID domain.ProposalInstanceID, commentID int64, at time.Time,
	transition func(domain.IssueCommentIntent) (domain.IssueCommentIntent, error),
	respond bool,
) (domain.PostedIssueComment, error) {
	fail := func(err error) (domain.PostedIssueComment, error) {
		return domain.PostedIssueComment{}, fmt.Errorf("%s %q: %w", operation, instanceID, err)
	}
	_, err := tx.advanceIssueComment(ctx, operation, instanceID, transition,
		func(next domain.IssueCommentIntent) error {
			posted := domain.PostedIssueComment{
				InstanceID: instanceID, RepositoryID: next.RepositoryID, IssueNumber: next.IssueNumber,
				CommentID: commentID, LedgeredAt: at.UTC(),
			}
			if err := posted.Validate(); err != nil {
				return err
			}
			if err := posted.BackedBy(next); err != nil {
				return err
			}
			body, err := encode(posted)
			if err != nil {
				return err
			}
			ledgered, err := tx.PostedIssueComment(ctx, next.RepositoryID, commentID)
			if err != nil {
				return err
			}
			if ledgered != nil {
				return fmt.Errorf("%w: comment %d in repository %d belongs to instance %q",
					domain.ErrIssueCommentLedgered, commentID, next.RepositoryID, ledgered.InstanceID)
			}
			if respond {
				if err := tx.writeCommentResponse(ctx, next); err != nil {
					return err
				}
			}
			if _, err := tx.tx.ExecContext(ctx, `INSERT INTO posted_issue_comments
				(repository_id, comment_id, issue_number, instance_id, body) VALUES (?, ?, ?, ?, ?)`,
				posted.RepositoryID, posted.CommentID, posted.IssueNumber, posted.InstanceID, body); err != nil {
				return err
			}
			return tx.writeCommentTerminal(ctx, next)
		})
	if err != nil {
		// advanceIssueComment already named the operation and instance.
		return domain.PostedIssueComment{}, err
	}
	posted, err := tx.PostedIssueCommentForInstance(ctx, instanceID)
	if err != nil {
		return fail(err)
	}
	if posted == nil {
		return fail(errRowInconsistent)
	}
	return *posted, nil
}

// writeCommentResponse persists the response the transition recorded on the
// intent's last attempt, guarded so a response is written once.
func (tx *InternalTx) writeCommentResponse(ctx context.Context, next domain.IssueCommentIntent) error {
	attempt := next.Attempts[len(next.Attempts)-1]
	var commentID any
	if attempt.Response.CommentID != nil {
		commentID = *attempt.Response.CommentID
	}
	return tx.execOneFilingRow(ctx, `UPDATE issue_comment_attempts
		SET response_class = ?, response_comment_id = ?, response_recorded_at = ?
		WHERE instance_id = ? AND ordinal = ? AND response_class IS NULL`,
		attempt.Response.Class, commentID, formatTime(attempt.Response.RecordedAt),
		next.InstanceID, attempt.Ordinal)
}

// writeCommentTerminal persists the terminal outcome the transition
// recorded, guarded so an outcome is written once.
func (tx *InternalTx) writeCommentTerminal(ctx context.Context, next domain.IssueCommentIntent) error {
	var reason any
	if next.Terminal.Reason != nil {
		reason = *next.Terminal.Reason
	}
	return tx.execOneFilingRow(ctx, `UPDATE issue_comment_intents
		SET outcome = ?, refusal_reason = ?, resolved_at = ?
		WHERE instance_id = ? AND outcome IS NULL`,
		next.Terminal.Outcome, reason, formatTime(next.Terminal.ResolvedAt), next.InstanceID)
}

// GetIssueCommentIntent reconstructs one instance's intent with its
// pre-dispatch set, attempts, and terminal outcome. ErrNotFound means the
// instance has no intent; an intent whose rows or backing proposal no longer
// hold together is an error, never ErrNotFound.
func (tx *ReadTx) GetIssueCommentIntent(
	ctx context.Context, instanceID domain.ProposalInstanceID,
) (domain.IssueCommentIntent, error) {
	row := tx.tx.QueryRowContext(ctx, commentIntentSQL, instanceID)
	stored, err := scanCommentIntentRow(row)
	if err != nil {
		return domain.IssueCommentIntent{}, fmt.Errorf("get issue comment intent %q: %w", instanceID, notFoundOr(err))
	}
	intent, err := tx.reconstructCommentIntent(ctx, stored)
	if err != nil {
		return domain.IssueCommentIntent{}, fmt.Errorf("get issue comment intent %q: %w", instanceID, err)
	}
	return intent, nil
}

// OutstandingIssueCommentIntent returns the issue's intent that has no
// terminal outcome, or nil when every intent there is resolved. Every intent
// on the issue must reconstruct for the answer to be nil.
func (tx *ReadTx) OutstandingIssueCommentIntent(
	ctx context.Context, repositoryID int64, issueNumber int,
) (*domain.IssueCommentIntent, error) {
	fail := func(err error) (*domain.IssueCommentIntent, error) {
		return nil, fmt.Errorf("outstanding issue comment intent on issue %d in repository %d: %w",
			issueNumber, repositoryID, err)
	}
	intents, err := tx.issueCommentIntents(ctx, repositoryID, issueNumber)
	if err != nil {
		return fail(err)
	}
	var outstanding *domain.IssueCommentIntent
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

// IssueCommentIssueAmbiguous reports whether any intent on the issue
// resolved as ambiguous, the state in which no later intent there may adopt.
func (tx *ReadTx) IssueCommentIssueAmbiguous(ctx context.Context, repositoryID int64, issueNumber int) (bool, error) {
	intents, err := tx.issueCommentIntents(ctx, repositoryID, issueNumber)
	if err != nil {
		return false, fmt.Errorf("issue comment ambiguity on issue %d in repository %d: %w",
			issueNumber, repositoryID, err)
	}
	for _, intent := range intents {
		if intent.Terminal != nil && intent.Terminal.Outcome == domain.FollowUpFilingAmbiguous {
			return true, nil
		}
	}
	return false, nil
}

// PostedIssueComment returns the ledger row for one comment, or nil when the
// daemon did not post it. A row that does not reconstruct is an error, never
// nil.
func (tx *ReadTx) PostedIssueComment(
	ctx context.Context, repositoryID, commentID int64,
) (*domain.PostedIssueComment, error) {
	posted, err := tx.postedIssueComments(ctx, postedCommentSQL, repositoryID, commentID)
	if err != nil {
		return nil, fmt.Errorf("posted issue comment %d in repository %d: %w", commentID, repositoryID, err)
	}
	if len(posted) == 0 {
		return nil, nil
	}
	return &posted[0], nil
}

// PostedIssueCommentForInstance returns the comment ledgered for one
// proposal instance, or nil when it has none. With no row the answer comes
// from the instance's intent: a ledgered intent whose row is gone fails to
// reconstruct, so that state is an error here too, never nil.
func (tx *ReadTx) PostedIssueCommentForInstance(
	ctx context.Context, instanceID domain.ProposalInstanceID,
) (*domain.PostedIssueComment, error) {
	posted, err := tx.postedIssueComments(ctx, instancePostedCommentSQL, instanceID)
	if err != nil {
		return nil, fmt.Errorf("posted issue comment for instance %q: %w", instanceID, err)
	}
	if len(posted) > 0 {
		return &posted[0], nil
	}
	if _, err := tx.GetIssueCommentIntent(ctx, instanceID); err != nil && !errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("posted issue comment for instance %q: %w", instanceID, err)
	}
	return nil, nil
}

// issueCommentIntents reconstructs every intent the issue's key columns
// name, in opening order.
func (tx *ReadTx) issueCommentIntents(
	ctx context.Context, repositoryID int64, issueNumber int,
) ([]domain.IssueCommentIntent, error) {
	rows, err := tx.tx.QueryContext(ctx, issueCommentIntentsSQL, repositoryID, issueNumber)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var stored []commentIntentRow
	for rows.Next() {
		row, err := scanCommentIntentRow(rows)
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
	intents := make([]domain.IssueCommentIntent, 0, len(stored))
	for _, row := range stored {
		intent, err := tx.reconstructCommentIntent(ctx, row)
		if err != nil {
			return nil, fmt.Errorf("intent %q: %w", row.instanceID, err)
		}
		intents = append(intents, intent)
	}
	return intents, nil
}

// commentIntentRow is one scanned intent row, before its attempts are read
// and before anything in it is trusted.
type commentIntentRow struct {
	instanceID          string
	repositoryID        int64
	issueNumber         int
	openedAt            string
	preDispatchIDs      sql.NullString
	preDispatchBot      sql.NullInt64
	preDispatchRecorded sql.NullString
	outcome             sql.NullString
	refusalReason       sql.NullString
	resolvedAt          sql.NullString
}

func scanCommentIntentRow(row scanner) (commentIntentRow, error) {
	var stored commentIntentRow
	err := row.Scan(&stored.instanceID, &stored.repositoryID, &stored.issueNumber, &stored.openedAt,
		&stored.preDispatchIDs, &stored.preDispatchBot, &stored.preDispatchRecorded,
		&stored.outcome, &stored.refusalReason, &stored.resolvedAt)
	return stored, err
}

// reconstructCommentIntent is the single reconstruction path for an intent:
// rebuild it from its row and attempt rows, validate it as a history the
// rule could have produced, then re-gate it against current rows. The
// instance must still be an approved issue comment whose re-gated repository
// and issue are the intent's, and the posted-comment row must be present
// exactly when the outcome is ledgered.
func (tx *ReadTx) reconstructCommentIntent(
	ctx context.Context, stored commentIntentRow,
) (domain.IssueCommentIntent, error) {
	fail := func(err error) (domain.IssueCommentIntent, error) {
		return domain.IssueCommentIntent{}, err
	}
	inconsistent := func(err error) (domain.IssueCommentIntent, error) {
		return fail(fmt.Errorf("%w: %w", errRowInconsistent, err))
	}
	openedAt, err := parseTime(stored.openedAt)
	if err != nil {
		return inconsistent(err)
	}
	intent := domain.IssueCommentIntent{
		InstanceID:   domain.ProposalInstanceID(stored.instanceID),
		RepositoryID: stored.repositoryID,
		IssueNumber:  stored.issueNumber,
		OpenedAt:     openedAt,
	}
	if intent.PreDispatch, err = decodeCommentPreDispatch(stored); err != nil {
		return inconsistent(err)
	}
	if intent.Terminal, err = decodeFilingTerminal(filingIntentRow{
		outcome: stored.outcome, refusalReason: stored.refusalReason, resolvedAt: stored.resolvedAt,
	}); err != nil {
		return inconsistent(err)
	}
	if intent.Attempts, err = tx.commentAttempts(ctx, intent.InstanceID); err != nil {
		return fail(err)
	}
	if err := intent.Validate(); err != nil {
		return inconsistent(err)
	}
	approved, err := tx.approvedIssueComment(ctx, intent.InstanceID)
	if err != nil {
		return fail(commentBackingError(err))
	}
	if approved.RepositoryID != intent.RepositoryID || approved.IssueNumber != intent.IssueNumber {
		return fail(fmt.Errorf("%w: intent names issue %d in repository %d, its proposal targets issue %d in %d",
			errRowInconsistent, intent.IssueNumber, intent.RepositoryID, approved.IssueNumber, approved.RepositoryID))
	}
	var ledgerRows int
	if err := tx.tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM posted_issue_comments WHERE instance_id = ?`,
		intent.InstanceID).Scan(&ledgerRows); err != nil {
		return fail(err)
	}
	ledgered := intent.Terminal != nil && intent.Terminal.Outcome == domain.FollowUpFilingLedgered
	if ledgered != (ledgerRows == 1) || ledgerRows > 1 {
		return fail(fmt.Errorf("%w: ledgered outcome and posted-comment row disagree", errRowInconsistent))
	}
	return intent, nil
}

// decodeCommentPreDispatch reads the recorded pre-dispatch set. The IDs are
// stored as a canonical JSON array, so a column that does not re-encode to
// itself was not written by the store. The identity is written with the set
// and never trusted from here: the dispatcher compares it with the
// repository's live App identity before every step that relies on it.
func decodeCommentPreDispatch(stored commentIntentRow) (*domain.IssueCommentPreDispatchSet, error) {
	if !stored.preDispatchIDs.Valid && !stored.preDispatchRecorded.Valid && !stored.preDispatchBot.Valid {
		return nil, nil
	}
	if !stored.preDispatchIDs.Valid || !stored.preDispatchRecorded.Valid || !stored.preDispatchBot.Valid {
		return nil, errors.New("pre-dispatch set is half recorded")
	}
	var ids []int64
	if err := json.Unmarshal([]byte(stored.preDispatchIDs.String), &ids); err != nil {
		return nil, fmt.Errorf("pre-dispatch comment ids: %w", err)
	}
	canonical, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	if ids == nil || string(canonical) != stored.preDispatchIDs.String {
		return nil, errors.New("pre-dispatch comment ids are not canonical")
	}
	recordedAt, err := parseTime(stored.preDispatchRecorded.String)
	if err != nil {
		return nil, err
	}
	return &domain.IssueCommentPreDispatchSet{
		CommentIDs: ids, BotUserID: stored.preDispatchBot.Int64, RecordedAt: recordedAt,
	}, nil
}

// commentAttempts reads an intent's attempt rows in ordinal order. It returns
// an empty, non-nil list for an intent with none, the shape the domain type
// requires.
func (tx *ReadTx) commentAttempts(
	ctx context.Context, instanceID domain.ProposalInstanceID,
) ([]domain.IssueCommentAttempt, error) {
	rows, err := tx.tx.QueryContext(ctx, selectCommentAttemptsSQL, instanceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	attempts := []domain.IssueCommentAttempt{}
	for rows.Next() {
		var (
			attempt    domain.IssueCommentAttempt
			startedAt  string
			class      sql.NullString
			commentID  sql.NullInt64
			recordedAt sql.NullString
		)
		if err := rows.Scan(&attempt.Ordinal, &startedAt, &class, &commentID, &recordedAt); err != nil {
			return nil, err
		}
		if attempt.DispatchStartedAt, err = parseTime(startedAt); err != nil {
			return nil, fmt.Errorf("%w: %w", errRowInconsistent, err)
		}
		if class.Valid != recordedAt.Valid || (commentID.Valid && !class.Valid) {
			return nil, fmt.Errorf("%w: attempt %d response is half recorded", errRowInconsistent, attempt.Ordinal)
		}
		if class.Valid {
			response := domain.IssueCommentAttemptResponse{Class: domain.FollowUpFilingResponseClass(class.String)}
			if response.RecordedAt, err = parseTime(recordedAt.String); err != nil {
				return nil, fmt.Errorf("%w: %w", errRowInconsistent, err)
			}
			if commentID.Valid {
				id := commentID.Int64
				response.CommentID = &id
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

// postedIssueComments reconstructs the ledger rows one of this file's
// key-column queries selects.
func (tx *ReadTx) postedIssueComments(
	ctx context.Context, query string, args ...any,
) ([]domain.PostedIssueComment, error) {
	rows, err := tx.tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var decoded []domain.PostedIssueComment
	for rows.Next() {
		posted, err := scanPostedIssueComment(rows)
		if err != nil {
			return nil, err
		}
		decoded = append(decoded, posted)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The backing checks run their own queries, so the cursor is drained first.
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for _, posted := range decoded {
		intent, err := tx.GetIssueCommentIntent(ctx, posted.InstanceID)
		if err != nil {
			return nil, fmt.Errorf("comment %d in repository %d: %w",
				posted.CommentID, posted.RepositoryID, commentBackingError(err))
		}
		if err := posted.BackedBy(intent); err != nil {
			return nil, fmt.Errorf("comment %d in repository %d: %w: %w",
				posted.CommentID, posted.RepositoryID, errRowInconsistent, err)
		}
	}
	return decoded, nil
}

// scanPostedIssueComment decodes and validates a ledger row's body, the
// authority, and cross-checks every key column against it.
func scanPostedIssueComment(row scanner) (domain.PostedIssueComment, error) {
	var (
		repositoryID int64
		commentID    int64
		issueNumber  int
		instanceID   string
		body         string
	)
	if err := row.Scan(&repositoryID, &commentID, &issueNumber, &instanceID, &body); err != nil {
		return domain.PostedIssueComment{}, err
	}
	posted, err := decode[domain.PostedIssueComment]([]byte(body))
	if err != nil {
		return domain.PostedIssueComment{}, fmt.Errorf("%w: %w", errRowInconsistent, err)
	}
	if err := posted.Validate(); err != nil {
		return domain.PostedIssueComment{}, fmt.Errorf("%w: %w", errRowInconsistent, err)
	}
	if posted.RepositoryID != repositoryID || posted.CommentID != commentID ||
		posted.IssueNumber != issueNumber || string(posted.InstanceID) != instanceID {
		return domain.PostedIssueComment{}, errRowInconsistent
	}
	return posted, nil
}

// ApprovedIssueComment is what an approved issue comment instance authorizes,
// re-derived from current rows: the repository and issue it posts on, the
// body, and the subject it originates from. ByPolicy says the policy actor
// approved it (gates.issue_comment off) rather than a person on its card.
type ApprovedIssueComment struct {
	RepositoryID int64
	Repo         string
	IssueNumber  int
	Body         domain.ScreenedIssueText
	Handle       domain.OpaqueSubjectHandle
	ProjectID    domain.ProjectID
	RunID        domain.RunID
	ByPolicy     bool
}

// ApprovedIssueComment re-derives what an approved issue comment instance
// authorizes, for the dispatcher. It is approvedIssueComment exported, so
// the dispatcher never reads the target from the decoded body alone.
func (tx *ReadTx) ApprovedIssueComment(
	ctx context.Context, instanceID domain.ProposalInstanceID,
) (ApprovedIssueComment, error) {
	return tx.approvedIssueComment(ctx, instanceID)
}

// approvedIssueComment re-derives, from current rows, what an approved issue
// comment instance authorizes. GetProposalInstance re-runs the registry gate
// and the comment gate (gateIssueComment), which re-reads the duplicate
// search record and re-derives the issue from it and the repository from the
// project row. The approval comes from exactly one recorder: a human approve,
// re-tied to its authoring command and the rendered digest that command
// decided, as RecordProposalDecision checked on write; or a policy approval,
// re-checked against the instance and the run's resolved policy
// (issueCommentPolicyApproval). Both recorded is corruption and fails closed.
// ErrIssueCommentNotApproved covers an instance of another effect kind, an
// undecided one, and a declined one.
func (tx *ReadTx) approvedIssueComment(
	ctx context.Context, instanceID domain.ProposalInstanceID,
) (ApprovedIssueComment, error) {
	fail := func(err error) (ApprovedIssueComment, error) {
		return ApprovedIssueComment{}, err
	}
	instance, err := tx.GetProposalInstance(ctx, instanceID)
	if err != nil {
		return fail(err)
	}
	comment := instance.Proposal.CommentProposal
	if instance.Proposal.Kind != domain.EffectIssueComment || comment == nil {
		return fail(fmt.Errorf("%w: instance is a %q proposal", domain.ErrIssueCommentNotApproved, instance.Proposal.Kind))
	}
	declaration, policy, err := tx.ResolveProposalSubject(ctx, comment.SubjectHandle)
	if err != nil {
		return fail(err)
	}
	project, err := tx.subjectProject(ctx, declaration)
	if err != nil {
		return fail(err)
	}
	decisions, err := tx.countProposalDecisions(ctx, instanceID)
	if err != nil {
		return fail(err)
	}
	policies, err := tx.countIssueCommentPolicyApprovals(ctx, instanceID)
	if err != nil {
		return fail(err)
	}
	switch {
	case decisions > 0 && policies > 0:
		return fail(fmt.Errorf("%w: both a decision and a policy approval are recorded", errRowInconsistent))
	case policies > 0:
		err = tx.issueCommentPolicyApproval(ctx, instance, policy)
	default:
		err = tx.humanIssueCommentApproval(ctx, instanceID)
	}
	if err != nil {
		return fail(err)
	}
	// gateIssueComment, run inside GetProposalInstance, proved the comment's
	// repository id is the project's and its issue is the search record's
	// duplicate, so both are current determinations, not decoded claims.
	return ApprovedIssueComment{
		RepositoryID: project.RepositoryID, Repo: project.Repo, IssueNumber: comment.IssueNumber,
		Body: comment.Body, Handle: comment.SubjectHandle,
		ProjectID: declaration.ProjectID, RunID: declaration.RunID, ByPolicy: policies > 0,
	}, nil
}

// humanIssueCommentApproval re-ties a comment's recorded decision to its
// authoring command and the rendered digest that command decided. An
// undecided or declined comment is ErrIssueCommentNotApproved.
func (tx *ReadTx) humanIssueCommentApproval(ctx context.Context, instanceID domain.ProposalInstanceID) error {
	var commandID, action string
	var selectedDigest sql.NullString
	err := tx.tx.QueryRowContext(ctx, `SELECT command_id, action, selected_digest
		FROM effect_proposal_decisions WHERE instance_id = ?`,
		instanceID).Scan(&commandID, &action, &selectedDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: no decision is recorded", domain.ErrIssueCommentNotApproved)
	}
	if err != nil {
		return err
	}
	if domain.Action(action) != domain.ActionApprove {
		return fmt.Errorf("%w: the recorded decision is %q", domain.ErrIssueCommentNotApproved, action)
	}
	command, err := tx.GetCommand(ctx, commandID)
	if err != nil {
		return err
	}
	boundInstance, boundDigest, _, err := tx.proposalItemBinding(ctx, command.ItemID)
	if err != nil {
		return err
	}
	if command.Action != domain.ActionApprove || boundInstance != instanceID ||
		!selectedDigest.Valid || domain.Digest(selectedDigest.String) != boundDigest {
		return fmt.Errorf("%w: approval does not bind its command and digest", errRowInconsistent)
	}
	return nil
}

// commentBackingError keeps a missing or unapproved backing row from reading
// as an absent ledger fact, as filingBackingError does for filings.
func commentBackingError(err error) error {
	if errors.Is(err, ErrNotFound) || errors.Is(err, domain.ErrIssueCommentNotApproved) {
		return fmt.Errorf("%w: %v", errRowInconsistent, err) //nolint:errorlint // deliberately unwrapped, see above
	}
	return err
}

// RunPostsMayHaveCreated counts the instances whose subject is handle, other
// than exclude, whose follow-up filing or issue comment intent may have
// created something on GitHub. The filing per-run cap counts filings and
// comments together (issue #3147), so the filer and the comment dispatcher
// both read it. Each intent is read through its gated getter, so one that no
// longer reads fails the count: a cap that cannot be evaluated does not pass.
func (tx *ReadTx) RunPostsMayHaveCreated(
	ctx context.Context, handle domain.OpaqueSubjectHandle, exclude domain.ProposalInstanceID,
) (int, error) {
	fail := func(err error) (int, error) {
		return 0, fmt.Errorf("count posts for subject %q: %w", handle, err)
	}
	type candidate struct {
		id   domain.ProposalInstanceID
		kind domain.EffectKind
	}
	rows, err := tx.tx.QueryContext(ctx, `
SELECT i.instance_id, i.effect_kind FROM effect_proposal_instances i
WHERE i.subject_handle = ? AND i.instance_id <> ? AND (
    (i.effect_kind = ? AND EXISTS (SELECT 1 FROM follow_up_filing_intents f WHERE f.instance_id = i.instance_id))
 OR (i.effect_kind = ? AND EXISTS (SELECT 1 FROM issue_comment_intents c WHERE c.instance_id = i.instance_id)))
ORDER BY i.instance_id`, handle, exclude, domain.EffectFollowUpFiling, domain.EffectIssueComment)
	if err != nil {
		return fail(err)
	}
	// Collect before reading each intent, on the store's one connection.
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.kind); err != nil {
			_ = rows.Close()
			return fail(err)
		}
		candidates = append(candidates, c)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fail(err)
	}
	count := 0
	for _, c := range candidates {
		var created bool
		switch c.kind {
		case domain.EffectFollowUpFiling:
			intent, err := tx.GetFollowUpFilingIntent(ctx, c.id)
			if err != nil {
				return fail(err)
			}
			created = intent.MayHaveCreated()
		case domain.EffectIssueComment:
			intent, err := tx.GetIssueCommentIntent(ctx, c.id)
			if err != nil {
				return fail(err)
			}
			created = intent.MayHaveCreated()
		default:
			return fail(fmt.Errorf("%w: instance %q is a %q proposal", errRowInconsistent, c.id, c.kind))
		}
		if created {
			count++
		}
	}
	return count, nil
}
