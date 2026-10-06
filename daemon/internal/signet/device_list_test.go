package signet_test

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// listDevicesOverHTTP reads GET /devices with the given token and returns the
// decoded entries keyed by device ID, plus the raw body.
func listDevicesOverHTTP(t *testing.T, handler http.Handler, token string) (map[domain.DeviceID]signet.DeviceListEntry, string) {
	t.Helper()
	response := bearerRequest(t, handler, http.MethodGet, "/devices", "Bearer "+token, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /devices status = %d body=%s, want 200", response.Code, response.Body.String())
	}
	var entries []signet.DeviceListEntry
	if err := json.Unmarshal(response.Body.Bytes(), &entries); err != nil {
		t.Fatalf("decode device list: %v", err)
	}
	if !slices.IsSortedFunc(entries, func(a, b signet.DeviceListEntry) int {
		return strings.Compare(string(a.Device.ID), string(b.Device.ID))
	}) {
		t.Errorf("device list is not ordered by id: %s", response.Body.String())
	}
	byID := make(map[domain.DeviceID]signet.DeviceListEntry, len(entries))
	for _, entry := range entries {
		byID[entry.Device.ID] = entry
	}
	return byID, response.Body.String()
}

// TestHTTPListDevices walks the device list over the wire with the real
// authorizer: it shows every device, active or revoked, reports last_seen_at
// as null until a device authenticates, refreshes it only once the recorded
// instant is a full granularity old, and never moves the server revision or a
// device's entity version by recording activity.
func TestHTTPListDevices(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	handler := signet.NewHTTPHandler(f.service, signet.NewRequestAuthorizer(f.store))
	callerToken, callerID := pairedDevice(t, f, "Caller")
	otherToken, otherID := pairedDevice(t, f, "Other")

	before, err := f.service.ListDevices(ctx)
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	if len(before) != 3 {
		t.Fatalf("listed %d devices, want the seeded one and two paired", len(before))
	}
	for _, entry := range before {
		if entry.LastSeenAt != nil {
			t.Errorf("device %q last_seen_at = %v before any authenticated request, want null",
				entry.Device.ID, entry.LastSeenAt)
		}
	}
	revision := f.revision(t)

	firstSeen := *f.now
	entries, _ := listDevicesOverHTTP(t, handler, callerToken)
	caller := entries[callerID]
	if caller.LastSeenAt == nil || !caller.LastSeenAt.Equal(firstSeen) {
		t.Fatalf("caller last_seen_at = %v, want its own request at %v", caller.LastSeenAt, firstSeen)
	}
	if caller.EntityVersion != 1 || caller.Device.Status != domain.DeviceActive || caller.Device.DisplayName != "Caller" {
		t.Errorf("caller entry = %+v, want the active version-1 device it paired as", caller)
	}
	if entries[otherID].LastSeenAt != nil || entries[f.device.ID].LastSeenAt != nil {
		t.Errorf("devices that never authenticated report last_seen_at: %+v", entries)
	}

	// Inside the granularity the recorded instant stays put; at it, it moves.
	*f.now = firstSeen.Add(5*time.Minute - time.Second)
	entries, _ = listDevicesOverHTTP(t, handler, callerToken)
	if got := entries[callerID].LastSeenAt; got == nil || !got.Equal(firstSeen) {
		t.Fatalf("last_seen_at inside the granularity = %v, want the first instant %v", got, firstSeen)
	}
	*f.now = firstSeen.Add(5 * time.Minute)
	entries, _ = listDevicesOverHTTP(t, handler, callerToken)
	if got := entries[callerID].LastSeenAt; got == nil || !got.Equal(*f.now) {
		t.Fatalf("last_seen_at at the granularity = %v, want %v", got, *f.now)
	}
	if got := f.revision(t); got != revision {
		t.Fatalf("recording activity moved the server revision %d -> %d", revision, got)
	}
	if got := entries[callerID].EntityVersion; got != 1 {
		t.Fatalf("recording activity moved the caller's entity_version to %d", got)
	}

	// The other device authenticates once, then is revoked: it stays in the
	// list as revoked and keeps the instant it was last seen.
	otherSeen := *f.now
	if response := bearerRequest(t, handler, http.MethodGet, "/sync/revision", "Bearer "+otherToken, nil); response.Code != http.StatusOK {
		t.Fatalf("other device read status = %d, want 200", response.Code)
	}
	*f.now = f.now.Add(time.Hour)
	revoke := bearerRequest(t, handler, http.MethodPost, "/devices/"+string(otherID)+"/revoke", "Bearer "+callerToken, nil)
	if revoke.Code != http.StatusOK {
		t.Fatalf("revoke status = %d body=%s, want 200", revoke.Code, revoke.Body.String())
	}
	entries, body := listDevicesOverHTTP(t, handler, callerToken)
	other := entries[otherID]
	if other.Device.Status != domain.DeviceRevoked || other.Device.RevokedAt == nil || other.EntityVersion != 2 {
		t.Fatalf("revoked entry = %+v, want revoked at entity_version 2", other)
	}
	if other.AsOfRevision != f.revision(t) || f.revision(t) != revision+1 {
		t.Errorf("revoked as_of_revision = %d at server revision %d, want the one revoking write after %d",
			other.AsOfRevision, f.revision(t), revision)
	}
	if other.LastSeenAt == nil || !other.LastSeenAt.Equal(otherSeen) {
		t.Errorf("revoked device last_seen_at = %v, want the instant it was last seen, %v", other.LastSeenAt, otherSeen)
	}
	if len(entries) != 3 {
		t.Errorf("listed %d devices after revocation, want 3: a revoked device stays listed", len(entries))
	}

	assertNoCredentialMaterial(t, f, body, callerToken, otherToken)
}

