package ward_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// TestExporterInputScriptsLive runs TestInputScriptsDeliverManifestTreeAndRejectChanges
// as root inside the exporter image, so the input seeder and observer scripts
// meet the BusyBox tar, stat, find, and sha256sum they run with in
// production, on a case-sensitive filesystem. Register its single container
// with the caller's held rig; never acquire a second rig.
//
//nolint:paralleltest // Serial: live suite on the shared Apple container runtime; not run in parallel
func TestExporterInputScriptsLive(t *testing.T) {
	if os.Getenv("FREESIDE_INPUT_SCRIPT_LIVE") != "1" {
		t.Skip("set FREESIDE_INPUT_SCRIPT_LIVE=1, FREESIDE_WARD_EXPORTER_IMAGE, FREESIDE_RIG_ACQUISITION and FREESIDE_STATE_DIR; requires a held rig and the cached exporter image")
	}
	image := os.Getenv("FREESIDE_WARD_EXPORTER_IMAGE")
	if image == "" {
		t.Fatal("FREESIDE_WARD_EXPORTER_IMAGE is required")
	}
	binaryDir := t.TempDir()
	build := exec.CommandContext(t.Context(), "go", "test", "-c", "-o", filepath.Join(binaryDir, "ward.test"), ".") //nolint:gosec // fixed toolchain invocation
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("cross-compile ward tests: %v: %s", err, out)
	}
	acquisition, err := os.ReadFile(os.Getenv("FREESIDE_RIG_ACQUISITION")) //nolint:gosec // explicit operator-selected acquisition file for this opt-in probe
	if err != nil {
		t.Fatal("read rig acquisition:", err)
	}
	var rig struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(acquisition, &rig); err != nil {
		t.Fatal("decode rig acquisition:", err)
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	name := "freeside-handoff-c" + hex.EncodeToString(random[:])[:31] + "-observer"
	if _, err := daemonlock.BindRigRuntimeResources(os.Getenv("FREESIDE_STATE_DIR"), rig.Token, []string{name}, nil, nil); err != nil {
		t.Fatal("register probe resource:", err)
	}
	authorization, err := daemonlock.AuthorizeRig(os.Getenv("FREESIDE_STATE_DIR"), rig.Token)
	if err != nil {
		t.Fatal("hold probe authorization:", err)
	}
	// Registered before the resource cleanup so LIFO cleanup releases the rig
	// gate only after the probe is gone.
	t.Cleanup(func() {
		if err := authorization.Close(); err != nil {
			t.Error(err)
		}
	})
	bin, err := exec.LookPath("container")
	if err != nil {
		t.Fatal(err)
	}
	rt := ward.NewCLIRuntime(bin)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	labels := []ward.Label{{Key: "freeside.prompt-probe", Value: name}}
	if err := rt.CreateContainer(ctx, ward.ContainerSpec{
		Name: name, Image: image, NetworkDisabled: true, Labels: labels,
		Size: ward.DefaultLaunchSize(ward.LaunchConformance),
		Command: []string{"sh", "-c", "n=0; while [ ! -f /probe-ready/ready ]; do n=$((n+1)); [ \"$n\" -lt 90 ] || exit 1; sleep 1; done; " +
			"cd /tmp && /probe/ward.test -test.run '^TestInputScriptsDeliverManifestTreeAndRejectChanges$' -test.v -test.count=1"},
	}); err != nil {
		t.Fatal(err)
	}
	before, err := rt.Inspect(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if !before.LabelsObserved || !slices.Equal(before.Labels, labels) || before.CreationDate == "" {
		t.Fatal("probe ownership unproved; leave resource for rig recovery")
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		rep, err := rt.Inspect(cleanup, name)
		if err != nil || rep.CreationDate != before.CreationDate || !rep.LabelsObserved || !slices.Equal(rep.Labels, labels) {
			t.Error("probe ownership changed; leave resource for rig recovery")
			return
		}
		if rep.State != ward.StateStopped {
			if err := rt.StopContainer(cleanup, name); err != nil {
				t.Error(err)
				return
			}
		}
		if err := rt.DeleteContainer(cleanup, name); err != nil {
			t.Error(err)
		}
	})
	ready := t.TempDir()
	if err := os.WriteFile(filepath.Join(ready, "ready"), []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rt.StartContainer(ctx, name); err != nil {
		t.Fatal(err)
	}
	for _, copy := range []struct{ source, target string }{{binaryDir, "/probe"}, {ready, "/probe-ready"}} {
		if err := rt.CopyIntoContainer(ctx, name, copy.source, copy.target); err != nil {
			t.Fatal(err)
		}
	}
	// Real time only bounds this opt-in I/O probe.
	poll := time.NewTicker(250 * time.Millisecond)
	defer poll.Stop()
	for {
		rep, err := rt.Inspect(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if rep.State == ward.StateStopped {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("probe completion:", ctx.Err())
		case <-poll.C:
		}
	}
	output, err := exec.CommandContext(ctx, bin, "logs", name).CombinedOutput() //nolint:gosec // resolved CLI, fixed operation, fixture-owned container
	if err != nil {
		t.Fatalf("read probe output: %v", err)
	}
	t.Log(strings.TrimSpace(string(output)))
	if !strings.Contains(string(output), "\nPASS") || strings.Contains(string(output), "case-insensitive") {
		t.Fatal("input scripts did not pass with every name on a case-sensitive filesystem")
	}
}
