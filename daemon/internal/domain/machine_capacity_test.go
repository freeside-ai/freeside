package domain

import (
	"errors"
	"testing"
	"time"
)

// TestMachineCapacityHoldFactMatchesItsReason: the fact rides exactly beside
// a machine_capacity hold, and it must describe a launch that is actually
// waiting: one that could never fit is a refusal, not a hold.
func TestMachineCapacityHoldFactMatchesItsReason(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	hold := func(reason RunHoldReason, fact *MachineCapacityHold) RunHoldObservation {
		return RunHoldObservation{
			RunID: "run-1", Reason: reason, MachineCapacity: fact,
			FirstObservedAt: at, LastObservedAt: at,
		}
	}
	waiting := MachineCapacityHold{ReservationMiB: 2304, ReservedMiB: 2304, BudgetMiB: 4096}
	for name, tc := range map[string]struct {
		hold RunHoldObservation
		want error
	}{
		"memory full":           {hold(HoldMachineCapacity, &waiting), nil},
		"fits but one is ahead": {hold(HoldMachineCapacity, &MachineCapacityHold{ReservationMiB: 1280, BudgetMiB: 4096, Ahead: 1}), nil},
		"reason without fact":   {hold(HoldMachineCapacity, nil), ErrHoldDetailMismatch},
		"fact beside another":   {hold(HoldUsagePoolExhausted, &waiting), ErrHoldDetailMismatch},
		"fits with none ahead":  {hold(HoldMachineCapacity, &MachineCapacityHold{ReservationMiB: 1280, BudgetMiB: 4096}), ErrHoldDetailMismatch},
		"never fits":            {hold(HoldMachineCapacity, &MachineCapacityHold{ReservationMiB: 5000, BudgetMiB: 4096}), ErrLaunchNeverFits},
		"no budget":             {hold(HoldMachineCapacity, &MachineCapacityHold{ReservationMiB: 1280}), ErrNonPositive},
		"negative ahead":        {hold(HoldMachineCapacity, &MachineCapacityHold{ReservationMiB: 1280, ReservedMiB: 4096, BudgetMiB: 4096, Ahead: -1}), ErrNegativeSize},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := tc.hold.Validate()
			if tc.want == nil && err != nil || tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("Validate = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestLaunchReservationValidate: a reservation names its launch, a known
// kind, a positive declared size, and a non-negative overhead.
func TestLaunchReservationValidate(t *testing.T) {
	t.Parallel()
	valid := LaunchReservation{
		InvocationID: "inv-1", RunID: "run-1", Kind: ReservationExecution, MemoryMiB: 4096, OverheadMiB: 256,
	}
	if err := valid.Validate(); err != nil || valid.TotalMiB() != 4352 {
		t.Fatalf("valid reservation: %v, total %d", err, valid.TotalMiB())
	}
	for name, tc := range map[string]struct {
		mutate func(*LaunchReservation)
		want   error
	}{
		"no invocation":     {func(r *LaunchReservation) { r.InvocationID = "" }, ErrEmptyID},
		"no run":            {func(r *LaunchReservation) { r.RunID = "" }, ErrEmptyID},
		"unknown kind":      {func(r *LaunchReservation) { r.Kind = "" }, ErrInvalidLaunchReservationKind},
		"no memory":         {func(r *LaunchReservation) { r.MemoryMiB = 0 }, ErrNonPositive},
		"negative overhead": {func(r *LaunchReservation) { r.OverheadMiB = -1 }, ErrNegativeSize},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := valid
			tc.mutate(&r)
			if err := r.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Validate = %v, want %v", err, tc.want)
			}
		})
	}
}
