package domain_test

import (
	"encoding/json"
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
	m1, err := domain.NewMessage("m1", "conv-1", domain.AuthorUser, "hello", nil, at)
	if err != nil {
		t.Fatal(err)
	}
	if m1.Sequence != 0 {
		t.Fatalf("NewMessage produced sequence %d, want 0 (unassigned until appended)", m1.Sequence)
	}
	if m1.Attachments == nil {
		t.Error("NewMessage left attachments nil; the stored body must carry an array-shaped field")
	}
	// An unsequenced message is not yet a valid stored message.
	if err := m1.Validate(); err == nil {
		t.Error("unsequenced message validated; a stored message needs a daemon-assigned sequence")
	}

	conv := domain.Conversation{ID: "conv-1", Status: domain.ConversationIdle}
	conv, stamped1 := conv.Append(m1)
	if stamped1.Sequence != 1 {
		t.Fatalf("first appended sequence = %d, want 1", stamped1.Sequence)
	}

	m2, _ := domain.NewMessage("m2", "conv-1", domain.AuthorAgent, "reply", nil, at.Add(time.Minute))
	convAfter, stamped2 := conv.Append(m2)
	if stamped2.Sequence != 2 {
		t.Fatalf("second appended sequence = %d, want 2", stamped2.Sequence)
	}
	// Append returns a copy: the earlier conversation value is unchanged.
	if len(conv.Messages) != 1 {
		t.Errorf("Append mutated the receiver; len = %d, want 1", len(conv.Messages))
	}
	// Append carries the conversation's status onto the copy.
	if convAfter.Status != domain.ConversationIdle {
		t.Errorf("Append dropped status; got %q, want %q", convAfter.Status, domain.ConversationIdle)
	}
	if err := convAfter.Validate(); err != nil {
		t.Errorf("appended conversation invalid: %v", err)
	}
}

func TestNewMessageRejects(t *testing.T) {
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	if _, err := domain.NewMessage("m", "c", "narrator", "x", nil, at); err == nil {
		t.Error("invalid author accepted")
	}
	if _, err := domain.NewMessage("", "c", domain.AuthorUser, "x", nil, at); err == nil {
		t.Error("empty id accepted")
	}
	if _, err := domain.NewMessage("m", "c", domain.AuthorUser, "x", nil, time.Time{}); err == nil {
		t.Error("zero created_at accepted")
	}
	if _, err := domain.NewMessage("m", "c", domain.AuthorUser, "x", []domain.Digest{""}, at); !errors.Is(err, domain.ErrEmptyField) {
		t.Error("empty attachment digest accepted")
	}
	if _, err := domain.NewMessage("m", "c", domain.AuthorUser, "x", []domain.Digest{"sha256:a", "sha256:a"}, at); !errors.Is(err, domain.ErrDuplicate) {
		t.Error("duplicate attachment digest accepted")
	}
	// Attachment order is authored content: an unsorted set is valid.
	if _, err := domain.NewMessage("m", "c", domain.AuthorUser, "x", []domain.Digest{"sha256:b", "sha256:a"}, at); err != nil {
		t.Errorf("unsorted attachments rejected: %v", err)
	}
}

