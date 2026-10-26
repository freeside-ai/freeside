package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

func (w *productionPublicationWorkflow) requireWorkflowOpen(ctx context.Context, runID domain.RunID) error {
	return w.store.Read(ctx, func(tx *store.ReadTx) error {
		return requireTaskExecutionOpen(ctx, tx, runID)
	})
}

func (w *productionPublicationWorkflow) claimProviderDispatch(ctx context.Context, runID domain.RunID) error {
	if w.claimDispatch != nil {
		return w.claimDispatch(ctx, runID)
	}
	return w.requireWorkflowOpen(ctx, runID)
}

func (w *productionPublicationWorkflow) reconcileClosedRunPublication(ctx context.Context, runID domain.RunID) error {
	if w.reconcileClosedPublication != nil {
		return w.reconcileClosedPublication(ctx, runID)
	}
	return w.publisher.ReconcileClosedWorkflowRun(ctx, runID)
}

// Closed runs keep their real provider and forge outcomes. This sweep is
// independent of publication tasks: a ready task can already have left the
// queue while its shadow review or an external acknowledgement is outstanding.
// It holds the lane's mutex for the publication pacing state; the lane runs
// it before reconcile, which takes the same mutex, so the two never nest.
func (w *productionPublicationWorkflow) observeClosedWorkflows(ctx context.Context) error {
	w.reconcileMu.Lock()
	defer w.reconcileMu.Unlock()
	if w.closedPublicationRetry == nil {
		w.closedPublicationRetry = make(map[domain.RunID]closedPublicationPace)
	}
	var closures []store.TaskWorkflowClosure
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		closures, err = tx.ListTaskWorkflowClosures(ctx)
		return err
	}); err != nil {
		return err
	}
	for _, closure := range closures {
		for _, runID := range closure.RunIDs {
			var reviews []domain.ReviewRequestRecord
			if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
				var err error
				reviews, err = tx.ListReviewRequests(ctx, runID)
				return err
			}); err != nil {
				return err
			}
			for _, review := range reviews {
				for _, arm := range []struct {
					id     domain.InvocationID
					source exec.ReviewSource
				}{
					{review.InvocationID, w.reviewSource},
					{ProductionShadowReviewInvocationID(runID, review.Round), w.shadowReviewSource},
				} {
					if arm.source == nil {
						continue
					}
					if err := w.observeClosedReview(ctx, review, arm.id, arm.source); err != nil {
						w.logger.Warn("closed workflow review observation remains unresolved", "run", runID, "invocation", arm.id, "error", err)
					}
				}
			}
			w.observeClosedPublication(ctx, runID)
		}
	}
	return nil
}

// closedPublicationPace is one closed run's place in its publication
// observation schedule. failures counts every consecutive failed attempt and
// sets the backoff; unsettled counts only the attempts since the last settle
// that the forge answered with no settled outcome, and alone raises the hold.
type closedPublicationPace struct {
	due       time.Time
	failures  int
	unsettled int
}

// closedPublicationRetryDelay waits holdRetryInterval after the first failed
// attempt and doubles per further failure, up to the cap.
func closedPublicationRetryDelay(interval time.Duration, failures int) time.Duration {
	return interval << min(failures-1, closedPublicationMaxBackoffShift)
}

// observeClosedPublication paces one closed run's publication observation
// (#2343). Without it the lane's 100 ms cadence turned an intent nothing can
// settle into a GitHub call per pass, on every later start of the root. A
// transient failure backs off like an unsettled one but records no hold: only
// an outcome that lasts until something outside the daemon changes the branch
// tells the operator to look. The hold is the run's existing projection, not
// an attention item, because closure retirement expires a closed episode's
// items on every pass. A failed hold write is logged and retried on the next
// due attempt, like this sweep's other observation failures. Closures are
// never deleted, so a run's entry lives until its intent settles.
func (w *productionPublicationWorkflow) observeClosedPublication(ctx context.Context, runID domain.RunID) {
	now := w.now()
	pace, paced := w.closedPublicationRetry[runID]
	if paced && now.Before(pace.due) {
		return
	}
	err := w.reconcileClosedRunPublication(ctx, runID)
	if err == nil {
		if !paced {
			return
		}
		// Only a run that failed in this process clears: clearing on every
		// settled pass would write once per pass for every closed run.
		if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
			return tx.ClearRunHoldCause(ctx, runID, domain.HoldExternalConflict)
		}); err != nil {
			w.logger.Warn("closed workflow publication hold remains after its intent settled", "run", runID, "error", err)
			return
		}
		delete(w.closedPublicationRetry, runID)
		return
	}
	pace.failures++
	pace.due = now.Add(closedPublicationRetryDelay(w.holdRetryInterval, pace.failures))
	unsettled := errors.Is(err, publish.ErrPublicationUnsettled)
	if unsettled {
		pace.unsettled++
	}
	w.closedPublicationRetry[runID] = pace
	w.logger.Warn("closed workflow publication observation remains unresolved",
		"run", runID, "attempt", pace.failures, "next_attempt_at", pace.due.UTC(), "error", err)
	if !unsettled || pace.unsettled < closedPublicationUnsettledTolerance {
		return
	}
	if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
		// A task held at publication before it was abandoned already carries
		// this cause under its publication invocation. Keeping that invocation
		// keeps the hold's first instant, so the operator still sees when the
		// conflict began; a different cause names none.
		var invocation domain.InvocationID
		held, found, err := tx.GetRunHold(ctx, runID)
		if err != nil {
			return err
		}
		if found && held.Reason == domain.HoldExternalConflict && held.InvocationID != nil {
			invocation = *held.InvocationID
		}
		return recordRunHold(ctx, tx, runID, invocation, domain.HoldExternalConflict, nil, now.UTC())
	}); err != nil {
		w.logger.Warn("closed workflow publication hold was not recorded", "run", runID, "error", err)
	}
}

