package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

var executionHostSince = time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)

func extraExecutionHost(id domain.ExecutionHostID, name string) domain.ExecutionHost {
	account := "runner"
	return domain.ExecutionHost{
		ID: id, Name: name,
		RunAccount: domain.HostRunAccount{Name: &account},
		Reach:      &domain.HostReach{Address: name + ".local", HostKey: "ssh-ed25519 AAAA" + name},
		Conditions: []domain.HostObservedCondition{},
	}
}

func readExecutionHost(t *testing.T, s *store.Store, id domain.ExecutionHostID) (domain.ExecutionHost, error) {
	t.Helper()
	var host domain.ExecutionHost
	err := s.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		host, err = tx.ExecutionHost(context.Background(), id)
		return err
	})
	return host, err
}

func writeExecutionHosts(t *testing.T, s *store.Store, fn func(context.Context, *store.WriteTx) error) error {
	t.Helper()
	ctx := context.Background()
	return s.Write(ctx, func(tx *store.WriteTx) error { return fn(ctx, tx) })
}

// TestExecutionHostDaemonHostSeeded: the migration leaves the daemon's host
// as a record with no reach, the owner's account, no override, and no
// conditions.
func TestExecutionHostDaemonHostSeeded(t *testing.T) {
	t.Parallel()
	s := openStoreAt(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	got, err := readExecutionHost(t, s, domain.DaemonHostID)
	if err != nil {
		t.Fatal(err)
	}
	want := domain.ExecutionHost{
		ID: domain.DaemonHostID, Name: "daemon",
		RunAccount: domain.HostRunAccount{DaemonOwner: true},
		Conditions: []domain.HostObservedCondition{},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("daemon's host = %+v, want %+v", got, want)
	}
}

// TestExecutionHostAddListRemove adds a host with an override and a
// condition, lists it after the daemon's host, and removes it.
func TestExecutionHostAddListRemove(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStoreAt(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	extra := extraExecutionHost("host-1", "studio")
	override := 2
	extra.CordonedCountOverride = &override
	extra.Conditions = []domain.HostObservedCondition{{Condition: domain.ExecutionHostLost, Since: executionHostSince}}
	if err := writeExecutionHosts(t, s, func(ctx context.Context, tx *store.WriteTx) error {
		return tx.AddExecutionHost(ctx, extra)
	}); err != nil {
		t.Fatal(err)
	}
	var hosts []domain.ExecutionHost
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		hosts, err = tx.ListExecutionHosts(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 2 || hosts[0].ID != domain.DaemonHostID || !reflect.DeepEqual(hosts[1], extra) {
		t.Fatalf("hosts = %+v, want the daemon's host then %+v", hosts, extra)
	}
	if err := writeExecutionHosts(t, s, func(ctx context.Context, tx *store.WriteTx) error {
		return tx.RemoveExecutionHost(ctx, extra.ID)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := readExecutionHost(t, s, extra.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("read after remove = %v, want ErrNotFound", err)
	}
}

// TestExecutionHostRefusals: one case per write the store refuses.
func TestExecutionHostRefusals(t *testing.T) {
	t.Parallel()
	s := openStoreAt(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err := writeExecutionHosts(t, s, func(ctx context.Context, tx *store.WriteTx) error {
		return tx.AddExecutionHost(ctx, extraExecutionHost("host-1", "studio"))
	}); err != nil {
		t.Fatal(err)
	}
	one := 1
	cases := []struct {
		name    string
		write   func(context.Context, *store.WriteTx) error
		wantErr error
	}{
		{"same id", func(ctx context.Context, tx *store.WriteTx) error {
			return tx.AddExecutionHost(ctx, extraExecutionHost("host-1", "other"))
		}, domain.ErrExecutionHostExists},
		{"same name", func(ctx context.Context, tx *store.WriteTx) error {
			return tx.AddExecutionHost(ctx, extraExecutionHost("host-2", "studio"))
		}, domain.ErrExecutionHostExists},
		{"invalid host", func(ctx context.Context, tx *store.WriteTx) error {
			host := extraExecutionHost("host-2", "other")
			host.Reach = nil
			return tx.AddExecutionHost(ctx, host)
		}, domain.ErrInvalidExecutionHost},
		{"remove the daemon's host", func(ctx context.Context, tx *store.WriteTx) error {
			return tx.RemoveExecutionHost(ctx, domain.DaemonHostID)
		}, domain.ErrDaemonHostRemoval},
		{"remove a missing host", func(ctx context.Context, tx *store.WriteTx) error {
			return tx.RemoveExecutionHost(ctx, "host-missing")
		}, store.ErrNotFound},
		{"set a condition on a missing host", func(ctx context.Context, tx *store.WriteTx) error {
			_, err := tx.SetExecutionHostCondition(ctx, "host-missing", domain.ExecutionHostLost, executionHostSince)
			return err
		}, store.ErrNotFound},
		{"clear a condition on a missing host", func(ctx context.Context, tx *store.WriteTx) error {
			_, err := tx.ClearExecutionHostCondition(ctx, "host-missing", domain.ExecutionHostLost)
			return err
		}, store.ErrNotFound},
		{"set an override on a missing host", func(ctx context.Context, tx *store.WriteTx) error {
			_, err := tx.SetExecutionHostCordonedCountOverride(ctx, "host-missing", &one)
			return err
		}, store.ErrNotFound},
		{"set an unknown condition", func(ctx context.Context, tx *store.WriteTx) error {
			_, err := tx.SetExecutionHostCondition(ctx, "host-1", "asleep", executionHostSince)
			return err
		}, domain.ErrInvalidExecutionHostCondition},
		{"set an override below 1", func(ctx context.Context, tx *store.WriteTx) error {
			zero := 0
			_, err := tx.SetExecutionHostCordonedCountOverride(ctx, "host-1", &zero)
			return err
		}, domain.ErrInvalidExecutionHost},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := writeExecutionHosts(t, s, tc.write); !errors.Is(err, tc.wantErr) {
				t.Fatalf("write = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestExecutionHostConditions sets every condition on a host, twice, and
// clears each: a second set keeps the first since, and each set or clear
// reports whether it changed anything.
func TestExecutionHostConditions(t *testing.T) {
	t.Parallel()
	s := openStoreAt(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	setAll := func(since time.Time, wantChanged bool) {
		t.Helper()
		// Set in reverse so the read's order comes from the enum, not the
		// order of the writes.
		for i := len(domain.AllExecutionHostConditions) - 1; i >= 0; i-- {
			condition := domain.AllExecutionHostConditions[i]
			var changed bool
			if err := writeExecutionHosts(t, s, func(ctx context.Context, tx *store.WriteTx) error {
				var err error
				changed, err = tx.SetExecutionHostCondition(ctx, domain.DaemonHostID, condition, since)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if changed != wantChanged {
				t.Fatalf("set %q changed = %t, want %t", condition, changed, wantChanged)
			}
		}
	}
	setAll(executionHostSince, true)
	setAll(executionHostSince.Add(time.Hour), false)
	host, err := readExecutionHost(t, s, domain.DaemonHostID)
	if err != nil {
		t.Fatal(err)
	}
	want := make([]domain.HostObservedCondition, 0, len(domain.AllExecutionHostConditions))
	for _, condition := range domain.AllExecutionHostConditions {
		want = append(want, domain.HostObservedCondition{Condition: condition, Since: executionHostSince})
	}
	if !reflect.DeepEqual(host.Conditions, want) {
		t.Fatalf("conditions = %+v, want %+v", host.Conditions, want)
	}
	for _, wantChanged := range []bool{true, false} {
		for _, condition := range domain.AllExecutionHostConditions {
			var changed bool
			if err := writeExecutionHosts(t, s, func(ctx context.Context, tx *store.WriteTx) error {
				var err error
				changed, err = tx.ClearExecutionHostCondition(ctx, domain.DaemonHostID, condition)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if changed != wantChanged {
				t.Fatalf("clear %q changed = %t, want %t", condition, changed, wantChanged)
			}
		}
	}
	if host, err := readExecutionHost(t, s, domain.DaemonHostID); err != nil || len(host.Conditions) != 0 {
		t.Fatalf("after clearing: %+v, %v; want no conditions", host, err)
	}
}

// TestExecutionHostCordonedCountOverride sets the override, changes it,
// and clears it, each twice: only the first of each pair reports a change.
func TestExecutionHostCordonedCountOverride(t *testing.T) {
	t.Parallel()
	s := openStoreAt(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	for i, override := range []*int{new(3), new(3), new(4), new(4), nil, nil} {
		var changed bool
		if err := writeExecutionHosts(t, s, func(ctx context.Context, tx *store.WriteTx) error {
			var err error
			changed, err = tx.SetExecutionHostCordonedCountOverride(ctx, domain.DaemonHostID, override)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if want := i%2 == 0; changed != want {
			t.Fatalf("write %d changed = %t, want %t", i, changed, want)
		}
		host, err := readExecutionHost(t, s, domain.DaemonHostID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(host.CordonedCountOverride, override) {
			t.Fatalf("override = %v, want %v", host.CordonedCountOverride, override)
		}
	}
}
