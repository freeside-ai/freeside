package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// rewindUpdateDrain returns a migrated store to the schema 0134 leaves: no
// update drain log.
func rewindUpdateDrain(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	for _, statement := range []string{
		`DROP TABLE update_drain_transitions`,
		`DELETE FROM schema_migrations WHERE name = '0135_update_drain.sql'`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

// TestUpdateDrainMigration: a store at 0134 migrates to an empty drain log,
// which reads as no drain, and the table refuses an empty build and a forced
// flag outside 0 and 1.
func TestUpdateDrainMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	copySnapshotThrough(t, path, "0135_")
	st := openTemplateStoreAt(t, path, Options{})
	assertAtHead(t, st.db)

	var count int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM update_drain_transitions`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("update_drain_transitions has %d rows after 0135, want 0", count)
	}
	if err := st.Read(ctx, func(tx *ReadTx) error {
		drain, err := tx.UpdateDrainInForce(ctx)
		if err == nil && drain != nil {
			t.Errorf("drain after 0135 = %+v, want none", drain)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, insert := range []string{
		`INSERT INTO update_drain_transitions (state, build, forced, occurred_at)
			VALUES ('draining', '', 0, '2026-01-02T03:04:05Z')`,
		`INSERT INTO update_drain_transitions (state, build, forced, occurred_at)
			VALUES ('draining', 'sha256:x', 2, '2026-01-02T03:04:05Z')`,
	} {
		if _, err := st.db.ExecContext(ctx, insert); err == nil {
			t.Fatalf("accepted %s", insert)
		}
	}
}
