package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Provider identities and their auth-store mutation leases (plan §5.4).
// Daemon-internal like the trust records: never synchronized, so the writes
// live on InternalTx with non-Put names (the #38 invariant) and the rows carry
// no entity_version/as_of_revision.
//
// The lease is the serialization point, not a note about one. One row per
// identity makes "at most one holder" the primary key; a takeover bumps the
// fence, so a holder that stalled past its expiry and woke up again presents a
// fence the row has left behind and is refused. Liveness is always decided
// against a caller-supplied instant: the store has no clock, and a row saying
// "held until T" is a claim, not an observation.

// ErrLeaseHeld is returned when a live lease belongs to another holder.
// LeaseHeldError unwraps to it, so errors.Is matches the class while
// errors.As reaches the current holder.
var ErrLeaseHeld = errors.New("auth store mutation lease is held by another holder")

// ErrLeaseNotHeld is returned when a caller's holder or fence does not match
// the live lease it is trying to renew or release. A stalled holder that woke
// after a takeover lands here.
var ErrLeaseNotHeld = errors.New("caller does not hold the auth store mutation lease")

// authIdentityRecord is the persisted shape of an identity declaration: the
// declaration itself plus the instant that orders its revisions. recorded_at
// is the authority requireForwardRevision compares against, so it cannot be a
// bare column — moved backward it would let a superseded declaration overwrite
// the current one, moved forward it would block legitimate revisions. Carrying
// it in the validated body, cross-checked against the column, puts it under
// the same authentication as every other field.
//
// It is package-private: a persistence format, not one of the contract shapes
// the goldens pin.
type authIdentityRecord struct {
	Identity   domain.AuthIdentity `json:"identity"`
	RecordedAt time.Time           `json:"recorded_at"`
}

func (r authIdentityRecord) Validate() error {
	if err := r.Identity.Validate(); err != nil {
		return err
	}
	if r.RecordedAt.IsZero() {
		return fmt.Errorf("auth identity %s recorded_at: %w", r.Identity.ID, domain.ErrMissingTimestamp)
	}
	if r.RecordedAt.Location() != time.UTC {
		return fmt.Errorf("auth identity %s recorded_at: %w", r.Identity.ID, domain.ErrTimestampNotUTC)
	}
	return nil
}

// ErrLeaseWindowRegresses is returned when a lease window would be set to an
// instant that has already passed, or when a renewal would move an existing
// expiry earlier. Either would hand the caller a "held" lease that another
// holder may already take, so both are refused rather than recorded.
var ErrLeaseWindowRegresses = errors.New("auth store mutation lease window would not extend into the future")

// ErrLeaseNotDeclared is returned when an identity's declaration does not
// require an auth-store mutation lease. Taking or reconstructing a lease
// against such an identity fails closed rather than granting an exclusion the
// identity never asked for.
var ErrLeaseNotDeclared = errors.New("auth identity does not declare an auth store mutation lease")

// LeaseHeldError names the live holder that refused an acquisition, so a
// caller can report or wait without parsing an error string.
type LeaseHeldError struct {
	AuthIdentityID domain.AuthIdentityID
	Holder         domain.InvocationID
	Fence          int64
	ExpiresAt      time.Time
}

func (e *LeaseHeldError) Error() string {
	return fmt.Sprintf("auth store mutation lease on %q held by %q (fence %d) until %s",
		e.AuthIdentityID, e.Holder, e.Fence, e.ExpiresAt.Format(time.RFC3339Nano))
}

// Unwrap makes errors.Is(err, ErrLeaseHeld) match the refusal class.
func (e *LeaseHeldError) Unwrap() error { return ErrLeaseHeld }

