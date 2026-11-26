package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Operator messages (migration 0151) are the messages an operator sent to a
// running cordoned writer, each recorded before the daemon offers it to the
// run and settled once (#3632). The row is the command's only record: a
// send_writer_message command holds no receipt, and a replay of its
// command_id reads the row. The rows are ordinary state (Tier B): each read
// re-runs domain.OperatorMessage.Validate, and a bad row fails the read.

const operatorMessageColumns = `command_id, sender, run_id, invocation_id, ordinal, text, recorded_at,
state, settled_at, refusal`

// RecordOperatorMessage records request as its invocation's next message,
// not yet settled, and returns the record. A command_id any command kind
// already holds, this one included, is ErrImmutableConflict: the caller
// replays a recorded message through GetOperatorMessage first.
func (tx *WriteTx) RecordOperatorMessage(
	ctx context.Context, request domain.OperatorMessageRequest, now time.Time,
) (domain.OperatorMessage, error) {
	if err := request.Validate(); err != nil {
		return domain.OperatorMessage{}, fmt.Errorf("record operator message: %w", err)
	}
	if err := tx.RequireCommandIDFree(ctx, request.CommandID); err != nil {
		return domain.OperatorMessage{}, fmt.Errorf("record operator message: %w", err)
	}
	var last int64
	if err := tx.tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(ordinal), 0) FROM operator_messages WHERE invocation_id = ?`, request.InvocationID).
		Scan(&last); err != nil {
		return domain.OperatorMessage{}, fmt.Errorf("record operator message %q: %w", request.CommandID, err)
	}
	m, err := domain.NewOperatorMessage(request, last+1, now)
	if err != nil {
		return domain.OperatorMessage{}, fmt.Errorf("record operator message: %w", err)
	}
	if _, err := tx.tx.ExecContext(ctx,
		`INSERT INTO operator_messages (`+operatorMessageColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL)`,
		m.CommandID, m.Sender, m.RunID, m.InvocationID, m.Ordinal, m.Text, formatTime(m.RecordedAt), m.State,
	); err != nil {
		return domain.OperatorMessage{}, fmt.Errorf("record operator message %q: %w", m.CommandID, err)
	}
	return m, nil
}

// SettleOperatorMessage settles a recorded message as delivered (refusal
// nil) or refused, and returns the settled record. A message already
// settled is domain.ErrOperatorMessageSettled, and none recorded is
// ErrNotFound.
func (tx *InternalTx) SettleOperatorMessage(
	ctx context.Context, commandID string, refusal *domain.OperatorMessageRefusal, now time.Time,
) (domain.OperatorMessage, error) {
	m, err := tx.GetOperatorMessage(ctx, commandID)
	if err != nil {
		return domain.OperatorMessage{}, err
	}
	settled, err := m.Settle(refusal, now)
	if err != nil {
		return domain.OperatorMessage{}, fmt.Errorf("settle operator message: %w", err)
	}
	result, err := tx.tx.ExecContext(ctx,
		`UPDATE operator_messages SET state = ?, settled_at = ?, refusal = ? WHERE command_id = ? AND state = ?`,
		settled.State, formatTimePtr(settled.SettledAt), settled.Refusal, commandID, domain.OperatorMessageRecorded)
	if err != nil {
		return domain.OperatorMessage{}, fmt.Errorf("settle operator message %q: %w", commandID, err)
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return domain.OperatorMessage{}, fmt.Errorf("settle operator message %q: updated %d rows: %w", commandID, n, err)
	}
	return settled, nil
}

// GetOperatorMessage reads the message recorded under commandID:
// ErrNotFound when none is.
func (tx *ReadTx) GetOperatorMessage(ctx context.Context, commandID string) (domain.OperatorMessage, error) {
	m, err := scanOperatorMessage(tx.tx.QueryRowContext(ctx,
		`SELECT `+operatorMessageColumns+` FROM operator_messages WHERE command_id = ?`, commandID))
	if err != nil {
		return domain.OperatorMessage{}, fmt.Errorf("get operator message %q: %w", commandID, notFoundOr(err))
	}
	return m, nil
}

// OperatorMessages lists an invocation's messages in ordinal order.
func (tx *ReadTx) OperatorMessages(ctx context.Context, id domain.InvocationID) ([]domain.OperatorMessage, error) {
	return tx.listOperatorMessages(ctx,
		`SELECT `+operatorMessageColumns+` FROM operator_messages WHERE invocation_id = ? ORDER BY ordinal`, id)
}

// UnsettledOperatorMessages lists every message still recorded, by
// invocation and then ordinal.
func (tx *ReadTx) UnsettledOperatorMessages(ctx context.Context) ([]domain.OperatorMessage, error) {
	return tx.listOperatorMessages(ctx,
		`SELECT `+operatorMessageColumns+` FROM operator_messages WHERE state = ? ORDER BY invocation_id, ordinal`,
		domain.OperatorMessageRecorded)
}

func (tx *ReadTx) listOperatorMessages(ctx context.Context, query string, arg any) ([]domain.OperatorMessage, error) {
	rows, err := tx.tx.QueryContext(ctx, query, arg)
	if err != nil {
		return nil, fmt.Errorf("list operator messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []domain.OperatorMessage
	for rows.Next() {
		m, err := scanOperatorMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("list operator messages: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list operator messages: %w", err)
	}
	return out, nil
}

func scanOperatorMessage(row interface{ Scan(...any) error }) (domain.OperatorMessage, error) {
	var (
		m                     domain.OperatorMessage
		recordedAt            string
		settledAt, refusalCol sql.NullString
	)
	if err := row.Scan(&m.CommandID, &m.Sender, &m.RunID, &m.InvocationID, &m.Ordinal, &m.Text, &recordedAt,
		&m.State, &settledAt, &refusalCol); err != nil {
		return domain.OperatorMessage{}, err
	}
	var err error
	if m.RecordedAt, err = parseTime(recordedAt); err != nil {
		return domain.OperatorMessage{}, fmt.Errorf("operator message %q recorded_at: %w", m.CommandID, err)
	}
	if settledAt.Valid {
		at, err := parseTime(settledAt.String)
		if err != nil {
			return domain.OperatorMessage{}, fmt.Errorf("operator message %q settled_at: %w", m.CommandID, err)
		}
		m.SettledAt = &at
	}
	if refusalCol.Valid {
		refusal := domain.OperatorMessageRefusal(refusalCol.String)
		m.Refusal = &refusal
	}
	if err := m.Validate(); err != nil {
		return domain.OperatorMessage{}, err
	}
	return m, nil
}
