package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestDeviceValidate checks the device's structural invariants, in particular
// that revoked_at corresponds exactly to the revoked status in both
// directions, so a device's access state is never ambiguous.
func TestDeviceValidate(t *testing.T) {
	base := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	valid := domain.Device{
		ID: "device-1", DisplayName: "Ben's iPhone",
		Status: domain.DeviceActive, PairedAt: base,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid device rejected: %v", err)
	}
	revoked := valid
	revoked.Status = domain.DeviceRevoked
	revoked.RevokedAt = ptr(base.Add(time.Hour))
	if err := revoked.Validate(); err != nil {
		t.Fatalf("valid revoked device rejected: %v", err)
	}

	tests := []struct {
		name    string
		mutate  func(*domain.Device)
		wantErr error
	}{
		{"empty id", func(d *domain.Device) { d.ID = "" }, domain.ErrEmptyID},
		{"empty display_name", func(d *domain.Device) { d.DisplayName = "" }, domain.ErrEmptyField},
		{"invalid status", func(d *domain.Device) { d.Status = "paused" }, domain.ErrInvalidDeviceStatus},
		{"zero status", func(d *domain.Device) { d.Status = "" }, domain.ErrInvalidDeviceStatus},
		{"zero paired_at", func(d *domain.Device) { d.PairedAt = time.Time{} }, domain.ErrMissingTimestamp},
		{"active with revoked_at", func(d *domain.Device) {
			d.RevokedAt = ptr(base.Add(time.Hour))
		}, domain.ErrStatusTimestampTooStrong},
		{"revoked without revoked_at", func(d *domain.Device) {
			d.Status = domain.DeviceRevoked
		}, domain.ErrStatusMissingTimestamp},
		{"zero revoked_at", func(d *domain.Device) {
			d.Status = domain.DeviceRevoked
			d.RevokedAt = &time.Time{}
		}, domain.ErrMissingTimestamp},
		{"revoked_at before paired_at", func(d *domain.Device) {
			d.Status = domain.DeviceRevoked
			d.RevokedAt = ptr(base.Add(-time.Hour))
		}, domain.ErrTimestampOutOfOrder},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := valid
			tt.mutate(&d)
			if err := d.Validate(); !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// TestDeviceCredentialValidate checks the credential record's structural
// invariants.
func TestDeviceCredentialValidate(t *testing.T) {
	valid := domain.DeviceCredential{ //nolint:gosec // fixture digest of a fixture string, not a credential
		DeviceID: "device-1", Kind: domain.CredentialHash, Credential: "sha256:4d1566a1d7df42a8517456d60ea06ed284e535cfe4c956aa6ee172dbcdf945f7",
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid credential rejected: %v", err)
	}
	tests := []struct {
		name    string
		mutate  func(*domain.DeviceCredential)
		wantErr error
	}{
		{"empty device_id", func(c *domain.DeviceCredential) { c.DeviceID = "" }, domain.ErrEmptyID},
		{"invalid kind", func(c *domain.DeviceCredential) { c.Kind = "plaintext" }, domain.ErrInvalidCredentialKind},
		{"zero kind", func(c *domain.DeviceCredential) { c.Kind = "" }, domain.ErrInvalidCredentialKind},
		{"empty credential", func(c *domain.DeviceCredential) { c.Credential = "" }, domain.ErrEmptyField},
		{"plaintext under credential_hash", func(c *domain.DeviceCredential) {
			c.Credential = "hunter2-a-reusable-plaintext-token"
		}, domain.ErrPlaintextCredential},
		{"digest-prefixed plaintext", func(c *domain.DeviceCredential) {
			c.Credential = "sha256:not-actually-hex"
		}, domain.ErrPlaintextCredential},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := valid
			tt.mutate(&c)
			if err := c.Validate(); !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tt.wantErr)
			}
		})
	}

	// The digest shape is enforced per kind: a public key is not a digest, so
	// the hash-format rule must not reject the other sanctioned kind (its
	// format is pinned when its consumer lands).
	pubkey := domain.DeviceCredential{ //nolint:gosec // fixture public key, not a secret
		DeviceID: "device-1", Kind: domain.CredentialPublicKey,
		Credential: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEx4mple",
	}
	if err := pubkey.Validate(); err != nil {
		t.Fatalf("valid public-key credential rejected: %v", err)
	}
}

// TestCredentialKindVocabulary is the issue's acceptance criterion 1: the
// credential vocabulary is exactly the two plan-sanctioned non-reusable shapes
// (§5.14: hash or public key), and no member is or names a plaintext secret,
// so reusable plaintext is structurally unrepresentable in storage.
func TestCredentialKindVocabulary(t *testing.T) {
	want := map[domain.DeviceCredentialKind]bool{
		domain.CredentialHash:      true,
		domain.CredentialPublicKey: true,
	}
	if len(domain.AllDeviceCredentialKinds) != len(want) {
		t.Fatalf("AllDeviceCredentialKinds = %v, want exactly %d sanctioned shapes",
			domain.AllDeviceCredentialKinds, len(want))
	}
	for _, k := range domain.AllDeviceCredentialKinds {
		if !want[k] {
			t.Errorf("unexpected credential kind %q", k)
		}
		lower := strings.ToLower(string(k))
		for _, banned := range []string{"plaintext", "token", "secret", "password"} {
			if strings.Contains(lower, banned) {
				t.Errorf("credential kind %q names a reusable secret shape (%q)", k, banned)
			}
		}
	}
}
