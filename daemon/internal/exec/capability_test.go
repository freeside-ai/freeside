package exec_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

// backend is a minimal declaring backend for capability tests; the permanent
// fake lives in exec/fake and is exercised in its own package's tests.
type backend struct {
	name string
	caps exec.CapabilitySet
}

func (b backend) Name() string                     { return b.name }
func (b backend) Capabilities() exec.CapabilitySet { return b.caps }

// TestCheckCapabilities is acceptance fixture 2: table-driven over the five
// §5.7 capabilities, an unmet policy minimum yields a typed refusal naming
// every missing capability, never a silent downgrade or partial pass.
func TestCheckCapabilities(t *testing.T) {
	cases := []struct {
		name        string
		declared    []exec.Capability
		minimum     []exec.Capability
		wantMissing []exec.Capability // nil means the check must pass
	}{
		{
			name:     "full set meets full minimum",
			declared: exec.AllCapabilities,
			minimum:  exec.AllCapabilities,
		},
		{
			name:     "empty minimum always met",
			declared: nil,
			minimum:  nil,
		},
		{
			name:     "superset declaration meets narrow minimum",
			declared: exec.AllCapabilities,
			minimum:  []exec.Capability{exec.CapPostExitExport},
		},
		{
			name: "missing detachable workspace",
			declared: []exec.Capability{
				exec.CapPostExitExport, exec.CapReadOnlyRemount,
				exec.CapCredentialVolumeDetach, exec.CapWorkspaceSnapshot,
			},
			minimum:     exec.AllCapabilities,
			wantMissing: []exec.Capability{exec.CapDetachableWorkspace},
		},
		{
			name: "missing post-exit export",
			declared: []exec.Capability{
				exec.CapDetachableWorkspace, exec.CapReadOnlyRemount,
				exec.CapCredentialVolumeDetach, exec.CapWorkspaceSnapshot,
			},
			minimum:     exec.AllCapabilities,
			wantMissing: []exec.Capability{exec.CapPostExitExport},
		},
		{
			name: "missing read-only remount",
			declared: []exec.Capability{
				exec.CapDetachableWorkspace, exec.CapPostExitExport,
				exec.CapCredentialVolumeDetach, exec.CapWorkspaceSnapshot,
			},
			minimum:     exec.AllCapabilities,
			wantMissing: []exec.Capability{exec.CapReadOnlyRemount},
		},
		{
			name: "missing credential volume detach",
			declared: []exec.Capability{
				exec.CapDetachableWorkspace, exec.CapPostExitExport,
				exec.CapReadOnlyRemount, exec.CapWorkspaceSnapshot,
			},
			minimum:     exec.AllCapabilities,
			wantMissing: []exec.Capability{exec.CapCredentialVolumeDetach},
		},
		{
			name: "missing workspace snapshot",
			declared: []exec.Capability{
				exec.CapDetachableWorkspace, exec.CapPostExitExport,
				exec.CapReadOnlyRemount, exec.CapCredentialVolumeDetach,
			},
			minimum:     exec.AllCapabilities,
			wantMissing: []exec.Capability{exec.CapWorkspaceSnapshot},
		},
		{
			name:     "empty declaration misses everything, sorted",
			declared: nil,
			minimum:  exec.AllCapabilities,
			// Sorted lexically, not in AllCapabilities order.
			wantMissing: []exec.Capability{
				exec.CapCredentialVolumeDetach,
				exec.CapDetachableWorkspace,
				exec.CapPostExitExport,
				exec.CapReadOnlyRemount,
				exec.CapWorkspaceSnapshot,
			},
		},
		{
			name:        "unknown capability in the minimum fails closed",
			declared:    exec.AllCapabilities,
			minimum:     []exec.Capability{"supports_time_travel"},
			wantMissing: []exec.Capability{"supports_time_travel"},
		},
		{
			name:        "duplicate missing capability reported once",
			declared:    nil,
			minimum:     []exec.Capability{exec.CapPostExitExport, exec.CapPostExitExport},
			wantMissing: []exec.Capability{exec.CapPostExitExport},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := backend{name: "test-backend", caps: exec.NewCapabilitySet(tc.declared...)}
			err := exec.CheckCapabilities(b, tc.minimum)

			if tc.wantMissing == nil {
				if err != nil {
					t.Fatalf("want pass, got %v", err)
				}
				return
			}
			if !errors.Is(err, exec.ErrCapabilityRefused) {
				t.Fatalf("want ErrCapabilityRefused class, got %v", err)
			}
			var refusal *exec.CapabilityRefusal
			if !errors.As(err, &refusal) {
				t.Fatalf("want *CapabilityRefusal, got %T", err)
			}
			if refusal.Backend != "test-backend" {
				t.Errorf("refusal backend = %q, want %q", refusal.Backend, "test-backend")
			}
			if !slices.Equal(refusal.Missing, tc.wantMissing) {
				t.Errorf("refusal missing = %v, want %v", refusal.Missing, tc.wantMissing)
			}
		})
	}
}
