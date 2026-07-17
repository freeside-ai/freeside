package ward

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // fixed test fixture name
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestDecodeInspectVolume pins the 1.1.0 inspect shape for the conforming
// exporter: one named-volume mount decoded with its volume name (not the
// host block-image path), options ["ro"] mapped to ReadOnly, image PATH
// environment, no SSH, no publications.
func TestDecodeInspectVolume(t *testing.T) {
	rep, err := decodeInspect(readFixture(t, "cli-inspect-volume.json"), "freeside-handoff-run-1-exporter")
	if err != nil {
		t.Fatal(err)
	}
	want := InspectReport{
		ID:                      "freeside-handoff-run-1-exporter",
		ImageReference:          "docker.io/library/alpine:3.22",
		ImageDigest:             "sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce",
		Command:                 []string{"sh", "-c", "echo hi"},
		WorkingDirectory:        "/",
		State:                   StateStopped,
		AllowlistFieldsObserved: true,
		Mounts: []Mount{{
			Type:     MountVolume,
			Source:   "freeside-handoff-run-1-ws",
			Target:   "/workspace",
			ReadOnly: true,
		}},
		Env:            []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
		Labels:         []Label{{Key: "freeside.handoff", Value: "run-1"}},
		LabelsObserved: true,
	}
	if !reflect.DeepEqual(rep, want) {
		t.Errorf("decoded report = %+v, want %+v", rep, want)
	}
	// This exact report passes check 4 against its allowlist: the decode
	// and the verifier agree on the conforming shape.
	cfg := testConfig()
	cfg.ExporterImage = want.ImageReference + "@" + want.ImageDigest
	cfg.ExporterCommand = want.Command
	if err := verifyExporterAllowlist(cfg, rep, want.ID, "freeside-handoff-run-1-ws"); err != nil {
		t.Errorf("conforming fixture fails allowlist: %v", err)
	}
}

// TestDecodeInspectHostile pins the mappings verification depends on to
// reject a hostile configuration: virtiofs decodes to the bind type, an
// unknown mount kind and an unnamed volume decode to invalid types (never
// silently dropped), and ssh/publications/extra env survive decoding.
func TestDecodeInspectHostile(t *testing.T) {
	rep, err := decodeInspect(readFixture(t, "cli-inspect-bind.json"), "hostile-fixture")
	if err != nil {
		t.Fatal(err)
	}
	if rep.State != StateRunning {
		t.Errorf("State = %q, want running", rep.State)
	}
	if len(rep.Mounts) != 3 {
		t.Fatalf("Mounts = %d, want 3 (nothing dropped)", len(rep.Mounts))
	}
	bind := rep.Mounts[0]
	if bind.Type != MountBind || bind.Source != "/Users/owner/project" || bind.Target != "/hostbind" {
		t.Errorf("bind mount decoded as %+v", bind)
	}
	if rep.Mounts[1].Type.valid() {
		t.Errorf("unknown mount kind decoded to valid type %q", rep.Mounts[1].Type)
	}
	if rep.Mounts[2].Type.valid() {
		t.Errorf("unnamed volume decoded to valid type %q", rep.Mounts[2].Type)
	}
	if !rep.SSH {
		t.Error("ssh flag lost in decoding")
	}
	if len(rep.PublishedSockets) != 1 || len(rep.PublishedPorts) != 1 {
		t.Errorf("publications lost: sockets %d, ports %d", len(rep.PublishedSockets), len(rep.PublishedPorts))
	}
	if len(rep.Env) != 2 {
		t.Errorf("environment lost: %v", rep.Env)
	}
	// And the allowlist rejects it.
	cfg := testConfig()
	if err := verifyExporterAllowlist(cfg, rep, "hostile-fixture", "any"); err == nil {
		t.Error("hostile fixture passed the allowlist")
	}
}

