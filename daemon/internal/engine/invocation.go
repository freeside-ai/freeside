package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// This value mirrors signet's private outbox kind. The string is durable
// storage vocabulary, not an exported signet API: the engine is the intended
// production consumer while signet remains the producer.
const kindAgentInvocationRequested = "agent_invocation_requested"

type invocationRequest struct {
	InvocationID   domain.InvocationID   `json:"invocation_id"`
	ConversationID domain.ConversationID `json:"conversation_id"`
	ItemID         domain.ItemID         `json:"item_id"`
	ItemVersion    int                   `json:"item_version"`
}

type invocationBinding struct {
	run          domain.Run
	item         domain.AttentionItem
	invocation   domain.AgentInvocation
	conversation domain.Conversation
}

func (e *Engine) reconcileInvocations(ctx context.Context) (int, int, error) {
	started, err := e.dispatchPendingInvocations(ctx)
	if err != nil {
		return started, 0, err
	}
	accepted, err := e.acceptCompletedInvocations(ctx)
	if err != nil {
		return started, accepted, err
	}
	return started, accepted, nil
}

// dispatchPendingInvocations converts signet's committed outbox request into
// durable engine state before starting the external effect. The Run attempt is
// the engine's restart index; after Start succeeds, the outbox row can follow
// its store contract and become dispatched without making the result
// undiscoverable after a daemon restart.
func (e *Engine) dispatchPendingInvocations(ctx context.Context) (int, error) {
	var pending []store.QueueEntry
	err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		pending, err = tx.ListPendingOutbox(ctx, kindAgentInvocationRequested)
		return err
	})
	if err != nil {
		return 0, err
	}

	started := 0
	for _, entry := range pending {
		request, binding, err := e.loadInvocationRequest(ctx, entry)
		if err != nil {
			if errors.Is(err, errForeignWorkflow) {
				continue
			}
			return started, fmt.Errorf("intent %q: %w", entry.IdempotencyKey, err)
		}
		stage, ok := findFeedbackStage(binding.run)
		if !ok {
			return started, fmt.Errorf("intent %q: run %q has no feedback stage",
				entry.IdempotencyKey, binding.run.ID)
		}
		// Durable state decides first. The run snapshot already says whether
		// this invocation has an attempt, and a recorded attempt starts under
		// the admission stored beside it, so the live capability gate runs
		// only for an attempt that does not exist yet. Admitting first would
		// let a backend that no longer clears the floor strand work that was
		// already admitted, which is the same mistake as letting the current
		// configuration decide whether to read the record at all.
		var fresh *domain.ExecutionAdmission
		if !attemptRecorded(binding.run, request.InvocationID) {
			admission, admitted, err := e.admitAttempt(ctx, binding, stage, request.InvocationID)
			if err != nil {
				// A backend below the floor is a typed refusal (§5.7): nothing
				// is appended and nothing is started, so the intent stays
				// pending for a pass under a backend that clears the floor.
				return started, fmt.Errorf("intent %q: %w", entry.IdempotencyKey, err)
			}
			if admitted {
				fresh = &admission
			}
		}
		// recordAttempt reports the admission that is actually durable: on a
		// replay it is the stored one, not a freshly built value whose
		// admission instant (and therefore identity) has moved since. Starting
		// under a fresh id would hand the driver an admission no reader can
		// reconstruct.
		_, effective, bound, err := e.recordAttempt(ctx, binding.run.ID, request.InvocationID, fresh)
		if err != nil {
			return started, err
		}

		startSpec := exec.StartSpec{RunID: binding.run.ID, StageID: stage.ID}
		if bound {
			startSpec = exec.StartSpecFromAdmission(effective)
		}
		if err := e.driver.Start(ctx, request.InvocationID, startSpec); err != nil {
			if !errors.Is(err, exec.ErrDuplicateStart) {
				return started, fmt.Errorf("intent %q: start: %w", entry.IdempotencyKey, err)
			}
		} else {
			started++
		}
		if err := e.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
			return tx.MarkOutboxDispatched(ctx, entry.IdempotencyKey)
		}); err != nil {
			return started, fmt.Errorf("intent %q: mark dispatched: %w", entry.IdempotencyKey, err)
		}
	}
	return started, nil
}