const (
	recordAuthIdentitySQL = `
INSERT INTO auth_identities
    (id, provider, auth_store_mutation_lease, max_parallel_executions,
     refresh_strategy, supports_read_only_auth_snapshot, recorded_at, body)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    provider                         = excluded.provider,
    auth_store_mutation_lease        = excluded.auth_store_mutation_lease,
    max_parallel_executions          = excluded.max_parallel_executions,
    refresh_strategy                 = excluded.refresh_strategy,
    supports_read_only_auth_snapshot = excluded.supports_read_only_auth_snapshot,
    recorded_at                      = excluded.recorded_at,
    body                             = excluded.body`
	getAuthIdentitySQL = `
SELECT provider, auth_store_mutation_lease, max_parallel_executions,
       refresh_strategy, supports_read_only_auth_snapshot, recorded_at, body
FROM auth_identities WHERE id = ?`

	getLeaseSQL = `
SELECT holder, fence, acquired_at, expires_at, released_at, body
FROM auth_store_mutation_leases WHERE auth_identity_id = ?`
	insertLeaseSQL = `
INSERT INTO auth_store_mutation_leases
    (auth_identity_id, holder, fence, acquired_at, expires_at, expires_at_unix_nano, released_at, body)
VALUES (?, ?, ?, ?, ?, ?, NULL, ?)
ON CONFLICT (auth_identity_id) DO NOTHING`
	// The takeover and renewal guards name the exact row that was read, so a
	// row that moved between the read and the write fails closed instead of
	// overwriting whatever is there now.
	takeoverLeaseSQL = `
UPDATE auth_store_mutation_leases
SET holder = ?, fence = ?, acquired_at = ?, expires_at = ?, expires_at_unix_nano = ?,
    released_at = NULL, body = ?
WHERE auth_identity_id = ? AND fence = ?`
	renewLeaseSQL = `
UPDATE auth_store_mutation_leases
SET expires_at = ?, expires_at_unix_nano = ?, body = ?
WHERE auth_identity_id = ? AND holder = ? AND fence = ? AND released_at IS NULL`
	releaseLeaseSQL = `
UPDATE auth_store_mutation_leases
SET released_at = ?, body = ?
WHERE auth_identity_id = ? AND holder = ? AND fence = ? AND released_at IS NULL`
)

// RecordAuthIdentity persists an identity declaration, guarded by the domain
// transition rule: the provider and the lease requirement are fixed, so an
// update may only re-measure the parallelism limit or record new snapshot
// support. recordedAt orders revisions; it is not part of the declaration.
func (tx *InternalTx) RecordAuthIdentity(ctx context.Context, identity domain.AuthIdentity, recordedAt time.Time) error {
	if recordedAt.IsZero() {
		return fmt.Errorf("record auth identity %q: zero recorded_at", identity.ID)
	}
	body, err := encode(authIdentityRecord{Identity: identity, RecordedAt: recordedAt.UTC()})
	if err != nil {
		return fmt.Errorf("record auth identity %q: %w", identity.ID, err)
	}
	stored, err := tx.GetAuthIdentity(ctx, identity.ID)
	switch {
	case err == nil:
		if err := domain.ValidateAuthIdentityTransition(stored, identity); err != nil {
			return fmt.Errorf("record auth identity %q: %w", identity.ID, mapTransition(err))
		}
		if err := tx.requireForwardRevision(ctx, identity.ID, recordedAt, stored, identity); err != nil {
			return fmt.Errorf("record auth identity %q: %w", identity.ID, err)
		}
	case !errors.Is(err, ErrNotFound):
		return fmt.Errorf("record auth identity %q: %w", identity.ID, err)
	}
	if _, err := tx.tx.ExecContext(ctx, recordAuthIdentitySQL,
		identity.ID, identity.Provider, identity.AuthStoreMutationLease,
		identity.MaxParallelExecutions, identity.RefreshStrategy,
		identity.SupportsReadOnlyAuthSnapshot, formatTime(recordedAt), body); err != nil {
		return fmt.Errorf("record auth identity %q: %w", identity.ID, err)
	}
	return nil
}

