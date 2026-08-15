package ward

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

const codexReviewShadowVolumeSizeMB int64 = 2

// codexReviewSnapshotVolumeSizeMB sizes the read-only credential volume. It
// must hold both admitted files at their independent ceilings (auth.json up to
// maxCodexAuthSnapshotBytes, AGENTS.md up to domain.MaxVendorInstructionBytes,
// 1 MiB each) plus filesystem overhead, so it is provisioned well above 2 MiB.
const codexReviewSnapshotVolumeSizeMB int64 = 8

// CodexReviewJournal is the durable seam for a prepared review launch. Put
// must be durable before it returns. Get must decode a fresh copy rather than
// return a caller-held value; CodexReview treats that copy as an untrusted
// claim and reconstructs its runtime evidence before start.
type CodexReviewJournal interface {
	PutCodexReviewRequest(context.Context, string, exec.ReviewRequest) error
	GetCodexReviewRequest(context.Context, string) (exec.ReviewRequest, error)
	PutCodexReviewOutcome(context.Context, string, CodexReviewSourceOutcome) error
	GetCodexReviewOutcome(context.Context, string) (CodexReviewSourceOutcome, bool, error)
	ListCodexReviewOutcomeIDs(context.Context) ([]string, error)
	MarkCodexReviewOutcomeReady(context.Context, string) error
	// PutCodexReviewWorkspaceBinding records the ward-created candidate volume
	// before any review topology may attach it.
	PutCodexReviewWorkspaceBinding(context.Context, CodexReviewWorkspaceBinding) error
	DeleteCodexReviewWorkspaceBinding(context.Context, CodexReviewWorkspaceBinding) error
	ListCodexReviewWorkspaceIDs(context.Context) ([]string, error)
	// GetCodexReviewWorkspaceBinding returns provenance durably written by
	// the ward lifecycle that created the candidate volume. CodexReview treats
	// every returned field as a claim and re-matches it to the live runtime.
	GetCodexReviewWorkspaceBinding(context.Context, string) (CodexReviewWorkspaceBinding, error)
	// BeginCodexReviewIntent durably records the owner and every deterministic
	// object name before a lease or runtime call can create an object.
	BeginCodexReviewIntent(context.Context, CodexReviewLaunchIntent) error
	GetCodexReviewIntent(context.Context, string) (CodexReviewLaunchIntent, error)
	ListCodexReviewIntentIDs(context.Context) ([]string, error)
	MarkCodexReviewIntentResource(context.Context, string, CodexReviewIntentResource) error
	MarkCodexReviewIntentPrepared(context.Context, string) error
	MarkCodexReviewIntentStarting(context.Context, string) error
	MarkCodexReviewIntentStarted(context.Context, string) error
	CloseCodexReviewIntent(context.Context, string) error
	PutCodexReviewBinding(context.Context, CodexReviewJournalBinding) error
	GetCodexReviewBinding(context.Context, string) (CodexReviewJournalBinding, error)
}

// ErrCodexReviewIntentNotFound distinguishes an unused run id from a durable
// launch that a later daemon must recover before it can retry.
var ErrCodexReviewIntentNotFound = errors.New("codex review launch intent not found")

var ErrCodexReviewBindingNotFound = errors.New("codex review durable binding not found")

var ErrCodexReviewWorkspaceNotFound = errors.New("codex review workspace preparation not found")

var ErrCodexReviewOperational = errors.New("codex review operational failure")

// ErrCodexReviewRequestRejected marks a persisted request body that decoded
// or validated incorrectly. The production adapter keeps this distinct from
// journal I/O so ReviewSource can authenticate and tear down any invocation
// the original request already started before reporting the contradiction.
var ErrCodexReviewRequestRejected = errors.New("codex review persisted request rejected")

// ErrCodexReviewOutcomeRejected marks a persisted outcome body that decoded
// or validated incorrectly. ReviewSource can still authenticate teardown from
// the independent launch intent and binding before surfacing the contradiction.
var ErrCodexReviewOutcomeRejected = errors.New("codex review persisted outcome rejected")

// ErrCodexReviewOutputInvalid marks malformed terminal content read from an
// already-authenticated, stopped review container. Unlike a topology
// conformance failure, this contradiction is safe to persist before running
// authenticated teardown.
var ErrCodexReviewOutputInvalid = errors.New("codex review output is invalid")

type CodexReviewIntentState string

const (
	CodexReviewIntentPreparing CodexReviewIntentState = "preparing"
	CodexReviewIntentPrepared  CodexReviewIntentState = "prepared"
	CodexReviewIntentStarting  CodexReviewIntentState = "starting"
	CodexReviewIntentStarted   CodexReviewIntentState = "started"
	CodexReviewIntentClosed    CodexReviewIntentState = "closed"
)

// AllCodexReviewIntentStates is the single registration point for the durable
// launch-intent lifecycle, ordered by progression. A new state joins here and
// the validity predicate, the exhaustive settlement switch, and the wardstore
// transition table then force every from-state and dispatch decision. The zero
// value is invalid by design.
var AllCodexReviewIntentStates = []CodexReviewIntentState{
	CodexReviewIntentPreparing, CodexReviewIntentPrepared,
	CodexReviewIntentStarting, CodexReviewIntentStarted, CodexReviewIntentClosed,
}

func (s CodexReviewIntentState) valid() bool {
	switch s {
	case CodexReviewIntentPreparing, CodexReviewIntentPrepared,
		CodexReviewIntentStarting, CodexReviewIntentStarted, CodexReviewIntentClosed:
		return true
	default:
		return false
	}
}

// CodexReviewIntentResource is earned evidence for one side effect. An empty
// fingerprint deliberately represents the create-return/inspect crash window:
// recovery may still act only when the durable unpredictable owner is present.
type CodexReviewIntentResource struct {
	Name           string `json:"name"`
	OwnershipToken string `json:"ownership_token"`
	Fingerprint    string `json:"fingerprint"`
}

// CodexReviewLaunchIntent is the non-secret, restart-safe pre-start record.
// It stores no prompt, auth snapshot, or instruction body. Runtime evidence is
// re-observed on recovery; these fields only authenticate candidates and bind
// a retry to the caller intent that opened the run.
type CodexReviewLaunchIntent struct {
	RunID           string `json:"run_id"`
	SpecDigest      string `json:"spec_digest"`
	OwnershipToken  string `json:"ownership_token"`
	ShadowVolume    string `json:"shadow_volume"`
	Network         string `json:"network"`
	ReviewContainer string `json:"review_container"`
	// SnapshotVolume is the ward-owned read-only credential volume added in #591.
	// It is empty on the pre-#591 six-resource generation, whose review container
	// delivered the two files as host binds; an empty value selects the legacy
	// lease and cleanup shape everywhere it is consumed.
	SnapshotVolume string                      `json:"snapshot_volume"`
	Resources      []CodexReviewIntentResource `json:"resources"`
	State          CodexReviewIntentState      `json:"state"`
}

// codexReviewLeaseVolumes is the exclusive-lease volume set for one review. The
// snapshot volume joins the workspace and shadow volumes on the #591 shape; a
// legacy intent with no snapshot volume keeps the original two-volume lease, so
// the same helper serves both the current launch path and legacy recovery.
func codexReviewLeaseVolumes(workspaceVolume, shadowVolume, snapshotVolume string) []string {
	volumes := []string{workspaceVolume, shadowVolume}
	if snapshotVolume != "" {
		volumes = append(volumes, snapshotVolume)
	}
	return volumes
}

// CodexReviewWorkspaceBinding is the minimum prior ward provenance needed to
// authenticate a candidate volume without adopting its self-reported labels.
type CodexReviewWorkspaceBinding struct {
	SourceRunID         string
	Volume              string
	OwnershipToken      string
	CreationFingerprint string
}

// CodexReviewLaunchSpec contains caller-owned intent only. Runtime identity,
// ownership, network, and observer evidence are deliberately absent: ward
// derives them inside CodexReview.
type CodexReviewLaunchSpec struct {
	RunID                string
	WorkflowRunID        domain.RunID
	Image                string
	WorkspaceSourceRunID string
	WorkspaceVolume      string
	ExpectedHead         string
	Prompt               string
	Boundary             CodexReviewBoundary
	AuthMode             CodexAuthMode
	AuthIdentityID       domain.AuthIdentityID
	AuthSnapshot         string
	Instructions         VendorInstructions
	InstructionFile      string
	InstructionBinding   exec.ReviewInstructionBinding
}

// CodexReviewLaunch is a started review and its reconstructed durable binding.
// Close ends only the daemon CONNECT proxy. ReviewSource owns later container
// collection and runtime cleanup in #427.
type CodexReviewLaunch struct {
	Binding CodexReviewJournalBinding
	proxy   *connectProxy
}

func (l *CodexReviewLaunch) Close() error {
	if l == nil || l.proxy == nil {
		return nil
	}
	err := l.proxy.Close()
	l.proxy = nil
	return err
}

// CodexReview is the only safe start path for the Codex review topology. It
// owns runtime creation and observation, persists the final binding, reloads
// it through the durable seam, reconstructs all live evidence, and only then
// starts the credential-bearing review container.
func (b *CodexReviewLifecycle) CodexReview(
	ctx context.Context,
	cfg CodexReviewConfig,
	launch CodexReviewLaunchSpec,
) (*CodexReviewLaunch, error) {
	if !b.valid() {
		return nil, fmt.Errorf("%w: Codex review lifecycle is not initialized", ErrInvalidConfig)
	}
	if err := validateCodexReviewLaunchShape(cfg, launch); err != nil {
		return nil, err
	}
	releaseRun, err := b.acquireCodexReviewRun(ctx, launch.RunID)
	if err != nil {
		return nil, codexReviewOperationalf("acquire Codex review run gate: %v", err)
	}
	defer releaseRun()
	return b.codexReview(ctx, cfg, launch)
}

