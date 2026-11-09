package domain

import (
	"fmt"
	"time"
)

// DeviceNotificationPreference is a device's notifications switch (plan §4:
// notifications are hints, the inbox is canonical). It lives beside Device,
// not on it, the way device activity does: a preference grants no authority,
// so it stays out of the device record the client-authority gates read. A
// device with no stored preference is off.
type DeviceNotificationPreference struct {
	DeviceID DeviceID `json:"device_id"`
	Enabled  bool     `json:"enabled"`
	// UpdatedAt is when the switch last changed value. While Enabled, it is
	// the instant the device was turned on, and the delivery dispatcher owes
	// the device only items created at or after it. A write that repeats
	// the stored value does not move it.
	UpdatedAt time.Time `json:"updated_at"`
}

// Validate reports whether the preference is structurally sound.
func (p DeviceNotificationPreference) Validate() error {
	if p.DeviceID == "" {
		return fmt.Errorf("device notification preference device_id: %w", ErrEmptyID)
	}
	if p.UpdatedAt.IsZero() {
		return fmt.Errorf("device notification preference updated_at: %w", ErrMissingTimestamp)
	}
	return nil
}
