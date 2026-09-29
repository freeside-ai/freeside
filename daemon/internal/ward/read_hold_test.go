package ward

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// testReadHeldHandoffSpec is the Claude writer's shape (#1585): one
// read-only identity-bound credential mount with a lease claim.
func testReadHeldHandoffSpec(runID string, holder domain.InvocationID) HandoffSpec {
	hs := testLeasedHandoffSpec()
	hs.RunID = runID
	hs.Agent.CredentialMounts[0].Writable = false
	hs.AuthStoreLease.Holder = holder
	return hs
}

// TestHandoffReadHeldConcurrentHoldersShareIdentity is the #1585
// acceptance: two read-held handoffs on one identity, with different
// holders, both complete and release their holds, and neither takes the
// exclusive lease. The second handoff runs to completion inside the first
// one's pre-start re-verify, so both windows are provably live at once.
func TestHandoffReadHeldConcurrentHoldersShareIdentity(t *testing.T) {
	fx := newHandoffFixture(t)
	j := fx.journalled()
	_, l := fx.leased(t)
	b := fx.backend(t)
	first := testReadHeldHandoffSpec("run-read-a", "inv-a")
	second := testReadHeldHandoffSpec("run-read-b", "inv-b")

	var (
		secondResult *HandoffResult
		secondErr    error
		overlapped   bool
	)
	l.onGetRead = func(current domain.AuthStoreReadHold) (domain.AuthStoreReadHold, error) {
		if current.Holder == "inv-a" && !overlapped {
			overlapped = true
			if n := l.liveReadHolds(); n != 1 {
				t.Errorf("live read holds before the second handoff = %d, want 1", n)
			}
			secondResult, secondErr = b.Handoff(context.Background(), second)
		}
		return current, nil
	}

	res, err := b.Handoff(context.Background(), first)
	if err != nil {
		t.Fatalf("first Handoff = %v, want success", err)
	}
	if !overlapped {
		t.Fatal("the second handoff never ran inside the first one's window")
	}
	if secondErr != nil {
		t.Fatalf("second Handoff = %v, want success beside the first read hold", secondErr)
	}
	for _, r := range []*HandoffResult{res, secondResult} {
		if !r.AuthStore.Leased || !r.AuthStore.Shared || r.AuthStore.Fence != 0 {
			t.Errorf("AuthStore = %+v, want a shared read window with no fence", r.AuthStore)
		}
	}
	if n := l.liveReadHolds(); n != 0 {
		t.Errorf("live read holds after both handoffs = %d, want 0", n)
	}
	for _, call := range []string{"lease-acquire identity-fixture", "journal-begin-leased " + first.RunID} {
		if i := fx.rt.callIndex(call); i != -1 {
			t.Errorf("a read-held handoff took the exclusive lease (%q at call %d)", call, i)
		}
	}
	begin := fx.rt.callIndex("journal-begin-read-held " + first.RunID)
	verify := fx.rt.callIndex("read-get identity-fixture")
	start := fx.rt.callIndex("start-container " + namesFor(first.RunID).Agent)
	if begin == -1 || verify == -1 || start == -1 || begin >= verify || verify >= start {
		t.Errorf("call order begin=%d read-get=%d start=%d, want the hold opened and re-verified before the writer", begin, verify, start)
	}
	for _, runID := range []string{first.RunID, second.RunID} {
		rec := j.snapshot(runID)
		if rec == nil || rec.ReadHold == nil || rec.Lease != nil ||
			rec.Outcome == nil || *rec.Outcome != HandoffCompleted {
			t.Errorf("journal record for %s = %+v, want a completed read-held record", runID, rec)
		}
	}
}

// TestHandoffReadHeldSlotModes pins the in-process slot's rule, which
// mirrors the store's: read slots with distinct holders share an identity,
// the same holder cannot open two, and the exclusive slot excludes readers
// in both directions.
func TestHandoffReadHeldSlotModes(t *testing.T) {
	fx := newHandoffFixture(t)
	_, l := fx.leased(t)
	b := fx.backend(t)
	ctx := context.Background()
	readA := AuthStoreLeaseClaim{AuthIdentityID: "identity-fixture", Holder: "inv-a"}
	readB := AuthStoreLeaseClaim{AuthIdentityID: "identity-fixture", Holder: "inv-b"}

	a := &runState{sharedWindow: true}
	if err := b.acquireAuthStoreReadHold(ctx, readA, a); err != nil {
		t.Fatalf("read A = %v, want nil", err)
	}
	bState := &runState{sharedWindow: true}
	if err := b.acquireAuthStoreReadHold(ctx, readB, bState); err != nil {
		t.Fatalf("read B beside A = %v, want nil", err)
	}
	wantCheckFailure(t, b.acquireAuthStoreReadHold(ctx, readA, &runState{sharedWindow: true}),
		CheckAuthStoreMutationLease)
	wantCheckFailure(t, b.acquireAuthStoreLease(ctx, readA, &runState{}), CheckAuthStoreMutationLease)

	for _, st := range []*runState{a, bState} {
		if problems := b.releaseAuthStoreLease(ctx, st); len(problems) > 0 {
			t.Fatalf("release = %v", problems)
		}
		b.freeLeaseSlot(st)
	}
	exclusive := &runState{}
	if err := b.acquireAuthStoreLease(ctx, readA, exclusive); err != nil {
		t.Fatalf("exclusive after readers left = %v, want nil", err)
	}
	wantCheckFailure(t, b.acquireAuthStoreReadHold(ctx, readB, &runState{sharedWindow: true}),
		CheckAuthStoreMutationLease)
	if !l.lease.HeldAt(fx.cfg.Now()) {
		t.Fatal("exclusive lease is not held")
	}
}

