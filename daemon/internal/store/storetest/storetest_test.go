package storetest_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

func TestOpenPreservesExistingDatabaseAndAppliesOptions(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "store.db")
	s := storetest.Open(t, path, store.Options{})
	before, err := s.ServerState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := storetest.Open(t, path, store.Options{BusyTimeout: time.Second})
	after, err := reopened.ServerState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if before.SyncEpoch == "" || before != after {
		t.Fatalf("reopen changed server state: before=%+v after=%+v", before, after)
	}
	pragmas, err := reopened.Pragmas(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if pragmas.BusyTimeout != time.Second {
		t.Fatalf("busy timeout = %v, want 1s", pragmas.BusyTimeout)
	}
}
