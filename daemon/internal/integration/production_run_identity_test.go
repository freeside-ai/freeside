package integration_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// A relaunch of the real-run harness re-records its identity bindings. It must
// keep a limit the operator raised rather than resetting it to the declared 1,
// and the final pass must accept that recorded limit.
func TestRealRunIdentitiesKeepRecordedLimit(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	declared := domain.AuthIdentity{
		ID: "claude-writer", Provider: "claude", AuthStoreMutationLease: true, MaxParallelExecutions: 1,
		Interim: domain.InterimClientFacts{AuthStoreVolume: "claude-auth", RefreshStrategy: domain.RefreshOnDemand},
	}
	limit := func() int {
		t.Helper()
		var got int
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			identity, err := tx.GetAuthIdentity(ctx, declared.ID)
			got = identity.MaxParallelExecutions
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}

	if err := realRunIdentities(ctx, st, false, declared); err != nil {
		t.Fatalf("first launch: %v", err)
	}
	if got := limit(); got != 1 {
		t.Fatalf("new identity limit = %d, want the declared 1", got)
	}
	raised := declared
	raised.MaxParallelExecutions = 4
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordAuthIdentity(ctx, raised, time.Now().UTC())
	}); err != nil {
		t.Fatal(err)
	}
	if err := realRunIdentities(ctx, st, false, declared); err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	if got := limit(); got != 4 {
		t.Fatalf("relaunch limit = %d, want the recorded 4", got)
	}
	if err := realRunIdentities(ctx, st, true, declared); err != nil {
		t.Fatalf("final pass with a raised limit: %v", err)
	}
	changed := declared
	changed.Interim.AuthStoreVolume = "other-auth"
	if err := realRunIdentities(ctx, st, true, changed); err == nil {
		t.Fatal("final pass accepted a changed auth store volume")
	}
}
