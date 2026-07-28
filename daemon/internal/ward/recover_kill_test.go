package ward

import (
	"context"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// The kill-boundary harness proves recovery against the states a real crash
// actually leaves, not hand-modeled approximations: a full leased, seeded
// Handoff runs over the fake runtime, and at each boundary a hook snapshots
// the world (runtime objects and journal rows) exactly as they stand — for
// runtime hooks that is before the triggering call's own mutation, and for
// journal hooks before the write becomes durable, which is precisely what a
// process death at that instant leaves behind. The snapshot is then replayed
// into a fresh runtime, journal, and backend (nothing carried in memory,
// like a restarted daemon), and Recover must land the documented outcome. A
// kill cannot be simulated by aborting Handoff in-process, because its
// deferred teardown always runs and would clean the world the crash leaves.
type killSnapshot struct {
	nets      map[string]fakeNetwork
	vols      map[string]fakeVol
	ctrs      map[string]fakeCtr
	volBase   map[string]string
	volTree   map[string]string
	credState map[string]string
	journal   map[string]HandoffJournalRecord
}

// takeSnapshot deep-copies the fake world and journal. It runs from hooks on
// the handoff goroutine, where unlocked reads of the fakes' state are safe
// (every mutation happens on that same goroutine).
func takeSnapshot(rt *fakeRuntime, j *fakeJournal) *killSnapshot {
	s := &killSnapshot{
		nets:      map[string]fakeNetwork{},
		vols:      map[string]fakeVol{},
		ctrs:      map[string]fakeCtr{},
		volBase:   maps.Clone(rt.volBase),
		volTree:   maps.Clone(rt.volTree),
		credState: maps.Clone(rt.credState),
		journal:   map[string]HandoffJournalRecord{},
	}
	for name, n := range rt.nets {
		s.nets[name] = fakeNetwork{labels: slices.Clone(n.labels), created: n.created}
	}
	for name, v := range rt.vols {
		s.vols[name] = fakeVol{labels: slices.Clone(v.labels), created: v.created}
	}
	for name, c := range rt.ctrs {
		cp := *c
		cp.spec = cloneContainerSpec(c.spec)
		s.ctrs[name] = cp
	}
	for id, r := range j.records {
		cp := *r
		if r.Lease != nil {
			lease := *r.Lease
			cp.Lease = &lease
		}
		if r.Outcome != nil {
			outcome := *r.Outcome
			cp.Outcome = &outcome
		}
		s.journal[id] = cp
	}
	return s
}

// restoreInto fills a fresh fake runtime and journal from the snapshot; the
// sequence counter starts high so fresh creates cannot collide with restored
// fingerprints.
func restoreInto(rt *fakeRuntime, s *killSnapshot) *fakeJournal {
	for name, n := range s.nets {
		cp := n
		rt.nets[name] = &cp
	}
	for name, v := range s.vols {
		cp := v
		rt.vols[name] = &cp
	}
	for name, c := range s.ctrs {
		cp := c
		rt.ctrs[name] = &cp
	}
	rt.volBase = maps.Clone(s.volBase)
	rt.volTree = maps.Clone(s.volTree)
	rt.credState = maps.Clone(s.credState)
	rt.seq = 1000
	j := newFakeJournal()
	for id, r := range s.journal {
		cp := r
		j.records[id] = &cp
	}
	return j
}

func TestRecoverKillBoundaries(t *testing.T) {
	type killPoint struct {
		name string
		// arm installs the hook that captures the snapshot at this boundary.
		arm  func(fx *handoffFixture, j *fakeJournal, names handoffNames, capture func())
		want RecoveryOutcome
	}
	points := []killPoint{
		{"lease and begin committed, no runtime object", func(fx *handoffFixture, _ *fakeJournal, _ handoffNames, capture func()) {
			fx.rt.onCreateVolume = func(string) error { capture(); return nil }
		}, RecoveryLoss},
		{"workspace created", func(fx *handoffFixture, _ *fakeJournal, _ handoffNames, capture func()) {
			fx.rt.onInspectVolume = func(_ string, v VolumeSummary) (VolumeSummary, error) { capture(); return v, nil }
		}, RecoveryLoss},
		{"mid-seed, seeder holds the workspace", func(fx *handoffFixture, _ *fakeJournal, _ handoffNames, capture func()) {
			fx.rt.onCopyIntoContainer = func(string, string, string) error { capture(); return nil }
		}, RecoveryLoss},
		{"base observed, credential observer next", func(fx *handoffFixture, _ *fakeJournal, names handoffNames, capture func()) {
			fx.rt.onCreateContainer = func(spec ContainerSpec) error {
				if spec.Name == names.CredObsPre {
					capture()
				}
				return nil
			}
		}, RecoveryLoss},
		{"observations journalled, egress next", func(fx *handoffFixture, _ *fakeJournal, _ handoffNames, capture func()) {
			fx.rt.onCreateNetwork = func(string) error { capture(); return nil }
		}, RecoveryLoss},
		{"agent created, not started", func(fx *handoffFixture, _ *fakeJournal, names handoffNames, capture func()) {
			fx.rt.onStart = func(id string) error {
				if id == names.Agent {
					capture()
				}
				return nil
			}
		}, RecoveryLoss},
		{"agent running", func(fx *handoffFixture, _ *fakeJournal, names handoffNames, capture func()) {
			started := false
			fx.rt.onStart = func(id string) error {
				if id == names.Agent {
					started = true
				}
				return nil
			}
			fx.rt.onInspect = func(id string, rep InspectReport) (InspectReport, error) {
				if id == names.Agent && started {
					capture()
				}
				return rep, nil
			}
		}, RecoveryLoss},
		{"agent gone, writer-complete not yet durable", func(_ *handoffFixture, j *fakeJournal, _ handoffNames, capture func()) {
			j.onCall = func(call string) {
				if strings.HasPrefix(call, "journal-writer-complete") {
					capture()
				}
			}
		}, RecoveryLoss},
		{"writer-complete durable, post-observer next", func(fx *handoffFixture, _ *fakeJournal, names handoffNames, capture func()) {
			fx.rt.onCreateContainer = func(spec ContainerSpec) error {
				if spec.Name == names.CredObsPost {
					capture()
				}
				return nil
			}
		}, RecoveryExported},
		{"exporter created, not started", func(fx *handoffFixture, _ *fakeJournal, names handoffNames, capture func()) {
			fx.rt.onStart = func(id string) error {
				if id == names.Exporter {
					capture()
				}
				return nil
			}
		}, RecoveryExported},
		{"exporter finished, export unverified", func(fx *handoffFixture, _ *fakeJournal, names handoffNames, capture func()) {
			fx.rt.onExport = func(id string, _ io.Writer) error {
				if id == names.Exporter {
					capture()
				}
				return nil
			}
		}, RecoveryExported},
		{"export verified, completed-close not yet durable", func(_ *handoffFixture, j *fakeJournal, _ handoffNames, capture func()) {
			j.onCall = func(call string) {
				if strings.HasPrefix(call, "journal-close") {
					capture()
				}
			}
		}, RecoveryLoss},
	}

	for _, kp := range points {
		t.Run(kp.name, func(t *testing.T) {
			// The original run: leased and seeded, so every journal
			// amendment and observer role is in play.
			fx := newHandoffFixture(t)
			j := fx.journalled()
			hs, _ := fx.leased(t)
			hs.Seed = fx.seed(t).Seed
			names := namesFor(hs.RunID)
			var snap *killSnapshot
			kp.arm(fx, j, names, func() {
				if snap == nil {
					snap = takeSnapshot(fx.rt, j)
				}
			})
			if _, err := fx.runSpec(t, hs); err != nil {
				t.Fatalf("original run: %v", err)
			}
			if snap == nil {
				t.Fatal("kill point never fired")
			}

			// The restarted daemon: fresh runtime, journal, leaser, and
			// backend; only the snapshot crosses the boundary.
			fx2 := newHandoffFixture(t)
			j2 := restoreInto(fx2.rt, snap)
			j2.rt = fx2.rt
			fx2.cfg.Journal = j2
			base := time.Date(2026, 7, 1, 14, 0, 0, 0, time.UTC)
			fx2.cfg.Now = func() time.Time { return base }
			l2 := &fakeLeaser{rt: fx2.rt}
			fx2.cfg.AuthStoreLeaser = l2
			rec := j2.snapshot(hs.RunID)
			if rec == nil {
				t.Fatal("snapshot carries no journal record")
			}
			if rec.Lease != nil {
				// The store outlives the crash: prime the fresh leaser with
				// the persisted row so the release finds the holder.
				l2.lease = domain.AuthStoreMutationLease{
					AuthIdentityID: rec.Lease.AuthIdentityID,
					Holder:         rec.Lease.Holder,
					Fence:          rec.Lease.Fence,
					AcquiredAt:     base.Add(-2 * time.Hour),
					ExpiresAt:      base.Add(2 * time.Hour),
				}
			}

			res, err := fx2.backend(t).Recover(context.Background(), hs.RunID, hs)
			if err != nil {
				t.Fatalf("Recover = %v, want %q", err, kp.want)
			}
			if res.ExportDir != "" {
				t.Cleanup(func() { _ = os.RemoveAll(res.ExportDir) })
			}
			if res.Outcome != kp.want {
				t.Fatalf("Outcome = %q (loss cause %q), want %q", res.Outcome, res.LossCause, kp.want)
			}
			switch kp.want {
			case RecoveryLoss:
				if res.ExportDir != "" {
					t.Error("a loss released an export directory")
				}
				final := j2.snapshot(hs.RunID)
				if final == nil || final.Outcome == nil || *final.Outcome != HandoffLoss {
					t.Errorf("record = %+v, want closed as loss", final)
				}
			case RecoveryExported:
				if len(res.Manifest.Entries) != 1 {
					t.Errorf("Manifest entries = %d, want the fixture archive's 1", len(res.Manifest.Entries))
				}
				if _, serr := os.Stat(res.ExportDir); serr != nil {
					t.Errorf("released output dir: %v", serr)
				}
				if !res.AuthStore.Leased || res.AuthStore.PreDigest != rec.CredentialPreDigest {
					t.Errorf("AuthStore = %+v, want the journalled pre-digest carried through", res.AuthStore)
				}
				if res.Workspace.ObservedBaseSHA != rec.ObservedBaseSHA || !res.Workspace.Seeded {
					t.Errorf("Workspace = %+v, want the journalled base attestation", res.Workspace)
				}
				final := j2.snapshot(hs.RunID)
				if final == nil || final.Outcome == nil || *final.Outcome != HandoffCompleted {
					t.Errorf("record = %+v, want closed as completed", final)
				}
			}
			// No orphan survives recovery, whichever way it ended.
			fx2.assertReaped(t)
		})
	}
}

// TestRecoverKillBoundaryForeignSurvivors: the replayed world also carries
// foreign same-name objects (another operator's run, no token); recovery
// commits its loss around them and leaves them standing.
func TestRecoverKillBoundaryForeignSurvivors(t *testing.T) {
	fx := newHandoffFixture(t)
	j := fx.journalled()
	hs, _ := fx.leased(t)
	names := namesFor(hs.RunID)
	var snap *killSnapshot
	fx.rt.onStart = func(id string) error {
		if id == names.Agent && snap == nil {
			snap = takeSnapshot(fx.rt, j)
		}
		return nil
	}
	if _, err := fx.runSpec(t, hs); err != nil {
		t.Fatalf("original run: %v", err)
	}
	if snap == nil {
		t.Fatal("kill point never fired")
	}

	fx2 := newHandoffFixture(t)
	j2 := restoreInto(fx2.rt, snap)
	fx2.cfg.Journal = j2
	l2 := &fakeLeaser{rt: fx2.rt}
	fx2.cfg.AuthStoreLeaser = l2
	rec := j2.snapshot(hs.RunID)
	if rec == nil {
		t.Fatal("snapshot carries no journal record")
	}
	// The store still holds the crashed run's row, lapsed by the time
	// recovery runs; a partial or zero row would be an incoherent store and
	// fail the re-gate closed instead of modeling this boundary.
	l2.lease = domain.AuthStoreMutationLease{
		AuthIdentityID: rec.Lease.AuthIdentityID, Holder: rec.Lease.Holder, Fence: rec.Lease.Fence,
		AcquiredAt: time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC),
		ExpiresAt:  time.Date(2026, 7, 1, 13, 0, 0, 0, time.UTC),
	}
	// A foreign exporter squatting one of this run's deterministic names,
	// with someone else's token.
	foreign := []Label{
		{Key: labelKey, Value: hs.RunID},
		{Key: ownershipLabelKey, Value: "ffffffffffffffffffffffffffffffff"},
	}
	if err := fx2.rt.CreateContainer(context.Background(), ContainerSpec{
		Name: names.Exporter, Image: "foreign.test/img@sha256:" + strings.Repeat("9", 64),
		Command: []string{"sh"}, Labels: foreign,
	}); err != nil {
		t.Fatal(err)
	}

	res, err := fx2.backend(t).Recover(context.Background(), hs.RunID, hs)
	if err != nil {
		t.Fatalf("Recover = %v, want committed loss", err)
	}
	if res.Outcome != RecoveryLoss {
		t.Fatalf("Outcome = %q, want loss", res.Outcome)
	}
	fx2.rt.mu.Lock()
	_, survives := fx2.rt.ctrs[names.Exporter]
	fx2.rt.mu.Unlock()
	if !survives {
		t.Fatal("recovery deleted a foreign object")
	}
}
