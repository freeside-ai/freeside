package wardstore_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
	"github.com/freeside-ai/freeside/daemon/internal/wardstore"
)

func probeDigest(seed string) domain.Digest {
	return domain.Digest(contentaddr.Sum([]byte(seed)))
}

// probeRig is a store with enrollments and a probe whose observations are
// test-owned. The clock is the probe's injected clock; the test advances it.
type probeRig struct {
	t        *testing.T
	st       *store.Store
	adapters *wardstore.Adapters
	clock    time.Time
	// setupToken and codex answer an observation by store locator.
	setupToken map[string]func() (ward.SetupTokenIntegrity, error)
	codex      map[string]func() (ward.CodexStoreIntegrity, error)
	observed   []string
}

func newProbeRig(t *testing.T) *probeRig {
	t.Helper()
	st, adapters := openEnrollmentStore(t)
	return &probeRig{
		t: t, st: st, adapters: adapters, clock: enrollmentTestAt.Add(time.Hour),
		setupToken: map[string]func() (ward.SetupTokenIntegrity, error){},
		codex:      map[string]func() (ward.CodexStoreIntegrity, error){},
	}
}

func (r *probeRig) probe() wardstore.IntegrityProbe {
	return wardstore.IntegrityProbe{
		Store: r.st,
		ObserveSetupToken: func(_ context.Context, volume string) (ward.SetupTokenIntegrity, error) {
			r.observed = append(r.observed, volume)
			answer, ok := r.setupToken[volume]
			if !ok {
				r.t.Fatalf("unexpected setup-token observation of %q", volume)
			}
			return answer()
		},
		ObserveCodexStore: func(path string) (ward.CodexStoreIntegrity, error) {
			r.observed = append(r.observed, path)
			answer, ok := r.codex[path]
			if !ok {
				r.t.Fatalf("unexpected Codex observation of %q", path)
			}
			return answer()
		},
		Now: func() time.Time { return r.clock },
	}
}

func (r *probeRig) run() map[domain.ClientEnrollmentID]wardstore.IntegrityProbeResult {
	r.t.Helper()
	results, err := r.probe().Run(context.Background())
	if err != nil {
		r.t.Fatalf("probe pass = %v", err)
	}
	byEnrollment := map[domain.ClientEnrollmentID]wardstore.IntegrityProbeResult{}
	for _, result := range results {
		if _, dup := byEnrollment[result.EnrollmentID]; dup {
			r.t.Fatalf("enrollment %s reported twice", result.EnrollmentID)
		}
		byEnrollment[result.EnrollmentID] = result
	}
	return byEnrollment
}

// enrollClaude records a Claude setup-token enrollment whose generation one
// carries recorded as its store digest.
func (r *probeRig) enrollClaude(id domain.AuthIdentityID, recorded domain.Digest) domain.ClientEnrollmentID {
	r.t.Helper()
	identity, bootstrap := claudeEnrollmentFixture(id, "acct-"+string(id))
	volume := string(id) + "-auth"
	identity.Interim.AuthStoreVolume = volume
	bootstrap.Binding.AuthStoreVolume = volume
	bootstrap.Binding.StoreManifestDigest = recorded
	r.bootstrap(identity, bootstrap, nil)
	return bootstrap.Enrollment.ID
}

// enrollCodex records a Codex OAuth enrollment, the store the daemon
// refreshes, whose generation one carries recorded as its store digest.
func (r *probeRig) enrollCodex(id domain.AuthIdentityID, path string, recorded domain.Digest) domain.ClientEnrollmentID {
	r.t.Helper()
	enrollmentID := domain.ClientEnrollmentID(string(id) + "/codex_cli")
	account := "acct-" + string(id)
	identity := domain.AuthIdentity{
		ID: id, Provider: "openai", AccountBinding: account, AuthStoreMutationLease: true,
		MaxParallelExecutions: 1, Enabled: true, CostOwner: "operator",
		Interim: domain.InterimClientFacts{
			AuthStoreVolume: path, RefreshStrategy: domain.RefreshOnDemand, SupportsReadOnlyAuthSnapshot: true,
		},
	}
	expiry := enrollmentTestAt.Add(24 * time.Hour)
	r.bootstrap(identity, ward.EnrollmentBootstrap{
		Enrollment: domain.ClientEnrollment{
			ID: enrollmentID, AuthIdentityID: id, HarnessClient: domain.HarnessClientCodexCLI,
			Route: "openai-subscription", AuthMethod: domain.AuthMethodOAuth,
			CredentialMode:  domain.CredentialSubscriptionContained,
			RefreshStrategy: domain.RefreshOnDemand, SupportsReadOnlyAuthSnapshot: true,
			AccountBinding: account,
		},
		Binding: domain.LeaseGenerationBinding{
			EnrollmentID: enrollmentID, AuthStoreVolume: path, StoreManifestDigest: recorded,
		},
	}, &expiry)
	return enrollmentID
}

