package ward

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const (
	// CodexHomeTarget is writable per invocation on the fresh container rootfs.
	// auth.json and AGENTS.md are symlinks into the read-only snapshot volume,
	// so CODEX_HOME stays writable while the credential bytes stay immutable.
	CodexHomeTarget = "/var/lib/freeside/codex-home"
	// CodexContainerHomeTarget is a clean per-invocation HOME, distinct from
	// CODEX_HOME so cross-agent skill discovery cannot reach operator state.
	CodexContainerHomeTarget = "/var/lib/freeside/home"
	CodexAuthFileTarget      = CodexHomeTarget + "/auth.json"
	CodexInstructionTarget   = CodexHomeTarget + "/AGENTS.md"
	// codexReviewSnapshotTarget is where the review container reads the two
	// admitted snapshots read-only. It is a named volume, not a host bind:
	// Apple container 1.1.0 rejects single-file bind sources (#591), and volumes
	// are ward's one live-proven read-only delivery transport. The two entries
	// carry fixed basenames on the volume regardless of their host source names.
	codexReviewSnapshotTarget      = "/var/lib/freeside/codex-snapshot"
	codexReviewSnapshotAuthName    = "auth.json"
	codexReviewSnapshotInstrName   = "AGENTS.md"
	codexReviewSnapshotAuthSource  = codexReviewSnapshotTarget + "/" + codexReviewSnapshotAuthName
	codexReviewSnapshotInstrSource = codexReviewSnapshotTarget + "/" + codexReviewSnapshotInstrName
	// codexReviewSnapshotSeedTarget is the read-write mount where the networkless
	// seeder places the two admitted files onto the fresh snapshot volume, and
	// codexReviewSnapshotObserverTarget is where the separate read-only observer
	// proves exactly those two files landed. The seed and observe roles use
	// distinct VMs and distinct targets for the same reason the workspace seeder
	// and observer do: the writer must never vouch for its own write.
	codexReviewSnapshotSeedTarget     = "/freeside-codex-snapshot-seed"
	codexReviewSnapshotObserverTarget = "/freeside-codex-snapshot"
	codexReviewSnapshotProofPath      = "/freeside-codex-snapshot-proof.txt"
	codexShadowObserverTarget         = "/freeside-agents-shadow"
	codexShadowProofPath              = "/freeside-agents-shadow-proof.txt"
	codexWorkspaceProofPath           = "/freeside-review-workspace-proof.txt"
	codexReviewOutputDir              = "/freeside-review-output"
	codexReviewResultPath             = codexReviewOutputDir + "/result.json"
	codexReviewEventsPath             = codexReviewOutputDir + "/events.jsonl"
	codexReviewStatusPath             = codexReviewOutputDir + "/status"
	codexReviewSchemaPath             = codexReviewOutputDir + "/schema.json"

	// Bumped to v3: the workspace-local .agents shadow mount became
	// conditional on the observed candidate tree. Apple container cannot
	// create a missing nested mountpoint under the read-only workspace, so a
	// candidate without .agents mounts no shadow there, and the chosen
	// topology is bound to the observation that justified it. A v2 binding
	// must not validate against the conditional shape; teardown
	// authentication alone still accepts v2 so pre-upgrade reviews can be
	// reaped. (v2 was #591's move to the read-only snapshot volume.)
	codexReviewTopologyVersion   = "codex_review_read_only_v3"
	codexReviewTopologyVersionV2 = "codex_review_read_only_v2"
	maxCodexAuthSnapshotBytes    = 1 << 20
	maxCodexReviewPromptBytes    = 31 << 10
	emptyCodexShadowDigest       = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	// codexWorkspaceAgentsKey is the workspace observer's proof line reporting
	// what the candidate tree holds at <workspace>/.agents. The shadow-mount
	// topology is derived from this observed value, never from a host-side
	// guess: "dir" keeps the empty shadow mounted over the repository-local
	// entry, "absent" omits only that workspace-local mount (the read-only
	// immutable workspace cannot grow one later), and any other entry kind
	// fails closed before a container is created.
	codexWorkspaceAgentsKey    = "workspace_agents"
	codexWorkspaceAgentsDir    = "dir"
	codexWorkspaceAgentsAbsent = "absent"
)

// ErrInvalidCodexReviewSpec is the typed admission refusal for a Codex review
// topology request. It is distinct from a conformance failure: invalid input
// launches nothing, while conformance means a generated or realized topology
// diverged from the admitted one.
var ErrInvalidCodexReviewSpec = errors.New("invalid codex review spec")

// Recovery must distinguish an exact-owner lease it can safely adopt from an
// attachment window atomically transferred into the review container. A
// foreign holder is never an adoption candidate.
var (
	ErrCodexReviewVolumeLeaseTransferred  = errors.New("codex review volume lease transferred")
	ErrCodexReviewVolumeLeaseForeignOwner = errors.New("codex review volume lease held by foreign owner")
)

// ErrCodexReviewContinuityRefused reports an attempt to put resume or
// continuity state on the fresh-context review path.
var ErrCodexReviewContinuityRefused = errors.New("codex review continuity is refused")

// CodexReviewBoundary is the requested process-entry shape. The non-fresh
// values are representable only so admission can reject them explicitly.
type CodexReviewBoundary string

const (
	CodexReviewFreshStart CodexReviewBoundary = "fresh_start"
	CodexReviewResume     CodexReviewBoundary = "resume"
	CodexReviewContinuity CodexReviewBoundary = "continuity"
)

// AllCodexReviewBoundaries is the single registration point for recognized
// review entry requests, including the two shapes admission refuses.
var AllCodexReviewBoundaries = []CodexReviewBoundary{
	CodexReviewFreshStart, CodexReviewResume, CodexReviewContinuity,
}

func (b CodexReviewBoundary) valid() bool {
	switch b {
	case CodexReviewFreshStart, CodexReviewResume, CodexReviewContinuity:
		return true
	default:
		return false
	}
}

// CodexAuthMode selects the one provider endpoint and auth.json shape the
// review may receive. The zero value is invalid by design.
type CodexAuthMode string

const (
	CodexAuthSubscription CodexAuthMode = "subscription"
	CodexAuthAPIKey       CodexAuthMode = "api_key"
)

// AllCodexAuthModes is the single registration point for Codex review auth.
var AllCodexAuthModes = []CodexAuthMode{CodexAuthSubscription, CodexAuthAPIKey}

func (m CodexAuthMode) valid() bool {
	switch m {
	case CodexAuthSubscription, CodexAuthAPIKey:
		return true
	default:
		return false
	}
}

func (m CodexAuthMode) providerEndpoints() []string {
	switch m {
	case CodexAuthSubscription:
		return []string{"chatgpt.com:443"}
	case CodexAuthAPIKey:
		return []string{"api.openai.com:443"}
	}
	return nil
}

// CodexReviewConfig is the trusted, deployment-scoped half of one review
// topology. InputRoot owns the two single-file snapshots; ProviderEndpoints
// must be exactly the endpoint AuthMode selects, never the refresh host;
// ObserverImage is the pinned, credential-free image that proves the .agents
// shadow empty and the candidate workspace's exact head and tree.
type CodexReviewConfig struct {
	InputRoot         string
	WorkspaceTarget   string
	ProviderEndpoints []string
	ProxyURL          string
	// ApprovedImage is the deployment-owned pin for the Codex CLI that
	// receives review credentials. Caller intent may name it, never select it.
	ApprovedImage            string
	ObserverImage            string
	Model                    string
	ReasoningEffort          string
	AccessTokenLifetimeFloor time.Duration
	// AccessTokenRefreshThreshold is the proactive host-side refresh point. It
	// must exceed AccessTokenLifetimeFloor so a transient refresh failure never
	// hands a marginal token to the reviewer.
	AccessTokenRefreshThreshold time.Duration
	AuthStoreLeaser             AuthStoreLeaser
	AuthRefresher               CodexAuthRefresher
	AuthState                   CodexAuthState
	Now                         func() time.Time
	Journal                     CodexReviewJournal
	// VolumeLifecycleLeaser holds exclusive attachment authority for the
	// workspace and .agents shadow through an atomic transfer into Start.
	VolumeLifecycleLeaser CodexReviewVolumeLifecycleLeaser
}

// CodexReviewVolumeLifecycleLeaser is the deployment/runtime seam that owns
// exclusive attachment authority for both review volumes. Acquire must refuse
// conflicting attachment requests until Start atomically transfers the lease
// into the target review container or Release ends it. A ward-local mutex is
// insufficient because it cannot constrain a runtime attachment.
type CodexReviewVolumeLifecycleLeaser interface {
	AcquireCodexReviewVolumeLease(
		ctx context.Context, holder string, volumes []string,
	) (CodexReviewVolumeLifecycleLease, error)
	// RecoverCodexReviewVolumeLease atomically adopts an unattached lease only
	// when it is free or held by this exact durable owner. If the exact review
	// container already attaches both volumes, it returns
	// ErrCodexReviewVolumeLeaseTransferred together with authenticated
	// attachment evidence; another holder returns
	// ErrCodexReviewVolumeLeaseForeignOwner. Once the coordinator itself
	// observes that the attachment's target container no longer exists,
	// the window is over: a later call from the exact durable owner adopts the
	// lease back as held, which is how ward retries after reaping a pre-handoff
	// attachment it could not adopt.
	RecoverCodexReviewVolumeLease(
		ctx context.Context, holder string, volumes []string,
	) (CodexReviewVolumeLifecycleLease, CodexReviewVolumeLeaseTransfer, error)
}

// CodexReviewVolumeLeaseTransfer is deployment-owned evidence that the exact
// review container immutably attaches the lifecycle window's two volumes.
type CodexReviewVolumeLeaseTransfer struct {
	Holder    string
	Volumes   []string
	Container string
}

// CodexReviewVolumeLifecycleLease is one held exclusive attachment window.
// Start transfers it atomically into the review container, preventing an
// unleased write between final observation and start. After Start succeeds,
// the runtime carries that immutable attachment invariant for #427's
// post-start collection and cleanup lifecycle.
type CodexReviewVolumeLifecycleLease interface {
	StartCodexReviewContainer(ctx context.Context, container string) error
	ReleaseCodexReviewVolumeLease(ctx context.Context) error
}

// CodexReviewShadowObservation is opaque evidence produced only after ward
// validates a runtime-owned shadow volume and a networkless, read-only
// observer's proof that the volume is empty. Callers cannot construct it by
// copying identity or digest strings into a request.
type CodexReviewShadowObservation struct {
	volume              string
	fingerprint         string
	digest              string
	observerImage       string
	observerFingerprint string
}

// CodexReviewWorkspaceObservation is opaque evidence from a pinned,
// networkless observer that the owned candidate volume held one exact,
// detached, clean commit and tree through a read-only mount.
type CodexReviewWorkspaceObservation struct {
	volume              string
	fingerprint         string
	head                string
	treeDigest          string
	agentsEntry         string
	observerImage       string
	observerFingerprint string
}

func (o CodexReviewWorkspaceObservation) valid() bool {
	_, treeDigestOK := contentaddr.FromHex(o.treeDigest)
	return o.volume != "" && cliSafe(o.volume) && o.fingerprint != "" &&
		commitSHAPattern.MatchString(o.head) && treeDigestOK &&
		(o.agentsEntry == codexWorkspaceAgentsDir || o.agentsEntry == codexWorkspaceAgentsAbsent) &&
		digestPinnedImagePattern.MatchString(o.observerImage) && o.observerFingerprint != ""
}

func (o CodexReviewWorkspaceObservation) verifyFresh(fresh CodexReviewWorkspaceObservation) error {
	if !o.valid() || !fresh.valid() || fresh.volume != o.volume ||
		fresh.fingerprint != o.fingerprint || fresh.head != o.head ||
		fresh.treeDigest != o.treeDigest || fresh.agentsEntry != o.agentsEntry ||
		fresh.observerImage != o.observerImage {
		return failf(CheckObservedBaseIdentity, "Codex review workspace changed before launch")
	}
	if fresh.observerFingerprint == o.observerFingerprint {
		return failf(CheckObservedBaseIdentity, "Codex review workspace was not re-observed before launch")
	}
	return nil
}

// CodexReviewNetworkObservation is opaque current-state evidence for the
// owned host-only network and daemon proxy binding used by one review.
type CodexReviewNetworkObservation struct {
	name           string
	fingerprint    string
	gateway        string
	subnet         string
	proxyAuthority string
}

