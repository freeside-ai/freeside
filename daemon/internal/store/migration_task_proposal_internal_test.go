package store

import (
	"context"
	"database/sql"
	"testing"
)

func serverRevision(t *testing.T, ctx context.Context, db *sql.DB) int64 {
	t.Helper()
	var revision int64
	if err := db.QueryRowContext(ctx,
		`SELECT revision FROM server_state WHERE id = 1`).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	return revision
}
