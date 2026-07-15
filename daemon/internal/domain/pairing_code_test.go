package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestPairingCodeValidate checks the pairing code's structural invariants: a
// real validity window, and consumption recorded as an all-or-nothing
// (consumed_at, device_id) pair.
func TestPairingCodeValidate(t *testing.T) {
	base := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	valid := domain.PairingCode{
		CodeHash: "sha256:e5da4a1cdb3c241cc8b3f2a9d7ba70a679960729bd9d8700791d412b34feef97", CreatedAt: base, ExpiresAt: base.Add(10 * time.Minute),
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid pairing code rejected: %v", err)
	}
	consumed := valid
	consumed.ConsumedAt = ptr(base.Add(time.Minute))
	consumed.DeviceID = ptr(domain.DeviceID("device-1"))
	if err := consumed.Validate(); err != nil {
		t.Fatalf("valid consumed pairing code rejected: %v", err)
	}
	// Consumption after expiry is not a structural fault: expiry is enforced at
	// redemption by the attention service (§5.14 test 13), and a row recording
	// a late consumption must remain readable evidence, not corrupt data.
	late := valid
	late.ConsumedAt = ptr(base.Add(time.Hour))
	late.DeviceID = ptr(domain.DeviceID("device-1"))
	if err := late.Validate(); err != nil {
		t.Fatalf("consumption after expiry rejected: %v", err)
	}

	tests := []struct {
		name    string
		mutate  func(*domain.PairingCode)
		wantErr error
	}{
		{"empty code_hash", func(p *domain.PairingCode) { p.CodeHash = "" }, domain.ErrEmptyID},
		{"plaintext code as code_hash", func(p *domain.PairingCode) { p.CodeHash = "483911" }, domain.ErrPlaintextCredential},
		{"zero created_at", func(p *domain.PairingCode) { p.CreatedAt = time.Time{} }, domain.ErrMissingTimestamp},
		{"zero expires_at", func(p *domain.PairingCode) { p.ExpiresAt = time.Time{} }, domain.ErrMissingTimestamp},
		{"expires before created", func(p *domain.PairingCode) {
			p.ExpiresAt = base.Add(-time.Minute)
		}, domain.ErrTimestampOutOfOrder},
		{"expires equal to created", func(p *domain.PairingCode) {
			p.ExpiresAt = base
		}, domain.ErrTimestampOutOfOrder},
		{"consumed_at without device_id", func(p *domain.PairingCode) {
			p.ConsumedAt = ptr(base.Add(time.Minute))
		}, domain.ErrConsumptionInconsistent},
		{"device_id without consumed_at", func(p *domain.PairingCode) {
			p.DeviceID = ptr(domain.DeviceID("device-1"))
		}, domain.ErrConsumptionInconsistent},
		{"zero consumed_at", func(p *domain.PairingCode) {
			p.ConsumedAt = &time.Time{}
			p.DeviceID = ptr(domain.DeviceID("device-1"))
		}, domain.ErrMissingTimestamp},
		{"consumed before created", func(p *domain.PairingCode) {
			p.ConsumedAt = ptr(base.Add(-time.Minute))
			p.DeviceID = ptr(domain.DeviceID("device-1"))
		}, domain.ErrTimestampOutOfOrder},
		{"empty consuming device_id", func(p *domain.PairingCode) {
			p.ConsumedAt = ptr(base.Add(time.Minute))
			p.DeviceID = ptr(domain.DeviceID(""))
		}, domain.ErrEmptyID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := valid
			tt.mutate(&p)
			if err := p.Validate(); !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}
