package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// hasQueuedCompletion reports whether the durable publication task has
// already accepted ownership of this completed invocation. Once that task is
// stored, reconciliation must not depend on the producing driver's private
// state: SQLite plus the artifact closure is the supported recovery frontier.
func (w *productionPublicationWorkflow) hasQueuedCompletion(
	ctx context.Context,
	run domain.Run,
	invocationID domain.InvocationID,
) (bool, error) {
	var entry store.QueueEntry
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		entry, err = productionTaskForInvocation(ctx, tx, run.ID, invocationID)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		// No task row: either none was ever written, or an operator repaired
		// an unreadable one by removing it. Absence proves the hold a task
		// notice describes has ended, and the pending scan can no longer
		// reach that row to retire it.
		return false, releaseProductionQuarantine(
			ctx, w.store, w.attention, productionTaskQuarantinePrefix, run.ID)
	}
	if err != nil {
		return false, err
	}
	task, err := decodeProductionPublicationTask(entry)
	if err != nil {
		// The acceptance scan reaches this row too, so it needs the pending
		// scan's hold: an unreadable task is a durable completion this daemon
		// cannot read, which is a reason to leave the attempt alone, not to
		// re-collect it or end the pass.
		if holdErr := w.holdUnreadableTask(ctx, run, err); holdErr != nil {
			return false, holdErr
		}
		return true, nil
	}
	if task.RunID != run.ID || task.ProjectID != run.ProjectID {
		return false, fmt.Errorf("queued production completion disagrees with run: %w",
			domain.ErrParentKeyMismatch)
	}
	if task.ProducingInvocationID != invocationID {
		if err := w.store.Read(ctx, func(tx *store.ReadTx) error { return authenticateTaskSuccessor(ctx, tx, task) }); err != nil {
			return false, err
		}
		currentRound, currentIsRemediation := remediationRoundForInvocation(
			run.ID, task.ProducingInvocationID)
		priorRound, priorIsRemediation := remediationRoundForInvocation(run.ID, invocationID)
		priorIsInitial := invocationID == productionInvocationID(run.ID)
		priorIsFeedback := task.Successor != nil && invocationID == task.Successor.FeedbackInvocationID
		if currentIsRemediation && (priorIsInitial || priorIsFeedback || priorIsRemediation && priorRound < currentRound) {
			var (
				admission domain.ExecutionAdmission
				exported  domain.ExecutionExport
			)
			if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
				var err error
				admission, err = tx.GetExecutionAdmissionRecord(ctx, invocationID)
				if err != nil {
					return err
				}
				exported, err = tx.GetExecutionExportRecord(ctx, invocationID)
				return err
			}); err != nil {
				return false, err
			}
			stage, ok := productionStageForInvocation(run, invocationID)
			if !ok || admission.RunID != run.ID || admission.StageID != stage.ID ||
				exported.InvocationID != invocationID || exported.ObservedBaseSHA != task.Replay.ObservedBaseSHA {
				return false, domain.ErrParentKeyMismatch
			}
			return true, nil
		}
		round, remediation := remediationRoundForInvocation(run.ID, invocationID)
		if !remediation {
			return false, fmt.Errorf("queued production completion disagrees with run: %w",
				domain.ErrParentKeyMismatch)
		}
		var requestEntry store.QueueEntry
		if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			requestEntry, err = tx.GetOutbox(ctx, string(invocationID))
			return err
		}); err != nil {
			return false, err
		}
		request, err := decodeRemediationRequest(requestEntry)
		if err != nil || !requestEntry.Dispatched() || request.Round != round ||
			task.HeadSHA != request.HeadSHA {
			return false, errors.Join(err, domain.ErrParentKeyMismatch)
		}
		feedbackProducer := task.Successor != nil && task.ProducingInvocationID == task.Successor.FeedbackInvocationID
		if priorRound, prior := remediationRoundForInvocation(run.ID, task.ProducingInvocationID); !feedbackProducer && task.ProducingInvocationID != productionInvocationID(run.ID) && (!prior || priorRound >= round) {
			return false, domain.ErrImmutableTransition
		}
		return false, nil
	}
	if entry.Dispatched() {
		// The caller read the inbox in an earlier transaction, and the lane
		// commits the terminal and dispatches the task in two of its own. Since
		// the lane runs on its own loop (issue #425), this scan can read the
		// inbox before the terminal commits and the outbox after the dispatch,
		// which is a publication completing beside it, not a contradiction. The
		// terminal write happens-before the dispatch, so re-reading the inbox
		// here settles it: still absent proves the real violation.
		recorded, err := w.hasRecordedTerminal(ctx, invocationID)
		if err != nil {
			return false, err
		}
		if !recorded {
			retired, err := w.retiredByHandoffConflict(ctx, task)
			if err != nil {
				return false, err
			}
			if !retired {
				return false, fmt.Errorf("production publication task dispatched without a terminal record: %w",
					domain.ErrImmutableTransition)
			}
		}
	}
	// The acceptance scan runs in every operating mode, while the pending
	// scan that also releases this notice returns early under a hold-only
	// publication lane. Retiring it here too is what keeps a repaired task
	// from leaving an open notice behind in attended_dev.
	return true, releaseProductionQuarantine(
		ctx, w.store, w.attention, productionTaskQuarantinePrefix, run.ID)
}

