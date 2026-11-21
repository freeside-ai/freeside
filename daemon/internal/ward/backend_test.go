package ward

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

// declaredCapabilities is the frozen declaration list from issue #76;
// backend_test asserts the backend matches it exactly, member for member.
var declaredCapabilities = []exec.Capability{
	exec.CapDetachableWorkspace,
	exec.CapPostExitExport,
	exec.CapReadOnlyRemount,
}

// conformancePendingCapabilities are valid vocabulary members this backend
// declares only after their live probe passes.
var conformancePendingCapabilities = []exec.Capability{
	exec.CapNetworklessExport,
	exec.CapEnforcedProviderEgress,
}

// refusedCapabilities must never be declared: the first two are refuted on
// the reference runtime (the same-VM fallback class and volume snapshots),
// the third belongs to the host verification executor's class, and the last
// four to the cordoned class, not the ward's.
var refusedCapabilities = []exec.Capability{
	exec.CapCredentialVolumeDetach,
	exec.CapWorkspaceSnapshot,
	exec.CapCredentialFreeHostVerification,
	exec.CapCordonedSecretDenial,
	exec.CapCordonedLaunchDenial,
	exec.CapCordonedDaemonProtection,
	exec.CapCordonedRunEndProof,
}

// testImageA and testImageB are two digest-pinned writer images, each with
// its own configuration and proof.
var (
	testImageA = "example.test/agent@sha256:" + strings.Repeat("1", 64)
	testImageB = "example.test/agent@sha256:" + strings.Repeat("2", 64)
)

// capabilitiesFor is the declaration admission reads for image.
func capabilitiesFor(b *Backend, image string) exec.CapabilitySet {
	_, caps, _ := b.ConformanceFor(image)
	return caps
}

