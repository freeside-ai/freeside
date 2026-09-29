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

func acquireReadHold(
	t *testing.T, s *store.Store, holder domain.InvocationID, now, expires time.Time,
) (domain.AuthStoreReadHold, error) {
	t.Helper()
	var hold domain.AuthStoreReadHold
	err := s.WriteInternal(context.Background(), func(tx *store.InternalTx) error {
		var err error
		hold, err = tx.AcquireAuthStoreReadHold(context.Background(), "auth-1", holder, now, expires)
		return err
	})
	return hold, err
}

func acquireLease(t *testing.T, s *store.Store, holder domain.InvocationID, now, expires time.Time) error {
	t.Helper()
	return s.WriteInternal(context.Background(), func(tx *store.InternalTx) error {
		_, err := tx.AcquireAuthStoreMutationLease(context.Background(), "auth-1", holder, now, expires)
		return err
	})
}

func releaseReadHold(t *testing.T, s *store.Store, hold domain.AuthStoreReadHold, at time.Time) error {
	t.Helper()
	return s.WriteInternal(context.Background(), func(tx *store.InternalTx) error {
		return tx.ReleaseAuthStoreReadHold(context.Background(), hold.AuthIdentityID, hold.Holder, hold.AcquiredAt, at)
	})
}

// TestReadHoldsShareAndExcludeTheLease pins the #1585 exclusion: read holds
// from different holders coexist, and the mutation lease is refused while any
// of them is live, until the last one is released.
func TestReadHoldsShareAndExcludeTheLease(t *testing.T) {
	t.Parallel()
	s := openWithIdentity(t, testAuthIdentity())
	expires := leaseEpoch.Add(time.Minute)

	first, err := acquireReadHold(t, s, "inv-1", leaseEpoch, expires)
	if err != nil {
		t.Fatalf("first read hold: %v", err)
	}
	second, err := acquireReadHold(t, s, "inv-2", leaseEpoch.Add(time.Second), expires)
	if err != nil {
		t.Fatalf("second read hold: %v", err)
	}

	err = acquireLease(t, s, "mutator", leaseEpoch.Add(2*time.Second), expires)
	if !errors.Is(err, store.ErrLeaseHeld) {
		t.Fatalf("lease under read = %v, want %v", err, store.ErrLeaseHeld)
	}
	var readHeld *store.ReadHeldError
	if !errors.As(err, &readHeld) || readHeld.Holder != "inv-1" {
		t.Fatalf("lease refusal = %v, want a ReadHeldError naming inv-1", err)
	}

	if err := releaseReadHold(t, s, first, leaseEpoch.Add(3*time.Second)); err != nil {
		t.Fatalf("release first: %v", err)
	}
	if err := acquireLease(t, s, "mutator", leaseEpoch.Add(4*time.Second), expires); !errors.Is(err, store.ErrLeaseHeld) {
		t.Fatalf("lease with one read hold left = %v, want %v", err, store.ErrLeaseHeld)
	}
	if err := releaseReadHold(t, s, second, leaseEpoch.Add(5*time.Second)); err != nil {
		t.Fatalf("release second: %v", err)
	}
	if err := acquireLease(t, s, "mutator", leaseEpoch.Add(6*time.Second), expires); err != nil {
		t.Fatalf("lease after every release: %v", err)
	}
}

// TestLeaseExcludesReadHolds is the other direction: no execution starts
// reading while a mutation holds the lease.
func TestLeaseExcludesReadHolds(t *testing.T) {
	t.Parallel()
	s := openWithIdentity(t, testAuthIdentity())
	expires := leaseEpoch.Add(time.Minute)
	if err := acquireLease(t, s, "mutator", leaseEpoch, expires); err != nil {
		t.Fatalf("lease: %v", err)
	}

	_, err := acquireReadHold(t, s, "inv-1", leaseEpoch.Add(time.Second), expires)
	var held *store.LeaseHeldError
	if !errors.As(err, &held) || held.Holder != "mutator" {
		t.Fatalf("read hold under lease = %v, want a LeaseHeldError naming mutator", err)
	}

	if err := s.WriteInternal(context.Background(), func(tx *store.InternalTx) error {
		return tx.ReleaseAuthStoreMutationLease(context.Background(), "auth-1", "mutator", 1, leaseEpoch.Add(2*time.Second))
	}); err != nil {
		t.Fatalf("release lease: %v", err)
	}
	if _, err := acquireReadHold(t, s, "inv-1", leaseEpoch.Add(3*time.Second), expires); err != nil {
		t.Fatalf("read hold after lease release: %v", err)
	}
}

