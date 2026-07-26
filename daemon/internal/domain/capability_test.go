package domain_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestNewCapabilitySnapshotIsCanonical(t *testing.T) {
	got := domain.NewCapabilitySnapshot(
		domain.CapReadOnlyRemount,
		domain.CapDetachableWorkspace,
		domain.CapReadOnlyRemount,
	)
	want := domain.CapabilitySnapshot{domain.CapDetachableWorkspace, domain.CapReadOnlyRemount}
	if !slices.Equal(got, want) {
		t.Fatalf("snapshot = %v, want %v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("canonical snapshot must validate: %v", err)
	}
	if empty := domain.NewCapabilitySnapshot(); empty != nil {
		t.Errorf("empty snapshot = %v, want nil (one representation for declaring nothing)", empty)
	}
}

// TestCapabilitySnapshotDetachesFromCaller pins that a snapshot cannot be
// mutated through the slice the caller passed in: the whole point of a frozen
// spawn-time declaration is that nothing later rewrites it.
func TestCapabilitySnapshotDetachesFromCaller(t *testing.T) {
	caller := []domain.RunnerCapability{domain.CapDetachableWorkspace, domain.CapPostExitExport}
	snapshot := domain.NewCapabilitySnapshot(caller...)
	caller[0] = domain.CapNetworklessExport
	if snapshot[0] != domain.CapDetachableWorkspace {
		t.Fatalf("snapshot[0] = %q, want %q: snapshot aliases the caller's array",
			snapshot[0], domain.CapDetachableWorkspace)
	}
}

func TestCapabilitySnapshotValidate(t *testing.T) {
	cases := []struct {
		name     string
		snapshot domain.CapabilitySnapshot
		wantErr  error
	}{
		{"nil", nil, nil},
		{"canonical", domain.CapabilitySnapshot{domain.CapDetachableWorkspace, domain.CapReadOnlyRemount}, nil},
		{"empty non-nil", domain.CapabilitySnapshot{}, domain.ErrCapabilitiesNotCanonical},
		{"unsorted", domain.CapabilitySnapshot{domain.CapReadOnlyRemount, domain.CapDetachableWorkspace}, domain.ErrCapabilitiesNotCanonical},
		{"duplicate", domain.CapabilitySnapshot{domain.CapPostExitExport, domain.CapPostExitExport}, domain.ErrDuplicate},
		{"unknown member", domain.CapabilitySnapshot{domain.RunnerCapability("supports_teleportation")}, domain.ErrInvalidRunnerCapability},
		{"zero member", domain.CapabilitySnapshot{domain.RunnerCapability("")}, domain.ErrInvalidRunnerCapability},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.snapshot.Validate()
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestMissingCapabilities(t *testing.T) {
	declared := domain.NewCapabilitySnapshot(domain.CapDetachableWorkspace, domain.CapPostExitExport)
	cases := []struct {
		name  string
		floor []domain.RunnerCapability
		want  []domain.RunnerCapability
	}{
		{"satisfied", []domain.RunnerCapability{domain.CapPostExitExport}, nil},
		{"empty floor", nil, nil},
		{
			"unmet",
			[]domain.RunnerCapability{domain.CapNetworklessExport, domain.CapDetachableWorkspace},
			[]domain.RunnerCapability{domain.CapNetworklessExport},
		},
		{
			// A policy typo must never widen into an accidental pass.
			"invalid floor member",
			[]domain.RunnerCapability{domain.RunnerCapability("supports_teleportation")},
			[]domain.RunnerCapability{domain.RunnerCapability("supports_teleportation")},
		},
		{
			"duplicates collapse",
			[]domain.RunnerCapability{domain.CapNetworklessExport, domain.CapNetworklessExport},
			[]domain.RunnerCapability{domain.CapNetworklessExport},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := domain.MissingCapabilities(declared, tc.floor); !slices.Equal(got, tc.want) {
				t.Fatalf("MissingCapabilities = %v, want %v", got, tc.want)
			}
		})
	}
	// A nil declaration satisfies nothing: the fail-closed direction.
	if got := domain.MissingCapabilities(nil, []domain.RunnerCapability{domain.CapPostExitExport}); len(got) != 1 {
		t.Fatalf("MissingCapabilities(nil, floor) = %v, want the whole floor", got)
	}
}
