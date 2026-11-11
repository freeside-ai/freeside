package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const (
	goldenOfferItemID       domain.ItemID         = "execution-failure-golden-offers"
	goldenConversationID    domain.ConversationID = "conversation-golden"
	goldenReviewInvocation  domain.InvocationID   = "review-golden"
	goldenConversationReply                       = "agent reply text that only the conversations read prints"
)

// addRecordV15Facts adds to the representative task what the seed lacks and
// v15 reads: two versions of the reviewer's line, a review admission that
// cites the second, and an execution-failure card that offers an alternate
// agent and carries a two-message conversation.
func addRecordV15Facts(ctx context.Context, tx *store.WriteTx, id domain.TaskID) error {
	run, err := tx.GetRun(ctx, fixtureActiveRunID)
	if err != nil {
		return err
	}
	// The seeded failure card's facts bind to a recorded outcome, which a
	// card's facts must.
	failure, err := tx.GetAttentionItem(ctx, "execution-failure-"+fixtureActiveRunID)
	if err != nil {
		return err
	}
	if _, err := tx.AppendTaskLine(ctx, domain.TaskLineInput{
		TaskID: id, Role: domain.RoleReviewer, Agent: "reviewer-golden",
		Source: domain.TaskLineSourceCLISubmit, SetBy: "submission-golden",
	}, taskRecordAsOf); err != nil {
		return err
	}
	line, err := tx.AppendTaskLine(ctx, domain.TaskLineInput{
		TaskID: id, Role: domain.RoleReviewer,
		Selection: &domain.TaskLineSelection{
			EnrollmentID: "anthropic-a/claude_code", ModelID: "claude-opus-5-5", Effort: "max",
		},
		Source: domain.TaskLineSourceRouter, SetBy: "router-golden",
		Provenance: &domain.TaskLineProvenance{
			PolicyRevision: 1, Tier: domain.TaskLineSelectionPolicyOrder, Pool: "identity:anthropic-a",
			RankedBy: []domain.TaskLineRankingReading{},
		},
	}, taskRecordAsOf.Add(time.Minute))
	if err != nil {
		return err
	}
	digest := func(fill string) domain.Digest { return domain.Digest("sha256:" + strings.Repeat(fill, 64)) }
	review, err := domain.NewReviewAdmission(domain.ReviewAdmission{
		InvocationID: goldenReviewInvocation, RunID: fixtureActiveRunID, Role: domain.RoleReviewer,
		AuthIdentityID: "auth-golden", LineageGroup: "anthropic",
		AgentBinding: domain.AdmissionAgentBinding{
			AgentDigest: digest("a"), LaunchDigest: digest("b"), TreatmentDigest: digest("d"),
			PricingRevision: "pricing-2026-01", LineupRevision: digest("c"),
			EnrollmentID: "anthropic-a/claude_code", EnrollmentGeneration: 2, StoreManifestDigest: digest("9"),
			EffectiveEgress: []string{"api.anthropic.com:443"},
			RouteModelID:    "claude-opus-5-5", RequestedEffort: domain.EffortMax, NativeEffort: "max",
			SelectionSource: domain.AgentSelectionSourceTaskLine, SelectionRecordID: line.ID,
			SelectionDigest: digest("e"), SelectionModelID: "claude-opus-5-5", SelectionEffort: "max",
			HarnessClient: domain.HarnessClientClaudeCode, LineageGroup: "anthropic",
		},
		AdmittedAt: taskRecordAsOf.Add(2 * time.Minute),
	})
	if err != nil {
		return err
	}
	if err := tx.PutReviewAdmission(ctx, review); err != nil {
		return err
	}
	conversation := domain.Conversation{ID: goldenConversationID, Status: domain.ConversationIdle, Messages: []domain.Message{}}
	for index, entry := range []struct {
		author domain.Author
		body   string
	}{
		{domain.AuthorUser, "why did the implementer stop?"},
		{domain.AuthorAgent, goldenConversationReply},
	} {
		message, err := domain.NewMessage(domain.MessageID("message-golden-"+string(rune('a'+index))),
			goldenConversationID, entry.author, entry.body, []domain.Digest{digest("f")},
			taskRecordAsOf.Add(time.Duration(3+index)*time.Minute))
		if err != nil {
			return err
		}
		conversation, _ = conversation.Append(message)
	}
	if err := tx.PutConversation(ctx, conversation); err != nil {
		return err
	}
	runID, createdAt, conversationID := run.ID, taskRecordAsOf, goldenConversationID
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: goldenOfferItemID, ProjectID: run.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionExecutionFailure, Priority: domain.PriorityHigh,
		Reason: "implementation failed",
		RequestedDecision: []domain.Action{
			domain.ActionRetryWithAlternateAgent, domain.ActionDiscuss, domain.ActionStop,
		},
		ExecutionFailure: failure.ExecutionFailure,
		AlternateAgents: []domain.AlternateAgentOffer{{
			Enrollment: "anthropic-b/claude_code", Pool: "identity:anthropic-b",
			Harness: domain.HarnessClientClaudeCode, Model: "claude-opus-5-5",
			Qualification: domain.AlternateAgentDifferentPool,
		}},
		ConversationID: &conversationID,
		ItemVersion:    1, InterruptionClass: domain.InterruptionExceptional,
		CreatedAt: &createdAt, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		return err
	}
	return tx.PutAttentionItem(ctx, item)
}

