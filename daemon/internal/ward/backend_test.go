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

// refusedCapabilities must never be declared: both are refuted on the
// reference runtime (the same-VM fallback class and volume snapshots).
var refusedCapabilities = []exec.Capability{
	exec.CapCredentialVolumeDetach,
	exec.CapWorkspaceSnapshot,
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
	b := newTestBackend(t)
	if b.Capabilities().Has(exec.CapNetworklessExport) {
		t.Fatal("new backend declared supports_networkless_export before conformance")
	}
	if _, err := exec.CheckCapabilities(b, conformancePendingCapabilities); !errors.Is(err, exec.ErrCapabilityRefused) {
		t.Fatalf("unproven networkless capability = %v, want ErrCapabilityRefused", err)
	}
	b.networkless.Store(true)
	if _, err := exec.CheckCapabilities(b, append(declaredCapabilities, exec.CapNetworklessExport)); err != nil {
		t.Fatalf("proven networkless capability = %v, want admission", err)
	}
}

func TestRestoreConformancePublishesDurableCapabilitiesOnce(t *testing.T) {
	b := newTestBackend(t)
	record, err := domain.NewBackendConformance(domain.BackendConformanceInput{
		Backend:             domain.BackendFreshVMReadOnlyVolumeHandoff,
		Outcome:             domain.ConformancePassed,
		ConfigurationDigest: b.ConfigurationDigest(),
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
		t.Fatalf("NewBackendConformance: %v", err)
	}
	record.Generation = 7
	if err := b.RestoreConformance(record); err != nil {
		t.Fatalf("RestoreConformance: %v", err)
	}
	for _, capability := range conformancePendingCapabilities {
		if !b.Capabilities().Has(capability) {
			t.Errorf("restored backend does not declare %q", capability)
		}
	}

	if _, err := b.beginConformanceProof(nil); err != nil {
		t.Fatalf("begin proof: %v", err)
	}
	if err := b.RestoreConformance(record); err == nil {
		t.Fatal("durable success was restored after a live proof began")
	}
	for _, capability := range conformancePendingCapabilities {
		if b.Capabilities().Has(capability) {
			t.Errorf("stale restore resurrected %q after a live proof began", capability)
		}
	}
}

func TestRestoreConformanceRejectsUnpersistedOrFailedRecords(t *testing.T) {
	t.Parallel()
	configurationDigest := newTestBackend(t).ConfigurationDigest()
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
			if err := b.RestoreConformance(tc.record); err == nil {
				t.Fatal("RestoreConformance accepted a record that cannot grant capabilities")
			}
			for _, capability := range conformancePendingCapabilities {
				if b.Capabilities().Has(capability) {
					t.Errorf("rejected record published %q", capability)
				}
			}
		})
	}
}

func TestRestoreConformanceRejectsAnotherBackendConfiguration(t *testing.T) {
	t.Parallel()
	proved := newTestBackend(t)
	record, err := domain.NewBackendConformance(domain.BackendConformanceInput{
		Backend:             domain.BackendFreshVMReadOnlyVolumeHandoff,
		Outcome:             domain.ConformancePassed,
		ConfigurationDigest: proved.ConfigurationDigest(),
		Capabilities:        domain.NewCapabilitySnapshot(provenCapabilities()...),
		ProvedAt:            time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("NewBackendConformance: %v", err)
	}
	record.Generation = 7

	cfg := testConfig()
	cfg.ExporterImage = "example.test/other-exporter@sha256:" + strings.Repeat("2", 64)
	active, err := New(stubRuntime{}, cfg)
	if err != nil {
		t.Fatalf("New(active): %v", err)
	}
	if err := active.RestoreConformance(record); err == nil {
		t.Fatal("proof from another exporter configuration was restored")
	}
	for _, capability := range conformancePendingCapabilities {
		if active.Capabilities().Has(capability) {
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
	b := newTestBackend(t)
	older, _ := b.beginConformanceProof(nil)
	newer, _ := b.beginConformanceProof(nil)
	if !b.finishConformanceProof(newer, false) {
		t.Fatal("current generation reported itself superseded")
	}
	if b.finishConformanceProof(older, true) {
		t.Fatal("superseded generation reported itself current")
	}
	for _, c := range conformancePendingCapabilities {
		if b.Capabilities().Has(c) {
			t.Fatalf("older successful proof overrode a newer failed proof for %q", c)
		}
	}

	latest, _ := b.beginConformanceProof(nil)
	if !b.finishConformanceProof(latest, true) {
		t.Fatal("latest proof reported itself superseded")
	}
	for _, c := range conformancePendingCapabilities {
		if !b.Capabilities().Has(c) {
			t.Fatalf("latest successful proof did not publish %q", c)
		}
	}
}

// TestConcludeHoldsTheGuardAcrossTheRecordStep pins the publish/record
// atomicity: a new pass cannot begin (and so cannot append its own record)
// while an earlier pass's conclude is still inside its record step, so the
// durable append order can never invert the supersession order.
func TestConcludeHoldsTheGuardAcrossTheRecordStep(t *testing.T) {
	b := newTestBackend(t)
	gen, _ := b.beginConformanceProof(nil)

	var mu sync.Mutex
	var order []string
	recordEntered := make(chan struct{})
	releaseRecord := make(chan struct{})
	concludeDone := make(chan struct{})
	go func() {
		defer close(concludeDone)
		published, err := b.concludeConformanceProof(gen, true, func() error {
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
		_, _ = b.beginConformanceProof(nil)
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
	b := newTestBackend(t)
	gen, _ := b.beginConformanceProof(nil)
	var observed []bool
	published, recordErr := b.concludeConformanceProof(gen, true, func() error {
		observed = append(observed, b.networkless.Load(), b.providerEgress.Load())
		return nil
	})
	if !published || recordErr != nil {
		t.Fatalf("conclude = (%v, %v), want published with no error", published, recordErr)
	}
	if slices.Contains(observed, true) {
		t.Fatal("capabilities were observable while the durable append was in flight")
	}
	if !b.Capabilities().Has(exec.CapEnforcedProviderEgress) {
		t.Fatal("publication missing after a successful append")
	}
}

// TestUnattendedFloorMatchesTheClassCeiling binds ward's hand-maintained
// unattended floor to the domain's registered ceiling: for this backend
// everything provable is required, so a capability added to either
// registration point without the other is a drift this test catches.
func TestUnattendedFloorMatchesTheClassCeiling(t *testing.T) {
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
	if got := newTestBackend(t).Name(); got != "fresh_vm_read_only_volume_handoff" {
		t.Errorf("Name() = %q, want the spike's isolation class name", got)
	}
}
