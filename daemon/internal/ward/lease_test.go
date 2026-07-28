package ward

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// fakeLeaser is a scripted AuthStoreLeaser. Its default behaviour grants
// exactly what is asked (fence 1) and accepts a matching release; per-method
// overrides script refusals and takeovers. It records its calls into the
// fake runtime's shared call log so tests can assert ordering against
// runtime operations.
type fakeLeaser struct {
	rt       *fakeRuntime
	lease    domain.AuthStoreMutationLease
	released bool
	// releaseCtxErr records the context state Release observed, so tests can
	// prove the release ran detached from an already-cancelled run context.
	releaseCtxErr error

	onAcquire func(id domain.AuthIdentityID, holder domain.InvocationID, now, expiresAt time.Time) (domain.AuthStoreMutationLease, error)
	onGet     func(current domain.AuthStoreMutationLease) (domain.AuthStoreMutationLease, error)
	onRelease func(id domain.AuthIdentityID, holder domain.InvocationID, fence int64, releasedAt time.Time) error
}

func (l *fakeLeaser) recordCall(s string) {
	l.rt.mu.Lock()
	defer l.rt.mu.Unlock()
	l.rt.calls = append(l.rt.calls, s)
}

func (l *fakeLeaser) Acquire(_ context.Context, id domain.AuthIdentityID, holder domain.InvocationID,
	now, expiresAt time.Time,
) (domain.AuthStoreMutationLease, error) {
	l.recordCall("lease-acquire " + string(id))
	if l.onAcquire != nil {
		return l.onAcquire(id, holder, now, expiresAt)
	}
	l.lease = domain.AuthStoreMutationLease{
		AuthIdentityID: id, Holder: holder, Fence: 1,
		AcquiredAt: now, ExpiresAt: expiresAt,
	}
	return l.lease, nil
}

func (l *fakeLeaser) Get(_ context.Context, id domain.AuthIdentityID) (domain.AuthStoreMutationLease, error) {
	l.recordCall("lease-get " + string(id))
	if l.onGet != nil {
		return l.onGet(l.lease)
	}
	return l.lease, nil
}

func (l *fakeLeaser) Release(ctx context.Context, id domain.AuthIdentityID, holder domain.InvocationID,
	fence int64, releasedAt time.Time,
) error {
	l.recordCall("lease-release " + string(id))
	l.releaseCtxErr = ctx.Err()
	if l.onRelease != nil {
		return l.onRelease(id, holder, fence, releasedAt)
	}
	if id != l.lease.AuthIdentityID || holder != l.lease.Holder || fence != l.lease.Fence {
		return errors.New("fake leaser: caller does not hold the lease")
	}
	l.released = true
	return nil
}

// leased wires a fixed clock and a fake leaser into the fixture and returns
// the leased spec.
func (fx *handoffFixture) leased(t *testing.T) (HandoffSpec, *fakeLeaser) {
	t.Helper()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	fx.cfg.Now = func() time.Time { return base }
	l := &fakeLeaser{rt: fx.rt}
	fx.cfg.AuthStoreLeaser = l
	return testLeasedHandoffSpec(), l
}

func (fx *handoffFixture) runSpec(t *testing.T, hs HandoffSpec) (*HandoffResult, error) {
	t.Helper()
	res, err := fx.backend(t).Handoff(context.Background(), hs)
	if res != nil {
		t.Cleanup(func() { _ = os.RemoveAll(res.ExportDir) })
	}
	return res, err
}

// TestHandoffLeasedLifecycle pins the §5.4 window around the writer's whole
// runtime: acquired before any runtime object exists, re-verified after the
// last pre-start inspection, and released only after the writer is provably
// gone.
func TestHandoffLeasedLifecycle(t *testing.T) {
	fx := newHandoffFixture(t)
	hs, l := fx.leased(t)
	names := namesFor(hs.RunID)

	if _, err := fx.runSpec(t, hs); err != nil {
		t.Fatalf("Handoff = %v, want success", err)
	}
	if !l.released {
		t.Error("lease was not released")
	}
	acquire := fx.rt.callIndex("lease-acquire identity-fixture")
	get := fx.rt.callIndex("lease-get identity-fixture")
	release := fx.rt.callIndex("lease-release identity-fixture")
	firstObject := fx.rt.callIndex("create-volume " + names.Workspace)
	start := fx.rt.callIndex("start-container " + names.Agent)
	switch {
	case acquire == -1 || get == -1 || release == -1:
		t.Fatalf("lease calls missing: acquire=%d get=%d release=%d", acquire, get, release)
	case firstObject != -1 && acquire > firstObject:
		t.Errorf("lease acquired at %d, after the first runtime object at %d", acquire, firstObject)
	case start != -1 && get > start:
		t.Errorf("lease re-verified at %d, after the writer started at %d", get, start)
	case release < start:
		t.Errorf("lease released at %d, before the writer started at %d", release, start)
	}
	fx.assertReaped(t)
}

