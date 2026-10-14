package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// InstallationMintHold is the janitor's durable per-installation mint hold
// (issue #2004): while HeldUntil is in the future, no grant-read token may be
// minted for InstallationID, because an earlier token's outcome is unknown or
// its revoke failed. It is the store-side row for publish's
// InstallationMintHold, kept as flat typed fields so the store needs no
// knowledge of publish types.
//
// It has no token field: a token retained for revoke retries lives in process
// memory only, so no read path can leak it. Like InstallationMintAudit this is
// daemon-internal bookkeeping, never synchronized, so the write methods live
// on InternalTx with non-Put names (the #38 invariant).
type InstallationMintHold struct {
	InstallationID int64
	RegistrationID int64
	RequestedAt    time.Time
	HeldUntil      time.Time
}

const (
	recordInstallationMintHoldSQL = `
INSERT INTO publish_installation_mint_holds (installation_id, registration_id, requested_at, held_until)
VALUES (?, ?, ?, ?)
ON CONFLICT (installation_id) DO UPDATE SET
    registration_id = excluded.registration_id,
    requested_at    = excluded.requested_at,
    held_until      = excluded.held_until`
	releaseInstallationMintHoldSQL = `
DELETE FROM publish_installation_mint_holds WHERE installation_id = ?`
	readInstallationMintHoldSQL = `
SELECT registration_id, requested_at, held_until
FROM publish_installation_mint_holds WHERE installation_id = ?`
)

// validate holds the invariants every hold must meet, checked on write and
// again on read. The schema CHECKs cover the IDs and empty text, but failing
// here names the problem; the ordering check has no schema counterpart
// because the columns are text. Re-checking on read fails closed on a row
// edited or corrupted outside the store: a deadline at or before its request
// would otherwise read as already expired and let the janitor mint again.
func (hold InstallationMintHold) validate() error {
	if hold.InstallationID <= 0 {
		return fmt.Errorf("installation id %d is not positive", hold.InstallationID)
	}
	if hold.RegistrationID <= 0 {
		return fmt.Errorf("registration id %d is not positive", hold.RegistrationID)
	}
	if hold.RequestedAt.IsZero() || hold.HeldUntil.IsZero() {
		return errors.New("zero time")
	}
	if !hold.HeldUntil.After(hold.RequestedAt) {
		return errors.New("deadline is not after the request")
	}
	return nil
}

// RecordInstallationMintHold records hold as the installation's current hold,
// replacing any earlier row: a new mint request supersedes the earlier
// deadline. The janitor calls it before the mint request leaves the daemon,
// so the commit is the barrier that makes a crash after it safe.
func (tx *InternalTx) RecordInstallationMintHold(ctx context.Context, hold InstallationMintHold) error {
	if err := hold.validate(); err != nil {
		return fmt.Errorf("record installation mint hold: %w", err)
	}
	if _, err := tx.tx.ExecContext(ctx, recordInstallationMintHoldSQL,
		hold.InstallationID, hold.RegistrationID,
		formatTime(hold.RequestedAt), formatTime(hold.HeldUntil)); err != nil {
		return fmt.Errorf("record installation mint hold: %w", err)
	}
	return nil
}

// ReleaseInstallationMintHold deletes the installation's hold. Releasing an
// installation with no hold is not an error: the outcome the caller proved
// holds either way.
func (tx *InternalTx) ReleaseInstallationMintHold(ctx context.Context, installationID int64) error {
	if _, err := tx.tx.ExecContext(ctx, releaseInstallationMintHoldSQL, installationID); err != nil {
		return fmt.Errorf("release installation mint hold: %w", err)
	}
	return nil
}

// InstallationMintHold returns the installation's hold and whether one is
// recorded. An expired row is still returned; the caller decides expiry
// against its own clock. A row that fails the write-side invariants is an
// error, never a hold the caller could treat as expired.
func (tx *ReadTx) InstallationMintHold(ctx context.Context, installationID int64) (InstallationMintHold, bool, error) {
	hold := InstallationMintHold{InstallationID: installationID}
	var requestedAt, heldUntil string
	err := tx.tx.QueryRowContext(ctx, readInstallationMintHoldSQL, installationID).
		Scan(&hold.RegistrationID, &requestedAt, &heldUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return InstallationMintHold{}, false, nil
	}
	if err != nil {
		return InstallationMintHold{}, false, fmt.Errorf("read installation mint hold: %w", err)
	}
	if hold.RequestedAt, err = parseTime(requestedAt); err != nil {
		return InstallationMintHold{}, false, fmt.Errorf("read installation mint hold: stored requested_at invalid: %w", err)
	}
	if hold.HeldUntil, err = parseTime(heldUntil); err != nil {
		return InstallationMintHold{}, false, fmt.Errorf("read installation mint hold: stored held_until invalid: %w", err)
	}
	if err := hold.validate(); err != nil {
		return InstallationMintHold{}, false, fmt.Errorf("read installation mint hold: stored row invalid: %w", err)
	}
	return hold, true, nil
}
