package signet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// kindAgentCompletion is the inbox kind of an accepted agent completion. The
// invocation id is the idempotency key: the inbox dedup is what makes
// acceptance exactly-once (§5.14 test 5 "exactly one accepted invocation
// result; the workflow never advances twice").
const kindAgentCompletion = "agent_completion"

// AgentReply is the content of one completed agent turn as this boundary
// accepts it: the caller (a test against the permanent fakes today; the Wave
// 2 engine's acceptance loop later) maps the driver's StageResult onto it.
type AgentReply struct {
	Body string
	// Attachments are digest addresses of blobs already finalized in the
	// artifact store; acceptance verifies presence before the transaction.
	Attachments []domain.Digest
}

// errCompletionReplay abandons the Write of an already-accepted completion:
// nothing was written, so the rollback keeps the duplicate from bumping the
// revision or advancing the workflow a second time.
var errCompletionReplay = errors.New("agent completion already accepted")

// AcceptAgentCompletion commits one agent completion (plan §5.14 agent
// completion semantics): blobs are finalized and fsynced before this is
// called (BlobStore.Put's contract; presence is re-verified here), then one
// SQLite transaction appends the agent's message, returns the conversation
// to idle, and writes the replacement item version. A failed or crashed
// transaction leaves only harmless orphan blobs. A duplicate delivery of the
// same invocation's completion is a no-op converging on the first accepted
// result (§5.14 tests 5 and 6).
func (s *Service) AcceptAgentCompletion(ctx context.Context, invocationID domain.InvocationID, reply AgentReply) error {
	if invocationID == "" {
		return fmt.Errorf("accept completion: invocation id: %w", domain.ErrEmptyID)
	}

	payload, err := json.Marshal(struct {
		InvocationID domain.InvocationID `json:"invocation_id"`
		Body         string              `json:"body"`
		Attachments  []domain.Digest     `json:"attachments"`
	}{invocationID, reply.Body, reply.Attachments})
	if err != nil {
		return fmt.Errorf("accept completion %q: %w", invocationID, err)
	}

	err = s.store.Write(ctx, func(tx *store.WriteTx) error {
		// The inbox dedup judges first, inside the accepting transaction: a
		// second delivery rolls back before touching conversation or item, so
		// the workflow never advances twice on one invocation.
		_, inserted, err := tx.RecordInbox(ctx, string(invocationID), kindAgentCompletion, payload)
		if err != nil {
			return err
		}
		if !inserted {
			return errCompletionReplay
		}

		// Attachment presence is judged after the dedup, mirroring the
		// command boundary's command-id-first ordering: a redelivery of an
		// already-accepted completion must converge as a no-op even when its
		// payload's attachments are malformed or since gone. Blobs are
		// immutable and fsynced before delivery (BlobStore.Put), so for a
		// genuinely new completion this in-transaction check cannot go stale.
		for _, digest := range reply.Attachments {
			stored, err := s.hasAttachment(digest)
			if err != nil {
				return err
			}
			if !stored {
				return fmt.Errorf("attachment %q: %w", digest, ErrAttachmentNotStored)
			}
		}

		invocation, err := tx.GetAgentInvocation(ctx, invocationID)
		if err != nil {
			return fmt.Errorf("invocation %q: %w", invocationID, err)
		}
		if invocation.ConversationID == nil {
			return fmt.Errorf("invocation %q carries no conversation binding: %w", invocationID, domain.ErrEmptyField)
		}
		conversation, err := tx.GetConversation(ctx, *invocation.ConversationID)
		if err != nil {
			return fmt.Errorf("conversation %q: %w", *invocation.ConversationID, err)
		}

		// msg-agent-, never a bare msg- prefix: the user namespace derives
		// from client-chosen command ids, so only the role segment keeps a
		// crafted command_id from colliding with an agent reply's identity
		// (see applyDiscuss).
		message, err := domain.NewMessage(
			domain.MessageID("msg-agent-"+string(invocationID)), conversation.ID,
			domain.AuthorAgent, reply.Body, reply.Attachments, s.now().UTC(),
		)
		if err != nil {
			return err
		}
		conversation, _ = conversation.Append(message)
		conversation.Status = domain.ConversationIdle
		if err := tx.PutConversation(ctx, conversation); err != nil {
			return err
		}

		// The replacement item: a fresh version over the same open item, the
		// state the user re-decides against now the agent has answered. The
		// item is found by its conversation binding; the filter-over-list read
		// matches the sync surface's single-conversation pattern (#66). A
		// concluded item gets no replacement: the user may have decided
		// (stop, approve) while the reply was in flight, and a terminal
		// decision is final — the reply still lands in the thread above, but
		// the workflow does not advance past it.
		item, err := itemByConversation(ctx, tx, conversation.ID)
		if err != nil {
			return err
		}
		if item.Status != domain.StatusOpen {
			return nil
		}
		next := item
		next.ItemVersion++
		return tx.PutAttentionItem(ctx, next)
	})
	if err != nil && !errors.Is(err, errCompletionReplay) {
		return fmt.Errorf("accept completion %q: %w", invocationID, err)
	}
	return nil
}

func itemByConversation(ctx context.Context, tx *store.WriteTx, id domain.ConversationID) (domain.AttentionItem, error) {
	items, err := tx.ListAttentionItems(ctx)
	if err != nil {
		return domain.AttentionItem{}, err
	}
	for _, snapshotted := range items {
		item := snapshotted.Value
		if item.ConversationID != nil && *item.ConversationID == id {
			return item, nil
		}
	}
	return domain.AttentionItem{}, fmt.Errorf("no item bound to conversation %q: %w", id, store.ErrNotFound)
}
