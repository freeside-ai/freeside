package domain

import (
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// Device is the daemon-side record of a paired client device (plan §5.14). It
// is synchronized state: revocation changes what a device may do (§5.14 tests
// 15-16), so writing it bumps the server revision now, and the sync read
// surface that carries devices to clients arrives with its consumer (a later
// contract change) without reworking revision accounting. The record
// deliberately carries no credential material: the credential lives on
// DeviceCredential, a separate daemon-internal type, so no synchronized body
// or wire schema can leak a secret, or even its hash.
type Device struct {
	ID DeviceID `json:"id"`
	// DisplayName is the human label chosen at pairing ("Ben's iPhone"). It is
	// the one mutable field: renaming a device is not a lifecycle event.
	DisplayName string       `json:"display_name"`
	Status      DeviceStatus `json:"status"`
	PairedAt    time.Time    `json:"paired_at"`
	// RevokedAt is set exactly when Status is revoked. Revocation stops future
	// access only (plan §5.14: no remote wipe), and is a recorded final
	// outcome: once set it never changes or clears.
	RevokedAt *time.Time `json:"revoked_at"`
}

// Validate reports whether the device is structurally sound.
func (d Device) Validate() error {
	if d.ID == "" {
		return fmt.Errorf("device id: %w", ErrEmptyID)
	}
	if d.DisplayName == "" {
		return fmt.Errorf("device display_name: %w", ErrEmptyField)
	}
	if !d.Status.valid() {
		return fmt.Errorf("device status %q: %w", d.Status, ErrInvalidDeviceStatus)
	}
	if d.PairedAt.IsZero() {
		return fmt.Errorf("device paired_at: %w", ErrMissingTimestamp)
	}
	// revoked_at corresponds exactly to the revoked status, in both directions,
	// so a device's access state is never ambiguous between the two fields.
	// Behaviour dispatch on the enum, so no default: a new status must decide
	// its own timestamp rule here.
	switch d.Status {
	case DeviceActive:
		if d.RevokedAt != nil {
			return fmt.Errorf("device status %q with a revoked_at: %w", d.Status, ErrStatusTimestampTooStrong)
		}
	case DeviceRevoked:
		if d.RevokedAt == nil {
			return fmt.Errorf("device status %q without revoked_at: %w", d.Status, ErrStatusMissingTimestamp)
		}
	}
	if d.RevokedAt != nil {
		if d.RevokedAt.IsZero() {
			return fmt.Errorf("device revoked_at: %w", ErrMissingTimestamp)
		}
		if d.RevokedAt.Before(d.PairedAt) {
			return fmt.Errorf("device revoked_at before paired_at: %w", ErrTimestampOutOfOrder)
		}
	}
	return nil
}

// DeviceCredential is the daemon-internal verifier record for a paired
// device's credential (plan §5.14): the digest of an issued bearer token, or
// the device's public key, never a reusable plaintext secret. The plaintext
// exclusion is structural twice over: the vocabulary has no plaintext kind,
// and the type is deliberately separate from Device with no API schema, so
// neither the sync surface nor the wire can represent it.
type DeviceCredential struct {
	DeviceID DeviceID             `json:"device_id"`
	Kind     DeviceCredentialKind `json:"kind"`
	// Credential is the stored verifier material for the kind: the
	// "sha256:<hex>" digest of the full issued token for credential_hash
	// (shape-enforced by Validate, so a plaintext token is unrepresentable,
	// not just unconventional), or the public key for device_public_key
	// (format pinned when its consumer lands).
	Credential string `json:"credential"`
}

// Validate reports whether the credential record is structurally sound.
func (c DeviceCredential) Validate() error {
	if c.DeviceID == "" {
		return fmt.Errorf("device credential device_id: %w", ErrEmptyID)
	}
	if !c.Kind.valid() {
		return fmt.Errorf("device credential kind %q: %w", c.Kind, ErrInvalidCredentialKind)
	}
	if c.Credential == "" {
		return fmt.Errorf("device credential material: %w", ErrEmptyField)
	}
	if c.Kind == CredentialHash && !isSHA256Digest(c.Credential) {
		return fmt.Errorf("device credential material: %w", ErrPlaintextCredential)
	}
	return nil
}

// isSHA256Digest reports whether s is exactly "sha256:" plus 64 lowercase hex
// digits. Credential-equivalent material (a stored credential hash, a pairing
// code's identity) must be a real digest so a plaintext secret cannot pass
// Validate wearing a digest's field. Deliberately local to the credential
// surface: Digest at large stays an opaque content address.
func isSHA256Digest(s string) bool {
	return contentaddr.Valid(s)
}
