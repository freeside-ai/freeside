package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
)

// TestKillRecoverySeedRefusesHeldDatabase pins #1333's guard: seeding the
// kill-recovery readiness rows while a daemon holds the database must fail at
// once with daemonlock.ErrAlreadyRunning, before any store open, rather than
// contend for SQLite's write lock and time out with SQLITE_BUSY.
func TestKillRecoverySeedRefusesHeldDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "freeside.db")
	resolution, waiver, event := killRecoveryReadinessFixture(t)

	held, err := daemonlock.Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	err = seedKillRecoveryReadinessRecords(path, resolution, waiver, event)
	if !errors.Is(err, daemonlock.ErrAlreadyRunning) {
		t.Fatalf("seed under a held daemon lock: err = %v, want %v", err, daemonlock.ErrAlreadyRunning)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("seed opened the store despite the held lock: stat err = %v", statErr)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}

	if err := seedKillRecoveryReadinessRecords(path, resolution, waiver, event); err != nil {
		t.Fatalf("seed after lock release: %v", err)
	}
	assertKillRecoveryReadiness(t, path)
}
