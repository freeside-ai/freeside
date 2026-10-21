package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// A run whose target base advanced after its candidate passed verification and
// review, but before its first publication committed an intent, re-enters
// instead of blocking (plan §7, revision 89; issue #2058). The run keeps its
// head. The re-entry merges that head into the newer base, verifies and
// reviews the merge, and publishes the head under a fresh publication identity
// against the newer base. Nothing was published, so there is no pull request
// to update: the publication is still the run's first.
//
// A successor cycle's head (a feedback return or a remediation continuation)
// re-enters the same way once the run's pull request exists (issue #2111),
// except that its publication updates that pull request to the kept head.
//
// The cycle reuses the head's original checkpoint and stored candidate
// authorization, which name the base the head was imported over, for the
// candidate the publisher re-gates. Its own verification at the newer base is
// a re-entry checkpoint, as an in-place base-advance re-entry records, and is
// what the review gate and readiness read.

// sealUnpublishedReentry answers a publication refused because the target base
// advanced, when no intent was committed. In one transaction it seals the
// re-entry authority against the newest recorded audit tip, reserves the
// authority's publication, queues the re-entry task, and retires the refused
// task's row. A run with no pull request yet seals an unpublished re-entry
// (v5); a feedback, remediation continuation, or successor re-entry cycle
// after the pull request exists seals a successor re-entry (v6, issue #2111),
// which later updates that pull request. When the review hard round limit
// leaves no round to re-review, it blocks the run instead. handled is false
// for a task this rule does not cover; the caller then blocks it as before.
func (w *productionPublicationWorkflow) sealUnpublishedReentry(
	ctx context.Context,
	task productionPublicationTask,
	run domain.Run,
	checkpoint productionVerificationCheckpoint,
) (outcome productionTaskOutcome, handled bool, err error) {
	if task.reevaluation != nil || !reentryCovers(task) {
		return productionTaskOutcome{}, false, nil
	}
	// A hold an earlier pass recorded describes a retry the re-entry replaces.
	if err := w.supersedeBlockedHold(ctx, task); err != nil {
		return productionTaskOutcome{}, true, err
	}
	exhausted, err := w.sealReentry(ctx, task, true)
	if err != nil {
		return productionTaskOutcome{}, true, err
	}
	if exhausted {
		outcome, err := w.blockUnpublishedReentry(
			ctx, task, run, checkpoint, checkpoint.Artifacts, productionBlockReentryRoundLimit)
		return outcome, true, err
	}
	return productionTaskOutcome{}, true, nil
}

// reentryCovers reports whether a base-advance refusal of the task's head can
// re-enter: the run's first publication or an unpublished re-entry of it, or
// a successor cycle whose authority admits a successor re-entry after it.
// A continuation intent is not a publication cycle of its own.
func reentryCovers(task productionPublicationTask) bool {
	if task.continuation != nil {
		return false
	}
	return task.Successor == nil || task.Successor.RepublishesUnpublished() ||
		task.Successor.AdmitsSuccessorReentry()
}

