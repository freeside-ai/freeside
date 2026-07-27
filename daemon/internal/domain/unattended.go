package domain

import (
	"fmt"
	"time"
)

// UnattendedOperationState is the durable operating state an operator
// transition moves unattended admission to (plan §4 stop_unattended, §5.7).
// The zero value is invalid so an unpopulated row cannot be mistaken for a
// recorded decision.
type UnattendedOperationState string

const (
	// UnattendedStopped: unattended admission is closed until an explicit
	// resume; a daemon restart never changes it (issue #319).
	UnattendedStopped UnattendedOperationState = "stopped"
	// UnattendedResumed: an operator explicitly reopened unattended
	// admission; every admission still gates on its own merits.
	UnattendedResumed UnattendedOperationState = "resumed"
)

// AllUnattendedOperationStates is the single registration point for
// unattended-operation states.
var AllUnattendedOperationStates = []UnattendedOperationState{
	UnattendedStopped,
	UnattendedResumed,
}

func (s UnattendedOperationState) valid() bool {
	switch s {
	case UnattendedStopped, UnattendedResumed:
		return true
	default:
		return false
	}
}

// UnattendedOperationTransition is one appended operator decision in the
// stop/resume log. The log is append-only and the latest row wins: "stopped"
// holds until a "resumed" row is appended by the explicit operator path, so
// surviving a restart is structural rather than a recovery step. CommandID
// binds the transition to the signet command that carried the decision (and
// through it the deciding device and item); nil is reserved for writers that
// have no command, none of which exist yet.
type UnattendedOperationTransition struct {
	State      UnattendedOperationState
	CommandID  *string
	Reason     string
	OccurredAt time.Time
}

// Validate reports whether the transition is structurally sound.
func (t UnattendedOperationTransition) Validate() error {
	if !t.State.valid() {
		return fmt.Errorf("unattended operation state %q: %w", t.State, ErrInvalidUnattendedOperationState)
	}
	if t.CommandID != nil && *t.CommandID == "" {
		return fmt.Errorf("unattended operation transition command_id: %w", ErrEmptyID)
	}
	if t.OccurredAt.IsZero() {
		return fmt.Errorf("unattended operation transition occurred_at: %w", ErrMissingTimestamp)
	}
	// One instant, one persisted byte form: the same canonicality the item
	// body enforces for DecidedAt.
	if t.OccurredAt.Location() != time.UTC {
		return fmt.Errorf("unattended operation transition occurred_at: %w", ErrTimestampNotUTC)
	}
	return nil
}
