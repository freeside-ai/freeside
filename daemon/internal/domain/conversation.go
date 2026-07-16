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
	// Attachments are digest addresses into the artifact store (plan §5.14
	// "text in SQLite; attachments in the artifact store by digest"). Unlike a
	// command's ArtifactDigests binding set, attachments are ordered message
	// content: order is authored, so it is preserved, never canonicalized.
	Attachments []Digest  `json:"attachments"`
	CreatedAt   time.Time `json:"created_at"`
}

// NewMessage builds an unsequenced message. It has no sequence parameter: the
// daemon assigns the sequence when the message is appended to its conversation
// (plan §5.14), so a client-supplied sequence is unrepresentable here. The
// returned message has Sequence 0 until Conversation.Append stamps it. The
// attachment slice is copied onto a non-nil base so the stored body always
// carries an array-shaped ("[]", never null) attachments field.
func NewMessage(id MessageID, conv ConversationID, author Author, body string, attachments []Digest, createdAt time.Time) (Message, error) {
	m := Message{
		ID:             id,
		ConversationID: conv,
		Author:         author,
		Body:           body,
		Attachments:    append([]Digest{}, attachments...),
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
	// Attachment entries are content addresses: empty is malformed, and the
	// same digest twice in one message is authoring noise, not a second
	// attachment. Order is content, so it is not required to be sorted.
	seen := make(map[Digest]struct{}, len(m.Attachments))
	for idx, d := range m.Attachments {
		if d == "" {
			return fmt.Errorf("message %s attachments[%d]: %w", m.ID, idx, ErrEmptyField)
		}
		if _, dup := seen[d]; dup {
			return fmt.Errorf("message %s attachments[%d] %q: %w", m.ID, idx, d, ErrDuplicate)
		}
		seen[d] = struct{}{}
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
// Status is carried on the conversation, not derived from its messages, so a
// status change is visible to a client whose message cursor is already past
// every stored sequence (plan §5.14 test 12).
type Conversation struct {
	ID       ConversationID     `json:"id"`
	Status   ConversationStatus `json:"status"`
	Messages []Message          `json:"messages"`
}

// Append returns a copy of the conversation with m added, stamping m with the
// next daemon-assigned sequence (one greater than the last message's, starting
// at 1) and its conversation id. The stamped message is returned alongside so
// callers see the assigned sequence. The receiver is not mutated, and the copy
// is deep through each message's Attachments: a shallow clone would leave the
// stored prefix, the receiver, and the caller's message sharing attachment
// backing arrays, so a later mutation of any of them could rewrite the
// supposedly immutable thread.
func (c Conversation) Append(m Message) (Conversation, Message) {
	m.ConversationID = c.ID
	m.Sequence = c.nextSequence()
	// Detach the returned message from the caller's array, then the stored
	// copy from the returned one: three distinct backing arrays.
	m.Attachments = append([]Digest{}, m.Attachments...)
	stored := m
	stored.Attachments = append([]Digest{}, m.Attachments...)
	messages := make([]Message, 0, len(c.Messages)+1)
	for _, prev := range c.Messages {
		prev.Attachments = slices.Clone(prev.Attachments)
		messages = append(messages, prev)
	}
	next := Conversation{
		ID:       c.ID,
		Status:   c.Status,
		Messages: append(messages, stored),
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
	if !c.Status.valid() {
		return fmt.Errorf("conversation %s status %q: %w", c.ID, c.Status, ErrInvalidConversationStatus)
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

// AgentInvocation binds an agent turn to the explicit immutable inputs it was
// given (plan §5.14): input artifact IDs, a conversation's immutable message
// prefix, or both — never live state — so a run is reproducible from its
// recorded inputs. A discuss invocation (plan §5.14 discuss semantics) binds
// the conversation and the sequence it was launched through: messages 1..N are
// immutable, so (ConversationID, ThroughSequence) is a content binding of the
// same standing as an artifact ID.
type AgentInvocation struct {
	ID       InvocationID `json:"id"`
	InputIDs []ArtifactID `json:"input_ids"`
	// ConversationID and ThroughSequence bind the immutable thread prefix the
	// invocation was launched against; nil/0 for invocations bound by input
	// artifacts alone. Pointer-for-optional renders explicit null on the wire.
	ConversationID  *ConversationID `json:"conversation_id"`
	ThroughSequence int             `json:"through_sequence"`
}

// NewAgentInvocation builds a validated invocation in canonical byte-form:
// InputIDs is copied onto a non-nil base so the write-once record always
// renders an array-shaped ("[]", never null) input_ids, and a retried
// construction of the same semantic invocation converges on the stored body
// instead of colliding under a false immutable conflict (the #33
// canonical-body lesson; store.PutAgentInvocation compares bytes). The copy
// also detaches the record from the caller's backing array.
func NewAgentInvocation(id InvocationID, inputIDs []ArtifactID, conversationID *ConversationID, throughSequence int) (AgentInvocation, error) {
	inv := AgentInvocation{
		ID:              id,
		InputIDs:        append([]ArtifactID{}, inputIDs...),
		ConversationID:  clonePtr(conversationID),
		ThroughSequence: throughSequence,
	}
	if err := inv.Validate(); err != nil {
		return AgentInvocation{}, err
	}
	return inv, nil
}

// Validate reports whether the invocation is well-formed. It is the
// reconstruction backstop for records that did not pass NewAgentInvocation
// (store decode, struct literals); canonical array-shape is the
// constructor's job, not an invariant of every decoded value.
func (a AgentInvocation) Validate() error {
	if a.ID == "" {
		return fmt.Errorf("agent invocation id: %w", ErrEmptyID)
	}
	// An invocation binds the immutable inputs it ran against so the run is
	// reproducible (plan §5.14); bound to neither artifacts nor a conversation
	// prefix it is bound to nothing.
	if len(a.InputIDs) == 0 && a.ConversationID == nil {
		return fmt.Errorf("agent invocation %s: %w", a.ID, ErrUnboundInvocation)
	}
	for idx, in := range a.InputIDs {
		if in == "" {
			return fmt.Errorf("agent invocation %s input_ids[%d]: %w", a.ID, idx, ErrEmptyID)
		}
	}
	if a.ConversationID != nil {
		if *a.ConversationID == "" {
			return fmt.Errorf("agent invocation %s conversation_id: %w", a.ID, ErrEmptyID)
		}
		if a.ThroughSequence < 1 {
			return fmt.Errorf("agent invocation %s through_sequence %d: %w", a.ID, a.ThroughSequence, ErrNonPositiveSeq)
		}
	} else if a.ThroughSequence != 0 {
		// A sequence without its conversation binds nothing; a stray value is
		// a malformed record, not a weaker binding.
		return fmt.Errorf("agent invocation %s through_sequence %d without conversation_id: %w", a.ID, a.ThroughSequence, ErrInvocationInconsistent)
	}
	return nil
}
