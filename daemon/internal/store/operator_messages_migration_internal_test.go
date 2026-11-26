package store

import (
	"context"
	"database/sql"
	"testing"
)

// rewindOperatorMessages returns a migrated store to the schema 0150
// leaves: it drops the table 0151 adds and forgets 0151. The migration is
// add-only, so it has no test of its own (daemon/README.md, Testing
// conventions); the rewind keeps the earlier migrations' tests at head.
func rewindOperatorMessages(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	for _, statement := range []string{
		`DROP TABLE operator_messages`,
		`DELETE FROM schema_migrations WHERE name = '0151_operator_messages.sql'`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}
