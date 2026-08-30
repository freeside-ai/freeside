package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const unavailableAttentionDiscussionReply = "Discussion is unavailable for this item right now; the decision set is unchanged."

type attentionDiscussion struct {
	entry        store.QueueEntry
	request      domain.ConversationInvocationIntent
	invocation   domain.AgentInvocation
	conversation domain.Conversation
	item         domain.AttentionItem
	reply        *domain.Message
}

type attentionDiscussionCardFacts struct {
	ExecutionFailure            *domain.ExecutionFailureFacts              `json:"execution_failure,omitempty"`
	ExecutionFailureReason      string                                     `json:"execution_failure_reason,omitempty"`
	ReviewConfigurationRecovery *domain.ReviewConfigurationRecoveryBinding `json:"review_configuration_recovery,omitempty"`
	ReviewDispute               *domain.ReviewDisputeBinding               `json:"review_dispute,omitempty"`
	ReviewDisputeClaims         []domain.AgentClaim                        `json:"review_dispute_claims,omitempty"`
}

// reconcileAttentionDiscussions consumes the conversation intents that do not
// belong to a workspace workflow. One malformed or foreign intent stays
// pending and cannot stop unrelated reconciliation work.
func (e *Engine) reconcileAttentionDiscussions(ctx context.Context) (int, error) {
	var pending []store.QueueEntry
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		pending, err = tx.ListPendingOutbox(ctx, kindAgentInvocationRequested)
		return err
	}); err != nil {
		return 0, err
	}

	accepted := 0
	for _, entry := range pending {
		discussion, err := e.loadAttentionDiscussion(ctx, entry)
		if err != nil {
			e.logger.Warn("attention discussion intent remains pending", "intent", entry.IdempotencyKey, "error", err)
			continue
		}
		if discussion == nil {
			continue
		}
		switch discussion.item.Type {
		case domain.AttentionFindingAdjudication, domain.AttentionSpecApproval:
			continue
		case domain.AttentionExecutionFailure, domain.AttentionReviewConfiguration, domain.AttentionReviewDispute:
		case domain.AttentionReviewDiminishing, domain.AttentionReviewContradiction,
			domain.AttentionAgentQuestion, domain.AttentionPublishBlocked,
			domain.AttentionReadyForFinalReview, domain.AttentionRunProposal,
			domain.AttentionSystemHealth, domain.AttentionBlocked:
			continue
		}

		if discussion.reply != nil {
			if err := e.retireAttentionDiscussion(ctx, discussion.entry.IdempotencyKey); err != nil {
				return accepted, err
			}
			continue
		}
		if discussion.item.Status == domain.StatusOpen &&
			discussion.item.ItemVersion != discussion.request.ItemVersion {
			continue
		}
		if e.inference == nil || !e.inference.SupportsSite(inference.AttentionDiscussionSiteID) {
			continue
		}
		held, err := e.attentionDiscussionHeld(ctx)
		if err != nil {
			return accepted, err
		}
		if held {
			continue
		}
		reply, err := e.produceAttentionDiscussionReply(ctx, *discussion)
		if err != nil {
			e.logger.Warn("attention discussion reply remains pending", "intent", entry.IdempotencyKey, "error", err)
			continue
		}
		if err := e.signet.AcceptAgentCompletion(ctx, discussion.invocation.ID, signet.AgentReply{Body: reply},
			signet.WithPreCommitGate(func(ctx context.Context, tx *store.ReadTx) error {
				item, err := tx.GetAttentionItem(ctx, discussion.item.ID)
				if err != nil {
					return err
				}
				if item.Status == domain.StatusOpen && item.ItemVersion != discussion.request.ItemVersion ||
					item.Type != discussion.item.Type || item.ConversationID == nil ||
					*item.ConversationID != discussion.request.ConversationID {
					return domain.ErrParentKeyMismatch
				}
				return nil
			}),
		); err != nil {
			e.logger.Warn("attention discussion completion remains pending", "intent", entry.IdempotencyKey, "error", err)
			continue
		}
		accepted++
		if err := e.retireAttentionDiscussion(ctx, discussion.entry.IdempotencyKey); err != nil {
			return accepted, err
		}
	}
	return accepted, nil
}

// attentionDiscussionHeld applies the same fail-closed operating-state rule
// as invocation dispatch before starting a new external inference call. A
// completion authenticated above is retirement work, not new work, and never
// reaches this gate.
func (e *Engine) attentionDiscussionHeld(ctx context.Context) (bool, error) {
	if e.admission != nil && e.admission.environment.OperatingMode == domain.ModeAttendedDev {
		return false, nil
	}
	err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		return tx.RequireUnattendedOperationOpen(ctx)
	})
	switch {
	case errors.Is(err, domain.ErrUnattendedOperationStopped),
		errors.Is(err, domain.ErrBlockingSystemHealth):
		return true, nil
	case err != nil:
		return false, err
	default:
		return false, nil
	}
}