// codexReview runs with the caller holding the per-run lifecycle gate. The
// ReviewSource uses it so workspace preparation, runtime launch, and proxy
// publication are one exclusive operation; direct backend callers use the
// exported lifecycle wrapper above.
func (b *CodexReviewLifecycle) codexReview(
	ctx context.Context,
	cfg CodexReviewConfig,
	launch CodexReviewLaunchSpec,
) (_ *CodexReviewLaunch, retErr error) {
	if !b.valid() {
		return nil, fmt.Errorf("%w: Codex review lifecycle is not initialized", ErrInvalidConfig)
	}
	if err := validateCodexReviewLaunchShape(cfg, launch); err != nil {
		return nil, err
	}
	if err := checkCodexAuthReenrollment(ctx, cfg, launch); err != nil {
		return nil, err
	}
	if err := validateCodexReviewLaunchStructure(cfg, launch); err != nil {
		return nil, err
	}
	if err := codexReviewOutcomeFence(ctx, cfg.Journal, launch.RunID); err != nil {
		return nil, err
	}
	cfg.ProviderEndpoints = slices.Clone(cfg.ProviderEndpoints)
	intentDigest, err := codexReviewIntentDigest(cfg, launch)
	if err != nil {
		return nil, fmt.Errorf("digest Codex review launch intent: %w", err)
	}
	// A prior pre-start record is never adopted into a second launch. Recovery
	// proves its own objects absent first, then a new intent can be opened.
	if prior, getErr := cfg.Journal.GetCodexReviewIntent(ctx, launch.RunID); getErr == nil {
		if prior.validateIdentity(launch.RunID) != nil {
			return nil, failf(CheckControlPlaneIsolation, "Codex review launch intent is invalid")
		}
		if prior.State == CodexReviewIntentStarted {
			return nil, failf(CheckControlPlaneIsolation,
				"Codex review launch has crossed the ReviewSource handoff boundary")
		}
		if prior.State != CodexReviewIntentClosed {
			if err := b.RecoverCodexReview(ctx, cfg, launch); err != nil {
				return nil, err
			}
		}
	} else if !errors.Is(getErr, ErrCodexReviewIntentNotFound) {
		if errors.Is(getErr, ErrConformance) {
			return nil, failf(
				CheckControlPlaneIsolation, "load Codex review launch intent: %v", getErr)
		}
		return nil, codexReviewOperationalCheckf(
			CheckControlPlaneIsolation, "load Codex review launch intent: %v", getErr,
		)
	}
	authGuard, err := b.acquireCodexReviewAuth(ctx, cfg, launch)
	if err != nil {
		return nil, err
	}
	authGuardReleased := authGuard == nil
	if authGuard != nil {
		defer func() {
			if !authGuardReleased {
				retErr = errors.Join(retErr, b.releaseCodexReviewAuthLease(ctx, authGuard))
			}
		}()
	}
	if err := validateCodexReviewLaunch(cfg, launch); err != nil {
		return nil, err
	}
	owner, err := newOwnershipLabel()
	if err != nil {
		return nil, fmt.Errorf("mint Codex review ownership: %w", err)
	}
	shadowName := codexReviewShadowVolumeName(launch.RunID)
	snapshotName := codexReviewSnapshotVolumeName(launch.RunID)
	intent := CodexReviewLaunchIntent{
		RunID: launch.RunID, SpecDigest: intentDigest, OwnershipToken: owner.Value,
		ShadowVolume: shadowName, Network: codexReviewNetworkName(launch.RunID),
		ReviewContainer: codexReviewContainerName(launch.RunID),
		SnapshotVolume:  snapshotName,
		Resources: []CodexReviewIntentResource{
			{Name: codexReviewWorkspaceObserverName(launch.RunID)},
			{Name: codexReviewShadowInitializerName(launch.RunID), OwnershipToken: owner.Value},
			{Name: codexReviewShadowObserverName(launch.RunID)},
			{Name: codexReviewContainerName(launch.RunID), OwnershipToken: owner.Value},
			{Name: shadowName, OwnershipToken: owner.Value},
			{Name: codexReviewNetworkName(launch.RunID), OwnershipToken: owner.Value},
			{Name: snapshotName, OwnershipToken: owner.Value},
			{Name: codexReviewSnapshotSeederName(launch.RunID), OwnershipToken: owner.Value},
			{Name: codexReviewSnapshotObserverName(launch.RunID)},
		}, State: CodexReviewIntentPreparing,
	}
	if err := cfg.Journal.BeginCodexReviewIntent(ctx, intent); err != nil {
		return nil, codexReviewJournalCheckf(
			CheckControlPlaneIsolation, "persist Codex review launch intent: %v", err,
		)
	}
	volumeLease, err := cfg.VolumeLifecycleLeaser.AcquireCodexReviewVolumeLease(
		ctx, owner.Value, codexReviewLeaseVolumes(launch.WorkspaceVolume, shadowName, snapshotName),
	)
	if err != nil {
		if errors.Is(err, ErrCodexReviewVolumeLeaseForeignOwner) {
			return nil, failf(CheckControlPlaneIsolation, "acquire Codex review volume lifecycle lease: %v", err)
		}
		return nil, codexReviewOperationalCheckf(
			CheckControlPlaneIsolation, "acquire Codex review volume lifecycle lease: %v", err,
		)
	}
	leaseTransferred := false
	leaseReleasable := true
	defer func() {
		if leaseTransferred || !leaseReleasable {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.cfg.TeardownTimeout)
		defer cancel()
		if releaseErr := volumeLease.ReleaseCodexReviewVolumeLease(cleanupCtx); releaseErr != nil {
			leaseErr := codexReviewOperationalCheckf(
				CheckControlPlaneIsolation, "release Codex review volume lifecycle lease: %v", releaseErr,
			)
			if retErr == nil {
				retErr = leaseErr
			} else {
				retErr = errors.Join(retErr, leaseErr)
			}
		}
	}()

	workspaceBinding, err := cfg.Journal.GetCodexReviewWorkspaceBinding(
		ctx, launch.WorkspaceSourceRunID,
	)
	if err != nil {
		if errors.Is(err, ErrConformance) {
			return nil, failf(
				CheckObservedBaseIdentity, "load Codex review workspace provenance: %v", err)
		}
		return nil, codexReviewOperationalCheckf(
			CheckObservedBaseIdentity, "load Codex review workspace provenance: %v", err,
		)
	}
	if err := workspaceBinding.validateFor(launch); err != nil {
		return nil, err
	}
	workspaceOwner := Label{Key: ownershipLabelKey, Value: workspaceBinding.OwnershipToken}
	workspaceReport, err := b.rt.InspectVolume(ctx, launch.WorkspaceVolume)
	if err != nil {
		return nil, codexReviewOperationalCheckf(
			CheckObservedBaseIdentity, "inspect Codex review workspace: %v", err,
		)
	}
	workspaceFingerprint, err := ownedFingerprint(
		workspaceReport.CreationDate, workspaceReport.Labels,
		workspaceReport.LabelsObserved, workspaceOwner,
	)
	if err != nil || workspaceFingerprint != workspaceBinding.CreationFingerprint {
		return nil, failf(
			CheckObservedBaseIdentity, "live Codex review workspace diverged from prior ward provenance",
		)
	}
	workspace, err := b.observeCodexReviewWorkspace(
		ctx, cfg, launch, workspaceOwner, workspaceReport,
	)
	if err != nil {
		return nil, err
	}

	networkName := codexReviewNetworkName(launch.RunID)
	shadowClaim := objectClaim{attempted: true}
	snapshotClaim := objectClaim{}
	networkClaim := objectClaim{}
	containerClaim := objectClaim{}
	var proxy *connectProxy
	started := false
	defer func() {
		if retErr == nil || started {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.cfg.TeardownTimeout)
		defer cancel()
		var cleanupErrs []error
		if proxy != nil {
			cleanupErrs = append(cleanupErrs, proxy.Close())
		}
		if containerClaim.attempted {
			containerErr := b.reapCodexReviewContainer(
				cleanupCtx, codexReviewContainerName(launch.RunID), containerClaim, owner,
			)
			if containerErr != nil {
				// Releasing the attachment lease while a credential-bearing
				// pre-start container may still exist would make that object
				// startable. Leave recovery a held, visible lease instead.
				leaseReleasable = false
				cleanupErrs = append(cleanupErrs, containerErr)
			}
		}
		if shadowClaim.attempted {
			cleanupErrs = append(cleanupErrs, b.deleteCodexReviewVolume(
				cleanupCtx, shadowName, shadowClaim, owner,
			))
		}
		if snapshotClaim.attempted {
			cleanupErrs = append(cleanupErrs, b.deleteCodexReviewVolume(
				cleanupCtx, snapshotName, snapshotClaim, owner,
			))
		}
		if networkClaim.attempted {
			cleanupErrs = append(cleanupErrs, b.teardownNetwork(
				cleanupCtx, networkName, networkClaim, owner,
			))
		}
		if cleanupErr := errors.Join(cleanupErrs...); cleanupErr != nil {
			retErr = errors.Join(retErr, failf(CheckTeardown, "Codex review preparation cleanup: %v", cleanupErr))
		}
	}()
	if err := b.rt.CreateVolume(
		ctx, shadowName, codexReviewShadowVolumeSizeMB,
		append(runLabels(launch.RunID), owner),
	); err != nil {
		return nil, codexReviewOperationalCheckf(
			CheckControlPlaneIsolation, "create Codex review shadow: %v", err,
		)
	}
	shadowClaim.owned = true
	shadowReport, err := b.rt.InspectVolume(ctx, shadowName)
	if err != nil {
		return nil, codexReviewOperationalCheckf(
			CheckControlPlaneIsolation, "inspect Codex review shadow: %v", err,
		)
	}
	shadowClaim.fingerprint, err = ownedFingerprint(
		shadowReport.CreationDate, shadowReport.Labels, shadowReport.LabelsObserved, owner,
	)
	if err != nil {
		return nil, failf(CheckControlPlaneIsolation, "authenticate Codex review shadow: %v", err)
	}
	if err := cfg.Journal.MarkCodexReviewIntentResource(ctx, launch.RunID,
		CodexReviewIntentResource{Name: shadowName, OwnershipToken: owner.Value, Fingerprint: shadowClaim.fingerprint}); err != nil {
		return nil, codexReviewJournalCheckf(
			CheckControlPlaneIsolation, "journal Codex review shadow: %v", err,
		)
	}
	if err := b.initializeCodexReviewShadow(ctx, cfg, launch.RunID, shadowName, owner,
		func(resource CodexReviewIntentResource) error {
			resource.OwnershipToken = owner.Value
			return cfg.Journal.MarkCodexReviewIntentResource(ctx, launch.RunID, resource)
		}); err != nil {
		return nil, err
	}

	shadow, err := b.observeCodexReviewShadow(ctx, cfg, launch.RunID, shadowName, owner, shadowReport)
	if err != nil {
		return nil, err
	}
	snapshotClaim.attempted = true
	if err := b.rt.CreateVolume(
		ctx, snapshotName, codexReviewSnapshotVolumeSizeMB,
		append(runLabels(launch.RunID), owner),
	); err != nil {
		return nil, codexReviewOperationalCheckf(
			CheckCredentialSeparation, "create Codex review snapshot: %v", err,
		)
	}
	snapshotClaim.owned = true
	snapshotReport, err := b.rt.InspectVolume(ctx, snapshotName)
	if err != nil {
		return nil, codexReviewOperationalCheckf(
			CheckCredentialSeparation, "inspect Codex review snapshot: %v", err,
		)
	}
	snapshotClaim.fingerprint, err = ownedFingerprint(
		snapshotReport.CreationDate, snapshotReport.Labels, snapshotReport.LabelsObserved, owner,
	)
	if err != nil {
		return nil, failf(CheckCredentialSeparation, "authenticate Codex review snapshot: %v", err)
	}
	if err := cfg.Journal.MarkCodexReviewIntentResource(ctx, launch.RunID,
		CodexReviewIntentResource{Name: snapshotName, OwnershipToken: owner.Value, Fingerprint: snapshotClaim.fingerprint}); err != nil {
		return nil, codexReviewJournalCheckf(
			CheckCredentialSeparation, "journal Codex review snapshot: %v", err,
		)
	}
	if err := verifyCodexAuthLaunchAdmission(ctx, cfg, launch, authGuard); err != nil {
		return nil, err
	}
	if err := b.seedCodexReviewSnapshot(ctx, cfg, launch, snapshotName, owner,
		func(resource CodexReviewIntentResource) error {
			resource.OwnershipToken = owner.Value
			return cfg.Journal.MarkCodexReviewIntentResource(ctx, launch.RunID, resource)
		}); err != nil {
		return nil, err
	}
	snapshot, err := b.observeCodexReviewSnapshot(ctx, cfg, launch.RunID, snapshotName, owner, snapshotReport)
	if err != nil {
		return nil, err
	}
	networkClaim.attempted = true
	if err := b.rt.CreateNetwork(ctx, networkName, append(runLabels(launch.RunID), owner)); err != nil {
		return nil, codexReviewOperationalCheckf(CheckAgentEgress, "create Codex review network: %v", err)
	}
	networkClaim.owned = true
	networkReport, err := b.rt.InspectNetwork(ctx, networkName)
	if err != nil {
		return nil, codexReviewOperationalCheckf(CheckAgentEgress, "inspect Codex review network: %v", err)
	}
	networkClaim.fingerprint, err = ownedFingerprint(
		networkReport.CreationDate, networkReport.Labels, networkReport.LabelsObserved, owner,
	)
	if err != nil {
		return nil, failf(CheckAgentEgress, "authenticate Codex review network: %v", err)
	}
	if err := cfg.Journal.MarkCodexReviewIntentResource(ctx, launch.RunID,
		CodexReviewIntentResource{Name: networkName, OwnershipToken: owner.Value, Fingerprint: networkClaim.fingerprint}); err != nil {
		return nil, codexReviewJournalCheckf(
			CheckControlPlaneIsolation, "journal Codex review network: %v", err,
		)
	}
	proxy, err = startConnectProxy(
		context.WithoutCancel(ctx), networkReport.IPv4Gateway, networkReport.IPv4Subnet,
		cfg.ProviderEndpoints, b.cfg.EgressProxyTimeout, b.cfg.EgressDialContext,
	)
	if err != nil {
		return nil, codexReviewOperationalCheckf(CheckAgentEgress, "start Codex review proxy: %v", err)
	}
	cfg.ProxyURL = proxy.URL()
	network, err := ObserveCodexReviewNetwork(cfg, launch.RunID, owner, networkReport)
	if err != nil {
		return nil, err
	}

	req := CodexReviewSpec{
		RunID: launch.RunID, Image: launch.Image,
		WorkspaceSourceRunID: launch.WorkspaceSourceRunID, WorkspaceVolume: launch.WorkspaceVolume,
		Workspace: workspace, Network: network, Prompt: launch.Prompt, Boundary: launch.Boundary,
		AuthMode: launch.AuthMode, AuthIdentityID: launch.AuthIdentityID,
		AuthSnapshot: launch.AuthSnapshot, Instructions: launch.Instructions,
		InstructionFile: launch.InstructionFile, InstructionBinding: launch.InstructionBinding,
		AgentsShadow: shadow, Snapshot: snapshot,
	}
	spec, binding, err := BuildCodexReviewAgentSpec(cfg, req)
	if err != nil {
		return nil, err
	}
	spec.Labels = append(spec.Labels, owner)
	containerClaim.attempted = true
	if err := b.rt.CreateContainer(ctx, cloneContainerSpec(spec)); err != nil {
		return nil, codexReviewOperationalCheckf(
			CheckControlPlaneIsolation, "create Codex review container: %v", err,
		)
	}
	containerClaim.owned = true
	containerReport, err := b.rt.Inspect(ctx, spec.Name)
	if err != nil {
		return nil, codexReviewOperationalCheckf(
			CheckControlPlaneIsolation, "inspect Codex review container: %v", err,
		)
	}
	containerClaim.fingerprint, err = ownedFingerprint(
		containerReport.CreationDate, containerReport.Labels, containerReport.LabelsObserved, owner,
	)
	if err != nil {
		return nil, failf(CheckControlPlaneIsolation, "authenticate Codex review container: %v", err)
	}
	if err := verifyAgentAllowlist(containerReport, spec); err != nil {
		return nil, err
	}
	if err := cfg.Journal.MarkCodexReviewIntentResource(ctx, launch.RunID,
		CodexReviewIntentResource{Name: spec.Name, OwnershipToken: owner.Value, Fingerprint: containerClaim.fingerprint}); err != nil {
		return nil, codexReviewJournalCheckf(
			CheckControlPlaneIsolation, "journal Codex review container: %v", err,
		)
	}
	binding.ReviewContainer = spec.Name
	binding.ReviewContainerFingerprint = containerClaim.fingerprint
	binding.ReviewOwnershipToken = owner.Value

	freshShadow, freshWorkspace, freshSnapshot, currentNetwork, err := b.reobserveCodexReview(
		ctx, cfg, launch, workspaceOwner, owner, shadowName, snapshotName,
	)
	if err != nil {
		return nil, err
	}
	binding, err = verifyCodexReviewAllowlistShape(
		cfg, req, binding, freshShadow, freshWorkspace, freshSnapshot, currentNetwork, containerReport, spec,
	)
	if err != nil {
		return nil, err
	}
	if err := cfg.Journal.PutCodexReviewBinding(ctx, cloneCodexReviewBinding(binding)); err != nil {
		return nil, codexReviewJournalCheckf(
			CheckControlPlaneIsolation, "persist Codex review binding: %v", err,
		)
	}
	persisted, err := cfg.Journal.GetCodexReviewBinding(ctx, launch.RunID)
	if err != nil {
		if errors.Is(err, ErrConformance) {
			return nil, failf(
				CheckControlPlaneIsolation, "reload Codex review binding: %v", err)
		}
		return nil, codexReviewOperationalCheckf(
			CheckControlPlaneIsolation, "reload Codex review binding: %v", err,
		)
	}
	if err := persisted.validateShape(); err != nil || !sameCodexReviewBinding(persisted, binding) {
		return nil, failf(CheckControlPlaneIsolation, "persisted Codex review binding diverged from live preparation")
	}
	if err := b.verifyCodexReviewWorkspaceExclusive(ctx, launch.WorkspaceVolume, spec.Name); err != nil {
		return nil, err
	}
	if err := b.reconstructCodexReview(
		ctx, cfg, launch, req, spec, persisted, workspaceOwner, owner, shadowName, snapshotName,
	); err != nil {
		return nil, err
	}
	if err := b.verifyCodexReviewWorkspaceExclusive(ctx, launch.WorkspaceVolume, spec.Name); err != nil {
		return nil, err
	}
	if err := validateCodexReviewStartLifetime(cfg, launch); err != nil {
		return nil, err
	}
	if err := verifyCodexAuthLaunchAdmission(ctx, cfg, launch, authGuard); err != nil {
		return nil, err
	}
	if err := cfg.Journal.MarkCodexReviewIntentPrepared(ctx, launch.RunID); err != nil {
		return nil, codexReviewJournalCheckf(
			CheckControlPlaneIsolation, "mark Codex review launch prepared: %v", err,
		)
	}
	if err := cfg.Journal.MarkCodexReviewIntentStarting(ctx, launch.RunID); err != nil {
		return nil, codexReviewJournalCheckf(
			CheckControlPlaneIsolation, "mark Codex review launch starting: %v", err,
		)
	}
	startCtx, cancelStart, err := reserveCodexAuthStartAdmission(ctx, cfg, launch, authGuard)
	if err != nil {
		return nil, err
	}
	startErr := volumeLease.StartCodexReviewContainer(startCtx, spec.Name)
	cancelStart()
	if startErr != nil {
		// Starting is durable before the effect, and the error can describe
		// either no effect or a successful atomic lease transfer. A
		// fresh-context review is disposable, so both outcomes resolve the same
		// way: close the proxy, reap whatever this launch durably owns, and
		// leave a clean retry. A failed recovery keeps the durable intent and
		// lease visible; nothing live survives this error return either way.
		leaseReleasable = false
		started = true
		startFailure := codexReviewOperationalCheckf(
			CheckControlPlaneIsolation, "start Codex review container: %v", startErr,
		)
		proxyErr := proxy.Close()
		proxy = nil
		if recoveryErr := b.recoverCodexReviewAfterStart(ctx, cfg, launch); recoveryErr != nil {
			return nil, errors.Join(startFailure, recoveryErr, proxyErr)
		}
		return nil, errors.Join(startFailure, proxyErr)
	}
	// A successful Start already transferred the lease. From here recovery owns
	// every durable object: if the process dies before the handoff record, a
	// later recovery sees `starting` and reaps the review for a fresh retry.
	// `started` is recorded only by the invocation that observed the successful
	// start; #427 never owns a review whose start ward did not witness.
	leaseTransferred = true
	started = true
	if err := b.releaseCodexReviewAuthLease(ctx, authGuard); err != nil {
		releaseErr := err
		proxyErr := proxy.Close()
		proxy = nil
		if recoveryErr := b.recoverCodexReviewAfterStart(ctx, cfg, launch); recoveryErr != nil {
			return nil, errors.Join(releaseErr, recoveryErr, proxyErr)
		}
		return nil, errors.Join(releaseErr, proxyErr)
	}
	authGuardReleased = true
	if err := cfg.Journal.MarkCodexReviewIntentStarted(ctx, launch.RunID); err != nil {
		// Without the durable handoff record #427 can never own this review, so
		// destroy it now rather than strand a running credential-bearing
		// container behind an error return.
		markErr := codexReviewJournalCheckf(
			CheckControlPlaneIsolation, "mark Codex review ReviewSource handoff: %v", err,
		)
		proxyErr := proxy.Close()
		proxy = nil
		if recoveryErr := b.recoverCodexReviewAfterStart(ctx, cfg, launch); recoveryErr != nil {
			return nil, errors.Join(markErr, recoveryErr, proxyErr)
		}
		return nil, errors.Join(markErr, proxyErr)
	}
	return &CodexReviewLaunch{Binding: persisted, proxy: proxy}, nil
}

