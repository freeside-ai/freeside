package signet_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func seedFindingAdjudicationItem(t *testing.T, f fixture) domain.AttentionItem {
	t.Helper()
	ctx := context.Background()
	runID := domain.RunID("run-finding-adjudication")
	findingIDs := []domain.FindingID{"finding-a", "finding-b"}
	findings := make([]domain.Finding, 0, len(findingIDs))
	entries := make([]domain.FindingAdjudicationEntry, 0, len(findingIDs))
	for _, findingID := range findingIDs {
		findings = append(findings, domain.Finding{
			ID: findingID, RunID: runID, Source: "codex_local",
			Location: &domain.FindingLocation{Path: "daemon/" + string(findingID) + ".go", StartLine: 1, EndLine: 1},
			Message:  "finding " + string(findingID), RawText: "finding " + string(findingID), CreatedAt: *f.now,
		})
		entry, err := domain.NewModelAdjudicationEntry(
			findingID, domain.GoalContradictory, nil, domain.RouteDecline,
			domain.ConfidenceHigh, "the finding contradicts the approved work unit",
			nil, []string{"AGENTS.md"}, []string{"the work contract is current"}, nil, nil,
		)
		if err != nil {
			t.Fatalf("new entry: %v", err)
		}
		entries = append(entries, entry)
	}
	digest := func(component string) domain.Digest {
		return domain.Digest("sha256:" + strings.Repeat(component, 64))
	}
	specDigest := digest("a")
	policyDigest := digest("b")
	instructionDigest := digest("c")
	record, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-finding-adjudication-1", RunID: runID, Round: 1,
		Provider: "openai", ModelConfiguration: "gpt-codex/high",
		ConfigurationDigest: digest("d"), InstructionDigest: instructionDigest,
		CostOwner: "owner", BaseSHA: "base", HeadSHA: "head", CompletedAt: *f.now,
		CompletionEvidence: digest("e"), Outcome: domain.ReviewFindings,
		FindingIDs: findingIDs,
	})
	if err != nil {
		t.Fatalf("new review record: %v", err)
	}
	artifact, err := domain.NewFindingAdjudication(
		runID, 1, specDigest, instructionDigest, policyDigest, entries, *f.now,
	)
	if err != nil {
		t.Fatalf("new adjudication: %v", err)
	}
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, domain.Run{
			ID: runID, ProjectID: "proj-1", SpecDigest: specDigest, PolicyDigest: policyDigest,
		}); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, record, findings); err != nil {
			return err
		}
		return tx.PutFindingAdjudication(ctx, artifact)
	}); err != nil {
		t.Fatalf("seed adjudication authority: %v", err)
	}

	proposals := make([]domain.FindingAdjudicationProposal, 0, len(entries))
	for _, entry := range entries {
		proposals = append(proposals, domain.FindingAdjudicationProposal{
			FindingID: entry.FindingID, Producer: entry.Producer,
			GoalRelationship: entry.GoalRelationship, Compatibility: entry.Compatibility,
			Route: entry.Route, Rationale: entry.Rationale,
			CitedRules: entry.CitedRules, Assumptions: entry.Assumptions,
			OpenQuestions: entry.OpenQuestions, Confidence: entry.Confidence,
			OfferedAlternatives: slices.Clone(entry.OfferedAlternatives),
		})
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "finding-adjudication-item", ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionFindingAdjudication, Priority: domain.PriorityHigh,
		Reason: "choose a route for the review findings",
		RequestedDecision: []domain.Action{
			domain.ActionAcceptRecommendedRoute, domain.ActionChooseAlternativeRoute,
			domain.ActionDiscuss, domain.ActionStop,
		},
		FindingAdjudication: &domain.FindingAdjudicationBinding{
			RunID: runID, Round: 1, AdjudicationDigest: artifact.Digest, Proposals: proposals,
		},
		PRHeadSHA: "head", ItemVersion: 1,
		InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatalf("new item: %v", err)
	}
	if err := f.service.PutItem(ctx, item); err != nil {
		t.Fatalf("put item: %v", err)
	}
	return item
}