func (o CodexReviewNetworkObservation) valid() bool {
	return o.name != "" && cliSafe(o.name) && o.fingerprint != "" &&
		o.gateway != "" && o.subnet != "" && o.proxyAuthority != ""
}

func (o CodexReviewNetworkObservation) verifyCurrent(current CodexReviewNetworkObservation) error {
	if !o.valid() || !current.valid() || current != o {
		return failf(CheckAgentEgress, "Codex review provider network changed before launch")
	}
	return nil
}

func (o CodexReviewShadowObservation) valid() bool {
	return o.volume != "" && cliSafe(o.volume) && o.fingerprint != "" && o.observerFingerprint != "" &&
		o.digest == emptyCodexShadowDigest && digestPinnedImagePattern.MatchString(o.observerImage)
}

func (o CodexReviewShadowObservation) verifyFresh(fresh CodexReviewShadowObservation) error {
	if !o.valid() || !fresh.valid() || fresh.volume != o.volume ||
		fresh.fingerprint != o.fingerprint || fresh.digest != o.digest ||
		fresh.observerImage != o.observerImage {
		return failf(CheckControlPlaneIsolation, "Codex review shadow volume changed before launch")
	}
	if fresh.observerFingerprint == o.observerFingerprint {
		return failf(CheckControlPlaneIsolation, "Codex review shadow emptiness was not re-observed before launch")
	}
	return nil
}

// CodexReviewSnapshotObservation is opaque evidence from a pinned, networkless,
// read-only observer that the ward-owned snapshot volume holds exactly the two
// admitted files (auth.json and AGENTS.md), both regular and unsymlinked, and
// carries their sha256 digests. Callers cannot construct it by copying identity
// or digest strings into a request; BuildCodexReviewAgentSpec ties the recorded
// digests back to the bytes admission re-read from the host.
type CodexReviewSnapshotObservation struct {
	volume              string
	fingerprint         string
	authDigest          string
	instructionDigest   string
	observerImage       string
	observerFingerprint string
}

func (o CodexReviewSnapshotObservation) valid() bool {
	return o.volume != "" && cliSafe(o.volume) && o.fingerprint != "" && o.observerFingerprint != "" &&
		contentaddr.Valid(o.authDigest) && contentaddr.Valid(o.instructionDigest) &&
		digestPinnedImagePattern.MatchString(o.observerImage)
}

func (o CodexReviewSnapshotObservation) verifyFresh(fresh CodexReviewSnapshotObservation) error {
	if !o.valid() || !fresh.valid() || fresh.volume != o.volume ||
		fresh.fingerprint != o.fingerprint || fresh.authDigest != o.authDigest ||
		fresh.instructionDigest != o.instructionDigest || fresh.observerImage != o.observerImage {
		return failf(CheckCredentialSeparation, "Codex review snapshot volume changed before launch")
	}
	if fresh.observerFingerprint == o.observerFingerprint {
		return failf(CheckCredentialSeparation, "Codex review snapshot was not re-observed before launch")
	}
	return nil
}

// CodexReviewSpec is the caller-owned request for one fresh review container.
// It carries paths only to daemon-prepared, single-file snapshots under
// Config.InputRoot; BuildCodexReviewAgentSpec re-opens and validates them.
type CodexReviewSpec struct {
	RunID                string
	Image                string
	WorkspaceSourceRunID string
	WorkspaceVolume      string
	Workspace            CodexReviewWorkspaceObservation
	Network              CodexReviewNetworkObservation
	Prompt               string
	Boundary             CodexReviewBoundary
	AuthMode             CodexAuthMode
	AuthIdentityID       domain.AuthIdentityID
	AuthSnapshot         string
	Instructions         VendorInstructions
	InstructionFile      string
	InstructionBinding   exec.ReviewInstructionBinding

	// AgentsShadow is the runtime-backed observation of one empty volume,
	// mounted read-only at HOME/.agents, at every in-container ancestor of
	// the workspace, and over the workspace's own .agents exactly when the
	// observed candidate tree carries that directory. Its evidence never
	// authorizes cleanup.
	AgentsShadow CodexReviewShadowObservation

	// Snapshot is the runtime-backed observation of the ward-owned read-only
	// volume that delivers exactly the two admitted files (auth.json, AGENTS.md)
	// to the review container. Its digests are tied to the admitted bodies.
	Snapshot CodexReviewSnapshotObservation
}

// CodexReviewJournalBinding is the non-secret topology evidence a review
// source persists before launch. VerifyCodexReviewAllowlist adds the mandatory
// pre-start observation beside the initial observation. The binding carries no
// prompt or credential bytes.
type CodexReviewJournalBinding struct {
	TopologyVersion                         string                         `json:"topology_version"`
	RunID                                   string                         `json:"run_id"`
	Boundary                                CodexReviewBoundary            `json:"boundary"`
	WorkspaceSourceRunID                    string                         `json:"workspace_source_run_id"`
	WorkspaceVolume                         string                         `json:"workspace_volume"`
	WorkspaceFingerprint                    string                         `json:"workspace_fingerprint"`
	WorkspaceHead                           string                         `json:"workspace_head"`
	WorkspaceTreeDigest                     string                         `json:"workspace_tree_digest"`
	WorkspaceAgentsEntry                    string                         `json:"workspace_agents_entry"`
	WorkspaceObserverImage                  string                         `json:"workspace_observer_image"`
	WorkspaceObserverFingerprint            string                         `json:"workspace_observer_fingerprint"`
	WorkspacePreStartObserverFingerprint    string                         `json:"workspace_pre_start_observer_fingerprint"`
	WorkspaceTarget                         string                         `json:"workspace_target"`
	WorkspaceReadOnly                       bool                           `json:"workspace_read_only"`
	HomeTarget                              string                         `json:"home_target"`
	CodexHomeTarget                         string                         `json:"codex_home_target"`
	FreshContext                            bool                           `json:"fresh_context"`
	ContinuityMounted                       bool                           `json:"continuity_mounted"`
	AuthMode                                CodexAuthMode                  `json:"auth_mode"`
	AuthIdentityID                          domain.AuthIdentityID          `json:"auth_identity_id"`
	AuthSnapshotDigest                      string                         `json:"auth_snapshot_digest"`
	AccessTokenExpiresAt                    *time.Time                     `json:"access_token_expires_at"`
	AuthReadOnly                            bool                           `json:"auth_read_only"`
	AuthStoreMutationLeaseRequired          bool                           `json:"auth_store_mutation_lease_required"`
	InstructionDigest                       domain.Digest                  `json:"instruction_digest"`
	InstructionCompositionVersion           string                         `json:"instruction_composition_version"`
	HostInstructionDigest                   *domain.Digest                 `json:"host_instruction_digest"`
	RepositoryInstructionSources            []exec.ReviewInstructionSource `json:"repository_instruction_sources"`
	InstructionReadOnly                     bool                           `json:"instruction_read_only"`
	SnapshotVolume                          string                         `json:"snapshot_volume"`
	SnapshotTarget                          string                         `json:"snapshot_target"`
	SnapshotFingerprint                     string                         `json:"snapshot_fingerprint"`
	SnapshotObserverImage                   string                         `json:"snapshot_observer_image"`
	SnapshotObserverFingerprint             string                         `json:"snapshot_observer_fingerprint"`
	SnapshotPreStartObserverFingerprint     string                         `json:"snapshot_pre_start_observer_fingerprint"`
	SnapshotReadOnly                        bool                           `json:"snapshot_read_only"`
	AgentsShadowVolume                      string                         `json:"agents_shadow_volume"`
	AgentsShadowFingerprint                 string                         `json:"agents_shadow_fingerprint"`
	AgentsShadowDigest                      string                         `json:"agents_shadow_digest"`
	AgentsShadowObserverImage               string                         `json:"agents_shadow_observer_image"`
	AgentsShadowObserverFingerprint         string                         `json:"agents_shadow_observer_fingerprint"`
	AgentsShadowPreStartObserverFingerprint string                         `json:"agents_shadow_pre_start_observer_fingerprint"`
	AgentsShadowTargets                     []string                       `json:"agents_shadow_targets"`
	AgentsShadowReadOnly                    bool                           `json:"agents_shadow_read_only"`
	ProviderEndpoints                       []string                       `json:"provider_endpoints"`
	ProviderNetwork                         string                         `json:"provider_network"`
	ProviderNetworkFingerprint              string                         `json:"provider_network_fingerprint"`
	ProviderNetworkHostOnly                 bool                           `json:"provider_network_host_only"`
	ProviderNetworkGateway                  string                         `json:"provider_network_gateway"`
	ProviderNetworkSubnet                   string                         `json:"provider_network_subnet"`
	ProviderProxyAuthority                  string                         `json:"provider_proxy_authority"`
	RefreshEndpointReachable                bool                           `json:"refresh_endpoint_reachable"`
	PublicationCredentials                  bool                           `json:"publication_credentials"`
	LauncherEnvironmentDigest               string                         `json:"launcher_environment_digest"`
	CommandDigest                           string                         `json:"command_digest"`
	ReviewContainer                         string                         `json:"review_container"`
	ReviewContainerFingerprint              string                         `json:"review_container_fingerprint"`
	ReviewOwnershipToken                    string                         `json:"review_ownership_token"`
}

// BuildCodexReviewShadowObserverSpec returns the exact networkless helper
// topology whose proof can establish that the .agents shadow is empty. It is
// a topology constructor for conformance tests; CodexReviewLifecycle.CodexReview owns the
// safe runtime lifecycle.
func BuildCodexReviewShadowObserverSpec(
	cfg CodexReviewConfig,
	runID, volume string,
	ownershipLabel Label,
) (ContainerSpec, error) {
	switch {
	case !runIDPattern.MatchString(runID):
		return ContainerSpec{}, fmt.Errorf("%w: RunID is invalid", ErrInvalidCodexReviewSpec)
	case volume == "" || !cliSafe(volume):
		return ContainerSpec{}, fmt.Errorf("%w: shadow volume is invalid", ErrInvalidCodexReviewSpec)
	case !digestPinnedImagePattern.MatchString(cfg.ObserverImage):
		return ContainerSpec{}, fmt.Errorf("%w: ObserverImage must be digest-pinned", ErrInvalidCodexReviewSpec)
	case !codexReviewOwnershipLabelValid(ownershipLabel):
		return ContainerSpec{}, fmt.Errorf("%w: ownership label is invalid", ErrInvalidCodexReviewSpec)
	}
	return ContainerSpec{
		Name:  codexReviewShadowObserverName(runID),
		Image: cfg.ObserverImage,
		Command: []string{
			"sh", "-c", codexReviewShadowObserverScript(
				ownershipLabel.Value, codexShadowObserverTarget, codexShadowProofPath,
			),
		},
		Mounts: []Mount{{
			Type: MountVolume, Source: volume, Target: codexShadowObserverTarget, ReadOnly: true,
		}},
		Labels:          append(runLabels(runID), ownershipLabel),
		NetworkDisabled: true,
	}, nil
}

// ObserveCodexReviewShadow validates runtime observations and the exported
// proof from BuildCodexReviewShadowObserverSpec. Success returns the only
// value BuildCodexReviewAgentSpec accepts as empty-shadow evidence.
func ObserveCodexReviewShadow(
	cfg CodexReviewConfig,
	runID, volume string,
	volumeOwnershipLabel, observerOwnershipLabel Label,
	volumeReport VolumeSummary,
	observerReport InspectReport,
	proof []byte,
) (CodexReviewShadowObservation, error) {
	spec, err := BuildCodexReviewShadowObserverSpec(cfg, runID, volume, observerOwnershipLabel)
	if err != nil {
		return CodexReviewShadowObservation{}, err
	}
	if volumeReport.Name != volume {
		return CodexReviewShadowObservation{}, failf(
			CheckControlPlaneIsolation, "Codex review shadow observation identified the wrong volume",
		)
	}
	fingerprint, err := ownedFingerprint(
		volumeReport.CreationDate, volumeReport.Labels, volumeReport.LabelsObserved, volumeOwnershipLabel,
	)
	if err != nil {
		return CodexReviewShadowObservation{}, failf(
			CheckControlPlaneIsolation, "Codex review shadow ownership: %v", err,
		)
	}
	if err := verifyCodexReviewShadowObserverAllowlist(observerReport, spec); err != nil {
		return CodexReviewShadowObservation{}, err
	}
	observerCreationFingerprint, err := ownedFingerprint(
		observerReport.CreationDate, observerReport.Labels,
		observerReport.LabelsObserved, observerOwnershipLabel,
	)
	if err != nil {
		return CodexReviewShadowObservation{}, failf(
			CheckControlPlaneIsolation, "Codex review shadow observer ownership is not fingerprinted",
		)
	}
	observerFingerprint := codexReviewObserverFingerprint(
		observerCreationFingerprint, observerOwnershipLabel,
	)
	wantProof := fmt.Sprintf(
		"nonce=%s\nempty=yes\ntree=%s\n", observerOwnershipLabel.Value, emptyCodexShadowDigest,
	)
	if !bytes.Equal(proof, []byte(wantProof)) {
		return CodexReviewShadowObservation{}, failf(
			CheckControlPlaneIsolation, "Codex review shadow observer did not prove an empty volume",
		)
	}
	return CodexReviewShadowObservation{
		volume: volume, fingerprint: fingerprint, digest: emptyCodexShadowDigest,
		observerImage: cfg.ObserverImage, observerFingerprint: observerFingerprint,
	}, nil
}

