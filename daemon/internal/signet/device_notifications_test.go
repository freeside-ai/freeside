package signet_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// putNotifications sends PUT /devices/{id}/notifications with the given raw
// body through handler, authenticating with header.
func putNotifications(t *testing.T, handler http.Handler, header string, id domain.DeviceID, body string) (int, signet.DeviceListEntry, string) {
	t.Helper()
	response := bearerRequest(t, handler, http.MethodPut, "/devices/"+string(id)+"/notifications", header, []byte(body))
	var entry signet.DeviceListEntry
	if response.Code == http.StatusOK {
		if err := json.Unmarshal(response.Body.Bytes(), &entry); err != nil {
			t.Fatalf("decode entry: %v", err)
		}
	}
	return response.Code, entry, response.Body.String()
}

// TestHTTPSetDeviceNotifications walks the switch over the wire with the real
// authorizer: a freshly paired device lists it off, another device turns it
// on and off again, each change moves the revision once and stamps the
// instant it happened, and a replay returns the same entry and writes
// nothing, so the instant a device was turned on never moves.
func TestHTTPSetDeviceNotifications(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	handler := signet.NewHTTPHandler(f.service, signet.NewRequestAuthorizer(f.store))
	callerToken, _ := pairedDevice(t, f, "Caller")
	_, phoneID := pairedDevice(t, f, "Phone")
	auth := "Bearer " + callerToken

	entries, _ := listDevicesOverHTTP(t, handler, callerToken)
	for id, entry := range entries {
		if entry.NotificationsEnabled {
			t.Errorf("device %q lists notifications on before anyone set them", id)
		}
	}

	revision := f.revision(t)
	turnedOn := *f.now
	code, on, body := putNotifications(t, handler, auth, phoneID, `{"enabled":true}`)
	if code != http.StatusOK {
		t.Fatalf("turn on status = %d body=%s, want 200", code, body)
	}
	if !on.NotificationsEnabled || on.Device.ID != phoneID || on.Device.Status != domain.DeviceActive {
		t.Errorf("turn on entry = %+v, want the active phone with notifications on", on)
	}
	if after := f.revision(t); after != revision+1 {
		t.Errorf("turning on moved the revision %d → %d, want one step", revision, after)
	}
	if listed, _ := listDevicesOverHTTP(t, handler, callerToken); !listed[phoneID].NotificationsEnabled {
		t.Errorf("device list shows the phone off after it was turned on")
	}
	assertSwitchedAt(t, f, phoneID, true, turnedOn)

	*f.now = (*f.now).Add(time.Hour)
	revision = f.revision(t)
	code, replay, body := putNotifications(t, handler, auth, phoneID, `{"enabled":true}`)
	if code != http.StatusOK || replay != on {
		t.Fatalf("replay = %d %+v body=%s, want 200 and the same entry %+v", code, replay, body, on)
	}
	if after := f.revision(t); after != revision {
		t.Errorf("replay moved the revision %d → %d", revision, after)
	}
	assertSwitchedAt(t, f, phoneID, true, turnedOn)

	code, off, body := putNotifications(t, handler, auth, phoneID, `{"enabled":false}`)
	if code != http.StatusOK || off.NotificationsEnabled {
		t.Fatalf("turn off = %d %+v body=%s, want 200 with notifications off", code, off, body)
	}
	if after := f.revision(t); after != revision+1 {
		t.Errorf("turning off moved the revision %d → %d, want one step", revision, after)
	}
	assertSwitchedAt(t, f, phoneID, false, *f.now)

	// Revoking a device that was on leaves its stored switch, but it lists
	// off: the daemon notifies no revoked device.
	if code, _, body := putNotifications(t, handler, auth, phoneID, `{"enabled":true}`); code != http.StatusOK {
		t.Fatalf("turn on again = %d body=%s, want 200", code, body)
	}
	if _, err := f.service.Revoke(context.Background(), f.device.ID, phoneID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if listed, _ := listDevicesOverHTTP(t, handler, callerToken); listed[phoneID].NotificationsEnabled {
		t.Errorf("a revoked device lists notifications on")
	}
}

// assertSwitchedAt checks the stored switch, which the route never returns
// directly: its value and the instant it last changed.
func assertSwitchedAt(t *testing.T, f fixture, id domain.DeviceID, enabled bool, at time.Time) {
	t.Helper()
	ctx := context.Background()
	var pref domain.DeviceNotificationPreference
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		pref, err = tx.GetDeviceNotificationPreference(ctx, id)
		return err
	}); err != nil {
		t.Fatalf("GetDeviceNotificationPreference: %v", err)
	}
	if pref.Enabled != enabled || !pref.UpdatedAt.Equal(at) {
		t.Errorf("stored switch = %+v, want enabled %v at %v", pref, enabled, at)
	}
}

