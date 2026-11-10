package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// RecordExecutionExport selects the persistence boundary from the immutable
// admission that owns the export, not from the daemon's current mode. A
// recovered unattended attempt therefore cannot be downgraded to an
// export-only write after an attended-mode restart.
//
// A production-arm export mints its publication task and is never
// collected, so once that write commits, replay.ClaudeUsage goes to usage
// (nil drops it with a warning on logger). Export-only arms report nothing
// here: their terminal is collected later and reports the capture then.
func RecordExecutionExport(
	ctx context.Context,
	st *store.Store,
	executionExport domain.ExecutionExport,
	replay ProductionReplay,
	usage ClaudeUsageSink,
	logger *slog.Logger,
) error {
	if st == nil {
		return errors.New("record execution export: nil store")
	}
	var admission domain.ExecutionAdmission
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		admission, err = tx.GetExecutionAdmissionRecord(ctx, executionExport.InvocationID)
		return err
	}); err != nil {
		return err
	}
	var operatorFeedback bool
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		operatorFeedback, err = authenticateOperatorFeedbackAttempt(
			ctx, tx, executionExport.InvocationID, admission.RunID, admission.StageID,
		)
		return err
	}); err != nil {
		return fmt.Errorf("authenticate operator-feedback export: %w", err)
	}
	if operatorFeedback {
		var publishedParent bool
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			_, _, found, err := tx.FeedbackPublicationParent(ctx, executionExport.InvocationID)
			publishedParent = found
			return err
		}); err != nil {
			return err
		}
		if publishedParent && admission.OperatingMode == domain.ModeUnattended {
			return recordProductionExportAndReportUsage(ctx, st, executionExport, replay, usage, logger)
		}
		// A feedback invocation resumes implementation from durable input, but
		// its first export is not itself a publication candidate. Recording it
		// export-only lets the attempt close without minting a task that would
		// incorrectly reuse the original production invocation's identity.
		return recordExportOnly(ctx, st, executionExport, replay.Usage)
	}
	switch admission.OperatingMode {
	case domain.ModeAttendedDev:
		return recordExportOnly(ctx, st, executionExport, replay.Usage)
	case domain.ModeUnattended:
		// An unattended specification run produces an export the same way, but its
		// output is a typed specification published outside the repo channel, and
		// it mints no publication task: it is a separate run whose ownership
		// marker is inv-specify-<run>-<iter>, not the inv-implement-<run> marker
		// loadProductionRequest authenticates below, so that call would fail
		// before any legacy fallback. Record it export-only, mirroring the
		// attended branch. Without this arm a debris-tolerant specification import
		// succeeds and then retries the export-record step forever (issue #768).
		if IsSpecificationInvocationIdentity(
			executionExport.InvocationID, admission.RunID, admission.StageID,
		) {
			return recordExportOnly(ctx, st, executionExport, replay.Usage)
		}
		// A discussion on a specification card runs under the same
		// specification run and stage and has the same shape: an export with no
		// publication task. Its ID names a command, not the run, so the arm
		// above cannot see it; its marker decides instead (issue #2991).
		var discussion bool
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			discussion, err = authenticateSpecificationDiscussionExport(
				ctx, tx, executionExport.InvocationID, admission.RunID, admission.StageID,
			)
			return err
		}); err != nil {
			return fmt.Errorf("authenticate specification-discussion export: %w", err)
		}
		if discussion {
			return recordExportOnly(ctx, st, executionExport, replay.Usage)
		}
		var run domain.Run
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			run, err = tx.GetRun(ctx, admission.RunID)
			return err
		}); err != nil {
			return err
		}
		request, err := (&productionPublicationWorkflow{store: st}).loadProductionRequest(ctx, run)
		if err != nil {
			return err
		}
		if request.Legacy {
			return recordExportOnly(ctx, st, executionExport, replay.Usage)
		}
		return recordProductionExportAndReportUsage(ctx, st, executionExport, replay, usage, logger)
	}
	return fmt.Errorf("execution admission %q has invalid operating mode %q: %w",
		admission.InvocationID, admission.OperatingMode, domain.ErrInvalidOperatingMode)
}

