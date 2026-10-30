package store

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

// TestTaskLineProvenanceMigrationKeepsOperatorRows: a line recorded before
// 0111 gains a NULL provenance and reads back with its original source and id,
// because the canonical encoding omits an absent provenance.
func TestTaskLineProvenanceMigrationKeepsOperatorRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTemplateStoreAt(t, t.TempDir()+"/task-line-provenance.db", Options{})
	taskID, _ := seedOriginTestTarget(t, ctx, st)
	want, err := domain.NewTaskLine(domain.TaskLineInput{
		TaskID: taskID, Role: domain.RoleImplementer, Agent: "codex",
		Source: domain.TaskLineSourceSubmitTask, SetBy: "cmd-old",
	}, 1, nil, time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC))
	if err != nil {
		t.Fatal(err)
	}

	// Return the table to the shape a daemon at 0110 holds, then record a
	// line the way that daemon did.
	rewindUsagePools(t, ctx, st.db)
	for _, statement := range []string{
		`DROP TABLE task_line_changes`,
		`DELETE FROM schema_migrations WHERE name = '0113_task_line_changes.sql'`,
		`ALTER TABLE task_lines DROP COLUMN provenance`,
		`DELETE FROM schema_migrations WHERE name = '0111_task_line_provenance.sql'`,
	} {
		if _, err := st.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO task_lines
		(task_id, role, version, id, agent, predecessor_id, source, set_by, set_at)
		VALUES (?, ?, 1, ?, ?, NULL, ?, ?, ?)`,
		want.TaskID, want.Role, want.ID, want.Agent, want.Source, want.SetBy, formatTime(want.SetAt)); err != nil {
		t.Fatal(err)
	}
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
	if !reflect.DeepEqual(got, []domain.TaskLine{want}) {
		t.Fatalf("lines after migration = %+v, want %+v", got, want)
	}
}
