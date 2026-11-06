package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

// Handoff is every publication: the run's first and each successor (a
// feedback return, a remediation continuation). At handoff the workflow
// audit resolves the target's tip and the store seals it for the
// publication (plan §6, §7, revision 110; issue #2644). When the tip is the
// base the run's evidence binds, the publication goes ahead as before. When it
// moved, the daemon builds the prospective merge of the unchanged head into
// that tip in its own checkout, never pushes it, verifies it under the
// admitted recipe, records the merge and its proof on the handoff check, and
// publishes the unchanged head. Review is not rerun: the head did not change.
//
// A textual conflict or a verification failure on the merge ends the run on
// the ordinary publish_blocked item, its reason naming the cause. So does a
// merge the tip keeps from being verified at all: a rebuild trust refuses, a
// merged tree the image cannot materialize, or an image that cannot serve the
// tip. Those block on the handoff-verification reason, and their one
// daemon-written evidence artifact names the cause, the head, and the tip,
// recorded on a write-once stop row recovery reads it back from.

const (
	productionHandoffStopKind    = "production_handoff_stop"
	productionHandoffStopVersion = "freeside.production-handoff-stop/v1"
)

// productionHandoffStop is the evidence of a handoff the tip stopped before
// any verifier report existed. The handoff's re-entry checkpoint cannot hold
// it: loadReentryCheckpoint reads the verifier's own report, and a stop has
// none.
type productionHandoffStop struct {
	Version   string            `json:"version"`
	TaskKey   string            `json:"task_key"`
	HeadSHA   string            `json:"head_sha"`
	TipSHA    string            `json:"tip_sha"`
	MergeSHA  string            `json:"merge_sha"`
	Artifacts []domain.Artifact `json:"artifacts"`
}

// handoffCheckpointKey names the verification checkpoint of a publication's
// handoff merge. The tip is sealed per publication, so the publication names
// the merge.
func handoffCheckpointKey(task productionPublicationTask) string {
	return productionVerificationCheckpointKey(task.RunID, task.HeadSHA, "") +
		"/handoff/" + string(task.PublicationID)
}

// handoffStopKey names a publication's handoff stop row, beside its
// checkpoint. A handoff has a checkpoint or a stop, never both.
func handoffStopKey(task productionPublicationTask) string {
	return productionVerificationCheckpointKey(task.RunID, task.HeadSHA, "") +
		"/handoff-stop/" + string(task.PublicationID)
}

// handoffVerificationInvocationID is the verification invocation of a
// publication's handoff merge, apart from the candidate's own so the evidence
// the stored authorization names stays where it is.
func handoffVerificationInvocationID(task productionPublicationTask) domain.InvocationID {
	return domain.InvocationID("verify-handoff-" + string(task.PublicationID))
}