func (r *probeRig) bootstrap(identity domain.AuthIdentity, bootstrap ward.EnrollmentBootstrap, expiry *time.Time) {
	r.t.Helper()
	ctx := context.Background()
	holder := domain.InvocationID("enroll-" + string(identity.ID))
	lease, err := r.adapters.Claude.Begin(ctx, identity, bootstrap, holder, enrollmentTestAt, enrollmentTestAt.Add(time.Minute))
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.adapters.Claude.AppendGeneration(ctx, domain.EnrollmentGeneration{
		EnrollmentID: bootstrap.Enrollment.ID, AuthStoreVolume: bootstrap.Binding.AuthStoreVolume,
		StoreManifestDigest: bootstrap.Binding.StoreManifestDigest, LeaseFence: lease.Fence,
		AccountBinding: identity.AccountBinding, TokenExpiry: expiry, RecordedAt: enrollmentTestAt,
	}, enrollmentTestAt); err != nil {
		r.t.Fatal(err)
	}
	if err := r.adapters.Leaser.Release(ctx, identity.ID, holder, lease.Fence, enrollmentTestAt); err != nil {
		r.t.Fatal(err)
	}
}

// appendGeneration appends the enrollment's next generation under a mutation
// lease taken and released at the given instant.
func (r *probeRig) appendGeneration(
	identity domain.AuthIdentityID, enrollment domain.ClientEnrollmentID, digest domain.Digest, at time.Time,
) error {
	ctx := context.Background()
	return r.st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		current, err := tx.CurrentEnrollmentGeneration(ctx, enrollment)
		if err != nil {
			return err
		}
		holder := domain.InvocationID("re-enroll-" + string(identity))
		lease, err := tx.AcquireAuthStoreMutationLeaseBound(ctx, identity, holder, &domain.LeaseGenerationBinding{
			EnrollmentID: enrollment, AuthStoreVolume: current.AuthStoreVolume,
			StoreManifestDigest: current.StoreManifestDigest, Generation: current.Ordinal,
		}, at, at.Add(time.Minute))
		if err != nil {
			return err
		}
		if _, err := tx.AppendEnrollmentGeneration(ctx, domain.EnrollmentGeneration{
			EnrollmentID: enrollment, AuthStoreVolume: current.AuthStoreVolume,
			StoreManifestDigest: digest, LeaseFence: lease.Fence,
			AccountBinding: current.AccountBinding, TokenExpiry: current.TokenExpiry, RecordedAt: at,
		}, at); err != nil {
			return err
		}
		return tx.ReleaseAuthStoreMutationLease(ctx, identity, holder, lease.Fence, at)
	})
}

func (r *probeRig) marks(enrollment domain.ClientEnrollmentID, ordinal int) []domain.GenerationIntegrityMark {
	r.t.Helper()
	ctx := context.Background()
	var marks []domain.GenerationIntegrityMark
	if err := r.st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		marks, err = tx.GenerationIntegrityMarks(ctx, enrollment, ordinal)
		return err
	}); err != nil {
		r.t.Fatal(err)
	}
	return marks
}

func findingsOf(marks []domain.GenerationIntegrityMark) []domain.CredentialIntegrityFinding {
	var findings []domain.CredentialIntegrityFinding
	for _, mark := range marks {
		findings = append(findings, mark.Finding)
	}
	return findings
}

