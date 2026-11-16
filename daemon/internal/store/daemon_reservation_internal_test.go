package store

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// daemonLaunch is a reservation for the daemon's own work declaring
// memoryMiB under one 256 MiB overhead.
func daemonLaunch(id string, kind domain.LaunchReservationKind, memoryMiB int) domain.LaunchReservation {
	return domain.LaunchReservation{
		InvocationID: domain.InvocationID(id), Kind: kind, MemoryMiB: memoryMiB, OverheadMiB: 256,
	}
}

// refuseDaemonAt reserves r at at, wants a machine capacity refusal, and
// records the daemon wait as the waiting helper does after one.
func refuseDaemonAt(t *testing.T, st *Store, r domain.LaunchReservation, at time.Time) domain.MachineCapacityHold {
	t.Helper()
	var refusal *domain.MachineCapacityError
	if err := reserveAt(t, st, r, at); !errors.As(err, &refusal) {
		t.Fatalf("reserve %s = %v, want a machine capacity refusal", r.InvocationID, err)
	}
	if err := st.Write(t.Context(), func(tx *WriteTx) error {
		return tx.RecordDaemonLaunchWait(t.Context(), r.InvocationID, at)
	}); err != nil {
		t.Fatal(err)
	}
	return refusal.Hold
}

func releaseDaemon(t *testing.T, st *Store, r domain.LaunchReservation, at time.Time) {
	t.Helper()
	if err := st.Write(t.Context(), func(tx *WriteTx) error {
		return tx.ReleaseDaemonReservation(t.Context(), r.InvocationID, at)
	}); err != nil {
		t.Fatal(err)
	}
}

// TestMachineCapacityGrantsADaemonWaitBeforeEarlierTasks: with the budget
// full and a task launch waiting, a daemon reservation that asks later is
// granted first when memory frees.
func TestMachineCapacityGrantsADaemonWaitBeforeEarlierTasks(t *testing.T) {
	t.Parallel()
	st := openSlotStore(t)
	seedCapacityRuns(t, st, "1-holder", "2-waiting")
	holder, task := verificationLaunch("1-holder", 3072), verificationLaunch("2-waiting", 1024)
	pass := daemonLaunch("proof-pass-1", domain.ReservationProofPass, 2048)
	if err := reserve(t, st, holder); err != nil {
		t.Fatal(err)
	}
	refuseAndWait(t, st, task)
	if got := refuseDaemonAt(t, st, pass, slotAt.Add(time.Second)); got.Ahead != 0 {
		t.Fatalf("daemon hold = %+v, want none ahead of it: the waiting task is behind it", got)
	}
	releaseVerification(t, st, holder)
	freed := slotAt.Add(2 * time.Second)
	var refusal *domain.MachineCapacityError
	if err := reserveAt(t, st, task, freed); !errors.As(err, &refusal) || refusal.Hold.Ahead != 1 {
		t.Fatalf("earlier task once memory frees = %v, want a refusal with the daemon wait ahead", err)
	}
	if err := reserveAt(t, st, pass, freed); err != nil {
		t.Fatalf("daemon reservation once memory frees: %v", err)
	}
	if err := reserveAt(t, st, task, freed); err != nil {
		t.Fatalf("task once the daemon reservation holds: %v", err)
	}
}

// TestMachineCapacityTaskDoesNotPassADaemonWait: a task launch that would fit
// stays behind a daemon reservation that is waiting.
func TestMachineCapacityTaskDoesNotPassADaemonWait(t *testing.T) {
	t.Parallel()
	st := openSlotStore(t)
	seedCapacityRuns(t, st, "1-holder", "2-small")
	holder, small := verificationLaunch("1-holder", 2048), verificationLaunch("2-small", 1024)
	build := daemonLaunch("image-build-1", domain.ReservationImageBuild, 2048)
	if err := reserve(t, st, holder); err != nil {
		t.Fatal(err)
	}
	refuseDaemonAt(t, st, build, slotAt)
	want := domain.MachineCapacityHold{ReservationMiB: 1280, ReservedMiB: 2304, BudgetMiB: capacityBudgetMiB, Ahead: 1}
	if got := refuseAndWait(t, st, small); got != want {
		t.Fatalf("small task's hold = %+v, want %+v: it fits, but the daemon waits ahead", got, want)
	}
}

