package ward

// Host-gated, permanent reference-runtime members of the workspace-handoff
// conformance suite (Apple container 1.1.0 on macOS). Opt-in, following the
// same convention as live_test.go:
//
//	FREESIDE_WARD_LIVE_TEST=1 go test ./internal/ward -run TestLiveConformance -v
//
// Not run in CI (GitHub macOS runners provide no Apple container); the gap is
// recorded in the PR and doc.go, and the scripted fake covers the suite
// orchestration and every induced check failure deterministically. These
// tests prove the passing runs and the negative probes on the real runtime:
//
//   - TestLiveConformanceSuite: Suite.Full (checks 1-5, 7 + the credential,
//     read-write-attach, and networkless-export probes) and Suite.PreJob pass.
//   - TestLiveConformanceSameVMRefutation: the third negative probe, driven
//     directly against the container CLI because it needs a CAP_SYS_ADMIN
//     guest process the gate's ContainerSpec deliberately cannot express.

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

func requireLiveContainer(t *testing.T) string {
	t.Helper()
	if os.Getenv("FREESIDE_WARD_LIVE_TEST") != "1" {
		t.Skip("live conformance test skipped: set FREESIDE_WARD_LIVE_TEST=1 (requires macOS, Apple container 1.1.0, `container system start`, and the pinned alpine:3.22 image)")
	}
	bin, err := osexec.LookPath("container")
	if err != nil {
		t.Fatalf("container CLI not on PATH: %v", err)
	}
	if out, err := osexec.Command(bin, "image", "pull", liveImage).CombinedOutput(); err != nil { //nolint:gosec // fixed args, resolved CLI path
		t.Logf("image pull (continuing; may be cached): %v: %s", err, out)
	}
	return bin
}

// liveSuiteExporterPayload is the suite's exporter shell. Unlike the handoff
// live test's liveExporterPayload (which manifests only result.txt, as that
// test asserts a single entry), it copies and manifests BOTH files the default
// writer produces — result.txt and nested/state.txt — which Suite.Full requires
// as this run's exact output. Entries are bytewise-sorted (nested precedes
// result), the manifest invariant.
const liveSuiteExporterPayload = `
ws=rw
grep " /workspace " /proc/mounts | grep -q -e " ro," -e " ro " && ws=ro
wp=succeeded
touch /workspace/.write-probe 2>/dev/null || wp=blocked
[ -e /credentials ] && cr=present || cr=absent
[ -e /Users ] && hh=present || hh=absent
printf "workspace_mounted=%s\nworkspace_write=%s\ncredentials=%s\nhost_home=%s\n" "$ws" "$wp" "$cr" "$hh" > /handoff-proof.txt
mkdir -p /handoff/blobs/sha256
dr=$(sha256sum /workspace/result.txt); dr=${dr%% *}
sr=$(wc -c < /workspace/result.txt)
cp /workspace/result.txt "/handoff/blobs/sha256/$dr"
dn=$(sha256sum /workspace/nested/state.txt); dn=${dn%% *}
sn=$(wc -c < /workspace/nested/state.txt)
cp /workspace/nested/state.txt "/handoff/blobs/sha256/$dn"
printf "{\"version\":\"freeside.export.manifest/v1\",\"entries\":[{\"path\":\"nested/state.txt\",\"kind\":\"regular\",\"mode\":\"0644\",\"size\":%s,\"digest\":\"sha256:%s\",\"target\":null},{\"path\":\"result.txt\",\"kind\":\"regular\",\"mode\":\"0644\",\"size\":%s,\"digest\":\"sha256:%s\",\"target\":null}]}" "$sn" "$dn" "$sr" "$dr" > /handoff/manifest.json
`

