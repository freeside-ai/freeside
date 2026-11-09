package macverify

import (
	"errors"
	"testing"
)

func TestSlotPool(t *testing.T) {
	t.Parallel()
	pool := newSlotPool([]string{"slot1", "slot2"})
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