// BuildCodexReviewSnapshotObserverSpec returns the pinned, networkless,
// read-only observer whose proof establishes that the snapshot volume holds
// exactly the two admitted files and reports their sha256 digests. It is a
// topology constructor for conformance tests; CodexReviewLifecycle.CodexReview owns the safe
// runtime lifecycle.
func BuildCodexReviewSnapshotObserverSpec(
	cfg CodexReviewConfig,
	runID, volume string,
	ownershipLabel Label,
) (ContainerSpec, error) {
	switch {
	case !runIDPattern.MatchString(runID):
		return ContainerSpec{}, fmt.Errorf("%w: RunID is invalid", ErrInvalidCodexReviewSpec)
	case volume == "" || !cliSafe(volume):
		return ContainerSpec{}, fmt.Errorf("%w: snapshot volume is invalid", ErrInvalidCodexReviewSpec)
	case !digestPinnedImagePattern.MatchString(cfg.ObserverImage):
		return ContainerSpec{}, fmt.Errorf("%w: ObserverImage must be digest-pinned", ErrInvalidCodexReviewSpec)
	case !codexReviewOwnershipLabelValid(ownershipLabel):
		return ContainerSpec{}, fmt.Errorf("%w: ownership label is invalid", ErrInvalidCodexReviewSpec)
	}
	return ContainerSpec{
		Name:  codexReviewSnapshotObserverName(runID),
		Image: cfg.ObserverImage,
		Command: []string{
			"sh", "-c", codexReviewSnapshotObserverScript(
				ownershipLabel.Value, codexReviewSnapshotObserverTarget, codexReviewSnapshotProofPath,
			),
		},
		Mounts: []Mount{{
			Type: MountVolume, Source: volume, Target: codexReviewSnapshotObserverTarget, ReadOnly: true,
		}},
		Labels:          append(runLabels(runID), ownershipLabel),
		NetworkDisabled: true,
	}, nil
}

// ObserveCodexReviewSnapshot validates the runtime-owned snapshot volume, the
// networkless read-only observer, and its nonce-bound proof. The returned value
// is the only snapshot evidence the review topology accepts. Its digests are
// tied to the admitted bodies inside BuildCodexReviewAgentSpec.
func ObserveCodexReviewSnapshot(
	cfg CodexReviewConfig,
	runID, volume string,
	volumeOwnershipLabel, observerOwnershipLabel Label,
	volumeReport VolumeSummary,
	observerReport InspectReport,
	proof []byte,
) (CodexReviewSnapshotObservation, error) {
	spec, err := BuildCodexReviewSnapshotObserverSpec(cfg, runID, volume, observerOwnershipLabel)
	if err != nil {
		return CodexReviewSnapshotObservation{}, err
	}
	if !codexReviewOwnershipLabelValid(volumeOwnershipLabel) {
		return CodexReviewSnapshotObservation{}, failf(
			CheckCredentialSeparation, "Codex review snapshot ownership claim is invalid",
		)
	}
	if volumeReport.Name != volume {
		return CodexReviewSnapshotObservation{}, failf(
			CheckCredentialSeparation, "Codex review snapshot observation identified the wrong volume",
		)
	}
	fingerprint, err := ownedFingerprint(
		volumeReport.CreationDate, volumeReport.Labels, volumeReport.LabelsObserved, volumeOwnershipLabel,
	)
	if err != nil {
		return CodexReviewSnapshotObservation{}, failf(
			CheckCredentialSeparation, "Codex review snapshot ownership: %v", err,
		)
	}
	if err := verifySeedRoleAllowlist(
		observerReport, spec, volume, codexReviewSnapshotObserverTarget, CheckCredentialSeparation,
	); err != nil {
		return CodexReviewSnapshotObservation{}, err
	}
	observerCreationFingerprint, err := ownedFingerprint(
		observerReport.CreationDate, observerReport.Labels,
		observerReport.LabelsObserved, observerOwnershipLabel,
	)
	if err != nil {
		return CodexReviewSnapshotObservation{}, failf(
			CheckCredentialSeparation, "Codex review snapshot observer ownership is not fingerprinted",
		)
	}
	observerFingerprint := codexReviewObserverFingerprint(
		observerCreationFingerprint, observerOwnershipLabel,
	)
	authDigest, instructionDigest, err := parseCodexReviewSnapshotProof(proof, observerOwnershipLabel.Value)
	if err != nil {
		return CodexReviewSnapshotObservation{}, err
	}
	return CodexReviewSnapshotObservation{
		volume: volume, fingerprint: fingerprint,
		authDigest: authDigest, instructionDigest: instructionDigest,
		observerImage: cfg.ObserverImage, observerFingerprint: observerFingerprint,
	}, nil
}

// codexReviewSnapshotObserverScript lists the snapshot volume and gates on it
// holding exactly {AGENTS.md, auth.json} as regular, unsymlinked files, then
// emits their sha256 digests. Any extra, missing, non-regular, or symlinked
// entry leaves valid=invalid, which the proof parser fails closed on.
func codexReviewSnapshotObserverScript(nonce, targetPath, proofPath string) string {
	target := shellQuote(targetPath)
	auth := shellQuote(targetPath + "/" + codexReviewSnapshotAuthName)
	instr := shellQuote(targetPath + "/" + codexReviewSnapshotInstrName)
	proof := shellQuote(proofPath)
	return "set -u; LC_ALL=C; export LC_ALL; valid=invalid; authsum=; instrsum=; " +
		"entries=\"$(cd " + target + " 2>/dev/null && find . ! -name . -print | sort)\"; " +
		"if [ \"$entries\" = './AGENTS.md\n./auth.json' ] && " +
		"[ -f " + auth + " ] && [ ! -L " + auth + " ] && " +
		"[ -f " + instr + " ] && [ ! -L " + instr + " ]; then " +
		"authsum=\"$(sha256sum " + auth + " | cut -d' ' -f1)\"; " +
		"instrsum=\"$(sha256sum " + instr + " | cut -d' ' -f1)\"; " +
		"if [ -n \"$authsum\" ] && [ -n \"$instrsum\" ]; then valid=valid; fi; fi; " +
		"printf 'nonce=%s\\nvalid=%s\\nauth=sha256:%s\\ninstr=sha256:%s\\n' " +
		shellQuote(nonce) + " \"$valid\" \"$authsum\" \"$instrsum\" > " + proof
}

// parseCodexReviewSnapshotProof validates the nonce-bound proof and returns the
// two digests. It fails closed unless the observer proved exactly the two files
// (valid=valid) and reported two well-formed sha256 digests.
func parseCodexReviewSnapshotProof(proof []byte, nonce string) (string, string, error) {
	if len(proof) == 0 || len(proof) > maxBaseProofBytes {
		return "", "", failf(CheckCredentialSeparation, "Codex review snapshot proof has an invalid size")
	}
	var sawNonce, valid bool
	var authDigest, instructionDigest string
	for _, line := range strings.Split(strings.ReplaceAll(string(proof), "\r\n", "\n"), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "nonce":
			if sawNonce || value != nonce {
				return "", "", failf(CheckCredentialSeparation, "Codex review snapshot proof nonce is invalid")
			}
			sawNonce = true
		case "valid":
			if valid || value != "valid" {
				return "", "", failf(
					CheckCredentialSeparation,
					"Codex review snapshot observer did not prove exactly the two admitted files",
				)
			}
			valid = true
		case "auth":
			if authDigest != "" || !contentaddr.Valid(value) {
				return "", "", failf(CheckCredentialSeparation, "Codex review snapshot proof auth digest is invalid")
			}
			authDigest = value
		case "instr":
			if instructionDigest != "" || !contentaddr.Valid(value) {
				return "", "", failf(CheckCredentialSeparation, "Codex review snapshot proof instruction digest is invalid")
			}
			instructionDigest = value
		}
	}
	if !sawNonce || !valid || authDigest == "" || instructionDigest == "" {
		return "", "", failf(CheckCredentialSeparation, "Codex review snapshot proof is incomplete")
	}
	return authDigest, instructionDigest, nil
}

// codexReviewSnapshotSeederScript is the fixed, gate-authored command that a
// networkless seeder runs in the pinned observer image: it waits for the host's
// completion sentinel, refuses a staged tree that is not exactly the two files,
// clears the volume's filesystem lost+found, and copies the two files onto the
// read-write-mounted snapshot volume. A separate read-only observer, in its own
// VM, is the proof; this writer never vouches for its own write.
func codexReviewSnapshotSeederScript(cfg Config, volumeTarget string) string {
	ready := shellQuote(path.Join(cfg.SeedReadyDir, seedReadyFile))
	stage := shellQuote(cfg.SeedStageDir)
	stageAuth := shellQuote(cfg.SeedStageDir + "/" + codexReviewSnapshotAuthName)
	stageInstr := shellQuote(cfg.SeedStageDir + "/" + codexReviewSnapshotInstrName)
	volAuth := shellQuote(volumeTarget + "/" + codexReviewSnapshotAuthName)
	volInstr := shellQuote(volumeTarget + "/" + codexReviewSnapshotInstrName)
	lostFound := shellQuote(volumeTarget + "/" + lostFoundDir)
	ticks := seederScriptTicks(cfg)
	return "set -eu; i=0; " +
		"while [ ! -f " + ready + " ]; do " +
		"i=$((i+1)); if [ \"$i\" -gt " + fmt.Sprintf("%d", ticks) + " ]; then exit 91; fi; " +
		"sleep 1; done; " +
		"staged=\"$(cd " + stage + " && find . ! -name . -print | sort)\"; " +
		"if [ \"$staged\" != './AGENTS.md\n./auth.json' ]; then exit 92; fi; " +
		"if [ ! -f " + stageAuth + " ] || [ -L " + stageAuth + " ]; then exit 93; fi; " +
		"if [ ! -f " + stageInstr + " ] || [ -L " + stageInstr + " ]; then exit 94; fi; " +
		"rm -rf " + lostFound + "; " +
		"cp " + stageAuth + " " + volAuth + "; " +
		"cp " + stageInstr + " " + volInstr + "; sync"
}

