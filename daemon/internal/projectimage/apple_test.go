package projectimage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

type recordingRunner struct {
	specs []commandSpec
	run   func(commandSpec) (commandOutput, error)
}

func (r *recordingRunner) Run(_ context.Context, spec commandSpec) (commandOutput, error) {
	spec.Args = append([]string{}, spec.Args...)
	r.specs = append(r.specs, spec)
	if r.run == nil {
		return commandOutput{}, nil
	}
	return r.run(spec)
}

type managedRunner struct {
	specs      []commandSpec
	containers map[string]string
	deletes    int
	nextID     int
	next       func(context.Context, commandSpec) (commandOutput, error)
}

func (r *managedRunner) Run(ctx context.Context, spec commandSpec) (commandOutput, error) {
	spec.Args = append([]string{}, spec.Args...)
	r.specs = append(r.specs, spec)
	if r.containers == nil {
		r.containers = map[string]string{}
	}
	if len(spec.Args) == 2 && spec.Args[0] == "inspect" {
		token, exists := r.containers[spec.Args[1]]
		if !exists {
			return commandOutput{bytes: []byte("Error: container not found: " + spec.Args[1])},
				errors.New("not found")
		}
		return containerInspectOutput(tidyContainer(spec.Args[1], token)), nil
	}
	if len(spec.Args) >= 1 && (spec.Args[0] == "run" || spec.Args[0] == "create") {
		cidPath, token := managedIdentity(spec.Args)
		if cidPath != "" {
			r.nextID++
			id := fmt.Sprintf("runtime-id-%d", r.nextID)
			if err := os.WriteFile(cidPath, []byte(id+"\n"), 0o600); err != nil {
				return commandOutput{}, err
			}
			r.containers[id] = token
		}
	}
	if len(spec.Args) == 3 && spec.Args[0] == "delete" {
		delete(r.containers, spec.Args[2])
		r.deletes++
		return commandOutput{}, nil
	}
	if r.next == nil {
		return commandOutput{}, nil
	}
	return r.next(ctx, spec)
}

func managedIdentity(args []string) (string, string) {
	var cidPath, token string
	for index := 0; index+1 < len(args); index++ {
		switch args[index] {
		case "--cidfile":
			cidPath = args[index+1]
		case "--label":
			if strings.HasPrefix(args[index+1], ownershipLabel+"=") {
				token = strings.TrimPrefix(args[index+1], ownershipLabel+"=")
			}
		}
	}
	return cidPath, token
}

func tidyContainer(name, token string) containerInspect {
	var report containerInspect
	report.ID = name
	report.Configuration.ID = name
	report.Configuration.Labels = map[string]string{ownershipLabel: token}
	return report
}

func containerInspectOutput(report containerInspect) commandOutput {
	body, _ := json.Marshal([]containerInspect{report})
	return commandOutput{bytes: body}
}

func registryContainer(id, token string, port int) containerInspect {
	report := tidyContainer(id, token)
	report.Configuration.Labels[registryPortLabel] = fmt.Sprint(port)
	report.Configuration.Image.Descriptor.Digest = registryImageDigest
	report.Status.State = "running"
	return report
}

func inspectOutput(digest string) commandOutput {
	return inspectOutputFor("ref", digest)
}

func inspectOutputFor(ref, digest string) commandOutput {
	return commandOutput{bytes: []byte(`[{"configuration":{"name":"` + ref + `","descriptor":{"digest":"` + digest + `"}}}]`)}
}

func taggedRef(specs []commandSpec, localRef string) string {
	for _, spec := range specs {
		if len(spec.Args) == 4 &&
			slices.Equal(spec.Args[:3], []string{"image", "tag", localRef}) {
			return spec.Args[3]
		}
	}
	return ""
}

func TestAppleRunPreservesRecipeArgvWithRuntimeWorkingDirectory(t *testing.T) {
	runner := &managedRunner{next: func(_ context.Context, spec commandSpec) (commandOutput, error) {
		if len(spec.Args) == 0 || spec.Args[0] != "run" {
			return commandOutput{}, nil
		}
		return commandOutput{exited: true}, nil
	}}
	backend := appleBackend{containerPath: "/usr/bin/container", runner: runner}
	argv := []string{"tool", "argument with spaces", "$(opaque)", "*.literal"}
	if _, err := backend.Run(t.Context(), runSpec{
		ImageRef:  "example.test/image@sha256:" + strings.Repeat("a", 64),
		Workspace: "/host/workspace", Argv: argv,
	}); err != nil {
		t.Fatal(err)
	}
	if len(runner.specs) != 3 {
		t.Fatalf("runs = %d", len(runner.specs))
	}
	args := runner.specs[0].Args
	wantPrefix := []string{
		"run", "--rm", "--cidfile", args[3], "--label", args[5],
		"--network", "none", "--volume", "/host/workspace:/workspace",
		"--workdir", "/workspace",
		"--", "example.test/image@sha256:" + strings.Repeat("a", 64),
	}
	if len(args) != len(wantPrefix)+len(argv) ||
		!slices.Equal(args[:len(wantPrefix)], wantPrefix) ||
		!slices.Equal(args[len(wantPrefix):], argv) {
		t.Fatalf("container argv = %q, want fixed wrapper plus exact %q", args, argv)
	}
}