func readConversations(t *testing.T, st *store.Store, id domain.TaskID) taskConversations {
	t.Helper()
	var conversations taskConversations
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		conversations, err = readTaskConversations(t.Context(), tx, id, taskRecordAsOf)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return conversations
}

// TestTaskConversationsGolden pins the read's shape. A change to these bytes
// is a change to taskConversationsVersion.
func TestTaskConversationsGolden(t *testing.T) {
	t.Parallel()
	st, _ := openSeededStore(t)
	id := taskOfRun(t, st, fixtureActiveRunID)
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		return addRecordV15Facts(t.Context(), tx, id)
	}); err != nil {
		t.Fatal(err)
	}
	conversations := readConversations(t, st, id)
	if conversations.Version != taskConversationsVersion {
		t.Fatalf("version = %q", conversations.Version)
	}
	if len(conversations.Conversations) != 1 {
		t.Fatalf("conversations = %+v, want the golden card's alone", conversations.Conversations)
	}
	got, err := json.MarshalIndent(conversations, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = bytes.ReplaceAll(got, []byte(id), []byte("task-GOLDEN"))
	golden.Assert(t, "task_conversations", append(got, '\n'))

	// The record names the conversation and never carries its text.
	record := readRecord(t, st, id)
	item := recordItem(record, goldenOfferItemID)
	if item == nil || item.ConversationID == nil || *item.ConversationID != goldenConversationID {
		t.Fatalf("record item = %+v, want it to name %s", item, goldenConversationID)
	}
	if out := mustJSON(t, record); bytes.Contains(out, []byte(goldenConversationReply)) {
		t.Fatal("the task record carries conversation text")
	}
}

// TestTaskConversationsReportsAnUnreadableConversation breaks the
// conversation row: the item keeps its ids and carries the error, and the
// read itself succeeds.
func TestTaskConversationsReportsAnUnreadableConversation(t *testing.T) {
	t.Parallel()
	st, dbPath := openSeededStore(t)
	id := taskOfRun(t, st, fixtureActiveRunID)
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		return addRecordV15Facts(t.Context(), tx, id)
	}); err != nil {
		t.Fatal(err)
	}
	execStoreSQL(t, dbPath, `UPDATE conversations SET body = '{}' WHERE id = '`+string(goldenConversationID)+`'`)
	conversations := readConversations(t, st, id)
	if len(conversations.Conversations) != 1 {
		t.Fatalf("conversations = %+v", conversations.Conversations)
	}
	got := conversations.Conversations[0]
	if got.ItemID != goldenOfferItemID || got.ConversationID == nil || got.Error == nil ||
		got.Status != nil || got.Messages != nil {
		t.Fatalf("conversation = %+v, want ids and an error only", got)
	}
}

func TestTaskConversationsUnknownTask(t *testing.T) {
	t.Parallel()
	st, _ := openSeededStore(t)
	err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		_, err := readTaskConversations(t.Context(), tx, "task-missing", taskRecordAsOf)
		return err
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want %v", err, store.ErrNotFound)
	}
}
