package store_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// putDevices commits the given devices in one synchronized write.
func putDevices(t *testing.T, s *store.Store, devices ...domain.Device) {
	t.Helper()
	ctx := context.Background()
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		for _, device := range devices {
			if err := tx.PutDevice(ctx, device); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("put devices: %v", err)
	}
}

func listDevices(t *testing.T, s *store.Store) []store.Snapshotted[domain.Device] {
	t.Helper()
	ctx := context.Background()
	var devices []store.Snapshotted[domain.Device]
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		devices, err = tx.ListDevices(ctx)
		return err
	}); err != nil {
		t.Fatalf("list devices: %v", err)
	}
	return devices
}

func deviceActivity(t *testing.T, s *store.Store) map[domain.DeviceID]time.Time {
	t.Helper()
	ctx := context.Background()
	var activity map[domain.DeviceID]time.Time
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		activity, err = tx.ListDeviceActivity(ctx)
		return err
	}); err != nil {
		t.Fatalf("list device activity: %v", err)
	}
	return activity
}

func touchDevice(t *testing.T, s *store.Store, id domain.DeviceID, at time.Time, minInterval time.Duration) {
	t.Helper()
	ctx := context.Background()
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.TouchDeviceActivity(ctx, id, at, minInterval)
	}); err != nil {
		t.Fatalf("touch device %q: %v", id, err)
	}
}

// TestListDevices covers the list read: every device, active or revoked, in
// id order with the sync metadata its single Get reports, and an empty table
// as an empty, non-nil slice.
func TestListDevices(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	f := newFixtures(t)

	if got := listDevices(t, s); got == nil || len(got) != 0 {
		t.Fatalf("empty list = %#v, want an empty non-nil slice", got)
	}

	second := f.device
	second.ID = "device-0"
	second.DisplayName = "Ben's Mac"
	putDevices(t, s, f.device, second)
	revoked := f.device
	revoked.Status = domain.DeviceRevoked
	revokedAt := f.device.PairedAt.Add(time.Hour)
	revoked.RevokedAt = &revokedAt
	putDevices(t, s, revoked)

	got := listDevices(t, s)
	if len(got) != 2 {
		t.Fatalf("listed %d devices, want 2", len(got))
	}
	if got[0].Value.ID != second.ID || got[1].Value.ID != revoked.ID {
		t.Fatalf("list order = %q, %q; want %q, %q", got[0].Value.ID, got[1].Value.ID, second.ID, revoked.ID)
	}
	if got[0].Value.Status != domain.DeviceActive || got[1].Value.Status != domain.DeviceRevoked {
		t.Fatalf("statuses = %q, %q; want active, revoked", got[0].Value.Status, got[1].Value.Status)
	}
	for _, listed := range got {
		var snap store.Snapshot
		if err := s.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			_, snap, err = tx.GetDeviceSnapshot(ctx, listed.Value.ID)
			return err
		}); err != nil {
			t.Fatalf("get device %q: %v", listed.Value.ID, err)
		}
		if listed.Snapshot != snap {
			t.Errorf("device %q list snapshot = %+v, Get snapshot = %+v", listed.Value.ID, listed.Snapshot, snap)
		}
	}
	// Revocation rewrote the row, so its version advanced past the untouched one.
	if got[0].Snapshot.EntityVersion != 1 || got[1].Snapshot.EntityVersion != 2 {
		t.Errorf("entity versions = %d, %d; want 1, 2", got[0].Snapshot.EntityVersion, got[1].Snapshot.EntityVersion)
	}
}

// TestListDevicesFailsClosedOnCorruptRow pins the list to the single Get's
// gates: one row whose status column disagrees with its body fails the whole
// list instead of being skipped or served.
func TestListDevicesFailsClosedOnCorruptRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "devices.db")
	s := openStoreAt(t, path, store.Options{})
	f := newFixtures(t)
	second := f.device
	second.ID = "device-2"
	putDevices(t, s, f.device, second)

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, `UPDATE devices SET status = 'revoked' WHERE id = ?`, second.ID); err != nil {
		t.Fatal(err)
	}

	err = s.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.ListDevices(ctx)
		return err
	})
	if !errors.Is(err, store.ErrRowInconsistent) {
		t.Fatalf("list with a corrupt row = %v, want ErrRowInconsistent", err)
	}
}

// TestTouchDeviceActivity covers the recording rules: the first touch stores
// the instant, a touch inside the interval changes nothing, one at the
// interval refreshes, and the stored instant never moves backwards.
func TestTouchDeviceActivity(t *testing.T) {
	t.Parallel()
	s := openStore(t, store.Options{})
	f := newFixtures(t)
	putDevices(t, s, f.device)
	const interval = 5 * time.Minute
	first := f.device.PairedAt.Add(time.Hour)

	if got := deviceActivity(t, s); len(got) != 0 {
		t.Fatalf("activity before any touch = %v, want none", got)
	}

	for _, step := range []struct {
		name string
		at   time.Time
		want time.Time
	}{
		{"first touch", first, first},
		{"inside the interval", first.Add(interval - time.Nanosecond), first},
		{"at the interval", first.Add(interval), first.Add(interval)},
		{"clock stepped back", first.Add(-time.Hour), first.Add(interval)},
	} {
		touchDevice(t, s, f.device.ID, step.at, interval)
		got, ok := deviceActivity(t, s)[f.device.ID]
		if !ok || !got.Equal(step.want) {
			t.Fatalf("%s: last seen = %v (recorded %v), want %v", step.name, got, ok, step.want)
		}
		if got.Location() != time.UTC {
			t.Fatalf("%s: last seen location = %v, want UTC", step.name, got.Location())
		}
	}
}

// TestTouchDeviceActivityIsNotSynchronized is the reason the table exists:
// recording activity moves neither the server revision nor the device's
// entity version, so a client's own polling never invalidates a cache.
func TestTouchDeviceActivityIsNotSynchronized(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	f := newFixtures(t)
	putDevices(t, s, f.device)

	before, err := s.ServerState(ctx)
	if err != nil {
		t.Fatalf("ServerState: %v", err)
	}
	listedBefore := listDevices(t, s)
	touchDevice(t, s, f.device.ID, f.device.PairedAt.Add(time.Hour), time.Minute)
	touchDevice(t, s, f.device.ID, f.device.PairedAt.Add(2*time.Hour), time.Minute)
	after, err := s.ServerState(ctx)
	if err != nil {
		t.Fatalf("ServerState: %v", err)
	}
	if after != before {
		t.Fatalf("touching activity moved server state %+v -> %+v, want unchanged", before, after)
	}
	listedAfter := listDevices(t, s)
	if listedAfter[0].Snapshot != listedBefore[0].Snapshot {
		t.Fatalf("touching activity moved the device snapshot %+v -> %+v, want unchanged",
			listedBefore[0].Snapshot, listedAfter[0].Snapshot)
	}
}

// TestTouchDeviceActivityRejectsUnknownDevice pins the foreign key: activity
// is recorded only for a paired device, so a stray id leaves no row for the
// list to trip over.
func TestTouchDeviceActivityRejectsUnknownDevice(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})

	for _, id := range []domain.DeviceID{"", "device-unknown"} {
		err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
			return tx.TouchDeviceActivity(ctx, id, time.Unix(1_700_000_000, 0), time.Minute)
		})
		if err == nil {
			t.Errorf("touching device %q succeeded, want an error", id)
		}
	}
	if got := deviceActivity(t, s); len(got) != 0 {
		t.Fatalf("activity after rejected touches = %v, want none", got)
	}
}
