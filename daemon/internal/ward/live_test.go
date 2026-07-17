package ward

// The full-lifecycle integration test against the reference runtime (Apple
// container 1.1.0 on macOS). Opt-in, following the publish package's live
// test pattern:
//
//	FREESIDE_WARD_LIVE_TEST=1 go test ./internal/ward -run TestLiveHandoffLifecycle -v
//
// Not run in CI: GitHub's macOS runners do not provide Apple container, so
// this is a recorded verification gap; the PR/README documents it, and the
// scripted fake covers every failure path deterministically.
//
// Requirements: macOS with Apple container 1.1.0 on PATH, `container system
// start` done, and the pinned alpine:3.22 image pullable (or cached). The
// test uses uniquely named, labeled volumes and containers, and sweeps its
// own leftovers.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// liveImage is the spike's pinned test image: alpine 3.22, by digest, so the
// exporter placeholder payload is digest-pinned trusted compute.
const liveImage = "docker.io/library/alpine:3.22@sha256:2c9d26f410d032d5b1525aa8a873e238b05b90c4ae8618743d4311f0cc827e37"

// liveMarker is the spike's inert credential marker; the §5.4 scanner hook
// greps the verified output for it, and finding it fails the gate.
const liveMarker = "FREESIDE_FAKE_PROVIDER_CREDENTIAL_DO_NOT_EXPORT"

// liveExporterPayload is the placeholder exporter payload (issue #76 allows
// landing against one until the pinned exporter image with freeside-export
// exists): it runs check 5's probes, writes the proof file with the
// observed values, and emits a minimal valid §5.6 manifest plus the one
// content blob so check 7's digest verification runs against real exported
// bytes.
const liveExporterPayload = `
ws=rw
grep " /workspace " /proc/mounts | grep -q -e " ro," -e " ro " && ws=ro
wp=succeeded
touch /workspace/.write-probe 2>/dev/null || wp=blocked
[ -e /credentials ] && cr=present || cr=absent
[ -e /Users ] && hh=present || hh=absent
printf "workspace_mounted=%s\nworkspace_write=%s\ncredentials=%s\nhost_home=%s\n" "$ws" "$wp" "$cr" "$hh" > /handoff-proof.txt
mkdir -p /handoff/blobs/sha256
d=$(sha256sum /workspace/result.txt); d=${d%% *}
s=$(wc -c < /workspace/result.txt)
cp /workspace/result.txt "/handoff/blobs/sha256/$d"
printf "{\"version\":\"freeside.export.manifest/v1\",\"entries\":[{\"path\":\"result.txt\",\"kind\":\"regular\",\"mode\":\"0644\",\"size\":%s,\"digest\":\"sha256:%s\",\"target\":null}]}" "$s" "$d" > /handoff/manifest.json
`