// passedRecordFor is a persisted passed record of image's configuration.
func passedRecordFor(t *testing.T, b *Backend, image string, generation uint64) domain.BackendConformance {
	t.Helper()
	record, err := domain.NewBackendConformance(domain.BackendConformanceInput{
		Backend:             domain.BackendFreshVMReadOnlyVolumeHandoff,
		Outcome:             domain.ConformancePassed,
		ConfigurationDigest: b.ConfigurationDigestFor(image),
		Capabilities:        domain.NewCapabilitySnapshot(provenCapabilities()...),
		ProvedAt:            time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("NewBackendConformance: %v", err)
	}
	record.Generation = generation
	return record
}

func newTestBackend(t *testing.T) *Backend {
	t.Helper()
	b, err := New(stubRuntime{}, testConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

// TestCapabilitySnapshot is acceptance 4: the spawn snapshot matches the
// declaration list exactly, and every capability in the registry is
// accounted for as either declared or refused, so registering a sixth
// capability forces this test to place it.
func TestCapabilitySnapshot(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)

	adm, err := exec.CheckCapabilities(b, declaredCapabilities)
	if err != nil {
		t.Fatalf("CheckCapabilities(declared) = %v, want admission", err)
	}
	if adm.Backend != BackendName {
		t.Errorf("Admission.Backend = %q, want %q", adm.Backend, BackendName)
	}
	for _, c := range exec.AllCapabilities {
		declared := adm.Declared.Has(c)
		wantDeclared := slices.Contains(declaredCapabilities, c)
		wantRefused := slices.Contains(refusedCapabilities, c)
		wantPending := slices.Contains(conformancePendingCapabilities, c)
		if !wantDeclared && !wantRefused && !wantPending {
			t.Errorf("capability %q is in exec.AllCapabilities but neither declared nor refused here; place it", c)
		}
		if declared != wantDeclared {
			t.Errorf("capability %q declared = %v, want %v", c, declared, wantDeclared)
		}
	}
}

func TestNetworklessCapabilityRequiresConformance(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	if capabilitiesFor(b, testImageA).Has(exec.CapNetworklessExport) {
		t.Fatal("new backend declared supports_networkless_export before conformance")
	}
	if _, err := exec.CheckDeclaredCapabilities(BackendName, capabilitiesFor(b, testImageA),
		conformancePendingCapabilities); !errors.Is(err, exec.ErrCapabilityRefused) {
		t.Fatalf("unproven networkless capability = %v, want ErrCapabilityRefused", err)
	}
	generation, _ := b.beginConformanceProof(testImageA, nil)
	b.finishConformanceProof(testImageA, generation, true)
	if _, err := exec.CheckDeclaredCapabilities(BackendName, capabilitiesFor(b, testImageA),
		append(declaredCapabilities, exec.CapNetworklessExport)); err != nil {
		t.Fatalf("proven networkless capability = %v, want admission", err)
	}
	// The base declaration never carries an image's earned capabilities.
	if b.Capabilities().Has(exec.CapNetworklessExport) {
		t.Fatal("base declaration carries supports_networkless_export")
	}
}

func TestRestoreConformancePublishesDurableCapabilitiesOnce(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	record := passedRecordFor(t, b, testImageA, 7)
	if err := b.RestoreConformance(testImageA, record); err != nil {
		t.Fatalf("RestoreConformance: %v", err)
	}
	for _, capability := range conformancePendingCapabilities {
		if !capabilitiesFor(b, testImageA).Has(capability) {
			t.Errorf("restored image does not declare %q", capability)
		}
		if capabilitiesFor(b, testImageB).Has(capability) {
			t.Errorf("restoring image A declared %q for image B", capability)
		}
	}
	if err := b.RestoreConformance(testImageA, record); err == nil {
		t.Fatal("a second restore of the same image was accepted")
	}

	if _, err := b.beginConformanceProof(testImageA, nil); err != nil {
		t.Fatalf("begin proof: %v", err)
	}
	if err := b.RestoreConformance(testImageA, record); err == nil {
		t.Fatal("durable success was restored after a live proof began")
	}
	for _, capability := range conformancePendingCapabilities {
		if capabilitiesFor(b, testImageA).Has(capability) {
			t.Errorf("stale restore resurrected %q after a live proof began", capability)
		}
	}
}

// TestRestoreIsPerImage: a live proof of one image leaves another image's
// restore open, and a restore is refused only once its own image's proof
// began.
func TestRestoreIsPerImage(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	generation, _ := b.beginConformanceProof(testImageB, nil)
	b.finishConformanceProof(testImageB, generation, true)
	if err := b.RestoreConformance(testImageA, passedRecordFor(t, b, testImageA, 3)); err != nil {
		t.Fatalf("restore image A after only image B's proof began = %v", err)
	}
	if err := b.RestoreConformance(testImageB, passedRecordFor(t, b, testImageB, 4)); err == nil {
		t.Fatal("restore of image B accepted after image B's live proof began")
	}
	if got := b.ProvenImages(); !slices.Equal(got, []string{testImageA, testImageB}) {
		t.Fatalf("ProvenImages = %q, want both images", got)
	}
}

// TestRestoreRefusesAPreviousConfigurationVersion: a row written under the
// per-backend rule (/v4) for the same image neither restores nor matches
// the image's digest, so the image proves again (#3211).
func TestRestoreRefusesAPreviousConfigurationVersion(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	v4 := b.configurationDigest("freeside.ward.conformance-configuration/v4", testImageA)
	if v4 == b.ConfigurationDigestFor(testImageA) {
		t.Fatal("a /v4 digest equals the current digest for the same image")
	}
	record := passedRecordFor(t, b, testImageA, 5)
	record.ConfigurationDigest = v4
	if err := b.RestoreConformance(testImageA, record); err == nil {
		t.Fatal("a /v4 proof was restored")
	}
	if digest, caps, proved := b.ConformanceFor(testImageA); digest == v4 || proved || caps.Has(exec.CapNetworklessExport) {
		t.Fatalf("ConformanceFor after a refused /v4 restore = (%s, %v)", digest, caps)
	}
}

func TestRestoreConformanceRejectsUnpersistedOrFailedRecords(t *testing.T) {
	t.Parallel()
	configurationDigest := newTestBackend(t).ConfigurationDigestFor(testImageA)
	passed, err := domain.NewBackendConformance(domain.BackendConformanceInput{
		Backend:             domain.BackendFreshVMReadOnlyVolumeHandoff,
		Outcome:             domain.ConformancePassed,
		ConfigurationDigest: configurationDigest,
		Capabilities: domain.NewCapabilitySnapshot(
			domain.CapDetachableWorkspace,
			domain.CapPostExitExport,
			domain.CapReadOnlyRemount,
			domain.CapNetworklessExport,
			domain.CapEnforcedProviderEgress,
		),
		ProvedAt: time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("NewBackendConformance(passed): %v", err)
	}
	failed, err := domain.NewBackendConformance(domain.BackendConformanceInput{
		Backend:             domain.BackendFreshVMReadOnlyVolumeHandoff,
		Outcome:             domain.ConformanceFailed,
		ConfigurationDigest: configurationDigest,
		ProvedAt:            time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("NewBackendConformance(failed): %v", err)
	}
	failed.Generation = 8

	for _, tc := range []struct {
		name   string
		record domain.BackendConformance
	}{
		{"unpersisted", passed},
		{"failed", failed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newTestBackend(t)
			if err := b.RestoreConformance(testImageA, tc.record); err == nil {
				t.Fatal("RestoreConformance accepted a record that cannot grant capabilities")
			}
			for _, capability := range conformancePendingCapabilities {
				if capabilitiesFor(b, testImageA).Has(capability) {
					t.Errorf("rejected record published %q", capability)
				}
			}
		})
	}
}

func TestRestoreConformanceRejectsAnotherBackendConfiguration(t *testing.T) {
	t.Parallel()
	proved := newTestBackend(t)
	record := passedRecordFor(t, proved, testImageA, 7)

	cfg := testConfig()
	cfg.ExporterImage = "example.test/other-exporter@sha256:" + strings.Repeat("2", 64)
	active, err := New(stubRuntime{}, cfg)
	if err != nil {
		t.Fatalf("New(active): %v", err)
	}
	if err := active.RestoreConformance(testImageA, record); err == nil {
		t.Fatal("proof from another exporter configuration was restored")
	}
	// A record of image A's configuration never restores as image B's.
	if err := proved.RestoreConformance(testImageB, record); err == nil {
		t.Fatal("image A's proof was restored for image B")
	}
	for _, capability := range conformancePendingCapabilities {
		if capabilitiesFor(active, testImageA).Has(capability) || capabilitiesFor(proved, testImageB).Has(capability) {
			t.Errorf("mismatched configuration published %q", capability)
		}
	}
}

func TestConfigurationDigestTracksConformanceSurfaces(t *testing.T) {
	t.Parallel()
	binDir := t.TempDir()
	binA := filepath.Join(binDir, "container-a")
	binB := filepath.Join(binDir, "container-b")
	for path, body := range map[string]string{binA: "runtime-a", binB: "runtime-b"} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write runtime fixture: %v", err)
		}
	}
	cfg := testConfig()
	cfg.ProviderEndpoints = []string{"api.anthropic.com:443", "console.anthropic.com:443"}
	base, err := New(NewCLIRuntime(binA), cfg)
	if err != nil {
		t.Fatalf("New(base): %v", err)
	}
	reordered := cfg
	reordered.ProviderEndpoints = []string{"console.anthropic.com:443", "api.anthropic.com:443"}
	same, err := New(NewCLIRuntime(binA), reordered)
	if err != nil {
		t.Fatalf("New(reordered): %v", err)
	}
	if same.ConfigurationDigest() != base.ConfigurationDigest() {
		t.Fatal("endpoint ordering changed a set-valued configuration identity")
	}
	baseDigest := base.ConfigurationDigest()
	originalDaemonIdentity := base.daemonIdentity
	base.daemonIdentity = "freesided@sha256:" + strings.Repeat("9", 64)
	if base.ConfigurationDigest() == baseDigest {
		t.Fatal("daemon executable replacement retained the conformance digest")
	}
	base.daemonIdentity = originalDaemonIdentity

	tests := []struct {
		name string
		rt   Runtime
		cfg  Config
	}{
		{"runtime command", NewCLIRuntime(binB), cfg},
		{"agent image", NewCLIRuntime(binA), func() Config {
			changed := cfg
			changed.AgentImage = "example.test/agent@sha256:" + strings.Repeat("3", 64)
			return changed
		}()},
		{"exporter image", NewCLIRuntime(binA), func() Config {
			changed := cfg
			changed.ExporterImage = "example.test/other-exporter@sha256:" + strings.Repeat("2", 64)
			return changed
		}()},
		{"provider endpoint", NewCLIRuntime(binA), func() Config {
			changed := cfg
			changed.ProviderEndpoints = []string{"api.anthropic.com:443"}
			return changed
		}()},
		{"seed root", NewCLIRuntime(binA), func() Config {
			changed := cfg
			changed.SeedRoot = "/different/seed-root"
			return changed
		}()},
		{"export root", NewCLIRuntime(binA), func() Config {
			changed := cfg
			changed.ExportRoot = "/different/export-root"
			return changed
		}()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			changed, err := New(tc.rt, tc.cfg)
			if err != nil {
				t.Fatalf("New(changed): %v", err)
			}
			if changed.ConfigurationDigest() == base.ConfigurationDigest() {
				t.Fatal("conformance-relevant configuration change retained the same digest")
			}
		})
	}
}

