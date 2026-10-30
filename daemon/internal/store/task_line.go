package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Task lines (migration 0095; plan §5.4, Admitted Agents) are append-only: a
// row is one line version and no statement here updates one. A change is the
// next version for its task and role. Migration 0111 adds a router line's
// provenance as JSON, NULL for every operator line. Migration 0113 adds the
// cleared version, whose agent is NULL, and the change records that back a
// cli_line_change line.

const taskLineColumns = `task_id, role, version, id, agent, predecessor_id, source, set_by, set_at, provenance, cleared`

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
// domain.TaskLine.Validate before any row is written, so a line no known
// path set, or a router line without its provenance, never reaches the table.
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
	var agent, predecessorID, provenance any
	if !line.Cleared {
		agent = line.Agent
	}
	if line.PredecessorID != nil {
		predecessorID = string(*line.PredecessorID)
	}
	if line.Provenance != nil {
		body, err := json.Marshal(line.Provenance)
		if err != nil {
			return domain.TaskLine{}, fmt.Errorf("append task line %q %q provenance: %w", in.TaskID, in.Role, err)
		}
		provenance = string(body)
	}
	if _, err := tx.tx.ExecContext(ctx, `INSERT INTO task_lines (`+taskLineColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		line.TaskID, line.Role, line.Version, line.ID, agent, predecessorID,
		line.Source, line.SetBy, formatTime(line.SetAt), provenance, line.Cleared); err != nil {
		return domain.TaskLine{}, fmt.Errorf("append task line %q %q: %w", in.TaskID, in.Role, err)
	}
	return line, nil
}

// RecordTaskLineChange records an operator's change and the line version that
// cites it, in the caller's transaction, and returns the line. Refusing a
// change whose ReplacesID is not the role's newest version keeps it from
// landing on a version it didn't see; the caller's error rolls back the
// change row with it.
//
// A change writes nothing else a client syncs, so this advances the task's
// own version. Without that, sync would keep serving the task's old
// task_lines.
func (tx *WriteTx) RecordTaskLineChange(ctx context.Context, change domain.TaskLineChange) (domain.TaskLine, error) {
	if err := change.Validate(); err != nil {
		return domain.TaskLine{}, fmt.Errorf("record task line change %q: %w", change.TaskID, err)
	}
	body, err := json.Marshal(change)
	if err != nil {
		return domain.TaskLine{}, fmt.Errorf("record task line change %q: %w", change.TaskID, err)
	}
	if _, err := tx.tx.ExecContext(ctx, `INSERT INTO task_line_changes (id, task_id, role, recorded_at, body)
		VALUES (?, ?, ?, ?, ?)`, change.ID, change.TaskID, change.Role, formatTime(change.RecordedAt), string(body)); err != nil {
		return domain.TaskLine{}, fmt.Errorf("record task line change %q: %w", change.TaskID, err)
	}
	line, err := tx.AppendTaskLine(ctx, domain.TaskLineInput{
		TaskID: change.TaskID, Role: change.Role, Agent: change.Agent, Cleared: change.Cleared,
		Source: domain.TaskLineSourceCLILineChange, SetBy: string(change.ID),
	}, change.RecordedAt)
	if err != nil {
		return domain.TaskLine{}, err
	}
	if !change.Replaces(line.PredecessorID) {
		return domain.TaskLine{}, fmt.Errorf("record task line change %q %s: the role's newest version is not the one it replaces: %w",
			change.TaskID, change.Role, domain.ErrTaskLineInconsistent)
	}
	task, err := tx.GetTask(ctx, change.TaskID)
	if err != nil {
		return domain.TaskLine{}, fmt.Errorf("record task line change %q: %w", change.TaskID, err)
	}
	if err := tx.updateTask(ctx, task); err != nil {
		return domain.TaskLine{}, fmt.Errorf("record task line change %q: %w", change.TaskID, err)
	}
	return line, nil
}

// GetTaskLineChange returns the change record with the given id. The decoded
// body is the authority: it must resolve to its own id and agree with the
// copied columns, so a row edited in place fails closed as a corrupt row.
func (tx *ReadTx) GetTaskLineChange(ctx context.Context, id domain.Digest) (domain.TaskLineChange, error) {
	var taskID, role, recordedAt, body string
	if err := tx.tx.QueryRowContext(ctx, `SELECT task_id, role, recorded_at, body FROM task_line_changes WHERE id = ?`, id).
		Scan(&taskID, &role, &recordedAt, &body); err != nil {
		return domain.TaskLineChange{}, fmt.Errorf("task line change %q: %w", id, notFoundOr(err))
	}
	var change domain.TaskLineChange
	if err := json.Unmarshal([]byte(body), &change); err != nil {
		return domain.TaskLineChange{}, fmt.Errorf("task line change %q: %w", id, errRowInconsistent)
	}
	if change.Validate() != nil || change.ID != id || string(change.TaskID) != taskID ||
		string(change.Role) != role || formatTime(change.RecordedAt) != recordedAt {
		return domain.TaskLineChange{}, fmt.Errorf("task line change %q: %w", id, errRowInconsistent)
	}
	return change, nil
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

// TaskLineHeads returns each role's newest version for the task, cleared or
// not. Only a reader that holds a clear against its record, or that records
// the next change, reads this; everyone else reads CurrentTaskLines. It reads
// every version, so a chain with an edited row or a missing earlier version
// fails the read instead of serving its head.
func (tx *ReadTx) TaskLineHeads(ctx context.Context, taskID domain.TaskID) (map[domain.RoleName]domain.TaskLine, error) {
	lines, err := tx.TaskLines(ctx, taskID)
	if err != nil {
		return nil, err
	}
	heads := make(map[domain.RoleName]domain.TaskLine, len(domain.TaskLineRoles))
	for _, line := range lines {
		// Ordered by version within a role, so the last one seen is newest.
		heads[line.Role] = line
	}
	return heads, nil
}

// CurrentTaskLines returns each role's latest line for the task. A role
// absent from the map has no line and stays on the lineup: it never had one,
// or its newest version is cleared.
func (tx *ReadTx) CurrentTaskLines(ctx context.Context, taskID domain.TaskID) (map[domain.RoleName]domain.TaskLine, error) {
	current, err := tx.TaskLineHeads(ctx, taskID)
	if err != nil {
		return nil, err
	}
	for role, line := range current {
		if line.Cleared {
			delete(current, role)
		}
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
			agent       sql.NullString
			predecessor sql.NullString
			setAt       string
			provenance  sql.NullString
		)
		if err := rows.Scan(&line.TaskID, &line.Role, &line.Version, &line.ID, &agent,
			&predecessor, &line.Source, &line.SetBy, &setAt, &provenance, &line.Cleared); err != nil {
			return nil, err
		}
		line.Agent = agent.String
		if provenance.Valid {
			line.Provenance = new(domain.TaskLineProvenance)
			if err := json.Unmarshal([]byte(provenance.String), line.Provenance); err != nil {
				return nil, errRowInconsistent
			}
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
