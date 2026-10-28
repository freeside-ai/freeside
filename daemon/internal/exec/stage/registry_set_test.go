package stage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"testing/synctest"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
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

func registryInputs(t *testing.T, spec *exec.StartSpec) exec.StageInputs {
	t.Helper()
	spec.EgressProfile = domain.EgressProviderRegistry
	return stageInputsWithBodies(t, spec, []byte("work"), []byte("prompt"), []byte(registrySetPolicy))
}

func registryIntent(t *testing.T, d *Driver) intent {
	t.Helper()
	in := testHandoffIntent(t, d)
	inputs := registryInputs(t, &in.Spec)
	in.Inputs = durableInputsFrom(inputs)
	var err error
	in.Prompt, err = renderPrompt(inputs)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

func TestRegistryStartRecordsOnlyItsRepositoryAndRecoveryReplaysIt(t *testing.T) {
	t.Parallel()
	for _, repositoryID := range []int64{42, 43} {
		t.Run(strconv.FormatInt(repositoryID, 10), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := context.Background()
				gate := &stubGate{handoffFn: func(ward.HandoffSpec) (*ward.HandoffResult, error) {
					return nil, errors.New("leave the running intent for recovery")
				}}
				d := newTestDriver(t, gate, newStubExports())
				t.Cleanup(func() { _ = d.Close(context.Background()) })
				adopted := map[int64][]string{42: {"pypi.org", "proxy.golang.org"}}
				d.adoptedHosts = func(_ context.Context, id int64) ([]string, error) {
					if id != repositoryID {
						t.Errorf("adoption lookup repository = %d, want %d", id, repositoryID)
					}
					return slices.Clone(adopted[id]), nil
				}
				spec := testStartSpec()
				spec.Base.RepositoryID = repositoryID
				inputs := registryInputs(t, &spec)
				if err := d.StartWithInputs(ctx, testInvoke, spec,
					func(context.Context) (exec.StageInputs, error) { return inputs, nil }); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				want := []string{"proxy.golang.org", "registry.npmjs.org"}
				if repositoryID == 42 {
					want = []string{"proxy.golang.org", "pypi.org", "registry.npmjs.org"}
				}
				if got := gate.lastSpec(t).RegistryHosts; !slices.Equal(got, want) {
					t.Fatalf("handoff registries = %v, want %v", got, want)
				}
				adopted[42] = append(adopted[42], "sum.golang.org")
				in, err := d.loadIntent(ctx, testInvoke)
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(in.RegistryHosts, want) {
					t.Fatalf("durable registries = %v, want %v", in.RegistryHosts, want)
				}
				recoveryStop := errors.New("recovery spec captured")
				var recovered []string
				gate.recoverFn = func(_ string, hs ward.HandoffSpec) (*ward.RecoveryResult, error) {
					recovered = hs.RegistryHosts
					return nil, recoveryStop
				}
				if err := d.recoverRunning(ctx, in); !errors.Is(err, recoveryStop) {
					t.Fatalf("recoverRunning = %v", err)
				}
				if !slices.Equal(recovered, want) {
					t.Fatalf("recovery registries = %v, want original %v", recovered, want)
				}
			})
		})
	}
}

