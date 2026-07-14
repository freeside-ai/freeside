package store_test

import (
	"context"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

// headVersion derives the expected schema head from the embedded migration
// set, so this test does not go stale as migrations land.
func headVersion(t *testing.T) int {
	t.Helper()
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("no embedded migrations")
	}
	return len(names)
}

// TestMigrateFreshAndIdempotent is acceptance fixture 2: a fresh database
// migrates 0 -> head, re-running is a no-op, and the schema version is
// recorded and readable.
func TestMigrateFreshAndIdempotent(t *testing.T) {
	ctx := context.Background()
	head := headVersion(t)
	path := filepath.Join(t.TempDir(), "store.db")

	s, err := store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	version, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if version != head {
		t.Fatalf("fresh database at version %d, want head %d", version, head)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopening re-runs migrate against an up-to-date database: a no-op.
	s, err = store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	version, err = s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion after reopen: %v", err)
	}
	if version != head {
		t.Fatalf("reopened database at version %d, want head %d", version, head)
	}
}
