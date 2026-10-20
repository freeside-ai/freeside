package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// RetireAbandonedTaskItems is the current-episode convenience entry point.
// Explicit command repair uses RetireClosedTaskWorkflowItems with its captured
// ordinal. Neither helper grants closure authority from display lifecycle.
func RetireAbandonedTaskItems(ctx context.Context, tx *store.WriteTx, taskID domain.TaskID) ([]domain.ItemID, error) {
	task, err := tx.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if task.CurrentStart() == nil {
		return []domain.ItemID{}, nil
	}
	if _, err := tx.GetTaskWorkflowClosure(ctx, taskID, task.CurrentStart().Ordinal); errors.Is(err, store.ErrNotFound) {
		return []domain.ItemID{}, nil
	} else if err != nil {
		return nil, err
	}
	return RetireClosedTaskWorkflowItems(ctx, tx, taskID, task.CurrentStart().Ordinal)
}

// RetireClosedTaskWorkflowItems expires only decisions owned by a durable
// closure. Expiry is not an operator decision and never sets decided_at.
func RetireClosedTaskWorkflowItems(ctx context.Context, tx *store.WriteTx, taskID domain.TaskID, ordinal int) ([]domain.ItemID, error) {
	if _, err := tx.GetTaskWorkflowClosure(ctx, taskID, ordinal); err != nil {
		return nil, err
	}
	retired := []domain.ItemID{}
	items, err := tx.ListOpenAttentionItemRecordsForTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	for _, kind := range []domain.AttentionType{domain.AttentionEffectProposal, domain.AttentionTaskProposal} {
		proposals, err := tx.ListOpenAttentionItems(ctx, kind)
		if err != nil {
			return nil, err
		}
		for _, item := range proposals {
			if item.Subject.TaskID == nil || *item.Subject.TaskID != taskID {
				items = append(items, item)
			}
		}
	}
	for _, item := range items {
		owner, episode, err := tx.AttentionItemWorkflowEpisode(ctx, item)
		if err != nil {
			return nil, err
		}
		if owner != taskID || episode != ordinal {
			continue
		}
		item.Status = domain.StatusExpired
		item.ItemVersion++
		// Some readers authenticate structured reason suffixes. The closure
		// record carries the retirement reason without rewriting those bytes.
		if err := tx.PutAttentionItem(ctx, item); err != nil {
			return nil, fmt.Errorf("expire abandoned-task item %q: %w", item.ID, err)
		}
		retired = append(retired, item.ID)
	}
	sort.Slice(retired, func(i, j int) bool { return retired[i] < retired[j] })
	return retired, nil
}

// Cleanup is restartable and never supplies the fence. A card whose current
// evidence policy fails stays visible for repair while other tasks continue.
func (e *Engine) reconcileWorkflowClosures(ctx context.Context) error {
	var closures []store.TaskWorkflowClosure
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		closures, err = tx.ListTaskWorkflowClosures(ctx)
		return err
	}); err != nil {
		return err
	}
	for _, closure := range closures {
		err := e.store.Write(ctx, func(tx *store.WriteTx) error {
			items, err := RetireClosedTaskWorkflowItems(ctx, tx, closure.TaskID, closure.StartOrdinal)
			if err != nil {
				return err
			}
			if len(items) == 0 {
				return errReplay
			}
			return nil
		})
		if err != nil && !errors.Is(err, errReplay) {
			e.logger.Warn("workflow closure cleanup remains incomplete", "task", closure.TaskID, "episode", closure.StartOrdinal, "error", err)
		}
	}
	if len(closures) != 0 {
		return e.retireClosedWorkflowIntents(ctx)
	}
	return nil
}

