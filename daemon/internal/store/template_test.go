package store_test

import (
	"os"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestTemplateDatabaseIsCompleteAndSeedsIndependentEpochs(t *testing.T) {
	t.Parallel()
	data, err := store.TemplateDatabase(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var epochs [2]string
	wantVersion, err := store.CurrentSchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	for i := range epochs {
		path := tempDBPath(t)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		// Validate before Open can repair missing migrations or seed an epoch.
		ro, err := store.OpenReadOnly(t.Context(), path, store.Options{})
		if err != nil {
			t.Fatal(err)
		}
		version, err := ro.SchemaVersion(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if version != wantVersion {
			t.Fatalf("schema version = %d, want %d", version, wantVersion)
		}
		state, err := ro.ServerState(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if state.SyncEpoch != "" {
			t.Fatalf("template epoch = %q, want empty", state.SyncEpoch)
		}
		if err := ro.Close(); err != nil {
			t.Fatal(err)
		}
		s, err := store.Open(t.Context(), path, store.Options{})
		if err != nil {
			t.Fatal(err)
		}
		state, err = s.ServerState(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		epochs[i] = string(state.SyncEpoch)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if epochs[0] == "" || epochs[1] == "" || epochs[0] == epochs[1] {
		t.Fatalf("copies must have distinct non-empty epochs: %q", epochs)
	}
}
