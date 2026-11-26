package daemoncompose

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/cordon"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

const (
	proofsHostB  domain.ExecutionHostID = "host-b"
	proofsDigest domain.Digest          = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	otherDigest  domain.Digest          = "sha256:4444444444444444444444444444444444444444444444444444444444444444"
)

// fakeHostSuite stands in for one host's cordoned suite: it records
// through its host's recorder as the real suite does, a superseding marker
// and then the outcome for a full pass, and a failure for a failed probe.
type fakeHostSuite struct {
	t                     *testing.T
	recorder              StoreConformanceRecorder
	digest                domain.Digest
	fullFails, probeFails bool
	fulls, probes         atomic.Int32
}

func (f *fakeHostSuite) record(ctx context.Context, outcome domain.ConformanceOutcome) {
	f.t.Helper()
	in := domain.BackendConformanceInput{
		Backend: domain.BackendCordonedHostProcess, Outcome: outcome,
		ConfigurationDigest: f.digest, ProvedAt: time.Now().UTC(),
	}
	if outcome == domain.ConformancePassed {
		in.Capabilities = domain.CordonedCapabilities()
	}
	record, err := domain.NewBackendConformance(in)
	if err != nil {
		f.t.Fatal(err)
	}
	// As the real suite does, a step is recorded past ctx's cancel.
	if err := f.recorder.RecordBackendConformance(context.WithoutCancel(ctx), record); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeHostSuite) Full(ctx context.Context) error {
	f.fulls.Add(1)
	f.record(ctx, domain.ConformanceSuperseded)
	if f.fullFails {
		f.record(ctx, domain.ConformanceFailed)
		return &cordon.ConformanceFailure{Check: "read", Reason: "the probe read it"}
	}
	f.record(ctx, domain.ConformancePassed)
	return nil
}

func (f *fakeHostSuite) ProbeBeforeJob(ctx context.Context) error {
	f.probes.Add(1)
	if f.probeFails {
		f.record(ctx, domain.ConformanceFailed)
		return &cordon.ConformanceFailure{Check: "write", Reason: "the probe wrote it"}
	}
	return nil
}

// twoHostProofs is a runner holding a suite for the daemon's host and one
// for host B, both on one configuration digest and both added, so each has
// run its first full pass.
func twoHostProofs(t *testing.T) (*store.Store, *cordonProofs, map[domain.ExecutionHostID]*fakeHostSuite) {
	t.Helper()
	ctx := t.Context()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	account := "runner"
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.AddExecutionHost(ctx, domain.ExecutionHost{
			ID: proofsHostB, Name: "studio",
			RunAccount: domain.HostRunAccount{Name: &account},
			Reach:      &domain.HostReach{Address: "studio.local", HostKey: "ssh-ed25519 AAAAstudio"},
			Conditions: []domain.HostObservedCondition{},
		})
	}); err != nil {
		t.Fatal(err)
	}
	proofs := newCordonProofs(st, nil)
	suites := map[domain.ExecutionHostID]*fakeHostSuite{}
	for _, host := range []domain.ExecutionHostID{domain.DaemonHostID, proofsHostB} {
		suite := &fakeHostSuite{t: t, recorder: proofs.Recorder(host), digest: proofsDigest}
		suites[host] = suite
		if err := proofs.Add(ctx, host, suite, func() domain.Digest { return suite.digest }); err != nil {
			t.Fatalf("add %s: %v", host, err)
		}
	}
	return st, proofs, suites
}

func latestCordoned(t *testing.T, st *store.Store, host domain.ExecutionHostID) domain.BackendConformance {
	t.Helper()
	var (
		record domain.BackendConformance
		found  bool
	)
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		record, found, err = tx.LatestBackendConformance(t.Context(), host, domain.BackendCordonedHostProcess)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("host %s has no cordoned record", host)
	}
	return record
}

func proofNotCurrent(t *testing.T, st *store.Store, host domain.ExecutionHostID) bool {
	t.Helper()
	var h domain.ExecutionHost
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		h, err = tx.ExecutionHost(t.Context(), host)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return slices.ContainsFunc(h.Conditions, func(o domain.HostObservedCondition) bool {
		return o.Condition == domain.ExecutionHostProofNotCurrent
	})
}

