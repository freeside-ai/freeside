package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Pairing codes and device credentials are daemon-internal bookkeeping, like
// the inbox/outbox queues: they are never exposed through synchronization
// (minting a code must not invalidate client caches), so they carry no
// as_of_revision and their write methods live on InternalTx with non-Put
// names, keeping the #38 invariant honest (every Put* is a synchronized write
// on WriteTx). The pairing flow that creates a device runs one Write (the
// device is synchronized and bumps the revision) and reaches these methods
// through WriteTx's embedding, so code consumption, device creation, and
// credential recording commit atomically.
//
// Lifecycle *enforcement* (expiry at redemption, revocation stopping
// commands) is the attention service's job (plan §5.14 tests 13-16); this
// file owns the storage shapes that make those rules enforceable: one-way
// consumption, write-once credentials, and the single-winner conditional
// consume.

const mintPairingCodeSQL = `
INSERT INTO pairing_codes (code_hash, created_at, expires_at, consumed_at, device_id, body)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (code_hash) DO NOTHING`

// MintPairingCode records a freshly minted code. Write-once via the
// putImmutable convergence rules: a byte-identical replay converges on the
// stored row, a same-hash write with different content fails with
// ErrImmutableConflict. Minting accepts only unconsumed codes: consumption is
// recorded exclusively by ConsumePairingCode, so a crafted pre-consumed mint
// cannot fabricate a redemption (or burn a device's one-code slot) that never
// went through the single-winner path.
func (tx *InternalTx) MintPairingCode(ctx context.Context, code domain.PairingCode) error {
	if code.ConsumedAt != nil || code.DeviceID != nil {
		return fmt.Errorf("mint pairing code %q: a minted code must be unconsumed: %w",
			code.CodeHash, ErrImmutableConflict)
	}
	body, err := encode(code)
	if err != nil {
		return fmt.Errorf("mint pairing code %q: %w", code.CodeHash, err)
	}
	if err := tx.putImmutable(ctx, mintPairingCodeSQL,
		[]any{
			code.CodeHash, formatTime(code.CreatedAt), formatTime(code.ExpiresAt),
			formatTimePtr(code.ConsumedAt), code.DeviceID, body,
		},
		`SELECT body FROM pairing_codes WHERE code_hash = ?`, []any{code.CodeHash}, body); err != nil {
		return fmt.Errorf("mint pairing code %q: %w", code.CodeHash, err)
	}
	return nil
}

const consumePairingCodeSQL = `
UPDATE pairing_codes
SET consumed_at = ?, device_id = ?, body = ?
WHERE code_hash = ? AND device_id IS NULL`

