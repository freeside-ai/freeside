package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func (w *productionPublicationWorkflow) pruneHeldTaskRetries(pending []store.QueueEntry) {
	pendingKeys := make(map[string]struct{}, len(pending))
	for _, entry := range pending {
		pendingKeys[entry.IdempotencyKey] = struct{}{}
	}
	for key := range w.holdRetryAfter {
		if _, found := pendingKeys[key]; !found {
			delete(w.holdRetryAfter, key)
		}
	}
	for key := range w.successorObservationPending {
		if _, found := pendingKeys[key]; !found {
			delete(w.successorObservationPending, key)
		}
	}
}

// recordAttendedPublicationHolds records the attended_mode_active hold for
// every queued publication task the hold-only composition is pausing, paced
// the same way as the engine's dispatch-side holds. A malformed task key
// records nothing here: the unquarantined-row handling belongs to the
// active composition, and the projection never widens its authority.
func (w *productionPublicationWorkflow) recordAttendedPublicationHolds(ctx context.Context) error {
	var pending []store.QueueEntry
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		pending, err = tx.ListPendingOutbox(ctx, KindProductionPublicationRequested)
		if err != nil {
			return err
		}
		reevaluations, err := tx.ListPendingOutbox(ctx, signet.PublicationReevaluationRequestedKind)
		if err != nil {
			return err
		}
		pending = append(pending, reevaluations...)
		continuations, err := tx.ListPendingOutbox(ctx, domain.PublicationContinuationRequestedKind)
		if err != nil {
			return err
		}
		pending = append(pending, continuations...)
		return nil
	}); err != nil {
		return err
	}
	runIDs := make([]domain.RunID, 0, len(pending))
	var joined error
	for _, entry := range pending {
		var runID domain.RunID
		var ok bool
		if entry.Kind == signet.PublicationReevaluationRequestedKind {
			task, err := w.reconstructProductionReevaluationTask(ctx, entry)
			if err != nil {
				joined = errors.Join(joined, fmt.Errorf(
					"reevaluation intent %q cannot be reconstructed: %w: %w",
					entry.IdempotencyKey, err, domain.ErrParentKeyMismatch,
				))
				continue
			}
			runID, ok = task.RunID, true
		} else {
			runID, ok = productionRunIDFromPendingTaskKey(entry.IdempotencyKey)
		}
		if !ok {
			continue
		}
		runIDs = append(runIDs, runID)
	}
	if joined != nil {
		return joined
	}
	for _, runID := range runIDs {
		now := w.now().UTC()
		if !w.holdPace.due("hold:"+string(runID), string(domain.HoldAttendedModeActive), now) {
			continue
		}
		if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
			return recordRunHold(ctx, tx, runID,
				domain.ProductionPublicationInvocationID(runID),
				domain.HoldAttendedModeActive, holdDetail{}, now)
		}); err != nil {
			w.holdPace.forget("hold:" + string(runID))
			return err
		}
	}
	return nil
}

func (w *productionPublicationWorkflow) deferHeldTask(task productionPublicationTask) {
	w.holdRetryAfter[task.intentKey()] = w.now().Add(w.holdRetryInterval)
}

// recordPublicationEnvironmentHold states the typed cause behind every
// environmental pause of one task: a work directory the daemon cannot
// prepare, a lock it cannot take, or a retryable reconcile failure (issue
// #394). The bounded retry window paces the write. A cancelled context is the
// daemon stopping rather than a cause an operator can act on, so it records
// nothing; any other write failure is a store fault the caller joins loud.
func (w *productionPublicationWorkflow) recordPublicationEnvironmentHold(
	ctx context.Context, task productionPublicationTask,
) error {
	if ctx.Err() != nil {
		return nil
	}
	return w.store.Write(ctx, func(tx *store.WriteTx) error {
		return recordRunHold(ctx, tx, task.RunID, task.PublicationID,
			domain.HoldPublicationEnvironment, holdDetail{}, w.now().UTC())
	})
}

