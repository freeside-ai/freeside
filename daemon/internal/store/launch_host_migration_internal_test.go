package store

import (
	"context"
	"database/sql"
	"testing"
)

// rewindLaunchHost returns a migrated store to the schema 0152 leaves: it
// drops the two columns 0153 adds and forgets 0153. The migration is
// add-only, so it has no test of its own (daemon/README.md, Testing
// conventions); the rewind keeps the earlier migrations' tests at head.
func rewindLaunchHost(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	for _, statement := range []string{
		`ALTER TABLE execution_admissions DROP COLUMN execution_host_id`,
		`ALTER TABLE run_hold_observations DROP COLUMN cordoned_launch`,
		`DELETE FROM schema_migrations WHERE name = '0153_launch_host.sql'`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}
