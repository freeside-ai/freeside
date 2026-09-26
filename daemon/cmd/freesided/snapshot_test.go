package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestSnapshotWithoutDaemon(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dbPath := filepath.Join(root, "live.db")
	st, err := store.Open(t.Context(), dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "copy.db")
	var out bytes.Buffer
	if err := runSnapshotCommand(t.Context(), []string{"-db", dbPath, output}, &out, io.Discard); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != output {
		t.Fatalf("snapshot printed %q, want %q", got, output)
	}
	requireSnapshotReadable(t, output)
	requireSnapshotRefusesExisting(t, dbPath, output)
}

func TestSnapshotRejectsBadArguments(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cases := map[string][]string{
		"missing db":     {filepath.Join(root, "copy.db")},
		"missing output": {"-db", filepath.Join(root, "live.db")},
		"extra output":   {"-db", filepath.Join(root, "live.db"), filepath.Join(root, "a.db"), filepath.Join(root, "b.db")},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := runSnapshotCommand(t.Context(), args, io.Discard, io.Discard); err == nil {
				t.Fatalf("snapshot %v succeeded, want error", args)
			}
		})
	}
}

// TestSnapshotThroughExclusiveDaemon: a daemon holding its live file
// exclusively still serves control-socket commands, which shows it opens no
// second handle on its own file, and the snapshot it writes opens while the
// live file refuses every other handle.
func TestSnapshotThroughExclusiveDaemon(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "live.db")
	// The dev tier selects exclusive locking. Its supervised-path guards
	// run in main, not run, so a temporary database is safe here.
	h, err := run(t.Context(), nil, config{Environment: environmentDev, DBPath: dbPath, ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})

	var doctorOut bytes.Buffer
	err = runDoctorCommand(t.Context(), []string{
		"-db", dbPath, "-backend-configuration-digest", "sha256:" + strings.Repeat("a", 64),
	}, &doctorOut, io.Discard)
	// A fresh daemon has no backup evidence, so doctor serves an unhealthy
	// report; a busy live file would fail before producing one.
	if err == nil || !strings.Contains(err.Error(), "unhealthy") || !json.Valid(doctorOut.Bytes()) {
		t.Fatalf("routed doctor = %s, %v", doctorOut.String(), err)
	}

	output := filepath.Join(root, "copy.db")
	var out bytes.Buffer
	if err := runSnapshotCommand(t.Context(), []string{"-db", dbPath, output}, &out, io.Discard); err != nil {
		t.Fatalf("routed snapshot: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != output {
		t.Fatalf("snapshot printed %q, want %q", got, output)
	}
	requireSnapshotReadable(t, output)
	requireSnapshotRefusesExisting(t, dbPath, output)

	live, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(50)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = live.Close() }()
	var tables int
	err = live.QueryRowContext(t.Context(), `SELECT count(*) FROM sqlite_master`).Scan(&tables)
	var driverErr *sqlite.Error
	if !errors.As(err, &driverErr) || driverErr.Code()&0xff != sqlite3.SQLITE_BUSY {
		t.Fatalf("second handle on the live file: err = %v, want SQLITE_BUSY", err)
	}
}

// requireSnapshotReadable opens the copy through a separate read-only handle
// that requires the schema at head, and checks it is owner-only.
func requireSnapshotReadable(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("snapshot mode = %v, want 0600", mode)
	}
	copied, err := store.OpenReadOnly(t.Context(), path, store.Options{BusyTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	if err := copied.Close(); err != nil {
		t.Fatal(err)
	}
}

// requireSnapshotRefusesExisting runs the command against an output that
// already exists and checks the file is left byte-for-byte unchanged.
func requireSnapshotRefusesExisting(t *testing.T, dbPath, output string) {
	t.Helper()
	before, err := os.ReadFile(output) //nolint:gosec // test path is under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	err = runSnapshotCommand(t.Context(), []string{"-db", dbPath, output}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "exists") {
		t.Fatalf("snapshot over an existing file: err = %v, want refusal", err)
	}
	after, err := os.ReadFile(output) //nolint:gosec // test path is under t.TempDir
	if err != nil {
		t.Fatalf("existing output removed: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("snapshot changed an existing output file")
	}
}
