package integration_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestOriginalPublicationReevaluationEscalatesWithoutProducer(t *testing.T) {
	p := newProductionPublicationHarnessWithBoundIssue(t, "", 79)
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		policy, err := tx.GetResolvedPolicy(p.ctx, p.runID)
		if err != nil {
			return err
		}
		body, err := json.Marshal(policy.Keys)
		if err != nil {
			return err
		}
		_, err = p.blobs.Put(policy.Digest, bytes.NewReader(body))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	p.room.fail = true
	p.startAndRecordExport(t)
	if result, err := p.reconcileLanes(); err != nil || result.BlockedItemsCreated != 1 {
		t.Fatalf("original block: %#v, %v", result, err)
	}
	blocked, err := p.attention.GetAttentionItem(p.ctx, domain.ProductionBlockedItemID(p.runID))
	if err != nil {
		t.Fatal(err)
	}
	repairPublicationTrustProfile(t, p)
	submitPublicationRerun(t, p, blocked, "original-recheck")
	p.room.fail = false
	configureSuccessorJudgments(t, p)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.reviewer.Script(engine.ProductionReviewInvocationID(p.runID, 1), fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: p.baseSHA, HeadSHA: p.replay.HeadSHA, Provider: "openai", ModelConfiguration: "codex/test", CostOwner: "test",
			CompletedAt: p.now, CompletionEvidence: productionDigest([]byte("original recheck findings")), Findings: []domain.Finding{{
				ID: "original-recheck-finding", RunID: p.runID, Source: "codex_local", Severity: domain.FindingSeverityP1,
				Location: &domain.FindingLocation{Path: "README.md", StartLine: 1, EndLine: 1}, Message: "Fix the original candidate.", RawText: "Fix the original candidate.", CreatedAt: p.now,
			}},
		},
	})
	if result, err := p.reconcileLanes(); err != nil || result.PublicationTasksCompleted != 1 {
		t.Fatalf("original recheck escalation: %#v, %v", result, err)
	}
	item := productionItemRecord(t, p, "successor-reevaluation-review-original-recheck")
	if item.Type != domain.AttentionReviewDispute || !reflect.DeepEqual(item.RequestedDecision, []domain.Action{domain.ActionApprove, domain.ActionDiscuss, domain.ActionStop}) {
		t.Fatalf("original dispute: %#v", item)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		entry, err := tx.GetOutbox(p.ctx, signet.PublicationReevaluationCompletionKey("original-recheck"))
		if err != nil {
			return err
		}
		completed, err := signet.DecodePublicationReevaluationCompletion(entry.Payload)
		if err != nil {
			return err
		}
		if !entry.Dispatched() || completed.Outcome != signet.PublicationReevaluationReviewEscalated {
			t.Fatalf("recheck completion: %#v", completed)
		}
		if _, err := tx.GetOutbox(p.ctx, "inv-remediate-1-"+string(p.runID)); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("recheck persisted remediation: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := p.attention.GetAttentionItem(p.ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	approve := signet.ClientCommand{
		CommandID: "approve-no-pr", DeviceID: "device-original-recheck", ExpectedEntityVersion: snapshot.EntityVersion,
		Payload: signet.DecisionPayload{ItemID: item.ID, ItemVersion: item.ItemVersion, PRHeadSHA: item.PRHeadSHA, ArtifactDigests: item.ArtifactDigests, Action: domain.ActionApprove},
	}
	if _, err := p.attention.Submit(p.ctx, approve); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatalf("no-PR approval refusal: %v", err)
		}
		p.restartDurableState(t)
		p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	}
	assertContinuationRefused(t, p, "approve-no-pr")
}

