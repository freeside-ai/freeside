package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/freeside-ai/freeside/daemon/migrations"
)

// rewindProjectReadiness returns a migrated store to the schema 0145
// leaves: it drops the table 0146 creates and forgets 0146.
func rewindProjectReadiness(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	rewindSupersededExecutionAdmissions(t, ctx, db)
	for _, statement := range []string{
		`DROP TABLE project_readiness`,
		`DELETE FROM schema_migrations WHERE name = '0146_project_readiness.sql'`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

// TestProjectReadinessMigrationCreatesTable proves migration 0146 creates the
// readiness table keyed by project and both revisions, refusing an empty
// project, a negative revision, and an empty body.
func TestProjectReadinessMigrationCreatesTable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRawThrough(t, "0146_")
	assertTableExists(t, db, "project_readiness", false)
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	assertAtHead(t, db)
	assertTableExists(t, db, "project_readiness", true)
	insert := `INSERT INTO project_readiness (project_id, instance_revision, project_revision, evaluated_at, body)
VALUES (?, ?, ?, ?, ?)`
	if _, err := db.ExecContext(ctx, insert, "p", 0, 0, "2026-11-16T12:00:00Z", "{}"); err != nil {
		t.Fatalf("file-settings row: %v", err)
	}
	if _, err := db.ExecContext(ctx, insert, "p", 0, 0, "2026-11-16T12:00:00Z", "{}"); err == nil {
		t.Fatal("a second row for one key was accepted")
	}
	for name, args := range map[string][]any{
		"empty project":     {"", 1, 1, "2026-11-16T12:00:00Z", "{}"},
		"negative revision": {"p", -1, 1, "2026-11-16T12:00:00Z", "{}"},
		"empty body":        {"p", 1, 1, "2026-11-16T12:00:00Z", ""},
	} {
		if _, err := db.ExecContext(ctx, insert, args...); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
