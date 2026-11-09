package store

import (
	"context"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Device notification preferences (migration 0131) hold each device's
// notifications switch. The rows carry no revision columns, but the device
// list reads them and a client changes them, so the write lives on WriteTx
// (the InternalTx comment in tx.go names this case): a changed switch moves
// the server revision, and that is what tells another client's Devices pane
// to reload. The method is not a Put because there is no entity version to
// stamp, the AppendUsageObservations precedent. A device with no row is off.

const setDeviceNotificationPreferenceSQL = `
INSERT INTO device_notification_preferences (device_id, enabled, updated_at)
VALUES (?1, ?2, ?3)
ON CONFLICT (device_id) DO UPDATE SET enabled = excluded.enabled, updated_at = excluded.updated_at`

// SetDeviceNotificationPreference stores the device's switch, replacing any
// earlier value. The caller decides whether the write is a change; this
// method writes what it is given.
func (tx *WriteTx) SetDeviceNotificationPreference(ctx context.Context, pref domain.DeviceNotificationPreference) error {
	if err := pref.Validate(); err != nil {
		return fmt.Errorf("set device notification preference: %w", err)
	}
	if _, err := tx.tx.ExecContext(ctx, setDeviceNotificationPreferenceSQL,
		pref.DeviceID, pref.Enabled, formatTime(pref.UpdatedAt)); err != nil {
		return fmt.Errorf("set device notification preference %q: %w", pref.DeviceID, err)
	}
	return nil
}

const deviceNotificationPreferenceColumns = `device_id, enabled, updated_at`

func scanDeviceNotificationPreference(row scanner) (domain.DeviceNotificationPreference, error) {
	var (
		id        string
		enabled   bool
		updatedAt string
	)
	if err := row.Scan(&id, &enabled, &updatedAt); err != nil {
		return domain.DeviceNotificationPreference{}, err
	}
	at, err := parseTime(updatedAt)
	if err != nil {
		return domain.DeviceNotificationPreference{}, fmt.Errorf("device notification preference %q updated_at: %w", id, err)
	}
	pref := domain.DeviceNotificationPreference{DeviceID: domain.DeviceID(id), Enabled: enabled, UpdatedAt: at}
	if err := pref.Validate(); err != nil {
		return domain.DeviceNotificationPreference{}, err
	}
	return pref, nil
}

// GetDeviceNotificationPreference returns the device's stored switch, or
// ErrNotFound when it has none (which reads as off).
func (tx *ReadTx) GetDeviceNotificationPreference(ctx context.Context, id domain.DeviceID) (domain.DeviceNotificationPreference, error) {
	pref, err := scanDeviceNotificationPreference(tx.tx.QueryRowContext(ctx,
		`SELECT `+deviceNotificationPreferenceColumns+` FROM device_notification_preferences WHERE device_id = ?`, id))
	if err != nil {
		return domain.DeviceNotificationPreference{}, fmt.Errorf("get device notification preference %q: %w", id, notFoundOr(err))
	}
	return pref, nil
}

// ListDeviceNotificationPreferences returns every stored switch by device.
// A device absent from the map has none and is off. A row that fails
// validation fails the whole read.
func (tx *ReadTx) ListDeviceNotificationPreferences(ctx context.Context) (map[domain.DeviceID]domain.DeviceNotificationPreference, error) {
	prefs, err := queryAll(ctx, tx.tx, scanDeviceNotificationPreference,
		`SELECT `+deviceNotificationPreferenceColumns+` FROM device_notification_preferences ORDER BY device_id`)
	if err != nil {
		return nil, fmt.Errorf("list device notification preferences: %w", err)
	}
	out := make(map[domain.DeviceID]domain.DeviceNotificationPreference, len(prefs))
	for _, pref := range prefs {
		out[pref.DeviceID] = pref
	}
	return out, nil
}