func TestHandoffRegistryRecordIsRegated(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		hosts   []string
		noHook  bool
		readErr bool
		wantErr bool
	}{
		{name: "legacy record", noHook: true},
		{name: "declared record needs no hook", hosts: []string{"proxy.golang.org", "registry.npmjs.org"}, noHook: true},
		{name: "adopted host", hosts: []string{"proxy.golang.org", "pypi.org", "registry.npmjs.org"}},
		{name: "missing declared host", hosts: []string{"pypi.org", "registry.npmjs.org"}, wantErr: true},
		{name: "foreign adoption", hosts: []string{"proxy.golang.org", "registry.npmjs.org", "sum.golang.org"}, wantErr: true},
		{name: "missing hook", hosts: []string{"proxy.golang.org", "pypi.org", "registry.npmjs.org"}, noHook: true, wantErr: true},
		{name: "store unavailable", hosts: []string{"proxy.golang.org", "pypi.org", "registry.npmjs.org"}, readErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestDriver(t, &stubGate{}, newStubExports())
			readErr := errors.New("store unavailable")
			if !tc.noHook {
				d.adoptedHosts = func(_ context.Context, repositoryID int64) ([]string, error) {
					if repositoryID != 42 {
						t.Fatalf("lookup for repository %d, want 42", repositoryID)
					}
					if tc.readErr {
						return nil, readErr
					}
					return []string{"pypi.org"}, nil
				}
			}
			in := registryIntent(t, d)
			in.RegistryHosts = tc.hosts
			body, err := json.Marshal(in)
			if err != nil {
				t.Fatal(err)
			}
			in, err = decodeIntent(body)
			if err != nil {
				t.Fatal(err)
			}
			hs, err := d.handoffSpec(context.Background(), in)
			if tc.wantErr {
				if !errors.Is(err, ErrUnsupportedStart) {
					t.Fatalf("handoffSpec = %v, want ErrUnsupportedStart", err)
				}
				return
			}
			if tc.readErr {
				if !errors.Is(err, readErr) || errors.Is(err, ErrUnsupportedStart) {
					t.Fatalf("handoffSpec = %v, want retryable store error", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := tc.hosts
			if len(want) == 0 {
				if bytes.Contains(body, []byte(`"registry_hosts"`)) {
					t.Fatal("legacy encoding gained registry_hosts")
				}
				want = []string{"proxy.golang.org", "registry.npmjs.org"}
			}
			if !slices.Equal(hs.RegistryHosts, want) {
				t.Fatalf("registries = %v, want %v", hs.RegistryHosts, want)
			}
		})
	}
}

func TestDecodeIntentRegatesRegistryShape(t *testing.T) {
	t.Parallel()
	d := newTestDriver(t, &stubGate{}, newStubExports())
	for _, tc := range []struct {
		name    string
		profile domain.EgressProfile
		hosts   []string
	}{
		{"provider only", domain.EgressProviderOnly, []string{"pypi.org"}},
		{"unsorted", domain.EgressProviderRegistry, []string{"pypi.org", "proxy.golang.org"}},
		{"duplicate", domain.EgressProviderRegistry, []string{"pypi.org", "pypi.org"}},
		{"malformed host", domain.EgressProviderRegistry, []string{"pypi.org:443"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := registryIntent(t, d)
			in.Spec.EgressProfile, in.RegistryHosts = tc.profile, tc.hosts
			body, err := json.Marshal(in)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decodeIntent(body); err == nil {
				t.Fatal("decoded an invalid registry record")
			}
		})
	}
}

func TestRegistryStartReadFailureStaysRetryableAndInvalidSetCommitsFailure(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		d := newTestDriver(t, &stubGate{}, newStubExports())
		readErr := errors.New("store unavailable")
		d.adoptedHosts = func(context.Context, int64) ([]string, error) { return nil, readErr }
		spec := testStartSpec()
		inputs := registryInputs(t, &spec)
		load := func(context.Context) (exec.StageInputs, error) { return inputs, nil }
		if err := d.StartWithInputs(context.Background(), testInvoke, spec, load); !errors.Is(err, readErr) || errors.Is(err, ErrUnsupportedStart) {
			t.Fatalf("start = %v, want retryable read error", err)
		}
		if _, err := os.Stat(d.intentPath(testRunIDFor(testInvoke))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("intent after read failure: %v", err)
		}
		if _, err := d.Collect(context.Background(), testInvoke); !errors.Is(err, exec.ErrUnknownInvocation) {
			t.Fatalf("collect after read failure: %v", err)
		}
		d.adoptedHosts = func(context.Context, int64) ([]string, error) { return []string{"PyPI.org"}, nil }
		if err := d.StartWithInputs(context.Background(), testInvoke, spec, load); err != nil {
			t.Fatal(err)
		}
		result, err := d.Collect(context.Background(), testInvoke)
		if err != nil || result.Status != exec.StatusFailed {
			t.Fatalf("collect invalid set = %v, %v, want failed", result, err)
		}
	})
}

func TestRegistryReadFailureAfterSeedingPreservesRecovery(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		records := newStubExports()
		gate := &stubGate{}
		d := newTestDriver(t, gate, records)
		t.Cleanup(func() { _ = d.Close(context.Background()) })
		readErr := errors.New("store unavailable after seeding")
		reads := 0
		d.adoptedHosts = func(context.Context, int64) ([]string, error) {
			reads++
			if reads > 2 {
				return nil, readErr
			}
			return []string{"pypi.org"}, nil
		}
		spec := testStartSpec()
		inputs := registryInputs(t, &spec)
		if err := d.StartWithInputs(context.Background(), testInvoke, spec,
			func(context.Context) (exec.StageInputs, error) { return inputs, nil }); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		in, err := d.loadIntent(context.Background(), testInvoke)
		if err != nil {
			t.Fatal(err)
		}
		if in.Phase != phaseSeeding || in.Result != nil || len(records.outcomes) != 0 || len(gate.specs) != 0 {
			t.Fatalf("store failure lost retry: phase %s, result %v, outcomes %d, handoffs %d",
				in.Phase, in.Result, len(records.outcomes), len(gate.specs))
		}
		if err := d.recoverRunning(context.Background(), in); !errors.Is(err, ErrRecoveryRetryable) || !errors.Is(err, readErr) || errors.Is(err, ErrUnsupportedStart) {
			t.Fatalf("recovery lookup = %v, want retryable store error", err)
		}
	})
}
