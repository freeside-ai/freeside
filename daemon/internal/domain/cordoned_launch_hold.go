package domain

import (
	"fmt"
	"slices"
)

// CordonedLaunchHold is the typed fact beside a cordoned_launch_cap hold: each
// host the waiting launch was considered for, with what closes it, and how
// many older tasks' cordoned launches wait ahead (plan §5.7 Placement). A
// reason to wait that a later unit adds becomes a condition on a host here,
// not a hold reason of its own. Hosts are named by id only; a client reads
// their names from the host list.
type CordonedLaunchHold struct {
	Hosts []CordonedLaunchHost `json:"hosts"`
	Ahead int                  `json:"ahead"`
}

// CordonedLaunchHost is one host a waiting cordoned launch was considered
// for. Conditions lists what closes the host to the launch, in
// AllExecutionHostConditions order, and is empty for a host that is open but
// has older launches ahead. CordonedCount is the host's running count and
// limit, present exactly when its cordoned count is full.
type CordonedLaunchHost struct {
	HostID        ExecutionHostID          `json:"host_id"`
	Conditions    []ExecutionHostCondition `json:"conditions"`
	CordonedCount *CordonedLaunchCount     `json:"cordoned_count"`
}

// CordonedLaunchCount is how many cordoned launches a host runs and how many
// it may. A limit of 0 means no cap is set, so the host takes none.
type CordonedLaunchCount struct {
	Running int `json:"running"`
	Limit   int `json:"limit"`
}

// Validate reports whether the fact describes a wait: at least one host,
// listed once each in id order; each host's conditions a list, possibly
// empty, of known conditions in canonical order; a count exactly beside cordoned_count_full, and one that is full; no
// negative number; and either an older launch ahead or every host closed.
func (h CordonedLaunchHold) Validate() error {
	if len(h.Hosts) == 0 {
		return fmt.Errorf("cordoned launch hold names no host: %w", ErrInvalidCordonedLaunchHold)
	}
	if h.Ahead < 0 {
		return fmt.Errorf("cordoned launch hold ahead %d: %w", h.Ahead, ErrNegativeSize)
	}
	everyHostClosed := true
	for i, host := range h.Hosts {
		if host.HostID == "" {
			return fmt.Errorf("cordoned launch hold host_id: %w", ErrEmptyID)
		}
		if i > 0 && host.HostID <= h.Hosts[i-1].HostID {
			return fmt.Errorf("cordoned launch hold host %q repeated or out of order: %w",
				host.HostID, ErrInvalidCordonedLaunchHold)
		}
		if err := host.validate(); err != nil {
			return err
		}
		everyHostClosed = everyHostClosed && len(host.Conditions) > 0
	}
	if h.Ahead == 0 && !everyHostClosed {
		return fmt.Errorf("cordoned launch hold has an open host and none ahead: %w", ErrHoldDetailMismatch)
	}
	return nil
}

func (h CordonedLaunchHost) validate() error {
	if h.Conditions == nil {
		return fmt.Errorf("cordoned launch hold host %q conditions: null, want a list: %w",
			h.HostID, ErrInvalidCordonedLaunchHold)
	}
	last := -1
	for _, condition := range h.Conditions {
		if err := condition.Validate(); err != nil {
			return fmt.Errorf("cordoned launch hold host %q: %w", h.HostID, err)
		}
		position := slices.Index(AllExecutionHostConditions, condition)
		if position <= last {
			return fmt.Errorf("cordoned launch hold host %q condition %q repeated or out of order: %w",
				h.HostID, condition, ErrInvalidCordonedLaunchHold)
		}
		last = position
	}
	full := slices.Contains(h.Conditions, ExecutionHostCordonedCountFull)
	if full != (h.CordonedCount != nil) {
		return fmt.Errorf("cordoned launch hold host %q cordoned_count present=%t beside full=%t: %w",
			h.HostID, h.CordonedCount != nil, full, ErrHoldDetailMismatch)
	}
	if count := h.CordonedCount; count != nil {
		switch {
		case count.Running < 0 || count.Limit < 0:
			return fmt.Errorf("cordoned launch hold host %q count %d of %d: %w",
				h.HostID, count.Running, count.Limit, ErrNegativeSize)
		case count.Running < count.Limit:
			return fmt.Errorf("cordoned launch hold host %q count %d of %d is not full: %w",
				h.HostID, count.Running, count.Limit, ErrHoldDetailMismatch)
		}
	}
	return nil
}

// CordonedLaunchCapError is an ErrCordonedLaunchCap refusal with the fact the
// run's cordoned_launch_cap hold records.
type CordonedLaunchCapError struct {
	Hold CordonedLaunchHold
}

func (e *CordonedLaunchCapError) Error() string {
	return fmt.Sprintf("%s: %d hosts considered, %d launches ahead",
		ErrCordonedLaunchCap, len(e.Hold.Hosts), e.Hold.Ahead)
}

// Is matches ErrCordonedLaunchCap.
func (e *CordonedLaunchCapError) Is(target error) bool { return target == ErrCordonedLaunchCap }