func TestAppleBuildPassesBuildOnlyProxyAsPredefinedArgs(t *testing.T) {
	runner := &recordingRunner{}
	backend := appleBackend{containerPath: "container", runner: runner}
	if err := backend.Build(t.Context(), buildSpec{
		ContextDir: "/tmp/context", LocalRef: "project:local",
		BaseRef: "base:local", BaseDigest: testBaseDigest,
		Repository: "freeasinbird/gh-imgup", RepositoryID: 1278475858,
		CommitSHA: testCommit, RecipeDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		BuildProxy: "http://192.168.64.1:53536",
	}); err != nil {
		t.Fatal(err)
	}
	if len(runner.specs) != 1 ||
		!slices.Contains(runner.specs[0].Args, "HTTP_PROXY=http://192.168.64.1:53536") ||
		!slices.Contains(runner.specs[0].Args, "HTTPS_PROXY=http://192.168.64.1:53536") {
		t.Fatalf("build args = %q", runner.specs)
	}
}

func TestAppleCheckAllowlistUsesConfiguredContainerPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	runner := &managedRunner{}
	var inspectedID string
	backend := appleBackend{
		containerPath: "/opt/apple/bin/container",
		runner:        runner,
		inspectAllowlist: func(_ context.Context, id string) (ward.InspectReport, error) {
			inspectedID = id
			return compliantImageAllowlistReport(id), nil
		},
	}
	if err := backend.CheckAllowlist(t.Context(), "project:local"); err != nil {
		t.Fatal(err)
	}
	if inspectedID != "runtime-id-1" || runner.deletes != 1 {
		t.Fatalf("allowlist inspection/deletes = %q/%d", inspectedID, runner.deletes)
	}
	for _, spec := range runner.specs {
		if spec.Path != "/opt/apple/bin/container" {
			t.Fatalf("allowlist invoked ambient helper %q", spec.Path)
		}
	}
	if len(runner.specs[0].Args) != 10 || !slices.Equal(runner.specs[0].Args, []string{
		"create", "--cidfile", runner.specs[0].Args[2],
		"--label", runner.specs[0].Args[4],
		"--", "project:local", "sh", "-c", "true",
	}) {
		t.Fatalf("allowlist create args = %q", runner.specs[0].Args)
	}
}

// listedContainer models Apple container's list output, which carries the
// identity but no labels.
func listedContainer(id string) containerInspect {
	var report containerInspect
	report.ID = id
	report.Configuration.ID = id
	return report
}

func TestAppleCheckAllowlistRecoversLostProbeIdentity(t *testing.T) {
	var token string
	var deleted []string
	runner := &recordingRunner{}
	runner.run = func(spec commandSpec) (commandOutput, error) {
		switch spec.Args[0] {
		case "create":
			// The runtime created the probe but its identity file was lost.
			_, token = managedIdentity(spec.Args)
			return commandOutput{}, nil
		case "list":
			// Apple container's list output omits labels; recovery must
			// resolve ownership through per-candidate inspection.
			body, err := json.Marshal([]containerInspect{
				listedContainer("orphan-1"), listedContainer("bystander"),
			})
			return commandOutput{bytes: body}, err
		case "inspect":
			if spec.Args[1] == "orphan-1" {
				return containerInspectOutput(tidyContainer("orphan-1", token)), nil
			}
			return containerInspectOutput(tidyContainer(spec.Args[1], "other-token")), nil
		case "delete":
			deleted = append(deleted, spec.Args[2])
			return commandOutput{}, nil
		}
		return commandOutput{}, nil
	}
	err := appleBackend{containerPath: "container", runner: runner}.
		CheckAllowlist(t.Context(), "project:local")
	if err == nil {
		t.Fatal("CheckAllowlist passed without a probe identity")
	}
	if !slices.Equal(deleted, []string{"orphan-1"}) {
		t.Fatalf("ownership recovery deleted %q, want only the token-owned probe", deleted)
	}
}

func TestAppleRunRecoversOrphanOnInterruptedCreate(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var token string
	var deleted []string
	runner := &recordingRunner{}
	runner.run = func(spec commandSpec) (commandOutput, error) {
		switch spec.Args[0] {
		case "run":
			// Cancellation interrupted the create before the identity file
			// existed; the instance may still have been registered.
			_, token = managedIdentity(spec.Args)
			cancel()
			return commandOutput{}, ctx.Err()
		case "list":
			body, err := json.Marshal([]containerInspect{listedContainer("orphan-2")})
			return commandOutput{bytes: body}, err
		case "inspect":
			return containerInspectOutput(tidyContainer(spec.Args[1], token)), nil
		case "delete":
			deleted = append(deleted, spec.Args[2])
			return commandOutput{}, nil
		}
		return commandOutput{}, nil
	}
	result, err := appleBackend{containerPath: "container", runner: runner}.
		Run(ctx, runSpec{ImageRef: "project:local", Argv: []string{"slow"}})
	if err != nil || result.ExitCode != -1 {
		t.Fatalf("Run = %+v, %v; want signal-style step result", result, err)
	}
	if !slices.Equal(deleted, []string{"orphan-2"}) {
		t.Fatalf("ownership recovery deleted %q, want the interrupted instance", deleted)
	}
}

func compliantImageAllowlistReport(id string) ward.InspectReport {
	return ward.InspectReport{
		ID: id, State: ward.StateStopped, AllowlistFieldsObserved: true,
		ImageReference: "project:local", Env: []string{allowlistPathEnv},
		WorkingDirectory: "/", Command: []string{"sh", "-c", "true"},
		NetworksObserved: true,
	}
}