// TestHandoffLeasedNilLeaserFailsClosed: a leased spec under a backend with
// no leaser refuses before any runtime object exists.
func TestHandoffLeasedNilLeaserFailsClosed(t *testing.T) {
	fx := newHandoffFixture(t)
	hs, _ := fx.leased(t)
	fx.cfg.AuthStoreLeaser = nil

	_, err := fx.runSpec(t, hs)
	wantCheckFailure(t, err, CheckAuthStoreMutationLease)
	if n := len(fx.rt.calls); n != 0 {
		t.Errorf("runtime saw %d calls before the lease refusal; want none (calls: %v)", n, fx.rt.calls)
	}
}

// TestHandoffLeasedAcquireRefusalStopsRun: a store refusal (e.g. a live
// lease held elsewhere) stops the run before any runtime object and keeps
// its typed cause reachable.
func TestHandoffLeasedAcquireRefusalStopsRun(t *testing.T) {
	errHeld := errors.New("fixture: lease held by another holder")
	fx := newHandoffFixture(t)
	hs, l := fx.leased(t)
	l.onAcquire = func(domain.AuthIdentityID, domain.InvocationID, time.Time, time.Time) (domain.AuthStoreMutationLease, error) {
		return domain.AuthStoreMutationLease{}, errHeld
	}

	_, err := fx.runSpec(t, hs)
	if !errors.Is(err, errHeld) {
		t.Fatalf("Handoff error = %v, want the store refusal reachable via errors.Is", err)
	}
	if errors.Is(err, ErrConformance) {
		t.Error("store refusal was converted into a conformance failure; it must stay an operational error")
	}
	for _, c := range fx.rt.calls {
		if c == "lease-acquire identity-fixture" {
			continue
		}
		t.Errorf("runtime call %q happened despite the acquire refusal", c)
	}
}

// TestHandoffLeasedShortWindowRefused: a granted window that ends before the
// run's budget is refused up front. A same-holder re-acquire converges on an
// existing window without extending it, so this is the reused-holder case as
// well as a misbehaving adapter.
func TestHandoffLeasedShortWindowRefused(t *testing.T) {
	fx := newHandoffFixture(t)
	hs, l := fx.leased(t)
	names := namesFor(hs.RunID)
	l.onAcquire = func(id domain.AuthIdentityID, holder domain.InvocationID, now, _ time.Time) (domain.AuthStoreMutationLease, error) {
		return domain.AuthStoreMutationLease{
			AuthIdentityID: id, Holder: holder, Fence: 1,
			AcquiredAt: now, ExpiresAt: now.Add(time.Second),
		}, nil
	}

	_, err := fx.runSpec(t, hs)
	wantCheckFailure(t, err, CheckAuthStoreMutationLease)
	if i := fx.rt.callIndex("create-volume " + names.Workspace); i != -1 {
		t.Error("workspace volume was created despite the short lease window")
	}
	// The short window is provably this run's (fresh at acquisition), so
	// the refusal releases it rather than abandoning it held to expiry.
	if fx.rt.callIndex("lease-release identity-fixture") == -1 {
		t.Error("the refused short window was never released")
	}
}

