package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const (
	recordUnattendedOperationTransitionSQL = `
INSERT INTO unattended_operation_transitions (state, command_id, reason, occurred_at)
VALUES (?, ?, ?, ?)`
	latestUnattendedOperationTransitionSQL = `
SELECT state, command_id, reason, occurred_at
FROM unattended_operation_transitions ORDER BY id DESC LIMIT 1`
	listOpenAttentionItemsByTypeSQL = `
SELECT id, project_id, conversation_id, item_type, status, entity_version, as_of_revision, body
FROM attention_items WHERE item_type = ? AND status = 'open' ORDER BY id`
	// The lookup columns can only fail open by omission: a row whose column
	// diverges from its canonical body is invisible to the WHERE clause
	// above, so the per-row cross-check in scanAttentionItemSnapshot never
	// sees it. This one SQL pass proves the column view agrees with the body
	// view for every row (COALESCE mirrors the 0017 backfill's treatment of
	// a body the extraction cannot read), so an omitted mismatch fails the
	// query instead of silently shrinking the blocking set.
	attentionColumnDivergenceSQL = `
SELECT COUNT(*) FROM attention_items
WHERE item_type <> COALESCE(json_extract(body, '$.type'), '')
   OR status <> COALESCE(json_extract(body, '$.status'), '')`
)

// RecordUnattendedOperationTransition appends one operator stop/resume
// decision (plan §4 stop_unattended; issue #319). Append-only by design: the
// log is the audit trail of operator intent, the latest row is the operating
// state, and a repeated state (a second stop while stopped) is a real
// recorded decision, not a conflict. Named Record, not Put: rows are never
// updated. Puts validate before writing: the transition must be authorized
// by the accepted command it names (requireTransitionCommand), the same
// binding reconstruction re-derives, so a row this boundary would refuse to
// write cannot be told apart from tampering when read back.
func (tx *InternalTx) RecordUnattendedOperationTransition(
	ctx context.Context, transition domain.UnattendedOperationTransition,
) error {
	if err := transition.Validate(); err != nil {
		return fmt.Errorf("record unattended operation transition: %w", err)
	}
	if err := tx.requireTransitionCommand(ctx, transition); err != nil {
		return fmt.Errorf("record unattended operation transition: %w", err)
	}
	if _, err := tx.tx.ExecContext(ctx, recordUnattendedOperationTransitionSQL,
		transition.State, *transition.CommandID, transition.Reason,
		formatTime(transition.OccurredAt)); err != nil {
		return fmt.Errorf("record unattended operation transition: %w", err)
	}
	return nil
}

// requireTransitionCommand re-derives a transition's authority from the
// immutable command it names: the command must reconstruct (running its own
// gates) and its accepted action must authorize the transition's state. The
// stored state is a decoded trust bit — "resumed" lifts a safety gate — so
// it is never trusted on its own: a single-column tamper flipping stopped to
// resumed fails closed here because the referenced command still says
// stop_unattended.
func (tx *ReadTx) requireTransitionCommand(
	ctx context.Context, transition domain.UnattendedOperationTransition,
) error {
	command, _, err := tx.GetCommandSnapshot(ctx, *transition.CommandID)
	if err != nil {
		return fmt.Errorf("transition command %q: %w", *transition.CommandID, err)
	}
	authorizing, ok := transition.State.AuthorizingAction()
	if !ok {
		return fmt.Errorf("unattended operation state %q: %w",
			transition.State, domain.ErrInvalidUnattendedOperationState)
	}
	if command.Action != authorizing {
		return fmt.Errorf("transition %q backed by command %q with action %q: %w",
			transition.State, command.CommandID, command.Action,
			domain.ErrTransitionCommandMismatch)
	}
	return nil
}

// LatestUnattendedOperationTransition reconstructs the current operating
// state: the newest appended transition, with presence reported separately
// because an empty log is the legitimate "never stopped" state, not an error
// (the LookupExecutionAdmission shape).
func (tx *ReadTx) LatestUnattendedOperationTransition(
	ctx context.Context,
) (domain.UnattendedOperationTransition, bool, error) {
	var (
		state      string
		commandID  sql.NullString
		reason     string
		occurredAt string
	)
	err := tx.tx.QueryRowContext(ctx, latestUnattendedOperationTransitionSQL).
		Scan(&state, &commandID, &reason, &occurredAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return domain.UnattendedOperationTransition{}, false, nil
	case err != nil:
		return domain.UnattendedOperationTransition{}, false,
			fmt.Errorf("latest unattended operation transition: %w", err)
	}
	at, err := time.Parse(time.RFC3339Nano, occurredAt)
	if err != nil {
		return domain.UnattendedOperationTransition{}, false,
			fmt.Errorf("latest unattended operation transition occurred_at %q: %w", occurredAt, err)
	}
	transition := domain.UnattendedOperationTransition{
		State:      domain.UnattendedOperationState(state),
		Reason:     reason,
		OccurredAt: at.UTC(),
	}
	if commandID.Valid {
		transition.CommandID = &commandID.String
	}
	// Gets validate after reading: a row the current vocabulary cannot
	// express (an unknown state written by tampering or a future schema, or
	// one naming no command) fails closed instead of reconstructing as "not
	// stopped", and the state is re-derived from the immutable command the
	// row names (requireTransitionCommand) rather than trusted from the
	// column — a tampered resumed row does not lift the stop while its
	// command still says stop_unattended.
	if err := transition.Validate(); err != nil {
		return domain.UnattendedOperationTransition{}, false,
			fmt.Errorf("latest unattended operation transition: %w", err)
	}
	if err := tx.requireTransitionCommand(ctx, transition); err != nil {
		return domain.UnattendedOperationTransition{}, false,
			fmt.Errorf("latest unattended operation transition: %w", err)
	}
	return transition, true, nil
}