// TestHandoffReadHeldRefusalStopsRun: a store refusal (a live mutation
// lease) stops the run before any runtime object, with its cause reachable.
func TestHandoffReadHeldRefusalStopsRun(t *testing.T) {
	errMutating := errors.New("fixture: mutation lease is live")
	fx := newHandoffFixture(t)
	_, l := fx.leased(t)
	l.onAcquireRead = func(domain.AuthIdentityID, domain.InvocationID, time.Time, time.Time) (domain.AuthStoreReadHold, error) {
		return domain.AuthStoreReadHold{}, errMutating
	}
	_, err := fx.runSpec(t, testReadHeldHandoffSpec("run-read-refused", "inv-a"))
	if !errors.Is(err, errMutating) {
		t.Fatalf("Handoff = %v, want the store refusal", err)
	}
	for _, call := range fx.rt.calls {
		if call == "create-volume "+namesFor("run-read-refused").Workspace {
			t.Fatal("a runtime object was created after the read hold was refused")
		}
	}
}

// TestHandoffReadHeldChangedBeforeStartRefused: a read window released or
// replaced before the writer starts is refused, like a lease takeover.
func TestHandoffReadHeldChangedBeforeStartRefused(t *testing.T) {
	fx := newHandoffFixture(t)
	_, l := fx.leased(t)
	l.onGetRead = func(current domain.AuthStoreReadHold) (domain.AuthStoreReadHold, error) {
		current.AcquiredAt = current.AcquiredAt.Add(-time.Minute)
		return current, nil
	}
	hs := testReadHeldHandoffSpec("run-read-moved", "inv-a")
	_, err := fx.runSpec(t, hs)
	wantCheckFailure(t, err, CheckAuthStoreMutationLease)
	if i := fx.rt.callIndex("start-container " + namesFor(hs.RunID).Agent); i != -1 {
		t.Errorf("writer started at %d over a changed read window", i)
	}
	if n := l.liveReadHolds(); n != 0 {
		t.Errorf("live read holds after the refusal = %d, want 0", n)
	}
}

// exclusiveOnlyLeaser hides the read-hold seam, modeling a leaser built only
// for writable mounts.
type exclusiveOnlyLeaser struct{ AuthStoreLeaser }

// TestHandoffReadHeldWithoutSeamFailsClosed: a read-only claim never runs
// with no window at all when the leaser cannot take read holds.
func TestHandoffReadHeldWithoutSeamFailsClosed(t *testing.T) {
	fx := newHandoffFixture(t)
	_, l := fx.leased(t)
	fx.cfg.AuthStoreLeaser = exclusiveOnlyLeaser{l}
	_, err := fx.runSpec(t, testReadHeldHandoffSpec("run-read-noseam", "inv-a"))
	wantCheckFailure(t, err, CheckAuthStoreMutationLease)
}

// TestHandoffReadHeldReleaseFailureFailsGate: a release refusal other than
// an ended window is a teardown problem, as for the lease.
func TestHandoffReadHeldReleaseFailureFailsGate(t *testing.T) {
	fx := newHandoffFixture(t)
	_, l := fx.leased(t)
	l.onReleaseRead = func(domain.AuthIdentityID, domain.InvocationID, time.Time, time.Time) error {
		return errors.New("fixture: store unavailable")
	}
	_, err := fx.runSpec(t, testReadHeldHandoffSpec("run-read-release", "inv-a"))
	wantCheckFailure(t, err, CheckTeardown)
}