// TestHandoffLeasedConvergedWindowRefused: a same-holder re-acquire converges
// on a still-live window this run did not open (a crashed run's, or a
// concurrent run under a reused holder ID). The gate refuses to ride it:
// recovery or expiry ends the old window first.
func TestHandoffLeasedConvergedWindowRefused(t *testing.T) {
	fx := newHandoffFixture(t)
	hs, l := fx.leased(t)
	names := namesFor(hs.RunID)
	l.onAcquire = func(id domain.AuthIdentityID, holder domain.InvocationID, now, expiresAt time.Time) (domain.AuthStoreMutationLease, error) {
		return domain.AuthStoreMutationLease{
			AuthIdentityID: id, Holder: holder, Fence: 1,
			AcquiredAt: now.Add(-time.Minute), ExpiresAt: expiresAt.Add(time.Hour),
		}, nil
	}

	_, err := fx.runSpec(t, hs)
	wantCheckFailure(t, err, CheckAuthStoreMutationLease)
	if fx.rt.callIndex("create-volume "+names.Workspace) != -1 {
		t.Error("workspace volume was created despite the converged window")
	}
}

// TestHandoffLeasedInProcessSlotSerializesIdentity: two concurrent leased
// runs on one identity in one process cannot both hold the window, even when
// a reused holder ID would make the store converge them: the backend's
// per-identity slot refuses the second and frees when the run ends.
func TestHandoffLeasedInProcessSlotSerializesIdentity(t *testing.T) {
	fx := newHandoffFixture(t)
	_, l := fx.leased(t)
	_ = l
	b := fx.backend(t)
	claim := AuthStoreLeaseClaim{AuthIdentityID: "identity-fixture", Holder: "holder-fixture"}
	ctx := context.Background()

	first := &runState{}
	if err := b.acquireAuthStoreLease(ctx, claim, first); err != nil {
		t.Fatalf("first acquire = %v, want nil", err)
	}
	second := &runState{}
	err := b.acquireAuthStoreLease(ctx, claim, second)
	wantCheckFailure(t, err, CheckAuthStoreMutationLease)

	b.freeLeaseSlot(first)
	third := &runState{}
	if err := b.acquireAuthStoreLease(ctx, claim, third); err != nil {
		t.Fatalf("acquire after the slot freed = %v, want nil", err)
	}
}

// TestHandoffLeasedWrongIdentityRefused: an adapter answering with some other
// identity's lease is store output, not gate state; it is refused rather
// than trusted.
func TestHandoffLeasedWrongIdentityRefused(t *testing.T) {
	fx := newHandoffFixture(t)
	hs, l := fx.leased(t)
	l.onAcquire = func(_ domain.AuthIdentityID, holder domain.InvocationID, now, expiresAt time.Time) (domain.AuthStoreMutationLease, error) {
		return domain.AuthStoreMutationLease{
			AuthIdentityID: "other-identity", Holder: holder, Fence: 1,
			AcquiredAt: now, ExpiresAt: expiresAt,
		}, nil
	}
	_, err := fx.runSpec(t, hs)
	wantCheckFailure(t, err, CheckAuthStoreMutationLease)
}

// TestHandoffLeasedTakeoverRefusesWriterStart: a bumped fence observed at the
// pre-start re-verification means another holder owns the window; the
// credential-bearing writer must never start.
func TestHandoffLeasedTakeoverRefusesWriterStart(t *testing.T) {
	fx := newHandoffFixture(t)
	hs, l := fx.leased(t)
	names := namesFor(hs.RunID)
	l.onGet = func(current domain.AuthStoreMutationLease) (domain.AuthStoreMutationLease, error) {
		current.Fence++
		current.Holder = "usurper"
		return current, nil
	}

	_, err := fx.runSpec(t, hs)
	wantCheckFailure(t, err, CheckAuthStoreMutationLease)
	if fx.rt.callIndex("start-container "+names.Agent) != -1 {
		t.Fatal("writer started after the lease was taken over")
	}
	fx.assertReaped(t)
}

// TestHandoffLeasedReVerifyRowRegated: the pre-start re-read crosses the
// same trust boundary as acquisition, so a row that would pass the old
// holder-and-fence check but is malformed, names another identity, or
// carries a moved window must refuse the writer start.
func TestHandoffLeasedReVerifyRowRegated(t *testing.T) {
	cases := map[string]func(domain.AuthStoreMutationLease) domain.AuthStoreMutationLease{
		"malformed": func(cur domain.AuthStoreMutationLease) domain.AuthStoreMutationLease {
			cur.AcquiredAt = time.Time{}
			return cur
		},
		"other-identity": func(cur domain.AuthStoreMutationLease) domain.AuthStoreMutationLease {
			cur.AuthIdentityID = "other-identity"
			return cur
		},
		"moved-window": func(cur domain.AuthStoreMutationLease) domain.AuthStoreMutationLease {
			cur.AcquiredAt = cur.AcquiredAt.Add(-time.Hour)
			return cur
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			fx := newHandoffFixture(t)
			hs, l := fx.leased(t)
			names := namesFor(hs.RunID)
			l.onGet = func(current domain.AuthStoreMutationLease) (domain.AuthStoreMutationLease, error) {
				return mutate(current), nil
			}

			_, err := fx.runSpec(t, hs)
			wantCheckFailure(t, err, CheckAuthStoreMutationLease)
			if fx.rt.callIndex("start-container "+names.Agent) != -1 {
				t.Fatal("writer started over an untrusted re-read lease row")
			}
			fx.assertReaped(t)
		})
	}
}