func assertContinuationRefused(t *testing.T, p *productionPublicationHarness, commandID string) {
	t.Helper()
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		items, err := tx.ListOpenAttentionItemRecordsForRun(p.ctx, p.runID)
		if err != nil {
			return err
		}
		found := false
		for _, item := range items {
			if item.Type == domain.AttentionSystemHealth && reflect.DeepEqual(item.RequestedDecision, []domain.Action{domain.ActionAcknowledge}) && strings.Contains(item.Reason, "remediation continuation cannot start") {
				found = true
			}
		}
		if !found {
			t.Fatal("missing acknowledgment-only continuation refusal")
		}
		pending, err := tx.ListPendingOutbox(p.ctx, engine.KindRemediationInvocationRequested)
		if err != nil {
			return err
		}
		for _, row := range pending {
			if strings.Contains(row.IdempotencyKey, string(p.runID)) {
				t.Fatal("refused continuation enqueued remediation")
			}
		}
		_, err = tx.GetPublicationSuccessor(p.ctx, p.runID, domain.InvocationID("publish-continuation-"+commandID))
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("refused continuation created successor: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func completePublicationContinuation(t *testing.T, p *productionPublicationHarness, run domain.Run, scenario string) {
	completePublicationContinuationCycle(t, p, run, scenario, "rerun-feedback-trust", "approve-continuation", 2)
}

func completePublicationContinuationCycle(t *testing.T, p *productionPublicationHarness, run domain.Run, scenario, rerunID, approveID string, chainLength int) {
	t.Helper()
	item, err := p.attention.GetAttentionItem(p.ctx, domain.ItemID(domain.PublicationContinuationItemPrefix+rerunID))
	if err != nil {
		t.Fatal(err)
	}
	round := item.Item.ReviewDispute.Round
	if strings.HasSuffix(scenario, "-upgrade") || strings.HasSuffix(scenario, "-stop") {
		restoreLegacyContinuationDispute(t, p, item.Item)
		item, err = p.attention.GetAttentionItem(p.ctx, item.Item.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if strings.HasSuffix(scenario, "-upgrade") {
		old := item
		keys := []string{"production-publication/" + string(p.runID), "production-publication-successor/" + string(p.runID) + "/returned-feedback", signet.PublicationReevaluationKey(p.runID, rerunID), signet.PublicationReevaluationCompletionKey(rerunID)}
		retained := make(map[string][]byte)
		for _, key := range keys {
			retained[key] = readOutboxPayload(t, p, key)
		}
		p.restartDurableState(t)
		p.workflow = p.newEngine(t, productionCrashSeams{}, true)
		for range 2 {
			if _, err := p.workflow.Reconcile(p.ctx); err != nil {
				t.Fatal(err)
			}
		}
		item, err = p.attention.GetAttentionItem(p.ctx, item.Item.ID)
		if err != nil {
			t.Fatal(err)
		}
		if item.Item.ItemVersion != old.Item.ItemVersion+1 || item.Item.ReviewDispute == nil || item.Item.ReviewDispute.Round != round || !item.Item.Offers(domain.ActionApprove) {
			t.Fatalf("legacy dispute did not converge to one bound approval surface: %#v", item)
		}
		_, err = p.attention.Submit(p.ctx, signet.ClientCommand{
			CommandID: "stale-legacy-approve", DeviceID: "retry-device", ExpectedEntityVersion: old.EntityVersion,
			Payload: signet.DecisionPayload{ItemID: old.Item.ID, ItemVersion: old.Item.ItemVersion, PRHeadSHA: old.Item.PRHeadSHA, ArtifactDigests: old.Item.ArtifactDigests, Action: domain.ActionApprove},
		})
		var stale *signet.StaleVersionError
		if !errors.As(err, &stale) {
			t.Fatalf("old card approved changed decision surface: %v", err)
		}
		for key, body := range retained {
			if !bytes.Equal(readOutboxPayload(t, p, key), body) {
				t.Fatalf("upgrade changed retained task or completion %s", key)
			}
		}
	}
	input := signet.ClientCommand{
		CommandID: approveID, DeviceID: "retry-device", ExpectedEntityVersion: item.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: item.Item.ID, ItemVersion: item.Item.ItemVersion,
			PRHeadSHA: item.Item.PRHeadSHA, ArtifactDigests: item.Item.ArtifactDigests, Action: domain.ActionApprove,
		},
	}
	if strings.HasSuffix(scenario, "-completed-before-approve") {
		recordOriginalCompletion(t, p)
		if _, err := p.attention.Submit(p.ctx, input); !errors.Is(err, store.ErrPublicationCompleted) {
			t.Fatalf("completed unit accepted approval: %v", err)
		}
		return
	}
	if strings.HasSuffix(scenario, "-stop") || strings.HasSuffix(scenario, "-discuss") {
		input.Payload.Action = domain.ActionStop
		if strings.HasSuffix(scenario, "-discuss") {
			input.Payload.Action, input.Payload.Message = domain.ActionDiscuss, "Explain the proposed correction."
		}
		if _, err := p.attention.Submit(p.ctx, input); err != nil {
			t.Fatal(err)
		}
		if _, err := p.workflow.Reconcile(p.ctx); err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(scenario, "-stop") {
			stopped := productionItemRecord(t, p, string(item.Item.ID))
			if stopped.Status == domain.StatusOpen || stopped.ReviewDispute != nil || stopped.Offers(domain.ActionApprove) {
				t.Fatalf("upgrade changed a stopped legacy dispute: %#v", stopped)
			}
		}
		if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
			_, err := tx.GetOutbox(p.ctx, "inv-remediate-2-"+string(p.runID))
			if !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("non-approve created remediation: %v", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return
	}
	first, err := p.attention.Submit(p.ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	replayCommand, err := p.attention.Submit(p.ctx, input)
	if err != nil || !reflect.DeepEqual(first.Record, replayCommand.Record) {
		t.Fatalf("approve replay: %#v, %v", replayCommand, err)
	}
	second := input
	second.CommandID = approveID + "-again"
	if _, err := p.attention.Submit(p.ctx, second); err == nil {
		t.Fatal("second approve accepted on closed dispute")
	}
	if strings.HasSuffix(scenario, "-completed-after-approve") {
		recordOriginalCompletion(t, p)
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatal(err)
		}
		assertContinuationRefused(t, p, input.CommandID)
		return
	}
	if _, err := p.workflow.Reconcile(p.ctx); err != nil {
		t.Fatal(err)
	}
	var intent domain.PublicationContinuationIntent
	var original, predecessor store.QueueEntry
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var entry store.QueueEntry
		intent, entry, err = tx.PublicationContinuationIntent(p.ctx, p.runID, input.CommandID)
		if err != nil {
			return err
		}
		if entry.Dispatched() {
			t.Fatal("continuation ran before publication lane")
		}
		original, err = tx.GetOutbox(p.ctx, "production-publication/"+string(p.runID))
		if err != nil {
			return err
		}
		predecessor, err = tx.GetOutbox(p.ctx, intent.SourceTaskKey)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(scenario, "-completed-after-queue") {
		recordOriginalCompletion(t, p)
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatal(err)
		}
		assertContinuationRefused(t, p, input.CommandID)
		return
	}
	if strings.HasSuffix(scenario, "-corrupt") {
		payload := readOutboxPayload(t, p, intent.Key())
		writeOutboxPayload(t, p, intent.Key(), []byte(`{"version":"invalid"}`))
		p.restartDurableState(t)
		p.workflow = p.newEngine(t, productionCrashSeams{}, true)
		unrelated := p.submitUnrelatedRun(t, "unrelated-to-continuation")
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatalf("damaged intent stopped unrelated work: %v", err)
		}
		if held := productionItemRecord(t, p, "production-task-quarantined-1-"+string(p.runID)); held.Status != domain.StatusOpen {
			t.Fatal("malformed continuation was not quarantined")
		}
		if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error { _, err := tx.GetExecutionAdmissionRecord(p.ctx, unrelated); return err }); err != nil {
			t.Fatalf("unrelated run did not dispatch: %v", err)
		}
		writeOutboxPayload(t, p, intent.Key(), payload)
		return
	}
	p.restartDurableState(t)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if _, err := p.workflow.ReconcileProductionPublications(p.ctx); err != nil {
		t.Fatalf("start continuation: %v", err)
	}
	producer := domain.InvocationID(fmt.Sprintf("inv-remediate-%d-%s", round, p.runID))
	p.driver.Script(producer, fake.StageScript{PendingInspects: 1, Outcome: fake.OutcomeComplete, Result: exec.StageResult{Summary: "Recheck findings fixed."}})
	if result, err := p.workflow.Reconcile(p.ctx); err != nil || result.InvocationsStarted != 1 {
		t.Fatalf("continuation remediation dispatch: %#v, %v", result, err)
	}
	var admission domain.ExecutionAdmission
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		admission, err = tx.GetExecutionAdmissionRecord(p.ctx, producer)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	replay := buildProductionReplayWithContentAt(t, p.publicationHarness, p.runID, run.SpecDigest,
		submissionSpecification(string(p.runID)), p.declaration.BoundIssue, producer, fakePublicationTime.Add(time.Duration(round+1)*time.Minute),
		fmt.Sprintf("production change\ncorrected decision note\nrecheck findings fixed round %d\n", round))
	exported, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: producer, AdmissionID: admission.ID,
		ObservedBaseSHA: p.baseSHA, HeadSHA: replay.HeadSHA, ManifestDigest: replay.ManifestDigest, RecordedAt: replay.ImportOptions.CommitDate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.RecordProductionExecutionExport(p.ctx, p.store, exported, replay); err != nil {
		t.Fatalf("continuation export: %v", err)
	}
	p.replay = replay
	p.now = p.now.Add(time.Minute)
	p.reviewer.Script(engine.ProductionReviewInvocationID(p.runID, round+1), fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: p.baseSHA, HeadSHA: replay.HeadSHA, Provider: "openai", ModelConfiguration: "codex/test", CostOwner: "test",
			CompletedAt: p.now, CompletionEvidence: productionDigest([]byte("clean continuation review")),
		},
	})
	before := p.room.runs
	if strings.HasSuffix(scenario, "-cyclic") {
		originalIntent := readOutboxPayload(t, p, intent.Key())
		originalAuthority := readOutboxPayload(t, p, intent.Successor.Key())
		intent.Successor.PredecessorItemID = intent.Successor.ReadyItemID()
		body, err := json.Marshal(intent)
		if err != nil {
			t.Fatal(err)
		}
		writeOutboxPayload(t, p, intent.Key(), body)
		body, err = json.Marshal(intent.Successor)
		if err != nil {
			t.Fatal(err)
		}
		writeOutboxPayload(t, p, intent.Successor.Key(), body)
		if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
			_, err := tx.PublicationSuccessorChain(p.ctx, p.runID)
			if err == nil {
				t.Fatal("cyclic continuation authority authenticated")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		unrelated := p.submitUnrelatedRun(t, "unrelated-to-cyclic-continuation")
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatalf("cyclic authority stopped unrelated work: %v", err)
		}
		if held := productionItemRecord(t, p, "remediation-marker-quarantined-1-"+string(p.runID)); held.Status != domain.StatusOpen {
			t.Fatal("cyclic continuation was not quarantined")
		}
		if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error { _, err := tx.GetExecutionAdmissionRecord(p.ctx, unrelated); return err }); err != nil {
			t.Fatalf("unrelated run did not dispatch: %v", err)
		}
		writeOutboxPayload(t, p, intent.Key(), originalIntent)
		writeOutboxPayload(t, p, intent.Successor.Key(), originalAuthority)
		return
	}
	if strings.HasSuffix(scenario, "-repeated") {
		p.room.fail = true
		if result, err := p.reconcileLanes(); err != nil || result.BlockedItemsCreated != 1 {
			t.Fatalf("continuation verification block: %#v, %v", result, err)
		}
		blocked, err := p.attention.GetAttentionItem(p.ctx, intent.Successor.BlockedItemID())
		if err != nil {
			t.Fatal(err)
		}
		// Recording an already known profile does not reactivate it. Explicitly
		// select the original profile, which this new head has not evaluated.
		if err := p.store.WriteInternal(p.ctx, func(tx *store.InternalTx) error {
			return tx.ActivateTrustProfile(p.ctx, p.profile.Repo, p.profile.ProfileDigest, p.now.Add(2*time.Hour))
		}); err != nil {
			t.Fatal(err)
		}
		submitPublicationRerun(t, p, blocked, "rerun-second-continuation")
		p.room.fail = false
		configureSuccessorJudgments(t, p)
		p.workflow = p.newEngine(t, productionCrashSeams{}, true)
		var recheckRound int
		if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
			entry, err := tx.GetOutbox(p.ctx, signet.PublicationReevaluationKey(p.runID, "rerun-second-continuation"))
			if err != nil {
				return err
			}
			request, err := signet.DecodePublicationReevaluationRequest(entry.Payload)
			recheckRound = request.ReviewRound
			return err
		}); err != nil {
			t.Fatal(err)
		}
		p.reviewer.Script(engine.ProductionReviewInvocationID(p.runID, recheckRound), fake.ReviewScript{
			Outcome: fake.OutcomeComplete,
			Result: exec.ReviewResult{
				BaseSHA: p.baseSHA, HeadSHA: replay.HeadSHA, Provider: "openai", ModelConfiguration: "codex/test", CostOwner: "test",
				CompletedAt: p.now, CompletionEvidence: productionDigest([]byte("second recheck findings")), Findings: []domain.Finding{{
					ID: "second-recheck-finding", RunID: p.runID, Source: "codex_local", Severity: domain.FindingSeverityP1,
					Location: &domain.FindingLocation{Path: "README.md", StartLine: 1, EndLine: 1}, Message: "Fix the next candidate.", RawText: "Fix the next candidate.", CreatedAt: p.now,
				}},
			},
		})
		if result, err := p.reconcileLanes(); err != nil || result.PublicationTasksCompleted != 1 {
			t.Fatalf("second recheck escalation: %#v, %v", result, err)
		}
		completePublicationContinuationCycle(t, p, run, "clean", "rerun-second-continuation", "approve-second-continuation", chainLength+1)
		return
	}
	if strings.HasSuffix(scenario, "-moved") || strings.HasSuffix(scenario, "-missing") || strings.HasSuffix(scenario, "-foreign") || strings.HasSuffix(scenario, "-closed") {
		wantPRs := 1
		p.forge.mu.Lock()
		switch {
		case strings.HasSuffix(scenario, "-moved"):
			for branch := range p.forge.refs {
				p.forge.refs[branch] = strings.Repeat("f", 40)
			}
		case strings.HasSuffix(scenario, "-missing"):
			p.forge.prs, wantPRs = nil, 0
		case strings.HasSuffix(scenario, "-foreign"):
			p.forge.prs[0].Body = "A pull request owned by someone else."
		case strings.HasSuffix(scenario, "-closed"):
			p.forge.prs[0].State = "closed"
		}
		p.forge.mu.Unlock()
		if result, err := p.reconcileLanes(); err != nil || result.BlockedItemsCreated != 1 {
			t.Fatalf("unavailable continuation target: %#v, %v", result, err)
		}
		blocked := productionItemRecord(t, p, string(intent.Successor.BlockedItemID()))
		if blocked.Type != domain.AttentionPublishBlocked {
			t.Fatal("unavailable target lacks continuation block")
		}
		if refs, prs := p.forge.counts(); refs != 1 || prs != wantPRs {
			t.Fatalf("unavailable target created replacement: %d refs %d prs", refs, prs)
		}
		return
	}
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 1 {
		t.Fatalf("publish continuation: %#v, %v", result, err)
	}
	if p.room.runs != before+1 {
		t.Fatalf("continuation reused verification: before=%d after=%d", before, p.room.runs)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		chain, err := tx.PublicationSuccessorChain(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if len(chain) != chainLength || chain[len(chain)-1] != intent.Successor {
			t.Fatalf("continuation chain: %#v", chain)
		}
		last := chain[len(chain)-1]
		if err := tx.AuthenticateSuccessorProducer(p.ctx, last, producer); err != nil {
			return err
		}
		if err := tx.AuthenticateSuccessorProducer(p.ctx, last, domain.InvocationID("inv-production-"+string(p.runID))); err == nil {
			t.Fatal("original producer authenticated for continuation")
		}
		for _, retained := range []store.QueueEntry{original, predecessor} {
			current, err := tx.GetOutbox(p.ctx, retained.IdempotencyKey)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(current, retained) {
				t.Fatal("continuation changed predecessor task")
			}
		}
		ready, err := tx.GetReadyItemPRBinding(p.ctx, last.ReadyItemID())
		if err != nil {
			return err
		}
		if ready.HeadSHA != replay.HeadSHA {
			t.Fatal("continuation ready head differs")
		}
		if _, err := tx.GetOutbox(p.ctx, (domain.PublicationContinuationIntent{Successor: domain.PublicationSuccessor{RunID: p.runID, CommandID: second.CommandID}}).Key()); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("second approval authority: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if refs, prs := p.forge.counts(); refs != 1 || prs != 1 {
		t.Fatalf("continuation created resources: refs=%d prs=%d", refs, prs)
	}
	assertContinuationRequiresTerminal(t, p, intent.Successor)
	p.restartDurableState(t)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	assertFeedbackRunProjection(t, p, domain.RunOutcomePublished)
}

// Reproduce the parent release's durable card and matching decision surface.
// Its recheck task is already dispatched, so only upgrade reconciliation can
// add the missing binding and action after the store is reopened.
func restoreLegacyContinuationDispute(t *testing.T, p *productionPublicationHarness, item domain.AttentionItem) {
	t.Helper()
	item.RequestedDecision = []domain.Action{domain.ActionDiscuss, domain.ActionStop}
	item.ReviewDispute, item.Recommendation = nil, nil
	item.Reason = "The rechecked successor needs code changes. This completed publication cycle cannot launch another remediation; discuss a new authorized continuation."
	surface, err := domain.NewDecisionSurface(item)
	if err != nil {
		t.Fatal(err)
	}
	item.DecisionSurface = domain.DecisionSurfaceRef{Epoch: surface.Epoch, Digest: surface.Digest}
	body, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	surfaceBody, err := json.Marshal(surface)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", p.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	tx, err := raw.BeginTx(p.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(p.ctx, `UPDATE attention_items SET body = ? WHERE id = ?`, string(body), item.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(p.ctx, `UPDATE attention_decision_surfaces SET epoch = ?, digest = ?, body = ? WHERE item_id = ?`, surface.Epoch, surface.Digest, string(surfaceBody), item.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func assertContinuationRequiresTerminal(t *testing.T, p *productionPublicationHarness, successor domain.PublicationSuccessor) {
	t.Helper()
	key := "production-reevaluation-terminal/" + successor.ReevaluationCommandID
	raw, err := sql.Open("sqlite", p.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	var original []byte
	if err := raw.QueryRowContext(p.ctx, `SELECT payload FROM inbox WHERE idempotency_key = ?`, key).Scan(&original); err != nil {
		t.Fatal(err)
	}
	write := func(payload []byte) {
		t.Helper()
		if _, err := raw.ExecContext(p.ctx, `UPDATE inbox SET payload = ? WHERE idempotency_key = ?`, payload, key); err != nil {
			t.Fatal(err)
		}
	}
	write([]byte(`{"status":"completed"}`))
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetPublicationSuccessor(p.ctx, p.runID, successor.PublicationID())
		if err == nil {
			t.Fatal("continuation authority survived corrupt terminal evidence")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	write(original)
}
