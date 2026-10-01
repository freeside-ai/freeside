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
		e.AuthIdentityID, e.Holder, e.Fence, formatTime(e.ExpiresAt))
}

// Unwrap makes errors.Is(err, ErrLeaseHeld) match the refusal class.
func (e *LeaseHeldError) Unwrap() error { return ErrLeaseHeld }

// ErrReadHoldNotHeld is returned when a caller releases a read hold whose
// window it no longer holds: the row is gone, replaced, or already over.
var ErrReadHoldNotHeld = errors.New("caller does not hold the auth store read hold")

// ReadHeldError names a live read hold that refused a mutation lease. It
// unwraps to ErrLeaseHeld, so every existing caller that classifies a held
// lease treats a store under read as held too.
type ReadHeldError struct {
	AuthIdentityID domain.AuthIdentityID
	Holder         domain.InvocationID
	ExpiresAt      time.Time
}

func (e *ReadHeldError) Error() string {
	return fmt.Sprintf("auth store of %q is under a read hold by %q until %s",
		e.AuthIdentityID, e.Holder, formatTime(e.ExpiresAt))
}

// Unwrap makes errors.Is(err, ErrLeaseHeld) match the refusal class.
func (e *ReadHeldError) Unwrap() error { return ErrLeaseHeld }

const (
	recordAuthIdentitySQL = `
INSERT INTO auth_identities
    (id, provider, account_binding, usage_pool, budget, auth_store_mutation_lease,
     auth_store_volume, max_parallel_executions, refresh_strategy,
     supports_read_only_auth_snapshot, enabled, cost_owner, recorded_at, body)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    provider                         = excluded.provider,
    account_binding                  = excluded.account_binding,
    usage_pool                       = excluded.usage_pool,
    budget                           = excluded.budget,
    auth_store_mutation_lease        = excluded.auth_store_mutation_lease,
    auth_store_volume                = excluded.auth_store_volume,
    max_parallel_executions          = excluded.max_parallel_executions,
    refresh_strategy                 = excluded.refresh_strategy,
    supports_read_only_auth_snapshot = excluded.supports_read_only_auth_snapshot,
    enabled                          = excluded.enabled,
    cost_owner                       = excluded.cost_owner,
    recorded_at                      = excluded.recorded_at,
    body                             = excluded.body`
	getAuthIdentitySQL = `
SELECT provider, account_binding, usage_pool, budget, auth_store_mutation_lease,
       auth_store_volume, max_parallel_executions, refresh_strategy,
       supports_read_only_auth_snapshot, enabled, cost_owner, recorded_at, body
FROM auth_identities WHERE id = ?`
	listAuthIdentityIDsSQL = `
SELECT id FROM auth_identities ORDER BY id`
	accountBindingHolderSQL = `
SELECT id FROM auth_identities WHERE account_binding = ? AND id <> ?`

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

	getReadHoldSQL = `
SELECT acquired_at, expires_at, released_at, body
FROM auth_store_read_holds WHERE auth_identity_id = ? AND holder = ?`
	// The upsert replaces only an ended row, which the caller has already
	// checked; the guard names the exact row read so a concurrent change
	// fails closed.
	insertReadHoldSQL = `
INSERT INTO auth_store_read_holds
    (auth_identity_id, holder, acquired_at, expires_at, expires_at_unix_nano, released_at, body)
VALUES (?, ?, ?, ?, ?, NULL, ?)
ON CONFLICT (auth_identity_id, holder) DO NOTHING`
	replaceReadHoldSQL = `
UPDATE auth_store_read_holds
SET acquired_at = ?, expires_at = ?, expires_at_unix_nano = ?, released_at = NULL, body = ?
WHERE auth_identity_id = ? AND holder = ? AND acquired_at = ?`
	releaseReadHoldSQL = `
UPDATE auth_store_read_holds
SET released_at = ?, body = ?
WHERE auth_identity_id = ? AND holder = ? AND acquired_at = ? AND released_at IS NULL`
	// Candidates only: liveness is decided by HeldAt on each reconstructed
	// row against the caller's clock, never by this filter alone.
	unreleasedReadHoldersSQL = `
SELECT holder FROM auth_store_read_holds
WHERE auth_identity_id = ? AND released_at IS NULL AND expires_at_unix_nano > ?
ORDER BY holder`
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
	if err := tx.requireAccountBindingFree(ctx, identity); err != nil {
		return fmt.Errorf("record auth identity %q: %w", identity.ID, err)
	}
	if _, err := tx.tx.ExecContext(ctx, recordAuthIdentitySQL,
		identity.ID, identity.Provider, identity.AccountBinding, identity.UsagePool,
		identity.Budget, identity.AuthStoreMutationLease,
		identity.Interim.AuthStoreVolume, identity.MaxParallelExecutions,
		interimRefreshColumn(identity), identity.Interim.SupportsReadOnlyAuthSnapshot,
		identity.Enabled, identity.CostOwner, formatTime(recordedAt), body); err != nil {
		return fmt.Errorf("record auth identity %q: %w", identity.ID, err)
	}
	return nil
}

