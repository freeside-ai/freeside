package signet

import (
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestValidateSnapshotRefutesImpossibleMetadata pins the extra trust gate the
// signet read boundary adds to values returned by store. Store reads can also
// occur inside an in-progress Write and therefore cannot enforce the upper
// bound; every signet sync projection is a pure Read and must reject it.
func TestValidateSnapshotRefutesImpossibleMetadata(t *testing.T) {
	state := store.ServerState{SyncEpoch: "epoch-1", Revision: 7}
	for _, test := range []struct {
		name     string
		snapshot store.Snapshot
	}{
		{"zero entity version", store.Snapshot{EntityVersion: 0, AsOfRevision: 1}},
		{"zero row revision", store.Snapshot{EntityVersion: 1, AsOfRevision: 0}},
		{"row ahead of server", store.Snapshot{EntityVersion: 1, AsOfRevision: 8}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateSnapshot(state, test.snapshot); !errors.Is(err, ErrInvalidSyncSnapshot) {
				t.Fatalf("validateSnapshot error = %v, want ErrInvalidSyncSnapshot", err)
			}
		})
	}
	if err := validateSnapshot(state, store.Snapshot{EntityVersion: 2, AsOfRevision: 7}); err != nil {
		t.Fatalf("valid snapshot rejected: %v", err)
	}
}

func TestValidateServerStateRefutesImpossibleState(t *testing.T) {
	for _, state := range []store.ServerState{
		{SyncEpoch: "", Revision: 0},
		{SyncEpoch: "epoch-1", Revision: -1},
	} {
		if err := validateServerState(state); !errors.Is(err, ErrInvalidSyncSnapshot) {
			t.Fatalf("validateServerState(%+v) error = %v, want ErrInvalidSyncSnapshot", state, err)
		}
	}
}
