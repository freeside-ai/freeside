package store

import (
	"context"
	"database/sql"
	"reflect"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

func TestSuccessorPublicationMigrationPreservesHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0062_")
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatal(err)
	}
	item := legacyReadyItem(t, "run-production", true)
	seedLegacyReadyItem(t, ctx, db, item)
	seedLegacyReadyBinding(t, ctx, db, item)
	migrateThrough(t, ctx, db, "0069_")
	if got := rawVersion(t, db); got != 68 {
		t.Fatalf("prior schema = %d", got)
	}
	for _, version := range []byte{1, 2, 3} {
		body := []byte{version, 0, 255}
		if _, err := db.ExecContext(ctx, `INSERT INTO outbox
			(idempotency_key, kind, payload, status, created_at, payload_version, payload_digest)
			VALUES (?, 'preservation-fixture', ?, 'pending', '2026-09-08T00:00:00Z', ?, ?)`,
			version, body, version, contentaddr.Sum(body)); err != nil {
			t.Fatal(err)
		}
	}
	queries := []string{
		`SELECT id, idempotency_key, kind, payload, status, created_at, payload_version, payload_digest FROM outbox ORDER BY id`,
		`SELECT * FROM ready_item_pr_bindings ORDER BY item_id`,
	}
	before := make([][][]any, len(queries))
	for i, query := range queries {
		before[i] = migrationRows(t, db, query)
	}
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	for i, query := range queries {
		if after := migrationRows(t, db, query); !reflect.DeepEqual(before[i], after) {
			t.Fatalf("migration changed retained rows for %s", query)
		}
	}
	if rows := migrationRows(t, db, `PRAGMA foreign_key_check`); len(rows) != 0 {
		t.Fatalf("migration left foreign-key violations: %v", rows)
	}
	for _, version := range []int{3, 4} {
		if _, err := db.ExecContext(ctx, `INSERT INTO outbox
			(idempotency_key, kind, payload, created_at, payload_version)
			VALUES ('new-publication-' || ?, 'publish.publication', X'7B7D', '2026-09-08T00:00:00Z', ?)`, version, version); err != nil {
			t.Fatalf("current publication version %d rejected: %v", version, err)
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE outbox SET payload_version = 3 WHERE idempotency_key = 'new-publication-4'`); err == nil {
		t.Fatal("successor intent allowed a payload downgrade")
	}
}

func migrationRows(t *testing.T, db *sql.DB, query string) [][]any {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var result [][]any
	for rows.Next() {
		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatal(err)
		}
		result = append(result, values)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}
