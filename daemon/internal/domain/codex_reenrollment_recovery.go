package domain

import (
	"fmt"
	"time"
)

// CodexReenrollmentRecoveryBinding identifies one verified replacement of a
// Codex identity's auth store. The binding is rendered on the system-health
// item and revalidated against the latest journal outcome before resolution.
type CodexReenrollmentRecoveryBinding struct {
	AuthIdentityID       AuthIdentityID `json:"auth_identity_id"`
	LeaseFence           int64          `json:"lease_fence"`
	AuthStoreDigest      Digest         `json:"auth_store_digest"`
	AccessTokenExpiresAt time.Time      `json:"access_token_expires_at"`
}

// Validate reports whether the binding names every verified coordinate.
func (b CodexReenrollmentRecoveryBinding) Validate() error {
	if b.AuthIdentityID == "" {
		return fmt.Errorf("codex re-enrollment identity: %w", ErrEmptyID)
	}
	if b.LeaseFence < 1 {
		return fmt.Errorf("codex re-enrollment lease fence %d: %w", b.LeaseFence, ErrNonPositive)
	}
	if b.AuthStoreDigest == "" {
		return fmt.Errorf("codex re-enrollment auth-store digest: %w", ErrEmptyField)
	}
	if b.AccessTokenExpiresAt.IsZero() {
		return fmt.Errorf("codex re-enrollment access-token expiry: %w", ErrMissingTimestamp)
	}
	if b.AccessTokenExpiresAt.Location() != time.UTC {
		return fmt.Errorf("codex re-enrollment access-token expiry: %w", ErrTimestampNotUTC)
	}
	return nil
}

// CodexReenrollmentRecoveryTransition records the accepted operator command
// that concluded a revoked-identity marker after verified re-enrollment.
type CodexReenrollmentRecoveryTransition struct {
	AuthIdentityID       AuthIdentityID `json:"auth_identity_id"`
	LeaseFence           int64          `json:"lease_fence"`
	AuthStoreDigest      Digest         `json:"auth_store_digest"`
	AccessTokenExpiresAt time.Time      `json:"access_token_expires_at"`
	CommandID            *string        `json:"command_id"`
	Reason               string         `json:"reason"`
	OccurredAt           time.Time      `json:"occurred_at"`
}

// Binding returns the verified operation coordinates this transition names.
func (t CodexReenrollmentRecoveryTransition) Binding() CodexReenrollmentRecoveryBinding {
	return CodexReenrollmentRecoveryBinding{
		AuthIdentityID: t.AuthIdentityID, LeaseFence: t.LeaseFence,
		AuthStoreDigest: t.AuthStoreDigest, AccessTokenExpiresAt: t.AccessTokenExpiresAt,
	}
}

// AuthorizingAction returns the sole action that can authorize this recovery.
func (CodexReenrollmentRecoveryTransition) AuthorizingAction() Action {
	return ActionResolveReenrollment
}

// Validate reports whether the transition is structurally sound.
func (t CodexReenrollmentRecoveryTransition) Validate() error {
	if err := t.Binding().Validate(); err != nil {
		return err
	}
	if t.CommandID == nil || *t.CommandID == "" {
		return fmt.Errorf("codex re-enrollment recovery transition: %w", ErrTransitionUnbacked)
	}
	if t.Reason == "" {
		return fmt.Errorf("codex re-enrollment recovery reason: %w", ErrEmptyField)
	}
	if t.OccurredAt.IsZero() {
		return fmt.Errorf("codex re-enrollment recovery occurred_at: %w", ErrMissingTimestamp)
	}
	if t.OccurredAt.Location() != time.UTC {
		return fmt.Errorf("codex re-enrollment recovery occurred_at: %w", ErrTimestampNotUTC)
	}
	return nil
}
