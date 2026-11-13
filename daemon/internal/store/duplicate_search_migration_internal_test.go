package store

import (
	"context"
	"database/sql"
	"testing"
)

// rewindDuplicateSearches returns a migrated store to the schema 0136
// leaves: no duplicate_searches table.
func rewindDuplicateSearches(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	for _, statement := range []string{
		`DROP TABLE duplicate_searches`,
		`DELETE FROM schema_migrations WHERE name = '0137_duplicate_searches.sql'`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}