func (e *Engine) acceptCompletedInvocations(ctx context.Context) (int, error) {
	var runs []store.Snapshotted[domain.Run]
	err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		runs, err = tx.ListRuns(ctx)
		return err
	})
	if err != nil {
		return 0, err
	}

	accepted := 0
	for _, snapshotted := range runs {
		run := snapshotted.Value
		owned, err := e.ownsFakeRun(ctx, run)
		if err != nil {
			return accepted, err
		}
		if !owned {
			continue
		}
		stage, ok := findFeedbackStage(run)
		if !ok {
			continue
		}
		for _, attempt := range stage.Attempts {
			didAccept, err := e.acceptAttempt(ctx, run, attempt)
			if err != nil {
				return accepted, fmt.Errorf("run %q invocation %q: %w", run.ID, attempt.InvocationID, err)
			}
			accepted += boolCount(didAccept)
		}
	}
	return accepted, nil
}

func (e *Engine) acceptAttempt(ctx context.Context, run domain.Run, attempt domain.Attempt) (bool, error) {
	if attempt.ID != attemptIDFor(attempt.InvocationID) {
		return false, fmt.Errorf("attempt %q disagrees with invocation %q: %w",
			attempt.ID, attempt.InvocationID, domain.ErrParentKeyMismatch)
	}
	binding, err := e.loadInvocationBinding(ctx, attempt.InvocationID)
	if err != nil {
		return false, err
	}
	if binding.run.ID != run.ID || attempt.StageID != feedbackStageID(run.ID) {
		return false, fmt.Errorf("attempt binding disagrees with run: %w", domain.ErrParentKeyMismatch)
	}
	accepted, err := completionAlreadyAccepted(binding.conversation, attempt.InvocationID)
	if err != nil {
		return false, err
	}
	if accepted {
		return false, nil
	}

	status, err := e.driver.Inspect(ctx, attempt.InvocationID)
	if err != nil {
		return false, fmt.Errorf("inspect: %w", err)
	}
	switch status {
	case exec.StatusPending, exec.StatusRunning:
		return false, nil
	case exec.StatusCompleted, exec.StatusFailed, exec.StatusCanceled, exec.StatusGone:
		// Collect below. A gone session may still carry a committed result.
	default:
		return false, fmt.Errorf("inspect returned status %q: %w", status, exec.ErrInvalidStatus)
	}

	result, err := e.driver.Collect(ctx, attempt.InvocationID)
	if err != nil {
		if status == exec.StatusGone && errors.Is(err, exec.ErrNoResult) {
			return false, fmt.Errorf("%w: %w", ErrInvocationLost, err)
		}
		return false, fmt.Errorf("collect: %w", err)
	}
	if err := result.Validate(); err != nil {
		return false, fmt.Errorf("validate collected result: %w", err)
	}
	if result.InvocationID != attempt.InvocationID {
		return false, fmt.Errorf("collected invocation_id %q, want %q: %w",
			result.InvocationID, attempt.InvocationID, domain.ErrParentKeyMismatch)
	}
	if status != exec.StatusGone && result.Status != status {
		return false, fmt.Errorf("collected status %q disagrees with inspected %q: %w",
			result.Status, status, exec.ErrInvalidStatus)
	}
	if result.Status != exec.StatusCompleted {
		return false, fmt.Errorf("result status %q: %w", result.Status, ErrInvocationUnsuccessful)
	}

	// Re-gated here rather than before Inspect/Collect: a verdict taken before
	// I/O and carried across it is a verdict about the past, and the trust
	// profile an unattended or waived admission is anchored to can change
	// while a driver call is in flight. This is the last point the engine
	// controls before the acceptance commits.
	//
	// It is still not inside the accepting transaction, which signet owns
	// (#316): a profile retired in the remaining window is not caught here.
	if err := e.requireAdmissible(ctx, attempt.InvocationID); err != nil {
		return false, err
	}
	if err := e.signet.AcceptAgentCompletion(ctx, attempt.InvocationID, signet.AgentReply{
		Body: result.Summary, Attachments: result.Artifacts,
	}); err != nil {
		return false, fmt.Errorf("accept result: %w", err)
	}
	return true, nil
}