// RequireUnattendedAdmissible is the operating-state half of §5.7's
// unattended conditions, checked in the admitting transaction (issue #321):
// no operator stop in force, and no blocking system_health item. It is a
// recording-time precondition on new unattended operation — consulted when an
// admission is recorded and when a stored admission is about to dispatch —
// not part of a record's meaning, so reconstruction (scanExecutionAdmission)
// deliberately does not re-run it: an operator stop must not make recorded
// history unreadable (lists, exports, backup closure), it must stop what
// happens next.
//
// The blocking rule is typed, never an item-ID convention: an open
// system_health item blocks unless it carries a BlockingSupersession whose
// condition holds against this transaction's live policy (plan §4: "a
// validated configuration supersedes it"). The stored condition is a claim,
// not a verdict — Supersedes re-derives it, so clearing or retargeting the
// waiver re-blocks every notice it covered with no write. Matched rows are
// fully reconstructed and re-gated (scanAttentionItemSnapshot) before they
// can block or be skipped; the extracted columns only select candidates.
func (tx *ReadTx) RequireUnattendedAdmissible(
	ctx context.Context, admission domain.ExecutionAdmission,
) error {
	if admission.OperatingMode != domain.ModeUnattended {
		return nil
	}
	if err := tx.RequireUnattendedOperationOpen(ctx); err != nil {
		return fmt.Errorf("admission %q: %w", admission.InvocationID, err)
	}
	return nil
}

// RequireUnattendedOperationOpen is the whole operating-state predicate with
// no admission in hand: no operator stop in force, and no blocking open
// system_health item. It exists as one function so every consumer gates on
// both conditions or neither — the engine's per-pass check calls it for a
// dispatch whose operating mode is unknowable (no admission configuration),
// where "unknown fails closed" must mean the full gate, not whichever half
// was remembered.
func (tx *ReadTx) RequireUnattendedOperationOpen(ctx context.Context) error {
	latest, found, err := tx.LatestUnattendedOperationTransition(ctx)
	if err != nil {
		return err
	}
	if found && latest.State == domain.UnattendedStopped {
		return domain.ErrUnattendedOperationStopped
	}
	items, err := tx.ListOpenAttentionItems(ctx, domain.AttentionSystemHealth)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.BlockingSupersession == nil {
			return fmt.Errorf("item %q: %w", item.ID, domain.ErrBlockingSystemHealth)
		}
		if err := item.BlockingSupersession.Supersedes(tx.admissionPolicy); err != nil {
			return fmt.Errorf("item %q: %w: %w", item.ID, domain.ErrBlockingSystemHealth, err)
		}
	}
	return nil
}

// ListOpenAttentionItems returns every open item of one type, in id order,
// selected by the extracted lookup columns and fully reconstructed through
// the shared scan (decode, cross-check, evidence re-gate), so a caller acting
// on the result acts on validated rows, never on column claims. Selection is
// guarded against omission too: the divergence count proves no row's columns
// disagree with its body, because a tampered column would otherwise hide an
// open blocking item from the WHERE clause with no scan left to refuse it.
// Consumers: the unattended admission gate above, and signet's stop
// transaction, which must find an existing resume-offering notice before
// raising another.
func (tx *ReadTx) ListOpenAttentionItems(
	ctx context.Context, itemType domain.AttentionType,
) ([]domain.AttentionItem, error) {
	var divergent int
	if err := tx.tx.QueryRowContext(ctx, attentionColumnDivergenceSQL).Scan(&divergent); err != nil {
		return nil, fmt.Errorf("list open %q items: column integrity: %w", itemType, err)
	}
	if divergent > 0 {
		return nil, fmt.Errorf("list open %q items: %d row(s) whose lookup columns diverge from their bodies: %w",
			itemType, divergent, errRowInconsistent)
	}
	rows, err := tx.tx.QueryContext(ctx, listOpenAttentionItemsByTypeSQL, itemType)
	if err != nil {
		return nil, fmt.Errorf("list open %q items: %w", itemType, err)
	}
	defer func() { _ = rows.Close() }()
	var items []domain.AttentionItem
	for rows.Next() {
		item, _, err := tx.scanAttentionItemSnapshot(rows)
		if err != nil {
			return nil, fmt.Errorf("list open %q items: %w", itemType, err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list open %q items: %w", itemType, err)
	}
	return items, nil
}
