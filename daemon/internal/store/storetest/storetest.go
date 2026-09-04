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
		data, err := template()
		if err != nil {
			t.Fatalf("build store template: %v", err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("copy store template: %v", err)
		}
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
