package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

// loadRemediationSourceTree reconstructs the prior reviewed candidate's tree
// identity from the daemon-authored verification checkpoint. Commit identity
// is deliberately insufficient here: every stage invocation receives a fresh
// commit date, so identical content normally reconstructs a different commit.
//
// checkoutDir is a checkout holding the request's base. Only the re-entry
// branch reads it, to rebuild the tree its checkpoint must name.
func (w *productionPublicationWorkflow) loadRemediationSourceTree(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	checkoutDir string,
) (string, error) {
	if binding.remediation == nil {
		return "", errors.Join(errRemediationSourceIdentity, domain.ErrParentKeyMismatch)
	}
	request := binding.remediation.request
	round, ok := remediationRoundForInvocation(task.RunID, task.ProducingInvocationID)
	if !ok || request.Round != round {
		return "", errors.Join(errRemediationSourceIdentity, domain.ErrParentKeyMismatch)
	}
	if task.Successor != nil && remediatesExternalFindings(*task.Successor, request.Round) {
		return w.loadReentryRemediationSourceTree(ctx, task, binding, request, checkoutDir)
	}
	if task.Successor != nil && task.Successor.AdmitsHandoffConflictRemediation(domain.RemediationInvocationIntent(request)) {
		// A handoff conflict's first remediation rebuilt the candidate on the
		// sealed tip, which the checkout holds as its base. It changed nothing
		// when its tree is the tip's: the head would propose no change.
		return w.reentryTreeSHA(ctx, checkoutDir, request.BaseSHA)
	}
	var (
		entry  store.QueueEntry
		legacy bool
	)
	sourceTaskKey := task.intentKey()
	sourceCheckpointKey := productionVerificationCheckpointKey(task.RunID, request.HeadSHA, "")
	var continuationVerification domain.InvocationID
	if task.Successor != nil && task.Successor.EffectiveOrigin() == domain.PublicationSuccessorRemediation {
		sourceCheckpointKey += "/continuation/" + task.Successor.CommandID
		if request.Round == task.Successor.ReviewRound-1 {
			if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
				r, _, err := tx.PublicationContinuationIntent(ctx, task.RunID, task.Successor.CommandID)
				if err != nil {
					return err
				}
				row, err := tx.GetOutbox(ctx, r.SourceTaskKey)
				if err != nil {
					return err
				}
				source, err := decodeProductionPublicationTask(row)
				if err != nil {
					return err
				}
				sourceTaskKey = signet.PublicationReevaluationKey(task.RunID, task.Successor.ReevaluationCommandID)
				sourceCheckpointKey = productionVerificationCheckpointKey(task.RunID, request.HeadSHA, task.Successor.ReevaluationCommandID)
				continuationVerification = domain.InvocationID(string(source.VerificationID) + "/reevaluation/" + task.Successor.ReevaluationCommandID)
				return nil
			}); err != nil {
				return "", errors.Join(errRemediationSourceIdentity, err)
			}
		}
	}
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetInbox(ctx, sourceCheckpointKey)
		if errors.Is(err, store.ErrNotFound) {
			entry, err = tx.GetInbox(ctx, "production-verification/"+string(task.RunID))
			legacy = err == nil
		}
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return "", errors.Join(errRemediationSourceIdentity, err)
	}
	if err != nil {
		return "", remediationSourceReadError(ctx, err)
	}
	if entry.Kind != productionVerificationCheckpointKind {
		return "", errors.Join(errRemediationSourceIdentity, domain.ErrParentKeyMismatch)
	}
	var checkpoint productionVerificationCheckpoint
	if err := strictjson.Decode(
		entry.Payload, &checkpoint, strictjson.TolerateInvalidUTF8, strictjson.NoLimit,
	); err != nil {
		return "", fmt.Errorf("decode remediation source checkpoint: %w",
			errors.Join(errRemediationSourceIdentity, err, domain.ErrParentKeyMismatch))
	}
	importDigest, err := digestJSON(checkpoint.Imported)
	if err != nil {
		return "", errors.Join(errRemediationSourceIdentity, err)
	}
	evidenceDigest, err := domain.ComputeEvidenceSnapshotDigest(checkpoint.Artifacts)
	if err != nil {
		return "", errors.Join(errRemediationSourceIdentity, err)
	}
	authorization := checkpoint.Authorization
	var (
		storedAuthorization domain.CandidateAuthorization
		sourceImage         domain.ProjectImage
	)
	err = w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		storedAuthorization, err = tx.GetCandidateAuthorization(ctx, authorization.ID)
		if err != nil {
			return err
		}
		sourceImage, err = tx.GetProjectImage(ctx, checkpoint.ProjectImage)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return "", errors.Join(errRemediationSourceIdentity, err)
	}
	if err != nil {
		return "", remediationSourceReadError(ctx, err)
	}
	versionMatches := !legacy && checkpoint.Version == productionVerificationVersion &&
		checkpoint.HeadSHA == request.HeadSHA
	if legacy {
		versionMatches = checkpoint.Version == productionVerificationVersionV1 && checkpoint.HeadSHA == ""
	}
	sourceVerificationValid := validRemediationSourceVerificationID(task.RunID, round, authorization.InvocationID)
	if continuationVerification != "" {
		sourceVerificationValid = authorization.InvocationID == continuationVerification
	}
	if task.Successor != nil && authorization.InvocationID == productionVerificationInvocationIDForProducer(task.RunID, task.Successor.FeedbackInvocationID) {
		sourceVerificationValid = true
	}
	if !versionMatches || checkpoint.TaskKey != sourceTaskKey ||
		checkpoint.Imported.CommitSHA != request.HeadSHA ||
		!validCommitSHA(checkpoint.Imported.TreeSHA) ||
		!importer.AllAdvisory(checkpoint.Imported.Findings, binding.profile.CandidateAutomationChanges) ||
		authorization.Validate() != nil ||
		authorization.ValidateAdvisoryStance(binding.profile.CandidateAutomationChanges) != nil ||
		authorization.Repo != binding.admission.Base.Repo ||
		authorization.BaseSHA != request.BaseSHA || authorization.HeadSHA != request.HeadSHA ||
		authorization.ImportResultDigest != importDigest ||
		authorization.EvidenceSnapshotDigest != evidenceDigest ||
		authorization.VerificationOutcome != domain.VerificationPassed ||
		!sourceVerificationValid ||
		!reflect.DeepEqual(storedAuthorization, authorization) ||
		sourceImage.ID != checkpoint.ProjectImage || sourceImage.Repository != authorization.Repo ||
		sourceImage.RepositoryID != binding.admission.Base.RepositoryID ||
		sourceImage.RecipeDigest != authorization.VerificationRecipeDigest {
		return "", errors.Join(errRemediationSourceIdentity, domain.ErrParentKeyMismatch)
	}
	for _, artifact := range checkpoint.Artifacts {
		if err := verifyArtifactBlob(w.artifacts, artifact); err != nil {
			// A transient open/read/close fault on a checkpoint blob is
			// operational and must stay retryable; only a digest mismatch or an
			// absent blob is a durable source-identity contradiction. Classify
			// like the adjacent checkpoint/authorization/image reads above.
			return "", fmt.Errorf("verify remediation source checkpoint artifact: %w",
				retryableOrTerminal(ctx, err, errRemediationSourceIdentity))
		}
	}
	return checkpoint.Imported.TreeSHA, nil
}