func (e *Engine) loadInvocationRequest(ctx context.Context, entry store.QueueEntry) (invocationRequest, invocationBinding, error) {
	request, err := decodeInvocationRequest(entry.Payload)
	if err != nil {
		return invocationRequest{}, invocationBinding{}, err
	}
	if string(request.InvocationID) != entry.IdempotencyKey {
		return invocationRequest{}, invocationBinding{}, fmt.Errorf(
			"payload invocation_id %q disagrees with key %q: %w",
			request.InvocationID, entry.IdempotencyKey, domain.ErrParentKeyMismatch,
		)
	}
	binding, err := e.loadInvocationBinding(ctx, request.InvocationID)
	if err != nil {
		return invocationRequest{}, invocationBinding{}, err
	}
	if *binding.invocation.ConversationID != request.ConversationID ||
		binding.item.ID != request.ItemID || binding.item.ItemVersion != request.ItemVersion {
		return invocationRequest{}, invocationBinding{}, fmt.Errorf(
			"durable invocation binding disagrees with payload: %w", domain.ErrParentKeyMismatch,
		)
	}
	return request, binding, nil
}

// requireAdmissible re-gates an attempt's admission before its result is
// accepted. Reading the record runs the store's reconstruction gate, so an
// attempt admitted under a floor policy has since raised stops here rather
// than advancing the workflow: accepting that output would publish work
// produced under an isolation class the operator now rejects, which is the
// silent downgrade §5.7 refuses.
//
// An attempt with no admission record is left alone. Admission is configured
// (see WithAdmission), and an attempt appended before it was, or by a build
// that predates the record, has no audited class to re-gate; failing those
// would wedge existing work on a contract that did not exist when it started.
func (e *Engine) requireAdmissible(ctx context.Context, invocationID domain.InvocationID) error {
	// Absence is a boolean here, never an error class. A refused
	// reconstruction can itself surface as a not-found (a waived admission
	// whose trusted profile is gone), and reading that as "no record" would
	// accept output whose gate had explicitly failed closed.
	err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		_, _, err := tx.LookupExecutionAdmission(ctx, invocationID)
		return err
	})
	if err != nil {
		return fmt.Errorf("invocation %q is no longer admissible: %w", invocationID, err)
	}
	return nil
}

// attemptRecorded reports whether the run snapshot already carries an attempt
// for this invocation. It reads the snapshot the pass already loaded rather
// than the store, and recordAttempt re-checks authoritatively inside its own
// transaction, so a concurrent append between the two is still caught.
func attemptRecorded(run domain.Run, invocationID domain.InvocationID) bool {
	for _, stage := range run.Stages {
		for _, attempt := range stage.Attempts {
			if attempt.InvocationID == invocationID {
				return true
			}
		}
	}
	return false
}

// attemptIDFor is the deterministic attempt identity for an invocation: the
// engine derives it rather than generating one, so a replayed dispatch and the
// admission recorded beside it name the same attempt.
func attemptIDFor(invocationID domain.InvocationID) domain.AttemptID {
	return domain.AttemptID("attempt-" + string(invocationID))
}