// sealReentry is the transaction behind sealUnpublishedReentry. retire marks
// the refused task's row dispatched; a rerun's source row already is, and the
// rerun's own intent completes through its completion record instead.
// exhausted reports that the hard round limit left no round, and nothing was
// sealed.
func (w *productionPublicationWorkflow) sealReentry(
	ctx context.Context, task productionPublicationTask, retire bool,
) (exhausted bool, err error) {
	hardLimit, err := w.reviewHardRoundLimit(ctx, task.RunID)
	if err != nil {
		return false, err
	}
	// The source task is the cycle the re-entry replaces; a rerun names it
	// through its own intent, which never becomes the authorizing task.
	source := task
	source.reevaluation = nil
	err = w.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.ClearRunHold(ctx, task.RunID); err != nil {
			return err
		}
		tip, err := tx.LatestRunWorkflowAuditTip(ctx, task.RunID)
		if err != nil {
			return fmt.Errorf("read the advanced target base: %w", err)
		}
		latest, err := tx.LatestReviewRecord(ctx, task.RunID)
		if err != nil {
			return fmt.Errorf("read the review the re-entry follows: %w", err)
		}
		if latest.Round+1 > hardLimit {
			exhausted = true
			return nil
		}
		predecessor := productionReadyItemID(task.RunID)
		authorizingTaskKey := source.intentKey()
		version, origin := domain.PublicationUnpublishedReentryVersion, domain.PublicationSuccessorUnpublishedBaseAdvance
		if task.Successor != nil {
			predecessor = task.Successor.ReadyItemID()
			if !task.Successor.RepublishesUnpublished() {
				version, origin = domain.PublicationSuccessorReentryVersion, domain.PublicationSuccessorBaseAdvance
			}
		}
		if task.AuthorizingTaskKey != "" {
			authorizingTaskKey = task.AuthorizingTaskKey
		}
		successor := domain.PublicationSuccessor{
			Version:                 version,
			RunID:                   task.RunID,
			PredecessorItemID:       predecessor,
			PriorReviewInvocationID: latest.InvocationID,
			ReviewRound:             latest.Round + 1,
			Origin:                  origin,
			Reentry: &domain.PublicationSuccessorReentry{
				Reason:  domain.ReadinessInvalidationBaseAdvanced,
				BaseSHA: tip, HeadSHA: task.HeadSHA,
			},
		}
		next := productionPublicationTask{
			Version: task.Version, RunID: task.RunID, ProjectID: task.ProjectID,
			ProducingInvocationID: task.ProducingInvocationID,
			VerificationID:        task.VerificationID,
			PublicationID:         successor.PublicationID(),
			HeadSHA:               task.HeadSHA,
			Artifacts:             task.Artifacts,
			Replay:                task.Replay,
			Publication:           task.Publication,
			Summary:               task.Summary,
			LegacyRemediationNoop: task.LegacyRemediationNoop,
			Successor:             &successor,
			AuthorizingTaskKey:    authorizingTaskKey,
		}
		if err := next.validate(); err != nil {
			return fmt.Errorf("build re-entry task: %w", err)
		}
		// The store seals only against the newest audit tip, from the chain
		// tip, while the replaced publication holds a bare reservation.
		if err := tx.RecordPublicationSuccessor(ctx, successor); err != nil {
			return fmt.Errorf("record re-entry authority: %w", err)
		}
		reservation, err := publish.NewReservation(next.PublicationID, next.RunID)
		if err != nil {
			return err
		}
		if err := publish.ClaimInvocation(ctx, tx, reservation); err != nil {
			return err
		}
		payload, err := json.Marshal(next)
		if err != nil {
			return err
		}
		entry, _, err := tx.EnqueueOutbox(ctx, next.intentKey(), KindProductionPublicationRequested, payload)
		if err != nil {
			return err
		}
		if entry.Kind != KindProductionPublicationRequested || !bytes.Equal(entry.Payload, payload) {
			return fmt.Errorf("re-entry task disagrees with stored row: %w",
				domain.ErrImmutableTransition)
		}
		if !retire {
			return nil
		}
		return tx.MarkOutboxDispatched(ctx, task.intentKey())
	})
	return exhausted, err
}

