package store_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// tempDBPath returns a database path in a per-test directory. SQLite needs a
// real file here: with a connection pool, :memory: would give every
// connection its own database, and WAL needs the -wal/-shm sidecar files.
func tempDBPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "store.db")
}

func openStore(t *testing.T, opts store.Options) *store.Store {
	t.Helper()
	return openStoreAt(t, tempDBPath(t), opts)
}

// openStoreAt opens a store at a caller-chosen path, so a test can reopen the
// same database under different Options: a row persisted under one approved
// recipe set can be read back under another to exercise the reconstruction gate.
func openStoreAt(t *testing.T, path string, opts store.Options) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), path, opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

// TestOpenPragmas is acceptance fixture 1: a freshly opened store reports the
// §5.2 pragma configuration.
func TestOpenPragmas(t *testing.T) {
	s := openStore(t, store.Options{BusyTimeout: 2 * time.Second})
	got, err := s.Pragmas(context.Background())
	if err != nil {
		t.Fatalf("Pragmas: %v", err)
	}
	cases := []struct {
		name string
		got  any
		want any
	}{
		{"journal_mode", got.JournalMode, "wal"},
		{"synchronous", got.Synchronous, 2}, // 2 is FULL
		{"foreign_keys", got.ForeignKeys, true},
		{"busy_timeout", got.BusyTimeout, 2 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("%s = %v, want %v", tc.name, tc.got, tc.want)
			}
		})
	}
}

// TestOpenDefaultBusyTimeout pins the default applied when Options.BusyTimeout
// is zero.
func TestOpenDefaultBusyTimeout(t *testing.T) {
	s := openStore(t, store.Options{})
	got, err := s.Pragmas(context.Background())
	if err != nil {
		t.Fatalf("Pragmas: %v", err)
	}
	if got.BusyTimeout != store.DefaultBusyTimeout {
		t.Fatalf("busy_timeout = %v, want %v", got.BusyTimeout, store.DefaultBusyTimeout)
	}
}

// TestOpenRejectsInvalidBusyTimeout: a negative or below-resolution timeout
// would truncate to busy_timeout(0), silently disabling waiting; Open must
// refuse it instead.
func TestOpenRejectsInvalidBusyTimeout(t *testing.T) {
	cases := []struct {
		name    string
		timeout time.Duration
	}{
		{"negative", -time.Second},
		{"below millisecond resolution", 500 * time.Microsecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := store.Open(context.Background(), tempDBPath(t), store.Options{BusyTimeout: tc.timeout})
			if err == nil {
				_ = s.Close()
				t.Fatalf("Open accepted BusyTimeout %v, want error", tc.timeout)
			}
		})
	}
}

// TestOpenPathWithSpecialCharacters: the path rides a file: URI, so URI
// metacharacters in a legal path must be escaped or Open silently uses a
// different file (everything after an unescaped '?' parses as query).
func TestOpenPathWithSpecialCharacters(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "we?ird #dir 100%")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "sto?re#.db")

	s, err := store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The migrated database file must exist at exactly the requested path:
	// an unescaped '?' would have opened (and created) a truncated path
	// instead, and this Stat would fail.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database file not at the requested path: %v", err)
	}

	// Reopening the same path must find the migrated database again.
	s, err = store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	version, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if version == 0 {
		t.Fatal("reopened database has no applied migrations, want the migrated store")
	}
}
