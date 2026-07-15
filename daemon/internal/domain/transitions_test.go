package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// The transition validators are the store guards lifted into the domain (issue
// #21); these tests mirror the store's guard tests (TestRunFixedBindingsAndHistory,
// TestConversationAppendOnly, TestAttentionItemFixedBindings,
// TestAttentionItemStaleWriteRejected, TestDeliveryLifecycleForwardOnly),
// asserting the domain error classes the store maps onto its own conflict/stale
// errors.

// TestValidateRunTransition covers the fixed run bindings (project, spec,
// policy) and append-only stage/attempt history: an append succeeds, any change
// to a fixed binding or recorded history is an immutable-transition conflict.
func TestValidateRunTransition(t *testing.T) {
	if err := domain.ValidateRunTransition(validRun(), appendedRun()); err != nil {
		t.Fatalf("appending an attempt rejected: %v", err)
	}
	if err := domain.ValidateRunTransition(validRun(), validRun()); err != nil {
		t.Fatalf("identical run rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*domain.Run)
	}{
		{"identity changes", func(r *domain.Run) { r.ID = "run-other" }},
		{"project changes", func(r *domain.Run) { r.ProjectID = "proj-other" }},
		{"spec digest changes", func(r *domain.Run) { r.SpecDigest = "sha256:other" }},
		{"policy digest changes", func(r *domain.Run) { r.PolicyDigest = "sha256:other" }},
		{"stage dropped", func(r *domain.Run) { r.Stages = nil }},
		{"stage identity changes", func(r *domain.Run) { r.Stages[0].ID = "stage-other" }},
		{"stage name changes", func(r *domain.Run) { r.Stages[0].Name = "review" }},
		{"recorded attempt changes", func(r *domain.Run) { r.Stages[0].Attempts[0].InvocationID = "inv-other" }},
		{"attempt dropped", func(r *domain.Run) { r.Stages[0].Attempts = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			updated := validRun()
			tt.mutate(&updated)
			if err := domain.ValidateRunTransition(validRun(), updated); !errors.Is(err, domain.ErrImmutableTransition) {
				t.Fatalf("ValidateRunTransition() = %v, want ErrImmutableTransition", err)
			}
		})
	}
}

// appendedRun is validRun with one appended attempt on its stage: a legal
// forward step that preserves all recorded history.
func appendedRun() domain.Run {
	r := validRun()
	r.Stages[0].Attempts = append(r.Stages[0].Attempts,
		domain.Attempt{ID: "attempt-2", StageID: "stage-1", Number: 2, InvocationID: "inv-2"})
	return r
}

// TestValidateConversationTransition covers append-only messages: appending is
// legal, dropping or rewriting any stored message is an immutable-transition
// conflict.
func TestValidateConversationTransition(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	m1 := mustMessage(t, "m1", "conv-1", domain.AuthorUser, "first", at)
	m2 := mustMessage(t, "m2", "conv-1", domain.AuthorAgent, "second", at.Add(time.Minute))

	old, _ := domain.Conversation{ID: "conv-1"}.Append(m1)
	appended, _ := old.Append(m2)
	if err := domain.ValidateConversationTransition(old, appended); err != nil {
		t.Fatalf("appending a message rejected: %v", err)
	}
	if err := domain.ValidateConversationTransition(old, old); err != nil {
		t.Fatalf("identical conversation rejected: %v", err)
	}

	t.Run("identity changes", func(t *testing.T) {
		other := domain.Conversation{ID: "conv-other", Messages: old.Messages}
		if err := domain.ValidateConversationTransition(old, other); !errors.Is(err, domain.ErrImmutableTransition) {
			t.Fatalf("changing conversation id = %v, want ErrImmutableTransition", err)
		}
	})
	t.Run("messages dropped", func(t *testing.T) {
		if err := domain.ValidateConversationTransition(appended, old); !errors.Is(err, domain.ErrImmutableTransition) {
			t.Fatalf("dropping a message = %v, want ErrImmutableTransition", err)
		}
	})
	t.Run("stored message rewritten", func(t *testing.T) {
		rewritten := domain.Conversation{ID: "conv-1", Messages: []domain.Message{old.Messages[0]}}
		rewritten.Messages[0].Body = "tampered"
		if err := domain.ValidateConversationTransition(old, rewritten); !errors.Is(err, domain.ErrImmutableTransition) {
			t.Fatalf("rewriting a stored message = %v, want ErrImmutableTransition", err)
		}
	})
}

func mustMessage(t *testing.T, id domain.MessageID, conv domain.ConversationID, author domain.Author, body string, at time.Time) domain.Message {
	t.Helper()
	m, err := domain.NewMessage(id, conv, author, body, at)
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	return m
}

// TestValidateAttentionItemFixedBindings covers the fixed subject/project/type:
// bumping the version to evolve status is legal, changing what the item is about
// is an immutable-transition conflict.
func TestValidateAttentionItemFixedBindings(t *testing.T) {
	old := mustItem(t, validItemInput(domain.AttentionSpecApproval))

	resolved := validItemInput(domain.AttentionSpecApproval)
	resolved.Status = domain.StatusResolved
	resolved.ItemVersion = 2
	if err := domain.ValidateAttentionItemTransition(old, mustItem(t, resolved)); err != nil {
		t.Fatalf("status transition rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*domain.AttentionItemInput)
	}{
		{"identity changes", func(in *domain.AttentionItemInput) { in.ID = "item-other" }},
		{"project changes", func(in *domain.AttentionItemInput) { in.ProjectID = "proj-other" }},
		{"type changes", func(in *domain.AttentionItemInput) { in.Type = domain.AttentionExecutionFailure }},
		{"subject id changes", func(in *domain.AttentionItemInput) { in.Subject.ID = "run-other" }},
		{"subject type changes", func(in *domain.AttentionItemInput) {
			in.Subject = domain.Subject{Type: domain.SubjectProject, ID: "proj-1"}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := validItemInput(domain.AttentionSpecApproval)
			in.ItemVersion = 2 // advance the version so only the fixed binding is at fault
			tt.mutate(&in)
			if err := domain.ValidateAttentionItemTransition(old, mustItem(t, in)); !errors.Is(err, domain.ErrImmutableTransition) {
				t.Fatalf("ValidateAttentionItemTransition() = %v, want ErrImmutableTransition", err)
			}
		})
	}
}

// TestValidateAttentionItemStaleWrite covers item_version monotonicity: a changed
// body that does not advance the version is a stale transition.
func TestValidateAttentionItemStaleWrite(t *testing.T) {
	v2in := validItemInput(domain.AttentionSpecApproval)
	v2in.Status = domain.StatusResolved
	v2in.ItemVersion = 2
	old := mustItem(t, v2in)

	t.Run("older version", func(t *testing.T) {
		stale := mustItem(t, validItemInput(domain.AttentionSpecApproval)) // version 1
		if err := domain.ValidateAttentionItemTransition(old, stale); !errors.Is(err, domain.ErrStaleTransition) {
			t.Fatalf("stale v1 over v2 = %v, want ErrStaleTransition", err)
		}
	})
	t.Run("same version, changed body", func(t *testing.T) {
		sameVersion := validItemInput(domain.AttentionSpecApproval)
		sameVersion.Status = domain.StatusDismissed
		sameVersion.ItemVersion = 2
		if err := domain.ValidateAttentionItemTransition(old, mustItem(t, sameVersion)); !errors.Is(err, domain.ErrStaleTransition) {
			t.Fatalf("same-version changed body = %v, want ErrStaleTransition", err)
		}
	})
}

// TestValidateAttentionItemStatusTerminality covers the status lifecycle: open
// may move to any terminal status, a same-status update may still advance the
// version, and no terminal status admits a successor (issue #55: resolved→open
// at an advanced version used to pass, reopening a decided item).
func TestValidateAttentionItemStatusTerminality(t *testing.T) {
	for _, from := range domain.AllItemStatuses {
		for _, to := range domain.AllItemStatuses {
			t.Run(string(from)+" to "+string(to), func(t *testing.T) {
				oldIn := validItemInput(domain.AttentionSpecApproval)
				oldIn.Status = from
				oldIn.ItemVersion = 2
				updatedIn := validItemInput(domain.AttentionSpecApproval)
				updatedIn.Status = to
				updatedIn.ItemVersion = 3
				err := domain.ValidateAttentionItemTransition(mustItem(t, oldIn), mustItem(t, updatedIn))
				if from == to || from == domain.StatusOpen {
					if err != nil {
						t.Fatalf("legal status move rejected: %v", err)
					}
					return
				}
				if !errors.Is(err, domain.ErrImmutableTransition) {
					t.Fatalf("ValidateAttentionItemTransition() = %v, want ErrImmutableTransition", err)
				}
			})
		}
	}

	t.Run("resolved reopened at advanced version", func(t *testing.T) {
		resolvedIn := validItemInput(domain.AttentionSpecApproval)
		resolvedIn.Status = domain.StatusResolved
		resolvedIn.ItemVersion = 2
		reopenedIn := validItemInput(domain.AttentionSpecApproval)
		reopenedIn.ItemVersion = 3 // version advances, so only the status move is at fault
		err := domain.ValidateAttentionItemTransition(mustItem(t, resolvedIn), mustItem(t, reopenedIn))
		if !errors.Is(err, domain.ErrImmutableTransition) {
			t.Fatalf("reopening a resolved item = %v, want ErrImmutableTransition", err)
		}
	})
}

func mustItem(t *testing.T, in domain.AttentionItemInput) domain.AttentionItem {
	t.Helper()
	item, err := domain.NewAttentionItem(in, nil)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	return item
}

// TestValidateAttentionDeliveryLifecycle covers the forward-only delivery
// lifecycle: advancing the status while preserving receipts is legal, a status
// regression is a stale transition, and rewriting a recorded receipt is an
// immutable-transition conflict.
func TestValidateAttentionDeliveryLifecycle(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	accepted := at.Add(time.Minute)
	opened := at.Add(5 * time.Minute)

	submitted := domain.AttentionDelivery{
		ItemID: "item-1", DeviceID: "dev-1", Channel: "ntfy", Attempt: 1,
		SubmittedAt: at, Status: domain.DeliverySubmitted,
	}
	acceptedDelivery := submitted
	acceptedDelivery.Status = domain.DeliveryChannelAccepted
	acceptedDelivery.ChannelAcceptedAt = ptr(accepted)

	if err := domain.ValidateAttentionDeliveryTransition(submitted, acceptedDelivery); err != nil {
		t.Fatalf("advancing to channel-accepted rejected: %v", err)
	}

	openedDelivery := acceptedDelivery
	openedDelivery.Status = domain.DeliveryOpened
	openedDelivery.OpenedAt = ptr(opened)
	if err := domain.ValidateAttentionDeliveryTransition(acceptedDelivery, openedDelivery); err != nil {
		t.Fatalf("advancing to opened rejected: %v", err)
	}

	t.Run("identity changes", func(t *testing.T) {
		other := openedDelivery
		other.Attempt = 2 // a different delivery attempt, not a successor
		if err := domain.ValidateAttentionDeliveryTransition(acceptedDelivery, other); !errors.Is(err, domain.ErrImmutableTransition) {
			t.Fatalf("changing the delivery key = %v, want ErrImmutableTransition", err)
		}
	})
	t.Run("status regresses", func(t *testing.T) {
		if err := domain.ValidateAttentionDeliveryTransition(acceptedDelivery, submitted); !errors.Is(err, domain.ErrStaleTransition) {
			t.Fatalf("regression = %v, want ErrStaleTransition", err)
		}
	})
	t.Run("same rank does not advance", func(t *testing.T) {
		if err := domain.ValidateAttentionDeliveryTransition(submitted, submitted); !errors.Is(err, domain.ErrStaleTransition) {
			t.Fatalf("same status = %v, want ErrStaleTransition", err)
		}
	})
	t.Run("recorded receipt rewritten", func(t *testing.T) {
		rewritten := openedDelivery
		rewritten.Status = domain.DeliveryOpened
		rewritten.ChannelAcceptedAt = ptr(accepted.Add(time.Hour))
		// A same-rank rewrite would trip the stale guard first; advance from
		// channel-accepted so only the receipt rewrite is at fault.
		if err := domain.ValidateAttentionDeliveryTransition(acceptedDelivery, rewritten); !errors.Is(err, domain.ErrImmutableTransition) {
			t.Fatalf("receipt rewrite = %v, want ErrImmutableTransition", err)
		}
	})
}