// requireForwardRevision refuses a declaration stamped before the stored one.
// recorded_at is what orders revisions, and 1B re-measures the parallelism
// limit, so a delayed older measurement arriving after a newer one would
// otherwise reinstate a superseded limit — raising concurrency past the latest
// safe result, which is the direction that matters.
//
// The same instant is only accepted for an identical declaration. A reused or
// coarse timestamp carries no ordering evidence at all, so a divergent body
// sharing it is a conflict rather than an update: taking it would let a
// conflicting retry restore a superseded limit through the equality case the
// staleness check leaves open.
func (tx *InternalTx) requireForwardRevision(
	ctx context.Context, id domain.AuthIdentityID, recordedAt time.Time,
	stored, proposed domain.AuthIdentity,
) error {
	storedAt, err := tx.authIdentityRecordedAt(ctx, id)
	if err != nil {
		return err
	}
	column := storedAt.Format(time.RFC3339Nano)
	if recordedAt.Before(storedAt) {
		return fmt.Errorf("revision stamped %s, stored revision is %s: %w",
			recordedAt.Format(time.RFC3339Nano), column, ErrStaleWrite)
	}
	if recordedAt.Equal(storedAt) && stored != proposed {
		return fmt.Errorf("divergent revision shares the stored instant %s: %w", column, ErrStaleWrite)
	}
	return nil
}

// authIdentityRecordedAt returns the authenticated revision instant: read
// through the same reconstruction as the declaration, so the ordering
// authority is the cross-checked body's value rather than a bare column an
// edit could move in either direction.
func (tx *ReadTx) authIdentityRecordedAt(ctx context.Context, id domain.AuthIdentityID) (time.Time, error) {
	_, recordedAt, err := tx.getAuthIdentityRecord(ctx, id)
	return recordedAt, err
}

// GetAuthIdentity reconstructs one identity declaration, cross-checking the
// extracted columns against the decoded body.
func (tx *ReadTx) GetAuthIdentity(ctx context.Context, id domain.AuthIdentityID) (domain.AuthIdentity, error) {
	identity, _, err := tx.getAuthIdentityRecord(ctx, id)
	return identity, err
}

// getAuthIdentityRecord is the single reconstruction path for the declaration
// and its revision instant: scan, decode, and cross-check every extracted
// column against the body, the timestamp included.
func (tx *ReadTx) getAuthIdentityRecord(
	ctx context.Context, id domain.AuthIdentityID,
) (domain.AuthIdentity, time.Time, error) {
	var (
		provider   string
		lease      bool
		parallel   int
		refresh    string
		snapshots  bool
		recordedAt string
		body       []byte
	)
	err := tx.tx.QueryRowContext(ctx, getAuthIdentitySQL, id).
		Scan(&provider, &lease, &parallel, &refresh, &snapshots, &recordedAt, &body)
	if err != nil {
		return domain.AuthIdentity{}, time.Time{}, fmt.Errorf("get auth identity %q: %w", id, notFoundOr(err))
	}
	record, err := decode[authIdentityRecord](body)
	if err != nil {
		return domain.AuthIdentity{}, time.Time{}, fmt.Errorf("get auth identity %q: %w", id, err)
	}
	identity := record.Identity
	if identity.ID != id || identity.Provider != provider ||
		identity.AuthStoreMutationLease != lease ||
		identity.MaxParallelExecutions != parallel ||
		string(identity.RefreshStrategy) != refresh ||
		identity.SupportsReadOnlyAuthSnapshot != snapshots ||
		!timeColumnEqual(recordedAt, record.RecordedAt) {
		return domain.AuthIdentity{}, time.Time{}, fmt.Errorf("get auth identity %q: %w", id, errRowInconsistent)
	}
	return identity, record.RecordedAt, nil
}

