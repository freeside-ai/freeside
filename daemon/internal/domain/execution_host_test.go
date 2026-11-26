package domain_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestExecutionHostValidate: one refusal per rule the record keeps, from a
// valid extra host and a valid daemon's host.
func TestExecutionHostValidate(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	extra := func() domain.ExecutionHost {
		account := "runner"
		override := 1
		return domain.ExecutionHost{
			ID: "host-1", Name: "studio",
			RunAccount:            domain.HostRunAccount{Name: &account},
			Reach:                 &domain.HostReach{Address: "studio.local", HostKey: "ssh-ed25519 AAAA"},
			CordonedCountOverride: &override,
			Conditions: []domain.HostObservedCondition{
				{Condition: domain.ExecutionHostUnreachable, Since: at},
				{Condition: domain.ExecutionHostMemoryPressure, Since: at},
			},
		}
	}
	daemonHost := func() domain.ExecutionHost {
		return domain.ExecutionHost{
			ID: domain.DaemonHostID, Name: "daemon", RunAccount: domain.HostRunAccount{DaemonOwner: true},
		}
	}
	name := func(s string) *string { return &s }
	cases := []struct {
		name    string
		host    func() domain.ExecutionHost
		mutate  func(*domain.ExecutionHost)
		wantErr error
	}{
		{"extra host", extra, func(*domain.ExecutionHost) {}, nil},
		{"daemon's host", daemonHost, func(*domain.ExecutionHost) {}, nil},
		{"extra host run as the daemon owner", extra, func(h *domain.ExecutionHost) { h.RunAccount.DaemonOwner = true }, nil},
		{"missing id", extra, func(h *domain.ExecutionHost) { h.ID = "" }, domain.ErrEmptyID},
		{"missing name", extra, func(h *domain.ExecutionHost) { h.Name = "" }, domain.ErrInvalidExecutionHost},
		{"name with surrounding space", extra, func(h *domain.ExecutionHost) { h.Name = " studio" }, domain.ErrInvalidExecutionHost},
		{"name with a line break", extra, func(h *domain.ExecutionHost) { h.Name = "stu\ndio" }, domain.ErrInvalidExecutionHost},
		{"reach on the daemon's host", daemonHost, func(h *domain.ExecutionHost) {
			h.Reach = &domain.HostReach{Address: "localhost", HostKey: "key"}
		}, domain.ErrInvalidExecutionHost},
		{"no reach on another host", extra, func(h *domain.ExecutionHost) { h.Reach = nil }, domain.ErrInvalidExecutionHost},
		{"address with a space", extra, func(h *domain.ExecutionHost) { h.Reach.Address = "studio local" }, domain.ErrInvalidExecutionHost},
		{"empty host key", extra, func(h *domain.ExecutionHost) { h.Reach.HostKey = "" }, domain.ErrInvalidExecutionHost},
		{"host key over two lines", extra, func(h *domain.ExecutionHost) { h.Reach.HostKey = "a\nb" }, domain.ErrInvalidExecutionHost},
		{"daemon's host with an account name", daemonHost, func(h *domain.ExecutionHost) {
			h.RunAccount.Name = name("owner")
		}, domain.ErrInvalidExecutionHost},
		{"daemon's host not the owner's", daemonHost, func(h *domain.ExecutionHost) {
			h.RunAccount.DaemonOwner = false
		}, domain.ErrInvalidExecutionHost},
		{"another host with no account name", extra, func(h *domain.ExecutionHost) { h.RunAccount.Name = nil }, domain.ErrInvalidExecutionHost},
		{"another host with an empty account name", extra, func(h *domain.ExecutionHost) {
			h.RunAccount.Name = name("")
		}, domain.ErrInvalidExecutionHost},
		{"override below 1", extra, func(h *domain.ExecutionHost) {
			zero := 0
			h.CordonedCountOverride = &zero
		}, domain.ErrInvalidExecutionHost},
		{"unknown condition", extra, func(h *domain.ExecutionHost) {
			h.Conditions[0].Condition = "asleep"
		}, domain.ErrInvalidExecutionHostCondition},
		{"repeated condition", extra, func(h *domain.ExecutionHost) {
			h.Conditions[1] = h.Conditions[0]
		}, domain.ErrInvalidExecutionHost},
		{"conditions out of order", extra, func(h *domain.ExecutionHost) {
			slices.Reverse(h.Conditions)
		}, domain.ErrInvalidExecutionHost},
		{"zero since", extra, func(h *domain.ExecutionHost) { h.Conditions[0].Since = time.Time{} }, domain.ErrMissingTimestamp},
		{"non-UTC since", extra, func(h *domain.ExecutionHost) {
			h.Conditions[0].Since = at.In(time.FixedZone("PST", -8*3600))
		}, domain.ErrTimestampNotUTC},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			host := tc.host()
			tc.mutate(&host)
			err := host.Validate()
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestExecutionHostConditionClosesEveryLaunch pins which conditions close a
// host to every launch: placement reads this instead of keeping a list.
func TestExecutionHostConditionClosesEveryLaunch(t *testing.T) {
	t.Parallel()
	closesEvery := []domain.ExecutionHostCondition{
		domain.ExecutionHostNoDesktopSession, domain.ExecutionHostUnreachable, domain.ExecutionHostProofNotCurrent,
		domain.ExecutionHostOtherBuild, domain.ExecutionHostLost,
	}
	for _, condition := range domain.AllExecutionHostConditions {
		if got, want := condition.ClosesEveryLaunch(), slices.Contains(closesEvery, condition); got != want {
			t.Errorf("%q.ClosesEveryLaunch() = %t, want %t", condition, got, want)
		}
	}
	if domain.ExecutionHostCondition("").ClosesEveryLaunch() {
		t.Error(`"".ClosesEveryLaunch() = true, want false`)
	}
}
