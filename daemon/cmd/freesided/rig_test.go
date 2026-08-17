package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

type fakeRigHost struct {
	supervised  bool
	live        bool
	description string
	containers  map[string]bool
	volumes     map[string]bool
	networks    map[string]bool
	runtimeCLI  bool
	inspected   []string
	deleted     []string
	probed      []string
}

func (h *fakeRigHost) ProbeDaemon(
	_ context.Context, address string,
) (string, bool, error) {
	h.probed = append(h.probed, address)
	return h.description, h.live, nil
}

func (h *fakeRigHost) SupervisedDaemon(context.Context, string) (bool, error) {
	return h.supervised, nil
}

func (h *fakeRigHost) PresentContainers(
	_ context.Context, _ string, names []string,
) ([]string, error) {
	h.inspected = append(h.inspected, names...)
	var present []string
	for _, name := range names {
		if h.containers[name] {
			present = append(present, name)
		}
	}
	return present, nil
}

func (h *fakeRigHost) DeleteContainers(
	_ context.Context, _ string, names []string,
) error {
	h.inspected = append(h.inspected, names...)
	for _, name := range names {
		if !h.containers[name] {
			continue
		}
		h.deleted = append(h.deleted, name)
		delete(h.containers, name)
	}
	return nil
}

func (h *fakeRigHost) PresentPersistentResources(
	_ context.Context, _ string, volumes, networks []string,
) ([]string, []string, error) {
	var presentVolumes, presentNetworks []string
	for _, name := range volumes {
		if h.volumes[name] {
			presentVolumes = append(presentVolumes, name)
		}
	}
	for _, name := range networks {
		if h.networks[name] {
			presentNetworks = append(presentNetworks, name)
		}
	}
	return presentVolumes, presentNetworks, nil
}

func (h *fakeRigHost) RuntimeCLIActive(context.Context, string) (bool, error) {
	return h.runtimeCLI, nil
}

func rigCommandRoots(t *testing.T) (stateRoot, seedRoot, databasePath string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	stateRoot = filepath.Join(root, "state")
	seedRoot = filepath.Join(root, "seed")
	databasePath = filepath.Join(stateRoot, "freeside.db")
	return stateRoot, seedRoot, databasePath
}

func rigHoldArgs(stateRoot, seedRoot, databasePath string) []string {
	return []string{
		"-state-root", stateRoot, "-db", databasePath,
		"-listen", "127.0.0.1:8677", "-seed-root", seedRoot,
	}
}

func TestRigHoldRefusesSupervisedAndListeningDaemonsBeforeReturningLease(t *testing.T) {
	tests := []struct {
		name string
		host *fakeRigHost
		want string
	}{
		{name: "supervised", host: &fakeRigHost{supervised: true}, want: `supervised daemon "ai.freeside.daemon" is loaded`},
		{name: "listener", host: &fakeRigHost{live: true, description: "freesided version=test"}, want: `listen address "127.0.0.1:8677" is already occupied`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stateRoot, seedRoot, databasePath := rigCommandRoots(t)
			var stdout, stderr bytes.Buffer
			err := runRigHoldWithLeaseRoot(
				context.Background(), rigHoldArgs(stateRoot, seedRoot, databasePath),
				&stdout, &stderr, tt.host, filepath.Join(filepath.Dir(stateRoot), "rig-locks"),
			)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("hold error = %v, want %q", err, tt.want)
			}
			if _, err := daemonlock.ReadRigManifest(stateRoot); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("refused hold left manifest: %v", err)
			}
		})
	}
}

func TestRigHoldProbesCanonicalListener(t *testing.T) {
	stateRoot, seedRoot, databasePath := rigCommandRoots(t)
	host := &fakeRigHost{live: true, description: "listener"}
	args := rigHoldArgs(stateRoot, seedRoot, databasePath)
	args[5] = "localhost:8677"
	var stdout, stderr bytes.Buffer
	err := runRigHoldWithLeaseRoot(
		context.Background(), args, &stdout, &stderr, host,
		filepath.Join(filepath.Dir(stateRoot), "rig-locks"),
	)
	if err == nil {
		t.Fatal("hold accepted a live listener")
	}
	if len(host.probed) != 1 || host.probed[0] == "localhost:8677" {
		t.Fatalf("probed addresses = %v, want one resolved listener", host.probed)
	}
}

