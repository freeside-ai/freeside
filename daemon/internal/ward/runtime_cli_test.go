package ward

import (
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
		State: StateStopped,
		Mounts: []Mount{{
			Type:     MountVolume,
			Source:   "freeside-handoff-run-1-ws",
			Target:   "/workspace",
			ReadOnly: true,
		}},
		Env: []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
	}
	if !reflect.DeepEqual(rep, want) {
		t.Errorf("decoded report = %+v, want %+v", rep, want)
	}
	// This exact report passes check 4 against its allowlist: the decode
	// and the verifier agree on the conforming shape.
	cfg := testConfig()
	if err := verifyExporterAllowlist(cfg, rep, "freeside-handoff-run-1-ws"); err != nil {
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
	if err := verifyExporterAllowlist(cfg, rep, "any"); err == nil {
		t.Error("hostile fixture passed the allowlist")
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
// 4's allowlist inputs (environment, ssh, publishedPorts, publishedSockets)
// fails closed rather than decoding the absence as an explicit clean report.
func TestDecodeInspectAllowlistFieldPresence(t *testing.T) {
	// A report with the correct id and state but each allowlist field omitted
	// in turn must be rejected.
	fields := map[string]string{
		"environment":      `"initProcess":{},"ssh":false,"publishedPorts":[],"publishedSockets":[]`,
		"ssh":              `"initProcess":{"environment":[]},"publishedPorts":[],"publishedSockets":[]`,
		"publishedPorts":   `"initProcess":{"environment":[]},"ssh":false,"publishedSockets":[]`,
		"publishedSockets": `"initProcess":{"environment":[]},"ssh":false,"publishedPorts":[]`,
	}
	for missing, cfg := range fields {
		t.Run("missing "+missing, func(t *testing.T) {
			out := []byte(`[{"id":"c","configuration":{"id":"c",` + cfg + `},"status":{"state":"stopped"}}]`)
			if _, err := decodeInspect(out, "c"); err == nil {
				t.Errorf("inspect missing %q decoded without error", missing)
			}
		})
	}
	// All four present decodes cleanly.
	out := []byte(`[{"id":"c","configuration":{"id":"c","initProcess":{"environment":[]},"ssh":false,"publishedPorts":[],"publishedSockets":[]},"status":{"state":"stopped"}}]`)
	if _, err := decodeInspect(out, "c"); err != nil {
		t.Errorf("complete report failed: %v", err)
	}
}

func TestDecodeContainerList(t *testing.T) {
	got, err := decodeContainerList(readFixture(t, "cli-list-all.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := []ContainerSummary{
		{ID: "freeside-handoff-run-1-agent", State: StateStopped, Labels: []Label{}},
		{ID: "unrelated-container", State: StateRunning, Labels: []Label{}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("decoded list = %+v, want %+v", got, want)
	}
	withLabels, err := decodeContainerList([]byte(`[{"id":"c","configuration":{"id":"c","labels":{"z":"2","a":"1"}},"status":{"state":"stopped"}}]`))
	if err != nil {
		t.Fatal(err)
	}
	wantLabels := []Label{{Key: "a", Value: "1"}, {Key: "z", Value: "2"}}
	if !reflect.DeepEqual(withLabels[0].Labels, wantLabels) {
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
		{Name: "freeside-handoff-run-1-ws", Labels: []Label{{Key: "freeside.handoff", Value: "run-1"}}},
		{Name: "unlabeled-volume", Labels: []Label{}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("decoded volumes = %+v, want %+v", got, want)
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
	// An explicit key=value env and clean mounts still pass.
	ok := base
	ok.Env = []string{"AGENT_MODE=fixture"}
	if _, err := createContainerArgs(ok); err != nil {
		t.Errorf("clean spec refused: %v", err)
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
