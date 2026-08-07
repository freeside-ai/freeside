package publish

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var expiryNow = time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)

// TestCheckInstallationTokenExpiry pins the bound itself, independently
// of any mint path: the arithmetic is stated in terms of the declared
// lifetime and skew rather than a copied literal, so widening either
// constant cannot silently pass an unchanged test.
func TestCheckInstallationTokenExpiry(t *testing.T) {
	t.Parallel()
	bound := expiryNow.Add(installationTokenLifetime + installationTokenSkew)
	accepted := []struct {
		name string
		raw  string
		want time.Time
	}{
		{"one-hour lifetime", "2026-07-16T13:00:00Z", expiryNow.Add(time.Hour)},
		{"non-UTC zone", "2026-07-16T15:00:00+02:00", expiryNow.Add(time.Hour)},
		{"exactly at the bound", bound.Format(time.RFC3339Nano), bound},
		{"one nanosecond inside the bound", bound.Add(-time.Nanosecond).Format(time.RFC3339Nano), bound.Add(-time.Nanosecond)},
		{"one nanosecond in the future", expiryNow.Add(time.Nanosecond).Format(time.RFC3339Nano), expiryNow.Add(time.Nanosecond)},
	}
	for _, tc := range accepted {
		t.Run("accepts "+tc.name, func(t *testing.T) {
			got, err := checkInstallationTokenExpiry(Secret(tc.raw), expiryNow)
			if err != nil {
				t.Fatalf("checkInstallationTokenExpiry(%q) = %v", tc.raw, err)
			}
			if !got.Equal(tc.want) || got.Location() != time.UTC {
				t.Errorf("expiry = %v, want %v as a UTC instant", got, tc.want)
			}
		})
	}

	rejected := []struct {
		name string
		raw  string
	}{
		{"missing", ""},
		{"whitespace", " "},
		{"malformed", "not-a-timestamp"},
		{"date only", "2026-07-16"},
		{"unix seconds", "1784206800"},
		{"RFC1123", "Thu, 16 Jul 2026 13:00:00 GMT"},
		{"no zone", "2026-07-16T13:00:00"},
		{"lapsed", "2026-07-16T11:00:00Z"},
		{"exactly now", "2026-07-16T12:00:00Z"},
		{"one nanosecond past the bound", bound.Add(time.Nanosecond).Format(time.RFC3339Nano)},
		{"a day out", "2026-07-17T12:00:00Z"},
		{"a century out", "2126-07-16T13:00:00Z"},
		{"past the bound in another zone", bound.Add(time.Hour).In(time.FixedZone("+02:00", 7200)).Format(time.RFC3339Nano)},
	}
	for _, tc := range rejected {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			got, err := checkInstallationTokenExpiry(Secret(tc.raw), expiryNow)
			if !errors.Is(err, errTokenExpiry) {
				t.Fatalf("checkInstallationTokenExpiry(%q) = %v, %v, want errTokenExpiry", tc.raw, got, err)
			}
			if !got.IsZero() {
				t.Errorf("rejected expiry returned %v, want the zero time", got)
			}
			// A blank value has no distinguishing text to leak, and
			// strings.Contains matches the empty string everywhere.
			if strings.TrimSpace(tc.raw) != "" && strings.Contains(err.Error(), tc.raw) {
				t.Errorf("error carries the rejected value %q: %v", tc.raw, err)
			}
		})
	}
}
