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
// binds the transition to the accepted signet command that carried the
// decision (and through it the deciding device and item); it is required,
// because the immutable command is the independently trusted authority a
// reconstruction re-derives the state from — an unbacked row is a forgery
// surface, not a convenience, and no non-command writer exists.
type UnattendedOperationTransition struct {
	State      UnattendedOperationState
	CommandID  *string
	Reason     string
	OccurredAt time.Time
}

// AuthorizingAction returns the accepted command action that authorizes a
// transition to this state. Behaviour dispatch, so no default: a new state
// must declare its authorizing action; the trailing return rejects the
// invalid zero value.
func (s UnattendedOperationState) AuthorizingAction() (Action, bool) {
	switch s {
	case UnattendedStopped:
		return ActionStopUnattended, true
	case UnattendedResumed:
		return ActionResumeUnattended, true
	}
	return "", false
}

// Validate reports whether the transition is structurally sound.
func (t UnattendedOperationTransition) Validate() error {
	if !t.State.valid() {
		return fmt.Errorf("unattended operation state %q: %w", t.State, ErrInvalidUnattendedOperationState)
	}
	if t.CommandID == nil || *t.CommandID == "" {
		return fmt.Errorf("unattended operation transition: %w", ErrTransitionUnbacked)
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

// UnattendedOperationGate is the verdict of the one gate on new unattended
// work (§5.7), as data: what closes it, not only whether it is closed. The
// admission predicate and the sync projection both read this value, so a
// client can never be told admission is open while the daemon refuses it.
type UnattendedOperationGate struct {
	// OperatorStop is the latest transition when it is "stopped"; nil when
	// the log is empty or the latest decision resumed.
	OperatorStop *UnattendedOperationTransition
	// Blocking lists, in item-id order, every open system_health item that
	// closes the gate. The notice an operator stop raises is one of them: it
	// is a blocking item in its own right until resume resolves it.
	Blocking []UnattendedBlockingItem
}

// UnattendedBlockingItem is one open system_health item that closes the gate,
// with the reason it does.
type UnattendedBlockingItem struct {
	Item AttentionItem
	// Err is the refusal the gate returns for this item: it wraps
	// ErrBlockingSystemHealth unless the supersession condition could not be
	// evaluated at all, which blocks without claiming the diagnostic stands.
	Err error
}

// Err is the admission predicate over the verdict: nil when the gate is
// open, otherwise the operator stop before the first blocking item.
func (g UnattendedOperationGate) Err() error {
	if g.OperatorStop != nil {
		return ErrUnattendedOperationStopped
	}
	if len(g.Blocking) > 0 {
		return g.Blocking[0].Err
	}
	return nil
}

// UnattendedAdmission is the synced verdict of the unattended-operation gate:
// whether the daemon admits new unattended work. The zero value is invalid.
type UnattendedAdmission string

const (
	// UnattendedAdmissionOpen: nothing closes the gate.
	UnattendedAdmissionOpen UnattendedAdmission = "open"
	// UnattendedAdmissionStopped: at least one stop is in force.
	UnattendedAdmissionStopped UnattendedAdmission = "stopped"
)

// AllUnattendedAdmissions is the single registration point for
// unattended-admission verdicts.
var AllUnattendedAdmissions = []UnattendedAdmission{
	UnattendedAdmissionOpen,
	UnattendedAdmissionStopped,
}

func (a UnattendedAdmission) valid() bool {
	switch a {
	case UnattendedAdmissionOpen, UnattendedAdmissionStopped:
		return true
	default:
		return false
	}
}

// UnattendedStopKind names why the gate is closed, so a client words an
// operator's decision differently from a finding the daemon raised itself.
// The zero value is invalid.
type UnattendedStopKind string

const (
	// UnattendedStopOperator: an accepted stop_unattended is in force and
	// only resume_unattended lifts it.
	UnattendedStopOperator UnattendedStopKind = "operator_stop"
	// UnattendedStopBlockingSystemHealth: an open blocking system_health
	// item no validated configuration supersedes (plan §4).
	UnattendedStopBlockingSystemHealth UnattendedStopKind = "blocking_system_health"
)

// AllUnattendedStopKinds is the single registration point for stop kinds.
var AllUnattendedStopKinds = []UnattendedStopKind{
	UnattendedStopOperator,
	UnattendedStopBlockingSystemHealth,
}

func (k UnattendedStopKind) valid() bool {
	switch k {
	case UnattendedStopOperator, UnattendedStopBlockingSystemHealth:
		return true
	default:
		return false
	}
}