func (b *CodexReviewLifecycle) recoverCodexReviewAfterStart(
	ctx context.Context, cfg CodexReviewConfig, launch CodexReviewLaunchSpec,
) error {
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.cfg.HandoffTimeout)
	defer cancel()
	return b.RecoverCodexReview(recoveryCtx, cfg, launch)
}

func codexReviewOutcomeFence(ctx context.Context, journal CodexReviewJournal, runID string) error {
	outcome, _, err := journal.GetCodexReviewOutcome(ctx, runID)
	if errors.Is(err, ErrCodexReviewOutcomeNotFound) {
		return nil
	}
	if err != nil {
		if errors.Is(err, ErrCodexReviewOutcomeRejected) || errors.Is(err, ErrConformance) {
			return failf(CheckControlPlaneIsolation, "load Codex review outcome fence: %v", err)
		}
		return codexReviewOperationalCheckf(
			CheckControlPlaneIsolation, "load Codex review outcome fence: %v", err)
	}
	if validateErr := outcome.Validate(); validateErr != nil || string(outcome.InvocationID) != runID {
		return failf(CheckControlPlaneIsolation, "Codex review outcome fence is invalid: %v",
			errors.Join(validateErr, domain.ErrParentKeyMismatch))
	}
	return failf(CheckControlPlaneIsolation, "Codex review outcome fence already exists")
}

// acquireCodexReviewRun serializes launch and rejection cleanup for one run.
// The gate is process-local by design: after a daemon restart no launch
// goroutine survives, so durable recovery must be free to proceed immediately.
func (b *CodexReviewLifecycle) acquireCodexReviewRun(ctx context.Context, runID string) (func(), error) {
	for {
		b.codexReviewMu.Lock()
		active := b.codexReviewRuns[runID]
		if active == nil {
			done := make(chan struct{})
			b.codexReviewRuns[runID] = done
			b.codexReviewMu.Unlock()
			return func() {
				b.codexReviewMu.Lock()
				if b.codexReviewRuns[runID] == done {
					delete(b.codexReviewRuns, runID)
					close(done)
				}
				b.codexReviewMu.Unlock()
			}, nil
		}
		b.codexReviewMu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-active:
		}
	}
}

func validateCodexReviewLaunchShape(cfg CodexReviewConfig, launch CodexReviewLaunchSpec) error {
	switch {
	case cfg.Journal == nil:
		return fmt.Errorf("%w: Journal is required", ErrInvalidCodexReviewSpec)
	case cfg.VolumeLifecycleLeaser == nil:
		return fmt.Errorf("%w: VolumeLifecycleLeaser is required", ErrInvalidCodexReviewSpec)
	case !runIDPattern.MatchString(launch.RunID):
		return fmt.Errorf("%w: RunID is invalid", ErrInvalidCodexReviewSpec)
	case launch.WorkflowRunID == "":
		return fmt.Errorf("%w: WorkflowRunID is required", ErrInvalidCodexReviewSpec)
	case !digestPinnedImagePattern.MatchString(launch.Image):
		return fmt.Errorf("%w: Image must be digest-pinned", ErrInvalidCodexReviewSpec)
	case !digestPinnedImagePattern.MatchString(cfg.ApprovedImage) || !sameImage(cfg.ApprovedImage, launch.Image):
		return fmt.Errorf("%w: Image does not match the deployment-approved Codex pin", ErrInvalidCodexReviewSpec)
	case !runIDPattern.MatchString(launch.WorkspaceSourceRunID):
		return fmt.Errorf("%w: WorkspaceSourceRunID is invalid", ErrInvalidCodexReviewSpec)
	// The review workspace must be self-sourced and carry the run-derived
	// volume name: teardown re-derives its targets from the run id alone, so
	// a workspace identity that cannot be re-derived could never be
	// authenticated for cleanup under a rewritten-journal threat model.
	case launch.WorkspaceSourceRunID != launch.RunID:
		return fmt.Errorf("%w: review workspace must be sourced from its own run", ErrInvalidCodexReviewSpec)
	case launch.WorkspaceVolume == "" || !cliSafe(launch.WorkspaceVolume) ||
		launch.WorkspaceVolume != namesFor(launch.RunID).Workspace:
		return fmt.Errorf("%w: WorkspaceVolume is invalid", ErrInvalidCodexReviewSpec)
	case !commitSHAPattern.MatchString(launch.ExpectedHead):
		return fmt.Errorf("%w: ExpectedHead is invalid", ErrInvalidCodexReviewSpec)
	case !cleanAbs(cfg.WorkspaceTarget) || !cliSafe(cfg.WorkspaceTarget) ||
		codexReviewWorkspaceOverlapsControlPath(cfg.WorkspaceTarget):
		return fmt.Errorf("%w: WorkspaceTarget is invalid", ErrInvalidCodexReviewSpec)
	case !digestPinnedImagePattern.MatchString(cfg.ObserverImage):
		return fmt.Errorf("%w: ObserverImage must be digest-pinned", ErrInvalidCodexReviewSpec)
	case cfg.Model == "" || !cliSafe(cfg.Model) ||
		cfg.ReasoningEffort == "" || !cliSafe(cfg.ReasoningEffort):
		return fmt.Errorf("%w: model configuration is invalid", ErrInvalidCodexReviewSpec)
	case launch.Prompt == "" || strings.IndexByte(launch.Prompt, 0) >= 0 ||
		len(launch.Prompt) > maxCodexReviewPromptBytes:
		return fmt.Errorf("%w: Prompt is invalid", ErrInvalidCodexReviewSpec)
	case launch.Boundary != CodexReviewFreshStart:
		return fmt.Errorf("%w: %w", ErrInvalidCodexReviewSpec, ErrCodexReviewContinuityRefused)
	case !launch.AuthMode.valid() || launch.AuthIdentityID == "":
		return fmt.Errorf("%w: auth identity is invalid", ErrInvalidCodexReviewSpec)
	case !cleanAbs(cfg.InputRoot):
		return fmt.Errorf("%w: InputRoot is invalid", ErrInvalidCodexReviewSpec)
	case cfg.AccessTokenLifetimeFloor <= 0 || cfg.Now == nil:
		return fmt.Errorf("%w: credential lifetime configuration is invalid", ErrInvalidCodexReviewSpec)
	case cfg.AccessTokenRefreshThreshold != 0 &&
		cfg.AccessTokenRefreshThreshold <= cfg.AccessTokenLifetimeFloor:
		return fmt.Errorf("%w: credential refresh threshold must exceed its lifetime floor", ErrInvalidCodexReviewSpec)
	case launch.AuthMode == CodexAuthSubscription &&
		(cfg.AuthStoreLeaser == nil || cfg.AuthRefresher == nil || cfg.AuthState == nil):
		return fmt.Errorf("%w: subscription host refresh dependencies are required", ErrInvalidCodexReviewSpec)
	case !slices.Equal(cfg.ProviderEndpoints, launch.AuthMode.providerEndpoints()):
		return fmt.Errorf("%w: provider_only endpoints do not exactly match auth mode", ErrInvalidCodexReviewSpec)
	}
	if err := launch.Instructions.validate(); err != nil ||
		launch.Instructions.Vendor != domain.AgentVendorCodex || !launch.Instructions.Present {
		return fmt.Errorf("%w: Codex instructions are invalid", ErrInvalidCodexReviewSpec)
	}
	if err := launch.InstructionBinding.Validate(); err != nil ||
		launch.InstructionBinding.ResultDigest != launch.Instructions.Digest {
		return fmt.Errorf("%w: Codex instruction provenance is invalid", ErrInvalidCodexReviewSpec)
	}
	if now := cfg.Now(); now.IsZero() || now.Location() != time.UTC {
		return fmt.Errorf("%w: credential clock is invalid", ErrInvalidCodexReviewSpec)
	}
	return nil
}

func validateCodexReviewLaunchStructure(cfg CodexReviewConfig, launch CodexReviewLaunchSpec) error {
	if err := validateCodexReviewLaunchShape(cfg, launch); err != nil {
		return err
	}
	authPath, authBody, err := readCodexReviewInput(
		cfg.InputRoot, launch.AuthSnapshot, maxCodexAuthSnapshotBytes,
	)
	if err != nil {
		return fmt.Errorf("%w: auth snapshot: %w", ErrInvalidCodexReviewSpec, err)
	}
	if _, _, err := inspectCodexHostAuth(launch.AuthMode, authBody); err != nil {
		return fmt.Errorf("%w: auth snapshot is invalid", ErrInvalidCodexReviewSpec)
	}
	instructionPath, instructionBody, err := readCodexReviewInput(
		cfg.InputRoot, launch.InstructionFile, domain.MaxVendorInstructionBytes,
	)
	if err != nil || !bytes.Equal(instructionBody, launch.Instructions.Body) ||
		authPath == instructionPath {
		return fmt.Errorf("%w: instruction snapshot is invalid", ErrInvalidCodexReviewSpec)
	}
	return nil
}

