package domain_test

import (
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestProvableCapabilities pins each registered class's ceiling: every member
// is registered vocabulary, the refuted capabilities are excluded, and an
// unregistered class has no ceiling at all.
func TestProvableCapabilities(t *testing.T) {
	for _, class := range domain.AllRunnerBackendClasses {
		ceiling, ok := domain.ProvableCapabilities(class)
		if !ok {
			t.Fatalf("ProvableCapabilities(%q) = not registered, want a ceiling", class)
		}
		if err := ceiling.Validate(); err != nil {
			t.Fatalf("ProvableCapabilities(%q) ceiling invalid: %v", class, err)
		}
	}

	freshVM, _ := domain.ProvableCapabilities(domain.BackendFreshVMReadOnlyVolumeHandoff)
	want := domain.NewCapabilitySnapshot(
		domain.CapDetachableWorkspace,
		domain.CapPostExitExport,
		domain.CapReadOnlyRemount,
		domain.CapNetworklessExport,
		domain.CapEnforcedProviderEgress,
	)
	if !slices.Equal(freshVM, want) {
		t.Errorf("fresh-vm ceiling = %v, want %v", freshVM, want)
	}
	for _, refuted := range []domain.RunnerCapability{
		domain.CapCredentialVolumeDetach, domain.CapWorkspaceSnapshot,
	} {
		if freshVM.Has(refuted) {
			t.Errorf("fresh-vm ceiling includes refuted capability %q", refuted)
		}
	}

	if ceiling, ok := domain.ProvableCapabilities("no_such_class"); ok || ceiling != nil {
		t.Errorf("ProvableCapabilities(unknown) = %v, %v; want nil, false", ceiling, ok)
	}
	if ceiling, ok := domain.ProvableCapabilities(""); ok || ceiling != nil {
		t.Errorf("ProvableCapabilities(zero) = %v, %v; want nil, false", ceiling, ok)
	}
}

func TestExcessCapabilities(t *testing.T) {
	allowed := domain.NewCapabilitySnapshot(
		domain.CapDetachableWorkspace, domain.CapNetworklessExport,
	)
	cases := []struct {
		name    string
		claimed domain.CapabilitySnapshot
		allowed domain.CapabilitySnapshot
		want    []domain.RunnerCapability
	}{
		{name: "nil claim has no excess", claimed: nil, allowed: allowed, want: nil},
		{
			name:    "claim within the ceiling",
			claimed: domain.NewCapabilitySnapshot(domain.CapDetachableWorkspace),
			allowed: allowed,
			want:    nil,
		},
		{
			name:    "exact ceiling is not excess",
			claimed: allowed,
			allowed: allowed,
			want:    nil,
		},
		{
			name: "members beyond the ceiling, sorted",
			claimed: domain.NewCapabilitySnapshot(
				domain.CapWorkspaceSnapshot,
				domain.CapDetachableWorkspace,
				domain.CapCredentialVolumeDetach,
			),
			allowed: allowed,
			want: []domain.RunnerCapability{
				domain.CapCredentialVolumeDetach, domain.CapWorkspaceSnapshot,
			},
		},
		{
			name:    "unregistered name is excess even when allowed lists it",
			claimed: domain.CapabilitySnapshot{"supports_time_travel"},
			allowed: domain.CapabilitySnapshot{"supports_time_travel"},
			want:    []domain.RunnerCapability{"supports_time_travel"},
		},
		{
			name:    "everything is excess against an empty ceiling",
			claimed: domain.NewCapabilitySnapshot(domain.CapNetworklessExport),
			allowed: nil,
			want:    []domain.RunnerCapability{domain.CapNetworklessExport},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := domain.ExcessCapabilities(tc.claimed, tc.allowed)
			if !slices.Equal(got, tc.want) {
				t.Errorf("ExcessCapabilities(%v, %v) = %v, want %v", tc.claimed, tc.allowed, got, tc.want)
			}
		})
	}
}
