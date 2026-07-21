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
		if _, err := e.recordAttempt(ctx, binding.run.ID, request.InvocationID); err != nil {
			return started, err
		}

		startSpec := exec.StartSpec{RunID: binding.run.ID, StageID: stage.ID}
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
	if attempt.ID != domain.AttemptID("attempt-"+string(attempt.InvocationID)) {
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

func (e *Engine) recordAttempt(ctx context.Context, runID domain.RunID, invocationID domain.InvocationID) (bool, error) {
	added := false
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
						attempt.ID != domain.AttemptID("attempt-"+string(invocationID)) {
						return fmt.Errorf("invocation %q is already bound to attempt %q in stage %q: %w",
							invocationID, attempt.ID, stage.ID, domain.ErrParentKeyMismatch)
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
			ID:           domain.AttemptID("attempt-" + string(invocationID)),
			StageID:      stage.ID,
			Number:       len(stage.Attempts) + 1,
			InvocationID: invocationID,
		})
		run.Stages[stageIndex] = stage
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		added = true
		return nil
	})
	if errors.Is(err, errReplay) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("record invocation %q on run %q: %w", invocationID, runID, err)
	}
	return added, nil
}

func findFeedbackStage(run domain.Run) (domain.Stage, bool) {
	for _, stage := range run.Stages {
		if stage.ID == feedbackStageID(run.ID) && stage.Name == feedbackStageName {
			return stage, true
		}
	}
	return domain.Stage{}, false
}
