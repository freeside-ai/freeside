package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// loadReadyPublicationOutcome authenticates the crash frontier after ready
// attention committed but before terminal/task cleanup. The ready item is the
// durable proof that live outcome verification already succeeded; without its
// exact finalized outcome, authorization, checkpoint, and task bindings this
// path fails closed rather than consulting mutable forge state.
func (w *productionPublicationWorkflow) loadReadyPublicationOutcome(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	checkpoint productionVerificationCheckpoint,
	candidate publish.Candidate,
	readiness productionReadiness,
) (publish.Result, error) {
	yieldHistory, err := w.reviewYieldHistory(ctx, task.RunID)
	if err != nil {
		return publish.Result{}, err
	}
	published, found, err := w.loadPublicationOutcome(ctx, task, candidate, func(
		ctx context.Context,
		candidate publish.Candidate,
		identity publish.Identity,
		outcome publish.Outcome,
	) error {
		published := publish.Result{
			Identity: identity, Branch: outcome.Branch, PRNumber: outcome.PRNumber,
		}
		compatible, err := w.hasCompatibleReadyItem(
			ctx, task, binding, checkpoint, published, readiness, yieldHistory,
		)
		if err != nil {
			return err
		}
		if !compatible {
			return fmt.Errorf("ready item disappeared during finalized recovery: %w", store.ErrNotFound)
		}
		return w.convergePublicationOutcome(ctx, candidate, identity, outcome)
	})
	if err != nil {
		return publish.Result{}, err
	}
	if !found {
		return publish.Result{}, fmt.Errorf(
			"ready item %q has no finalized publication outcome: %w",
			task.readyItemID(), domain.ErrParentKeyMismatch,
		)
	}
	return published, nil
}

func (w *productionPublicationWorkflow) convergePublicationOutcome(
	ctx context.Context,
	candidate publish.Candidate,
	identity publish.Identity,
	outcome publish.Outcome,
) error {
	recipes, err := w.recipes(ctx)
	if err != nil {
		return err
	}
	return w.publisher.ConvergeOutcome(
		ctx, candidate, recipes, identity, outcome,
	)
}

func (w *productionPublicationWorkflow) holdPublicationRepairRefusal(
	ctx context.Context,
	task productionPublicationTask,
	imported importer.Result,
	err error,
) (productionTaskOutcome, bool, error) {
	var reason string
	var cause domain.RunHoldReason
	switch {
	case errors.Is(err, domain.ErrUnapprovedRecipe):
		reason = productionBlockRepairRecipeRevoked
		cause = domain.HoldRecipeRevoked
	case errors.Is(err, publish.ErrTrustProfileDrift),
		errors.Is(err, publish.ErrUnauthorizedPublication):
		reason = productionBlockRepairTrustRefused
		cause = domain.HoldTrustBlocked
	default:
		return productionTaskOutcome{}, false, nil
	}
	held, holdErr := w.holdBlockedTask(ctx, task, imported, reason, cause)
	return held, true, holdErr
}

func (w *productionPublicationWorkflow) armReadyItemWatches(
	ctx context.Context, task productionPublicationTask,
) error {
	var item domain.AttentionItem
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		item, err = tx.GetAttentionItem(ctx, task.readyItemID())
		return err
	}); err != nil {
		return fmt.Errorf("arm publication watches: %w", err)
	}
	return armPublicationWatches(ctx, w.store, item, w.now())
}

func (w *productionPublicationWorkflow) hasReadyItemRecord(
	ctx context.Context, task productionPublicationTask,
) (bool, error) {
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetAttentionItemRecord(ctx, task.readyItemID())
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (w *productionPublicationWorkflow) loadPublicationOutcome(
	ctx context.Context,
	task productionPublicationTask,
	candidate publish.Candidate,
	verify publish.OutcomeVerifier,
) (publish.Result, bool, error) {
	identity, err := publicationIdentity(candidate)
	if err != nil {
		return publish.Result{}, false, err
	}
	intentKey, err := publish.IntentKey(candidate.InvocationID, publish.IntentKindPublication)
	if err != nil {
		return publish.Result{}, false, err
	}
	var entry store.QueueEntry
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetOutbox(ctx, intentKey)
		return err
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return publish.Result{}, false, nil
		}
		return publish.Result{}, false, err
	}
	if entry.IdempotencyKey != intentKey {
		return publish.Result{}, false, fmt.Errorf(
			"production publication intent read back the wrong key: %w", domain.ErrParentKeyMismatch,
		)
	}
	if entry.Kind == publish.IntentKindReservation {
		held, err := publish.DecodeReservation(entry.Payload)
		if err != nil {
			return publish.Result{}, false, fmt.Errorf(
				"decode durable production publication reservation: %w",
				errors.Join(err, domain.ErrParentKeyMismatch),
			)
		}
		if held.InvocationID != task.PublicationID || held.RunID != task.RunID {
			return publish.Result{}, false, fmt.Errorf(
				"production publication reservation disagrees with task: %w", domain.ErrParentKeyMismatch,
			)
		}
		return publish.Result{}, false, nil
	}
	if entry.Kind != publish.IntentKindPublication || !entry.Dispatched() {
		return publish.Result{}, false, nil
	}
	intent, err := publish.DecodeStoredIntent(entry)
	if err != nil {
		return publish.Result{}, false, fmt.Errorf(
			"decode durable production publication intent: %w",
			errors.Join(err, domain.ErrParentKeyMismatch),
		)
	}
	if candidate.AuthorizationID == nil ||
		intent.InvocationID != task.PublicationID ||
		intent.Identity != identity.Digest() || intent.Repo != candidate.Repo ||
		intent.BaseRef != candidate.BaseRef || intent.SourceHeadSHA != task.HeadSHA ||
		intent.AuthorizationID != *candidate.AuthorizationID ||
		intent.ProducingInvocationID != task.ProducingInvocationID ||
		intent.ReservationRunID != task.RunID {
		return publish.Result{}, false, fmt.Errorf(
			"production publication intent disagrees with task: %w", domain.ErrParentKeyMismatch,
		)
	}
	if err := publish.ValidateIntentBranch(intent, candidate, identity); err != nil {
		return publish.Result{}, false, err
	}
	if err := publish.ValidateIntentDispositionHistory(intent, candidate); err != nil {
		return publish.Result{}, false, fmt.Errorf(
			"production publication intent disposition history: %w", err,
		)
	}
	if err := w.validatePublicationOutcomeAuthorization(ctx, candidate, intent.AuthorizationID); err != nil {
		return publish.Result{}, false, err
	}
	outcomeRecipes, err := w.recipes(ctx)
	if err != nil {
		return publish.Result{}, false, err
	}
	if candidate.RecipeDigest != nil {
		// A dispatched outcome records effects authorized before current trust
		// changed. Reconstruct its historical identity under its frozen recipe;
		// the immutable authorization and live forge coordinates are still
		// revalidated below. Current approval continues to gate every new effect.
		outcomeRecipes[*candidate.RecipeDigest] = true
	}
	outcome, found, err := publish.LoadOutcome(
		ctx, w.store, candidate, outcomeRecipes, verify,
	)
	if err != nil {
		if productionPublicationRetryableFailure(err) || isDurablePublicationConflict(err) ||
			errors.Is(err, domain.ErrUnapprovedRecipe) ||
			errors.Is(err, publish.ErrTrustProfileDrift) ||
			errors.Is(err, publish.ErrUnauthorizedPublication) {
			return publish.Result{}, found, err
		}
		return publish.Result{}, found, fmt.Errorf(
			"reconstruct durable production publication outcome: %w",
			errors.Join(err, domain.ErrParentKeyMismatch),
		)
	}
	if !found {
		return publish.Result{}, found, err
	}
	return publish.Result{
		Identity: identity, Branch: outcome.Branch, PRNumber: outcome.PRNumber,
	}, true, nil
}

