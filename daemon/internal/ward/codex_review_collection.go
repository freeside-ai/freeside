package ward

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

const (
	maxCodexReviewResultBytes = 1 << 20
	maxCodexReviewEventsBytes = 8 << 20
)

// CodexReviewCollection is the bounded raw terminal account exported from the
// stopped review container. ReviewSource parses it only after ward has
// authenticated the exact started container.
type CodexReviewCollection struct {
	ExitStatus int
	Result     []byte
	Events     []byte
}

func (b *CodexReviewLifecycle) InspectCodexReview(
	ctx context.Context, cfg CodexReviewConfig, runID string,
) (ContainerState, error) {
	intent, binding, report, err := b.authenticateCodexReviewContainer(ctx, cfg, runID)
	if err != nil {
		return "", err
	}
	if intent.State != CodexReviewIntentStarted || binding.RunID != runID {
		return "", failf(CheckControlPlaneIsolation, "Codex review is not in the started state")
	}
	return report.State, nil
}

func (b *CodexReviewLifecycle) CollectCodexReview(
	ctx context.Context, cfg CodexReviewConfig, runID string,
) (CodexReviewCollection, error) {
	_, _, report, err := b.authenticateCodexReviewContainer(ctx, cfg, runID)
	if err != nil {
		return CodexReviewCollection{}, err
	}
	if report.State != StateStopped {
		return CodexReviewCollection{}, fmt.Errorf("codex review %q is not stopped", runID)
	}
	dir, err := os.MkdirTemp("", "freeside-codex-review-"+runID+"-")
	if err != nil {
		return CodexReviewCollection{}, fmt.Errorf("%w: create collection directory: %w",
			ErrCodexReviewOperational, err)
	}
	defer os.RemoveAll(dir) //nolint:errcheck // bounded review scratch
	archive := filepath.Join(dir, "rootfs.tar")
	if err := b.materializeRootFS(ctx, report.ID, archive, CheckControlPlaneIsolation); err != nil {
		return CodexReviewCollection{}, fmt.Errorf("%w: export review result: %w",
			ErrCodexReviewOperational, err)
	}
	read := func(path string, limit int64) ([]byte, error) {
		file, err := os.Open(archive) //nolint:gosec // gate-owned temp path
		if err != nil {
			return nil, fmt.Errorf("%w: open review archive: %w", ErrCodexReviewOperational, err)
		}
		defer file.Close() //nolint:errcheck // read-only temp file
		body, found, err := extractArchiveRegularFile(file, path, limit)
		if err != nil {
			return nil, fmt.Errorf("%w: read review archive: %w", ErrCodexReviewOperational, err)
		}
		if !found {
			return nil, errors.Join(ErrCodexReviewOutputInvalid,
				failf(CheckControlPlaneIsolation,
					"Codex review omitted %s", filepath.Base(path)))
		}
		return body, nil
	}
	statusBody, err := read(codexReviewStatusPath, 32)
	if err != nil {
		return CodexReviewCollection{}, err
	}
	status, err := strconv.Atoi(strings.TrimSpace(string(statusBody)))
	if err != nil || status < 0 || status > 255 {
		return CodexReviewCollection{}, errors.Join(ErrCodexReviewOutputInvalid,
			failf(CheckControlPlaneIsolation, "Codex review status is invalid"))
	}
	events, err := read(codexReviewEventsPath, maxCodexReviewEventsBytes)
	if err != nil {
		return CodexReviewCollection{}, err
	}
	collection := CodexReviewCollection{ExitStatus: status, Events: events}
	var result []byte
	if status == 0 {
		result, err = read(codexReviewResultPath, maxCodexReviewResultBytes)
		if err != nil {
			return collection, err
		}
		if len(bytes.TrimSpace(result)) == 0 {
			return collection, errors.Join(ErrCodexReviewOutputInvalid,
				failf(CheckControlPlaneIsolation, "Codex review result is empty"))
		}
	}
	collection.Result = result
	return collection, nil
}

