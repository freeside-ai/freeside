package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

var leaseEpoch = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func testAuthIdentity() domain.AuthIdentity {
	return domain.AuthIdentity{
		ID: "auth-1", Provider: "claude", AuthStoreMutationLease: true,
		MaxParallelExecutions: 1,
		Interim:               domain.InterimClientFacts{AuthStoreVolume: "provider-cred", RefreshStrategy: domain.RefreshOnDemand},
	}
}

// openWithIdentity opens a store carrying one lease-guarded identity.
func openWithIdentity(t *testing.T, identity domain.AuthIdentity) *store.Store {
	t.Helper()
	ctx := context.Background()
	s := storetest.Open(t, tempDBPath(t), store.Options{})
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordAuthIdentity(ctx, identity, leaseEpoch)
	}); err != nil {
		t.Fatalf("RecordAuthIdentity: %v", err)
	}
	return s
}

func TestAuthIdentityRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	identity := testAuthIdentity()
	s := openWithIdentity(t, identity)

	var got domain.AuthIdentity
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		got, err = tx.GetAuthIdentity(ctx, identity.ID)
		return err
	}); err != nil {
		t.Fatalf("GetAuthIdentity: %v", err)
	}
	if got != identity {
		t.Fatalf("identity = %+v, want %+v", got, identity)
	}

	// A re-measured parallelism limit is a legal successor; a changed
	// provider or a dropped lease requirement is not.
	remeasured := identity
	remeasured.MaxParallelExecutions = 4
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordAuthIdentity(ctx, remeasured, leaseEpoch.Add(time.Hour))
	}); err != nil {
		t.Fatalf("re-measured limit must be recordable: %v", err)
	}

	retired := identity
	retired.AuthStoreMutationLease = false
	err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordAuthIdentity(ctx, retired, leaseEpoch.Add(2*time.Hour))
	})
	if !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("dropping the lease requirement = %v, want %v", err, store.ErrImmutableConflict)
	}

	rebound := identity
	rebound.Interim.AuthStoreVolume = "other-provider-cred"
	err = s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordAuthIdentity(ctx, rebound, leaseEpoch.Add(3*time.Hour))
	})
	if !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("changing the auth-store volume = %v, want %v", err, store.ErrImmutableConflict)
	}
}

// TestAuthIdentityRevisionsOnlyMoveForward refuses a declaration stamped
// before the stored one. 1B re-measures the parallelism limit, so a delayed
// older measurement landing after a newer one would otherwise reinstate a
// superseded limit, and the direction that matters is the one that raises
// concurrency past the latest safe result.
func TestAuthIdentityRevisionsOnlyMoveForward(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	identity := testAuthIdentity()
	s := openWithIdentity(t, identity)

	newer := identity
	newer.MaxParallelExecutions = 1
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordAuthIdentity(ctx, newer, leaseEpoch.Add(time.Hour))
	}); err != nil {
		t.Fatalf("newer revision: %v", err)
	}

	stale := identity
	stale.MaxParallelExecutions = 8
	err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordAuthIdentity(ctx, stale, leaseEpoch.Add(time.Minute))
	})
	if !errors.Is(err, store.ErrStaleWrite) {
		t.Fatalf("stale revision = %v, want %v", err, store.ErrStaleWrite)
	}

	var got domain.AuthIdentity
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		got, err = tx.GetAuthIdentity(ctx, identity.ID)
		return err
	}); err != nil {
		t.Fatalf("GetAuthIdentity: %v", err)
	}
	if got.MaxParallelExecutions != 1 {
		t.Fatalf("max_parallel_executions = %d, want the newer revision's 1", got.MaxParallelExecutions)
	}

	// The same instant is a retry of the recorded revision, not a regression.
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordAuthIdentity(ctx, newer, leaseEpoch.Add(time.Hour))
	}); err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}

	// A divergent declaration sharing that instant is a conflict, not an
	// update: an equal timestamp is no ordering evidence, so it must not be
	// the gap a superseded limit comes back through.
	conflicting := newer
	conflicting.MaxParallelExecutions = 8
	err = s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordAuthIdentity(ctx, conflicting, leaseEpoch.Add(time.Hour))
	})
	if !errors.Is(err, store.ErrStaleWrite) {
		t.Fatalf("divergent revision at the stored instant = %v, want %v", err, store.ErrStaleWrite)
	}
}

