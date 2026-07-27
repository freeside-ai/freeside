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
)

// RecordUnattendedOperationTransition appends one operator stop/resume
// decision (plan §4 stop_unattended; issue #319). Append-only by design: the
// log is the audit trail of operator intent, the latest row is the operating
// state, and a repeated state (a second stop while stopped) is a real
// recorded decision, not a conflict. Named Record, not Put: rows are never
// updated.
func (tx *InternalTx) RecordUnattendedOperationTransition(
	ctx context.Context, transition domain.UnattendedOperationTransition,
) error {
	if err := transition.Validate(); err != nil {
		return fmt.Errorf("record unattended operation transition: %w", err)
	}
	var command any
	if transition.CommandID != nil {
		command = *transition.CommandID
	}
	if _, err := tx.tx.ExecContext(ctx, recordUnattendedOperationTransitionSQL,
		transition.State, command, transition.Reason,
		formatTime(transition.OccurredAt)); err != nil {
		return fmt.Errorf("record unattended operation transition: %w", err)
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
	// express (an unknown state written by tampering or a future schema)
	// fails closed instead of reconstructing as "not stopped".
	if err := transition.Validate(); err != nil {
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
	latest, found, err := tx.LatestUnattendedOperationTransition(ctx)
	if err != nil {
		return fmt.Errorf("admission %q: %w", admission.InvocationID, err)
	}
	if found && latest.State == domain.UnattendedStopped {
		return fmt.Errorf("admission %q: %w",
			admission.InvocationID, domain.ErrUnattendedOperationStopped)
	}
	rows, err := tx.tx.QueryContext(ctx, listOpenAttentionItemsByTypeSQL, domain.AttentionSystemHealth)
	if err != nil {
		return fmt.Errorf("admission %q: open system_health items: %w", admission.InvocationID, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		item, _, err := tx.scanAttentionItemSnapshot(rows)
		if err != nil {
			return fmt.Errorf("admission %q: open system_health items: %w", admission.InvocationID, err)
		}
		if item.BlockingSupersession == nil {
			return fmt.Errorf("admission %q: item %q: %w",
				admission.InvocationID, item.ID, domain.ErrBlockingSystemHealth)
		}
		if err := item.BlockingSupersession.Supersedes(tx.admissionPolicy); err != nil {
			return fmt.Errorf("admission %q: item %q: %w: %w",
				admission.InvocationID, item.ID, domain.ErrBlockingSystemHealth, err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("admission %q: open system_health items: %w", admission.InvocationID, err)
	}
	return nil
}
