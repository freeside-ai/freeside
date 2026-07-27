package ward

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

// scannerFunc adapts a function to OutputScanner for test fixtures.
type scannerFunc func(ctx context.Context, dir string) error

func (f scannerFunc) Scan(ctx context.Context, dir string) error { return f(ctx, dir) }

// testConfig is the fixed valid fixture configuration; tests copy and mutate
// it. The all-zero digest marks it as inert fixture data.
func testConfig() Config {
	return Config{
		ExporterImage:   "example.test/exporter@sha256:" + strings.Repeat("0", 64),
		ExporterCommand: export.HelperCommand(),
		Scanner:         scannerFunc(func(context.Context, string) error { return nil }),
	}.withDefaults()
}

func testHandoffSpec() HandoffSpec {
	return HandoffSpec{
		RunID:           "golden-run",
		WorkspaceSizeMB: 64,
		Seed:            WorkspaceSeed{Mode: SeedBlank},
		Agent: AgentSpec{
			Image:   "example.test/agent@sha256:" + strings.Repeat("1", 64),
			Command: []string{"sh", "-c", "true"},
			Env:     []string{"AGENT_MODE=fixture"},
			CredentialMounts: []CredentialMount{
				{Volume: "provider-cred", Target: "/credentials"},
			},
		},
	}
}

func testOwnershipLabel() Label {
	return Label{Key: ownershipLabelKey, Value: "00000000000000000000000000000000"}
}

func TestHandoffSpecValidate(t *testing.T) {
	if err := testHandoffSpec().validate(); err != nil {
		t.Fatalf("valid fixture: validate() = %v, want nil", err)
	}

	cases := []struct {
		name   string
		mutate func(*HandoffSpec)
	}{
		{"empty run id", func(s *HandoffSpec) { s.RunID = "" }},
		{"uppercase run id", func(s *HandoffSpec) { s.RunID = "Golden-Run" }},
		{"run id with slash", func(s *HandoffSpec) { s.RunID = "a/b" }},
		{"run id too long", func(s *HandoffSpec) { s.RunID = strings.Repeat("a", 33) }},
		{"zero workspace size", func(s *HandoffSpec) { s.WorkspaceSizeMB = 0 }},
		{"missing agent image", func(s *HandoffSpec) { s.Agent.Image = "" }},
		{"unpinned agent image", func(s *HandoffSpec) { s.Agent.Image = "example.test/agent:latest" }},
		{"short agent digest", func(s *HandoffSpec) { s.Agent.Image = "example.test/agent@sha256:abc" }},
		{"missing agent command", func(s *HandoffSpec) { s.Agent.Command = nil }},
		// The seed is part of the spec's caller-error surface, so its rejections
		// must reach ErrInvalidHandoffSpec through HandoffSpec.validate too.
		{"unset seed mode", func(s *HandoffSpec) { s.Seed = WorkspaceSeed{} }},
		{"unknown seed mode", func(s *HandoffSpec) { s.Seed = WorkspaceSeed{Mode: SeedMode("copy")} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := testHandoffSpec()
			tc.mutate(&s)
			if err := s.validate(); !errors.Is(err, ErrInvalidHandoffSpec) {
				t.Errorf("validate() = %v, want ErrInvalidHandoffSpec", err)
			}
		})
	}
}

func TestNamesFor(t *testing.T) {
	n := namesFor("run-1")
	want := handoffNames{
		Workspace: "freeside-handoff-run-1-ws",
		Seeder:    "freeside-handoff-run-1-seeder",
		Observer:  "freeside-handoff-run-1-observer",
		Agent:     "freeside-handoff-run-1-agent",
		Exporter:  "freeside-handoff-run-1-exporter",
	}
	if n != want {
		t.Errorf("namesFor(run-1) = %+v, want %+v", n, want)
	}
	if got := WorkspaceRef("run-1"); got != want.Workspace {
		t.Errorf("WorkspaceRef(run-1) = %q, want %q", got, want.Workspace)
	}
}

