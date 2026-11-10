package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/freeside-ai/freeside/daemon/migrations"
)

// rewindLaunchWaitKeepalive returns a store at head to the shape a daemon at
// 0132 holds: launch waits with no keepalive column.
func rewindLaunchWaitKeepalive(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	for _, statement := range []string{
		`ALTER TABLE launch_waits DROP COLUMN kept_at`,
		`DELETE FROM schema_migrations WHERE name = '0133_launch_wait_keepalive.sql'`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
}

// TestLaunchWaitKeepaliveMigration: a wait recorded at 0132 reads back after
// 0133 with no keepalive, and the column refuses an empty time.
func TestLaunchWaitKeepaliveMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTemplateStoreAt(t, t.TempDir()+"/launch-wait-keepalive.db", Options{})
	rewindLaunchWaitKeepalive(t, ctx, st.db)
	if _, err := st.db.ExecContext(ctx, `INSERT INTO launch_waits
		(invocation_id, run_id, task_created_at, waiting_since, refused_at)
		VALUES ('inv-1', 'run-1', '2026-01-02T03:04:05Z', '2026-01-02T03:05:05Z', '2026-01-02T03:06:05Z')`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, st.db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	assertAtHead(t, st.db)

	var waits []launchWait
	if err := st.Read(ctx, func(tx *ReadTx) error {
		var err error
		waits, err = tx.listLaunchWaits(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(waits) != 1 || !waits[0].keptAt.IsZero() || waits[0].refreshedAt() != waits[0].refusedAt {
		t.Fatalf("waits after 0133 = %+v, want the one wait with no keepalive", waits)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE launch_waits SET kept_at = '' WHERE invocation_id = 'inv-1'`); err == nil {
		t.Fatal("an empty kept_at was accepted")
	}
}