func TestDaemonConfigurationIdentityHashesExecutableBytes(t *testing.T) {
	t.Parallel()
	bin := filepath.Join(t.TempDir(), "freesided")
	if err := os.WriteFile(bin, []byte("daemon-v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := executableIdentity(bin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("daemon-v2"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := executableIdentity(bin)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("in-place daemon replacement retained its executable identity")
	}
}

func TestConfigurationDigestTracksRuntimeExecutableBytes(t *testing.T) {
	t.Parallel()
	bin := t.TempDir() + "/container"
	if err := os.WriteFile(bin, []byte("runtime-v1"), 0o600); err != nil {
		t.Fatalf("write runtime v1: %v", err)
	}
	first, err := New(NewCLIRuntime(bin), testConfig())
	if err != nil {
		t.Fatalf("New(first): %v", err)
	}
	if err := os.WriteFile(bin, []byte("runtime-v2"), 0o600); err != nil {
		t.Fatalf("write runtime v2: %v", err)
	}
	second, err := New(NewCLIRuntime(bin), testConfig())
	if err != nil {
		t.Fatalf("New(second): %v", err)
	}
	if first.ConfigurationDigest() == second.ConfigurationDigest() {
		t.Fatal("in-place runtime replacement retained the conformance digest")
	}
	current, err := runtimeConfigurationIdentity(first.rt)
	if err != nil {
		t.Fatalf("current runtime identity: %v", err)
	}
	if current == first.runtimeIdentity {
		t.Fatal("an existing backend did not detect in-place runtime replacement")
	}
}

func TestConformanceProofRejectsStaleGeneration(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	older, _ := b.beginConformanceProof(testImageA, nil)
	newer, _ := b.beginConformanceProof(testImageA, nil)
	if !b.finishConformanceProof(testImageA, newer, false) {
		t.Fatal("current generation reported itself superseded")
	}
	if b.finishConformanceProof(testImageA, older, true) {
		t.Fatal("superseded generation reported itself current")
	}
	for _, c := range conformancePendingCapabilities {
		if capabilitiesFor(b, testImageA).Has(c) {
			t.Fatalf("older successful proof overrode a newer failed proof for %q", c)
		}
	}

	latest, _ := b.beginConformanceProof(testImageA, nil)
	if !b.finishConformanceProof(testImageA, latest, true) {
		t.Fatal("latest proof reported itself superseded")
	}
	for _, c := range conformancePendingCapabilities {
		if !capabilitiesFor(b, testImageA).Has(c) {
			t.Fatalf("latest successful proof did not publish %q", c)
		}
	}
}

// TestConformanceForBindsEachImagesDigest pins that the image is part of the
// identity an admission records: each image yields its own digest, exactly
// the one ConfigurationDigestFor predicts, and the static digest is the
// configured image's.
func TestConformanceForBindsEachImagesDigest(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.AgentImage = testImageA
	b, err := New(stubRuntime{}, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	digestA, _, _ := b.ConformanceFor(testImageA)
	digestB, _, _ := b.ConformanceFor(testImageB)
	if digestA != b.ConfigurationDigestFor(testImageA) || digestA != b.ConfigurationDigest() {
		t.Fatalf("ConformanceFor(A) digest = %s, want the configured image's", digestA)
	}
	if digestB != b.ConfigurationDigestFor(testImageB) || digestB == digestA {
		t.Fatalf("ConformanceFor(B) digest = %s, want image B's own", digestB)
	}
}

// TestProofsAreHeldPerImage is #3211's in-memory rule: a begin, a failed
// conclude, or a stale pass for image A never touches image B's published
// proof, and a pass for A that a newer begin for A superseded cannot
// publish.
func TestProofsAreHeldPerImage(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	generationB, _ := b.beginConformanceProof(testImageB, nil)
	b.finishConformanceProof(testImageB, generationB, true)
	published := func(image string) bool {
		return capabilitiesFor(b, image).Has(exec.CapNetworklessExport) &&
			capabilitiesFor(b, image).Has(exec.CapEnforcedProviderEgress)
	}

	stale, _ := b.beginConformanceProof(testImageA, nil)
	if !published(testImageB) {
		t.Fatal("a begin for image A cleared image B's proof")
	}
	current, _ := b.beginConformanceProof(testImageA, nil)
	if b.finishConformanceProof(testImageA, stale, true) || published(testImageA) {
		t.Fatal("a superseded pass for image A published")
	}
	if !b.finishConformanceProof(testImageA, current, false) {
		t.Fatal("the current pass for image A reported itself superseded")
	}
	if published(testImageA) || !published(testImageB) {
		t.Fatalf("after A failed: A published=%v, B published=%v; want only B", published(testImageA), published(testImageB))
	}
	// A pass for B can never conclude a generation stamped for A.
	if b.finishConformanceProof(testImageB, current, false) || !published(testImageB) {
		t.Fatal("image A's generation concluded image B's proof")
	}
	if got := b.ProvenImages(); !slices.Equal(got, []string{testImageB}) {
		t.Fatalf("ProvenImages = %q, want only image B", got)
	}
}

// TestRetireConformanceWritesTheMarkerFirst: retiring appends the
// superseding marker for the image's own configuration, then stops tracking
// the image, so its proof is unpublished and a later pass proves it afresh;
// another image's proof is untouched. A failed append still unpublishes the
// image and keeps tracking it.
func TestRetireConformanceWritesTheMarkerFirst(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	for _, image := range []string{testImageA, testImageB} {
		generation, _ := b.beginConformanceProof(image, nil)
		b.finishConformanceProof(image, generation, true)
	}
	rec := &recordingConformance{}
	if err := b.RetireConformance(t.Context(), testImageA, rec); err != nil {
		t.Fatalf("RetireConformance = %v", err)
	}
	if len(rec.records) != 1 || rec.records[0].Outcome != domain.ConformanceSuperseded ||
		rec.records[0].ConfigurationDigest != b.ConfigurationDigestFor(testImageA) {
		t.Fatalf("retire recorded %+v, want one marker for image A's configuration", rec.records)
	}
	if got := b.TrackedImages(); !slices.Equal(got, []string{testImageB}) {
		t.Fatalf("TrackedImages = %q, want only image B", got)
	}
	if got := b.ProvenImages(); !slices.Equal(got, []string{testImageB}) {
		t.Fatalf("ProvenImages = %q, want only image B", got)
	}
	generation, _ := b.beginConformanceProof(testImageA, nil)
	if !b.finishConformanceProof(testImageA, generation, true) || !slices.Contains(b.ProvenImages(), testImageA) {
		t.Fatal("a retired image did not prove again")
	}

	failing := &recordingConformance{err: errors.New("disk full")}
	if err := b.RetireConformance(t.Context(), testImageB, failing); err == nil {
		t.Fatal("a failed marker append retired the image")
	}
	if slices.Contains(b.ProvenImages(), testImageB) || !slices.Contains(b.TrackedImages(), testImageB) {
		t.Fatal("a failed retirement left image B published or untracked")
	}
}

// TestConcludeHoldsTheGuardAcrossTheRecordStep pins the publish/record
// atomicity: a new pass cannot begin (and so cannot append its own record)
// while an earlier pass's conclude is still inside its record step, so the
// durable append order can never invert the supersession order.
func TestConcludeHoldsTheGuardAcrossTheRecordStep(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	gen, _ := b.beginConformanceProof(testImageA, nil)

	var mu sync.Mutex
	var order []string
	recordEntered := make(chan struct{})
	releaseRecord := make(chan struct{})
	concludeDone := make(chan struct{})
	go func() {
		defer close(concludeDone)
		published, err := b.concludeConformanceProof(testImageA, gen, true, func() error {
			close(recordEntered)
			<-releaseRecord
			mu.Lock()
			order = append(order, "record")
			mu.Unlock()
			return nil
		})
		if !published || err != nil {
			t.Errorf("conclude = (%v, %v), want published with no error", published, err)
		}
	}()

	<-recordEntered
	beginDone := make(chan struct{})
	go func() {
		defer close(beginDone)
		// Another image's begin waits too: one guard covers every image.
		_, _ = b.beginConformanceProof(testImageB, nil)
		mu.Lock()
		order = append(order, "begin")
		mu.Unlock()
	}()
	// Give a broken (guard-released) implementation the chance to let begin
	// overtake the in-flight record step; a correct one blocks begin until
	// the record completes, whatever the scheduler does here.
	time.Sleep(20 * time.Millisecond)
	close(releaseRecord)
	<-concludeDone
	<-beginDone

	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(order, []string{"record", "begin"}) {
		t.Fatalf("order = %v, want the record step to complete before a new pass begins", order)
	}
}

// TestConcludePersistsBeforePublishing pins the persist-then-publish order:
// the capabilities must stay unobservable while the durable append is in
// flight, or a concurrent admission could be enabled by the fresh
// declaration while gated against the previous pass's stale row.
func TestConcludePersistsBeforePublishing(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	gen, _ := b.beginConformanceProof(testImageA, nil)
	var observed []bool
	published, recordErr := b.concludeConformanceProof(testImageA, gen, true, func() error {
		// The record step runs under proofMu, so read the flag directly.
		observed = append(observed, b.proofs[testImageA].published)
		return nil
	})
	if !published || recordErr != nil {
		t.Fatalf("conclude = (%v, %v), want published with no error", published, recordErr)
	}
	if slices.Contains(observed, true) {
		t.Fatal("capabilities were observable while the durable append was in flight")
	}
	if !capabilitiesFor(b, testImageA).Has(exec.CapEnforcedProviderEgress) {
		t.Fatal("publication missing after a successful append")
	}
}

// TestUnattendedFloorMatchesTheClassCeiling binds ward's hand-maintained
// unattended floor to the domain's registered ceiling: for this backend
// everything provable is required, so a capability added to either
// registration point without the other is a drift this test catches.
func TestUnattendedFloorMatchesTheClassCeiling(t *testing.T) {
	t.Parallel()
	ceiling, ok := domain.ProvableCapabilities(domain.BackendFreshVMReadOnlyVolumeHandoff)
	if !ok {
		t.Fatal("fresh-vm class has no registered ceiling")
	}
	floor := domain.NewCapabilitySnapshot(unattendedCapabilities...)
	if !slices.Equal(floor, ceiling) {
		t.Fatalf("unattendedCapabilities = %v, want the class ceiling %v", floor, ceiling)
	}
}

// TestProvenCapabilitiesMatchTheClassCeiling binds ward's explicit proven set
// to the domain's registered ceiling for its class: a capability added on one
// side without the other is a drift this test catches, in both directions.
func TestProvenCapabilitiesMatchTheClassCeiling(t *testing.T) {
	t.Parallel()
	ceiling, ok := domain.ProvableCapabilities(domain.BackendFreshVMReadOnlyVolumeHandoff)
	if !ok {
		t.Fatal("fresh-vm class has no registered ceiling")
	}
	proven := domain.NewCapabilitySnapshot(provenCapabilities()...)
	if excess := domain.ExcessCapabilities(proven, ceiling); len(excess) > 0 {
		t.Errorf("ward proves %v beyond the domain ceiling", excess)
	}
	if missing := domain.MissingCapabilities(proven, ceiling); len(missing) > 0 {
		t.Errorf("domain ceiling holds %v that ward's pass never proves", missing)
	}
}

// TestRefusedCapabilitiesRefuse proves policy asking for a refuted
// capability gets a typed refusal, never a silent downgrade.
func TestRefusedCapabilitiesRefuse(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	for _, c := range refusedCapabilities {
		_, err := exec.CheckCapabilities(b, []exec.Capability{c})
		if !errors.Is(err, exec.ErrCapabilityRefused) {
			t.Errorf("CheckCapabilities(%q) = %v, want ErrCapabilityRefused", c, err)
		}
	}
}

// TestCapabilitiesImmutable proves mutating a returned set does not change
// the backend's declaration (fixed at spawn, §5.3).
func TestCapabilitiesImmutable(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	got := b.Capabilities()
	delete(got, exec.CapDetachableWorkspace)
	got[exec.CapWorkspaceSnapshot] = struct{}{}

	fresh := b.Capabilities()
	if !fresh.Has(exec.CapDetachableWorkspace) {
		t.Error("mutating a returned set removed a declared capability")
	}
	if fresh.Has(exec.CapWorkspaceSnapshot) {
		t.Error("mutating a returned set added a refused capability")
	}
}

// TestUninitializedBackendDeclaresNothing pins the zero-value contract: only
// New can construct a backend that advertises the strong handoff class.
func TestUninitializedBackendDeclaresNothing(t *testing.T) {
	t.Parallel()
	for name, b := range map[string]*Backend{
		"typed nil":  nil,
		"zero value": {},
	} {
		t.Run(name, func(t *testing.T) {
			if got := b.Capabilities(); len(got) != 0 {
				t.Errorf("Capabilities = %v, want empty", got)
			}
			if _, err := exec.CheckCapabilities(b, declaredCapabilities); !errors.Is(err, exec.ErrCapabilityRefused) {
				t.Errorf("CheckCapabilities = %v, want ErrCapabilityRefused", err)
			}
		})
	}
}

func TestNewFreezesConfigSlices(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	b, err := New(stubRuntime{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := b.cfg.ExporterCommand[0]
	cfg.ExporterCommand[0] = "caller-rewrite"
	if b.cfg.ExporterCommand[0] != want {
		t.Errorf("backend command = %q after caller mutation, want frozen %q", b.cfg.ExporterCommand[0], want)
	}
	wantEndpoint := b.cfg.ProviderEndpoints[0]
	cfg.ProviderEndpoints[0] = "caller-rewrite.example:443"
	if b.cfg.ProviderEndpoints[0] != wantEndpoint {
		t.Errorf("backend provider endpoint = %q after caller mutation, want frozen %q", b.cfg.ProviderEndpoints[0], wantEndpoint)
	}
}

func TestNewValidation(t *testing.T) {
	t.Parallel()
	if _, err := New(nil, testConfig()); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("New(nil runtime) = %v, want ErrInvalidConfig", err)
	}
	bad := testConfig()
	bad.Scanner = nil
	if _, err := New(stubRuntime{}, bad); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("New(nil scanner) = %v, want ErrInvalidConfig", err)
	}
}

func TestBackendName(t *testing.T) {
	t.Parallel()
	if got := newTestBackend(t).Name(); got != "fresh_vm_read_only_volume_handoff" {
		t.Errorf("Name() = %q, want the spike's isolation class name", got)
	}
}