func (b *CodexReviewLifecycle) authenticateCodexReviewContainer(
	ctx context.Context, cfg CodexReviewConfig, runID string,
) (CodexReviewLaunchIntent, CodexReviewJournalBinding, InspectReport, error) {
	return b.authenticateCodexReviewContainerWithImage(
		ctx, b.reviewProvider(), cfg, runID, cfg.ApprovedImage,
	)
}

func (b *CodexReviewLifecycle) authenticateCodexReviewContainerForProviderCleanup(
	ctx context.Context, provider reviewProvider, cfg CodexReviewConfig, runID string,
) (CodexReviewLaunchIntent, CodexReviewJournalBinding, InspectReport, error) {
	return b.authenticateCodexReviewContainerWithImage(ctx, provider, cfg, runID, "")
}

func (b *CodexReviewLifecycle) authenticateCodexReviewContainerWithImage(
	ctx context.Context, provider reviewProvider, cfg CodexReviewConfig, runID, approvedImage string,
) (CodexReviewLaunchIntent, CodexReviewJournalBinding, InspectReport, error) {
	intent, err := cfg.Journal.GetCodexReviewIntent(ctx, runID)
	if err != nil {
		if errors.Is(err, ErrCodexReviewIntentNotFound) || errors.Is(err, ErrConformance) {
			return CodexReviewLaunchIntent{}, CodexReviewJournalBinding{}, InspectReport{},
				failf(CheckControlPlaneIsolation, "Codex review launch intent is invalid: %v", err)
		}
		return CodexReviewLaunchIntent{}, CodexReviewJournalBinding{}, InspectReport{},
			codexReviewOperationalf("load Codex review launch intent: %v", err)
	}
	// A rewritten intent must not redirect observation or collection at a
	// container the run id does not derive.
	if intent.validateIdentity(runID) != nil {
		return CodexReviewLaunchIntent{}, CodexReviewJournalBinding{}, InspectReport{},
			failf(CheckControlPlaneIsolation, "Codex review launch intent is invalid")
	}
	binding, err := cfg.Journal.GetCodexReviewBinding(ctx, runID)
	if err != nil {
		if errors.Is(err, ErrCodexReviewBindingNotFound) || errors.Is(err, ErrConformance) {
			return CodexReviewLaunchIntent{}, CodexReviewJournalBinding{}, InspectReport{},
				failf(CheckControlPlaneIsolation, "Codex review durable binding is invalid: %v", err)
		}
		return CodexReviewLaunchIntent{}, CodexReviewJournalBinding{}, InspectReport{},
			codexReviewOperationalf("load Codex review durable binding: %v", err)
	}
	bindingErr := binding.validateShape(provider)
	if approvedImage == "" {
		bindingErr = binding.validateForTeardown(provider)
	}
	if bindingErr != nil || binding.RunID != runID ||
		binding.ReviewContainer != intent.ReviewContainer ||
		binding.ReviewOwnershipToken != intent.OwnershipToken {
		return CodexReviewLaunchIntent{}, CodexReviewJournalBinding{}, InspectReport{},
			failf(CheckControlPlaneIsolation, "Codex review durable binding is invalid")
	}
	report, err := b.rt.Inspect(ctx, binding.ReviewContainer)
	owner := Label{Key: ownershipLabelKey, Value: binding.ReviewOwnershipToken}
	if err != nil {
		return CodexReviewLaunchIntent{}, CodexReviewJournalBinding{}, InspectReport{},
			fmt.Errorf("%w: inspect review container: %w", ErrCodexReviewOperational, err)
	}
	if report.ID != binding.ReviewContainer || classifyEvidence(
		objectClaim{owned: true, fingerprint: binding.ReviewContainerFingerprint}, owner,
		report.CreationDate, report.Labels, report.LabelsObserved,
	) != evidenceOurs {
		return CodexReviewLaunchIntent{}, CodexReviewJournalBinding{}, InspectReport{},
			failf(CheckControlPlaneIsolation, "Codex review container is foreign or unprovable")
	}
	launcherEnv, hasSingleRuntimePath := stripCodexReviewRuntimePath(report.Env)
	if (approvedImage != "" && !sameImage(report.ImageReference, approvedImage)) ||
		!hasSingleRuntimePath ||
		digestStrings(report.Command) != binding.CommandDigest ||
		digestEnvironment(launcherEnv) != binding.LauncherEnvironmentDigest {
		return CodexReviewLaunchIntent{}, CodexReviewJournalBinding{}, InspectReport{},
			failf(CheckControlPlaneIsolation, "Codex review realized command changed after start")
	}
	return intent, binding, report, nil
}