// TestAcquireLeaseSingleWinner is the §5.4 serialization point: one holder per
// identity, and a second acquirer is refused with the current holder named
// rather than queued behind a lock nobody can see.
func TestAcquireLeaseSingleWinner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openWithIdentity(t, testAuthIdentity())
	expires := leaseEpoch.Add(time.Minute)

	var first domain.AuthStoreMutationLease
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		first, err = tx.AcquireAuthStoreMutationLease(ctx, "auth-1", "inv-1", leaseEpoch, expires)
		return err
	}); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if first.Fence != 1 || first.Holder != "inv-1" {
		t.Fatalf("first lease = %+v, want fence 1 held by inv-1", first)
	}

	err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.AcquireAuthStoreMutationLease(ctx, "auth-1", "inv-2", leaseEpoch.Add(time.Second), expires)
		return err
	})
	if !errors.Is(err, store.ErrLeaseHeld) {
		t.Fatalf("second acquire = %v, want %v", err, store.ErrLeaseHeld)
	}
	var held *store.LeaseHeldError
	if !errors.As(err, &held) {
		t.Fatalf("want *LeaseHeldError, got %T", err)
	}
	if held.Holder != "inv-1" || held.Fence != 1 {
		t.Errorf("refusal names %q (fence %d), want inv-1 (fence 1)", held.Holder, held.Fence)
	}

	// The same holder re-acquiring converges on the lease it already has,
	// without extending the window a retry did not earn.
	var again domain.AuthStoreMutationLease
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		again, err = tx.AcquireAuthStoreMutationLease(ctx, "auth-1", "inv-1",
			leaseEpoch.Add(time.Second), expires.Add(time.Hour))
		return err
	}); err != nil {
		t.Fatalf("same-holder re-acquire: %v", err)
	}
	if again != first {
		t.Fatalf("re-acquired lease = %+v, want the unchanged %+v", again, first)
	}
}

// TestLeaseTakeoverBumpsTheFence covers the process that died holding a lease:
// nothing releases it, so expiry is the only recovery, and the fence is what
// keeps the zombie from acting on a lease it no longer has.
func TestLeaseTakeoverBumpsTheFence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openWithIdentity(t, testAuthIdentity())
	expires := leaseEpoch.Add(time.Minute)

	var stale domain.AuthStoreMutationLease
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		stale, err = tx.AcquireAuthStoreMutationLease(ctx, "auth-1", "inv-1", leaseEpoch, expires)
		return err
	}); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	after := expires.Add(time.Second)
	var taken domain.AuthStoreMutationLease
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		taken, err = tx.AcquireAuthStoreMutationLease(ctx, "auth-1", "inv-2", after, after.Add(time.Minute))
		return err
	}); err != nil {
		t.Fatalf("takeover after expiry: %v", err)
	}
	if taken.Fence != stale.Fence+1 || taken.Holder != "inv-2" {
		t.Fatalf("taken lease = %+v, want fence %d held by inv-2", taken, stale.Fence+1)
	}
	if taken.ReleasedAt != nil {
		t.Errorf("taken lease carries the previous release: %+v", taken)
	}

	// The stalled first holder wakes up and tries to act on its lease.
	err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.RenewAuthStoreMutationLease(ctx, "auth-1", "inv-1", stale.Fence,
			after.Add(time.Second), after.Add(time.Hour))
		return err
	})
	if !errors.Is(err, store.ErrLeaseNotHeld) {
		t.Fatalf("stale renew = %v, want %v", err, store.ErrLeaseNotHeld)
	}
	err = s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.ReleaseAuthStoreMutationLease(ctx, "auth-1", "inv-1", stale.Fence, after.Add(time.Second))
	})
	if !errors.Is(err, store.ErrLeaseNotHeld) {
		t.Fatalf("stale release = %v, want %v", err, store.ErrLeaseNotHeld)
	}
}

