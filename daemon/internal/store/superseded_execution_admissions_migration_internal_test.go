package store

import (
	"context"
	"database/sql"
	"testing"
)

// rewindSupersededExecutionAdmissions returns a migrated store to the
// schema 0146 leaves: it drops the table 0147 creates and forgets 0147.
func rewindSupersededExecutionAdmissions(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	rewindRunMilestoneKinds(t, ctx, db)
	for _, statement := range []string{
		`DROP TABLE superseded_execution_admissions`,
		`DELETE FROM schema_migrations WHERE name = '0147_superseded_execution_admissions.sql'`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}
