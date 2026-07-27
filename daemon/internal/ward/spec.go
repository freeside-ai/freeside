package ward

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// BackendName is the backend's name in policy, refusals, and audit records:
// the isolation class the workspace-handoff spike proved on Apple container
// 1.1.0 and this backend realizes.
const BackendName = "fresh_vm_read_only_volume_handoff"

// labelKey marks every volume and container a handoff creates with its run
// ID for inspection. Teardown does not infer ownership from this label:
// caller-owned volumes may carry the same metadata.
const labelKey = "freeside.handoff"

// ownershipLabelKey marks every runtime object with an unpredictable token
// for one Handoff invocation. The token distinguishes an ambiguous create
// that made this invocation's object from an ordinary already-exists
// collision; unlike labelKey, it is ownership evidence for teardown.
const ownershipLabelKey = "freeside.handoff-owner"

// ErrInvalidHandoffSpec is the class sentinel for a HandoffSpec the gate
// refuses to run at all; it is a caller error, not a conformance failure.
var ErrInvalidHandoffSpec = errors.New("invalid handoff spec")

var (
	// runIDPattern keeps run IDs safe as container and volume name segments.
	runIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	// digestPinnedImagePattern binds an image reference to one full lowercase
	// sha256 digest, not a tag or a merely digest-shaped prefix.
	digestPinnedImagePattern = regexp.MustCompile(`^.+@sha256:[0-9a-f]{64}$`)
	// commitSHAPattern is the exact shape of a resolved git commit. The gate
	// compares a declared base against one observed in the seeded workspace
	// byte for byte, so the shape is pinned here even though
	// domain.BaseRevision only requires the field to be non-empty: an
	// abbreviated, uppercase, or ref-shaped value would compare unequal against
	// a full lowercase observation for reasons that have nothing to do with the
	// bases differing.
	commitSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// splitImageRef parses an OCI image reference into its name and its pinned
// @digest, normalizing away an optional tag so the two sides of an image
// comparison line up regardless of how the tag is spelled. ok is false when no
// @digest is present, so a caller comparing observed against expected fails
// closed on a tagless (unpinned) reference.
//
// The digest is the trust anchor: check 2/4 pin the image by digest, and Apple
// container 1.1.0 reports the pinned digest in configuration.image.reference
// while dropping the tag (a spec of "repo/name:3.22@sha256:PIN" is observed as
// "repo/name@sha256:PIN"). Stripping the tag from the name's final path segment
// lets those match on both name and digest.
func splitImageRef(ref string) (name, digest string, ok bool) {
	name, digest, ok = strings.Cut(ref, "@")
	if !ok {
		return "", "", false
	}
	// A tag is a ':' within the final path segment (the part after the last
	// '/'); a ':' before the last '/' is a registry port and is preserved
	// ("registry:5000/repo" keeps the port, strips a trailing ":tag"). A
	// reference with no '/' is a single "repo:tag" component ("alpine:3.22").
	// A bare "host:port" with no repository path is not a valid image
	// reference and is out of scope; the digest gate and symmetric parsing
	// mean mishandling it can never admit a wrong image regardless.
	segStart := strings.LastIndex(name, "/") + 1
	if colon := strings.LastIndex(name[segStart:], ":"); colon >= 0 {
		name = name[:segStart+colon]
	}
	return name, digest, true
}

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

// SeedMode is how a run's workspace volume is populated before the writer
// starts. The zero value is invalid by design: an absent mode would make an
// unseeded workspace the silent default, and a run whose base nobody declared
// is exactly the run §5.9's exact-base binding exists to prevent.
type SeedMode string

const (
	// SeedBlank leaves the workspace empty. It is safe only for a synthetic
	// caller that deliberately has no base; the full conformance suite uses
	// SeedBaseCheckout so it exercises the production seeder and observer.
	// A blank seed produces no observed base: domain.BaseRevision requires a
	// commit, so a caller comparing a declared base against a blank workspace's
	// empty observation can never match.
	SeedBlank SeedMode = "blank"
	// SeedBaseCheckout copies a daemon-owned checkout of the declared base into
	// the workspace before the writer starts.
	SeedBaseCheckout SeedMode = "base_checkout"
)

// AllSeedModes lists every valid SeedMode.
var AllSeedModes = []SeedMode{SeedBlank, SeedBaseCheckout}

func (m SeedMode) valid() bool {
	switch m {
	case SeedBlank, SeedBaseCheckout:
		return true
	default:
		return false
	}
}

// WorkspaceSeed declares what the workspace holds when the writer starts.
//
// Base is the caller's *declaration*, never evidence: the gate reads the base
// the seeded volume actually carries from a read-only observer VM and compares
// it against this value. A caller that declares one base and stages another is
// refused, which is the whole point of recording an observed identity rather
// than echoing the request (plan §5.9).
type WorkspaceSeed struct {
	Mode SeedMode
	// SourceDir is the daemon-owned checkout to stage, required for
	// SeedBaseCheckout and empty otherwise. It must resolve under
	// Config.SeedRoot and carry the daemon-authored canonical repository
	// name/ID binding; the gate never accepts an arbitrary host path.
	SourceDir string
	// Base is the exact revision SourceDir is declared to hold.
	Base domain.BaseRevision
}

// validate reports the first caller error in the seed declaration. Filesystem
// facts about SourceDir (existence, shape, size) are verified while
// stageSeedSource snapshots it; this is the syntactic gate that runs before
// anything is touched.
func (s WorkspaceSeed) validate() error {
	if !s.Mode.valid() {
		return fmt.Errorf("%w: Seed.Mode %q is not one of %v", ErrInvalidHandoffSpec, s.Mode, AllSeedModes)
	}
	if s.Mode == SeedBlank {
		// A blank seed carrying a source or a base is an ambiguous request: the
		// caller either meant to seed and named the wrong mode, or is passing a
		// base the gate will never verify. Both are refused rather than silently
		// resolved in one direction.
		if s.SourceDir != "" {
			return fmt.Errorf("%w: Seed.SourceDir is set on a %s seed", ErrInvalidHandoffSpec, SeedBlank)
		}
		if s.Base != (domain.BaseRevision{}) {
			return fmt.Errorf("%w: Seed.Base is set on a %s seed", ErrInvalidHandoffSpec, SeedBlank)
		}
		return nil
	}
	if !cleanAbs(s.SourceDir) {
		return fmt.Errorf("%w: Seed.SourceDir %q is not a clean absolute non-root path", ErrInvalidHandoffSpec, s.SourceDir)
	}
	if !copyPathSafe(s.SourceDir) {
		return fmt.Errorf("%w: Seed.SourceDir %q carries a container-reference delimiter", ErrInvalidHandoffSpec, s.SourceDir)
	}
	if err := s.Base.Validate(); err != nil {
		return fmt.Errorf("%w: Seed.Base: %w", ErrInvalidHandoffSpec, err)
	}
	if !commitSHAPattern.MatchString(s.Base.BaseSHA) {
		return fmt.Errorf("%w: Seed.Base.BaseSHA %q is not a full lowercase commit SHA", ErrInvalidHandoffSpec, s.Base.BaseSHA)
	}
	return nil
}

// HandoffSpec is one full handoff request: seed a fresh workspace volume at a
// declared exact base, run the agent against it, prove its VM terminated, and
// export the workspace through the read-only exporter.
type HandoffSpec struct {
	// RunID names this run's volumes and containers; it must match
	// ^[a-z0-9][a-z0-9-]{0,31}$ and be unique among live runs.
	RunID string
	// WorkspaceSizeMB is the workspace volume size in megabytes.
	WorkspaceSizeMB int64
	// Seed declares what the workspace holds when the writer starts. It is
	// required: see SeedMode on why absence is not a mode.
	Seed WorkspaceSeed
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
	case !digestPinnedImagePattern.MatchString(s.Agent.Image):
		return fmt.Errorf("%w: Agent.Image must be digest-pinned", ErrInvalidHandoffSpec)
	case len(s.Agent.Command) == 0:
		return fmt.Errorf("%w: Agent.Command is required", ErrInvalidHandoffSpec)
	}
	return s.Seed.validate()
}

