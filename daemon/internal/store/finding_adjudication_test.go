package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func adjudicationDigest(component string) domain.Digest {
	return domain.Digest("sha256:" + strings.Repeat(component, 64/len(component)))
}

// adjSpecDigest, adjPolicyDigest, and adjInstructionDigest are the authoritative
// binding values for an adjudication artifact: the run's spec and policy digests
// and the review round's instruction binding. The artifact must carry the same
// values or the store's binding rejects it, so the seed run, the seed review
// record, and the fixtures share them.
var (
	adjSpecDigest        = adjudicationDigest("a")
	adjPolicyDigest      = adjudicationDigest("c")
	adjInstructionDigest = adjudicationDigest("d")
)

func adjudicationEngineEntry(t *testing.T, id domain.FindingID) domain.FindingAdjudicationEntry {
	t.Helper()
	// A deterministic fast-path fact: required goal, presumptive-allowed
	// compatibility, remediation route — the single engine-producible row (the
	// no-model fast path is one-directional toward remediation; see the domain
	// contract's engine-entry restriction).
	entry, err := domain.NewEngineAdjudicationEntry(
		id, domain.GoalRequired, ptrCompat(domain.CompatibilityAllowed), domain.RouteRemediate,
		"in declared scope", nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("engine entry %q: %v", id, err)
	}
	return entry
}

func ptrCompat(c domain.WorkUnitCompatibility) *domain.WorkUnitCompatibility { return &c }

func newAdjudication(
	t *testing.T, runID domain.RunID, round int, findingIDs []domain.FindingID, createdAt time.Time,
) domain.FindingAdjudication {
	t.Helper()
	entries := make([]domain.FindingAdjudicationEntry, 0, len(findingIDs))
	for _, id := range findingIDs {
		entries = append(entries, adjudicationEngineEntry(t, id))
	}
	artifact, err := domain.NewFindingAdjudication(
		runID, round,
		adjSpecDigest, adjInstructionDigest, adjPolicyDigest,
		entries, createdAt)
	if err != nil {
		t.Fatalf("new adjudication: %v", err)
	}
	return artifact
}

func adjudicationConversation(
	t *testing.T, id domain.ConversationID, bodies []string, at time.Time,
) domain.Conversation {
	t.Helper()
	conversation := domain.Conversation{ID: id, Status: domain.ConversationIdle}
	for i, body := range bodies {
		message, err := domain.NewMessage(
			domain.MessageID("message-"+strconv.Itoa(i+1)), id, domain.AuthorUser,
			body, nil, at.Add(time.Duration(i)*time.Minute),
		)
		if err != nil {
			t.Fatalf("new message %d: %v", i+1, err)
		}
		conversation, _ = conversation.Append(message)
	}
	if err := conversation.Validate(); err != nil {
		t.Fatalf("conversation: %v", err)
	}
	return conversation
}

func adjudicationFeedback(
	t *testing.T, conversation domain.Conversation, invocationID domain.InvocationID, through int,
) (domain.AgentInvocation, domain.AdjudicationFeedback) {
	t.Helper()
	id := conversation.ID
	invocation, err := domain.NewAgentInvocation(invocationID, nil, &id, through)
	if err != nil {
		t.Fatalf("new invocation: %v", err)
	}
	digest, _, err := conversation.PrefixContent(through)
	if err != nil {
		t.Fatalf("conversation prefix: %v", err)
	}
	return invocation, domain.AdjudicationFeedback{
		InvocationID: invocation.ID, ConversationID: conversation.ID,
		ThroughSequence: through, PrefixDigest: digest,
	}
}

