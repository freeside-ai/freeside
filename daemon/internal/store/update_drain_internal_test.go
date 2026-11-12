package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestUpdateDrainRoundTrip: a row in every drain state reads back with its
// build and forced flag.
func TestUpdateDrainRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, state := range domain.AllUpdateDrainStates {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			st := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "store.db"), Options{})
			want := domain.UpdateDrainTransition{
				State:      state,
				Build:      "sha256:2222222222222222222222222222222222222222222222222222222222222222",
				Forced:     state == domain.UpdateDrainDraining,
				OccurredAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
			}
			if err := st.WriteInternal(ctx, func(tx *InternalTx) error {
				return tx.RecordUpdateDrainTransition(ctx, want)
			}); err != nil {
				t.Fatal(err)
			}
			var (
				got   domain.UpdateDrainTransition
				found bool
			)
			if err := st.Read(ctx, func(tx *ReadTx) error {
				var err error
				got, found, err = tx.latestUpdateDrainTransition(ctx)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if !found || got != want {
				t.Fatalf("round trip = %+v (found %v), want %+v", got, found, want)
			}
		})
	}
}