func (e *Engine) loadInvocationBinding(ctx context.Context, invocationID domain.InvocationID) (invocationBinding, error) {
	var binding invocationBinding
	err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		binding.invocation, err = tx.GetAgentInvocation(ctx, invocationID)
		if err != nil {
			return err
		}
		if binding.invocation.ConversationID == nil {
			return fmt.Errorf("invocation has no conversation binding: %w", domain.ErrEmptyID)
		}
		binding.conversation, err = tx.GetConversation(ctx, *binding.invocation.ConversationID)
		if err != nil {
			return err
		}
		if binding.invocation.ThroughSequence > len(binding.conversation.Messages) ||
			binding.conversation.Messages[binding.invocation.ThroughSequence-1].Author != domain.AuthorUser {
			return fmt.Errorf("invocation conversation prefix is not present: %w", domain.ErrParentKeyMismatch)
		}

		items, err := tx.ListAttentionItems(ctx)
		if err != nil {
			return err
		}
		matches := 0
		for _, snapshotted := range items {
			item := snapshotted.Value
			if item.ConversationID == nil || *item.ConversationID != *binding.invocation.ConversationID {
				continue
			}
			matches++
			binding.item = item
		}
		if matches != 1 {
			return fmt.Errorf("conversation %q binds %d attention items, want 1",
				*binding.invocation.ConversationID, matches)
		}
		if binding.item.Subject.RunID == nil || *binding.item.Subject.RunID == "" {
			return fmt.Errorf("attention item %q has no run binding: %w", binding.item.ID, domain.ErrEmptyID)
		}
		binding.run, err = tx.GetRun(ctx, *binding.item.Subject.RunID)
		if err != nil {
			return err
		}
		marker, err := tx.GetAttentionItem(ctx, initialItemID(binding.run.ID))
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("%w: run %q has no fake workflow marker", errForeignWorkflow, binding.run.ID)
			}
			return fmt.Errorf("fake workflow marker for run %q: %w", binding.run.ID, err)
		}
		if !sameWorkflowItem(marker, initialItem(binding.run)) {
			return fmt.Errorf("fake workflow marker for run %q disagrees with its binding: %w",
				binding.run.ID, domain.ErrParentKeyMismatch)
		}
		if marker.Status != domain.StatusResolved {
			return fmt.Errorf("fake workflow marker for run %q has status %q, want resolved: %w",
				binding.run.ID, marker.Status, domain.ErrParentKeyMismatch)
		}
		return nil
	})
	if err != nil {
		return invocationBinding{}, err
	}
	if !sameWorkflowItem(binding.item, feedbackItem(binding.run)) {
		return invocationBinding{}, fmt.Errorf("attention item %q is not the feedback item for run %q: %w",
			binding.item.ID, binding.run.ID, domain.ErrParentKeyMismatch)
	}
	return binding, nil
}

func completionAlreadyAccepted(conversation domain.Conversation, invocationID domain.InvocationID) (bool, error) {
	wantMessage := domain.MessageID("msg-agent-" + string(invocationID))
	found := false
	for _, message := range conversation.Messages {
		if message.ID == wantMessage {
			if message.Author != domain.AuthorAgent {
				return false, fmt.Errorf("accepted message %q has author %q, want agent",
					wantMessage, message.Author)
			}
			found = true
			break
		}
	}
	// A later discuss may already have returned the conversation to awaiting
	// while this older attempt remains in the append-only Run history. Its own
	// immutable agent message is sufficient proof that this result was accepted.
	if found {
		return true, nil
	}
	switch conversation.Status {
	case domain.ConversationAwaitingAgent:
		return false, nil
	case domain.ConversationIdle:
		return false, fmt.Errorf("conversation is idle without accepted message %q", wantMessage)
	}
	return false, fmt.Errorf("conversation status %q is invalid", conversation.Status)
}

