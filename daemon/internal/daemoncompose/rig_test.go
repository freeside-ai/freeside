package daemoncompose

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec/claude"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

func rigCommandRoots(t *testing.T) (stateRoot, seedRoot, databasePath string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	stateRoot = filepath.Join(root, "state")
	seedRoot = filepath.Join(root, "seed")
	databasePath = filepath.Join(stateRoot, "freeside.db")
	return stateRoot, seedRoot, databasePath
}

//nolint:paralleltest // Serial: rigCommandRoots sets HOME with t.Setenv.
func TestBindRigInvocationResourcesAddsLaterIteration(t *testing.T) {
	stateRoot, seedRoot, databasePath := rigCommandRoots(t)
	lease, err := daemonlock.AcquireRig(daemonlock.RigAcquireConfig{
		Owner:     daemonlock.RigOwner{User: "operator", Host: "host", PID: os.Getpid()},
		StateRoot: stateRoot, DatabasePath: databasePath,
		ListenAddress: "127.0.0.1:8677", SeedRoot: seedRoot,
		LeaseRoot: filepath.Join(filepath.Dir(stateRoot), "rig-locks"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	tokenFile := filepath.Join(t.TempDir(), "rig.json")
	body, err := json.Marshal(RigHoldOutput{Token: lease.Token(), Manifest: lease.Manifest()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, body, 0o600); err != nil {
		t.Fatal(err)
	}
	invocations := []domain.InvocationID{"specification-iteration-1", "specification-iteration-2"}
	for _, invocationID := range invocations {
		if err := bindRigInvocationResources(stateRoot, tokenFile, invocationID); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := daemonlock.ReadRigManifest(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, invocationID := range invocations {
		resources := ward.RuntimeResourceNamesFor(claude.RunIDFor(invocationID))
		wantContainers := append(
			slices.Clone(resources.Containers), ward.PreJobContainerNameForInvocation(invocationID),
		)
		for _, name := range wantContainers {
			if !slices.Contains(manifest.Resources.Containers, name) {
				t.Errorf("later invocation %q omitted container %q", invocationID, name)
			}
		}
		for _, name := range resources.Volumes {
			if !slices.Contains(manifest.Resources.Volumes, name) {
				t.Errorf("later invocation %q omitted volume %q", invocationID, name)
			}
		}
		for _, name := range resources.Networks {
			if !slices.Contains(manifest.Resources.Networks, name) {
				t.Errorf("later invocation %q omitted network %q", invocationID, name)
			}
		}
	}
}

//nolint:paralleltest // Serial: rigCommandRoots sets HOME with t.Setenv.
func TestRunClaudeConformanceBindsExactNamespaceBeforeSetup(t *testing.T) {
	stateRoot, seedRoot, databasePath := rigCommandRoots(t)
	lease, err := daemonlock.AcquireRig(daemonlock.RigAcquireConfig{
		Owner:     daemonlock.RigOwner{User: "operator", Host: "host", PID: os.Getpid()},
		StateRoot: stateRoot, DatabasePath: databasePath,
		ListenAddress: "127.0.0.1:8677", SeedRoot: seedRoot,
		LeaseRoot: filepath.Join(filepath.Dir(stateRoot), "rig-locks"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	tokenFile := filepath.Join(t.TempDir(), "rig.json")
	body, err := json.Marshal(RigHoldOutput{Token: lease.Token(), Manifest: lease.Manifest()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, body, 0o600); err != nil {
		t.Fatal(err)
	}
	blockedSeedRoot := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedSeedRoot, []byte("block mkdir"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = runClaudeConformance(t.Context(), nil, nil, nil, ClaudeDriverConfig{
		StateDir: stateRoot, RigTokenFile: tokenFile, SeedRoot: blockedSeedRoot,
	}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "create conformance seed root") {
		t.Fatalf("conformance setup error = %v, want blocked seed root", err)
	}
	manifest, err := daemonlock.ReadRigManifest(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	// Full's two synthetic handoffs each bind a seeder; only the first run
	// also owns the suite's probe objects.
	runID, registryRunID := "", ""
	for _, name := range manifest.Resources.Containers {
		if !strings.HasPrefix(name, "freeside-handoff-conf-") || !strings.HasSuffix(name, "-seeder") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(name, "freeside-handoff-"), "-seeder")
		if slices.Contains(manifest.Resources.Containers, "freeside-ward-conf-"+id+"-liveness") {
			runID = id
		} else {
			registryRunID = id
		}
	}
	for _, id := range []string{runID, registryRunID} {
		if !strings.HasPrefix(id, "conf-") || len(id) != len("conf-")+16 {
			t.Fatalf("bound conformance run IDs = %q and %q, want two conf-<16hex>", runID, registryRunID)
		}
	}
	want := ward.FullConformanceRuntimeResourceNamesFor(runID, registryRunID)
	for _, names := range []struct {
		kind string
		got  []string
		want []string
	}{
		{kind: "containers", got: manifest.Resources.Containers, want: want.Containers},
		{kind: "volumes", got: manifest.Resources.Volumes, want: want.Volumes},
		{kind: "networks", got: manifest.Resources.Networks, want: want.Networks},
	} {
		slices.Sort(names.got)
		slices.Sort(names.want)
		if !slices.Equal(names.got, names.want) {
			t.Errorf("bound conformance %s = %v, want %v", names.kind, names.got, names.want)
		}
	}
}

// TestProductionRigRuntimeAuthorizerBindsCredentialObserver proves the exact
// authorizer the preflight credential check builds records the observer name in
// the held rig manifest, closing the wiring from InspectCredentialVolumeManifest
// to rig cleanup.
//
//nolint:paralleltest // Serial: rigCommandRoots sets HOME with t.Setenv.
func TestProductionRigRuntimeAuthorizerBindsCredentialObserver(t *testing.T) {
	stateRoot, seedRoot, databasePath := rigCommandRoots(t)
	lease, err := daemonlock.AcquireRig(daemonlock.RigAcquireConfig{
		Owner:     daemonlock.RigOwner{User: "operator", Host: "host", PID: os.Getpid()},
		StateRoot: stateRoot, DatabasePath: databasePath,
		ListenAddress: "127.0.0.1:8677", SeedRoot: seedRoot,
		LeaseRoot: filepath.Join(filepath.Dir(stateRoot), "rig-locks"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	tokenFile := filepath.Join(t.TempDir(), "rig.json")
	body, err := json.Marshal(RigHoldOutput{Token: lease.Token(), Manifest: lease.Manifest()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, body, 0o600); err != nil {
		t.Fatal(err)
	}

	authorize := ProductionRigRuntimeAuthorizer(stateRoot, tokenFile)
	if authorize == nil {
		t.Fatal("authorizer is nil for a configured rig token file")
	}
	observer := "freeside-preflight-credential-" + strings.Repeat("b", 12)
	if err := authorize(
		context.Background(), ward.RuntimeResourceNames{Containers: []string{observer}},
	); err != nil {
		t.Fatalf("authorize credential observer: %v", err)
	}
	manifest, err := daemonlock.ReadRigManifest(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(manifest.Resources.Containers, observer) {
		t.Fatalf("bound containers = %v, want the credential observer %q", manifest.Resources.Containers, observer)
	}
}

// heldRigTokenFile acquires a rig over fresh roots and writes its hold file,
// as the deputy does before it starts the daemon.
func heldRigTokenFile(t *testing.T) (stateRoot, tokenFile string) {
	t.Helper()
	stateRoot, seedRoot, databasePath := rigCommandRoots(t)
	lease, err := daemonlock.AcquireRig(daemonlock.RigAcquireConfig{
		Owner:     daemonlock.RigOwner{User: "operator", Host: "host", PID: os.Getpid()},
		StateRoot: stateRoot, DatabasePath: databasePath,
		ListenAddress: "127.0.0.1:8677", SeedRoot: seedRoot,
		LeaseRoot: filepath.Join(filepath.Dir(stateRoot), "rig-locks"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	tokenFile = filepath.Join(t.TempDir(), "rig.json")
	body, err := json.Marshal(RigHoldOutput{Token: lease.Token(), Manifest: lease.Manifest()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return stateRoot, tokenFile
}

// taskRuntimeNames is one task's per-task rig footprint: its writer
// invocation's handoff namespace and pre-job container, plus one review.
func taskRuntimeNames(cycle int) ward.RuntimeResourceNames {
	invocationID := domain.InvocationID(fmt.Sprintf("task-%d-writer", cycle))
	writer := ward.RuntimeResourceNamesFor(claude.RunIDFor(invocationID))
	review := ward.CodexReviewRuntimeResourceNamesFor(fmt.Sprintf("review-%024x", cycle))
	return ward.RuntimeResourceNames{
		Containers: slices.Concat(writer.Containers, review.Containers,
			[]string{ward.PreJobContainerNameForInvocation(invocationID)}),
		Volumes:  slices.Concat(writer.Volumes, review.Volumes),
		Networks: slices.Concat(writer.Networks, review.Networks),
	}
}

// TestRigHoldSurvivesRepeatedTasksWithRelease is the #833 regression: one
// deputy hold must serve any number of tasks. Without release the manifest
// passes its decoding limit after about 22 tasks.
//
//nolint:paralleltest // Serial: heldRigTokenFile sets HOME with t.Setenv.
func TestRigHoldSurvivesRepeatedTasksWithRelease(t *testing.T) {
	t.Run("released", func(t *testing.T) {
		stateRoot, tokenFile := heldRigTokenFile(t)
		authorize := ProductionRigRuntimeAuthorizer(stateRoot, tokenFile)
		release := ProductionRigRuntimeReleaser(stateRoot, tokenFile)
		for cycle := range 100 {
			names := taskRuntimeNames(cycle)
			if err := authorize(t.Context(), names); err != nil {
				t.Fatalf("bind task %d: %v", cycle, err)
			}
			if err := release(t.Context(), names); err != nil {
				t.Fatalf("release task %d: %v", cycle, err)
			}
		}
		manifest, err := daemonlock.ReadRigManifest(stateRoot)
		if err != nil {
			t.Fatal(err)
		}
		if n := len(manifest.Resources.Containers) + len(manifest.Resources.Volumes) +
			len(manifest.Resources.Networks); n != 0 {
			t.Fatalf("rig holds %d names after 100 released tasks, want 0", n)
		}
	})
	t.Run("unreleased", func(t *testing.T) {
		stateRoot, tokenFile := heldRigTokenFile(t)
		authorize := ProductionRigRuntimeAuthorizer(stateRoot, tokenFile)
		for cycle := range 100 {
			err := authorize(t.Context(), taskRuntimeNames(cycle))
			if err == nil {
				continue
			}
			if !strings.Contains(err.Error(), "decoding limit") || cycle < 10 {
				t.Fatalf("bind task %d without release: %v, want the decoding-limit refusal after about 22 tasks", cycle, err)
			}
			return
		}
		t.Fatal("100 unreleased tasks fit the manifest; the regression no longer shows the ceiling it guards")
	})
}

// listingRuntime answers only the listings ward.ReleaseAbsentRuntimeResources
// makes; any other runtime call panics on the nil embedded interface.
type listingRuntime struct {
	ward.Runtime
	containers []ward.ContainerSummary
}

func (r listingRuntime) ListContainers(context.Context) ([]ward.ContainerSummary, error) {
	return r.containers, nil
}

func (listingRuntime) ListVolumes(context.Context) ([]ward.VolumeSummary, error) { return nil, nil }

func (listingRuntime) ListNetworks(context.Context) ([]ward.NetworkSummary, error) { return nil, nil }

//nolint:paralleltest // Serial: heldRigTokenFile sets HOME with t.Setenv.
func TestReleasePreJobContainerReleasesOnlyAnAbsentPreJobName(t *testing.T) {
	invocationID := domain.InvocationID("specification-iteration-1")
	preJob := ward.PreJobContainerNameForInvocation(invocationID)
	for _, test := range []struct {
		name      string
		runtime   listingRuntime
		wantBound bool
	}{
		{name: "reaped", runtime: listingRuntime{}},
		{
			name:      "still listed",
			runtime:   listingRuntime{containers: []ward.ContainerSummary{{ID: preJob}}},
			wantBound: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			stateRoot, tokenFile := heldRigTokenFile(t)
			if err := bindRigInvocationResources(stateRoot, tokenFile, invocationID); err != nil {
				t.Fatal(err)
			}
			releasePreJobContainer(t.Context(), test.runtime,
				ProductionRigRuntimeReleaser(stateRoot, tokenFile), invocationID, nil)
			manifest, err := daemonlock.ReadRigManifest(stateRoot)
			if err != nil {
				t.Fatal(err)
			}
			if bound := slices.Contains(manifest.Resources.Containers, preJob); bound != test.wantBound {
				t.Fatalf("pre-job container bound = %v, want %v", bound, test.wantBound)
			}
			// The handoff namespace bound beside it waits for its journal close.
			for _, name := range ward.RuntimeResourceNamesFor(claude.RunIDFor(invocationID)).Containers {
				if !slices.Contains(manifest.Resources.Containers, name) {
					t.Errorf("handoff container %q was released with the pre-job container", name)
				}
			}
		})
	}
}