// recordRetryableHold records the hold behind a retryable reconcile failure:
// machine_capacity, or the can-never-fit notice, for a verification job the
// memory budget refused, under the invocation that reserved;
// image_upkeep_pending for a task waiting on an image (a re-entry whose base
// waits on upkeep, or a rebuild the busy builder could not start), and
// publication_environment for any other environmental back-off. A
// verification job whose macOS executor has every slot quarantined keeps
// publication_environment and also raises a notice on the run naming the
// executor, because no retry gets a slot until the daemon restarts
// (retireHostSlotsNotice retires it).
func (w *productionPublicationWorkflow) recordRetryableHold(
	ctx context.Context, task productionPublicationTask, err error,
) error {
	invocationID := task.verificationInvocationID()
	var reservation *verificationReservationRefused
	if errors.As(err, &reservation) {
		invocationID = reservation.invocationID
	}
	if launchMemoryRefusal(err) {
		return noticeLaunchMemoryRefusal(
			ctx, w.store, w.attention, &w.holdPace, task.RunID, invocationID, task.ProjectID, err)
	}
	var capacity *domain.MachineCapacityError
	if errors.As(err, &capacity) {
		// Recorded each retry window, which paces it, so its numbers stay
		// current while the job waits.
		return w.store.Write(ctx, func(tx *store.WriteTx) error {
			return recordRunHold(ctx, tx, task.RunID, invocationID,
				domain.HoldMachineCapacity, holdDetailFor(err), w.now().UTC())
		})
	}
	var pending *ImageUpkeepPendingError
	upkeep := errors.As(err, &pending)
	if !upkeep && !errors.Is(err, errProjectImageBuilderBusy) {
		var noticeErr error
		if refused, quarantined := hostSlotsQuarantined(err); quarantined && ctx.Err() == nil {
			noticeErr = recordProductionQuarantine(ctx, w.store, w.attention, hostSlotsQuarantinedPrefix,
				task.RunID, task.ProjectID, hostSlotsQuarantinedReason(refused))
		}
		return errors.Join(noticeErr, w.recordPublicationEnvironmentHold(ctx, task))
	}
	if ctx.Err() != nil {
		return nil
	}
	hold := domain.ImageUpkeepHold{Pending: domain.ImageUpkeepPendingRebuild}
	if upkeep {
		hold = pending.Hold
	}
	if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
		return recordRunHold(ctx, tx, task.RunID, task.PublicationID,
			domain.HoldImageUpkeepPending, holdDetail{imageUpkeep: &hold}, w.now().UTC())
	}); err != nil {
		return err
	}
	// The clear after an attempt that no longer waits must not be paced
	// away while this hold is on the read surface.
	w.holdPace.forget(imageUpkeepClearKey(task))
	return nil
}

// imageUpkeepClearKey paces clearImageUpkeepHold per run.
func imageUpkeepClearKey(task productionPublicationTask) string {
	return "image-upkeep:" + string(task.RunID)
}

// clearImageUpkeepHold ends the image_upkeep_pending hold once an attempt
// ends without waiting on an image again. It runs after the attempt, not at
// its start, so a wait that recurs keeps its row and its span. Paced, so a
// run that never held costs one delete per pacing window, not one per pass.
func (w *productionPublicationWorkflow) clearImageUpkeepHold(
	ctx context.Context, task productionPublicationTask,
) error {
	key := imageUpkeepClearKey(task)
	if !w.holdPace.due(key, publicationAttemptPaceState, w.now().UTC()) {
		return nil
	}
	if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.ClearRunHoldCause(ctx, task.RunID, domain.HoldImageUpkeepPending)
	}); err != nil {
		w.holdPace.forget(key)
		return err
	}
	return nil
}

// waitsOnImage reports whether a reconcile failure is a wait on a project
// image (recordRetryableHold), which keeps the image_upkeep_pending hold.
func waitsOnImage(err error) bool {
	return productionPublicationRetryableFailure(err) &&
		(errors.Is(err, ErrImageUpkeepPending) || errors.Is(err, errProjectImageBuilderBusy))
}

// publicationAttemptPaceState paces the clear below on the hold key without
// ever naming a hold reason: it is the pacer's record that this run's
// attended-mode hold was already cleared, not an observation of a hold.
const publicationAttemptPaceState = "attempt-accepted"

// clearAttendedPublicationHold ends the hold-only composition's pause once an
// active composition accepts the queued task. Cause-scoped, so a hold this
// attempt is about to re-record keeps its row and its span, and paced like
// the hold writes so the reconcile cadence does not turn an already-cleared
// run into a delete per pass.
func (w *productionPublicationWorkflow) clearAttendedPublicationHold(
	ctx context.Context, task productionPublicationTask,
) error {
	key := "hold:" + string(task.RunID)
	if !w.holdPace.due(key, publicationAttemptPaceState, w.now().UTC()) {
		return nil
	}
	if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.ClearRunHoldCause(ctx, task.RunID, domain.HoldAttendedModeActive)
	}); err != nil {
		w.holdPace.forget(key)
		return err
	}
	return nil
}
