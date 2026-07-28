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
// 1.1.0 and this backend realizes. It derives from the domain's registered
// class so the name the store gates conformance records under (#320) and the
// name this backend admits under cannot drift.
const BackendName = string(domain.BackendFreshVMReadOnlyVolumeHandoff)

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
	// sha256HexPattern is the exact shape of the credential observer's tree
	// digest: proof content is unscanned archive output, so the digest is
	// shape-checked before anything compares or reports it.
	sha256HexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
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

// CredentialMount places one existing credential volume into the agent VM at
// Target: read-only unless Writable. The volume is caller-owned: the gate
// mounts it into the writer and proves it absent from everything downstream;
// it never creates or deletes it.
type CredentialMount struct {
	// Volume is the existing named volume holding the provider credential.
	Volume string
	// Target is the absolute mount path inside the agent VM.
	Target string
	// Writable mounts the volume read-write so the contained vendor CLI can
	// mutate its own auth store (§5.4: refresh, login state, configuration
	// writes, store replacement). It is valid only on the single mount the
	// spec's AuthStoreLease covers; every other credential mount stays
	// read-only, and a writable mount without the lease claim is refused.
	Writable bool
}

// AgentSpec describes the credential-bearing writer container.
type AgentSpec struct {
	Image         string
	Command       []string
	Env           []string
	EgressProfile domain.EgressProfile
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

// AuthStoreLeaseClaim names the identity whose auth-store mutation window
// this handoff runs inside, and the holder the gate acquires the lease as.
// The claim is a request, not evidence: the gate acquires and verifies the
// per-identity domain.AuthStoreMutationLease itself before the writer can
// start, so a caller cannot satisfy §5.4's serialization by asserting it.
type AuthStoreLeaseClaim struct {
	// AuthIdentityID is the identity whose auth store the writable mount
	// carries. The identity must declare auth_store_mutation_lease; the
	// store refuses acquisition for one that does not.
	AuthIdentityID domain.AuthIdentityID
	// Holder is the lease holder recorded for this run, so an abandoned
	// window can be traced back to what abandoned it (§5.4 does not bind it
	// to a recorded agent invocation; see domain.AuthStoreMutationLease).
	Holder domain.InvocationID
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
	// AuthStoreLease is required exactly when one credential mount is
	// Writable: the mutation window the writable mount rides in. Nil means
	// every credential mount is read-only.
	AuthStoreLease *AuthStoreLeaseClaim
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
	case s.Agent.EgressProfile != domain.EgressProviderOnly:
		return fmt.Errorf("%w: Agent.EgressProfile %q is not enforceable by this backend", ErrInvalidHandoffSpec, s.Agent.EgressProfile)
	}
	// A writable credential mount and the lease claim travel together, both
	// ways. A writable mount without the claim would mutate an auth store
	// outside any §5.4 window; a claim without a writable mount is an
	// ambiguous request (the caller either meant to mark a mount writable or
	// is reserving a window nothing uses), refused rather than silently
	// resolved in one direction — the same posture as a blank seed carrying
	// a source. One mount per claim: the lease serializes one identity's
	// store, so a second writable volume would ride a window that does not
	// cover it.
	writable := 0
	for _, cm := range s.Agent.CredentialMounts {
		if cm.Writable {
			writable++
		}
	}
	if s.AuthStoreLease == nil {
		if writable != 0 {
			return fmt.Errorf("%w: a writable credential mount requires AuthStoreLease", ErrInvalidHandoffSpec)
		}
		return s.Seed.validate()
	}
	if s.AuthStoreLease.AuthIdentityID == "" {
		return fmt.Errorf("%w: AuthStoreLease.AuthIdentityID is required", ErrInvalidHandoffSpec)
	}
	if s.AuthStoreLease.Holder == "" {
		return fmt.Errorf("%w: AuthStoreLease.Holder is required", ErrInvalidHandoffSpec)
	}
	if writable != 1 {
		return fmt.Errorf("%w: AuthStoreLease requires exactly one writable credential mount, got %d",
			ErrInvalidHandoffSpec, writable)
	}
	return s.Seed.validate()
}