func (w *productionPublicationWorkflow) observeClosedReview(
	ctx context.Context, binding domain.ReviewRequestRecord, id domain.InvocationID,
	source exec.ReviewSource,
) error {
	var request exec.ReviewRequest
	retained := false
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		row, err := tx.GetCodexReviewRequest(ctx, string(id))
		if err != nil {
			return err
		}
		retained = true
		binding.InvocationID = id
		if _, err := ward.DecodeRetainedCodexReviewRequestFacts(binding, row.Body, row.BodyDigest); err != nil {
			return err
		}
		if err := strictjson.Decode(row.Body, &request, strictjson.RejectInvalidUTF8, strictjson.NoLimit); err != nil {
			return err
		}
		if err := exec.ValidateRetainedReviewRequest(row.Body, request); err != nil {
			return err
		}
		admission, err := tx.GetReviewAdmission(ctx, id)
		if err != nil {
			return err
		}
		if !admission.Equal(request.Admission) {
			return domain.ErrReviewAdmissionMismatch
		}
		return nil
	})
	if !retained && errors.Is(err, store.ErrNotFound) {
		return nil // No authenticated dispatched request; never start one.
	}
	if err != nil {
		return err
	}
	authority, err := request.AuthorityDigest()
	if err != nil {
		return err
	}
	verifier, ok := source.(exec.ReviewRequestAuthorityVerifier)
	if !ok {
		return errors.New("review source cannot authenticate retained request")
	}
	if err := verifier.VerifyRequestAuthority(ctx, id, authority); err != nil {
		return err
	}
	// Inspect can relaunch a retained request after restart. Poll collects
	// only a result that already exists and releases real provider capacity.
	result, err := source.Poll(ctx, id)
	if errors.Is(err, exec.ErrResultNotReady) || errors.Is(err, exec.ErrUnknownInvocation) {
		return nil
	}
	var failure *exec.ReviewSourceFailure
	if errors.Is(err, exec.ErrNoResult) && errors.As(err, &failure) {
		if err := appendUsageObservations(ctx, w.store, id, failure.Usage); err != nil {
			return err
		}
		payload, err := json.Marshal(struct {
			InvocationID domain.InvocationID       `json:"invocation_id"`
			Class        domain.ReviewFailureClass `json:"class"`
			Usage        []exec.UsageMeasurement   `json:"usage"`
		}{id, exec.ClassifyReviewSourceFailure(failure), failure.Usage})
		if err != nil {
			return err
		}
		return w.recordClosedReviewObservation(ctx, request.RunID, id, payload)
	}
	if errors.Is(err, exec.ErrNoResult) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := result.Validate(); err != nil {
		return err
	}
	if result.InvocationID != id || result.BaseSHA != request.BaseSHA || result.HeadSHA != request.HeadSHA ||
		result.InstructionDigest != request.Instructions.ResultDigest ||
		!result.Admission.Equal(request.Admission) {
		return domain.ErrParentKeyMismatch
	}
	for _, finding := range result.Findings {
		if finding.RunID != request.RunID {
			return domain.ErrParentKeyMismatch
		}
	}
	if err := source.Verify(ctx, id, request.BaseSHA, request.HeadSHA); err != nil {
		return err
	}
	// The source authenticates its retained result, including its historical
	// configuration digest. Today's configuration grants no new authority
	// here and may legitimately differ after restart.
	if err := appendUsageObservations(ctx, w.store, id, result.Usage); err != nil {
		return err
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return w.recordClosedReviewObservation(ctx, request.RunID, id, payload)
}

func (w *productionPublicationWorkflow) recordClosedReviewObservation(
	ctx context.Context, runID domain.RunID, id domain.InvocationID, payload []byte,
) error {
	err := w.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RequireRunWorkflowOpen(ctx, runID); !errors.Is(err, store.ErrTaskWorkflowClosed) {
			return fmt.Errorf("closed review lost its episode binding: %w", errors.Join(err, domain.ErrParentKeyMismatch))
		}
		stored, inserted, err := tx.RecordInbox(ctx, "closed-workflow-review/"+string(id), "closed_workflow_review", payload)
		if err != nil {
			return err
		}
		if stored.Kind != "closed_workflow_review" || !bytes.Equal(stored.Payload, payload) {
			return store.ErrImmutableConflict
		}
		if !inserted {
			return errReplay
		}
		return nil
	})
	if errors.Is(err, errReplay) {
		return nil
	}
	return err
}