// loadReentryRemediationSourceTree is loadRemediationSourceTree for the
// remediation an external review cycle's first round started. That cycle
// verified its head as a re-entered cycle: under its own checkpoint kind,
// with no import account and no stored authorization. The tree the
// remediator's import is compared with is the one that checkpoint recorded,
// re-bound to the request and the run the way the ordinary checkpoint is.
//
// The ordinary checkpoint's outcome and tree are bound through its stored
// authorization. This one has none, so the row is trusted for neither. The
// outcome is read back from the cycle's verification report. The tree is held
// to the remediation input the request authenticates: it carries the
// base-to-head patch the remediator starts from, derived apart from the
// checkpoint, and the tree that patch rebuilds on the base in checkoutDir
// must be the recorded one.
func (w *productionPublicationWorkflow) loadReentryRemediationSourceTree(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	request remediationInvocationRequest,
	checkoutDir string,
) (string, error) {
	// The re-entered task's key: the cycle's head under the cycle's
	// publication, which the remediated task shares.
	key := reentryCheckpointKey(task.RunID, request.HeadSHA, task.Successor.PublicationID())
	var entry store.QueueEntry
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetInbox(ctx, key)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return "", errors.Join(errRemediationSourceIdentity, err)
	}
	if err != nil {
		return "", remediationSourceReadError(ctx, err)
	}
	if entry.Kind != productionReentryCheckpointKind {
		return "", errors.Join(errRemediationSourceIdentity, domain.ErrParentKeyMismatch)
	}
	var checkpoint productionReentryCheckpoint
	if err := strictjson.Decode(
		entry.Payload, &checkpoint, strictjson.TolerateInvalidUTF8, strictjson.NoLimit,
	); err != nil {
		return "", fmt.Errorf("decode re-entry remediation source checkpoint: %w",
			errors.Join(errRemediationSourceIdentity, err, domain.ErrParentKeyMismatch))
	}
	evidenceDigest, err := domain.ComputeEvidenceSnapshotDigest(checkpoint.Artifacts)
	if err != nil {
		return "", errors.Join(errRemediationSourceIdentity, err)
	}
	// An external review cycle evaluates its head itself, never a merge, so
	// the recorded tree is the head's. A checkpoint from before the tree was
	// recorded has no source identity to offer.
	if checkpoint.Version != productionReentryCheckpointVersion ||
		checkpoint.TaskKey != task.intentKey() ||
		checkpoint.BaseSHA != request.BaseSHA || checkpoint.HeadSHA != request.HeadSHA ||
		checkpoint.EvaluatedSHA != "" || !checkpoint.clean() ||
		!validCommitSHA(checkpoint.TreeSHA) ||
		checkpoint.EvidenceSnapshotDigest != evidenceDigest {
		return "", errors.Join(errRemediationSourceIdentity, domain.ErrParentKeyMismatch)
	}
	// The checkpoint is held to the image the cycle verified its head under,
	// as loadReentryCheckpoint holds it: the admitted image, or the rebuild
	// the run bound to that head (production_rebuild.go), which keeps the
	// admitted recipe. It is never held to an image it names itself: another
	// registered image of the same repository, with its own recipe, is
	// self-consistent and still not the one the cycle verified under.
	source := binding
	if err := w.resolveVerificationImage(ctx, &source, request.HeadSHA); err != nil {
		if errors.Is(err, domain.ErrParentKeyMismatch) {
			return "", errors.Join(errRemediationSourceIdentity, err)
		}
		return "", remediationSourceReadError(ctx, err)
	}
	if checkpoint.ProjectImage != source.verificationImage().ID ||
		checkpoint.RecipeDigest != binding.image.RecipeDigest {
		return "", errors.Join(errRemediationSourceIdentity, domain.ErrParentKeyMismatch)
	}
	for _, artifact := range checkpoint.Artifacts {
		if err := verifyArtifactBlob(w.artifacts, artifact); err != nil {
			return "", fmt.Errorf("verify re-entry remediation source checkpoint artifact: %w",
				retryableOrTerminal(ctx, err, errRemediationSourceIdentity))
		}
	}
	// The clean outcome is the verifier's to state, as loadReentryCheckpoint
	// holds it: read back from the cycle's own report, not from the row.
	raw, err := loadVerificationReport(
		w.artifacts, checkpoint.Artifacts, reentryVerificationInvocationID(*task.Successor))
	if err != nil {
		return "", fmt.Errorf("load re-entry remediation source report: %w",
			retryableOrTerminal(ctx, err, errRemediationSourceIdentity))
	}
	report, err := verify.ParseReport(raw)
	if err != nil || !checkpoint.reportAgrees(report, request.BaseSHA, request.HeadSHA, "") {
		return "", errors.Join(errRemediationSourceIdentity, err, domain.ErrParentKeyMismatch)
	}
	input, _, err := loadRemediationInput(w.artifacts, request.InputArtifactDigest)
	if err != nil {
		return "", fmt.Errorf("load re-entry remediation source input: %w",
			retryableOrTerminal(ctx, err, errRemediationSourceIdentity))
	}
	if input.BaseSHA != request.BaseSHA || input.HeadSHA != request.HeadSHA {
		return "", errors.Join(errRemediationSourceIdentity, domain.ErrParentKeyMismatch)
	}
	scratch, err := os.MkdirTemp(w.workDir, ".remediation-source-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // daemon-owned scratch
	_, rebuilt, err := patchedTree(ctx, scratch, checkoutDir, request.BaseSHA, input.CandidatePatchBase64)
	if err != nil {
		return "", fmt.Errorf("rebuild re-entry remediation source tree: %w",
			retryableOrTerminal(ctx, err, errRemediationSourceIdentity))
	}
	if rebuilt != checkpoint.TreeSHA {
		return "", errors.Join(errRemediationSourceIdentity, domain.ErrParentKeyMismatch)
	}
	return checkpoint.TreeSHA, nil
}

// retryableOrTerminal keeps a transient open/read/close fault on a
// daemon-authored checkpoint blob retryable, while a deterministic contradiction
// (a digest mismatch, or an absent blob the store reports non-operationally) is
// terminalized with the caller's durable sentinel. Context cancellation stays
// the context error. Callers must not force every blob fault to a permanent
// dispute: a transient I/O blip would otherwise stop an otherwise valid run.
func retryableOrTerminal(ctx context.Context, err error, sentinel error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if productionPublicationRetryableFailure(err) {
		return err
	}
	return errors.Join(sentinel, err)
}

func remediationSourceReadError(ctx context.Context, err error) error {
	return retryableOrTerminal(ctx, err, errRemediationSourceIdentity)
}

func validRemediationSourceVerificationID(
	runID domain.RunID,
	currentRound int,
	invocationID domain.InvocationID,
) bool {
	if invocationID == productionVerificationInvocationID(runID) {
		return true
	}
	for round := 1; round < currentRound; round++ {
		if invocationID == productionVerificationInvocationIDForProducer(
			runID, remediationInvocationID(runID, round),
		) {
			return true
		}
	}
	return false
}