// BuildCodexReviewWorkspaceObserverSpec returns the pinned, networkless,
// read-only observer used to bind the review to one candidate commit and tree.
// CodexReviewLifecycle.CodexReview is the safe runtime lifecycle that executes it.
func BuildCodexReviewWorkspaceObserverSpec(
	cfg CodexReviewConfig,
	runID, volume string,
	ownershipLabel Label,
) (ContainerSpec, error) {
	switch {
	case !runIDPattern.MatchString(runID):
		return ContainerSpec{}, fmt.Errorf("%w: RunID is invalid", ErrInvalidCodexReviewSpec)
	case volume == "" || !cliSafe(volume):
		return ContainerSpec{}, fmt.Errorf("%w: workspace volume is invalid", ErrInvalidCodexReviewSpec)
	case !cleanAbs(cfg.WorkspaceTarget) || !cliSafe(cfg.WorkspaceTarget):
		return ContainerSpec{}, fmt.Errorf("%w: WorkspaceTarget is invalid", ErrInvalidCodexReviewSpec)
	case codexReviewWorkspaceOverlapsControlPath(cfg.WorkspaceTarget):
		return ContainerSpec{}, fmt.Errorf("%w: WorkspaceTarget overlaps a protected path", ErrInvalidCodexReviewSpec)
	case !digestPinnedImagePattern.MatchString(cfg.ObserverImage):
		return ContainerSpec{}, fmt.Errorf("%w: ObserverImage must be digest-pinned", ErrInvalidCodexReviewSpec)
	case !codexReviewOwnershipLabelValid(ownershipLabel):
		return ContainerSpec{}, fmt.Errorf("%w: ownership label is invalid", ErrInvalidCodexReviewSpec)
	}
	observerCfg := Config{
		WorkspaceTarget: cfg.WorkspaceTarget,
		BaseProofPath:   codexWorkspaceProofPath,
	}
	// The shared base-observer script, extended with the review-only .agents
	// probe: the shadow-mount topology depends on what the candidate tree
	// holds at <workspace>/.agents, and that fact must come from the same
	// pinned, networkless, read-only observation that proves HEAD and tree,
	// not from a host-side stat of a mutable checkout.
	script := observerScript(observerCfg, ownershipLabel.Value) + "; " +
		codexWorkspaceAgentsProbeScript(cfg.WorkspaceTarget, codexWorkspaceProofPath)
	return ContainerSpec{
		Name:            codexReviewWorkspaceObserverName(runID),
		Image:           cfg.ObserverImage,
		Command:         []string{"sh", "-c", script},
		NetworkDisabled: true,
		Mounts: []Mount{{
			Type: MountVolume, Source: volume, Target: cfg.WorkspaceTarget, ReadOnly: true,
		}},
		Labels: append(runLabels(runID), ownershipLabel),
	}, nil
}

// ObserveCodexReviewWorkspace validates the runtime-owned candidate volume,
// the observer container, and its nonce-bound proof. The returned value is
// the only workspace evidence accepted by the review topology.
func ObserveCodexReviewWorkspace(
	cfg CodexReviewConfig,
	runID, volume, expectedHead string,
	workspaceOwnershipLabel, observerOwnershipLabel Label,
	volumeReport VolumeSummary,
	observerReport InspectReport,
	proof []byte,
) (CodexReviewWorkspaceObservation, error) {
	spec, err := BuildCodexReviewWorkspaceObserverSpec(
		cfg, runID, volume, observerOwnershipLabel,
	)
	if err != nil {
		return CodexReviewWorkspaceObservation{}, err
	}
	if !codexReviewOwnershipLabelValid(workspaceOwnershipLabel) {
		return CodexReviewWorkspaceObservation{}, failf(
			CheckObservedBaseIdentity, "Codex review workspace ownership claim is invalid",
		)
	}
	if volumeReport.Name != volume {
		return CodexReviewWorkspaceObservation{}, failf(
			CheckObservedBaseIdentity, "Codex review workspace observation identified the wrong volume",
		)
	}
	fingerprint, err := ownedFingerprint(
		volumeReport.CreationDate, volumeReport.Labels,
		volumeReport.LabelsObserved, workspaceOwnershipLabel,
	)
	if err != nil || fingerprint == "" {
		return CodexReviewWorkspaceObservation{}, failf(
			CheckObservedBaseIdentity, "Codex review workspace ownership is not fingerprinted",
		)
	}
	if err := verifySeedRoleAllowlist(
		observerReport, spec, volume, cfg.WorkspaceTarget, CheckObservedBaseIdentity,
	); err != nil {
		return CodexReviewWorkspaceObservation{}, err
	}
	observerCreationFingerprint, err := ownedFingerprint(
		observerReport.CreationDate, observerReport.Labels,
		observerReport.LabelsObserved, observerOwnershipLabel,
	)
	if err != nil {
		return CodexReviewWorkspaceObservation{}, failf(
			CheckObservedBaseIdentity, "Codex review workspace observer ownership is not fingerprinted",
		)
	}
	observerFingerprint := codexReviewObserverFingerprint(
		observerCreationFingerprint, observerOwnershipLabel,
	)
	treeDigest, err := codexReviewProofTreeDigest(proof)
	if err != nil {
		return CodexReviewWorkspaceObservation{}, err
	}
	agentsEntry, err := codexReviewProofWorkspaceAgents(proof)
	if err != nil {
		return CodexReviewWorkspaceObservation{}, err
	}
	observedHead, err := verifyBaseProof(proof, observerOwnershipLabel.Value, treeDigest,
		map[string]string{codexWorkspaceAgentsKey: agentsEntry})
	if err != nil || observedHead != expectedHead {
		return CodexReviewWorkspaceObservation{}, failf(
			CheckObservedBaseIdentity, "Codex review workspace does not hold the requested head and tree",
		)
	}
	return CodexReviewWorkspaceObservation{
		volume: volume, fingerprint: fingerprint, head: observedHead,
		treeDigest: treeDigest, agentsEntry: agentsEntry, observerImage: cfg.ObserverImage,
		observerFingerprint: observerFingerprint,
	}, nil
}

func codexReviewObserverFingerprint(creationFingerprint string, ownershipLabel Label) string {
	return ownershipLabel.Value + ":" + creationFingerprint
}

// ObserveCodexReviewNetwork converts a current runtime inspection into the
// opaque host-only network evidence admitted by the review topology.
func ObserveCodexReviewNetwork(
	cfg CodexReviewConfig,
	runID string,
	ownershipLabel Label,
	report NetworkReport,
) (CodexReviewNetworkObservation, error) {
	if !codexReviewOwnershipLabelValid(ownershipLabel) {
		return CodexReviewNetworkObservation{}, failf(
			CheckAgentEgress, "Codex review provider network ownership claim is invalid",
		)
	}
	if report.Name != codexReviewNetworkName(runID) || report.Mode != NetworkHostOnly {
		return CodexReviewNetworkObservation{}, failf(
			CheckAgentEgress, "Codex review provider network identity or mode diverged",
		)
	}
	fingerprint, err := ownedFingerprint(
		report.CreationDate, report.Labels, report.LabelsObserved, ownershipLabel,
	)
	if err != nil || fingerprint == "" {
		return CodexReviewNetworkObservation{}, failf(
			CheckAgentEgress, "Codex review provider network ownership is not fingerprinted",
		)
	}
	proxyAuthority, err := proxyAddress(cfg.ProxyURL)
	if err != nil {
		return CodexReviewNetworkObservation{}, failf(CheckAgentEgress, "Codex review proxy URL is invalid")
	}
	proxyHost, _, err := net.SplitHostPort(proxyAuthority)
	if err != nil || proxyHost != report.IPv4Gateway || !codexReviewHostOnlyNetworkValid(report) {
		return CodexReviewNetworkObservation{}, failf(
			CheckAgentEgress, "Codex review proxy is not bound to the host-only network gateway",
		)
	}
	return CodexReviewNetworkObservation{
		name: report.Name, fingerprint: fingerprint, gateway: report.IPv4Gateway,
		subnet: report.IPv4Subnet, proxyAuthority: proxyAuthority,
	}, nil
}

// BuildCodexReviewAgentSpec admits the two prepared files, constructs the
// complete minimal container spec, independently re-verifies it, and returns
// a prepared shape. It does not authorize start; CodexReviewLifecycle.CodexReview supplies
// ownership, live reconstruction, durable binding, and the runtime start.
func BuildCodexReviewAgentSpec(
	cfg CodexReviewConfig,
	req CodexReviewSpec,
) (ContainerSpec, CodexReviewJournalBinding, error) {
	if err := validateCodexReviewRequest(cfg, req); err != nil {
		return ContainerSpec{}, CodexReviewJournalBinding{}, err
	}
	authPath, hostAuthBody, err := readCodexReviewInput(cfg.InputRoot, req.AuthSnapshot, maxCodexAuthSnapshotBytes)
	if err != nil {
		return ContainerSpec{}, CodexReviewJournalBinding{}, fmt.Errorf("%w: auth snapshot: %w", ErrInvalidCodexReviewSpec, err)
	}
	authBody, accessExpiry, err := codexReviewAgentAuthSnapshot(req.AuthMode, hostAuthBody)
	if err != nil {
		return ContainerSpec{}, CodexReviewJournalBinding{}, fmt.Errorf("%w: auth snapshot: %w", ErrInvalidCodexReviewSpec, err)
	}
	now := cfg.Now()
	if accessExpiry != nil && accessExpiry.Sub(now) < cfg.AccessTokenLifetimeFloor {
		return ContainerSpec{}, CodexReviewJournalBinding{}, fmt.Errorf(
			"%w: identity %q access token has %s remaining, floor %s",
			ErrInvalidCodexReviewSpec, req.AuthIdentityID,
			accessExpiry.Sub(now), cfg.AccessTokenLifetimeFloor,
		)
	}
	instructionPath, instructionBody, err := readCodexReviewInput(
		cfg.InputRoot, req.InstructionFile, domain.MaxVendorInstructionBytes,
	)
	if err != nil {
		return ContainerSpec{}, CodexReviewJournalBinding{}, fmt.Errorf("%w: instruction snapshot: %w", ErrInvalidCodexReviewSpec, err)
	}
	if !bytes.Equal(instructionBody, req.Instructions.Body) {
		return ContainerSpec{}, CodexReviewJournalBinding{}, fmt.Errorf(
			"%w: instruction snapshot does not match its admitted digest-bound body",
			ErrInvalidCodexReviewSpec,
		)
	}
	if authPath == instructionPath {
		return ContainerSpec{}, CodexReviewJournalBinding{}, fmt.Errorf(
			"%w: auth and instruction snapshots must be distinct files",
			ErrInvalidCodexReviewSpec,
		)
	}

	// The two admitted bodies are delivered on one ward-owned read-only volume.
	// The snapshot observation proves that volume already holds exactly these two
	// files; tie its per-file digests to the bytes admission just re-read so a
	// seeded volume that diverged from the admitted body cannot start.
	authSum := sha256.Sum256(authBody)
	wantAuthDigest := contentaddr.Format(authSum[:])
	instructionSum := sha256.Sum256(instructionBody)
	wantInstructionDigest := contentaddr.Format(instructionSum[:])
	if req.Snapshot.authDigest != wantAuthDigest || req.Snapshot.instructionDigest != wantInstructionDigest {
		return ContainerSpec{}, CodexReviewJournalBinding{}, failf(
			CheckCredentialSeparation, "Codex review snapshot volume does not hold the admitted credential and instruction bytes",
		)
	}

	shadowTargets := codexAgentsShadowTargets(cfg.WorkspaceTarget, req.Workspace.agentsEntry)
	env := append([]string{
		"HOME=" + CodexContainerHomeTarget,
		"CODEX_HOME=" + CodexHomeTarget,
	}, proxyEnvironment(cfg.ProxyURL)...)
	command := codexReviewCommand(cfg.WorkspaceTarget, cfg.Model, cfg.ReasoningEffort, req.Prompt)
	mounts := []Mount{
		{Type: MountVolume, Source: req.WorkspaceVolume, Target: cfg.WorkspaceTarget, ReadOnly: true},
		{Type: MountVolume, Source: req.Snapshot.volume, Target: codexReviewSnapshotTarget, ReadOnly: true},
	}
	for _, target := range shadowTargets {
		mounts = append(mounts, Mount{
			Type: MountVolume, Source: req.AgentsShadow.volume, Target: target, ReadOnly: true,
		})
	}
	spec := ContainerSpec{
		Name:    codexReviewContainerName(req.RunID),
		Image:   req.Image,
		Command: command,
		Env:     env,
		Mounts:  mounts,
		Labels:  runLabels(req.RunID),
		Network: codexReviewNetworkName(req.RunID),
	}
	binding := CodexReviewJournalBinding{
		TopologyVersion:                 codexReviewTopologyVersion,
		RunID:                           req.RunID,
		Boundary:                        req.Boundary,
		WorkspaceSourceRunID:            req.WorkspaceSourceRunID,
		WorkspaceVolume:                 req.WorkspaceVolume,
		WorkspaceFingerprint:            req.Workspace.fingerprint,
		WorkspaceHead:                   req.Workspace.head,
		WorkspaceTreeDigest:             req.Workspace.treeDigest,
		WorkspaceAgentsEntry:            req.Workspace.agentsEntry,
		WorkspaceObserverImage:          req.Workspace.observerImage,
		WorkspaceObserverFingerprint:    req.Workspace.observerFingerprint,
		WorkspaceTarget:                 cfg.WorkspaceTarget,
		WorkspaceReadOnly:               true,
		HomeTarget:                      CodexContainerHomeTarget,
		CodexHomeTarget:                 CodexHomeTarget,
		FreshContext:                    true,
		ContinuityMounted:               false,
		AuthMode:                        req.AuthMode,
		AuthIdentityID:                  req.AuthIdentityID,
		AuthSnapshotDigest:              wantAuthDigest,
		AccessTokenExpiresAt:            accessExpiry,
		AuthReadOnly:                    true,
		AuthStoreMutationLeaseRequired:  true,
		InstructionDigest:               req.Instructions.Digest,
		InstructionCompositionVersion:   req.InstructionBinding.CompositionVersion,
		HostInstructionDigest:           cloneOptionalDigest(req.InstructionBinding.HostDigest),
		RepositoryInstructionSources:    slices.Clone(req.InstructionBinding.RepositorySources),
		InstructionReadOnly:             true,
		SnapshotVolume:                  req.Snapshot.volume,
		SnapshotTarget:                  codexReviewSnapshotTarget,
		SnapshotFingerprint:             req.Snapshot.fingerprint,
		SnapshotObserverImage:           req.Snapshot.observerImage,
		SnapshotObserverFingerprint:     req.Snapshot.observerFingerprint,
		SnapshotReadOnly:                true,
		AgentsShadowVolume:              req.AgentsShadow.volume,
		AgentsShadowFingerprint:         req.AgentsShadow.fingerprint,
		AgentsShadowDigest:              req.AgentsShadow.digest,
		AgentsShadowObserverImage:       req.AgentsShadow.observerImage,
		AgentsShadowObserverFingerprint: req.AgentsShadow.observerFingerprint,
		AgentsShadowTargets:             slices.Clone(shadowTargets),
		AgentsShadowReadOnly:            true,
		ProviderEndpoints:               slices.Clone(cfg.ProviderEndpoints),
		ProviderNetwork:                 req.Network.name,
		ProviderNetworkFingerprint:      req.Network.fingerprint,
		ProviderNetworkHostOnly:         true,
		ProviderNetworkGateway:          req.Network.gateway,
		ProviderNetworkSubnet:           req.Network.subnet,
		ProviderProxyAuthority:          req.Network.proxyAuthority,
		RefreshEndpointReachable:        false,
		PublicationCredentials:          false,
		LauncherEnvironmentDigest:       digestEnvironment(env),
		CommandDigest:                   digestStrings(command),
	}
	if err := validateCodexReviewAgentSpec(cfg, req, spec, binding); err != nil {
		return ContainerSpec{}, CodexReviewJournalBinding{}, err
	}
	return spec, binding, nil
}