// TestAppendDetachesAttachmentBackingArrays: Append's copy must be deep
// through each message's Attachments, or the stored prefix, the receiver, and
// the caller's message value share backing arrays and a later mutation of any
// of them rewrites the supposedly immutable thread.
func TestAppendDetachesAttachmentBackingArrays(t *testing.T) {
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	caller := []domain.Digest{"sha256:a"}
	m1, err := domain.NewMessage("m1", "conv-1", domain.AuthorUser, "x", caller, at)
	if err != nil {
		t.Fatal(err)
	}
	conv, stamped1 := domain.Conversation{ID: "conv-1", Status: domain.ConversationIdle}.Append(m1)

	// Mutating the caller's slice, the message value NewMessage returned, and
	// the stamped copy must not reach the conversation's stored thread.
	caller[0] = "sha256:tampered"
	m1.Attachments[0] = "sha256:tampered"
	stamped1.Attachments[0] = "sha256:tampered"
	if got := conv.Messages[0].Attachments[0]; got != "sha256:a" {
		t.Fatalf("stored attachment = %q, want sha256:a (backing array shared)", got)
	}

	// The receiver's stored prefix must be detached from the appended copy's.
	m2, err := domain.NewMessage("m2", "conv-1", domain.AuthorAgent, "y", nil, at.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	after, _ := conv.Append(m2)
	conv.Messages[0].Attachments[0] = "sha256:tampered"
	if got := after.Messages[0].Attachments[0]; got != "sha256:a" {
		t.Fatalf("appended prefix attachment = %q, want sha256:a (prefix shares backing array)", got)
	}
}

// TestConversationRejectsDuplicateMessageID checks message identity is unique
// within a conversation, so retries converge on one immutable message.
func TestConversationRejectsDuplicateMessageID(t *testing.T) {
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	conv := domain.Conversation{ID: "c", Status: domain.ConversationIdle, Messages: []domain.Message{
		{ID: "m1", ConversationID: "c", Sequence: 1, Author: domain.AuthorUser, Body: "a", CreatedAt: at},
		{ID: "m1", ConversationID: "c", Sequence: 2, Author: domain.AuthorAgent, Body: "b", CreatedAt: at},
	}}
	if err := conv.Validate(); !errors.Is(err, domain.ErrDuplicate) {
		t.Fatalf("duplicate message id accepted: %v", err)
	}
}

// TestConversationStatusRequired checks the status enum gate: the zero value
// and an unknown member are invalid, every registered member is valid (§5.14
// test 12 needs a status to change).
func TestConversationStatusRequired(t *testing.T) {
	if err := (domain.Conversation{ID: "c"}).Validate(); !errors.Is(err, domain.ErrInvalidConversationStatus) {
		t.Errorf("zero-status conversation accepted: %v", err)
	}
	if err := (domain.Conversation{ID: "c", Status: "paused"}).Validate(); !errors.Is(err, domain.ErrInvalidConversationStatus) {
		t.Errorf("unknown status accepted: %v", err)
	}
	for _, s := range domain.AllConversationStatuses {
		if err := (domain.Conversation{ID: "c", Status: s}).Validate(); err != nil {
			t.Errorf("registered status %q rejected: %v", s, err)
		}
	}
}

// TestNewAgentInvocationCanonicalInputs: the write-once record's byte-form
// must not depend on whether the caller passed nil or an empty slice; the
// constructor renders input_ids array-shaped either way, so a retried
// construction converges on the stored body (store.PutAgentInvocation
// compares bytes).
func TestNewAgentInvocationCanonicalInputs(t *testing.T) {
	conv := domain.ConversationID("conv-1")
	fromNil, err := domain.NewAgentInvocation("inv-1", nil, &conv, 2)
	if err != nil {
		t.Fatal(err)
	}
	fromEmpty, err := domain.NewAgentInvocation("inv-1", []domain.ArtifactID{}, &conv, 2)
	if err != nil {
		t.Fatal(err)
	}
	if fromNil.InputIDs == nil {
		t.Error("constructor left InputIDs nil; the record would render input_ids null")
	}
	a, err := json.Marshal(fromNil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(fromEmpty)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Errorf("nil and empty inputs render different byte-forms:\n%s\n%s", a, b)
	}
}

func TestAgentInvocationValidate(t *testing.T) {
	conv := domain.ConversationID("conv-1")
	empty := domain.ConversationID("")
	cases := []struct {
		name string
		inv  domain.AgentInvocation
		want error // nil means valid
	}{
		{"inputs only", domain.AgentInvocation{ID: "inv-1", InputIDs: []domain.ArtifactID{"a", "b"}}, nil},
		{"conversation only", domain.AgentInvocation{ID: "inv-1", ConversationID: &conv, ThroughSequence: 1}, nil},
		{"inputs and conversation", domain.AgentInvocation{ID: "inv-1", InputIDs: []domain.ArtifactID{"a"}, ConversationID: &conv, ThroughSequence: 3}, nil},
		{"no id", domain.AgentInvocation{}, domain.ErrEmptyID},
		{"empty input id", domain.AgentInvocation{ID: "inv-1", InputIDs: []domain.ArtifactID{""}}, domain.ErrEmptyID},
		{"bound to nothing", domain.AgentInvocation{ID: "inv-1"}, domain.ErrUnboundInvocation},
		{"empty conversation id", domain.AgentInvocation{ID: "inv-1", ConversationID: &empty, ThroughSequence: 1}, domain.ErrEmptyID},
		{"conversation without sequence", domain.AgentInvocation{ID: "inv-1", ConversationID: &conv}, domain.ErrNonPositiveSeq},
		{"sequence without conversation", domain.AgentInvocation{ID: "inv-1", InputIDs: []domain.ArtifactID{"a"}, ThroughSequence: 2}, domain.ErrInvocationInconsistent},
	}
	for _, tc := range cases {
		err := tc.inv.Validate()
		if tc.want == nil {
			if err != nil {
				t.Errorf("%s: valid invocation rejected: %v", tc.name, err)
			}
			continue
		}
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, err, tc.want)
		}
	}
}