// handoffNames are the runtime object names one run owns.
type handoffNames struct {
	Workspace string
	Seeder    string
	Observer  string
	Agent     string
	Exporter  string
}

func namesFor(runID string) handoffNames {
	return handoffNames{
		Workspace: "freeside-handoff-" + runID + "-ws",
		Seeder:    "freeside-handoff-" + runID + "-seeder",
		Observer:  "freeside-handoff-" + runID + "-observer",
		Agent:     "freeside-handoff-" + runID + "-agent",
		Exporter:  "freeside-handoff-" + runID + "-exporter",
	}
}

// WorkspaceRef is the ward lane's opaque workspace reference for a run: the
// name of the workspace volume the handoff creates and the exporter reads.
// exec.StartSpec.Workspace is declared opaque and ward-defined (plan §5.7);
// this function is that definition, so a caller can name the workspace without
// reconstructing the gate's naming scheme.
func WorkspaceRef(runID string) string { return namesFor(runID).Workspace }

// runLabels label every runtime object the gate creates.
func runLabels(runID string) []Label {
	return []Label{{Key: labelKey, Value: runID}}
}

// buildAgentSpec generates the writer container: the workspace volume
// read-write at the configured target, every credential volume read-only at
// its own target, nothing else. validateAgentSpec re-verifies the result
// rather than trusting this construction.
func buildAgentSpec(cfg Config, hs HandoffSpec, names handoffNames, ownershipLabel Label) ContainerSpec {
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
		Labels:  append(runLabels(hs.RunID), ownershipLabel),
	}
}

