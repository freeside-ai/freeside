package store_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
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

// routerTaskLineInput is a router line that passed over an exhausted pool:
// the provenance shape that carries every optional citation.
func routerTaskLineInput(taskID domain.TaskID) domain.TaskLineInput {
	return domain.TaskLineInput{
		TaskID: taskID, Role: domain.RoleImplementer, Agent: "claude-b",
		Source: domain.TaskLineSourceRouter, SetBy: "router:" + string(taskID),
		Provenance: &domain.TaskLineProvenance{
			PolicyRevision: 2, Trigger: domain.TaskLineSelectionExhaustedMark,
			Reading: &domain.TaskLineSelectionReading{
				Pool: "claude-a", Window: "seven_day_opus", ObservedAt: taskLineSetAt.Add(-time.Hour),
			},
			PassedOver: new("claude-a"),
		},
	}
}

// TestTaskLineProvenanceRoundTrip: a router line's provenance is stored as
// JSON and decodes to the line it was written as, an operator line stores
// NULL, and the table's CHECK holds the column to the source for a writer
// that bypasses the store.
func TestTaskLineProvenanceRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "task-lines.db")
	s := openStoreAt(t, path, store.Options{})
	taskID, _ := seedTaskSubmissionTarget(t, ctx, s)
	routed, err := appendTaskLine(t, s, routerTaskLineInput(taskID), taskLineSetAt)
	if err != nil {
		t.Fatal(err)
	}
	operator, err := appendTaskLine(t, s, domain.TaskLineInput{
		TaskID: taskID, Role: domain.RoleReviewer, Agent: "codex",
		Source: domain.TaskLineSourceSubmitTask, SetBy: "cmd-1",
	}, taskLineSetAt)
	if err != nil {
		t.Fatal(err)
	}
	all, current, err := readTaskLines(t, s, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(all, []domain.TaskLine{routed, operator}) {
		t.Fatalf("lines = %+v, want %+v then %+v", all, routed, operator)
	}
	if !reflect.DeepEqual(current[domain.RoleImplementer], routed) {
		t.Fatalf("current implementer line = %+v, want %+v", current[domain.RoleImplementer], routed)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var provenance sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT provenance FROM task_lines WHERE role = 'reviewer'`).
		Scan(&provenance); err != nil || provenance.Valid {
		t.Fatalf("operator line provenance column = %+v (%v), want NULL", provenance, err)
	}
	for name, statement := range map[string]string{
		"router without provenance": `INSERT INTO task_lines (task_id, role, version, id, agent, source, set_by, set_at)
			VALUES (?, 'specifier', 1, 'sha256:x', 'codex', 'router', 'router:x', '2026-01-02T03:04:05Z')`,
		"operator with provenance": `INSERT INTO task_lines (task_id, role, version, id, agent, source, set_by, set_at, provenance)
			VALUES (?, 'specifier', 1, 'sha256:y', 'codex', 'submit_task', 'cmd-1', '2026-01-02T03:04:05Z', '{}')`,
	} {
		if _, err := db.ExecContext(ctx, statement, taskID); err == nil {
			t.Fatalf("%s: the table accepted the row", name)
		}
	}
}

// TestTaskLinesFailClosedOnEditedProvenance: provenance is bound into the
// line's id, so an edited or unparsable column fails the read like any other
// edited column.
func TestTaskLinesFailClosedOnEditedProvenance(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, tamper := range map[string]string{
		"edited revision":    `UPDATE task_lines SET provenance = json_set(provenance, '$.policy_revision', 3)`,
		"edited passed over": `UPDATE task_lines SET provenance = json_set(provenance, '$.passed_over', 'codex')`,
		"unparsable":         `UPDATE task_lines SET provenance = '{'`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "task-lines.db")
			s := openStoreAt(t, path, store.Options{})
			taskID, _ := seedTaskSubmissionTarget(t, ctx, s)
			if _, err := appendTaskLine(t, s, routerTaskLineInput(taskID), taskLineSetAt); err != nil {
				t.Fatal(err)
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
		})
	}
}

// TestGetTaskLineResolvesAVersionByID covers the read that resolves the line
// an admission cites: any recorded version resolves by its id, a superseded
// one included, because the line an attempt was admitted under stays the
// choice that authorized it. An id nobody recorded is not found.
func TestGetTaskLineResolvesAVersionByID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	taskID, _ := seedTaskSubmissionTarget(t, ctx, s)
	input := domain.TaskLineInput{
		TaskID: taskID, Role: domain.RoleImplementer, Agent: "codex",
		Source: domain.TaskLineSourceSubmitTask, SetBy: "cmd-1",
	}
	first, err := appendTaskLine(t, s, input, taskLineSetAt)
	if err != nil {
		t.Fatal(err)
	}
	changed := input
	changed.Agent = "claude-b"
	second, err := appendTaskLine(t, s, changed, taskLineSetAt)
	if err != nil {
		t.Fatal(err)
	}
	get := func(id domain.Digest) (domain.TaskLine, error) {
		var line domain.TaskLine
		err := s.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			line, err = tx.GetTaskLine(ctx, id)
			return err
		})
		return line, err
	}
	for _, want := range []domain.TaskLine{first, second} {
		got, err := get(want.ID)
		if err != nil || got.ID != want.ID || got.Agent != want.Agent || got.Version != want.Version ||
			got.TaskID != taskID {
			t.Fatalf("GetTaskLine(v%d) = %+v, %v; want %+v", want.Version, got, err, want)
		}
	}
	unknown := domain.Digest("sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
	if _, err := get(unknown); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetTaskLine(unrecorded id) = %v, want %v", err, store.ErrNotFound)
	}
}

// TestGetTaskLineFailsClosedOnABrokenChain: the cited version is served only
// out of a chain that reads whole, so an intact row behind a removed or
// edited one fails the way a current read does.
func TestGetTaskLineFailsClosedOnABrokenChain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, tamper := range map[string]string{
		"removed first version": `DELETE FROM task_lines WHERE version = 1`,
		"edited earlier agent":  `UPDATE task_lines SET agent = 'claude-b' WHERE version = 1`,
		"edited cited agent":    `UPDATE task_lines SET agent = 'claude-b' WHERE version = 2`,
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
			var cited domain.TaskLine
			for range 2 {
				var err error
				if cited, err = appendTaskLine(t, s, input, taskLineSetAt); err != nil {
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
			err = s.Read(ctx, func(tx *store.ReadTx) error {
				_, err := tx.GetTaskLine(ctx, cited.ID)
				return err
			})
			if !errors.Is(err, store.ErrRowInconsistent) {
				t.Fatalf("GetTaskLine after tampering: err = %v, want %v", err, store.ErrRowInconsistent)
			}
		})
	}
}

// recordTaskLineChange records a change and its line, as the engine does.
func recordTaskLineChange(
	t *testing.T, s *store.Store, taskID domain.TaskID, role domain.RoleName, agent string, cleared bool,
	replaces *domain.Digest, at time.Time,
) (domain.TaskLine, error) {
	t.Helper()
	ctx := context.Background()
	change, err := domain.NewTaskLineChange(taskID, role, agent, cleared, replaces, at)
	if err != nil {
		t.Fatal(err)
	}
	var line domain.TaskLine
	err = s.Write(ctx, func(tx *store.WriteTx) error {
		var err error
		line, err = tx.RecordTaskLineChange(ctx, change)
		return err
	})
	return line, err
}

// TestClearedHeadReadsAsNoLine: a role whose newest version is cleared has no
// current line, a later change gives it one again, and the whole chain stays
// readable.
func TestClearedHeadReadsAsNoLine(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "task-lines.db")
	s := openStoreAt(t, path, store.Options{})
	taskID, _ := seedTaskSubmissionTarget(t, ctx, s)
	first, err := appendTaskLine(t, s, domain.TaskLineInput{
		TaskID: taskID, Role: domain.RoleReviewer, Agent: "codex",
		Source: domain.TaskLineSourceSubmitTask, SetBy: "cmd-1",
	}, taskLineSetAt)
	if err != nil {
		t.Fatal(err)
	}
	cleared, err := recordTaskLineChange(t, s, taskID, domain.RoleReviewer, "", true, &first.ID, taskLineSetAt.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !cleared.Cleared || cleared.Agent != "" || cleared.Version != 2 || cleared.Source != domain.TaskLineSourceCLILineChange {
		t.Fatalf("cleared version = %+v", cleared)
	}
	all, current, err := readTaskLines(t, s, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := current[domain.RoleReviewer]; ok || !reflect.DeepEqual(all, []domain.TaskLine{first, cleared}) {
		t.Fatalf("current = %+v, all = %+v; want no current line over both versions", current, all)
	}
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		heads, err := tx.TaskLineHeads(ctx, taskID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(heads[domain.RoleReviewer], cleared) {
			t.Errorf("head = %+v, want the cleared version", heads[domain.RoleReviewer])
		}
		got, err := tx.GetTaskLine(ctx, cleared.ID)
		if err != nil || !reflect.DeepEqual(got, cleared) {
			t.Errorf("GetTaskLine = %+v, %v", got, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var agent sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT agent FROM task_lines WHERE id = ?`, cleared.ID).Scan(&agent); err != nil {
		t.Fatal(err)
	}
	if agent.Valid {
		t.Fatalf("a cleared version stored agent %q, want NULL", agent.String)
	}

	again, err := recordTaskLineChange(t, s, taskID, domain.RoleReviewer, "claude-b", false, &cleared.ID, taskLineSetAt.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	all, current, err = readTaskLines(t, s, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(current[domain.RoleReviewer], again) || len(all) != 3 || again.Version != 3 {
		t.Fatalf("current = %+v, all = %+v", current, all)
	}
}

// TestRecordTaskLineChangeRefusesAVersionItDidNotSee: a change whose replaced
// version isn't the role's newest writes neither its record nor its line.
func TestRecordTaskLineChangeRefusesAVersionItDidNotSee(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStoreAt(t, filepath.Join(t.TempDir(), "task-lines.db"), store.Options{})
	taskID, _ := seedTaskSubmissionTarget(t, ctx, s)
	first, err := recordTaskLineChange(t, s, taskID, domain.RoleImplementer, "codex", false, nil, taskLineSetAt)
	if err != nil {
		t.Fatal(err)
	}
	second, err := recordTaskLineChange(t, s, taskID, domain.RoleImplementer, "claude-b", false, &first.ID, taskLineSetAt.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	stale, err := domain.NewTaskLineChange(taskID, domain.RoleImplementer, "claude-c", false, &first.ID, taskLineSetAt.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		_, err := tx.RecordTaskLineChange(ctx, stale)
		return err
	}); !errors.Is(err, domain.ErrTaskLineInconsistent) {
		t.Fatalf("a stale change: %v", err)
	}
	if _, err := recordTaskLineChange(t, s, taskID, domain.RoleSpecifier, "codex", false, &first.ID, taskLineSetAt); err == nil {
		t.Fatal("a change replacing a version of a role with no line was recorded")
	}
	all, _, err := readTaskLines(t, s, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(all, []domain.TaskLine{first, second}) {
		t.Fatalf("lines = %+v", all)
	}
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetTaskLineChange(ctx, stale.ID)
		return err
	}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the stale change's record: %v", err)
	}
}

// TestRecordTaskLineChangeAdvancesTheTask: sync reads a task by its version,
// so a change that moved none would leave clients on the old task_lines.
func TestRecordTaskLineChangeAdvancesTheTask(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStoreAt(t, filepath.Join(t.TempDir(), "task-lines.db"), store.Options{})
	taskID, _ := seedTaskSubmissionTarget(t, ctx, s)
	version := func() store.Snapshot {
		t.Helper()
		var snapshot store.Snapshot
		if err := s.Read(ctx, func(tx *store.ReadTx) error {
			got, err := tx.GetTaskSnapshot(ctx, taskID)
			snapshot = got.Snapshot
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	before := version()
	if _, err := recordTaskLineChange(t, s, taskID, domain.RoleImplementer, "codex", false, nil, taskLineSetAt); err != nil {
		t.Fatal(err)
	}
	if after := version(); after.EntityVersion != before.EntityVersion+1 || after.AsOfRevision <= before.AsOfRevision {
		t.Fatalf("task snapshot = %+v after %+v, want it advanced", after, before)
	}
}

func TestGetTaskLineChangeFailsClosedOnAnEditedRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, tamper := range map[string]string{
		"edited body":        `UPDATE task_line_changes SET body = replace(body, '"codex"', '"claude-b"')`,
		"unparsable body":    `UPDATE task_line_changes SET body = 'not json'`,
		"edited role":        `UPDATE task_line_changes SET role = 'reviewer'`,
		"edited recorded_at": `UPDATE task_line_changes SET recorded_at = '2027-01-01T00:00:00Z'`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "task-lines.db")
			s := openStoreAt(t, path, store.Options{})
			taskID, _ := seedTaskSubmissionTarget(t, ctx, s)
			line, err := recordTaskLineChange(t, s, taskID, domain.RoleImplementer, "codex", false, nil, taskLineSetAt)
			if err != nil {
				t.Fatal(err)
			}
			id := domain.Digest(line.SetBy)
			read := func() (domain.TaskLineChange, error) {
				var change domain.TaskLineChange
				err := s.Read(ctx, func(tx *store.ReadTx) error {
					var err error
					change, err = tx.GetTaskLineChange(ctx, id)
					return err
				})
				return change, err
			}
			change, err := read()
			if err != nil {
				t.Fatal(err)
			}
			if !change.Backs(line) {
				t.Fatalf("change = %+v does not back %+v", change, line)
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			if _, err := db.ExecContext(ctx, tamper); err != nil {
				t.Fatal(err)
			}
			if _, err := read(); !errors.Is(err, store.ErrRowInconsistent) {
				t.Fatalf("read after tampering: err = %v, want %v", err, store.ErrRowInconsistent)
			}
		})
	}
}