func stripCodexReviewRuntimePath(environment []string) ([]string, bool) {
	pathIndex := slices.Index(environment, fixedContainerPathEnv)
	if pathIndex < 0 || slices.Contains(environment[pathIndex+1:], fixedContainerPathEnv) {
		return environment, false
	}
	launcherEnvironment := slices.Clone(environment)
	return append(launcherEnvironment[:pathIndex], launcherEnvironment[pathIndex+1:]...), true
}

// CleanupCodexReview reaps the authenticated terminal topology only after the
// source has durably stored its collected account.
func (b *CodexReviewLifecycle) CleanupCodexReview(
	ctx context.Context, cfg CodexReviewConfig, runID string,
) error {
	return b.cleanupCodexReview(ctx, b.reviewProvider(), cfg, runID, false)
}

// AbortCodexReview closes a started review whose daemon-owned CONNECT proxy
// was lost. The review is disposable and read-only; its durable source
// outcome records the transient loss before this destructive path runs.
func (b *CodexReviewLifecycle) AbortCodexReview(
	ctx context.Context, cfg CodexReviewConfig, runID string,
) error {
	return b.cleanupCodexReview(ctx, b.reviewProvider(), cfg, runID, true)
}

func (b *CodexReviewLifecycle) cleanupCodexReview(
	ctx context.Context, provider reviewProvider, cfg CodexReviewConfig, runID string, abort bool,
) error {
	intent, err := cfg.Journal.GetCodexReviewIntent(ctx, runID)
	if err != nil {
		if errors.Is(err, ErrCodexReviewIntentNotFound) {
			return failf(CheckTeardown, "Codex review launch intent is missing")
		}
		if errors.Is(err, ErrConformance) {
			return failf(CheckControlPlaneIsolation, "Codex review launch intent is invalid: %v", err)
		}
		return codexReviewOperationalf("load Codex review launch intent: %v", err)
	}
	// Re-derive every teardown target from the run id before trusting the
	// stored rows: a rewritten intent or binding must not be able to redirect
	// deletion at a sibling invocation's resources or fake convergence by
	// naming resources that never existed.
	names, namesErr := intent.validatedResourceNames(runID)
	if namesErr != nil {
		return failf(CheckControlPlaneIsolation, "Codex review launch intent is invalid")
	}
	if err := b.authorizeRuntime(ctx, codexReviewRuntimeResourceNames(runID, names)); err != nil {
		return codexReviewOperationalf("authorize Codex review cleanup resources: %v", err)
	}
	binding, err := cfg.Journal.GetCodexReviewBinding(ctx, runID)
	if err != nil {
		if errors.Is(err, ErrCodexReviewBindingNotFound) {
			return failf(CheckControlPlaneIsolation, "Codex review durable binding is missing")
		}
		if errors.Is(err, ErrConformance) {
			return failf(CheckControlPlaneIsolation, "Codex review durable binding is invalid: %v", err)
		}
		return codexReviewOperationalf("load Codex review durable binding: %v", err)
	}
	if binding.validateForTeardown(provider) != nil || binding.RunID != runID ||
		binding.ReviewContainer != intent.ReviewContainer ||
		binding.ReviewOwnershipToken != intent.OwnershipToken ||
		binding.WorkspaceSourceRunID != runID ||
		binding.WorkspaceVolume != namesFor(runID).Workspace {
		return failf(CheckControlPlaneIsolation, "Codex review durable binding is invalid")
	}
	owner := Label{Key: ownershipLabelKey, Value: intent.OwnershipToken}
	containerClaim := objectClaim{attempted: true, owned: true, fingerprint: binding.ReviewContainerFingerprint}
	containers, err := b.rt.ListContainers(ctx)
	if err != nil {
		return codexReviewOperationalf("list Codex review containers: %v", err)
	}
	container, found, err := uniqueContainer(containers, intent.ReviewContainer)
	if err != nil {
		return failf(CheckTeardown, "%v", err)
	}
	if found {
		_, _, report, authErr := b.authenticateCodexReviewContainerForProviderCleanup(
			ctx, provider, cfg, runID,
		)
		if errors.Is(authErr, ErrCodexReviewOperational) {
			return authErr
		}
		if authErr != nil || report.ID != container.ID {
			return failf(CheckTeardown, "Codex review container is foreign or unprovable")
		}
		if report.State != StateStopped && !abort {
			return failf(CheckTeardown, "Codex review container is still running")
		}
		if err := b.reapCodexReviewContainer(
			ctx, intent.ReviewContainer, containerClaim, owner,
		); err != nil {
			if errors.Is(err, ErrCodexReviewOperational) {
				return err
			}
			return failf(CheckTeardown, "reap Codex review container: %v", err)
		}
	}
	if err := b.verifyCodexReviewContainerAbsent(ctx, intent.ReviewContainer, containerClaim, owner); err != nil {
		return err
	}
	lease, _, err := cfg.VolumeLifecycleLeaser.RecoverCodexReviewVolumeLease(
		ctx, owner.Value, codexReviewLeaseVolumes(binding.WorkspaceVolume, intent.ShadowVolume, intent.SnapshotVolume),
	)
	if err != nil {
		// The leaser's authenticated refusals are contradictions, not runtime
		// I/O: a foreign or unprovable owner, or a lease still transferred
		// after the container was verified absent, means the durable topology
		// no longer proves ours, and a transient wrapper would retry it
		// silently forever instead of failing loudly.
		if errors.Is(err, ErrCodexReviewVolumeLeaseForeignOwner) ||
			errors.Is(err, ErrCodexReviewVolumeLeaseTransferred) {
			return failf(CheckControlPlaneIsolation,
				"recover terminal Codex review volume lease: %v", err)
		}
		return codexReviewOperationalf("recover terminal Codex review volume lease: %v", err)
	}
	released := false
	defer func() {
		if !released {
			_ = lease.ReleaseCodexReviewVolumeLease(context.WithoutCancel(ctx))
		}
	}()
	claims := make(map[string]objectClaim, len(intent.Resources))
	for _, resource := range intent.Resources {
		claims[resource.Name] = objectClaim{attempted: true, owned: true, fingerprint: resource.Fingerprint}
	}
	if err := b.deleteCodexReviewVolume(ctx, intent.ShadowVolume, claims[intent.ShadowVolume], owner); err != nil {
		return err
	}
	// The credential snapshot volume is ward-owned and part of the terminal
	// lease. A legacy (pre-#591) intent carries none, so this is skipped there.
	if intent.SnapshotVolume != "" {
		if err := b.deleteCodexReviewVolume(ctx, intent.SnapshotVolume, claims[intent.SnapshotVolume], owner); err != nil {
			return err
		}
	}
	workspaceBinding, err := cfg.Journal.GetCodexReviewWorkspaceBinding(
		ctx, binding.WorkspaceSourceRunID,
	)
	if err != nil {
		if errors.Is(err, ErrCodexReviewWorkspaceNotFound) {
			return failf(CheckTeardown, "Codex review workspace ownership is missing")
		}
		if errors.Is(err, ErrConformance) {
			return failf(CheckTeardown, "Codex review workspace ownership is invalid: %v", err)
		}
		return codexReviewOperationalf("load Codex review workspace ownership: %v", err)
	}
	if workspaceBinding.Volume != binding.WorkspaceVolume {
		return failf(CheckTeardown, "load Codex review workspace ownership")
	}
	workspaceOwner := Label{Key: ownershipLabelKey, Value: workspaceBinding.OwnershipToken}
	if err := b.deleteCodexReviewVolume(ctx, binding.WorkspaceVolume,
		objectClaim{attempted: true, owned: true, fingerprint: workspaceBinding.CreationFingerprint},
		workspaceOwner,
	); err != nil {
		return err
	}
	if err := b.teardownCodexReviewNetwork(ctx, intent.Network, claims[intent.Network], owner); err != nil {
		return err
	}
	if err := lease.ReleaseCodexReviewVolumeLease(ctx); err != nil {
		return codexReviewOperationalf("release terminal Codex review volume lease: %v", err)
	}
	released = true
	if err := cfg.Journal.CloseCodexReviewIntent(ctx, runID); err != nil {
		return codexReviewJournalCheckf(
			CheckControlPlaneIsolation, "close Codex review launch intent: %v", err)
	}
	return nil
}

