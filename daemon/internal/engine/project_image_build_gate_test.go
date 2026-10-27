package engine

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
)

// The gate hands itself over oldest waiter first, a publication rebuild's
// try never queues, and a waiter whose pass ends leaves the queue.
func TestProjectImageBuildGateIsFIFO(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var gate projectImageBuildGate
		if !gate.tryAcquire() {
			t.Fatal("an open gate refused a try")
		}
		var order []string
		wait := func(name string, ctx context.Context) chan error {
			done := make(chan error, 1)
			go func() {
				err := gate.acquire(ctx)
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
		if gate.tryAcquire() {
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
		if len(order) != 2 || order[0] != "first" || order[1] != "second" || !gate.tryAcquire() {
			t.Fatalf("order = %v, want first then second, then an open gate", order)
		}
	})
}