func TestRenewAndReleaseLease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openWithIdentity(t, testAuthIdentity())
	expires := leaseEpoch.Add(time.Minute)

	var lease domain.AuthStoreMutationLease
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		lease, err = tx.AcquireAuthStoreMutationLease(ctx, "auth-1", "inv-1", leaseEpoch, expires)
		return err
	}); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// Renewal extends the window and does not bump the fence: renewing is not
	// a change of ownership, so an outstanding fence stays valid.
	var renewed domain.AuthStoreMutationLease
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		renewed, err = tx.RenewAuthStoreMutationLease(ctx, "auth-1", "inv-1", lease.Fence,
			leaseEpoch.Add(30*time.Second), expires.Add(time.Minute))
		return err
	}); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if renewed.Fence != lease.Fence {
		t.Errorf("renewal bumped the fence to %d, want %d", renewed.Fence, lease.Fence)
	}
	if !renewed.ExpiresAt.Equal(expires.Add(time.Minute)) {
		t.Errorf("renewed expiry = %s, want %s", renewed.ExpiresAt, expires.Add(time.Minute))
	}

	// A non-holder can neither renew nor release.
	err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.RenewAuthStoreMutationLease(ctx, "auth-1", "inv-2", lease.Fence,
			leaseEpoch.Add(31*time.Second), expires.Add(2*time.Minute))
		return err
	})
	if !errors.Is(err, store.ErrLeaseNotHeld) {
		t.Fatalf("foreign renew = %v, want %v", err, store.ErrLeaseNotHeld)
	}
	err = s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.ReleaseAuthStoreMutationLease(ctx, "auth-1", "inv-2", lease.Fence, leaseEpoch.Add(32*time.Second))
	})
	if !errors.Is(err, store.ErrLeaseNotHeld) {
		t.Fatalf("foreign release = %v, want %v", err, store.ErrLeaseNotHeld)
	}

	// A release stamped past the window is not a release: the lease was
	// already over, and recording it would poison later acquisitions, whose
	// instants must follow the current generation's release.
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.ReleaseAuthStoreMutationLease(ctx, "auth-1", "inv-1", lease.Fence,
			renewed.ExpiresAt.Add(24*time.Hour))
	}); !errors.Is(err, store.ErrLeaseWindowRegresses) {
		t.Fatalf("release past the window = %v, want %v", err, store.ErrLeaseWindowRegresses)
	}

	releasedAt := leaseEpoch.Add(40 * time.Second)
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.ReleaseAuthStoreMutationLease(ctx, "auth-1", "inv-1", lease.Fence, releasedAt)
	}); err != nil {
		t.Fatalf("release: %v", err)
	}
	// A repeated release of the same lease converges rather than failing a
	// crash-retry that already did its job.
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.ReleaseAuthStoreMutationLease(ctx, "auth-1", "inv-1", lease.Fence, releasedAt)
	}); err != nil {
		t.Fatalf("repeated release: %v", err)
	}

	// Released early, the lease is free before its expiry.
	var next domain.AuthStoreMutationLease
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		next, err = tx.AcquireAuthStoreMutationLease(ctx, "auth-1", "inv-2",
			releasedAt.Add(time.Second), releasedAt.Add(time.Minute))
		return err
	}); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if next.Holder != "inv-2" || next.Fence != lease.Fence+1 {
		t.Fatalf("next lease = %+v, want fence %d held by inv-2", next, lease.Fence+1)
	}
}