func TestImageAllowlistRejectsRuntimeDrift(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ward.InspectReport)
	}{
		{"wrong identity", func(r *ward.InspectReport) { r.ID = "other" }},
		{"required field omitted", func(r *ward.InspectReport) { r.AllowlistFieldsObserved = false }},
		{"running", func(r *ward.InspectReport) { r.State = ward.StateRunning }},
		{"wrong image", func(r *ward.InspectReport) { r.ImageReference = "other:local" }},
		{"extra environment", func(r *ward.InspectReport) { r.Env = append(r.Env, "TOKEN=inert") }},
		{"working directory", func(r *ward.InspectReport) { r.WorkingDirectory = "/workspace" }},
		{"command", func(r *ward.InspectReport) { r.Command = append(r.Command, "drift") }},
		{"mount", func(r *ward.InspectReport) { r.Mounts = []ward.Mount{{Type: ward.MountVolume}} }},
		{"ssh", func(r *ward.InspectReport) { r.SSH = true }},
		{"published port", func(r *ward.InspectReport) { r.PublishedPorts = []string{"8080"} }},
		{"network omitted", func(r *ward.InspectReport) { r.NetworksObserved = false }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := compliantImageAllowlistReport("runtime-id")
			tc.mutate(&report)
			if err := validateImageAllowlist(report, "runtime-id", "project:local"); err == nil {
				t.Fatal("validateImageAllowlist accepted runtime drift")
			}
		})
	}
}

func TestAppleCheckAllowlistCleansProbeAfterInspectionFailure(t *testing.T) {
	runner := &managedRunner{}
	backend := appleBackend{
		containerPath: "container", runner: runner,
		inspectAllowlist: func(context.Context, string) (ward.InspectReport, error) {
			return ward.InspectReport{}, errors.New("runtime inspection failed")
		},
	}
	if err := backend.CheckAllowlist(t.Context(), "project:local"); err == nil {
		t.Fatal("CheckAllowlist accepted a failed inspection")
	}
	if runner.deletes != 1 || len(runner.containers) != 0 {
		t.Fatal("failed allowlist inspection left its owned probe container")
	}
}

func TestAppleCheckAllowlistFailsWhenIdentityFileCleanupFails(t *testing.T) {
	runner := &managedRunner{}
	backend := appleBackend{
		containerPath: "container", runner: runner,
		inspectAllowlist: func(_ context.Context, id string) (ward.InspectReport, error) {
			cidPath := runner.specs[0].Args[2]
			if err := os.Remove(cidPath); err != nil {
				return ward.InspectReport{}, err
			}
			if err := os.Mkdir(cidPath, 0o700); err != nil {
				return ward.InspectReport{}, err
			}
			t.Cleanup(func() { _ = os.RemoveAll(cidPath) })
			if err := os.WriteFile(filepath.Join(cidPath, "blocked"), []byte("x"), 0o600); err != nil {
				return ward.InspectReport{}, err
			}
			return compliantImageAllowlistReport(id), nil
		},
	}
	err := backend.CheckAllowlist(t.Context(), "project:local")
	if err == nil || !strings.Contains(err.Error(), "remove agent-image allowlist identity file") {
		t.Fatalf("CheckAllowlist = %v, want identity-file cleanup failure", err)
	}
	if runner.deletes != 1 || len(runner.containers) != 0 {
		t.Fatal("identity-file cleanup failure prevented owned probe cleanup")
	}
}

func TestAppleCheckAllowlistRefusesCleanupOnAmbiguousOwnershipJSON(t *testing.T) {
	deleted := false
	var runner *recordingRunner
	runner = &recordingRunner{run: func(spec commandSpec) (commandOutput, error) {
		switch {
		case len(spec.Args) > 0 && spec.Args[0] == "create":
			cidPath, _ := managedIdentity(spec.Args)
			if err := os.WriteFile(cidPath, []byte("probe-id\n"), 0o600); err != nil {
				return commandOutput{}, err
			}
			return commandOutput{}, nil
		case slices.Equal(spec.Args, []string{"inspect", "probe-id"}):
			_, token := managedIdentity(runner.specs[0].Args)
			body := fmt.Sprintf(
				`[{"id":"probe-id","configuration":{"id":"probe-id","labels":{"%s":"%s"},"labels":{"%s":"%s"}}}]`,
				ownershipLabel, token, ownershipLabel, token,
			)
			return commandOutput{bytes: []byte(body)}, nil
		case slices.Equal(spec.Args, []string{"delete", "--force", "probe-id"}):
			deleted = true
			return commandOutput{}, nil
		default:
			return commandOutput{}, fmt.Errorf("unexpected command: %q", spec.Args)
		}
	}}
	backend := appleBackend{
		containerPath: "container", runner: runner,
		inspectAllowlist: func(_ context.Context, id string) (ward.InspectReport, error) {
			return compliantImageAllowlistReport(id), nil
		},
	}
	if err := backend.CheckAllowlist(t.Context(), "project:local"); err == nil {
		t.Fatal("CheckAllowlist accepted ambiguous cleanup ownership evidence")
	}
	if deleted {
		t.Fatal("CheckAllowlist deleted a probe using ambiguous ownership evidence")
	}
}

func TestAppleImageDigestRequiresFullLowercaseDigest(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output commandOutput
		want   string
		ok     bool
	}{
		{"valid", inspectOutput(testImageDigest), testImageDigest, true},
		{"canonical local name", inspectOutputFor("docker.io/library/ref", testImageDigest), testImageDigest, true},
		{"short", inspectOutput("sha256:abc"), "", false},
		{"uppercase", inspectOutput("sha256:" + strings.Repeat("A", 64)), "", false},
		{"wrong object", inspectOutputFor("other", testImageDigest), "", false},
		{"multiple objects", commandOutput{bytes: append(
			inspectOutput(testImageDigest).bytes, inspectOutput(testImageDigest).bytes...)}, "", false},
		{"unrelated text", commandOutput{bytes: []byte(
			`[{"message":"sha256:` + strings.Repeat("a", 64) + `"}]`)}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &recordingRunner{run: func(commandSpec) (commandOutput, error) {
				return tc.output, nil
			}}
			got, err := (appleBackend{containerPath: "container", runner: runner}).ImageDigest(t.Context(), "ref")
			if (err == nil) != tc.ok || got != tc.want {
				t.Fatalf("ImageDigest = %q, %v; want %q, ok=%v", got, err, tc.want, tc.ok)
			}
		})
	}
}

