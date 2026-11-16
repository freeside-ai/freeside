package daemoncompose

import (
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

// AppAuthority is how a Claude-mode daemon comes to hold the App authority
// writer lock it opens the authority under. At most one field is set.
type AppAuthority struct {
	// Lock is a lock the caller already holds on the Claude configuration's
	// StateRoot: a deputy's, or main's for a daemon started by hand with a
	// driver. Start owns it from the call.
	Lock *publish.AuthorityWriterLock
	// Prod is prod's gated order: Start takes the lock itself once the
	// store is open, and serves driverless when it cannot or when a rig
	// lease exists, instead of failing, because prod runs under KeepAlive
	// and must keep serving pairing and state.
	Prod *ProdAuthorityGate
}

// ProdAuthorityGate is what prod's start order reads besides its own
// database: the production rig lease root a deputy run publishes its
// manifest under.
type ProdAuthorityGate struct {
	LeaseRoot string
}

// ExecutionState is what a daemon runs and, when it runs no driver, why. The
// zero value is invalid.
type ExecutionState string

const (
	// ExecutionConfigured: a driver runs.
	ExecutionConfigured ExecutionState = "configured"
	// ExecutionNotConfigured: the daemon was started without a driver.
	ExecutionNotConfigured ExecutionState = "not_configured"
	// ExecutionAuthorityInUse: prod was started with a driver, but another
	// process held the App authority writer lock.
	ExecutionAuthorityInUse ExecutionState = "app_authority_in_use"
	// ExecutionRigLeasePresent: prod was started with a driver and took the
	// lock, but a production rig lease manifest, live or stale, existed.
	ExecutionRigLeasePresent ExecutionState = "rig_lease_present"
	// ExecutionPaused: prod was started with a driver while an execution
	// pause it had released was in force, so it took no lock.
	ExecutionPaused ExecutionState = "execution_paused"
)

// AllExecutionStates is the single registration point for ExecutionState.
var AllExecutionStates = []ExecutionState{
	ExecutionConfigured,
	ExecutionNotConfigured,
	ExecutionAuthorityInUse,
	ExecutionRigLeasePresent,
	ExecutionPaused,
}

func (s ExecutionState) valid() bool {
	switch s {
	case ExecutionConfigured, ExecutionNotConfigured, ExecutionAuthorityInUse, ExecutionRigLeasePresent,
		ExecutionPaused:
		return true
	default:
		return false
	}
}

// Runs reports whether a driver runs in this state.
func (s ExecutionState) Runs() bool { return s == ExecutionConfigured }

// gateProdAppAuthority runs prod's start order once its own database is open:
// take the App authority writer lock on stateDir, then look for a rig lease
// manifest under leaseRoot. It returns the held lock with ExecutionConfigured,
// or no lock and the driverless reason. It never returns a lock beside a
// driverless reason, so a deputy that starts while prod serves driverless can
// take it. Any other failure is an error: prod cannot tell whether it may
// open the authority.
//
// The lock comes before the lease check. A deputy takes the lease and then
// the lock, so however the two starts interleave, at least one sees the
// other: a lease published before prod's check makes prod release the lock
// and decline, and otherwise prod took the lock before the deputy tried, so
// the deputy is refused.
func gateProdAppAuthority(stateDir, leaseRoot string) (ExecutionState, *publish.AuthorityWriterLock, error) {
	lock, err := publish.AcquireAuthorityWriterLock(stateDir)
	if errors.Is(err, publish.ErrAuthorityWriterLockHeld) {
		return ExecutionAuthorityInUse, nil, nil
	}
	if err != nil {
		return "", nil, fmt.Errorf("take the App authority writer lock: %w", err)
	}
	present, err := daemonlock.RigManifestPresent(leaseRoot)
	if err != nil {
		return "", nil, errors.Join(
			fmt.Errorf("check for a production rig lease under %q: %w", leaseRoot, err), lock.Close())
	}
	if present {
		if err := lock.Close(); err != nil {
			return "", nil, fmt.Errorf("release the App authority writer lock: %w", err)
		}
		return ExecutionRigLeasePresent, nil, nil
	}
	return ExecutionConfigured, lock, nil
}
