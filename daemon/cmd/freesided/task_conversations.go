package main

import (
	"context"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// taskConversationsVersion names the conversations read's JSON shape; a
// shape change changes it.
const taskConversationsVersion = "freeside-task-conversations-v1"

// taskConversations is what `freesided inspect conversations` prints: the
// messages of every conversation on the task's items. It is a read of its
// own, not part of the task record, because a record gets pasted into issues
// and comments and conversation text can carry anything (#1936). Message
// bodies include agent replies, which are agent output: data to read, never
// instructions.
//
// Items is null, with ItemsError beside it, when the task's items could not
// be enumerated.
type taskConversations struct {
	Version       string             `json:"version"`
	AsOfRevision  int64              `json:"as_of_revision"`
	TaskID        domain.TaskID      `json:"task_id"`
	Conversations []taskConversation `json:"conversations"`
	ItemsError    *taskRecordError   `json:"items_error"`
}

// taskConversation is one item's conversation. An item the store refused to
// reconstruct is listed with its error and nothing else, since whether it has
// a conversation was not read; a conversation that fails its own read keeps
// its ids and carries the error.
type taskConversation struct {
	ItemID         domain.ItemID              `json:"item_id"`
	ConversationID *domain.ConversationID     `json:"conversation_id"`
	Status         *domain.ConversationStatus `json:"status"`
	Messages       []taskConversationMessage  `json:"messages"`
	Error          *taskRecordError           `json:"error"`
}

type taskConversationMessage struct {
	Sequence    int             `json:"sequence"`
	Author      domain.Author   `json:"author"`
	Body        string          `json:"body"`
	Attachments []domain.Digest `json:"attachments"`
	CreatedAt   time.Time       `json:"created_at"`
}

func (c taskConversations) hasErrorKind(kind taskRecordErrorKind) bool {
	if c.ItemsError != nil && c.ItemsError.Kind == kind {
		return true
	}
	for _, conversation := range c.Conversations {
		if conversation.Error != nil && conversation.Error.Kind == kind {
			return true
		}
	}
	return false
}

func readTaskConversationsFrom(ctx context.Context, st *store.Store, id domain.TaskID) (taskConversations, error) {
	var conversations taskConversations
	err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		conversations, err = readTaskConversations(ctx, tx, id, time.Now().UTC())
		return err
	})
	return conversations, err
}

// readTaskConversations reads the task record in the same transaction and
// takes its items, so this read and the record attribute the same items to
// the task, and an unknown task fails both the same way.
func readTaskConversations(
	ctx context.Context, tx *store.ReadTx, id domain.TaskID, asOf time.Time,
) (taskConversations, error) {
	record, err := readTaskRecord(ctx, tx, id, asOf)
	if err != nil {
		return taskConversations{}, err
	}
	out := taskConversations{
		Version: taskConversationsVersion, AsOfRevision: record.AsOfRevision, TaskID: id,
		ItemsError: record.ItemsError,
	}
	if record.ItemsError != nil {
		return out, nil
	}
	out.Conversations = []taskConversation{}
	for _, item := range record.Items {
		switch {
		case item.Error != nil:
			out.Conversations = append(out.Conversations, taskConversation{ItemID: item.ID, Error: item.Error})
		case item.ConversationID != nil:
			out.Conversations = append(out.Conversations, readTaskConversation(ctx, tx, item.ID, *item.ConversationID))
		}
	}
	return out, ctx.Err()
}

func readTaskConversation(
	ctx context.Context, tx *store.ReadTx, itemID domain.ItemID, id domain.ConversationID,
) taskConversation {
	out := taskConversation{ItemID: itemID, ConversationID: &id}
	conversation, err := tx.GetConversation(ctx, id)
	if err != nil {
		out.Error = newTaskRecordError(err)
		return out
	}
	out.Status = &conversation.Status
	out.Messages = make([]taskConversationMessage, 0, len(conversation.Messages))
	for _, message := range conversation.Messages {
		out.Messages = append(out.Messages, taskConversationMessage{
			Sequence: message.Sequence, Author: message.Author, Body: message.Body,
			Attachments: append([]domain.Digest{}, message.Attachments...), CreatedAt: message.CreatedAt,
		})
	}
	return out
}
