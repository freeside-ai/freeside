package ward

import (
	"errors"
	"strings"
	"testing"
)

// TestValidateAgentSpecViolations induces every check 1/2 violation the spec
// vocabulary can express and asserts each fails closed with the right typed
// check (acceptance 2 for checks 1 and 2).
func TestValidateAgentSpecViolations(t *testing.T) {
	cfg := testConfig()
	hs := testHandoffSpec()
	names := namesFor(hs.RunID)

	cases := []struct {
		name      string
		mutate    func(*ContainerSpec)
		wantCheck Check
	}{
		{
			"unknown mount type",
			func(s *ContainerSpec) { s.Mounts[1].Type = "tmpfs" },
			CheckControlPlaneIsolation,
		},
		{
			"zero-value mount type",
			func(s *ContainerSpec) { s.Mounts[1].Type = "" },
			CheckControlPlaneIsolation,
		},
		{
			"host bind mount",
			func(s *ContainerSpec) {
				s.Mounts = append(s.Mounts, Mount{
					Type: MountBind, Source: "/Users/owner/.ssh", Target: "/ssh", ReadOnly: true,
				})
			},
			CheckControlPlaneIsolation,
		},
		{
			"host bind of the container CLI",
			func(s *ContainerSpec) {
				s.Mounts = append(s.Mounts, Mount{
					Type: MountBind, Source: "/usr/local/bin/container", Target: "/usr/local/bin/container",
				})
			},
			CheckControlPlaneIsolation,
		},
		{
			"relative mount target",
			func(s *ContainerSpec) { s.Mounts[1].Target = "credentials" },
			CheckCredentialSeparation,
		},
		{
			"root mount target",
			func(s *ContainerSpec) { s.Mounts[1].Target = "/" },
			CheckCredentialSeparation,
		},
		{
			"uncleaned mount target",
			func(s *ContainerSpec) { s.Mounts[1].Target = "/credentials/../etc" },
			CheckCredentialSeparation,
		},
		{
			"duplicate mount target",
			func(s *ContainerSpec) { s.Mounts[1].Target = s.Mounts[0].Target },
			CheckCredentialSeparation,
		},
		{
			"workspace mount wrong volume",
			func(s *ContainerSpec) { s.Mounts[0].Source = "other-volume" },
			CheckCredentialSeparation,
		},
		{
			"workspace mount read-only",
			func(s *ContainerSpec) { s.Mounts[0].ReadOnly = true },
			CheckCredentialSeparation,
		},
		{
			"credential inside workspace",
			func(s *ContainerSpec) { s.Mounts[1].Target = cfg.WorkspaceTarget + "/creds" },
			CheckCredentialSeparation,
		},
		{
			"credential reuses workspace volume",
			func(s *ContainerSpec) { s.Mounts[1].Source = names.Workspace },
			CheckCredentialSeparation,
		},
		{
			"credential mount read-write",
			func(s *ContainerSpec) { s.Mounts[1].ReadOnly = false },
			CheckCredentialSeparation,
		},
		{
			"no workspace mount",
			func(s *ContainerSpec) { s.Mounts = s.Mounts[1:] },
			CheckCredentialSeparation,
		},
		{
			"host-inheriting bare env key",
			func(s *ContainerSpec) { s.Env = append(s.Env, "GITHUB_TOKEN") },
			CheckControlPlaneIsolation,
		},
		{
			"empty env key",
			func(s *ContainerSpec) { s.Env = append(s.Env, "=orphan-value") },
			CheckControlPlaneIsolation,
		},
		{
			"comma injected into credential volume name",
			func(s *ContainerSpec) { s.Mounts[1].Source = "cred,readonly=false" },
			CheckCredentialSeparation,
		},
		{
			"comma injected into credential target",
			func(s *ContainerSpec) { s.Mounts[1].Target = "/creds,type=tmpfs" },
			CheckCredentialSeparation,
		},
		{
			"control character in mount target",
			func(s *ContainerSpec) { s.Mounts[1].Target = "/creds\nevil" },
			CheckCredentialSeparation,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := buildAgentSpec(cfg, hs, names, testOwnershipLabel())
			tc.mutate(&spec)
			err := validateAgentSpec(cfg, spec, names.Workspace)
			if !errors.Is(err, ErrConformance) {
				t.Fatalf("validateAgentSpec = %v, want ErrConformance", err)
			}
			var cf *ConformanceFailure
			if !errors.As(err, &cf) {
				t.Fatalf("error %v is not a *ConformanceFailure", err)
			}
			if cf.Check != tc.wantCheck {
				t.Errorf("Check = %q, want %q (reason: %s)", cf.Check, tc.wantCheck, cf.Reason)
			}
		})
	}
}

