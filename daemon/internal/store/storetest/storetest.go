// Package storetest provides file-backed test stores from a migrated template.
// Tests of migrations or Open behavior must use store.Open directly instead.
package storetest

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/store"
)

var template = sync.OnceValues(func() ([]byte, error) {
	return store.TemplateDatabase(context.Background())
})

// Open copies the process's unseeded template to path if it does not exist,
// then opens it with opts and closes the store at test cleanup. Existing files
// open as-is, preserving rows and epochs in reopen tests.
func Open(t testing.TB, path string, opts store.Options) *store.Store {
	t.Helper()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		Copy(t, path)
	} else if err != nil {
		t.Fatalf("stat store path: %v", err)
	}
	s, err := store.Open(t.Context(), path, opts)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return s
}

// Copy writes the process's unseeded template to path without opening it,
// for tests whose code under test opens the store itself: its first open
// seeds the epoch exactly as it would on a freshly migrated file. Path must
// not exist, so Copy never overwrites a fixture's rows.
func Copy(t testing.TB, path string) {
	t.Helper()
	data, err := template()
	if err != nil {
		t.Fatalf("build store template: %v", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // test-owned path
	if err != nil {
		t.Fatalf("create store copy: %v", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		t.Fatalf("copy store template: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close store copy: %v", err)
	}
}