// TestHTTPSetDeviceNotificationsRefuses covers every refusal, each with no
// effect: an unauthenticated request (401), an unknown or revoked target
// (404, since a revoked device has no switch), a caller revoked after the
// middleware vouched for it (403, the in-transaction gate revokeDevice runs),
// and a body that is not one {"enabled": bool} (400).
func TestHTTPSetDeviceNotificationsRefuses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	handler := signet.NewHTTPHandler(f.service, signet.NewRequestAuthorizer(f.store))
	callerToken, _ := pairedDevice(t, f, "Caller")
	_, retiredID := pairedDevice(t, f, "Retired")
	_, doomedID := pairedDevice(t, f, "Doomed")
	if _, err := f.service.Revoke(ctx, f.device.ID, retiredID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := f.service.Revoke(ctx, f.device.ID, doomedID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	// A middleware still vouching for the revoked device is a revocation
	// racing authentication.
	raced := signet.NewHTTPHandler(f.service, func(*http.Request) (domain.DeviceID, bool) { return doomedID, true })
	auth := "Bearer " + callerToken
	before := f.revision(t)

	for name, tc := range map[string]struct {
		handler http.Handler
		header  string
		target  domain.DeviceID
		body    string
		want    int
	}{
		"no credential":      {handler, "", f.device.ID, `{"enabled":true}`, http.StatusUnauthorized},
		"unknown target":     {handler, auth, "device-never", `{"enabled":true}`, http.StatusNotFound},
		"revoked target":     {handler, auth, retiredID, `{"enabled":true}`, http.StatusNotFound},
		"revoked caller":     {raced, "", f.device.ID, `{"enabled":true}`, http.StatusForbidden},
		"missing enabled":    {handler, auth, f.device.ID, `{}`, http.StatusBadRequest},
		"unknown field":      {handler, auth, f.device.ID, `{"enabled":true,"posture":"all"}`, http.StatusBadRequest},
		"not a boolean":      {handler, auth, f.device.ID, `{"enabled":"yes"}`, http.StatusBadRequest},
		"two objects":        {handler, auth, f.device.ID, `{"enabled":true}{"enabled":false}`, http.StatusBadRequest},
		"duplicate key":      {handler, auth, f.device.ID, `{"enabled":true,"enabled":false}`, http.StatusBadRequest},
		"null enabled":       {handler, auth, f.device.ID, `{"enabled":null}`, http.StatusBadRequest},
		"empty request body": {handler, auth, f.device.ID, ``, http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			if code, _, body := putNotifications(t, tc.handler, tc.header, tc.target, tc.body); code != tc.want {
				t.Errorf("status = %d body=%s, want %d", code, body, tc.want)
			}
		})
	}
	if after := f.revision(t); after != before {
		t.Errorf("refused requests moved the revision %d → %d", before, after)
	}
	entries, err := f.service.ListDevices(ctx)
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	for _, entry := range entries {
		if entry.NotificationsEnabled {
			t.Errorf("device %q is on after only refused requests", entry.Device.ID)
		}
	}
}
