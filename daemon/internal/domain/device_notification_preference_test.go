package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestDeviceNotificationPreferenceValidate(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		pref domain.DeviceNotificationPreference
		want error
	}{
		{"on", domain.DeviceNotificationPreference{DeviceID: "device-1", Enabled: true, UpdatedAt: at}, nil},
		{"off", domain.DeviceNotificationPreference{DeviceID: "device-1", UpdatedAt: at}, nil},
		{"no device", domain.DeviceNotificationPreference{Enabled: true, UpdatedAt: at}, domain.ErrEmptyID},
		{"no instant", domain.DeviceNotificationPreference{DeviceID: "device-1", Enabled: true}, domain.ErrMissingTimestamp},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.pref.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Validate() = %v, want %v", err, tc.want)
			}
		})
	}
}
