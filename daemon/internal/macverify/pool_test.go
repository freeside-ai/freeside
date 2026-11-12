package macverify

import (
	"errors"
	"slices"
	"testing"
)

func TestSlotPool(t *testing.T) {
	t.Parallel()
	pool := newSlotPool("/executor", []string{"slot1", "slot2"})
	lease := func() string {
		t.Helper()
		slot, err := pool.lease()
		if err != nil {
			t.Fatalf("lease: %v", err)
		}
		return slot
	}
	if a, b := lease(), lease(); a != "slot1" || b != "slot2" {
		t.Fatalf("leased %s and %s", a, b)
	}
	if _, err := pool.lease(); !errors.Is(err, ErrNoFreeSlot) {
		t.Fatalf("lease from a full pool: %v", err)
	}
	pool.release("slot1")
	if slot := lease(); slot != "slot1" {
		t.Fatalf("leased %s after releasing slot1", slot)
	}
	pool.quarantine("slot1")
	pool.release("slot2")
	if slot := lease(); slot != "slot2" {
		t.Fatalf("leased %s, want slot2 with slot1 quarantined", slot)
	}
	pool.release("slot2")
	pool.quarantine("slot2")
	pool.release("slot1") // releasing a quarantined slot does not return it
	if _, err := pool.lease(); !errors.Is(err, ErrNoFreeSlot) {
		t.Fatalf("lease with every slot quarantined: %v", err)
	}
}

// TestSlotPoolRefusalTellsQuarantinedFromBusy pins the refusal the engine's
// notice reads (#3004): only a pool whose every slot is quarantined reports
// AllQuarantined, never one with a leased slot that will free.
func TestSlotPoolRefusalTellsQuarantinedFromBusy(t *testing.T) {
	t.Parallel()
	refusal := func(t *testing.T, pool *slotPool) *NoFreeSlotError {
		t.Helper()
		_, err := pool.lease()
		if !errors.Is(err, ErrNoFreeSlot) {
			t.Fatalf("lease = %v, want ErrNoFreeSlot", err)
		}
		var refused *NoFreeSlotError
		if !errors.As(err, &refused) {
			t.Fatalf("lease = %T, want *NoFreeSlotError", err)
		}
		if refused.ExecutorRoot != "/executor" || refused.Slots != 2 {
			t.Fatalf("refusal = %+v, want root /executor and 2 slots", refused)
		}
		return refused
	}
	leaseAll := func(t *testing.T, pool *slotPool) {
		t.Helper()
		for range 2 {
			if _, err := pool.lease(); err != nil {
				t.Fatalf("lease: %v", err)
			}
		}
	}

	t.Run("every slot quarantined", func(t *testing.T) {
		t.Parallel()
		pool := newSlotPool("/executor", []string{"slot2", "slot1"})
		leaseAll(t, pool)
		pool.quarantine("slot2")
		pool.quarantine("slot1")
		refused := refusal(t, pool)
		if !refused.AllQuarantined() || !slices.Equal(refused.Quarantined, []string{"slot1", "slot2"}) {
			t.Fatalf("refusal = %+v, want all quarantined, sorted", refused)
		}
	})
	t.Run("one leased, the rest quarantined", func(t *testing.T) {
		t.Parallel()
		pool := newSlotPool("/executor", []string{"slot1", "slot2"})
		leaseAll(t, pool)
		pool.quarantine("slot1")
		refused := refusal(t, pool)
		if refused.AllQuarantined() || !slices.Equal(refused.Quarantined, []string{"slot1"}) {
			t.Fatalf("refusal = %+v, want busy with slot1 quarantined", refused)
		}
	})
	t.Run("every slot leased", func(t *testing.T) {
		t.Parallel()
		pool := newSlotPool("/executor", []string{"slot1", "slot2"})
		leaseAll(t, pool)
		refused := refusal(t, pool)
		if refused.AllQuarantined() || len(refused.Quarantined) != 0 {
			t.Fatalf("refusal = %+v, want busy with none quarantined", refused)
		}
	})
	t.Run("an empty pool", func(t *testing.T) {
		t.Parallel()
		if (&NoFreeSlotError{}).AllQuarantined() {
			t.Fatal("an empty pool reports all quarantined")
		}
	})
}