// TestCordonProofsProveEachHost is #2165's acceptance 5 for start and the
// schedule: each host takes a full pass when it joins and again on every
// scheduled pass, each pass is recorded against its own host, and a host
// whose condition doesn't change moves no revision.
func TestCordonProofsProveEachHost(t *testing.T) {
	t.Parallel()
	st, proofs, suites := twoHostProofs(t)
	for host, suite := range suites {
		if suite.fulls.Load() != 1 {
			t.Fatalf("host %s ran %d full passes when added, want 1", host, suite.fulls.Load())
		}
		if got := latestCordoned(t, st, host); got.Outcome != domain.ConformancePassed || got.Host != host {
			t.Fatalf("host %s's newest record = %+v, want its own pass", host, got)
		}
		if proofNotCurrent(t, st, host) {
			t.Fatalf("host %s carries %s after a pass", host, domain.ExecutionHostProofNotCurrent)
		}
	}

	changed := st.RevisionChanged()
	if err := proofs.RunScheduled(t.Context()); err != nil {
		t.Fatal(err)
	}
	for host, suite := range suites {
		if suite.fulls.Load() != 2 {
			t.Fatalf("host %s ran %d full passes after the schedule, want 2", host, suite.fulls.Load())
		}
	}
	select {
	case <-changed:
		t.Fatal("a scheduled pass that changed no condition moved the server revision")
	default:
	}
}

// TestCordonProofsFailedPassClosesItsHostAlone is acceptance 2's condition
// half: a failed pass sets the proof condition on its host and no other,
// and the next pass there clears it.
func TestCordonProofsFailedPassClosesItsHostAlone(t *testing.T) {
	t.Parallel()
	st, proofs, suites := twoHostProofs(t)

	suites[proofsHostB].fullFails = true
	if err := proofs.RunScheduled(t.Context()); err != nil {
		t.Fatalf("a failed pass = %v, want nil", err)
	}
	if !proofNotCurrent(t, st, proofsHostB) {
		t.Fatalf("host B's failed pass didn't set %s", domain.ExecutionHostProofNotCurrent)
	}
	if proofNotCurrent(t, st, domain.DaemonHostID) {
		t.Fatalf("host B's failed pass set %s on the daemon's host", domain.ExecutionHostProofNotCurrent)
	}

	suites[proofsHostB].fullFails = false
	if err := proofs.RunScheduled(t.Context()); err != nil {
		t.Fatal(err)
	}
	if proofNotCurrent(t, st, proofsHostB) {
		t.Fatalf("host B's next pass didn't clear %s", domain.ExecutionHostProofNotCurrent)
	}
}

// TestCordonProofsPreJobRerunsOnADigestChange is acceptance 5's digest
// half: a job reruns the full pass on its host when that host's
// configuration digest changed, and nowhere else.
func TestCordonProofsPreJobRerunsOnADigestChange(t *testing.T) {
	t.Parallel()
	st, proofs, suites := twoHostProofs(t)

	if err := proofs.PreJob(t.Context(), proofsHostB); err != nil {
		t.Fatal(err)
	}
	if suites[proofsHostB].fulls.Load() != 1 || suites[proofsHostB].probes.Load() != 1 {
		t.Fatalf("an unchanged host ran %d full passes and %d probes before a job, want 1 and 1",
			suites[proofsHostB].fulls.Load(), suites[proofsHostB].probes.Load())
	}

	suites[proofsHostB].digest = otherDigest
	if err := proofs.PreJob(t.Context(), proofsHostB); err != nil {
		t.Fatal(err)
	}
	if suites[proofsHostB].fulls.Load() != 2 {
		t.Fatalf("host B ran %d full passes after its digest changed, want 2", suites[proofsHostB].fulls.Load())
	}
	if got := latestCordoned(t, st, proofsHostB); got.ConfigurationDigest != otherDigest {
		t.Fatalf("host B's newest record is for %s, want the new digest", got.ConfigurationDigest)
	}
	if err := proofs.PreJob(t.Context(), domain.DaemonHostID); err != nil {
		t.Fatal(err)
	}
	if suites[domain.DaemonHostID].fulls.Load() != 1 {
		t.Fatalf("host B's digest change reran the daemon's host: %d full passes", suites[domain.DaemonHostID].fulls.Load())
	}
}