// reenterRerunBaseAdvance answers an operator rerun of a successor cycle
// blocked because the target base advanced (issue #2111). Rerunning that
// cycle at the base it was imported over would spend a review round on a
// merge the publisher refuses again, so the rerun seals the successor
// re-entry the refusal would have sealed and completes as reentered; the
// re-entry then reviews and publishes the head at the newer base. Only the
// cycle's own block qualifies (ReentersOnRerunOf), since the re-entry reuses
// that cycle's verification, under the profile the cycle binds. Signet
// accepts such a rerun under the trust profile that already authorized the
// head, which an ordinary reevaluation could not record again, so a rerun
// under the cycle's profile never falls through to one: with no review round
// left it blocks on the round limit, and a rerun whose re-entry an earlier
// pass already sealed records only its completion. handled is false for any
// other rerun.
func (w *productionPublicationWorkflow) reenterRerunBaseAdvance(
	ctx context.Context, task productionPublicationTask, binding productionBinding,
) (outcome productionTaskOutcome, handled bool, err error) {
	if task.reevaluation == nil || task.Successor == nil || !task.Successor.AdmitsSuccessorReentry() {
		return productionTaskOutcome{}, false, nil
	}
	sourceItemID, err := w.reevaluationSourceItemID(ctx, task)
	if err != nil {
		return productionTaskOutcome{}, true, err
	}
	var source domain.AttentionItem
	var current *domain.PublicationSuccessor
	err = w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if source, err = tx.GetAttentionItemRecord(ctx, sourceItemID); err != nil {
			return err
		}
		current, err = tx.CurrentPublicationSuccessor(ctx, task.RunID)
		return err
	})
	if err != nil {
		return productionTaskOutcome{}, true, err
	}
	if !task.Successor.ReentersOnRerunOf(source) {
		return productionTaskOutcome{}, false, nil
	}
	switch {
	case current == nil:
		return productionTaskOutcome{}, true, fmt.Errorf(
			"rerun %q has no publication cycle: %w", task.intentKey(), domain.ErrParentKeyMismatch)
	case current.PublicationID() == task.Successor.PublicationID():
		// The re-entry reuses the cycle's verification under the profile the
		// cycle binds. A rerun under another profile is an ordinary
		// reevaluation, as before the re-entry existed.
		replaced := task
		replaced.reevaluation = nil
		cycle, err := w.loadBinding(ctx, replaced)
		if err != nil {
			return productionTaskOutcome{}, true, err
		}
		if cycle.profile.ProfileDigest != binding.profile.ProfileDigest {
			return productionTaskOutcome{}, false, nil
		}
		exhausted, err := w.sealReentry(ctx, task, false)
		if err != nil {
			return productionTaskOutcome{}, true, err
		}
		if exhausted {
			checkpoint, found, err := w.loadCheckpoint(ctx, replaced, cycle)
			if err != nil || !found {
				return productionTaskOutcome{}, true, errors.Join(err, domain.ErrParentKeyMismatch)
			}
			outcome, err := w.blockUnpublishedReentry(
				ctx, task, binding.run, checkpoint, checkpoint.Artifacts, productionBlockReentryRoundLimit)
			return outcome, true, err
		}
	case current.Version != domain.PublicationSuccessorReentryVersion ||
		current.Origin != domain.PublicationSuccessorBaseAdvance ||
		current.PredecessorItemID != task.Successor.ReadyItemID():
		return productionTaskOutcome{}, true, fmt.Errorf(
			"rerun %q is not of the run's current publication cycle: %w",
			task.intentKey(), domain.ErrParentKeyMismatch)
	}
	accepted, err := w.recordCompletedTerminalAtBoundary(ctx, binding.run, task, signet.PublicationReevaluationReentered)
	if err != nil {
		return productionTaskOutcome{}, true, err
	}
	if err := w.finishTask(ctx, task); err != nil {
		return productionTaskOutcome{}, true, err
	}
	return productionTaskOutcome{completed: true, accepted: accepted}, true, nil
}

// blockUnpublishedReentry ends a base-advance re-entry on a cause a retry
// cannot clear. The candidate it would have published is the original
// checkpoint's, which the block names; the evidence it shows is the cause's.
func (w *productionPublicationWorkflow) blockUnpublishedReentry(
	ctx context.Context,
	task productionPublicationTask,
	run domain.Run,
	checkpoint productionVerificationCheckpoint,
	artifacts []domain.Artifact,
	reason string,
) (productionTaskOutcome, error) {
	facts, err := definitivePublishBlockFacts(reason)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	return w.completeBlockedTask(ctx, task, run, checkpoint.Imported, artifacts,
		reason, productionShadowStopBlockActions, facts)
}