// reconcileHandoff returns the publication's satisfied handoff check, sealing
// its tip and verifying the merge into a moved tip as needed. A non-nil
// outcome ends or holds the task instead. It never reviews and never touches
// the review round count.
//
// checkout is at the evidence base with the head imported, the checkout the
// publisher pushes from; checkpoint is the candidate's own authorizing
// verification.
func (w *productionPublicationWorkflow) reconcileHandoff(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	checkout PublicationCheckout,
	checkoutDir string,
	checkpoint productionVerificationCheckpoint,
) (domain.HandoffCheck, *productionTaskOutcome, error) {
	legacyIntent, err := w.unsealedCommittedIntent(ctx, task)
	if err != nil {
		return domain.HandoffCheck{}, nil, err
	}
	if legacyIntent {
		// Recovered under the decision it was committed with: the publisher
		// gates only a new reservation, and a tip resolved now would move the
		// candidate's base away from what that decision named.
		base := binding.admission.Base.BaseSHA
		return domain.HandoffCheck{
			RunID: task.RunID, PublicationID: task.PublicationID, HeadSHA: task.HeadSHA,
			EvidenceBaseSHA: base, TipSHA: base,
		}, nil, nil
	}
	check, sealedNow, err := w.sealHandoffTip(ctx, task, binding)
	if err != nil {
		outcome, err := w.reentryTransportFailure(ctx, task, "resolve the handoff tip", err)
		return domain.HandoffCheck{}, &outcome, err
	}
	if check.Proved() {
		return check, nil, nil
	}
	block := func(reason string, artifacts []domain.Artifact) (domain.HandoffCheck, *productionTaskOutcome, error) {
		outcome, err := w.blockHandoff(ctx, task, binding.run, checkpoint, artifacts, reason)
		return domain.HandoffCheck{}, &outcome, err
	}
	if task.HeadSHA == check.TipSHA {
		// The head is already the target's tip: there is nothing to merge,
		// and publishing a branch at the tip would propose no change.
		return block(productionBlockBaseAdvanced, checkpoint.Artifacts)
	}
	transport, ok := w.transport.(interface {
		ProspectiveMergeOnto(context.Context, PublicationCheckout, string, string) (string, error)
	})
	if !ok {
		return domain.HandoffCheck{}, nil, errors.New(
			"publication transport does not support a merge onto the handoff tip")
	}
	merge, err := transport.ProspectiveMergeOnto(ctx, checkout, check.TipSHA, task.HeadSHA)
	if err != nil {
		var conflict *publish.MergeConflictError
		if errors.As(err, &conflict) {
			return block(productionBlockHandoffConflict, checkpoint.Artifacts)
		}
		if errors.Is(err, publish.ErrRemoteMissingBase) {
			if sealedNow {
				// The checkout was fetched at the start of the pass, before the
				// audit sealed the tip, so a push in between leaves the tip
				// out of it. The next pass fetches the branch again.
				return domain.HandoffCheck{}, nil, productionPublicationRetryableError(
					fmt.Errorf("handoff tip %s is newer than this pass's checkout: %w", check.TipSHA, err))
			}
			// A checkout fetched after the seal lacks the tip: the base
			// branch was rewritten, and the sealed tip is not re-resolved
			// for this publication.
			return block(productionBlockBaseAdvanced, checkpoint.Artifacts)
		}
		outcome, err := w.reentryTransportFailure(ctx, task, "build the handoff merge", err)
		return domain.HandoffCheck{}, &outcome, err
	}
	identity := check.MergeIdentity()
	identity.MergeSHA = merge
	if err := identity.Validate(); err != nil {
		return domain.HandoffCheck{}, nil, fmt.Errorf("handoff merge: %w", errors.Join(err, domain.ErrParentKeyMismatch))
	}
	createdAt, err := w.taskRecordedAt(ctx, task)
	if err != nil {
		return domain.HandoffCheck{}, nil, err
	}
	// cycle is the binding the merge is verified under, at the tip. The
	// original binding still names the candidate the publisher re-gates and
	// the readiness its evidence earned.
	cycle := binding
	cycle.admission.Base.BaseSHA = check.TipSHA
	cycle.evaluatedSHA = merge
	cycle.remediation = nil
	cycle.reentry = &reentryCycle{createdAt: createdAt, admittedBaseSHA: binding.admission.Base.BaseSHA}
	// A stop an earlier pass recorded is the handoff's result: the merge is
	// not verified again, and the evidence shown is the one first recorded.
	stop, stopped, err := w.loadHandoffStop(ctx, task, check)
	if err != nil {
		return domain.HandoffCheck{}, nil, err
	}
	if stopped {
		if stop.MergeSHA != merge {
			return domain.HandoffCheck{}, nil, fmt.Errorf(
				"handoff stop of %s names merge %s, the tip and head now merge to %s: %w",
				task.PublicationID, stop.MergeSHA, merge, domain.ErrParentKeyMismatch)
		}
		return block(productionBlockHandoffVerification, stop.Artifacts)
	}
	if err := w.resolveVerificationImage(ctx, &cycle, merge); err != nil {
		return domain.HandoffCheck{}, nil, err
	}
	verifyTask := task
	verifyTask.VerificationID = handoffVerificationInvocationID(task)
	key := handoffCheckpointKey(task)
	stored, found, err := w.loadReentryCheckpoint(ctx, key, verifyTask, cycle)
	if err != nil {
		return domain.HandoffCheck{}, nil, err
	}
	if !found {
		stored, err = w.verifyReentry(ctx, key, verifyTask, cycle, checkoutDir)
		if verificationMemoryLimit(err) {
			return block(productionBlockMemoryLimit, checkpoint.Artifacts)
		}
		// The tip itself decides these: a merged tree the image cannot
		// verify, a rebuild trust refuses, or an image that cannot serve the
		// tip with nothing pending in image upkeep (plan §5.7). Each blocks
		// as a failed verification of the merge does, its evidence naming the
		// cause. While upkeep has a resolution pending, verifyReentry returns
		// a retryable error the lane holds the task under instead.
		if account, stop := handoffStopAccount(task, check.TipSHA, merge, binding.image.ImageRef, err); stop {
			artifacts, err := w.recordHandoffStop(ctx, task, binding, check, merge, account, createdAt)
			if err != nil {
				return domain.HandoffCheck{}, nil, err
			}
			return block(productionBlockHandoffVerification, artifacts)
		}
		if err != nil {
			return domain.HandoffCheck{}, nil, err
		}
		if w.afterVerification != nil {
			if err := w.afterVerification(); err != nil {
				return domain.HandoffCheck{}, nil, fmt.Errorf("after handoff verification: %w",
					errors.Join(err, errProductionCrashSeam))
			}
		}
	}
	if !stored.clean() {
		return block(productionBlockHandoffVerification, stored.Artifacts)
	}
	proved, err := w.proveHandoffMerge(ctx, binding, check, identity)
	if err != nil {
		return domain.HandoffCheck{}, nil, err
	}
	return proved, nil, nil
}