// TestLeaseWindowOnlyExtends refuses the windows a delayed or reordered call
// would otherwise record: one that has already passed, and one that moves an
// existing expiry earlier. Either would report a held lease the holder does
// not really have for as long as it thinks.
func TestLeaseWindowOnlyExtends(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openWithIdentity(t, testAuthIdentity())
	expires := leaseEpoch.Add(time.Minute)

	// An acquisition whose window has already closed grants nothing.
	err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.AcquireAuthStoreMutationLease(ctx, "auth-1", "inv-1",
			leaseEpoch, leaseEpoch.Add(-time.Second))
		return err
	})
	if !errors.Is(err, store.ErrLeaseWindowRegresses) {
		t.Fatalf("acquire with a past expiry = %v, want %v", err, store.ErrLeaseWindowRegresses)
	}

	var lease domain.AuthStoreMutationLease
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		lease, err = tx.AcquireAuthStoreMutationLease(ctx, "auth-1", "inv-1", leaseEpoch, expires)
		return err
	}); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	cases := []struct {
		name      string
		now       time.Time
		expiresAt time.Time
	}{
		{
			// A regressed clock names an instant before this generation
			// existed: renewing there would extend it without a takeover.
			"clock regressed before acquisition",
			leaseEpoch.Add(-time.Second), expires.Add(time.Hour),
		},
		{"shortened window", leaseEpoch.Add(10 * time.Second), expires.Add(-30 * time.Second)},
		{"already expired", leaseEpoch.Add(10 * time.Second), leaseEpoch.Add(5 * time.Second)},
		{"exactly now", leaseEpoch.Add(10 * time.Second), leaseEpoch.Add(10 * time.Second)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
				_, err := tx.RenewAuthStoreMutationLease(ctx, "auth-1", "inv-1", lease.Fence, tc.now, tc.expiresAt)
				return err
			})
			// A pre-acquisition instant is refused as not-held; the others
			// are refused as a window that would not extend.
			if !errors.Is(err, store.ErrLeaseWindowRegresses) && !errors.Is(err, store.ErrLeaseNotHeld) {
				t.Fatalf("renew = %v, want a refusal", err)
			}
		})
	}

	// The stored window is untouched by the refusals, and an exact replay of
	// the current expiry is idempotent rather than an error.
	var after domain.AuthStoreMutationLease
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		after, err = tx.RenewAuthStoreMutationLease(ctx, "auth-1", "inv-1", lease.Fence,
			leaseEpoch.Add(10*time.Second), expires)
		return err
	}); err != nil {
		t.Fatalf("idempotent renew: %v", err)
	}
	if !after.ExpiresAt.Equal(expires) {
		t.Fatalf("expiry = %s, want the unchanged %s", after.ExpiresAt, expires)
	}
}

// TestStaleAcquisitionAfterReleaseIsRefused covers the delayed acquirer whose
// instant predates the generation it would take over: a released lease is not
// held at any instant, so nothing else stops such a call from installing its
// own (still future-dated) window and blocking the holders that come after it.
func TestStaleAcquisitionAfterReleaseIsRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openWithIdentity(t, testAuthIdentity())

	var first domain.AuthStoreMutationLease
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		first, err = tx.AcquireAuthStoreMutationLease(ctx, "auth-1", "inv-1",
			leaseEpoch, leaseEpoch.Add(time.Minute))
		return err
	}); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// A later generation takes over and is released.
	takeover := leaseEpoch.Add(2 * time.Minute)
	var second domain.AuthStoreMutationLease
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		second, err = tx.AcquireAuthStoreMutationLease(ctx, "auth-1", "inv-2",
			takeover, takeover.Add(time.Minute))
		return err
	}); err != nil {
		t.Fatalf("takeover: %v", err)
	}
	released := takeover.Add(10 * time.Second)
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.ReleaseAuthStoreMutationLease(ctx, "auth-1", "inv-2", second.Fence, released)
	}); err != nil {
		t.Fatalf("release: %v", err)
	}

	// A request stamped before that generation even began now arrives.
	err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.AcquireAuthStoreMutationLease(ctx, "auth-1", "inv-3",
			leaseEpoch.Add(30*time.Second), leaseEpoch.Add(10*time.Minute))
		return err
	})
	if !errors.Is(err, store.ErrLeaseWindowRegresses) {
		t.Fatalf("stale acquisition after release = %v, want %v", err, store.ErrLeaseWindowRegresses)
	}

	// One stamped after the release is ordinary and still succeeds.
	var third domain.AuthStoreMutationLease
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		third, err = tx.AcquireAuthStoreMutationLease(ctx, "auth-1", "inv-3",
			released.Add(time.Second), released.Add(time.Minute))
		return err
	}); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if third.Fence != second.Fence+1 || third.Holder != "inv-3" {
		t.Fatalf("lease = %+v, want fence %d held by inv-3", third, second.Fence+1)
	}
	if first.Fence != 1 {
		t.Fatalf("first generation fence = %d, want 1", first.Fence)
	}
}