// reconcileUnpublishedReentry runs one re-entry cycle for a task that
// re-enters before publication, whether it makes the run's first publication
// or updates the run's pull request. checkout is at the base the head was imported
// over, with the head imported; it is the checkout the publisher pushes from.
func (w *productionPublicationWorkflow) reconcileUnpublishedReentry(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	checkout PublicationCheckout,
	checkoutDir string,
	imported importer.Result,
	instructionsDir string,
) (productionTaskOutcome, error) {
	checkpoint, found, err := w.loadCheckpoint(ctx, task, binding)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	// The seal follows only a verified, authorized candidate, so its
	// checkpoint must exist and still describe this reconstruction.
	if !found || !reflect.DeepEqual(checkpoint.Imported, imported) ||
		!checkpoint.Authorization.AuthorizesPublication {
		return productionTaskOutcome{}, fmt.Errorf(
			"unpublished re-entry %q has no authorizing verification checkpoint: %w",
			task.intentKey(), domain.ErrParentKeyMismatch)
	}
	readyExists, err := w.hasReadyItemRecord(ctx, task)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	// A failed verification at the new base blocks with that verification's
	// evidence, so its recovery waits for the re-entry checkpoint below.
	blockReason, err := w.blockedItemReason(ctx, task)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	if blockReason != productionBlockReentryVerification {
		blocked, err := w.recoverDefinitiveBlockedTask(
			ctx, task, binding, imported, checkpoint, true, readyExists)
		if err != nil || blocked != nil {
			return derefOutcome(blocked), err
		}
	}
	newBase := task.Successor.Reentry.BaseSHA
	if task.HeadSHA == newBase {
		// The head is already the target's tip: there is nothing to merge,
		// and publishing a branch at the base would propose no change.
		return w.blockUnpublishedReentry(
			ctx, task, binding.run, checkpoint, checkpoint.Artifacts, productionBlockBaseAdvanced)
	}
	transport, ok := w.transport.(interface {
		ProspectiveMergeOnto(context.Context, PublicationCheckout, string, string) (string, error)
	})
	if !ok {
		return productionTaskOutcome{}, errors.New(
			"publication transport does not support a merge onto an advanced base")
	}
	merge, err := transport.ProspectiveMergeOnto(ctx, checkout, newBase, task.HeadSHA)
	if err != nil {
		var conflict *publish.MergeConflictError
		if errors.As(err, &conflict) {
			return w.blockUnpublishedReentry(
				ctx, task, binding.run, checkpoint, checkpoint.Artifacts, productionBlockReentryConflict)
		}
		if errors.Is(err, publish.ErrRemoteMissingBase) {
			// The base branch no longer holds the tip the audit observed: it
			// was rewritten after the seal, and this authority names a base
			// nobody can merge into.
			return w.blockUnpublishedReentry(
				ctx, task, binding.run, checkpoint, checkpoint.Artifacts, productionBlockBaseAdvanced)
		}
		return w.reentryTransportFailure(ctx, task, "build prospective merge onto the advanced base", err)
	}
	// The merge is evidence only for the pair the authority sealed. Nothing
	// verifies, reviews, or proves against it before this gate admits it.
	if !task.Successor.AllowsProspectiveMerge(domain.ProspectiveMergeIdentity{
		BaseSHA: newBase, HeadSHA: task.HeadSHA, MergeSHA: merge,
	}) {
		return productionTaskOutcome{}, fmt.Errorf(
			"unpublished re-entry authority does not admit prospective merge %q: %w",
			merge, domain.ErrParentKeyMismatch)
	}
	createdAt, err := w.taskRecordedAt(ctx, task)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	// cycle is the binding the newer base is reviewed and verified under. The
	// original binding still names the candidate the publisher re-gates.
	cycle := binding
	cycle.admission.Base.BaseSHA = newBase
	cycle.evaluatedSHA = merge
	cycle.remediation = nil
	cycle.reentry = &reentryCycle{createdAt: createdAt, admittedBaseSHA: binding.admission.Base.BaseSHA}
	reviewInstructions, heldOutcome, err := w.exactBaseReviewInstructions(
		ctx, task, checkout, instructionsDir, newBase)
	if err != nil || heldOutcome != nil {
		return derefOutcome(heldOutcome), err
	}
	if err := w.resolveVerificationImage(ctx, &cycle, merge); err != nil {
		return productionTaskOutcome{}, err
	}
	// The cycle's verification is its own invocation: the original's evidence
	// stays where the stored authorization names it.
	verifyTask := task
	verifyTask.VerificationID = reentryVerificationInvocationID(*task.Successor)
	stored, storedFound, err := w.loadReentryCheckpoint(ctx, verifyTask, cycle)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	if readyExists && !storedFound {
		return productionTaskOutcome{}, fmt.Errorf(
			"ready item %q has no re-entry verification checkpoint: %w",
			task.readyItemID(), domain.ErrParentKeyMismatch)
	}
	if !readyExists && !w.approvedRecipes[cycle.image.RecipeDigest] {
		return w.holdBlockedTask(ctx, task, imported,
			"Publication is durably held because current trust no longer approves the admitted project-image recipe. Restore that approval to resume.",
			domain.HoldRecipeRevoked)
	}
	if !storedFound {
		stored, err = w.verifyReentry(ctx, verifyTask, cycle, checkoutDir)
		if stop := unpublishedReentryVerifyStop(task, newBase, err); stop {
			return w.blockUnpublishedReentry(
				ctx, task, binding.run, checkpoint, checkpoint.Artifacts, productionBlockBaseAdvanced)
		}
		if err != nil {
			return productionTaskOutcome{}, err
		}
		if w.afterVerification != nil {
			if err := w.afterVerification(); err != nil {
				return productionTaskOutcome{}, fmt.Errorf("after production verification: %w",
					errors.Join(err, errProductionCrashSeam))
			}
		}
	}
	if blockReason == productionBlockReentryVerification {
		blocked, err := w.recoverDefinitiveBlockedTask(
			ctx, task, binding, imported, withArtifacts(checkpoint, stored.Artifacts), true, readyExists)
		if err != nil || blocked != nil {
			return derefOutcome(blocked), err
		}
	}
	if !stored.clean() {
		return w.blockUnpublishedReentry(
			ctx, task, binding.run, checkpoint, stored.Artifacts, productionBlockReentryVerification)
	}
	view := stored.view(cycle.admission.Base.Repo)
	reviewState, err := w.reconcileReviewGate(ctx, task, cycle, view, checkout, reviewInstructions)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	switch reviewState {
	case productionReviewPending:
		return productionTaskOutcome{}, nil
	case productionReviewEscalated:
		return w.completeReviewEscalationTask(ctx, task, binding)
	case productionReviewPassed:
	case productionReviewContinue:
		return productionTaskOutcome{}, fmt.Errorf(
			"review continuation escaped the review gate: %w", domain.ErrParentKeyMismatch)
	}
	_, persistReadiness, err := w.assertReviewedCandidate(ctx, task, cycle, view, reviewInstructions)
	if err != nil {
		if errors.Is(err, errShadowReviewStopped) {
			return w.completeBlockedTask(
				ctx, task, binding.run, checkpoint.Imported, checkpoint.Artifacts,
				productionBlockTrust, productionShadowStopBlockActions, nil)
		}
		if errors.Is(err, errShadowReviewBlocksReady) {
			return productionTaskOutcome{}, nil
		}
		if errors.Is(err, domain.ErrReviewConfigurationUnapproved) {
			return w.holdReviewConfigurationMismatch(ctx, task, checkpoint.Imported, err)
		}
		if errors.Is(err, domain.ErrParentKeyMismatch) {
			return w.holdBlockedTask(
				ctx, task, checkpoint.Imported,
				"Publication is durably held because the complete current verification requirement set is not ready under its exact policy, base, and registry bindings.",
				domain.HoldTrustBlocked,
			)
		}
		return productionTaskOutcome{}, err
	}
	if !readyExists {
		if err := persistReadiness(ctx); err != nil {
			return productionTaskOutcome{}, err
		}
	}
	// The ready item shows the evidence and diff at the newer base, and the
	// claims the candidate's import carried.
	completion := view
	completion.Imported = checkpoint.Imported
	adoptedProfile, err := w.adoptedReviewProfileDigest(ctx, binding)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	report, err := loadVerificationReport(w.artifacts, checkpoint.Artifacts, task.verificationInvocationID())
	if err != nil {
		return productionTaskOutcome{}, productionPublicationRetryableError(err)
	}
	if err := w.reconcilePublicationAuthoring(
		ctx, task, binding, checkpoint, reviewInstructions, checkoutDir, report,
	); err != nil {
		return productionTaskOutcome{}, productionPublicationRetryableError(err)
	}
	// The closure approval binds the merge that will land, at the newer base,
	// so an approval for the admitted base never authorizes a Closes here.
	if err := w.reconcilePublicationClosure(
		ctx, task, binding, newBase, checkpoint, reviewInstructions, checkoutDir, report,
	); err != nil {
		return productionTaskOutcome{}, productionPublicationRetryableError(err)
	}
	candidate, err := w.productionCandidate(ctx, task, binding, checkpoint, adoptedProfile, nil, report)
	if err != nil {
		return w.holdPublicMetadataTask(ctx, task, checkpoint.Imported, err)
	}
	candidate.BaseSHA = newBase
	candidate.ProspectiveMergeSHA = merge
	// A successor re-entry updates the pull request its chain published.
	if err := w.bindSuccessorCandidate(ctx, task, &candidate); err != nil {
		return productionTaskOutcome{}, err
	}
	history, err := publish.LoadDispositionHistory(ctx, w.store, candidate, reviewInstructions.ResultDigest)
	if err != nil {
		if errors.Is(err, domain.ErrReviewConfigurationUnapproved) {
			return w.holdReviewConfigurationMismatch(ctx, task, checkpoint.Imported, err)
		}
		if errors.Is(err, domain.ErrParentKeyMismatch) {
			return w.holdDispositionHistoryMismatch(ctx, task, checkpoint.Imported)
		}
		return productionTaskOutcome{}, fmt.Errorf("load re-entry disposition history: %w", err)
	}
	candidate.DispositionHistory = &history
	published, outcomeFound, err := w.loadPublicationOutcome(
		ctx, task, candidate, w.convergePublicationOutcome)
	if err != nil {
		return w.unpublishedReentryPublishFailure(ctx, task, binding.run, checkpoint, err, true)
	}
	if !outcomeFound {
		if err := runDurableTransitionHook(w.transitionHook,
			DurableTransitionPublicationEffect, DurableTransitionBefore); err != nil {
			return productionTaskOutcome{}, err
		}
		published, err = w.publishCandidate(ctx, task, candidate, checkout)
		if err != nil {
			return w.unpublishedReentryPublishFailure(ctx, task, binding.run, checkpoint, err, false)
		}
		if err := runDurableTransitionHook(w.transitionHook,
			DurableTransitionPublicationEffect, DurableTransitionAfter); err != nil {
			return productionTaskOutcome{}, err
		}
		if w.afterPublication != nil {
			if err := w.afterPublication(); err != nil {
				return productionTaskOutcome{}, fmt.Errorf("after production publication: %w",
					errors.Join(err, errProductionCrashSeam))
			}
		}
	}
	return w.completePublishedTask(ctx, task, cycle, completion, published, reviewInstructions)
}