// TestToMountAccessOptions pins the access mapping the allowlist depends on:
// ro alone is read-only, rw alone (or neither) is not, and a mount claiming
// both is contradictory and flagged so verification fails closed rather than
// trusting ro's mere presence.
func TestToMountAccessOptions(t *testing.T) {
	vol := map[string]json.RawMessage{"volume": json.RawMessage(`{"name":"ws"}`)}
	cases := []struct {
		name         string
		options      []string
		wantReadOnly bool
		wantConflict bool
	}{
		{"ro only", []string{"ro"}, true, false},
		{"rw only", []string{"rw"}, false, false},
		{"neither", nil, false, false},
		{"both ro and rw", []string{"ro", "rw"}, false, true},
		{"both, rw first", []string{"rw", "ro"}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := cliMount{Destination: "/workspace", Options: tc.options, Type: vol}.toMount()
			if m.ReadOnly != tc.wantReadOnly || m.AccessConflict != tc.wantConflict {
				t.Errorf("toMount(%v) = {ReadOnly:%t, AccessConflict:%t}, want {%t, %t}",
					tc.options, m.ReadOnly, m.AccessConflict, tc.wantReadOnly, tc.wantConflict)
			}
		})
	}
}

func TestDecodeInspectCardinality(t *testing.T) {
	if _, err := decodeInspect([]byte("[]"), "x"); err == nil {
		t.Error("empty inspect array decoded without error")
	}
	if _, err := decodeInspect([]byte("not json"), "x"); err == nil {
		t.Error("malformed inspect output decoded without error")
	}
}

// TestDecodeInspectIdentity: the report must be for the requested container.
// A missing or mismatched id fails closed, so checks 3/4 never act on the
// wrong (or unidentified) object's state and mounts.
func TestDecodeInspectIdentity(t *testing.T) {
	// The captured fixture is for the exporter; requesting a different name
	// must fail rather than trust the report.
	if _, err := decodeInspect(readFixture(t, "cli-inspect-volume.json"), "some-other-container"); err == nil {
		t.Error("inspect report for a different container was trusted")
	}
	// An object with no id fails closed.
	if _, err := decodeInspect([]byte(`[{"status":{"state":"stopped"}}]`), "wanted"); err == nil {
		t.Error("inspect report with no id was trusted")
	}
	// A matching top-level id but a different configuration id fails closed:
	// mounts/env/ssh all come from configuration.
	mismatch := `[{"id":"c","configuration":{"id":"other","initProcess":{"environment":[]},"ssh":false,"publishedPorts":[],"publishedSockets":[]},"status":{"state":"stopped"}}]`
	if _, err := decodeInspect([]byte(mismatch), "c"); err == nil {
		t.Error("inspect with a mismatched configuration id was trusted")
	}
	// The matching identity still decodes.
	if _, err := decodeInspect(readFixture(t, "cli-inspect-volume.json"), "freeside-handoff-run-1-exporter"); err != nil {
		t.Errorf("matching-identity inspect failed: %v", err)
	}
}