func validateCodexReviewLaunch(cfg CodexReviewConfig, launch CodexReviewLaunchSpec) error {
	if err := validateCodexReviewLaunchStructure(cfg, launch); err != nil {
		return err
	}
	_, authBody, err := readCodexReviewInput(
		cfg.InputRoot, launch.AuthSnapshot, maxCodexAuthSnapshotBytes,
	)
	if err != nil {
		return fmt.Errorf("%w: auth snapshot: %w", ErrInvalidCodexReviewSpec, err)
	}
	_, expires, err := codexReviewAgentAuthSnapshot(launch.AuthMode, authBody)
	if err != nil {
		return fmt.Errorf("%w: auth snapshot: %w", ErrInvalidCodexReviewSpec, err)
	}
	if expires != nil && expires.Sub(cfg.Now()) < cfg.AccessTokenLifetimeFloor {
		return fmt.Errorf(
			"%w: identity %q access token has %s remaining, floor %s",
			ErrInvalidCodexReviewSpec, launch.AuthIdentityID,
			expires.Sub(cfg.Now()), cfg.AccessTokenLifetimeFloor,
		)
	}
	return nil
}

// codexReviewIntentDigest deliberately commits only non-secret launch shape.
// The auth snapshot, prompt, and instruction body stay outside durable state;
// their live content is re-read and re-gated before the handoff boundary.
// WorkflowRunID routes a refresh failure to its owning run but does not change
// runtime topology, so excluding it preserves compatibility with open intents.
func codexReviewIntentDigest(cfg CodexReviewConfig, launch CodexReviewLaunchSpec) (string, error) {
	shape := struct {
		RunID, Image, WorkspaceSourceRunID, WorkspaceVolume, ExpectedHead     string
		Boundary                                                              CodexReviewBoundary
		AuthMode                                                              CodexAuthMode
		AuthIdentityID                                                        domain.AuthIdentityID
		InstructionBinding                                                    exec.ReviewInstructionBinding
		ApprovedImage, ObserverImage, WorkspaceTarget, Model, ReasoningEffort string
		ProviderEndpoints                                                     []string
	}{
		RunID: launch.RunID, Image: launch.Image, WorkspaceSourceRunID: launch.WorkspaceSourceRunID,
		WorkspaceVolume: launch.WorkspaceVolume, ExpectedHead: launch.ExpectedHead,
		Boundary: launch.Boundary, AuthMode: launch.AuthMode, AuthIdentityID: launch.AuthIdentityID,
		InstructionBinding: launch.InstructionBinding,
		ApprovedImage:      cfg.ApprovedImage, ObserverImage: cfg.ObserverImage,
		WorkspaceTarget: cfg.WorkspaceTarget, Model: cfg.Model, ReasoningEffort: cfg.ReasoningEffort,
		ProviderEndpoints: slices.Clone(cfg.ProviderEndpoints),
	}
	data, err := json.Marshal(shape)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum), nil
}

// RecoverCodexReview cleans only a durably-owned launch that did not cross the
// ReviewSource handoff boundary. It is intentionally cleanup-only: a
// fresh-context review is disposable, so even a Start whose lease transfer
// already succeeded is reaped rather than adopted, and resuming a partial
// observer/proxy topology would turn missing observations into trust.
func (b *CodexReviewLifecycle) RecoverCodexReview(
	ctx context.Context, cfg CodexReviewConfig, launch CodexReviewLaunchSpec,
) error {
	intent, err := cfg.Journal.GetCodexReviewIntent(ctx, launch.RunID)
	if err != nil {
		if errors.Is(err, ErrConformance) {
			return failf(CheckControlPlaneIsolation, "load Codex review recovery intent: %v", err)
		}
		return codexReviewOperationalCheckf(
			CheckControlPlaneIsolation, "load Codex review recovery intent: %v", err)
	}
	digest, err := codexReviewIntentDigest(cfg, launch)
	if err != nil || intent.validateFor(launch, digest) != nil {
		return failf(CheckControlPlaneIsolation, "Codex review recovery intent is invalid or does not match launch")
	}
	if intent.State == CodexReviewIntentStarted {
		return failf(CheckControlPlaneIsolation, "Codex review recovery belongs to ReviewSource after handoff")
	}
	if intent.State == CodexReviewIntentClosed {
		return nil
	}
	return b.recoverCodexReviewIntent(ctx, cfg, intent, false)
}

func (b *CodexReviewLifecycle) recoverCodexReviewIntent(
	ctx context.Context, cfg CodexReviewConfig, intent CodexReviewLaunchIntent, discardWorkspace bool,
) error {
	names, namesErr := intent.validatedResourceNames(intent.RunID)
	if namesErr != nil || intent.State == CodexReviewIntentStarted {
		return failf(CheckControlPlaneIsolation, "Codex review recovery intent is invalid")
	}
	if intent.State == CodexReviewIntentClosed {
		return nil
	}
	if err := b.authorizeRuntime(ctx, codexReviewRuntimeResourceNames(intent.RunID, names)); err != nil {
		return codexReviewOperationalCheckf(
			CheckControlPlaneIsolation, "authorize Codex review recovery resources: %v", err)
	}
	// Wipe the daemon's own host credential stage BEFORE the lease gate, so a
	// crash in the seeder window cannot strand the plaintext auth.json even when
	// the later runtime-object recovery fails closed. This is the daemon's own
	// host file, derived from the
	// trusted b.cfg.ExportRoot plus the validated runID, not a runtime object, so
	// it needs no lease, claim, or owner proof. Fail closed on error: return an
	// operational (retryable) error BEFORE acquiring the lease or closing the
	// intent, so the reconciler retries the whole recovery next cycle and the
	// intent stays open until the wipe succeeds. A transient FS error can never
	// close the intent over a surviving credential (the closed-intent path never
	// revisits this wipe). Idempotent: RemoveAll no-ops when absent, so a legacy
	// pre-#591 intent is a clean no-op.
	if stageErr := os.RemoveAll(codexReviewSnapshotStagePath(b.cfg.ExportRoot, intent.RunID)); stageErr != nil {
		return codexReviewOperationalCheckf(
			CheckCredentialSeparation, "remove recovery Codex review snapshot stage: %v", stageErr)
	}
	launch := CodexReviewLaunchSpec{
		RunID: intent.RunID, WorkspaceVolume: namesFor(intent.RunID).Workspace,
	}
	owner := Label{Key: ownershipLabelKey, Value: intent.OwnershipToken}
	claims, owners := codexReviewRecoveryEvidence(intent)
	preparationContainers := codexReviewPreparationContainers(names)
	// Preparation containers attach only one member of the leased set, so they
	// cannot pass the atomic-transfer reconstruction below. Reap only the exact
	// journaled names whose current runtime identity authenticates against the
	// intent. A foreign or unprovable candidate stays in place for the unchanged
	// lease gate to reject; the transferred review container is deliberately not
	// part of this pre-gate subset.
	cleanupErrs, listErr := b.reapCodexReviewRecoveryContainers(
		ctx, preparationContainers, claims, owners, false,
	)
	if listErr != nil {
		return codexReviewOperationalCheckf(
			CheckControlPlaneIsolation, "list pre-lease Codex review preparation containers: %v", listErr)
	}
	if len(cleanupErrs) > 0 {
		err := errors.Join(cleanupErrs...)
		if allCodexReviewFailuresOperational(cleanupErrs) {
			return codexReviewOperationalCheckf(
				CheckTeardown, "Codex review pre-lease preparation recovery: %v", err)
		}
		return failf(CheckTeardown, "Codex review pre-lease preparation recovery: %v", err)
	}
	leaseVolumes := codexReviewLeaseVolumes(launch.WorkspaceVolume, intent.ShadowVolume, intent.SnapshotVolume)
	lease, transfer, err := cfg.VolumeLifecycleLeaser.RecoverCodexReviewVolumeLease(
		ctx, owner.Value, leaseVolumes,
	)
	if err != nil {
		if !errors.Is(err, ErrCodexReviewVolumeLeaseTransferred) {
			if errors.Is(err, ErrCodexReviewVolumeLeaseForeignOwner) {
				return failf(CheckControlPlaneIsolation,
					"acquire Codex review recovery lease: %v", err)
			}
			return codexReviewOperationalCheckf(
				CheckControlPlaneIsolation, "acquire Codex review recovery lease: %v", err)
		}
		// A restarted coordinator reconstructs the same evidence for a stopped
		// container created before Start and for a container whose Start already
		// transferred the lease. Every valid state reaching this branch is before
		// the ReviewSource handoff, so reap the authenticated attachment, then
		// re-acquire the freed lease for ordinary cleanup and a fresh retry.
		if reapErr := b.reapCodexReviewTransferredAttachment(ctx, launch, intent, owner, transfer); reapErr != nil {
			return reapErr
		}
		lease, _, err = cfg.VolumeLifecycleLeaser.RecoverCodexReviewVolumeLease(
			ctx, owner.Value, leaseVolumes,
		)
		if err != nil {
			if errors.Is(err, ErrCodexReviewVolumeLeaseForeignOwner) ||
				errors.Is(err, ErrCodexReviewVolumeLeaseTransferred) {
				return failf(CheckControlPlaneIsolation,
					"reacquire Codex review recovery lease after reaping a transferred attachment: %v", err)
			}
			return codexReviewOperationalCheckf(CheckControlPlaneIsolation,
				"reacquire Codex review recovery lease after reaping a transferred attachment: %v", err)
		}
	}
	leaseReleasable := true
	leaseReleased := false
	defer func() {
		if !leaseReleasable || leaseReleased {
			return
		}
		_ = lease.ReleaseCodexReviewVolumeLease(context.WithoutCancel(ctx))
	}()
	cleanupErrs, listErr = b.reapCodexReviewRecoveryContainers(
		ctx, preparationContainers, claims, owners, true,
	)
	if listErr != nil {
		return codexReviewOperationalCheckf(
			CheckControlPlaneIsolation, "list Codex review recovery containers: %v", listErr)
	}
	containers, listErr := b.rt.ListContainers(ctx)
	if listErr != nil {
		return codexReviewOperationalCheckf(
			CheckControlPlaneIsolation, "list Codex review recovery containers: %v", listErr)
	}
	if slices.ContainsFunc(containers, func(c ContainerSummary) bool { return c.ID == names.reviewContainer }) {
		name := names.reviewContainer
		report, inspectErr := b.rt.Inspect(ctx, name)
		if inspectErr != nil {
			cleanupErrs = append(cleanupErrs, codexReviewOperationalf(
				"inspect Codex review recovery container %q: %v", name, inspectErr))
			leaseReleasable = false
		} else if report.ID != name || classifyEvidence(
			claims[name], owners[name], report.CreationDate, report.Labels, report.LabelsObserved,
		) != evidenceOurs {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("recovery container %q is foreign or unprovable", name))
			leaseReleasable = false
		} else if err := b.reapCodexReviewContainer(ctx, name, claims[name], owners[name]); err != nil {
			leaseReleasable = false
			cleanupErrs = append(cleanupErrs, err)
		}
	}
	volumes, listErr := b.rt.ListVolumes(ctx)
	if listErr != nil {
		cleanupErrs = append(cleanupErrs,
			codexReviewOperationalf("list recovery volumes: %v", listErr))
	} else {
		// The shadow and snapshot volumes are both ward-owned and part of the
		// exclusive lease; the candidate workspace is cleaned on its own path and
		// is never deleted here. A legacy intent carries no snapshot volume.
		recoveryVolumes := []string{intent.ShadowVolume}
		if intent.SnapshotVolume != "" {
			recoveryVolumes = append(recoveryVolumes, intent.SnapshotVolume)
		}
		for _, name := range recoveryVolumes {
			if !slices.ContainsFunc(volumes, func(v VolumeSummary) bool { return v.Name == name }) {
				continue
			}
			report, inspectErr := b.rt.InspectVolume(ctx, name)
			if inspectErr != nil {
				cleanupErrs = append(cleanupErrs,
					codexReviewOperationalf("inspect recovery volume %q: %v", name, inspectErr))
			} else if report.Name != name || classifyEvidence(
				claims[name], owners[name], report.CreationDate, report.Labels, report.LabelsObserved,
			) != evidenceOurs {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("recovery volume %q is foreign or unprovable", name))
			} else if err := b.deleteCodexReviewVolume(ctx, name, claims[name], owners[name]); err != nil {
				cleanupErrs = append(cleanupErrs, err)
			}
		}
	}
	networks, listErr := b.rt.ListNetworks(ctx)
	if listErr != nil {
		cleanupErrs = append(cleanupErrs,
			codexReviewOperationalf("list recovery networks: %v", listErr))
	} else if slices.ContainsFunc(networks, func(n NetworkSummary) bool { return n.Name == intent.Network }) {
		report, inspectErr := b.rt.InspectNetwork(ctx, intent.Network)
		if inspectErr != nil {
			cleanupErrs = append(cleanupErrs,
				codexReviewOperationalf("inspect recovery network: %v", inspectErr))
		} else if report.Name != intent.Network || classifyEvidence(
			claims[intent.Network], owners[intent.Network], report.CreationDate, report.Labels, report.LabelsObserved,
		) != evidenceOurs {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("recovery network is foreign or unprovable"))
		} else if err := b.teardownCodexReviewNetwork(
			ctx, intent.Network, claims[intent.Network], owners[intent.Network],
		); err != nil {
			cleanupErrs = append(cleanupErrs, err)
		}
	}
	if err := errors.Join(cleanupErrs...); err != nil {
		if allCodexReviewFailuresOperational(cleanupErrs) {
			return codexReviewOperationalCheckf(
				CheckTeardown, "Codex review pre-start recovery: %v", err)
		}
		return failf(CheckTeardown, "Codex review pre-start recovery: %v", err)
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.cfg.TeardownTimeout)
	defer cancel()
	if err := lease.ReleaseCodexReviewVolumeLease(cleanupCtx); err != nil {
		return codexReviewOperationalCheckf(
			CheckControlPlaneIsolation, "release Codex review recovery lease: %v", err)
	}
	leaseReleased = true
	if discardWorkspace {
		if err := b.CleanupCodexReviewWorkspace(ctx, cfg.Journal, intent.RunID); err != nil {
			return err
		}
	}
	if err := cfg.Journal.CloseCodexReviewIntent(ctx, launch.RunID); err != nil {
		if errors.Is(err, ErrConformance) {
			return failf(CheckControlPlaneIsolation, "close recovered Codex review intent: %v", err)
		}
		return codexReviewOperationalCheckf(
			CheckControlPlaneIsolation, "close recovered Codex review intent: %v", err)
	}
	if discardWorkspace {
		if err := b.cleanupOrphanedCodexReviewWorkspace(ctx, cfg.Journal, intent.RunID); err != nil {
			return err
		}
	}
	return nil
}