// TestNamesForFitsRuntimeIDLimit pins every role name against the longest
// valid run ID: Apple container 1.1.0 refuses an ID over 64 bytes, and the
// networkless-export work already had to shorten a role prefix once for
// exactly this reason. "observer" is the longest suffix, so a new role longer
// than it must re-check this bound.
func TestNamesForFitsRuntimeIDLimit(t *testing.T) {
	const runtimeIDLimit = 64
	longest := "a" + strings.Repeat("b", 31) // the max runIDPattern admits
	if !runIDPattern.MatchString(longest) {
		t.Fatalf("fixture run ID %q is not a valid run ID", longest)
	}
	n := namesFor(longest)
	for role, name := range map[string]string{
		"workspace": n.Workspace, "seeder": n.Seeder, "observer": n.Observer,
		"agent": n.Agent, "exporter": n.Exporter,
	} {
		if len(name) > runtimeIDLimit {
			t.Errorf("%s name %q is %d bytes, over the runtime's %d-byte limit", role, name, len(name), runtimeIDLimit)
		}
	}
}

// TestExporterSpecGolden pins check 4's generated allowlist: the exporter
// spec is the security contract the pre-execution inspection verifies
// against, so a drift in its shape must be a reviewed diff.
func TestExporterSpecGolden(t *testing.T) {
	cfg := testConfig()
	hs := testHandoffSpec()
	spec := buildExporterSpec(cfg, hs, namesFor(hs.RunID), testOwnershipLabel())
	got, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		t.Fatalf("marshal exporter spec: %v", err)
	}
	golden.Assert(t, "exporter-spec", append(got, '\n'))
}

// TestSeederSpecGolden pins the seeder's generated allowlist for the same
// reason the exporter's is pinned, and with one extra stake: the seeder is the
// only container before the writer that holds the workspace read-write, and
// its command is the only gate-authored payload that writes the workspace. A
// drift in either must be a reviewed diff.
func TestSeederSpecGolden(t *testing.T) {
	cfg := testConfig()
	hs := testHandoffSpec()
	spec := buildSeederSpec(cfg, hs, namesFor(hs.RunID), testOwnershipLabel())
	got, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		t.Fatalf("marshal seeder spec: %v", err)
	}
	golden.Assert(t, "seeder-spec", append(got, '\n'))
}

// TestObserverSpecGolden pins the attesting container's allowlist. The
// observer is where the unit's evidence comes from, so its topology and its
// proof-writing command must not drift unreviewed.
func TestObserverSpecGolden(t *testing.T) {
	cfg := testConfig()
	hs := testHandoffSpec()
	spec := buildObserverSpec(cfg, hs, namesFor(hs.RunID), testOwnershipLabel())
	got, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		t.Fatalf("marshal observer spec: %v", err)
	}
	golden.Assert(t, "observer-spec", append(got, '\n'))
}

// TestObserverSpecReadsOnly is the observer's whole argument in one assertion:
// it cannot write what it attests. A read-write observer would be the writer
// vouching for its own write.
func TestObserverSpecReadsOnly(t *testing.T) {
	cfg := testConfig()
	hs := testHandoffSpec()
	names := namesFor(hs.RunID)
	spec := buildObserverSpec(cfg, hs, names, testOwnershipLabel())

	if len(spec.Mounts) != 1 {
		t.Fatalf("Mounts = %+v, want exactly the workspace", spec.Mounts)
	}
	if !spec.Mounts[0].ReadOnly {
		t.Error("observer workspace mount is read-write; it must be read-only")
	}
	if spec.Mounts[0].Source != names.Workspace {
		t.Errorf("observer mounts %q, want the workspace %q", spec.Mounts[0].Source, names.Workspace)
	}
	if spec.Name == names.Seeder {
		t.Error("observer and seeder share a name; the attestation must come from a different VM")
	}
	if !spec.NetworkDisabled || len(spec.Env) != 0 {
		t.Errorf("observer is not credential-free and network-free: env=%q networkDisabled=%v", spec.Env, spec.NetworkDisabled)
	}
	// The nonce must reach the guest, or the proof could be any run's.
	if !strings.Contains(observerScript(cfg, testOwnershipLabel().Value), testOwnershipLabel().Value) {
		t.Error("observer script does not carry this invocation's nonce")
	}
}