// TestLeaseRenewalExcludesReadHolds: a renewal carrying an instant sampled
// before its lease expired must not extend that lease over a read hold
// opened after the expiry, or a mutation could run under a live reader.
func TestLeaseRenewalExcludesReadHolds(t *testing.T) {
	t.Parallel()
	s := openWithIdentity(t, testAuthIdentity())
	expires := leaseEpoch.Add(time.Minute)
	if err := acquireLease(t, s, "mutator", leaseEpoch, expires); err != nil {
		t.Fatalf("lease: %v", err)
	}
	hold, err := acquireReadHold(t, s, "inv-1", expires, expires.Add(time.Minute))
	if err != nil {
		t.Fatalf("read hold after lease expiry: %v", err)
	}

	renew := func(now time.Time) error {
		return s.WriteInternal(context.Background(), func(tx *store.InternalTx) error {
			_, err := tx.RenewAuthStoreMutationLease(context.Background(), "auth-1", "mutator", 1,
				now, expires.Add(5*time.Minute))
			return err
		})
	}
	if err := renew(expires.Add(-30 * time.Second)); !errors.Is(err, store.ErrLeaseWindowRegresses) {
		t.Fatalf("renewal at a pre-expiry instant under a later read hold = %v, want %v",
			err, store.ErrLeaseWindowRegresses)
	}
	if err := releaseReadHold(t, s, hold, expires.Add(time.Second)); err != nil {
		t.Fatalf("release read hold: %v", err)
	}
	if err := s.Read(context.Background(), func(tx *store.ReadTx) error {
		lease, err := tx.GetAuthStoreMutationLease(context.Background(), "auth-1")
		if err == nil && !lease.ExpiresAt.Equal(expires) {
			t.Errorf("lease expiry = %v, want the unrenewed %v", lease.ExpiresAt, expires)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// TestReadHoldExpiryFreesTheLease covers a reader that died holding its
// window: expiry, measured on the caller's clock, is the backstop.
func TestReadHoldExpiryFreesTheLease(t *testing.T) {
	t.Parallel()
	s := openWithIdentity(t, testAuthIdentity())
	expires := leaseEpoch.Add(time.Minute)
	if _, err := acquireReadHold(t, s, "inv-1", leaseEpoch, expires); err != nil {
		t.Fatalf("read hold: %v", err)
	}
	if err := acquireLease(t, s, "mutator", expires.Add(-time.Nanosecond), expires.Add(time.Minute)); !errors.Is(err, store.ErrLeaseHeld) {
		t.Fatalf("lease before expiry = %v, want %v", err, store.ErrLeaseHeld)
	}
	if err := acquireLease(t, s, "mutator", expires, expires.Add(time.Minute)); err != nil {
		t.Fatalf("lease at expiry: %v", err)
	}
	// A clock that regressed behind a read hold's opening is not evidence
	// the hold is over.
	s2 := openWithIdentity(t, testAuthIdentity())
	if _, err := acquireReadHold(t, s2, "inv-1", leaseEpoch.Add(time.Minute), leaseEpoch.Add(2*time.Minute)); err != nil {
		t.Fatalf("read hold: %v", err)
	}
	if err := acquireLease(t, s2, "mutator", leaseEpoch, leaseEpoch.Add(3*time.Minute)); !errors.Is(err, store.ErrLeaseWindowRegresses) {
		t.Fatalf("lease at a regressed instant = %v, want %v", err, store.ErrLeaseWindowRegresses)
	}
}

// TestReadHoldSameHolderNeverConverges pins that a run opens its own window:
// a live same-holder window refuses, an ended one is replaced, and a stale
// release naming the old window cannot end the new one.
func TestReadHoldSameHolderNeverConverges(t *testing.T) {
	t.Parallel()
	s := openWithIdentity(t, testAuthIdentity())
	expires := leaseEpoch.Add(time.Minute)
	old, err := acquireReadHold(t, s, "inv-1", leaseEpoch, expires)
	if err != nil {
		t.Fatalf("read hold: %v", err)
	}
	if _, err := acquireReadHold(t, s, "inv-1", leaseEpoch.Add(time.Second), expires); !errors.Is(err, store.ErrLeaseHeld) {
		t.Fatalf("live same-holder re-acquire = %v, want %v", err, store.ErrLeaseHeld)
	}
	fresh, err := acquireReadHold(t, s, "inv-1", expires, expires.Add(time.Minute))
	if err != nil {
		t.Fatalf("re-acquire after expiry: %v", err)
	}
	if err := releaseReadHold(t, s, old, expires.Add(time.Second)); !errors.Is(err, store.ErrReadHoldNotHeld) {
		t.Fatalf("stale release = %v, want %v", err, store.ErrReadHoldNotHeld)
	}
	if err := releaseReadHold(t, s, fresh, expires.Add(time.Second)); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := releaseReadHold(t, s, fresh, expires.Add(2*time.Second)); err != nil {
		t.Fatalf("release replay: %v", err)
	}
	if err := releaseReadHold(t, s, domain.AuthStoreReadHold{
		AuthIdentityID: "auth-1", Holder: "inv-unknown", AcquiredAt: leaseEpoch,
	}, leaseEpoch); !errors.Is(err, store.ErrReadHoldNotHeld) {
		t.Fatalf("release of a missing hold = %v, want %v", err, store.ErrReadHoldNotHeld)
	}
}

func TestReadHoldReleaseOutsideTheWindowIsRefused(t *testing.T) {
	t.Parallel()
	s := openWithIdentity(t, testAuthIdentity())
	expires := leaseEpoch.Add(time.Minute)
	hold, err := acquireReadHold(t, s, "inv-1", leaseEpoch, expires)
	if err != nil {
		t.Fatalf("read hold: %v", err)
	}
	if err := releaseReadHold(t, s, hold, expires); !errors.Is(err, store.ErrLeaseWindowRegresses) {
		t.Fatalf("release at expiry = %v, want %v", err, store.ErrLeaseWindowRegresses)
	}
	if _, err := acquireReadHold(t, s, "inv-2", leaseEpoch, leaseEpoch); !errors.Is(err, store.ErrLeaseWindowRegresses) {
		t.Fatalf("empty window = %v, want %v", err, store.ErrLeaseWindowRegresses)
	}
}

func TestReadHoldRequiresADeclaringIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	unguarded := testAuthIdentity()
	unguarded.AuthStoreMutationLease = false
	s := openWithIdentity(t, unguarded)
	if _, err := acquireReadHold(t, s, "inv-1", leaseEpoch, leaseEpoch.Add(time.Minute)); !errors.Is(err, store.ErrLeaseNotDeclared) {
		t.Fatalf("read hold on an unguarded identity = %v, want %v", err, store.ErrLeaseNotDeclared)
	}
	err := s.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetAuthStoreReadHold(ctx, "auth-1", "inv-1")
		return err
	})
	if !errors.Is(err, store.ErrLeaseNotDeclared) {
		t.Fatalf("get on an unguarded identity = %v, want %v", err, store.ErrLeaseNotDeclared)
	}
}

// TestReadHoldSurvivesReopen proves a read hold is durable: a restarted
// daemon still refuses a mutation while the window is open.
func TestReadHoldSurvivesReopen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := tempDBPath(t)
	expires := leaseEpoch.Add(time.Minute)
	s := storetest.Open(t, path, store.Options{})
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RecordAuthIdentity(ctx, testAuthIdentity(), leaseEpoch); err != nil {
			return err
		}
		_, err := tx.AcquireAuthStoreReadHold(ctx, "auth-1", "inv-1", leaseEpoch, expires)
		return err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened := storetest.Open(t, path, store.Options{})
	if err := acquireLease(t, reopened, "mutator", leaseEpoch.Add(time.Second), expires); !errors.Is(err, store.ErrLeaseHeld) {
		t.Fatalf("lease after restart = %v, want %v", err, store.ErrLeaseHeld)
	}
}