func codexReviewRecoveryEvidence(
	intent CodexReviewLaunchIntent,
) (map[string]objectClaim, map[string]Label) {
	claims := make(map[string]objectClaim, len(intent.Resources))
	owners := make(map[string]Label, len(intent.Resources))
	for _, resource := range intent.Resources {
		claims[resource.Name] = objectClaim{attempted: true, owned: true, fingerprint: resource.Fingerprint}
		// Ward-created resources journal the intent token explicitly. Observers
		// journal their independently minted token before CreateContainer; an
		// empty observer token therefore proves no legitimate observer could yet
		// exist and must not inherit the intent owner during destructive recovery.
		owners[resource.Name] = Label{Key: ownershipLabelKey, Value: resource.OwnershipToken}
	}
	return claims, owners
}

func codexReviewPreparationContainers(names codexReviewResourceNames) []string {
	containers := []string{names.workspaceObserver, names.shadowInitializer, names.shadowObserver}
	if names.snapshotSeeder != "" {
		containers = append(containers, names.snapshotSeeder)
	}
	if names.snapshotObserver != "" {
		containers = append(containers, names.snapshotObserver)
	}
	return containers
}

// reapCodexReviewRecoveryContainers reaps authenticated containers from one
// caller-selected resource subset. Pre-lease recovery tolerates foreign or
// unprovable candidates so the lease gate remains the fail-closed authority;
// post-lease cleanup reports the same evidence as a teardown contradiction.
func (b *CodexReviewLifecycle) reapCodexReviewRecoveryContainers(
	ctx context.Context,
	names []string,
	claims map[string]objectClaim,
	owners map[string]Label,
	requireOwned bool,
) ([]error, error) {
	containers, err := b.rt.ListContainers(ctx)
	if err != nil {
		return nil, err
	}
	var cleanupErrs []error
	for _, name := range names {
		if !slices.ContainsFunc(containers, func(c ContainerSummary) bool { return c.ID == name }) {
			continue
		}
		report, inspectErr := b.rt.Inspect(ctx, name)
		if inspectErr != nil {
			cleanupErrs = append(cleanupErrs, codexReviewOperationalf(
				"inspect Codex review recovery container %q: %v", name, inspectErr))
			continue
		}
		owner, ownerJournaled := owners[name]
		if report.ID != name || !ownerJournaled || !codexReviewOwnershipLabelValid(owner) || classifyEvidence(
			claims[name], owner, report.CreationDate, report.Labels, report.LabelsObserved,
		) != evidenceOurs {
			if requireOwned {
				cleanupErrs = append(cleanupErrs, fmt.Errorf(
					"recovery container %q is foreign or unprovable", name))
			}
			continue
		}
		if err := b.reapCodexReviewContainer(ctx, name, claims[name], owners[name]); err != nil {
			cleanupErrs = append(cleanupErrs, err)
		}
	}
	return cleanupErrs, nil
}

// CodexReviewRecovery converges only durable review topology left by a prior
// daemon. It carries no launch credentials, prompt, instructions, or reviewer
// configuration. The optional input root only lets it remove an already
// materialized daemon-owned snapshot, never read or launch from one.
type CodexReviewRecovery struct {
	lifecycle *CodexReviewLifecycle
	cfg       CodexReviewConfig
}

func NewCodexReviewRecovery(
	lifecycle *CodexReviewLifecycle, journal CodexReviewJournal, leaser CodexReviewVolumeLifecycleLeaser, inputRoot string,
) (*CodexReviewRecovery, error) {
	if !lifecycle.valid() || journal == nil || leaser == nil || inputRoot != "" && !cleanAbs(inputRoot) {
		return nil, errors.New("nil Codex review recovery dependency")
	}
	return &CodexReviewRecovery{lifecycle: lifecycle, cfg: CodexReviewConfig{
		Journal: journal, VolumeLifecycleLeaser: leaser, InputRoot: inputRoot,
	}}, nil
}

// Reconcile retries every non-closed launch and every closed launch whose
// terminal outcome still needs its ready mark. A started container cannot be
// adopted after restart because its daemon-owned proxy is gone, so recovery
// durably records that loss before authenticated abort cleanup.
func (r *CodexReviewRecovery) Reconcile(ctx context.Context) error {
	ids, err := r.cfg.Journal.ListCodexReviewIntentIDs(ctx)
	if err != nil {
		return codexReviewJournalCheckf(
			CheckControlPlaneIsolation, "list recoverable Codex review intents: %v", err)
	}
	var recoveryErrs []error
	intentIDs := make(map[string]struct{}, len(ids))
	knownIntentIDs := make(map[string]struct{}, len(ids))
	allIntentsAuthenticated := true
	for _, id := range ids {
		intent, err := r.cfg.Journal.GetCodexReviewIntent(ctx, id)
		if err != nil {
			allIntentsAuthenticated = false
			recoveryErrs = append(recoveryErrs, fmt.Errorf("recover Codex review %q: %w", id,
				codexReviewJournalCheckf(CheckControlPlaneIsolation,
					"load recoverable Codex review intent: %v", err)))
			continue
		}
		if intent.validateIdentity(id) != nil {
			allIntentsAuthenticated = false
			recoveryErrs = append(recoveryErrs, fmt.Errorf("recover Codex review %q: %w", id,
				failf(CheckControlPlaneIsolation, "recoverable Codex review intent is invalid")))
			continue
		}
		knownIntentIDs[id] = struct{}{}
		if intent.State != CodexReviewIntentClosed {
			intentIDs[id] = struct{}{}
		}
		if err := r.reconcileIntent(ctx, id, intent); err != nil {
			recoveryErrs = append(recoveryErrs, fmt.Errorf("recover Codex review %q: %w", id, err))
		}
	}
	if !allIntentsAuthenticated {
		return errors.Join(recoveryErrs...)
	}
	workspaceIDs, err := r.cfg.Journal.ListCodexReviewWorkspaceIDs(ctx)
	if err != nil {
		recoveryErrs = append(recoveryErrs, codexReviewJournalCheckf(
			CheckControlPlaneIsolation, "list recoverable Codex review workspaces: %v", err))
		return errors.Join(recoveryErrs...)
	}
	orphanWorkspaceIDs := make(map[string]struct{}, len(workspaceIDs))
	for _, id := range workspaceIDs {
		if _, exists := intentIDs[id]; exists {
			continue
		}
		if err := r.removeInstructionSnapshot(id); err != nil {
			orphanWorkspaceIDs[id] = struct{}{}
			recoveryErrs = append(recoveryErrs,
				fmt.Errorf("recover orphaned Codex review snapshot %q: %w", id, err))
			continue
		}
		if err := r.lifecycle.cleanupOrphanedCodexReviewWorkspace(ctx, r.cfg.Journal, id); err != nil {
			orphanWorkspaceIDs[id] = struct{}{}
			recoveryErrs = append(recoveryErrs,
				fmt.Errorf("recover orphaned Codex review workspace %q: %w", id, err))
		}
	}
	outcomeIDs, err := r.cfg.Journal.ListCodexReviewOutcomeIDs(ctx)
	if err != nil {
		recoveryErrs = append(recoveryErrs, codexReviewJournalCheckf(
			CheckControlPlaneIsolation, "list recoverable Codex review outcomes: %v", err))
		return errors.Join(recoveryErrs...)
	}
	for _, id := range outcomeIDs {
		if _, exists := knownIntentIDs[id]; exists {
			continue
		}
		if _, exists := orphanWorkspaceIDs[id]; exists {
			continue
		}
		if err := r.markFenceOnlyOutcomeReady(ctx, id); err != nil {
			recoveryErrs = append(recoveryErrs,
				fmt.Errorf("recover fence-only Codex review outcome %q: %w", id, err))
		}
	}
	return errors.Join(recoveryErrs...)
}

// markFenceOnlyOutcomeReady converges a rejection fence written before a
// launch persisted any intent or workspace. There is no runtime topology to
// authenticate in this state; re-reading and validating the durable outcome
// is the only required recovery evidence before its ready transition.
func (r *CodexReviewRecovery) markFenceOnlyOutcomeReady(ctx context.Context, id string) error {
	outcome, ready, err := r.cfg.Journal.GetCodexReviewOutcome(ctx, id)
	if err != nil {
		return codexReviewJournalCheckf(
			CheckControlPlaneIsolation, "load fence-only Codex review outcome: %v", err)
	}
	if err := outcome.Validate(); err != nil || string(outcome.InvocationID) != id {
		return failf(CheckControlPlaneIsolation, "fence-only Codex review outcome is invalid: %v",
			errors.Join(err, domain.ErrParentKeyMismatch))
	}
	if ready {
		return nil
	}
	if err := r.cfg.Journal.MarkCodexReviewOutcomeReady(ctx, id); err != nil {
		return codexReviewJournalCheckf(
			CheckControlPlaneIsolation, "mark fence-only Codex review outcome ready: %v", err)
	}
	return nil
}

func (r *CodexReviewRecovery) reconcileIntent(
	ctx context.Context, id string, intent CodexReviewLaunchIntent,
) error {
	_, ready, outcomeErr := r.cfg.Journal.GetCodexReviewOutcome(ctx, id)
	outcomeMissing := errors.Is(outcomeErr, ErrCodexReviewOutcomeNotFound)
	outcomeRejected := errors.Is(outcomeErr, ErrCodexReviewOutcomeRejected)
	if outcomeErr != nil && !outcomeMissing && !outcomeRejected {
		return codexReviewJournalCheckf(
			CheckControlPlaneIsolation, "load recoverable Codex review outcome: %v", outcomeErr)
	}
	if intent.State == CodexReviewIntentClosed {
		if err := r.removeInstructionSnapshot(id); err != nil {
			return err
		}
		if outcomeErr == nil && !ready {
			if err := r.cfg.Journal.MarkCodexReviewOutcomeReady(ctx, id); err != nil {
				return codexReviewJournalCheckf(
					CheckControlPlaneIsolation, "mark recovered Codex review outcome ready: %v", err)
			}
		}
		if outcomeRejected {
			// Recovery owns topology convergence, not result authority. A closed
			// intent proves no credential-bearing resources remain; leave the
			// rejected outcome for ReviewSource to fail closed if a workflow still
			// tries to consume it. This also lets upgrades pass historical outcomes
			// whose pre-instruction schema the current result decoder rejects.
			return nil
		}
		return nil
	}
	if intent.State != CodexReviewIntentStarted {
		cleanupErr := r.lifecycle.recoverCodexReviewIntent(ctx, r.cfg, intent, true)
		if cleanupErr == nil {
			cleanupErr = r.removeInstructionSnapshot(id)
		}
		if cleanupErr == nil && outcomeErr == nil && !ready {
			if err := r.cfg.Journal.MarkCodexReviewOutcomeReady(ctx, id); err != nil {
				cleanupErr = codexReviewJournalCheckf(
					CheckControlPlaneIsolation, "mark recovered Codex review outcome ready: %v", err)
			}
		}
		if outcomeRejected {
			return errors.Join(cleanupErr, outcomeErr)
		}
		return cleanupErr
	}
	if outcomeMissing {
		outcome := CodexReviewSourceOutcome{
			InvocationID:  domain.InvocationID(id),
			FailureClass:  domain.ReviewFailureTransient,
			Failure:       "daemon restarted while Codex review was running; the invocation proxy was lost",
			AbortRequired: true,
		}
		if err := r.cfg.Journal.PutCodexReviewOutcome(ctx, id, outcome); err != nil {
			return codexReviewJournalCheckf(
				CheckControlPlaneIsolation, "persist recovered Codex review outcome: %v", err)
		}
	}
	cleanupErr := r.lifecycle.AbortCodexReview(ctx, r.cfg, id)
	if cleanupErr == nil {
		cleanupErr = r.removeInstructionSnapshot(id)
	}
	if cleanupErr == nil && !outcomeRejected {
		if err := r.cfg.Journal.MarkCodexReviewOutcomeReady(ctx, id); err != nil {
			cleanupErr = codexReviewJournalCheckf(
				CheckControlPlaneIsolation, "mark recovered Codex review outcome ready: %v", err)
		}
	}
	if outcomeRejected {
		return errors.Join(cleanupErr, outcomeErr)
	}
	return cleanupErr
}

func (r *CodexReviewRecovery) removeInstructionSnapshot(id string) error {
	if r.cfg.InputRoot == "" {
		return nil
	}
	if err := removeCodexReviewInstructionSnapshot(r.cfg.InputRoot, id); err != nil {
		return codexReviewOperationalCheckf(
			CheckControlPlaneIsolation, "remove recovered review instruction snapshot: %v", err)
	}
	return nil
}

func allCodexReviewFailuresOperational(errs []error) bool {
	return len(errs) > 0 && !slices.ContainsFunc(errs, func(err error) bool {
		return !errors.Is(err, ErrCodexReviewOperational)
	})
}

