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

func agentReport(spec ContainerSpec) InspectReport {
	return InspectReport{
		ID: spec.Name,
		// The runtime reports the full pinned reference (name@digest), as
		// Apple container 1.1.0 does; the verifier parses it back out.
		ImageReference:          spec.Image,
		Command:                 append([]string(nil), spec.Command...),
		WorkingDirectory:        "/",
		State:                   StateStopped,
		AllowlistFieldsObserved: true,
		Mounts:                  append([]Mount(nil), spec.Mounts...),
		Env:                     append([]string{fixedContainerPathEnv}, spec.Env...),
	}
}

func TestVerifyAgentAllowlistViolations(t *testing.T) {
	cfg := testConfig()
	hs := testHandoffSpec()
	names := namesFor(hs.RunID)
	spec := buildAgentSpec(cfg, hs, names, testOwnershipLabel())
	if err := verifyAgentAllowlist(agentReport(spec), spec); err != nil {
		t.Fatalf("conforming report: %v, want nil", err)
	}
	agentImageName, agentImageDigest, _ := strings.Cut(spec.Image, "@")

	// A tag on the observed reference is normalized away: the same name and
	// pinned digest still conform, so a spec pinned by tag+digest matches the
	// runtime's tag-dropped reference (acceptance #2).
	tagged := agentReport(spec)
	tagged.ImageReference = agentImageName + ":v1@" + agentImageDigest
	if err := verifyAgentAllowlist(tagged, spec); err != nil {
		t.Fatalf("tag-normalized reference: %v, want nil", err)
	}

	cases := []struct {
		name   string
		mutate func(*InspectReport)
	}{
		{"wrong identity", func(r *InspectReport) { r.ID = "other-agent" }},
		{"required field omitted", func(r *InspectReport) { r.AllowlistFieldsObserved = false }},
		{"running before approval", func(r *InspectReport) { r.State = StateRunning }},
		{"unknown state", func(r *InspectReport) { r.State = "unknown" }},
		{"wrong image name", func(r *InspectReport) {
			r.ImageReference = "example.test/other@" + agentImageDigest
		}},
		{"wrong image digest", func(r *InspectReport) {
			r.ImageReference = agentImageName + "@sha256:" + strings.Repeat("2", 64)
		}},
		{"reference missing digest", func(r *InspectReport) { r.ImageReference = agentImageName }},
		{"workspace working directory", func(r *InspectReport) { r.WorkingDirectory = "/workspace" }},
		{"wrong command", func(r *InspectReport) { r.Command = append(r.Command, "--drift") }},
		{"different environment", func(r *InspectReport) { r.Env = append(r.Env, "HOST_TOKEN=inert") }},
		{"extra host bind", func(r *InspectReport) {
			r.Mounts = append(r.Mounts, Mount{Type: MountBind, Source: "/", Target: "/host"})
		}},
		{"missing credential mount", func(r *InspectReport) { r.Mounts = r.Mounts[:1] }},
		{"credential mount access conflict", func(r *InspectReport) { r.Mounts[1].AccessConflict = true }},
		{"ssh forwarding", func(r *InspectReport) { r.SSH = true }},
		{"published socket", func(r *InspectReport) { r.PublishedSockets = []string{"/tmp/agent.sock"} }},
		{"published port", func(r *InspectReport) { r.PublishedPorts = []string{"8080"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := agentReport(spec)
			tc.mutate(&rep)
			if err := verifyAgentAllowlist(rep, spec); !errors.Is(err, ErrConformance) {
				t.Fatalf("verifyAgentAllowlist = %v, want ErrConformance", err)
			}
		})
	}
}

// exporterReport is the runtime report matching the generated allowlist:
// the fixture the check-4 violation cases mutate.
func exporterReport(cfg Config, exporterID, workspaceVolume string) InspectReport {
	return InspectReport{
		ID: exporterID,
		// The runtime reports the full pinned reference (name@digest).
		ImageReference:          cfg.ExporterImage,
		Command:                 append([]string(nil), cfg.ExporterCommand...),
		WorkingDirectory:        "/",
		State:                   StateStopped,
		AllowlistFieldsObserved: true,
		NetworksObserved:        true,
		Mounts: []Mount{{
			Type:     MountVolume,
			Source:   workspaceVolume,
			Target:   cfg.WorkspaceTarget,
			ReadOnly: true,
		}},
		Env: []string{fixedContainerPathEnv},
	}
}

// TestVerifyExporterAllowlistViolations induces every check 4 violation and
// asserts each fails closed with the exporter_allowlist check (acceptance 2
// for check 4).
func TestVerifyExporterAllowlistViolations(t *testing.T) {
	cfg := testConfig()
	const exporter = "freeside-handoff-run-exporter"
	const ws = "freeside-handoff-run-ws"

	if err := verifyExporterAllowlist(cfg, exporterReport(cfg, exporter, ws), exporter, ws); err != nil {
		t.Fatalf("conforming report: %v, want nil", err)
	}
	exporterImageName, exporterImageDigest, _ := strings.Cut(cfg.ExporterImage, "@")

	// A tag on the observed reference is normalized away (acceptance #2).
	tagged := exporterReport(cfg, exporter, ws)
	tagged.ImageReference = exporterImageName + ":v1@" + exporterImageDigest
	if err := verifyExporterAllowlist(cfg, tagged, exporter, ws); err != nil {
		t.Fatalf("tag-normalized reference: %v, want nil", err)
	}

	cases := []struct {
		name   string
		mutate func(*InspectReport)
	}{
		{"wrong identity", func(r *InspectReport) { r.ID = "other-exporter" }},
		{"required field omitted", func(r *InspectReport) { r.AllowlistFieldsObserved = false }},
		{"wrong image name", func(r *InspectReport) {
			r.ImageReference = "example.test/other@" + exporterImageDigest
		}},
		{"wrong image digest", func(r *InspectReport) {
			r.ImageReference = exporterImageName + "@sha256:" + strings.Repeat("1", 64)
		}},
		{"reference missing digest", func(r *InspectReport) { r.ImageReference = exporterImageName }},
		{"wrong executable", func(r *InspectReport) { r.Command[0] = "/bin/other" }},
		{"workspace working directory", func(r *InspectReport) { r.WorkingDirectory = "/workspace" }},
		{"extra argument", func(r *InspectReport) { r.Command = append(r.Command, "--other") }},
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
		{"contradictory workspace access", func(r *InspectReport) { r.Mounts[0].AccessConflict = true }},
		{"ssh forwarding", func(r *InspectReport) { r.SSH = true }},
		{"published socket", func(r *InspectReport) { r.PublishedSockets = []string{"/tmp/agent.sock"} }},
		{"published port", func(r *InspectReport) { r.PublishedPorts = []string{"8080"} }},
		{"networks omitted", func(r *InspectReport) { r.NetworksObserved = false }},
		{"network attached", func(r *InspectReport) { r.NetworkAttachmentCount = 1 }},
		{"environment credential", func(r *InspectReport) { r.Env = append(r.Env, "PROVIDER_TOKEN=inert-fixture") }},
		{"workspace in PATH", func(r *InspectReport) { r.Env = []string{"PATH=/workspace:/usr/bin:/bin"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := exporterReport(cfg, exporter, ws)
			tc.mutate(&rep)
			err := verifyExporterAllowlist(cfg, rep, exporter, ws)
			var cf *ConformanceFailure
			if !errors.As(err, &cf) || cf.Check != CheckExporterAllowlist {
				t.Fatalf("verifyExporterAllowlist = %v, want exporter_allowlist failure", err)
			}
		})
	}
}

// TestVerifyExporterAllowlistRedactsValues proves an environment violation
// reports no runtime-derived key or value: either could itself be a
// credential the gate exists to contain.
func TestVerifyExporterAllowlistRedactsValues(t *testing.T) {
	cfg := testConfig()
	const exporter = "freeside-handoff-run-exporter"
	const ws = "freeside-handoff-run-ws"
	rep := exporterReport(cfg, exporter, ws)
	rep.Env = append(rep.Env, "PROVIDER_TOKEN=super-secret-fixture-value")
	err := verifyExporterAllowlist(cfg, rep, exporter, ws)
	if err == nil {
		t.Fatal("want failure")
	}
	if strings.Contains(err.Error(), "super-secret-fixture-value") {
		t.Errorf("failure reason leaks the environment value: %v", err)
	}
	if strings.Contains(err.Error(), "PROVIDER_TOKEN") {
		t.Errorf("failure reason leaks the environment key: %v", err)
	}
}

// TestConformanceReasonsRedactUntrustedFields pins the same invariant for
// caller-built agent mounts and runtime-observed exporter fields.
func TestConformanceReasonsRedactUntrustedFields(t *testing.T) {
	const secret = "secret-field-fixture-value"
	cfg := testConfig()
	hs := testHandoffSpec()
	names := namesFor(hs.RunID)
	agent := buildAgentSpec(cfg, hs, names, testOwnershipLabel())
	agent.Mounts[1].Target = secret
	if err := validateAgentSpec(cfg, agent, names.Workspace); err == nil || strings.Contains(err.Error(), secret) {
		t.Errorf("agent conformance failure leaked or accepted an untrusted field: %v", err)
	}
	rep := exporterReport(cfg, names.Exporter, names.Workspace)
	rep.Mounts[0].Source = secret
	if err := verifyExporterAllowlist(cfg, rep, names.Exporter, names.Workspace); err == nil || strings.Contains(err.Error(), secret) {
		t.Errorf("exporter conformance failure leaked or accepted an untrusted field: %v", err)
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

// TestVerifyProofRedactsArchiveContent proves every parser category keeps
// unscanned proof bytes out of the returned conformance reason.
func TestVerifyProofRedactsArchiveContent(t *testing.T) {
	const secret = "secret-proof-fixture-value"
	cases := map[string]string{
		"malformed line": secret + "\n",
		"unknown key":    secret + "=value\n",
		"wrong value":    "credentials=" + secret + "\n",
	}
	for name, proof := range cases {
		t.Run(name, func(t *testing.T) {
			err := verifyProof([]byte(proof))
			if !errors.Is(err, ErrConformance) {
				t.Fatalf("verifyProof = %v, want ErrConformance", err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("proof failure leaked archive content: %v", err)
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