// writableCredentialTarget is the target of the one leased writable
// credential mount, or "" when every credential mount is read-only. It is
// meaningful only on a spec validate() accepted, which ties the writable
// mount to the lease claim in both directions.
func (s HandoffSpec) writableCredentialTarget() string {
	if s.AuthStoreLease == nil {
		return ""
	}
	for _, cm := range s.Agent.CredentialMounts {
		if cm.Writable {
			return cm.Target
		}
	}
	return ""
}

// writableCredentialVolume is the runtime volume of the one leased writable
// credential mount. The trusted identity binding is compared with this value
// before the mutation window is opened.
func (s HandoffSpec) writableCredentialVolume() string {
	if s.AuthStoreLease == nil {
		return ""
	}
	for _, cm := range s.Agent.CredentialMounts {
		if cm.Writable {
			return cm.Volume
		}
	}
	return ""
}

// handoffNames are the runtime object names one run owns.
type handoffNames struct {
	Workspace   string
	Seeder      string
	Observer    string
	CredObsPre  string
	CredObsPost string
	Agent       string
	Exporter    string
	Network     string
}

func namesFor(runID string) handoffNames {
	return handoffNames{
		Workspace:   "freeside-handoff-" + runID + "-ws",
		Seeder:      "freeside-handoff-" + runID + "-seeder",
		Observer:    "freeside-handoff-" + runID + "-observer",
		CredObsPre:  "freeside-handoff-" + runID + "-cred-pre",
		CredObsPost: "freeside-handoff-" + runID + "-cred-post",
		Agent:       "freeside-handoff-" + runID + "-agent",
		Exporter:    "freeside-handoff-" + runID + "-exporter",
		Network:     "freeside-handoff-" + runID + "-egress",
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
// read-write at the configured target, every credential volume at its own
// target — read-only except the single leased writable mount, when the spec
// declares one — nothing else. validateAgentSpec re-verifies the result
// rather than trusting this construction.
func buildAgentSpec(cfg Config, hs HandoffSpec, names handoffNames, ownershipLabel Label, proxyURL string) ContainerSpec {
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
			ReadOnly: !cm.Writable,
		})
	}
	return ContainerSpec{
		Name:    names.Agent,
		Image:   hs.Agent.Image,
		Command: hs.Agent.Command,
		Env:     append(slices.Clone(hs.Agent.Env), proxyEnvironment(proxyURL)...),
		Mounts:  mounts,
		Labels:  append(runLabels(hs.RunID), ownershipLabel),
		Network: names.Network,
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

// seederGuestBudget is how long the seeder waits for the completion sentinel
// before giving up. It covers both host copies (SeedTimeout each) plus a
// margin, so the guest never fires first; the host's waitStopped is the real
// deadline.
func seederGuestBudget(seedTimeout time.Duration) time.Duration {
	return 3 * seedTimeout
}

// seederScriptTicks converts the guest budget into whole `sleep 1` iterations,
// rounding UP. The loop can only count seconds, so truncating would undo the
// budget's purpose at subsecond timeouts: a 600ms SeedTimeout gives a 1.8s
// budget that truncates to one tick, and the seeder would give up after about
// a second while the two host copies may legitimately take 1.2s — reinstating
// the sentinel-copy race the budget exists to prevent.
func seederScriptTicks(cfg Config) int {
	ticks := int((seederGuestBudget(cfg.SeedTimeout) + time.Second - 1) / time.Second)
	if ticks < 1 {
		ticks = 1
	}
	return ticks
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
//
// The guest budget deliberately exceeds one SeedTimeout. Both host copies are
// bounded at SeedTimeout each and both happen after the seeder starts, so a
// guest budget equal to one of them would let a large but legitimate staged
// copy race the seeder's own exit: the seeder would give up, and the sentinel
// copy would then fail against a stopped container. Sizing the backstop above
// the host bounds it is racing keeps it a backstop rather than a second,
// tighter deadline nobody declared.
func seederScript(cfg Config) string {
	ready := shellQuote(path.Join(cfg.SeedReadyDir, seedReadyFile))
	stage := shellQuote(cfg.SeedStageDir)
	ws := shellQuote(cfg.WorkspaceTarget)
	ticks := seederScriptTicks(cfg)
	return "set -eu; i=0; " +
		"while [ ! -f " + ready + " ]; do " +
		"i=$((i+1)); if [ \"$i\" -gt " + strconv.Itoa(ticks) + " ]; then exit 91; fi; " +
		"sleep 1; done; " +
		// The staged tree is a checkout or it is nothing: refusing here keeps a
		// half-copied or wrong-shaped stage from being merged onto the volume,
		// where the observer would then have to distinguish it from a seed that
		// never happened.
		"if [ ! -d " + stage + "/.git ]; then exit 92; fi; " +
		// Clear the filesystem's own lost+found before staging, so the workspace
		// afterwards holds exactly the source tree and nothing the volume added.
		// The observer can then digest the whole workspace instead of pruning a
		// path by name, which would otherwise make a repository that genuinely
		// tracks a root-level lost+found undigestable on one side and digested
		// on the other -- a mismatch no honest seed could ever clear.
		"rm -rf " + ws + "/" + lostFoundDir + "; " +
		"cp -a " + stage + "/. " + ws + "/; sync"
}

// buildObserverSpec generates the container that attests what the workspace
// actually holds: the same pinned image and the same credential-free,
// network-free position as the seeder, but the workspace mounted READ-ONLY.
//
// It is a separate VM from the seeder on purpose. The seeder holds the
// workspace read-write and is the thing that placed the tree; an observation
// taken through that same handle would be the writer vouching for its own
// write. This container did not place anything, cannot write anything, and
// reads the workspace through the same access class the exporter later uses,
// so what it attests is the workspace as a reader sees it.
func buildObserverSpec(cfg Config, hs HandoffSpec, names handoffNames, ownershipLabel Label) ContainerSpec {
	return ContainerSpec{
		Name:            names.Observer,
		Image:           cfg.ExporterImage,
		Command:         observerCommand(cfg, ownershipLabel.Value),
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

func observerCommand(cfg Config, nonce string) []string {
	return []string{"sh", "-c", observerScript(cfg, nonce)}
}

// Proof keys the observer emits. They are the gate's contract with its own
// pinned image, in the same shape check 5's proof uses.
const (
	baseProofNonceKey    = "nonce"
	baseProofGitDirKey   = "git_dir"
	baseProofDetachedKey = "head_detached"
	baseProofSHAKey      = "base_sha"
	baseProofTreeKey     = "tree_sha256"
	// baseProofWorktreeKey reports whether raw worktree paths, bytes, and
	// executable bits exactly match the commit tree, without Git attribute
	// conversions or the copied index participating. "clean" accepts a
	// legitimately empty commit; an unmaterialized non-empty checkout is dirty
	// because every tracked path is missing.
	baseProofWorktreeKey = "worktree"
	// baseProofReplacementsKey reports whether the repository carries replace
	// refs or legacy grafts. The observer disables their interpretation while
	// resolving the commit, and refuses their presence so the writer cannot
	// see a different history than the gate proved.
	baseProofReplacementsKey = "git_replacements"
	// baseProofIrregularKey reports whether the workspace holds anything that
	// is neither a regular file nor a directory.
	//
	// The host refuses such entries in the source, but until the observer says
	// so too, that refusal was never attested: the digest hashes only regular
	// files, so a symlink introduced into the source after the walk and before
	// the copy would be invisible to both sides and would reach the
	// credential-bearing writer unapproved. Attesting the rule the host
	// enforces is what closes that window.
	//
	// When #339 teaches the gate to carry tracked symlinks, this key gives way
	// to a type-and-target dimension in the digest; until then the policy is
	// "none", and this is that policy observed rather than assumed.
	baseProofIrregularKey = "irregular"
)

// lostFoundDir is the ext4 volume's own directory, present on a fresh volume
// before anything is seeded. The seeder removes it before staging rather than
// the observer pruning it by name: a repository may legitimately track a
// root-level lost+found, and excluding the path on one side while the host
// digests it on the other would make such a tree permanently unseedable.
// Clearing it instead lets the attestation cover the whole workspace.
const lostFoundDir = "lost+found"

// observerScript reads the seeded base off the workspace and writes it, with
// this invocation's unpredictable nonce, to the observer's own root
// filesystem, where the host collects it by exporting the stopped container.
//
// The nonce is what makes the proof this run's. Without it, a proof file baked
// into the image or left behind by an earlier run would satisfy the gate, and
// the attestation would prove only that some workspace once held some base.
//
// The raw HEAD shape is checked before Git resolves it: a checkout from
// publish.Transport.FetchBase is detached at the base, so requiring the
// 40-lowercase-hex shape is stricter than following a symbolic ref. Git then
// resolves that commit with replacement processing disabled and compares the
// read-only worktree's raw paths, bytes, and modes with its tree.
func observerScript(cfg Config, nonce string) string {
	ws := shellQuote(cfg.WorkspaceTarget)
	proof := shellQuote(cfg.BaseProofPath)
	// Config.validate accepts any clean absolute path disjoint from the others,
	// so the proof may sit below a directory the pinned image does not carry.
	// Without this the redirect fails, no proof is exported, and every seeded
	// handoff fails on a configuration the gate said was valid.
	mkProofDir := ""
	if parent := path.Dir(cfg.BaseProofPath); parent != "/" && parent != "." {
		mkProofDir = "mkdir -p " + shellQuote(parent) + "; "
	}
	// No `set -e`: every branch must reach the proof write, because a proof
	// that reports an unexpected observation is what verifyBaseProof rejects.
	// A missing proof file and a proof reporting "absent" are both failures,
	// but only the second tells a reader what was wrong.
	//
	// LC_ALL=C is load-bearing, not hygiene. A bracket range in a shell
	// pattern is collated, not byte-valued, so under a UTF-8 locale `[!0-9a-f]`
	// does not reject `A` through `E`: they collate inside the a-f range. A
	// guest with LANG set would then attest an uppercase HEAD as a valid
	// detached commit. verifyBaseProof re-tests the shape host-side and would
	// still refuse it, but the guest expression must be strict on its own
	// rather than leaning on an unstated environment assumption.
	return "LC_ALL=C; export LC_ALL; " + mkProofDir +
		"g=absent; if [ -d " + ws + "/.git ]; then g=present; fi; " +
		"d=no; s=none; w=error; r=error; " +
		"if [ -f " + ws + "/.git/HEAD ]; then " +
		"h=\"$(cat " + ws + "/.git/HEAD 2>/dev/null || true)\"; " +
		// The shape test is the detachment test: a symbolic ref does not match
		// 40 hex characters, so one expression settles both.
		"case \"$h\" in " +
		"*[!0-9a-f]*) ;; " +
		"????????????????????????????????????????) d=yes;; " +
		"esac; fi; " +
		observerGitScript(cfg.WorkspaceTarget, cfg.BaseProofPath+".git") +
		// The tree digest, over the three dimensions the gate preserves: every
		// regular file's sha256 against its path, which files carry the
		// user-execute bit, and which directories exist. Each is hashed from
		// bytewise-sorted lines and the three results are hashed together. The
		// host computes it the same way over the verified source, so the two
		// agree only if the tree that landed is the tree the gate approved.
		// LC_ALL=C above is what makes the sorts byte-ordered.
		//
		// Three batched passes rather than a per-file loop: a real checkout is
		// thousands of files, and spawning a process each would put that cost
		// on every seeded handoff.
		// Anything that is not a regular file or a directory, reported so the
		// host's refusal of such entries is corroborated by observation rather
		// than trusted from a walk that ran before the copy.
		"n=present; if [ -z \"$(cd " + ws + " 2>/dev/null && " +
		"find . ! -type f ! -type d -print 2>/dev/null | head -n 1)\" ]; then n=absent; fi; " +
		"t=none; if [ \"$g\" = present ]; then " +
		"tc=\"$(cd " + ws + " && find . -type f -exec sha256sum {} + 2>/dev/null " +
		"| sort | sha256sum | cut -d' ' -f1)\"; " +
		"tx=\"$(cd " + ws + " && find . -type f -perm -u+x -print 2>/dev/null " +
		"| sort | sha256sum | cut -d' ' -f1)\"; " +
		"td=\"$(cd " + ws + " && find . -type d -print 2>/dev/null " +
		"| sort | sha256sum | cut -d' ' -f1)\"; " +
		"t=\"$(printf '%s\\n%s\\n%s\\n' \"$tc\" \"$tx\" \"$td\" | sha256sum | cut -d' ' -f1)\"; fi; " +
		"printf '" + baseProofNonceKey + "=%s\\n" +
		baseProofGitDirKey + "=%s\\n" +
		baseProofDetachedKey + "=%s\\n" +
		baseProofSHAKey + "=%s\\n" +
		baseProofWorktreeKey + "=%s\\n" +
		baseProofReplacementsKey + "=%s\\n" +
		baseProofIrregularKey + "=%s\\n" +
		baseProofTreeKey + "=%s\\n' " +
		shellQuote(nonce) + " \"$g\" \"$d\" \"$s\" \"$w\" \"$r\" \"$n\" \"$t\" > " + proof + "; sync"
}

// observerGitScript resolves HEAD and compares the raw worktree against its
// commit tree. It expects d to say whether the raw HEAD was detached and leaves
// s (resolved commit), w (clean, dirty, or error), and r (replacement metadata
// absent, present, or error) for the proof renderer. Scratch files live on the
// observer's own writable rootfs; the workspace remains read-only.
func observerGitScript(workspace, scratchPrefix string) string {
	ws := shellQuote(workspace)
	commitFile := shellQuote(scratchPrefix + ".commit")
	treeFile := shellQuote(scratchPrefix + ".tree")
	expectedFile := shellQuote(scratchPrefix + ".expected")
	actualFile := shellQuote(scratchPrefix + ".actual")
	pathFile := shellQuote(scratchPrefix + ".paths")
	expectedHashFile := shellQuote(scratchPrefix + ".expected-hashes")
	actualHashFile := shellQuote(scratchPrefix + ".actual-hashes")
	expectedExecFile := shellQuote(scratchPrefix + ".expected-exec")
	actualExecFile := shellQuote(scratchPrefix + ".actual-exec")
	replacementsFile := shellQuote(scratchPrefix + ".replacements")
	dirtyFile := shellQuote(scratchPrefix + ".dirty")
	gitEnv := "GIT_CONFIG_NOSYSTEM=1 HOME=/nonexistent XDG_CONFIG_HOME=/nonexistent "
	git := gitEnv + "git -c safe.directory=" + ws + " -C " + ws + " "
	rawGit := "GIT_NO_REPLACE_OBJECTS=1 " + git
	batchHash := "env GIT_NO_REPLACE_OBJECTS=1 GIT_CONFIG_NOSYSTEM=1 HOME=/nonexistent " +
		"XDG_CONFIG_HOME=/nonexistent git -c safe.directory=" + ws + " -C " + ws +
		" hash-object --no-filters -- "
	cleanup := "rm -f " + commitFile + " " + treeFile + " " + expectedFile + " " +
		actualFile + " " + pathFile + " " + expectedHashFile + " " + actualHashFile + " " +
		expectedExecFile + " " + actualExecFile + " " + replacementsFile + " " + dirtyFile + "; "
	processEntries := "tab=\"$(printf '\\t')\"; for entry do " +
		"meta=\"${entry%%\"$tab\"*}\"; p=\"${entry#*\"$tab\"}\"; " +
		"mode=\"${meta%% *}\"; rest=\"${meta#* }\"; type=\"${rest%% *}\"; oid=\"${rest#* }\"; " +
		"case \"$mode $type\" in '100644 blob'|'100755 blob') ;; " +
		"*) : > " + dirtyFile + "; continue;; esac; " +
		"file=" + ws + "/\"$p\"; if [ ! -f \"$file\" ] || [ -L \"$file\" ]; " +
		"then : > " + dirtyFile + "; continue; fi; " +
		"printf '%s\\000' \"$p\" >> " + pathFile + "; printf '%s\\n' \"$oid\" >> " + expectedHashFile + "; " +
		"printf './%s\\n' \"$p\" >> " + expectedFile + "; " +
		"if [ \"$mode\" = 100755 ]; then printf './%s\\n' \"$p\" >> " + expectedExecFile + "; fi; done"
	return cleanup +
		"if [ \"$d\" = yes ] && " +
		rawGit + "rev-parse --verify 'HEAD^{commit}' > " + commitFile + " 2>/dev/null; then " +
		"s=\"$(cat " + commitFile + " 2>/dev/null || true)\"; " +
		"if [ \"$s\" = \"$h\" ] && " + git + "replace -l > " + replacementsFile + " 2>/dev/null; then " +
		"if [ -s " + replacementsFile + " ] || [ -e " + ws + "/.git/info/grafts ]; " +
		"then r=present; else r=absent; fi; fi; " +
		"if [ \"$r\" = absent ] && " +
		rawGit + "ls-tree -rz --full-tree HEAD > " + treeFile + " 2>/dev/null; then " +
		"w=clean; : > " + expectedFile + "; : > " + pathFile + "; : > " + expectedHashFile +
		"; : > " + expectedExecFile + "; " +
		"if ! xargs -0 -n 100 sh -c " + shellQuote(processEntries) + " sh < " + treeFile +
		"; then w=error; elif [ -e " + dirtyFile + " ]; then w=dirty; fi; " +
		"if [ \"$w\" = clean ]; then " +
		"if [ -s " + pathFile + " ]; then xargs -0 -n 100 " + batchHash + "< " + pathFile +
		" > " + actualHashFile + " 2>/dev/null || w=dirty; else : > " + actualHashFile + "; fi; " +
		"fi; if [ \"$w\" = clean ]; then " +
		"(cd " + ws + " && find . -path ./.git -prune -o -type f -print | LC_ALL=C sort) > " +
		actualFile + " 2>/dev/null || w=error; " +
		"(cd " + ws + " && find . -path ./.git -prune -o -type f -perm -u+x -print | LC_ALL=C sort) > " +
		actualExecFile + " 2>/dev/null || w=error; " +
		"fi; if [ \"$w\" = clean ]; then " +
		"LC_ALL=C sort -o " + expectedFile + " " + expectedFile + " 2>/dev/null && " +
		"LC_ALL=C sort -o " + expectedExecFile + " " + expectedExecFile + " 2>/dev/null || w=error; fi; " +
		"if [ \"$w\" = clean ] && { ! cmp -s " + expectedFile + " " + actualFile +
		" || ! cmp -s " + expectedHashFile + " " + actualHashFile +
		" || ! cmp -s " + expectedExecFile + " " + actualExecFile + "; }; then w=dirty; fi; " +
		"fi; fi; " + cleanup
}

// Proof keys the credential-store observer emits, in the same key=value shape
// as the base and check-5 proofs.
const (
	credProofNonceKey = "nonce"
	credProofTreeKey  = "cred_tree"
)

// buildCredentialObserverSpec generates the container that attests the leased
// credential volume's content digest: once before the writer starts and once
// after it is proven absent, under the given per-instant name. Same position
// as the base observer — the pinned exporter image, network-free, the
// observed volume READ-ONLY, nothing else — and for the same reason: the
// observation must come from a VM that cannot write what it attests and did
// not run the writer. The digest is content evidence, never content: the §5.4
// residual is that the store mutated, and the proof carries a hash, not the
// store.
func buildCredentialObserverSpec(cfg Config, hs HandoffSpec, name string, ownershipLabel Label) ContainerSpec {
	var volume string
	for _, cm := range hs.Agent.CredentialMounts {
		if cm.Writable {
			volume = cm.Volume
			break
		}
	}
	return ContainerSpec{
		Name:            name,
		Image:           cfg.ExporterImage,
		Command:         credObserverCommand(cfg, ownershipLabel.Value, hs.writableCredentialTarget()),
		NetworkDisabled: true,
		Mounts: []Mount{{
			Type:     MountVolume,
			Source:   volume,
			Target:   hs.writableCredentialTarget(),
			ReadOnly: true,
		}},
		Labels: append(runLabels(hs.RunID), ownershipLabel),
	}
}

func credObserverCommand(cfg Config, nonce, target string) []string {
	return []string{"sh", "-c", credObserverScript(cfg, nonce, target)}
}

// credObserverScript digests the mounted credential volume and writes the
// proof, with this invocation's unpredictable nonce, to the observer's own
// root filesystem. The digest extends the base observer's three-pass shape
// (content hashes, executable bits, directories, each from bytewise-sorted
// lines, hashed together; LC_ALL=C makes the sorts byte-ordered) with a
// fourth pass over symlinks, a fifth over every remaining node kind
// (FIFOs, sockets, devices), and a sixth over every entry's inode, mode,
// hard-link count, owner, and group (ls -ldi's corresponding fields; both
// observations read the same volume with the same observer image, so
// inode values and name resolution are stable between them): the seeded
// workspace refuses non-file entries outright, but the leased store is
// the writer's to mutate, so any node created, removed, retargeted,
// re-kinded, re-moded, re-owned, or re-linked is store content and must
// move the digest — the inode binds each path to its identity, so even a
// count-preserving relinking of equivalence classes moves it. Each link is recorded as the hash of its path beside
// the hash of its target, hashed separately so a writer-chosen name
// embedding a separator cannot alias two different link sets into one
// record; each remaining node as its kind beside the hash of its path. As
// with the base observer, every branch reaches the proof write.
// Readability is deliberately not attested: an unreadable volume digests
// identically to an empty one (a failed cd feeds all six passes empty
// input, and per-file read errors digest the readable subset), so Mutated
// compares content identity between the two observations, nothing more —
// it is an audit record, never a release gate.
func credObserverScript(cfg Config, nonce, target string) string {
	tgt := shellQuote(target)
	proof := shellQuote(cfg.CredProofPath)
	mkProofDir := ""
	if parent := path.Dir(cfg.CredProofPath); parent != "/" && parent != "." {
		mkProofDir = "mkdir -p " + shellQuote(parent) + "; "
	}
	return "LC_ALL=C; export LC_ALL; " + mkProofDir +
		"tc=\"$(cd " + tgt + " 2>/dev/null && find . -type f -exec sha256sum {} + 2>/dev/null " +
		"| sort | sha256sum | cut -d' ' -f1)\"; " +
		"tx=\"$(cd " + tgt + " 2>/dev/null && find . -type f -perm -u+x -print 2>/dev/null " +
		"| sort | sha256sum | cut -d' ' -f1)\"; " +
		"td=\"$(cd " + tgt + " 2>/dev/null && find . -type d -print 2>/dev/null " +
		"| sort | sha256sum | cut -d' ' -f1)\"; " +
		"tl=\"$(cd " + tgt + " 2>/dev/null && find . -type l -exec sh -c " +
		"'for p; do printf \"%s %s\\n\" \"$(printf \"%s\" \"$p\" | sha256sum | cut -d\" \" -f1)\" " +
		"\"$(readlink \"$p\" | sha256sum | cut -d\" \" -f1)\"; done' sh {} + 2>/dev/null " +
		"| sort | sha256sum | cut -d' ' -f1)\"; " +
		"tn=\"$(cd " + tgt + " 2>/dev/null && find . ! -type f ! -type d ! -type l -exec sh -c " +
		"'for p; do k=other; [ -p \"$p\" ] && k=fifo; [ -S \"$p\" ] && k=socket; [ -b \"$p\" ] && k=block; [ -c \"$p\" ] && k=char; " +
		"printf \"%s %s\\n\" \"$k\" \"$(printf \"%s\" \"$p\" | sha256sum | cut -d\" \" -f1)\"; done' sh {} + 2>/dev/null " +
		"| sort | sha256sum | cut -d' ' -f1)\"; " +
		"tm=\"$(cd " + tgt + " 2>/dev/null && find . -exec sh -c " +
		"'for p; do set -- $(ls -ldi \"$p\" 2>/dev/null) _ _ _ _ _; " +
		"printf \"%s:%s:%s:%s:%s %s\\n\" \"$1\" \"$2\" \"$3\" \"$4\" \"$5\" \"$(printf \"%s\" \"$p\" | sha256sum | cut -d\" \" -f1)\"; done' sh {} + 2>/dev/null " +
		"| sort | sha256sum | cut -d' ' -f1)\"; " +
		"t=\"$(printf '%s\\n%s\\n%s\\n%s\\n%s\\n%s\\n' \"$tc\" \"$tx\" \"$td\" \"$tl\" \"$tn\" \"$tm\" | sha256sum | cut -d' ' -f1)\"; " +
		"printf '" + credProofNonceKey + "=%s\\n" + credProofTreeKey + "=%s\\n' " +
		shellQuote(nonce) + " \"$t\" > " + proof + "; sync"
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