func codexReviewOperationalf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrCodexReviewOperational, fmt.Sprintf(format, args...))
}

func codexReviewOperationalCheckf(check Check, format string, args ...any) error {
	return fmt.Errorf("%w: %w", ErrCodexReviewOperational, failf(check, format, args...))
}

func codexReviewJournalCheckf(check Check, format string, err error) error {
	if errors.Is(err, ErrConformance) {
		return failf(check, format, err)
	}
	return codexReviewOperationalCheckf(check, format, err)
}

func (b *CodexReviewLifecycle) verifyCodexReviewContainerAbsent(
	ctx context.Context, id string, claim objectClaim, owner Label,
) error {
	containers, err := b.rt.ListContainers(ctx)
	if err != nil {
		return codexReviewOperationalf("list containers to verify %q absent: %v", id, err)
	}
	candidate, found, err := uniqueContainer(containers, id)
	if err != nil {
		return failf(CheckTeardown, "verify %q absent: %v", id, err)
	}
	if !found {
		return nil
	}
	evidence := classifyEvidence(claim, owner, candidate.CreationDate, candidate.Labels, candidate.LabelsObserved)
	if evidence == evidenceUnprovable && underObserved(candidate.CreationDate, candidate.LabelsObserved, claim) {
		report, inspectErr := b.rt.Inspect(ctx, candidate.ID)
		if inspectErr != nil {
			return codexReviewOperationalf("inspect container %q ownership: %v", candidate.ID, inspectErr)
		}
		if report.ID != candidate.ID {
			return failf(CheckTeardown, "inspect container %q returned the wrong identity", candidate.ID)
		}
		evidence = classifyEvidence(claim, owner, report.CreationDate, report.Labels, report.LabelsObserved)
	}
	if evidence == evidenceForeign {
		return nil
	}
	return failf(CheckTeardown, "container %q survived cleanup or has unprovable ownership", id)
}

