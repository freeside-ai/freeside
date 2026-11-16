// Package publishtest holds test helpers for opening the App authority. It is
// imported only by tests.
package publishtest

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

var (
	heldMu sync.Mutex
	held   = map[string]*publish.AuthorityWriterLock{}
)

// HoldAuthority creates stateDir owner-only when it is missing and returns the
// App authority writer lock on it, released when the test that first took it
// ends. A test that opens the keystore and the authority store over one
// directory, as a process does, gets the same handle for both: a second
// acquisition would conflict with the first.
func HoldAuthority(t testing.TB, stateDir string) *publish.AuthorityWriterLock {
	t.Helper()
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("create App authority state directory: %v", err)
	}
	key, err := filepath.EvalSymlinks(stateDir)
	if err != nil {
		t.Fatalf("resolve App authority state directory: %v", err)
	}
	heldMu.Lock()
	defer heldMu.Unlock()
	if lock, ok := held[key]; ok {
		return lock
	}
	lock, err := publish.AcquireAuthorityWriterLock(stateDir)
	if err != nil {
		t.Fatalf("acquire App authority writer lock: %v", err)
	}
	held[key] = lock
	t.Cleanup(func() {
		heldMu.Lock()
		delete(held, key)
		heldMu.Unlock()
		_ = lock.Close()
	})
	return lock
}

// Release closes the lock HoldAuthority took on stateDir, so code under test
// can take its own.
func Release(t testing.TB, stateDir string) {
	t.Helper()
	key, err := filepath.EvalSymlinks(stateDir)
	if err != nil {
		t.Fatalf("resolve App authority state directory: %v", err)
	}
	heldMu.Lock()
	lock := held[key]
	delete(held, key)
	heldMu.Unlock()
	_ = lock.Close()
}