// sealHandoffTip returns the publication's sealed handoff check, sealing it
// from a fresh workflow audit on the first pass. The audit is recorded with
// the seal, so the run's audit history names every tip a handoff relied on.
// A sealed tip is never re-resolved. sealedNow reports a seal this call
// wrote, whose tip the pass's checkout, fetched before it, may not hold yet.
func (w *productionPublicationWorkflow) sealHandoffTip(
	ctx context.Context, task productionPublicationTask, binding productionBinding,
) (check domain.HandoffCheck, sealedNow bool, err error) {
	check, found, err := w.loadHandoffCheck(ctx, task)
	if err != nil || found {
		return check, false, err
	}
	if w.publisher == nil {
		return domain.HandoffCheck{}, false, errors.New("handoff: no publisher to audit the target tip")
	}
	base := binding.admission.Base
	audit, err := w.publisher.AuditHandoffTip(ctx, base.Repo, base.BaseRef)
	if err != nil {
		return domain.HandoffCheck{}, false, err
	}
	check = domain.HandoffCheck{
		RunID: task.RunID, PublicationID: task.PublicationID, HeadSHA: task.HeadSHA,
		EvidenceBaseSHA: base.BaseSHA, TipSHA: audit.AuditedCommitSHA,
	}
	if err := w.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		record, err := tx.RecordWorkflowAudit(ctx, audit)
		if err != nil {
			return fmt.Errorf("record handoff workflow audit: %w", err)
		}
		check.WorkflowAuditID = record.ID
		return tx.SealHandoffTip(ctx, check, w.now().UTC())
	}); err != nil {
		return domain.HandoffCheck{}, false, err
	}
	return check, true, nil
}

// loadHandoffCheck reads the publication's sealed handoff check.
func (w *productionPublicationWorkflow) loadHandoffCheck(
	ctx context.Context, task productionPublicationTask,
) (check domain.HandoffCheck, found bool, err error) {
	err = w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		check, err = tx.GetHandoffCheck(ctx, task.RunID, task.PublicationID)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return domain.HandoffCheck{}, false, nil
	}
	if err == nil && check.HeadSHA != task.HeadSHA {
		err = fmt.Errorf("handoff check of %s names head %s, task publishes %s: %w",
			task.PublicationID, check.HeadSHA, task.HeadSHA, domain.ErrParentKeyMismatch)
	}
	return check, err == nil, err
}

// unsealedCommittedIntent reports a publication intent an earlier daemon
// committed before handoff checks existed: one is pending and no tip was
// sealed. A daemon with handoff checks seals before it commits an intent.
func (w *productionPublicationWorkflow) unsealedCommittedIntent(
	ctx context.Context, task productionPublicationTask,
) (bool, error) {
	_, sealed, err := w.loadHandoffCheck(ctx, task)
	if err != nil || sealed {
		return false, err
	}
	return w.hasPendingPublicationIntent(ctx, task.PublicationID)
}