// interimRefreshColumn is the refresh_strategy column value for an identity:
// the interim fact where one is recorded, and the marker "none" for a
// post-adoption identity with no interim facts. The 0013 CHECK requires the
// column non-empty, and "none" is deliberately outside the RefreshStrategy
// enum, so it can never be confused with a recorded strategy; the
// reconstruction cross-check compares through this same function.
func interimRefreshColumn(identity domain.AuthIdentity) string {
	if identity.Interim.Present() {
		return string(identity.Interim.RefreshStrategy)
	}
	return "none"
}

// requireAccountBindingFree enforces the kept revision 36 rule (§5.4): an
// account binding belongs to at most one identity, so one subscription never
// holds two leases or two budgets. The typed refusal fires before the write;
// the partial unique index in 0052 is the mechanical backstop underneath it.
func (tx *InternalTx) requireAccountBindingFree(ctx context.Context, identity domain.AuthIdentity) error {
	if identity.AccountBinding == "" {
		return nil
	}
	var holder domain.AuthIdentityID
	err := tx.tx.QueryRowContext(ctx, accountBindingHolderSQL, identity.AccountBinding, identity.ID).Scan(&holder)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return err
	}
	return fmt.Errorf("account binding %q is held by identity %q: %w",
		identity.AccountBinding, holder, domain.ErrAccountBindingTaken)
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
	column := formatTime(storedAt)
	if recordedAt.Before(storedAt) {
		return fmt.Errorf("revision stamped %s, stored revision is %s: %w",
			formatTime(recordedAt), column, ErrStaleWrite)
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

// ListAuthIdentities reconstructs every identity declaration in id order.
// Each row goes through GetAuthIdentity, so a listed identity carries exactly
// the cross-checks a single read does, and one inconsistent row fails the
// whole listing rather than being skipped.
func (tx *ReadTx) ListAuthIdentities(ctx context.Context) ([]domain.AuthIdentity, error) {
	ids, err := queryIDs[domain.AuthIdentityID](ctx, tx, listAuthIdentityIDsSQL)
	if err != nil {
		return nil, fmt.Errorf("list auth identities: %w", err)
	}
	identities := make([]domain.AuthIdentity, 0, len(ids))
	for _, id := range ids {
		identity, err := tx.GetAuthIdentity(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("list auth identities: %w", err)
		}
		identities = append(identities, identity)
	}
	return identities, nil
}

// queryIDs collects one string-keyed id column in full before the caller
// reconstructs each row: the transaction holds one connection, so a nested
// read cannot run while these rows are still open.
func queryIDs[T ~string](ctx context.Context, tx *ReadTx, query string, args ...any) ([]T, error) {
	rows, err := tx.tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []T
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, T(id))
	}
	return ids, rows.Err()
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
		provider       string
		accountBinding string
		usagePool      string
		budget         int64
		lease          bool
		volume         sql.NullString
		parallel       int
		refresh        string
		snapshots      bool
		enabled        bool
		costOwner      string
		recordedAt     string
		body           []byte
	)
	err := tx.tx.QueryRowContext(ctx, getAuthIdentitySQL, id).
		Scan(&provider, &accountBinding, &usagePool, &budget, &lease, &volume,
			&parallel, &refresh, &snapshots, &enabled, &costOwner, &recordedAt, &body)
	if err != nil {
		return domain.AuthIdentity{}, time.Time{}, fmt.Errorf("get auth identity %q: %w", id, notFoundOr(err))
	}
	record, err := decode[authIdentityRecord](body)
	if err != nil {
		return domain.AuthIdentity{}, time.Time{}, fmt.Errorf("get auth identity %q: %w", id, err)
	}
	identity := record.Identity
	if identity.ID != id || identity.Provider != provider ||
		identity.AccountBinding != accountBinding ||
		identity.UsagePool != usagePool ||
		identity.Budget != budget ||
		identity.AuthStoreMutationLease != lease ||
		identity.Interim.AuthStoreVolume != volume.String ||
		identity.MaxParallelExecutions != parallel ||
		interimRefreshColumn(identity) != refresh ||
		identity.Interim.SupportsReadOnlyAuthSnapshot != snapshots ||
		identity.Enabled != enabled ||
		identity.CostOwner != costOwner ||
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
	return tx.AcquireAuthStoreMutationLeaseBound(ctx, id, holder, nil, now, expiresAt)
}