// validateShape checks the decoded binding's structural posture only. It is
// deliberately not an authorization gate: CodexReview reloads the durable
// record and reconstructs every runtime observation before start.
func (b CodexReviewJournalBinding) validateShape() error {
	return b.validate(true, false)
}

// validateForTeardown accepts the historical shapes that predate the current
// contract: the instruction binding without explicit composition provenance,
// and the v2 topology without the observed workspace .agents entry. It is
// used only to authenticate cleanup targets; legacy authority can never
// launch or satisfy a review result.
func (b CodexReviewJournalBinding) validateForTeardown() error {
	return b.validate(true, true)
}

func (b CodexReviewJournalBinding) validatePrepared() error {
	if b.AgentsShadowPreStartObserverFingerprint != "" ||
		b.WorkspacePreStartObserverFingerprint != "" ||
		b.SnapshotPreStartObserverFingerprint != "" {
		return errors.New("codex review prepared journal binding carries pre-start evidence")
	}
	return b.validate(false, false)
}

func (b CodexReviewJournalBinding) validate(
	requirePreStartObservation, allowLegacyTeardownBinding bool,
) error {
	topologyVersionValid := b.TopologyVersion == codexReviewTopologyVersion
	if allowLegacyTeardownBinding && b.TopologyVersion == codexReviewTopologyVersionV2 {
		topologyVersionValid = true
	}
	if !topologyVersionValid ||
		!runIDPattern.MatchString(b.RunID) ||
		b.Boundary != CodexReviewFreshStart || !b.FreshContext || b.ContinuityMounted ||
		!b.WorkspaceReadOnly || !b.AuthReadOnly || !b.AuthStoreMutationLeaseRequired ||
		!b.InstructionReadOnly || !b.SnapshotReadOnly || !b.AgentsShadowReadOnly ||
		b.RefreshEndpointReachable || b.PublicationCredentials {
		return errors.New("codex review journal posture is invalid")
	}
	if !runIDPattern.MatchString(b.WorkspaceSourceRunID) ||
		b.WorkspaceVolume == "" || !cliSafe(b.WorkspaceVolume) || !cleanAbs(b.WorkspaceTarget) ||
		!cliSafe(b.WorkspaceTarget) ||
		codexReviewWorkspaceOverlapsControlPath(b.WorkspaceTarget) ||
		b.HomeTarget != CodexContainerHomeTarget || b.CodexHomeTarget != CodexHomeTarget {
		return errors.New("codex review journal mount targets are invalid")
	}
	workspaceAgents := b.WorkspaceAgentsEntry
	workspaceAgentsValid := workspaceAgents == codexWorkspaceAgentsDir ||
		workspaceAgents == codexWorkspaceAgentsAbsent
	if allowLegacyTeardownBinding && b.TopologyVersion == codexReviewTopologyVersionV2 &&
		workspaceAgents == "" {
		// v2 predates the observed .agents entry and always shadowed the
		// workspace root; legacy authority reaps, it never launches.
		workspaceAgentsValid = true
		workspaceAgents = codexWorkspaceAgentsDir
	}
	_, workspaceTreeDigestOK := contentaddr.FromHex(b.WorkspaceTreeDigest)
	if b.WorkspaceFingerprint == "" || !commitSHAPattern.MatchString(b.WorkspaceHead) ||
		!workspaceTreeDigestOK || !workspaceAgentsValid ||
		!digestPinnedImagePattern.MatchString(b.WorkspaceObserverImage) ||
		b.WorkspaceObserverFingerprint == "" {
		return errors.New("codex review journal workspace evidence is invalid")
	}
	if requirePreStartObservation &&
		(b.WorkspacePreStartObserverFingerprint == "" ||
			b.WorkspacePreStartObserverFingerprint == b.WorkspaceObserverFingerprint) {
		return errors.New("codex review journal omits distinct pre-start workspace evidence")
	}
	instructionBindingValid := b.instructionBinding().Validate() == nil
	if allowLegacyTeardownBinding && b.InstructionCompositionVersion == "" &&
		b.HostInstructionDigest == nil && len(b.RepositoryInstructionSources) == 0 &&
		contentaddr.Valid(string(b.InstructionDigest)) {
		instructionBindingValid = true
	}
	if !b.AuthMode.valid() || b.AuthIdentityID == "" ||
		!contentaddr.Valid(b.AuthSnapshotDigest) || !instructionBindingValid {
		return errors.New("codex review journal credential or instruction binding is invalid")
	}
	if b.AuthMode == CodexAuthSubscription && b.AccessTokenExpiresAt == nil {
		return errors.New("codex review subscription binding omits access-token expiry")
	}
	if b.AuthMode == CodexAuthAPIKey && b.AccessTokenExpiresAt != nil {
		return errors.New("codex review API-key binding carries access-token expiry")
	}
	if b.SnapshotVolume == "" || !cliSafe(b.SnapshotVolume) || b.SnapshotVolume != codexReviewSnapshotVolumeName(b.RunID) ||
		b.SnapshotTarget != codexReviewSnapshotTarget || b.SnapshotFingerprint == "" ||
		!digestPinnedImagePattern.MatchString(b.SnapshotObserverImage) ||
		b.SnapshotObserverFingerprint == "" {
		return errors.New("codex review journal snapshot binding is invalid")
	}
	if requirePreStartObservation &&
		(b.SnapshotPreStartObserverFingerprint == "" ||
			b.SnapshotPreStartObserverFingerprint == b.SnapshotObserverFingerprint) {
		return errors.New("codex review journal omits distinct pre-start snapshot evidence")
	}
	if b.AgentsShadowVolume == "" || !cliSafe(b.AgentsShadowVolume) ||
		b.AgentsShadowFingerprint == "" || b.AgentsShadowDigest != emptyCodexShadowDigest ||
		!digestPinnedImagePattern.MatchString(b.AgentsShadowObserverImage) ||
		b.AgentsShadowObserverFingerprint == "" ||
		!slices.Equal(b.AgentsShadowTargets, codexAgentsShadowTargets(b.WorkspaceTarget, workspaceAgents)) {
		return errors.New("codex review journal .agents shadow binding is invalid")
	}
	if requirePreStartObservation &&
		(b.AgentsShadowPreStartObserverFingerprint == "" ||
			b.AgentsShadowPreStartObserverFingerprint == b.AgentsShadowObserverFingerprint) {
		return errors.New("codex review journal omits distinct pre-start shadow evidence")
	}
	if requirePreStartObservation &&
		(b.ReviewContainer != codexReviewContainerName(b.RunID) ||
			b.ReviewContainerFingerprint == "" ||
			!ownershipTokenPattern.MatchString(b.ReviewOwnershipToken)) {
		return errors.New("codex review journal container ownership evidence is invalid")
	}
	if !slices.Equal(b.ProviderEndpoints, b.AuthMode.providerEndpoints()) ||
		b.ProviderNetwork != codexReviewNetworkName(b.RunID) || !cliSafe(b.ProviderNetwork) ||
		b.ProviderNetworkFingerprint == "" || b.ProviderNetworkGateway == "" ||
		!b.ProviderNetworkHostOnly || b.ProviderNetworkSubnet == "" || b.ProviderProxyAuthority == "" ||
		!codexReviewJournalNetworkValid(b) ||
		!contentaddr.Valid(b.LauncherEnvironmentDigest) || !contentaddr.Valid(b.CommandDigest) {
		return errors.New("codex review journal egress, environment, or command binding is invalid")
	}
	return nil
}