// proveHandoffMerge records the proof that the merge passed verification,
// under the run's own clean-verification resolution and the admitted recipe,
// and the merge on the handoff check, in one transaction.
func (w *productionPublicationWorkflow) proveHandoffMerge(
	ctx context.Context,
	binding productionBinding,
	check domain.HandoffCheck,
	identity domain.ProspectiveMergeIdentity,
) (domain.HandoffCheck, error) {
	resolutions, err := productionReadinessResolutions(binding, w.store.VerificationFloorRegistryGeneration())
	if err != nil {
		return domain.HandoffCheck{}, err
	}
	var cleanVerification *domain.RequirementResolution
	for i := range resolutions {
		if resolutions[i].CheckClass == domain.CheckClassCleanVerification {
			cleanVerification = &resolutions[i]
		}
	}
	if cleanVerification == nil {
		return domain.HandoffCheck{}, fmt.Errorf("production readiness resolves no clean verification: %w",
			domain.ErrParentKeyMismatch)
	}
	tip := binding.admission.Base
	tip.BaseSHA = check.TipSHA
	proof, err := domain.NewProspectiveMergeCheckProof(*cleanVerification, identity, tip, binding.image.RecipeDigest)
	if err != nil {
		return domain.HandoffCheck{}, err
	}
	merge := domain.HandoffMerge{MergeSHA: identity.MergeSHA, VerificationProofDigest: proof.Digest}
	if err := w.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RecordRequirementResolution(ctx, *cleanVerification); err != nil {
			return err
		}
		if err := tx.RecordCheckProof(ctx, proof); err != nil {
			return err
		}
		return tx.ProveHandoffMerge(ctx, check.RunID, check.PublicationID, merge, w.now().UTC())
	}); err != nil {
		return domain.HandoffCheck{}, err
	}
	check.Merge = &merge
	return check, nil
}

