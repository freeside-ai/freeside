package ward

import (
	"bufio"
	"bytes"
	"fmt"
	"sort"
	"strings"
)

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

// verifyExporterAllowlist is check 4: the exporter's runtime-observed
// configuration, inspected before it ever executes, must match the generated
// allowlist exactly — one persistent mount (the expected workspace volume,
// read-only, at the expected target), no credential volume, no host bind, no
// SSH forwarding, no environment beyond the image PATH, and no published
// socket or port. It verifies the runtime's report, not the gate's own spec:
// what the VM would actually get, not what was asked for.
func verifyExporterAllowlist(cfg Config, rep InspectReport, workspaceVolume string) error {
	// Inspect-before-execution: the report must describe a container that has
	// not run. On the reference runtime a created-not-started container is
	// stopped; any other observed state means the trusted payload may already
	// have executed before the gate approved its configuration.
	if rep.State != StateStopped {
		return failf(CheckExporterAllowlist,
			"exporter observed in state %q before execution, want %q", rep.State, StateStopped)
	}
	if n := len(rep.Mounts); n != 1 {
		return failf(CheckExporterAllowlist,
			"exporter has %d persistent mounts, want exactly 1 (%s)", n, describeMounts(rep.Mounts))
	}
	m := rep.Mounts[0]
	switch {
	case m.Type != MountVolume:
		return failf(CheckExporterAllowlist,
			"exporter mount %q has type %q, want %q", m.Target, m.Type, MountVolume)
	case m.Source != workspaceVolume:
		return failf(CheckExporterAllowlist,
			"exporter mounts volume %q, want %q", m.Source, workspaceVolume)
	case m.Target != cfg.WorkspaceTarget:
		return failf(CheckExporterAllowlist,
			"exporter mounts workspace at %q, want %q", m.Target, cfg.WorkspaceTarget)
	case !m.ReadOnly:
		return failf(CheckExporterAllowlist,
			"exporter workspace mount at %q is not read-only", m.Target)
	}
	if rep.SSH {
		return failf(CheckExporterAllowlist, "exporter has SSH forwarding configured")
	}
	if n := len(rep.PublishedSockets); n > 0 {
		return failf(CheckExporterAllowlist, "exporter publishes %d sockets, want 0", n)
	}
	if n := len(rep.PublishedPorts); n > 0 {
		return failf(CheckExporterAllowlist, "exporter publishes %d ports, want 0", n)
	}
	for _, e := range rep.Env {
		key, _, _ := strings.Cut(e, "=")
		// Key only: an unexpected value could itself be a credential and
		// must never travel in a failure reason.
		if key != "PATH" {
			return failf(CheckExporterAllowlist,
				"exporter environment carries variable %q, want image PATH only", key)
		}
	}
	return nil
}

// describeMounts renders mount targets and types (never sources: a bind
// source is a host path and stays out of failure reasons).
func describeMounts(mounts []Mount) string {
	parts := make([]string, len(mounts))
	for i, m := range mounts {
		parts[i] = fmt.Sprintf("%s:%s", m.Type, m.Target)
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

// requiredProof is check 5's contract with the exporter payload: the probes
// run inside the exporter VM and their observations land as exactly these
// key=value lines in the proof file. Values are inert markers, safe to echo
// in failure reasons.
var requiredProof = map[string]string{
	// /proc/mounts reports the workspace mounted read-only.
	"workspace_mounted": "ro",
	// A write probe into the workspace failed.
	"workspace_write": "blocked",
	// The expected credential path does not exist in the exporter.
	"credentials": "absent",
	// No host home directory is visible in the exporter.
	"host_home": "absent",
}

// verifyProof is check 5: the proof file collected from the exported rootfs
// must contain exactly the required observations — every required key, the
// required value, no duplicates, no unknown keys, nothing else. Any
// deviation, including a missing or empty file, is a conformance failure;
// the exporter's exit status is not consulted because a stopped container
// exports regardless of how its payload exited.
func verifyProof(data []byte) error {
	seen := make(map[string]bool, len(requiredProof))
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return failf(CheckInExporterVerification, "proof line %q is not key=value", line)
		}
		want, known := requiredProof[key]
		if !known {
			return failf(CheckInExporterVerification, "proof carries unknown key %q", key)
		}
		if seen[key] {
			return failf(CheckInExporterVerification, "proof repeats key %q", key)
		}
		seen[key] = true
		if value != want {
			return failf(CheckInExporterVerification,
				"proof reports %s=%s, want %s=%s", key, value, key, want)
		}
	}
	if err := sc.Err(); err != nil {
		return failf(CheckInExporterVerification, "proof unreadable: %v", err)
	}
	keys := make([]string, 0, len(requiredProof))
	for key := range requiredProof {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !seen[key] {
			return failf(CheckInExporterVerification, "proof is missing key %q", key)
		}
	}
	return nil
}
