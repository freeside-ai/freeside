package ward

// Host-gated live regression for the #591 credential snapshot volume. It proves
// on the reference runtime (Apple container 1.1.0) what the scripted fake cannot
// fully guarantee: that the two admitted files are delivered on one read-only
// named volume, that the networkless observer proves exactly those two files
// with their digests, and that the review container reads them through symlinks
// inside its writable CODEX_HOME while the snapshot bytes stay read-only and no
// sibling host content leaks in.
//
// Opt-in and CI-blind, following live_test.go: it needs macOS, the `container`
// CLI, `container system start`, and the pinned alpine image. It is skipped by
// default like every other FREESIDE_WARD_LIVE_TEST suite.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestLiveCodexReviewLifecycleCrossesReconstructionStartBoundary drives the
// complete production launch topology on Apple container. The strict journal
// is deliberate: before #605's ordering fix its preparing-only resource guard
// rejects the final reconstruction observations before Start.
func TestLiveCodexReviewLifecycleCrossesReconstructionStartBoundary(t *testing.T) {
	if os.Getenv("FREESIDE_WARD_LIVE_TEST") != "1" {
		t.Skip("live codex-review lifecycle test skipped: set FREESIDE_WARD_LIVE_TEST=1 (requires macOS, Apple container 1.1.0, `container system start`, FREESIDE_WARD_EXPORTER_IMAGE, and FREESIDE_WARD_CODEX_AGENT_IMAGE)")
	}
	reviewImage := os.Getenv("FREESIDE_WARD_CODEX_AGENT_IMAGE")
	if reviewImage == "" {
		t.Skip("live codex-review lifecycle test skipped: set FREESIDE_WARD_CODEX_AGENT_IMAGE to the digest-pinned Codex agent image")
	}
	bin, err := osexec.LookPath("container")
	if err != nil {
		t.Fatalf("container CLI not on PATH: %v", err)
	}
	if out, pullErr := osexec.Command(bin, "image", "pull", liveImage).CombinedOutput(); pullErr != nil { //nolint:gosec // fixed args, resolved CLI path
		t.Logf("image pull (continuing; may be cached): %v: %s", pullErr, out)
	}
	exporterImage := liveExporterImage(t)
	requireExporterGit(t, bin, exporterImage)

	ctx := context.Background()
	rt := NewCLIRuntime(bin)
	runID := fmt.Sprintf("livereview-%d", time.Now().Unix())
	names := codexReviewNames(runID)
	workspace := namesFor(runID).Workspace
	t.Cleanup(func() {
		for _, name := range []string{
			names.workspaceObserver, names.shadowInitializer, names.shadowObserver,
			names.snapshotSeeder, names.snapshotObserver, names.reviewContainer,
		} {
			_ = rt.StopContainer(ctx, name)
			_ = rt.DeleteContainer(ctx, name)
		}
		_ = rt.DeleteNetwork(ctx, names.network)
		_ = rt.DeleteVolume(ctx, names.shadowVolume)
		_ = rt.DeleteVolume(ctx, names.snapshotVolume)
		_ = rt.DeleteVolume(ctx, workspace)
	})

	root := t.TempDir()
	checkout := initLiveSeedCheckout(t, root)
	if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("review fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(checkout, ".agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, ".agents", ".keep"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate := commitLiveSeedCheckout(t, checkout)
	journal := &fakeCodexReviewJournal{}
	backendConfig := testConfig()
	backendConfig.ExporterImage = exporterImage
	backendConfig.SeedRoot = root
	backendConfig.PollInterval = 500 * time.Millisecond
	backendConfig.SeedTimeout = 2 * time.Minute
	backend, err := New(rt, backendConfig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.PrepareCodexReviewWorkspace(
		ctx, journal, runID, checkout, candidate, 64,
	); err != nil {
		t.Fatalf("PrepareCodexReviewWorkspace: %v", err)
	}

	reviewConfig, request := testCodexReview(t)
	reviewConfig.ApprovedImage = reviewImage
	reviewConfig.ObserverImage = exporterImage
	reviewConfig.Journal = journal
	reviewConfig.ProxyURL = ""
	reviewConfig.VolumeLifecycleLeaser, err = NewRuntimeCodexReviewVolumeLeaser(rt)
	if err != nil {
		t.Fatal(err)
	}
	launchSpec := CodexReviewLaunchSpec{
		RunID: runID, Image: reviewImage, WorkspaceSourceRunID: runID,
		WorkspaceVolume: workspace, ExpectedHead: candidate.BaseSHA,
		Prompt: request.Prompt, Boundary: request.Boundary,
		AuthMode: request.AuthMode, AuthIdentityID: request.AuthIdentityID,
		AuthSnapshot: request.AuthSnapshot, Instructions: request.Instructions,
		InstructionFile: request.InstructionFile, InstructionBinding: request.InstructionBinding,
	}
	launch, err := backend.CodexReview(ctx, reviewConfig, launchSpec)
	if err != nil {
		t.Fatalf("CodexReview through final reconstruction and Start: %v", err)
	}
	if journal.intent == nil || journal.intent.State != CodexReviewIntentStarted {
		t.Fatalf("intent = %+v, want started handoff", journal.intent)
	}
	if err := launch.Close(); err != nil {
		t.Fatalf("close review proxy: %v", err)
	}

	// #606 tracks Apple container's post-start environment reordering, which
	// currently makes the ordinary AbortCodexReview identity comparison reject
	// this otherwise authenticated fixture. Keep #605's live boundary proof
	// independent of that adjacent bug: use the same durable ownership
	// fingerprints and exclusive volume lease to reap only this test's objects.
	intent := *journal.intent
	owner := Label{Key: ownershipLabelKey, Value: intent.OwnershipToken}
	if err := backend.reapCodexReviewContainer(ctx, names.reviewContainer,
		objectClaim{attempted: true, owned: true, fingerprint: launch.Binding.ReviewContainerFingerprint}, owner,
	); err != nil {
		t.Fatalf("reap live review container: %v", err)
	}
	lease, _, err := reviewConfig.VolumeLifecycleLeaser.RecoverCodexReviewVolumeLease(
		ctx, owner.Value, codexReviewLeaseVolumes(workspace, names.shadowVolume, names.snapshotVolume),
	)
	if err != nil {
		t.Fatalf("recover live review volume lease: %v", err)
	}
	claims := make(map[string]objectClaim, len(intent.Resources))
	for _, resource := range intent.Resources {
		claims[resource.Name] = objectClaim{attempted: true, owned: true, fingerprint: resource.Fingerprint}
	}
	if err := backend.deleteCodexReviewVolume(ctx, names.shadowVolume, claims[names.shadowVolume], owner); err != nil {
		t.Fatalf("delete live shadow volume: %v", err)
	}
	if err := backend.deleteCodexReviewVolume(ctx, names.snapshotVolume, claims[names.snapshotVolume], owner); err != nil {
		t.Fatalf("delete live snapshot volume: %v", err)
	}
	workspaceOwner := Label{Key: ownershipLabelKey, Value: journal.workspaceBinding.OwnershipToken}
	if err := backend.deleteCodexReviewVolume(ctx, workspace,
		objectClaim{attempted: true, owned: true, fingerprint: journal.workspaceBinding.CreationFingerprint},
		workspaceOwner,
	); err != nil {
		t.Fatalf("delete live workspace volume: %v", err)
	}
	if err := backend.teardownCodexReviewNetwork(ctx, names.network, claims[names.network], owner); err != nil {
		t.Fatalf("delete live review network: %v", err)
	}
	if err := lease.ReleaseCodexReviewVolumeLease(ctx); err != nil {
		t.Fatalf("release live review volume lease: %v", err)
	}
	if err := journal.CloseCodexReviewIntent(ctx, runID); err != nil {
		t.Fatalf("close live review intent: %v", err)
	}
	if journal.intent.State != CodexReviewIntentClosed {
		t.Fatalf("intent state after teardown = %q, want closed", journal.intent.State)
	}

	containers, err := rt.ListContainers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, container := range containers {
		for _, owned := range []string{
			names.workspaceObserver, names.shadowInitializer, names.shadowObserver,
			names.snapshotSeeder, names.snapshotObserver, names.reviewContainer,
		} {
			if container.ID == owned {
				t.Errorf("container %q survived live Codex review teardown", owned)
			}
		}
	}
	volumes, err := rt.ListVolumes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, volume := range volumes {
		if volume.Name == workspace || volume.Name == names.shadowVolume || volume.Name == names.snapshotVolume {
			t.Errorf("volume %q survived live Codex review teardown", volume.Name)
		}
	}
	networks, err := rt.ListNetworks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, network := range networks {
		if network.Name == names.network {
			t.Errorf("network %q survived live Codex review teardown", network.Name)
		}
	}
}

func TestLiveCodexReviewSnapshotDeliversExactlyTwoFilesReadOnly(t *testing.T) {
	if os.Getenv("FREESIDE_WARD_LIVE_TEST") != "1" {
		t.Skip("live codex-review snapshot test skipped: set FREESIDE_WARD_LIVE_TEST=1 (requires macOS, Apple container 1.1.0, `container system start`, and the pinned alpine:3.22 image)")
	}
	bin, err := osexec.LookPath("container")
	if err != nil {
		t.Fatalf("container CLI not on PATH: %v", err)
	}
	if out, perr := osexec.Command(bin, "image", "pull", liveImage).CombinedOutput(); perr != nil { //nolint:gosec // fixed args, resolved CLI path
		t.Logf("image pull (continuing; may be cached): %v: %s", perr, out)
	}
	ctx := context.Background()
	rt := NewCLIRuntime(bin)
	runID := fmt.Sprintf("livesnap-%d", time.Now().Unix())

	volume := codexReviewSnapshotVolumeName(runID)
	seeder := codexReviewSnapshotSeederName(runID)
	observer := codexReviewSnapshotObserverName(runID)
	review := codexReviewContainerName(runID)
	t.Cleanup(func() {
		for _, c := range []string{seeder, observer, review} {
			_ = rt.StopContainer(ctx, c)
			_ = rt.DeleteContainer(ctx, c)
		}
		_ = rt.DeleteVolume(ctx, volume)
	})

	label, err := newOwnershipLabel()
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()

	// Fixed, distinct byte contents so the review container's exact-bytes check
	// and the observer's digest are both deterministic.
	authBody := []byte(`{"auth_mode":"api_key","OPENAI_API_KEY":"live-fixture-key","tokens":null}`)
	instructionBody := []byte("# Review instructions\nlive fixture\n")
	authSum := sha256.Sum256(authBody)
	instrSum := sha256.Sum256(instructionBody)

	stage := t.TempDir()
	if err := os.WriteFile(filepath.Join(stage, codexReviewSnapshotAuthName), authBody, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, codexReviewSnapshotInstrName), instructionBody, 0o400); err != nil {
		t.Fatal(err)
	}
	readyDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(readyDir, seedReadyFile), []byte("ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := rt.CreateVolume(ctx, volume, codexReviewSnapshotVolumeSizeMB, append(runLabels(runID), label)); err != nil {
		t.Fatalf("create snapshot volume: %v", err)
	}

	// Seed: a networkless running seeder receives the two files by copy and moves
	// them onto the read-write-mounted volume, then is deleted.
	if err := rt.CreateContainer(ctx, ContainerSpec{
		Name:            seeder,
		Image:           liveImage,
		Command:         []string{"sh", "-c", codexReviewSnapshotSeederScript(cfg, codexReviewSnapshotSeedTarget)},
		Mounts:          []Mount{{Type: MountVolume, Source: volume, Target: codexReviewSnapshotSeedTarget}},
		Labels:          append(runLabels(runID), label),
		NetworkDisabled: true,
	}); err != nil {
		t.Fatalf("create snapshot seeder: %v", err)
	}
	if err := rt.StartContainer(ctx, seeder); err != nil {
		t.Fatalf("start snapshot seeder: %v", err)
	}
	if err := rt.CopyIntoContainer(ctx, seeder, stage, cfg.SeedStageDir); err != nil {
		t.Fatalf("copy snapshot stage: %v", err)
	}
	if err := rt.CopyIntoContainer(ctx, seeder, readyDir, cfg.SeedReadyDir); err != nil {
		t.Fatalf("copy snapshot sentinel: %v", err)
	}
	waitLiveStopped(t, rt, seeder)
	if err := rt.DeleteContainer(ctx, seeder); err != nil {
		t.Fatalf("delete snapshot seeder: %v", err)
	}

	// Observe: a separate read-only VM proves exactly the two files and their
	// digests. This is the trust boundary; the writer never vouches for itself.
	if err := rt.CreateContainer(ctx, ContainerSpec{
		Name:  observer,
		Image: liveImage,
		Command: []string{"sh", "-c", codexReviewSnapshotObserverScript(
			label.Value, codexReviewSnapshotObserverTarget, codexReviewSnapshotProofPath,
		)},
		Mounts:          []Mount{{Type: MountVolume, Source: volume, Target: codexReviewSnapshotObserverTarget, ReadOnly: true}},
		Labels:          append(runLabels(runID), label),
		NetworkDisabled: true,
	}); err != nil {
		t.Fatalf("create snapshot observer: %v", err)
	}
	if err := rt.StartContainer(ctx, observer); err != nil {
		t.Fatalf("start snapshot observer: %v", err)
	}
	waitLiveStopped(t, rt, observer)
	proof := liveReadContainerFile(t, rt, observer, codexReviewSnapshotProofPath)
	wantProof := fmt.Sprintf("nonce=%s\nvalid=valid\nauth=sha256:%x\ninstr=sha256:%x\n", label.Value, authSum, instrSum)
	if string(proof) != wantProof {
		t.Fatalf("snapshot observer proof = %q, want %q", proof, wantProof)
	}
	if err := rt.DeleteContainer(ctx, observer); err != nil {
		t.Fatalf("delete snapshot observer: %v", err)
	}

	// Review: mount the snapshot read-only, run the production symlink preamble,
	// then verify the container reads exactly the two files through the links,
	// the snapshot mount is read-only, and CODEX_HOME carries no sibling content
	// beyond the two links. Copy the dereferenced links into the container rootfs
	// so the host can compare their complete bytes without shell normalization.
	const (
		authProofPath        = "/live-review-auth.bin"
		instructionProofPath = "/live-review-instruction.bin"
	)
	verify := "set +e; mkdir -p " + shellQuote(CodexHomeTarget) + "; " +
		"ln -s " + shellQuote(codexReviewSnapshotAuthSource) + " " + shellQuote(CodexAuthFileTarget) + "; " +
		"ln -s " + shellQuote(codexReviewSnapshotInstrSource) + " " + shellQuote(CodexInstructionTarget) + "; " +
		"cp " + shellQuote(CodexAuthFileTarget) + " " + shellQuote(authProofPath) + "; " +
		"cp " + shellQuote(CodexInstructionTarget) + " " + shellQuote(instructionProofPath) + "; " +
		"ok=1; " +
		"[ -L " + shellQuote(CodexAuthFileTarget) + " ] || ok=0; " +
		"[ -L " + shellQuote(CodexInstructionTarget) + " ] || ok=0; " +
		"entries=\"$(cd " + shellQuote(codexReviewSnapshotTarget) + " && find . ! -name . -print | sort | tr '\\n' ',')\"; " +
		"[ \"$entries\" = './AGENTS.md,./auth.json,' ] || ok=0; " +
		"if echo probe > " + shellQuote(codexReviewSnapshotTarget+"/probe") + " 2>/dev/null; then ok=0; fi; " +
		"home=\"$(cd " + shellQuote(CodexHomeTarget) + " && find . ! -name . -print | sort | tr '\\n' ',')\"; " +
		"[ \"$home\" = './AGENTS.md,./auth.json,' ] || ok=0; " +
		"printf 'ok=%s\\n' \"$ok\" > /live-review-proof.txt"
	if err := rt.CreateContainer(ctx, ContainerSpec{
		Name:    review,
		Image:   liveImage,
		Command: []string{"sh", "-c", verify},
		Env:     []string{"CODEX_HOME=" + CodexHomeTarget},
		Mounts: []Mount{
			{Type: MountVolume, Source: volume, Target: codexReviewSnapshotTarget, ReadOnly: true},
		},
		Labels:          append(runLabels(runID), label),
		NetworkDisabled: true,
	}); err != nil {
		t.Fatalf("create review container: %v", err)
	}
	if err := rt.StartContainer(ctx, review); err != nil {
		t.Fatalf("start review container: %v", err)
	}
	waitLiveStopped(t, rt, review)
	reviewProof := liveReadContainerFile(t, rt, review, "/live-review-proof.txt")
	if string(reviewProof) != "ok=1\n" {
		t.Fatalf("review container snapshot verification = %q, want ok=1", reviewProof)
	}
	if err := compareCodexReviewSnapshotBytes("auth.json", liveReadContainerFile(t, rt, review, authProofPath), authBody); err != nil {
		t.Fatal(err)
	}
	if err := compareCodexReviewSnapshotBytes("AGENTS.md", liveReadContainerFile(t, rt, review, instructionProofPath), instructionBody); err != nil {
		t.Fatal(err)
	}
}

func TestCompareCodexReviewSnapshotBytesRejectsTrailingDifferences(t *testing.T) {
	authBody := []byte(`{"auth_mode":"api_key","OPENAI_API_KEY":"live-fixture-key","tokens":null}`)
	instructionBody := []byte("# Review instructions\nlive fixture\n")

	tests := []struct {
		name string
		want []byte
		got  []byte
	}{
		{name: "auth trailing byte added", want: authBody, got: append(append([]byte(nil), authBody...), '\n')},
		{name: "auth trailing byte removed", want: authBody, got: authBody[:len(authBody)-1]},
		{name: "instruction trailing byte added", want: instructionBody, got: append(append([]byte(nil), instructionBody...), 0)},
		{name: "instruction trailing newline removed", want: instructionBody, got: instructionBody[:len(instructionBody)-1]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := compareCodexReviewSnapshotBytes(tt.name, tt.got, tt.want); err == nil {
				t.Fatal("trailing difference accepted")
			}
		})
	}
}