// blockHandoff ends the run on a handoff cause no retry clears. The block
// names the candidate's import; the evidence it shows is the cause's.
func (w *productionPublicationWorkflow) blockHandoff(
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

// handoffStopAccount reports whether verifying merge, the head merged into
// the tip, failed on a cause the tip itself decides, with the evidence text
// that names it: a rebuild trust refuses, a tree the image cannot verify, or
// an image that does not serve the tip. imageRef is the image the run was
// admitted with. Candidate-written text reaches the account only as the
// rebuild gate already bounded and quoted it, or through reentryQuoted.
//
// Each first line is fixed, so a reader finds the cause in one place. A
// rebuild refusal's is the one a first publication's refusal shows.
func handoffStopAccount(
	task productionPublicationTask, tipSHA, mergeSHA string, imageRef domain.ImageRef, err error,
) (string, bool) {
	subject := fmt.Sprintf("candidate %s merged into target tip %s (merge %s)", task.HeadSHA, tipSHA, mergeSHA)
	var refused *reentryRebuildRefusal
	if errors.As(err, &refused) {
		return fmt.Sprintf(
			"%s\n\n"+
				"Freeside did not verify %s. The merge's dependency manifests differ from the ones project image %s was built with, and Freeside rebuilds an image without a person only for a change inside the project's policy. No verification command ran, and nothing was published.\n\n"+
				"Rerun the work against the current base once the change is within the project's policy, or rebuild the project image.\n",
			rebuildRefusedLine(refused.refusal), subject, imageRef), true
	}
	if cause, ok := unverifiableTreeCause(err); ok {
		return fmt.Sprintf(
			"Handoff merge cannot be verified: %s (%s).\n\n"+
				"Freeside could not verify %s, and nothing was published.\n\n"+
				"Change the pull request so the merged tree can be verified, then rerun the work against the current base.\n",
			cause, reentryQuoted(err.Error()), subject), true
	}
	if errors.Is(err, domain.ErrProjectImageIncompatible) {
		return fmt.Sprintf(
			"Project image cannot serve target tip %s (%s).\n\n"+
				"Freeside did not verify %s, because no project image this run may verify with serves the tip (the run was admitted with project image %s). No verification command ran, and nothing was published.\n\n"+
				"Rerun the work with a project image that serves the tip.\n",
			tipSHA, reentryQuoted(err.Error()), subject, imageRef), true
	}
	return "", false
}

// recordHandoffStop writes the stop's evidence and its stop row, write-once,
// and returns the artifacts the block shows. The row is in place before the
// item, so a pass that restarts between them blocks on the same evidence,
// unless the base branch was rewritten in between: that pass blocks as a
// rewritten tip before it reads the row.
func (w *productionPublicationWorkflow) recordHandoffStop(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	check domain.HandoffCheck,
	mergeSHA, account string,
	createdAt time.Time,
) ([]domain.Artifact, error) {
	recipes, err := w.recipes(ctx)
	if err != nil {
		return nil, err
	}
	evidence, err := daemonVerificationEvidence(handoffVerificationInvocationID(task), task.HeadSHA,
		binding.image.RecipeDigest, account, createdAt, recipes)
	if err != nil {
		return nil, err
	}
	artifacts, err := w.storeVerificationEvidence([]verify.Evidence{evidence})
	if err != nil {
		return nil, err
	}
	stop := productionHandoffStop{
		Version: productionHandoffStopVersion, TaskKey: task.intentKey(),
		HeadSHA: task.HeadSHA, TipSHA: check.TipSHA, MergeSHA: mergeSHA,
		Artifacts: artifacts,
	}
	payload, err := json.Marshal(stop)
	if err != nil {
		return nil, err
	}
	if err := runDurableTransitionHook(w.transitionHook,
		DurableTransitionVerificationEvidence, DurableTransitionBefore); err != nil {
		return nil, err
	}
	if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
		for _, artifact := range artifacts {
			if err := tx.PutArtifact(ctx, artifact); err != nil {
				return err
			}
		}
		entry, _, err := tx.RecordInbox(ctx, handoffStopKey(task), productionHandoffStopKind, payload)
		if err != nil {
			return err
		}
		if entry.Kind != productionHandoffStopKind || !bytes.Equal(entry.Payload, payload) {
			return fmt.Errorf("handoff stop of %s disagrees with stored row: %w",
				task.PublicationID, domain.ErrImmutableTransition)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := runDurableTransitionHook(w.transitionHook,
		DurableTransitionVerificationEvidence, DurableTransitionAfter); err != nil {
		return nil, err
	}
	return artifacts, nil
}

// loadHandoffStop reads the publication's handoff stop row and binds it to
// the task and its sealed check. A row that names another task, head, or tip
// contradicts the daemon's own state; its evidence must still be in the
// artifact store, as a checkpoint's must. The caller compares the merge with
// the one it built.
func (w *productionPublicationWorkflow) loadHandoffStop(
	ctx context.Context, task productionPublicationTask, check domain.HandoffCheck,
) (productionHandoffStop, bool, error) {
	var entry store.QueueEntry
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetInbox(ctx, handoffStopKey(task))
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return productionHandoffStop{}, false, nil
	}
	if err != nil {
		return productionHandoffStop{}, false, err
	}
	if entry.Kind != productionHandoffStopKind {
		return productionHandoffStop{}, false, fmt.Errorf("handoff stop of %s has kind %q: %w",
			task.PublicationID, entry.Kind, domain.ErrParentKeyMismatch)
	}
	var stop productionHandoffStop
	if err := strictjson.Decode(
		entry.Payload, &stop, strictjson.TolerateInvalidUTF8, strictjson.NoLimit,
	); err != nil {
		return productionHandoffStop{}, false, fmt.Errorf("decode handoff stop of %s: %w",
			task.PublicationID, errors.Join(err, domain.ErrParentKeyMismatch))
	}
	if stop.Version != productionHandoffStopVersion || stop.TaskKey != task.intentKey() ||
		stop.HeadSHA != task.HeadSHA || stop.TipSHA != check.TipSHA ||
		!validCommitSHA(stop.MergeSHA) || len(stop.Artifacts) != 1 {
		return productionHandoffStop{}, false, fmt.Errorf("handoff stop of %s disagrees with task: %w",
			task.PublicationID, domain.ErrParentKeyMismatch)
	}
	for _, artifact := range stop.Artifacts {
		if err := verifyArtifactBlob(w.artifacts, artifact); err != nil {
			return productionHandoffStop{}, false, fmt.Errorf("verify handoff stop artifact: %w",
				retryableOrTerminal(ctx, err, domain.ErrParentKeyMismatch))
		}
	}
	return stop, true, nil
}

// applySealedHandoffCheck binds a recovered candidate to its publication's
// sealed handoff check, when one was sealed, so the body and closure it
// renders name the tip the publication was decided against. A publication
// committed before handoff checks existed keeps its evidence base.
func (w *productionPublicationWorkflow) applySealedHandoffCheck(
	ctx context.Context, task productionPublicationTask, candidate *publish.Candidate,
) error {
	check, found, err := w.loadHandoffCheck(ctx, task)
	if err != nil || !found {
		return err
	}
	candidate.BaseSHA = check.TipSHA
	candidate.HandoffCheck = &check
	return nil
}

// withPublishedHandoff names on a published candidate's readiness detail the
// tip its sealed handoff check resolved and the verified merge into it. A
// publication intent committed before handoff checks existed has no sealed
// check; reconcileHandoff treats its evidence base as the tip, and so does
// this. A sealed moved tip without a merge never reaches publication, so one
// here is a broken invariant.
func (w *productionPublicationWorkflow) withPublishedHandoff(
	ctx context.Context, task productionPublicationTask, binding productionBinding,
	readiness productionReadiness,
) (productionReadiness, error) {
	check, found, err := w.loadHandoffCheck(ctx, task)
	if err != nil {
		return productionReadiness{}, err
	}
	if !found {
		return readiness.withHandoff(binding.admission.Base.BaseSHA, "")
	}
	if !check.Proved() {
		return productionReadiness{}, fmt.Errorf(
			"published %s has an unproved handoff merge onto tip %s: %w",
			task.PublicationID, check.TipSHA, domain.ErrParentKeyMismatch)
	}
	merge := ""
	if check.Merge != nil {
		merge = check.Merge.MergeSHA
	}
	return readiness.withHandoff(check.TipSHA, merge)
}

// handoffBlockArtifacts is the evidence a recovered handoff-verification block
// shows: the stop row's, when the tip kept the merge from being verified, or
// else the stored verification of the merge that failed.
func (w *productionPublicationWorkflow) handoffBlockArtifacts(
	ctx context.Context, task productionPublicationTask, binding productionBinding,
) ([]domain.Artifact, error) {
	check, found, err := w.loadHandoffCheck(ctx, task)
	if err != nil {
		return nil, err
	}
	if !found || !check.Moved() || check.Merge != nil {
		return nil, fmt.Errorf("handoff verification block of %s has no failed merge: %w",
			task.PublicationID, domain.ErrParentKeyMismatch)
	}
	stop, stopped, err := w.loadHandoffStop(ctx, task, check)
	if err != nil {
		return nil, err
	}
	if stopped {
		return stop.Artifacts, nil
	}
	merge, err := w.loadHandoffCheckpointMerge(ctx, task)
	if err != nil {
		return nil, err
	}
	cycle := binding
	cycle.admission.Base.BaseSHA = check.TipSHA
	cycle.evaluatedSHA = merge
	cycle.remediation = nil
	if err := w.resolveVerificationImage(ctx, &cycle, merge); err != nil {
		return nil, err
	}
	verifyTask := task
	verifyTask.VerificationID = handoffVerificationInvocationID(task)
	stored, found, err := w.loadReentryCheckpoint(ctx, handoffCheckpointKey(task), verifyTask, cycle)
	if err != nil {
		return nil, err
	}
	if !found || stored.clean() {
		return nil, fmt.Errorf("handoff verification block of %s has no failed verification: %w",
			task.PublicationID, domain.ErrParentKeyMismatch)
	}
	return stored.Artifacts, nil
}

// loadHandoffCheckpointMerge reads which merge the publication's handoff
// checkpoint verified. loadReentryCheckpoint then re-binds the whole row to
// it, so this read decides nothing by itself.
func (w *productionPublicationWorkflow) loadHandoffCheckpointMerge(
	ctx context.Context, task productionPublicationTask,
) (string, error) {
	var merge string
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		entry, err := tx.GetInbox(ctx, handoffCheckpointKey(task))
		if err != nil {
			return err
		}
		var row struct {
			EvaluatedSHA string `json:"evaluated_sha"`
		}
		if entry.Kind != productionReentryCheckpointKind {
			return domain.ErrParentKeyMismatch
		}
		if err := json.Unmarshal(entry.Payload, &row); err != nil {
			return errors.Join(err, domain.ErrParentKeyMismatch)
		}
		merge = row.EvaluatedSHA
		return nil
	})
	if err == nil && merge == "" {
		err = fmt.Errorf("handoff checkpoint of %s names no merge: %w", task.PublicationID, domain.ErrParentKeyMismatch)
	}
	return merge, err
}