func decodeInvocationRequest(payload []byte) (invocationRequest, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var request invocationRequest
	if err := decoder.Decode(&request); err != nil {
		return invocationRequest{}, fmt.Errorf("decode payload: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return invocationRequest{}, errors.New("decode payload: trailing JSON value")
	}
	if request.InvocationID == "" || request.ConversationID == "" || request.ItemID == "" {
		return invocationRequest{}, fmt.Errorf("decode payload: required identity is empty: %w", domain.ErrEmptyID)
	}
	if request.ItemVersion < 1 {
		return invocationRequest{}, fmt.Errorf("decode payload: item_version %d: %w", request.ItemVersion, domain.ErrNonPositive)
	}
	return request, nil
}

// recordAttempt appends the attempt that makes an invocation discoverable
// after a restart and, when the dispatch was admitted, records that admission
// in the same transaction. Splitting the two would leave either an attempt
// with no audited class or an admission for an attempt that was never
// appended, and a crash between them is exactly when the record matters.
func (e *Engine) recordAttempt(
	ctx context.Context, runID domain.RunID, invocationID domain.InvocationID,
	fresh *domain.ExecutionAdmission,
) (bool, domain.ExecutionAdmission, bool, error) {
	added := false
	var effective domain.ExecutionAdmission
	bound := fresh != nil
	if fresh != nil {
		effective = *fresh
	}
	err := e.store.Write(ctx, func(tx *store.WriteTx) error {
		run, err := tx.GetRun(ctx, runID)
		if err != nil {
			return err
		}
		stageIndex := -1
		for i, stage := range run.Stages {
			for _, attempt := range stage.Attempts {
				if attempt.InvocationID == invocationID {
					if stage.ID != feedbackStageID(runID) ||
						attempt.ID != attemptIDFor(invocationID) {
						return fmt.Errorf("invocation %q is already bound to attempt %q in stage %q: %w",
							invocationID, attempt.ID, stage.ID, domain.ErrParentKeyMismatch)
					}
					// The attempt is already durable, so this pass must start
					// under whatever admission is stored beside it, never
					// under the one it just built (whose instant, and so
					// whose identity, has moved). The lookup does not depend
					// on this process being configured to admit: an attempt
					// that was admitted stays admitted, and a restart that
					// happens to have lost the configuration must not
					// downgrade it to an unbound start. Reading the record
					// also re-gates it, so a floor raised in the meantime
					// stops the replay here.
					stored, found, err := tx.LookupExecutionAdmission(ctx, invocationID)
					switch {
					case err != nil:
						// Reconstruction was refused, which is not the same as
						// having no record: fail the replay rather than
						// treating a closed gate as a legacy attempt.
						return fmt.Errorf("replayed invocation %q: %w", invocationID, err)
					case found:
						effective, bound = stored, true
					case e.admission == nil:
						// No record exists, so there is no durable decision to
						// honour here and the only question is what to do in
						// its absence. That question, unlike anything about a
						// record that does exist, is legitimately the current
						// configuration's: an engine that admits nothing
						// starts the attempt as it always did, while a
						// configured one falls through and fails closed rather
						// than starting unaudited work.
						bound = false
					default:
						// Configured to admit, but the attempt carries no
						// audited class: fail closed rather than invent one.
						return fmt.Errorf("replayed invocation %q has no admission: %w",
							invocationID, store.ErrNotFound)
					}
					return errReplay
				}
			}
			if stage.ID == feedbackStageID(runID) {
				stageIndex = i
			}
		}
		if stageIndex < 0 {
			return fmt.Errorf("run %q has no feedback stage", runID)
		}
		stage := run.Stages[stageIndex]
		stage.Attempts = append(stage.Attempts, domain.Attempt{
			ID:           attemptIDFor(invocationID),
			StageID:      stage.ID,
			Number:       len(stage.Attempts) + 1,
			InvocationID: invocationID,
		})
		run.Stages[stageIndex] = stage
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if fresh != nil {
			if err := tx.RecordExecutionAdmission(ctx, *fresh); err != nil {
				return err
			}
			// §5.7 requires a waived admission to surface its degraded
			// posture, not only to record the waiver. It lands in this same
			// transaction: an admission committed without its notice would
			// leave unattended work running on the exception with nothing
			// telling the operator so.
			if fresh.BackupEncryptionWaiver != nil {
				item, err := waivedPostureItem(run, invocationID, *fresh.BackupEncryptionWaiver)
				if err != nil {
					return err
				}
				if err := tx.PutAttentionItem(ctx, item); err != nil {
					return err
				}
			}
		}
		added = true
		return nil
	})
	if errors.Is(err, errReplay) {
		return false, effective, bound, nil
	}
	if err != nil {
		return false, domain.ExecutionAdmission{}, false,
			fmt.Errorf("record invocation %q on run %q: %w", invocationID, runID, err)
	}
	return added, effective, bound, nil
}

func findFeedbackStage(run domain.Run) (domain.Stage, bool) {
	for _, stage := range run.Stages {
		if stage.ID == feedbackStageID(run.ID) && stage.Name == feedbackStageName {
			return stage, true
		}
	}
	return domain.Stage{}, false
}
