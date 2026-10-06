package store

import (
	"context"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Device activity (migration 0094) is daemon-internal bookkeeping: when each
// device last made an authenticated request. It is never synchronized, so the
// write lives on InternalTx with a non-Put name and the rows carry no
// revision columns; recording it cannot move the server revision or a
// device's entity_version.

const touchDeviceActivitySQL = `
INSERT INTO device_activity (device_id, last_seen_at)
VALUES (?1, ?2)
ON CONFLICT (device_id) DO UPDATE SET last_seen_at = excluded.last_seen_at
WHERE excluded.last_seen_at - device_activity.last_seen_at >= ?3`

// TouchDeviceActivity records that the device was seen at the given instant.
// A device seen before is refreshed only when its stored instant is at least
// minInterval older, so a polling client costs one row write per interval,
// not one per request. The stored instant never moves backwards: a request
// stamped earlier than the stored one (a clock step) changes nothing.
func (tx *InternalTx) TouchDeviceActivity(ctx context.Context, id domain.DeviceID, at time.Time, minInterval time.Duration) error {
	if id == "" {
		return fmt.Errorf("touch device activity: device id: %w", domain.ErrEmptyID)
	}
	if _, err := tx.tx.ExecContext(ctx, touchDeviceActivitySQL,
		id, at.UTC().UnixNano(), minInterval.Nanoseconds()); err != nil {
		return fmt.Errorf("touch device activity %q: %w", id, err)
	}
	return nil
}

// ListDeviceActivity returns the last-seen instant of every device that has
// one. A device absent from the map has made no authenticated request since
// activity recording began.
func (tx *ReadTx) ListDeviceActivity(ctx context.Context) (map[domain.DeviceID]time.Time, error) {
	rows, err := tx.tx.QueryContext(ctx, `SELECT device_id, last_seen_at FROM device_activity`)
	if err != nil {
		return nil, fmt.Errorf("list device activity: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[domain.DeviceID]time.Time{}
	for rows.Next() {
		var (
			id         string
			lastSeenAt int64
		)
		if err := rows.Scan(&id, &lastSeenAt); err != nil {
			return nil, fmt.Errorf("list device activity: %w", err)
		}
		out[domain.DeviceID(id)] = time.Unix(0, lastSeenAt).UTC()
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list device activity: %w", err)
	}
	return out, nil
}