func TestSubmitFindingAdjudicationAcceptAndReplay(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	item := seedFindingAdjudicationItem(t, f)
	command := commandOn(item, "command-accept-recommended", domain.ActionAcceptRecommendedRoute)
	result, err := f.service.Submit(ctx, command)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if result.Record.Action != domain.ActionAcceptRecommendedRoute || result.Record.Message != "" ||
		len(result.Record.Attachments) != 0 {
		t.Fatalf("record = %+v, want content-free accepted recommendation", result.Record)
	}
	decided, _ := f.itemSnapshotFor(t, item.ID)
	if decided.Status != domain.StatusResolved || decided.DecidedAt == nil {
		t.Fatalf("decided item = status %q at %v", decided.Status, decided.DecidedAt)
	}
	replay, err := f.service.Submit(ctx, command)
	if err != nil || replay.Revision != result.Revision {
		t.Fatalf("replay = %+v, %v; want revision %d", replay, err, result.Revision)
	}
}

func TestSubmitFindingAdjudicationReplayIdentityMatrix(t *testing.T) {
	ctx := context.Background()
	actions := []domain.Action{
		domain.ActionAcceptRecommendedRoute, domain.ActionChooseAlternativeRoute,
	}
	attachmentModes := []string{"absent", "empty", "present"}
	offered := signet.AlternativeChoice{FindingID: "finding-a", Route: domain.RouteDispute}
	for _, action := range actions {
		choiceModes := []string{"absent", "empty", "present"}
		if action == domain.ActionChooseAlternativeRoute {
			choiceModes = append(choiceModes, "different")
		}
		for _, choiceMode := range choiceModes {
			for _, attachmentMode := range attachmentModes {
				t.Run(string(action)+"/"+choiceMode+"/"+attachmentMode, func(t *testing.T) {
					f := newFixture(t)
					item := seedFindingAdjudicationItem(t, f)
					original := commandOn(item, "command-replay-matrix", action)
					if action == domain.ActionChooseAlternativeRoute {
						original.Payload.AlternativeChoices = []signet.AlternativeChoice{offered}
					}
					first, err := f.service.Submit(ctx, original)
					if err != nil {
						t.Fatalf("Submit(original): %v", err)
					}

					retry := original
					switch choiceMode {
					case "absent":
						retry.Payload.AlternativeChoices = nil
					case "empty":
						retry.Payload.AlternativeChoices = []signet.AlternativeChoice{}
					case "present":
						retry.Payload.AlternativeChoices = []signet.AlternativeChoice{offered}
					case "different":
						retry.Payload.AlternativeChoices = []signet.AlternativeChoice{{
							FindingID: offered.FindingID, Route: domain.RouteDefer,
						}}
					}
					if action == domain.ActionChooseAlternativeRoute &&
						(choiceMode == "absent" || choiceMode == "empty") {
						retry.Payload.Message = first.Record.Message
					}
					switch attachmentMode {
					case "absent":
						retry.Payload.Attachments = nil
					case "empty":
						retry.Payload.Attachments = []domain.Digest{}
					case "present":
						retry.Payload.Attachments = []domain.Digest{
							domain.Digest("sha256:" + strings.Repeat("f", 64)),
						}
					}

					replayed, err := f.service.Submit(ctx, retry)
					shouldReplay := choiceMode != "different" && attachmentMode != "present"
					if shouldReplay {
						if err != nil || replayed.Revision != first.Revision || replayed.Record.CommandID != first.Record.CommandID {
							t.Fatalf("Submit(retry) = %+v, %v; want recorded result %+v", replayed, err, first)
						}
					} else if !errors.Is(err, store.ErrImmutableConflict) {
						t.Fatalf("Submit(retry) = %v, want ErrImmutableConflict", err)
					}
				})
			}
		}
	}
}