// taskRecordedAt is when the task's row was recorded. A handoff stamps its
// merge's verification evidence with it, so a re-run reproduces the same
// bytes.
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

// retireReentryTask ends a re-entry cycle an earlier daemon queued for a base
// advance (a v5 or v6 publication successor, now read-only legacy). The cycle
// is never run: its rule is retired, and a moved tip is verified at handoff
// instead (issue #2644). The run ends on publish_blocked naming that, or holds
// when that daemon already committed the cycle's publication intent.
func (w *productionPublicationWorkflow) retireReentryTask(
	ctx context.Context, task productionPublicationTask, binding productionBinding,
) (productionTaskOutcome, error) {
	checkpoint, found, err := w.retiredReentryCheckpoint(ctx, task, binding)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	imported := importer.Result{CommitSHA: task.HeadSHA}
	if found {
		imported = checkpoint.Imported
	}
	recovered, err := w.recoverDefinitiveBlockedTask(ctx, task, binding, imported, checkpoint, found, false)
	if err != nil || recovered != nil {
		return derefOutcome(recovered), err
	}
	pending, err := w.hasPendingPublicationIntent(ctx, task.PublicationID)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	if pending {
		// The retired cycle's publication may have reached the forge, and
		// this daemon cannot finish it (its disposition history no longer
		// reads), so the run holds where a person can see it rather than
		// blocking over a live intent.
		return w.holdBlockedTask(ctx, task, imported, productionHoldReentryRetiredIntent, domain.HoldTrustBlocked)
	}
	facts, err := definitivePublishBlockFacts(productionBlockReentryRetired)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	return w.completeBlockedTask(ctx, task, binding.run, imported, nil,
		productionBlockReentryRetired, productionShadowStopBlockActions, facts)
}