// retiredByHandoffConflict reports whether a sealed handoff_conflict
// authority ended the task: the write that seals it marks the conflicted
// task's row done without a terminal, because the cycle it starts carries the
// run on and records its own (issue #2646).
func (w *productionPublicationWorkflow) retiredByHandoffConflict(
	ctx context.Context, task productionPublicationTask,
) (bool, error) {
	retired := false
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		chain, err := tx.PublicationSuccessorChain(ctx, task.RunID)
		for _, sealed := range chain {
			retired = retired || (sealed.RemediatesHandoffConflict() &&
				sealed.PredecessorItemID == task.readyItemID() && sealed.Reentry.HeadSHA == task.HeadSHA)
		}
		return err
	})
	return retired, err
}

// hasRecordedTerminal reports whether this invocation's terminal row exists.
// Presence is the whole question: whether the row authenticates against the
// durable task is authenticatesTerminal's, on the pass that reads it as the
// recorded terminal.
func (w *productionPublicationWorkflow) hasRecordedTerminal(
	ctx context.Context, invocationID domain.InvocationID,
) (bool, error) {
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetInbox(ctx, string(invocationID))
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// authenticatesTerminal proves a completed terminal from the SQLite-backed
// publication task and immutable export. Once publication owns completion,
// reconciliation must not call back into private driver state merely to
// authenticate history it has already durably accepted.
func (w *productionPublicationWorkflow) authenticatesTerminal(
	ctx context.Context,
	run domain.Run,
	terminal productionTerminalRecord,
) (bool, error) {
	var (
		entry           store.QueueEntry
		admission       domain.ExecutionAdmission
		executionExport domain.ExecutionExport
	)
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		entry, err = productionTaskForInvocation(ctx, tx, run.ID, terminal.InvocationID)
		if err != nil {
			return err
		}
		admission, err = tx.GetExecutionAdmissionRecord(ctx, terminal.InvocationID)
		if err != nil {
			return err
		}
		executionExport, err = tx.GetExecutionExportRecord(ctx, terminal.InvocationID)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		// Any of the three lookups may be the missing one, and only an absent
		// task row proves a task notice's hold has ended: a missing admission
		// or export record must not be read as a repair.
		return false, w.releaseTaskQuarantineIfRowAbsent(ctx, run)
	}
	if err != nil {
		return false, err
	}
	task, err := decodeProductionPublicationTask(entry)
	if err != nil {
		// The acceptance scan reaches this row too, so it needs the pending
		// scan's hold: an unreadable task is a durable completion this daemon
		// cannot read, which is a reason to leave the attempt alone, not to
		// re-collect it or end the pass.
		if holdErr := w.holdUnreadableTask(ctx, run, err); holdErr != nil {
			return false, holdErr
		}
		return true, nil
	}
	if err := validateProductionPublicationCompletion(run, task, terminal); err != nil {
		return false, err
	}
	stage, stageFound := productionStageForInvocation(run, task.ProducingInvocationID)
	if admission.RunID != task.RunID || !stageFound || admission.StageID != stage.ID ||
		executionExport.AdmissionID != admission.ID ||
		executionExport.InvocationID != task.ProducingInvocationID ||
		executionExport.HeadSHA != task.HeadSHA ||
		task.Replay.ObservedBaseSHA != admission.Base.BaseSHA ||
		task.Replay.ManifestDigest != executionExport.ManifestDigest ||
		!sameOptionalDigest(task.Replay.EvidenceManifestDigest, executionExport.EvidenceManifestDigest) ||
		(task.Replay.CommitPlanDigest != nil) != executionExport.CommitPlanPresent ||
		!productionReplayPrecedesCompletion(task.Replay.ImportOptions.CommitDate, executionExport.RecordedAt) {
		return false, fmt.Errorf("production terminal disagrees with durable publication task: %w",
			domain.ErrParentKeyMismatch)
	}
	return true, releaseProductionQuarantine(
		ctx, w.store, w.attention, productionTaskQuarantinePrefix, run.ID)
}