// unpublishedReentryPublishFailure classifies a failed publication the way the
// first cycle does: recovering is true for a failure converging an outcome
// already committed, false for one the publication effect returned. A base
// that advanced again before any intent committed seals the next re-entry.
func (w *productionPublicationWorkflow) unpublishedReentryPublishFailure(
	ctx context.Context,
	task productionPublicationTask,
	run domain.Run,
	checkpoint productionVerificationCheckpoint,
	err error,
	recovering bool,
) (productionTaskOutcome, error) {
	imported := checkpoint.Imported
	if isDurablePublicationConflict(err) {
		return w.holdBlockedTask(
			ctx, task, imported,
			"Publication is durably held because the external branch or pull request conflicts with the committed identity. Inspect and repair that external state to resume recovery.",
			domain.HoldExternalConflict,
		)
	}
	if productionPublicationPermanentExternalFailure(err) {
		return w.holdBlockedTask(ctx, task, imported, productionBlockExternal, domain.HoldExternalConflict)
	}
	if recovering {
		if held, handled, holdErr := w.holdPublicationRepairRefusal(ctx, task, imported, err); handled {
			return held, holdErr
		}
		return productionTaskOutcome{}, err
	}
	if isDefinitiveTrustRefusal(err) {
		pendingIntent, pendingErr := w.hasPendingPublicationIntent(ctx, task.PublicationID)
		if pendingErr != nil {
			return productionTaskOutcome{}, errors.Join(err, pendingErr)
		}
		if pendingIntent {
			return w.holdBlockedTask(
				ctx, task, imported,
				"Publication is durably held because current trust definitively refused the committed publication intent. Repair the trust failure to resume recovery.",
				domain.HoldTrustBlocked,
			)
		}
		if errors.Is(err, publish.ErrTargetBaseAdvanced) {
			outcome, _, sealErr := w.sealUnpublishedReentry(ctx, task, run, checkpoint)
			if sealErr != nil {
				return productionTaskOutcome{}, errors.Join(err, sealErr)
			}
			return outcome, nil
		}
		outcome, blockErr := w.completeBlockedTask(
			ctx, task, run, imported, checkpoint.Artifacts,
			productionBlockTrust, productionShadowStopBlockActions,
			publishBlockFacts(domain.HoldTrustBlocked, err),
		)
		if blockErr != nil {
			return productionTaskOutcome{}, errors.Join(err, blockErr)
		}
		return outcome, nil
	}
	if productionPublicationRetryableFailure(err) {
		return productionTaskOutcome{}, err
	}
	return productionTaskOutcome{}, fmt.Errorf("publish unpublished re-entry candidate: %w", err)
}

