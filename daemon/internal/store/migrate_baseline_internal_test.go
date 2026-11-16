package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

// The squashed fixture stands in for a baseline: 0001_a and 0002_b are the
// files an older build applied, 0002_base is the two squashed into one, and
// 0003_next is a migration above it.
const (
	squashedA    = "CREATE TABLE a (id INTEGER PRIMARY KEY) STRICT;"
	squashedB    = "CREATE TABLE b (id INTEGER PRIMARY KEY) STRICT;"
	squashedNext = "CREATE TABLE c (id INTEGER PRIMARY KEY) STRICT;"
)

func preSquashFS(through int) map[string]string {
	files := map[string]string{"0001_a.sql": squashedA, "0002_b.sql": squashedB}
	if through == 1 {
		delete(files, "0002_b.sql")
	}
	return files
}

func squashedFS() map[string]string {
	return map[string]string{
		"0002_base.sql": squashedA + "\n" + squashedB,
		"0003_next.sql": squashedNext,
		squashedHistoryFile: "1 0001_a.sql " + contentaddr.Sum([]byte(squashedA)) + "\n" +
			"2 0002_b.sql " + contentaddr.Sum([]byte(squashedB)) + "\n",
	}
}

// ledger renders the recorded history as "version name" lines.
func ledger(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT version, name FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var lines []string
	for rows.Next() {
		var (
			version int
			name    string
		)
		if err := rows.Scan(&version, &name); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, fmt.Sprintf("%d %s", version, name))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

// TestMigrateFreshDatabaseRecordsTheBaseline: an empty database applies the
// baseline as its version and records the baseline to head, nothing below.
func TestMigrateFreshDatabaseRecordsTheBaseline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	for range 2 {
		if err := migrate(ctx, db, mapFS(squashedFS())); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	}
	if got, want := ledger(t, db), []string{"2 0002_base.sql", "3 0003_next.sql"}; !slices.Equal(got, want) {
		t.Fatalf("ledger = %q, want %q", got, want)
	}
	for _, table := range []string{"a", "b", "c"} {
		assertTableExists(t, db, table, true)
	}
}

// TestMigrateAcceptsThePreSquashHistory: a database an older build migrated
// through the squashed files keeps their rows and applies only what lies
// above the baseline.
func TestMigrateAcceptsThePreSquashHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	if err := migrate(ctx, db, mapFS(preSquashFS(2))); err != nil {
		t.Fatalf("migrate before the squash: %v", err)
	}
	if err := migrate(ctx, db, mapFS(squashedFS())); err != nil {
		t.Fatalf("migrate after the squash: %v", err)
	}
	if got, want := ledger(t, db), []string{"1 0001_a.sql", "2 0002_b.sql", "3 0003_next.sql"}; !slices.Equal(got, want) {
		t.Fatalf("ledger = %q, want %q", got, want)
	}
	assertTableExists(t, db, "c", true)
}

// TestMigrateRefusesBelowBaseline: a database whose history stops below the
// baseline is refused with BelowBaselineError and left as it was.
func TestMigrateRefusesBelowBaseline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	if err := migrate(ctx, db, mapFS(preSquashFS(1))); err != nil {
		t.Fatalf("migrate before the squash: %v", err)
	}
	before := schemaShape(t, db)
	err := migrate(ctx, db, mapFS(squashedFS()))
	var below *BelowBaselineError
	if !errors.As(err, &below) || below.Database != 1 || below.Baseline != 2 {
		t.Fatalf("migrate: err = %v, want BelowBaselineError{Database: 1, Baseline: 2}", err)
	}
	if !strings.Contains(err.Error(), lastUnsquashedCommit) {
		t.Fatalf("err text = %q, want it to name the build %s", err, lastUnsquashedCommit)
	}
	if after := schemaShape(t, db); !slices.Equal(after, before) {
		t.Fatalf("refused migrate changed the database\nbefore: %q\nafter:  %q", before, after)
	}
}

// TestMigrateRefusesDivergedSquashedHistory: below the baseline, a recorded
// row must equal the squashed list by name and digest. A divergent row is
// refused as a divergence even when the history also stops below the
// baseline, because no build migrates it.
func TestMigrateRefusesDivergedSquashedHistory(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		older map[string]string
	}{
		{"rewritten content", map[string]string{
			"0001_a.sql": squashedA, "0002_b.sql": "CREATE TABLE b (id INTEGER PRIMARY KEY, extra TEXT) STRICT;",
		}},
		{"renamed file", map[string]string{"0001_a.sql": squashedA, "0002_other.sql": squashedB}},
		{"below the baseline and rewritten", map[string]string{"0001_a.sql": squashedB}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			db := openRaw(t)
			if err := migrate(ctx, db, mapFS(tc.older)); err != nil {
				t.Fatalf("migrate before the squash: %v", err)
			}
			before := schemaShape(t, db)
			err := migrate(ctx, db, mapFS(squashedFS()))
			if err == nil || !strings.Contains(err.Error(), "migration history diverged") {
				t.Fatalf("migrate: err = %v, want a diverged history", err)
			}
			if after := schemaShape(t, db); !slices.Equal(after, before) {
				t.Fatalf("refused migrate changed the database\nbefore: %q\nafter:  %q", before, after)
			}
		})
	}
}

