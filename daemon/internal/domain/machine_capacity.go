package domain

import "fmt"

// LaunchReservationKind names the record whose end releases a launch's memory
// reservation (plan §5.4): an execution or review admission releases with the
// usage-pool slot it holds, and a verification job, which takes no slot,
// releases explicitly once its containers are proved absent. The zero value ""
// is invalid by design.
type LaunchReservationKind string

const (
	// ReservationExecution is a writer launch behind an execution admission.
	ReservationExecution LaunchReservationKind = "execution"
	// ReservationReview is a review launch behind a review admission.
	ReservationReview LaunchReservationKind = "review"
	// ReservationVerification is a verification job's room.
	ReservationVerification LaunchReservationKind = "verification"
)

// AllLaunchReservationKinds is the single registration point for kinds.
var AllLaunchReservationKinds = []LaunchReservationKind{
	ReservationExecution, ReservationReview, ReservationVerification,
}

func (k LaunchReservationKind) valid() bool {
	switch k {
	case ReservationExecution, ReservationReview, ReservationVerification:
		return true
	default:
		return false
	}
}

// LaunchReservation is the memory one launch or job reserves against the
// instance budget: its declared memory plus one per-VM overhead, because its
// containers run one at a time.
type LaunchReservation struct {
	InvocationID InvocationID          `json:"invocation_id"`
	RunID        RunID                 `json:"run_id"`
	Kind         LaunchReservationKind `json:"kind"`
	MemoryMiB    int                   `json:"memory_mib"`
	OverheadMiB  int                   `json:"overhead_mib"`
}

// TotalMiB is what the reservation counts against the budget.
func (r LaunchReservation) TotalMiB() int { return r.MemoryMiB + r.OverheadMiB }

// Validate reports whether the reservation names its launch and a positive
// memory size; the overhead is zero only for a composition that starts no VM.
func (r LaunchReservation) Validate() error {
	if r.InvocationID == "" {
		return fmt.Errorf("launch reservation invocation_id: %w", ErrEmptyID)
	}
	if r.RunID == "" {
		return fmt.Errorf("launch reservation %s run_id: %w", r.InvocationID, ErrEmptyID)
	}
	if !r.Kind.valid() {
		return fmt.Errorf("launch reservation %s kind %q: %w", r.InvocationID, r.Kind, ErrInvalidLaunchReservationKind)
	}
	if r.MemoryMiB < 1 {
		return fmt.Errorf("launch reservation %s memory_mib %d: %w", r.InvocationID, r.MemoryMiB, ErrNonPositive)
	}
	if r.OverheadMiB < 0 {
		return fmt.Errorf("launch reservation %s overhead_mib %d: %w", r.InvocationID, r.OverheadMiB, ErrNegativeSize)
	}
	return nil
}

// MachineCapacityHold is the typed fact beside a machine_capacity hold: the
// waiting launch's reservation, the memory other launches hold, the budget,
// and how many older tasks' launches wait ahead. A hold exists only for a
// launch that can fit the budget once memory frees and that is either behind
// an older launch or does not fit now; anything else is no wait.
type MachineCapacityHold struct {
	ReservationMiB int `json:"reservation_mib"`
	ReservedMiB    int `json:"reserved_mib"`
	BudgetMiB      int `json:"budget_mib"`
	Ahead          int `json:"ahead"`
}

// Validate reports whether the numbers describe a wait that can end.
func (h MachineCapacityHold) Validate() error {
	switch {
	case h.ReservationMiB < 1 || h.BudgetMiB < 1:
		return fmt.Errorf("machine capacity hold reservation %d MiB, budget %d MiB: %w",
			h.ReservationMiB, h.BudgetMiB, ErrNonPositive)
	case h.ReservedMiB < 0 || h.Ahead < 0:
		return fmt.Errorf("machine capacity hold reserved %d MiB, %d ahead: %w",
			h.ReservedMiB, h.Ahead, ErrNegativeSize)
	case h.ReservationMiB > h.BudgetMiB:
		return fmt.Errorf("machine capacity hold reservation %d MiB exceeds budget %d MiB: %w",
			h.ReservationMiB, h.BudgetMiB, ErrLaunchNeverFits)
	case h.Ahead == 0 && h.ReservedMiB+h.ReservationMiB <= h.BudgetMiB:
		return fmt.Errorf("machine capacity hold of %d MiB fits %d of %d MiB with none ahead: %w",
			h.ReservationMiB, h.ReservedMiB, h.BudgetMiB, ErrHoldDetailMismatch)
	}
	return nil
}

// MachineCapacityError is an ErrMachineCapacity refusal with the fact the
// run's machine_capacity hold records.
type MachineCapacityError struct {
	Hold MachineCapacityHold
}

func (e *MachineCapacityError) Error() string {
	return fmt.Sprintf("%s: %d MiB with %d of %d MiB reserved and %d launches ahead",
		ErrMachineCapacity, e.Hold.ReservationMiB, e.Hold.ReservedMiB, e.Hold.BudgetMiB, e.Hold.Ahead)
}

// Is matches ErrMachineCapacity.
func (e *MachineCapacityError) Is(target error) bool { return target == ErrMachineCapacity }

// LaunchNeverFitsError is an ErrLaunchNeverFits refusal naming the three
// numbers the operator needs: the declared size, the per-VM overhead, and the
// budget their sum exceeds.
type LaunchNeverFitsError struct {
	SizeMiB     int
	OverheadMiB int
	BudgetMiB   int
}

func (e *LaunchNeverFitsError) Error() string {
	return fmt.Sprintf("%s: %d MiB declared plus %d MiB per-VM overhead exceeds the %d MiB budget",
		ErrLaunchNeverFits, e.SizeMiB, e.OverheadMiB, e.BudgetMiB)
}

// Is matches ErrLaunchNeverFits.
func (e *LaunchNeverFitsError) Is(target error) bool { return target == ErrLaunchNeverFits }

// RuntimeOverheadUnknownError is an ErrRuntimeOverheadUnknown refusal naming
// the runtime version that has no recorded overhead.
type RuntimeOverheadUnknownError struct {
	RuntimeVersion string
}

func (e *RuntimeOverheadUnknownError) Error() string {
	return fmt.Sprintf("%s: %s", ErrRuntimeOverheadUnknown, e.RuntimeVersion)
}

// Is matches ErrRuntimeOverheadUnknown.
func (e *RuntimeOverheadUnknownError) Is(target error) bool {
	return target == ErrRuntimeOverheadUnknown
}