func sameFindings(got []domain.GenerationIntegrityMark, want ...domain.CredentialIntegrityFinding) bool {
	findings := findingsOf(got)
	if len(findings) != len(want) {
		return false
	}
	for i := range want {
		if findings[i] != want[i] {
			return false
		}
	}
	return true
}

// TestIntegrityProbeLeavesIntactStoresUnmarked: a Claude generation recorded
// under either digest convention (the token hash from auth add, the tree
// digest from auth adopt) is intact when the store still matches it, and a
// refreshed Codex store is not compared at all.
func TestIntegrityProbeLeavesIntactStoresUnmarked(t *testing.T) {
	r := newProbeRig(t)
	tokenHash, tree := probeDigest("token bytes"), probeDigest("volume tree")
	added := r.enrollClaude("claude-added", tokenHash)
	adopted := r.enrollClaude("claude-adopted", tree)
	// A refresh rewrote this store, so its bytes no longer match generation
	// one's digest.
	refreshed := r.enrollCodex("codex-main", "/codex/auth.json", probeDigest("before the refresh"))
	intact := func() (ward.SetupTokenIntegrity, error) {
		return ward.SetupTokenIntegrity{TokenDigest: tokenHash, TreeDigest: tree}, nil
	}
	r.setupToken["claude-added-auth"] = intact
	r.setupToken["claude-adopted-auth"] = intact
	r.codex["/codex/auth.json"] = func() (ward.CodexStoreIntegrity, error) {
		return ward.CodexStoreIntegrity{ContentDigest: probeDigest("after the refresh")}, nil
	}

	results := r.run()
	for _, id := range []domain.ClientEnrollmentID{added, adopted} {
		result := results[id]
		if result.Skipped != "" || !result.CorruptionChecked || len(result.Marks) != 0 || result.Ordinal != 1 {
			t.Errorf("%s = %+v, want checked, compared, and unmarked", id, result)
		}
		if marks := r.marks(id, 1); len(marks) != 0 {
			t.Errorf("%s carries marks %+v on an unchanged store", id, marks)
		}
	}
	codex := results[refreshed]
	if codex.Skipped != "" || codex.CorruptionChecked || len(codex.Marks) != 0 {
		t.Errorf("refreshed Codex store = %+v, want checked for length only and unmarked", codex)
	}
	if marks := r.marks(refreshed, 1); len(marks) != 0 {
		t.Errorf("refreshed Codex store carries marks %+v", marks)
	}
}

// TestIntegrityProbeMarksEachFinding: a short token marks truncation, a
// changed token marks corruption, a cut-off auth.json marks truncation, and
// a repeat pass keeps the first observation.
func TestIntegrityProbeMarksEachFinding(t *testing.T) {
	r := newProbeRig(t)
	tokenHash, tree := probeDigest("token bytes"), probeDigest("volume tree")
	short := r.enrollClaude("claude-short", tokenHash)
	changed := r.enrollClaude("claude-changed", tree)
	cutOff := r.enrollCodex("codex-main", "/codex/auth.json", probeDigest("auth.json"))
	r.setupToken["claude-short-auth"] = func() (ward.SetupTokenIntegrity, error) {
		// The digest still matches, so the length verdict alone marks.
		return ward.SetupTokenIntegrity{TokenDigest: tokenHash, TreeDigest: tree, Truncated: true}, nil
	}
	r.setupToken["claude-changed-auth"] = func() (ward.SetupTokenIntegrity, error) {
		return ward.SetupTokenIntegrity{
			TokenDigest: probeDigest("other bytes"), TreeDigest: probeDigest("other tree"),
		}, nil
	}
	r.codex["/codex/auth.json"] = func() (ward.CodexStoreIntegrity, error) {
		return ward.CodexStoreIntegrity{ContentDigest: probeDigest("auth.js"), Truncated: true}, nil
	}

	firstPass := r.clock
	results := r.run()
	want := map[domain.ClientEnrollmentID]domain.CredentialIntegrityFinding{
		short:   domain.CredentialIntegrityTruncation,
		changed: domain.CredentialIntegrityCorruption,
		cutOff:  domain.CredentialIntegrityTruncation,
	}
	for id, finding := range want {
		if result := results[id]; result.Skipped != "" || !sameFindings(result.Marks, finding) {
			t.Errorf("%s = %+v, want one %s mark", id, result, finding)
		}
		if marks := r.marks(id, 1); !sameFindings(marks, finding) || !marks[0].ObservedAt.Equal(firstPass) {
			t.Errorf("%s stored marks = %+v, want one %s mark observed at the pass", id, marks, finding)
		}
	}
	if results[cutOff].CorruptionChecked {
		t.Error("a refreshed store reported its corruption check as run")
	}

	r.clock = r.clock.Add(time.Hour)
	for id, finding := range want {
		result := r.run()[id]
		if !sameFindings(result.Marks, finding) || !result.Marks[0].ObservedAt.Equal(firstPass) {
			t.Errorf("%s repeat pass = %+v, want the first observation's mark", id, result)
		}
	}
}