// TestLiveConformanceSuite runs the invocable suite against the reference
// runtime: Full proves checks 1-5, 7, the credential-containment probe, and
// the read-write-attach exclusion probe on real VMs, and PreJob proves the
// lightweight precondition path.
func TestLiveConformanceSuite(t *testing.T) {
	bin := requireLiveContainer(t)
	ctx := context.Background()
	rt := NewCLIRuntime(bin)
	runID := fmt.Sprintf("conf-%d", time.Now().UnixNano())

	// Failsafe sweep so an aborted assertion cannot orphan runtime state; the
	// suite reaps its own objects on every path, this only backstops a panic.
	names := namesFor(runID)
	prefix := conformanceObjectPrefix + runID + "-"
	t.Cleanup(func() {
		for _, c := range []string{
			names.Agent, names.Exporter,
			prefix + "seed", prefix + "audit", prefix + "prejob",
			prefix + networklessProbeSuffix, prefix + networklessLivenessProbeSuffix,
			prefix + "excl-writer", prefix + "excl-second",
		} {
			_ = rt.StopContainer(ctx, c)
			_ = rt.DeleteContainer(ctx, c)
		}
		for _, v := range []string{
			names.Workspace, prefix + "cred", prefix + "excl-ws",
			prefix + networklessLivenessVolumeProbeSuffix,
		} {
			_ = rt.DeleteVolume(ctx, v)
		}
	})

	// The §5.4 scanner proves the configured hook runs and never sees the
	// marker in the released output.
	scanner := scannerFunc(func(_ context.Context, dir string) error {
		return filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			data, err := os.ReadFile(p) //nolint:gosec // walking the gate-owned verified output
			if err != nil {
				return err
			}
			if bytes.Contains(data, []byte(liveMarker)) {
				return fmt.Errorf("credential marker found in %s", p)
			}
			return nil
		})
	})

	cfg := Config{
		ExporterImage:     liveImage,
		ExporterCommand:   []string{"sh", "-c", liveSuiteExporterPayload},
		WriterStopTimeout: 3 * time.Minute,
		ExporterTimeout:   3 * time.Minute,
		Scanner:           scanner,
	}
	b, err := New(rt, cfg)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSuite(b, SuiteFixture{
		AgentImage:       liveImage,
		CredentialMarker: liveMarker,
		RunID:            runID,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Full(ctx); err != nil {
		t.Fatalf("Full = %v, want nil", err)
	}
	if err := s.PreJob(ctx); err != nil {
		t.Fatalf("PreJob = %v, want nil", err)
	}
}

// TestLiveConformanceSameVMRefutation is the third negative probe as a
// permanent test: a guest CAP_SYS_ADMIN process unmounts the credential
// mount, then a later process in the same VM remounts the same block device
// and rereads the marker. The marker remaining readable proves the guest
// unmount is not a credential-device detach, so the same-VM fallback class is
// not isolation. It drives the CLI directly (--cap-add CAP_SYS_ADMIN is
// outside the gate's ContainerSpec vocabulary, by design).
func TestLiveConformanceSameVMRefutation(t *testing.T) {
	bin := requireLiveContainer(t)
	ctx := context.Background()
	rt := NewCLIRuntime(bin)
	runID := fmt.Sprintf("refute-%d", time.Now().UnixNano())
	ws := "freeside-ward-refute-ws-" + runID
	cred := "freeside-ward-refute-cred-" + runID
	sameVM := "freeside-ward-refute-samevm-" + runID

	t.Cleanup(func() {
		_, _ = run(bin, "stop", sameVM)
		_, _ = run(bin, "delete", sameVM)
		_ = rt.DeleteVolume(ctx, ws)
		_ = rt.DeleteVolume(ctx, cred)
	})

	if err := rt.CreateVolume(ctx, ws, 64, nil); err != nil {
		t.Fatalf("create workspace volume: %v", err)
	}
	if err := rt.CreateVolume(ctx, cred, 8, nil); err != nil {
		t.Fatalf("create credential volume: %v", err)
	}
	// Seed the credential marker.
	if out, err := run(bin, "run", "--rm", "--name", "freeside-ward-refute-seed-"+runID,
		"--mount", "type=volume,source="+cred+",target=/credentials",
		liveImage, "sh", "-c",
		"printf '%s\\n' "+liveMarker+" > /credentials/token; sync"); err != nil {
		t.Fatalf("seed credential: %v: %s", err, out)
	}

	// A live VM holds the workspace rw and the credential ro, with the
	// strongest plausible guest privilege to unmount.
	if out, err := run(bin, "run", "--detach", "--cap-add", "CAP_SYS_ADMIN", "--name", sameVM,
		"--mount", "type=volume,source="+ws+",target=/workspace",
		"--mount", "type=volume,source="+cred+",target=/credentials,readonly",
		liveImage, "sleep", "300"); err != nil {
		t.Fatalf("start same-VM container: %v: %s", err, out)
	}

	// Capture the credential block device, then guest-unmount it.
	if out, err := run(bin, "exec", sameVM, "sh", "-c",
		`grep " /credentials " /proc/mounts | cut -d " " -f 1 > /tmp/credential-device; umount /credentials; cat /tmp/credential-device`); err != nil {
		t.Fatalf("guest unmount: %v: %s", err, out)
	} else if !strings.HasPrefix(strings.TrimSpace(out), "/dev/") {
		t.Fatalf("guest unmount did not report a block device, got %q", out)
	}

	// The refutation: the same VM remounts the still-attached block device and
	// rereads the marker. Exit 0 means the marker survived the guest unmount,
	// proving it was never detached.
	out, err := run(bin, "exec", sameVM, "sh", "-c",
		`mkdir /credential-remount; mount -t ext4 -o ro "$(cat /tmp/credential-device)" /credential-remount; test "$(cat /credential-remount/token)" = `+liveMarker)
	if err != nil {
		t.Fatalf("same-VM guest unmount detached the credential (marker unreadable after remount): %v: %s\n"+
			"this would contradict the spike's refutation; the same-VM class must stay refuted", err, out)
	}
}

// run executes a container CLI subcommand and returns its combined output.
func run(bin string, args ...string) (string, error) {
	out, err := osexec.Command(bin, args...).CombinedOutput() //nolint:gosec // resolved CLI path, fixed test args
	return string(out), err
}
