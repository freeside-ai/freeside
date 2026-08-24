package domain

import (
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// AuthIdentity is one provider identity the daemon executes under: the
// account, its budget and usage attribution, and the two independent
// concurrency controls plan §5.4 attaches to it. Auth-store mutation
// (refresh, login state, configuration writes, store replacement) is
// serialized per identity — the one lease fences every enrollment's store —
// and inference execution has a separate parallelism limit. Scheduling shows
// the constraint rather than hiding it in a lock, which is why the limit is a
// recorded declaration and not an implementation detail of whatever holds the
// lock.
//
// The declaration says a mutation lease is required; the store's lease table
// is what enforces it. Keeping the two apart means a writer cannot satisfy the
// invariant by agreeing with the declaration.
//
// The client facts the identity carried before the admitted-agent revision
// (store locator, refresh strategy, snapshot support) belong to the
// ClientEnrollment and its generations: one identity has many enrollments,
// and a singular fact here cannot say which client's store it describes.
// Interim carries them for a pre-enrollment identity only.
type AuthIdentity struct {
	ID       AuthIdentityID `json:"id"`
	Provider string         `json:"provider"`
	// AccountBinding is the stable provider-account fingerprint this identity
	// is bound to, unique across identities (§5.4: one subscription never
	// holds two leases or two budgets). Empty until enrollment or adoption
	// binds the account; set once, never rebound.
	AccountBinding string `json:"account_binding"`
	// UsagePool names the provider quota pool this identity draws on. Two
	// clients on one subscription share one pool, which is why alternate-agent
	// eligibility for a quota failure needs a different pool, not merely a
	// different enrollment. Empty until the account is characterized; set
	// once.
	UsagePool string `json:"usage_pool"`
	// Budget is the account budget in provider cost units, 0 when none is
	// declared. Cost attribution reads CostOwner; the budget bounds spend.
	Budget int64 `json:"budget"`
	// AuthStoreMutationLease declares that this identity's auth stores may
	// only be mutated under a lease. An identity that declares no lease cannot
	// take one: the store refuses, rather than treating the declaration as
	// advice.
	AuthStoreMutationLease bool `json:"auth_store_mutation_lease"`
	// MaxParallelExecutions is the inference-execution limit, independent of
	// the mutation lease (§5.4). 1B establishes it experimentally; it is at
	// least one, since an identity that can run nothing is not an identity.
	MaxParallelExecutions int `json:"max_parallel_executions"`
	// Enabled and CostOwner are the operator fields the dissolved
	// ProviderProfile left behind (§5.4): agent resolution refuses a disabled
	// identity, and every selection reads and records the cost owner.
	Enabled   bool   `json:"enabled"`
	CostOwner string `json:"cost_owner"`
	// Interim carries the client facts for an identity enrolled before the
	// agent cutover (#867): exactly one implicit client, so the singular
	// locator is still truthful. Adoption moves the facts onto a
	// ClientEnrollment and its generations; a post-adoption identity carries
	// the zero value. Kept as a value (not a pointer) so the identity stays
	// comparable — live convergence checks compare declarations with ==.
	Interim InterimClientFacts `json:"interim"`
}

// InterimClientFacts is the pre-enrollment carrier for the client facts §5.4
// moved to ClientEnrollment and its generations. It exists so the interim
// flag-selection path keeps a truthful record while enrollments are dormant;
// the #867 adoption empties it.
type InterimClientFacts struct {
	// AuthStoreVolume is the trusted locator that carries the identity's one
	// interim auth store. Writable container identities bind a runtime
	// volume; read-only host-snapshot identities bind the canonical host
	// store path.
	AuthStoreVolume              string          `json:"auth_store_volume"`
	RefreshStrategy              RefreshStrategy `json:"refresh_strategy"`
	SupportsReadOnlyAuthSnapshot bool            `json:"supports_read_only_auth_snapshot"`
}

// Present reports whether any interim client fact is recorded.
func (f InterimClientFacts) Present() bool { return f != InterimClientFacts{} }

// SameFixedBindings reports whether two declarations agree on the identity's
// fixed bindings: provider, lease declaration, and interim client facts.
// Everything else — the parallelism limit, the operator fields, and the
// set-once account binding and usage pool — may lawfully differ between a
// caller's expectation and the stored declaration, so a boundary that needs
// "this is the same identity I enrolled" compares through this, never whole
// records.
func (i AuthIdentity) SameFixedBindings(other AuthIdentity) bool {
	return i.Provider == other.Provider &&
		i.AuthStoreMutationLease == other.AuthStoreMutationLease &&
		i.Interim == other.Interim
}

// Validate reports whether the identity declaration is well-formed.
func (i AuthIdentity) Validate() error {
	if i.ID == "" {
		return fmt.Errorf("auth identity id: %w", ErrEmptyID)
	}
	if i.Provider == "" {
		return fmt.Errorf("auth identity %s provider: %w", i.ID, ErrEmptyField)
	}
	if i.Budget < 0 {
		return fmt.Errorf("auth identity %s budget %d: %w", i.ID, i.Budget, ErrNonPositive)
	}
	if i.MaxParallelExecutions < 1 {
		return fmt.Errorf("auth identity %s max_parallel_executions %d: %w",
			i.ID, i.MaxParallelExecutions, ErrNonPositive)
	}
	if i.Interim.Present() {
		if !i.Interim.RefreshStrategy.valid() {
			return fmt.Errorf("auth identity %s interim refresh_strategy %q: %w",
				i.ID, i.Interim.RefreshStrategy, ErrInvalidRefreshStrategy)
		}
		if i.AuthStoreMutationLease && i.Interim.AuthStoreVolume == "" {
			return fmt.Errorf("auth identity %s interim auth_store_volume: %w", i.ID, ErrEmptyField)
		}
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
	// GenerationBinding names the exact enrollment store this fence guards
	// (§5.4: every fence names enrollment id, generation, exact locator, and
	// store manifest digest). Nil on a lease taken for a pre-enrollment
	// interim identity, whose one store the identity's Interim facts locate;
	// required once enrollments carry the stores, so a holder can never lease
	// identity A's fence while mutating a store the fence does not name.
	GenerationBinding *LeaseGenerationBinding `json:"generation_binding"`
}

// LeaseGenerationBinding is the store binding one lease fence names: which
// enrollment's store the holder may mutate, the generation it mutates from,
// the exact locator, and the manifest digest of the bytes it found there.
type LeaseGenerationBinding struct {
	EnrollmentID ClientEnrollmentID `json:"enrollment_id"`
	// Generation is the ordinal of the enrollment generation the mutation
	// starts from; the mutation's own successful append becomes the next one.
	// Zero names the bootstrap mutation: the store holds no generation yet,
	// and the append creates its first. The store refuses an append whose
	// binding does not name its current state, so a fence taken against a
	// superseded generation cannot install a rollback as the newest entry.
	Generation          int    `json:"generation"`
	AuthStoreVolume     string `json:"auth_store_volume"`
	StoreManifestDigest Digest `json:"store_manifest_digest"`
}

// Validate reports whether the binding is well-formed.
func (b LeaseGenerationBinding) Validate() error {
	if b.EnrollmentID == "" {
		return fmt.Errorf("lease generation binding enrollment_id: %w", ErrEmptyID)
	}
	// Zero is the bootstrap binding (no generation to start from yet); only
	// a negative value is malformed.
	if b.Generation < 0 {
		return fmt.Errorf("lease generation binding %s generation %d: %w",
			b.EnrollmentID, b.Generation, ErrNonPositive)
	}
	if b.AuthStoreVolume == "" {
		return fmt.Errorf("lease generation binding %s auth_store_volume: %w", b.EnrollmentID, ErrEmptyField)
	}
	if !contentaddr.Valid(string(b.StoreManifestDigest)) {
		return fmt.Errorf("lease generation binding %s store_manifest_digest %q: %w",
			b.EnrollmentID, b.StoreManifestDigest, ErrInvalidDigest)
	}
	return nil
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
	if l.GenerationBinding != nil {
		if err := l.GenerationBinding.Validate(); err != nil {
			return fmt.Errorf("auth store mutation lease %s: %w", l.AuthIdentityID, err)
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
// to the stored identity old. The identity's key, provider, lease
// declaration, and interim client facts are fixed: changing any mutation rule
// while a holder relies on the recorded identity would invalidate that
// lease's authority. The account binding and usage pool are set once — empty
// to a value at enrollment or adoption, never rebound, since rebinding would
// re-home recorded usage onto a different account. The independently measured
// parallelism limit and the operator fields (enabled, cost owner, budget) may
// change freely.
func ValidateAuthIdentityTransition(old, updated AuthIdentity) error {
	if updated.ID != old.ID {
		return fmt.Errorf("auth identity %s: identity would change from %s: %w",
			updated.ID, old.ID, ErrImmutableTransition)
	}
	if !updated.SameFixedBindings(old) {
		return fmt.Errorf("auth identity %s: fixed bindings would change: %w",
			updated.ID, ErrImmutableTransition)
	}
	if old.AccountBinding != "" && updated.AccountBinding != old.AccountBinding {
		return fmt.Errorf("auth identity %s: account binding would change from %q: %w",
			updated.ID, old.AccountBinding, ErrImmutableTransition)
	}
	if old.UsagePool != "" && updated.UsagePool != old.UsagePool {
		return fmt.Errorf("auth identity %s: usage pool would change from %q: %w",
			updated.ID, old.UsagePool, ErrImmutableTransition)
	}
	return nil
}
