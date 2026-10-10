package store_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

var taskLineSetAt = time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)

func appendTaskLine(t *testing.T, s *store.Store, in domain.TaskLineInput, at time.Time) (domain.TaskLine, error) {
	t.Helper()
	ctx := context.Background()
	var line domain.TaskLine
	err := s.Write(ctx, func(tx *store.WriteTx) error {
		var err error
		line, err = tx.AppendTaskLine(ctx, in, at)
		return err
	})
	return line, err
}

func readTaskLines(t *testing.T, s *store.Store, taskID domain.TaskID) ([]domain.TaskLine, map[domain.RoleName]domain.TaskLine, error) {
	t.Helper()
	ctx := context.Background()
	var (
		all     []domain.TaskLine
		current map[domain.RoleName]domain.TaskLine
	)
	err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if all, err = tx.TaskLines(ctx, taskID); err != nil {
			return err
		}
		current, err = tx.CurrentTaskLines(ctx, taskID)
		return err
	})
	return all, current, err
}

// taskLineRow is one row exactly as stored, for comparing a version's bytes
// before and after a later append.
type taskLineRow struct {
	id, agent, source, setBy, setAt string
	predecessor                     sql.NullString
}

func rawTaskLineRow(t *testing.T, path string, taskID domain.TaskID, role domain.RoleName, version int) taskLineRow {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var row taskLineRow
	if err := db.QueryRowContext(context.Background(), `SELECT id, agent, predecessor_id, source, set_by, set_at
		FROM task_lines WHERE task_id = ? AND role = ? AND version = ?`, taskID, role, version).
		Scan(&row.id, &row.agent, &row.predecessor, &row.source, &row.setBy, &row.setAt); err != nil {
		t.Fatalf("read task line row %s %s v%d: %v", taskID, role, version, err)
	}
	return row
}

// TestAppendTaskLineSupersedesWithoutEditing is the acceptance fixture for a
// later change: the store appends version 2 naming version 1, version 1's
// row is byte-identical afterwards, and the current read returns version 2.
func TestAppendTaskLineSupersedesWithoutEditing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "task-lines.db")
	s := openStoreAt(t, path, store.Options{})
	taskID, _ := seedTaskSubmissionTarget(t, ctx, s)

	input := domain.TaskLineInput{
		TaskID: taskID, Role: domain.RoleImplementer, Agent: "codex",
		Source: domain.TaskLineSourceSubmitTask, SetBy: "cmd-1",
	}
	first, err := appendTaskLine(t, s, input, taskLineSetAt)
	if err != nil {
		t.Fatalf("append version 1: %v", err)
	}
	if first.Version != 1 || first.PredecessorID != nil {
		t.Fatalf("first line = %+v, want version 1 with no predecessor", first)
	}
	reviewer := input
	reviewer.Role, reviewer.Agent = domain.RoleReviewer, "claude-b"
	if _, err := appendTaskLine(t, s, reviewer, taskLineSetAt); err != nil {
		t.Fatalf("append reviewer line: %v", err)
	}
	before := rawTaskLineRow(t, path, taskID, domain.RoleImplementer, 1)

	changed := input
	changed.Agent, changed.Source, changed.SetBy = "claude-b", domain.TaskLineSourceCLISubmit, "cli:submission-1"
	second, err := appendTaskLine(t, s, changed, taskLineSetAt.Add(time.Hour))
	if err != nil {
		t.Fatalf("append version 2: %v", err)
	}
	if second.Version != 2 || second.PredecessorID == nil || *second.PredecessorID != first.ID {
		t.Fatalf("second line = %+v, want version 2 superseding %s", second, first.ID)
	}
	if after := rawTaskLineRow(t, path, taskID, domain.RoleImplementer, 1); after != before {
		t.Fatalf("version 1 row changed:\n before %+v\n after  %+v", before, after)
	}

	all, current, err := readTaskLines(t, s, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].ID != first.ID || all[1].ID != second.ID || all[2].Role != domain.RoleReviewer {
		t.Fatalf("all lines = %+v, want implementer v1, implementer v2, reviewer v1", all)
	}
	if len(current) != 2 || current[domain.RoleImplementer].ID != second.ID ||
		current[domain.RoleReviewer].Agent != "claude-b" {
		t.Fatalf("current lines = %+v, want implementer v2 and the reviewer line", current)
	}
	// The stored instant round-trips to the identity it was appended under.
	if !all[0].SetAt.Equal(taskLineSetAt) || all[0].SetAt.Location() != time.UTC {
		t.Fatalf("set_at = %v, want %v in UTC", all[0].SetAt, taskLineSetAt)
	}
}

