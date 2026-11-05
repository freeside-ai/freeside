package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Alternate-agent retry records (migration 0125; plan §5.4 admission step 5)
// are immutable: a row is one operator choice and nothing updates it. The
// body is the authority; task_id, command_id, and recorded_at are copies a
// read checks against it, and the id is re-derived from the decoded body, so
// a row edited in place fails closed as a corrupt row.

const alternateAgentRetryColumns = `id, task_id, command_id, recorded_at, body`

// RecordAlternateAgentRetry stores an operator's choice in the caller's
// transaction. The record is validated first, so a record whose id does not
// address its content never reaches the table. Storing the same record again
// is an idempotent replay; a second record for one command is refused by the
// table's unique command_id.
func (tx *WriteTx) RecordAlternateAgentRetry(ctx context.Context, record domain.AlternateAgentRetry) error {
	if err := record.Validate(); err != nil {
		return fmt.Errorf("record alternate agent retry: %w", err)
	}
	existing, err := tx.GetAlternateAgentRetry(ctx, record.ID)
	if err == nil {
		if reflect.DeepEqual(existing, record) {
			return nil
		}
		return fmt.Errorf("record alternate agent retry %s: %w", record.ID, ErrImmutableConflict)
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	body, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("record alternate agent retry %s: %w", record.ID, err)
	}
	if _, err := tx.tx.ExecContext(ctx, `INSERT INTO alternate_agent_retries (`+alternateAgentRetryColumns+`)
		VALUES (?, ?, ?, ?, ?)`,
		record.ID, record.TaskID, record.CommandID, formatTime(record.RecordedAt), string(body)); err != nil {
		return fmt.Errorf("record alternate agent retry %s: %w", record.ID, err)
	}
	return nil
}

// GetAlternateAgentRetry returns the record with the given id.
func (tx *ReadTx) GetAlternateAgentRetry(ctx context.Context, id domain.Digest) (domain.AlternateAgentRetry, error) {
	record, err := scanAlternateAgentRetry(tx.tx.QueryRowContext(ctx,
		`SELECT `+alternateAgentRetryColumns+` FROM alternate_agent_retries WHERE id = ?`, id))
	if err != nil {
		return domain.AlternateAgentRetry{}, fmt.Errorf("alternate agent retry %q: %w", id, notFoundOr(err))
	}
	if record.ID != id {
		return domain.AlternateAgentRetry{}, fmt.Errorf("alternate agent retry %q: %w", id, errRowInconsistent)
	}
	return record, nil
}

// GetAlternateAgentRetryByCommand returns the record the command made: the
// engine finds a card retry's record from its decision this way.
func (tx *ReadTx) GetAlternateAgentRetryByCommand(ctx context.Context, commandID string) (domain.AlternateAgentRetry, error) {
	record, err := scanAlternateAgentRetry(tx.tx.QueryRowContext(ctx,
		`SELECT `+alternateAgentRetryColumns+` FROM alternate_agent_retries WHERE command_id = ?`, commandID))
	if err != nil {
		return domain.AlternateAgentRetry{}, fmt.Errorf("alternate agent retry for command %q: %w", commandID, notFoundOr(err))
	}
	if record.CommandID != commandID {
		return domain.AlternateAgentRetry{}, fmt.Errorf("alternate agent retry for command %q: %w", commandID, errRowInconsistent)
	}
	return record, nil
}

func scanAlternateAgentRetry(row scanner) (domain.AlternateAgentRetry, error) {
	var id, taskID, commandID, recordedAt, body string
	if err := row.Scan(&id, &taskID, &commandID, &recordedAt, &body); err != nil {
		return domain.AlternateAgentRetry{}, err
	}
	var record domain.AlternateAgentRetry
	if err := json.Unmarshal([]byte(body), &record); err != nil {
		return domain.AlternateAgentRetry{}, errRowInconsistent
	}
	if record.Validate() != nil || string(record.ID) != id || string(record.TaskID) != taskID ||
		record.CommandID != commandID || formatTime(record.RecordedAt) != recordedAt {
		return domain.AlternateAgentRetry{}, errRowInconsistent
	}
	return record, nil
}

// RecordAlternateAgentMoveLines records the lines a parked task's move sets,
// in the caller's transaction, and returns them. The record must already be
// stored in this transaction and be a parked move; every input must be an
// alternate_agent_move line of the record's task that cites it, and one must
// set the record's role to its agent. Unlike a router line, a moved line
// supersedes the role's current version, whatever set it.
//
// Like RecordTaskLineChange, this advances the task's own version, so sync
// serves the new task_lines.
func (tx *WriteTx) RecordAlternateAgentMoveLines(
	ctx context.Context, record domain.AlternateAgentRetry, inputs []domain.TaskLineInput,
) ([]domain.TaskLine, error) {
	stored, err := tx.GetAlternateAgentRetry(ctx, record.ID)
	if err != nil {
		return nil, fmt.Errorf("record alternate agent move lines: %w", err)
	}
	if !reflect.DeepEqual(stored, record) || !record.ParkedMove() {
		return nil, fmt.Errorf("record alternate agent move lines %s: not the stored parked move: %w",
			record.ID, domain.ErrTaskLineInconsistent)
	}
	if !slices.ContainsFunc(inputs, func(in domain.TaskLineInput) bool {
		return in.Role == record.Role && in.Agent == record.Agent && !in.Cleared
	}) {
		return nil, fmt.Errorf("record alternate agent move lines %s: no line moves %s to %s: %w",
			record.ID, record.Role, record.Agent, domain.ErrTaskLineInconsistent)
	}
	lines := make([]domain.TaskLine, 0, len(inputs))
	for _, in := range inputs {
		if in.Source != domain.TaskLineSourceAlternateAgentMove || in.TaskID != record.TaskID ||
			in.SetBy != string(record.ID) {
			return nil, fmt.Errorf("record alternate agent move lines %s: a %s line of task %q set by %q: %w",
				record.ID, in.Source, in.TaskID, in.SetBy, domain.ErrTaskLineInconsistent)
		}
		line, err := tx.AppendTaskLine(ctx, in, record.RecordedAt)
		if err != nil {
			return nil, err
		}
		lines = append(lines, line)
	}
	task, err := tx.GetTask(ctx, record.TaskID)
	if err != nil {
		return nil, fmt.Errorf("record alternate agent move lines %s: %w", record.ID, err)
	}
	if err := tx.updateTask(ctx, task); err != nil {
		return nil, fmt.Errorf("record alternate agent move lines %s: %w", record.ID, err)
	}
	return lines, nil
}
