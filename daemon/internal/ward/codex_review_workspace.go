package ward

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// PrepareCodexReviewWorkspace snapshots one exact candidate checkout into a
// ward-owned volume, proves its HEAD and tree from a separate read-only VM,
// and durably binds the resulting runtime identity before returning it.
func (b *CodexReviewLifecycle) PrepareCodexReviewWorkspace(
	ctx context.Context,
	journal CodexReviewJournal,
	runID, sourceDir string,
	candidate domain.BaseRevision,
	workspaceSizeMB int64,
) (_ CodexReviewWorkspaceBinding, retErr error) {
	if !b.valid() {
		return CodexReviewWorkspaceBinding{}, fmt.Errorf("%w: Codex review lifecycle is not initialized", ErrInvalidConfig)
	}
	if journal == nil || !runIDPattern.MatchString(runID) || workspaceSizeMB <= 0 {
		return CodexReviewWorkspaceBinding{}, fmt.Errorf("%w: invalid review workspace request", ErrInvalidCodexReviewSpec)
	}
	seed := WorkspaceSeed{Mode: SeedBaseCheckout, SourceDir: sourceDir, Base: candidate}
	if err := seed.validate(); err != nil {
		return CodexReviewWorkspaceBinding{}, err
	}
	names := namesFor(runID)
	var binding CodexReviewWorkspaceBinding
	binding, err := journal.GetCodexReviewWorkspaceBinding(ctx, runID)
	switch {
	case err == nil:
		if binding.SourceRunID != runID || binding.Volume != names.Workspace ||
			binding.OwnershipToken == "" {
			return CodexReviewWorkspaceBinding{}, failf(CheckWorkspaceSeeding,
				"stored Codex review workspace preparation is invalid")
		}
		if binding.CreationFingerprint != "" {
			return binding, nil
		}
	case errors.Is(err, ErrCodexReviewWorkspaceNotFound):
		owner, ownerErr := newOwnershipLabel()
		if ownerErr != nil {
			return CodexReviewWorkspaceBinding{}, fmt.Errorf("mint review workspace ownership: %w", ownerErr)
		}
		binding = CodexReviewWorkspaceBinding{
			SourceRunID: runID, Volume: names.Workspace, OwnershipToken: owner.Value,
		}
		if err := journal.PutCodexReviewWorkspaceBinding(ctx, binding); err != nil {
			return CodexReviewWorkspaceBinding{}, codexReviewJournalCheckf(
				CheckWorkspaceSeeding, "persist workspace preparation: %v", err)
		}
	default:
		return CodexReviewWorkspaceBinding{}, err
	}
	owner := Label{Key: ownershipLabelKey, Value: binding.OwnershipToken}
	// A crash may leave a partially seeded volume after the pending binding
	// committed. Its unpredictable owner makes deletion safe; reconstruction
	// always starts from a new empty volume rather than adopting partial data.
	if err := b.deleteCodexReviewVolume(ctx, names.Workspace,
		objectClaim{attempted: true, owned: true}, owner); err != nil {
		return CodexReviewWorkspaceBinding{}, err
	}
	hs := HandoffSpec{RunID: runID, WorkspaceSizeMB: workspaceSizeMB, Seed: seed}
	st := &runState{ownershipLabel: owner}
	cleanup := true
	defer func() {
		if st.seedSnapshotDir != "" {
			_ = os.RemoveAll(st.seedSnapshotDir)
		}
		if cleanup {
			retErr = errors.Join(retErr, b.teardown(context.WithoutCancel(ctx), names, st))
		}
	}()

	st.workspace.attempted = true
	labels := append(runLabels(runID), owner)
	if err := b.rt.CreateVolume(ctx, names.Workspace, workspaceSizeMB, slices.Clone(labels)); err != nil {
		return CodexReviewWorkspaceBinding{}, fmt.Errorf("%w: create workspace volume: %w",
			ErrCodexReviewOperational, err)
	}
	st.workspace.owned = true
	view, err := b.rt.InspectVolume(ctx, names.Workspace)
	if err != nil {
		return CodexReviewWorkspaceBinding{}, fmt.Errorf("%w: inspect workspace volume: %w",
			ErrCodexReviewOperational, err)
	}
	if view.Name != names.Workspace {
		return CodexReviewWorkspaceBinding{}, failf(CheckWorkspaceSeeding,
			"observe Codex review workspace identity")
	}
	st.workspace.fingerprint, err = ownedFingerprint(
		view.CreationDate, view.Labels, view.LabelsObserved, owner,
	)
	if err != nil {
		return CodexReviewWorkspaceBinding{}, failf(CheckWorkspaceSeeding,
			"authenticate Codex review workspace: %v", err)
	}
	if err := b.seedWorkspace(ctx, hs, names, st); err != nil {
		return CodexReviewWorkspaceBinding{}, err
	}
	if _, err := b.observeSeededBase(ctx, hs, names, st); err != nil {
		return CodexReviewWorkspaceBinding{}, err
	}
	binding.CreationFingerprint = st.workspace.fingerprint
	if err := journal.PutCodexReviewWorkspaceBinding(ctx, binding); err != nil {
		return CodexReviewWorkspaceBinding{}, codexReviewJournalCheckf(
			CheckWorkspaceSeeding, "persist workspace binding: %v", err)
	}
	cleanup = false
	return binding, nil
}

// CleanupCodexReviewWorkspace removes one prepared candidate volume by its
// durable unpredictable owner. It is used when launch fails before
// ReviewSource receives the started-container handoff.
func (b *CodexReviewLifecycle) CleanupCodexReviewWorkspace(
	ctx context.Context, journal CodexReviewJournal, sourceRunID string,
) error {
	return b.cleanupCodexReviewWorkspace(ctx, journal, sourceRunID, false)
}

func (b *CodexReviewLifecycle) cleanupOrphanedCodexReviewWorkspace(
	ctx context.Context, journal CodexReviewJournal, sourceRunID string,
) error {
	return b.cleanupCodexReviewWorkspace(ctx, journal, sourceRunID, true)
}

func (b *CodexReviewLifecycle) cleanupCodexReviewWorkspace(
	ctx context.Context, journal CodexReviewJournal, sourceRunID string, deleteBinding bool,
) error {
	binding, err := journal.GetCodexReviewWorkspaceBinding(ctx, sourceRunID)
	if err != nil {
		return err
	}
	// Re-pin the stored row to this invocation's deterministic identity
	// before acting on it: a rewritten binding must not be able to redirect
	// this deletion at another invocation's volume.
	if binding.SourceRunID != sourceRunID || binding.Volume != namesFor(sourceRunID).Workspace ||
		binding.OwnershipToken == "" {
		return failf(CheckTeardown, "stored Codex review workspace ownership is invalid")
	}
	if err := b.authorizeRuntime(ctx, codexReviewWorkspaceRuntimeResourceNames(sourceRunID)); err != nil {
		return codexReviewOperationalf("authorize Codex review workspace cleanup resources: %v", err)
	}
	owner := Label{Key: ownershipLabelKey, Value: binding.OwnershipToken}
	if err := b.deleteCodexReviewVolume(
		ctx, binding.Volume,
		objectClaim{attempted: true, owned: true, fingerprint: binding.CreationFingerprint}, owner,
	); err != nil {
		return err
	}
	if !deleteBinding {
		return nil
	}
	if err := journal.DeleteCodexReviewWorkspaceBinding(ctx, binding); err != nil {
		return codexReviewJournalCheckf(
			CheckControlPlaneIsolation, "delete Codex review workspace binding: %v", err)
	}
	return nil
}