func TestTaskLinesEmptyForTaskWithoutLines(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	taskID, _ := seedTaskSubmissionTarget(t, ctx, s)
	all, current, err := readTaskLines(t, s, taskID)
	if err != nil || len(all) != 0 || len(current) != 0 {
		t.Fatalf("lines = %v, %v (%v), want none", all, current, err)
	}
}

// TestAppendTaskLineRefusals pins the write gate: only an eligible role, a
// well-formed agent name, an operator source, and an existing task reach the
// table, and a refusal writes nothing.
func TestAppendTaskLineRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	taskID, _ := seedTaskSubmissionTarget(t, ctx, s)
	valid := domain.TaskLineInput{
		TaskID: taskID, Role: domain.RoleImplementer, Agent: "codex",
		Source: domain.TaskLineSourceSubmitTask, SetBy: "cmd-1",
	}
	for name, tc := range map[string]struct {
		mutate func(*domain.TaskLineInput)
		want   error
	}{
		"shadow reviewer": {func(in *domain.TaskLineInput) { in.Role = domain.RoleShadowReviewer }, domain.ErrTaskLineRoleIneligible},
		"wardless role":   {func(in *domain.TaskLineInput) { in.Role = domain.RoleDiagnostic }, domain.ErrTaskLineRoleIneligible},
		"bad agent name":  {func(in *domain.TaskLineInput) { in.Agent = "Not An Agent" }, domain.ErrInvalidAgentName},
		// Intake is not an operator path, so no source names it.
		"intake source": {func(in *domain.TaskLineInput) { in.Source = "intake" }, domain.ErrInvalidTaskLineSource},
		"empty source":  {func(in *domain.TaskLineInput) { in.Source = "" }, domain.ErrInvalidTaskLineSource},
		"empty set_by":  {func(in *domain.TaskLineInput) { in.SetBy = "" }, domain.ErrEmptyField},
	} {
		in := valid
		tc.mutate(&in)
		if _, err := appendTaskLine(t, s, in, taskLineSetAt); !errors.Is(err, tc.want) {
			t.Fatalf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
	missing := valid
	missing.TaskID = "task-missing"
	if _, err := appendTaskLine(t, s, missing, taskLineSetAt); err == nil {
		t.Fatal("a line for a task that does not exist was accepted")
	}
	if all, _, err := readTaskLines(t, s, taskID); err != nil || len(all) != 0 {
		t.Fatalf("lines after refusals = %v (%v), want none", all, err)
	}
	if all, _, err := readTaskLines(t, s, "task-missing"); err != nil || len(all) != 0 {
		t.Fatalf("lines for the missing task = %v (%v), want none", all, err)
	}
}

// TestTaskLinesFailClosedOnEditedRow pins the decode boundary: a row edited
// in place no longer resolves to its id, and a version removed from the
// start or the middle of a chain breaks it, so both reads refuse instead of
// serving the altered choice and an append refuses to extend it.
func TestTaskLinesFailClosedOnEditedRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, tamper := range map[string]string{
		"edited agent":      `UPDATE task_lines SET agent = 'claude-b' WHERE version = 1`,
		"edited source":     `UPDATE task_lines SET source = 'cli_submit' WHERE version = 1`,
		"edited set_at":     `UPDATE task_lines SET set_at = '2027-01-01T00:00:00Z' WHERE version = 2`,
		"unparsable set_at": `UPDATE task_lines SET set_at = 'yesterday' WHERE version = 2`,
		"removed version":   `DELETE FROM task_lines WHERE version = 2`,
		"removed first":     `DELETE FROM task_lines WHERE version = 1`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "task-lines.db")
			s := openStoreAt(t, path, store.Options{})
			taskID, _ := seedTaskSubmissionTarget(t, ctx, s)
			input := domain.TaskLineInput{
				TaskID: taskID, Role: domain.RoleImplementer, Agent: "codex",
				Source: domain.TaskLineSourceSubmitTask, SetBy: "cmd-1",
			}
			for range 3 {
				if _, err := appendTaskLine(t, s, input, taskLineSetAt); err != nil {
					t.Fatal(err)
				}
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			if _, err := db.ExecContext(ctx, tamper); err != nil {
				t.Fatal(err)
			}
			if _, _, err := readTaskLines(t, s, taskID); !errors.Is(err, store.ErrRowInconsistent) {
				t.Fatalf("read after tampering: err = %v, want %v", err, store.ErrRowInconsistent)
			}
			// An append refuses the same chain instead of extending it.
			if _, err := appendTaskLine(t, s, input, taskLineSetAt); !errors.Is(err, store.ErrRowInconsistent) {
				t.Fatalf("append after tampering: err = %v, want %v", err, store.ErrRowInconsistent)
			}
		})
	}
}
