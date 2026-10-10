package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Task lines (migration 0095; plan §5.4, Admitted Agents) are append-only: a
// row is one line version and no statement here updates one. A change is the
// next version for its task and role.

const taskLineColumns = `task_id, role, version, id, agent, predecessor_id, source, set_by, set_at`

// AppendTaskLine records the next version of a task's line for one role and
// returns it. The version and the predecessor are assigned here, from the
// role's last version read in this transaction, so a caller cannot choose
// them and cannot name a line that is not the one it supersedes. Version 1
// has no predecessor.
//
// Appending is not idempotent: every call is a new version. A caller
// replaying a recorded command must not call it again (the submission paths
// return their recorded result before reaching it).
//
// The role, the agent name's shape, and the source are refused by
// domain.TaskLine.Validate before any row is written, so a line no operator
// path set never reaches the table.
func (tx *WriteTx) AppendTaskLine(ctx context.Context, in domain.TaskLineInput, setAt time.Time) (domain.TaskLine, error) {
	// The role's whole chain is read and checked, not only its last row, so
	// a chain that already fails a read is never extended.
	chain, err := tx.queryTaskLines(ctx, `SELECT `+taskLineColumns+` FROM task_lines
		WHERE task_id = ? AND role = ? ORDER BY version`, in.TaskID, in.Role)
	if err == nil {
		err = validateTaskLineChains(chain)
	}
	if err != nil {
		return domain.TaskLine{}, fmt.Errorf("append task line %q %q: %w", in.TaskID, in.Role, err)
	}
	version := 1
	var predecessor *domain.Digest
	if len(chain) > 0 {
		last := chain[len(chain)-1]
		version = last.Version + 1
		predecessor = &last.ID
	}
	line, err := domain.NewTaskLine(in, version, predecessor, setAt)
	if err != nil {
		return domain.TaskLine{}, fmt.Errorf("append task line %q %q: %w", in.TaskID, in.Role, err)
	}
	var predecessorID any
	if line.PredecessorID != nil {
		predecessorID = string(*line.PredecessorID)
	}
	if _, err := tx.tx.ExecContext(ctx, `INSERT INTO task_lines (`+taskLineColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		line.TaskID, line.Role, line.Version, line.ID, line.Agent, predecessorID,
		line.Source, line.SetBy, formatTime(line.SetAt)); err != nil {
		return domain.TaskLine{}, fmt.Errorf("append task line %q %q: %w", in.TaskID, in.Role, err)
	}
	return line, nil
}

// TaskLines returns every line version recorded for the task, ordered by
// role and then version. A task with no lines returns none.
func (tx *ReadTx) TaskLines(ctx context.Context, taskID domain.TaskID) ([]domain.TaskLine, error) {
	lines, err := tx.queryTaskLines(ctx, `SELECT `+taskLineColumns+` FROM task_lines
		WHERE task_id = ? ORDER BY role, version`, taskID)
	if err != nil {
		return nil, fmt.Errorf("task lines %q: %w", taskID, err)
	}
	if err := validateTaskLineChains(lines); err != nil {
		return nil, fmt.Errorf("task lines %q: %w", taskID, err)
	}
	return lines, nil
}

// CurrentTaskLines returns each role's latest line for the task: the lines
// an admission reads. A role absent from the map has no line and stays on
// the lineup. It reads every version, so a chain with an edited row or a
// missing earlier version fails the read instead of serving its head.
func (tx *ReadTx) CurrentTaskLines(ctx context.Context, taskID domain.TaskID) (map[domain.RoleName]domain.TaskLine, error) {
	lines, err := tx.TaskLines(ctx, taskID)
	if err != nil {
		return nil, err
	}
	current := make(map[domain.RoleName]domain.TaskLine, len(domain.TaskLineRoles))
	for _, line := range lines {
		// Ordered by version within a role, so the last one seen is current.
		current[line.Role] = line
	}
	return current, nil
}

// GetTaskLine returns the line version with the given id: the read that
// resolves the line an admission cites. It reads the version's whole chain
// for its task and role and checks it as CurrentTaskLines does, so a cited
// line that sits in a broken chain fails here too.
func (tx *ReadTx) GetTaskLine(ctx context.Context, id domain.Digest) (domain.TaskLine, error) {
	var (
		taskID domain.TaskID
		role   domain.RoleName
	)
	if err := tx.tx.QueryRowContext(ctx, `SELECT task_id, role FROM task_lines WHERE id = ?`, id).
		Scan(&taskID, &role); err != nil {
		return domain.TaskLine{}, fmt.Errorf("task line %q: %w", id, notFoundOr(err))
	}
	chain, err := tx.queryTaskLines(ctx, `SELECT `+taskLineColumns+` FROM task_lines
		WHERE task_id = ? AND role = ? ORDER BY version`, taskID, role)
	if err == nil {
		err = validateTaskLineChains(chain)
	}
	if err != nil {
		return domain.TaskLine{}, fmt.Errorf("task line %q: %w", id, err)
	}
	for _, line := range chain {
		if line.ID == id {
			return line, nil
		}
	}
	// The id column found the row, and no decoded version resolves to it.
	return domain.TaskLine{}, fmt.Errorf("task line %q: %w", id, errRowInconsistent)
}

// queryTaskLines decodes task-line rows and re-runs the record's validation
// on each: the row is rebuilt from its columns and must resolve to its own
// id, so an edited column fails closed as a corrupt row.
func (tx *ReadTx) queryTaskLines(ctx context.Context, query string, args ...any) ([]domain.TaskLine, error) {
	rows, err := tx.tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var lines []domain.TaskLine
	for rows.Next() {
		var (
			line        domain.TaskLine
			predecessor sql.NullString
			setAt       string
		)
		if err := rows.Scan(&line.TaskID, &line.Role, &line.Version, &line.ID, &line.Agent,
			&predecessor, &line.Source, &line.SetBy, &setAt); err != nil {
			return nil, err
		}
		if predecessor.Valid {
			id := domain.Digest(predecessor.String)
			line.PredecessorID = &id
		}
		if line.SetAt, err = parseTime(setAt); err != nil {
			return nil, errRowInconsistent
		}
		if err := line.Validate(); err != nil {
			return nil, errRowInconsistent
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return lines, nil
}

// validateTaskLineChains checks what one row's validation cannot: within a
// role, versions run from 1 without a gap and each names the version before
// it. The lines arrive ordered by role and then version.
//
// The check catches a row edited in place and a missing first or middle
// version. It cannot catch a chain cut short at its end, or rows rewritten
// together with their ids: an id is an unkeyed hash of the row's own
// columns, so whoever can write the table can mint a consistent chain. A
// reader that acts on a line as the operator's choice must also hold it
// against the command that set it (SetBy).
func validateTaskLineChains(lines []domain.TaskLine) error {
	for i, line := range lines {
		if i == 0 || lines[i-1].Role != line.Role {
			if line.Version != 1 {
				return errRowInconsistent
			}
			continue
		}
		previous := lines[i-1]
		if line.Version != previous.Version+1 || line.PredecessorID == nil || *line.PredecessorID != previous.ID {
			return errRowInconsistent
		}
	}
	return nil
}