// TestHandoffLeasedExpiredBeforeStartRefused: a window observed lapsed at the
// pre-start re-verification refuses the start even when holder and fence
// still match (an incoherent store row, not an expected lapse — the window
// was sized past the budget).
func TestHandoffLeasedExpiredBeforeStartRefused(t *testing.T) {
	fx := newHandoffFixture(t)
	hs, l := fx.leased(t)
	names := namesFor(hs.RunID)
	l.onGet = func(current domain.AuthStoreMutationLease) (domain.AuthStoreMutationLease, error) {
		released := current.AcquiredAt
		current.ReleasedAt = &released
		return current, nil
	}

	_, err := fx.runSpec(t, hs)
	wantCheckFailure(t, err, CheckAuthStoreMutationLease)
	if fx.rt.callIndex("start-container "+names.Agent) != -1 {
		t.Fatal("writer started under a lapsed lease")
	}
	fx.assertReaped(t)
}

// TestReleaseAuthStoreLeaseDetachesFromCancelledContext: the one call that
// ends the window must reach the store even when the run's context is
// already cancelled — a window outliving the run blocks the identity until
// expiry.
func TestReleaseAuthStoreLeaseDetachesFromCancelledContext(t *testing.T) {
	fx := newHandoffFixture(t)
	_, l := fx.leased(t)
	b := fx.backend(t)
	l.lease = domain.AuthStoreMutationLease{
		AuthIdentityID: "identity-fixture", Holder: "holder-fixture", Fence: 1,
		AcquiredAt: time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC),
		ExpiresAt:  time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC),
	}
	st := &runState{leaseHeld: true, lease: l.lease}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if problems := b.releaseAuthStoreLease(ctx, st); len(problems) != 0 {
		t.Fatalf("release under a cancelled context reported %v", problems)
	}
	if !l.released {
		t.Fatal("release never reached the store")
	}
	if l.releaseCtxErr != nil {
		t.Fatalf("Release observed context error %v; the release must run detached", l.releaseCtxErr)
	}
}

// TestHandoffLeasedReleaseFailureFailsGate: teardown's release failure is a
// teardown problem — the identity stays serialized until expiry, and the gate
// must say so rather than deliver a result that looks fully cleaned up.
func TestHandoffLeasedReleaseFailureFailsGate(t *testing.T) {
	fx := newHandoffFixture(t)
	hs, l := fx.leased(t)
	l.onRelease = func(domain.AuthIdentityID, domain.InvocationID, int64, time.Time) error {
		return errors.New("fixture: release refused")
	}

	res, err := fx.runSpec(t, hs)
	wantCheckFailure(t, err, CheckTeardown)
	if res != nil {
		t.Error("release failure still delivered a result")
	}
	fx.assertReaped(t)
}

// TestHandoffLeasedEarlyRefusalStillReleases: a refusal after acquisition but
// before the first runtime object (here: a preflight spec violation) must not
// leave the identity serialized until expiry.
func TestHandoffLeasedEarlyRefusalStillReleases(t *testing.T) {
	fx := newHandoffFixture(t)
	hs, l := fx.leased(t)
	// Force a preflight refusal after the acquire: an extra read-only mount
	// colliding with the workspace target fails validateAgentSpec.
	hs.Agent.CredentialMounts = append(hs.Agent.CredentialMounts,
		CredentialMount{Volume: "collide", Target: fx.cfg.withDefaults().WorkspaceTarget})

	_, err := fx.runSpec(t, hs)
	if err == nil {
		t.Fatal("Handoff succeeded; fixture meant to fail preflight")
	}
	if !l.released {
		t.Error("lease survived an early preflight refusal")
	}
}

