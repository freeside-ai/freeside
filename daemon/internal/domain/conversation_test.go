package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestMessageSequenceDaemonAssigned is acceptance criterion 6: message sequence
// is daemon-assigned. NewMessage takes no sequence (a fresh message is
// unsequenced, sequence 0), and Conversation.Append stamps a monotonic sequence
// starting at 1 without mutating the receiver.
func TestMessageSequenceDaemonAssigned(t *testing.T) {
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	m1, err := domain.NewMessage("m1", "conv-1", domain.AuthorUser, "hello", at)
	if err != nil {
		t.Fatal(err)
	}
	if m1.Sequence != 0 {
		t.Fatalf("NewMessage produced sequence %d, want 0 (unassigned until appended)", m1.Sequence)
	}
	// An unsequenced message is not yet a valid stored message.
	if err := m1.Validate(); err == nil {
		t.Error("unsequenced message validated; a stored message needs a daemon-assigned sequence")
	}

	conv := domain.Conversation{ID: "conv-1"}
	conv, stamped1 := conv.Append(m1)
	if stamped1.Sequence != 1 {
		t.Fatalf("first appended sequence = %d, want 1", stamped1.Sequence)
	}

	m2, _ := domain.NewMessage("m2", "conv-1", domain.AuthorAgent, "reply", at.Add(time.Minute))
	convAfter, stamped2 := conv.Append(m2)
	if stamped2.Sequence != 2 {
		t.Fatalf("second appended sequence = %d, want 2", stamped2.Sequence)
	}
	// Append returns a copy: the earlier conversation value is unchanged.
	if len(conv.Messages) != 1 {
		t.Errorf("Append mutated the receiver; len = %d, want 1", len(conv.Messages))
	}
	if err := convAfter.Validate(); err != nil {
		t.Errorf("appended conversation invalid: %v", err)
	}
}

func TestNewMessageRejects(t *testing.T) {
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	if _, err := domain.NewMessage("m", "c", "narrator", "x", at); err == nil {
		t.Error("invalid author accepted")
	}
	if _, err := domain.NewMessage("", "c", domain.AuthorUser, "x", at); err == nil {
		t.Error("empty id accepted")
	}
	if _, err := domain.NewMessage("m", "c", domain.AuthorUser, "x", time.Time{}); err == nil {
		t.Error("zero created_at accepted")
	}
}

// TestConversationRejectsDuplicateMessageID checks message identity is unique
// within a conversation, so retries converge on one immutable message.
func TestConversationRejectsDuplicateMessageID(t *testing.T) {
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	conv := domain.Conversation{ID: "c", Messages: []domain.Message{
		{ID: "m1", ConversationID: "c", Sequence: 1, Author: domain.AuthorUser, Body: "a", CreatedAt: at},
		{ID: "m1", ConversationID: "c", Sequence: 2, Author: domain.AuthorAgent, Body: "b", CreatedAt: at},
	}}
	if err := conv.Validate(); !errors.Is(err, domain.ErrDuplicate) {
		t.Fatalf("duplicate message id accepted: %v", err)
	}
}

func TestAgentInvocationValidate(t *testing.T) {
	if err := (domain.AgentInvocation{ID: "inv-1", InputIDs: []domain.ArtifactID{"a", "b"}}).Validate(); err != nil {
		t.Fatalf("valid invocation rejected: %v", err)
	}
	if err := (domain.AgentInvocation{}).Validate(); err == nil {
		t.Error("invocation without id accepted")
	}
	if err := (domain.AgentInvocation{ID: "inv-1", InputIDs: []domain.ArtifactID{""}}).Validate(); !errors.Is(err, domain.ErrEmptyID) {
		t.Errorf("invocation with an empty input id accepted")
	}
	if err := (domain.AgentInvocation{ID: "inv-1"}).Validate(); !errors.Is(err, domain.ErrEmptyField) {
		t.Errorf("invocation with no inputs accepted (breaks reproducibility)")
	}
}