func TestSubmitFindingAdjudicationAlternativeChoice(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	item := seedFindingAdjudicationItem(t, f)
	command := commandOn(item, "command-choose-alternative", domain.ActionChooseAlternativeRoute)
	command.Payload.AlternativeChoices = []signet.AlternativeChoice{
		{FindingID: "finding-b", Route: domain.RouteDispute},
		{FindingID: "finding-a", Route: domain.RouteDispute},
	}
	result, err := f.service.Submit(ctx, command)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	wantMessage := `[{"finding_id":"finding-a","route":"dispute"},{"finding_id":"finding-b","route":"dispute"}]`
	if result.Record.Action != domain.ActionChooseAlternativeRoute || result.Record.Message != wantMessage {
		t.Fatalf("record = %+v, want canonical alternative message %s", result.Record, wantMessage)
	}
	decided, _ := f.itemSnapshotFor(t, item.ID)
	if decided.Status != domain.StatusResolved || decided.DecidedAt == nil {
		t.Fatalf("decided item = status %q at %v", decided.Status, decided.DecidedAt)
	}
}

func TestSubmitFindingAdjudicationRejectsUnofferedChoice(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	item := seedFindingAdjudicationItem(t, f)
	before := f.revision(t)
	command := commandOn(item, "command-unoffered-alternative", domain.ActionChooseAlternativeRoute)
	command.Payload.AlternativeChoices = []signet.AlternativeChoice{{
		FindingID: "finding-a", Route: domain.RouteDefer,
	}}
	if _, err := f.service.Submit(ctx, command); !errors.Is(err, signet.ErrAlternativeNotOffered) {
		t.Fatalf("Submit = %v, want ErrAlternativeNotOffered", err)
	}
	if after := f.revision(t); after != before {
		t.Fatalf("rejected choice moved revision %d -> %d", before, after)
	}
}

// TestSubmitFindingAdjudicationRejectsRawRowForgedOffer proves raw-row tampering
// cannot make a non-offered route choosable (#893). The stored item is rewritten
// so finding-a recommends dispute and appears to offer decline — a
// self-consistent payload the digest-bound artifact never authorized — then a
// choose command for that forged route is submitted. Submit loads the item
// through the re-gating snapshot read, which binds the offered set to the
// artifact and rejects the divergence before the choice is evaluated.
func TestSubmitFindingAdjudicationRejectsRawRowForgedOffer(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	item := seedFindingAdjudicationItem(t, f)
	before := f.revision(t)

	item.FindingAdjudication.Proposals[0].Route = domain.RouteDispute
	item.FindingAdjudication.Proposals[0].OfferedAlternatives = []domain.OfferedAlternative{{
		Route: domain.RouteDecline, Consequence: "record the finding as declined instead",
	}}
	body, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal forged item: %v", err)
	}
	db, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatalf("open raw database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(ctx, `UPDATE attention_items SET body = ? WHERE id = ?`, string(body), item.ID); err != nil {
		t.Fatalf("forge item body: %v", err)
	}

	command := commandOn(item, "command-forged-offer", domain.ActionChooseAlternativeRoute)
	command.Payload.AlternativeChoices = []signet.AlternativeChoice{{
		FindingID: "finding-a", Route: domain.RouteDecline,
	}}
	if _, err := f.service.Submit(ctx, command); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("Submit = %v, want ErrParentKeyMismatch", err)
	}
	if after := f.revision(t); after != before {
		t.Fatalf("forged choice moved revision %d -> %d", before, after)
	}
}

