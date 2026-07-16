package ward

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

// scannerFunc adapts a function to OutputScanner for test fixtures.
type scannerFunc func(ctx context.Context, dir string) error

func (f scannerFunc) Scan(ctx context.Context, dir string) error { return f(ctx, dir) }

// testConfig is the fixed valid fixture configuration; tests copy and mutate
// it. The all-zero digest marks it as inert fixture data.
func testConfig() Config {
	return Config{
		ExporterImage: "example.test/exporter@sha256:" + strings.Repeat("0", 64),
		ExporterCommand: []string{
			"/usr/local/bin/freeside-export",
			"-workspace", "/workspace",
			"-out", "/handoff",
		},
		Scanner: scannerFunc(func(context.Context, string) error { return nil }),
	}.withDefaults()
}

func testHandoffSpec() HandoffSpec {
	return HandoffSpec{
		RunID:           "golden-run",
		WorkspaceSizeMB: 64,
		Agent: AgentSpec{
			Image:   "example.test/agent:dev",
			Command: []string{"sh", "-c", "true"},
			Env:     []string{"AGENT_MODE=fixture"},
			CredentialMounts: []CredentialMount{
				{Volume: "provider-cred", Target: "/credentials"},
			},
		},
	}
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
		{"missing agent command", func(s *HandoffSpec) { s.Agent.Command = nil }},
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
		Agent:     "freeside-handoff-run-1-agent",
		Exporter:  "freeside-handoff-run-1-exporter",
	}
	if n != want {
		t.Errorf("namesFor(run-1) = %+v, want %+v", n, want)
	}
}

// TestExporterSpecGolden pins check 4's generated allowlist: the exporter
// spec is the security contract the pre-execution inspection verifies
// against, so a drift in its shape must be a reviewed diff.
func TestExporterSpecGolden(t *testing.T) {
	cfg := testConfig()
	hs := testHandoffSpec()
	spec := buildExporterSpec(cfg, hs, namesFor(hs.RunID))
	got, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		t.Fatalf("marshal exporter spec: %v", err)
	}
	golden.Assert(t, "exporter-spec", append(got, '\n'))
}

func TestBuildAgentSpec(t *testing.T) {
	cfg := testConfig()
	hs := testHandoffSpec()
	names := namesFor(hs.RunID)
	spec := buildAgentSpec(cfg, hs, names)

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
