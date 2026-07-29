package domain_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

var provedAt = time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)

// validPassedConformance is the fixture the failure table mutates: a passed
// record at the fresh-vm class's full ceiling.
func validPassedConformance(t *testing.T) domain.BackendConformance {
	t.Helper()
	ceiling, _ := domain.ProvableCapabilities(domain.BackendFreshVMReadOnlyVolumeHandoff)
	c, err := domain.NewBackendConformance(domain.BackendConformanceInput{
		Backend:             domain.BackendFreshVMReadOnlyVolumeHandoff,
		Outcome:             domain.ConformancePassed,
		ConfigurationDigest: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		Capabilities:        ceiling,
		ProvedAt:            provedAt,
	})
	if err != nil {
		t.Fatalf("NewBackendConformance: %v", err)
	}
	return c
}

func TestBackendConformanceValidate(t *testing.T) {
	valid := validPassedConformance(t)
	if valid.Generation != 0 {
		t.Errorf("constructor set Generation = %d, want 0 (store-assigned)", valid.Generation)
	}

	failed, err := domain.NewBackendConformance(domain.BackendConformanceInput{
		Backend:             domain.BackendFreshVMReadOnlyVolumeHandoff,
		Outcome:             domain.ConformanceFailed,
		ConfigurationDigest: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		ProvedAt:            provedAt,
	})
	if err != nil {
		t.Fatalf("failed record with nil capabilities: %v", err)
	}
	if failed.Capabilities != nil {
		t.Errorf("failed record capabilities = %v, want nil", failed.Capabilities)
	}

	cases := []struct {
		name    string
		mutate  func(*domain.BackendConformance)
		wantErr error
	}{
		{
			name:    "unknown backend class",
			mutate:  func(c *domain.BackendConformance) { c.Backend = "no_such_class" },
			wantErr: domain.ErrInvalidRunnerBackendClass,
		},
		{
			name:    "zero backend class",
			mutate:  func(c *domain.BackendConformance) { c.Backend = "" },
			wantErr: domain.ErrInvalidRunnerBackendClass,
		},
		{
			name:    "unknown outcome",
			mutate:  func(c *domain.BackendConformance) { c.Outcome = "pending" },
			wantErr: domain.ErrInvalidConformanceOutcome,
		},
		{
			name: "invalid configuration digest",
			mutate: func(c *domain.BackendConformance) {
				c.ConfigurationDigest = "sha256:not-a-digest"
			},
			wantErr: domain.ErrConformanceConfigurationUnbound,
		},
		{
			name: "failed pass carrying a capability set",
			mutate: func(c *domain.BackendConformance) {
				c.Outcome = domain.ConformanceFailed
			},
			wantErr: domain.ErrConformanceCapabilitiesWithoutPass,
		},
		{
			name: "superseding marker carrying a capability set",
			mutate: func(c *domain.BackendConformance) {
				c.Outcome = domain.ConformanceSuperseded
			},
			wantErr: domain.ErrConformanceCapabilitiesWithoutPass,
		},
		{
			name: "claim beyond the class ceiling",
			mutate: func(c *domain.BackendConformance) {
				c.Capabilities = domain.NewCapabilitySnapshot(
					append(c.Capabilities.Clone(), domain.CapCredentialVolumeDetach)...)
			},
			wantErr: domain.ErrConformanceOverclaim,
		},
		{
			name: "unregistered capability name in the claim",
			mutate: func(c *domain.BackendConformance) {
				c.Capabilities = domain.NewCapabilitySnapshot(
					append(c.Capabilities.Clone(), "supports_time_travel")...)
			},
			// The snapshot's own membership check fires before the ceiling
			// comparison; either way the claim fails closed.
			wantErr: domain.ErrInvalidRunnerCapability,
		},
		{
			name: "non-canonical capability snapshot",
			mutate: func(c *domain.BackendConformance) {
				c.Capabilities = domain.CapabilitySnapshot{
					domain.CapNetworklessExport, domain.CapDetachableWorkspace,
				}
			},
			wantErr: domain.ErrCapabilitiesNotCanonical,
		},
		{
			name:    "zero proved_at",
			mutate:  func(c *domain.BackendConformance) { c.ProvedAt = time.Time{} },
			wantErr: domain.ErrMissingTimestamp,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := valid
			c.Capabilities = valid.Capabilities.Clone()
			tc.mutate(&c)
			if err := c.Validate(); !errors.Is(err, tc.wantErr) {
				t.Errorf("Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

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
