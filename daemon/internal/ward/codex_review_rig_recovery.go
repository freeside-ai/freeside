package ward

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

// RecoverCodexReviewResources reconciles only review journals whose derived
// runtime namespace belongs to resources. The caller must hold the stale rig
// and database locks for the whole call. Journal ownership and fingerprints
// remain the authority for teardown; the rig list only restricts its scope.
// No launch configuration, credentials, source reader or workflow is composed.
// Interrupted outcomes are retained through ordinary review recovery, so cleanup
// cannot turn a lost invocation into either a successful result or a fresh run.
func RecoverCodexReviewResources(
	ctx context.Context, rt Runtime, journal CodexReviewJournal,
	exportRoot string, resources RuntimeResourceNames,
) error {
	if rt == nil || journal == nil || !cleanAbs(exportRoot) {
		return fmt.Errorf("%w: runtime, journal and absolute export root are required", ErrInvalidConfig)
	}
	resources = RuntimeResourceNames{
		Containers: slices.Clone(resources.Containers),
		Volumes:    slices.Clone(resources.Volumes),
		Networks:   slices.Clone(resources.Networks),
	}
	authorize := func(_ context.Context, requested RuntimeResourceNames) error {
		if !reviewResourcesWithin(requested, resources) {
			return failf(CheckControlPlaneIsolation, "review recovery resources are outside the stale rig")
		}
		return nil
	}
	// Recovery uses only the teardown defaults. Construct no Backend or launch
	// lifecycle: those require image, provider and credential configuration.
	cfg := (Config{ExportRoot: exportRoot}).withDefaults()
	lifecycle := &CodexReviewLifecycle{
		runtimeOps: newRuntimeOps(rt, cfg), cfg: newCodexReviewLifecycleConfig(cfg),
		authorizeRuntimeResources: authorize, codexReviewRuns: map[string]chan struct{}{},
	}
	leaser, err := NewRuntimeCodexReviewVolumeLeaser(rt)
	if err != nil {
		return err
	}
	recovery, err := NewCodexReviewRecovery(lifecycle, journal, leaser, "")
	if err != nil {
		return err
	}
	ids, err := journal.ListCodexReviewIntentIDs(ctx)
	if err != nil {
		return err
	}
	known := make(map[string]struct{}, len(ids))
	var recoveryErrors []error
	for _, id := range ids {
		intent, err := journal.GetCodexReviewIntent(ctx, id)
		if err != nil {
			return err
		}
		names, err := intent.validatedResourceNames(id)
		if err != nil {
			return err
		}
		known[id] = struct{}{}
		if !reviewResourcesWithin(codexReviewRuntimeResourceNames(id, names), resources) {
			continue
		}
		if err := recovery.reconcileIntent(ctx, id, intent); err != nil {
			recoveryErrors = append(recoveryErrors, fmt.Errorf("recover rig review %q: %w", id, err))
		}
	}
	// A crash during workspace preparation can precede the launch intent.
	// Recover only such bindings, never a workspace belonging to an excluded
	// intent or one whose authenticated teardown has just failed.
	workspaceIDs, err := journal.ListCodexReviewWorkspaceIDs(ctx)
	if err != nil {
		return errors.Join(append(recoveryErrors, err)...)
	}
	for _, id := range workspaceIDs {
		if _, exists := known[id]; exists {
			continue
		}
		if !reviewResourcesWithin(codexReviewWorkspaceRuntimeResourceNames(id), resources) {
			continue
		}
		if err := lifecycle.cleanupOrphanedCodexReviewWorkspace(ctx, journal, id); err != nil {
			recoveryErrors = append(recoveryErrors, fmt.Errorf("recover rig review workspace %q: %w", id, err))
		}
	}
	return errors.Join(recoveryErrors...)
}

func reviewResourcesWithin(requested, allowed RuntimeResourceNames) bool {
	for _, group := range []struct{ requested, allowed []string }{
		{requested.Containers, allowed.Containers},
		{requested.Volumes, allowed.Volumes},
		{requested.Networks, allowed.Networks},
	} {
		for _, name := range group.requested {
			if !slices.Contains(group.allowed, name) {
				return false
			}
		}
	}
	return true
}