// TestSeederSpecShape asserts the properties the golden would let drift
// silently if someone regenerated it: the seeder is credential-free,
// network-free, and holds the workspace read-write at the configured target.
func TestSeederSpecShape(t *testing.T) {
	cfg := testConfig()
	hs := testHandoffSpec()
	names := namesFor(hs.RunID)
	spec := buildSeederSpec(cfg, hs, names, testOwnershipLabel())

	if spec.Image != cfg.ExporterImage {
		t.Errorf("Image = %q, want the pinned exporter image %q", spec.Image, cfg.ExporterImage)
	}
	if !spec.NetworkDisabled {
		t.Error("seeder is not network-disabled")
	}
	if len(spec.Env) != 0 {
		t.Errorf("Env = %q, want none", spec.Env)
	}
	if len(spec.Mounts) != 1 {
		t.Fatalf("Mounts = %+v, want exactly the workspace", spec.Mounts)
	}
	m := spec.Mounts[0]
	if m.Type != MountVolume || m.Source != names.Workspace || m.Target != cfg.WorkspaceTarget {
		t.Errorf("mount = %+v, want the workspace volume at %q", m, cfg.WorkspaceTarget)
	}
	if m.ReadOnly {
		t.Error("seeder workspace mount is read-only; it must be read-write to place the checkout")
	}
	// The staging and sentinel destinations must stay outside the mount: a copy
	// aimed inside it is discarded silently by the reference runtime.
	script := seederScript(cfg)
	for _, inside := range []string{cfg.WorkspaceTarget + "/.git", cfg.WorkspaceTarget + "/seed"} {
		if strings.Contains(script, inside) {
			t.Errorf("seeder script references %q inside the workspace mount", inside)
		}
	}
	for _, want := range []string{cfg.SeedStageDir, cfg.SeedReadyDir, cfg.WorkspaceTarget} {
		if !strings.Contains(script, want) {
			t.Errorf("seeder script does not reference %q", want)
		}
	}
	// The guest's give-up budget must sit above the host bounds it races: both
	// staged copies are bounded at SeedTimeout each and both happen after the
	// seeder starts, so a guest deadline at or below one of them would abort a
	// large but legitimate copy.
	if got := seederGuestBudget(cfg.SeedTimeout); got <= 2*cfg.SeedTimeout {
		t.Errorf("seeder guest budget %s does not exceed the two host copy budgets (%s)", got, 2*cfg.SeedTimeout)
	}
}

func TestBuildAgentSpec(t *testing.T) {
	cfg := testConfig()
	hs := testHandoffSpec()
	names := namesFor(hs.RunID)
	spec := buildAgentSpec(cfg, hs, names, testOwnershipLabel())

	if spec.Name != names.Agent {
		t.Errorf("Name = %q, want %q", spec.Name, names.Agent)
	}
	if len(spec.Mounts) != 2 {
		t.Fatalf("len(Mounts) = %d, want 2", len(spec.Mounts))
	}
	ws := spec.Mounts[0]
	if ws.Source != names.Workspace || ws.Target != cfg.WorkspaceTarget || ws.ReadOnly {
		t.Errorf("workspace mount = %+v, want %q rw at %q", ws, names.Workspace, cfg.WorkspaceTarget)
	}
	cred := spec.Mounts[1]
	if cred.Source != "provider-cred" || cred.Target != "/credentials" || !cred.ReadOnly {
		t.Errorf("credential mount = %+v, want provider-cred ro at /credentials", cred)
	}
	// The generated spec passes its own gate.
	if err := validateAgentSpec(cfg, spec, names.Workspace); err != nil {
		t.Errorf("validateAgentSpec(generated) = %v, want nil", err)
	}
}