func putAdjudicationDispatchAuthority(
	t *testing.T, ctx context.Context, tx *store.WriteTx,
	predecessor domain.FindingAdjudication, conversation domain.Conversation,
	invocation domain.AgentInvocation, itemID domain.ItemID, itemVersion int,
) (domain.AttentionItem, error) {
	t.Helper()
	if err := tx.PutConversation(ctx, conversation); err != nil {
		return domain.AttentionItem{}, err
	}
	if err := tx.PutAgentInvocation(ctx, invocation); err != nil {
		return domain.AttentionItem{}, err
	}
	item := adjudicationItem(t, itemID, bindingFromAdjudication(predecessor))
	item.ItemVersion = itemVersion
	item.ConversationID = &conversation.ID
	if err := tx.PutAttentionItem(ctx, item); err != nil {
		return domain.AttentionItem{}, err
	}
	payload, err := json.Marshal(domain.ConversationInvocationIntent{
		InvocationID: invocation.ID, ConversationID: conversation.ID,
		ItemID: item.ID, ItemVersion: itemVersion,
	})
	if err != nil {
		return domain.AttentionItem{}, err
	}
	_, _, err = tx.EnqueueOutbox(
		ctx, string(invocation.ID), string(domain.AgentInvocationRequestedKind), payload,
	)
	return item, err
}

func putAdjudicationFeedbackAuthority(
	t *testing.T, ctx context.Context, tx *store.WriteTx,
	predecessor domain.FindingAdjudication, conversation domain.Conversation,
	invocation domain.AgentInvocation, itemID domain.ItemID, itemVersion int,
) error {
	t.Helper()
	item, err := putAdjudicationDispatchAuthority(
		t, ctx, tx, predecessor, conversation, invocation, itemID, itemVersion,
	)
	if err != nil {
		return err
	}
	replyBody := "reply for " + string(invocation.ID)
	completionPayload, err := json.Marshal(struct {
		InvocationID domain.InvocationID `json:"invocation_id"`
		Body         string              `json:"body"`
		Attachments  []domain.Digest     `json:"attachments"`
	}{invocation.ID, replyBody, nil})
	if err != nil {
		return err
	}
	if _, _, err := tx.RecordInbox(
		ctx, string(invocation.ID), "agent_completion", completionPayload,
	); err != nil {
		return err
	}
	reply, err := domain.NewMessage(
		domain.MessageID("msg-agent-"+string(invocation.ID)), conversation.ID,
		domain.AuthorAgent, replyBody, nil,
		conversation.Messages[len(conversation.Messages)-1].CreatedAt.Add(time.Second),
	)
	if err != nil {
		return err
	}
	conversation, _ = conversation.Append(reply)
	conversation.Status = domain.ConversationIdle
	if err := tx.PutConversation(ctx, conversation); err != nil {
		return err
	}
	item.ItemVersion++
	return tx.PutAttentionItem(ctx, item)
}

func successorAdjudication(
	t *testing.T, prior domain.FindingAdjudication, feedback domain.AdjudicationFeedback,
	rationale string, at time.Time,
) domain.FindingAdjudication {
	t.Helper()
	entries := slices.Clone(prior.Entries)
	entries[0].Rationale = rationale
	artifact, err := domain.NewSuccessorFindingAdjudication(prior, feedback, entries, at)
	if err != nil {
		t.Fatalf("new successor: %v", err)
	}
	return artifact
}

func resignAdjudication(t *testing.T, artifact domain.FindingAdjudication) domain.FindingAdjudication {
	t.Helper()
	digest, err := artifact.ComputeDigest()
	if err != nil {
		t.Fatalf("compute adjudication digest: %v", err)
	}
	artifact.Digest = digest
	return artifact
}

func adjudicationReviewRecord(
	t *testing.T, runID domain.RunID, round int, findingIDs []domain.FindingID, completedAt time.Time,
) domain.ReviewRecord {
	t.Helper()
	record, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: domain.InvocationID("review-" + string(runID) + "-" + strconv.Itoa(round)),
		RunID:        runID, Round: round, Provider: "openai", ModelConfiguration: "gpt-codex/high",
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		InstructionDigest:   adjInstructionDigest,
		CostOwner:           "owner", BaseSHA: "base", HeadSHA: "head-" + strconv.Itoa(round), CompletedAt: completedAt,
		CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("e", 64)),
		Outcome:            domain.ReviewFindings, FindingIDs: findingIDs,
	})
	if err != nil {
		t.Fatalf("review record round %d: %v", round, err)
	}
	return record
}

func adjudicationFinding(id domain.FindingID, runID domain.RunID, path string, at time.Time) domain.Finding {
	return domain.Finding{
		ID: id, RunID: runID, Source: "codex_local",
		Location: &domain.FindingLocation{Path: path, StartLine: 1, EndLine: 1},
		Message:  "finding " + string(id), RawText: "finding " + string(id), CreatedAt: at,
	}
}

