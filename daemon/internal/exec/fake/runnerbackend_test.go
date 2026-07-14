package fake_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
)

// TestRunnerBackendCapabilityCheck exercises the §5.7 check through the
// permanent declaring-side fake (the exhaustive per-capability table lives
// with the exec package): a declared subset passes its own minimum and is
// refused above it, with the refusal naming this backend.
func TestRunnerBackendCapabilityCheck(t *testing.T) {
	b := fake.RunnerBackend{
		BackendName: "fake-runner",
		Caps: exec.NewCapabilitySet(
			exec.CapPostExitExport,
			exec.CapReadOnlyRemount,
		),
	}

	if _, err := exec.CheckCapabilities(b, []exec.Capability{exec.CapPostExitExport}); err != nil {
		t.Fatalf("declared minimum = %v, want pass", err)
	}

	_, err := exec.CheckCapabilities(b, exec.AllCapabilities)
	var refusal *exec.CapabilityRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("want *CapabilityRefusal, got %v", err)
	}
	if refusal.Backend != "fake-runner" {
		t.Errorf("refusal backend = %q, want %q", refusal.Backend, "fake-runner")
	}
	wantMissing := []exec.Capability{
		exec.CapCredentialVolumeDetach,
		exec.CapDetachableWorkspace,
		exec.CapWorkspaceSnapshot,
	}
	if !slices.Equal(refusal.Missing, wantMissing) {
		t.Errorf("refusal missing = %v, want %v", refusal.Missing, wantMissing)
	}
}

// TestRunnerBackendCapabilitiesAreCopied proves Capabilities() hands back an
// independent set (issue #39): mutating the returned map does not change the
// fake's declaration, so a later read stays fixed at what the test declared.
func TestRunnerBackendCapabilitiesAreCopied(t *testing.T) {
	b := fake.RunnerBackend{
		BackendName: "fake-runner",
		Caps:        exec.NewCapabilitySet(exec.CapPostExitExport),
	}

	got := b.Capabilities()
	got[exec.CapDetachableWorkspace] = struct{}{} // widen the returned copy
	delete(got, exec.CapPostExitExport)           // narrow the returned copy

	again := b.Capabilities()
	if !again.Has(exec.CapPostExitExport) {
		t.Error("mutating a returned set removed a declared capability from the backend")
	}
	if again.Has(exec.CapDetachableWorkspace) {
		t.Error("mutating a returned set added a capability to the backend")
	}
}
