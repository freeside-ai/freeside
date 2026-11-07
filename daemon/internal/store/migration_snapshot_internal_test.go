package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/freeside-ai/freeside/daemon/migrations"
)

// snapshots caches raw database images by schema version for migration
// tests that start from an empty database. A version is built from the
// highest cached version below it, and every build also keeps the image at
// each multiple of snapshotStride it passes. Parallel tests ask for
// versions in no particular order, so without those stops a request below
// every cached version would rebuild from empty; with them it starts at
// most snapshotStride-1 migrations back. One lock covers the builds, which
// serializes them, and memory stays at the requested versions plus about
// head/snapshotStride stops.
var snapshots = struct {
	sync.Mutex
	images map[int][]byte
}{images: map[int][]byte{}}

const snapshotStride = 16

// migrateImage writes base (nil for an empty database) to a scratch file,
// migrates it through version on a raw handle, and returns the file after
// the close has checkpointed the WAL into it, as templateDatabase does.
func migrateImage(ctx context.Context, base []byte, version int) ([]byte, error) {
	prefix, err := migrationsThrough(version)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "freeside-store-snapshot-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "store.db")
	if base != nil {
		if err := os.WriteFile(path, base, 0o600); err != nil {
			return nil, err
		}
	}
	db, err := openDB(path, Options{})
	if err != nil {
		return nil, err
	}
	if err := migrate(ctx, db, prefix); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := db.Close(); err != nil {
		return nil, err
	}
	return os.ReadFile(path) //nolint:gosec // test-owned scratch path
}

// snapshotImage returns the cached image at version, building it first
// under the cache lock. Whatever base it starts from, migrate takes the
// image to exactly the version asked for, and a base above it fails loudly.
func snapshotImage(version int) ([]byte, error) {
	snapshots.Lock()
	defer snapshots.Unlock()
	if image, ok := snapshots.images[version]; ok {
		return image, nil
	}
	from := 0
	var image []byte
	for v := version - 1; v >= 1; v-- {
		if cached, ok := snapshots.images[v]; ok {
			from, image = v, cached
			break
		}
	}
	for stop := (from/snapshotStride + 1) * snapshotStride; stop <= version; stop += snapshotStride {
		next, err := migrateImage(context.Background(), image, stop)
		if err != nil {
			return nil, err
		}
		snapshots.images[stop], image = next, next
	}
	if _, ok := snapshots.images[version]; !ok {
		next, err := migrateImage(context.Background(), image, version)
		if err != nil {
			return nil, err
		}
		snapshots.images[version], image = next, next
	}
	return image, nil
}

// versionBefore is the schema version migrateThrough(before) leaves: the
// count of embedded migrations whose names sort before before.
func versionBefore(t testing.TB, before string) int {
	t.Helper()
	names, err := migrationFiles(migrations.FS)
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	version, _ := slices.BinarySearch(names, before)
	if version < 1 {
		t.Fatalf("no migration sorts before %q", before)
	}
	return version
}

// copySnapshotThrough writes to path, which must not exist, a database at
// the head immediately before the named migration: the state openRaw plus
// migrateThrough(before) leaves, copied from the snapshot cache. It is for
// tests that open, close, and reopen path themselves; openRawThrough is the
// handle-returning form.
func copySnapshotThrough(t testing.TB, path, before string) {
	t.Helper()
	copySnapshotAt(t, path, versionBefore(t, before))
}

// copySnapshotAt is copySnapshotThrough for a caller that names the schema
// version rather than the next migration: path, which must not exist,
// receives the cached image at version.
func copySnapshotAt(t testing.TB, path string, version int) {
	t.Helper()
	image, err := snapshotImage(version)
	if err != nil {
		t.Fatalf("build snapshot at version %d: %v", version, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // test-owned path
	if err != nil {
		t.Fatalf("create snapshot copy: %v", err)
	}
	if _, err := f.Write(image); err != nil {
		_ = f.Close()
		t.Fatalf("copy snapshot: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close snapshot copy: %v", err)
	}
}

// openRawThrough replaces openRaw followed by migrateThrough(before) on the
// empty database: a raw handle at the head immediately before the named
// migration, closed at cleanup. A test that then seeds rows still calls
// migrateThrough to run the migrations under test for real.
func openRawThrough(t *testing.T, before string) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.db")
	copySnapshotThrough(t, path, before)
	db, err := openDB(path, Options{})
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	if got, want := rawVersion(t, db), versionBefore(t, before); got != want {
		t.Fatalf("snapshot before %s at schema version %d, want %d", before, got, want)
	}
	return db
}

// TestMigrationSnapshotsMatchInProcessMigration pins that a snapshot equals
// the database migrateThrough would have left. One raw handle steps from
// empty through every version in process; a second chain closes, copies,
// and reopens the file between every version, which is the most reopens
// any snapshot cache chain can make. After each step both must hold the
// same schema objects and the same applied migrations. A migration that
// left state on its connection (a temp object, a persistent pragma) would
// diverge here; sqlite_temp_master is not compared, so extend this test if
// a migration ever creates temp objects.
func TestMigrationSnapshotsMatchInProcessMigration(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	head, err := CurrentSchemaVersion()
	if err != nil {
		t.Fatalf("current schema version: %v", err)
	}
	stepped := openRaw(t)
	var image []byte
	for version := 1; version <= head; version++ {
		prefix, err := migrationsThrough(version)
		if err != nil {
			t.Fatal(err)
		}
		if err := migrate(ctx, stepped, prefix); err != nil {
			t.Fatalf("step to version %d: %v", version, err)
		}
		image, err = migrateImage(ctx, image, version)
		if err != nil {
			t.Fatalf("snapshot version %d: %v", version, err)
		}
		snapshot := openRawImage(t, image)
		if got, want := schemaShape(t, snapshot), schemaShape(t, stepped); !slices.Equal(got, want) {
			t.Fatalf("version %d: snapshot schema differs from in-process migration\nsnapshot: %q\nstepped:  %q", version, got, want)
		}
		if err := snapshot.Close(); err != nil {
			t.Fatalf("close snapshot: %v", err)
		}
	}
}

func openRawImage(t *testing.T, image []byte) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "snapshot.db")
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	db, err := openDB(path, Options{})
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	return db
}

// schemaShape renders every schema object and applied migration as one
// line each; applied_at is left out because it records when, not what.
func schemaShape(t *testing.T, db *sql.DB) []string {
	t.Helper()
	var shape []string
	for _, query := range []string{
		`SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_master ORDER BY type, name`,
		`SELECT CAST(version AS TEXT), name, digest, '' FROM schema_migrations ORDER BY version`,
	} {
		rows, err := db.Query(query)
		if err != nil {
			t.Fatalf("query %q: %v", query, err)
		}
		for rows.Next() {
			var a, b, c, d string
			if err := rows.Scan(&a, &b, &c, &d); err != nil {
				_ = rows.Close()
				t.Fatalf("scan: %v", err)
			}
			shape = append(shape, fmt.Sprintf("%s|%s|%s|%s", a, b, c, d))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("close rows: %v", err)
		}
	}
	return shape
}