// TestRecoverReadHeldRecordReleasesItsWindow: a crashed read-held run
// recovers through the read-hold re-gate and releases its exact window after
// the audit.
func TestRecoverReadHeldRecordReleasesItsWindow(t *testing.T) {
	fx := newRecoveryFixture(t)
	fx.j.leaser = fx.l
	hs := testReadHeldHandoffSpec("run-read-recover", "inv-a")
	digest, err := specDigest(hs)
	if err != nil {
		t.Fatal(err)
	}
	opened := time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC)
	hold, err := fx.l.grantRead("identity-fixture", "inv-a", opened, opened.Add(100*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	fx.j.put(HandoffJournalRecord{
		RunID: hs.RunID, OwnershipToken: testRecoveryToken, SpecDigest: digest, OpenedAt: opened,
		ReadHold: &HandoffJournalReadHold{
			AuthIdentityID: hold.AuthIdentityID, Holder: hold.Holder,
			AcquiredAt: hold.AcquiredAt, ExpiresAt: hold.ExpiresAt,
		},
	})

	res, err := fx.recover(t, hs.RunID, hs)
	if err != nil {
		t.Fatalf("Recover = %v, want committed loss", err)
	}
	if res.Outcome != RecoveryLoss {
		t.Fatalf("Outcome = %q, want loss", res.Outcome)
	}
	if n := fx.l.liveReadHolds(); n != 0 {
		t.Fatalf("live read holds after recovery = %d, want 0", n)
	}
	fx.wantClosed(t, hs.RunID, HandoffLoss)
}

// TestRecoverReadHeldWriterCompleteReportsWindow: an adopted read-held run
// reports the recorded window's bounds, as a live handoff does, so a
// recovered run keeps its audit window.
func TestRecoverReadHeldWriterCompleteReportsWindow(t *testing.T) {
	fx := newRecoveryFixture(t)
	fx.j.leaser = fx.l
	hs := testReadHeldHandoffSpec("run-read-adopt", "inv-a")
	opened := time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC)
	hold, err := fx.l.grantRead("identity-fixture", "inv-a", opened, opened.Add(100*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	fx.putReadHeldRecord(t, hs, hold)
	if err := fx.j.MarkCredentialObserved(context.Background(), hs.RunID, strings.Repeat("ab", 32)); err != nil {
		t.Fatal(err)
	}
	if err := fx.j.MarkWriterComplete(context.Background(), hs.RunID); err != nil {
		t.Fatal(err)
	}
	fx.worldVolume(t, namesFor(hs.RunID).Workspace, fx.runLabels(hs.RunID))

	res, err := fx.recover(t, hs.RunID, hs)
	if err != nil {
		t.Fatalf("Recover = %v, want adoption", err)
	}
	if res.Outcome != RecoveryExported || !res.AuthStore.Shared {
		t.Fatalf("result = %+v, want a read-held export", res)
	}
	if !res.AuthStore.AcquiredAt.Equal(hold.AcquiredAt) || !res.AuthStore.ExpiresAt.Equal(hold.ExpiresAt) {
		t.Errorf("recovered window = [%v, %v], want the recorded [%v, %v]",
			res.AuthStore.AcquiredAt, res.AuthStore.ExpiresAt, hold.AcquiredAt, hold.ExpiresAt)
	}
	fx.wantClosed(t, hs.RunID, HandoffCompleted)
}

// TestRecoverPreUpgradeLeasedReadOnlyRecord: a Claude run journalled before
// #1585 holds the exclusive lease for a read-only mount. Recovery must still
// finish it on the lease path, or the run could never recover.
func TestRecoverPreUpgradeLeasedReadOnlyRecord(t *testing.T) {
	fx := newRecoveryFixture(t)
	hs := testReadHeldHandoffSpec("run-read-legacy", "holder-fixture")
	rec := fx.openRecord(t, hs)
	fx.l.lease = domain.AuthStoreMutationLease{
		AuthIdentityID: rec.Lease.AuthIdentityID, Holder: rec.Lease.Holder, Fence: rec.Lease.Fence,
		AcquiredAt: rec.Lease.AcquiredAt, ExpiresAt: rec.Lease.ExpiresAt,
	}

	res, err := fx.recover(t, hs.RunID, hs)
	if err != nil {
		t.Fatalf("Recover = %v, want committed loss", err)
	}
	if res.Outcome != RecoveryLoss {
		t.Fatalf("Outcome = %q, want loss", res.Outcome)
	}
	if !fx.l.released {
		t.Fatal("the pre-upgrade exclusive lease was not released")
	}
	fx.wantClosed(t, hs.RunID, HandoffLoss)
}

// TestRecoverReadHoldForWritableMountRefused: a read hold can only have
// been taken for a read-only mount; a record pairing one with a writable
// spec is damaged.
func TestRecoverReadHoldForWritableMountRefused(t *testing.T) {
	fx := newRecoveryFixture(t)
	hs := testLeasedHandoffSpec()
	digest, err := specDigest(hs)
	if err != nil {
		t.Fatal(err)
	}
	opened := time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC)
	fx.j.put(HandoffJournalRecord{
		RunID: hs.RunID, OwnershipToken: testRecoveryToken, SpecDigest: digest, OpenedAt: opened,
		ReadHold: &HandoffJournalReadHold{
			AuthIdentityID: hs.AuthStoreLease.AuthIdentityID, Holder: hs.AuthStoreLease.Holder,
			AcquiredAt: opened, ExpiresAt: opened.Add(time.Hour),
		},
	})
	if _, err := fx.recover(t, hs.RunID, hs); !errors.Is(err, ErrInvalidJournalRecord) {
		t.Fatalf("Recover = %v, want %v", err, ErrInvalidJournalRecord)
	}
}

// putReadHeldRecord journals an open read-held record for hs whose window is
// recorded, and returns it.
func (fx *recoveryFixture) putReadHeldRecord(t *testing.T, hs HandoffSpec, recorded domain.AuthStoreReadHold) HandoffJournalRecord {
	t.Helper()
	digest, err := specDigest(hs)
	if err != nil {
		t.Fatal(err)
	}
	rec := HandoffJournalRecord{
		RunID: hs.RunID, OwnershipToken: testRecoveryToken, SpecDigest: digest, OpenedAt: recorded.AcquiredAt,
		ReadHold: &HandoffJournalReadHold{
			AuthIdentityID: recorded.AuthIdentityID, Holder: recorded.Holder,
			AcquiredAt: recorded.AcquiredAt, ExpiresAt: recorded.ExpiresAt,
		},
	}
	fx.j.put(rec)
	return rec
}

// TestRecoverReadHeldLaterWindowNeverReleased: the holder's row now carries
// a later live window than the record's. It is not this run's window, so
// recovery commits its loss without releasing it.
func TestRecoverReadHeldLaterWindowNeverReleased(t *testing.T) {
	fx := newRecoveryFixture(t)
	hs := testReadHeldHandoffSpec("run-read-later", "inv-a")
	opened := time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC)
	fx.putReadHeldRecord(t, hs, domain.AuthStoreReadHold{
		AuthIdentityID: "identity-fixture", Holder: "inv-a", AcquiredAt: opened, ExpiresAt: opened.Add(100 * time.Hour),
	})
	later := opened.Add(30 * time.Minute)
	if _, err := fx.l.grantRead("identity-fixture", "inv-a", later, later.Add(100*time.Hour)); err != nil {
		t.Fatal(err)
	}

	res, err := fx.recover(t, hs.RunID, hs)
	if err != nil {
		t.Fatalf("Recover = %v, want committed loss", err)
	}
	if res.Outcome != RecoveryLoss {
		t.Fatalf("Outcome = %q, want loss", res.Outcome)
	}
	if fx.rt.callIndex("read-release identity-fixture") != -1 {
		t.Fatal("recovery released a read window opened after its own record")
	}
	if n := fx.l.liveReadHolds(); n != 1 {
		t.Fatalf("live read holds = %d, want the later window kept", n)
	}
	fx.wantClosed(t, hs.RunID, HandoffLoss)
}

