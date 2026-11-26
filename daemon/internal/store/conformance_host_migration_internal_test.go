package store

import (
	"context"
	"database/sql"
	"testing"
)

// rewindConformanceHost returns a migrated store to the schema 0151 leaves:
// it drops the host index and column 0152 adds, restores the index by
// backend, and forgets 0152. The migration is add-only, so it has no test
// of its own (daemon/README.md, Testing conventions); the rewind keeps the
// earlier migrations' tests at head.
func rewindConformanceHost(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	rewindLaunchHost(t, ctx, db)
	for _, statement := range []string{
		`DROP INDEX backend_conformance_by_host_backend`,
		`ALTER TABLE backend_conformance_records DROP COLUMN host_id`,
		`CREATE INDEX backend_conformance_by_backend ON backend_conformance_records (backend, id)`,
		`DELETE FROM schema_migrations WHERE name = '0152_conformance_host.sql'`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}
