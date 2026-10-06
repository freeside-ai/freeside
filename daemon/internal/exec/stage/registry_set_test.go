package stage

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

const registrySetPolicy = `[{"key":"execution.egress_profile","value":"provider_registry"},` +
	`{"key":"execution.registry_set","value":"[\"proxy.golang.org\",\"registry.npmjs.org\"]"},` +
	`{"key":"paths","value":"daemon/**"}]`

func TestHandoffSpecCarriesTheDeclaredRegistrySet(t *testing.T) {
	t.Parallel()
	hs, err := handoffSpecWithProfileAndPolicy(t, domain.EgressProviderRegistry, registrySetPolicy)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"proxy.golang.org", "registry.npmjs.org"}
	if hs.Agent.EgressProfile != domain.EgressProviderRegistry || !slices.Equal(hs.RegistryHosts, want) {
		t.Fatalf("handoff = %s with registries %v, want provider_registry with %v",
			hs.Agent.EgressProfile, hs.RegistryHosts, want)
	}
}

// A policy may declare a set without the run being admitted under the wider
// profile; the handoff then carries none, and the proxy stays provider-only.
func TestHandoffSpecCarriesNoRegistrySetUnderProviderOnly(t *testing.T) {
	t.Parallel()
	hs, err := handoffSpecWithProfileAndPolicy(t, domain.EgressProviderOnly, registrySetPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if hs.Agent.EgressProfile != domain.EgressProviderOnly || len(hs.RegistryHosts) != 0 {
		t.Fatalf("handoff = %s with registries %v, want provider_only with none",
			hs.Agent.EgressProfile, hs.RegistryHosts)
	}
}

func TestHandoffSpecRefusesProviderRegistryWithoutAValidSet(t *testing.T) {
	t.Parallel()
	for name, policy := range map[string]string{
		"no set": `[{"key":"paths","value":"daemon/**"}]`,
		"empty set": `[{"key":"execution.registry_set","value":"[]"},` +
			`{"key":"paths","value":"daemon/**"}]`,
		"unsorted set": `[{"key":"execution.registry_set",` +
			`"value":"[\"registry.npmjs.org\",\"proxy.golang.org\"]"},{"key":"paths","value":"daemon/**"}]`,
		"host with a port": `[{"key":"execution.registry_set","value":"[\"registry.npmjs.org:8443\"]"},` +
			`{"key":"paths","value":"daemon/**"}]`,
		"not an array": `[{"key":"execution.registry_set","value":"registry.npmjs.org"},` +
			`{"key":"paths","value":"daemon/**"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := handoffSpecWithProfileAndPolicy(t, domain.EgressProviderRegistry, policy)
			if !errors.Is(err, ErrUnsupportedStart) {
				t.Fatalf("handoffSpec = %v, want ErrUnsupportedStart", err)
			}
		})
	}
}

// A provider_registry intent survives the reconstruction gate, so a restart
// recovers the run instead of refusing its own record.
func TestProviderRegistryIntentPassesTheReconstructionGate(t *testing.T) {
	t.Parallel()
	d := newTestDriver(t, &stubGate{}, newStubExports())
	spec := testStartSpec()
	spec.EgressProfile = domain.EgressProviderRegistry
	inputs := stageInputsWithBodies(t, &spec,
		[]byte("# Work item\nDo the thing.\n"),
		[]byte("You are the Phase 1A implementer.\n"),
		[]byte(registrySetPolicy),
	)
	instructions, err := ward.VendorInstructionsFromStageInputs(inputs)
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := renderPrompt(inputs)
	if err != nil {
		t.Fatal(err)
	}
	in := intent{
		InvocationID: testInvoke, RunID: testRunIDFor(testInvoke), Phase: phaseRunning,
		Spec: spec, Seed: filepath.Join(d.seedRoot, testRunIDFor(testInvoke)),
		Prompt: prompt, Inputs: durableInputsFrom(inputs),
		Instructions: instructions, RecordedAt: fixedNow, CommitDate: fixedNow,
	}
	if err := d.saveIntent(in); err != nil {
		t.Fatalf("save intent: %v", err)
	}
	got, err := d.loadIntent(context.Background(), testInvoke)
	if err != nil {
		t.Fatalf("load provider_registry intent: %v", err)
	}
	if got.Spec.EgressProfile != domain.EgressProviderRegistry {
		t.Fatalf("reloaded profile = %q, want provider_registry", got.Spec.EgressProfile)
	}
}