// ProductionPublicationIdentity names the authenticated owners of the final
// production publication records. The producing invocation owns the terminal;
// the dedicated publication invocation owns the publication milestones.
type ProductionPublicationIdentity struct {
	ProducingInvocationID   domain.InvocationID
	PublicationInvocationID domain.InvocationID
}

// ProductionPublicationCompletion authenticates the selected run's final
// publication-task and terminal records and returns both exact invocation
// owners after completion. It is the read-only completion boundary used by
// supervision: absence or a pending task is ordinary incompletion, while a
// dispatched task with absent or divergent terminal authority fails closed.
// An in-place re-entry has no terminal: its authenticated ready binding proves
// completion, while a cycle that finishes without a ready item is incomplete.
func ProductionPublicationCompletion(
	ctx context.Context, tx *store.ReadTx, run domain.Run,
) (ProductionPublicationIdentity, bool, error) {
	entry, err := currentProductionTaskEntry(ctx, tx, run.ID)
	return productionPublicationCompletion(ctx, tx, run, entry, err, false)
}

func productionPublicationCompletion(ctx context.Context, tx *store.ReadTx, run domain.Run, entry store.QueueEntry, entryErr error, allowSuperseded bool) (ProductionPublicationIdentity, bool, error) {
	err := entryErr
	if errors.Is(err, store.ErrNotFound) {
		return ProductionPublicationIdentity{}, false, nil
	}
	if err != nil {
		return ProductionPublicationIdentity{}, false, err
	}
	task, err := decodeProductionPublicationTask(entry)
	if err != nil {
		return ProductionPublicationIdentity{}, false, err
	}
	if task.RunID != run.ID || task.ProjectID != run.ProjectID {
		return ProductionPublicationIdentity{}, false, fmt.Errorf(
			"production publication task disagrees with selected run: %w",
			domain.ErrParentKeyMismatch,
		)
	}
	if !entry.Dispatched() {
		return ProductionPublicationIdentity{}, false, nil
	}
	ready, readyErr := tx.GetAttentionItemRecord(ctx, task.readyItemID())
	if readyErr == nil && ready.Status == domain.StatusSuperseded && !allowSuperseded {
		return ProductionPublicationIdentity{}, false, nil
	} else if readyErr != nil && !errors.Is(readyErr, store.ErrNotFound) {
		return ProductionPublicationIdentity{}, false, readyErr
	}
	if task.reentersInPlace() {
		if errors.Is(readyErr, store.ErrNotFound) {
			return ProductionPublicationIdentity{}, false, nil
		}
		if err := authenticateTaskSuccessor(ctx, tx, task); err != nil {
			return ProductionPublicationIdentity{}, false, err
		}
		// The store re-gates the binding against the sealed cycle and its
		// predecessor's publication proof. A finished cycle with a ready item
		// must have that proof; missing nested authority is not incompletion.
		binding, err := tx.GetReadyItemPRBinding(ctx, task.readyItemID())
		if err != nil {
			return ProductionPublicationIdentity{}, false, err
		}
		if binding.RunID != task.RunID || binding.PublicationInvocationID != task.PublicationID ||
			binding.HeadSHA != task.HeadSHA {
			return ProductionPublicationIdentity{}, false, fmt.Errorf(
				"re-entered publication binding disagrees with completed task: %w", domain.ErrParentKeyMismatch)
		}
		return ProductionPublicationIdentity{
			ProducingInvocationID:   binding.ProducingInvocationID,
			PublicationInvocationID: task.PublicationID,
		}, true, nil
	}
	terminalEntry, err := tx.GetInbox(ctx, string(task.ProducingInvocationID))
	if errors.Is(err, store.ErrNotFound) {
		return ProductionPublicationIdentity{}, false, fmt.Errorf(
			"dispatched production publication task has no terminal record: %w",
			domain.ErrImmutableTransition,
		)
	}
	if err != nil {
		return ProductionPublicationIdentity{}, false, err
	}
	terminal, err := decodeProductionTerminal(terminalEntry, run)
	if err != nil {
		return ProductionPublicationIdentity{}, false, err
	}
	if err := validateProductionPublicationCompletion(run, task, terminal); err != nil {
		return ProductionPublicationIdentity{}, false, err
	}
	return ProductionPublicationIdentity{
		ProducingInvocationID:   task.ProducingInvocationID,
		PublicationInvocationID: task.PublicationID,
	}, true, nil
}