// seedReviewRound opens a store, seeds a run and one review round with the given
// findings, and returns the store for adjudication writes.
func seedReviewRound(
	t *testing.T, runID domain.RunID, round int, findings []domain.Finding, at time.Time,
) *store.Store {
	t.Helper()
	return seedReviewRoundAt(t, filepath.Join(t.TempDir(), "store.db"), runID, round, findings, at)
}

func seedReviewRoundAt(
	t *testing.T, path string, runID domain.RunID, round int, findings []domain.Finding, at time.Time,
) *store.Store {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ids := make([]domain.FindingID, 0, len(findings))
	for _, f := range findings {
		ids = append(ids, f.ID)
	}
	record := adjudicationReviewRecord(t, runID, round, ids, at)
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, domain.Run{
			ID: runID, ProjectID: "project-1", SpecDigest: adjSpecDigest, PolicyDigest: adjPolicyDigest,
		}); err != nil {
			return err
		}
		return tx.PutReviewRecord(ctx, record, findings)
	}); err != nil {
		t.Fatalf("seed review round: %v", err)
	}
	return st
}

func TestFindingAdjudicationRoundTripAndReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-adj")
	at := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	findings := []domain.Finding{
		adjudicationFinding("finding-a", runID, "daemon/a.go", at),
		adjudicationFinding("finding-b", runID, "daemon/b.go", at),
	}
	st := seedReviewRound(t, runID, 1, findings, at)

	artifact := newAdjudication(t, runID, 1, []domain.FindingID{"finding-b", "finding-a"}, at)

	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutFindingAdjudication(ctx, artifact)
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	// A byte-identical replay converges.
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutFindingAdjudication(ctx, artifact)
	}); err != nil {
		t.Fatalf("replay: %v", err)
	}

	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		byDigest, err := tx.GetFindingAdjudication(ctx, artifact.Digest)
		if err != nil {
			return err
		}
		if byDigest.Digest != artifact.Digest {
			t.Fatalf("get by digest returned %q", byDigest.Digest)
		}
		byRound, err := tx.GetFindingAdjudicationForRound(ctx, runID, 1)
		if err != nil {
			return err
		}
		if byRound.Digest != artifact.Digest {
			t.Fatalf("get by round returned %q", byRound.Digest)
		}
		list, err := tx.ListFindingAdjudications(ctx, runID)
		if err != nil {
			return err
		}
		if len(list) != 1 || list[0].Digest != artifact.Digest {
			t.Fatalf("list returned %d artifacts", len(list))
		}
		return nil
	}); err != nil {
		t.Fatalf("read: %v", err)
	}
}

func TestFindingAdjudicationImmutableConflict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-conflict")
	at := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	findings := []domain.Finding{adjudicationFinding("finding-a", runID, "daemon/a.go", at)}
	st := seedReviewRound(t, runID, 1, findings, at)

	first := newAdjudication(t, runID, 1, []domain.FindingID{"finding-a"}, at)
	// Same round, same finding set, different content (later timestamp).
	second := newAdjudication(t, runID, 1, []domain.FindingID{"finding-a"}, at.Add(time.Minute))
	if first.Digest == second.Digest {
		t.Fatal("fixtures share a digest; conflict test is vacuous")
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutFindingAdjudication(ctx, first)
	}); err != nil {
		t.Fatalf("put first: %v", err)
	}
	err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutFindingAdjudication(ctx, second)
	})
	if !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("conflicting put = %v, want ErrImmutableConflict", err)
	}
}