func TestAppleCheckProvenanceBindsLabelsAndEmbeddedFiles(t *testing.T) {
	recipeDigest := verify.RecipeDigest([]byte(testRecipe))
	spec := provenanceSpec{
		ImageDigest:  testImageDigest,
		BaseBuildRef: "base:local", BaseDigest: testBaseDigest,
		Repository:   "freeasinbird/gh-imgup",
		RepositoryID: 1278475858, CommitSHA: testCommit, RecipeDigest: recipeDigest,
	}
	ref := "project:local"
	projectLabels := map[string]string{
		"org.opencontainers.image.title":    "freeside-project-image",
		"ai.freeside.base.digest":           spec.BaseDigest,
		"ai.freeside.project.repository":    spec.Repository,
		"ai.freeside.project.repository-id": "1278475858",
		"ai.freeside.project.commit":        spec.CommitSHA,
		"ai.freeside.project.recipe-digest": string(spec.RecipeDigest),
	}
	runner := &recordingRunner{run: func(call commandSpec) (commandOutput, error) {
		if len(call.Args) >= 2 && call.Args[0] == "image" && call.Args[1] == "inspect" {
			if call.Args[2] == spec.BaseBuildRef {
				return inspectOutputFor(spec.BaseBuildRef, testBaseDigest), nil
			}
			return inspectOutputFor(ref, testImageDigest), nil
		}
		return commandOutput{}, nil
	}}
	var baseConfig ociImageConfig
	baseConfig.RootFS.DiffIDs = []string{"sha256:base-layer"}
	var projectConfig ociImageConfig
	projectConfig.Config.Labels = projectLabels
	projectConfig.RootFS.DiffIDs = []string{"sha256:base-layer", "sha256:project-layer"}
	backend := appleBackend{
		containerPath: "container", runner: runner,
		readEvidence: func(_ context.Context, imageRef, _ string, requireFiles bool) (ociEvidence, error) {
			if imageRef == spec.BaseBuildRef {
				return ociEvidence{Config: baseConfig}, nil
			}
			if !requireFiles {
				return ociEvidence{}, errors.New("project evidence omitted rootfs proof")
			}
			return ociEvidence{Config: projectConfig, Files: map[string][]byte{
				"/usr/local/share/freeside/project-recipe.json": []byte(testRecipe),
				PreparationPath: []byte(prepareScript),
			}}, nil
		},
	}
	if err := backend.CheckProvenance(t.Context(), ref, spec); err != nil {
		t.Fatal(err)
	}
	projectLabels["ai.freeside.project.commit"] = strings.Repeat("0", 40)
	if err := backend.CheckProvenance(t.Context(), ref, spec); err == nil {
		t.Fatal("CheckProvenance accepted a mismatched commit label")
	}
	projectLabels["ai.freeside.project.commit"] = spec.CommitSHA
	projectConfig.Config.User = "node"
	if err := backend.CheckProvenance(t.Context(), ref, spec); err == nil {
		t.Fatal("CheckProvenance accepted a project config with a non-root user")
	}
	projectConfig.Config.User = "root"
	baseConfig.Config.User = "1000"
	if err := backend.CheckProvenance(t.Context(), ref, spec); err == nil {
		t.Fatal("CheckProvenance accepted a base config with a non-root user")
	}
	baseConfig.Config.User = "0:0"
	if err := backend.CheckProvenance(t.Context(), ref, spec); err != nil {
		t.Fatalf("CheckProvenance rejected explicit root users: %v", err)
	}
	drifted := spec
	drifted.ImageDigest = "sha256:" + strings.Repeat("c", 64)
	if err := backend.CheckProvenance(t.Context(), ref, drifted); err == nil {
		t.Fatal("CheckProvenance proved a tag that no longer resolves to the built digest")
	}
	unbound := spec
	unbound.ImageDigest = ""
	if err := backend.CheckProvenance(t.Context(), ref, unbound); err == nil {
		t.Fatal("CheckProvenance ran without a built-image digest to bind")
	}
}

func TestRootImageUser(t *testing.T) {
	valid := []string{"", "root", "0", "root:root", "root:0", "0:root", "0:0"}
	for _, user := range valid {
		if !rootImageUser(user) {
			t.Errorf("rootImageUser(%q) = false, want true", user)
		}
	}
	invalid := []string{
		"node", "1000", "root ", " root", "Root", "ROOT", "00", "0x0",
		"root\n", "root:", ":root", ":", "root:1000", "1000:root",
		"root:wheel", "root:root:root", "root:Root", "0:00", "root\x00",
	}
	for _, user := range invalid {
		if rootImageUser(user) {
			t.Errorf("rootImageUser(%q) = true, want false", user)
		}
	}
}

