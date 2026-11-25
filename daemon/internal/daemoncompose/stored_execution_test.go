package daemoncompose

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// TestStoredStoreOptionsCarryTheUnattendedPolicy pins the policy the
// installed daemon opens its store under, whatever the settings decide: the
// unattended floor, the contained credential mode, and the settings'
// recipes beside the caller's, without changing the caller's map.
func TestStoredStoreOptionsCarryTheUnattendedPolicy(t *testing.T) {
	t.Parallel()
	flagged := domain.Digest("sha256:" + strings.Repeat("a", 64))
	stored := domain.Digest("sha256:" + strings.Repeat("b", 64))
	caller := store.Options{ApprovedRecipes: map[domain.Digest]bool{flagged: true}, ExclusiveLocking: true}
	opts := storedStoreOptions(caller, map[domain.Digest]bool{stored: true})
	if floor, ok := opts.AdmissionFloors[domain.ModeUnattended]; !ok || len(opts.AdmissionFloors) != 1 ||
		!reflect.DeepEqual(floor, AdmissionCapabilitySnapshot(domain.ModeUnattended)) {
		t.Fatalf("admission floors %+v, want the unattended floor alone", opts.AdmissionFloors)
	}
	if !slices.Equal(opts.ApprovedCredentialModes, []domain.CredentialMode{domain.CredentialSubscriptionContained}) {
		t.Fatalf("credential modes %v", opts.ApprovedCredentialModes)
	}
	if !opts.ApprovedRecipes[flagged] || !opts.ApprovedRecipes[stored] || len(opts.ApprovedRecipes) != 2 ||
		!opts.ExclusiveLocking {
		t.Fatalf("options %+v", opts)
	}
	if caller.ApprovedRecipes[stored] {
		t.Fatal("the caller's recipe map gained the stored recipe")
	}
}

// TestDecideStoredExecutionStopsAtTheFirstThatApplies pins the order of the
// stored half of the start: a carried composition failure, named when a
// start check or the agent selection caused it, then an unreadable
// execution switch, then execution off, which wins over every project-level
// problem, then an unreadable project switch. The cases after it run
// through the start in cmd/freesided's stored-execution tests.
func TestDecideStoredExecutionStopsAtTheFirstThatApplies(t *testing.T) {
	t.Parallel()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	refused := errors.New("refused")
	for _, tc := range []struct {
		name     string
		switches storedSwitches
		failure  error
		want     ExecutionState
		detail   string
	}{
		{
			"carried failure",
			storedSwitches{executionRefused: refused},
			errors.New("compose"),
			ExecutionCompositionFailed, "compose",
		},
		{
			"failed start check",
			storedSwitches{},
			fmt.Errorf("run macOS verification executor conformance: %w", &HostCheckError{
				Check: "host_verification_conformance", Evidence: "slot reached the network", Fix: "repair it",
			}),
			ExecutionHostCheckFailed,
			"host_verification_conformance: slot reached the network; repair it",
		},
		{
			"unresolved agent line",
			storedSwitches{},
			fmt.Errorf("compose: %w", &RoleAdmissionError{Role: domain.RoleReviewer, Err: refused}),
			ExecutionHostCheckFailed,
			"agent_selection: role reviewer has no lineup line that resolves against the store; " +
				"adopt the agent tree at the commit the agent_tree section names with freesided auth adopt, " +
				"or set another commit with freesided settings set -section agent_tree",
		},
		{
			"unreadable execution",
			storedSwitches{executionRefused: refused, projectRefused: refused},
			nil,
			ExecutionSettingsRefused,
			"refused",
		},
		{
			"execution off",
			storedSwitches{projectRefused: refused, enabled: []domain.ProjectID{"a", "b"}},
			nil,
			ExecutionNotConfigured,
			"",
		},
		{
			"unreadable project",
			storedSwitches{executionOn: true, projectRefused: refused},
			nil,
			ExecutionSettingsRefused,
			"refused",
		},
	} {
		got, detail, _, err := decideStoredExecution(t.Context(), st, tc.switches, tc.failure)
		if err != nil || got != tc.want || detail != tc.detail {
			t.Errorf("%s: %q (%q), %v; want %q (%q)", tc.name, got, detail, err, tc.want, tc.detail)
		}
	}
}
