package domain

import (
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// UpdateDrainState is the state an update drain transition moves the daemon
// to. A drain holds new launches so an update can wait for running work
// instead of canceling it (issue #3103). It is not an
// UnattendedOperationState: the operator stop/resume log is latest-row-wins
// and command-backed, so a drain row in it would lift an operator stop, and
// a drain is lifted by a new build with no command at all. The zero value is
// invalid.
type UpdateDrainState string

const (
	// UpdateDrainDraining: a drain is in force; new launches wait for the
	// update.
	UpdateDrainDraining UpdateDrainState = "draining"
	// UpdateDrainLifted: the drain no longer holds launches.
	UpdateDrainLifted UpdateDrainState = "lifted"
)

// AllUpdateDrainStates is the single registration point for update drain
// states.
var AllUpdateDrainStates = []UpdateDrainState{
	UpdateDrainDraining,
	UpdateDrainLifted,
}

func (s UpdateDrainState) valid() bool {
	switch s {
	case UpdateDrainDraining, UpdateDrainLifted:
		return true
	default:
		return false
	}
}

// UpdateDrainTransition is one appended row of the update drain log. The log
// is append-only and the latest row is the state; an empty log means no
// drain. The row holds work and grants nothing, so it carries no command
// binding and decodes with plain validation.
type UpdateDrainTransition struct {
	State UpdateDrainState `json:"state"`
	// Build is the digest of the daemon executable that wrote the row. The
	// installer stamps no version into the binary, so only the executable's
	// own digest tells two local builds apart.
	Build Digest `json:"build"`
	// Forced records that the drain was requested with forced cancellation.
	// Only a draining row can be forced; lifting is never forced.
	Forced     bool      `json:"forced"`
	OccurredAt time.Time `json:"occurred_at"`
}

// Validate reports whether the transition is structurally sound.
func (t UpdateDrainTransition) Validate() error {
	if !t.State.valid() {
		return fmt.Errorf("update drain state %q: %w", t.State, ErrInvalidUpdateDrainState)
	}
	if !contentaddr.Valid(string(t.Build)) {
		return fmt.Errorf("update drain build %q: %w", t.Build, ErrInvalidDigest)
	}
	if t.Forced && t.State != UpdateDrainDraining {
		return fmt.Errorf("update drain state %q: %w", t.State, ErrUpdateDrainLiftForced)
	}
	if t.OccurredAt.IsZero() {
		return fmt.Errorf("update drain occurred_at: %w", ErrMissingTimestamp)
	}
	if t.OccurredAt.Location() != time.UTC {
		return fmt.Errorf("update drain occurred_at: %w", ErrTimestampNotUTC)
	}
	return nil
}