func validateCodexReviewRequest(cfg CodexReviewConfig, req CodexReviewSpec) error {
	switch {
	case !runIDPattern.MatchString(req.RunID):
		return fmt.Errorf("%w: RunID is invalid", ErrInvalidCodexReviewSpec)
	case !runIDPattern.MatchString(req.WorkspaceSourceRunID):
		return fmt.Errorf("%w: WorkspaceSourceRunID is invalid", ErrInvalidCodexReviewSpec)
	case !digestPinnedImagePattern.MatchString(req.Image):
		return fmt.Errorf("%w: Image must be digest-pinned", ErrInvalidCodexReviewSpec)
	case !digestPinnedImagePattern.MatchString(cfg.ApprovedImage) || !sameImage(cfg.ApprovedImage, req.Image):
		return fmt.Errorf("%w: Image does not match the deployment-approved Codex pin", ErrInvalidCodexReviewSpec)
	case !cleanAbs(cfg.WorkspaceTarget):
		return fmt.Errorf("%w: WorkspaceTarget must be a clean absolute non-root path", ErrInvalidCodexReviewSpec)
	case !cliSafe(cfg.WorkspaceTarget):
		return fmt.Errorf("%w: WorkspaceTarget must be CLI-safe", ErrInvalidCodexReviewSpec)
	case codexReviewWorkspaceOverlapsControlPath(cfg.WorkspaceTarget):
		return fmt.Errorf("%w: WorkspaceTarget overlaps a protected Codex review path", ErrInvalidCodexReviewSpec)
	case req.WorkspaceVolume == "" || !cliSafe(req.WorkspaceVolume):
		return fmt.Errorf("%w: WorkspaceVolume is required and must be CLI-safe", ErrInvalidCodexReviewSpec)
	case !req.Workspace.valid() || req.Workspace.volume != req.WorkspaceVolume ||
		req.Workspace.observerImage != cfg.ObserverImage:
		return fmt.Errorf("%w: runtime-backed workspace observation is required", ErrInvalidCodexReviewSpec)
	case !req.Network.valid() || req.Network.name != codexReviewNetworkName(req.RunID):
		return fmt.Errorf("%w: runtime-backed provider network observation is required", ErrInvalidCodexReviewSpec)
	case req.Prompt == "" || strings.IndexByte(req.Prompt, 0) >= 0 ||
		len(req.Prompt) > maxCodexReviewPromptBytes:
		return fmt.Errorf("%w: Prompt must be nonempty, NUL-free, and at most %d bytes",
			ErrInvalidCodexReviewSpec, maxCodexReviewPromptBytes)
	case !req.Boundary.valid():
		return fmt.Errorf("%w: unsupported boundary %q", ErrInvalidCodexReviewSpec, req.Boundary)
	case req.Boundary != CodexReviewFreshStart:
		return fmt.Errorf("%w: %w", ErrInvalidCodexReviewSpec, ErrCodexReviewContinuityRefused)
	case !req.AuthMode.valid():
		return fmt.Errorf("%w: unsupported auth mode %q", ErrInvalidCodexReviewSpec, req.AuthMode)
	case req.AuthIdentityID == "":
		return fmt.Errorf("%w: AuthIdentityID is required", ErrInvalidCodexReviewSpec)
	case !req.AgentsShadow.valid() || req.AgentsShadow.observerImage != cfg.ObserverImage:
		return fmt.Errorf("%w: runtime-backed empty shadow observation is required", ErrInvalidCodexReviewSpec)
	case !req.Snapshot.valid() || req.Snapshot.observerImage != cfg.ObserverImage ||
		req.Snapshot.volume != codexReviewSnapshotVolumeName(req.RunID):
		return fmt.Errorf("%w: runtime-backed snapshot volume observation is required", ErrInvalidCodexReviewSpec)
	case !cleanAbs(cfg.InputRoot):
		return fmt.Errorf("%w: InputRoot must be a clean absolute non-root path", ErrInvalidCodexReviewSpec)
	case cfg.AccessTokenLifetimeFloor <= 0:
		return fmt.Errorf("%w: AccessTokenLifetimeFloor must be positive", ErrInvalidCodexReviewSpec)
	case codexAuthRefreshThreshold(cfg) <= cfg.AccessTokenLifetimeFloor:
		return fmt.Errorf("%w: AccessTokenRefreshThreshold must exceed AccessTokenLifetimeFloor", ErrInvalidCodexReviewSpec)
	case cfg.Now == nil:
		return fmt.Errorf("%w: Now is required", ErrInvalidCodexReviewSpec)
	case !slices.Equal(cfg.ProviderEndpoints, req.AuthMode.providerEndpoints()):
		return fmt.Errorf("%w: provider_only endpoints do not exactly match auth mode", ErrInvalidCodexReviewSpec)
	case slices.Contains(cfg.ProviderEndpoints, "auth.openai.com:443"):
		return fmt.Errorf("%w: refresh endpoint is forbidden in the review profile", ErrInvalidCodexReviewSpec)
	}
	if _, err := proxyAddress(cfg.ProxyURL); err != nil {
		return fmt.Errorf("%w: ProxyURL is invalid", ErrInvalidCodexReviewSpec)
	}
	now := cfg.Now()
	if now.IsZero() || now.Location() != time.UTC {
		return fmt.Errorf("%w: Now must return a nonzero UTC instant", ErrInvalidCodexReviewSpec)
	}
	if req.Network.proxyAuthority != mustProxyAddress(cfg.ProxyURL) {
		return fmt.Errorf("%w: provider network observation does not bind the configured proxy", ErrInvalidCodexReviewSpec)
	}
	if err := req.Instructions.validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidCodexReviewSpec, err)
	}
	if req.Instructions.Vendor != domain.AgentVendorCodex || !req.Instructions.Present {
		return fmt.Errorf("%w: a present Codex append-file instruction is required", ErrInvalidCodexReviewSpec)
	}
	if err := req.InstructionBinding.Validate(); err != nil ||
		req.InstructionBinding.ResultDigest != req.Instructions.Digest {
		return fmt.Errorf("%w: review instruction provenance is invalid", ErrInvalidCodexReviewSpec)
	}
	return nil
}

func (b CodexReviewJournalBinding) instructionBinding() exec.ReviewInstructionBinding {
	return exec.ReviewInstructionBinding{
		CompositionVersion: b.InstructionCompositionVersion,
		HostDigest:         cloneOptionalDigest(b.HostInstructionDigest),
		RepositorySources:  slices.Clone(b.RepositoryInstructionSources),
		ResultDigest:       b.InstructionDigest,
	}
}

func cloneOptionalDigest(in *domain.Digest) *domain.Digest {
	if in == nil {
		return nil
	}
	digest := *in
	return &digest
}

type codexReviewInputMetadata struct {
	Mode     os.FileMode
	UID, GID uint32
	Device   string
	Ino      uint64
}

func readCodexReviewInput(root, file string, limit int64) (string, []byte, error) {
	path, body, _, err := readCodexReviewInputWithMetadata(root, file, limit)
	return path, body, err
}

func readCodexReviewInputWithMetadata(
	root, file string, limit int64,
) (string, []byte, codexReviewInputMetadata, error) {
	rootInfo, err := os.Lstat(root)
	var rootStat *syscall.Stat_t
	rootStatOK := false
	if rootInfo != nil {
		rootStat, rootStatOK = rootInfo.Sys().(*syscall.Stat_t)
	}
	rootOwned := rootStatOK && codexReviewUIDMatches(rootStat, os.Geteuid())
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode().Perm()&0o077 != 0 || !rootOwned {
		return "", nil, codexReviewInputMetadata{}, errors.New("input root is not a private directory")
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", nil, codexReviewInputMetadata{}, errors.New("input root cannot be resolved")
	}
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() {
		return "", nil, codexReviewInputMetadata{}, errors.New("input is not a regular file")
	}
	resolvedFile, err := filepath.EvalSymlinks(file)
	if err != nil {
		return "", nil, codexReviewInputMetadata{}, errors.New("input cannot be resolved")
	}
	rel, err := filepath.Rel(resolvedRoot, resolvedFile)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", nil, codexReviewInputMetadata{}, errors.New("input resolves outside the trusted root")
	}
	if !cliSafe(resolvedFile) {
		return "", nil, codexReviewInputMetadata{}, errors.New("input path is not CLI-safe")
	}
	f, err := os.OpenFile(resolvedFile, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", nil, codexReviewInputMetadata{}, errors.New("input cannot be opened")
	}
	defer f.Close() //nolint:errcheck // read-only file
	openedInfo, err := f.Stat()
	if err != nil || !os.SameFile(info, openedInfo) {
		return "", nil, codexReviewInputMetadata{}, errors.New("input changed while it was admitted")
	}
	stat, ok := openedInfo.Sys().(*syscall.Stat_t)
	if !openedInfo.Mode().IsRegular() || openedInfo.Mode().Perm()&0o077 != 0 ||
		openedInfo.Mode().Perm()&0o400 == 0 || !ok || stat.Nlink != 1 ||
		!codexReviewUIDMatches(stat, os.Geteuid()) {
		return "", nil, codexReviewInputMetadata{}, errors.New("input is not a private, singly linked regular file")
	}
	body, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return "", nil, codexReviewInputMetadata{}, errors.New("input cannot be read")
	}
	if int64(len(body)) > limit {
		return "", nil, codexReviewInputMetadata{}, errors.New("input exceeds its byte limit")
	}
	metadata := codexReviewInputMetadata{
		Mode: openedInfo.Mode().Perm(), UID: stat.Uid, GID: stat.Gid,
		Device: fmt.Sprint(stat.Dev), Ino: uint64(stat.Ino),
	}
	return resolvedFile, body, metadata, nil
}

func codexReviewUIDMatches(stat *syscall.Stat_t, euid int) bool {
	return stat != nil && euid >= 0 && uint64(stat.Uid) == uint64(euid)
}

type codexAuthTokens struct {
	IDToken      string  `json:"id_token"`
	AccessToken  string  `json:"access_token"`
	RefreshToken *string `json:"refresh_token"`
	AccountID    *string `json:"account_id,omitempty"`
}

type codexAuthFile struct {
	AuthMode     string           `json:"auth_mode,omitempty"`
	OpenAIAPIKey *string          `json:"OPENAI_API_KEY"`
	Tokens       *codexAuthTokens `json:"tokens"`
	LastRefresh  json.RawMessage  `json:"last_refresh,omitempty"`
}

func inspectCodexAuthSnapshot(mode CodexAuthMode, body []byte) (*time.Time, error) {
	auth, expires, err := inspectCodexHostAuth(mode, body)
	if err != nil {
		return nil, err
	}
	if mode == CodexAuthSubscription && auth.Tokens != nil && auth.Tokens.RefreshToken != nil &&
		*auth.Tokens.RefreshToken != "" {
		return nil, errors.New("subscription snapshot carries a refresh token")
	}
	return expires, nil
}

func inspectCodexHostAuth(
	mode CodexAuthMode, body []byte,
) (codexAuthFile, *time.Time, error) {
	var auth codexAuthFile
	if err := strictjson.Decode(
		body, &auth, strictjson.TolerateInvalidUTF8, strictjson.Limit(maxCodexAuthSnapshotBytes),
	); err != nil {
		if errors.Is(err, strictjson.ErrTrailingData) {
			return codexAuthFile{}, nil, errors.New("auth.json carries trailing content")
		}
		return codexAuthFile{}, nil, errors.New("auth.json is malformed or carries an unknown field")
	}
	if len(auth.LastRefresh) != 0 && !bytes.Equal(auth.LastRefresh, []byte("null")) {
		var refreshed time.Time
		if err := json.Unmarshal(auth.LastRefresh, &refreshed); err != nil || refreshed.IsZero() {
			return codexAuthFile{}, nil, errors.New("auth.json last_refresh is not an instant or null")
		}
	}
	switch mode {
	case CodexAuthSubscription:
		if auth.AuthMode != "" && auth.AuthMode != "chatgpt" {
			return codexAuthFile{}, nil, errors.New("auth.json mode is not chatgpt")
		}
		if auth.OpenAIAPIKey != nil && *auth.OpenAIAPIKey != "" {
			return codexAuthFile{}, nil, errors.New("subscription snapshot also carries an API key")
		}
		if auth.Tokens == nil || auth.Tokens.AccessToken == "" {
			return codexAuthFile{}, nil, errors.New("subscription snapshot carries no access token")
		}
		if auth.Tokens.RefreshToken == nil {
			return codexAuthFile{}, nil, errors.New("subscription snapshot carries no explicit refresh-token field")
		}
		if codexAuthRefreshTokenAliased(auth.Tokens) {
			return codexAuthFile{}, nil, errors.New("subscription refresh token is aliased into an agent-visible field")
		}
		expires, err := jwtExpiry(auth.Tokens.AccessToken)
		if err != nil {
			return codexAuthFile{}, nil, err
		}
		return auth, &expires, nil
	case CodexAuthAPIKey:
		if auth.AuthMode != "" && auth.AuthMode != "apikey" && auth.AuthMode != "api_key" {
			return codexAuthFile{}, nil, errors.New("auth.json mode is not API key")
		}
		if auth.OpenAIAPIKey == nil || *auth.OpenAIAPIKey == "" {
			return codexAuthFile{}, nil, errors.New("API-key snapshot carries no key")
		}
		if auth.Tokens != nil {
			return codexAuthFile{}, nil, errors.New("API-key snapshot also carries token credentials")
		}
		return auth, nil, nil
	}
	return codexAuthFile{}, nil, errors.New("unsupported auth mode")
}

func codexAuthRefreshTokenAliased(tokens *codexAuthTokens) bool {
	if tokens == nil || tokens.RefreshToken == nil || *tokens.RefreshToken == "" {
		return false
	}
	return codexAuthTokensExpose(tokens, *tokens.RefreshToken)
}

func codexAuthTokensExpose(tokens *codexAuthTokens, secret string) bool {
	if tokens == nil || secret == "" {
		return false
	}
	if strings.Contains(tokens.IDToken, secret) || strings.Contains(tokens.AccessToken, secret) {
		return true
	}
	return tokens.AccountID != nil && strings.Contains(*tokens.AccountID, secret)
}

// codexReviewAgentAuthSnapshot derives the container-facing credential from
// the host store. The host retains the rotating refresh token; the reviewer
// receives byte-identical input only when it is already access-token-only.
func codexReviewAgentAuthSnapshot(
	mode CodexAuthMode, body []byte,
) ([]byte, *time.Time, error) {
	auth, expires, err := inspectCodexHostAuth(mode, body)
	if err != nil {
		return nil, nil, err
	}
	if mode != CodexAuthSubscription || auth.Tokens == nil ||
		auth.Tokens.RefreshToken == nil || *auth.Tokens.RefreshToken == "" {
		return bytes.Clone(body), expires, nil
	}
	empty := ""
	auth.Tokens.RefreshToken = &empty
	snapshot, err := json.Marshal(auth)
	if err != nil {
		return nil, nil, errors.New("auth.json snapshot cannot be encoded")
	}
	return snapshot, expires, nil
}