func TestFindingAdjudicationRevisionHistoryAndReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-revisions")
	at := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	st := seedReviewRound(t, runID, 1,
		[]domain.Finding{adjudicationFinding("finding-a", runID, "daemon/a.go", at)}, at)
	conversationOne := adjudicationConversation(t, "conversation-revisions-1", []string{"first"}, at)
	conversationTwo := adjudicationConversation(t, "conversation-revisions-2", []string{"second"}, at)
	invocationOne, feedbackOne := adjudicationFeedback(t, conversationOne, "invocation-revision-2", 1)
	invocationTwo, feedbackTwo := adjudicationFeedback(t, conversationTwo, "invocation-revision-3", 1)
	initial := newAdjudication(t, runID, 1, []domain.FindingID{"finding-a"}, at)
	revisionTwo := successorAdjudication(t, initial, feedbackOne, "first revision", at.Add(time.Minute))
	revisionThree := successorAdjudication(t, revisionTwo, feedbackTwo, "second revision", at.Add(2*time.Minute))

	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutFindingAdjudication(ctx, initial); err != nil {
			return err
		}
		if err := putAdjudicationFeedbackAuthority(
			t, ctx, tx, initial, conversationOne, invocationOne, "item-revision-2", 2,
		); err != nil {
			return err
		}
		if err := tx.PutFindingAdjudication(ctx, revisionTwo); err != nil {
			return err
		}
		if err := putAdjudicationFeedbackAuthority(
			t, ctx, tx, revisionTwo, conversationTwo, invocationTwo, "item-revision-3", 2,
		); err != nil {
			return err
		}
		return tx.PutFindingAdjudication(ctx, revisionThree)
	}); err != nil {
		t.Fatalf("put history: %v", err)
	}
	// Historical reconstruction accepts a later item version because the
	// transition contract preserves the predecessor binding while versions only
	// advance. New successor insertion still requires the dispatch's exact
	// version.
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		item, err := tx.GetAttentionItem(ctx, "item-revision-2")
		if err != nil {
			return err
		}
		item.ItemVersion++
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatalf("advance historical item: %v", err)
	}
	// A historical successor replay converges even after the head advances.
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutFindingAdjudication(ctx, revisionTwo)
	}); err != nil {
		t.Fatalf("replay revision two: %v", err)
	}

	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		head, err := tx.GetFindingAdjudicationForRound(ctx, runID, 1)
		if err != nil {
			return err
		}
		if head.Digest != revisionThree.Digest {
			t.Fatalf("head = %q, want revision three %q", head.Digest, revisionThree.Digest)
		}
		for _, historical := range []domain.FindingAdjudication{initial, revisionTwo} {
			got, err := tx.GetFindingAdjudication(ctx, historical.Digest)
			if err != nil {
				return err
			}
			if got.Digest != historical.Digest {
				t.Fatalf("historical digest = %q, want %q", got.Digest, historical.Digest)
			}
		}
		history, err := tx.ListFindingAdjudications(ctx, runID)
		if err != nil {
			return err
		}
		if len(history) != 3 || history[0].Revision != 1 || history[1].Revision != 2 || history[2].Revision != 3 {
			t.Fatalf("history revisions = %+v", history)
		}
		return nil
	}); err != nil {
		t.Fatalf("read history: %v", err)
	}
}