// reapCodexReviewTransferredAttachment resolves a review container whose two
// volume attachments survived the launching process. The evidence cannot say
// whether Start ran, and adoption is deliberately not offered: after Start the
// proxy listener died with the process, while before Start the remaining live
// observations are incomplete. Ward authenticates the deployment's evidence
// against the durable intent, then stops and deletes the recorded owner's
// container and proves it absent; a foreign or unprovable object refuses,
// leaving the attachment visible.
func (b *CodexReviewLifecycle) reapCodexReviewTransferredAttachment(
	ctx context.Context,
	launch CodexReviewLaunchSpec,
	intent CodexReviewLaunchIntent,
	owner Label,
	transfer CodexReviewVolumeLeaseTransfer,
) error {
	if transfer.Holder != owner.Value ||
		!slices.Equal(transfer.Volumes, codexReviewLeaseVolumes(launch.WorkspaceVolume, intent.ShadowVolume, intent.SnapshotVolume)) ||
		transfer.Container != intent.ReviewContainer {
		return failf(CheckControlPlaneIsolation, "Codex review starting lease transfer is foreign or malformed")
	}
	var claim objectClaim
	for _, resource := range intent.Resources {
		if resource.Name == intent.ReviewContainer {
			claim = objectClaim{attempted: true, owned: true, fingerprint: resource.Fingerprint}
		}
	}
	containers, err := b.rt.ListContainers(ctx)
	if err != nil {
		return codexReviewOperationalCheckf(CheckControlPlaneIsolation,
			"list containers for transferred Codex review recovery: %v", err)
	}
	if !slices.ContainsFunc(containers, func(c ContainerSummary) bool { return c.ID == intent.ReviewContainer }) {
		return b.verifyCodexReviewContainerAbsent(
			ctx, intent.ReviewContainer, claim, owner)
	}
	report, err := b.rt.Inspect(ctx, intent.ReviewContainer)
	if err != nil {
		return codexReviewOperationalCheckf(CheckControlPlaneIsolation,
			"inspect transferred Codex review container: %v", err)
	}
	if report.ID != intent.ReviewContainer || classifyEvidence(
		claim, owner, report.CreationDate, report.Labels, report.LabelsObserved,
	) != evidenceOurs {
		return failf(CheckControlPlaneIsolation, "transferred Codex review container is foreign or unprovable")
	}
	if err := b.reapCodexReviewContainer(ctx, intent.ReviewContainer, claim, owner); err != nil {
		if errors.Is(err, ErrCodexReviewOperational) {
			return codexReviewOperationalCheckf(CheckControlPlaneIsolation,
				"reap transferred Codex review container: %v", err)
		}
		return failf(CheckControlPlaneIsolation, "reap transferred Codex review container: %v", err)
	}
	return nil
}

func (intent CodexReviewLaunchIntent) validateFor(launch CodexReviewLaunchSpec, digest string) error {
	if intent.SpecDigest != digest {
		return errors.New("launch identity is invalid")
	}
	return intent.validateIdentity(launch.RunID)
}

// validateIdentity re-derives every deterministic resource name from the
// caller-supplied run id and rejects a stored intent that diverges. Teardown
// boundaries run it before trusting the row: under a rewritten-journal
// threat model, a stored name that cannot be re-derived could redirect
// destruction at a sibling invocation's resources or fake convergence by
// naming resources that never existed.
func (intent CodexReviewLaunchIntent) validateIdentity(runID string) error {
	_, err := intent.validatedResourceNames(runID)
	return err
}

// validatedResourceNames authenticates both the current topology and the one
// exact legacy topology that an upgrade may need to reap. Matching individual
// legacy names is insufficient: the complete stored set must be one coherent
// generation, preventing a rewritten row from composing teardown targets.
func (intent CodexReviewLaunchIntent) validatedResourceNames(runID string) (codexReviewResourceNames, error) {
	if intent.RunID != runID ||
		!codexReviewOwnershipLabelValid(Label{Key: ownershipLabelKey, Value: intent.OwnershipToken}) {
		return codexReviewResourceNames{}, errors.New("launch identity is invalid")
	}
	if !intent.State.valid() {
		return codexReviewResourceNames{}, errors.New("launch state is invalid")
	}
	// Order matters only for disambiguating the two six-resource generations by
	// their observer names; resourceNamesMatch keys the current nine-resource
	// shape off a non-empty snapshot volume, so a persisted pre-#591 intent
	// (run 482's round-2 record) is authenticated as cleanup-only legacy.
	for _, names := range []codexReviewResourceNames{
		codexReviewNames(runID), preSnapshotCodexReviewNames(runID), legacyCodexReviewNames(runID),
	} {
		if intent.resourceNamesMatch(names) {
			return names, nil
		}
	}
	return codexReviewResourceNames{}, errors.New("launch resources are invalid")
}

func (intent CodexReviewLaunchIntent) resourceNamesMatch(names codexReviewResourceNames) bool {
	if intent.ShadowVolume != names.shadowVolume || intent.Network != names.network ||
		intent.ReviewContainer != names.reviewContainer || intent.SnapshotVolume != names.snapshotVolume {
		return false
	}
	expected := map[string]struct{}{
		names.workspaceObserver: {},
		names.shadowInitializer: {},
		names.shadowObserver:    {},
		intent.ReviewContainer:  {},
		intent.ShadowVolume:     {},
		intent.Network:          {},
	}
	// The snapshot volume is the discriminator between the nine-resource #591
	// shape and the six-resource host-bind generations. An empty snapshotVolume
	// (legacy/pre-snapshot) omits all three snapshot resources.
	if names.snapshotVolume != "" {
		expected[names.snapshotVolume] = struct{}{}
		expected[names.snapshotSeeder] = struct{}{}
		expected[names.snapshotObserver] = struct{}{}
	}
	if len(intent.Resources) != len(expected) {
		return false
	}
	for _, resource := range intent.Resources {
		if _, ok := expected[resource.Name]; !ok {
			return false
		}
		delete(expected, resource.Name)
		if resource.OwnershipToken != "" && !ownershipTokenPattern.MatchString(resource.OwnershipToken) {
			return false
		}
		// Resources ward creates directly carry the durable owner token; the three
		// observers mint their own. snapshotSeeder and snapshotVolume are
		// ward-created, so they must bear the intent owner; snapshotObserver may not.
		if resource.Name == intent.ShadowVolume || resource.Name == intent.Network ||
			resource.Name == intent.ReviewContainer || resource.Name == names.shadowInitializer ||
			(names.snapshotVolume != "" && (resource.Name == names.snapshotVolume || resource.Name == names.snapshotSeeder)) {
			if resource.OwnershipToken != intent.OwnershipToken {
				return false
			}
		}
	}
	return len(expected) == 0
}

func (b CodexReviewWorkspaceBinding) validateFor(launch CodexReviewLaunchSpec) error {
	if b.SourceRunID != launch.WorkspaceSourceRunID ||
		b.Volume != launch.WorkspaceVolume ||
		!ownershipTokenPattern.MatchString(b.OwnershipToken) ||
		b.CreationFingerprint == "" {
		return failf(CheckObservedBaseIdentity, "Codex review workspace provenance is invalid")
	}
	return nil
}

func (b *CodexReviewLifecycle) initializeCodexReviewShadow(
	ctx context.Context, cfg CodexReviewConfig, runID, volume string, owner Label,
	mark func(CodexReviewIntentResource) error,
) (retErr error) {
	spec := ContainerSpec{
		Name:    codexReviewShadowInitializerName(runID),
		Image:   cfg.ObserverImage,
		Command: []string{"sh", "-c", stateSeederScript(codexShadowObserverTarget, stateManifestEmpty)},
		Mounts: []Mount{{
			Type: MountVolume, Source: volume, Target: codexShadowObserverTarget,
		}},
		Labels:          append(runLabels(runID), owner),
		NetworkDisabled: true,
	}
	claim := objectClaim{attempted: true}
	defer func() {
		if retErr == nil || !claim.attempted {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.cfg.TeardownTimeout)
		defer cancel()
		if cleanupErr := b.reapUnlistedContainer(cleanupCtx, spec.Name, claim, owner); cleanupErr != nil {
			retErr = errors.Join(retErr, failf(
				CheckControlPlaneIsolation, "Codex review shadow initializer cleanup: %v", cleanupErr,
			))
		}
	}()
	if err := b.rt.CreateContainer(ctx, cloneContainerSpec(spec)); err != nil {
		return codexReviewOperationalCheckf(
			CheckControlPlaneIsolation, "create Codex review shadow initializer: %v", err,
		)
	}
	claim.owned = true
	rep, err := b.rt.Inspect(ctx, spec.Name)
	if err != nil {
		return codexReviewOperationalCheckf(
			CheckControlPlaneIsolation, "inspect Codex review shadow initializer: %v", err,
		)
	}
	if err := verifySeedRoleAllowlist(
		rep, spec, volume, codexShadowObserverTarget, CheckControlPlaneIsolation,
	); err != nil {
		return err
	}
	claim.fingerprint, err = ownedFingerprint(
		rep.CreationDate, rep.Labels, rep.LabelsObserved, owner,
	)
	if err != nil {
		return failf(CheckControlPlaneIsolation, "authenticate Codex review shadow initializer: %v", err)
	}
	if err := mark(CodexReviewIntentResource{Name: spec.Name, Fingerprint: claim.fingerprint}); err != nil {
		return codexReviewJournalCheckf(
			CheckControlPlaneIsolation, "journal Codex review shadow initializer: %v", err,
		)
	}
	if err := b.rt.StartContainer(ctx, spec.Name); err != nil {
		return codexReviewOperationalCheckf(
			CheckControlPlaneIsolation, "start Codex review shadow initializer: %v", err,
		)
	}
	if err := b.waitStopped(ctx, spec.Name, claim, owner, b.cfg.SeedTimeout); err != nil {
		return codexReviewOperationalCheckf(
			CheckControlPlaneIsolation, "wait for Codex review shadow initializer: %v", err,
		)
	}
	if err := b.rt.DeleteContainer(ctx, spec.Name); err != nil {
		return codexReviewOperationalCheckf(
			CheckControlPlaneIsolation, "delete Codex review shadow initializer: %v", err,
		)
	}
	if err := b.verifyContainerAbsent(
		ctx, spec.Name, claim, owner, CheckControlPlaneIsolation,
	); err != nil {
		return err
	}
	claim.attempted = false
	return nil
}

// codexReviewSnapshotStagePath is the deterministic, runID-keyed host directory
// where seedCodexReviewSnapshot stages the two admitted files before copying
// them into the networkless seeder. Its root is the trusted daemon-owned
// ExportRoot (b.cfg.ExportRoot: validated cleanAbs at init, stable across
// restart, independent of the mutable -review-input-root), so both seeding and
// recoverCodexReviewIntent re-derive the exact same path from the same trusted
// Backend config, never from the journal. That trusted-and-stable derivation is
// what lets recovery reap a crashed run's plaintext-credential residue that the
// seeder's defer wipe could not, and it fails closed (recovery always knows the
// one root). runID is runIDPattern-validated upstream, so it carries no path
// separators.
func codexReviewSnapshotStagePath(exportRoot, runID string) string {
	return filepath.Join(exportRoot, ".codex-review-snapshot-stage-"+runID)
}

// createPrivateStageDir creates path as a fresh, private (0700), daemon-owned
// directory, defeating a pre-created symlink or residue under a possibly-shared
// root (ExportRoot can default to a shared temp dir). It removes any existing
// entry, including a symlink (RemoveAll unlinks the symlink itself, never its
// target), recreates the directory, then Lstat-verifies the result is a real,
// unsymlinked, 0700, euid-owned directory before the caller writes secrets into
// it. This closes a /tmp-style pre-attack on the credential stage.
func createPrivateStageDir(path string) error {
	if err := os.Mkdir(path, 0o700); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			return err
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, owned := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 ||
		!owned || !codexReviewUIDMatches(stat, os.Geteuid()) {
		return errors.New("stage is not a private daemon-owned directory")
	}
	return nil
}

