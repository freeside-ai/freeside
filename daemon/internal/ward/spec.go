package ward

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// BackendName is the backend's name in policy, refusals, and audit records:
// the isolation class the workspace-handoff spike proved on Apple container
// 1.1.0 and this backend realizes.
const BackendName = "fresh_vm_read_only_volume_handoff"

// labelKey marks every volume and container a handoff creates with its run
// ID, so teardown can prove nothing was left behind.
const labelKey = "freeside.handoff"

// ErrInvalidHandoffSpec is the class sentinel for a HandoffSpec the gate
// refuses to run at all; it is a caller error, not a conformance failure.
var ErrInvalidHandoffSpec = errors.New("invalid handoff spec")

// runIDPattern keeps run IDs safe as container and volume name segments.
var runIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// CredentialMount places one existing credential volume into the agent VM,
// read-only, at Target. The volume is caller-owned: the gate mounts it into
// the writer and proves it absent from everything downstream; it never
// creates or deletes it.
type CredentialMount struct {
	// Volume is the existing named volume holding the provider credential.
	Volume string
	// Target is the absolute mount path inside the agent VM.
	Target string
}

// AgentSpec describes the credential-bearing writer container.
type AgentSpec struct {
	Image   string
	Command []string
	Env     []string
	// CredentialMounts lists every provider credential the agent gets. Each
	// is its own mount, distinct from the workspace (spike check 1); the
	// spec vocabulary cannot express a credential inside the root filesystem
	// or workspace.
	CredentialMounts []CredentialMount
}

// HandoffSpec is one full handoff request: run the agent against a fresh
// workspace volume, prove its VM terminated, and export the workspace
// through the read-only exporter.
type HandoffSpec struct {
	// RunID names this run's volumes and containers; it must match
	// ^[a-z0-9][a-z0-9-]{0,31}$ and be unique among live runs.
	RunID string
	// WorkspaceSizeMB is the workspace volume size in megabytes.
	WorkspaceSizeMB int64
	// Agent is the writer container.
	Agent AgentSpec
}

// validate reports the first caller error in the spec. Mount-topology rules
// (checks 1 and 2) live in validateAgentSpec, not here: this is "can the
// gate even name things", not conformance.
func (s HandoffSpec) validate() error {
	switch {
	case !runIDPattern.MatchString(s.RunID):
		return fmt.Errorf("%w: RunID %q does not match %s", ErrInvalidHandoffSpec, s.RunID, runIDPattern)
	case s.WorkspaceSizeMB <= 0:
		return fmt.Errorf("%w: WorkspaceSizeMB %d is not positive", ErrInvalidHandoffSpec, s.WorkspaceSizeMB)
	case s.Agent.Image == "":
		return fmt.Errorf("%w: Agent.Image is required", ErrInvalidHandoffSpec)
	case len(s.Agent.Command) == 0:
		return fmt.Errorf("%w: Agent.Command is required", ErrInvalidHandoffSpec)
	}
	return nil
}

// handoffNames are the runtime object names one run owns.
type handoffNames struct {
	Workspace string
	Agent     string
	Exporter  string
}

func namesFor(runID string) handoffNames {
	return handoffNames{
		Workspace: "freeside-handoff-" + runID + "-ws",
		Agent:     "freeside-handoff-" + runID + "-agent",
		Exporter:  "freeside-handoff-" + runID + "-exporter",
	}
}

// runLabels label every object a run creates.
func runLabels(runID string) []Label {
	return []Label{{Key: labelKey, Value: runID}}
}

// buildAgentSpec generates the writer container: the workspace volume
// read-write at the configured target, every credential volume read-only at
// its own target, nothing else. validateAgentSpec re-verifies the result
// rather than trusting this construction.
func buildAgentSpec(cfg Config, hs HandoffSpec, names handoffNames) ContainerSpec {
	mounts := []Mount{{
		Type:   MountVolume,
		Source: names.Workspace,
		Target: cfg.WorkspaceTarget,
	}}
	for _, cm := range hs.Agent.CredentialMounts {
		mounts = append(mounts, Mount{
			Type:     MountVolume,
			Source:   cm.Volume,
			Target:   cm.Target,
			ReadOnly: true,
		})
	}
	return ContainerSpec{
		Name:    names.Agent,
		Image:   hs.Agent.Image,
		Command: hs.Agent.Command,
		Env:     hs.Agent.Env,
		Mounts:  mounts,
		Labels:  runLabels(hs.RunID),
	}
}

// buildExporterSpec generates the exporter container and, with it, check 4's
// mount allowlist: the pinned exporter image, the workspace volume read-only
// at the configured target, no environment, and nothing else.
func buildExporterSpec(cfg Config, hs HandoffSpec, names handoffNames) ContainerSpec {
	return ContainerSpec{
		Name:    names.Exporter,
		Image:   cfg.ExporterImage,
		Command: cfg.ExporterCommand,
		Mounts: []Mount{{
			Type:     MountVolume,
			Source:   names.Workspace,
			Target:   cfg.WorkspaceTarget,
			ReadOnly: true,
		}},
		Labels: runLabels(hs.RunID),
	}
}

// cleanAbs reports whether p is an absolute, cleaned, non-root path: the
// only shape a mount target may take.
func cleanAbs(p string) bool {
	return strings.HasPrefix(p, "/") && p != "/" && path.Clean(p) == p
}

// cliSafe reports whether s is safe to place inside a container CLI --mount
// value. A comma (the CLI's mount-option separator) or a control character
// would let the CLI parse a suffix as an additional option, so a realized
// mount could diverge from the validated spec; such a value is refused
// rather than escaped. The empty string is not safe (a mount field is
// always required).
func cliSafe(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r == ',' || r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// envInherits reports whether an environment entry would make the container
// CLI inherit the value from the host. `--env key=value` sets an explicit
// value; a bare `--env key` (no '=') tells the CLI to copy the host's value,
// which would pull a host credential into the VM (control-plane isolation
// breach). An empty key is equally rejected.
func envInherits(entry string) bool {
	k, _, ok := strings.Cut(entry, "=")
	return !ok || k == ""
}