func TestReadOCIConfigBindsManifestDescriptor(t *testing.T) {
	configBody := []byte(`{"config":{"Labels":{"proof":"bound"}},"rootfs":{"diff_ids":["sha256:` +
		strings.Repeat("a", 64) + `"]}}`)
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(configBody))
	descriptor := ociDescriptor{
		MediaType: ociConfigMediaType,
		Digest:    digest,
		Size:      int64(len(configBody)),
	}
	writeArchive := func(t *testing.T, body []byte) string {
		t.Helper()
		path := t.TempDir() + "/image.tar"
		file, err := os.Create(path) //nolint:gosec // fixed filename under test-owned scratch
		if err != nil {
			t.Fatal(err)
		}
		writer := tar.NewWriter(file)
		header := &tar.Header{
			Name: "blobs/sha256/" + strings.TrimPrefix(digest, "sha256:"),
			Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(body); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		return path
	}
	manifest := ociManifest{
		Config: descriptor,
		Layers: []ociDescriptor{{Digest: "sha256:" + strings.Repeat("b", 64)}},
	}
	config, err := readOCIConfig(writeArchive(t, configBody), manifest)
	if err != nil || config.Config.Labels["proof"] != "bound" ||
		len(config.RootFS.DiffIDs) != 1 {
		t.Fatalf("readOCIConfig = %+v, %v", config, err)
	}
	if _, err := readOCIConfig(writeArchive(t, append(configBody, '\n')), manifest); err == nil {
		t.Fatal("readOCIConfig accepted bytes that did not match the manifest descriptor")
	}
}

func TestVerifyOCIDiffIDsHashesUncompressedLayerBytes(t *testing.T) {
	body := []byte("uncompressed layer bytes")
	diffID := fmt.Sprintf("sha256:%x", sha256.Sum256(body))
	plainPath := t.TempDir() + "/plain.layer"
	if err := os.WriteFile(plainPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	gzipPath := t.TempDir() + "/gzip.layer"
	if err := os.WriteFile(gzipPath, compressed.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		path      string
		mediaType string
	}{
		{"plain", plainPath, ociLayerMediaType},
		{"gzip", gzipPath, ociGzipMediaType},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := verifyOCIDiffIDs(
				[]string{tc.path},
				[]ociDescriptor{{MediaType: tc.mediaType}},
				[]string{diffID},
			); err != nil {
				t.Fatal(err)
			}
		})
	}
	if err := verifyOCIDiffIDs(
		[]string{plainPath},
		[]ociDescriptor{{MediaType: ociLayerMediaType}},
		[]string{"sha256:" + strings.Repeat("0", 64)},
	); err == nil {
		t.Fatal("verifyOCIDiffIDs accepted a layer that did not match the bound config")
	}
}