// TestIntegrityProbeConfirmsCorruptionBeforeMarking: the observer's tree
// digest moves when a tool fails inside it, and the proof still parses. A
// corruption finding has to reproduce before it marks. One that does not is
// dropped alone: a truncation both observations report still marks, and an
// intact store costs one observation.
func TestIntegrityProbeConfirmsCorruptionBeforeMarking(t *testing.T) {
	type observation = func() (ward.SetupTokenIntegrity, error)
	recorded := probeDigest("volume tree")
	seen := func(tree string, truncated bool) observation {
		return func() (ward.SetupTokenIntegrity, error) {
			return ward.SetupTokenIntegrity{
				TokenDigest: probeDigest("token bytes"), TreeDigest: probeDigest(tree), Truncated: truncated,
			}, nil
		}
	}
	failure := func() (ward.SetupTokenIntegrity, error) {
		return ward.SetupTokenIntegrity{}, errors.New("observer failed")
	}
	absent := func() (ward.SetupTokenIntegrity, error) {
		return ward.SetupTokenIntegrity{}, ward.ErrCredentialStoreAbsent
	}
	const truncation, corruption = domain.CredentialIntegrityTruncation, domain.CredentialIntegrityCorruption
	cases := []struct {
		name         string
		observations []observation
		skipped      wardstore.IntegritySkipReason
		unconfirmed  bool
		marks        []domain.CredentialIntegrityFinding
	}{
		{
			name:         "intact store",
			observations: []observation{seen("volume tree", false)},
		},
		{
			name:         "truncated store with a matching digest",
			observations: []observation{seen("volume tree", true)},
			marks:        []domain.CredentialIntegrityFinding{truncation},
		},
		{
			name:         "one incomplete observation",
			observations: []observation{seen("partial tree", false), seen("volume tree", false)},
			unconfirmed:  true,
		},
		{
			name:         "two different incomplete observations",
			observations: []observation{seen("partial tree", false), seen("another partial tree", false)},
			unconfirmed:  true,
		},
		{
			name:         "truncated store, one incomplete observation",
			observations: []observation{seen("partial tree", true), seen("volume tree", true)},
			unconfirmed:  true,
			marks:        []domain.CredentialIntegrityFinding{truncation},
		},
		{
			name:         "length verdicts disagree",
			observations: []observation{seen("other tree", true), seen("other tree", false)},
			skipped:      wardstore.IntegritySkipObservationUnstable,
		},
		{
			name:         "second observation fails",
			observations: []observation{seen("other tree", true), failure},
			skipped:      wardstore.IntegritySkipObservationFailed,
		},
		{
			name:         "store gone by the second observation",
			observations: []observation{seen("other tree", true), absent},
			skipped:      wardstore.IntegritySkipStoreAbsent,
		},
		{
			name:         "reproduced change",
			observations: []observation{seen("other tree", false), seen("other tree", false)},
			marks:        []domain.CredentialIntegrityFinding{corruption},
		},
		{
			name:         "reproduced change on a truncated store",
			observations: []observation{seen("other tree", true), seen("other tree", true)},
			marks:        []domain.CredentialIntegrityFinding{truncation, corruption},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newProbeRig(t)
			id := r.enrollClaude("claude-main", recorded)
			calls := 0
			r.setupToken["claude-main-auth"] = func() (ward.SetupTokenIntegrity, error) {
				if calls >= len(tc.observations) {
					t.Fatalf("observation %d, want at most %d", calls+1, len(tc.observations))
				}
				calls++
				return tc.observations[calls-1]()
			}
			result := r.run()[id]
			if calls != len(tc.observations) {
				t.Errorf("observed %d times, want %d", calls, len(tc.observations))
			}
			if result.Skipped != tc.skipped || result.CorruptionUnconfirmed != tc.unconfirmed {
				t.Errorf("skipped = %q (%v), corruption unconfirmed = %t, want %q and %t",
					result.Skipped, result.Err, result.CorruptionUnconfirmed, tc.skipped, tc.unconfirmed)
			}
			if result.CorruptionChecked != (tc.skipped == "" && !tc.unconfirmed) {
				t.Errorf("corruption checked = %t on %+v", result.CorruptionChecked, result)
			}
			// The store lists a generation's marks by finding, the pass in
			// the order it recorded them.
			stored, want := findingsOf(r.marks(id, 1)), slices.Clone(tc.marks)
			slices.Sort(stored)
			slices.Sort(want)
			if !slices.Equal(stored, want) || !sameFindings(result.Marks, tc.marks...) {
				t.Errorf("stored marks = %v, reported %+v, want %v", stored, result.Marks, tc.marks)
			}
		})
	}
}