func TestLiveHandoffLifecycle(t *testing.T) {
	if os.Getenv("FREESIDE_WARD_LIVE_TEST") != "1" {
		t.Skip("live workspace-handoff test skipped: set FREESIDE_WARD_LIVE_TEST=1 (requires macOS, Apple container 1.1.0, `container system start`, and the pinned alpine:3.22 image)")
	}
	bin, err := osexec.LookPath("container")
	if err != nil {
		t.Fatalf("container CLI not on PATH: %v", err)
	}
	// Best-effort pre-pull; a cached image makes this a fast no-op.
	if out, err := osexec.Command(bin, "image", "pull", liveImage).CombinedOutput(); err != nil { //nolint:gosec // fixed args, resolved CLI path
		t.Logf("image pull (continuing; may be cached): %v: %s", err, out)
	}

	ctx := context.Background()
	rt := NewCLIRuntime(bin)
	runID := fmt.Sprintf("live-%d", time.Now().Unix())
	names := namesFor(runID)

	// Failsafe sweep so an aborted assertion cannot orphan runtime state;
	// every call is best-effort on already-clean state. The seed container is
	// swept too: a failure before its explicit delete would otherwise keep it
	// attached to the credential volume and block that volume's deletion.
	credVolume := "freeside-ward-live-cred-" + runID
	seedName := "freeside-ward-live-seed-" + runID
	t.Cleanup(func() {
		for _, c := range []string{names.Agent, names.Exporter, seedName} {
			_ = rt.StopContainer(ctx, c)
			_ = rt.DeleteContainer(ctx, c)
		}
		_ = rt.DeleteVolume(ctx, names.Workspace)
		_ = rt.DeleteVolume(ctx, credVolume)
	})

	// Seed the caller-owned credential volume with the inert marker.
	if err := rt.CreateVolume(ctx, credVolume, 8, []Label{{Key: "freeside.ward-live", Value: runID}}); err != nil {
		t.Fatalf("create credential volume: %v", err)
	}
	if err := rt.CreateContainer(ctx, ContainerSpec{
		Name:    seedName,
		Image:   liveImage,
		Command: []string{"sh", "-c", "printf " + liveMarker + " > /credentials/token"},
		Mounts:  []Mount{{Type: MountVolume, Source: credVolume, Target: "/credentials"}},
	}); err != nil {
		t.Fatalf("create seed container: %v", err)
	}
	if err := rt.StartContainer(ctx, seedName); err != nil {
		t.Fatalf("start seed container: %v", err)
	}
	waitLiveStopped(t, rt, seedName)
	if err := rt.DeleteContainer(ctx, seedName); err != nil {
		t.Fatalf("delete seed container: %v", err)
	}

	// The scanner proves the marker never reaches the verified output, and
	// that it actually saw the extracted files.
	scannedFiles := 0
	scanner := scannerFunc(func(_ context.Context, dir string) error {
		return filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			data, err := os.ReadFile(p) //nolint:gosec // walking the gate-owned verified output
			if err != nil {
				return err
			}
			scannedFiles++
			if bytes.Contains(data, []byte(liveMarker)) {
				return fmt.Errorf("credential marker found in %s", p)
			}
			return nil
		})
	})

	cfg := Config{
		ExporterImage:     liveImage,
		ExporterCommand:   []string{"sh", "-c", liveExporterPayload},
		WriterStopTimeout: 3 * time.Minute,
		ExporterTimeout:   3 * time.Minute,
		Scanner:           scanner,
	}
	b, err := New(rt, cfg)
	if err != nil {
		t.Fatal(err)
	}

	res, err := b.Handoff(ctx, HandoffSpec{
		RunID:           runID,
		WorkspaceSizeMB: 64,
		Agent: AgentSpec{
			Image: liveImage,
			Command: []string{
				"sh", "-c",
				"cat /credentials/token > /dev/null && " +
					"echo agent-output > /workspace/result.txt && " +
					"mkdir -p /workspace/nested && " +
					"echo durable-workspace > /workspace/nested/state.txt",
			},
			CredentialMounts: []CredentialMount{{Volume: credVolume, Target: "/credentials"}},
		},
	})
	if err != nil {
		t.Fatalf("Handoff = %v, want success", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(res.ExportDir) })

	// Admission snapshot (acceptance 4, on the reference runtime).
	if res.Admission.Backend != BackendName {
		t.Errorf("Admission.Backend = %q, want %q", res.Admission.Backend, BackendName)
	}

	// The manifest binds the one exported file; its blob's bytes are what
	// the agent wrote with the workspace read-write.
	if len(res.Manifest.Entries) != 1 || res.Manifest.Entries[0].Path != "result.txt" {
		t.Fatalf("manifest entries = %+v, want the one result.txt entry", res.Manifest.Entries)
	}
	hexDigest := strings.TrimPrefix(string(*res.Manifest.Entries[0].Digest), "sha256:")
	blob, err := os.ReadFile(filepath.Join(res.ExportDir, "blobs", "sha256", hexDigest)) //nolint:gosec // digest from the verified manifest
	if err != nil {
		t.Fatalf("read exported blob: %v", err)
	}
	if string(blob) != "agent-output\n" {
		t.Errorf("exported blob = %q, want the agent's write", blob)
	}
	if scannedFiles == 0 {
		t.Error("scanner hook never saw a file")
	}

	// Teardown left nothing it owns (acceptance 5): the run's containers and
	// workspace volume are gone; only the caller-owned credential volume
	// remains, still holding the contained marker.
	ctrs, err := rt.ListContainers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range ctrs {
		if c.ID == names.Agent || c.ID == names.Exporter {
			t.Errorf("container %q survived teardown", c.ID)
		}
	}
	vols, err := rt.ListVolumes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	credSurvives := false
	for _, v := range vols {
		if v.Name == names.Workspace {
			t.Errorf("workspace volume %q survived teardown", v.Name)
		}
		if v.Name == credVolume {
			credSurvives = true
		}
	}
	if !credSurvives {
		t.Error("caller-owned credential volume was deleted; containment must come from detachment, not deletion")
	}
	if err := rt.DeleteVolume(ctx, credVolume); err != nil {
		t.Errorf("delete credential volume: %v", err)
	}
}

// waitLiveStopped polls the real runtime until the container is observed
// stopped; test-setup plumbing, not the gate's own wait.
func waitLiveStopped(t *testing.T, rt Runtime, id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		rep, err := rt.Inspect(context.Background(), id)
		if err != nil {
			t.Fatalf("inspect %s: %v", id, err)
		}
		if rep.State == StateStopped {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s still %q after 2m", id, rep.State)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