func TestRigHoldWritesAcquisitionBeforeWaitingAndPreservesManifestOnInterrupt(t *testing.T) {
	stateRoot, seedRoot, databasePath := rigCommandRoots(t)
	t.Cleanup(func() {
		recovery, err := daemonlock.AcquireStaleRig(stateRoot)
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		if err != nil {
			t.Errorf("acquire interrupted rig for test cleanup: %v", err)
			return
		}
		if err := recovery.Close(); err != nil {
			t.Errorf("close interrupted rig during test cleanup: %v", err)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	if err := runRigHoldWithLeaseRoot(
		ctx, rigHoldArgs(stateRoot, seedRoot, databasePath),
		&stdout, &stderr, &fakeRigHost{}, filepath.Join(filepath.Dir(stateRoot), "rig-locks"),
	); err != nil {
		t.Fatal(err)
	}
	var output rigHoldOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode acquisition: %v", err)
	}
	if output.Token == "" || output.Manifest.Resources.StateRoot == "" {
		t.Fatalf("acquisition = %#v, want token and manifest", output)
	}
	if _, err := daemonlock.ReadRigManifest(stateRoot); err != nil {
		t.Fatalf("interrupted hold did not preserve a stale manifest: %v", err)
	}
}

func TestRigHoldCleanReleaseRemovesManifest(t *testing.T) {
	stateRoot, seedRoot, databasePath := rigCommandRoots(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errRigCleanRelease)
	var stdout, stderr bytes.Buffer
	if err := runRigHoldWithLeaseRoot(
		ctx, rigHoldArgs(stateRoot, seedRoot, databasePath),
		&stdout, &stderr, &fakeRigHost{}, filepath.Join(filepath.Dir(stateRoot), "rig-locks"),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := daemonlock.ReadRigManifest(stateRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clean release left manifest: %v", err)
	}
}

func TestRigHoldRefusesAProcessHoldingTheDatabase(t *testing.T) {
	stateRoot, seedRoot, databasePath := rigCommandRoots(t)
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	databaseLock, err := daemonlock.Acquire(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = databaseLock.Close() })
	var stdout, stderr bytes.Buffer
	err = runRigHoldWithLeaseRoot(
		context.Background(), rigHoldArgs(stateRoot, seedRoot, databasePath),
		&stdout, &stderr, &fakeRigHost{}, filepath.Join(filepath.Dir(stateRoot), "rig-locks"),
	)
	if !errors.Is(err, daemonlock.ErrAlreadyRunning) {
		t.Fatalf("database-holder refusal = %v, want ErrAlreadyRunning", err)
	}
	if _, err := daemonlock.ReadRigManifest(stateRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("database-holder refusal left manifest: %v", err)
	}
}

func acquireBoundRig(
	t *testing.T, invocations ...domain.InvocationID,
) (stateRoot string, lease *daemonlock.RigLease, tokenFile string, names []string) {
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
	var volumes, networks []string
	for _, invocation := range invocations {
		resources := ward.RuntimeResourceNamesFor(claude.RunIDFor(invocation))
		names = append(names, resources.Containers...)
		names = append(names, ward.PreJobContainerNameForInvocation(invocation))
		volumes = append(volumes, resources.Volumes...)
		networks = append(networks, resources.Networks...)
	}
	manifest, err := daemonlock.BindRigRuntimeResources(
		stateRoot, lease.Token(), names, volumes, networks,
	)
	if err != nil {
		t.Fatal(err)
	}
	names = manifest.Resources.Containers
	tokenFile = filepath.Join(t.TempDir(), "rig.json")
	body, err := json.Marshal(rigHoldOutput{Token: lease.Token(), Manifest: lease.Manifest()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return stateRoot, lease, tokenFile, names
}

func TestRunRigBindRecordsPreJobContainer(t *testing.T) {
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
	body, err := json.Marshal(rigHoldOutput{Token: lease.Token(), Manifest: lease.Manifest()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, body, 0o600); err != nil {
		t.Fatal(err)
	}
	invocationID := domain.InvocationID("inv-implement")
	var output bytes.Buffer
	if err := runRigBind([]string{
		"-state-root", stateRoot, "-token-file", tokenFile,
		"-invocation", string(invocationID),
	}, &output, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	var manifest daemonlock.RigManifest
	if err := json.Unmarshal(output.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	want := ward.PreJobContainerNameForInvocation(invocationID)
	if !slices.Contains(manifest.Resources.Containers, want) {
		t.Fatalf("bound containers = %v, want pre-job probe %q", manifest.Resources.Containers, want)
	}
}

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
	body, err := json.Marshal(rigHoldOutput{Token: lease.Token(), Manifest: lease.Manifest()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, body, 0o600); err != nil {
		t.Fatal(err)
	}
	invocations := []domain.InvocationID{"elaboration-iteration-1", "elaboration-iteration-2"}
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
	body, err := json.Marshal(rigHoldOutput{Token: lease.Token(), Manifest: lease.Manifest()})
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
	err = runClaudeConformance(t.Context(), nil, nil, nil, claudeDriverConfig{
		StateDir: stateRoot, RigTokenFile: tokenFile, SeedRoot: blockedSeedRoot,
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "create conformance seed root") {
		t.Fatalf("conformance setup error = %v, want blocked seed root", err)
	}
	manifest, err := daemonlock.ReadRigManifest(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	runID := ""
	for _, name := range manifest.Resources.Containers {
		if strings.HasPrefix(name, "freeside-handoff-conf-") && strings.HasSuffix(name, "-seeder") {
			runID = strings.TrimSuffix(strings.TrimPrefix(name, "freeside-handoff-"), "-seeder")
			break
		}
	}
	if !strings.HasPrefix(runID, "conf-") || len(runID) != len("conf-")+16 {
		t.Fatalf("bound conformance run ID = %q, want conf-<16hex>", runID)
	}
	want := ward.FullConformanceRuntimeResourceNamesFor(runID)
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

func assertVolumeResiduePreservesGlobalGate(
	t *testing.T, resources ward.RuntimeResourceNames, residualVolume string,
) {
	t.Helper()
	stateRoot, seedRoot, databasePath := rigCommandRoots(t)
	root := filepath.Dir(stateRoot)
	leaseRoot := filepath.Join(root, "rig-locks")
	lease, err := daemonlock.AcquireRig(daemonlock.RigAcquireConfig{
		Owner:     daemonlock.RigOwner{User: "operator", Host: "host", PID: os.Getpid()},
		StateRoot: stateRoot, DatabasePath: databasePath,
		ListenAddress: "127.0.0.1:8677", SeedRoot: seedRoot, LeaseRoot: leaseRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(t.TempDir(), "rig.json")
	body, err := json.Marshal(rigHoldOutput{Token: lease.Token(), Manifest: lease.Manifest()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := bindRigRuntimeResources(stateRoot, tokenFile, resources); err != nil {
		t.Fatal(err)
	}
	if err := lease.Abandon(); err != nil {
		t.Fatal(err)
	}

	secondCfg := daemonlock.RigAcquireConfig{
		Owner:         daemonlock.RigOwner{User: "operator", Host: "host", PID: os.Getpid()},
		StateRoot:     filepath.Join(root, "state-second"),
		DatabasePath:  filepath.Join(root, "state-second", "freeside.db"),
		ListenAddress: "127.0.0.1:8678", SeedRoot: filepath.Join(root, "seed-second"),
		LeaseRoot: leaseRoot,
	}
	if _, err := daemonlock.AcquireRig(secondCfg); !errors.Is(err, daemonlock.ErrRigStale) {
		t.Fatalf("disjoint campaign over crashed runtime namespace = %v, want ErrRigStale", err)
	}

	host := &fakeRigHost{volumes: map[string]bool{residualVolume: true}}
	err = runRigRecover(t.Context(), []string{
		"-state-root", stateRoot, "-container-bin", "container-test", "-confirm",
	}, ioDiscard{}, ioDiscard{}, host)
	if err == nil || !strings.Contains(err.Error(), "recover their owning journal first") {
		t.Fatalf("recovery over persistent runtime residue = %v", err)
	}
	if _, err := daemonlock.ReadRigManifest(stateRoot); err != nil {
		t.Fatalf("persistent runtime residue cleared global authority: %v", err)
	}
	if _, err := daemonlock.AcquireRig(secondCfg); !errors.Is(err, daemonlock.ErrRigStale) {
		t.Fatalf("second campaign after refused recovery = %v, want ErrRigStale", err)
	}

	delete(host.volumes, residualVolume)
	if err := runRigRecover(t.Context(), []string{
		"-state-root", stateRoot, "-container-bin", "container-test", "-confirm",
	}, ioDiscard{}, ioDiscard{}, host); err != nil {
		t.Fatal(err)
	}
	second, err := daemonlock.AcquireRig(secondCfg)
	if err != nil {
		t.Fatalf("campaign after runtime-resource convergence: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCodexReviewResiduePreservesGlobalGateAcrossCrash(t *testing.T) {
	reviewID := "review-" + strings.Repeat("d", 24)
	assertVolumeResiduePreservesGlobalGate(
		t,
		ward.CodexReviewRuntimeResourceNamesFor(reviewID),
		"freeside-review-"+reviewID+"-agents",
	)
}

func TestFullConformanceResiduePreservesGlobalGateAcrossCrash(t *testing.T) {
	runID := "conf-" + strings.Repeat("e", 16)
	assertVolumeResiduePreservesGlobalGate(
		t,
		ward.FullConformanceRuntimeResourceNamesFor(runID),
		"freeside-ward-conf-"+runID+"-cred",
	)
}

func TestRigCleanupTouchesOnlyManifestContainers(t *testing.T) {
	stateRoot, lease, tokenFile, names := acquireBoundRig(t, "inv-elaborate", "inv-implement")
	t.Cleanup(func() { _ = lease.Close() })
	foreign := "freeside-handoff-c" + strings.Repeat("f", 31) + "-agent"
	host := &fakeRigHost{containers: map[string]bool{
		names[0]: true, names[len(names)-1]: true, foreign: true,
	}}
	databaseLock, err := daemonlock.Acquire(lease.Manifest().Resources.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	err = runRigCleanup(context.Background(), []string{
		"-state-root", stateRoot, "-token-file", tokenFile, "-container-bin", "container-test",
	}, ioDiscard{}, ioDiscard{}, host)
	if !errors.Is(err, daemonlock.ErrAlreadyRunning) {
		t.Fatalf("live-database cleanup error = %v, want ErrAlreadyRunning", err)
	}
	if len(host.deleted) != 0 {
		t.Fatalf("live-database cleanup deleted %v", host.deleted)
	}
	if err := databaseLock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := runRigCleanup(context.Background(), []string{
		"-state-root", stateRoot, "-token-file", tokenFile, "-container-bin", "container-test",
	}, ioDiscard{}, ioDiscard{}, host); err != nil {
		t.Fatal(err)
	}
	wantInspected := append(slices.Clone(names), names...)
	if !slices.Equal(host.inspected, wantInspected) {
		t.Fatalf("inspected = %v, want cleanup plus absence proof %v", host.inspected, wantInspected)
	}
	wantDeleted := []string{names[0], names[len(names)-1]}
	if !slices.Equal(host.deleted, wantDeleted) {
		t.Fatalf("deleted = %v, want %v", host.deleted, wantDeleted)
	}
	if !host.containers[foreign] {
		t.Fatal("cleanup deleted the unrecorded container")
	}
}

func TestRigRecoverRequiresDeadListenerAndExplicitConfirmation(t *testing.T) {
	stateRoot, lease, _, names := acquireBoundRig(t, "inv-implement")
	if err := lease.Abandon(); err != nil {
		t.Fatal(err)
	}
	host := &fakeRigHost{containers: map[string]bool{names[0]: true}}
	var inspection bytes.Buffer
	err := runRigRecover(context.Background(), []string{
		"-state-root", stateRoot, "-container-bin", "container-test",
	}, &inspection, ioDiscard{}, host)
	if !errors.Is(err, daemonlock.ErrRigRecoveryConfirmation) {
		t.Fatalf("unconfirmed recovery error = %v, want confirmation", err)
	}
	if len(host.deleted) != 0 {
		t.Fatalf("unconfirmed recovery deleted %v", host.deleted)
	}
	if !strings.Contains(inspection.String(), names[0]) {
		t.Fatalf("inspection = %q, want recorded live container", inspection.String())
	}
	host.supervised = true
	err = runRigRecover(context.Background(), []string{
		"-state-root", stateRoot, "-container-bin", "container-test", "-confirm",
	}, ioDiscard{}, ioDiscard{}, host)
	if err == nil || !strings.Contains(err.Error(), "supervised daemon") {
		t.Fatalf("supervised-daemon recovery error = %v", err)
	}
	if len(host.deleted) != 0 {
		t.Fatalf("supervised-daemon recovery deleted %v", host.deleted)
	}
	host.supervised = false
	databaseLock, err := daemonlock.Acquire(lease.Manifest().Resources.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	err = runRigRecover(context.Background(), []string{
		"-state-root", stateRoot, "-container-bin", "container-test", "-confirm",
	}, ioDiscard{}, ioDiscard{}, host)
	if !errors.Is(err, daemonlock.ErrAlreadyRunning) {
		t.Fatalf("live-database recovery error = %v, want ErrAlreadyRunning", err)
	}
	if len(host.deleted) != 0 {
		t.Fatalf("live-database recovery deleted %v", host.deleted)
	}
	if err := databaseLock.Close(); err != nil {
		t.Fatal(err)
	}

	host.live = true
	host.description = "freesided version=test"
	err = runRigRecover(context.Background(), []string{
		"-state-root", stateRoot, "-container-bin", "container-test", "-confirm",
	}, ioDiscard{}, ioDiscard{}, host)
	if err == nil || !strings.Contains(err.Error(), "is still live") {
		t.Fatalf("live-daemon recovery error = %v", err)
	}
	if len(host.deleted) != 0 {
		t.Fatalf("live-daemon recovery deleted %v", host.deleted)
	}

	host.live = false
	host.runtimeCLI = true
	err = runRigRecover(context.Background(), []string{
		"-state-root", stateRoot, "-container-bin", "container-test", "-confirm",
	}, ioDiscard{}, ioDiscard{}, host)
	if err == nil || !strings.Contains(err.Error(), "runtime CLI process is still active") {
		t.Fatalf("orphan-runtime recovery error = %v", err)
	}
	if _, err := daemonlock.ReadRigManifest(stateRoot); err != nil {
		t.Fatalf("active runtime CLI cleared stale gate: %v", err)
	}
	host.runtimeCLI = false
	staleManifest, err := daemonlock.ReadRigManifest(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	recordedVolume := staleManifest.Resources.Volumes[0]
	host.volumes = map[string]bool{recordedVolume: true}
	err = runRigRecover(context.Background(), []string{
		"-state-root", stateRoot, "-container-bin", "container-test", "-confirm",
	}, ioDiscard{}, ioDiscard{}, host)
	if err == nil || !strings.Contains(err.Error(), "recover their owning journal first") {
		t.Fatalf("persistent-volume recovery error = %v", err)
	}
	if _, err := daemonlock.ReadRigManifest(stateRoot); err != nil {
		t.Fatalf("persistent volume cleared stale gate: %v", err)
	}
	delete(host.volumes, recordedVolume)
	if err := runRigRecover(context.Background(), []string{
		"-state-root", stateRoot, "-container-bin", "container-test", "-confirm",
	}, ioDiscard{}, ioDiscard{}, host); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(host.deleted, []string{names[0]}) {
		t.Fatalf("confirmed recovery deleted %v, want %q", host.deleted, names[0])
	}
	if _, err := daemonlock.ReadRigManifest(stateRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("confirmed recovery left manifest: %v", err)
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(body []byte) (int, error) { return len(body), nil }
