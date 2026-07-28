package domain

import (
	"fmt"
	"time"
)

// AuthIdentity is one provider identity the daemon executes under, and the two
// independent concurrency controls plan §5.4 attaches to it: auth-store
// mutation (refresh, login state, configuration writes, store replacement) is
// serialized per identity, and inference execution has a separate parallelism
// limit. Scheduling shows the constraint rather than hiding it in a lock,
// which is why the limit is a recorded declaration and not an implementation
// detail of whatever holds the lock.
//
// The declaration says a mutation lease is required; the store's lease table
// is what enforces it. Keeping the two apart means a writer cannot satisfy the
// invariant by agreeing with the declaration.
type AuthIdentity struct {
	ID       AuthIdentityID `json:"id"`
	Provider string         `json:"provider"`
	// AuthStoreMutationLease declares that this identity's auth store may only
	// be mutated under a lease. An identity that declares no lease cannot take
	// one: the store refuses, rather than treating the declaration as advice.
	AuthStoreMutationLease bool `json:"auth_store_mutation_lease"`
	// AuthStoreVolume is the trusted runtime volume that carries this
	// identity's auth store. Every lease-declaring identity binds one exact
	// volume, so a caller cannot lease identity A while mounting identity B's
	// writable store.
	AuthStoreVolume string `json:"auth_store_volume"`
	// MaxParallelExecutions is the inference-execution limit, independent of
	// the mutation lease (§5.4). 1B establishes it experimentally; it is at
	// least one, since an identity that can run nothing is not an identity.
	MaxParallelExecutions        int             `json:"max_parallel_executions"`
	RefreshStrategy              RefreshStrategy `json:"refresh_strategy"`
	SupportsReadOnlyAuthSnapshot bool            `json:"supports_read_only_auth_snapshot"`
}

// Validate reports whether the identity declaration is well-formed.
func (i AuthIdentity) Validate() error {
	if i.ID == "" {
		return fmt.Errorf("auth identity id: %w", ErrEmptyID)
	}
	if i.Provider == "" {
		return fmt.Errorf("auth identity %s provider: %w", i.ID, ErrEmptyField)
	}
	if i.AuthStoreMutationLease && i.AuthStoreVolume == "" {
		return fmt.Errorf("auth identity %s auth_store_volume: %w", i.ID, ErrEmptyField)
	}
	if i.MaxParallelExecutions < 1 {
		return fmt.Errorf("auth identity %s max_parallel_executions %d: %w",
			i.ID, i.MaxParallelExecutions, ErrNonPositive)
	}
	if !i.RefreshStrategy.valid() {
		return fmt.Errorf("auth identity %s refresh_strategy %q: %w",
			i.ID, i.RefreshStrategy, ErrInvalidRefreshStrategy)
	}
	return nil
}