// TestRecoverReadHeldIncoherentRowFailsClosed: a re-gate row naming another
// holder is an incoherent store, so recovery errors retryably and leaves the
// record open.
func TestRecoverReadHeldIncoherentRowFailsClosed(t *testing.T) {
	fx := newRecoveryFixture(t)
	hs := testReadHeldHandoffSpec("run-read-incoherent", "inv-a")
	opened := time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC)
	recorded := domain.AuthStoreReadHold{
		AuthIdentityID: "identity-fixture", Holder: "inv-a", AcquiredAt: opened, ExpiresAt: opened.Add(100 * time.Hour),
	}
	fx.putReadHeldRecord(t, hs, recorded)
	fx.l.onGetRead = func(current domain.AuthStoreReadHold) (domain.AuthStoreReadHold, error) {
		current.Holder = "inv-other"
		return current, nil
	}
	if _, err := fx.l.grantRead("identity-fixture", "inv-a", opened, recorded.ExpiresAt); err != nil {
		t.Fatal(err)
	}

	if _, err := fx.recover(t, hs.RunID, hs); err == nil {
		t.Fatal("Recover = nil, want the incoherent row refused")
	}
	if fx.rt.callIndex("read-release identity-fixture") != -1 {
		t.Fatal("recovery released a window through an incoherent row")
	}
	if rec := fx.j.snapshot(hs.RunID); rec == nil || rec.Outcome != nil {
		t.Fatalf("journal record = %+v, want it left open for a retry", rec)
	}
}