// authenticateSpecificationDiscussionExport reports whether invocationID is a
// specification discussion, whose unattended export records export-only. It
// decides from the invocation's own outbox marker, never the ID prefix: a
// discussion ID names a command, not a run, so only the marker binds it to
// the admitted run. A KindSpecificationDiscussionRequested marker is
// re-checked with AuthenticateSpecificationDiscussionTransition against the
// admitted run and stage, the check admission ran, and one that fails it
// returns true with the error, so the caller records nothing and never falls
// through to another arm. An absent marker or another kind returns false.
func authenticateSpecificationDiscussionExport(
	ctx context.Context,
	tx *store.ReadTx,
	invocationID domain.InvocationID,
	runID domain.RunID,
	stageID domain.StageID,
) (bool, error) {
	entry, err := tx.GetOutbox(ctx, string(invocationID))
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if entry.Kind != KindSpecificationDiscussionRequested {
		return false, nil
	}
	return true, AuthenticateSpecificationDiscussionTransition(ctx, tx, entry, runID, stageID)
}

// recordProductionExportAndReportUsage commits the production export and
// then reports the writer turn's capture. Reporting waits for the commit,
// outside the write, because the sink reads the store; a failed write
// reports nothing. A convergent re-record reports the same capture again,
// which the sink drops as no newer than the one it holds.
func recordProductionExportAndReportUsage(
	ctx context.Context,
	st *store.Store,
	executionExport domain.ExecutionExport,
	replay ProductionReplay,
	usage ClaudeUsageSink,
	logger *slog.Logger,
) error {
	if err := RecordProductionExecutionExport(ctx, st, executionExport, replay); err != nil {
		return err
	}
	reportClaudeUsageCapture(ctx, st, usage, logger, executionExport.InvocationID, replay.ClaudeUsage)
	return nil
}

// recordExportOnly records an export that mints no publication task, with
// its usage in the same write. The attended, specification, and discussion
// invocations it serves are collected later and append the same measurements
// again, which converges on the rows written here.
func recordExportOnly(
	ctx context.Context,
	st *store.Store,
	executionExport domain.ExecutionExport,
	usage []exec.UsageMeasurement,
) error {
	return st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.RecordExecutionExport(ctx, executionExport); err != nil {
			return err
		}
		_, err := tx.AppendUsageObservations(ctx, executionExport.InvocationID, usage)
		return err
	})
}