// assertNoCredentialMaterial is the no-credential rule of the device list:
// the body is exactly the contract's fields and holds no token, stored
// digest, or notification topic for any device.
func assertNoCredentialMaterial(t *testing.T, f fixture, body string, tokens ...string) {
	t.Helper()
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("decode raw device list: %v", err)
	}
	for _, entry := range raw {
		if got, want := slices.Sorted(maps.Keys(entry)), []string{"as_of_revision", "device", "entity_version", "last_seen_at"}; !slices.Equal(got, want) {
			t.Errorf("entry fields = %v, want %v", got, want)
		}
		var device map[string]json.RawMessage
		if err := json.Unmarshal(entry["device"], &device); err != nil {
			t.Fatalf("decode raw device: %v", err)
		}
		if got, want := slices.Sorted(maps.Keys(device)), []string{"display_name", "id", "paired_at", "revoked_at", "status"}; !slices.Equal(got, want) {
			t.Errorf("device fields = %v, want %v", got, want)
		}
	}

	forbidden := []string{"fsd1.", "sha256:", "credential", "public_key", "device_token", "ntfy", "topic"}
	for _, token := range tokens {
		parts := strings.Split(token, ".")
		forbidden = append(forbidden, token, parts[len(parts)-1])
	}
	if err := f.store.Read(context.Background(), func(tx *store.ReadTx) error {
		devices, err := tx.ListDevices(context.Background())
		if err != nil {
			return err
		}
		for _, device := range devices {
			credential, err := tx.GetDeviceCredential(context.Background(), device.Value.ID)
			if err != nil {
				continue // the seeded fixture device has no credential row
			}
			forbidden = append(forbidden, credential.Credential)
		}
		return nil
	}); err != nil {
		t.Fatalf("read stored credentials: %v", err)
	}
	for _, secret := range forbidden {
		if strings.Contains(body, secret) {
			t.Errorf("device list body contains credential material %q: %s", secret, body)
		}
	}
}

// TestHTTPActivityRecordingFailureDoesNotDenyTheRequest: last_seen_at is
// advisory, so a request whose activity cannot be recorded is still served.
// The authorizer here vouches for a device with no row, which the activity
// table's foreign key rejects.
func TestHTTPActivityRecordingFailureDoesNotDenyTheRequest(t *testing.T) {
	f := newFixture(t)
	handler := signet.NewHTTPHandler(f.service, func(*http.Request) (domain.DeviceID, bool) {
		return "device-without-a-row", true
	})

	response := bearerRequest(t, handler, http.MethodGet, "/devices", "Bearer unused", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200 despite the failed activity write", response.Code, response.Body.String())
	}
	var entries []signet.DeviceListEntry
	if err := json.Unmarshal(response.Body.Bytes(), &entries); err != nil {
		t.Fatalf("decode device list: %v", err)
	}
	for _, entry := range entries {
		if entry.LastSeenAt != nil {
			t.Errorf("device %q last_seen_at = %v, want null: the only request came from an unrecorded device",
				entry.Device.ID, entry.LastSeenAt)
		}
	}
}
