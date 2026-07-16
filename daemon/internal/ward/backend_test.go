package ward

import (
	"errors"
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

// declaredCapabilities is the frozen declaration list from issue #76;
// backend_test asserts the backend matches it exactly, member for member.
var declaredCapabilities = []exec.Capability{
	exec.CapDetachableWorkspace,
	exec.CapPostExitExport,
	exec.CapReadOnlyRemount,
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
		if !wantDeclared && !wantRefused {
			t.Errorf("capability %q is in exec.AllCapabilities but neither declared nor refused here; place it", c)
		}
		if declared != wantDeclared {
			t.Errorf("capability %q declared = %v, want %v", c, declared, wantDeclared)
		}
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