// TestHandoffLeasedMutationAttested: the writer mutates its store inside the
// window, and the result attests it — digests differ, Mutated is true, and
// the record carries the lease's identity, holder, fence, and window.
func TestHandoffLeasedMutationAttested(t *testing.T) {
	fx := newHandoffFixture(t)
	hs, _ := fx.leased(t)
	names := namesFor(hs.RunID)
	vol := hs.Agent.CredentialMounts[0].Volume
	fx.rt.onStart = func(id string) error {
		if id == names.Agent {
			fx.rt.credState[vol] = "mutated-by-writer"
		}
		return nil
	}

	res, err := fx.runSpec(t, hs)
	if err != nil {
		t.Fatalf("Handoff = %v, want success", err)
	}
	obs := res.AuthStore
	if !obs.Leased || !obs.Mutated {
		t.Errorf("AuthStore = %+v, want Leased and Mutated", obs)
	}
	if obs.PreDigest == obs.PostDigest {
		t.Error("digests are equal despite the mutation")
	}
	if !sha256HexPattern.MatchString(obs.PreDigest) || !sha256HexPattern.MatchString(obs.PostDigest) {
		t.Errorf("digests are not tree-digest shaped: pre=%q post=%q", obs.PreDigest, obs.PostDigest)
	}
	if obs.AuthIdentityID != "identity-fixture" || obs.Holder != "holder-fixture" || obs.Fence != 1 {
		t.Errorf("AuthStore identity fields = %+v, want the acquired lease's", obs)
	}
	fx.assertReaped(t)
}

// TestHandoffLeasedUnchangedStoreAttestedUnmutated: a leased writer that
// never touches its store yields equal digests and Mutated false — the
// observation reports what happened, not what the lease allowed.
func TestHandoffLeasedUnchangedStoreAttestedUnmutated(t *testing.T) {
	fx := newHandoffFixture(t)
	hs, _ := fx.leased(t)

	res, err := fx.runSpec(t, hs)
	if err != nil {
		t.Fatalf("Handoff = %v, want success", err)
	}
	obs := res.AuthStore
	if !obs.Leased || obs.Mutated {
		t.Errorf("AuthStore = %+v, want Leased and not Mutated", obs)
	}
	if obs.PreDigest != obs.PostDigest || obs.PreDigest == "" {
		t.Errorf("digests: pre=%q post=%q, want equal and non-empty", obs.PreDigest, obs.PostDigest)
	}
	fx.assertReaped(t)
}

// TestHandoffNonLeasedZeroAuthStoreObservation: a run with no lease claim
// reports the zero observation, distinguishable by Leased.
func TestHandoffNonLeasedZeroAuthStoreObservation(t *testing.T) {
	fx := newHandoffFixture(t)
	res, err := fx.run(t)
	if err != nil {
		t.Fatalf("Handoff = %v, want success", err)
	}
	if res.AuthStore != (AuthStoreObservation{}) {
		t.Errorf("AuthStore = %+v, want zero", res.AuthStore)
	}
	names := namesFor(testHandoffSpec().RunID)
	for _, id := range []string{names.CredObsPre, names.CredObsPost} {
		if fx.rt.callIndex("create-container "+id) != -1 {
			t.Errorf("credential observer %q ran on a non-leased handoff", id)
		}
	}
}

// TestHandoffLeasedTamperedCredProofRefused: a proof carrying a wrong nonce
// is a file anyone could have left; the observation is refused, categorically.
func TestHandoffLeasedTamperedCredProofRefused(t *testing.T) {
	fx := newHandoffFixture(t)
	hs, _ := fx.leased(t)
	names := namesFor(hs.RunID)
	fx.rt.observerProof = func(id string, proof []byte) []byte {
		if id == names.CredObsPre {
			return credProofFor("0000feedbeef0000feedbeef0000feed", credStateDigest("forged"))
		}
		return proof
	}

	_, err := fx.runSpec(t, hs)
	wantCheckFailure(t, err, CheckAuthStoreMutationLease)
	fx.assertReaped(t)
}