func TestFindingAdjudicationSuccessorRejections(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-successor-rejections")
	at := time.Date(2026, 8, 25, 11, 0, 0, 0, time.UTC)
	st := seedReviewRound(t, runID, 1,
		[]domain.Finding{adjudicationFinding("finding-a", runID, "daemon/a.go", at)}, at)
	conversation := adjudicationConversation(t, "conversation-rejections", []string{"first"}, at)
	invocation, feedback := adjudicationFeedback(t, conversation, "invocation-rejections", 1)
	unrequestedConversation := adjudicationConversation(t, "conversation-unrequested", []string{"unrequested"}, at)
	unrequestedInvocation, unrequestedFeedback := adjudicationFeedback(
		t, unrequestedConversation, "invocation-unrequested", 1,
	)
	staleConversation := adjudicationConversation(t, "conversation-stale-item", []string{"stale"}, at)
	staleInvocation, staleFeedback := adjudicationFeedback(t, staleConversation, "invocation-stale-item", 1)
	pendingConversation := adjudicationConversation(t, "conversation-pending-item", []string{"pending"}, at)
	pendingInvocation, pendingFeedback := adjudicationFeedback(t, pendingConversation, "invocation-pending-item", 1)
	initial := newAdjudication(t, runID, 1, []domain.FindingID{"finding-a"}, at)
	valid := successorAdjudication(t, initial, feedback, "successor", at.Add(time.Minute))
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutFindingAdjudication(ctx, initial); err != nil {
			return err
		}
		if err := putAdjudicationFeedbackAuthority(
			t, ctx, tx, initial, conversation, invocation, "item-rejections", 2,
		); err != nil {
			return err
		}
		if err := tx.PutAgentInvocation(ctx, unrequestedInvocation); err != nil {
			return err
		}
		if err := putAdjudicationFeedbackAuthority(
			t, ctx, tx, initial, staleConversation, staleInvocation, "item-stale-version", 2,
		); err != nil {
			return err
		}
		if _, err := putAdjudicationDispatchAuthority(
			t, ctx, tx, initial, pendingConversation, pendingInvocation, "item-pending-completion", 2,
		); err != nil {
			return err
		}
		item, err := tx.GetAttentionItem(ctx, "item-stale-version")
		if err != nil {
			return err
		}
		item.ItemVersion++
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatalf("seed successor authorities: %v", err)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*domain.FindingAdjudication)
		want   error
	}{
		{"skipped revision", func(a *domain.FindingAdjudication) { a.Revision = 3 }, store.ErrImmutableConflict},
		{"stale parent", func(a *domain.FindingAdjudication) {
			digest := adjudicationDigest("f")
			a.PredecessorDigest = &digest
		}, store.ErrImmutableConflict},
		{"changed approved spec", func(a *domain.FindingAdjudication) { a.ApprovedSpecDigest = adjudicationDigest("f") }, domain.ErrParentKeyMismatch},
		{"changed instruction snapshot", func(a *domain.FindingAdjudication) { a.InstructionSnapshotDigest = adjudicationDigest("f") }, domain.ErrParentKeyMismatch},
		{"changed resolved policy", func(a *domain.FindingAdjudication) { a.ResolvedPolicyDigest = adjudicationDigest("f") }, domain.ErrParentKeyMismatch},
		{"changed finding batch", func(a *domain.FindingAdjudication) { a.FindingBatchDigest = adjudicationDigest("f") }, domain.ErrFindingAdjudicationInconsistent},
		{"missing invocation", func(a *domain.FindingAdjudication) { a.Feedback.InvocationID = "missing-invocation" }, domain.ErrParentKeyMismatch},
		{"foreign conversation", func(a *domain.FindingAdjudication) { a.Feedback.ConversationID = "foreign-conversation" }, domain.ErrParentKeyMismatch},
		{"foreign sequence", func(a *domain.FindingAdjudication) { a.Feedback.ThroughSequence = 2 }, domain.ErrParentKeyMismatch},
		{"wrong prefix digest", func(a *domain.FindingAdjudication) { a.Feedback.PrefixDigest = adjudicationDigest("f") }, domain.ErrParentKeyMismatch},
		{"missing dispatch intent", func(a *domain.FindingAdjudication) { *a.Feedback = unrequestedFeedback }, domain.ErrParentKeyMismatch},
		{"missing accepted completion", func(a *domain.FindingAdjudication) { *a.Feedback = pendingFeedback }, domain.ErrParentKeyMismatch},
		{"stale item version", func(a *domain.FindingAdjudication) { *a.Feedback = staleFeedback }, domain.ErrParentKeyMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			artifact := valid
			feedbackCopy := *artifact.Feedback
			artifact.Feedback = &feedbackCopy
			tc.mutate(&artifact)
			artifact = resignAdjudication(t, artifact)
			err := st.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutFindingAdjudication(ctx, artifact)
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("put = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestFindingAdjudicationRejectsFeedbackFromUnrelatedDiscuss(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	at := time.Date(2026, 8, 25, 11, 20, 0, 0, time.UTC)
	targetRunID := domain.RunID("run-feedback-target")
	targetFinding := adjudicationFinding("finding-feedback-target", targetRunID, "daemon/target.go", at)
	st := seedReviewRound(t, targetRunID, 1, []domain.Finding{targetFinding}, at)
	target := newAdjudication(t, targetRunID, 1, []domain.FindingID{targetFinding.ID}, at)

	foreignRunID := domain.RunID("run-feedback-foreign")
	foreignFinding := adjudicationFinding("finding-feedback-foreign", foreignRunID, "daemon/foreign.go", at)
	foreignRecord := adjudicationReviewRecord(t, foreignRunID, 1, []domain.FindingID{foreignFinding.ID}, at)
	foreign := newAdjudication(t, foreignRunID, 1, []domain.FindingID{foreignFinding.ID}, at)
	conversation := adjudicationConversation(t, "conversation-feedback-foreign", []string{"reconsider"}, at)
	invocation, feedback := adjudicationFeedback(t, conversation, "invocation-feedback-foreign", 1)
	successor := successorAdjudication(t, target, feedback, "foreign feedback", at.Add(time.Minute))

	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, domain.Run{
			ID: foreignRunID, ProjectID: "project-1",
			SpecDigest: adjSpecDigest, PolicyDigest: adjPolicyDigest,
		}); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, foreignRecord, []domain.Finding{foreignFinding}); err != nil {
			return err
		}
		if err := tx.PutFindingAdjudication(ctx, target); err != nil {
			return err
		}
		if err := tx.PutFindingAdjudication(ctx, foreign); err != nil {
			return err
		}
		return putAdjudicationFeedbackAuthority(
			t, ctx, tx, foreign, conversation, invocation, "item-feedback-foreign", 2,
		)
	}); err != nil {
		t.Fatalf("seed foreign Discuss authority: %v", err)
	}

	err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutFindingAdjudication(ctx, successor)
	})
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("foreign Discuss feedback = %v, want ErrParentKeyMismatch", err)
	}
}