// AcquireAuthStoreMutationLease takes the lease on an identity's auth store
// for holder until expiresAt, and returns the lease it holds.
//
// It is the single-winner gate §5.4 requires: a live lease held by anyone else
// refuses with ErrLeaseHeld, and an expired or released one is taken over with
// a bumped fence. The identity's own declaration is the live authority — an
// identity that does not require a lease cannot grant one — and it is read in
// this same transaction, so a retired declaration cannot be raced.
//
// Re-acquiring a lease the caller already holds converges without a write and
// returns the existing lease unchanged; extending it is RenewAuthStoreMutationLease's
// job, so a stale retry cannot silently lengthen a window.
func (tx *InternalTx) AcquireAuthStoreMutationLease(
	ctx context.Context, id domain.AuthIdentityID, holder domain.InvocationID,
	now, expiresAt time.Time,
) (domain.AuthStoreMutationLease, error) {
	if err := tx.requireLeaseDeclared(ctx, id); err != nil {
		return domain.AuthStoreMutationLease{}, fmt.Errorf("acquire auth store mutation lease %q: %w", id, err)
	}
	if !expiresAt.After(now) {
		return domain.AuthStoreMutationLease{}, fmt.Errorf(
			"acquire auth store mutation lease %q: window ends at %s, now is %s: %w",
			id, expiresAt.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), ErrLeaseWindowRegresses)
	}
	current, err := tx.GetAuthStoreMutationLease(ctx, id)
	switch {
	case errors.Is(err, ErrNotFound):
		return tx.insertLease(ctx, id, holder, now, expiresAt)
	case err != nil:
		return domain.AuthStoreMutationLease{}, fmt.Errorf("acquire auth store mutation lease %q: %w", id, err)
	}
	// A delayed acquisition must not reach back past the generation it is
	// taking over from: `now` predating the current row's own timeline is no
	// evidence about the present, and honouring it would install a stale
	// request's window (possibly still future-dated) over a lease that has
	// since been released, blocking the holders that come after it.
	if now.Before(current.AcquiredAt) || (current.ReleasedAt != nil && now.Before(*current.ReleasedAt)) {
		return domain.AuthStoreMutationLease{}, fmt.Errorf(
			"acquire auth store mutation lease %q: instant %s predates the current generation: %w",
			id, now.Format(time.RFC3339Nano), ErrLeaseWindowRegresses)
	}
	if current.HeldAt(now) {
		if current.Holder == holder {
			return current, nil
		}
		return domain.AuthStoreMutationLease{}, fmt.Errorf("acquire auth store mutation lease %q: %w", id,
			&LeaseHeldError{
				AuthIdentityID: id, Holder: current.Holder,
				Fence: current.Fence, ExpiresAt: current.ExpiresAt,
			})
	}
	return tx.takeoverLease(ctx, current, holder, now, expiresAt)
}

