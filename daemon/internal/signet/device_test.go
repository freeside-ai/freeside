package signet_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestRevokeDeviceTerminalIdempotent: revocation flips the device to its
// terminal state at the next entity_version; a re-revoke returns the same
// recorded snapshot without bumping the server revision (the API's
// "terminal and idempotent" contract).
func TestRevokeDeviceTerminalIdempotent(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.seedDevice(t, "device-admin")
	*f.now = f.now.Add(time.Minute)

	first, err := f.service.Revoke(ctx, "device-admin", f.device.ID)
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	device := first.Device
	if device.Status != domain.DeviceRevoked || device.RevokedAt == nil || !device.RevokedAt.Equal(*f.now) {
		t.Errorf("revoked device = %+v, want revoked at the service clock", device)
	}
	if first.EntityVersion != 2 {
		t.Errorf("revoked entity_version = %d, want 2 (the write after pairing)", first.EntityVersion)
	}
	afterRevoke := f.revision(t)
	if first.AsOfRevision != afterRevoke {
		t.Errorf("revoked as_of_revision = %d, want the revoking transaction's revision %d",
			first.AsOfRevision, afterRevoke)
	}

	*f.now = f.now.Add(time.Hour) // a later replay must not move RevokedAt
	replay, err := f.service.Revoke(ctx, "device-admin", f.device.ID)
	if err != nil {
		t.Fatalf("re-Revoke: %v", err)
	}
	if marshal(t, replay) != marshal(t, first) {
		t.Errorf("re-revocation changed the snapshot:\ngot:  %s\nwant: %s", marshal(t, replay), marshal(t, first))
	}
	if got := f.revision(t); got != afterRevoke {
		t.Errorf("idempotent re-revoke moved the server revision %d -> %d", afterRevoke, got)
	}
}

// TestRevokeClampsBackwardsClock: a clock behind PairedAt must neither block
// revocation (it is a security action) nor persist a revoked_at that
// violates the domain's revoked-after-paired ordering; it clamps to
// PairedAt.
func TestRevokeClampsBackwardsClock(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.seedDevice(t, "device-admin")
	*f.now = f.device.PairedAt.Add(-time.Hour)

	snapshot, err := f.service.Revoke(ctx, "device-admin", f.device.ID)
	if err != nil {
		t.Fatalf("Revoke under a backwards clock: %v", err)
	}
	device := snapshot.Device
	if device.Status != domain.DeviceRevoked || device.RevokedAt == nil || !device.RevokedAt.Equal(f.device.PairedAt) {
		t.Errorf("revoked device = %+v, want revoked_at clamped to paired_at %v", device, f.device.PairedAt)
	}
	if err := device.Validate(); err != nil {
		t.Errorf("persisted revoked device is domain-invalid: %v", err)
	}
}

// TestRevokeRejectsRevokedCaller closes the authorize-then-revoke race: a
// device revoked after the HTTP middleware authorized its request must not
// have that request commit a revocation of another device (a compromised
// device retaliating against the owner's remaining one). The caller's gate
// runs inside the revoking transaction, like Submit's.
func TestRevokeRejectsRevokedCaller(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.seedDevice(t, "device-2")

	if _, err := f.service.Revoke(ctx, f.device.ID, f.device.ID); err != nil {
		t.Fatalf("self-revoke: %v", err)
	}
	if _, err := f.service.Revoke(ctx, f.device.ID, "device-2"); !errors.Is(err, signet.ErrDeviceNotActive) {
		t.Fatalf("revoked caller's Revoke error = %v, want ErrDeviceNotActive", err)
	}
	var target domain.Device
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		target, err = tx.GetDevice(ctx, "device-2")
		return err
	}); err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	if target.Status != domain.DeviceActive {
		t.Errorf("target device = %q, want still active", target.Status)
	}
}

func TestRevokeUnknownDevice(t *testing.T) {
	f := newFixture(t)
	if _, err := f.service.Revoke(context.Background(), f.device.ID, "device-ghost"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Revoke error = %v, want ErrNotFound", err)
	}
}

