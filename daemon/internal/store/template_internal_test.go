package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Internal tests cannot import storetest, which imports this package.
var internalTemplate = sync.OnceValues(func() ([]byte, error) {
	return TemplateDatabase(context.Background())
})

func openTemplateStore(t testing.TB, opts Options) *Store {
	t.Helper()
	return openTemplateStoreAt(t, filepath.Join(t.TempDir(), "store.db"), opts)
}

func openTemplateStoreAt(t testing.TB, path string, opts Options) *Store {
	t.Helper()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		data, err := internalTemplate()
		if err != nil {
			t.Fatalf("build store template: %v", err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("copy store template: %v", err)
		}
	} else if err != nil {
		t.Fatalf("stat store path: %v", err)
	}
	s, err := Open(t.Context(), path, opts)
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