func TestFindingAdjudicationRejectsForeignPredecessor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-foreign-predecessor")
	at := time.Date(2026, 8, 25, 11, 30, 0, 0, time.UTC)
	finding := adjudicationFinding("finding-a", runID, "daemon/a.go", at)
	st := seedReviewRound(t, runID, 1, []domain.Finding{finding}, at)
	target := newAdjudication(t, runID, 1, []domain.FindingID{finding.ID}, at)
	roundTwoRecord := adjudicationReviewRecord(t, runID, 2, []domain.FindingID{finding.ID}, at.Add(time.Minute))
	roundTwo := newAdjudication(t, runID, 2, []domain.FindingID{finding.ID}, at.Add(time.Minute))
	otherRunID := domain.RunID("run-other-predecessor")
	otherFinding := adjudicationFinding("finding-other", otherRunID, "daemon/other.go", at)
	otherRecord := adjudicationReviewRecord(t, otherRunID, 1, []domain.FindingID{otherFinding.ID}, at)
	otherRun := newAdjudication(t, otherRunID, 1, []domain.FindingID{otherFinding.ID}, at)
	conversation := adjudicationConversation(t, "conversation-foreign-predecessor", []string{"feedback"}, at)
	invocation, feedback := adjudicationFeedback(t, conversation, "invocation-foreign-predecessor", 1)
	valid := successorAdjudication(t, target, feedback, "successor", at.Add(2*time.Minute))

	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, domain.Run{
			ID: otherRunID, ProjectID: "project-1", SpecDigest: adjSpecDigest, PolicyDigest: adjPolicyDigest,
		}); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, roundTwoRecord, []domain.Finding{finding}); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, otherRecord, []domain.Finding{otherFinding}); err != nil {
			return err
		}
		for _, artifact := range []domain.FindingAdjudication{target, roundTwo, otherRun} {
			if err := tx.PutFindingAdjudication(ctx, artifact); err != nil {
				return err
			}
		}
		return putAdjudicationFeedbackAuthority(
			t, ctx, tx, target, conversation, invocation, "item-foreign-predecessor", 2,
		)
	}); err != nil {
		t.Fatalf("seed predecessor histories: %v", err)
	}
	for _, tc := range []struct {
		name        string
		predecessor domain.Digest
	}{
		{"cross-round", roundTwo.Digest},
		{"cross-run", otherRun.Digest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			artifact := valid
			artifact.PredecessorDigest = &tc.predecessor
			artifact = resignAdjudication(t, artifact)
			err := st.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutFindingAdjudication(ctx, artifact)
			})
			if !errors.Is(err, store.ErrImmutableConflict) {
				t.Fatalf("foreign predecessor = %v, want ErrImmutableConflict", err)
			}
		})
	}
}

