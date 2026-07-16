package ward

import (
	"errors"
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
			spec := buildAgentSpec(cfg, hs, names)
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

func TestValidateAgentSpecNoCredentials(t *testing.T) {
	cfg := testConfig()
	hs := testHandoffSpec()
	hs.Agent.CredentialMounts = nil
	names := namesFor(hs.RunID)
	if err := validateAgentSpec(cfg, buildAgentSpec(cfg, hs, names), names.Workspace); err != nil {
		t.Errorf("credential-free agent spec: %v, want nil", err)
	}
}
