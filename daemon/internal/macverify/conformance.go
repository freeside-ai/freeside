package macverify

import (
	"context"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Confinement names one of the confinements the conformance pass and the
// pre-job probe prove of a slot. The zero value "" is invalid by design
// (domain enum convention). The offline dependency cache, the fourth
// confinement doc.go names, is enforced per job by the room and is not one.
type Confinement string

const (
	// ConfinementSandboxNetwork is the sandbox profile's network denial: a
	// loopback connection, the system resolver, and off-host TCP and UDP are
	// all refused, each with the sandbox's EPERM.
	ConfinementSandboxNetwork Confinement = "sandbox_network"
	// ConfinementPacketFilter is the second egress layer: with the sandbox
	// off, the packet-filter anchor keyed on the slot still stops off-host
	// TCP and UDP.
	ConfinementPacketFilter Confinement = "packet_filter"
	// ConfinementWriteConfinement is the sandbox profile's write rule: a
	// write outside the job directory is refused with EPERM, and a write
	// inside it succeeds.
	ConfinementWriteConfinement Confinement = "write_confinement"
	// ConfinementHomeReset is the helper's home reset: a file a job leaves
	// in the slot's home is gone in the slot's next job.
	ConfinementHomeReset Confinement = "home_reset"
)

// AllConfinements lists every valid Confinement.
var AllConfinements = []Confinement{
	ConfinementSandboxNetwork, ConfinementPacketFilter, ConfinementWriteConfinement, ConfinementHomeReset,
}

func (c Confinement) valid() bool {
	switch c {
	case ConfinementSandboxNetwork, ConfinementPacketFilter, ConfinementWriteConfinement, ConfinementHomeReset:
		return true
	default:
		return false
	}
}

// ErrConformance is the class sentinel for a confinement the executor could
// not prove. ConformanceFailure unwraps to it, so errors.Is matches the class
// while errors.As reaches the confinement and slot.
var ErrConformance = errors.New("macverify: confinement unproven")

// ConformanceFailure is the typed failure of one confinement: a probe check
// that failed, a probe report that did not prove its checks, or a positive
// control that failed as the daemon user. Slot is empty for a control, which
// runs outside every slot.
type ConformanceFailure struct {
	Confinement Confinement
	Slot        string
	Reason      string
}

func (e *ConformanceFailure) Error() string {
	if e.Slot == "" {
		return fmt.Sprintf("macverify: %s confinement unproven: %s", e.Confinement, e.Reason)
	}
	return fmt.Sprintf("macverify: slot %s failed the %s confinement: %s", e.Slot, e.Confinement, e.Reason)
}

// Unwrap makes errors.Is(err, ErrConformance) match the class.
func (e *ConformanceFailure) Unwrap() error { return ErrConformance }

// ConformanceRecorder persists a completed pass's BackendConformance record.
// Its method set is the ward's, so the daemon's store-backed recorder serves
// both; macverify defines it rather than importing the store.
type ConformanceRecorder interface {
	RecordBackendConformance(ctx context.Context, record domain.BackendConformance) error
}