// TestDecodeInspectAllowlistFieldPresence: a report that omits any of check
// 4's allowlist inputs (image, command, environment, ssh, and publications)
// is marked incomplete and rejected by check 4 rather than reading as clean.
func TestDecodeInspectAllowlistFieldPresence(t *testing.T) {
	// A report with the correct id and state but each allowlist field omitted
	// in turn must preserve identity while remaining unapprovable.
	fields := map[string]string{
		"image":             `"initProcess":{"executable":"sh","arguments":[],"environment":[],"workingDirectory":"/"},"ssh":false,"publishedPorts":[],"publishedSockets":[]`,
		"image reference":   `"image":{"descriptor":{"digest":"sha256:abc"}},"initProcess":{"executable":"sh","arguments":[],"environment":[],"workingDirectory":"/"},"ssh":false,"publishedPorts":[],"publishedSockets":[]`,
		"image digest":      `"image":{"reference":"example.test/exporter"},"initProcess":{"executable":"sh","arguments":[],"environment":[],"workingDirectory":"/"},"ssh":false,"publishedPorts":[],"publishedSockets":[]`,
		"executable":        `"image":{"reference":"example.test/exporter","descriptor":{"digest":"sha256:abc"}},"initProcess":{"arguments":[],"environment":[],"workingDirectory":"/"},"ssh":false,"publishedPorts":[],"publishedSockets":[]`,
		"arguments":         `"image":{"reference":"example.test/exporter","descriptor":{"digest":"sha256:abc"}},"initProcess":{"executable":"sh","environment":[],"workingDirectory":"/"},"ssh":false,"publishedPorts":[],"publishedSockets":[]`,
		"environment":       `"image":{"reference":"example.test/exporter","descriptor":{"digest":"sha256:abc"}},"initProcess":{"executable":"sh","arguments":[],"workingDirectory":"/"},"ssh":false,"publishedPorts":[],"publishedSockets":[]`,
		"working directory": `"image":{"reference":"example.test/exporter","descriptor":{"digest":"sha256:abc"}},"initProcess":{"executable":"sh","arguments":[],"environment":[]},"ssh":false,"publishedPorts":[],"publishedSockets":[]`,
		"ssh":               `"image":{"reference":"example.test/exporter","descriptor":{"digest":"sha256:abc"}},"initProcess":{"executable":"sh","arguments":[],"environment":[],"workingDirectory":"/"},"publishedPorts":[],"publishedSockets":[]`,
		"publishedPorts":    `"image":{"reference":"example.test/exporter","descriptor":{"digest":"sha256:abc"}},"initProcess":{"executable":"sh","arguments":[],"environment":[],"workingDirectory":"/"},"ssh":false,"publishedSockets":[]`,
		"publishedSockets":  `"image":{"reference":"example.test/exporter","descriptor":{"digest":"sha256:abc"}},"initProcess":{"executable":"sh","arguments":[],"environment":[],"workingDirectory":"/"},"ssh":false,"publishedPorts":[]`,
	}
	for missing, cfg := range fields {
		t.Run("missing "+missing, func(t *testing.T) {
			out := []byte(`[{"id":"c","configuration":{"id":"c",` + cfg + `},"status":{"state":"stopped"}}]`)
			rep, err := decodeInspect(out, "c")
			if err != nil {
				t.Fatalf("inspect missing %q could not decode identity/labels: %v", missing, err)
			}
			if rep.AllowlistFieldsObserved {
				t.Errorf("inspect missing %q marked the exporter allowlist complete", missing)
			}
			if err := verifyExporterAllowlist(testConfig(), rep, "c", "workspace"); err == nil {
				t.Errorf("inspect missing %q passed the exporter allowlist", missing)
			}
		})
	}
	// Every required field present decodes cleanly.
	out := []byte(`[{"id":"c","configuration":{"id":"c","image":{"reference":"example.test/exporter","descriptor":{"digest":"sha256:abc"}},"initProcess":{"executable":"sh","arguments":[],"environment":[],"workingDirectory":"/"},"ssh":false,"publishedPorts":[],"publishedSockets":[]},"status":{"state":"stopped"}}]`)
	rep, err := decodeInspect(out, "c")
	if err != nil {
		t.Errorf("complete report failed: %v", err)
	} else if !rep.AllowlistFieldsObserved {
		t.Error("complete report marked the exporter allowlist incomplete")
	}
}