// productionHoldReentryRetiredIntent is the hold of a retired re-entry cycle
// whose publication intent an earlier daemon committed.
const productionHoldReentryRetiredIntent = "Publication is durably held because an earlier daemon committed this run's publication under a base-advance re-entry rule Freeside has retired, and this daemon cannot finish it. Inspect the run's pull request and disposition the run manually."

// retiredReentryCheckpoint is the checkpoint a retired cycle's block is
// recovered against. An earlier daemon may have blocked the cycle and stopped
// before finishing the task. That block named the authorizing checkpoint's
// import and evidence, except a failed re-entry verification, which showed
// the cycle's own evidence. That evidence is taken from the stored item, not
// re-derived: the cycle that produced it no longer runs, and the daemon's own
// item is trusted state.
func (w *productionPublicationWorkflow) retiredReentryCheckpoint(
	ctx context.Context, task productionPublicationTask, binding productionBinding,
) (productionVerificationCheckpoint, bool, error) {
	checkpoint, found, err := w.loadCheckpoint(ctx, task, binding)
	if err != nil || !found {
		return checkpoint, found, err
	}
	var current domain.AttentionItem
	err = w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		current, err = tx.GetAttentionItemRecord(ctx, task.blockedItemID())
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return checkpoint, true, nil
	}
	if err != nil {
		return productionVerificationCheckpoint{}, false, err
	}
	if current.Reason == productionBlockReentryVerification {
		checkpoint.Artifacts = current.EvidenceSnapshot
	}
	return checkpoint, true, nil
}

func derefOutcome(outcome *productionTaskOutcome) productionTaskOutcome {
	if outcome == nil {
		return productionTaskOutcome{}
	}
	return *outcome
}