// ConsumePairingCode records the code's one redemption by deviceID. The
// conditional update (WHERE device_id IS NULL) is the structural half of
// §5.14 test 14: at most one consume ever changes a row, so simultaneous
// pairing attempts with one code yield one device. A retry replaying the
// identical consumption converges; consuming an already-consumed code fails
// with ErrImmutableConflict. Expiry is deliberately not checked here: that
// rejection is redemption policy (test 13), owned by the attention service.
func (tx *InternalTx) ConsumePairingCode(ctx context.Context, codeHash domain.Digest, deviceID domain.DeviceID, consumedAt time.Time) error {
	stored, err := tx.GetPairingCode(ctx, codeHash)
	if err != nil {
		return fmt.Errorf("consume pairing code %q: %w", codeHash, err)
	}
	updated := stored
	updated.ConsumedAt = &consumedAt
	updated.DeviceID = &deviceID
	body, err := encode(updated)
	if err != nil {
		return fmt.Errorf("consume pairing code %q: %w", codeHash, err)
	}
	// The transition validator owns the one-way rule: recording a first
	// consumption passes, an identical replay passes (and converges below), a
	// different consumption is immutable-transition conflict.
	if err := domain.ValidatePairingCodeTransition(stored, updated); err != nil {
		return fmt.Errorf("consume pairing code %q: %w", codeHash, mapTransition(err))
	}
	if stored.ConsumedAt != nil {
		// The validator accepted, so this is a byte-identical replay of the
		// recorded consumption; converge without a write.
		return nil
	}
	// A device consumes at most one code, ever (the UNIQUE on device_id): map
	// the violation to the store's conflict error here instead of leaking a
	// raw constraint failure callers cannot errors.Is. The UNIQUE stays the
	// backstop.
	var occupied int
	err = tx.tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pairing_codes WHERE device_id = ?`, deviceID).Scan(&occupied)
	if err != nil {
		return fmt.Errorf("consume pairing code %q: %w", codeHash, err)
	}
	if occupied > 0 {
		return fmt.Errorf("consume pairing code %q: device %q already consumed a code: %w",
			codeHash, deviceID, ErrImmutableConflict)
	}
	res, err := tx.tx.ExecContext(ctx, consumePairingCodeSQL,
		formatTime(consumedAt), deviceID, body, codeHash)
	if err != nil {
		return fmt.Errorf("consume pairing code %q: %w", codeHash, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("consume pairing code %q: %w", codeHash, err)
	}
	if affected != 1 {
		// The row was unconsumed when read but not when updated: a concurrent
		// consume won. Single-winner, fail closed.
		return fmt.Errorf("consume pairing code %q: %w", codeHash, ErrImmutableConflict)
	}
	return nil
}

func (tx *ReadTx) GetPairingCode(ctx context.Context, codeHash domain.Digest) (domain.PairingCode, error) {
	var (
		createdAt  string
		expiresAt  string
		consumedAt sql.NullString
		deviceID   sql.NullString
		body       []byte
	)
	err := tx.tx.QueryRowContext(ctx,
		`SELECT created_at, expires_at, consumed_at, device_id, body FROM pairing_codes WHERE code_hash = ?`, codeHash).
		Scan(&createdAt, &expiresAt, &consumedAt, &deviceID, &body)
	if err != nil {
		return domain.PairingCode{}, fmt.Errorf("get pairing code %q: %w", codeHash, notFoundOr(err))
	}
	code, err := decode[domain.PairingCode](body)
	if err != nil {
		return domain.PairingCode{}, fmt.Errorf("get pairing code %q: %w", codeHash, err)
	}
	// Cross-check the extracted columns against the body: the consume
	// condition and the one-device-per-code UNIQUE act on the columns, so a
	// divergent body would be trusted domain data the constraints do not back.
	consistent := code.CodeHash == codeHash &&
		timeColumnEqual(createdAt, code.CreatedAt) &&
		timeColumnEqual(expiresAt, code.ExpiresAt) &&
		optionalTimeColumnEqual(consumedAt, code.ConsumedAt)
	if deviceID.Valid {
		consistent = consistent && code.DeviceID != nil && *code.DeviceID == domain.DeviceID(deviceID.String)
	} else {
		consistent = consistent && code.DeviceID == nil
	}
	if !consistent {
		return domain.PairingCode{}, fmt.Errorf("get pairing code %q: %w", codeHash, errRowInconsistent)
	}
	return code, nil
}

//nolint:gosec // constant SQL over the verifier-material table; no credential value appears
const recordDeviceCredentialSQL = `
INSERT INTO device_credentials (device_id, credential_kind, credential)
VALUES (?, ?, ?)
ON CONFLICT (device_id) DO NOTHING`

// RecordDeviceCredential stores a device's verifier material (a token digest
// or public key, per the domain's no-plaintext vocabulary). Write-once: a
// device's credential is fixed at pairing, so an identical replay converges
// and a different credential under the same device is ErrImmutableConflict
// (re-credentialing is a new pairing, hence a new device).
func (tx *InternalTx) RecordDeviceCredential(ctx context.Context, credential domain.DeviceCredential) error {
	if err := credential.Validate(); err != nil {
		return fmt.Errorf("record device credential %q: %w", credential.DeviceID, err)
	}
	res, err := tx.tx.ExecContext(ctx, recordDeviceCredentialSQL,
		credential.DeviceID, string(credential.Kind), credential.Credential)
	if err != nil {
		return fmt.Errorf("record device credential %q: %w", credential.DeviceID, err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("record device credential %q: %w", credential.DeviceID, err)
	}
	if inserted > 0 {
		return nil
	}
	existing, err := tx.GetDeviceCredential(ctx, credential.DeviceID)
	if err != nil {
		return fmt.Errorf("record device credential %q: %w", credential.DeviceID, err)
	}
	if existing != credential {
		return fmt.Errorf("record device credential %q: %w", credential.DeviceID, ErrImmutableConflict)
	}
	return nil
}

func (tx *ReadTx) GetDeviceCredential(ctx context.Context, deviceID domain.DeviceID) (domain.DeviceCredential, error) {
	var kind, material string
	err := tx.tx.QueryRowContext(ctx,
		`SELECT credential_kind, credential FROM device_credentials WHERE device_id = ?`, deviceID).
		Scan(&kind, &material)
	if err != nil {
		return domain.DeviceCredential{}, fmt.Errorf("get device credential %q: %w", deviceID, notFoundOr(err))
	}
	credential := domain.DeviceCredential{
		DeviceID: deviceID, Kind: domain.DeviceCredentialKind(kind), Credential: material,
	}
	// The row is columns-only (no body to cross-check); Validate is the
	// reconstruction backstop, rejecting a forged kind outside the
	// no-plaintext vocabulary.
	if err := credential.Validate(); err != nil {
		return domain.DeviceCredential{}, fmt.Errorf("get device credential %q: %w", deviceID, err)
	}
	return credential, nil
}

// formatTime renders a timestamp the way every Go-written column does
// (RFC3339Nano, UTC); the store never uses SQLite clock functions.
func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// formatTimePtr renders an optional timestamp, nil binding as NULL.
func formatTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatTime(*t)
}

// timeColumnEqual reports whether a stored RFC3339Nano column names the same
// instant as the body's field; an unparsable column is inconsistent, never
// equal.
func timeColumnEqual(column string, want time.Time) bool {
	parsed, err := time.Parse(time.RFC3339Nano, column)
	if err != nil {
		return false
	}
	return parsed.Equal(want)
}

// optionalTimeColumnEqual is timeColumnEqual for a nullable column and an
// optional body field: both absent, or both the same instant.
func optionalTimeColumnEqual(column sql.NullString, want *time.Time) bool {
	if !column.Valid || want == nil {
		return !column.Valid && want == nil
	}
	return timeColumnEqual(column.String, *want)
}