// TestIntegrityProbeMarksBothFindingsOnOneGeneration: a store that is both
// short and changed carries both marks.
func TestIntegrityProbeMarksBothFindingsOnOneGeneration(t *testing.T) {
	r := newProbeRig(t)
	id := r.enrollClaude("claude-main", probeDigest("token bytes"))
	r.setupToken["claude-main-auth"] = func() (ward.SetupTokenIntegrity, error) {
		return ward.SetupTokenIntegrity{
			TokenDigest: probeDigest("tok"), TreeDigest: probeDigest("other tree"), Truncated: true,
		}, nil
	}
	r.run()
	// The store lists marks in finding order.
	if marks := r.marks(id, 1); !sameFindings(marks,
		domain.CredentialIntegrityCorruption, domain.CredentialIntegrityTruncation) {
		t.Errorf("marks = %+v, want corruption and truncation", marks)
	}
}

// TestIntegrityProbeNeverMarksAnUnobservedStore covers every way a pass can
// fall short of a finished observation. Each is a skip with a fixed reason
// and no mark, even though the observation it could not finish would have
// reported damage.
func TestIntegrityProbeNeverMarksAnUnobservedStore(t *testing.T) {
	damaged := func() (ward.SetupTokenIntegrity, error) {
		return ward.SetupTokenIntegrity{
			TokenDigest: probeDigest("other bytes"), TreeDigest: probeDigest("other tree"), Truncated: true,
		}, nil
	}
	recorded := probeDigest("token bytes")
	const (
		identity = domain.AuthIdentityID("claude-main")
		volume   = "claude-main-auth"
	)

	t.Run("observation error", func(t *testing.T) {
		r := newProbeRig(t)
		id := r.enrollClaude(identity, recorded)
		cause := errors.New("runtime unavailable")
		r.setupToken[volume] = func() (ward.SetupTokenIntegrity, error) { return ward.SetupTokenIntegrity{}, cause }
		result := r.run()[id]
		if result.Skipped != wardstore.IntegritySkipObservationFailed || !errors.Is(result.Err, cause) {
			t.Errorf("result = %+v, want an observation_failed skip carrying its cause", result)
		}
		if marks := r.marks(id, 1); len(marks) != 0 {
			t.Errorf("an unobserved store was marked: %+v", marks)
		}
	})

	t.Run("absent store", func(t *testing.T) {
		r := newProbeRig(t)
		id := r.enrollClaude(identity, recorded)
		r.setupToken[volume] = func() (ward.SetupTokenIntegrity, error) {
			return ward.SetupTokenIntegrity{}, ward.ErrCredentialStoreAbsent
		}
		if result := r.run()[id]; result.Skipped != wardstore.IntegritySkipStoreAbsent || len(result.Marks) != 0 {
			t.Errorf("result = %+v, want a store_absent skip", result)
		}
		if marks := r.marks(id, 1); len(marks) != 0 {
			t.Errorf("an absent store was marked: %+v", marks)
		}
	})

	t.Run("live mutation lease", func(t *testing.T) {
		r := newProbeRig(t)
		id := r.enrollClaude(identity, recorded)
		r.setupToken[volume] = damaged
		ctx := context.Background()
		lease, err := r.adapters.Leaser.Acquire(ctx, identity, "refresh-in-flight", r.clock, r.clock.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		result := r.run()[id]
		if result.Skipped != wardstore.IntegritySkipMutationLeaseLive || len(r.observed) != 0 {
			t.Errorf("result = %+v after %d observations, want a mutation_lease_live skip and no read",
				result, len(r.observed))
		}
		// The probe neither took over nor disturbed the mutator's lease.
		current, err := r.adapters.Leaser.Get(ctx, identity)
		if err != nil || current.Holder != lease.Holder || current.Fence != lease.Fence || !current.HeldAt(r.clock) {
			t.Errorf("mutation lease after the pass = %+v, %v; want the mutator's own", current, err)
		}
		if marks := r.marks(id, 1); len(marks) != 0 {
			t.Errorf("a store under a live mutation lease was marked: %+v", marks)
		}
	})

	t.Run("read hold ended during the observation", func(t *testing.T) {
		r := newProbeRig(t)
		id := r.enrollClaude(identity, recorded)
		r.setupToken[volume] = func() (ward.SetupTokenIntegrity, error) {
			r.clock = r.clock.Add(wardstore.DefaultIntegrityProbeHold)
			return damaged()
		}
		if result := r.run()[id]; result.Skipped != wardstore.IntegritySkipReadHoldEnded {
			t.Errorf("result = %+v, want a read_hold_ended skip", result)
		}
		if marks := r.marks(id, 1); len(marks) != 0 {
			t.Errorf("an observation that outlived its hold marked: %+v", marks)
		}
	})

	t.Run("generation changed during the observation", func(t *testing.T) {
		r := newProbeRig(t)
		id := r.enrollClaude(identity, recorded)
		r.setupToken[volume] = func() (ward.SetupTokenIntegrity, error) {
			// A writer whose clock runs ahead of the probe's sees the hold
			// as over and appends, while the probe's clock still shows it
			// live.
			ahead := r.clock.Add(2 * wardstore.DefaultIntegrityProbeHold)
			if err := r.appendGeneration(identity, id, probeDigest("re-enrolled"), ahead); err != nil {
				t.Fatal(err)
			}
			return damaged()
		}
		result := r.run()[id]
		if result.Skipped != wardstore.IntegritySkipGenerationChanged || result.Ordinal != 1 {
			t.Errorf("result = %+v, want a generation_changed skip of generation 1", result)
		}
		for ordinal := 1; ordinal <= 2; ordinal++ {
			if marks := r.marks(id, ordinal); len(marks) != 0 {
				t.Errorf("generation %d was marked from a superseded observation: %+v", ordinal, marks)
			}
		}
	})

	t.Run("one unobservable store does not stop the pass", func(t *testing.T) {
		r := newProbeRig(t)
		broken := r.enrollClaude("claude-a", recorded)
		checked := r.enrollClaude("claude-b", recorded)
		r.setupToken["claude-a-auth"] = func() (ward.SetupTokenIntegrity, error) {
			return ward.SetupTokenIntegrity{}, errors.New("runtime unavailable")
		}
		r.setupToken["claude-b-auth"] = damaged
		results := r.run()
		if results[broken].Skipped != wardstore.IntegritySkipObservationFailed {
			t.Errorf("unobservable store = %+v, want a skip", results[broken])
		}
		if results[checked].Skipped != "" || len(results[checked].Marks) != 2 {
			t.Errorf("the store after it = %+v, want it checked and marked", results[checked])
		}
	})
}

// TestIntegrityProbeReadsOnlyUnderTheReadHold: while a store is observed its
// identity's read hold is live, so no mutation can start; once the pass ends
// the hold is released; and the probe never holds the mutation lease.
func TestIntegrityProbeReadsOnlyUnderTheReadHold(t *testing.T) {
	r := newProbeRig(t)
	const identity = domain.AuthIdentityID("claude-main")
	recorded := probeDigest("token bytes")
	r.enrollClaude(identity, recorded)
	ctx := context.Background()
	acquireMutation := func() error {
		_, err := r.adapters.Leaser.Acquire(ctx, identity, "mutator", r.clock, r.clock.Add(time.Minute))
		return err
	}
	r.setupToken["claude-main-auth"] = func() (ward.SetupTokenIntegrity, error) {
		var held *store.ReadHeldError
		if err := acquireMutation(); !errors.As(err, &held) {
			t.Errorf("a mutation started during the observation: %v", err)
		}
		// The only lease row is the released enrollment lease.
		if lease, err := r.adapters.Leaser.Get(ctx, identity); err != nil || lease.HeldAt(r.clock) {
			t.Errorf("mutation lease during the observation = %+v, %v; want none live", lease, err)
		}
		return ward.SetupTokenIntegrity{TokenDigest: recorded}, nil
	}
	r.run()
	if len(r.observed) != 1 {
		t.Fatalf("observations = %v, want exactly one", r.observed)
	}
	if err := acquireMutation(); err != nil {
		t.Errorf("a mutation cannot start after the pass, so the hold was not released: %v", err)
	}
}

// TestIntegrityProbeSkipsAnEnrollmentWithNoGeneration: an enrollment that
// never appended a generation has no store, so it is neither observed nor
// reported.
func TestIntegrityProbeSkipsAnEnrollmentWithNoGeneration(t *testing.T) {
	r := newProbeRig(t)
	identity, bootstrap := claudeEnrollmentFixture("claude-main", "acct-claude-main")
	ctx := context.Background()
	lease, err := r.adapters.Claude.Begin(ctx, identity, bootstrap, "enroll", enrollmentTestAt, enrollmentTestAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.adapters.Leaser.Release(ctx, identity.ID, "enroll", lease.Fence, enrollmentTestAt); err != nil {
		t.Fatal(err)
	}
	if results := r.run(); len(results) != 0 || len(r.observed) != 0 {
		t.Errorf("results = %+v after %d observations, want neither", results, len(r.observed))
	}
}

// TestIntegrityProbeCancelsAnObserverThatOutlivesTheHold: the observation's
// context ends with the hold window, so an observer that hangs is cancelled
// instead of reading a store nothing holds, and the store is not marked.
func TestIntegrityProbeCancelsAnObserverThatOutlivesTheHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newProbeRig(t)
		id := r.enrollClaude("claude-main", probeDigest("token bytes"))
		probe := r.probe()
		probe.HoldDuration = 30 * time.Second
		// The bubble's clock starts before the fixture's lease history, so
		// the probe's clock is the fixture instant plus bubble time elapsed.
		start := time.Now()
		probe.Now = func() time.Time { return r.clock.Add(time.Since(start)) }
		probe.ObserveSetupToken = func(ctx context.Context, _ string) (ward.SetupTokenIntegrity, error) {
			<-ctx.Done()
			return ward.SetupTokenIntegrity{}, ctx.Err()
		}
		results, err := probe.Run(context.Background())
		if err != nil || len(results) != 1 {
			t.Fatalf("pass = %+v, %v", results, err)
		}
		if results[0].Skipped != wardstore.IntegritySkipObservationFailed ||
			!errors.Is(results[0].Err, context.DeadlineExceeded) {
			t.Errorf("result = %+v, want the observation cancelled at the deadline", results[0])
		}
		if waited := time.Since(start); waited != 30*time.Second {
			t.Errorf("the observer ran %s, want it cancelled at the 30s hold window", waited)
		}
		if marks := r.marks(id, 1); len(marks) != 0 {
			t.Errorf("a cancelled observation marked: %+v", marks)
		}
	})
}

func TestIntegrityProbeRequiresItsDependencies(t *testing.T) {
	r := newProbeRig(t)
	probe := r.probe()
	probe.ObserveCodexStore = nil
	if _, err := probe.Run(context.Background()); err == nil {
		t.Error("a probe with no Codex observation ran")
	}
	probe = r.probe()
	probe.HoldDuration = -time.Second
	if _, err := probe.Run(context.Background()); err == nil {
		t.Error("a probe with a negative hold ran")
	}
}