// TestValidateAgentSpecRedactsMalformedEnv proves the pre-create refusal
// never echoes an environment entry. A malformed bare entry is untrusted
// caller input and may itself be a credential copied into the list without
// a variable name.
func TestValidateAgentSpecRedactsMalformedEnv(t *testing.T) {
	cfg := testConfig()
	hs := testHandoffSpec()
	names := namesFor(hs.RunID)
	const secret = "secret-fixture-value"
	for _, entry := range []string{
		"github_pat_" + secret,
		"=" + secret,
	} {
		t.Run(redactPath(entry), func(t *testing.T) {
			spec := buildAgentSpec(cfg, hs, names, testOwnershipLabel())
			spec.Env = append(spec.Env, entry)
			err := validateAgentSpec(cfg, spec, names.Workspace)
			if !errors.Is(err, ErrConformance) {
				t.Fatalf("validateAgentSpec = %v, want ErrConformance", err)
			}
			if strings.Contains(err.Error(), entry) || strings.Contains(err.Error(), secret) {
				t.Errorf("failure reason leaked malformed environment entry: %v", err)
			}
		})
	}
}

// exporterReport is the runtime report matching the generated allowlist:
// the fixture the check-4 violation cases mutate.
func exporterReport(cfg Config, workspaceVolume string) InspectReport {
	return InspectReport{
		State: StateStopped,
		Mounts: []Mount{{
			Type:     MountVolume,
			Source:   workspaceVolume,
			Target:   cfg.WorkspaceTarget,
			ReadOnly: true,
		}},
		Env: []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
	}
}

// TestVerifyExporterAllowlistViolations induces every check 4 violation and
// asserts each fails closed with the exporter_allowlist check (acceptance 2
// for check 4).
func TestVerifyExporterAllowlistViolations(t *testing.T) {
	cfg := testConfig()
	const ws = "freeside-handoff-run-ws"

	if err := verifyExporterAllowlist(cfg, exporterReport(cfg, ws), ws); err != nil {
		t.Fatalf("conforming report: %v, want nil", err)
	}

	cases := []struct {
		name   string
		mutate func(*InspectReport)
	}{
		{"running before execution", func(r *InspectReport) { r.State = StateRunning }},
		{"unknown state before execution", func(r *InspectReport) { r.State = "" }},
		{"no mounts", func(r *InspectReport) { r.Mounts = nil }},
		{"extra credential mount", func(r *InspectReport) {
			r.Mounts = append(r.Mounts, Mount{Type: MountVolume, Source: "cred", Target: "/credentials", ReadOnly: true})
		}},
		{"host bind instead of volume", func(r *InspectReport) { r.Mounts[0].Type = MountBind }},
		{"unknown mount type", func(r *InspectReport) { r.Mounts[0].Type = "tmpfs" }},
		{"wrong volume", func(r *InspectReport) { r.Mounts[0].Source = "other-volume" }},
		{"wrong target", func(r *InspectReport) { r.Mounts[0].Target = "/data" }},
		{"read-write workspace", func(r *InspectReport) { r.Mounts[0].ReadOnly = false }},
		{"ssh forwarding", func(r *InspectReport) { r.SSH = true }},
		{"published socket", func(r *InspectReport) { r.PublishedSockets = []string{"/tmp/agent.sock"} }},
		{"published port", func(r *InspectReport) { r.PublishedPorts = []string{"8080"} }},
		{"environment credential", func(r *InspectReport) { r.Env = append(r.Env, "PROVIDER_TOKEN=inert-fixture") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := exporterReport(cfg, ws)
			tc.mutate(&rep)
			err := verifyExporterAllowlist(cfg, rep, ws)
			var cf *ConformanceFailure
			if !errors.As(err, &cf) || cf.Check != CheckExporterAllowlist {
				t.Fatalf("verifyExporterAllowlist = %v, want exporter_allowlist failure", err)
			}
		})
	}
}