// AcquireAuthStoreMutationLeaseBound is AcquireAuthStoreMutationLease with a
// generation binding: the fence it grants names the exact enrollment store the
// holder may mutate (§5.4). A nil binding takes an unbound lease — the
// pre-enrollment interim path, whose identity's one store the Interim facts
// locate. Re-acquisition by the current holder converges on the existing
// lease unchanged, whatever binding it was taken with; a holder that needs a
// different binding releases and re-acquires, so a fence can never silently
// change which store it guards.
func (tx *InternalTx) AcquireAuthStoreMutationLeaseBound(
	ctx context.Context, id domain.AuthIdentityID, holder domain.InvocationID,
	binding *domain.LeaseGenerationBinding, now, expiresAt time.Time,
) (domain.AuthStoreMutationLease, error) {
	if err := tx.requireLeaseDeclared(ctx, id); err != nil {
		return domain.AuthStoreMutationLease{}, fmt.Errorf("acquire auth store mutation lease %q: %w", id, err)
	}
	if binding != nil {
		detached := *binding
		binding = &detached
	}
	if !expiresAt.After(now) {
		return domain.AuthStoreMutationLease{}, fmt.Errorf(
			"acquire auth store mutation lease %q: window ends at %s, now is %s: %w",
			id, formatTime(expiresAt), formatTime(now), ErrLeaseWindowRegresses)
	}
	// A mutation must not change the bytes a running execution reads, so
	// any live read hold refuses the lease, whoever asks.
	if err := tx.refuseLiveReadHold(ctx, id, now); err != nil {
		return domain.AuthStoreMutationLease{}, fmt.Errorf("acquire auth store mutation lease %q: %w", id, err)
	}
	current, err := tx.GetAuthStoreMutationLease(ctx, id)
	switch {
	case errors.Is(err, ErrNotFound):
		return tx.insertLease(ctx, id, holder, binding, now, expiresAt)
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
			id, formatTime(now), ErrLeaseWindowRegresses)
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
	return tx.takeoverLease(ctx, current, holder, binding, now, expiresAt)
}

