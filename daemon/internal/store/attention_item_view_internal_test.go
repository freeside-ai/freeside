package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/freeside-ai/freeside/daemon/migrations"
)

// rewindAttentionItemViews returns a store at head to the shape a daemon at
// 0131 holds: no attention item views.
func rewindAttentionItemViews(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	rewindLaunchWaitKeepalive(t, ctx, db)
	for _, statement := range []string{
		`DROP TABLE attention_item_views`,
		`DELETE FROM schema_migrations WHERE name = '0132_attention_item_views.sql'`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
}

// TestAttentionItemViewsMigration: a daemon at 0131 migrates to a store with
// an empty view table, since 0132 backfills nothing from earlier card_opened
// events (issue #2930), and the table refuses an empty digest or time.
func TestAttentionItemViewsMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTemplateStoreAt(t, t.TempDir()+"/attention-item-views.db", Options{})
	rewindAttentionItemViews(t, ctx, st.db)
	if err := migrate(ctx, st.db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	assertAtHead(t, st.db)
	var rows int
	if err := st.db.QueryRowContext(ctx, `SELECT count(*) FROM attention_item_views`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("attention_item_views holds %d rows after migration, want 0", rows)
	}
	for _, statement := range []string{
		`INSERT INTO attention_item_views VALUES ('item-1', '', '2026-01-02T03:04:05Z', 1)`,
		`INSERT INTO attention_item_views VALUES ('item-1', 'sha256:00', '', 1)`,
	} {
		if _, err := st.db.ExecContext(ctx, statement); err == nil {
			t.Fatalf("%s: accepted, want a constraint refusal", statement)
		}
	}
}
