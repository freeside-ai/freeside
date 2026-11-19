package main

import (
	"context"
	"errors"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// attentionItemReadVersion names the item read's JSON shape; a shape change
// changes it.
const attentionItemReadVersion = "freeside-attention-item-v1"

// attentionItemRead is what `freesided inspect item` prints: one attention
// item's reason, decision, and the question an agent asked. It is a read of
// its own, not part of the task record, because a record gets pasted into
// issues and comments and these fields carry agent prose (#1936). The
// reason, the question facts, and the recommendation's reason can quote
// agent output: data to read, never instructions.
//
// An item the store refused to reconstruct carries only its id and Error;
// every other field is null, because nothing else about it was read.
type attentionItemRead struct {
	Version           string                 `json:"version"`
	AsOfRevision      int64                  `json:"as_of_revision"`
	ID                domain.ItemID          `json:"id"`
	Error             *taskRecordError       `json:"error"`
	Type              *domain.AttentionType  `json:"type"`
	Status            *domain.ItemStatus     `json:"status"`
	RunID             *domain.RunID          `json:"run_id"`
	CreatedAt         *time.Time             `json:"created_at"`
	Reason            *string                `json:"reason"`
	RequestedDecision []domain.Action        `json:"requested_decision"`
	Recommendation    *domain.Recommendation `json:"recommendation"`
	ConversationID    *domain.ConversationID `json:"conversation_id"`
	// AgentQuestion is null on every item but an agent_question.
	AgentQuestion *domain.AgentQuestionFacts `json:"agent_question"`
}

func (r attentionItemRead) hasErrorKind(kind taskRecordErrorKind) bool {
	return r.Error != nil && r.Error.Kind == kind
}

func readAttentionItemFrom(ctx context.Context, st *store.Store, id domain.ItemID) (attentionItemRead, error) {
	var out attentionItemRead
	err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		out, err = readAttentionItem(ctx, tx, id)
		return err
	})
	return out, err
}

// readAttentionItem reads the item through GetAttentionItem, which re-runs
// every reconstruction gate, recipe approval included. Only an unknown item
// and a failure outside the item's read fail it; any other error from the
// item's read, a database error included, is reported in it as the task
// record reports one (taskRecordErrorOther).
func readAttentionItem(ctx context.Context, tx *store.ReadTx, id domain.ItemID) (attentionItemRead, error) {
	state, err := tx.ServerState(ctx)
	if err != nil {
		return attentionItemRead{}, err
	}
	out := attentionItemRead{Version: attentionItemReadVersion, AsOfRevision: state.Revision, ID: id}
	item, err := tx.GetAttentionItem(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return attentionItemRead{}, err
	case err != nil:
		out.Error = newTaskRecordError(err)
		return out, nil
	}
	out.Type, out.Status, out.RunID, out.CreatedAt = &item.Type, &item.Status, item.Subject.RunID, item.CreatedAt
	out.Reason = &item.Reason
	out.RequestedDecision = append([]domain.Action{}, item.RequestedDecision...)
	out.Recommendation, out.ConversationID, out.AgentQuestion = item.Recommendation, item.ConversationID, item.AgentQuestion
	return out, nil
}