func TestSubmitFindingAdjudicationDiscussAndStop(t *testing.T) {
	ctx := context.Background()
	t.Run("discuss", func(t *testing.T) {
		f := newFixture(t)
		item := seedFindingAdjudicationItem(t, f)
		command := commandOn(item, "command-discuss-adjudication", domain.ActionDiscuss)
		command.Payload.Message = "The scope assumption is wrong."
		if _, err := f.service.Submit(ctx, command); err != nil {
			t.Fatalf("Submit(discuss): %v", err)
		}
		updated, _ := f.itemSnapshotFor(t, item.ID)
		if updated.Status != domain.StatusOpen || updated.ItemVersion != 2 || updated.ConversationID == nil {
			t.Fatalf("discussed item = status %q version %d conversation %v",
				updated.Status, updated.ItemVersion, updated.ConversationID)
		}
	})
	t.Run("stop", func(t *testing.T) {
		f := newFixture(t)
		item := seedFindingAdjudicationItem(t, f)
		if _, err := f.service.Submit(ctx, commandOn(item, "command-stop-adjudication", domain.ActionStop)); err != nil {
			t.Fatalf("Submit(stop): %v", err)
		}
		updated, _ := f.itemSnapshotFor(t, item.ID)
		if updated.Status != domain.StatusResolved {
			t.Fatalf("stop status = %q, want resolved", updated.Status)
		}
	})
}

func TestSubmitFindingAdjudicationRejectsDiscussUntilAcceptedReplyConsumed(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	item := seedFindingAdjudicationItem(t, f)
	first := commandOn(item, "command-first-adjudication-discuss", domain.ActionDiscuss)
	first.Payload.Message = "First challenge."
	if _, err := f.service.Submit(ctx, first); err != nil {
		t.Fatalf("Submit(first discuss): %v", err)
	}
	if err := f.service.AcceptAgentCompletion(
		ctx, "inv-command-first-adjudication-discuss", signet.AgentReply{Body: "Revised recommendation."},
	); err != nil {
		t.Fatalf("AcceptAgentCompletion: %v", err)
	}

	current, snapshot := f.itemSnapshotFor(t, item.ID)
	second := commandOn(current, "command-second-adjudication-discuss", domain.ActionDiscuss)
	second.ExpectedEntityVersion = snapshot.EntityVersion
	second.Payload.Message = "Second challenge."
	before := f.revision(t)
	_, err := f.service.Submit(ctx, second)
	var pending *signet.AgentPendingError
	if !errors.As(err, &pending) || !errors.Is(err, signet.ErrAgentReplyPending) {
		t.Fatalf("Submit(second discuss) = %v, want AgentPendingError", err)
	}
	if after := f.revision(t); after != before {
		t.Fatalf("rejected second discuss moved revision %d -> %d", before, after)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetCommand(ctx, second.CommandID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("rejected second command persisted: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.MarkOutboxDispatched(ctx, "inv-command-first-adjudication-discuss")
	}); err != nil {
		t.Fatalf("consume first reply: %v", err)
	}
	if _, err := f.service.Submit(ctx, second); err != nil {
		t.Fatalf("Submit(second discuss after consumption): %v", err)
	}
}

func TestSubmitFindingAdjudicationStaleAcceptReturnsReplacement(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	item := seedFindingAdjudicationItem(t, f)
	stale := commandOn(item, "command-stale-accept", domain.ActionAcceptRecommendedRoute)
	advanced := item
	advanced.ItemVersion++
	if err := f.service.PutItem(ctx, advanced); err != nil {
		t.Fatalf("advance item: %v", err)
	}
	_, err := f.service.Submit(ctx, stale)
	var staleErr *signet.StaleVersionError
	if !errors.As(err, &staleErr) || staleErr.Replacement.ItemVersion != advanced.ItemVersion {
		t.Fatalf("Submit = %v, want StaleVersionError with replacement version %d", err, advanced.ItemVersion)
	}
}

func (f fixture) itemSnapshotFor(t *testing.T, itemID domain.ItemID) (domain.AttentionItem, store.Snapshot) {
	t.Helper()
	var item domain.AttentionItem
	var snapshot store.Snapshot
	if err := f.store.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		item, snapshot, err = tx.GetAttentionItemSnapshot(context.Background(), itemID)
		return err
	}); err != nil {
		t.Fatalf("GetAttentionItemSnapshot: %v", err)
	}
	return item, snapshot
}