// buildExporterSpec generates the exporter container and, with it, check 4's
// mount allowlist: the pinned exporter image, the workspace volume read-only
// at the configured target, no environment, and nothing else.
func buildExporterSpec(cfg Config, hs HandoffSpec, names handoffNames, ownershipLabel Label) ContainerSpec {
	return ContainerSpec{
		Name:            names.Exporter,
		Image:           cfg.ExporterImage,
		Command:         cfg.ExporterCommand,
		NetworkDisabled: true,
		Mounts: []Mount{{
			Type:     MountVolume,
			Source:   names.Workspace,
			Target:   cfg.WorkspaceTarget,
			ReadOnly: true,
		}},
		Labels: append(runLabels(hs.RunID), ownershipLabel),
	}
}

// seedReadyFile is the sentinel the host copies, as its own second copy, once
// the staged checkout is complete. A marker written as part of the checkout
// copy would be unsound: a directory copy is not atomic, so the marker could
// become visible before the rest of the tree and the seeder would move a
// partial checkout onto the workspace.
const seedReadyFile = "ready"

// buildSeederSpec generates the container that puts the staged checkout onto
// the workspace volume: the pinned exporter image, the workspace read-write at
// the configured target, no environment, no network, and nothing else.
//
// It reuses the exporter image rather than introducing a second pinned image.
// The seeder needs a shell and coreutils and nothing more, the exporter image
// already provides both, and a second image would be a second supply-chain
// surface for no gain.
//
// The workspace is read-write here, which is the one place before the writer
// where it is. That is unavoidable: the runtime refuses to copy into a
// container that is not running and silently discards a copy aimed at a
// mounted volume, so something inside a VM has to move the tree. Keeping that
// something a fixed, gate-authored command in a pinned image is the bound; the
// observer, which mounts the workspace read-only in a different VM, is the
// proof.
func buildSeederSpec(cfg Config, hs HandoffSpec, names handoffNames, ownershipLabel Label) ContainerSpec {
	return ContainerSpec{
		Name:            names.Seeder,
		Image:           cfg.ExporterImage,
		Command:         seederCommand(cfg),
		NetworkDisabled: true,
		Mounts: []Mount{{
			Type:   MountVolume,
			Source: names.Workspace,
			Target: cfg.WorkspaceTarget,
		}},
		Labels: append(runLabels(hs.RunID), ownershipLabel),
	}
}

func seederCommand(cfg Config) []string {
	return []string{"sh", "-c", seederScript(cfg)}
}

// seederScript waits for the host to signal that the staged checkout is
// complete, then moves it onto the workspace volume and exits.
//
// The wait is bounded in the guest as well as by the host's own stop timeout,
// so a seeder whose host died cannot spin forever holding the workspace
// read-write. The distinct exit codes are for a human reading a stuck run's
// state; the gate never interprets them, because the seeder's exit status is
// its own account of itself. What the gate believes is what the observer reads
// off the volume afterwards.
func seederScript(cfg Config) string {
	ready := shellQuote(path.Join(cfg.SeedReadyDir, seedReadyFile))
	stage := shellQuote(cfg.SeedStageDir)
	ws := shellQuote(cfg.WorkspaceTarget)
	ticks := int(cfg.SeedTimeout / time.Second)
	if ticks < 1 {
		ticks = 1
	}
	return "set -eu; i=0; " +
		"while [ ! -f " + ready + " ]; do " +
		"i=$((i+1)); if [ \"$i\" -gt " + strconv.Itoa(ticks) + " ]; then exit 91; fi; " +
		"sleep 1; done; " +
		// The staged tree is a checkout or it is nothing: refusing here keeps a
		// half-copied or wrong-shaped stage from being merged onto the volume,
		// where the observer would then have to distinguish it from a seed that
		// never happened.
		"if [ ! -d " + stage + "/.git ]; then exit 92; fi; " +
		"cp -a " + stage + "/. " + ws + "/; sync"
}

// cloneContainerSpec detaches every reference field before a spec crosses the
// Runtime boundary. Runtime implementations may normalize or retain their
// input; neither may rewrite the immutable expected spec used by the gate's
// post-create allowlist comparison.
func cloneContainerSpec(spec ContainerSpec) ContainerSpec {
	spec.Command = slices.Clone(spec.Command)
	spec.Env = slices.Clone(spec.Env)
	spec.Mounts = slices.Clone(spec.Mounts)
	spec.Labels = slices.Clone(spec.Labels)
	return spec
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

// copyPathSafe reports whether p is safe to place in a `container copy`
// argument. The CLI addresses a container-side path as <container>:<path>, so
// a ':' anywhere in either argument could reparse the argument into a
// different container's path; control characters are refused for the same
// reason cliSafe refuses them. Such a value is refused, never escaped. The
// empty string is not safe (both copy arguments are always required).
func copyPathSafe(p string) bool {
	if p == "" {
		return false
	}
	for _, r := range p {
		if r == ':' || r < 0x20 || r == 0x7f {
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
