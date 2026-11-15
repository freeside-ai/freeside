package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/freeside-ai/freeside/daemon/migrations"
)

// rewindSettingsRevisions returns a migrated store to the schema 0141
// leaves: it drops the tables 0142 creates and forgets 0142.
func rewindSettingsRevisions(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	for _, statement := range []string{
		`DROP TABLE instance_settings_revisions`,
		`DROP TABLE project_settings_revisions`,
		`DELETE FROM schema_migrations WHERE name = '0142_settings_revisions.sql'`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

// TestSettingsRevisionsMigrationCreatesTables proves migration 0142 creates
// both settings tables keyed as the records need: one row per instance
// revision, one per project and revision, and every column non-empty.
func TestSettingsRevisionsMigrationCreatesTables(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRawThrough(t, "0142_")
	if got := rawVersion(t, db); got != 141 {
		t.Fatalf("schema version before 0142 = %d, want 141", got)
	}
	assertTableExists(t, db, "instance_settings_revisions", false)
	assertTableExists(t, db, "project_settings_revisions", false)
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	assertAtHead(t, db)
	assertTableExists(t, db, "instance_settings_revisions", true)
	assertTableExists(t, db, "project_settings_revisions", true)

	instance := `INSERT INTO instance_settings_revisions (revision, section, set_by, set_at, body) VALUES (?, ?, ?, ?, ?)`
	if _, err := db.ExecContext(ctx, instance, 1, "seed_root", "cli:op", "2026-11-15T00:00:00Z", "{}"); err != nil {
		t.Fatalf("well-formed instance revision rejected: %v", err)
	}
	if _, err := db.ExecContext(ctx, instance, 1, "images", "cli:op", "2026-11-15T00:00:00Z", "{}"); err == nil {
		t.Fatal("second instance revision 1 accepted; the primary key does not hold")
	}
	project := `INSERT INTO project_settings_revisions (project_id, revision, section, set_by, set_at, body) VALUES (?, ?, ?, ?, ?, ?)`
	for _, id := range []string{"project-a", "project-b"} {
		if _, err := db.ExecContext(ctx, project, id, 1, "repository", "cli:op", "2026-11-15T00:00:00Z", "{}"); err != nil {
			t.Fatalf("project %s revision 1 rejected: %v", id, err)
		}
	}
	if _, err := db.ExecContext(ctx, project, "project-a", 1, "allowed_paths", "cli:op", "2026-11-15T00:00:00Z", "{}"); err == nil {
		t.Fatal("second project-a revision 1 accepted; the primary key does not hold")
	}
	for _, row := range [][]any{
		{"", 2, "repository", "cli:op", "2026-11-15T00:00:00Z", "{}"},
		{"project-a", 0, "repository", "cli:op", "2026-11-15T00:00:00Z", "{}"},
		{"project-a", 2, "", "cli:op", "2026-11-15T00:00:00Z", "{}"},
		{"project-a", 2, "repository", "", "2026-11-15T00:00:00Z", "{}"},
		{"project-a", 2, "repository", "cli:op", "", "{}"},
		{"project-a", 2, "repository", "cli:op", "2026-11-15T00:00:00Z", ""},
	} {
		if _, err := db.ExecContext(ctx, project, row...); err == nil {
			t.Fatalf("malformed row accepted: %v", row)
		}
	}
}
