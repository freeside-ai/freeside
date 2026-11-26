package domain

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
)

// ExecutionHostCondition names one condition that closes an execution host
// to new cordoned launches, all of them or some (plan §5.7 Execution Hosts,
// issue #2162). A host can be in several at once. The unit that observes a
// condition writes it; this enum names all of them so placement and a hold's
// fact can name any. The zero value is invalid.
type ExecutionHostCondition string

const (
	// ExecutionHostNoDesktopSession: no user is logged in to the host's
	// desktop, so a launch that needs one can't start.
	ExecutionHostNoDesktopSession ExecutionHostCondition = "no_desktop_session"
	// ExecutionHostUnreachable: the daemon can't reach the host.
	ExecutionHostUnreachable ExecutionHostCondition = "unreachable"
	// ExecutionHostProofNotCurrent: the host's conformance proof failed or
	// lapsed.
	ExecutionHostProofNotCurrent ExecutionHostCondition = "proof_not_current"
	// ExecutionHostOtherBuild: the host runs a build other than the daemon's.
	ExecutionHostOtherBuild ExecutionHostCondition = "other_build"
	// ExecutionHostLost: the host was declared lost.
	ExecutionHostLost ExecutionHostCondition = "lost"
	// ExecutionHostCordonedCountFull: the host runs as many cordoned
	// launches as its count allows.
	ExecutionHostCordonedCountFull ExecutionHostCondition = "cordoned_count_full"
	// ExecutionHostMemoryPressure: the host's memory pressure isn't normal.
	ExecutionHostMemoryPressure ExecutionHostCondition = "memory_pressure"
	// ExecutionHostNoDesktopControl: the host can't grant desktop control,
	// so only a launch that needs it is closed.
	ExecutionHostNoDesktopControl ExecutionHostCondition = "no_desktop_control"
)

// AllExecutionHostConditions is the single registration point for host
// conditions, and the order a host lists the conditions it is in.
var AllExecutionHostConditions = []ExecutionHostCondition{
	ExecutionHostNoDesktopSession,
	ExecutionHostUnreachable,
	ExecutionHostProofNotCurrent,
	ExecutionHostOtherBuild,
	ExecutionHostLost,
	ExecutionHostCordonedCountFull,
	ExecutionHostMemoryPressure,
	ExecutionHostNoDesktopControl,
}

func (c ExecutionHostCondition) valid() bool {
	switch c {
	case ExecutionHostNoDesktopSession, ExecutionHostUnreachable, ExecutionHostProofNotCurrent,
		ExecutionHostOtherBuild, ExecutionHostLost, ExecutionHostCordonedCountFull,
		ExecutionHostMemoryPressure, ExecutionHostNoDesktopControl:
		return true
	default:
		return false
	}
}

// ClosesEveryLaunch reports whether the condition closes the host to every
// new launch, rather than to some work only. Placement asks the condition
// instead of keeping its own list. The zero value closes nothing.
func (c ExecutionHostCondition) ClosesEveryLaunch() bool {
	switch c {
	case ExecutionHostNoDesktopSession, ExecutionHostUnreachable, ExecutionHostProofNotCurrent,
		ExecutionHostOtherBuild, ExecutionHostLost:
		return true
	case ExecutionHostCordonedCountFull, ExecutionHostMemoryPressure, ExecutionHostNoDesktopControl:
		return false
	}
	return false
}

// HostRunAccount is the account a host's runs execute as. The Simulator and
// desktop-control settings are honored only where DaemonOwner is false
// (#3629). The daemon's host runs as the daemon's own account, which a
// migration can't read, so its Name is nil; every other host records the
// login, which is also the one the daemon reaches it as.
type HostRunAccount struct {
	Name        *string `json:"name"`
	DaemonOwner bool    `json:"daemon_owner"`
}

// HostReach is how the daemon reaches a host other than its own: the address
// and the host key pinned when the operator named the host. It holds no
// secret. The key is checked where a connection is made (#3625).
type HostReach struct {
	Address string `json:"address"`
	HostKey string `json:"host_key"`
}

// HostObservedCondition is one condition a host is in and when it began.
type HostObservedCondition struct {
	Condition ExecutionHostCondition `json:"condition"`
	Since     time.Time              `json:"since"`
}

