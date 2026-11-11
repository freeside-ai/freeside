package daemonlock

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func rigAuthorityRecordCount(t *testing.T, stateRoot string) int {
	t.Helper()
	// #nosec G304 -- the test path uses a fixed lock basename under t.TempDir.
	body, err := os.ReadFile(filepath.Join(stateRoot, rigLockName))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Count(body, []byte{'\n'})
}

func rigMirrorPaths(manifest RigManifest) []string {
	_, manifestPaths := rigPaths(manifest.Resources)
	return append([]string{globalRigStatePath(manifest.Resources)}, manifestPaths...)
}

func TestRigUnbindRemovesNamesFromAuthorityAndEveryMirror(t *testing.T) {
	root := t.TempDir()
	cfg := rigTestConfig(t, filepath.Join(root, "state"), filepath.Join(root, "seed"))
	lease, err := AcquireRig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	kept := "freeside-handoff-c" + strings.Repeat("a", 31)
	released := "freeside-handoff-c" + strings.Repeat("b", 31)
	if _, err := BindRigRuntimeResources(
		cfg.StateRoot, lease.Token(),
		[]string{kept + "-agent", released + "-agent"},
		[]string{kept + "-ws", released + "-ws"},
		[]string{kept + "-egress", released + "-egress"},
	); err != nil {
		t.Fatal(err)
	}
	records := rigAuthorityRecordCount(t, cfg.StateRoot)
	got, err := UnbindRigRuntimeResources(
		cfg.StateRoot, lease.Token(),
		[]string{released + "-agent"}, []string{released + "-ws"}, []string{released + "-egress"},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := RigResources{
		Containers: []string{kept + "-agent"}, Volumes: []string{kept + "-ws"},
		Networks: []string{kept + "-egress"},
	}
	assertRigNames := func(source string, resources RigResources) {
		t.Helper()
		if !slices.Equal(resources.Containers, want.Containers) ||
			!slices.Equal(resources.Volumes, want.Volumes) ||
			!slices.Equal(resources.Networks, want.Networks) {
			t.Errorf("%s names = %v %v %v, want %v %v %v", source,
				resources.Containers, resources.Volumes, resources.Networks,
				want.Containers, want.Volumes, want.Networks)
		}
	}
	assertRigNames("unbind result", got.Resources)
	authority, err := ReadRigManifest(cfg.StateRoot)
	if err != nil {
		t.Fatal(err)
	}
	assertRigNames("authority", authority.Resources)
	for _, path := range rigMirrorPaths(got) {
		mirror, err := readRigManifest(path)
		if err != nil {
			t.Fatal(err)
		}
		assertRigNames(path, mirror.Resources)
	}
	if after := rigAuthorityRecordCount(t, cfg.StateRoot); after != records+1 {
		t.Fatalf("authority records after unbind = %d, want %d", after, records+1)
	}
	// A released name can be bound again: the log then holds a shrink
	// followed by a grow.
	if _, err := BindRigRuntimeResources(
		cfg.StateRoot, lease.Token(), []string{released + "-agent"}, nil, nil,
	); err != nil {
		t.Fatal(err)
	}
	rebound, err := ReadRigManifest(cfg.StateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(rebound.Resources.Containers, released+"-agent") {
		t.Fatalf("rebound containers = %v, want %q", rebound.Resources.Containers, released+"-agent")
	}
}

func TestRigUnbindRequiresLiveHolderAndToken(t *testing.T) {
	root := t.TempDir()
	cfg := rigTestConfig(t, filepath.Join(root, "state"), filepath.Join(root, "seed"))
	lease, err := AcquireRig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	name := "freeside-handoff-c" + strings.Repeat("a", 31) + "-agent"
	if _, err := BindRigRuntimeResources(cfg.StateRoot, lease.Token(), []string{name}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := UnbindRigRuntimeResources(cfg.StateRoot, "wrong", []string{name}, nil, nil); !errors.Is(err, ErrRigToken) {
		t.Fatalf("wrong-token unbind = %v, want ErrRigToken", err)
	}
	if err := lease.Abandon(); err != nil {
		t.Fatal(err)
	}
	if _, err := UnbindRigRuntimeResources(cfg.StateRoot, lease.Token(), []string{name}, nil, nil); !errors.Is(err, ErrRigNotHeld) {
		t.Fatalf("unbind after holder death = %v, want ErrRigNotHeld", err)
	}
	manifest, err := ReadRigManifest(cfg.StateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(manifest.Resources.Containers, []string{name}) {
		t.Fatalf("containers after refused unbinds = %v, want %q", manifest.Resources.Containers, name)
	}
}

func TestRigNoOpAmendmentWritesNothing(t *testing.T) {
	root := t.TempDir()
	cfg := rigTestConfig(t, filepath.Join(root, "state"), filepath.Join(root, "seed"))
	lease, err := AcquireRig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	name := "freeside-handoff-c" + strings.Repeat("a", 31) + "-agent"
	never := "freeside-handoff-c" + strings.Repeat("b", 31) + "-agent"
	if _, err := BindRigRuntimeResources(cfg.StateRoot, lease.Token(), []string{name}, nil, nil); err != nil {
		t.Fatal(err)
	}
	paths := append([]string{filepath.Join(cfg.StateRoot, rigLockName)}, rigMirrorPaths(lease.Manifest())...)
	before := make(map[string][]byte, len(paths))
	for _, path := range paths {
		// #nosec G304 -- fixed test paths under t.TempDir.
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = body
	}
	for _, amend := range []struct {
		name string
		run  func() (RigManifest, error)
	}{
		{"rebind", func() (RigManifest, error) {
			return BindRigRuntimeResources(cfg.StateRoot, lease.Token(), []string{name}, nil, nil)
		}},
		{"unbind never bound", func() (RigManifest, error) {
			return UnbindRigRuntimeResources(cfg.StateRoot, lease.Token(), []string{never}, nil, nil)
		}},
		{"empty unbind", func() (RigManifest, error) {
			return UnbindRigRuntimeResources(cfg.StateRoot, lease.Token(), nil, nil, nil)
		}},
	} {
		manifest, err := amend.run()
		if err != nil {
			t.Fatalf("%s: %v", amend.name, err)
		}
		if !slices.Equal(manifest.Resources.Containers, []string{name}) {
			t.Fatalf("%s containers = %v, want %q", amend.name, manifest.Resources.Containers, name)
		}
	}
	for _, path := range paths {
		// #nosec G304 -- fixed test paths under t.TempDir.
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(after, before[path]) {
			t.Errorf("no-op amendment rewrote %s", path)
		}
	}
}

// An unbind that published its shrunk mirrors but never appended the shrunk
// authority leaves each mirror a subset of the authority. Both readers must
// accept that and return the larger, authoritative set.
func TestRigInterruptedUnbindKeepsLargerAuthority(t *testing.T) {
	root := t.TempDir()
	cfg := rigTestConfig(t, filepath.Join(root, "state"), filepath.Join(root, "seed"))
	lease, err := AcquireRig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	prefix := "freeside-handoff-c" + strings.Repeat("a", 31)
	bound, err := BindRigRuntimeResources(
		cfg.StateRoot, lease.Token(),
		[]string{prefix + "-agent"}, []string{prefix + "-ws"}, []string{prefix + "-egress"},
	)
	if err != nil {
		t.Fatal(err)
	}
	shrunk := cloneRigManifest(bound)
	shrunk.Resources.Containers = nil
	shrunk.Resources.Volumes = nil
	shrunk.Resources.Networks = nil
	for _, path := range rigMirrorPaths(bound) {
		if err := writeRigManifest(path, shrunk); err != nil {
			t.Fatal(err)
		}
	}
	assertBound := func(source string, resources RigResources) {
		t.Helper()
		if !slices.Equal(resources.Containers, bound.Resources.Containers) ||
			!slices.Equal(resources.Volumes, bound.Resources.Volumes) ||
			!slices.Equal(resources.Networks, bound.Resources.Networks) {
			t.Fatalf("%s resources = %+v, want the authority's %+v", source, resources, bound.Resources)
		}
	}
	read, err := ReadRigManifest(cfg.StateRoot)
	if err != nil {
		t.Fatalf("read after interrupted unbind: %v", err)
	}
	assertBound("ReadRigManifest", read.Resources)
	if err := lease.Abandon(); err != nil {
		t.Fatal(err)
	}
	recovery, err := AcquireStaleRig(cfg.StateRoot)
	if err != nil {
		t.Fatalf("stale recovery after interrupted unbind: %v", err)
	}
	assertBound("stale recovery", recovery.Manifest().Resources)
	if err := recovery.Close(); err != nil {
		t.Fatal(err)
	}
}

// appendSignedRigRecord appends a correctly signed authority record, so the
// test exercises the log's shape rule rather than its signature check.
func appendSignedRigRecord(t *testing.T, stateRoot, token string, manifest RigManifest) {
	t.Helper()
	if err := appendRigLockMetadata(filepath.Join(stateRoot, rigLockName), manifest, token); err != nil {
		t.Fatal(err)
	}
}

func TestRigAuthorityLogRejectsSignedRecordThatAddsAndDrops(t *testing.T) {
	root := t.TempDir()
	cfg := rigTestConfig(t, filepath.Join(root, "state"), filepath.Join(root, "seed"))
	lease, err := AcquireRig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	legitimate := "freeside-handoff-c" + strings.Repeat("a", 31)
	forged := "freeside-handoff-c" + strings.Repeat("b", 31)
	bound, err := BindRigRuntimeResources(
		cfg.StateRoot, lease.Token(),
		[]string{legitimate + "-agent"}, []string{legitimate + "-ws"}, []string{legitimate + "-egress"},
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*RigResources)
	}{
		{"container", func(r *RigResources) { r.Containers = []string{forged + "-agent"} }},
		{"volume", func(r *RigResources) { r.Volumes = []string{forged + "-ws"} }},
		{"network", func(r *RigResources) { r.Networks = []string{forged + "-egress"} }},
		{"across kinds", func(r *RigResources) {
			r.Containers = append(r.Containers, forged+"-agent")
			r.Volumes = nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			lockPath := filepath.Join(cfg.StateRoot, rigLockName)
			// #nosec G304 -- the test path uses a fixed lock basename under t.TempDir.
			original, err := os.ReadFile(lockPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// #nosec G703 -- the test path uses a fixed lock basename under t.TempDir.
				if err := os.WriteFile(lockPath, original, 0o600); err != nil {
					t.Fatal(err)
				}
			})
			substituted := cloneRigManifest(bound)
			test.mutate(&substituted.Resources)
			appendSignedRigRecord(t, cfg.StateRoot, lease.Token(), substituted)
			if _, err := ReadRigManifest(cfg.StateRoot); err == nil ||
				!strings.Contains(err.Error(), "both adds and drops runtime resources") {
				t.Fatalf("substituting record error = %v", err)
			}
			if _, err := UnbindRigRuntimeResources(
				cfg.StateRoot, lease.Token(), []string{legitimate + "-agent"}, nil, nil,
			); err == nil {
				t.Fatal("unbind accepted a log holding a substituting record")
			}
		})
	}
	// The same substitution split into a shrink and a grow is two legitimate
	// records.
	if _, err := UnbindRigRuntimeResources(
		cfg.StateRoot, lease.Token(), []string{legitimate + "-agent"}, nil, nil,
	); err != nil {
		t.Fatalf("shrink: %v", err)
	}
	if _, err := BindRigRuntimeResources(
		cfg.StateRoot, lease.Token(), []string{forged + "-agent"}, nil, nil,
	); err != nil {
		t.Fatalf("grow: %v", err)
	}
	got, err := ReadRigManifest(cfg.StateRoot)
	if err != nil {
		t.Fatalf("shrink then grow: %v", err)
	}
	if !slices.Equal(got.Resources.Containers, []string{forged + "-agent"}) {
		t.Fatalf("containers after shrink then grow = %v, want %q", got.Resources.Containers, forged+"-agent")
	}
}

func TestRigAuthorityLogRejectsSignedIdentityChangeOnShrink(t *testing.T) {
	root := t.TempDir()
	cfg := rigTestConfig(t, filepath.Join(root, "state"), filepath.Join(root, "seed"))
	lease, err := AcquireRig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	bound, err := BindRigRuntimeResources(
		cfg.StateRoot, lease.Token(),
		[]string{"freeside-handoff-c" + strings.Repeat("a", 31) + "-agent"}, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	shrunk := cloneRigManifest(bound)
	shrunk.Resources.Containers = nil
	shrunk.Owner.Note = "rewritten"
	appendSignedRigRecord(t, cfg.StateRoot, lease.Token(), shrunk)
	if _, err := ReadRigManifest(cfg.StateRoot); err == nil ||
		!strings.Contains(err.Error(), "changes the rig identity") {
		t.Fatalf("identity-changing shrink error = %v", err)
	}
}

// After an unbind, a mirror that still names a released resource claims
// authority the log withdrew. Every reader must refuse it, exactly as it
// refuses a name the log never granted.
func TestRigMirrorRejectsNameAuthorityReleased(t *testing.T) {
	prefix := "freeside-handoff-c" + strings.Repeat("a", 31)
	released := "freeside-handoff-c" + strings.Repeat("b", 31) + "-agent"
	for _, test := range []struct {
		name    string
		mirror  func(RigManifest, RigAcquireConfig) string
		read    func(*testing.T, RigAcquireConfig, *RigLease) error
		message string
	}{
		{
			name: "state root",
			mirror: func(_ RigManifest, cfg RigAcquireConfig) string {
				return filepath.Join(cfg.StateRoot, stateManifestName)
			},
			read: func(_ *testing.T, cfg RigAcquireConfig, _ *RigLease) error {
				_, err := ReadRigManifest(cfg.StateRoot)
				return err
			},
			message: "rig manifest resources are not authorized",
		},
		{
			name:   "global",
			mirror: func(m RigManifest, _ RigAcquireConfig) string { return globalRigStatePath(m.Resources) },
			read: func(_ *testing.T, cfg RigAcquireConfig, _ *RigLease) error {
				_, err := ReadRigManifest(cfg.StateRoot)
				return err
			},
			message: "global rig resources are not authorized",
		},
		{
			name:   "seed root",
			mirror: func(_ RigManifest, cfg RigAcquireConfig) string { return filepath.Join(cfg.SeedRoot, seedManifestName) },
			read: func(t *testing.T, cfg RigAcquireConfig, lease *RigLease) error {
				if err := lease.Abandon(); err != nil {
					t.Fatal(err)
				}
				recovery, err := AcquireStaleRig(cfg.StateRoot)
				if err == nil {
					_ = recovery.Close()
				}
				return err
			},
			message: "seed-root rig resources are not authorized",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			cfg := rigTestConfig(t, filepath.Join(root, "state"), filepath.Join(root, "seed"))
			lease, err := AcquireRig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = lease.Close() })
			bound, err := BindRigRuntimeResources(
				cfg.StateRoot, lease.Token(), []string{prefix + "-agent", released}, nil, nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := UnbindRigRuntimeResources(
				cfg.StateRoot, lease.Token(), []string{released}, nil, nil,
			); err != nil {
				t.Fatal(err)
			}
			if err := writeRigManifest(test.mirror(bound, cfg), bound); err != nil {
				t.Fatal(err)
			}
			if err := test.read(t, cfg, lease); err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("stale %s mirror error = %v, want %q", test.name, err, test.message)
			}
		})
	}
}
