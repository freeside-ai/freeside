package domain

import (
	"fmt"
	"time"
)

// PairingCode is one short-lived pairing code minted by the daemon and
// displayed on the daemon host (plan §5.14). The code plaintext is
// credential-equivalent, so it is shown once and discarded: only a keyed
// digest persists, and redeeming presents the plaintext for the daemon to
// re-derive and look up. Consumption is recorded once as the (consumed_at,
// device_id) pair; expiry is a comparison against expires_at at redemption
// (the attention service's job, §5.14 test 13), not a stored state.
type PairingCode struct {
	// CodeHash is the code's identity: "sha256:<hex>" of HMAC-SHA256 over the
	// code plaintext under a daemon-held pairing key that never enters the
	// store. Keyed, because a displayed code is short: a bare unsalted hash
	// of a small input space would let a leaked database or checkpoint be
	// brute-forced offline while a code is still valid. Shape-enforced by
	// Validate so the plaintext itself cannot be persisted in the digest's
	// place; the key and derivation live with the attention service.
	CodeHash  Digest    `json:"code_hash"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	// ConsumedAt and DeviceID record the one redemption together: both absent
	// until the code is consumed, then both set, so a consumed code can never
	// create a second device (§5.14 tests 13-14).
	ConsumedAt *time.Time `json:"consumed_at"`
	DeviceID   *DeviceID  `json:"device_id"`
}

// Validate reports whether the pairing code is structurally sound.
func (p PairingCode) Validate() error {
	if p.CodeHash == "" {
		return fmt.Errorf("pairing code code_hash: %w", ErrEmptyID)
	}
	if !isSHA256Digest(string(p.CodeHash)) {
		return fmt.Errorf("pairing code code_hash: %w", ErrPlaintextCredential)
	}
	if p.CreatedAt.IsZero() {
		return fmt.Errorf("pairing code created_at: %w", ErrMissingTimestamp)
	}
	if p.ExpiresAt.IsZero() {
		return fmt.Errorf("pairing code expires_at: %w", ErrMissingTimestamp)
	}
	if !p.ExpiresAt.After(p.CreatedAt) {
		return fmt.Errorf("pairing code expires_at not after created_at: %w", ErrTimestampOutOfOrder)
	}
	if (p.ConsumedAt == nil) != (p.DeviceID == nil) {
		return fmt.Errorf("pairing code consumption pair: %w", ErrConsumptionInconsistent)
	}
	if p.ConsumedAt != nil {
		if p.ConsumedAt.IsZero() {
			return fmt.Errorf("pairing code consumed_at: %w", ErrMissingTimestamp)
		}
		if p.ConsumedAt.Before(p.CreatedAt) {
			return fmt.Errorf("pairing code consumed_at before created_at: %w", ErrTimestampOutOfOrder)
		}
		if *p.DeviceID == "" {
			return fmt.Errorf("pairing code device_id: %w", ErrEmptyID)
		}
	}
	return nil
}