// seedCodexReviewSnapshot places exactly the two admitted files onto the fresh
// read-only credential volume using the running-seeder + CopyIntoContainer
// pattern, so the credential bytes transit only a ward-owned, networkless,
// pinned-image container that is proven absent afterward. The host stage is a
// private 0700 directory with 0400 files at a deterministic, recovery-reapable
// path, wiped on return; a crash before that wipe is swept by
// recoverCodexReviewIntent.
func (b *CodexReviewLifecycle) seedCodexReviewSnapshot(
	ctx context.Context, cfg CodexReviewConfig, launch CodexReviewLaunchSpec, volume string, owner Label,
	mark func(CodexReviewIntentResource) error,
) (retErr error) {
	_, hostAuthBody, err := readCodexReviewInput(cfg.InputRoot, launch.AuthSnapshot, maxCodexAuthSnapshotBytes)
	if err != nil {
		return failf(CheckCredentialSeparation, "Codex review auth snapshot changed before snapshot seeding")
	}
	authBody, _, err := codexReviewAgentAuthSnapshot(launch.AuthMode, hostAuthBody)
	if err != nil {
		return failf(CheckCredentialSeparation, "Codex review auth snapshot changed before snapshot seeding")
	}
	_, instructionBody, err := readCodexReviewInput(cfg.InputRoot, launch.InstructionFile, domain.MaxVendorInstructionBytes)
	if err != nil || !bytes.Equal(instructionBody, launch.Instructions.Body) {
		return failf(CheckCredentialSeparation, "Codex review instruction snapshot changed before snapshot seeding")
	}
	// The stage is a deterministic, runID-keyed private 0700 directory under the
	// trusted daemon-owned ExportRoot (not the mutable -review-input-root and not
	// the journal), so no other host user can read the credential bytes and
	// recovery re-derives the exact same path from the same trusted config. The
	// files are 0400 and the whole stage is wiped on return whether or not seeding
	// succeeded; the wipe error is surfaced rather than dropped, because a crash
	// before the defer runs leaves the plaintext auth.json for
	// recoverCodexReviewIntent to reap. createPrivateStageDir defeats a residue or
	// pre-created symlink at the path under a possibly-shared ExportRoot.
	dir := codexReviewSnapshotStagePath(b.cfg.ExportRoot, launch.RunID)
	// Host-filesystem staging failures below are transient (full disk, temporary
	// permission), so they are operational/retryable, not durable contradictions:
	// a passing full disk must not terminate the review. codexReviewOperationalCheckf
	// keeps the credential-separation check context while classifying retryable, the
	// same way the seeder's create/start/inspect steps already are.
	if err := createPrivateStageDir(dir); err != nil {
		return codexReviewOperationalCheckf(CheckCredentialSeparation, "create Codex review snapshot stage: %v", err)
	}
	defer func() {
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			retErr = errors.Join(retErr, codexReviewOperationalCheckf(
				CheckCredentialSeparation, "wipe Codex review snapshot stage: %v", rmErr,
			))
		}
	}()
	if err := os.WriteFile(filepath.Join(dir, codexReviewSnapshotAuthName), authBody, 0o400); err != nil {
		return codexReviewOperationalCheckf(CheckCredentialSeparation, "stage Codex review snapshot auth: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, codexReviewSnapshotInstrName), instructionBody, 0o400); err != nil {
		return codexReviewOperationalCheckf(CheckCredentialSeparation, "stage Codex review snapshot instructions: %v", err)
	}
	readyDir, err := os.MkdirTemp("", "freeside-codex-review-snapshot-ready-"+launch.RunID+"-")
	if err != nil {
		return codexReviewOperationalCheckf(CheckCredentialSeparation, "create Codex review snapshot sentinel: %v", err)
	}
	defer os.RemoveAll(readyDir) //nolint:errcheck // best-effort cleanup of a host sentinel dir
	if err := os.WriteFile(filepath.Join(readyDir, seedReadyFile), []byte("ready\n"), 0o600); err != nil {
		return codexReviewOperationalCheckf(CheckCredentialSeparation, "write Codex review snapshot sentinel: %v", err)
	}
	spec := ContainerSpec{
		Name:    codexReviewSnapshotSeederName(launch.RunID),
		Image:   cfg.ObserverImage,
		Command: []string{"sh", "-c", codexReviewSnapshotSeederScript(b.cfg.seedConfig(), codexReviewSnapshotSeedTarget)},
		Mounts: []Mount{{
			Type: MountVolume, Source: volume, Target: codexReviewSnapshotSeedTarget,
		}},
		Labels:          append(runLabels(launch.RunID), owner),
		NetworkDisabled: true,
	}
	claim := objectClaim{attempted: true}
	defer func() {
		if retErr == nil || !claim.attempted {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.cfg.TeardownTimeout)
		defer cancel()
		if cleanupErr := b.reapUnlistedContainer(cleanupCtx, spec.Name, claim, owner); cleanupErr != nil {
			retErr = errors.Join(retErr, failf(
				CheckCredentialSeparation, "Codex review snapshot seeder cleanup: %v", cleanupErr,
			))
		}
	}()
	if err := b.rt.CreateContainer(ctx, cloneContainerSpec(spec)); err != nil {
		return codexReviewOperationalCheckf(CheckCredentialSeparation, "create Codex review snapshot seeder: %v", err)
	}
	claim.owned = true
	rep, err := b.rt.Inspect(ctx, spec.Name)
	if err != nil {
		return codexReviewOperationalCheckf(CheckCredentialSeparation, "inspect Codex review snapshot seeder: %v", err)
	}
	if err := verifySeedRoleAllowlist(rep, spec, volume, codexReviewSnapshotSeedTarget, CheckCredentialSeparation); err != nil {
		return err
	}
	claim.fingerprint, err = ownedFingerprint(rep.CreationDate, rep.Labels, rep.LabelsObserved, owner)
	if err != nil {
		return failf(CheckCredentialSeparation, "authenticate Codex review snapshot seeder: %v", err)
	}
	if err := mark(CodexReviewIntentResource{Name: spec.Name, Fingerprint: claim.fingerprint}); err != nil {
		return codexReviewJournalCheckf(CheckCredentialSeparation, "journal Codex review snapshot seeder: %v", err)
	}
	if err := b.rt.StartContainer(ctx, spec.Name); err != nil {
		return codexReviewOperationalCheckf(CheckCredentialSeparation, "start Codex review snapshot seeder: %v", err)
	}
	if err := b.copyIntoSeeder(ctx, spec.Name, dir, b.cfg.SeedStageDir); err != nil {
		// copyIntoSeeder classifies its CLI copy failure as a durable contradiction
		// (shared with the workspace seeder); for the credential stage a transient
		// host-copy failure is retryable, so reclassify operational at this site.
		return codexReviewOperationalCheckf(CheckCredentialSeparation, "copy Codex review snapshot stage into seeder: %v", err)
	}
	if err := b.copyIntoSeeder(ctx, spec.Name, readyDir, b.cfg.SeedReadyDir); err != nil {
		return codexReviewOperationalCheckf(CheckCredentialSeparation, "copy Codex review snapshot sentinel into seeder: %v", err)
	}
	if err := b.waitStopped(ctx, spec.Name, claim, owner, b.cfg.SeedTimeout); err != nil {
		return codexReviewOperationalCheckf(CheckCredentialSeparation, "wait for Codex review snapshot seeder: %v", err)
	}
	if err := b.rt.DeleteContainer(ctx, spec.Name); err != nil {
		return codexReviewOperationalCheckf(CheckCredentialSeparation, "delete Codex review snapshot seeder: %v", err)
	}
	if err := b.verifyContainerAbsent(ctx, spec.Name, claim, owner, CheckCredentialSeparation); err != nil {
		return err
	}
	claim.attempted = false
	return nil
}

func (b *CodexReviewLifecycle) observeCodexReviewSnapshot(
	ctx context.Context, cfg CodexReviewConfig, runID, volume string,
	volumeOwner Label, volumeReport VolumeSummary,
) (CodexReviewSnapshotObservation, error) {
	observerOwner, err := newOwnershipLabel()
	if err != nil {
		return CodexReviewSnapshotObservation{}, failf(CheckCredentialSeparation,
			"mint Codex review snapshot observer ownership: %v", err)
	}
	spec, err := BuildCodexReviewSnapshotObserverSpec(cfg, runID, volume, observerOwner)
	if err != nil {
		return CodexReviewSnapshotObservation{}, err
	}
	if err := cfg.Journal.MarkCodexReviewIntentResource(ctx, runID,
		CodexReviewIntentResource{Name: spec.Name, OwnershipToken: observerOwner.Value}); err != nil {
		return CodexReviewSnapshotObservation{}, codexReviewJournalCheckf(
			CheckCredentialSeparation, "journal snapshot observer intent: %v", err,
		)
	}
	report, proof, err := b.runCodexReviewObserver(
		ctx, spec, observerOwner, codexReviewSnapshotProofPath, CheckCredentialSeparation,
		func(resource CodexReviewIntentResource) error {
			resource.OwnershipToken = observerOwner.Value
			return cfg.Journal.MarkCodexReviewIntentResource(ctx, runID, resource)
		},
		func(rep InspectReport) error {
			return verifySeedRoleAllowlist(rep, spec, volume, codexReviewSnapshotObserverTarget, CheckCredentialSeparation)
		},
	)
	if err != nil {
		return CodexReviewSnapshotObservation{}, err
	}
	return ObserveCodexReviewSnapshot(
		cfg, runID, volume, volumeOwner, observerOwner, volumeReport, report, proof,
	)
}

func (b *CodexReviewLifecycle) observeCodexReviewWorkspace(
	ctx context.Context, cfg CodexReviewConfig, launch CodexReviewLaunchSpec,
	workspaceOwner Label, volumeReport VolumeSummary,
) (CodexReviewWorkspaceObservation, error) {
	observerOwner, err := newOwnershipLabel()
	if err != nil {
		return CodexReviewWorkspaceObservation{}, failf(CheckObservedBaseIdentity,
			"mint Codex review workspace observer ownership: %v", err)
	}
	spec, err := BuildCodexReviewWorkspaceObserverSpec(
		cfg, launch.RunID, launch.WorkspaceVolume, observerOwner,
	)
	if err != nil {
		return CodexReviewWorkspaceObservation{}, err
	}
	if err := cfg.Journal.MarkCodexReviewIntentResource(ctx, launch.RunID,
		CodexReviewIntentResource{Name: spec.Name, OwnershipToken: observerOwner.Value}); err != nil {
		return CodexReviewWorkspaceObservation{}, codexReviewJournalCheckf(
			CheckObservedBaseIdentity, "journal workspace observer intent: %v", err,
		)
	}
	report, proof, err := b.runCodexReviewObserver(
		ctx, spec, observerOwner, codexWorkspaceProofPath, CheckObservedBaseIdentity,
		func(resource CodexReviewIntentResource) error {
			resource.OwnershipToken = observerOwner.Value
			return cfg.Journal.MarkCodexReviewIntentResource(ctx, launch.RunID, resource)
		},
		func(rep InspectReport) error {
			return verifySeedRoleAllowlist(
				rep, spec, launch.WorkspaceVolume, cfg.WorkspaceTarget, CheckObservedBaseIdentity,
			)
		},
	)
	if err != nil {
		return CodexReviewWorkspaceObservation{}, err
	}
	return ObserveCodexReviewWorkspace(
		cfg, launch.RunID, launch.WorkspaceVolume, launch.ExpectedHead,
		workspaceOwner, observerOwner, volumeReport, report, proof,
	)
}

func (b *CodexReviewLifecycle) observeCodexReviewShadow(
	ctx context.Context, cfg CodexReviewConfig, runID, volume string,
	volumeOwner Label, volumeReport VolumeSummary,
) (CodexReviewShadowObservation, error) {
	observerOwner, err := newOwnershipLabel()
	if err != nil {
		return CodexReviewShadowObservation{}, failf(CheckControlPlaneIsolation,
			"mint Codex review shadow observer ownership: %v", err)
	}
	spec, err := BuildCodexReviewShadowObserverSpec(cfg, runID, volume, observerOwner)
	if err != nil {
		return CodexReviewShadowObservation{}, err
	}
	if err := cfg.Journal.MarkCodexReviewIntentResource(ctx, runID,
		CodexReviewIntentResource{Name: spec.Name, OwnershipToken: observerOwner.Value}); err != nil {
		return CodexReviewShadowObservation{}, codexReviewJournalCheckf(
			CheckControlPlaneIsolation, "journal shadow observer intent: %v", err,
		)
	}
	report, proof, err := b.runCodexReviewObserver(
		ctx, spec, observerOwner, codexShadowProofPath, CheckControlPlaneIsolation,
		func(resource CodexReviewIntentResource) error {
			resource.OwnershipToken = observerOwner.Value
			return cfg.Journal.MarkCodexReviewIntentResource(ctx, runID, resource)
		},
		func(rep InspectReport) error { return verifyCodexReviewShadowObserverAllowlist(rep, spec) },
	)
	if err != nil {
		return CodexReviewShadowObservation{}, err
	}
	return ObserveCodexReviewShadow(
		cfg, runID, volume, volumeOwner, observerOwner, volumeReport, report, proof,
	)
}

func (b *CodexReviewLifecycle) runCodexReviewObserver(
	ctx context.Context, spec ContainerSpec, owner Label, proofPath string, check Check,
	mark func(CodexReviewIntentResource) error,
	verify func(InspectReport) error,
) (_ InspectReport, _ []byte, retErr error) {
	claim := objectClaim{attempted: true}
	defer func() {
		if retErr != nil {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.cfg.TeardownTimeout)
			defer cancel()
			if cleanupErr := b.reapUnlistedContainer(cleanupCtx, spec.Name, claim, owner); cleanupErr != nil {
				retErr = errors.Join(retErr, failf(check, "observer cleanup: %v", cleanupErr))
			}
		}
	}()
	if err := b.rt.CreateContainer(ctx, cloneContainerSpec(spec)); err != nil {
		return InspectReport{}, nil, codexReviewOperationalCheckf(check, "create observer: %v", err)
	}
	claim.owned = true
	pre, err := b.rt.Inspect(ctx, spec.Name)
	if err != nil {
		return InspectReport{}, nil, codexReviewOperationalCheckf(check, "inspect observer before start: %v", err)
	}
	claim.fingerprint, err = ownedFingerprint(pre.CreationDate, pre.Labels, pre.LabelsObserved, owner)
	if err != nil {
		return InspectReport{}, nil, failf(check, "observer ownership before start: %v", err)
	}
	if err := mark(CodexReviewIntentResource{Name: spec.Name, Fingerprint: claim.fingerprint}); err != nil {
		return InspectReport{}, nil, codexReviewJournalCheckf(check, "journal observer: %v", err)
	}
	if err := verify(pre); err != nil {
		return InspectReport{}, nil, failf(check, "observer shape before start: %v", err)
	}
	if err := b.rt.StartContainer(ctx, spec.Name); err != nil {
		return InspectReport{}, nil, codexReviewOperationalCheckf(check, "start observer: %v", err)
	}
	if err := b.waitStopped(ctx, spec.Name, claim, owner, b.cfg.SeedTimeout); err != nil {
		return InspectReport{}, nil, codexReviewOperationalCheckf(check, "wait for observer: %v", err)
	}
	report, err := b.rt.Inspect(ctx, spec.Name)
	if err != nil {
		return InspectReport{}, nil, codexReviewOperationalCheckf(check, "inspect stopped observer: %v", err)
	}
	if err := verify(report); err != nil {
		return InspectReport{}, nil, failf(check, "stopped observer shape: %v", err)
	}
	proof, err := b.readCodexReviewProof(ctx, spec.Name, proofPath, check)
	if err != nil {
		return InspectReport{}, nil, err
	}
	if err := b.rt.DeleteContainer(ctx, spec.Name); err != nil {
		return InspectReport{}, nil, codexReviewOperationalCheckf(check, "delete observer: %v", err)
	}
	if err := b.verifyContainerAbsent(ctx, spec.Name, claim, owner, check); err != nil {
		return InspectReport{}, nil, err
	}
	claim.attempted = false
	return report, proof, nil
}