func TestDecodeContainerList(t *testing.T) {
	got, err := decodeContainerList(readFixture(t, "cli-list-all.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := []ContainerSummary{
		{ID: "freeside-handoff-run-1-agent", State: StateStopped},
		{ID: "unrelated-container", State: StateRunning},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("decoded list = %+v, want %+v", got, want)
	}
	withLabels, err := decodeContainerList([]byte(`[{"id":"c","configuration":{"id":"c","labels":{"z":"2","a":"1"}},"status":{"state":"stopped"}}]`))
	if err != nil {
		t.Fatal(err)
	}
	wantLabels := []Label{{Key: "a", Value: "1"}, {Key: "z", Value: "2"}}
	if !withLabels[0].LabelsObserved || !reflect.DeepEqual(withLabels[0].Labels, wantLabels) {
		t.Errorf("decoded labels = %+v, want %+v", withLabels[0].Labels, wantLabels)
	}
}

// TestDecodeListFailsClosedOnMissingIdentity: a list entry without an id
// (containers) or name (volumes) is a decode error, never a silently
// unidentified entry the absence proofs would treat as absent.
func TestDecodeListFailsClosedOnMissingIdentity(t *testing.T) {
	if _, err := decodeContainerList([]byte(`[{"status":{"state":"running"}}]`)); err == nil {
		t.Error("container list entry with no id decoded without error")
	}
	// A well-formed entry alongside a broken one still fails closed.
	if _, err := decodeContainerList([]byte(`[{"id":"ok","status":{"state":"stopped"}},{"status":{"state":"running"}}]`)); err == nil {
		t.Error("container list with one unidentified entry decoded without error")
	}
	if _, err := decodeVolumeList([]byte(`[{"id":"v","configuration":{"labels":{}}}]`)); err == nil {
		t.Error("volume list entry with no name decoded without error")
	}
	// A volume whose top-level id disagrees with its configuration name fails
	// closed: teardown matches on the name, so a drifted pair is ambiguous.
	if _, err := decodeContainerList([]byte(`[{"id":"c","configuration":{"id":"other"},"status":{"state":"stopped"}}]`)); err == nil {
		t.Error("container list entry with a mismatched configuration id was trusted")
	}
	if _, err := decodeVolumeList([]byte(`[{"id":"a","configuration":{"name":"b","labels":{}}}]`)); err == nil {
		t.Error("volume list entry with id != name was trusted")
	}
}

func TestDecodeVolumeList(t *testing.T) {
	got, err := decodeVolumeList(readFixture(t, "cli-volume-list.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := []VolumeSummary{
		{Name: "freeside-handoff-run-1-ws", Labels: []Label{{Key: "freeside.handoff", Value: "run-1"}}, LabelsObserved: true},
		{Name: "unlabeled-volume", Labels: []Label{}, LabelsObserved: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("decoded volumes = %+v, want %+v", got, want)
	}
	missingLabels, err := decodeVolumeList([]byte(`[{"id":"v","configuration":{"name":"v"}}]`))
	if err != nil {
		t.Fatal(err)
	}
	if missingLabels[0].LabelsObserved {
		t.Error("omitted volume labels decoded as observed")
	}
}

// TestCreateContainerArgs pins the CLI phrasing of the generated exporter
// spec, readonly flag included, and the refusal to phrase non-volume mounts.
func TestCreateContainerArgs(t *testing.T) {
	cfg := testConfig()
	hs := testHandoffSpec()
	names := namesFor(hs.RunID)

	args, err := createContainerArgs(buildExporterSpec(cfg, hs, names, testOwnershipLabel()))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"create", "--name", "freeside-handoff-golden-run-exporter",
		"--label", "freeside.handoff=golden-run",
		"--label", "freeside.handoff-owner=00000000000000000000000000000000",
		"--mount", "type=volume,source=freeside-handoff-golden-run-ws,target=/workspace,readonly",
		"--", cfg.ExporterImage,
	}
	want = append(want, cfg.ExporterCommand...)
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args = %q, want %q", args, want)
	}

	agent := buildAgentSpec(cfg, hs, names, testOwnershipLabel())
	agentArgs, err := createContainerArgs(agent)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(agentArgs, " ")
	if !strings.Contains(joined, "type=volume,source=freeside-handoff-golden-run-ws,target=/workspace ") {
		t.Errorf("writer workspace mount missing or mis-phrased: %q", joined)
	}
	if strings.Contains(joined, "target=/workspace,readonly") {
		t.Errorf("writer workspace phrased readonly: %q", joined)
	}
	if !strings.Contains(joined, "type=volume,source=provider-cred,target=/credentials,readonly") {
		t.Errorf("credential mount not phrased readonly: %q", joined)
	}

	if _, err := createContainerArgs(ContainerSpec{
		Name:   "x",
		Image:  "img",
		Mounts: []Mount{{Type: MountBind, Source: "/host", Target: "/m"}},
	}); err == nil {
		t.Error("bind mount phrased instead of refused")
	}
}

// TestCreateContainerArgsRefusesInjection is the CLI boundary's fail-closed
// guard for the two review findings: a bare-key env entry (which the CLI
// would inherit from the host) and a comma-bearing mount field (which the
// CLI would parse as an injected mount option) are both refused at phrasing
// time, so a direct Runtime caller cannot bypass the spec-level checks.
func TestCreateContainerArgsRefusesInjection(t *testing.T) {
	base := ContainerSpec{
		Name:   "c",
		Image:  "img",
		Mounts: []Mount{{Type: MountVolume, Source: "ws", Target: "/workspace"}},
	}
	cases := []struct {
		name   string
		mutate func(*ContainerSpec)
	}{
		{"bare env key inherits host", func(s *ContainerSpec) { s.Env = []string{"GITHUB_TOKEN"} }},
		{"empty env key", func(s *ContainerSpec) { s.Env = []string{"=v"} }},
		{"comma in mount source", func(s *ContainerSpec) { s.Mounts[0].Source = "ws,readonly" }},
		{"comma in mount target", func(s *ContainerSpec) { s.Mounts[0].Target = "/workspace,type=bind" }},
		{"newline in mount target", func(s *ContainerSpec) { s.Mounts[0].Target = "/workspace\nx" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := base
			spec.Mounts = append([]Mount(nil), base.Mounts...)
			tc.mutate(&spec)
			if _, err := createContainerArgs(spec); err == nil {
				t.Errorf("createContainerArgs accepted injection: %+v", spec)
			}
		})
	}
	const secret = "secret-fixture-value"
	secretSpec := base
	secretSpec.Env = []string{"github_pat_" + secret}
	if _, err := createContainerArgs(secretSpec); err == nil || strings.Contains(err.Error(), secret) {
		t.Errorf("bare-entry refusal leaked or accepted the environment value: %v", err)
	}
	// An explicit key=value env and clean mounts still pass.
	ok := base
	ok.Env = []string{"AGENT_MODE=fixture"}
	if _, err := createContainerArgs(ok); err != nil {
		t.Errorf("clean spec refused: %v", err)
	}
}

// TestCreateContainerRedactsStderr proves an external CLI failure cannot
// echo an accepted explicit environment credential through the Runtime
// error. The exit status remains available for operational classification.
func TestCreateContainerRedactsStderr(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "container-fixture")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" >&2\nexit 7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bin, 0o700); err != nil { //nolint:gosec // executable test fixture is isolated in t.TempDir
		t.Fatal(err)
	}
	const secret = "secret-fixture-value"
	err := NewCLIRuntime(bin).CreateContainer(context.Background(), ContainerSpec{
		Name:  "fixture",
		Image: "fixture-image",
		Env:   []string{"PROVIDER_TOKEN=" + secret},
	})
	if err == nil {
		t.Fatal("CreateContainer returned nil, want exit error")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "PROVIDER_TOKEN") {
		t.Errorf("CreateContainer error leaked an environment credential: %v", err)
	}
	if !strings.Contains(err.Error(), "exit status 7") {
		t.Errorf("CreateContainer error lost the safe exit status: %v", err)
	}
}