// TestCordonProofsRedProbeRefusesOnItsHost is acceptance 4: a red probe on
// host B refuses the job, sets the proof condition there, and leaves the
// daemon's host's records and conditions as they were.
func TestCordonProofsRedProbeRefusesOnItsHost(t *testing.T) {
	t.Parallel()
	st, proofs, suites := twoHostProofs(t)
	before := latestCordoned(t, st, domain.DaemonHostID)

	suites[proofsHostB].probeFails = true
	var failure *cordon.ConformanceFailure
	if err := proofs.PreJob(t.Context(), proofsHostB); !errors.As(err, &failure) {
		t.Fatalf("a red probe = %v, want its conformance failure", err)
	}
	if got := latestCordoned(t, st, proofsHostB); got.Outcome != domain.ConformanceFailed {
		t.Fatalf("host B's newest record after a red probe = %s, want failed", got.Outcome)
	}
	if !proofNotCurrent(t, st, proofsHostB) {
		t.Fatalf("host B's red probe didn't set %s", domain.ExecutionHostProofNotCurrent)
	}
	if after := latestCordoned(t, st, domain.DaemonHostID); after.Generation != before.Generation {
		t.Fatalf("host B's red probe moved the daemon's host's record from %d to %d", before.Generation, after.Generation)
	}
	if proofNotCurrent(t, st, domain.DaemonHostID) {
		t.Fatalf("host B's red probe set %s on the daemon's host", domain.ExecutionHostProofNotCurrent)
	}
	if err := proofs.PreJob(t.Context(), "host-c"); err == nil {
		t.Fatal("a job on a host the runner holds no suite for was proved")
	}
}

// TestCordonProofsDigestChangeRerunsOnce shows jobs that see one digest
// change together rerun the host's full pass once, so a second pass's
// marker can't refuse the first job's probe.
func TestCordonProofsDigestChangeRerunsOnce(t *testing.T) {
	t.Parallel()
	_, proofs, suites := twoHostProofs(t)
	suites[proofsHostB].digest = otherDigest
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Go(func() { errs[i] = proofs.PreJob(t.Context(), proofsHostB) })
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	if got := suites[proofsHostB].fulls.Load(); got != 2 {
		t.Fatalf("host B ran %d full passes, want 2: one when added and one for the change", got)
	}
}

// TestCordonProofsRecheckUnderWayLeavesTheCondition shows a settle outside
// a pass, such as after a probe a beginning pass refused, leaves the proof
// condition alone while the newest record is a recheck's marker; the pass
// settles the host when it ends.
func TestCordonProofsRecheckUnderWayLeavesTheCondition(t *testing.T) {
	t.Parallel()
	st, proofs, suites := twoHostProofs(t)
	suites[proofsHostB].record(t.Context(), domain.ConformanceSuperseded)
	if err := proofs.Settle(t.Context(), proofsHostB); err != nil {
		t.Fatal(err)
	}
	if proofNotCurrent(t, st, proofsHostB) {
		t.Fatalf("a recheck under way set %s", domain.ExecutionHostProofNotCurrent)
	}
}

// TestCordonProofsCanceledJobSettlesItsRedProbe shows a probe failure the
// suite records after the job's ctx ended still sets the proof condition.
func TestCordonProofsCanceledJobSettlesItsRedProbe(t *testing.T) {
	t.Parallel()
	st, proofs, suites := twoHostProofs(t)
	suites[proofsHostB].probeFails = true
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := proofs.PreJob(ctx, proofsHostB); err == nil {
		t.Fatal("a red probe under a canceled job was proved")
	}
	if !proofNotCurrent(t, st, proofsHostB) {
		t.Fatalf("a red probe under a canceled job didn't set %s", domain.ExecutionHostProofNotCurrent)
	}
}