// TestVerifyExporterAllowlistRedactsValues proves an environment violation
// reports the variable name, never its value: the value could itself be the
// credential the gate exists to contain.
func TestVerifyExporterAllowlistRedactsValues(t *testing.T) {
	cfg := testConfig()
	const ws = "freeside-handoff-run-ws"
	rep := exporterReport(cfg, ws)
	rep.Env = append(rep.Env, "PROVIDER_TOKEN=super-secret-fixture-value")
	err := verifyExporterAllowlist(cfg, rep, ws)
	if err == nil {
		t.Fatal("want failure")
	}
	if strings.Contains(err.Error(), "super-secret-fixture-value") {
		t.Errorf("failure reason leaks the environment value: %v", err)
	}
	if !strings.Contains(err.Error(), "PROVIDER_TOKEN") {
		t.Errorf("failure reason should name the variable: %v", err)
	}
}

func validProof() []byte {
	return []byte("workspace_mounted=ro\nworkspace_write=blocked\ncredentials=absent\nhost_home=absent\n")
}

// TestVerifyProof exercises check 5's strict proof contract (acceptance 2
// for check 5): every deviation from the exact required observations fails.
func TestVerifyProof(t *testing.T) {
	if err := verifyProof(validProof()); err != nil {
		t.Fatalf("valid proof: %v, want nil", err)
	}
	// Order-insensitive and CRLF-tolerant.
	shuffled := []byte("host_home=absent\r\nworkspace_write=blocked\nworkspace_mounted=ro\n\ncredentials=absent\n")
	if err := verifyProof(shuffled); err != nil {
		t.Fatalf("reordered proof: %v, want nil", err)
	}

	cases := []struct {
		name  string
		proof string
	}{
		{"empty", ""},
		{"missing key", "workspace_mounted=ro\nworkspace_write=blocked\ncredentials=absent\n"},
		{"wrong value", "workspace_mounted=rw\nworkspace_write=blocked\ncredentials=absent\nhost_home=absent\n"},
		{"credential present", "workspace_mounted=ro\nworkspace_write=blocked\ncredentials=present\nhost_home=absent\n"},
		{"write not blocked", "workspace_mounted=ro\nworkspace_write=succeeded\ncredentials=absent\nhost_home=absent\n"},
		{"unknown key", string(validProof()) + "extra=1\n"},
		{"duplicate key", string(validProof()) + "credentials=absent\n"},
		{"not key=value", "workspace_mounted\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyProof([]byte(tc.proof))
			var cf *ConformanceFailure
			if !errors.As(err, &cf) || cf.Check != CheckInExporterVerification {
				t.Fatalf("verifyProof = %v, want in_exporter_verification failure", err)
			}
		})
	}
}

func TestValidateAgentSpecNoCredentials(t *testing.T) {
	cfg := testConfig()
	hs := testHandoffSpec()
	hs.Agent.CredentialMounts = nil
	names := namesFor(hs.RunID)
	if err := validateAgentSpec(cfg, buildAgentSpec(cfg, hs, names, testOwnershipLabel()), names.Workspace); err != nil {
		t.Errorf("credential-free agent spec: %v, want nil", err)
	}
}
