package domain

import (
	"errors"
	"testing"
	"time"
)

// TestCordonedLaunchHoldValidate: the fact rides only beside a
// cordoned_launch_cap hold, may be absent there, and must describe a wait:
// every host closed or an older launch ahead, each host listed once in id
// order with its conditions in canonical order, and a full count exactly
// beside cordoned_count_full.
func TestCordonedLaunchHoldValidate(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	hold := func(reason RunHoldReason, fact *CordonedLaunchHold) RunHoldObservation {
		return RunHoldObservation{
			RunID: "run-1", Reason: reason, CordonedLaunch: fact,
			FirstObservedAt: at, LastObservedAt: at,
		}
	}
	full := func(id ExecutionHostID, running, limit int) CordonedLaunchHost {
		return CordonedLaunchHost{
			HostID: id, Conditions: []ExecutionHostCondition{ExecutionHostCordonedCountFull},
			CordonedCount: &CordonedLaunchCount{Running: running, Limit: limit},
		}
	}
	open := CordonedLaunchHost{HostID: DaemonHostID, Conditions: []ExecutionHostCondition{}}
	for name, tc := range map[string]struct {
		fact *CordonedLaunchHold
		want error
	}{
		"cap reached":   {&CordonedLaunchHold{Hosts: []CordonedLaunchHost{full(DaemonHostID, 2, 2)}}, nil},
		"no cap set":    {&CordonedLaunchHold{Hosts: []CordonedLaunchHost{full(DaemonHostID, 0, 0)}}, nil},
		"older ahead":   {&CordonedLaunchHold{Hosts: []CordonedLaunchHost{open}, Ahead: 1}, nil},
		"stored before": {nil, nil},
		"two hosts closed": {&CordonedLaunchHold{Hosts: []CordonedLaunchHost{
			full(DaemonHostID, 1, 1),
			{HostID: "mac-studio", Conditions: []ExecutionHostCondition{ExecutionHostUnreachable, ExecutionHostLost}},
		}}, nil},
		"no host":         {&CordonedLaunchHold{Ahead: 1}, ErrInvalidCordonedLaunchHold},
		"empty host":      {&CordonedLaunchHold{Hosts: []CordonedLaunchHost{{HostID: ""}}, Ahead: 1}, ErrEmptyID},
		"null conditions": {&CordonedLaunchHold{Hosts: []CordonedLaunchHost{{HostID: DaemonHostID}}, Ahead: 1}, ErrInvalidCordonedLaunchHold},
		"hosts repeat":    {&CordonedLaunchHold{Hosts: []CordonedLaunchHost{open, open}, Ahead: 1}, ErrInvalidCordonedLaunchHold},
		"hosts out of order": {&CordonedLaunchHold{Hosts: []CordonedLaunchHost{
			{HostID: "mac-studio", Conditions: []ExecutionHostCondition{ExecutionHostLost}}, full(DaemonHostID, 1, 1),
		}}, ErrInvalidCordonedLaunchHold},
		"unknown condition": {&CordonedLaunchHold{Hosts: []CordonedLaunchHost{
			{HostID: DaemonHostID, Conditions: []ExecutionHostCondition{"asleep"}},
		}}, ErrInvalidExecutionHostCondition},
		"conditions out of order": {&CordonedLaunchHold{Hosts: []CordonedLaunchHost{
			{HostID: DaemonHostID, Conditions: []ExecutionHostCondition{ExecutionHostLost, ExecutionHostUnreachable}},
		}}, ErrInvalidCordonedLaunchHold},
		"full without count": {&CordonedLaunchHold{Hosts: []CordonedLaunchHost{
			{HostID: DaemonHostID, Conditions: []ExecutionHostCondition{ExecutionHostCordonedCountFull}},
		}}, ErrHoldDetailMismatch},
		"count without full": {&CordonedLaunchHold{Hosts: []CordonedLaunchHost{
			{HostID: DaemonHostID, Conditions: []ExecutionHostCondition{ExecutionHostLost}, CordonedCount: &CordonedLaunchCount{Running: 1, Limit: 1}},
		}}, ErrHoldDetailMismatch},
		"count not full":   {&CordonedLaunchHold{Hosts: []CordonedLaunchHost{full(DaemonHostID, 1, 2)}}, ErrHoldDetailMismatch},
		"negative count":   {&CordonedLaunchHold{Hosts: []CordonedLaunchHost{full(DaemonHostID, 0, -1)}}, ErrNegativeSize},
		"negative ahead":   {&CordonedLaunchHold{Hosts: []CordonedLaunchHost{full(DaemonHostID, 1, 1)}, Ahead: -1}, ErrNegativeSize},
		"open, none ahead": {&CordonedLaunchHold{Hosts: []CordonedLaunchHost{open}}, ErrHoldDetailMismatch},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := hold(HoldCordonedLaunchCap, tc.fact).Validate()
			if tc.want == nil && err != nil || tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("Validate = %v, want %v", err, tc.want)
			}
		})
	}
	t.Run("fact beside another reason", func(t *testing.T) {
		t.Parallel()
		fact := &CordonedLaunchHold{Hosts: []CordonedLaunchHost{full(DaemonHostID, 1, 1)}}
		if err := hold(HoldUsagePoolExhausted, fact).Validate(); !errors.Is(err, ErrHoldDetailMismatch) {
			t.Fatalf("Validate = %v, want %v", err, ErrHoldDetailMismatch)
		}
	})
	if !errors.Is(&CordonedLaunchCapError{}, ErrCordonedLaunchCap) {
		t.Fatal("CordonedLaunchCapError does not match ErrCordonedLaunchCap")
	}
}
