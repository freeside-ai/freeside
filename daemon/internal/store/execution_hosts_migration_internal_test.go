package store

import (
	"context"
	"database/sql"
	"testing"
)

// rewindExecutionHosts returns a migrated store to the schema 0149 leaves:
// it drops the two tables 0150 adds, with the daemon's host's row, and
// forgets 0150. The migration is add-only, so it has no test of its own
// (daemon/README.md, Testing conventions); the rewind keeps the earlier
// migrations' tests at head.
func rewindExecutionHosts(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	rewindOperatorMessages(t, ctx, db)
	for _, statement := range []string{
		`DROP TABLE execution_host_conditions`,
		`DROP TABLE execution_hosts`,
		`DELETE FROM schema_migrations WHERE name = '0150_execution_hosts.sql'`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}