func TestFindingAdjudicationConcurrentSuccessor(t *testing.T) {
	ctx := context.Background()
	runID := domain.RunID("run-concurrent-successor")
	at := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	st := seedReviewRound(t, runID, 1,
		[]domain.Finding{adjudicationFinding("finding-a", runID, "daemon/a.go", at)}, at)
	conversationOne := adjudicationConversation(t, "conversation-concurrent-1", []string{"first"}, at)
	conversationTwo := adjudicationConversation(t, "conversation-concurrent-2", []string{"second"}, at)
	invocationOne, feedbackOne := adjudicationFeedback(t, conversationOne, "invocation-concurrent-1", 1)
	invocationTwo, feedbackTwo := adjudicationFeedback(t, conversationTwo, "invocation-concurrent-2", 1)
	initial := newAdjudication(t, runID, 1, []domain.FindingID{"finding-a"}, at)
	first := successorAdjudication(t, initial, feedbackOne, "first contender", at.Add(time.Minute))
	second := successorAdjudication(t, initial, feedbackTwo, "second contender", at.Add(time.Minute))
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutFindingAdjudication(ctx, initial); err != nil {
			return err
		}
		if err := putAdjudicationFeedbackAuthority(
			t, ctx, tx, initial, conversationOne, invocationOne, "item-concurrent-1", 2,
		); err != nil {
			return err
		}
		return putAdjudicationFeedbackAuthority(
			t, ctx, tx, initial, conversationTwo, invocationTwo, "item-concurrent-2", 2,
		)
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	for _, artifact := range []domain.FindingAdjudication{first, second} {
		go func() {
			<-start
			results <- st.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutFindingAdjudication(ctx, artifact)
			})
		}()
	}
	close(start)
	errOne, errTwo := <-results, <-results
	if (errOne == nil) == (errTwo == nil) {
		t.Fatalf("concurrent results = %v, %v; want one success", errOne, errTwo)
	}
	loser := errOne
	if loser == nil {
		loser = errTwo
	}
	if !errors.Is(loser, store.ErrImmutableConflict) {
		t.Fatalf("losing successor = %v, want ErrImmutableConflict", loser)
	}
}

func TestFindingAdjudicationRevisionHistorySurvivesRestore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-revision-restore")
	at := time.Date(2026, 8, 25, 14, 0, 0, 0, time.UTC)
	st := seedReviewRound(t, runID, 1,
		[]domain.Finding{adjudicationFinding("finding-a", runID, "daemon/a.go", at)}, at)
	conversationOne := adjudicationConversation(t, "conversation-restore-1", []string{"first"}, at)
	conversationTwo := adjudicationConversation(t, "conversation-restore-2", []string{"second"}, at)
	invocationOne, feedbackOne := adjudicationFeedback(t, conversationOne, "invocation-restore-1", 1)
	invocationTwo, feedbackTwo := adjudicationFeedback(t, conversationTwo, "invocation-restore-2", 1)
	initial := newAdjudication(t, runID, 1, []domain.FindingID{"finding-a"}, at)
	revisionTwo := successorAdjudication(t, initial, feedbackOne, "before checkpoint", at.Add(time.Minute))
	revisionThree := successorAdjudication(t, revisionTwo, feedbackTwo, "after checkpoint", at.Add(2*time.Minute))
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutFindingAdjudication(ctx, initial); err != nil {
			return err
		}
		if err := putAdjudicationFeedbackAuthority(
			t, ctx, tx, initial, conversationOne, invocationOne, "item-restore-2", 2,
		); err != nil {
			return err
		}
		if err := tx.PutFindingAdjudication(ctx, revisionTwo); err != nil {
			return err
		}
		return putAdjudicationFeedbackAuthority(
			t, ctx, tx, revisionTwo, conversationTwo, invocationTwo, "item-restore-3", 2,
		)
	}); err != nil {
		t.Fatalf("seed checkpoint history: %v", err)
	}
	checkpoint := filepath.Join(t.TempDir(), "checkpoint.db")
	if err := st.Checkpoint(ctx, checkpoint); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutFindingAdjudication(ctx, revisionThree)
	}); err != nil {
		t.Fatalf("append after checkpoint: %v", err)
	}
	if _, err := st.Restore(ctx, checkpoint); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		history, err := tx.ListFindingAdjudications(ctx, runID)
		if err != nil {
			return err
		}
		if len(history) != 2 || history[0].Digest != initial.Digest || history[1].Digest != revisionTwo.Digest {
			t.Fatalf("restored history = %+v", history)
		}
		for _, artifact := range history {
			if _, err := tx.GetFindingAdjudication(ctx, artifact.Digest); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("read restored history: %v", err)
	}
}

func TestFindingAdjudicationBindingFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-binding")
	at := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	findings := []domain.Finding{
		adjudicationFinding("finding-a", runID, "daemon/a.go", at),
		adjudicationFinding("finding-b", runID, "daemon/b.go", at),
	}
	st := seedReviewRound(t, runID, 1, findings, at)

	cases := []struct {
		name     string
		artifact domain.FindingAdjudication
	}{
		{"missing review record", newAdjudication(t, runID, 9, []domain.FindingID{"finding-a", "finding-b"}, at)},
		{"foreign finding", newAdjudication(t, runID, 1, []domain.FindingID{"finding-a", "finding-c"}, at)},
		{"missing finding", newAdjudication(t, runID, 1, []domain.FindingID{"finding-a"}, at)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := st.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutFindingAdjudication(ctx, tc.artifact)
			})
			if !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("%s put = %v, want ErrParentKeyMismatch", tc.name, err)
			}
		})
	}
}

// TestFindingAdjudicationDigestBinding proves the store re-gates every
// caller-supplied binding digest against its authority: the approved-spec and
// resolved-policy digests against the run, and the instruction snapshot against
// the review round. A syntactically valid but disagreeing digest cannot persist
// an adjudication bound to a spec, policy, or instruction snapshot the round is
// not.
func TestFindingAdjudicationDigestBinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-digest-binding")
	at := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	findings := []domain.Finding{adjudicationFinding("finding-a", runID, "daemon/a.go", at)}
	st := seedReviewRound(t, runID, 1, findings, at)

	// A run-round-and-finding-set-correct artifact whose spec, policy, or
	// instruction digest disagrees with its authority must be rejected, isolating
	// each binding: every parameter but the varied one carries its correct value.
	newBound := func(t *testing.T, spec, instruction, policy domain.Digest) domain.FindingAdjudication {
		t.Helper()
		entry := adjudicationEngineEntry(t, "finding-a")
		artifact, err := domain.NewFindingAdjudication(
			runID, 1, spec, instruction, policy,
			[]domain.FindingAdjudicationEntry{entry}, at)
		if err != nil {
			t.Fatalf("new adjudication: %v", err)
		}
		return artifact
	}

	cases := []struct {
		name     string
		artifact domain.FindingAdjudication
	}{
		{"spec digest mismatch", newBound(t, adjudicationDigest("f"), adjInstructionDigest, adjPolicyDigest)},
		{"policy digest mismatch", newBound(t, adjSpecDigest, adjInstructionDigest, adjudicationDigest("f"))},
		{"instruction digest mismatch", newBound(t, adjSpecDigest, adjudicationDigest("f"), adjPolicyDigest)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := st.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutFindingAdjudication(ctx, tc.artifact)
			})
			if !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("%s put = %v, want ErrParentKeyMismatch", tc.name, err)
			}
		})
	}
}

func TestFindingAdjudicationStoredBodyGolden(t *testing.T) {
	t.Parallel()
	runID := domain.RunID("run-golden")
	at := time.Date(2026, 8, 21, 15, 0, 0, 0, time.UTC)
	artifact := newAdjudication(t, runID, 2, []domain.FindingID{"finding-0001", "finding-0002"}, at)
	body, err := artifact.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// Re-indent the persisted compact body for a readable, stable golden.
	var pretty json.RawMessage = body
	indented, err := json.MarshalIndent(pretty, "", "  ")
	if err != nil {
		t.Fatalf("indent: %v", err)
	}
	golden.Assert(t, "finding_adjudication", append(indented, '\n'))
}
