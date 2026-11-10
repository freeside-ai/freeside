package main

import (
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

// TestClaudeStoreOptionsCarryTheSelectedModePolicy is the regression for a
// daemon whose own admissions the store would refuse: the persistence gate
// re-checks every recorded admission against the operator's policy, and an
// unset floor means "no policy configured", which fails closed.
func TestClaudeStoreOptionsCarryTheSelectedModePolicy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		mode  domain.OperatingMode
		floor []domain.RunnerCapability
	}{
		{domain.ModeAttendedDev, []domain.RunnerCapability{
			domain.CapDetachableWorkspace,
			domain.CapPostExitExport,
			domain.CapReadOnlyRemount,
		}},
		{domain.ModeUnattended, []domain.RunnerCapability{
			domain.CapDetachableWorkspace,
			domain.CapPostExitExport,
			domain.CapReadOnlyRemount,
			domain.CapNetworklessExport,
			domain.CapEnforcedProviderEgress,
		}},
	}
	for _, test := range tests {
		test := test
		t.Run(string(test.mode), func(t *testing.T) {
			t.Parallel()
			cfg := config{
				DBPath: "/var/freeside/freeside.db",
				Claude: &daemoncompose.ClaudeDriverConfig{OperatingMode: test.mode},
			}
			opts, err := cfg.storeOptions()
			if err != nil {
				t.Fatalf("storeOptions: %v", err)
			}
			floor, ok := opts.AdmissionFloors[test.mode]
			if !ok {
				t.Fatalf("claude mode configured no %s admission floor", test.mode)
			}
			if len(floor) != len(test.floor) {
				t.Fatalf("configured floor = %v, want %v", floor, test.floor)
			}
			for _, capability := range test.floor {
				if !floor.Has(capability) {
					t.Errorf("configured floor lacks %q", capability)
				}
			}
			if !slices.Equal(daemoncompose.AdmissionFloor(test.mode), test.floor) {
				t.Errorf("engine floor = %v, want %v", daemoncompose.AdmissionFloor(test.mode), test.floor)
			}
			if !slices.Contains(opts.ApprovedCredentialModes, domain.CredentialSubscriptionContained) {
				t.Errorf("approved credential modes = %v, want subscription_contained",
					opts.ApprovedCredentialModes)
			}
		})
	}
	if slices.Contains(daemoncompose.AttendedAdmissionFloor, exec.CapNetworklessExport) ||
		slices.Contains(daemoncompose.AttendedAdmissionFloor, exec.CapEnforcedProviderEgress) {
		t.Fatal("attended_dev floor includes unattended-only conformance capabilities")
	}

	// Fake mode keeps the walking skeleton's exact store policy.
	fakeOpts, err := (config{DBPath: "/var/freeside/freeside.db"}).storeOptions()
	if err != nil {
		t.Fatalf("fake storeOptions: %v", err)
	}
	if fakeOpts.AdmissionFloors != nil || fakeOpts.ApprovedCredentialModes != nil {
		t.Errorf("fake mode store options changed: %#v", fakeOpts)
	}
}

func TestClaudeDriverConfigRejectsUnscopedAllowlistGlobs(t *testing.T) {
	t.Parallel()
	for _, pattern := range []string{
		"**", "**/*", "**/**", "*/**", "?*", "[a-z]*", "/daemon/**", "../daemon/**",
	} {
		t.Run(pattern, func(t *testing.T) {
			if engine.ExplicitAllowedPaths([]string{pattern}) {
				t.Fatalf("engine.ExplicitAllowedPaths(%q) = true", pattern)
			}
		})
	}
	for _, pattern := range []string{"daemon/**", "docs/*.md", "README.md"} {
		t.Run("accept "+pattern, func(t *testing.T) {
			if !engine.ExplicitAllowedPaths([]string{pattern}) {
				t.Fatalf("engine.ExplicitAllowedPaths(%q) = false", pattern)
			}
		})
	}
}