func isDurablePublicationConflict(err error) bool {
	return errors.Is(err, publish.ErrPublicationConflict) ||
		errors.Is(err, publish.ErrForeignResource)
}

// durablePublicationConflictReason is the publish_blocked card's reason for
// a durable conflict. A pull request without this run's identity marker is
// named with its branch (#2339). Usually it is another publication's, which
// pins the branch for good: a pull request can't be deleted and the
// committed intent can't change branch. It can also be this run's own pull
// request with its marker edited away, which restoring the marker repairs,
// so the reason gives both remedies.
func durablePublicationConflictReason(err error) string {
	var foreign *publish.ForeignPullRequestError
	if !errors.As(err, &foreign) {
		return productionBlockConflict
	}
	return fmt.Sprintf(
		"Publication is held because pull request #%d on branch %s doesn't carry this run's identity marker. If it is this run's pull request and its marker was edited away, restore the marker to resume. Otherwise it belongs to another publication: a pull request can't be deleted, so this run can't publish on %s. Abandon the task with `freesided abandon` and resubmit it with a publication file that declares another branch or none.",
		foreign.Number, foreign.Branch, foreign.Branch,
	)
}

func (w *productionPublicationWorkflow) validatePublicationOutcomeAuthorization(
	ctx context.Context,
	candidate publish.Candidate,
	authorizationID domain.Digest,
) error {
	if candidate.RecipeDigest == nil || candidate.TrustProfileDigest == nil {
		return errors.New("production publication outcome candidate lost its trust bindings")
	}
	evidenceDigest, err := domain.ComputeEvidenceSnapshotDigest(candidate.Artifacts)
	if err != nil {
		return err
	}
	var authorization domain.CandidateAuthorization
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		authorization, err = tx.GetCandidateAuthorization(ctx, authorizationID)
		return err
	}); err != nil {
		return err
	}
	if err := authorization.Validate(); err != nil {
		return err
	}
	if authorization.ID != authorizationID || authorization.Repo != candidate.Repo ||
		authorization.HeadSHA != candidate.HeadSHA ||
		authorization.VerificationRecipeDigest != *candidate.RecipeDigest ||
		authorization.EvidenceSnapshotDigest != evidenceDigest ||
		authorization.TrustProfileDigest != *candidate.TrustProfileDigest ||
		!authorization.AuthorizesPublication {
		return fmt.Errorf("production publication authorization disagrees with outcome: %w",
			domain.ErrParentKeyMismatch)
	}
	return nil
}

func (w *productionPublicationWorkflow) hasPendingPublicationIntent(
	ctx context.Context,
	invocationID domain.InvocationID,
) (bool, error) {
	key, err := publish.IntentKey(invocationID, publish.IntentKindPublication)
	if err != nil {
		return false, err
	}
	var pending []store.QueueEntry
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		pending, err = tx.ListPendingOutbox(ctx, publish.IntentKindPublication)
		return err
	}); err != nil {
		return false, err
	}
	return slices.ContainsFunc(pending, func(entry store.QueueEntry) bool {
		return entry.IdempotencyKey == key
	}), nil
}

func (w *productionPublicationWorkflow) hasFinalizedPublicationIntent(
	ctx context.Context,
	invocationID domain.InvocationID,
) (bool, error) {
	key, err := publish.IntentKey(invocationID, publish.IntentKindPublication)
	if err != nil {
		return false, err
	}
	var entry store.QueueEntry
	err = w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetOutbox(ctx, key)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return entry.Kind == publish.IntentKindPublication && entry.Dispatched(), nil
}