// ExecutionHost is a machine that runs cordoned launches for this daemon
// (plan §5.7). The daemon's own host is a record like any other, with the id
// DaemonHostID and no reach.
type ExecutionHost struct {
	ID         ExecutionHostID `json:"id"`
	Name       string          `json:"name"`
	RunAccount HostRunAccount  `json:"run_account"`
	// Reach is nil exactly for the daemon's host.
	Reach *HostReach `json:"reach"`
	// CordonedCountOverride is the operator's override of how many cordoned
	// launches the host runs at once: nil, or at least 1.
	CordonedCountOverride *int `json:"cordoned_count_override"`
	// Conditions lists each condition the host is in, once each, in
	// AllExecutionHostConditions order.
	Conditions []HostObservedCondition `json:"conditions"`
}

// Validate reports whether the host record is consistent: an id and a name
// with no surrounding space or line break; a reach exactly when the host
// isn't the daemon's; the daemon's host running as the daemon owner with no
// account name and every other host naming its account; an override of at
// least 1 when set; and known conditions with a UTC start, listed once each
// in canonical order.
func (h ExecutionHost) Validate() error {
	if h.ID == "" {
		return fmt.Errorf("execution host id: %w", ErrEmptyID)
	}
	if !singleLine(h.Name) {
		return fmt.Errorf("execution host %q name %q: %w", h.ID, h.Name, ErrInvalidExecutionHost)
	}
	daemonHost := h.ID == DaemonHostID
	switch {
	case daemonHost && h.Reach != nil:
		return fmt.Errorf("execution host %q: the daemon's host has no reach: %w", h.ID, ErrInvalidExecutionHost)
	case !daemonHost && h.Reach == nil:
		return fmt.Errorf("execution host %q: a host other than the daemon's needs a reach: %w", h.ID, ErrInvalidExecutionHost)
	case h.Reach != nil:
		if h.Reach.Address == "" || strings.ContainsFunc(h.Reach.Address, unicode.IsSpace) {
			return fmt.Errorf("execution host %q address %q: %w", h.ID, h.Reach.Address, ErrInvalidExecutionHost)
		}
		if !singleLine(h.Reach.HostKey) {
			return fmt.Errorf("execution host %q host key: %w", h.ID, ErrInvalidExecutionHost)
		}
	}
	account := h.RunAccount
	switch {
	case daemonHost && (!account.DaemonOwner || account.Name != nil):
		return fmt.Errorf("execution host %q: the daemon's host runs as the daemon owner with no account name: %w",
			h.ID, ErrInvalidExecutionHost)
	case !daemonHost && (account.Name == nil || *account.Name == "" || strings.ContainsFunc(*account.Name, unicode.IsSpace)):
		return fmt.Errorf("execution host %q: a host other than the daemon's names its run account: %w",
			h.ID, ErrInvalidExecutionHost)
	}
	if h.CordonedCountOverride != nil && *h.CordonedCountOverride < 1 {
		return fmt.Errorf("execution host %q cordoned count override %d: %w",
			h.ID, *h.CordonedCountOverride, ErrInvalidExecutionHost)
	}
	last := -1
	for _, observed := range h.Conditions {
		if err := observed.Condition.Validate(); err != nil {
			return fmt.Errorf("execution host %q: %w", h.ID, err)
		}
		position := slices.Index(AllExecutionHostConditions, observed.Condition)
		if position <= last {
			return fmt.Errorf("execution host %q condition %q repeated or out of order: %w",
				h.ID, observed.Condition, ErrInvalidExecutionHost)
		}
		last = position
		if observed.Since.IsZero() {
			return fmt.Errorf("execution host %q condition %q since: %w", h.ID, observed.Condition, ErrMissingTimestamp)
		}
		if observed.Since.Location() != time.UTC {
			return fmt.Errorf("execution host %q condition %q since: %w", h.ID, observed.Condition, ErrTimestampNotUTC)
		}
	}
	return nil
}

// Validate reports whether c is a known condition, for a caller that sets or
// clears one condition without a whole record.
func (c ExecutionHostCondition) Validate() error {
	if !c.valid() {
		return fmt.Errorf("execution host condition %q: %w", c, ErrInvalidExecutionHostCondition)
	}
	return nil
}

// singleLine reports whether s is non-empty with no line break and no
// surrounding space.
func singleLine(s string) bool {
	return s != "" && s == strings.TrimSpace(s) && !strings.ContainsAny(s, "\r\n")
}
