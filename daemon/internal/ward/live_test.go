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

	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
)

// liveImage is the spike's pinned agent/base image: alpine 3.22, by digest.
const liveImage = "docker.io/library/alpine:3.22@sha256:2c9d26f410d032d5b1525aa8a873e238b05b90c4ae8618743d4311f0cc827e37"

// liveMarker is the spike's inert credential marker; the §5.4 scanner hook
// greps the verified output for it, and finding it fails the gate.
const liveMarker = "FREESIDE_FAKE_PROVIDER_CREDENTIAL_DO_NOT_EXPORT"

// liveExporterImage returns the digest-pinned exporter image the live tests run
// the real freeside-export helper from, read from FREESIDE_WARD_EXPORTER_IMAGE.
// Build and push it with `scripts/build-exporter-image.sh --registry <host>` and
// set the env to the printed reference (ward resolves a digest only through a
// registry, so a local-only build is not enough). The test skips when it is
// unset so the alpine-only members still run.
func liveExporterImage(t *testing.T) string {
	t.Helper()
	ref := os.Getenv("FREESIDE_WARD_EXPORTER_IMAGE")
	if ref == "" {
		t.Skip("exporter-image live test skipped: set FREESIDE_WARD_EXPORTER_IMAGE to the digest-pinned exporter image (scripts/build-exporter-image.sh --registry <host>)")
	}
	return ref
}

// liveAgentEvidenceStaging is appended to the agent command: it stages one
// head-independent PNG evidence artifact under the reserved subtree so the real
// helper emits the evidence channel alongside the repo channel. The PNG magic
// (octal) satisfies the importer's images-only magic check.
const liveAgentEvidenceStaging = `mkdir -p /workspace/.freeside-evidence && ` +
	`printf '\211PNG\015\012\032\012agent-evidence' > /workspace/.freeside-evidence/shot.png && ` +
	`printf '%s' '{"version":"freeside.export.evidence-source/v1","sources":[{"label":"shot","media_type":"image/png","path":".freeside-evidence/shot.png","head_binding":"head_independent","sensitivity_class":"normal","producer_invocation_id":"live-run"}]}' > /workspace/.freeside-evidence/evidence.json`

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
	// The cleanup contract's evidence surface must be real on the reference
	// runtime: per-object volume inspect exists and reports labels and a
	// creation instant (the identity fingerprint cleanup compares).
	credView, err := rt.InspectVolume(ctx, credVolume)
	if err != nil {
		t.Fatalf("inspect credential volume: %v", err)
	}
	if credView.CreationDate == "" {
		t.Error("volume inspect reported no creation instant; cleanup would degrade to label evidence")
	}
	if !credView.LabelsObserved {
		t.Error("volume inspect omitted labels")
	}
	if err := rt.CreateContainer(ctx, ContainerSpec{
		Name:    seedName,
		Image:   liveImage,
		Command: []string{"sh", "-c", "printf " + liveMarker + " > /credentials/token"},
		Mounts:  []Mount{{Type: MountVolume, Source: credVolume, Target: "/credentials"}},
	}); err != nil {
		t.Fatalf("create seed container: %v", err)
	}
	// Containers report the fingerprint through both inspect and the full
	// listing, the two observation paths teardown evidence uses.
	seedRep, err := rt.Inspect(ctx, seedName)
	if err != nil {
		t.Fatalf("inspect seed container: %v", err)
	}
	if seedRep.CreationDate == "" {
		t.Error("container inspect reported no creation instant")
	}
	seedList, err := rt.ListContainers(ctx)
	if err != nil {
		t.Fatalf("list containers after seed create: %v", err)
	}
	seedListed := false
	for _, c := range seedList {
		if c.ID != seedName {
			continue
		}
		seedListed = true
		if c.CreationDate == "" {
			t.Error("container list reported no creation instant for the seed")
		}
		// Cleanup compares fingerprints captured via inspect against listing
		// rows raw (never parsed), so the two endpoints must agree
		// byte-for-byte; a format drift would silently classify this run's
		// own objects as foreign and leak them.
		if c.CreationDate != seedRep.CreationDate {
			t.Errorf("creation instant differs between inspect (%q) and list (%q)", seedRep.CreationDate, c.CreationDate)
		}
	}
	if !seedListed {
		t.Fatal("just-created seed container missing from the container list; the creation-instant probes were skipped")
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
		// The exporter runs the REAL freeside-export helper from the pinned
		// exporter image; the agent stays on the alpine base.
		ExporterImage:     liveExporterImage(t),
		ExporterCommand:   export.HelperCommand(),
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
					"echo durable-workspace > /workspace/nested/state.txt && " +
					liveAgentEvidenceStaging,
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

	// The real helper exports every regular file the agent wrote (the reserved
	// evidence subtree excluded from the repo channel): result.txt plus
	// nested/state.txt, bytewise-sorted.
	repoPaths := map[string]bool{}
	for _, e := range res.Manifest.Entries {
		repoPaths[e.Path] = true
		if strings.HasPrefix(e.Path, ".freeside-evidence") {
			t.Errorf("repo manifest carries a reserved-subtree entry %q", e.Path)
		}
	}
	if !repoPaths["result.txt"] || !repoPaths["nested/state.txt"] {
		t.Fatalf("manifest entries = %+v, want result.txt and nested/state.txt", res.Manifest.Entries)
	}
	resultBlob := readManifestBlob(t, res, "result.txt")
	if string(resultBlob) != "agent-output\n" {
		t.Errorf("exported result.txt blob = %q, want the agent's write", resultBlob)
	}

	// The evidence channel the real helper emitted from the reserved subtree.
	if !res.EvidencePresent || len(res.Evidence.Entries) != 1 || res.Evidence.Entries[0].Label != "shot" {
		t.Fatalf("evidence = present:%v %+v, want the one shot entry", res.EvidencePresent, res.Evidence)
	}
	if scannedFiles == 0 {
		t.Error("scanner hook never saw a file")
	}

	// The #83 ward/gauntlet convergence gate: the real image's real helper
	// output flows through the gauntlet importer, both channels together. The
	// repo change becomes a commit and the evidence becomes a labeled claim.
	imported := importLiveExport(t, res.ExportDir)
	if len(imported.Claims) != 1 || imported.Claims[0].Label != "shot" {
		t.Errorf("imported claims = %+v, want the one shot claim", imported.Claims)
	}
	var sawResult bool
	for _, c := range imported.Changes {
		if c.Path == "result.txt" {
			sawResult = true
		}
	}
	if !sawResult {
		t.Errorf("imported changes = %+v, want result.txt added", imported.Changes)
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
			if v.CreationDate == "" {
				t.Error("volume list reported no creation instant for the credential volume")
			}
			// Same byte-equality requirement as containers: volume inspect
			// and the volume listing must report one raw creation instant.
			if v.CreationDate != credView.CreationDate {
				t.Errorf("creation instant differs between volume inspect (%q) and list (%q)", credView.CreationDate, v.CreationDate)
			}
		}
	}
	if !credSurvives {
		t.Error("caller-owned credential volume was deleted; containment must come from detachment, not deletion")
	}
	if err := rt.DeleteVolume(ctx, credVolume); err != nil {
		t.Errorf("delete credential volume: %v", err)
	}
}

