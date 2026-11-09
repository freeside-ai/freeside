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

func setNotificationPreference(t *testing.T, s *store.Store, pref domain.DeviceNotificationPreference) {
	t.Helper()
	ctx := context.Background()
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.SetDeviceNotificationPreference(ctx, pref)
	}); err != nil {
		t.Fatalf("set notification preference %q: %v", pref.DeviceID, err)
	}
}

func notificationPreferences(t *testing.T, s *store.Store) map[domain.DeviceID]domain.DeviceNotificationPreference {
	t.Helper()
	ctx := context.Background()
	var prefs map[domain.DeviceID]domain.DeviceNotificationPreference
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		prefs, err = tx.ListDeviceNotificationPreferences(ctx)
		return err
	}); err != nil {
		t.Fatalf("list notification preferences: %v", err)
	}
	return prefs
}

func serverRevision(t *testing.T, s *store.Store) int64 {
	t.Helper()
	ctx := context.Background()
	var revision int64
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		state, err := tx.ServerState(ctx)
		revision = state.Revision
		return err
	}); err != nil {
		t.Fatalf("read server state: %v", err)
	}
	return revision
}

// TestDeviceNotificationPreferenceRoundTrip covers the switch's store
// surface: a device with no row is absent from the list and not found by
// Get, a set reads back exactly, a second set replaces the first, and each
// set moves the server revision (the signal another client reloads on).
func TestDeviceNotificationPreferenceRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	f := newFixtures(t)
	putDevices(t, s, f.device)

	if got := notificationPreferences(t, s); len(got) != 0 {
		t.Fatalf("preferences before any set = %#v, want none", got)
	}
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetDeviceNotificationPreference(ctx, f.device.ID)
		return err
	}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get with no row = %v, want ErrNotFound", err)
	}

	on := domain.DeviceNotificationPreference{
		DeviceID: f.device.ID, Enabled: true, UpdatedAt: f.device.PairedAt.Add(time.Hour),
	}
	before := serverRevision(t, s)
	setNotificationPreference(t, s, on)
	if after := serverRevision(t, s); after != before+1 {
		t.Fatalf("revision after set = %d, want %d", after, before+1)
	}
	var got domain.DeviceNotificationPreference
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		got, err = tx.GetDeviceNotificationPreference(ctx, f.device.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got != on {
		t.Fatalf("get = %+v, want %+v", got, on)
	}

	off := domain.DeviceNotificationPreference{DeviceID: f.device.ID, UpdatedAt: on.UpdatedAt.Add(time.Minute)}
	setNotificationPreference(t, s, off)
	if listed := notificationPreferences(t, s); len(listed) != 1 || listed[f.device.ID] != off {
		t.Fatalf("list after second set = %#v, want only %+v", listed, off)
	}
}

// TestSetDeviceNotificationPreferenceRefuses covers the write's refusals: an
// invalid preference never reaches the table, and the foreign key refuses a
// device that was never paired.
func TestSetDeviceNotificationPreferenceRefuses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	at := time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC)

	err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.SetDeviceNotificationPreference(ctx, domain.DeviceNotificationPreference{DeviceID: "device-1", Enabled: true})
	})
	if !errors.Is(err, domain.ErrMissingTimestamp) {
		t.Fatalf("set with no instant = %v, want ErrMissingTimestamp", err)
	}
	err = s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.SetDeviceNotificationPreference(ctx, domain.DeviceNotificationPreference{DeviceID: "never-paired", Enabled: true, UpdatedAt: at})
	})
	if err == nil {
		t.Fatal("set for an unpaired device succeeded, want the foreign key to refuse it")
	}
}

// TestListDeviceNotificationPreferencesFailsOnBadRow pins the Tier B decode:
// a row whose instant does not parse fails the whole read instead of being
// skipped or read as off.
func TestListDeviceNotificationPreferencesFailsOnBadRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "prefs.db")
	s := openStoreAt(t, path, store.Options{})
	f := newFixtures(t)
	putDevices(t, s, f.device)
	setNotificationPreference(t, s, domain.DeviceNotificationPreference{
		DeviceID: f.device.ID, Enabled: true, UpdatedAt: f.device.PairedAt,
	})

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx,
		`UPDATE device_notification_preferences SET updated_at = 'yesterday' WHERE device_id = ?`, f.device.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.ListDeviceNotificationPreferences(ctx)
		return err
	}); err == nil {
		t.Fatal("list over a bad row succeeded, want an error")
	}
}
