package daemoncompose

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/operations"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// TestCredentialIntegrityProbeRunOnce: each call observes once and hands back
// a replay of that result, so a later pass never reuses an earlier pass's
// observation, and no probe stays no probe.
func TestCredentialIntegrityProbeRunOnce(t *testing.T) {
	t.Parallel()
	if replay := CredentialIntegrityProbe(nil).runOnce(t.Context()); replay != nil {
		t.Error("a nil probe produced a replay, so a pass with no probe would claim it probed")
	}
	calls := 0
	failure := errors.New("probe failed")
	probe := CredentialIntegrityProbe(func(context.Context) ([]operations.CredentialIntegrityOutcome, error) {
		calls++
		if calls == 2 {
			return nil, failure
		}
		return []operations.CredentialIntegrityOutcome{{EnrollmentID: "claude-main/claude_code"}}, nil
	})
	first := probe.runOnce(t.Context())
	if calls != 1 {
		t.Fatalf("runOnce observed %d times, want once before it returns", calls)
	}
	for range 2 {
		outcomes, err := first(t.Context())
		if err != nil || len(outcomes) != 1 || calls != 1 {
			t.Fatalf("replay = %+v, %v after %d observations, want the first result and no new observation", outcomes, err, calls)
		}
	}
	second := probe.runOnce(t.Context())
	if _, err := second(t.Context()); !errors.Is(err, failure) || calls != 2 {
		t.Errorf("second pass = %v after %d observations, want a fresh observation's own error", err, calls)
	}
}

// observerCreateRuntime holds one volume and refuses every container create,
// recording the name it was asked for. Inspect fails too, so the observer's
// cleanup finds nothing to reap; any other call panics on the nil embedded
// runtime, so the test fails rather than passing on an unexpected path.
type observerCreateRuntime struct {
	ward.Runtime
	volume  string
	created []string
}

func (r *observerCreateRuntime) ListVolumes(context.Context) ([]ward.VolumeSummary, error) {
	return []ward.VolumeSummary{{Name: r.volume}}, nil
}

func (r *observerCreateRuntime) CreateContainer(_ context.Context, spec ward.ContainerSpec) error {
	r.created = append(r.created, spec.Name)
	return errors.New("create refused by test runtime")
}

func (r *observerCreateRuntime) Inspect(context.Context, string) (ward.InspectReport, error) {
	return ward.InspectReport{}, errors.New("no such container")
}

// TestProductionCredentialIntegrityObserversBindSetupTokenObserver: the
// probe's setup-token observation records its observer container in the
// held rig manifest before it asks the runtime to create it, so rig cleanup
// can find an observer a crash leaves behind. The test reads the name from
// the create request, so it fails if the probe observes without the rig
// authorizer, which still creates the container but never binds its name.
//
//nolint:paralleltest // Serial: rigCommandRoots sets HOME with t.Setenv.
func TestProductionCredentialIntegrityObserversBindSetupTokenObserver(t *testing.T) {
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

	runtime := &observerCreateRuntime{volume: "freeside-auth-claude-main"}
	cfg := ClaudeDriverConfig{
		StateDir: stateRoot, RigTokenFile: tokenFile,
		ExporterImage: "ghcr.io/x/exporter@sha256:" + strings.Repeat("b", 64),
	}
	if _, err := ProductionCredentialIntegrityObservers(cfg, runtime).SetupToken(t.Context(), runtime.volume); err == nil {
		t.Fatal("observation succeeded against a runtime that refuses every create")
	}
	if len(runtime.created) != 1 {
		t.Fatalf("observer creates = %v, want one", runtime.created)
	}
	manifest, err := daemonlock.ReadRigManifest(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(manifest.Resources.Containers, runtime.created[0]) {
		t.Fatalf("bound containers = %v, want the integrity observer %q", manifest.Resources.Containers, runtime.created[0])
	}
}