// TestLeaseLivenessUsesTheCallersClock pins the reconstruction contract: the
// stored row reports what it says, and whether the window is open is decided
// against the caller's instant, never read out of the row as a verdict.
func TestLeaseLivenessUsesTheCallersClock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openWithIdentity(t, testAuthIdentity())
	expires := leaseEpoch.Add(time.Minute)
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.AcquireAuthStoreMutationLease(ctx, "auth-1", "inv-1", leaseEpoch, expires)
		return err
	}); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	var stored domain.AuthStoreMutationLease
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		stored, err = tx.GetAuthStoreMutationLease(ctx, "auth-1")
		return err
	}); err != nil {
		t.Fatalf("GetAuthStoreMutationLease: %v", err)
	}
	if !stored.HeldAt(leaseEpoch.Add(time.Second)) {
		t.Error("lease should be held inside its window")
	}
	if stored.HeldAt(expires) {
		t.Error("lease should not be held at its expiry")
	}
}

// TestLeaseRequiresADeclaringIdentity is the trust-boundary re-gate: the
// identity's own declaration is the live authority, so a lease can neither be
// taken nor reconstructed for an identity that does not require one, and an
// unknown identity is not found rather than implicitly created.
func TestLeaseRequiresADeclaringIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	unguarded := testAuthIdentity()
	unguarded.ID = "auth-unguarded"
	unguarded.AuthStoreMutationLease = false
	s := openWithIdentity(t, unguarded)

	err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.AcquireAuthStoreMutationLease(ctx, unguarded.ID, "inv-1", leaseEpoch, leaseEpoch.Add(time.Minute))
		return err
	})
	if !errors.Is(err, store.ErrLeaseNotDeclared) {
		t.Fatalf("acquire on an unguarded identity = %v, want %v", err, store.ErrLeaseNotDeclared)
	}

	err = s.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetAuthStoreMutationLease(ctx, unguarded.ID)
		return err
	})
	if !errors.Is(err, store.ErrLeaseNotDeclared) {
		t.Fatalf("get on an unguarded identity = %v, want %v", err, store.ErrLeaseNotDeclared)
	}

	err = s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.AcquireAuthStoreMutationLease(ctx, "auth-unknown", "inv-1", leaseEpoch, leaseEpoch.Add(time.Minute))
		return err
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("acquire on an unknown identity = %v, want %v", err, store.ErrNotFound)
	}
}

// TestLeaseSurvivesReopen proves the lease is durable state, not process
// state: a restarted daemon still sees the window it must wait out.
func TestLeaseSurvivesReopen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := tempDBPath(t)
	identity := testAuthIdentity()
	expires := leaseEpoch.Add(time.Minute)

	s := storetest.Open(t, path, store.Options{})
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RecordAuthIdentity(ctx, identity, leaseEpoch); err != nil {
			return err
		}
		_, err := tx.AcquireAuthStoreMutationLease(ctx, identity.ID, "inv-1", leaseEpoch, expires)
		return err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := storetest.Open(t, path, store.Options{})

	err := reopened.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.AcquireAuthStoreMutationLease(ctx, identity.ID, "inv-2",
			leaseEpoch.Add(time.Second), expires)
		return err
	})
	if !errors.Is(err, store.ErrLeaseHeld) {
		t.Fatalf("acquire after restart = %v, want %v", err, store.ErrLeaseHeld)
	}
}