func (b *CodexReviewLifecycle) reapCodexReviewContainer(
	ctx context.Context, id string, claim objectClaim, owner Label,
) error {
	report, err := b.rt.Inspect(ctx, id)
	if err != nil {
		return codexReviewOperationalf("inspect Codex review container %q: %v", id, err)
	}
	if report.ID != id {
		return failf(CheckTeardown, "inspect Codex review container %q returned the wrong identity", id)
	}
	switch classifyEvidence(claim, owner, report.CreationDate, report.Labels, report.LabelsObserved) {
	case evidenceOurs:
		var stopErr error
		if report.State != StateStopped {
			if err := b.rt.StopContainer(ctx, id); err != nil {
				stopErr = codexReviewOperationalf("stop Codex review container %q: %v", id, err)
			}
		}
		if err := b.rt.DeleteContainer(ctx, id); err != nil {
			return errors.Join(stopErr, codexReviewOperationalf("delete Codex review container %q: %v", id, err))
		}
		if stopErr != nil {
			return stopErr
		}
	case evidenceForeign:
		return nil
	case evidenceUnprovable:
		return failf(CheckTeardown, "Codex review container %q ownership is unprovable", id)
	}
	return b.verifyCodexReviewContainerAbsent(ctx, id, claim, owner)
}

func (b *CodexReviewLifecycle) readCodexReviewProof(ctx context.Context, id, proofPath string, check Check) ([]byte, error) {
	dir, err := os.MkdirTemp("", "freeside-codex-review-proof-")
	if err != nil {
		return nil, failf(check, "create observer proof directory: %v", err)
	}
	defer os.RemoveAll(dir) //nolint:errcheck // best-effort cleanup of gate-owned scratch
	tarPath := filepath.Join(dir, "observer.tar")
	if err := b.materializeRootFS(ctx, id, tarPath, check); err != nil {
		return nil, err
	}
	f, err := os.Open(tarPath) //nolint:gosec // gate-owned path under a fresh temp directory
	if err != nil {
		return nil, failf(check, "open observer proof archive: %v", err)
	}
	defer f.Close() //nolint:errcheck // read-only temp handle
	proof, found, err := extractArchiveRegularFile(f, proofPath, maxBaseProofBytes)
	if err != nil {
		return nil, failf(check, "read observer proof: %v", err)
	}
	if !found {
		return nil, failf(check, "observer produced no proof")
	}
	return proof, nil
}

func (b *CodexReviewLifecycle) verifyCodexReviewWorkspaceExclusive(
	ctx context.Context, workspaceVolume, reviewContainer string,
) error {
	containers, err := b.rt.ListContainers(ctx)
	if err != nil {
		return codexReviewOperationalCheckf(
			CheckObservedBaseIdentity, "list Codex review workspace attachments: %v", err,
		)
	}
	seen := make(map[string]struct{}, len(containers))
	for _, container := range containers {
		if container.ID == "" || !cliSafe(container.ID) {
			return failf(CheckObservedBaseIdentity, "workspace attachment has an invalid container identity")
		}
		if _, duplicate := seen[container.ID]; duplicate {
			return failf(CheckObservedBaseIdentity, "workspace attachment identity is duplicated")
		}
		seen[container.ID] = struct{}{}
		report, err := b.rt.Inspect(ctx, container.ID)
		if err != nil {
			return codexReviewOperationalCheckf(
				CheckObservedBaseIdentity, "inspect Codex review workspace attachment %q: %v",
				container.ID, err,
			)
		}
		if report.ID != container.ID || !report.AllowlistFieldsObserved {
			return failf(
				CheckObservedBaseIdentity,
				"Codex review workspace attachment %q has incomplete identity or mount evidence",
				container.ID,
			)
		}
		for _, mount := range report.Mounts {
			if mount.Type == MountVolume && mount.Source == workspaceVolume && container.ID != reviewContainer {
				return failf(
					CheckObservedBaseIdentity,
					"candidate workspace remains attached to container %q", container.ID,
				)
			}
		}
	}
	return nil
}

func validateCodexReviewStartLifetime(
	cfg CodexReviewConfig, launch CodexReviewLaunchSpec,
) error {
	_, authBody, err := readCodexReviewInput(
		cfg.InputRoot, launch.AuthSnapshot, maxCodexAuthSnapshotBytes,
	)
	if err != nil {
		return failf(CheckCredentialSeparation, "Codex review auth snapshot changed before start")
	}
	_, expires, err := codexReviewAgentAuthSnapshot(launch.AuthMode, authBody)
	if err != nil {
		return failf(CheckCredentialSeparation, "Codex review auth snapshot changed before start")
	}
	now := cfg.Now()
	if now.IsZero() || now.Location() != time.UTC ||
		expires != nil && expires.Sub(now) < cfg.AccessTokenLifetimeFloor {
		return failf(CheckCredentialSeparation, "Codex review access token fell below its lifetime floor before start")
	}
	return nil
}

func (b *CodexReviewLifecycle) reobserveCodexReview(
	ctx context.Context, cfg CodexReviewConfig, launch CodexReviewLaunchSpec,
	workspaceOwner, owner Label, shadowName, snapshotName string,
) (CodexReviewShadowObservation, CodexReviewWorkspaceObservation, CodexReviewSnapshotObservation, CodexReviewNetworkObservation, error) {
	var (
		emptyShadow    CodexReviewShadowObservation
		emptyWorkspace CodexReviewWorkspaceObservation
		emptySnapshot  CodexReviewSnapshotObservation
		emptyNetwork   CodexReviewNetworkObservation
	)
	workspaceReport, err := b.rt.InspectVolume(ctx, launch.WorkspaceVolume)
	if err != nil {
		return emptyShadow, emptyWorkspace, emptySnapshot, emptyNetwork,
			codexReviewOperationalCheckf(CheckObservedBaseIdentity, "inspect Codex review workspace: %v", err)
	}
	workspace, err := b.observeCodexReviewWorkspace(ctx, cfg, launch, workspaceOwner, workspaceReport)
	if err != nil {
		return emptyShadow, emptyWorkspace, emptySnapshot, emptyNetwork, err
	}
	shadowReport, err := b.rt.InspectVolume(ctx, shadowName)
	if err != nil {
		return emptyShadow, emptyWorkspace, emptySnapshot, emptyNetwork,
			codexReviewOperationalCheckf(CheckControlPlaneIsolation, "inspect Codex review shadow: %v", err)
	}
	shadow, err := b.observeCodexReviewShadow(ctx, cfg, launch.RunID, shadowName, owner, shadowReport)
	if err != nil {
		return emptyShadow, emptyWorkspace, emptySnapshot, emptyNetwork, err
	}
	snapshotReport, err := b.rt.InspectVolume(ctx, snapshotName)
	if err != nil {
		return emptyShadow, emptyWorkspace, emptySnapshot, emptyNetwork,
			codexReviewOperationalCheckf(CheckCredentialSeparation, "inspect Codex review snapshot: %v", err)
	}
	snapshot, err := b.observeCodexReviewSnapshot(ctx, cfg, launch.RunID, snapshotName, owner, snapshotReport)
	if err != nil {
		return emptyShadow, emptyWorkspace, emptySnapshot, emptyNetwork, err
	}
	networkReport, err := b.rt.InspectNetwork(ctx, codexReviewNetworkName(launch.RunID))
	if err != nil {
		return emptyShadow, emptyWorkspace, emptySnapshot, emptyNetwork,
			codexReviewOperationalCheckf(CheckAgentEgress, "inspect Codex review network: %v", err)
	}
	network, err := ObserveCodexReviewNetwork(cfg, launch.RunID, owner, networkReport)
	return shadow, workspace, snapshot, network, err
}

func (b *CodexReviewLifecycle) reconstructCodexReview(
	ctx context.Context, cfg CodexReviewConfig, launch CodexReviewLaunchSpec,
	req CodexReviewSpec, spec ContainerSpec, binding CodexReviewJournalBinding,
	workspaceOwner, owner Label, shadowName, snapshotName string,
) error {
	shadow, workspace, snapshot, network, err := b.reobserveCodexReview(
		ctx, cfg, launch, workspaceOwner, owner, shadowName, snapshotName,
	)
	if err != nil {
		return err
	}
	if binding.AgentsShadowFingerprint != shadow.fingerprint ||
		binding.AgentsShadowDigest != shadow.digest ||
		binding.WorkspaceFingerprint != workspace.fingerprint ||
		binding.WorkspaceHead != workspace.head ||
		binding.WorkspaceTreeDigest != workspace.treeDigest ||
		binding.WorkspaceAgentsEntry != workspace.agentsEntry ||
		binding.SnapshotFingerprint != snapshot.fingerprint ||
		binding.SnapshotVolume != snapshot.volume ||
		binding.AuthSnapshotDigest != snapshot.authDigest ||
		req.Snapshot.instructionDigest != snapshot.instructionDigest {
		return failf(CheckControlPlaneIsolation, "reconstructed Codex review volumes diverged")
	}
	current := CodexReviewNetworkObservation{
		name: binding.ProviderNetwork, fingerprint: binding.ProviderNetworkFingerprint,
		gateway: binding.ProviderNetworkGateway, subnet: binding.ProviderNetworkSubnet,
		proxyAuthority: binding.ProviderProxyAuthority,
	}
	if err := current.verifyCurrent(network); err != nil {
		return err
	}
	report, err := b.rt.Inspect(ctx, binding.ReviewContainer)
	if err != nil {
		return codexReviewOperationalCheckf(
			CheckControlPlaneIsolation, "inspect reconstructed Codex review container: %v", err,
		)
	}
	if report.ID != binding.ReviewContainer ||
		classifyEvidence(
			objectClaim{owned: true, fingerprint: binding.ReviewContainerFingerprint}, owner,
			report.CreationDate, report.Labels, report.LabelsObserved,
		) != evidenceOurs {
		return failf(CheckControlPlaneIsolation, "reconstructed Codex review container is not owned")
	}
	prepared := binding
	prepared.AgentsShadowPreStartObserverFingerprint = ""
	prepared.WorkspacePreStartObserverFingerprint = ""
	prepared.SnapshotPreStartObserverFingerprint = ""
	if err := validateCodexReviewAgentSpec(cfg, req, spec, prepared); err != nil {
		return err
	}
	return verifyAgentAllowlist(report, spec)
}

func sameCodexReviewBinding(a, b CodexReviewJournalBinding) bool {
	aJSON, aErr := json.Marshal(a)
	bJSON, bErr := json.Marshal(b)
	return aErr == nil && bErr == nil && bytes.Equal(aJSON, bJSON)
}

func cloneCodexReviewBinding(binding CodexReviewJournalBinding) CodexReviewJournalBinding {
	binding.AgentsShadowTargets = slices.Clone(binding.AgentsShadowTargets)
	binding.ProviderEndpoints = slices.Clone(binding.ProviderEndpoints)
	binding.RepositoryInstructionSources = slices.Clone(binding.RepositoryInstructionSources)
	if binding.HostInstructionDigest != nil {
		digest := *binding.HostInstructionDigest
		binding.HostInstructionDigest = &digest
	}
	if binding.AccessTokenExpiresAt != nil {
		expires := *binding.AccessTokenExpiresAt
		binding.AccessTokenExpiresAt = &expires
	}
	return binding
}

func (b *CodexReviewLifecycle) deleteCodexReviewVolume(
	ctx context.Context, name string, claim objectClaim, owner Label,
) error {
	volumes, err := b.rt.ListVolumes(ctx)
	if err != nil {
		return codexReviewOperationalf("list volumes before deleting %q: %v", name, err)
	}
	_, found, err := uniqueVolume(volumes, name)
	if err != nil {
		return failf(CheckTeardown, "%v", err)
	}
	if !found {
		return nil
	}
	report, err := b.rt.InspectVolume(ctx, name)
	if err != nil {
		return codexReviewOperationalf("inspect volume %q: %v", name, err)
	}
	if report.Name != name {
		return failf(CheckTeardown, "inspect volume %q returned the wrong identity", name)
	}
	switch classifyEvidence(claim, owner, report.CreationDate, report.Labels, report.LabelsObserved) {
	case evidenceOurs:
		if err := b.rt.DeleteVolume(ctx, name); err != nil {
			return codexReviewOperationalf("delete volume %q: %v", name, err)
		}
		volumes, err := b.rt.ListVolumes(ctx)
		if err != nil {
			return codexReviewOperationalf("list volumes after deleting %q: %v", name, err)
		}
		remaining, found, err := uniqueVolume(volumes, name)
		if err != nil {
			return failf(CheckTeardown, "%v", err)
		}
		if !found {
			return nil
		}
		switch classifyEvidence(claim, owner, remaining.CreationDate, remaining.Labels, remaining.LabelsObserved) {
		case evidenceOurs:
			return failf(CheckTeardown, "volume %q remains after delete", name)
		case evidenceForeign:
			return nil
		case evidenceUnprovable:
			return failf(CheckTeardown, "volume %q absence unprovable after delete", name)
		}
		return failf(CheckTeardown, "volume %q returned invalid absence evidence", name)
	case evidenceForeign:
		return nil
	case evidenceUnprovable:
		return failf(CheckTeardown, "volume %q ownership unprovable; not deleting", name)
	}
	return failf(CheckTeardown, "volume %q returned invalid ownership evidence", name)
}

func codexReviewShadowVolumeName(runID string) string {
	return codexReviewNames(runID).shadowVolume
}

func codexReviewShadowInitializerName(runID string) string {
	return codexReviewNames(runID).shadowInitializer
}