// TestMigrateRefusesHistoryStartingElsewhere: a recorded history must start
// at 1 or at the baseline. A database created from an older baseline, which
// a later squash has not taught the store to accept, is refused.
func TestMigrateRefusesHistoryStartingElsewhere(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	if err := migrate(ctx, db, mapFS(squashedFS())); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	later := map[string]string{
		"0003_base.sql": squashedA + "\n" + squashedB + "\n" + squashedNext,
		squashedHistoryFile: "1 0001_a.sql " + contentaddr.Sum([]byte(squashedA)) + "\n" +
			"2 0002_b.sql " + contentaddr.Sum([]byte(squashedB)) + "\n" +
			"3 0003_next.sql " + contentaddr.Sum([]byte(squashedNext)) + "\n",
	}
	err := migrate(ctx, db, mapFS(later))
	if err == nil || !strings.Contains(err.Error(), "starts at version 2") {
		t.Fatalf("migrate: err = %v, want a history starting at 2 refused", err)
	}
}

// preSquashDatabase writes a database at head whose ledger records the
// squashed files' rows in place of the baseline's own, as a database an
// older build created holds, cut at through. It returns the path.
func preSquashDatabase(t *testing.T, through int) string {
	t.Helper()
	ctx := context.Background()
	image, err := TemplateDatabase(ctx)
	if err != nil {
		t.Fatal(err)
	}
	history, err := loadMigrationHistory(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if history.baseline <= 1 {
		t.Fatalf("embedded baseline = %d, want a squash above 1", history.baseline)
	}
	path := filepath.Join(t.TempDir(), "store.db")
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := openDB(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	}()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version = ?`, history.baseline); err != nil {
		t.Fatal(err)
	}
	for _, row := range history.squashed {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name, digest, applied_at) VALUES (?, ?, ?, ?)`,
			row.version, row.name, row.digest, "2026-01-02T03:04:05Z"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version > ?`, through); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return path
}

// openPaths is every way a caller opens an existing database; each shares
// the history rule.
var openPaths = []struct {
	name string
	open func(context.Context, string, Options) (*Store, error)
}{
	{"Open", Open},
	{"MigrateExisting", MigrateExisting},
	{"OpenExisting", OpenExisting},
	{"OpenReadOnly", OpenReadOnly},
}

// TestOpenPathsAcceptThePreSquashHistory: a database an older build migrated
// to head through the squashed files opens on every path.
func TestOpenPathsAcceptThePreSquashHistory(t *testing.T) {
	t.Parallel()
	head, err := CurrentSchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range openPaths {
		t.Run(path.name, func(t *testing.T) {
			t.Parallel()
			s, err := path.open(t.Context(), preSquashDatabase(t, head), Options{})
			if err != nil {
				t.Fatalf("%s: %v", path.name, err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestOpenPathsRefuseBelowBaseline: a database whose history stops below the
// embedded baseline is refused on every path with BelowBaselineError, and
// its ledger and schema are left as they were.
func TestOpenPathsRefuseBelowBaseline(t *testing.T) {
	t.Parallel()
	history, err := loadMigrationHistory(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range openPaths {
		t.Run(path.name, func(t *testing.T) {
			t.Parallel()
			file := preSquashDatabase(t, history.baseline-1)
			before := schemaShape(t, openRawFile(t, file))
			_, err := path.open(t.Context(), file, Options{})
			var below *BelowBaselineError
			if !errors.As(err, &below) || below.Database != history.baseline-1 || below.Baseline != history.baseline {
				t.Fatalf("%s: err = %v, want BelowBaselineError{Database: %d, Baseline: %d}",
					path.name, err, history.baseline-1, history.baseline)
			}
			if after := schemaShape(t, openRawFile(t, file)); !slices.Equal(after, before) {
				t.Fatalf("%s changed the refused database", path.name)
			}
		})
	}
}

// openRawFile opens path on a raw handle closed at cleanup.
func openRawFile(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := openDB(path, Options{})
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

// TestTemplateDatabaseAtStartsAtTheBaseline: no fixture can stand below the
// baseline, and one at the baseline records only the baseline's own row.
func TestTemplateDatabaseAtStartsAtTheBaseline(t *testing.T) {
	t.Parallel()
	history, err := loadMigrationHistory(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := TemplateDatabaseAt(t.Context(), history.baseline-1); err == nil {
		t.Fatalf("TemplateDatabaseAt(%d) succeeded below the baseline", history.baseline-1)
	}
	image, err := TemplateDatabaseAt(t.Context(), history.baseline)
	if err != nil {
		t.Fatalf("TemplateDatabaseAt(%d): %v", history.baseline, err)
	}
	path := filepath.Join(t.TempDir(), "store.db")
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatal(err)
	}
	want := []string{fmt.Sprintf("%d %s", history.baseline, history.files[0])}
	if got := ledger(t, openRawFile(t, path)); !slices.Equal(got, want) {
		t.Fatalf("ledger = %q, want %q", got, want)
	}
}