// TestRevokedDeviceCannotSubmitPreparedCommand is §5.14 test 15: a command
// prepared while the device was active, but not yet committed, is rejected
// after revocation, leaving the item and the server revision untouched.
func TestRevokedDeviceCannotSubmitPreparedCommand(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	prepared := f.command("cmd-prepared", domain.ActionStop)

	if _, err := f.service.Revoke(ctx, f.device.ID, f.device.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	before := f.revision(t)
	if _, err := f.service.Submit(ctx, prepared); !errors.Is(err, signet.ErrDeviceNotActive) {
		t.Fatalf("Submit error = %v, want ErrDeviceNotActive", err)
	}
	if after := f.revision(t); after != before {
		t.Errorf("rejected command moved the server revision %d -> %d", before, after)
	}
	item, _ := f.itemSnapshot(t)
	if item.Status != domain.StatusOpen || item.ItemVersion != 1 {
		t.Errorf("item = %q v%d, want the untouched open v1", item.Status, item.ItemVersion)
	}
}

// TestSubmitRejectsUnknownDevice: the gate reads the durable device row, so
// an identity that was never paired is rejected the same way (defense in
// depth behind the HTTP authorizer).
func TestSubmitRejectsUnknownDevice(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	command := f.command("cmd-ghost", domain.ActionStop)
	command.DeviceID = "device-ghost"
	if _, err := f.service.Submit(ctx, command); !errors.Is(err, signet.ErrDeviceNotActive) {
		t.Fatalf("Submit error = %v, want ErrDeviceNotActive", err)
	}
}

// TestCommandRetryAfterRevocationNoNewEffect is §5.14 test 16: a retry of a
// command committed before revocation may return its recorded result, and
// produces no new side effect — the replay branch never writes, so the item
// and the server revision stay exactly where the original commit left them.
func TestCommandRetryAfterRevocationNoNewEffect(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	original, err := f.service.Submit(ctx, f.command("cmd-committed", domain.ActionStop))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if _, err := f.service.Revoke(ctx, f.device.ID, f.device.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	afterRevoke := f.revision(t)

	retried, err := f.service.Submit(ctx, f.command("cmd-committed", domain.ActionStop))
	if err != nil {
		t.Fatalf("retry Submit: %v", err)
	}
	if marshal(t, retried) != marshal(t, original) {
		t.Errorf("retry changed the recorded result:\ngot:  %s\nwant: %s",
			marshal(t, retried), marshal(t, original))
	}
	if got := f.revision(t); got != afterRevoke {
		t.Errorf("retry moved the server revision %d -> %d", afterRevoke, got)
	}
	item, _ := f.itemSnapshot(t)
	if item.Status != domain.StatusResolved || item.ItemVersion != 2 {
		t.Errorf("item = %q v%d, want the original resolution (resolved v2) with no second effect",
			item.Status, item.ItemVersion)
	}
}

// pairedDevice runs the real pairing flow and returns the granted token and
// device ID, for authorizer tests that need a verifiable credential.
func pairedDevice(t *testing.T, f fixture, label string) (string, domain.DeviceID) {
	t.Helper()
	ctx := context.Background()
	plaintext, _, err := f.service.MintPairingCode(ctx)
	if err != nil {
		t.Fatalf("MintPairingCode: %v", err)
	}
	grant, err := f.service.Pair(ctx, plaintext, label)
	if err != nil {
		t.Fatalf("Pair: %v", err)
	}
	return grant.DeviceToken, grant.Device.Device.ID
}

func authorize(f fixture, header string) (domain.DeviceID, bool) {
	request := httptest.NewRequest(http.MethodGet, "/sync/revision", nil)
	if header != "" {
		request.Header.Set("Authorization", header)
	}
	return signet.NewRequestAuthorizer(f.store)(request)
}

// TestRequestAuthorizerVerifiesCredential: the granted token authenticates
// as exactly the device it embeds; revocation stops it.
func TestRequestAuthorizerVerifiesCredential(t *testing.T) {
	f := newFixture(t)
	token, deviceID := pairedDevice(t, f, "Authorized device")

	// The auth-scheme is case-insensitive with one or more spaces
	// (RFC 7235 §2.1); all three forms carry the same credential.
	for _, header := range []string{"Bearer " + token, "bearer " + token, "Bearer  " + token} {
		got, ok := authorize(f, header)
		if !ok || got != deviceID {
			t.Fatalf("authorize(%q) = (%q, %v), want (%q, true)", header, got, ok, deviceID)
		}
	}
	if _, err := f.service.Revoke(context.Background(), f.device.ID, deviceID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if got, ok := authorize(f, "Bearer "+token); ok {
		t.Fatalf("revoked credential still authorizes as %q", got)
	}
}

// TestRequestAuthorizerRejectsForgedTokens enumerates the presented-token
// input space: every malformed, mismatched, or unverifiable credential is
// the same bare denial.
func TestRequestAuthorizerRejectsForgedTokens(t *testing.T) {
	f := newFixture(t)
	token, _ := pairedDevice(t, f, "Victim device")
	parts := strings.Split(token, ".")

	flip := func(s string) string {
		if s[len(s)-1] == 'A' {
			return s[:len(s)-1] + "B"
		}
		return s[:len(s)-1] + "A"
	}
	cases := map[string]string{
		"no header":              "",
		"not bearer":             "Basic " + token,
		"bare token":             token[len("fsd1."):],
		"empty bearer":           "Bearer ",
		"wrong prefix":           "Bearer fsd2." + parts[1] + "." + parts[2],
		"two segments":           "Bearer fsd1." + parts[1],
		"four segments":          "Bearer " + token + ".extra",
		"empty device id":        "Bearer fsd1.." + parts[2],
		"invalid base64 id":      "Bearer fsd1.!!!." + parts[2],
		"empty secret":           "Bearer fsd1." + parts[1] + ".",
		"wrong secret":           "Bearer fsd1." + parts[1] + "." + flip(parts[2]),
		"unknown device":         "Bearer fsd1.ZGV2aWNlLWdob3N0." + parts[2],
		"credential-less device": "Bearer fsd1.ZGV2aWNlLTE." + parts[2], // seeded device-1 has no credential row
		"case-shifted device id": "Bearer fsd1." + strings.ToUpper(parts[1]) + "." + parts[2],
	}
	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			if got, ok := authorize(f, header); ok {
				t.Fatalf("header %q authorized as %q", header, got)
			}
		})
	}
}