func (tx *InternalTx) insertLease(
	ctx context.Context, id domain.AuthIdentityID, holder domain.InvocationID,
	now, expiresAt time.Time,
) (domain.AuthStoreMutationLease, error) {
	lease := domain.AuthStoreMutationLease{
		AuthIdentityID: id, Holder: holder, Fence: 1,
		AcquiredAt: now.UTC(), ExpiresAt: expiresAt.UTC(),
	}
	body, err := encode(lease)
	if err != nil {
		return domain.AuthStoreMutationLease{}, fmt.Errorf("acquire auth store mutation lease %q: %w", id, err)
	}
	res, err := tx.tx.ExecContext(ctx, insertLeaseSQL,
		id, holder, lease.Fence, formatTime(lease.AcquiredAt),
		formatTime(lease.ExpiresAt), lease.ExpiresAt.UnixNano(), body)
	if err != nil {
		return domain.AuthStoreMutationLease{}, fmt.Errorf("acquire auth store mutation lease %q: %w", id, err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return domain.AuthStoreMutationLease{}, fmt.Errorf("acquire auth store mutation lease %q: %w", id, err)
	}
	if inserted != 1 {
		// The row was absent when read and present when written: a concurrent
		// acquirer won. Single-winner, fail closed.
		return domain.AuthStoreMutationLease{}, fmt.Errorf("acquire auth store mutation lease %q: %w", id, ErrLeaseHeld)
	}
	return lease, nil
}

func (tx *InternalTx) takeoverLease(
	ctx context.Context, current domain.AuthStoreMutationLease,
	holder domain.InvocationID, now, expiresAt time.Time,
) (domain.AuthStoreMutationLease, error) {
	id := current.AuthIdentityID
	lease := domain.AuthStoreMutationLease{
		AuthIdentityID: id, Holder: holder, Fence: current.Fence + 1,
		AcquiredAt: now.UTC(), ExpiresAt: expiresAt.UTC(),
	}
	body, err := encode(lease)
	if err != nil {
		return domain.AuthStoreMutationLease{}, fmt.Errorf("acquire auth store mutation lease %q: %w", id, err)
	}
	res, err := tx.tx.ExecContext(ctx, takeoverLeaseSQL,
		holder, lease.Fence, formatTime(lease.AcquiredAt), formatTime(lease.ExpiresAt),
		lease.ExpiresAt.UnixNano(), body, id, current.Fence)
	if err != nil {
		return domain.AuthStoreMutationLease{}, fmt.Errorf("acquire auth store mutation lease %q: %w", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return domain.AuthStoreMutationLease{}, fmt.Errorf("acquire auth store mutation lease %q: %w", id, err)
	}
	if affected != 1 {
		// The fence moved between the read and the write: another taker won.
		return domain.AuthStoreMutationLease{}, fmt.Errorf("acquire auth store mutation lease %q: %w", id, ErrLeaseHeld)
	}
	return lease, nil
}

// RenewAuthStoreMutationLease extends a lease the caller still holds. The
// guard names the caller's exact fence, so a holder whose lease was taken over
// cannot extend the new holder's window; an expired lease is not renewable
// either, since someone else may already be entitled to it. Both refuse with
// ErrLeaseNotHeld, and re-acquisition is the caller's path back.
func (tx *InternalTx) RenewAuthStoreMutationLease(
	ctx context.Context, id domain.AuthIdentityID, holder domain.InvocationID,
	fence int64, now, expiresAt time.Time,
) (domain.AuthStoreMutationLease, error) {
	current, err := tx.GetAuthStoreMutationLease(ctx, id)
	if err != nil {
		return domain.AuthStoreMutationLease{}, fmt.Errorf("renew auth store mutation lease %q: %w", id, err)
	}
	if current.Holder != holder || current.Fence != fence || !current.HeldAt(now) {
		return domain.AuthStoreMutationLease{}, fmt.Errorf("renew auth store mutation lease %q: %w", id, ErrLeaseNotHeld)
	}
	// A renewal only ever extends. A delayed or reordered call carrying an
	// earlier instant would otherwise report success while shortening the
	// window the caller believes it holds, letting another holder take the
	// lease sooner than the renewer expects. An exact replay of the current
	// expiry is idempotent and allowed; anything earlier, or already past, is
	// refused.
	if !expiresAt.After(now) || expiresAt.Before(current.ExpiresAt) {
		return domain.AuthStoreMutationLease{}, fmt.Errorf(
			"renew auth store mutation lease %q: window ends at %s, now is %s, current expiry is %s: %w",
			id, expiresAt.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano),
			current.ExpiresAt.Format(time.RFC3339Nano), ErrLeaseWindowRegresses)
	}
	renewed := current
	renewed.ExpiresAt = expiresAt.UTC()
	body, err := encode(renewed)
	if err != nil {
		return domain.AuthStoreMutationLease{}, fmt.Errorf("renew auth store mutation lease %q: %w", id, err)
	}
	res, err := tx.tx.ExecContext(ctx, renewLeaseSQL,
		formatTime(renewed.ExpiresAt), renewed.ExpiresAt.UnixNano(), body, id, holder, fence)
	if err != nil {
		return domain.AuthStoreMutationLease{}, fmt.Errorf("renew auth store mutation lease %q: %w", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return domain.AuthStoreMutationLease{}, fmt.Errorf("renew auth store mutation lease %q: %w", id, err)
	}
	if affected != 1 {
		return domain.AuthStoreMutationLease{}, fmt.Errorf("renew auth store mutation lease %q: %w", id, ErrLeaseNotHeld)
	}
	return renewed, nil
}

// ReleaseAuthStoreMutationLease ends a lease the caller holds, freeing the
// identity before the window expires. Releasing an already-released lease the
// caller last held converges; a non-holder, or a holder whose fence has been
// left behind, is refused.
func (tx *InternalTx) ReleaseAuthStoreMutationLease(
	ctx context.Context, id domain.AuthIdentityID, holder domain.InvocationID,
	fence int64, releasedAt time.Time,
) error {
	current, err := tx.GetAuthStoreMutationLease(ctx, id)
	if err != nil {
		return fmt.Errorf("release auth store mutation lease %q: %w", id, err)
	}
	if current.Holder != holder || current.Fence != fence {
		return fmt.Errorf("release auth store mutation lease %q: %w", id, ErrLeaseNotHeld)
	}
	if current.ReleasedAt != nil {
		return nil
	}
	// The release has to land inside the window it ends. A stamp past the
	// expiry is not a release (the lease was already over), and recording one
	// poisons the row: acquisition refuses an instant that predates the
	// current generation's release, so a far-future stamp would block every
	// legitimate takeover until it passed.
	if !current.HeldAt(releasedAt) {
		return fmt.Errorf(
			"release auth store mutation lease %q: instant %s is outside the window %s..%s: %w",
			id, releasedAt.Format(time.RFC3339Nano),
			current.AcquiredAt.Format(time.RFC3339Nano),
			current.ExpiresAt.Format(time.RFC3339Nano), ErrLeaseWindowRegresses)
	}
	released := current
	at := releasedAt.UTC()
	released.ReleasedAt = &at
	body, err := encode(released)
	if err != nil {
		return fmt.Errorf("release auth store mutation lease %q: %w", id, err)
	}
	res, err := tx.tx.ExecContext(ctx, releaseLeaseSQL, formatTime(at), body, id, holder, fence)
	if err != nil {
		return fmt.Errorf("release auth store mutation lease %q: %w", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("release auth store mutation lease %q: %w", id, err)
	}
	if affected != 1 {
		return fmt.Errorf("release auth store mutation lease %q: %w", id, ErrLeaseNotHeld)
	}
	return nil
}

// GetAuthStoreMutationLease reconstructs the lease row for an identity. It
// re-gates against the identity's current declaration (an identity that no
// longer requires a lease cannot have one reconstructed as live) and
// cross-checks every extracted column against the decoded body. It reports
// what the row says; whether the lease is live is HeldAt's answer, against the
// caller's clock.
func (tx *ReadTx) GetAuthStoreMutationLease(ctx context.Context, id domain.AuthIdentityID) (domain.AuthStoreMutationLease, error) {
	if err := tx.requireLeaseDeclared(ctx, id); err != nil {
		return domain.AuthStoreMutationLease{}, fmt.Errorf("get auth store mutation lease %q: %w", id, err)
	}
	var (
		holder     string
		fence      int64
		acquiredAt string
		expiresAt  string
		releasedAt sql.NullString
		body       []byte
	)
	err := tx.tx.QueryRowContext(ctx, getLeaseSQL, id).
		Scan(&holder, &fence, &acquiredAt, &expiresAt, &releasedAt, &body)
	if err != nil {
		return domain.AuthStoreMutationLease{}, fmt.Errorf("get auth store mutation lease %q: %w", id, notFoundOr(err))
	}
	lease, err := decode[domain.AuthStoreMutationLease](body)
	if err != nil {
		return domain.AuthStoreMutationLease{}, fmt.Errorf("get auth store mutation lease %q: %w", id, err)
	}
	if lease.AuthIdentityID != id || string(lease.Holder) != holder || lease.Fence != fence ||
		!timeColumnEqual(acquiredAt, lease.AcquiredAt) ||
		!timeColumnEqual(expiresAt, lease.ExpiresAt) ||
		!optionalTimeColumnEqual(releasedAt, lease.ReleasedAt) {
		return domain.AuthStoreMutationLease{}, fmt.Errorf("get auth store mutation lease %q: %w", id, errRowInconsistent)
	}
	return lease, nil
}

// requireLeaseDeclared fails closed unless the identity exists and declares
// that its auth store is lease-guarded. It is the live authority both the
// acquisition and the reconstruction of a lease are checked against, so a
// lease row alone never grants exclusion.
func (tx *ReadTx) requireLeaseDeclared(ctx context.Context, id domain.AuthIdentityID) error {
	identity, err := tx.GetAuthIdentity(ctx, id)
	if err != nil {
		return err
	}
	if !identity.AuthStoreMutationLease {
		return ErrLeaseNotDeclared
	}
	return nil
}