// Retirement runs before admission/health holds. Loaders authenticate each
// request's owning run; unreadable requests remain for their ordinary
// quarantine path and can never gain closure authority from guessed fields.
func (e *Engine) retireClosedWorkflowIntents(ctx context.Context) error {
	for _, kind := range []string{
		KindProductionInvocationRequested, KindSpecificationInvocationRequested,
		KindSpecificationDiscussionRequested, KindRemediationInvocationRequested,
		KindOperatorFeedbackInvocationRequested, kindAgentInvocationRequested,
	} {
		var pending []store.QueueEntry
		if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			pending, err = tx.ListPendingOutbox(ctx, kind)
			return err
		}); err != nil {
			return err
		}
		for _, entry := range pending {
			var binding invocationBinding
			var err error
			switch kind {
			case KindProductionInvocationRequested:
				runID, _ := productionRunIDFromInvocationID(domain.InvocationID(entry.IdempotencyKey))
				var request productionInvocationRequest
				request, err = authenticateProductionMarker(entry, runID)
				if err == nil {
					binding, err = e.loadProductionBinding(ctx, request)
				}
			case KindSpecificationInvocationRequested:
				_, binding, err = e.loadSpecificationBinding(ctx, entry)
			case KindSpecificationDiscussionRequested:
				_, binding, err = e.loadSpecificationDiscussionBinding(ctx, entry)
			case KindRemediationInvocationRequested:
				var request remediationInvocationRequest
				request, err = decodeRemediationRequest(entry)
				if err == nil {
					binding, err = e.loadRemediationBinding(ctx, request)
				}
			case KindOperatorFeedbackInvocationRequested:
				_, binding, err = e.loadOperatorFeedbackBinding(ctx, entry)
			case kindAgentInvocationRequested:
				var discussion *attentionDiscussion
				discussion, err = e.loadAttentionDiscussion(ctx, entry)
				if err == nil && discussion != nil {
					binding.invocation = discussion.invocation
					err = e.store.Read(ctx, func(tx *store.ReadTx) error {
						taskID, ordinal, err := tx.AttentionItemWorkflowEpisode(ctx, discussion.item)
						if err != nil || ordinal == 0 {
							return err
						}
						closure, err := tx.GetTaskWorkflowClosure(ctx, taskID, ordinal)
						if err != nil {
							return err
						}
						binding.run, err = tx.GetRun(ctx, closure.StartRunID)
						return err
					})
				}
			}
			if err != nil || binding.run.ID == "" {
				continue
			}
			if _, err := e.retireClosedInvocationIntent(ctx, entry, binding.run.ID, binding.invocation.ConversationID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *Engine) retireClosedInvocationIntent(ctx context.Context, entry store.QueueEntry, runID domain.RunID, conversationID *domain.ConversationID) (bool, error) {
	retired := false
	err := e.store.Write(ctx, func(tx *store.WriteTx) error {
		err := tx.RequireRunWorkflowOpen(ctx, runID)
		if err == nil {
			return errReplay
		}
		if !errors.Is(err, store.ErrTaskWorkflowClosed) {
			return err
		}
		current, err := tx.GetOutbox(ctx, entry.IdempotencyKey)
		if err != nil {
			return err
		}
		if current.Kind != entry.Kind || !bytes.Equal(current.Payload, entry.Payload) {
			return domain.ErrParentKeyMismatch
		}
		retired = true
		if current.Dispatched() {
			return errReplay
		}
		if entry.Kind == KindSpecificationDiscussionRequested {
			request, err := decodeSpecificationDiscussionRequest(current)
			if err != nil {
				return err
			}
			verified, err := verifySpecificationDiscussionBinding(ctx, &tx.ReadTx, request)
			if err != nil {
				return err
			}
			if verified.binding.run.ID != runID {
				return domain.ErrParentKeyMismatch
			}
			if err := tx.MarkOutboxDispatched(ctx, string(request.DiscussInvocationID)); err != nil {
				return err
			}
		}
		if conversationID != nil {
			conversation, err := tx.GetConversation(ctx, *conversationID)
			if err != nil {
				return err
			}
			if conversation.Status == domain.ConversationAwaitingAgent {
				conversation.Status = domain.ConversationIdle
				if err := tx.PutConversation(ctx, conversation); err != nil {
					return err
				}
			}
		}
		return tx.MarkOutboxDispatched(ctx, entry.IdempotencyKey)
	})
	if errors.Is(err, errReplay) {
		err = nil
	}
	return retired, err
}

// collectClosedRun retains validated provider observations and usage without
// feeding them to specification, recovery, readiness or publication consumers.
func (e *Engine) collectClosedRun(ctx context.Context, run domain.Run) (int, error) {
	accepted := 0
	for _, stage := range run.Stages {
		for _, attempt := range stage.Attempts {
			owned := false
			err := e.store.Read(ctx, func(tx *store.ReadTx) error {
				entry, err := tx.GetOutbox(ctx, string(attempt.InvocationID))
				if errors.Is(err, store.ErrNotFound) {
					return nil // A stage owned by another workflow has no intent here.
				}
				if err != nil {
					return err
				}
				switch entry.Kind {
				case kindAgentInvocationRequested, KindProductionInvocationRequested,
					KindSpecificationInvocationRequested, KindSpecificationDiscussionRequested,
					KindRemediationInvocationRequested, KindOperatorFeedbackInvocationRequested:
					owned = true
				default:
					return nil
				}
				admission, err := tx.GetExecutionAdmissionRecord(ctx, attempt.InvocationID)
				if errors.Is(err, store.ErrNotFound) {
					return nil // Attended fixtures may predate durable admission.
				}
				if err != nil {
					return err
				}
				if admission.RunID != run.ID || admission.StageID != stage.ID || admission.AttemptID != attempt.ID {
					return domain.ErrParentKeyMismatch
				}
				return nil
			})
			if err != nil {
				return accepted, err
			}
			if !owned {
				continue
			}
			result, ready, err := e.collectWorkflowTerminal(ctx, run.ID, attempt, true)
			if errors.Is(err, exec.ErrUnknownInvocation) || errors.Is(err, ErrInvocationLost) {
				continue // Absence is neither successful cancellation nor completion.
			}
			if err != nil {
				return accepted, err
			}
			if !ready {
				continue
			}
			payload, err := json.Marshal(struct {
				RunID   domain.RunID     `json:"run_id"`
				StageID domain.StageID   `json:"stage_id"`
				Result  exec.StageResult `json:"result"`
			}{run.ID, stage.ID, result})
			if err != nil {
				return accepted, err
			}
			err = e.store.Write(ctx, func(tx *store.WriteTx) error {
				if err := tx.RequireRunWorkflowOpen(ctx, run.ID); !errors.Is(err, store.ErrTaskWorkflowClosed) {
					return errors.Join(err, domain.ErrParentKeyMismatch)
				}
				stored, inserted, err := tx.RecordInbox(ctx, "closed-workflow-result/"+string(attempt.InvocationID), "closed_workflow_result", payload)
				if err != nil {
					return err
				}
				if stored.Kind != "closed_workflow_result" || !bytes.Equal(stored.Payload, payload) {
					return store.ErrImmutableConflict
				}
				if !inserted {
					return errReplay
				}
				return nil
			})
			if err != nil && !errors.Is(err, errReplay) {
				return accepted, err
			}
			if err == nil {
				accepted++
			}
			if err := e.acceptClosedConversationReply(ctx, run, attempt, result); err != nil {
				return accepted, err
			}
		}
	}
	return accepted, nil
}

func (e *Engine) acceptClosedConversationReply(ctx context.Context, run domain.Run, attempt domain.Attempt, result exec.StageResult) error {
	var invocation domain.AgentInvocation
	var entry store.QueueEntry
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		invocation, err = tx.GetAgentInvocation(ctx, attempt.InvocationID)
		if err != nil {
			return err
		}
		entry, err = tx.GetOutbox(ctx, string(attempt.InvocationID))
		return err
	}); err != nil {
		return err
	}
	if invocation.ConversationID == nil || result.Status != exec.StatusCompleted {
		return nil
	}
	if entry.Kind == KindSpecificationDiscussionRequested {
		request, err := decodeSpecificationDiscussionRequest(entry)
		if err != nil {
			return err
		}
		if request.SpecificationRunID != run.ID {
			return domain.ErrParentKeyMismatch
		}
		if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
			_, err := verifySpecificationDiscussionBinding(ctx, tx, request)
			return err
		}); err != nil {
			return err
		}
		output, err := e.readSpecificationOutput(ctx, request.InvocationID, result, specify.DecodeTranscript)
		if err != nil {
			return err
		}
		// Retain an actual authenticated reply only. A refused or malformed
		// result stays in the observation inbox, with no invented answer.
		if output.Reply == nil || importer.ContainsSecret([]byte(*output.Reply)) {
			return nil
		}
		return e.acceptSpecDiscussionReply(ctx, request, *output.Reply)
	}
	binding, err := e.loadInvocationBinding(ctx, invocation.ID)
	if err != nil {
		return err
	}
	if binding.run.ID != run.ID || attempt.StageID != feedbackStageID(run.ID) {
		return domain.ErrParentKeyMismatch
	}
	return e.signet.AcceptAgentCompletion(ctx, invocation.ID, signet.AgentReply{
		Body: result.Summary, Attachments: result.Artifacts,
	}, signet.WithPreCommitGate(func(ctx context.Context, tx *store.ReadTx) error {
		admission, err := tx.GetExecutionAdmissionRecord(ctx, invocation.ID)
		if errors.Is(err, store.ErrNotFound) {
			return nil // Legacy attended work has no admission record.
		}
		if err != nil {
			return err
		}
		if admission.RunID != run.ID || admission.StageID != attempt.StageID || admission.AttemptID != attempt.ID {
			return domain.ErrParentKeyMismatch
		}
		return nil
	}))
}
