package ward

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Shared read holds for a read-only identity mount (#1585). The handoff keeps
// every check the exclusive lease drives (the identity-to-volume binding, the
// pre- and post-writer credential digests, the atomic journal open, the
// pre-start re-verify, and recovery's re-gate); only the kind of window
// changes. A read hold carries no fence, so the checks below bind the window
// by identity, holder, and its exact bounds instead.

// readHolder returns the configured leaser's read-hold seam, failing closed
// when the leaser does not provide one: a read-only claim must never fall
// back to running with no window at all.
func (b *Backend) readHolder(claim AuthStoreLeaseClaim) (AuthStoreReadHolder, error) {
	if b.cfg.AuthStoreLeaser == nil {
		return nil, failf(CheckAuthStoreMutationLease,
			"spec claims an auth-store read hold for identity %q but no leaser is configured", claim.AuthIdentityID)
	}
	holder, ok := b.cfg.AuthStoreLeaser.(AuthStoreReadHolder)
	if !ok {
		return nil, failf(CheckAuthStoreMutationLease,
			"leaser does not support auth-store read holds for identity %q", claim.AuthIdentityID)
	}
	return holder, nil
}

// beginReadHeldHandoff is beginLeasedHandoff for a shared read window: it
// reserves the in-process slot, then commits the journal record and read hold
// in one transaction, holding the record recover-only until the returned hold
// passes the gate's trust checks.
func (b *Backend) beginReadHeldHandoff(
	ctx context.Context,
	opener ReadHeldHandoffOpener,
	rec HandoffJournalRecord,
	claim AuthStoreLeaseClaim,
	st *runState,
) error {
	if _, err := b.readHolder(claim); err != nil {
		return err
	}
	if err := b.reserveAuthStoreLeaseSlot(claim, st); err != nil {
		return err
	}
	now, wantExpiry := b.authStoreLeaseWindow()
	rec.OpenedAt = now
	hold, err := opener.BeginReadHeld(ctx, rec, claim, now, wantExpiry)
	if err != nil {
		return fmt.Errorf("begin read-held handoff journal for identity %q: %w", claim.AuthIdentityID, err)
	}
	st.journalOpen = true
	st.journalRecoverOnly = true
	if err := b.acceptAuthStoreReadHold(ctx, claim, now, wantExpiry, hold, st); err != nil {
		return err
	}
	st.journalRecoverOnly = false
	return nil
}

// acquireAuthStoreReadHold is the journalless counterpart of
// beginReadHeldHandoff, mirroring acquireAuthStoreLease.
func (b *Backend) acquireAuthStoreReadHold(ctx context.Context, claim AuthStoreLeaseClaim, st *runState) error {
	holder, err := b.readHolder(claim)
	if err != nil {
		return err
	}
	if err := b.reserveAuthStoreLeaseSlot(claim, st); err != nil {
		return err
	}
	now, wantExpiry := b.authStoreLeaseWindow()
	hold, err := holder.AcquireRead(ctx, claim.AuthIdentityID, claim.Holder, now, wantExpiry)
	if err != nil {
		return fmt.Errorf("acquire auth store read hold for identity %q: %w", claim.AuthIdentityID, err)
	}
	return b.acceptAuthStoreReadHold(ctx, claim, now, wantExpiry, hold, st)
}

// acceptAuthStoreReadHold re-verifies the returned hold as store output, with
// acceptAuthStoreLease's checks minus the fence: a valid row naming this
// claim, live and opened at exactly this acquisition, and covering the run's
// whole budget.
func (b *Backend) acceptAuthStoreReadHold(
	ctx context.Context,
	claim AuthStoreLeaseClaim,
	now, wantExpiry time.Time,
	hold domain.AuthStoreReadHold,
	st *runState,
) error {
	if err := hold.Validate(); err != nil {
		return failf(CheckAuthStoreMutationLease, "acquired read hold for identity %q is malformed: %v", claim.AuthIdentityID, err)
	}
	if hold.AuthIdentityID != claim.AuthIdentityID || hold.Holder != claim.Holder {
		return failf(CheckAuthStoreMutationLease,
			"acquired read hold does not name the claimed identity %q and holder", claim.AuthIdentityID)
	}
	if !hold.HeldAt(now) {
		return failf(CheckAuthStoreMutationLease, "acquired read hold for identity %q is not live at acquisition", claim.AuthIdentityID)
	}
	if !hold.AcquiredAt.Equal(now) {
		return failf(CheckAuthStoreMutationLease,
			"identity %q returned a read window from an earlier acquisition", claim.AuthIdentityID)
	}
	if hold.ExpiresAt.Before(wantExpiry) {
		// Provably this run's window (opened at exactly now), so release it
		// rather than leave it blocking mutation until expiry.
		st.readHold, st.leaseHeld = hold, true
		reason := fmt.Sprintf("acquired read hold for identity %q ends at %s, before the run's budget needs it",
			claim.AuthIdentityID, hold.ExpiresAt)
		if problems := b.releaseAuthStoreReadHold(ctx, st); len(problems) > 0 {
			reason += "; " + strings.Join(problems, "; ")
		}
		if !st.leaseHeld {
			st.journalRecoverOnly = false
		}
		return failf(CheckAuthStoreMutationLease, "%s", reason)
	}
	st.readHold = hold
	st.leaseHeld = true
	return nil
}

// verifyAuthStoreReadHoldLive re-reads the run's read hold immediately
// before the writer starts, held to the acquisition's gate: a valid row, the
// exact acquired window, still live. A read hold cannot be taken over, but it
// can be released or replaced by a stale actor, and the writer must not start
// outside a window that excludes mutation.
func (b *Backend) verifyAuthStoreReadHoldLive(ctx context.Context, st *runState) error {
	claim := AuthStoreLeaseClaim{AuthIdentityID: st.readHold.AuthIdentityID, Holder: st.readHold.Holder}
	holder, err := b.readHolder(claim)
	if err != nil {
		return err
	}
	current, err := holder.GetRead(ctx, st.readHold.AuthIdentityID, st.readHold.Holder)
	if err != nil {
		return fmt.Errorf("re-verify auth store read hold for identity %q: %w", st.readHold.AuthIdentityID, err)
	}
	if verr := current.Validate(); verr != nil {
		return failf(CheckAuthStoreMutationLease,
			"re-read read hold for identity %q is malformed: %v", st.readHold.AuthIdentityID, verr)
	}
	if !sameReadWindow(current, st.readHold) {
		return failf(CheckAuthStoreMutationLease,
			"read hold for identity %q changed before the writer started", st.readHold.AuthIdentityID)
	}
	if !current.HeldAt(b.cfg.Now()) {
		return failf(CheckAuthStoreMutationLease,
			"read hold for identity %q is no longer live before the writer started", st.readHold.AuthIdentityID)
	}
	return nil
}

// releaseAuthStoreReadHold ends the run's read window, detached from the
// run's context as the lease release is. An already-ended window is nothing
// left to release.
func (b *Backend) releaseAuthStoreReadHold(ctx context.Context, st *runState) []string {
	if !st.leaseHeld {
		return nil
	}
	claim := AuthStoreLeaseClaim{AuthIdentityID: st.readHold.AuthIdentityID, Holder: st.readHold.Holder}
	holder, err := b.readHolder(claim)
	if err != nil {
		return []string{fmt.Sprintf("release auth store read hold for identity %q: %v", claim.AuthIdentityID, err)}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.cfg.TeardownTimeout)
	defer cancel()
	if err := holder.ReleaseRead(ctx, st.readHold.AuthIdentityID, st.readHold.Holder,
		st.readHold.AcquiredAt, b.cfg.Now()); err != nil {
		if errors.Is(err, ErrLeaseWindowEnded) {
			st.leaseHeld = false
			return nil
		}
		return []string{fmt.Sprintf("release auth store read hold for identity %q: %v", st.readHold.AuthIdentityID, err)}
	}
	st.leaseHeld = false
	return nil
}

// sameReadWindow reports whether two read hold rows name the same window.
func sameReadWindow(a, b domain.AuthStoreReadHold) bool {
	return a.AuthIdentityID == b.AuthIdentityID && a.Holder == b.Holder &&
		a.AcquiredAt.Equal(b.AcquiredAt) && a.ExpiresAt.Equal(b.ExpiresAt)
}

// windowKind names the run's auth-store window for teardown messages.
func (st *runState) windowKind() string {
	if st.sharedWindow {
		return "read hold"
	}
	return "mutation lease"
}

// windowIdentity is the identity whose auth-store window the run holds.
func (st *runState) windowIdentity() domain.AuthIdentityID {
	if st.sharedWindow {
		return st.readHold.AuthIdentityID
	}
	return st.lease.AuthIdentityID
}

// windowExpiresAt is the end of the run's auth-store window.
func (st *runState) windowExpiresAt() time.Time {
	if st.sharedWindow {
		return st.readHold.ExpiresAt
	}
	return st.lease.ExpiresAt
}
