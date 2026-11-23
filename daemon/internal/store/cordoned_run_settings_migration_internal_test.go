package store

import (
	"context"
	"database/sql"
	"testing"
)

// rewindCordonedRunSettings returns a migrated store to the schema 0148
// leaves: it drops the table 0149 adds and forgets 0149.
// The migration is add-only, so it has no test of its own (daemon/README.md,
// Testing conventions); the rewind keeps the earlier migrations' tests at
// head.
func rewindCordonedRunSettings(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	for _, statement := range []string{
		`DROP TABLE task_writer_classes`,
		`DELETE FROM schema_migrations WHERE name = '0149_cordoned_run_settings.sql'`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}