// TestExportRootFSStreamsStdout pins the boundary CLIRuntime can enforce: the
// CLI emits its default stdout stream into the caller-owned bounded Writer.
func TestExportRootFSStreamsStdout(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "container-fixture")
	script := "#!/bin/sh\n" +
		"[ \"$1\" = export ] || exit 8\n" +
		"[ \"$2\" = fixture ] || exit 9\n" +
		"printf archive-bytes\n"
	if err := os.WriteFile(bin, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bin, 0o700); err != nil { //nolint:gosec // executable test fixture is isolated in t.TempDir
		t.Fatal(err)
	}
	var dest bytes.Buffer
	if err := NewCLIRuntime(bin).ExportRootFS(context.Background(), "fixture", &dest, 1024); err != nil {
		t.Fatal(err)
	}
	if got := dest.String(); got != "archive-bytes" {
		t.Errorf("streamed archive = %q, want archive-bytes", got)
	}
}

// TestCreateContainerArgsTerminatesOptions proves a dash-prefixed image or
// command word cannot be reparsed as a create option: "--" precedes the
// image, so everything after it is positional. Without this, an image of
// "--mount" would let the next word realize a host bind outside the spec.
func TestCreateContainerArgsTerminatesOptions(t *testing.T) {
	spec := ContainerSpec{
		Name:    "c",
		Image:   "--mount",
		Command: []string{"type=bind,source=/Users,target=/host"},
		Mounts:  []Mount{{Type: MountVolume, Source: "ws", Target: "/workspace"}},
	}
	args, err := createContainerArgs(spec)
	if err != nil {
		t.Fatal(err)
	}
	// The image must appear immediately after a "--" terminator.
	term := -1
	for i, a := range args {
		if a == "--" {
			term = i
			break
		}
	}
	if term < 0 || term+1 >= len(args) || args[term+1] != "--mount" {
		t.Fatalf("image not positioned after a -- terminator: %q", args)
	}
	// The image and command sit in the positional tail, after the terminator.
	if got := args[term+1:]; got[0] != spec.Image || got[1] != spec.Command[0] {
		t.Errorf("positional tail = %q, want image then command", got)
	}
}