func compareCodexReviewSnapshotBytes(name string, got, want []byte) error {
	if !bytes.Equal(got, want) {
		return fmt.Errorf("%s bytes differ: got %q, want %q", name, got, want)
	}
	return nil
}

// TestLiveCodexReviewSnapshotPreambleFailsClosedWhenImageShadowsCredential is
// the #591 fail-closed regression for the launch prologue. A derived or updated
// review image that already ships a file at a fixed CODEX_HOME path must not let
// the review proceed: the `set -e` prologue's `ln -s` hits "File exists" and
// aborts before `codex exec` runs, so the schema/output artifacts never appear
// and the image-provided credential is left untouched (never read by codex).
// Under the prior `set +e` prologue the ln failure was swallowed, so the
// schema.json probe is the discriminator between fail-open and fail-closed.
//
// It runs the exact production launch command from codexReviewCommand, so it
// cannot drift from the code under test. Like the sibling live test it is opt-in
// and CI-blind (needs macOS, the `container` CLI, and the pinned alpine image).
func TestLiveCodexReviewSnapshotPreambleFailsClosedWhenImageShadowsCredential(t *testing.T) {
	if os.Getenv("FREESIDE_WARD_LIVE_TEST") != "1" {
		t.Skip("live codex-review fail-closed test skipped: set FREESIDE_WARD_LIVE_TEST=1 (requires macOS, Apple container 1.1.0, `container system start`, and the pinned alpine:3.22 image)")
	}
	bin, err := osexec.LookPath("container")
	if err != nil {
		t.Fatalf("container CLI not on PATH: %v", err)
	}
	if out, perr := osexec.Command(bin, "image", "pull", liveImage).CombinedOutput(); perr != nil { //nolint:gosec // fixed args, resolved CLI path
		t.Logf("image pull (continuing; may be cached): %v: %s", perr, out)
	}
	ctx := context.Background()
	rt := NewCLIRuntime(bin)
	runID := fmt.Sprintf("livesnapfc-%d", time.Now().Unix())
	review := codexReviewContainerName(runID)
	t.Cleanup(func() {
		_ = rt.StopContainer(ctx, review)
		_ = rt.DeleteContainer(ctx, review)
	})

	label, err := newOwnershipLabel()
	if err != nil {
		t.Fatal(err)
	}

	// Simulate the derived image: pre-create a regular file at the fixed auth
	// path, then hand off to the unmodified production command. No snapshot volume
	// is mounted because the abort is triggered by the pre-existing target, not by
	// the link source; this isolates the prologue's fail-closed property.
	prod := codexReviewCommand("/workspace/project", "gpt-5.2-codex", "high", "unused")
	command := "mkdir -p " + shellQuote(CodexHomeTarget) + "; " +
		"printf 'image-shadow' > " + shellQuote(CodexAuthFileTarget) + "; " + prod[2]

	if err := rt.CreateContainer(ctx, ContainerSpec{
		Name:            review,
		Image:           liveImage,
		Command:         []string{"sh", "-c", command},
		Env:             []string{"CODEX_HOME=" + CodexHomeTarget},
		Labels:          append(runLabels(runID), label),
		NetworkDisabled: true,
	}); err != nil {
		t.Fatalf("create review container: %v", err)
	}
	if err := rt.StartContainer(ctx, review); err != nil {
		t.Fatalf("start review container: %v", err)
	}
	waitLiveStopped(t, rt, review)

	if _, found := liveContainerFileOptional(t, rt, review, codexReviewSchemaPath); found {
		t.Fatal("fail-open: schema.json was written, so the prologue continued past the failed credential link toward codex exec")
	}
	shadow, found := liveContainerFileOptional(t, rt, review, CodexAuthFileTarget)
	if !found {
		t.Fatal("image-provided credential file vanished; expected the aborted prologue to leave it intact")
	}
	if string(shadow) != "image-shadow" {
		t.Fatalf("image credential = %q, want it untouched (%q)", shadow, "image-shadow")
	}
}

// liveReadContainerFile exports a stopped container's rootfs and returns the
// bytes of one regular file, under the same byte cap the proof paths use.
func liveReadContainerFile(t *testing.T, rt Runtime, id, path string) []byte {
	t.Helper()
	data, found := liveContainerFileOptional(t, rt, id, path)
	if !found {
		t.Fatalf("%s produced no %s", id, path)
	}
	return data
}

// liveContainerFileOptional is liveReadContainerFile without the must-exist
// assertion: it reports whether the regular file was present, so a test can
// assert an artifact's absence (e.g. a fail-closed abort leaving no output).
func liveContainerFileOptional(t *testing.T, rt Runtime, id, path string) ([]byte, bool) {
	t.Helper()
	var buf bytes.Buffer
	if err := rt.ExportRootFS(context.Background(), id, &buf, maxBaseProofBytes<<4); err != nil {
		t.Fatalf("export %s rootfs: %v", id, err)
	}
	data, found, err := extractArchiveRegularFile(bytes.NewReader(buf.Bytes()), path, maxBaseProofBytes)
	if err != nil {
		t.Fatalf("read %s from %s rootfs: %v", path, id, err)
	}
	return data, found
}