// AuthenticateProductionPublicationReady binds publication_ready to the
// completed production task and its durable ready-item PR identity. The caller
// supplies the completion identity returned by ProductionPublicationCompletion
// from the same read transaction.
func AuthenticateProductionPublicationReady(
	ctx context.Context,
	tx *store.ReadTx,
	run domain.Run,
	observation domain.RunObservation,
	identity ProductionPublicationIdentity,
) (bool, error) {
	ready := false
	for _, milestone := range observation.Milestones {
		if milestone.Kind == domain.MilestonePublicationReady &&
			milestone.InvocationID != nil &&
			*milestone.InvocationID == identity.PublicationInvocationID {
			ready = true
			break
		}
	}
	if !ready {
		return false, nil
	}
	itemID := domain.ProductionReadyItemID(run.ID)
	if identity.PublicationInvocationID != domain.ProductionPublicationInvocationID(run.ID) {
		successor, err := tx.GetPublicationSuccessor(ctx, run.ID, identity.PublicationInvocationID)
		if err != nil {
			return false, err
		}
		itemID = successor.ReadyItemID()
	}
	binding, err := tx.GetReadyItemPRBinding(ctx, itemID)
	if err != nil {
		return false, fmt.Errorf("authenticate publication-ready milestone: %w", err)
	}
	if binding.RunID != run.ID ||
		binding.ProducingInvocationID != identity.ProducingInvocationID ||
		binding.PublicationInvocationID != identity.PublicationInvocationID {
		return false, fmt.Errorf(
			"publication-ready binding disagrees with completed publication: %w",
			domain.ErrParentKeyMismatch,
		)
	}
	return true, nil
}

// AuthenticatedProductionRunConclusion combines the production completion
// binding with signet's accepted-rerun classifier. It is the shared adapter for
// engine and command consumers that already hold one coherent store read.
func AuthenticatedProductionRunConclusion(
	ctx context.Context,
	tx *store.ReadTx,
	run domain.Run,
	observation domain.RunObservation,
) (domain.RunConclusion, error) {
	// A ready-after-blocked milestone can commit before the reevaluation's
	// terminal transaction. Let the shared classifier recognize that live
	// interval before binding readiness to a completed publication.
	if domain.ConcludeRun(observation).Outcome == domain.RunOutcomeBlocked {
		conclusion, preliminaryErr := signet.AuthenticatedRunConclusion(
			ctx, tx, run, observation, false,
		)
		if preliminaryErr == nil && !conclusion.Final &&
			conclusion.Outcome == domain.RunOutcomePending {
			return conclusion, nil
		}
	}
	identity, completed, err := ProductionPublicationCompletion(ctx, tx, run)
	if domain.ConcludeRun(observation).Outcome == domain.RunOutcomeCompleted {
		itemID, itemErr := tx.PublishedProductionReadyItemID(ctx, run.ID)
		if itemErr != nil {
			return domain.RunConclusion{}, itemErr
		}
		entry, entryErr := productionTaskForReadyItem(ctx, tx, run.ID, itemID)
		identity, completed, err = productionPublicationCompletion(ctx, tx, run, entry, entryErr, true)
	}
	if err != nil {
		return domain.RunConclusion{}, err
	}
	readyAuthenticated := false
	if completed {
		readyAuthenticated, err = AuthenticateProductionPublicationReady(
			ctx, tx, run, observation, identity,
		)
		if err != nil {
			return domain.RunConclusion{}, err
		}
	}
	return signet.AuthenticatedRunConclusion(ctx, tx, run, observation, readyAuthenticated)
}

func validateProductionPublicationCompletion(
	run domain.Run, task productionPublicationTask, terminal productionTerminalRecord,
) error {
	stage, stageFound := productionStageForInvocation(run, task.ProducingInvocationID)
	if run.ID != task.RunID || run.ProjectID != task.ProjectID ||
		!stageFound ||
		terminal.Status != exec.StatusCompleted ||
		terminal.InvocationID != task.ProducingInvocationID || terminal.RunID != task.RunID ||
		terminal.StageID != stage.ID || terminal.HeadSHA != task.HeadSHA ||
		!slices.Equal(terminal.Artifacts, task.Artifacts) || terminal.Summary != task.Summary {
		return fmt.Errorf("production terminal disagrees with durable publication task: %w",
			domain.ErrParentKeyMismatch)
	}
	return nil
}