// RecordProductionExecutionExport atomically commits an unattended export and
// the publication task that owns every byte needed to replay it. A crash can
// therefore expose neither row or both rows, never an ExecutionExport whose
// completion still depends on the Claude driver's private state directory.
func RecordProductionExecutionExport(
	ctx context.Context,
	st *store.Store,
	executionExport domain.ExecutionExport,
	replay ProductionReplay,
) error {
	if st == nil {
		return errors.New("record production execution export: nil store")
	}
	if replay.InvocationID != executionExport.InvocationID ||
		replay.ObservedBaseSHA != executionExport.ObservedBaseSHA ||
		replay.HeadSHA != executionExport.HeadSHA ||
		replay.ManifestDigest != executionExport.ManifestDigest ||
		!sameOptionalDigest(replay.EvidenceManifestDigest, executionExport.EvidenceManifestDigest) ||
		(replay.CommitPlanDigest != nil) != executionExport.CommitPlanPresent ||
		!productionReplayPrecedesCompletion(replay.ImportOptions.CommitDate, executionExport.RecordedAt) {
		return fmt.Errorf("production replay disagrees with execution export: %w", domain.ErrParentKeyMismatch)
	}
	if err := validateProductionReplayRecord(replay); err != nil {
		return err
	}

	var (
		admission         domain.ExecutionAdmission
		run               domain.Run
		publication       ProductionPublication
		remediation       *remediationInvocationRequest
		transition        authenticatedRemediationTransition
		previousTask      *productionPublicationTask
		alreadyReplaced   bool
		legacyNoop        bool
		continuationFirst bool
		successor         *domain.PublicationSuccessor
	)
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		admission, err = tx.GetExecutionAdmissionRecord(ctx, executionExport.InvocationID)
		if err != nil {
			return err
		}
		run, err = tx.GetRun(ctx, admission.RunID)
		if err != nil {
			return err
		}
		if domain.HasOperatorFeedbackInvocationPrefix(executionExport.InvocationID) {
			entry, err := tx.GetOutbox(ctx, string(executionExport.InvocationID))
			if err != nil {
				return err
			}
			_, inherited, err := authenticateOperatorFeedbackTransition(ctx, tx, entry, run.ID, admission.StageID)
			if err != nil {
				return err
			}
			publication = inherited
			successor, err = preparePublicationSuccessor(ctx, tx, executionExport.InvocationID)
			return err
		}
		if round, ok := remediationRoundForInvocation(run.ID, executionExport.InvocationID); ok {
			entry, err := tx.GetOutbox(ctx, string(executionExport.InvocationID))
			if err != nil {
				return err
			}
			verified, err := authenticateRemediationInvocationTransition(
				ctx, tx, entry, run.ID, admission.StageID,
			)
			if err != nil || verified.request.Round != round {
				return errors.Join(err, domain.ErrParentKeyMismatch)
			}
			request := verified.request
			latestReview, err := tx.LatestReviewRecord(ctx, run.ID)
			if err != nil || latestReview.InvocationID != request.ReviewInvocationID ||
				latestReview.Round != request.Round || !verified.admitsReviewOutcome(latestReview.Outcome) {
				return errors.Join(err, domain.ErrParentKeyMismatch)
			}
			remediation, transition = &request, verified
			taskEntry, err := productionTaskForInvocation(ctx, tx, run.ID, executionExport.InvocationID)
			if errors.Is(err, store.ErrNotFound) && request.SuccessorPublicationID != "" {
				cycle, readErr := tx.GetPublicationSuccessor(ctx, run.ID, request.SuccessorPublicationID)
				if readErr != nil {
					return readErr
				}
				// A remediation continuation and a handoff conflict each start
				// their cycle with a remediation on the round before it, which
				// creates the cycle's task.
				starts := cycle.EffectiveOrigin() == domain.PublicationSuccessorRemediation || cycle.RemediatesHandoffConflict()
				if starts && request.Round == cycle.ReviewRound-1 && cycle.AllowsRemediation(domain.RemediationInvocationIntent(request)) {
					publication, successor, continuationFirst = verified.publication, &cycle, true
					return nil
				}
			}
			if err != nil {
				return err
			}
			current, err := decodeProductionPublicationTask(taskEntry)
			if err != nil {
				return err
			}
			if current.reentersInPlace() {
				// A re-entered cycle has no publication of its own and no
				// producer. The one remediation it starts is its external
				// review first round's; the new row inherits the producer's
				// publication the request already proved.
				if !remediatesExternalFindings(*current.Successor, round) {
					return domain.ErrImmutableTransition
				}
				publication, successor = verified.publication, current.Successor
				previous := current
				previousTask = &previous
				return nil
			}
			if !reflect.DeepEqual(current.Publication, verified.publication) {
				return domain.ErrParentKeyMismatch
			}
			publication = verified.publication
			successor = current.Successor
			switch current.ProducingInvocationID {
			case executionExport.InvocationID:
				alreadyReplaced = true
				legacyNoop = current.LegacyRemediationNoop
			case productionInvocationID(run.ID):
				previous := current
				previousTask = &previous
			default:
				if current.Successor != nil && current.ProducingInvocationID == current.Successor.FeedbackInvocationID {
					previous := current
					previousTask = &previous
					return nil
				}
				if priorRound, ok := remediationRoundForInvocation(run.ID, current.ProducingInvocationID); !ok || priorRound >= round {
					return domain.ErrImmutableTransition
				}
				previous := current
				previousTask = &previous
			}
			return nil
		}
		entry, err := tx.GetOutbox(ctx, string(productionInvocationID(run.ID)))
		if err != nil {
			return err
		}
		request, err := authenticateProductionMarker(entry, run.ID)
		if err != nil {
			return err
		}
		if request.Legacy {
			return fmt.Errorf("legacy production invocation %q has no publication authority: %w",
				executionExport.InvocationID, domain.ErrParentKeyMismatch)
		}
		publication = request.Publication
		return nil
	}); err != nil {
		return err
	}
	if remediation != nil {
		if admission.StageID != remediation.StageID ||
			remediation.BaseSHA != replay.ObservedBaseSHA {
			return fmt.Errorf("remediation export disagrees with its request: %w", domain.ErrParentKeyMismatch)
		}
		if previousTask != nil && (previousTask.HeadSHA != remediation.HeadSHA ||
			previousTask.admittedBaseSHA() != remediation.BaseSHA) {
			return fmt.Errorf("remediation export does not supersede the current candidate: %w",
				domain.ErrParentKeyMismatch)
		}
	} else if successor != nil {
		if executionExport.InvocationID != successor.FeedbackInvocationID ||
			admission.StageID != operatorFeedbackStageID(executionExport.InvocationID) {
			return domain.ErrParentKeyMismatch
		}
	} else if admission.StageID != productionStageID(run.ID) ||
		executionExport.InvocationID != productionInvocationID(run.ID) {
		return fmt.Errorf("production export disagrees with its initial stage: %w", domain.ErrParentKeyMismatch)
	}
	artifacts := make([]domain.Digest, 0, len(replay.Evidence.Entries))
	for _, entry := range replay.Evidence.Entries {
		artifacts = append(artifacts, domain.Digest(entry.Digest))
	}
	task := productionPublicationTask{
		Version: productionPublicationTaskVersion, RunID: run.ID, ProjectID: run.ProjectID,
		ProducingInvocationID: executionExport.InvocationID,
		LegacyRemediationNoop: legacyNoop,
		VerificationID: productionVerificationInvocationIDForProducer(
			run.ID, executionExport.InvocationID),
		PublicationID: domain.ProductionPublicationInvocationID(run.ID),
		HeadSHA:       executionExport.HeadSHA, Artifacts: artifacts,
		Replay: replay, Publication: publication,
		Summary: fmt.Sprintf("Imported candidate %s over base %s.",
			executionExport.HeadSHA, executionExport.ObservedBaseSHA),
	}
	if successor != nil {
		task.Successor = successor
		task.PublicationID = successor.PublicationID()
	}
	if err := task.validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(task)
	if err != nil {
		return err
	}
	w := &productionPublicationWorkflow{store: st}
	return w.store.Write(ctx, func(tx *store.WriteTx) error {
		currentAdmission, err := tx.GetExecutionAdmissionRecord(ctx, task.ProducingInvocationID)
		if err != nil {
			return err
		}
		currentRun, err := tx.GetRun(ctx, task.RunID)
		if err != nil {
			return err
		}
		expectedStage, stageFound := productionStageForInvocation(currentRun, task.ProducingInvocationID)
		if currentAdmission.ID != admission.ID || currentAdmission.RunID != task.RunID ||
			!stageFound || currentAdmission.StageID != expectedStage.ID {
			return fmt.Errorf("production publication admission disagrees with task: %w",
				domain.ErrParentKeyMismatch)
		}
		if currentRun.ProjectID != task.ProjectID {
			return fmt.Errorf("production publication run disagrees with task: %w",
				domain.ErrParentKeyMismatch)
		}
		resolvedPolicy, err := tx.GetResolvedPolicy(ctx, task.RunID)
		if err != nil {
			return err
		}
		if currentAdmission.TrustProfileDigest == nil {
			return fmt.Errorf("unattended production admission has no trust profile: %w",
				domain.ErrParentKeyMismatch)
		}
		profile, err := tx.GetTrustProfile(ctx, *currentAdmission.TrustProfileDigest)
		if err != nil {
			return err
		}
		if err := validateProductionReplayOptions(productionBinding{
			run: currentRun, admission: currentAdmission,
			resolvedPolicy: resolvedPolicy, profile: profile, replay: task.Replay,
		}, task.Publication); err != nil {
			return err
		}
		if err := tx.RecordExecutionExportRecord(ctx, executionExport); err != nil {
			return err
		}
		// An unattended export is never collected, so this write is the only
		// one that records its usage. It precedes the open-execution fence
		// because a late export's spend was still incurred.
		if _, err := tx.AppendUsageObservations(
			ctx, executionExport.InvocationID, replay.Usage,
		); err != nil {
			return err
		}
		if err := requireTaskExecutionOpen(ctx, &tx.ReadTx, task.RunID); err != nil {
			if workflowAdvancementFenced(err) {
				return nil // Retain the late export without publication authority.
			}
			return err
		}
		if task.Successor != nil {
			if err := tx.RecordPublicationSuccessor(ctx, *task.Successor); err != nil {
				return err
			}
		}
		reservation, err := publish.NewReservation(task.PublicationID, task.RunID)
		if err != nil {
			return err
		}
		if err := publish.ClaimInvocation(ctx, tx, reservation); err != nil {
			return err
		}
		key := task.intentKey()
		if remediation == nil {
			entry, _, err := tx.EnqueueOutbox(
				ctx, key, KindProductionPublicationRequested, payload)
			if err != nil {
				return err
			}
			if entry.Kind != KindProductionPublicationRequested || !bytes.Equal(entry.Payload, payload) {
				return fmt.Errorf("production publication task disagrees with stored row: %w",
					domain.ErrImmutableTransition)
			}
			return nil
		}
		requestEntry, err := tx.GetOutbox(ctx, string(remediation.InvocationID))
		if err != nil {
			return err
		}
		currentRequest, err := decodeRemediationRequest(requestEntry)
		if err != nil || !reflect.DeepEqual(currentRequest, *remediation) || !requestEntry.Dispatched() {
			return errors.Join(err, domain.ErrParentKeyMismatch)
		}
		latestReview, err := tx.LatestReviewRecord(ctx, task.RunID)
		if err != nil || latestReview.InvocationID != remediation.ReviewInvocationID ||
			latestReview.Round != remediation.Round || !transition.admitsReviewOutcome(latestReview.Outcome) {
			return errors.Join(err, domain.ErrParentKeyMismatch)
		}
		currentEntry, err := tx.GetOutbox(ctx, key)
		if continuationFirst {
			if !errors.Is(err, store.ErrNotFound) {
				return errors.Join(err, domain.ErrImmutableTransition)
			}
			entry, _, err := tx.EnqueueOutbox(ctx, key, KindProductionPublicationRequested, payload)
			if err != nil || entry.Kind != KindProductionPublicationRequested || !bytes.Equal(entry.Payload, payload) {
				return errors.Join(err, domain.ErrImmutableTransition)
			}
			return nil
		}
		if err != nil {
			return err
		}
		if alreadyReplaced {
			if currentEntry.Kind != KindProductionPublicationRequested ||
				!bytes.Equal(currentEntry.Payload, payload) {
				return domain.ErrImmutableTransition
			}
			return nil
		}
		if previousTask == nil || currentEntry.Dispatched() {
			return domain.ErrImmutableTransition
		}
		previousPayload, err := json.Marshal(previousTask)
		if err != nil || !bytes.Equal(currentEntry.Payload, previousPayload) {
			return errors.Join(err, domain.ErrImmutableTransition)
		}
		if _, promoted, err := tx.PromoteOutbox(
			ctx, key, KindProductionPublicationRequested, productionPublicationSupersedingKind,
			previousPayload, previousPayload,
		); err != nil || !promoted {
			return errors.Join(err, domain.ErrImmutableTransition)
		}
		entry, promoted, err := tx.PromoteOutbox(
			ctx, key, productionPublicationSupersedingKind, KindProductionPublicationRequested,
			previousPayload, payload,
		)
		if err != nil || !promoted || entry.Kind != KindProductionPublicationRequested ||
			!bytes.Equal(entry.Payload, payload) {
			return errors.Join(err, domain.ErrImmutableTransition)
		}
		return nil
	})
}
