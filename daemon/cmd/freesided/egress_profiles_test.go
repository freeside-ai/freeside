package main

import (
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// The composition's enforceable set is what admission lets policy or a
// capability manifest select, so it is pinned exactly: provider_registry is
// selectable because the ward proxy enforces it, and provider_web_read is not
// because ward refuses that handoff.
func TestWardCompositionEnforcesExactlyTheProxyProfiles(t *testing.T) {
	t.Parallel()
	want := []domain.EgressProfile{domain.EgressProviderOnly, domain.EgressProviderRegistry}
	if got := wardEnforceableEgressProfiles(); !slices.Equal(got, want) {
		t.Fatalf("enforceable egress profiles = %v, want exactly %v", got, want)
	}
}