// TestMachineCapacityGrantsDaemonWaitsInTheOrderTheyAsked: two waiting daemon
// reservations are granted in the order they first asked, and a refresh keeps
// a wait's place.
func TestMachineCapacityGrantsDaemonWaitsInTheOrderTheyAsked(t *testing.T) {
	t.Parallel()
	st := openSlotStore(t)
	seedCapacityRuns(t, st, "1-holder")
	holder := verificationLaunch("1-holder", 3072)
	// The later one sorts first by id, so only the recorded order can put the
	// first one ahead.
	first := daemonLaunch("z-probe", domain.ReservationProbe, 1024)
	second := daemonLaunch("a-proof-pass", domain.ReservationProofPass, 1024)
	if err := reserve(t, st, holder); err != nil {
		t.Fatal(err)
	}
	refuseDaemonAt(t, st, first, slotAt)
	if got := refuseDaemonAt(t, st, second, slotAt.Add(time.Second)); got.Ahead != 1 {
		t.Fatalf("second daemon hold = %+v, want the first ahead", got)
	}
	// The first refreshes after the second asked and keeps its place.
	if got := refuseDaemonAt(t, st, first, slotAt.Add(2*time.Second)); got.Ahead != 0 {
		t.Fatalf("first daemon hold after a refresh = %+v, want none ahead", got)
	}
	releaseVerification(t, st, holder)
	freed := slotAt.Add(3 * time.Second)
	var refusal *domain.MachineCapacityError
	if err := reserveAt(t, st, second, freed); !errors.As(err, &refusal) || refusal.Hold.Ahead != 1 {
		t.Fatalf("second once memory frees = %v, want a refusal with the first ahead", err)
	}
	if err := reserveAt(t, st, first, freed); err != nil {
		t.Fatalf("first once memory frees: %v", err)
	}
	if err := reserveAt(t, st, second, freed); err != nil {
		t.Fatalf("second once the first holds: %v", err)
	}
}

// TestMachineCapacityRefusesADaemonReservationThatCanNeverFit: a daemon
// reservation over the whole budget is refused at once naming the size, the
// overhead, and the budget, and never waits.
func TestMachineCapacityRefusesADaemonReservationThatCanNeverFit(t *testing.T) {
	t.Parallel()
	st := openSlotStore(t)
	pass := domain.LaunchReservation{
		InvocationID: "proof-pass-huge", Kind: domain.ReservationProofPass, MemoryMiB: 4096, OverheadMiB: 512,
	}
	var never *domain.LaunchNeverFitsError
	if err := reserve(t, st, pass); !errors.As(err, &never) || errors.Is(err, domain.ErrMachineCapacity) {
		t.Fatalf("reserve = %v, want a never-fits refusal and no wait", err)
	}
	if *never != (domain.LaunchNeverFitsError{SizeMiB: 4096, OverheadMiB: 512, BudgetMiB: capacityBudgetMiB}) {
		t.Fatalf("refusal = %+v", *never)
	}
}

