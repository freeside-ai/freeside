package ward

import "strings"

// validateAgentSpec verifies the generated writer spec against checks 1 and
// 2. It runs on the spec the gate itself built: the gate re-verifies its own
// construction instead of trusting it, so a future spec-builder change that
// violates the contract fails here, not in review.
//
// Check 1 (credential_separation): the workspace is its own named volume,
// mounted read-write at exactly the configured target; every credential is a
// different named volume, read-only, at its own absolute target outside the
// workspace. The spec vocabulary cannot place a credential in the root
// filesystem image; a mount that tries (target "/", or a path under the
// workspace) is rejected here.
//
// Check 2 (control_plane_isolation): no host bind of any kind, so no host
// CLI, runtime socket, daemon state, SSH agent, home directory, or registry
// credential can reach the VM; ContainerSpec cannot express SSH forwarding
// or published sockets at all. Environment content is not scanned: a
// credential smuggled into Env is not mechanically detectable, and §5.4
// scanning of the export is the honest downstream control.
func validateAgentSpec(cfg Config, spec ContainerSpec, workspaceVolume string) error {
	// A bare-key env entry makes the CLI inherit the host's value, pulling a
	// host credential into the writer VM (check 2); every entry must set an
	// explicit value.
	for _, e := range spec.Env {
		if envInherits(e) {
			key, _, _ := strings.Cut(e, "=")
			return failf(CheckControlPlaneIsolation,
				"agent env %q would inherit a host value; every variable must be key=value", key)
		}
	}

	seenTargets := make(map[string]bool, len(spec.Mounts))
	workspaceMounts := 0
	for _, m := range spec.Mounts {
		if !m.Type.valid() {
			return failf(CheckControlPlaneIsolation,
				"agent mount %q has unknown type %q", m.Target, m.Type)
		}
		if m.Type == MountBind {
			return failf(CheckControlPlaneIsolation,
				"agent mount %q binds host path %q; host binds are never allowed", m.Target, m.Source)
		}
		// A comma or control character in a mount field would let the CLI
		// parse an injected mount option, realizing a topology this
		// spec-level check never approved (the agent is not re-inspected).
		if !cliSafe(m.Source) {
			return failf(CheckCredentialSeparation,
				"agent mount source for target %q carries a CLI mount-option delimiter", m.Target)
		}
		if !cliSafe(m.Target) {
			return failf(CheckCredentialSeparation,
				"agent mount target %q carries a CLI mount-option delimiter", m.Target)
		}
		if !cleanAbs(m.Target) {
			return failf(CheckCredentialSeparation,
				"agent mount target %q is not a clean absolute non-root path", m.Target)
		}
		if seenTargets[m.Target] {
			return failf(CheckCredentialSeparation,
				"agent mount target %q appears twice", m.Target)
		}
		seenTargets[m.Target] = true

		if m.Target == cfg.WorkspaceTarget {
			workspaceMounts++
			if m.Source != workspaceVolume {
				return failf(CheckCredentialSeparation,
					"workspace target %q mounts volume %q, want %q", m.Target, m.Source, workspaceVolume)
			}
			if m.ReadOnly {
				return failf(CheckCredentialSeparation,
					"workspace mount is read-only; the writer needs read-write")
			}
			continue
		}

		// Every non-workspace mount is a credential mount.
		if strings.HasPrefix(m.Target, cfg.WorkspaceTarget+"/") {
			return failf(CheckCredentialSeparation,
				"credential mount %q is inside the workspace %q", m.Target, cfg.WorkspaceTarget)
		}
		if m.Source == workspaceVolume {
			return failf(CheckCredentialSeparation,
				"credential mount %q reuses the workspace volume %q", m.Target, workspaceVolume)
		}
		if !m.ReadOnly {
			return failf(CheckCredentialSeparation,
				"credential mount %q is not read-only", m.Target)
		}
	}
	if workspaceMounts != 1 {
		return failf(CheckCredentialSeparation,
			"agent spec has %d workspace mounts at %q, want exactly 1", workspaceMounts, cfg.WorkspaceTarget)
	}
	return nil
}