// TestHandoffLeasedRuntimeReadOnlyLeasedMountRefused: the runtime realizing
// the leased mount read-only would deliver a writer that cannot refresh; the
// mount-topology comparison catches the divergence before the writer starts.
func TestHandoffLeasedRuntimeReadOnlyLeasedMountRefused(t *testing.T) {
	fx := newHandoffFixture(t)
	hs, _ := fx.leased(t)
	names := namesFor(hs.RunID)
	target := hs.writableCredentialTarget()
	fx.rt.onInspect = func(id string, rep InspectReport) (InspectReport, error) {
		if id == names.Agent {
			for i, m := range rep.Mounts {
				if m.Target == target {
					rep.Mounts[i].ReadOnly = true
				}
			}
		}
		return rep, nil
	}

	_, err := fx.runSpec(t, hs)
	wantCheckFailure(t, err, CheckCredentialSeparation)
	if fx.rt.callIndex("start-container "+names.Agent) != -1 {
		t.Fatal("writer started with a diverged mount topology")
	}
	fx.assertReaped(t)
}

// TestHandoffLeasedWriterSurvivalKeepsLease: when teardown cannot prove the
// writer absent, releasing the window would invite a second holder to mutate
// beside a possibly-live writer. The lease stays held and the gate says so.
func TestHandoffLeasedWriterSurvivalKeepsLease(t *testing.T) {
	fx := newHandoffFixture(t)
	hs, l := fx.leased(t)
	names := namesFor(hs.RunID)
	// The writer refuses to stop: waitStopped times out mid-run, and
	// teardown's delete reports success while the survivor stays listed. The
	// running state is faked only once the writer actually started, so the
	// pre-start inspect-while-stopped gate still passes.
	started := false
	fx.rt.onStart = func(id string) error {
		if id == names.Agent {
			started = true
		}
		return nil
	}
	fx.rt.onStop = func(id string) error {
		if id == names.Agent {
			return errors.New("fixture: stop refused")
		}
		return nil
	}
	fx.rt.onInspect = func(id string, rep InspectReport) (InspectReport, error) {
		if id == names.Agent && started {
			rep.State = StateRunning
		}
		return rep, nil
	}
	fx.rt.onDeleteContainer = func(id string) (bool, error) {
		if id == names.Agent {
			return true, nil // lie: report success, keep the survivor
		}
		return false, nil
	}

	_, err := fx.runSpec(t, hs)
	// The mid-run stop timeout is the primary failure; teardown's kept-held
	// record joins it rather than replacing it.
	wantCheckFailure(t, err, CheckWriterTermination)
	if err == nil || !strings.Contains(err.Error(), "kept held") {
		t.Errorf("error does not record the kept-held lease: %v", err)
	}
	if l.released {
		t.Fatal("lease released while the writer's absence was unproven")
	}
	if fx.rt.callIndex("lease-release identity-fixture") != -1 {
		t.Error("release was attempted despite the surviving writer")
	}
}

// TestHandoffLeasedTargetCoveringProofPathRefused: a writable credential
// target overlapping the configured credential proof path in either
// direction fails every leased handoff mid-run — covering it shadows the
// proof write; nested beneath it, the proof path is forced to be a
// directory the observer's file redirect can never land on. Both are
// refused up front, before the lease or any object.
func TestHandoffLeasedTargetCoveringProofPathRefused(t *testing.T) {
	for _, target := range []string{"/handoff-cred.txt", "/handoff-cred.txt/store", "/handoff-cred.txt/nested-under-proof"} {
		t.Run(target, func(t *testing.T) {
			fx := newHandoffFixture(t)
			hs, _ := fx.leased(t)
			switch target {
			case "/handoff-cred.txt/store":
				// The covering direction: the proof path sits under the target.
				fx.cfg.CredProofPath = "/handoff-cred.txt/store/proof.txt"
				hs.Agent.CredentialMounts[0].Target = "/handoff-cred.txt/store"
			default:
				// Equal, and the inverse direction: the target sits under the
				// configured proof path.
				hs.Agent.CredentialMounts[0].Target = target
			}

			_, err := fx.runSpec(t, hs)
			if !errors.Is(err, ErrInvalidHandoffSpec) {
				t.Fatalf("Handoff = %v, want ErrInvalidHandoffSpec", err)
			}
			if len(fx.rt.calls) != 0 {
				t.Errorf("runtime saw calls before the refusal: %v", fx.rt.calls)
			}
			if fx.rt.callIndex("lease-acquire identity-fixture") != -1 {
				t.Error("lease acquired despite the covered proof path")
			}
		})
	}
}