func (tx *InternalTx) insertLease(
	ctx context.Context, id domain.AuthIdentityID, holder domain.InvocationID,
	binding *domain.LeaseGenerationBinding, now, expiresAt time.Time,
) (domain.AuthStoreMutationLease, error) {
	lease := domain.AuthStoreMutationLease{
		AuthIdentityID: id, Holder: holder, Fence: 1,
		AcquiredAt: now.UTC(), ExpiresAt: expiresAt.UTC(),
		GenerationBinding: binding,
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
	holder domain.InvocationID, binding *domain.LeaseGenerationBinding,
	now, expiresAt time.Time,
) (domain.AuthStoreMutationLease, error) {
	id := current.AuthIdentityID
	lease := domain.AuthStoreMutationLease{
		AuthIdentityID: id, Holder: holder, Fence: current.Fence + 1,
		AcquiredAt: now.UTC(), ExpiresAt: expiresAt.UTC(),
		GenerationBinding: binding,
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
//
// A live read hold refuses renewal as it refuses acquisition. A renewal
// whose instant was sampled before the lease expired could otherwise
// extend it after a reader opened a window on the expired lease, leaving
// both live (#1585).
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
	if err := tx.refuseLiveReadHold(ctx, id, now); err != nil {
		return domain.AuthStoreMutationLease{}, fmt.Errorf("renew auth store mutation lease %q: %w", id, err)
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
			id, formatTime(expiresAt), formatTime(now),
			formatTime(current.ExpiresAt), ErrLeaseWindowRegresses)
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
			id, formatTime(releasedAt),
			formatTime(current.AcquiredAt),
			formatTime(current.ExpiresAt), ErrLeaseWindowRegresses)
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

// AcquireAuthStoreReadHold opens a shared read window on an identity's auth
// store for holder, from now until expiresAt. Any number of holders may read
// at once; a live mutation lease refuses with a LeaseHeldError, since a
// reader must not start while the store may be changing.
//
// A holder opens one window per run. Unlike the lease it never converges on
// a live window it already holds: a new run attaching an old window could
// have that window released by the old run's recovery while it still reads.
// A live same-holder row refuses with ErrLeaseHeld; an ended one is replaced.
func (tx *InternalTx) AcquireAuthStoreReadHold(
	ctx context.Context, id domain.AuthIdentityID, holder domain.InvocationID,
	now, expiresAt time.Time,
) (domain.AuthStoreReadHold, error) {
	if err := tx.requireLeaseDeclared(ctx, id); err != nil {
		return domain.AuthStoreReadHold{}, fmt.Errorf("acquire auth store read hold %q: %w", id, err)
	}
	if !expiresAt.After(now) {
		return domain.AuthStoreReadHold{}, fmt.Errorf(
			"acquire auth store read hold %q: window ends at %s, now is %s: %w",
			id, formatTime(expiresAt), formatTime(now), ErrLeaseWindowRegresses)
	}
	lease, err := tx.GetAuthStoreMutationLease(ctx, id)
	switch {
	case errors.Is(err, ErrNotFound):
	case err != nil:
		return domain.AuthStoreReadHold{}, fmt.Errorf("acquire auth store read hold %q: %w", id, err)
	default:
		// The same stale-instant rule as the lease: an instant before the
		// lease's own timeline says nothing about whether it is live now.
		if now.Before(lease.AcquiredAt) || (lease.ReleasedAt != nil && now.Before(*lease.ReleasedAt)) {
			return domain.AuthStoreReadHold{}, fmt.Errorf(
				"acquire auth store read hold %q: instant %s predates the current lease generation: %w",
				id, formatTime(now), ErrLeaseWindowRegresses)
		}
		if lease.HeldAt(now) {
			return domain.AuthStoreReadHold{}, fmt.Errorf("acquire auth store read hold %q: %w", id,
				&LeaseHeldError{
					AuthIdentityID: id, Holder: lease.Holder,
					Fence: lease.Fence, ExpiresAt: lease.ExpiresAt,
				})
		}
	}
	hold := domain.AuthStoreReadHold{
		AuthIdentityID: id, Holder: holder,
		AcquiredAt: now.UTC(), ExpiresAt: expiresAt.UTC(),
	}
	body, err := encode(hold)
	if err != nil {
		return domain.AuthStoreReadHold{}, fmt.Errorf("acquire auth store read hold %q: %w", id, err)
	}
	current, err := tx.GetAuthStoreReadHold(ctx, id, holder)
	var res sql.Result
	switch {
	case errors.Is(err, ErrNotFound):
		res, err = tx.tx.ExecContext(ctx, insertReadHoldSQL,
			id, holder, formatTime(hold.AcquiredAt), formatTime(hold.ExpiresAt),
			hold.ExpiresAt.UnixNano(), body)
	case err != nil:
		return domain.AuthStoreReadHold{}, fmt.Errorf("acquire auth store read hold %q: %w", id, err)
	default:
		if now.Before(current.AcquiredAt) || (current.ReleasedAt != nil && now.Before(*current.ReleasedAt)) {
			return domain.AuthStoreReadHold{}, fmt.Errorf(
				"acquire auth store read hold %q: instant %s predates the holder's current window: %w",
				id, formatTime(now), ErrLeaseWindowRegresses)
		}
		if current.HeldAt(now) {
			return domain.AuthStoreReadHold{}, fmt.Errorf(
				"acquire auth store read hold %q: holder %q already holds a live window: %w",
				id, holder, ErrLeaseHeld)
		}
		res, err = tx.tx.ExecContext(ctx, replaceReadHoldSQL,
			formatTime(hold.AcquiredAt), formatTime(hold.ExpiresAt),
			hold.ExpiresAt.UnixNano(), body, id, holder, formatTime(current.AcquiredAt))
	}
	if err != nil {
		return domain.AuthStoreReadHold{}, fmt.Errorf("acquire auth store read hold %q: %w", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return domain.AuthStoreReadHold{}, fmt.Errorf("acquire auth store read hold %q: %w", id, err)
	}
	if affected != 1 {
		// The row changed between the read and the write. Fail closed.
		return domain.AuthStoreReadHold{}, fmt.Errorf("acquire auth store read hold %q: %w", id, ErrLeaseHeld)
	}
	return hold, nil
}

// ReleaseAuthStoreReadHold ends the read window holder opened at acquiredAt.
// The acquisition instant names the exact window, so a stale release cannot
// end a later window the same holder opened. Releasing an already-released
// window converges; a missing, replaced, or expired one is refused with
// ErrReadHoldNotHeld, and a release instant outside the window with
// ErrLeaseWindowRegresses.
func (tx *InternalTx) ReleaseAuthStoreReadHold(
	ctx context.Context, id domain.AuthIdentityID, holder domain.InvocationID,
	acquiredAt, releasedAt time.Time,
) error {
	current, err := tx.GetAuthStoreReadHold(ctx, id, holder)
	switch {
	case errors.Is(err, ErrNotFound):
		return fmt.Errorf("release auth store read hold %q: %w", id, ErrReadHoldNotHeld)
	case err != nil:
		return fmt.Errorf("release auth store read hold %q: %w", id, err)
	}
	if !current.AcquiredAt.Equal(acquiredAt) {
		return fmt.Errorf("release auth store read hold %q: %w", id, ErrReadHoldNotHeld)
	}
	if current.ReleasedAt != nil {
		return nil
	}
	// As with the lease, a release stamp outside the window is not a
	// release, and a far-future one would hold off mutation until it passed.
	if !current.HeldAt(releasedAt) {
		return fmt.Errorf(
			"release auth store read hold %q: instant %s is outside the window %s..%s: %w",
			id, formatTime(releasedAt), formatTime(current.AcquiredAt),
			formatTime(current.ExpiresAt), ErrLeaseWindowRegresses)
	}
	released := current
	at := releasedAt.UTC()
	released.ReleasedAt = &at
	body, err := encode(released)
	if err != nil {
		return fmt.Errorf("release auth store read hold %q: %w", id, err)
	}
	res, err := tx.tx.ExecContext(ctx, releaseReadHoldSQL,
		formatTime(at), body, id, holder, formatTime(current.AcquiredAt))
	if err != nil {
		return fmt.Errorf("release auth store read hold %q: %w", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("release auth store read hold %q: %w", id, err)
	}
	if affected != 1 {
		return fmt.Errorf("release auth store read hold %q: %w", id, ErrReadHoldNotHeld)
	}
	return nil
}

// GetAuthStoreReadHold reconstructs one holder's read hold row, re-gated
// against the identity's current lease declaration and cross-checked against
// its columns like the lease. Liveness is HeldAt's answer.
func (tx *ReadTx) GetAuthStoreReadHold(
	ctx context.Context, id domain.AuthIdentityID, holder domain.InvocationID,
) (domain.AuthStoreReadHold, error) {
	if err := tx.requireLeaseDeclared(ctx, id); err != nil {
		return domain.AuthStoreReadHold{}, fmt.Errorf("get auth store read hold %q: %w", id, err)
	}
	var (
		acquiredAt string
		expiresAt  string
		releasedAt sql.NullString
		body       []byte
	)
	err := tx.tx.QueryRowContext(ctx, getReadHoldSQL, id, holder).
		Scan(&acquiredAt, &expiresAt, &releasedAt, &body)
	if err != nil {
		return domain.AuthStoreReadHold{}, fmt.Errorf("get auth store read hold %q: %w", id, notFoundOr(err))
	}
	hold, err := decode[domain.AuthStoreReadHold](body)
	if err != nil {
		return domain.AuthStoreReadHold{}, fmt.Errorf("get auth store read hold %q: %w", id, err)
	}
	if hold.AuthIdentityID != id || hold.Holder != holder ||
		!timeColumnEqual(acquiredAt, hold.AcquiredAt) ||
		!timeColumnEqual(expiresAt, hold.ExpiresAt) ||
		!optionalTimeColumnEqual(releasedAt, hold.ReleasedAt) {
		return domain.AuthStoreReadHold{}, fmt.Errorf("get auth store read hold %q: %w", id, errRowInconsistent)
	}
	return hold, nil
}

// refuseLiveReadHold returns a ReadHeldError naming the first read hold on
// the identity that is live at now. Candidates are selected by the released
// and expiry columns the store writes beside each body; every candidate is
// then reconstructed and cross-checked, so a malformed candidate fails the
// mutation closed rather than being skipped.
func (tx *ReadTx) refuseLiveReadHold(ctx context.Context, id domain.AuthIdentityID, now time.Time) error {
	rows, err := tx.tx.QueryContext(ctx, unreleasedReadHoldersSQL, id, now.UnixNano())
	if err != nil {
		return err
	}
	var holders []domain.InvocationID
	for rows.Next() {
		var holder string
		if err := rows.Scan(&holder); err != nil {
			_ = rows.Close()
			return err
		}
		holders = append(holders, domain.InvocationID(holder))
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, holder := range holders {
		hold, err := tx.GetAuthStoreReadHold(ctx, id, holder)
		if err != nil {
			return err
		}
		// An instant before the hold opened is a stale or regressed clock,
		// not evidence the hold is over.
		if now.Before(hold.AcquiredAt) {
			return fmt.Errorf("instant %s predates read hold %q: %w",
				formatTime(now), hold.Holder, ErrLeaseWindowRegresses)
		}
		if hold.HeldAt(now) {
			return &ReadHeldError{AuthIdentityID: id, Holder: hold.Holder, ExpiresAt: hold.ExpiresAt}
		}
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