// readManifestBlob reads the exported blob for a repo-manifest entry by path.
func readManifestBlob(t *testing.T, res *HandoffResult, path string) []byte {
	t.Helper()
	for _, e := range res.Manifest.Entries {
		if e.Path == path && e.Digest != nil {
			hexDigest := strings.TrimPrefix(string(*e.Digest), "sha256:")
			b, err := os.ReadFile(filepath.Join(res.ExportDir, "blobs", "sha256", hexDigest)) //nolint:gosec // digest from the verified manifest
			if err != nil {
				t.Fatalf("read blob for %q: %v", path, err)
			}
			return b
		}
	}
	t.Fatalf("no manifest entry for %q", path)
	return nil
}

// importLiveExport feeds a verified handoff directory through the gauntlet
// importer against a fresh empty base, proving the real image's output imports
// (the #83 ward/gauntlet convergence). The empty base makes every exported file
// an addition.
func importLiveExport(t *testing.T, exportDir string) importer.Result {
	t.Helper()
	base := t.TempDir()
	rungitLive(t, base, "init", "-q")
	rungitLive(t, base, "commit", "-q", "--allow-empty", "-m", "base")
	head := strings.TrimSpace(rungitLive(t, base, "rev-parse", "HEAD"))
	clone := filepath.Join(t.TempDir(), "clone")
	rungitLive(t, base, "clone", "-q", "--no-hardlinks", ".", clone)
	res, err := importer.Import(context.Background(), exportDir, clone, importer.Options{
		BaseSHA:    head,
		CommitDate: time.Unix(1700000100, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("importer.Import(live export): %v", err)
	}
	return res
}

// rungitLive runs a git command in dir for the live test's import plumbing. It
// isolates git config and pins a fixture identity (like the importer's own test
// runner), so the temporary repo's commit does not inherit or require host
// user.name/user.email or trip a host commit.gpgsign.
func rungitLive(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := osexec.Command("git", args...) //nolint:gosec // fixed args, test-owned dir
	cmd.Dir = dir
	cmd.Env = append(
		os.Environ(),
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=fixture",
		"GIT_AUTHOR_EMAIL=fixture@test.invalid",
		"GIT_AUTHOR_DATE=1700000000 +0000",
		"GIT_COMMITTER_NAME=fixture",
		"GIT_COMMITTER_EMAIL=fixture@test.invalid",
		"GIT_COMMITTER_DATE=1700000000 +0000",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
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
