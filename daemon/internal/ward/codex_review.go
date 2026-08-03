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
)

const (
	// CodexHomeTarget is writable per invocation on the fresh container rootfs.
	// Only auth.json and AGENTS.md are overlaid read-only as single files.
	CodexHomeTarget = "/var/lib/freeside/codex-home"
	// CodexContainerHomeTarget is a clean per-invocation HOME, distinct from
	// CODEX_HOME so cross-agent skill discovery cannot reach operator state.
	CodexContainerHomeTarget  = "/var/lib/freeside/home"
	CodexAuthFileTarget       = CodexHomeTarget + "/auth.json"
	CodexInstructionTarget    = CodexHomeTarget + "/AGENTS.md"
	codexShadowObserverTarget = "/freeside-agents-shadow"
	codexShadowProofPath      = "/freeside-agents-shadow-proof.txt"
	codexWorkspaceProofPath   = "/freeside-review-workspace-proof.txt"
	codexReviewOutputDir      = "/freeside-review-output"
	codexReviewResultPath     = codexReviewOutputDir + "/result.json"
	codexReviewEventsPath     = codexReviewOutputDir + "/events.jsonl"
	codexReviewStatusPath     = codexReviewOutputDir + "/status"
	codexReviewSchemaPath     = codexReviewOutputDir + "/schema.json"

	codexReviewTopologyVersion = "codex_review_read_only_v1"
	maxCodexAuthSnapshotBytes  = 1 << 20
	maxCodexReviewPromptBytes  = 31 << 10
	emptyCodexShadowDigest     = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
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
	Now                      func() time.Time
	Journal                  CodexReviewJournal
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
	observerImage       string
	observerFingerprint string
}

func (o CodexReviewWorkspaceObservation) valid() bool {
	return o.volume != "" && cliSafe(o.volume) && o.fingerprint != "" &&
		commitSHAPattern.MatchString(o.head) && contentaddr.Valid("sha256:"+o.treeDigest) &&
		digestPinnedImagePattern.MatchString(o.observerImage) && o.observerFingerprint != ""
}