func jwtExpiry(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, errors.New("access token is not a compact JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, errors.New("access token payload is not base64url")
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var claims map[string]any
	if err := dec.Decode(&claims); err != nil {
		return time.Time{}, errors.New("access token payload is not JSON")
	}
	exp, ok := claims["exp"].(json.Number)
	if !ok {
		return time.Time{}, errors.New("access token carries no numeric exp claim")
	}
	seconds, err := exp.Int64()
	if err != nil || seconds <= 0 {
		return time.Time{}, errors.New("access token exp claim is invalid")
	}
	return time.Unix(seconds, 0).UTC(), nil
}

func codexReviewCommand(workspaceTarget, model, reasoningEffort, prompt string) []string {
	schema := `{"type":"object","properties":{"findings":{"type":"array","items":{"type":"object","properties":{"severity":{"type":"string","enum":["P1","P2","P3"]},"location":{"type":"string"},"explanation":{"type":"string"}},"required":["severity","location","explanation"],"additionalProperties":false}}},"required":["findings"],"additionalProperties":false}`
	// CODEX_HOME lives on the fresh, writable container rootfs; auth.json and
	// AGENTS.md are symlinks into the read-only snapshot volume, so the credential
	// bytes stay immutable while CODEX_HOME itself remains writable for the CLI's
	// own scratch state. Apple container rejects single-file binds (#591), so the
	// two files arrive on one seeded volume and are linked into place here.
	//
	// The setup prologue runs under `set -e` so it fails closed: if a derived or
	// updated review image already ships either fixed CODEX_HOME path, the `ln -s`
	// hits "File exists" and aborts the container before `codex exec` runs, rather
	// than silently leaving codex to read the image-provided credential or
	// instruction. `set +e` is relaxed only immediately before `codex exec`, so a
	// nonzero codex exit is still captured into review_status and the status file.
	command := "set -e; mkdir -p " + shellQuote(CodexHomeTarget) + "; " +
		"ln -s " + shellQuote(codexReviewSnapshotAuthSource) + " " + shellQuote(CodexAuthFileTarget) + "; " +
		"ln -s " + shellQuote(codexReviewSnapshotInstrSource) + " " + shellQuote(CodexInstructionTarget) + "; " +
		"mkdir -p " + shellQuote(codexReviewOutputDir) + "; " +
		"printf '%s' " + shellQuote(schema) + " > " + shellQuote(codexReviewSchemaPath) + "; " +
		"set +e; codex exec --json --ephemeral --skip-git-repo-check -s read-only -C " + shellQuote(workspaceTarget) +
		" -m " + shellQuote(model) + " -c " + shellQuote("model_reasoning_effort=\""+reasoningEffort+"\"") +
		" -c project_doc_max_bytes=0 --ignore-user-config --ignore-rules" +
		" --output-schema " + shellQuote(codexReviewSchemaPath) +
		" --output-last-message " + shellQuote(codexReviewResultPath) +
		" -- " + shellQuote(prompt) + " > " + shellQuote(codexReviewEventsPath) + " 2>&1; " +
		"review_status=$?; printf '%s\\n' \"$review_status\" > " + shellQuote(codexReviewStatusPath) +
		"; exit \"$review_status\""
	return []string{"sh", "-c", command}
}

type codexReviewResourceNames struct {
	workspaceObserver string
	shadowInitializer string
	shadowObserver    string
	reviewContainer   string
	shadowVolume      string
	network           string
	// snapshot resources are empty on the pre-#591 generation, which delivered
	// the two files as read-only host binds instead of a seeded volume. An empty
	// snapshotVolume selects the six-resource legacy shape in resourceNamesMatch.
	snapshotVolume   string
	snapshotSeeder   string
	snapshotObserver string
}

// codexReviewNames is the single registration point for runtime resources
// owned by one review invocation. It preserves the complete admitted run ID,
// so names remain collision-resistant without truncation or a hash alias.
func codexReviewNames(runID string) codexReviewResourceNames {
	prefix := "freeside-review-" + runID
	return codexReviewResourceNames{
		workspaceObserver: prefix + "-ws-obs",
		shadowInitializer: prefix + "-agents-init",
		shadowObserver:    prefix + "-agents-obs",
		reviewContainer:   prefix + "-codex",
		shadowVolume:      prefix + "-agents",
		network:           prefix + "-egress",
		snapshotVolume:    prefix + "-snap",
		snapshotSeeder:    prefix + "-snap-init",
		snapshotObserver:  prefix + "-snap-obs",
	}
}

// legacyCodexReviewNames re-derives the exact topology persisted before #587.
// It is accepted only while authenticating existing intents for cleanup; no
// new resource is ever created with these names. It carries no snapshot
// resources: pre-#587 launches used host binds, so their intents have the
// six-resource shape.
func legacyCodexReviewNames(runID string) codexReviewResourceNames {
	prefix := "freeside-review-" + runID
	return codexReviewResourceNames{
		workspaceObserver: prefix + "-workspace-observer",
		shadowInitializer: prefix + "-agents-init",
		shadowObserver:    prefix + "-agents-observer",
		reviewContainer:   prefix + "-codex",
		shadowVolume:      prefix + "-agents",
		network:           prefix + "-egress",
	}
}

// preSnapshotCodexReviewNames re-derives the #587..#590 topology: the current
// short names but still the six-resource, host-bind shape from before the
// snapshot volume (#591). It authenticates the round-2 intent persisted by
// run 482 for cleanup only; no new resource is created with this shape.
func preSnapshotCodexReviewNames(runID string) codexReviewResourceNames {
	prefix := "freeside-review-" + runID
	return codexReviewResourceNames{
		workspaceObserver: prefix + "-ws-obs",
		shadowInitializer: prefix + "-agents-init",
		shadowObserver:    prefix + "-agents-obs",
		reviewContainer:   prefix + "-codex",
		shadowVolume:      prefix + "-agents",
		network:           prefix + "-egress",
	}
}

func codexReviewShadowObserverName(runID string) string {
	return codexReviewNames(runID).shadowObserver
}

func codexReviewSnapshotObserverName(runID string) string {
	return codexReviewNames(runID).snapshotObserver
}

func codexReviewSnapshotSeederName(runID string) string {
	return codexReviewNames(runID).snapshotSeeder
}

func codexReviewSnapshotVolumeName(runID string) string {
	return codexReviewNames(runID).snapshotVolume
}

func codexReviewShadowObserverScript(nonce, targetPath, proofPath string) string {
	target := shellQuote(targetPath)
	proof := shellQuote(proofPath)
	return "LC_ALL=C; export LC_ALL; empty=no; " +
		"if entries=\"$(cd " + target + " 2>/dev/null && find . ! -name . -print 2>/dev/null)\" && " +
		"[ -z \"$entries\" ]; then empty=yes; fi; " +
		"printf 'nonce=%s\\nempty=%s\\ntree=%s\\n' " + shellQuote(nonce) +
		" \"$empty\" " + shellQuote(emptyCodexShadowDigest) + " > " + proof
}

func verifyCodexReviewShadowObserverAllowlist(rep InspectReport, spec ContainerSpec) error {
	if rep.ID != spec.Name || !rep.AllowlistFieldsObserved || rep.State != StateStopped ||
		!sameImage(spec.Image, rep.ImageReference) || rep.WorkingDirectory != "/" ||
		!slices.Equal(rep.Command, spec.Command) ||
		!sameEnvironment(rep.Env, []string{fixedContainerPathEnv}) {
		return failf(CheckControlPlaneIsolation, "Codex review shadow observer identity or execution shape diverged")
	}
	if !sameMounts(rep.Mounts, spec.Mounts) || rep.SSH ||
		len(rep.PublishedSockets) != 0 || len(rep.PublishedPorts) != 0 ||
		!rep.NetworksObserved || rep.NetworkAttachmentCount != 0 || len(rep.Networks) != 0 {
		return failf(CheckControlPlaneIsolation, "Codex review shadow observer containment diverged")
	}
	return nil
}

// codexAgentsShadowTargets derives the empty-shadow mount set from the
// observed workspace .agents entry. The ambient locations (the reviewer's
// home and every in-container ancestor of the workspace) sit on the
// container's writable rootfs and are always shadowed. The workspace's own
// .agents lies inside the read-only candidate mount, where the runtime can
// mount over an existing directory but cannot create a missing mountpoint,
// so it is shadowed exactly when the attested tree carries the directory;
// the immutable read-only workspace cannot grow one after observation.
func codexAgentsShadowTargets(workspaceTarget, workspaceAgents string) []string {
	seen := map[string]struct{}{path.Join(CodexContainerHomeTarget, ".agents"): {}}
	if workspaceAgents == codexWorkspaceAgentsDir {
		seen[path.Join(path.Clean(workspaceTarget), ".agents")] = struct{}{}
	}
	for current := path.Dir(path.Clean(workspaceTarget)); ; current = path.Dir(current) {
		seen[path.Join(current, ".agents")] = struct{}{}
		if current == "/" {
			break
		}
	}
	targets := make([]string, 0, len(seen))
	for target := range seen {
		targets = append(targets, target)
	}
	sort.Strings(targets)
	return targets
}

// codexWorkspaceAgentsProbeScript classifies the workspace's own .agents
// entry for the observer proof: "dir" for a real directory, "absent" for no
// entry, "other" for a symlink or any non-directory kind. The symlink test
// comes first because -d follows links; "other" is deliberately emitted
// rather than suppressed so the host refuses it instead of defaulting.
func codexWorkspaceAgentsProbeScript(workspaceTarget, proofPath string) string {
	agents := shellQuote(path.Join(path.Clean(workspaceTarget), ".agents"))
	proof := shellQuote(proofPath)
	return "a=absent; if [ -h " + agents + " ]; then a=other; " +
		"elif [ -d " + agents + " ]; then a=" + codexWorkspaceAgentsDir + "; " +
		"elif [ -e " + agents + " ]; then a=other; fi; " +
		"printf '" + codexWorkspaceAgentsKey + "=%s\\n' \"$a\" >> " + proof + "; sync"
}

// codexReviewProofWorkspaceAgents extracts the observed workspace .agents
// entry from the proof. Anything but the two launchable values fails closed:
// a symlink or non-directory .agents never reaches a review container.
func codexReviewProofWorkspaceAgents(proof []byte) (string, error) {
	if len(proof) == 0 || len(proof) > maxBaseProofBytes {
		return "", failf(CheckObservedBaseIdentity, "Codex review workspace proof has an invalid size")
	}
	var entry string
	for _, line := range strings.Split(strings.ReplaceAll(string(proof), "\r\n", "\n"), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || key != codexWorkspaceAgentsKey {
			continue
		}
		if entry != "" {
			return "", failf(CheckObservedBaseIdentity, "Codex review workspace proof repeats the .agents entry")
		}
		switch value {
		case codexWorkspaceAgentsDir, codexWorkspaceAgentsAbsent:
			entry = value
		default:
			return "", failf(CheckControlPlaneIsolation,
				"Codex review workspace .agents entry is neither a directory nor absent")
		}
	}
	if entry == "" {
		return "", failf(CheckObservedBaseIdentity, "Codex review workspace proof omits the .agents entry")
	}
	return entry, nil
}

func codexReviewWorkspaceOverlapsControlPath(workspaceTarget string) bool {
	for current := workspaceTarget; current != "/"; current = path.Dir(current) {
		if path.Base(current) == ".agents" {
			return true
		}
	}
	for _, protected := range []string{
		"/bin",
		"/dev",
		"/etc",
		"/lib",
		"/lib64",
		"/proc",
		"/run",
		"/sbin",
		"/sys",
		"/usr",
		CodexHomeTarget,
		CodexContainerHomeTarget,
		CodexAuthFileTarget,
		CodexInstructionTarget,
		"/.agents",
		path.Join(CodexContainerHomeTarget, ".agents"),
	} {
		if pathsOverlap(workspaceTarget, protected) {
			return true
		}
	}
	return false
}

func codexReviewOwnershipLabelValid(label Label) bool {
	return label.Key == ownershipLabelKey && ownershipTokenPattern.MatchString(label.Value)
}

func codexReviewProofTreeDigest(proof []byte) (string, error) {
	if len(proof) == 0 || len(proof) > maxBaseProofBytes {
		return "", failf(CheckObservedBaseIdentity, "Codex review workspace proof has an invalid size")
	}
	var digest string
	for _, line := range strings.Split(strings.ReplaceAll(string(proof), "\r\n", "\n"), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || key != baseProofTreeKey {
			continue
		}
		_, validDigest := contentaddr.FromHex(value)
		if digest != "" || !validDigest {
			return "", failf(CheckObservedBaseIdentity, "Codex review workspace proof tree digest is invalid")
		}
		digest = value
	}
	if digest == "" {
		return "", failf(CheckObservedBaseIdentity, "Codex review workspace proof omits the tree digest")
	}
	return digest, nil
}

func codexReviewHostOnlyNetworkValid(report NetworkReport) bool {
	gateway := net.ParseIP(report.IPv4Gateway).To4()
	address, network, err := net.ParseCIDR(report.IPv4Subnet)
	if err != nil || gateway == nil || address.To4() == nil {
		return false
	}
	ones, bits := network.Mask.Size()
	networkIP := network.IP.To4()
	return bits == 32 && ones == 24 && address.Equal(network.IP) && network.Contains(gateway) &&
		gateway[0] == networkIP[0] && gateway[1] == networkIP[1] &&
		gateway[2] == networkIP[2] && gateway[3] == networkIP[3]+1
}

func codexReviewJournalNetworkValid(binding CodexReviewJournalBinding) bool {
	authorityHost, authorityPort, err := net.SplitHostPort(binding.ProviderProxyAuthority)
	if err != nil || authorityPort == "" || authorityHost != binding.ProviderNetworkGateway {
		return false
	}
	return codexReviewHostOnlyNetworkValid(NetworkReport{
		NetworkSummary: NetworkSummary{
			Name: binding.ProviderNetwork, Mode: NetworkHostOnly,
		},
		IPv4Gateway: binding.ProviderNetworkGateway,
		IPv4Subnet:  binding.ProviderNetworkSubnet,
	})
}

func pathsOverlap(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

func codexReviewContainerName(runID string) string { return codexReviewNames(runID).reviewContainer }
func codexReviewNetworkName(runID string) string   { return codexReviewNames(runID).network }
func codexReviewWorkspaceObserverName(runID string) string {
	return codexReviewNames(runID).workspaceObserver
}

func digestStrings(values []string) string {
	h := sha256.New()
	for _, value := range values {
		h.Write([]byte(value))
		h.Write([]byte{0})
	}
	return contentaddr.Format(h.Sum(nil))
}

func digestEnvironment(environment []string) string {
	canonical := slices.Clone(environment)
	slices.Sort(canonical)
	return digestStrings(canonical)
}

func validateCodexReviewAgentSpec(
	cfg CodexReviewConfig,
	req CodexReviewSpec,
	spec ContainerSpec,
	binding CodexReviewJournalBinding,
) error {
	if err := validateCodexReviewRequest(cfg, req); err != nil {
		return err
	}
	_, hostAuthBody, err := readCodexReviewInput(
		cfg.InputRoot, req.AuthSnapshot, maxCodexAuthSnapshotBytes,
	)
	if err != nil {
		return failf(CheckCredentialSeparation, "Codex review auth snapshot changed after admission")
	}
	_, instructionBody, err := readCodexReviewInput(
		cfg.InputRoot, req.InstructionFile, domain.MaxVendorInstructionBytes,
	)
	if err != nil || !bytes.Equal(instructionBody, req.Instructions.Body) {
		return failf(CheckControlPlaneIsolation, "Codex review instruction snapshot changed after admission")
	}
	authBody, expires, err := codexReviewAgentAuthSnapshot(req.AuthMode, hostAuthBody)
	if err != nil {
		return failf(CheckCredentialSeparation, "Codex review auth snapshot changed after admission")
	}
	if expires != nil && expires.Sub(cfg.Now()) < cfg.AccessTokenLifetimeFloor {
		return failf(CheckCredentialSeparation, "Codex review access token fell below its lifetime floor")
	}
	authSum := sha256.Sum256(authBody)
	wantAuthDigest := contentaddr.Format(authSum[:])
	instructionSum := sha256.Sum256(instructionBody)
	wantInstructionDigest := contentaddr.Format(instructionSum[:])
	// Re-tie the snapshot observation's per-file digests to the bytes admission
	// re-read, so a snapshot volume seeded with different content cannot pass the
	// final pre-start reconstruction.
	if req.Snapshot.authDigest != wantAuthDigest || req.Snapshot.instructionDigest != wantInstructionDigest {
		return failf(CheckCredentialSeparation, "Codex review snapshot volume diverged from the admitted bytes")
	}
	wantCommand := codexReviewCommand(cfg.WorkspaceTarget, cfg.Model, cfg.ReasoningEffort, req.Prompt)
	wantEnv := append([]string{
		"HOME=" + CodexContainerHomeTarget,
		"CODEX_HOME=" + CodexHomeTarget,
	}, proxyEnvironment(cfg.ProxyURL)...)
	if spec.Name != codexReviewContainerName(req.RunID) || spec.Image != req.Image ||
		spec.NetworkDisabled || spec.Network != codexReviewNetworkName(req.RunID) ||
		!slices.Equal(spec.Command, wantCommand) || !slices.Equal(spec.Env, wantEnv) {
		return failf(CheckControlPlaneIsolation, "Codex review command, environment, image, or network diverged")
	}
	if _, ok := environmentByKey(append([]string{fixedContainerPathEnv}, spec.Env...)); !ok {
		return failf(CheckControlPlaneIsolation, "Codex review environment is malformed or duplicates a key")
	}
	wantTargets := codexAgentsShadowTargets(cfg.WorkspaceTarget, req.Workspace.agentsEntry)
	if len(spec.Mounts) != 2+len(wantTargets) {
		return failf(CheckControlPlaneIsolation, "Codex review carries an unexpected mount count")
	}
	workspace, snapshot := spec.Mounts[0], spec.Mounts[1]
	if workspace.Type != MountVolume || workspace.Source != req.WorkspaceVolume ||
		workspace.Target != cfg.WorkspaceTarget || !workspace.ReadOnly {
		return failf(CheckCredentialSeparation, "Codex review workspace is not the admitted read-only volume")
	}
	if snapshot.Type != MountVolume || snapshot.Source != req.Snapshot.volume ||
		snapshot.Target != codexReviewSnapshotTarget || !snapshot.ReadOnly {
		return failf(CheckCredentialSeparation, "Codex review snapshot is not the admitted read-only volume")
	}
	for i, target := range wantTargets {
		m := spec.Mounts[2+i]
		if m.Type != MountVolume || m.Source != req.AgentsShadow.volume ||
			m.Target != target || !m.ReadOnly {
			return failf(CheckControlPlaneIsolation, "Codex review .agents shadow topology diverged")
		}
	}
	if err := binding.validatePrepared(); err != nil {
		return failf(CheckControlPlaneIsolation, "Codex review journal binding is invalid")
	}
	if binding.TopologyVersion != codexReviewTopologyVersion ||
		binding.RunID != req.RunID ||
		binding.WorkspaceSourceRunID != req.WorkspaceSourceRunID ||
		binding.Boundary != CodexReviewFreshStart || !binding.FreshContext || binding.ContinuityMounted ||
		binding.WorkspaceVolume != req.WorkspaceVolume || binding.WorkspaceTarget != cfg.WorkspaceTarget ||
		binding.WorkspaceFingerprint != req.Workspace.fingerprint ||
		binding.WorkspaceHead != req.Workspace.head ||
		binding.WorkspaceTreeDigest != req.Workspace.treeDigest ||
		binding.WorkspaceAgentsEntry != req.Workspace.agentsEntry ||
		binding.WorkspaceObserverImage != req.Workspace.observerImage ||
		binding.WorkspaceObserverFingerprint != req.Workspace.observerFingerprint ||
		binding.WorkspacePreStartObserverFingerprint != "" ||
		!binding.WorkspaceReadOnly || !binding.AuthReadOnly || !binding.AuthStoreMutationLeaseRequired ||
		!binding.InstructionReadOnly ||
		!binding.AgentsShadowReadOnly || binding.RefreshEndpointReachable || binding.PublicationCredentials ||
		binding.HomeTarget != CodexContainerHomeTarget || binding.CodexHomeTarget != CodexHomeTarget ||
		binding.AuthMode != req.AuthMode || binding.AuthIdentityID != req.AuthIdentityID ||
		binding.AuthSnapshotDigest != wantAuthDigest || !sameOptionalTime(binding.AccessTokenExpiresAt, expires) ||
		binding.InstructionDigest != req.Instructions.Digest ||
		!sameReviewInstructionBinding(binding.instructionBinding(), req.InstructionBinding) ||
		binding.SnapshotVolume != req.Snapshot.volume || binding.SnapshotTarget != codexReviewSnapshotTarget ||
		binding.SnapshotFingerprint != req.Snapshot.fingerprint ||
		binding.SnapshotObserverImage != req.Snapshot.observerImage ||
		binding.SnapshotObserverFingerprint != req.Snapshot.observerFingerprint ||
		binding.SnapshotPreStartObserverFingerprint != "" || !binding.SnapshotReadOnly ||
		binding.AgentsShadowVolume != req.AgentsShadow.volume ||
		binding.AgentsShadowFingerprint != req.AgentsShadow.fingerprint ||
		binding.AgentsShadowDigest != req.AgentsShadow.digest ||
		binding.AgentsShadowObserverImage != req.AgentsShadow.observerImage ||
		binding.AgentsShadowObserverFingerprint != req.AgentsShadow.observerFingerprint ||
		binding.AgentsShadowPreStartObserverFingerprint != "" ||
		!slices.Equal(binding.AgentsShadowTargets, wantTargets) ||
		!slices.Equal(binding.ProviderEndpoints, req.AuthMode.providerEndpoints()) ||
		binding.ProviderNetwork != req.Network.name ||
		binding.ProviderNetworkFingerprint != req.Network.fingerprint ||
		!binding.ProviderNetworkHostOnly ||
		binding.ProviderNetworkGateway != req.Network.gateway ||
		binding.ProviderNetworkSubnet != req.Network.subnet ||
		binding.ProviderProxyAuthority != req.Network.proxyAuthority ||
		binding.LauncherEnvironmentDigest != digestEnvironment(wantEnv) ||
		binding.CommandDigest != digestStrings(wantCommand) ||
		!contentaddr.Valid(binding.AuthSnapshotDigest) || !contentaddr.Valid(string(binding.InstructionDigest)) {
		return failf(CheckControlPlaneIsolation, "Codex review journal binding diverged from the admitted topology")
	}
	return nil
}

func sameOptionalTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// verifyCodexReviewAllowlistShape compares already-collected values. It is a
// shape sanity check used inside CodexReviewLifecycle.CodexReview, not a safe launch gate
// for callers holding decoded observations.
func verifyCodexReviewAllowlistShape(
	cfg CodexReviewConfig,
	req CodexReviewSpec,
	binding CodexReviewJournalBinding,
	freshShadow CodexReviewShadowObservation,
	freshWorkspace CodexReviewWorkspaceObservation,
	freshSnapshot CodexReviewSnapshotObservation,
	currentNetwork CodexReviewNetworkObservation,
	rep InspectReport,
	spec ContainerSpec,
) (CodexReviewJournalBinding, error) {
	if err := req.AgentsShadow.verifyFresh(freshShadow); err != nil {
		return CodexReviewJournalBinding{}, err
	}
	if err := req.Workspace.verifyFresh(freshWorkspace); err != nil {
		return CodexReviewJournalBinding{}, err
	}
	if err := req.Snapshot.verifyFresh(freshSnapshot); err != nil {
		return CodexReviewJournalBinding{}, err
	}
	if err := req.Network.verifyCurrent(currentNetwork); err != nil {
		return CodexReviewJournalBinding{}, err
	}
	if err := validateCodexReviewAgentSpec(cfg, req, spec, binding); err != nil {
		return CodexReviewJournalBinding{}, err
	}
	if err := verifyAgentAllowlist(rep, spec); err != nil {
		return CodexReviewJournalBinding{}, err
	}
	binding.AgentsShadowPreStartObserverFingerprint = freshShadow.observerFingerprint
	binding.WorkspacePreStartObserverFingerprint = freshWorkspace.observerFingerprint
	binding.SnapshotPreStartObserverFingerprint = freshSnapshot.observerFingerprint
	if err := binding.validateShape(); err != nil {
		return CodexReviewJournalBinding{}, failf(
			CheckControlPlaneIsolation, "Codex review final journal binding is invalid",
		)
	}
	return binding, nil
}
