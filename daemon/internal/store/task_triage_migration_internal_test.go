package store

import (
	"context"
	"database/sql"
	"testing"
)

// rewindTaskTriages returns a store at head to the shape a daemon at 0122
// holds.
func rewindTaskTriages(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	rewindTaskAttachments(t, ctx, db)
	for _, statement := range []string{
		`DROP TABLE task_triages`,
		`DELETE FROM schema_migrations WHERE name = '0123_task_triages.sql'`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
}
