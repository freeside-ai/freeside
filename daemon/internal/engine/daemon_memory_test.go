package engine

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

const daemonMemoryBudgetMiB = 4096

func newDaemonMemory(t *testing.T, budgetMiB int) *DaemonMemory {
	t.Helper()
	return &DaemonMemory{
		Store: storetest.Open(t, filepath.Join(t.TempDir(), "state.db"), store.Options{}),
		Bound: ward.LaunchBound{BudgetMiB: budgetMiB, RuntimeVersion: ward.RuntimeNone},
		Now:   time.Now,
	}
}

func daemonWork(id domain.InvocationID, memoryMiB int) DaemonWork {
	return DaemonWork{
		Kind: domain.ReservationProofPass, ID: id,
		Size: ward.ContainerSize{CPUs: 1, MemoryMiB: memoryMiB}, VMs: 1,
	}
}

// storedOpen is every daemon reservation the store holds open, held by this
// process or not.
func storedOpen(t *testing.T, m *DaemonMemory) []domain.LaunchReservation {
	t.Helper()
	var open []domain.LaunchReservation
	if err := m.Store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		open, err = tx.ListOpenDaemonReservations(t.Context())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return open
}

func openReservation(t *testing.T, m *DaemonMemory, id domain.InvocationID) bool {
	t.Helper()
	for _, r := range storedOpen(t, m) {
		if r.InvocationID == id {
			return true
		}
	}
	return false
}

func waiting(t *testing.T, m *DaemonMemory, id domain.InvocationID) (bool, time.Time) {
	t.Helper()
	var (
		refusedAt time.Time
		found     bool
	)
	if err := m.Store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		refusedAt, found, err = tx.LaunchWaitRefusedAt(t.Context(), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return found, refusedAt
}

// TestDaemonMemoryWaitsUntilTheHolderReleases: a waiting reservation that
// does not fit beside a holder waits, refreshes its wait inside the lease for
// as long as the holder keeps its memory, and is granted once the holder
// proves its containers gone, leaving no wait behind.
func TestDaemonMemoryWaitsUntilTheHolderReleases(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m := newDaemonMemory(t, daemonMemoryBudgetMiB)
		holder, err := m.TryReserve(t.Context(), daemonWork("holder", 3072))
		if err != nil || !holder.Held() {
			t.Fatalf("holder = %+v, %v", holder, err)
		}
		type result struct {
			reservation DaemonReservation
			err         error
		}
		done := make(chan result, 1)
		go func() {
			r, err := m.Wait(t.Context(), daemonWork("waiter", 2048))
			done <- result{r, err}
		}()
		time.Sleep(20 * time.Minute)
		synctest.Wait()
		found, refusedAt := waiting(t, m, "waiter")
		if !found || time.Since(refusedAt) > store.LaunchWaitRefresh {
			t.Fatalf("wait after 20 minutes = %t, refused %s ago; want it refreshed inside %s",
				found, time.Since(refusedAt), store.LaunchWaitRefresh)
		}
		select {
		case r := <-done:
			t.Fatalf("waiter returned while the holder holds: %+v, %v", r.reservation, r.err)
		default:
		}
		if err := holder.Release(t.Context(), nil); err != nil {
			t.Fatal(err)
		}
		r := <-done
		if r.err != nil || r.reservation.ID() != "waiter" {
			t.Fatalf("waiter = %+v, %v; want it granted", r.reservation, r.err)
		}
		if found, _ := waiting(t, m, "waiter"); found {
			t.Fatal("a granted reservation left its wait")
		}
		if openReservation(t, m, "holder") || !openReservation(t, m, "waiter") {
			t.Fatal("want only the waiter's reservation open")
		}
	})
}

// TestDaemonMemoryTryReserveNeverWaits: a reservation refused at once joins
// no order, so a launch that asks next is not held behind it.
func TestDaemonMemoryTryReserveNeverWaits(t *testing.T) {
	t.Parallel()
	m := newDaemonMemory(t, daemonMemoryBudgetMiB)
	if _, err := m.TryReserve(t.Context(), daemonWork("holder", 3072)); err != nil {
		t.Fatal(err)
	}
	r, err := m.TryReserve(t.Context(), daemonWork("refused", 2048))
	if !errors.Is(err, domain.ErrMachineCapacity) || r.Held() {
		t.Fatalf("refused = %+v, %v; want %v", r, err, domain.ErrMachineCapacity)
	}
	if found, _ := waiting(t, m, "refused"); found {
		t.Fatal("a reservation refused at once left a wait")
	}
}

