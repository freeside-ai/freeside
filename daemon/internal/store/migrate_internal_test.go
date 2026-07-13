package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"testing/fstest"
)

// This file is an internal test (package store): the failure paths exercise
// migrate and openDB directly with a substitute fs.FS, so no broken SQL is
// ever embedded in daemon/migrations.

func mapFS(files map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, body := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return fsys
}

func openRaw(t *testing.T) *sql.DB {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "store.db"), Options{})
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return db
}

func rawVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var version int
	err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version)
	if err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	return version
}

// TestFailingMigrationRollsBack is acceptance fixture 3: a deliberately
// failing migration leaves the database at the prior version, with none of
// the failed file's earlier statements applied.
func TestFailingMigrationRollsBack(t *testing.T) {
	ctx := context.Background()
	db := openRaw(t)
	fsys := mapFS(map[string]string{
		"0001_ok.sql": "CREATE TABLE ok (id INTEGER PRIMARY KEY) STRICT;",
		// The first statement succeeds, the second is a syntax error: the
		// whole file must roll back, not just the failing statement.
		"0002_bad.sql": "CREATE TABLE partial (id INTEGER PRIMARY KEY) STRICT;\nCREATE TABLE oops (;",
	})

	if err := migrate(ctx, db, fsys); err == nil {
		t.Fatal("migrate succeeded, want failure from 0002_bad.sql")
	}
	if got := rawVersion(t, db); got != 1 {
		t.Fatalf("schema version after failed migration = %d, want 1", got)
	}
	assertTableExists(t, db, "ok", true)
	assertTableExists(t, db, "partial", false)
}

func assertTableExists(t *testing.T, db *sql.DB, name string, want bool) {
	t.Helper()
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = ?`, name).Scan(&n)
	if err != nil {
		t.Fatalf("query sqlite_schema: %v", err)
	}
	if got := n == 1; got != want {
		t.Fatalf("table %q exists = %v, want %v", name, got, want)
	}
}

// TestMigrationFileValidation pins the naming and ordering rules.
func TestMigrationFileValidation(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
	}{
		{"gap", map[string]string{
			"0001_a.sql": "SELECT 1;",
			"0003_c.sql": "SELECT 1;",
		}},
		{"duplicate version", map[string]string{
			"0001_a.sql": "SELECT 1;",
			"0001_b.sql": "SELECT 1;",
		}},
		{"bad name", map[string]string{
			"first.sql": "SELECT 1;",
		}},
		{"not starting at 0001", map[string]string{
			"0002_a.sql": "SELECT 1;",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := migrationFiles(mapFS(tc.files)); err == nil {
				t.Fatal("migrationFiles succeeded, want error")
			}
		})
	}
}

// TestMigrateRejectsDivergedHistory: an applied migration whose recorded name
// or content digest differs from the embedded file of the same version is a
// hard error; a same-name rewrite must not silently diverge existing and
// fresh databases.
func TestMigrateRejectsDivergedHistory(t *testing.T) {
	cases := []struct {
		name     string
		diverged map[string]string
	}{
		{"renamed file", map[string]string{
			"0001_rewritten.sql": "CREATE TABLE a (id INTEGER PRIMARY KEY) STRICT;",
		}},
		{"rewritten content under the same name", map[string]string{
			"0001_original.sql": "CREATE TABLE a (id INTEGER PRIMARY KEY, extra TEXT) STRICT;",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db := openRaw(t)
			if err := migrate(ctx, db, mapFS(map[string]string{
				"0001_original.sql": "CREATE TABLE a (id INTEGER PRIMARY KEY) STRICT;",
			})); err != nil {
				t.Fatalf("migrate: %v", err)
			}
			if err := migrate(ctx, db, mapFS(tc.diverged)); err == nil {
				t.Fatal("migrate accepted a diverged history, want error")
			}
		})
	}
}

// TestPragmasOnEveryConnection widens the pool to prove the DSN applies the
// pragmas to each new connection, not just the first: with database/sql
// pooling, every pragma except journal_mode is per-connection state.
func TestPragmasOnEveryConnection(t *testing.T) {
	ctx := context.Background()
	db, err := openDB(filepath.Join(t.TempDir(), "store.db"), Options{})
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	}()
	db.SetMaxOpenConns(2)

	first, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("first conn: %v", err)
	}
	defer func() { _ = first.Close() }()
	second, err := db.Conn(ctx) // held concurrently, so it is a distinct connection
	if err != nil {
		t.Fatalf("second conn: %v", err)
	}
	defer func() { _ = second.Close() }()

	for i, conn := range []*sql.Conn{first, second} {
		p, err := connPragmas(ctx, conn)
		if err != nil {
			t.Fatalf("conn %d pragmas: %v", i+1, err)
		}
		if !p.ForeignKeys || p.Synchronous != 2 || p.JournalMode != "wal" {
			t.Fatalf("conn %d pragmas = %+v, want wal/FULL/foreign keys on", i+1, p)
		}
	}
}
