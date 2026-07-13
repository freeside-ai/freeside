package domain

import (
	"fmt"
	"slices"
	"time"
)

// Message is one immutable turn in a conversation (plan §5.14). Its Sequence is
// daemon-assigned by Conversation.Append, never supplied by a caller: NewMessage
// has no sequence parameter, and a message is only sequenced once appended.
// Corrections are new messages, never edits of an existing one.
type Message struct {
	ID             MessageID      `json:"id"`
	ConversationID ConversationID `json:"conversation_id"`
	Sequence       int            `json:"sequence"`
	Author         Author         `json:"author"`
	Body           string         `json:"body"`
	CreatedAt      time.Time      `json:"created_at"`
}

// NewMessage builds an unsequenced message. It has no sequence parameter: the
// daemon assigns the sequence when the message is appended to its conversation
// (plan §5.14), so a client-supplied sequence is unrepresentable here. The
// returned message has Sequence 0 until Conversation.Append stamps it.
func NewMessage(id MessageID, conv ConversationID, author Author, body string, createdAt time.Time) (Message, error) {
	m := Message{
		ID:             id,
		ConversationID: conv,
		Author:         author,
		Body:           body,
		CreatedAt:      createdAt,
	}
	if err := m.validateUnsequenced(); err != nil {
		return Message{}, err
	}
	return m, nil
}

func (m Message) validateUnsequenced() error {
	if m.ID == "" {
		return fmt.Errorf("message id: %w", ErrEmptyID)
	}
	if m.ConversationID == "" {
		return fmt.Errorf("message conversation_id: %w", ErrEmptyID)
	}
	if !m.Author.valid() {
		return fmt.Errorf("message author %q: %w", m.Author, ErrInvalidAuthor)
	}
	if m.CreatedAt.IsZero() {
		return fmt.Errorf("message %s created_at: %w", m.ID, ErrMissingTimestamp)
	}
	return nil
}

// Validate reports whether the message is a well-formed, sequenced message.
func (m Message) Validate() error {
	if err := m.validateUnsequenced(); err != nil {
		return err
	}
	if m.Sequence < 1 {
		return fmt.Errorf("message %s sequence %d: %w", m.ID, m.Sequence, ErrNonPositiveSeq)
	}
	return nil
}

// Conversation is an ordered, append-only thread of messages (plan §5.14).
type Conversation struct {
	ID       ConversationID `json:"id"`
	Messages []Message      `json:"messages"`
}

// Append returns a copy of the conversation with m added, stamping m with the
// next daemon-assigned sequence (one greater than the last message's, starting
// at 1) and its conversation id. The stamped message is returned alongside so
// callers see the assigned sequence. The receiver is not mutated.
func (c Conversation) Append(m Message) (Conversation, Message) {
	m.ConversationID = c.ID
	m.Sequence = c.nextSequence()
	next := Conversation{
		ID:       c.ID,
		Messages: append(slices.Clone(c.Messages), m),
	}
	return next, m
}

func (c Conversation) nextSequence() int {
	if len(c.Messages) == 0 {
		return 1
	}
	return c.Messages[len(c.Messages)-1].Sequence + 1
}

// Validate reports whether the conversation is well-formed: contiguous
// sequences starting at 1, every message valid and owned by this conversation.
func (c Conversation) Validate() error {
	if c.ID == "" {
		return fmt.Errorf("conversation id: %w", ErrEmptyID)
	}
	seen := make(map[MessageID]struct{}, len(c.Messages))
	for idx, m := range c.Messages {
		if err := m.Validate(); err != nil {
			return err
		}
		if m.ConversationID != c.ID {
			return fmt.Errorf("message %s conversation_id: %w", m.ID, ErrEmptyID)
		}
		if want := idx + 1; m.Sequence != want {
			return fmt.Errorf("message %s sequence %d, want %d: %w", m.ID, m.Sequence, want, ErrNonPositiveSeq)
		}
		// Message identity is immutable and load-bearing for retry convergence,
		// so a conversation may not carry the same MessageID twice.
		if _, dup := seen[m.ID]; dup {
			return fmt.Errorf("message id %s: %w", m.ID, ErrDuplicate)
		}
		seen[m.ID] = struct{}{}
	}
	return nil
}

// AgentInvocation binds an agent turn to the explicit input artifact IDs it was
// given (plan §5.14): invocations reference immutable input IDs, not live
// state, so a run is reproducible from its recorded inputs.
type AgentInvocation struct {
	ID       InvocationID `json:"id"`
	InputIDs []ArtifactID `json:"input_ids"`
}

// Validate reports whether the invocation is well-formed.
func (a AgentInvocation) Validate() error {
	if a.ID == "" {
		return fmt.Errorf("agent invocation id: %w", ErrEmptyID)
	}
	// An invocation binds the immutable inputs it ran against so the run is
	// reproducible (plan §5.14); with no inputs it is bound to nothing.
	if len(a.InputIDs) == 0 {
		return fmt.Errorf("agent invocation %s input_ids: %w", a.ID, ErrEmptyField)
	}
	for idx, in := range a.InputIDs {
		if in == "" {
			return fmt.Errorf("agent invocation %s input_ids[%d]: %w", a.ID, idx, ErrEmptyID)
		}
	}
	return nil
}
