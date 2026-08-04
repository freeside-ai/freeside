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
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

const codexReviewShadowVolumeSizeMB int64 = 2

// CodexReviewJournal is the durable seam for a prepared review launch. Put
// must be durable before it returns. Get must decode a fresh copy rather than
// return a caller-held value; CodexReview treats that copy as an untrusted
// claim and reconstructs its runtime evidence before start.
type CodexReviewJournal interface {
	PutCodexReviewRequest(context.Context, string, exec.ReviewRequest) error
	GetCodexReviewRequest(context.Context, string) (exec.ReviewRequest, error)
	PutCodexReviewOutcome(context.Context, string, CodexReviewSourceOutcome) error
	GetCodexReviewOutcome(context.Context, string) (CodexReviewSourceOutcome, bool, error)
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
	RunID           string                      `json:"run_id"`
	SpecDigest      string                      `json:"spec_digest"`
	OwnershipToken  string                      `json:"ownership_token"`
	ShadowVolume    string                      `json:"shadow_volume"`
	Network         string                      `json:"network"`
	ReviewContainer string                      `json:"review_container"`
	Resources       []CodexReviewIntentResource `json:"resources"`
	State           CodexReviewIntentState      `json:"state"`
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
func (b *Backend) CodexReview(
	ctx context.Context,
	cfg CodexReviewConfig,
	launch CodexReviewLaunchSpec,
) (_ *CodexReviewLaunch, retErr error) {
	if b == nil || !b.initialized {
		return nil, fmt.Errorf("%w: backend is not initialized", ErrInvalidConfig)
	}
	if err := validateCodexReviewLaunch(cfg, launch); err != nil {
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
	owner, err := newOwnershipLabel()
	if err != nil {
		return nil, fmt.Errorf("mint Codex review ownership: %w", err)
	}
	shadowName := codexReviewShadowVolumeName(launch.RunID)
	intent := CodexReviewLaunchIntent{
		RunID: launch.RunID, SpecDigest: intentDigest, OwnershipToken: owner.Value,
		ShadowVolume: shadowName, Network: codexReviewNetworkName(launch.RunID),
		ReviewContainer: codexReviewContainerName(launch.RunID),
		Resources: []CodexReviewIntentResource{
			{Name: codexReviewWorkspaceObserverName(launch.RunID)},
			{Name: codexReviewShadowInitializerName(launch.RunID), OwnershipToken: owner.Value},
			{Name: codexReviewShadowObserverName(launch.RunID)},
			{Name: codexReviewContainerName(launch.RunID), OwnershipToken: owner.Value},
			{Name: shadowName, OwnershipToken: owner.Value},
			{Name: codexReviewNetworkName(launch.RunID), OwnershipToken: owner.Value},
		}, State: CodexReviewIntentPreparing,
	}
	if err := cfg.Journal.BeginCodexReviewIntent(ctx, intent); err != nil {
		return nil, codexReviewJournalCheckf(
			CheckControlPlaneIsolation, "persist Codex review launch intent: %v", err,
		)
	}
	volumeLease, err := cfg.VolumeLifecycleLeaser.AcquireCodexReviewVolumeLease(
		ctx, owner.Value, []string{launch.WorkspaceVolume, shadowName},
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
		AgentsShadow: shadow,
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

	freshShadow, freshWorkspace, currentNetwork, err := b.reobserveCodexReview(
		ctx, cfg, launch, workspaceOwner, owner, shadowName,
	)
	if err != nil {
		return nil, err
	}
	binding, err = verifyCodexReviewAllowlistShape(
		cfg, req, binding, freshShadow, freshWorkspace, currentNetwork, containerReport, spec,
	)
	if err != nil {
		return nil, err
	}
	if err := cfg.Journal.PutCodexReviewBinding(ctx, cloneCodexReviewBinding(binding)); err != nil {
		return nil, codexReviewJournalCheckf(
			CheckControlPlaneIsolation, "persist Codex review binding: %v", err,
		)
	}
	if err := cfg.Journal.MarkCodexReviewIntentPrepared(ctx, launch.RunID); err != nil {
		return nil, codexReviewJournalCheckf(
			CheckControlPlaneIsolation, "mark Codex review launch prepared: %v", err,
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
		ctx, cfg, launch, req, spec, persisted, workspaceOwner, owner, shadowName,
	); err != nil {
		return nil, err
	}
	if err := b.verifyCodexReviewWorkspaceExclusive(ctx, launch.WorkspaceVolume, spec.Name); err != nil {
		return nil, err
	}
	if err := validateCodexReviewStartLifetime(cfg, launch); err != nil {
		return nil, err
	}
	if err := cfg.Journal.MarkCodexReviewIntentStarting(ctx, launch.RunID); err != nil {
		return nil, codexReviewJournalCheckf(
			CheckControlPlaneIsolation, "mark Codex review launch starting: %v", err,
		)
	}
	if err := volumeLease.StartCodexReviewContainer(ctx, spec.Name); err != nil {
		// Starting is durable before the effect, and the error can describe
		// either no effect or a successful atomic lease transfer. A
		// fresh-context review is disposable, so both outcomes resolve the same
		// way: close the proxy, reap whatever this launch durably owns, and
		// leave a clean retry. A failed recovery keeps the durable intent and
		// lease visible; nothing live survives this error return either way.
		leaseReleasable = false
		started = true
		startErr := codexReviewOperationalCheckf(
			CheckControlPlaneIsolation, "start Codex review container: %v", err,
		)
		proxyErr := proxy.Close()
		proxy = nil
		if recoveryErr := b.RecoverCodexReview(ctx, cfg, launch); recoveryErr != nil {
			return nil, errors.Join(startErr, recoveryErr, proxyErr)
		}
		return nil, errors.Join(startErr, proxyErr)
	}
	// A successful Start already transferred the lease. From here recovery owns
	// every durable object: if the process dies before the handoff record, a
	// later recovery sees `starting` and reaps the review for a fresh retry.
	// `started` is recorded only by the invocation that observed the successful
	// start; #427 never owns a review whose start ward did not witness.
	leaseTransferred = true
	started = true
	if err := cfg.Journal.MarkCodexReviewIntentStarted(ctx, launch.RunID); err != nil {
		// Without the durable handoff record #427 can never own this review, so
		// destroy it now rather than strand a running credential-bearing
		// container behind an error return.
		markErr := codexReviewJournalCheckf(
			CheckControlPlaneIsolation, "mark Codex review ReviewSource handoff: %v", err,
		)
		proxyErr := proxy.Close()
		proxy = nil
		if recoveryErr := b.RecoverCodexReview(ctx, cfg, launch); recoveryErr != nil {
			return nil, errors.Join(markErr, recoveryErr, proxyErr)
		}
		return nil, errors.Join(markErr, proxyErr)
	}
	return &CodexReviewLaunch{Binding: persisted, proxy: proxy}, nil
}

func validateCodexReviewLaunch(cfg CodexReviewConfig, launch CodexReviewLaunchSpec) error {
	switch {
	case cfg.Journal == nil:
		return fmt.Errorf("%w: Journal is required", ErrInvalidCodexReviewSpec)
	case cfg.VolumeLifecycleLeaser == nil:
		return fmt.Errorf("%w: VolumeLifecycleLeaser is required", ErrInvalidCodexReviewSpec)
	case !runIDPattern.MatchString(launch.RunID):
		return fmt.Errorf("%w: RunID is invalid", ErrInvalidCodexReviewSpec)
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
	authPath, authBody, err := readCodexReviewInput(
		cfg.InputRoot, launch.AuthSnapshot, maxCodexAuthSnapshotBytes,
	)
	if err != nil {
		return fmt.Errorf("%w: auth snapshot: %w", ErrInvalidCodexReviewSpec, err)
	}
	now := cfg.Now()
	if now.IsZero() || now.Location() != time.UTC {
		return fmt.Errorf("%w: credential clock is invalid", ErrInvalidCodexReviewSpec)
	}
	expires, err := inspectCodexAuthSnapshot(launch.AuthMode, authBody)
	if err != nil || expires != nil && expires.Sub(now) < cfg.AccessTokenLifetimeFloor {
		return fmt.Errorf("%w: auth snapshot is invalid or below its lifetime floor", ErrInvalidCodexReviewSpec)
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

// codexReviewIntentDigest deliberately commits only non-secret launch shape.
// The auth snapshot, prompt, and instruction body stay outside durable state;
// their live content is re-read and re-gated before the handoff boundary.
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
func (b *Backend) RecoverCodexReview(
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

func (b *Backend) recoverCodexReviewIntent(
	ctx context.Context, cfg CodexReviewConfig, intent CodexReviewLaunchIntent, discardWorkspace bool,
) error {
	if intent.validateIdentity(intent.RunID) != nil || intent.State == CodexReviewIntentStarted {
		return failf(CheckControlPlaneIsolation, "Codex review recovery intent is invalid")
	}
	if intent.State == CodexReviewIntentClosed {
		return nil
	}
	launch := CodexReviewLaunchSpec{
		RunID: intent.RunID, WorkspaceVolume: namesFor(intent.RunID).Workspace,
	}
	owner := Label{Key: ownershipLabelKey, Value: intent.OwnershipToken}
	lease, transfer, err := cfg.VolumeLifecycleLeaser.RecoverCodexReviewVolumeLease(
		ctx, owner.Value, []string{launch.WorkspaceVolume, intent.ShadowVolume},
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
			ctx, owner.Value, []string{launch.WorkspaceVolume, intent.ShadowVolume},
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
	claims := make(map[string]objectClaim, len(intent.Resources))
	owners := make(map[string]Label, len(intent.Resources))
	for _, resource := range intent.Resources {
		claims[resource.Name] = objectClaim{attempted: true, owned: true, fingerprint: resource.Fingerprint}
		token := resource.OwnershipToken
		if token == "" {
			token = intent.OwnershipToken
		}
		owners[resource.Name] = Label{Key: ownershipLabelKey, Value: token}
	}
	var cleanupErrs []error
	containers, listErr := b.rt.ListContainers(ctx)
	if listErr != nil {
		return codexReviewOperationalCheckf(
			CheckControlPlaneIsolation, "list Codex review recovery containers: %v", listErr)
	}
	for _, name := range []string{codexReviewWorkspaceObserverName(launch.RunID), codexReviewShadowInitializerName(launch.RunID), codexReviewShadowObserverName(launch.RunID), intent.ReviewContainer} {
		if !slices.ContainsFunc(containers, func(c ContainerSummary) bool { return c.ID == name }) {
			continue
		}
		report, inspectErr := b.rt.Inspect(ctx, name)
		if inspectErr != nil {
			cleanupErrs = append(cleanupErrs, codexReviewOperationalf(
				"inspect Codex review recovery container %q: %v", name, inspectErr))
			if name == intent.ReviewContainer {
				leaseReleasable = false
			}
			continue
		}
		if report.ID != name || classifyEvidence(
			claims[name], owners[name], report.CreationDate, report.Labels, report.LabelsObserved,
		) != evidenceOurs {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("recovery container %q is foreign or unprovable", name))
			if name == intent.ReviewContainer {
				leaseReleasable = false
			}
			continue
		}
		if err := b.reapCodexReviewContainer(ctx, name, claims[name], owners[name]); err != nil {
			if name == intent.ReviewContainer {
				leaseReleasable = false
			}
			cleanupErrs = append(cleanupErrs, err)
		}
	}
	volumes, listErr := b.rt.ListVolumes(ctx)
	if listErr != nil {
		cleanupErrs = append(cleanupErrs,
			codexReviewOperationalf("list recovery volumes: %v", listErr))
	} else if slices.ContainsFunc(volumes, func(v VolumeSummary) bool { return v.Name == intent.ShadowVolume }) {
		report, inspectErr := b.rt.InspectVolume(ctx, intent.ShadowVolume)
		if inspectErr != nil {
			cleanupErrs = append(cleanupErrs,
				codexReviewOperationalf("inspect recovery shadow volume: %v", inspectErr))
		} else if report.Name != intent.ShadowVolume || classifyEvidence(
			claims[intent.ShadowVolume], owners[intent.ShadowVolume], report.CreationDate, report.Labels, report.LabelsObserved,
		) != evidenceOurs {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("recovery shadow volume is foreign or unprovable"))
		} else if err := b.deleteCodexReviewVolume(ctx, intent.ShadowVolume, claims[intent.ShadowVolume], owners[intent.ShadowVolume]); err != nil {
			cleanupErrs = append(cleanupErrs, err)
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

// CodexReviewRecovery converges only durable review topology left by a prior
// daemon. It carries no launch credentials, prompt, instructions, or reviewer
// configuration. The optional input root only lets it remove an already
// materialized daemon-owned snapshot, never read or launch from one.
type CodexReviewRecovery struct {
	backend   *Backend
	cfg       CodexReviewConfig
	inputRoot string
}

func NewCodexReviewRecovery(
	backend *Backend, journal CodexReviewJournal, leaser CodexReviewVolumeLifecycleLeaser, inputRoot string,
) (*CodexReviewRecovery, error) {
	if backend == nil || journal == nil || leaser == nil || inputRoot != "" && !cleanAbs(inputRoot) {
		return nil, errors.New("nil Codex review recovery dependency")
	}
	return &CodexReviewRecovery{backend: backend, cfg: CodexReviewConfig{
		Journal: journal, VolumeLifecycleLeaser: leaser,
	}, inputRoot: inputRoot}, nil
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
	for _, id := range workspaceIDs {
		if _, exists := intentIDs[id]; exists {
			continue
		}
		if err := r.removeInstructionSnapshot(id); err != nil {
			recoveryErrs = append(recoveryErrs,
				fmt.Errorf("recover orphaned Codex review snapshot %q: %w", id, err))
			continue
		}
		if err := r.backend.cleanupOrphanedCodexReviewWorkspace(ctx, r.cfg.Journal, id); err != nil {
			recoveryErrs = append(recoveryErrs,
				fmt.Errorf("recover orphaned Codex review workspace %q: %w", id, err))
		}
	}
	return errors.Join(recoveryErrs...)
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
		cleanupErr := r.backend.recoverCodexReviewIntent(ctx, r.cfg, intent, true)
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
	cleanupErr := r.backend.AbortCodexReview(ctx, r.cfg, id)
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
	if r.inputRoot == "" {
		return nil
	}
	if err := removeCodexReviewInstructionSnapshot(r.inputRoot, id); err != nil {
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
func (b *Backend) reapCodexReviewTransferredAttachment(
	ctx context.Context,
	launch CodexReviewLaunchSpec,
	intent CodexReviewLaunchIntent,
	owner Label,
	transfer CodexReviewVolumeLeaseTransfer,
) error {
	if transfer.Holder != owner.Value ||
		!slices.Equal(transfer.Volumes, []string{launch.WorkspaceVolume, intent.ShadowVolume}) ||
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
	if intent.RunID != runID ||
		!codexReviewOwnershipLabelValid(Label{Key: ownershipLabelKey, Value: intent.OwnershipToken}) ||
		intent.ShadowVolume != codexReviewShadowVolumeName(runID) ||
		intent.Network != codexReviewNetworkName(runID) ||
		intent.ReviewContainer != codexReviewContainerName(runID) {
		return errors.New("launch identity is invalid")
	}
	switch intent.State {
	case CodexReviewIntentPreparing, CodexReviewIntentPrepared, CodexReviewIntentStarting, CodexReviewIntentStarted, CodexReviewIntentClosed:
	default:
		return errors.New("launch state is invalid")
	}
	expected := map[string]struct{}{
		codexReviewWorkspaceObserverName(runID): {},
		codexReviewShadowInitializerName(runID): {},
		codexReviewShadowObserverName(runID):    {},
		intent.ReviewContainer:                  {},
		intent.ShadowVolume:                     {},
		intent.Network:                          {},
	}
	if len(intent.Resources) != len(expected) {
		return errors.New("launch resources are incomplete")
	}
	for _, resource := range intent.Resources {
		if _, ok := expected[resource.Name]; !ok {
			return errors.New("launch resource is invalid")
		}
		delete(expected, resource.Name)
		if resource.OwnershipToken != "" && !ownershipTokenPattern.MatchString(resource.OwnershipToken) {
			return errors.New("launch resource owner is invalid")
		}
		if resource.Name == intent.ShadowVolume || resource.Name == intent.Network ||
			resource.Name == intent.ReviewContainer || resource.Name == codexReviewShadowInitializerName(runID) {
			if resource.OwnershipToken != intent.OwnershipToken {
				return errors.New("fixed launch resource owner diverged")
			}
		}
	}
	if len(expected) != 0 {
		return errors.New("launch resource is missing")
	}
	return nil
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

func (b *Backend) initializeCodexReviewShadow(
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

func (b *Backend) observeCodexReviewWorkspace(
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

func (b *Backend) observeCodexReviewShadow(
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

func (b *Backend) runCodexReviewObserver(
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

func (b *Backend) reapCodexReviewContainer(
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

func (b *Backend) readCodexReviewProof(ctx context.Context, id, proofPath string, check Check) ([]byte, error) {
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

func (b *Backend) verifyCodexReviewWorkspaceExclusive(
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
	expires, err := inspectCodexAuthSnapshot(launch.AuthMode, authBody)
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

func (b *Backend) reobserveCodexReview(
	ctx context.Context, cfg CodexReviewConfig, launch CodexReviewLaunchSpec,
	workspaceOwner, owner Label, shadowName string,
) (CodexReviewShadowObservation, CodexReviewWorkspaceObservation, CodexReviewNetworkObservation, error) {
	workspaceReport, err := b.rt.InspectVolume(ctx, launch.WorkspaceVolume)
	if err != nil {
		return CodexReviewShadowObservation{}, CodexReviewWorkspaceObservation{}, CodexReviewNetworkObservation{},
			codexReviewOperationalCheckf(CheckObservedBaseIdentity, "inspect Codex review workspace: %v", err)
	}
	workspace, err := b.observeCodexReviewWorkspace(ctx, cfg, launch, workspaceOwner, workspaceReport)
	if err != nil {
		return CodexReviewShadowObservation{}, CodexReviewWorkspaceObservation{}, CodexReviewNetworkObservation{}, err
	}
	shadowReport, err := b.rt.InspectVolume(ctx, shadowName)
	if err != nil {
		return CodexReviewShadowObservation{}, CodexReviewWorkspaceObservation{}, CodexReviewNetworkObservation{},
			codexReviewOperationalCheckf(CheckControlPlaneIsolation, "inspect Codex review shadow: %v", err)
	}
	shadow, err := b.observeCodexReviewShadow(ctx, cfg, launch.RunID, shadowName, owner, shadowReport)
	if err != nil {
		return CodexReviewShadowObservation{}, CodexReviewWorkspaceObservation{}, CodexReviewNetworkObservation{}, err
	}
	networkReport, err := b.rt.InspectNetwork(ctx, codexReviewNetworkName(launch.RunID))
	if err != nil {
		return CodexReviewShadowObservation{}, CodexReviewWorkspaceObservation{}, CodexReviewNetworkObservation{},
			codexReviewOperationalCheckf(CheckAgentEgress, "inspect Codex review network: %v", err)
	}
	network, err := ObserveCodexReviewNetwork(cfg, launch.RunID, owner, networkReport)
	return shadow, workspace, network, err
}

func (b *Backend) reconstructCodexReview(
	ctx context.Context, cfg CodexReviewConfig, launch CodexReviewLaunchSpec,
	req CodexReviewSpec, spec ContainerSpec, binding CodexReviewJournalBinding,
	workspaceOwner, owner Label, shadowName string,
) error {
	shadow, workspace, network, err := b.reobserveCodexReview(
		ctx, cfg, launch, workspaceOwner, owner, shadowName,
	)
	if err != nil {
		return err
	}
	if binding.AgentsShadowFingerprint != shadow.fingerprint ||
		binding.AgentsShadowDigest != shadow.digest ||
		binding.WorkspaceFingerprint != workspace.fingerprint ||
		binding.WorkspaceHead != workspace.head ||
		binding.WorkspaceTreeDigest != workspace.treeDigest {
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

func (b *Backend) deleteCodexReviewVolume(
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
	return "freeside-review-" + runID + "-agents"
}

func codexReviewShadowInitializerName(runID string) string {
	return "freeside-review-" + runID + "-agents-init"
}
