package daemonlock

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func rigTestConfig(t *testing.T, stateRoot, seedRoot string) RigAcquireConfig {
	t.Helper()
	return RigAcquireConfig{
		Owner:     RigOwner{User: "operator", Host: "rig-host", PID: os.Getpid(), Note: "acceptance"},
		StateRoot: stateRoot, DatabasePath: filepath.Join(stateRoot, "freeside.db"),
		ListenAddress: "127.0.0.1:8677", SeedRoot: seedRoot,
		LeaseRoot: filepath.Join(filepath.Dir(stateRoot), "rig-locks"),
		Now:       func() time.Time { return time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC) },
	}
}

func TestRigLeaseCanonicalAliasesCollideAndReportOwner(t *testing.T) {
	t.Setenv("FREESIDE_TEST_SECRET", "must-not-appear-in-rig-refusal")
	root := t.TempDir()
	state := filepath.Join(root, "state")
	seed := filepath.Join(root, "seed")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(seed, 0o700); err != nil {
		t.Fatal(err)
	}
	stateAlias := filepath.Join(root, "state-alias")
	seedAlias := filepath.Join(root, "seed-alias")
	if err := os.Symlink(state, stateAlias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(seed, seedAlias); err != nil {
		t.Fatal(err)
	}
	lease, err := AcquireRig(rigTestConfig(t, stateAlias, seedAlias))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })

	_, err = AcquireRig(rigTestConfig(t, state, seed))
	if !errors.Is(err, ErrRigHeld) {
		t.Fatalf("alias acquisition error = %v, want ErrRigHeld", err)
	}
	message := err.Error()
	for _, want := range []string{
		"operator@rig-host", "pid=", "2026-08-15T12:00:00Z",
		state, filepath.Join(state, "freeside.db"), "127.0.0.1:8677", seed,
	} {
		if !strings.Contains(message, want) {
			t.Errorf("refusal %q does not contain %q", message, want)
		}
	}
	if strings.Contains(message, lease.Token()) {
		t.Fatal("refusal disclosed the lease token")
	}
	if strings.Contains(message, os.Getenv("FREESIDE_TEST_SECRET")) {
		t.Fatal("refusal disclosed an environment value")
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relativeState, err := filepath.Rel(cwd, state)
	if err != nil {
		t.Fatal(err)
	}
	relativeSeed, err := filepath.Rel(cwd, seed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireRig(rigTestConfig(t, relativeState, relativeSeed)); !errors.Is(err, ErrRigHeld) {
		t.Fatalf("relative-root acquisition error = %v, want ErrRigHeld", err)
	}
}

func TestRigLeaseSimultaneousAcquisitionHasOneWinner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		root := t.TempDir()
		cfg := rigTestConfig(t, filepath.Join(root, "state"), filepath.Join(root, "seed"))
		const contenders = 8
		start := make(chan struct{})
		results := make(chan struct {
			lease *RigLease
			err   error
		}, contenders)
		var ready sync.WaitGroup
		ready.Add(contenders)
		for range contenders {
			go func() {
				ready.Done()
				<-start
				lease, err := AcquireRig(cfg)
				results <- struct {
					lease *RigLease
					err   error
				}{lease: lease, err: err}
			}()
		}
		ready.Wait()
		close(start)
		var winners []*RigLease
		for range contenders {
			result := <-results
			if result.err == nil {
				winners = append(winners, result.lease)
				continue
			}
			if !errors.Is(result.err, ErrRigHeld) {
				t.Errorf("loser error = %v, want ErrRigHeld", result.err)
			}
		}
		for _, winner := range winners {
			if err := winner.Close(); err != nil {
				t.Fatal(err)
			}
		}
		if len(winners) != 1 {
			t.Fatalf("simultaneous winners = %d, want 1", len(winners))
		}
	})
}

func TestRigLeaseSharedSeedRootCollidesAcrossStateRoots(t *testing.T) {
	root := t.TempDir()
	seedRoot := filepath.Join(root, "seed")
	first, err := AcquireRig(rigTestConfig(t, filepath.Join(root, "state-a"), seedRoot))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	_, err = AcquireRig(rigTestConfig(t, filepath.Join(root, "state-b"), seedRoot))
	if !errors.Is(err, ErrRigHeld) {
		t.Fatalf("shared-seed acquisition error = %v, want ErrRigHeld", err)
	}
	if !strings.Contains(err.Error(), filepath.Join(root, "state-a")) {
		t.Fatalf("shared-seed refusal = %q, want first holder resources", err)
	}
}

