package store

import (
	"context"
	"database/sql"
	"testing"
)

// rewindAdmittedAgentBases returns a migrated store to the schema 0153
// leaves: it drops the two tables 0154 adds and forgets 0154.
func rewindAdmittedAgentBases(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	execAll(t, ctx, db,
		`DROP TABLE admitted_agent_base_pointers`,
		`DROP TABLE admitted_agent_bases`,
		`DELETE FROM schema_migrations WHERE name = '0154_admitted_agent_bases.sql'`,
	)
}

func execAll(t *testing.T, ctx context.Context, db *sql.DB, statements ...string) {
	t.Helper()
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}