func (e *Engine) loadAttentionDiscussion(
	ctx context.Context, entry store.QueueEntry,
) (*attentionDiscussion, error) {
	var result *attentionDiscussion
	err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		request, err := domain.DecodeConversationInvocationIntent(entry.Payload)
		if err != nil {
			return err
		}
		invocation, err := tx.GetAgentInvocation(ctx, request.InvocationID)
		if err != nil {
			return err
		}
		conversation, err := tx.GetConversation(ctx, request.ConversationID)
		if err != nil {
			return err
		}
		item, err := tx.GetAttentionItem(ctx, request.ItemID)
		if err != nil {
			return err
		}
		if entry.Kind != string(domain.AgentInvocationRequestedKind) ||
			entry.IdempotencyKey != string(request.InvocationID) ||
			invocation.ID != request.InvocationID || invocation.ConversationID == nil ||
			*invocation.ConversationID != request.ConversationID ||
			item.ID != request.ItemID || item.ConversationID == nil ||
			*item.ConversationID != request.ConversationID || invocation.ThroughSequence < 1 {
			return domain.ErrParentKeyMismatch
		}
		if _, _, err := conversation.PrefixContent(invocation.ThroughSequence); err != nil {
			return err
		}
		if conversation.Messages[invocation.ThroughSequence-1].Author != domain.AuthorUser {
			return domain.ErrParentKeyMismatch
		}
		var reply *domain.Message
		if invocation.ThroughSequence < len(conversation.Messages) {
			candidate := conversation.Messages[invocation.ThroughSequence]
			if candidate.ID != domain.MessageID("msg-agent-"+string(invocation.ID)) ||
				candidate.Author != domain.AuthorAgent || candidate.Sequence != invocation.ThroughSequence+1 {
				return domain.ErrParentKeyMismatch
			}
			if err := signet.AuthenticateAgentCompletion(ctx, tx, invocation.ID, signet.AgentReply{
				Body: candidate.Body, Attachments: candidate.Attachments,
			}); err != nil {
				return err
			}
			reply = &candidate
		} else if conversation.Status != domain.ConversationAwaitingAgent {
			return domain.ErrParentKeyMismatch
		}
		result = &attentionDiscussion{
			entry: entry, request: request, invocation: invocation,
			conversation: conversation, item: item, reply: reply,
		}
		return nil
	})
	return result, err
}

func (e *Engine) produceAttentionDiscussionReply(
	ctx context.Context, discussion attentionDiscussion,
) (string, error) {
	if discussion.item.Subject.RunID == nil || *discussion.item.Subject.RunID == "" {
		return "", domain.ErrParentKeyMismatch
	}
	_, prefix, err := discussion.conversation.PrefixContent(discussion.invocation.ThroughSequence)
	if err != nil {
		return "", err
	}
	facts := attentionDiscussionCardFacts{}
	switch discussion.item.Type {
	case domain.AttentionExecutionFailure:
		facts.ExecutionFailure = discussion.item.ExecutionFailure
		facts.ExecutionFailureReason = discussion.item.Reason
	case domain.AttentionReviewConfiguration:
		facts.ReviewConfigurationRecovery = discussion.item.ReviewConfigurationRecovery
	case domain.AttentionReviewDispute:
		facts.ReviewDispute = discussion.item.ReviewDispute
		facts.ReviewDisputeClaims = discussion.item.AgentClaims
	case domain.AttentionFindingAdjudication, domain.AttentionSpecApproval,
		domain.AttentionReviewDiminishing, domain.AttentionReviewContradiction,
		domain.AttentionAgentQuestion, domain.AttentionPublishBlocked,
		domain.AttentionReadyForFinalReview, domain.AttentionRunProposal,
		domain.AttentionSystemHealth, domain.AttentionBlocked:
		return "", domain.ErrParentKeyMismatch
	}
	cardFacts, err := json.Marshal(facts)
	if err != nil {
		return "", fmt.Errorf("encode attention discussion facts: %w", err)
	}
	if importer.ContainsSecret(prefix) || importer.ContainsSecret(cardFacts) ||
		importer.ContainsSecret([]byte(discussion.item.Reason)) {
		return unavailableAttentionDiscussionReply, nil
	}
	reply, fallback, err := e.inference.DiscussAttentionItem(ctx, inference.DiscussionInput{
		Project: string(discussion.item.ProjectID), RootLineage: string(*discussion.item.Subject.RunID),
		ItemType: string(discussion.item.Type), Reason: discussion.item.Reason,
		CardFacts: string(cardFacts), Conversation: string(prefix),
	})
	if err != nil {
		return "", err
	}
	if fallback {
		return unavailableAttentionDiscussionReply, nil
	}
	return reply, nil
}

func (e *Engine) retireAttentionDiscussion(ctx context.Context, key string) error {
	return e.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.MarkOutboxDispatched(ctx, key)
	})
}
