package store_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestCopyDatabaseFileIgnoresSchema copies a database an older build left,
// which OpenExisting refuses, into an owner-only file, and refuses to write
// over an existing one.
func TestCopyDatabaseFileIgnoresSchema(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	head, err := store.CurrentSchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	body, err := store.TemplateDatabaseAt(ctx, head-1)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "live.db")
	if err := os.WriteFile(src, body, 0o600); err != nil {
		t.Fatal(err)
	}
	var mismatch *store.SchemaMismatchError
	if _, err := store.OpenExisting(ctx, src, store.Options{}); !errors.As(err, &mismatch) {
		t.Fatalf("OpenExisting on an older database: err = %v, want a schema mismatch", err)
	}

	dst := filepath.Join(dir, "copy.db")
	if err := store.CopyDatabaseFile(ctx, src, dst); err != nil {
		t.Fatalf("CopyDatabaseFile: %v", err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("copy mode = %v, want 0600", mode)
	}
	version, err := store.ReadSchemaVersion(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	if version != head-1 {
		t.Fatalf("copy schema version = %d, want %d", version, head-1)
	}
	after, err := os.ReadFile(src) //nolint:gosec // test path is under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, after) {
		t.Fatal("CopyDatabaseFile changed its source")
	}

	existing, err := os.ReadFile(dst) //nolint:gosec // test path is under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CopyDatabaseFile(ctx, src, dst); err == nil {
		t.Fatal("CopyDatabaseFile over an existing file succeeded")
	}
	again, err := os.ReadFile(dst) //nolint:gosec // test path is under t.TempDir
	if err != nil {
		t.Fatalf("existing copy removed: %v", err)
	}
	if !bytes.Equal(existing, again) {
		t.Fatal("CopyDatabaseFile changed an existing file")
	}
}

// TestCopyDatabaseFileNeverCreatesSource refuses a missing source rather
// than copying an empty database it created.
func TestCopyDatabaseFileNeverCreatesSource(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "missing.db")
	if err := store.CopyDatabaseFile(t.Context(), src, filepath.Join(dir, "copy.db")); err == nil {
		t.Fatal("CopyDatabaseFile from a missing source succeeded")
	}
	if _, err := os.Lstat(src); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source after a refused copy: %v, want it absent", err)
	}
}
