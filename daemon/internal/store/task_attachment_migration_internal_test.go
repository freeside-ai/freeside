package store

import (
	"context"
	"database/sql"
	"testing"
)

// rewindTaskAttachments returns a store at head to the shape a daemon at 0123
// holds.
func rewindTaskAttachments(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	rewindAlternateAgentRetries(t, ctx, db)
	for _, statement := range []string{
		`DROP TABLE task_attachments`,
		`DELETE FROM schema_migrations WHERE name = '0124_task_attachments.sql'`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
}
