package store

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

// TestTaskLineChangesMigrationKeepsEveryLine: every line stored before 0113
// reads back with its id and bytes, operator and router alike, and the
// rebuild keeps the constraints of 0095 and 0111.
func TestTaskLineChangesMigrationKeepsEveryLine(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTemplateStoreAt(t, t.TempDir()+"/task-line-changes.db", Options{})
	taskID, _ := seedOriginTestTarget(t, ctx, st)
	at := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	operator, err := domain.NewTaskLine(domain.TaskLineInput{
		TaskID: taskID, Role: domain.RoleImplementer, Agent: "codex",
		Source: domain.TaskLineSourceSubmitTask, SetBy: "cmd-old",
	}, 1, nil, at)
	if err != nil {
		t.Fatal(err)
	}
	superseding, err := domain.NewTaskLine(domain.TaskLineInput{
		TaskID: taskID, Role: domain.RoleImplementer, Agent: "claude-b",
		Source: domain.TaskLineSourceCLISubmit, SetBy: "cli:old",
	}, 2, &operator.ID, at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	router, err := domain.NewTaskLine(domain.TaskLineInput{
		TaskID: taskID, Role: domain.RoleReviewer, Agent: "claude-a",
		Source: domain.TaskLineSourceRouter, SetBy: "router:old",
		Provenance: &domain.TaskLineProvenance{PolicyRevision: 1, Trigger: domain.TaskLineSelectionFirstAdmission},
	}, 1, nil, at)
	if err != nil {
		t.Fatal(err)
	}

	// Return the table to the shape a daemon at 0111 holds, then store each
	// line the way that daemon did.
	for _, statement := range []string{
		`DROP TABLE task_line_changes`,
		`DROP TABLE task_lines`,
		`CREATE TABLE task_lines (
			task_id        TEXT    NOT NULL REFERENCES tasks (id),
			role           TEXT    NOT NULL CHECK (role IN ('specifier', 'implementer', 'remediator', 'reviewer')),
			version        INTEGER NOT NULL CHECK (version > 0),
			id             TEXT    NOT NULL UNIQUE CHECK (id <> ''),
			agent          TEXT    NOT NULL CHECK (agent <> ''),
			predecessor_id TEXT,
			source         TEXT    NOT NULL CHECK (source <> ''),
			set_by         TEXT    NOT NULL CHECK (set_by <> ''),
			set_at         TEXT    NOT NULL,
			provenance     TEXT    CHECK ((source = 'router') = (provenance IS NOT NULL)),
			CHECK ((version = 1) = (predecessor_id IS NULL)),
			PRIMARY KEY (task_id, role, version)
		) STRICT`,
		`DELETE FROM schema_migrations WHERE name = '0113_task_line_changes.sql'`,
	} {
		if _, err := st.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	insert := func(line domain.TaskLine, predecessor, provenance any) {
		t.Helper()
		if _, err := st.db.ExecContext(ctx, `INSERT INTO task_lines
			(task_id, role, version, id, agent, predecessor_id, source, set_by, set_at, provenance)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			line.TaskID, line.Role, line.Version, line.ID, line.Agent, predecessor,
			line.Source, line.SetBy, formatTime(line.SetAt), provenance); err != nil {
			t.Fatal(err)
		}
	}
	insert(operator, nil, nil)
	insert(superseding, string(operator.ID), nil)
	insert(router, nil, `{"policy_revision":1,"trigger":"first_admission","reading":null,"passed_over":null}`)
	if err := migrate(ctx, st.db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	assertAtHead(t, st.db)

	var got []domain.TaskLine
	if err := st.Read(ctx, func(tx *ReadTx) error {
		var err error
		got, err = tx.TaskLines(ctx, taskID)
		return err
	}); err != nil {
		t.Fatalf("read after migration: %v", err)
	}
	if want := []domain.TaskLine{operator, superseding, router}; !reflect.DeepEqual(got, want) {
		t.Fatalf("lines after migration = %+v, want %+v", got, want)
	}
	var agent, setAt string
	if err := st.db.QueryRowContext(ctx, `SELECT agent, set_at FROM task_lines WHERE id = ?`, superseding.ID).
		Scan(&agent, &setAt); err != nil {
		t.Fatal(err)
	}
	if agent != superseding.Agent || setAt != formatTime(superseding.SetAt) {
		t.Fatalf("stored bytes = %q %q", agent, setAt)
	}

	// The rebuilt table refuses what the old ones did, and a row that claims
	// a clear while naming an agent.
	for name, statement := range map[string]string{
		"a router row without provenance": `INSERT INTO task_lines (task_id, role, version, id, agent, source, set_by, set_at)
			VALUES ('` + string(taskID) + `', 'specifier', 1, 'a', 'codex', 'router', 'x', 'y')`,
		"an ineligible role": `INSERT INTO task_lines (task_id, role, version, id, agent, source, set_by, set_at)
			VALUES ('` + string(taskID) + `', 'shadow_reviewer', 1, 'b', 'codex', 'cli_submit', 'x', 'y')`,
		"a later version with no predecessor": `INSERT INTO task_lines (task_id, role, version, id, agent, source, set_by, set_at)
			VALUES ('` + string(taskID) + `', 'specifier', 2, 'c', 'codex', 'cli_submit', 'x', 'y')`,
		"no agent and no clear": `INSERT INTO task_lines (task_id, role, version, id, source, set_by, set_at)
			VALUES ('` + string(taskID) + `', 'specifier', 1, 'd', 'cli_submit', 'x', 'y')`,
		"an empty agent": `INSERT INTO task_lines (task_id, role, version, id, agent, source, set_by, set_at)
			VALUES ('` + string(taskID) + `', 'specifier', 1, 'e', '', 'cli_submit', 'x', 'y')`,
		"a clear naming an agent": `INSERT INTO task_lines (task_id, role, version, id, agent, source, set_by, set_at, cleared)
			VALUES ('` + string(taskID) + `', 'specifier', 1, 'f', 'codex', 'cli_line_change', 'x', 'y', 1)`,
	} {
		if _, err := st.db.ExecContext(ctx, statement); err == nil {
			t.Errorf("%s was stored", name)
		}
	}
}