func (b *CodexReviewLifecycle) teardownCodexReviewNetwork(
	ctx context.Context, name string, claim objectClaim, owner Label,
) error {
	networks, err := b.rt.ListNetworks(ctx)
	if err != nil {
		return codexReviewOperationalf("list Codex review networks: %v", err)
	}
	candidate, found, err := uniqueNetwork(networks, name)
	if err != nil {
		return failf(CheckTeardown, "%v", err)
	}
	if !found {
		return nil
	}
	switch classifyEvidence(claim, owner, candidate.CreationDate, candidate.Labels, candidate.LabelsObserved) {
	case evidenceOurs:
		if err := b.rt.DeleteNetwork(ctx, name); err != nil {
			return codexReviewOperationalf("delete Codex review network %q: %v", name, err)
		}
	case evidenceForeign:
		return nil
	case evidenceUnprovable:
		return failf(CheckTeardown, "Codex review network %q ownership is unprovable", name)
	}
	networks, err = b.rt.ListNetworks(ctx)
	if err != nil {
		return codexReviewOperationalf("re-list Codex review networks: %v", err)
	}
	candidate, found, err = uniqueNetwork(networks, name)
	if err != nil {
		return failf(CheckTeardown, "%v", err)
	}
	if !found || classifyEvidence(claim, owner, candidate.CreationDate, candidate.Labels, candidate.LabelsObserved) == evidenceForeign {
		return nil
	}
	return failf(CheckTeardown, "Codex review network %q survived cleanup or has unprovable ownership", name)
}
