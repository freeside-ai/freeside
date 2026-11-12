package store

import (
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestRestartReadinessCountsHeldReviewSlot: a review admission holding its
// slot blocks a restart, and once its slot is released it no longer does.
func TestRestartReadinessCountsHeldReviewSlot(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st, admission := reviewSlotFixture(t)
	if err := st.WriteInternal(ctx, func(tx *InternalTx) error {
		return tx.RecordUpdateDrainTransition(ctx, domain.UpdateDrainTransition{
			State:      domain.UpdateDrainDraining,
			Build:      "sha256:2222222222222222222222222222222222222222222222222222222222222222",
			OccurredAt: slotAt,
		})
	}); err != nil {
		t.Fatal(err)
	}
	read := func() domain.RestartReadiness {
		t.Helper()
		var readiness domain.RestartReadiness
		if err := st.Read(ctx, func(tx *ReadTx) error {
			var err error
			readiness, err = tx.RestartReadiness(ctx, slotAt.Add(time.Minute))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return readiness
	}
	want := []domain.RestartBlocker{{Kind: domain.RestartBlockerReviewLaunch, Count: 1}}
	if got := read(); got.Ready || !reflect.DeepEqual(got.Blockers, want) {
		t.Fatalf("readiness = %+v, want not ready with blockers %+v", got, want)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		return tx.ReleaseReviewSlot(ctx, admission.InvocationID, slotAt)
	}); err != nil {
		t.Fatal(err)
	}
	if got := read(); !got.Ready {
		t.Fatalf("readiness after the review slot was released = %+v, want ready", got)
	}
}