func (o CodexReviewWorkspaceObservation) verifyFresh(fresh CodexReviewWorkspaceObservation) error {
	if !o.valid() || !fresh.valid() || fresh.volume != o.volume ||
		fresh.fingerprint != o.fingerprint || fresh.head != o.head ||
		fresh.treeDigest != o.treeDigest || fresh.observerImage != o.observerImage {
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

	// AgentsShadow is the runtime-backed observation of one empty volume,
	// mounted read-only at HOME/.agents and at the workspace root plus every
	// in-container ancestor. Its evidence never authorizes cleanup.
	AgentsShadow CodexReviewShadowObservation
}

// CodexReviewJournalBinding is the non-secret topology evidence a review
// source persists before launch. VerifyCodexReviewAllowlist adds the mandatory
// pre-start observation beside the initial observation. The binding carries no
// prompt or credential bytes.
type CodexReviewJournalBinding struct {
	TopologyVersion                         string                `json:"topology_version"`
	RunID                                   string                `json:"run_id"`
	Boundary                                CodexReviewBoundary   `json:"boundary"`
	WorkspaceSourceRunID                    string                `json:"workspace_source_run_id"`
	WorkspaceVolume                         string                `json:"workspace_volume"`
	WorkspaceFingerprint                    string                `json:"workspace_fingerprint"`
	WorkspaceHead                           string                `json:"workspace_head"`
	WorkspaceTreeDigest                     string                `json:"workspace_tree_digest"`
	WorkspaceObserverImage                  string                `json:"workspace_observer_image"`
	WorkspaceObserverFingerprint            string                `json:"workspace_observer_fingerprint"`
	WorkspacePreStartObserverFingerprint    string                `json:"workspace_pre_start_observer_fingerprint"`
	WorkspaceTarget                         string                `json:"workspace_target"`
	WorkspaceReadOnly                       bool                  `json:"workspace_read_only"`
	HomeTarget                              string                `json:"home_target"`
	CodexHomeTarget                         string                `json:"codex_home_target"`
	FreshContext                            bool                  `json:"fresh_context"`
	ContinuityMounted                       bool                  `json:"continuity_mounted"`
	AuthMode                                CodexAuthMode         `json:"auth_mode"`
	AuthIdentityID                          domain.AuthIdentityID `json:"auth_identity_id"`
	AuthSnapshotDigest                      string                `json:"auth_snapshot_digest"`
	AccessTokenExpiresAt                    *time.Time            `json:"access_token_expires_at"`
	AuthReadOnly                            bool                  `json:"auth_read_only"`
	AuthStoreMutationLeaseRequired          bool                  `json:"auth_store_mutation_lease_required"`
	InstructionDigest                       domain.Digest         `json:"instruction_digest"`
	InstructionReadOnly                     bool                  `json:"instruction_read_only"`
	AgentsShadowVolume                      string                `json:"agents_shadow_volume"`
	AgentsShadowFingerprint                 string                `json:"agents_shadow_fingerprint"`
	AgentsShadowDigest                      string                `json:"agents_shadow_digest"`
	AgentsShadowObserverImage               string                `json:"agents_shadow_observer_image"`
	AgentsShadowObserverFingerprint         string                `json:"agents_shadow_observer_fingerprint"`
	AgentsShadowPreStartObserverFingerprint string                `json:"agents_shadow_pre_start_observer_fingerprint"`
	AgentsShadowTargets                     []string              `json:"agents_shadow_targets"`
	AgentsShadowReadOnly                    bool                  `json:"agents_shadow_read_only"`
	ProviderEndpoints                       []string              `json:"provider_endpoints"`
	ProviderNetwork                         string                `json:"provider_network"`
	ProviderNetworkFingerprint              string                `json:"provider_network_fingerprint"`
	ProviderNetworkHostOnly                 bool                  `json:"provider_network_host_only"`
	ProviderNetworkGateway                  string                `json:"provider_network_gateway"`
	ProviderNetworkSubnet                   string                `json:"provider_network_subnet"`
	ProviderProxyAuthority                  string                `json:"provider_proxy_authority"`
	RefreshEndpointReachable                bool                  `json:"refresh_endpoint_reachable"`
	PublicationCredentials                  bool                  `json:"publication_credentials"`
	LauncherEnvironmentDigest               string                `json:"launcher_environment_digest"`
	CommandDigest                           string                `json:"command_digest"`
	ReviewContainer                         string                `json:"review_container"`
	ReviewContainerFingerprint              string                `json:"review_container_fingerprint"`
	ReviewOwnershipToken                    string                `json:"review_ownership_token"`
}

// BuildCodexReviewShadowObserverSpec returns the exact networkless helper
// topology whose proof can establish that the .agents shadow is empty. It is
// a topology constructor for conformance tests; Backend.CodexReview owns the
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

// BuildCodexReviewWorkspaceObserverSpec returns the pinned, networkless,
// read-only observer used to bind the review to one candidate commit and tree.
// Backend.CodexReview is the safe runtime lifecycle that executes it.
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
	return ContainerSpec{
		Name:            codexReviewWorkspaceObserverName(runID),
		Image:           cfg.ObserverImage,
		Command:         observerCommand(observerCfg, ownershipLabel.Value),
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
	observedHead, err := verifyBaseProof(proof, observerOwnershipLabel.Value, treeDigest)
	if err != nil || observedHead != expectedHead {
		return CodexReviewWorkspaceObservation{}, failf(
			CheckObservedBaseIdentity, "Codex review workspace does not hold the requested head and tree",
		)
	}
	return CodexReviewWorkspaceObservation{
		volume: volume, fingerprint: fingerprint, head: observedHead,
		treeDigest: treeDigest, observerImage: cfg.ObserverImage,
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
// a prepared shape. It does not authorize start; Backend.CodexReview supplies
// ownership, live reconstruction, durable binding, and the runtime start.
func BuildCodexReviewAgentSpec(
	cfg CodexReviewConfig,
	req CodexReviewSpec,
) (ContainerSpec, CodexReviewJournalBinding, error) {
	if err := validateCodexReviewRequest(cfg, req); err != nil {
		return ContainerSpec{}, CodexReviewJournalBinding{}, err
	}
	authPath, authBody, err := readCodexReviewInput(cfg.InputRoot, req.AuthSnapshot, maxCodexAuthSnapshotBytes)
	if err != nil {
		return ContainerSpec{}, CodexReviewJournalBinding{}, fmt.Errorf("%w: auth snapshot: %w", ErrInvalidCodexReviewSpec, err)
	}
	accessExpiry, err := inspectCodexAuthSnapshot(req.AuthMode, authBody)
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

	shadowTargets := codexAgentsShadowTargets(cfg.WorkspaceTarget)
	env := append([]string{
		"HOME=" + CodexContainerHomeTarget,
		"CODEX_HOME=" + CodexHomeTarget,
	}, proxyEnvironment(cfg.ProxyURL)...)
	command := codexReviewCommand(cfg.WorkspaceTarget, cfg.Model, cfg.ReasoningEffort, req.Prompt)
	mounts := []Mount{
		{Type: MountVolume, Source: req.WorkspaceVolume, Target: cfg.WorkspaceTarget, ReadOnly: true},
		{Type: MountBind, Source: authPath, Target: CodexAuthFileTarget, ReadOnly: true},
		{Type: MountBind, Source: instructionPath, Target: CodexInstructionTarget, ReadOnly: true},
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
	authSum := sha256.Sum256(authBody)
	binding := CodexReviewJournalBinding{
		TopologyVersion:                 codexReviewTopologyVersion,
		RunID:                           req.RunID,
		Boundary:                        req.Boundary,
		WorkspaceSourceRunID:            req.WorkspaceSourceRunID,
		WorkspaceVolume:                 req.WorkspaceVolume,
		WorkspaceFingerprint:            req.Workspace.fingerprint,
		WorkspaceHead:                   req.Workspace.head,
		WorkspaceTreeDigest:             req.Workspace.treeDigest,
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
		AuthSnapshotDigest:              fmt.Sprintf("sha256:%x", authSum),
		AccessTokenExpiresAt:            accessExpiry,
		AuthReadOnly:                    true,
		AuthStoreMutationLeaseRequired:  true,
		InstructionDigest:               req.Instructions.Digest,
		InstructionReadOnly:             true,
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
		LauncherEnvironmentDigest:       digestStrings(env),
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
	return b.validate(true)
}

func (b CodexReviewJournalBinding) validatePrepared() error {
	if b.AgentsShadowPreStartObserverFingerprint != "" ||
		b.WorkspacePreStartObserverFingerprint != "" {
		return errors.New("codex review prepared journal binding carries pre-start evidence")
	}
	return b.validate(false)
}

func (b CodexReviewJournalBinding) validate(requirePreStartObservation bool) error {
	if b.TopologyVersion != codexReviewTopologyVersion ||
		!runIDPattern.MatchString(b.RunID) ||
		b.Boundary != CodexReviewFreshStart || !b.FreshContext || b.ContinuityMounted ||
		!b.WorkspaceReadOnly || !b.AuthReadOnly || !b.AuthStoreMutationLeaseRequired ||
		!b.InstructionReadOnly || !b.AgentsShadowReadOnly ||
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
	if b.WorkspaceFingerprint == "" || !commitSHAPattern.MatchString(b.WorkspaceHead) ||
		!contentaddr.Valid("sha256:"+b.WorkspaceTreeDigest) ||
		!digestPinnedImagePattern.MatchString(b.WorkspaceObserverImage) ||
		b.WorkspaceObserverFingerprint == "" {
		return errors.New("codex review journal workspace evidence is invalid")
	}
	if requirePreStartObservation &&
		(b.WorkspacePreStartObserverFingerprint == "" ||
			b.WorkspacePreStartObserverFingerprint == b.WorkspaceObserverFingerprint) {
		return errors.New("codex review journal omits distinct pre-start workspace evidence")
	}
	if !b.AuthMode.valid() || b.AuthIdentityID == "" ||
		!contentaddr.Valid(b.AuthSnapshotDigest) || !contentaddr.Valid(string(b.InstructionDigest)) {
		return errors.New("codex review journal credential or instruction binding is invalid")
	}
	if b.AuthMode == CodexAuthSubscription && b.AccessTokenExpiresAt == nil {
		return errors.New("codex review subscription binding omits access-token expiry")
	}
	if b.AuthMode == CodexAuthAPIKey && b.AccessTokenExpiresAt != nil {
		return errors.New("codex review API-key binding carries access-token expiry")
	}
	if b.AgentsShadowVolume == "" || !cliSafe(b.AgentsShadowVolume) ||
		b.AgentsShadowFingerprint == "" || b.AgentsShadowDigest != emptyCodexShadowDigest ||
		!digestPinnedImagePattern.MatchString(b.AgentsShadowObserverImage) ||
		b.AgentsShadowObserverFingerprint == "" ||
		!slices.Equal(b.AgentsShadowTargets, codexAgentsShadowTargets(b.WorkspaceTarget)) {
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
	case !cleanAbs(cfg.InputRoot):
		return fmt.Errorf("%w: InputRoot must be a clean absolute non-root path", ErrInvalidCodexReviewSpec)
	case cfg.AccessTokenLifetimeFloor <= 0:
		return fmt.Errorf("%w: AccessTokenLifetimeFloor must be positive", ErrInvalidCodexReviewSpec)
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
	return nil
}

func readCodexReviewInput(root, file string, limit int64) (string, []byte, error) {
	rootInfo, err := os.Lstat(root)
	var rootStat *syscall.Stat_t
	rootStatOK := false
	if rootInfo != nil {
		rootStat, rootStatOK = rootInfo.Sys().(*syscall.Stat_t)
	}
	rootOwned := rootStatOK && codexReviewUIDMatches(rootStat, os.Geteuid())
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode().Perm()&0o077 != 0 || !rootOwned {
		return "", nil, errors.New("input root is not a private directory")
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", nil, errors.New("input root cannot be resolved")
	}
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() {
		return "", nil, errors.New("input is not a regular file")
	}
	resolvedFile, err := filepath.EvalSymlinks(file)
	if err != nil {
		return "", nil, errors.New("input cannot be resolved")
	}
	rel, err := filepath.Rel(resolvedRoot, resolvedFile)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", nil, errors.New("input resolves outside the trusted root")
	}
	if !cliSafe(resolvedFile) {
		return "", nil, errors.New("input path is not CLI-safe")
	}
	f, err := os.OpenFile(resolvedFile, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", nil, errors.New("input cannot be opened")
	}
	defer f.Close() //nolint:errcheck // read-only file
	openedInfo, err := f.Stat()
	if err != nil || !os.SameFile(info, openedInfo) {
		return "", nil, errors.New("input changed while it was admitted")
	}
	stat, ok := openedInfo.Sys().(*syscall.Stat_t)
	if !openedInfo.Mode().IsRegular() || openedInfo.Mode().Perm()&0o077 != 0 ||
		openedInfo.Mode().Perm()&0o400 == 0 || !ok || stat.Nlink != 1 ||
		!codexReviewUIDMatches(stat, os.Geteuid()) {
		return "", nil, errors.New("input is not a private, singly linked regular file")
	}
	body, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return "", nil, errors.New("input cannot be read")
	}
	if int64(len(body)) > limit {
		return "", nil, errors.New("input exceeds its byte limit")
	}
	return resolvedFile, body, nil
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
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var auth codexAuthFile
	if err := dec.Decode(&auth); err != nil {
		return nil, errors.New("auth.json is malformed or carries an unknown field")
	}
	if err := ensureJSONEOF(dec); err != nil {
		return nil, errors.New("auth.json carries trailing content")
	}
	if len(auth.LastRefresh) != 0 && !bytes.Equal(auth.LastRefresh, []byte("null")) {
		var refreshed time.Time
		if err := json.Unmarshal(auth.LastRefresh, &refreshed); err != nil || refreshed.IsZero() {
			return nil, errors.New("auth.json last_refresh is not an instant or null")
		}
	}
	switch mode {
	case CodexAuthSubscription:
		if auth.AuthMode != "" && auth.AuthMode != "chatgpt" {
			return nil, errors.New("auth.json mode is not chatgpt")
		}
		if auth.OpenAIAPIKey != nil && *auth.OpenAIAPIKey != "" {
			return nil, errors.New("subscription snapshot also carries an API key")
		}
		if auth.Tokens == nil || auth.Tokens.AccessToken == "" {
			return nil, errors.New("subscription snapshot carries no access token")
		}
		if auth.Tokens.RefreshToken == nil {
			return nil, errors.New("subscription snapshot carries no explicit refresh-token field")
		}
		if *auth.Tokens.RefreshToken != "" {
			return nil, errors.New("subscription snapshot carries a refresh token")
		}
		expires, err := jwtExpiry(auth.Tokens.AccessToken)
		if err != nil {
			return nil, err
		}
		return &expires, nil
	case CodexAuthAPIKey:
		if auth.AuthMode != "" && auth.AuthMode != "apikey" && auth.AuthMode != "api_key" {
			return nil, errors.New("auth.json mode is not API key")
		}
		if auth.OpenAIAPIKey == nil || *auth.OpenAIAPIKey == "" {
			return nil, errors.New("API-key snapshot carries no key")
		}
		if auth.Tokens != nil {
			return nil, errors.New("API-key snapshot also carries token credentials")
		}
		return nil, nil
	}
	return nil, errors.New("unsupported auth mode")
}

func ensureJSONEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("not EOF")
	}
	return nil
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
	command := "set +e; mkdir -p " + shellQuote(codexReviewOutputDir) + "; " +
		"printf '%s' " + shellQuote(schema) + " > " + shellQuote(codexReviewSchemaPath) + "; " +
		"codex exec --json --ephemeral --skip-git-repo-check -s read-only -C " + shellQuote(workspaceTarget) +
		" -m " + shellQuote(model) + " -c " + shellQuote("model_reasoning_effort=\""+reasoningEffort+"\"") +
		" -c project_doc_max_bytes=0 --ignore-user-config --ignore-rules" +
		" --output-schema " + shellQuote(codexReviewSchemaPath) +
		" --output-last-message " + shellQuote(codexReviewResultPath) +
		" -- " + shellQuote(prompt) + " > " + shellQuote(codexReviewEventsPath) + " 2>&1; " +
		"review_status=$?; printf '%s\\n' \"$review_status\" > " + shellQuote(codexReviewStatusPath) +
		"; exit \"$review_status\""
	return []string{"sh", "-c", command}
}

func codexReviewShadowObserverName(runID string) string {
	return "freeside-review-" + runID + "-agents-observer"
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

func codexAgentsShadowTargets(workspaceTarget string) []string {
	seen := map[string]struct{}{path.Join(CodexContainerHomeTarget, ".agents"): {}}
	for current := path.Clean(workspaceTarget); ; current = path.Dir(current) {
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
		if digest != "" || !contentaddr.Valid("sha256:"+value) {
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

func codexReviewContainerName(runID string) string { return "freeside-review-" + runID + "-codex" }
func codexReviewNetworkName(runID string) string   { return "freeside-review-" + runID + "-egress" }
func codexReviewWorkspaceObserverName(runID string) string {
	return "freeside-review-" + runID + "-workspace-observer"
}

func digestStrings(values []string) string {
	h := sha256.New()
	for _, value := range values {
		h.Write([]byte(value))
		h.Write([]byte{0})
	}
	return fmt.Sprintf("sha256:%x", h.Sum(nil))
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
	authPath, authBody, err := readCodexReviewInput(
		cfg.InputRoot, req.AuthSnapshot, maxCodexAuthSnapshotBytes,
	)
	if err != nil {
		return failf(CheckCredentialSeparation, "Codex review auth snapshot changed after admission")
	}
	instructionPath, instructionBody, err := readCodexReviewInput(
		cfg.InputRoot, req.InstructionFile, domain.MaxVendorInstructionBytes,
	)
	if err != nil || !bytes.Equal(instructionBody, req.Instructions.Body) {
		return failf(CheckControlPlaneIsolation, "Codex review instruction snapshot changed after admission")
	}
	expires, err := inspectCodexAuthSnapshot(req.AuthMode, authBody)
	if err != nil {
		return failf(CheckCredentialSeparation, "Codex review auth snapshot changed after admission")
	}
	if expires != nil && expires.Sub(cfg.Now()) < cfg.AccessTokenLifetimeFloor {
		return failf(CheckCredentialSeparation, "Codex review access token fell below its lifetime floor")
	}
	authSum := sha256.Sum256(authBody)
	wantAuthDigest := fmt.Sprintf("sha256:%x", authSum)
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
	wantTargets := codexAgentsShadowTargets(cfg.WorkspaceTarget)
	if len(spec.Mounts) != 3+len(wantTargets) {
		return failf(CheckControlPlaneIsolation, "Codex review carries an unexpected mount count")
	}
	workspace, auth, instructions := spec.Mounts[0], spec.Mounts[1], spec.Mounts[2]
	if workspace.Type != MountVolume || workspace.Source != req.WorkspaceVolume ||
		workspace.Target != cfg.WorkspaceTarget || !workspace.ReadOnly {
		return failf(CheckCredentialSeparation, "Codex review workspace is not the admitted read-only volume")
	}
	if auth.Type != MountBind || auth.Source != authPath ||
		auth.Target != CodexAuthFileTarget || !auth.ReadOnly {
		return failf(CheckCredentialSeparation, "Codex review auth snapshot is not a read-only single-file bind")
	}
	if instructions.Type != MountBind || instructions.Source != instructionPath ||
		instructions.Target != CodexInstructionTarget || !instructions.ReadOnly {
		return failf(CheckControlPlaneIsolation, "Codex review instructions are not a read-only single-file bind")
	}
	for i, target := range wantTargets {
		m := spec.Mounts[3+i]
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
		binding.LauncherEnvironmentDigest != digestStrings(wantEnv) ||
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
// shape sanity check used inside Backend.CodexReview, not a safe launch gate
// for callers holding decoded observations.
func verifyCodexReviewAllowlistShape(
	cfg CodexReviewConfig,
	req CodexReviewSpec,
	binding CodexReviewJournalBinding,
	freshShadow CodexReviewShadowObservation,
	freshWorkspace CodexReviewWorkspaceObservation,
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
	if err := binding.validateShape(); err != nil {
		return CodexReviewJournalBinding{}, failf(
			CheckControlPlaneIsolation, "Codex review final journal binding is invalid",
		)
	}
	return binding, nil
}
