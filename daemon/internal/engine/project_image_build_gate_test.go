package engine

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
)

// At a limit of one the gate hands itself over oldest waiter first, a
// publication rebuild's try never queues, and a waiter whose pass ends
// leaves the queue.
func TestProjectImageBuildGateIsFIFO(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var gate projectImageBuildGate
		if !gate.tryAcquire(1) {
			t.Fatal("an open gate refused a try")
		}
		var order []string
		wait := func(name string, ctx context.Context) chan error {
			done := make(chan error, 1)
			go func() {
				err := gate.acquire(ctx, 1)
				if err == nil {
					order = append(order, name)
				}
				done <- err
			}()
			synctest.Wait()
			return done
		}
		cancelled, cancel := context.WithCancel(t.Context())
		first := wait("first", t.Context())
		gone := wait("gone", cancelled)
		second := wait("second", t.Context())
		if gate.tryAcquire(1) {
			t.Fatal("a try took a held gate")
		}
		cancel()
		if err := <-gone; !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled waiter = %v, want its context's error", err)
		}
		gate.release()
		if err := <-first; err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if len(order) != 1 {
			t.Fatalf("order = %v, want only the first waiter holding", order)
		}
		gate.release()
		if err := <-second; err != nil {
			t.Fatal(err)
		}
		gate.release()
		if len(order) != 2 || order[0] != "first" || order[1] != "second" || !gate.tryAcquire(1) {
			t.Fatalf("order = %v, want first then second, then an open gate", order)
		}
	})
}

// At a limit of two, two builds hold slots at once, a third waits and a try
// is refused while it does, and a release hands the waiter the freed slot.
func TestProjectImageBuildGateHoldsUpToItsLimit(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var gate projectImageBuildGate
		for range 2 {
			if err := gate.acquire(t.Context(), 2); err != nil {
				t.Fatal(err)
			}
		}
		third := make(chan error, 1)
		go func() { third <- gate.acquire(t.Context(), 2) }()
		synctest.Wait()
		select {
		case err := <-third:
			t.Fatalf("a third build took a slot past the limit: %v", err)
		default:
		}
		gate.release()
		if gate.tryAcquire(2) {
			t.Fatal("a try took the slot a release handed to the waiter")
		}
		if err := <-third; err != nil {
			t.Fatal(err)
		}
		gate.release()
		if !gate.tryAcquire(2) {
			t.Fatal("a try was refused a free slot")
		}
	})
}
