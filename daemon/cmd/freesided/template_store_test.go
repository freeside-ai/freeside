package main

import (
	"errors"
	"os"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// seedTemplateStore lays down the migrated template and its topic key at
// dbPath when no store is there yet, and leaves an existing store alone so a
// restart keeps its rows. The command under test then opens an existing store
// with its key through topicstore.Open and finds every migration already
// applied; its first open seeds the epoch as it would on a fresh file. Tests
// of first-run behaviour (a missing store or key, or a refusal that must leave
// no store) must not use it.
func seedTemplateStore(t testing.TB, dbPath string) {
	t.Helper()
	if _, err := os.Stat(dbPath); err == nil {
		return
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat store path: %v", err)
	}
	if _, err := loadOrCreateTopicKey(dbPath, false); err != nil {
		t.Fatalf("create topic key: %v", err)
	}
	storetest.Copy(t, dbPath)
}