func TestRigLeaseSwappedStateAndSeedRootsCollide(t *testing.T) {
	root := t.TempDir()
	left := filepath.Join(root, "left")
	right := filepath.Join(root, "right")
	first, err := AcquireRig(rigTestConfig(t, left, right))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	if _, err := AcquireRig(rigTestConfig(t, right, left)); !errors.Is(err, ErrRigHeld) {
		t.Fatalf("swapped-root acquisition error = %v, want ErrRigHeld", err)
	}
}

func TestRigLeaseGloballyExcludesDisjointCampaigns(t *testing.T) {
	root := t.TempDir()
	firstCfg := rigTestConfig(t, filepath.Join(root, "state-a"), filepath.Join(root, "seed-a"))
	secondCfg := rigTestConfig(t, filepath.Join(root, "state-b"), filepath.Join(root, "seed-b"))
	secondCfg.ListenAddress = "127.0.0.1:8678"
	first, err := AcquireRig(firstCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	if _, err := AcquireRig(secondCfg); !errors.Is(err, ErrRigHeld) {
		t.Fatalf("disjoint campaign acquisition error = %v, want ErrRigHeld", err)
	}
}

func TestRigLeaseGlobalExclusionSurvivesHolderCrash(t *testing.T) {
	root := t.TempDir()
	firstCfg := rigTestConfig(t, filepath.Join(root, "state-a"), filepath.Join(root, "seed-a"))
	secondCfg := rigTestConfig(t, filepath.Join(root, "state-b"), filepath.Join(root, "seed-b"))
	secondCfg.ListenAddress = "127.0.0.1:8678"
	first, err := AcquireRig(firstCfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Abandon(); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireRig(secondCfg); !errors.Is(err, ErrRigStale) {
		t.Fatalf("post-crash disjoint acquisition error = %v, want ErrRigStale", err)
	}
	recovery, err := AcquireStaleRig(firstCfg.StateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := recovery.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := AcquireRig(secondCfg)
	if err != nil {
		t.Fatalf("acquisition after confirmed recovery: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStaleRigRecoveryCannotClearAnotherCampaignGlobalGate(t *testing.T) {
	root := t.TempDir()
	firstCfg := rigTestConfig(t, filepath.Join(root, "state-a"), filepath.Join(root, "seed-a"))
	secondCfg := rigTestConfig(t, filepath.Join(root, "state-b"), filepath.Join(root, "seed-b"))
	secondCfg.ListenAddress = "127.0.0.1:8678"
	first, err := AcquireRig(firstCfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := removeRigManifest(globalRigStatePath(first.Manifest().Resources)); err != nil {
		t.Fatal(err)
	}
	if err := first.Abandon(); err != nil {
		t.Fatal(err)
	}
	second, err := AcquireRig(secondCfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Abandon(); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireStaleRig(firstCfg.StateRoot); err == nil ||
		!strings.Contains(err.Error(), "state-root and global rig manifests disagree") {
		t.Fatalf("cross-campaign stale recovery error = %v", err)
	}
	secondRecovery, err := AcquireStaleRig(secondCfg.StateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := secondRecovery.Close(); err != nil {
		t.Fatal(err)
	}
	firstRecovery, err := AcquireStaleRig(firstCfg.StateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := firstRecovery.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRigLeaseListenAddressCollidesAcrossRoots(t *testing.T) {
	root := t.TempDir()
	firstCfg := rigTestConfig(t, filepath.Join(root, "state-a"), filepath.Join(root, "seed-a"))
	secondCfg := rigTestConfig(t, filepath.Join(root, "state-b"), filepath.Join(root, "seed-b"))
	first, err := AcquireRig(firstCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	if _, err := AcquireRig(secondCfg); !errors.Is(err, ErrRigHeld) {
		t.Fatalf("shared-listen acquisition error = %v, want ErrRigHeld", err)
	} else if !strings.Contains(err.Error(), "operator@rig-host") {
		t.Fatalf("shared-listen refusal = %q, want owner metadata", err)
	}
	for _, path := range []string{secondCfg.StateRoot, secondCfg.SeedRoot} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("losing campaign mutated %s: %v", path, err)
		}
	}
}

func TestRigLeaseRejectsEphemeralListener(t *testing.T) {
	root := t.TempDir()
	cfg := rigTestConfig(t, filepath.Join(root, "state"), filepath.Join(root, "seed"))
	cfg.ListenAddress = "127.0.0.1:0"
	if _, err := AcquireRig(cfg); err == nil || !strings.Contains(err.Error(), "unsupported address") {
		t.Fatalf("ephemeral-listener acquisition error = %v", err)
	}
}

func TestDefaultRigLeaseRootIgnoresHomeEnvironment(t *testing.T) {
	before, err := defaultRigLeaseRoot()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	after, err := defaultRigLeaseRoot()
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("lease root changed with HOME: before %q, after %q", before, after)
	}
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(current.HomeDir, ".freeside", "rig-locks"); before != want {
		t.Fatalf("lease root = %q, want durable account root %q", before, want)
	}
}

func TestRigLeaseRequiresDatabaseUnderStateRoot(t *testing.T) {
	root := t.TempDir()
	cfg := rigTestConfig(t, filepath.Join(root, "state"), filepath.Join(root, "seed"))
	cfg.DatabasePath = filepath.Join(root, "shared.db")
	if _, err := AcquireRig(cfg); err == nil || !strings.Contains(err.Error(), "under the state root") {
		t.Fatalf("external database acquisition error = %v", err)
	}
}

func TestRigLeaseStaleRecoveryUsesFlockNotPIDOrAge(t *testing.T) {
	root := t.TempDir()
	cfg := rigTestConfig(t, filepath.Join(root, "state"), filepath.Join(root, "seed"))
	cfg.Now = func() time.Time { return time.Unix(1, 0).UTC() }
	cfg.Owner.PID = os.Getpid()
	lease, err := AcquireRig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireStaleRig(cfg.StateRoot); !errors.Is(err, ErrRigHeld) {
		t.Fatalf("recovery while flock is live = %v, want ErrRigHeld", err)
	}
	if err := lease.Abandon(); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireRig(cfg); !errors.Is(err, ErrRigStale) {
		t.Fatalf("ordinary acquisition over stale manifest = %v, want ErrRigStale", err)
	}
	recovery, err := AcquireStaleRig(cfg.StateRoot)
	if err != nil {
		t.Fatalf("acquire stale lease: %v", err)
	}
	if err := recovery.Abandon(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateRoot, stateManifestName)); err != nil {
		t.Fatalf("unconfirmed recovery removed stale manifest: %v", err)
	}
}

func TestRigLeaseRecoveryRejectsManifestRemovedAfterInitialRead(t *testing.T) {
	root := t.TempDir()
	cfg := rigTestConfig(t, filepath.Join(root, "state"), filepath.Join(root, "seed"))
	lease, err := AcquireRig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = acquireStaleRig(cfg.StateRoot, func() {
		if closeErr := lease.Close(); closeErr != nil {
			t.Fatalf("close live lease during recovery race: %v", closeErr)
		}
	})
	if err == nil || !strings.Contains(err.Error(), "re-read authoritative rig manifest") {
		t.Fatalf("recovery crossing clean release error = %v", err)
	}
}

func TestRigLeasePreservesAuthoritativeManifestWhenSeedPublishFails(t *testing.T) {
	root := t.TempDir()
	stateRoot := filepath.Join(root, "state")
	seedRoot := filepath.Join(root, "seed")
	if err := os.MkdirAll(seedRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seedRoot, rigLockName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(seedRoot, 0o500); err != nil { //nolint:gosec // directory fixture must refuse new files.
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(seedRoot, 0o700) }) //nolint:gosec // restore private directory traversal for cleanup.
	_, err := AcquireRig(rigTestConfig(t, stateRoot, seedRoot))
	if err == nil {
		t.Fatal("acquisition succeeded with a directory blocking the seed manifest")
	}
	if _, readErr := ReadRigManifest(stateRoot); readErr != nil {
		t.Fatalf("seed publication failure lost authoritative manifest: %v", readErr)
	}
}

func TestRigLeaseStaleRecoveryMergesInterruptedBind(t *testing.T) {
	root := t.TempDir()
	cfg := rigTestConfig(t, filepath.Join(root, "state"), filepath.Join(root, "seed"))
	lease, err := AcquireRig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	before := lease.Manifest()
	name := "freeside-handoff-c" + strings.Repeat("a", 31) + "-agent"
	volume := "freeside-handoff-c" + strings.Repeat("a", 31) + "-ws"
	network := "freeside-handoff-c" + strings.Repeat("a", 31) + "-egress"
	if _, err := BindRigRuntimeResources(
		cfg.StateRoot, lease.Token(), []string{name}, []string{volume}, []string{network},
	); err != nil {
		t.Fatal(err)
	}
	if err := writeRigManifest(filepath.Join(cfg.StateRoot, stateManifestName), before); err != nil {
		t.Fatal(err)
	}
	if err := lease.Abandon(); err != nil {
		t.Fatal(err)
	}
	recovery, err := AcquireStaleRig(cfg.StateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(recovery.Manifest().Resources.Containers, []string{name}) {
		t.Fatalf("recovered containers = %v, want %q", recovery.Manifest().Resources.Containers, name)
	}
	if !slices.Equal(recovery.Manifest().Resources.Volumes, []string{volume}) ||
		!slices.Equal(recovery.Manifest().Resources.Networks, []string{network}) {
		t.Fatalf("recovered persistence = volumes %v networks %v", recovery.Manifest().Resources.Volumes,
			recovery.Manifest().Resources.Networks)
	}
	if err := recovery.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRigBindRequiresLiveHolderAndToken(t *testing.T) {
	root := t.TempDir()
	cfg := rigTestConfig(t, filepath.Join(root, "state"), filepath.Join(root, "seed"))
	lease, err := AcquireRig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	name := "freeside-handoff-c" + strings.Repeat("a", 31) + "-agent"
	if _, err := BindRigRuntimeResources(cfg.StateRoot, "wrong", []string{name}, nil, nil); !errors.Is(err, ErrRigToken) {
		t.Fatalf("wrong-token bind = %v, want ErrRigToken", err)
	}
	manifest, err := BindRigRuntimeResources(cfg.StateRoot, lease.Token(), []string{name, name}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Resources.Containers) != 1 || manifest.Resources.Containers[0] != name {
		t.Fatalf("bound containers = %v, want only %q", manifest.Resources.Containers, name)
	}
	if err := lease.Abandon(); err != nil {
		t.Fatal(err)
	}
	if _, err := BindRigRuntimeResources(cfg.StateRoot, lease.Token(), []string{name}, nil, nil); !errors.Is(err, ErrRigNotHeld) {
		t.Fatalf("bind after holder death = %v, want ErrRigNotHeld", err)
	}
}

func TestRigBindRejectsOversizedManifestBeforeAuthorityAppend(t *testing.T) {
	root := t.TempDir()
	cfg := rigTestConfig(t, filepath.Join(root, "state"), filepath.Join(root, "seed"))
	lease, err := AcquireRig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	_, manifestPaths := rigPaths(lease.Manifest().Resources)
	paths := append([]string{
		filepath.Join(cfg.StateRoot, rigLockName),
		globalRigStatePath(lease.Manifest().Resources),
	}, manifestPaths...)
	before := make(map[string][]byte, len(paths))
	for _, path := range paths {
		// #nosec G304 -- fixed test paths under t.TempDir.
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = body
	}
	containers := make([]string, 1200)
	for i := range containers {
		containers[i] = fmt.Sprintf("freeside-handoff-c%031x-agent", i)
	}
	if _, err := BindRigRuntimeResources(
		cfg.StateRoot, lease.Token(), containers, nil, nil,
	); err == nil || !strings.Contains(err.Error(), "decoding limit") {
		t.Fatalf("oversized bind error = %v, want decoding-limit refusal", err)
	}
	for _, path := range paths {
		// #nosec G304 -- fixed test paths under t.TempDir.
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(after, before[path]) {
			t.Errorf("oversized bind mutated %s", path)
		}
	}
	name := "freeside-ward-conf-" + strings.Repeat("a", 16) + "-prejob"
	manifest, err := BindRigRuntimeResources(
		cfg.StateRoot, lease.Token(), []string{name}, nil, nil,
	)
	if err != nil {
		t.Fatalf("small bind after oversized refusal: %v", err)
	}
	if !slices.Equal(manifest.Resources.Containers, []string{name}) {
		t.Fatalf("containers after retry = %v, want %q", manifest.Resources.Containers, name)
	}
}

func TestRigBindClosesCodexReviewRuntimeNamespace(t *testing.T) {
	root := t.TempDir()
	valid := RigResources{
		Containers: []string{
			"freeside-handoff-review-" + strings.Repeat("a", 24) + "-seeder",
			"freeside-handoff-review-" + strings.Repeat("a", 24) + "-observer",
			"freeside-review-review-" + strings.Repeat("a", 24) + "-ws-obs",
			"freeside-review-review-" + strings.Repeat("a", 24) + "-workspace-observer",
			"freeside-review-review-" + strings.Repeat("a", 24) + "-agents-init",
			"freeside-review-review-" + strings.Repeat("a", 24) + "-agents-obs",
			"freeside-review-review-" + strings.Repeat("a", 24) + "-agents-observer",
			"freeside-review-review-" + strings.Repeat("a", 24) + "-snap-init",
			"freeside-review-review-" + strings.Repeat("a", 24) + "-snap-obs",
			"freeside-review-review-" + strings.Repeat("a", 24) + "-codex",
		},
		Volumes: []string{
			"freeside-handoff-review-" + strings.Repeat("a", 24) + "-ws",
			"freeside-review-review-" + strings.Repeat("a", 24) + "-agents",
			"freeside-review-review-" + strings.Repeat("a", 24) + "-snap",
		},
		Networks: []string{
			"freeside-review-review-" + strings.Repeat("a", 24) + "-egress",
		},
	}
	cfg := rigTestConfig(t, filepath.Join(root, "state"), filepath.Join(root, "seed"))
	lease, err := AcquireRig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	if _, err := BindRigRuntimeResources(
		cfg.StateRoot, lease.Token(), valid.Containers, valid.Volumes, valid.Networks,
	); err != nil {
		t.Fatalf("bind exact current and legacy Codex review names: %v", err)
	}
	for _, tc := range []struct {
		name       string
		containers []string
		volumes    []string
		networks   []string
	}{
		{name: "unknown container role", containers: []string{"freeside-review-review-" + strings.Repeat("a", 24) + "-shell"}},
		{name: "short review id", containers: []string{"freeside-review-review-" + strings.Repeat("a", 23) + "-codex"}},
		{name: "review handoff agent", containers: []string{"freeside-handoff-review-" + strings.Repeat("a", 24) + "-agent"}},
		{name: "unknown review volume", volumes: []string{"freeside-review-review-" + strings.Repeat("a", 24) + "-workspace"}},
		{name: "handoff review network", networks: []string{"freeside-handoff-review-" + strings.Repeat("a", 24) + "-egress"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := BindRigRuntimeResources(
				cfg.StateRoot, lease.Token(), tc.containers, tc.volumes, tc.networks,
			); err == nil || !strings.Contains(err.Error(), "outside the owned namespace") {
				t.Fatalf("bind malformed Codex review namespace = %v", err)
			}
		})
	}
}

func TestRigBindClosesFullConformanceRuntimeNamespace(t *testing.T) {
	hexID := strings.Repeat("b", 16)
	runID := "conf-" + hexID
	handoffPrefix := "freeside-handoff-" + runID + "-"
	probePrefix := "freeside-ward-conf-" + runID + "-"
	valid := RigResources{
		Containers: []string{
			handoffPrefix + "seeder", handoffPrefix + "observer",
			handoffPrefix + "ins-seed", handoffPrefix + "ins-check",
			handoffPrefix + "agent", handoffPrefix + "exporter",
			probePrefix + "liveness", probePrefix + "seed", probePrefix + "audit",
			probePrefix + "excl-writer", probePrefix + "excl-second",
			probePrefix + "net-live", probePrefix + "net",
			probePrefix + "inx-live", probePrefix + "inx",
		},
		Volumes: []string{
			handoffPrefix + "ws", handoffPrefix + "ins",
			probePrefix + "cred", probePrefix + "liveness-ws", probePrefix + "excl-ws",
			probePrefix + "net-live-ws", probePrefix + "inx-ws",
		},
		Networks: []string{handoffPrefix + "egress"},
	}
	root := t.TempDir()
	cfg := rigTestConfig(t, filepath.Join(root, "state"), filepath.Join(root, "seed"))
	lease, err := AcquireRig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	if _, err := BindRigRuntimeResources(
		cfg.StateRoot, lease.Token(), valid.Containers, valid.Volumes, valid.Networks,
	); err != nil {
		t.Fatalf("bind exact full-conformance names: %v", err)
	}
	for _, tc := range []struct {
		name       string
		containers []string
		volumes    []string
		networks   []string
	}{
		{name: "unknown probe container", containers: []string{probePrefix + "shell"}},
		{name: "short conformance id", containers: []string{"freeside-ward-conf-conf-" + strings.Repeat("b", 15) + "-audit"}},
		{name: "prejob role in full prefix", containers: []string{probePrefix + "prejob"}},
		{name: "unused synthetic handoff role", containers: []string{handoffPrefix + "writer-check"}},
		{name: "unknown probe volume", volumes: []string{probePrefix + "workspace"}},
		{name: "probe network", networks: []string{probePrefix + "egress"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := BindRigRuntimeResources(
				cfg.StateRoot, lease.Token(), tc.containers, tc.volumes, tc.networks,
			); err == nil || !strings.Contains(err.Error(), "outside the owned namespace") {
				t.Fatalf("bind malformed full-conformance namespace = %v", err)
			}
		})
	}
}

func TestRigBindRejectsContainerOutsideOwnedNamespace(t *testing.T) {
	root := t.TempDir()
	cfg := rigTestConfig(t, filepath.Join(root, "state"), filepath.Join(root, "seed"))
	lease, err := AcquireRig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	if _, err := BindRigRuntimeResources(cfg.StateRoot, lease.Token(), []string{"--all"}, nil, nil); err == nil ||
		!strings.Contains(err.Error(), "outside the owned namespace") {
		t.Fatalf("unsafe container bind error = %v", err)
	}
}

// TestRigBindAdmitsPreflightCredentialNamespace proves the preflight
// credential-observer name (ward's networkless setup-token probe, named from a
// 12-hex slice of its ownership label) is inside the owned namespace, so an
// interrupted preflight that bound the name leaves a container rig cleanup can
// enumerate. Near-misses stay rejected so the admission does not widen past the
// exact probe name.
func TestRigBindAdmitsPreflightCredentialNamespace(t *testing.T) {
	root := t.TempDir()
	cfg := rigTestConfig(t, filepath.Join(root, "state"), filepath.Join(root, "seed"))
	lease, err := AcquireRig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })

	accepted := "freeside-preflight-credential-" + strings.Repeat("a", 12)
	manifest, err := BindRigRuntimeResources(cfg.StateRoot, lease.Token(), []string{accepted}, nil, nil)
	if err != nil {
		t.Fatalf("bind exact preflight credential name: %v", err)
	}
	if !slices.Equal(manifest.Resources.Containers, []string{accepted}) {
		t.Fatalf("bound containers = %v, want %q", manifest.Resources.Containers, accepted)
	}

	for _, tc := range []struct {
		name      string
		container string
	}{
		{name: "short slice", container: "freeside-preflight-credential-" + strings.Repeat("a", 11)},
		{name: "long slice", container: "freeside-preflight-credential-" + strings.Repeat("a", 13)},
		{name: "uppercase slice", container: "freeside-preflight-credential-" + strings.Repeat("A", 12)},
		{name: "non-hex slice", container: "freeside-preflight-credential-" + strings.Repeat("g", 12)},
		{name: "missing slice", container: "freeside-preflight-credential-"},
		{name: "trailing suffix", container: "freeside-preflight-credential-" + strings.Repeat("a", 12) + "-obs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := BindRigRuntimeResources(
				cfg.StateRoot, lease.Token(), []string{tc.container}, nil, nil,
			); err == nil || !strings.Contains(err.Error(), "outside the owned namespace") {
				t.Fatalf("bind %q = %v, want outside-namespace refusal", tc.container, err)
			}
		})
	}
}

func TestRigManifestRejectsNoncanonicalEncoding(t *testing.T) {
	root := t.TempDir()
	cfg := rigTestConfig(t, filepath.Join(root, "state"), filepath.Join(root, "seed"))
	lease, err := AcquireRig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	path := filepath.Join(cfg.StateRoot, stateManifestName)
	// #nosec G304 -- the test path uses a fixed manifest basename under t.TempDir.
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	noncanonical := strings.Replace(string(body), "{\n", "{ \n", 1)
	// #nosec G703 -- the test path uses a fixed manifest basename under t.TempDir.
	if err := os.WriteFile(path, []byte(noncanonical), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRigManifest(cfg.StateRoot); err == nil ||
		!strings.Contains(err.Error(), "not in canonical form") {
		t.Fatalf("noncanonical manifest error = %v", err)
	}
}

func TestRigManifestRevalidatesDecodedResourceAuthority(t *testing.T) {
	root := t.TempDir()
	cfg := rigTestConfig(t, filepath.Join(root, "state"), filepath.Join(root, "seed"))
	lease, err := AcquireRig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	path := filepath.Join(cfg.StateRoot, stateManifestName)

	for _, test := range []struct {
		name      string
		mutate    func(*RigManifest)
		wantError string
	}{
		{
			name: "substituted database",
			mutate: func(manifest *RigManifest) {
				manifest.Resources.DatabasePath = filepath.Join(root, "idle.db")
			},
			wantError: "canonical freeside.db under the state root",
		},
		{
			name: "unsupported listener",
			mutate: func(manifest *RigManifest) {
				manifest.Resources.ListenAddress = "127.0.0.1:0"
			},
			wantError: "unsupported address",
		},
		{
			name: "substituted canonical listener",
			mutate: func(manifest *RigManifest) {
				manifest.Resources.ListenAddress = "127.0.0.1:8678"
			},
			wantError: "differs from its acquisition metadata",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			manifest := lease.Manifest()
			test.mutate(&manifest)
			if err := writeRigManifest(path, manifest); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadRigManifest(cfg.StateRoot); err == nil ||
				!strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("decoded authority error = %v, want %q", err, test.wantError)
			}
		})
	}
}

func TestRigManifestRejectsSubstitutedRuntimeResourceAuthority(t *testing.T) {
	root := t.TempDir()
	cfg := rigTestConfig(t, filepath.Join(root, "state"), filepath.Join(root, "seed"))
	lease, err := AcquireRig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	legitimatePrefix := "freeside-handoff-c" + strings.Repeat("a", 31)
	manifest, err := BindRigRuntimeResources(
		cfg.StateRoot, lease.Token(),
		[]string{legitimatePrefix + "-agent"},
		[]string{legitimatePrefix + "-ws"},
		[]string{legitimatePrefix + "-egress"},
	)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.StateRoot, stateManifestName)
	forgedPrefix := "freeside-handoff-c" + strings.Repeat("b", 31)

	for _, test := range []struct {
		name   string
		mutate func(*RigManifest)
	}{
		{
			name: "container",
			mutate: func(candidate *RigManifest) {
				candidate.Resources.Containers = append(candidate.Resources.Containers, forgedPrefix+"-agent")
			},
		},
		{
			name: "volume",
			mutate: func(candidate *RigManifest) {
				candidate.Resources.Volumes = append(candidate.Resources.Volumes, forgedPrefix+"-ws")
			},
		},
		{
			name: "network",
			mutate: func(candidate *RigManifest) {
				candidate.Resources.Networks = append(candidate.Resources.Networks, forgedPrefix+"-egress")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := cloneRigManifest(manifest)
			test.mutate(&candidate)
			if err := writeRigManifest(path, candidate); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadRigManifest(cfg.StateRoot); err == nil ||
				!strings.Contains(err.Error(), "not authorized by durable amendment metadata") {
				t.Fatalf("substituted %s authority error = %v", test.name, err)
			}
		})
	}
}

func TestRigManifestIgnoresInterruptedFinalAuthorityRecord(t *testing.T) {
	root := t.TempDir()
	cfg := rigTestConfig(t, filepath.Join(root, "state"), filepath.Join(root, "seed"))
	lease, err := AcquireRig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	prefix := "freeside-handoff-c" + strings.Repeat("a", 31)
	want, err := BindRigRuntimeResources(
		cfg.StateRoot, lease.Token(), []string{prefix + "-agent"}, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(cfg.StateRoot, rigLockName)
	// #nosec G304 -- the test path uses a fixed lock basename under t.TempDir.
	f, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("999 incomplete"); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRigManifest(cfg.StateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Resources.Containers, want.Resources.Containers) {
		t.Fatalf("resources after interrupted record = %v, want %v",
			got.Resources.Containers, want.Resources.Containers)
	}
}

func TestStaleRigRecoveryRejectsSubstitutedSeedResourceAuthority(t *testing.T) {
	root := t.TempDir()
	cfg := rigTestConfig(t, filepath.Join(root, "state"), filepath.Join(root, "seed"))
	lease, err := AcquireRig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	prefix := "freeside-handoff-c" + strings.Repeat("a", 31)
	manifest, err := BindRigRuntimeResources(
		cfg.StateRoot, lease.Token(), []string{prefix + "-agent"}, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Resources.Containers = append(
		manifest.Resources.Containers,
		"freeside-handoff-c"+strings.Repeat("b", 31)+"-agent",
	)
	if err := writeRigManifest(filepath.Join(cfg.SeedRoot, seedManifestName), manifest); err != nil {
		t.Fatal(err)
	}
	if err := lease.Abandon(); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireStaleRig(cfg.StateRoot); err == nil ||
		!strings.Contains(err.Error(), "seed-root rig resources are not authorized") {
		t.Fatalf("substituted seed authority error = %v", err)
	}
}

func TestRigManifestRejectsSubstitutedGlobalResourceAuthority(t *testing.T) {
	root := t.TempDir()
	cfg := rigTestConfig(t, filepath.Join(root, "state"), filepath.Join(root, "seed"))
	lease, err := AcquireRig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	prefix := "freeside-handoff-c" + strings.Repeat("a", 31)
	manifest, err := BindRigRuntimeResources(
		cfg.StateRoot, lease.Token(), []string{prefix + "-agent"}, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Resources.Containers = append(
		manifest.Resources.Containers,
		"freeside-handoff-c"+strings.Repeat("b", 31)+"-agent",
	)
	if err := writeRigManifest(globalRigStatePath(manifest.Resources), manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRigManifest(cfg.StateRoot); err == nil ||
		!strings.Contains(err.Error(), "global rig resources are not authorized") {
		t.Fatalf("substituted global authority error = %v", err)
	}
}

func TestRigManifestRejectsRecomputedChecksumWithoutAmendmentSignature(t *testing.T) {
	root := t.TempDir()
	cfg := rigTestConfig(t, filepath.Join(root, "state"), filepath.Join(root, "seed"))
	lease, err := AcquireRig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	prefix := "freeside-handoff-c" + strings.Repeat("a", 31)
	if _, err := BindRigRuntimeResources(
		cfg.StateRoot, lease.Token(), []string{prefix + "-agent"}, nil, nil,
	); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(cfg.StateRoot, rigLockName)
	// #nosec G304 -- the test path uses a fixed lock basename under t.TempDir.
	body, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSuffix(body, []byte{'\n'}), []byte{'\n'})
	record, err := decodeRigLockMetadataRecord(lines[len(lines)-1])
	if err != nil {
		t.Fatal(err)
	}
	record.Manifest.Resources.Containers = append(
		record.Manifest.Resources.Containers,
		"freeside-handoff-c"+strings.Repeat("b", 31)+"-agent",
	)
	payload, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	lines[len(lines)-1] = fmt.Appendf(nil, "%d %s %s", len(payload), hex.EncodeToString(digest[:]), payload)
	forged := append(bytes.Join(lines, []byte{'\n'}), '\n')
	// #nosec G703 -- the test path uses a fixed lock basename under t.TempDir.
	if err := os.WriteFile(lockPath, forged, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRigManifest(cfg.StateRoot); err == nil ||
		!strings.Contains(err.Error(), "record signature is invalid") {
		t.Fatalf("forged signed authority error = %v", err)
	}
}

func TestRigManifestRejectsDatabaseAliasReplacement(t *testing.T) {
	root := t.TempDir()
	cfg := rigTestConfig(t, filepath.Join(root, "state"), filepath.Join(root, "seed"))
	lease, err := AcquireRig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	idleDatabase := filepath.Join(root, "idle.db")
	if err := os.WriteFile(idleDatabase, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(idleDatabase, cfg.DatabasePath); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRigManifest(cfg.StateRoot); err == nil ||
		!strings.Contains(err.Error(), "no longer resolves to the leased path") {
		t.Fatalf("database alias replacement error = %v", err)
	}
}