// unpublishedReentryVerifyStop reports whether verification at the newer base
// failed on a cause the base itself decides: a tree the image cannot verify, a
// rebuild trust refuses, or an image that does not serve the base.
func unpublishedReentryVerifyStop(task productionPublicationTask, baseSHA string, err error) bool {
	if _, stop := reentryUnverifiableTreeReason(task, baseSHA, err); stop {
		return true
	}
	if _, stop := reentryRebuildRefusedReason(task, baseSHA, err); stop {
		return true
	}
	_, stop := reentryUnservedBaseReason(task, baseSHA, err)
	return stop
}

// taskRecordedAt is when the task's row was recorded. A re-entry cycle stamps
// its verification evidence with it, so a re-run reproduces the same bytes.
func (w *productionPublicationWorkflow) taskRecordedAt(
	ctx context.Context, task productionPublicationTask,
) (recordedAt time.Time, err error) {
	err = w.store.Read(ctx, func(tx *store.ReadTx) error {
		entry, err := tx.GetOutbox(ctx, task.intentKey())
		if err != nil {
			return err
		}
		recordedAt = entry.CreatedAt.UTC()
		return nil
	})
	if err == nil && recordedAt.IsZero() {
		err = fmt.Errorf("task row %q has no recorded time: %w", task.intentKey(), domain.ErrParentKeyMismatch)
	}
	return recordedAt, err
}

// blockedItemReason reads the reason of the task's definitive blocked item,
// or "" when it has none.
func (w *productionPublicationWorkflow) blockedItemReason(
	ctx context.Context, task productionPublicationTask,
) (string, error) {
	var reason string
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		item, err := tx.GetAttentionItemRecord(ctx, task.blockedItemID())
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		reason = item.Reason
		return nil
	})
	return reason, err
}

func withArtifacts(checkpoint productionVerificationCheckpoint, artifacts []domain.Artifact) productionVerificationCheckpoint {
	checkpoint.Artifacts = artifacts
	return checkpoint
}

func derefOutcome(outcome *productionTaskOutcome) productionTaskOutcome {
	if outcome == nil {
		return productionTaskOutcome{}
	}
	return *outcome
}