// TestDaemonMemoryRefusesAReservationThatCanNeverFit: a waiting reservation
// larger than the whole budget is refused at once rather than waiting for
// memory that cannot free.
func TestDaemonMemoryRefusesAReservationThatCanNeverFit(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m := newDaemonMemory(t, daemonMemoryBudgetMiB)
		var never *domain.LaunchNeverFitsError
		if _, err := m.Wait(t.Context(), daemonWork("huge", daemonMemoryBudgetMiB+1)); !errors.As(err, &never) ||
			errors.Is(err, domain.ErrMachineCapacity) {
			t.Fatalf("huge = %v, want only a never-fits refusal", err)
		}
		if found, _ := waiting(t, m, "huge"); found {
			t.Fatal("a reservation that can never fit left a wait")
		}
	})
}

// TestDaemonMemoryWaitLeavesTheOrderWhenCanceled: a waiter whose context ends
// returns its cause and removes its wait, so it holds back no launch.
func TestDaemonMemoryWaitLeavesTheOrderWhenCanceled(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m := newDaemonMemory(t, daemonMemoryBudgetMiB)
		if _, err := m.TryReserve(t.Context(), daemonWork("holder", 3072)); err != nil {
			t.Fatal(err)
		}
		stop := errors.New("daemon stopping")
		ctx, cancel := context.WithCancelCause(t.Context())
		done := make(chan error, 1)
		go func() {
			_, err := m.Wait(ctx, daemonWork("waiter", 2048))
			done <- err
		}()
		time.Sleep(time.Minute)
		synctest.Wait()
		if found, _ := waiting(t, m, "waiter"); !found {
			t.Fatal("the waiter recorded no wait")
		}
		cancel(stop)
		if err := <-done; !errors.Is(err, stop) {
			t.Fatalf("canceled waiter = %v, want %v", err, stop)
		}
		if found, _ := waiting(t, m, "waiter"); found {
			t.Fatal("a canceled waiter left its wait")
		}
	})
}

// TestDaemonMemoryKeepsAnUnprovenReservation: a release whose proof failed
// keeps the reservation open, so the budget still counts the memory an
// unproven container may hold; a release with proof ends it.
func TestDaemonMemoryKeepsAnUnprovenReservation(t *testing.T) {
	t.Parallel()
	m := newDaemonMemory(t, daemonMemoryBudgetMiB)
	r, err := m.TryReserve(t.Context(), daemonWork("pass", 2048))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Release(t.Context(), errors.New("container still listed")); err != nil {
		t.Fatal(err)
	}
	if !openReservation(t, m, "pass") {
		t.Fatal("an unproven release ended the reservation")
	}
	if err := m.Recovered("pass").Release(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if openReservation(t, m, "pass") {
		t.Fatal("a proved release kept the reservation")
	}
}

// TestDaemonMemoryWithoutABudgetReservesNothing: with no budget, and with no
// DaemonMemory at all, work reserves nothing and its context carries no
// reservation.
func TestDaemonMemoryWithoutABudgetReservesNothing(t *testing.T) {
	t.Parallel()
	m := newDaemonMemory(t, 0)
	for name, memory := range map[string]*DaemonMemory{"no budget": m, "nil": nil} {
		r, err := memory.Wait(t.Context(), daemonWork("pass", 2048))
		if err != nil || r.Held() {
			t.Fatalf("%s: reservation = %+v, %v", name, r, err)
		}
		if _, ok := ward.LaunchReservationFrom(r.Context(t.Context())); ok {
			t.Fatalf("%s: the context carries a reservation", name)
		}
		if err := r.Release(t.Context(), nil); err != nil {
			t.Fatalf("%s: release = %v", name, err)
		}
	}
	if openReservation(t, m, "pass") {
		t.Fatal("unbudgeted work wrote a reservation")
	}
}

// TestDaemonMemoryWaitLeavesTheOrderWhenRefusedForGood: a waiter the budget
// later refuses for good, lowered below its size while it waits, returns the
// refusal and removes its wait, which would otherwise hold back every task
// launch until its lease lapsed.
func TestDaemonMemoryWaitLeavesTheOrderWhenRefusedForGood(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		m := newDaemonMemory(t, daemonMemoryBudgetMiB)
		if _, err := m.TryReserve(t.Context(), daemonWork("holder", 3072)); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := m.Wait(t.Context(), daemonWork("waiter", 2048))
			done <- err
		}()
		time.Sleep(time.Minute)
		synctest.Wait()
		if found, _ := waiting(t, m, "waiter"); !found {
			t.Fatal("the waiter recorded no wait")
		}
		recordBudget(t, m.Store, 1024)
		var never *domain.LaunchNeverFitsError
		if err := <-done; !errors.As(err, &never) {
			t.Fatalf("waiter under a lowered budget = %v, want a never-fits refusal", err)
		}
		if found, _ := waiting(t, m, "waiter"); found {
			t.Fatal("a waiter refused for good left its wait")
		}
	})
}