func TestReadProvenanceArchiveRequiresUniqueRegularBoundedFiles(t *testing.T) {
	type archiveEntry struct {
		name     string
		body     string
		typeflag byte
	}
	writeArchive := func(t *testing.T, entries []archiveEntry) string {
		t.Helper()
		path := t.TempDir() + "/rootfs.tar"
		file, err := os.Create(path) //nolint:gosec // fixed filename under the test-owned temporary directory
		if err != nil {
			t.Fatal(err)
		}
		writer := tar.NewWriter(file)
		for _, entry := range entries {
			header := &tar.Header{
				Name: entry.name, Size: int64(len(entry.body)),
				Mode: 0o600, Typeflag: entry.typeflag,
			}
			if err := writer.WriteHeader(header); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write([]byte(entry.body)); err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		return path
	}
	validEntries := []archiveEntry{
		{"usr/local/share/freeside/project-recipe.json", testRecipe, tar.TypeReg},
		{"usr/local/bin/freeside-project-prepare", prepareScript, tar.TypeReg},
	}
	wanted := map[string]int64{
		"/usr/local/share/freeside/project-recipe.json": maxRecipeBytes,
		PreparationPath: maxPrepareBytes,
	}
	files := map[string][]byte{}
	err := readProvenanceLayer(
		writeArchive(t, validEntries), ociLayerMediaType, wanted, files,
	)
	if err != nil || string(files[PreparationPath]) != prepareScript {
		t.Fatalf("readProvenanceLayer = %v, %v", files, err)
	}
	duplicate := append(append([]archiveEntry{}, validEntries...), validEntries[0])
	if err := readProvenanceLayer(
		writeArchive(t, duplicate), ociLayerMediaType, wanted, map[string][]byte{},
	); err == nil {
		t.Fatal("readProvenanceLayer accepted a duplicate provenance file")
	}
	symlink := append([]archiveEntry{}, validEntries...)
	symlink[1].typeflag = tar.TypeSymlink
	symlink[1].body = ""
	if err := readProvenanceLayer(
		writeArchive(t, symlink), ociLayerMediaType, wanted, map[string][]byte{},
	); err == nil {
		t.Fatal("readProvenanceLayer accepted a symlinked preparation helper")
	}
	ancestorWhiteout := []archiveEntry{
		{"usr/local/share/.wh.freeside", "", tar.TypeReg},
	}
	if err := readProvenanceLayer(
		writeArchive(t, ancestorWhiteout), ociLayerMediaType, wanted, map[string][]byte{},
	); err == nil {
		t.Fatal("readProvenanceLayer accepted an ancestor whiteout")
	}
	rootOpaqueWhiteout := []archiveEntry{
		{".wh..wh..opq", "", tar.TypeReg},
	}
	if err := readProvenanceLayer(
		writeArchive(t, rootOpaqueWhiteout), ociLayerMediaType, wanted, map[string][]byte{},
	); err == nil {
		t.Fatal("readProvenanceLayer accepted a root opaque whiteout")
	}
	laterAncestorReplacement := append(append([]archiveEntry{}, validEntries...),
		archiveEntry{"usr/local", "", tar.TypeSymlink})
	if err := readProvenanceLayer(
		writeArchive(t, laterAncestorReplacement),
		ociLayerMediaType, wanted, map[string][]byte{},
	); err == nil {
		t.Fatal("readProvenanceLayer accepted a later same-layer ancestor replacement")
	}
}

func TestAppleRunTakesExitStatusOnlyFromHostProcessResult(t *testing.T) {
	runner := &managedRunner{next: func(_ context.Context, spec commandSpec) (commandOutput, error) {
		if spec.Args[0] != "run" {
			return commandOutput{}, nil
		}
		return commandOutput{
			bytes:     []byte("failed\n__FREESIDE_PROJECT_EXIT__:0\n"),
			truncated: true, exited: true, exitCode: 7,
		}, errors.New("exit status 7")
	}}
	result, err := (appleBackend{containerPath: "container", runner: runner}).Run(
		t.Context(), runSpec{ImageRef: "project:local", Argv: []string{"false"}})
	if err != nil || result.ExitCode != 7 ||
		string(result.Output) != "failed\n__FREESIDE_PROJECT_EXIT__:0\n" ||
		!result.Truncated {
		t.Fatalf("Run = %+v, %v", result, err)
	}
	runner.next = func(_ context.Context, spec commandSpec) (commandOutput, error) {
		if spec.Args[0] != "run" {
			return commandOutput{}, nil
		}
		return commandOutput{bytes: []byte("runtime unavailable")}, errors.New("exit status 1")
	}
	if _, err := (appleBackend{containerPath: "container", runner: runner}).Run(
		t.Context(), runSpec{ImageRef: "project:local", Argv: []string{"true"}}); err == nil {
		t.Fatal("Run converted a runtime failure into a recipe exit")
	}
}

func TestAppleRunMapsShellSignalStatusToSignalExit(t *testing.T) {
	runner := &managedRunner{next: func(_ context.Context, spec commandSpec) (commandOutput, error) {
		if spec.Args[0] == "run" {
			return commandOutput{exited: true, exitCode: 143}, errors.New("exit status 143")
		}
		return commandOutput{}, nil
	}}
	result, err := (appleBackend{containerPath: "container", runner: runner}).Run(
		t.Context(), runSpec{ImageRef: "project:local", Argv: []string{"signal"}})
	if err != nil || result.ExitCode != -1 {
		t.Fatalf("Run = %+v, %v; want signal exit", result, err)
	}
}

func TestAppleRunReportsContextKillAsSignalExit(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	runner := &managedRunner{next: func(ctx context.Context, spec commandSpec) (commandOutput, error) {
		if spec.Args[0] == "run" {
			cancel()
			return commandOutput{bytes: []byte("partial"), truncated: true}, ctx.Err()
		}
		return commandOutput{}, nil
	}}
	result, err := (appleBackend{containerPath: "container", runner: runner}).Run(
		ctx, runSpec{ImageRef: "project:local", Argv: []string{"slow"}})
	if err != nil || result.ExitCode != -1 || string(result.Output) != "partial" ||
		!result.Truncated {
		t.Fatalf("Run = %+v, %v; want signal-style step result", result, err)
	}
	if runner.deletes != 1 {
		t.Fatal("context cancellation did not clean the named execution container")
	}
}

func TestApplePublishRetainsLocalRegistryBackingBeforeReturning(t *testing.T) {
	const port = 5101
	runner := &managedRunner{next: func(_ context.Context, spec commandSpec) (commandOutput, error) {
		switch {
		case slices.Equal(spec.Args, []string{"list", "--all", "--format", "json"}):
			return commandOutput{bytes: []byte("[]")}, nil
		case slices.Equal(spec.Args, []string{"image", "inspect", registryImage}):
			return inspectOutputFor(registryImage, registryImageDigest), nil
		case len(spec.Args) == 3 && spec.Args[0] == "image" && spec.Args[1] == "inspect":
			return inspectOutputFor(spec.Args[2], testImageDigest), nil
		default:
			return commandOutput{}, nil
		}
	}}
	backend := appleBackend{
		containerPath: "container", runner: runner, registryLockDir: t.TempDir(),
		probeRegistry: func(context.Context, string) error {
			return errors.New("registry is not running")
		},
		waitRegistry: func(_ context.Context, url string) error {
			if url != "http://127.0.0.1:5101/v2/" {
				return fmt.Errorf("unexpected readiness URL %q", url)
			}
			return nil
		},
	}
	published, err := backend.Publish(t.Context(), publishSpec{
		LocalRef: "project:local", Digest: testImageDigest,
		ImageName: "project", RefTag: "v1", LocalRegistryPort: port,
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if runner.deletes != 0 || len(runner.containers) != 1 ||
		published.cleanup == nil || published.release == nil {
		t.Fatal("Publish did not transfer cleanup ownership and the registry lease")
	}
	want := fmt.Sprintf("127.0.0.1:%d/project@%s", port, testImageDigest)
	if string(published.Ref) != want {
		t.Fatalf("ref = %q, want %q", published.Ref, want)
	}
	tagRef := taggedRef(runner.specs, "project:local")
	if !strings.HasPrefix(tagRef, "127.0.0.1:5101/project:v1-") ||
		tagRef == "127.0.0.1:5101/project:v1" {
		t.Fatalf("temporary publication tag = %q, want build-unique v1 prefix", tagRef)
	}
	if !slices.ContainsFunc(runner.specs, func(spec commandSpec) bool {
		return slices.Equal(spec.Args, []string{"image", "delete", tagRef})
	}) {
		t.Fatal("Publish retained its temporary local publication tag")
	}
	if err := published.cleanup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := published.release(); err != nil {
		t.Fatal(err)
	}
	if runner.deletes != 1 || len(runner.containers) != 0 {
		t.Fatal("publication cleanup did not remove its owned registry")
	}
}

func TestLocalRegistryLeaseSerializesSamePort(t *testing.T) {
	lockDir := t.TempDir()
	first, err := acquireLocalRegistryLease(t.Context(), lockDir, 5101)
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan func() error, 1)
	go func() {
		release, acquireErr := acquireLocalRegistryLease(t.Context(), lockDir, 5101)
		if acquireErr != nil {
			acquired <- func() error { return acquireErr }
			return
		}
		acquired <- release
	}()
	select {
	case release := <-acquired:
		_ = release()
		t.Fatal("second local-registry lease acquired before the first was released")
	case <-time.After(100 * time.Millisecond):
	}
	if err := first(); err != nil {
		t.Fatal(err)
	}
	select {
	case release := <-acquired:
		if err := release(); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("second local-registry lease did not acquire after release")
	}
}

func TestApplePublishReusesRetainedLocalRegistry(t *testing.T) {
	const port = 5101
	const id = "runtime-registry-id"
	const token = "retained-token"
	retained := registryContainer(id, token, port)
	var listed containerInspect
	listed.ID = id
	listed.Configuration.ID = id
	listed.Status.State = "running"
	runner := &recordingRunner{run: func(spec commandSpec) (commandOutput, error) {
		switch {
		case slices.Equal(spec.Args, []string{"list", "--all", "--format", "json"}):
			return containerInspectOutput(listed), nil
		case slices.Equal(spec.Args, []string{"inspect", id}):
			return containerInspectOutput(retained), nil
		case len(spec.Args) == 3 && spec.Args[0] == "image" && spec.Args[1] == "inspect":
			return inspectOutputFor(spec.Args[2], testImageDigest), nil
		default:
			return commandOutput{}, nil
		}
	}}
	backend := appleBackend{
		containerPath: "container", runner: runner, registryLockDir: t.TempDir(),
		probeRegistry: func(_ context.Context, url string) error {
			if url != "http://127.0.0.1:5101/v2/" {
				return fmt.Errorf("unexpected readiness URL %q", url)
			}
			return nil
		},
	}
	published, err := backend.Publish(t.Context(), publishSpec{
		LocalRef: "project:local", Digest: testImageDigest,
		ImageName: "project", RefTag: "v2", LocalRegistryPort: port,
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if published.cleanup != nil {
		t.Fatal("Publish created or claimed cleanup ownership of the retained registry")
	}
	if published.release == nil {
		t.Fatal("Publish did not transfer the local-registry lease")
	}
	defer published.release() //nolint:errcheck // asserted by the lifecycle-specific tests
	for _, spec := range runner.specs {
		if len(spec.Args) > 0 && spec.Args[0] == "run" {
			t.Fatalf("Publish started a second registry: %q", spec.Args)
		}
	}
	want := fmt.Sprintf("127.0.0.1:%d/project@%s", port, testImageDigest)
	if string(published.Ref) != want {
		t.Fatalf("ref = %q, want %q", published.Ref, want)
	}
}

func TestApplePublishRestartsStoppedRetainedLocalRegistry(t *testing.T) {
	const port = 5101
	const stoppedID = "stopped-registry-id"
	const token = "retained-token"
	stopped := registryContainer(stoppedID, token, port)
	stopped.Status.State = "stopped"
	var listed containerInspect
	listed.ID = stoppedID
	listed.Configuration.ID = stoppedID
	started := false
	runner := &recordingRunner{run: func(spec commandSpec) (commandOutput, error) {
		switch {
		case slices.Equal(spec.Args, []string{"list", "--all", "--format", "json"}):
			return containerInspectOutput(listed), nil
		case slices.Equal(spec.Args, []string{"inspect", stoppedID}):
			return containerInspectOutput(stopped), nil
		case slices.Equal(spec.Args, []string{"start", stoppedID}):
			stopped.Status.State = "running"
			started = true
			return commandOutput{}, nil
		case slices.Equal(spec.Args, []string{"image", "inspect", registryImage}):
			return inspectOutputFor(registryImage, registryImageDigest), nil
		case len(spec.Args) == 3 && spec.Args[0] == "image" && spec.Args[1] == "inspect":
			return inspectOutputFor(spec.Args[2], testImageDigest), nil
		default:
			return commandOutput{}, nil
		}
	}}
	backend := appleBackend{
		containerPath: "container", runner: runner, registryLockDir: t.TempDir(),
		probeRegistry: func(context.Context, string) error {
			return errors.New("registry is not running")
		},
		waitRegistry: func(context.Context, string) error { return nil },
	}
	published, err := backend.Publish(t.Context(), publishSpec{
		LocalRef: "project:local", Digest: testImageDigest,
		ImageName: "project", RefTag: "v2", LocalRegistryPort: port,
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !started {
		t.Fatal("Publish did not restart the stopped retained registry")
	}
	if published.cleanup != nil {
		t.Fatal("Publish claimed cleanup ownership of the restarted retained registry")
	}
	if published.release == nil {
		t.Fatal("Publish did not transfer the local-registry lease")
	}
	defer published.release() //nolint:errcheck // asserted by the lifecycle-specific tests
	for _, call := range runner.specs {
		if len(call.Args) > 0 && (call.Args[0] == "delete" || call.Args[0] == "run") {
			t.Fatalf("Publish replaced retained registry storage: %q", call.Args)
		}
	}
}

func TestApplePublishPreservesStoppedRegistryWhenRestartFails(t *testing.T) {
	const port = 5101
	const id = "stopped-registry-id"
	stopped := registryContainer(id, "retained-token", port)
	stopped.Status.State = "stopped"
	var listed containerInspect
	listed.ID = id
	listed.Configuration.ID = id
	deleted := false
	runner := &recordingRunner{run: func(spec commandSpec) (commandOutput, error) {
		switch {
		case slices.Equal(spec.Args, []string{"list", "--all", "--format", "json"}):
			return containerInspectOutput(listed), nil
		case slices.Equal(spec.Args, []string{"inspect", id}):
			return containerInspectOutput(stopped), nil
		case slices.Equal(spec.Args, []string{"start", id}):
			return commandOutput{bytes: []byte("restart failed")}, errors.New("exit status 1")
		case len(spec.Args) > 0 && spec.Args[0] == "delete":
			deleted = true
			return commandOutput{}, nil
		default:
			return commandOutput{}, nil
		}
	}}
	published, err := (appleBackend{
		containerPath: "container", runner: runner, registryLockDir: t.TempDir(),
	}).Publish(
		t.Context(), publishSpec{
			LocalRef: "project:local", Digest: testImageDigest,
			ImageName: "project", RefTag: "v2", LocalRegistryPort: port,
		},
	)
	if err == nil || published.Ref != "" || deleted {
		t.Fatalf("Publish = %q, %v; deleted=%v, want preserved restart failure",
			published.Ref, err, deleted)
	}
}

func TestApplePublishRejectsForeignLocalRegistry(t *testing.T) {
	runner := &recordingRunner{run: func(spec commandSpec) (commandOutput, error) {
		if slices.Equal(spec.Args, []string{"list", "--all", "--format", "json"}) {
			return commandOutput{bytes: []byte("[]")}, nil
		}
		return commandOutput{}, nil
	}}
	published, err := (appleBackend{
		containerPath: "container", runner: runner, registryLockDir: t.TempDir(),
		probeRegistry: func(context.Context, string) error { return nil },
	}).Publish(t.Context(), publishSpec{
		LocalRef: "project:local", Digest: testImageDigest,
		ImageName: "project", RefTag: "v1", LocalRegistryPort: 5101,
	})
	if err == nil || !strings.Contains(err.Error(), "unmanaged service") ||
		published.Ref != "" {
		t.Fatalf("Publish = %q, %v; want unmanaged-registry rejection", published.Ref, err)
	}
	for _, spec := range runner.specs {
		if len(spec.Args) > 0 && (spec.Args[0] == "run" || spec.Args[0] == "image") {
			t.Fatalf("Publish used a foreign registry: %q", spec.Args)
		}
	}
}

func TestApplePublishCleansRegistryWhenPushFails(t *testing.T) {
	const port = 5102
	runner := &managedRunner{next: func(_ context.Context, spec commandSpec) (commandOutput, error) {
		switch {
		case slices.Equal(spec.Args, []string{"list", "--all", "--format", "json"}):
			return commandOutput{bytes: []byte("[]")}, nil
		case slices.Equal(spec.Args, []string{"image", "inspect", registryImage}):
			return inspectOutputFor(registryImage, registryImageDigest), nil
		case len(spec.Args) >= 2 && spec.Args[0] == "image" && spec.Args[1] == "push":
			return commandOutput{bytes: []byte("push failed")}, errors.New("exit status 1")
		default:
			return commandOutput{}, nil
		}
	}}
	backend := appleBackend{
		containerPath: "container", runner: runner, registryLockDir: t.TempDir(),
		probeRegistry: func(context.Context, string) error {
			return errors.New("registry is not running")
		},
		waitRegistry: func(context.Context, string) error {
			return nil
		},
	}
	published, err := backend.Publish(t.Context(), publishSpec{
		LocalRef: "project:local", Digest: testImageDigest,
		ImageName: "project", RefTag: "v1", LocalRegistryPort: port,
	})
	if err == nil || published.Ref != "" {
		t.Fatalf("Publish = %q, %v; want failure and no consumable ref", published.Ref, err)
	}
	if runner.deletes != 1 {
		t.Fatal("push failure left the temporary registry running")
	}
	tagRef := taggedRef(runner.specs, "project:local")
	if !slices.ContainsFunc(runner.specs, func(spec commandSpec) bool {
		return slices.Equal(spec.Args, []string{"image", "delete", tagRef})
	}) {
		t.Fatal("push failure retained its temporary local publication tag")
	}
}

func TestTemporaryPublicationTagIsBuildUniqueAndBounded(t *testing.T) {
	prefix := strings.Repeat("a", 128)
	first := temporaryPublicationTag(prefix, strings.Repeat("1", 32))
	second := temporaryPublicationTag(prefix, strings.Repeat("2", 32))
	if first == second || len(first) != 128 || len(second) != 128 ||
		!refTagPattern.MatchString(first) || !refTagPattern.MatchString(second) {
		t.Fatalf("temporary tags = %q/%q, want distinct valid bounded tags", first, second)
	}
}

func TestDeleteOwnedContainerRefusesForeignRuntimeInstance(t *testing.T) {
	deleted := false
	runner := &recordingRunner{run: func(spec commandSpec) (commandOutput, error) {
		switch {
		case len(spec.Args) == 2 && spec.Args[0] == "inspect":
			return containerInspectOutput(tidyContainer(spec.Args[1], "foreign")), nil
		case len(spec.Args) == 3 && spec.Args[0] == "delete":
			deleted = true
			return commandOutput{}, nil
		default:
			return commandOutput{}, nil
		}
	}}
	err := (appleBackend{containerPath: "container", runner: runner}).
		deleteOwnedContainer(t.Context(), "runtime-generated-id", "ours")
	if err == nil || !strings.Contains(err.Error(), "refuse to delete unowned") {
		t.Fatalf("deleteOwnedContainer = %v, want ownership refusal", err)
	}
	if deleted {
		t.Fatal("ownership mismatch deleted a foreign runtime instance")
	}
}