// AuthStoreMutationLease is the durable record of who may mutate one
// identity's auth store right now (plan §5.4). It is the serialization point
// itself, not a note about one: a holder proves its claim with the fence,
// which increases on every takeover, so a holder that stalled past its expiry
// and woke up again presents a fence the current row has left behind.
//
// Liveness is never read from the record. HeldAt takes the caller's instant,
// because a decoded row saying "held until T" is a claim about a clock this
// value does not have.
type AuthStoreMutationLease struct {
	AuthIdentityID AuthIdentityID `json:"auth_identity_id"`
	// Holder identifies whoever took the lease, so an abandoned one can be
	// traced back to what abandoned it. It is deliberately not required to
	// name a recorded agent invocation: §5.4 serializes refresh, login state,
	// configuration writes, and store replacement, and those last two are
	// daemon or operator actions with no agent turn behind them. Binding the
	// holder to the §5.14 invocation record would lock the lease to the one
	// case that has one.
	Holder InvocationID `json:"holder"`
	// Fence increases by one on every takeover, and never on a renewal by the
	// same holder: renewing is not a change of ownership.
	Fence      int64      `json:"fence"`
	AcquiredAt time.Time  `json:"acquired_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	ReleasedAt *time.Time `json:"released_at"`
}

// Validate reports whether the lease record is well-formed.
func (l AuthStoreMutationLease) Validate() error {
	if l.AuthIdentityID == "" {
		return fmt.Errorf("auth store mutation lease identity: %w", ErrEmptyID)
	}
	if l.Holder == "" {
		return fmt.Errorf("auth store mutation lease %s holder: %w", l.AuthIdentityID, ErrEmptyID)
	}
	if l.Fence < 1 {
		return fmt.Errorf("auth store mutation lease %s fence %d: %w", l.AuthIdentityID, l.Fence, ErrNonPositive)
	}
	if l.AcquiredAt.IsZero() {
		return fmt.Errorf("auth store mutation lease %s acquired_at: %w", l.AuthIdentityID, ErrMissingTimestamp)
	}
	if l.ExpiresAt.IsZero() {
		return fmt.Errorf("auth store mutation lease %s expires_at: %w", l.AuthIdentityID, ErrMissingTimestamp)
	}
	// A lease is a bounded window: one that expires when or before it was
	// taken never granted anything, and one whose release precedes its
	// acquisition is an incoherent record, not a short lease.
	if !l.ExpiresAt.After(l.AcquiredAt) {
		return fmt.Errorf("auth store mutation lease %s expires_at %s, acquired_at %s: %w",
			l.AuthIdentityID, l.ExpiresAt, l.AcquiredAt, ErrTimestampOutOfOrder)
	}
	if l.ReleasedAt != nil {
		// A release falls inside the window it ends. Outside it the record is
		// incoherent, and a future one is actively harmful: acquisition
		// refuses an instant preceding the current generation's release, so an
		// imported or malformed row carrying one would block every takeover
		// until it passed — the denial the release path already refuses to
		// create, reachable here through reconstruction instead.
		if l.ReleasedAt.Before(l.AcquiredAt) || l.ReleasedAt.After(l.ExpiresAt) {
			return fmt.Errorf("auth store mutation lease %s released_at %s, window %s..%s: %w",
				l.AuthIdentityID, l.ReleasedAt, l.AcquiredAt, l.ExpiresAt, ErrTimestampOutOfOrder)
		}
	}
	return nil
}

// HeldAt reports whether the lease is live at the caller's instant: not
// released, and inside [AcquiredAt, ExpiresAt). Both bounds matter. Expiry is
// exclusive, so a lease "until T" is not held at T; and an instant before the
// generation was taken is outside it too, so a caller whose clock regressed
// cannot renew a generation that did not yet exist when it thinks it is.
func (l AuthStoreMutationLease) HeldAt(now time.Time) bool {
	if l.ReleasedAt != nil {
		return false
	}
	return !now.Before(l.AcquiredAt) && now.Before(l.ExpiresAt)
}

// ValidateAuthIdentityTransition reports whether updated is a legal successor
// to the stored identity old. The identity's key, provider, auth-store volume,
// and lease declaration are fixed: changing either store binding would let a
// holder keep a lease over a different volume than the writer now mutates.
// The parallelism limit and snapshot support may change, since 1B measures the
// limit and a provider can gain read-only snapshot support.
func ValidateAuthIdentityTransition(old, updated AuthIdentity) error {
	if updated.ID != old.ID {
		return fmt.Errorf("auth identity %s: identity would change from %s: %w",
			updated.ID, old.ID, ErrImmutableTransition)
	}
	if updated.Provider != old.Provider ||
		updated.AuthStoreMutationLease != old.AuthStoreMutationLease ||
		updated.AuthStoreVolume != old.AuthStoreVolume {
		return fmt.Errorf("auth identity %s: fixed bindings would change: %w",
			updated.ID, ErrImmutableTransition)
	}
	return nil
}