// TestDaemonReservationsRoundTripEveryKind: a reservation of every registered
// kind is stored and read back, a daemon kind holds until its explicit
// release, and only daemon kinds are listed as the daemon's.
func TestDaemonReservationsRoundTripEveryKind(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st := openSlotStore(t)
	var stored []domain.LaunchReservation
	for _, kind := range domain.AllLaunchReservationKinds {
		r := domain.LaunchReservation{
			InvocationID: domain.InvocationID("inv-" + string(kind)), Kind: kind, MemoryMiB: 256, OverheadMiB: 256,
		}
		if kind.ForRun() {
			r.RunID = "run-1"
		}
		stored = append(stored, r)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		for _, r := range stored {
			if err := tx.putLaunchReservation(ctx, r, slotAt); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var daemon []domain.LaunchReservation
	if err := st.Read(ctx, func(tx *ReadTx) error {
		for _, want := range stored {
			got, found, err := tx.getLaunchReservation(ctx, want.InvocationID)
			if err != nil || !found || got != want {
				t.Errorf("%s read back %+v, %t, %v", want.Kind, got, found, err)
			}
		}
		var err error
		daemon, err = tx.ListOpenDaemonReservations(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var wantDaemon []domain.LaunchReservation
	for _, r := range stored {
		if !r.Kind.ForRun() {
			wantDaemon = append(wantDaemon, r)
		}
	}
	slices.SortFunc(wantDaemon, func(a, b domain.LaunchReservation) int {
		return cmp.Compare(a.InvocationID, b.InvocationID)
	})
	if !slices.Equal(daemon, wantDaemon) {
		t.Fatalf("open daemon reservations = %+v, want %+v", daemon, wantDaemon)
	}
}

// TestDaemonReservationHoldsUntilItsRelease: a daemon reservation counts
// against the budget until its explicit release, and the daemon release
// leaves a run's reservation alone.
func TestDaemonReservationHoldsUntilItsRelease(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st := openSlotStore(t)
	seedCapacityRuns(t, st, "1-run")
	probe := daemonLaunch("probe-1", domain.ReservationProbe, 1024)
	verification := verificationLaunch("1-run", 1024)
	for _, r := range []domain.LaunchReservation{probe, verification} {
		if err := reserve(t, st, r); err != nil {
			t.Fatalf("reserve %s: %v", r.Kind, err)
		}
	}
	holders := func() []domain.InvocationID {
		t.Helper()
		var held []domain.InvocationID
		if err := st.Write(ctx, func(tx *WriteTx) error {
			var err error
			_, held, err = tx.reservedMemory(ctx, slotAt.Add(time.Minute))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return held
	}
	if got := holders(); !slices.Contains(got, probe.InvocationID) {
		t.Fatalf("holders before release = %v, want the probe", got)
	}
	releaseDaemon(t, st, probe, slotAt.Add(time.Minute))
	releaseDaemon(t, st, verification, slotAt.Add(time.Minute))
	if got := holders(); slices.Contains(got, probe.InvocationID) || !slices.Contains(got, verification.InvocationID) {
		t.Fatalf("holders after release = %v, want the verification only", got)
	}
}

// TestDaemonLaunchWaitsLapseAndClear: a daemon wait whose waiter stops
// refreshing it lapses with the lease, a publication pass's keepalive never
// extends one, and a start clears the daemon waits an earlier process left
// and keeps every task launch's.
func TestDaemonLaunchWaitsLapseAndClear(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st := openSlotStore(t)
	seedCapacityRuns(t, st, "1-holder", "2-small", "3-waiting")
	holder, small := verificationLaunch("1-holder", 2048), verificationLaunch("2-small", 1024)
	pass := daemonLaunch("proof-pass-1", domain.ReservationProofPass, 2048)
	if err := reserve(t, st, holder); err != nil {
		t.Fatal(err)
	}
	refuseDaemonAt(t, st, pass, slotAt)
	keepWaits(t, st, slotAt, slotAt.Add(launchWaitLease-time.Second))
	if err := reserveAt(t, st, small, slotAt.Add(launchWaitLease)); err != nil {
		t.Fatalf("task behind a lapsed daemon wait that a keepalive did not extend: %v", err)
	}

	refuseDaemonAt(t, st, daemonLaunch("proof-pass-2", domain.ReservationProofPass, 2048), slotAt.Add(launchWaitLease))
	task := verificationLaunch("3-waiting", 2048)
	refuseAndWait(t, st, task)
	if err := st.Write(ctx, func(tx *WriteTx) error { return tx.ClearDaemonLaunchWaits(ctx) }); err != nil {
		t.Fatal(err)
	}
	if err := st.Read(ctx, func(tx *ReadTx) error {
		waits, err := tx.listLaunchWaits(ctx)
		if err == nil && (len(waits) != 1 || waits[0].invocation != task.InvocationID) {
			err = fmt.Errorf("waits after the start cleared the daemon's = %+v, want only the task's", waits)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// TestMachineCapacityGrantsAnOpenDaemonReservationAgain: re-reserving a daemon
// reservation that is still open, as a build that reuses a running registry
// does, passes as already held even with another daemon wait ahead of it, and
// counts its memory once.
func TestMachineCapacityGrantsAnOpenDaemonReservationAgain(t *testing.T) {
	t.Parallel()
	st := openSlotStore(t)
	registry := daemonLaunch("image-registry-5101", domain.ReservationImageRegistry, 1024)
	if err := reserve(t, st, registry); err != nil {
		t.Fatal(err)
	}
	refuseDaemonAt(t, st, daemonLaunch("proof-pass-1", domain.ReservationProofPass, capacityBudgetMiB-1024), slotAt)
	if err := reserveAt(t, st, registry, slotAt.Add(time.Minute)); err != nil {
		t.Fatalf("re-reserve an open registry behind a daemon wait = %v, want it held", err)
	}
}
