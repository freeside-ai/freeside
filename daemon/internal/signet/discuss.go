package signet

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// kindAgentInvocationRequested is the outbox kind of a committed discuss
// intent (plan §5.14 discuss semantics): the durable request that an agent
// turn be invoked. The dispatch loop (DispatchPendingInvocations here; the
// Wave 2 engine later) scans this kind.
const kindAgentInvocationRequested = "agent_invocation_requested"

// invocationRequest is the AgentInvocationRequested outbox payload: what a
// dispatcher needs to start the agent turn and what recovery needs to
// reconcile it (§5.14 test 5). The invocation id doubles as the row's
// idempotency key.
type invocationRequest struct {
	InvocationID   domain.InvocationID   `json:"invocation_id"`
	ConversationID domain.ConversationID `json:"conversation_id"`
	ItemID         domain.ItemID         `json:"item_id"`
	// ItemVersion is the superseding version the discuss committed; the
	// agent's completion produces the replacement for it.
	ItemVersion int `json:"item_version"`
}

// validateCommandContent is the per-action conversation-content policy the
// domain deliberately does not own (the fields are content, which actions
// require them is acceptance policy): discuss requires a non-empty message
// and every referenced attachment already stored; every other action carries
// no conversation content, and silently dropping supplied content would lose
// the user's data, so it is rejected loudly. It runs before the Write so a
// rejected command consumes no revision; attachment blobs are immutable, so
// the pre-transaction existence check cannot go stale.
func (s *Service) validateCommandContent(command domain.Command) error {
	if _, kind := actionOutcome(command.Action); kind != outcomeDiscusses {
		if command.Message != "" || len(command.Attachments) > 0 {
			return fmt.Errorf("action %q: %w", command.Action, ErrContentNotAllowed)
		}
		return nil
	}
	if command.Message == "" {
		return fmt.Errorf("action %q: %w", command.Action, ErrMessageRequired)
	}
	for _, digest := range command.Attachments {
		stored, err := s.hasAttachment(digest)
		if err != nil {
			return err
		}
		if !stored {
			return fmt.Errorf("attachment %q: %w", digest, ErrAttachmentNotStored)
		}
	}
	return nil
}

// applyDiscuss runs the discuss transaction's item-side steps inside the
// accepting Write (plan §5.14: append message → record item version and
// bindings → supersede/transition → AgentInvocationRequested outbox intent →
// command result → revision increment; the bindings-carrying command record
// was already put by Submit, and the enclosing Write's commit is the single
// revision increment). Identities derive from the accepted CommandID
// (message "msg-<id>", invocation "inv-<id>") and the item ("conv-<id>"), so
// they need no randomness and a replayed command converges on the same rows.
func (s *Service) applyDiscuss(ctx context.Context, tx *store.WriteTx, command domain.Command, item domain.AttentionItem, snap store.Snapshot) error {
	conversation := domain.Conversation{
		ID:     domain.ConversationID("conv-" + string(item.ID)),
		Status: domain.ConversationIdle,
	}
	if item.ConversationID != nil {
		existing, err := tx.GetConversation(ctx, *item.ConversationID)
		if err != nil {
			return fmt.Errorf("conversation %q: %w", *item.ConversationID, err)
		}
		conversation = existing
	}
	// One outstanding agent turn per conversation: a discuss against the
	// superseding version while the reply is still in flight would append
	// mid-turn and commit a second invocation (mid-turn steering is Phase 3,
	// plan §5.14). The 409 carries the current item so the client re-renders
	// the awaiting state and retries after the reply lands.
	if conversation.Status == domain.ConversationAwaitingAgent {
		return &AgentPendingError{CommandID: command.CommandID, Item: item, Snapshot: snap}
	}

	// The user and agent namespaces carry distinct fixed prefixes: message
	// identity derives from a client-chosen command_id here but from the
	// daemon's invocation id on completion, and without the role segment a
	// client command_id of "inv-X" would collide with the agent reply
	// "msg-inv-X" and wedge the thread on the duplicate-ID gate.
	message, err := domain.NewMessage(
		domain.MessageID("msg-user-"+command.CommandID), conversation.ID,
		domain.AuthorUser, command.Message, command.Attachments, s.now().UTC(),
	)
	if err != nil {
		return err
	}
	conversation, stamped := conversation.Append(message)
	conversation.Status = domain.ConversationAwaitingAgent
	if err := tx.PutConversation(ctx, conversation); err != nil {
		return err
	}

	// Supersede: the discussed version's rendered choices are stale, so the
	// version advances while the item stays open awaiting the agent's
	// replacement (the §5.14 test 7 single winner falls out of this bump:
	// the loser's expected_entity_version no longer matches).
	next := item
	next.ItemVersion++
	next.ConversationID = &conversation.ID
	if err := tx.PutAttentionItem(ctx, next); err != nil {
		return err
	}

	// The constructor renders canonical byte-form (input_ids [] never null),
	// so a recovery-path reconstruction of the same invocation converges on
	// the write-once row.
	invocation, err := domain.NewAgentInvocation(
		domain.InvocationID("inv-"+command.CommandID), nil, &conversation.ID, stamped.Sequence,
	)
	if err != nil {
		return err
	}
	if err := tx.PutAgentInvocation(ctx, invocation); err != nil {
		return err
	}

	payload, err := json.Marshal(invocationRequest{
		InvocationID: invocation.ID, ConversationID: conversation.ID,
		ItemID: next.ID, ItemVersion: next.ItemVersion,
	})
	if err != nil {
		return err
	}
	if _, _, err := tx.EnqueueOutbox(ctx, string(invocation.ID), kindAgentInvocationRequested, payload); err != nil {
		return err
	}
	return nil
}
