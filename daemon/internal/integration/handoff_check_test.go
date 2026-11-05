package integration_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	inferencefake "github.com/freeside-ai/freeside/daemon/internal/inference/fake"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// advanceBaseBeforePublication moves the target branch after the run was
// admitted and before its publication's handoff, and rebuilds the engine so
// the handoff's audit observes the new tip.
func (p *productionPublicationHarness) advanceBaseBeforePublication(
	t *testing.T, name, content string,
) (string, *reviewedTree) {
	t.Helper()
	advanced := p.advanceBase(t, name, content)
	p.audit.AuditedCommitSHA = advanced
	return advanced, p.recordReviews(t)
}

// handoffCheck reads the sealed handoff check of the run's current
// publication.
func (p *productionPublicationHarness) handoffCheck(t *testing.T) domain.HandoffCheck {
	t.Helper()
	var check domain.HandoffCheck
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		publication, err := tx.CurrentPublicationInvocationID(p.ctx, p.runID)
		if err != nil {
			return err
		}
		check, err = tx.GetHandoffCheck(p.ctx, p.runID, publication)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return check
}

// assertHandoffVerified checks that the current publication's handoff sealed
// tip over the run's admitted base and recorded a verified merge of the
// unchanged head into it.
func (p *productionPublicationHarness) assertHandoffVerified(t *testing.T, tip string) {
	t.Helper()
	check := p.handoffCheck(t)
	if check.TipSHA != tip || check.EvidenceBaseSHA != p.baseSHA || check.HeadSHA != p.replay.HeadSHA ||
		check.Merge == nil {
		t.Fatalf("handoff check = %#v, want a verified merge of %s into %s over %s",
			check, p.replay.HeadSHA, tip, p.baseSHA)
	}
}

// assertHandoffBlocked checks that the first publication stopped with an open
// publish_blocked item naming reason, that nothing reached the forge, and
// that the run sealed no successor.
func (p *productionPublicationHarness) assertHandoffBlocked(t *testing.T, reason string) {
	t.Helper()
	blocked, err := p.attention.GetAttentionItem(p.ctx, domain.ProductionBlockedItemID(p.runID))
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Item.Type != domain.AttentionPublishBlocked || blocked.Item.Reason != reason ||
		blocked.Item.Status != domain.StatusOpen {
		t.Fatalf("handoff blocked item = %#v, want open publish_blocked: %s", blocked.Item, reason)
	}
	if refs, prs := p.forge.Counts(); refs != 0 || prs != 0 {
		t.Fatalf("blocked handoff produced forge effects = %d refs, %d prs", refs, prs)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetAttentionItem(p.ctx, domain.ProductionReadyItemID(p.runID)); err == nil {
			t.Error("a blocked handoff wrote a ready item")
		}
		chain, err := tx.PublicationSuccessorChain(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if len(chain) != 0 {
			t.Errorf("a blocked handoff sealed %d successors", len(chain))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// assertNoReviewAtTheTip checks every review the run requested after the
// advance was at the admitted base: the handoff reviews nothing.
func (p *productionPublicationHarness) assertNoReviewAtTheTip(t *testing.T, reviews *reviewedTree) {
	t.Helper()
	for _, request := range reviews.requests {
		if request.BaseSHA != p.baseSHA {
			t.Fatalf("review round %d at base %s, want every review at the admitted base %s",
				request.Round, request.BaseSHA, p.baseSHA)
		}
	}
}

// assertReviewedOnce checks the run kept the one review round it earned at
// the admitted base: the handoff reviews nothing.
func (p *productionPublicationHarness) assertReviewedOnce(t *testing.T, reviews *reviewedTree) {
	t.Helper()
	p.assertNoReviewAtTheTip(t, reviews)
	if len(reviews.requests) != 1 {
		t.Fatalf("run requested %d reviews, want its one round", len(reviews.requests))
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		review, err := tx.LatestReviewRecord(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if review.Round != 1 || review.BaseSHA != p.baseSHA || review.HeadSHA != p.replay.HeadSHA {
			t.Errorf("latest review = round %d at %s/%s, want round 1 at the admitted base",
				review.Round, review.BaseSHA, review.HeadSHA)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestHandoffAtAMovedTipPublishesTheVerifiedHead: plan revision 110 §6. A
// target that advanced before the first publication's handoff does not
// re-enter the run: the unchanged head's merge into the tip is verified and
// the head is published once, with no review round.
func TestHandoffAtAMovedTipPublishesTheVerifiedHead(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	advanced, reviews := p.advanceBaseBeforePublication(t, "UPSTREAM.md", "upstream change\n")
	p.reconcileUntilReady(t, 3)

	p.assertHandoffVerified(t, advanced)
	p.assertReviewedOnce(t, reviews)
	ready := p.currentReadyItem(t)
	if ready.ID != domain.ProductionReadyItemID(p.runID) || ready.PRHeadSHA != p.replay.HeadSHA {
		t.Fatalf("ready item = %#v, want the run's own at the unchanged head", ready)
	}
	if refs, prs := p.forge.Counts(); refs != 1 || prs != 1 {
		t.Fatalf("publication effects = %d refs, %d prs, want one of each", refs, prs)
	}
	if pr := p.forge.PullRequests()[0]; pr.HeadSHA != p.replay.HeadSHA {
		t.Fatalf("published head = %s, want the unchanged head %s", pr.HeadSHA, p.replay.HeadSHA)
	}
	if p.room.runs != 2 || p.transport.MergeCount() != 1 {
		t.Fatalf("verification runs = %d, merges = %d, want 2 and 1",
			p.room.runs, p.transport.MergeCount())
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		chain, err := tx.PublicationSuccessorChain(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if len(chain) != 0 {
			t.Errorf("handoff sealed %d successors, want none", len(chain))
		}
		published, err := tx.PublishedPublicationInvocationID(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if published != domain.ProductionPublicationInvocationID(p.runID) {
			t.Errorf("published invocation = %s, want the run's first", published)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertObservedReentryConclusion(t, p, domain.RunConclusion{Outcome: domain.RunOutcomePublished, Final: true})
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 ||
		result.PublicationTasksCompleted != 0 || result.BlockedItemsCreated != 0 {
		t.Fatalf("converged replay = %#v, %v", result, err)
	}
}

// TestHandoffAtAnUnchangedTipPublishesAsBefore: a tip that did not move
// needs no merge; the handoff seals it and publishes with one verification.
func TestHandoffAtAnUnchangedTipPublishesAsBefore(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	p.reconcileUntilReady(t, 3)
	check := p.handoffCheck(t)
	if check.TipSHA != p.baseSHA || check.EvidenceBaseSHA != p.baseSHA || check.Merge != nil {
		t.Fatalf("handoff check = %#v, want an unchanged tip at %s with no merge", check, p.baseSHA)
	}
	if p.room.runs != 1 || p.transport.MergeCount() != 0 {
		t.Fatalf("verification runs = %d, merges = %d, want 1 and 0", p.room.runs, p.transport.MergeCount())
	}
}

// TestHandoffAtTheRoundLimitPublishes: the handoff spends no review round,
// so a run that used its last round before the advance still publishes.
func TestHandoffAtTheRoundLimitPublishes(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarnessWithPolicyKeys(t, "", []domain.PolicyKey{{
		Key: "review.hard_round_limit", Value: fmt.Sprint(1),
		Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride,
			Digest: submissionDigest("run-production-publication", "review-hard-round-limit"),
		},
	}})
	p.startAndRecordExport(t)
	advanced, reviews := p.advanceBaseBeforePublication(t, "UPSTREAM.md", "upstream change\n")
	p.reconcileUntilReady(t, 3)
	p.assertHandoffVerified(t, advanced)
	p.assertReviewedOnce(t, reviews)
}

// TestHandoffBindsTheClosureToTheTip: the publication's policy-approved
// close binds the merge into the tip it publishes against, never the
// admitted base.
func TestHandoffBindsTheClosureToTheTip(t *testing.T) {
	t.Parallel()
	source := engine.ProductionPublication{SourceIssue: "https://github.com/" + fakePublicationRepo + "/issues/82"}
	h := newPublicationHarnessWithBaseFiles(
		t, []byte(`{"commands":[["/usr/bin/true"]],"capture":"none"}`), productionBaseManifests())
	p := newProductionPublicationHarnessWithMetadata(t, h, "", nil, nil, nil, source, nil, "")
	output, err := json.Marshal(map[string]any{
		"title": "Authored PR title", "body": "## Why\n\nAuthored body prose.\n\n## What\n\n- Handle empty input.",
		"reviewer_notes": nil, "evidence_refs": []string{}, "outcome_summary": "Reported checks passed.",
	})
	if err != nil {
		t.Fatal(err)
	}
	p.replay = withPublicAccount(t, p, p.replay, publicAccount)
	explain := inferencefake.Script{Response: inference.Response{Output: output, ComputeUnits: 5}}
	propose := inferencefake.Script{Response: inference.Response{Output: []byte(`{"resolves":true}`), ComputeUnits: 1}}
	scriptPublicationSites(t, p, explain, &propose)
	p.startAndRecordExport(t)
	advanced, _ := p.advanceBaseBeforePublication(t, "UPSTREAM.md", "upstream change\n")
	p.reconcileUntilReady(t, 3)

	prs := p.forge.PullRequests()
	if len(prs) != 1 || !strings.Contains(prs[0].Body, "Closes #82") {
		t.Fatalf("handoff did not publish the policy-approved close: %d PRs", len(prs))
	}
	var base string
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		entry, err := tx.GetInbox(p.ctx, "production-closure/"+string(p.runID)+"/"+
			string(domain.ProductionPublicationInvocationID(p.runID)))
		if err != nil {
			return err
		}
		var checkpoint struct {
			InstanceID domain.ProposalInstanceID `json:"instance_id"`
		}
		if err := json.Unmarshal(entry.Payload, &checkpoint); err != nil {
			return err
		}
		approval, err := tx.ClosureApprovalForInstance(p.ctx, checkpoint.InstanceID)
		if err != nil {
			return err
		}
		if approval == nil || approval.Actor != domain.ClosureApprovalActorPolicy {
			return fmt.Errorf("closure approval = %+v, want policy actor", approval)
		}
		base = approval.BaseSHA
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if base != advanced {
		t.Fatalf("closure approval binds base %s, want the handoff tip %s", base, advanced)
	}
}

// TestHandoffConflictBlocks: a head that conflicts with the tip has no merge
// to verify, so the run ends on publish_blocked naming the conflict.
func TestHandoffConflictBlocks(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	_, reviews := p.advanceBaseBeforePublication(t, "README.md", "conflicting upstream change\n")
	for range 3 {
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatal(err)
		}
	}
	p.assertHandoffBlocked(t, domain.PublicationBlockHandoffConflict)
	p.assertNoReviewAtTheTip(t, reviews)
	if len(reviews.requests) != 1 || p.room.runs != 1 {
		t.Fatalf("conflicting handoff reviewed %d times, verified %d times, want 1 and 1",
			len(reviews.requests), p.room.runs)
	}
}

// failHandoffVerification rebuilds the engine so the room fails every
// verification after the run's own, which the handoff's verification is.
func (p *productionPublicationHarness) failHandoffVerification(t *testing.T, fail func(*productionRoom)) {
	t.Helper()
	p.workflow = p.newEngine(t, productionCrashSeams{
		afterVerification: func() error {
			fail(p.room)
			return nil
		},
	}, true)
}

// TestHandoffVerificationFailureBlocks: a merge into the tip that fails
// verification ends the run on publish_blocked naming the handoff's
// verification, with nothing published and no later pass verifying again.
func TestHandoffVerificationFailureBlocks(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	p.advanceBaseBeforePublication(t, "UPSTREAM.md", "upstream change\n")
	p.failHandoffVerification(t, func(room *productionRoom) { room.fail = true })
	for range 3 {
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatal(err)
		}
	}
	p.assertHandoffBlocked(t, domain.PublicationBlockHandoffVerification)
	if p.room.runs != 2 {
		t.Fatalf("failed handoff verified %d times, want 2", p.room.runs)
	}
	if check := p.handoffCheck(t); check.Merge != nil {
		t.Fatalf("failed handoff recorded merge %#v", check.Merge)
	}
}

// TestHandoffVerificationMemoryLimitBlocks: a merge whose verification room
// is killed at its memory limit would meet the same limit on every retry, so
// the run blocks naming the limit.
func TestHandoffVerificationMemoryLimitBlocks(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	p.advanceBaseBeforePublication(t, "UPSTREAM.md", "upstream change\n")
	p.failHandoffVerification(t, func(room *productionRoom) {
		room.runErr = &ward.MemoryLimitError{
			Class: ward.LaunchVerification, Size: ward.DefaultLaunchSize(ward.LaunchVerification),
			Cause: errors.New("verification container exited 137"),
		}
	})
	for range 3 {
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatal(err)
		}
	}
	p.assertHandoffBlocked(t, domain.PublicationBlockVerificationMemoryLimit)
	if p.room.runs != 2 {
		t.Fatalf("killed handoff verified %d times, want 2", p.room.runs)
	}
}

// TestHandoffDoesNotReresolveAnAdvanceAfterTheSeal: the tip is resolved once
// per publication. A target that advances again while the merge is verified
// does not restart the check: the handoff proves the merge into the sealed
// tip and publishes.
func TestHandoffDoesNotReresolveAnAdvanceAfterTheSeal(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	first, reviews := p.advanceBaseBeforePublication(t, "UPSTREAM.md", "upstream change\n")
	// The handoff's verification is the run's second; stop the process right
	// after it, before the merge is proved.
	p.workflow = p.newEngine(t, productionCrashSeams{
		afterVerification: func() error {
			if p.room.runs >= 2 {
				return errors.New("stop after the handoff verification")
			}
			return nil
		},
	}, true)
	if result, err := p.reconcileLanes(); err == nil {
		t.Fatalf("handoff seam did not interrupt the pass: %#v", result)
	}
	if check := p.handoffCheck(t); check.TipSHA != first || check.Merge != nil {
		t.Fatalf("interrupted handoff = %#v, want sealed at %s and unproved", check, first)
	}
	p.advanceBase(t, "FURTHER.md", "further upstream change\n")
	p.audit.AuditedCommitSHA = runGit(t, p.baseDir, "rev-parse", "HEAD")
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.reconcileUntilReady(t, 3)

	p.assertHandoffVerified(t, first)
	p.assertReviewedOnce(t, reviews)
	// The resumed pass rebuilds the same merge into the sealed tip and finds
	// its verification checkpoint, so nothing is verified again.
	if p.room.runs != 2 || p.transport.MergeCount() != 2 {
		t.Fatalf("verification runs = %d, merges = %d, want 2 and 2 after the further advance",
			p.room.runs, p.transport.MergeCount())
	}
	if refs, prs := p.forge.Counts(); refs != 1 || prs != 1 {
		t.Fatalf("publication effects = %d refs, %d prs, want one of each", refs, prs)
	}
}

// advanceBaseDuringThePass rebuilds the engine so the target advances while
// the pass verifies the head: after the pass fetched its checkout and before
// the handoff's audit. It returns the tip the audit then observes.
func (p *productionPublicationHarness) advanceBaseDuringThePass(t *testing.T) string {
	t.Helper()
	tip := ""
	p.liveAudit = true
	p.workflow = p.newEngine(t, productionCrashSeams{
		afterVerification: func() error {
			if tip == "" {
				tip = p.advanceBase(t, "UPSTREAM.md", "upstream change\n")
				p.audit.AuditedCommitSHA = tip
			}
			return nil
		},
	}, true)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	if tip == "" {
		t.Fatal("the pass verified nothing")
	}
	if check := p.handoffCheck(t); check.TipSHA != tip || check.Merge != nil {
		t.Fatalf("handoff after a mid-pass advance = %#v, want sealed at %s and unproved", check, tip)
	}
	if blocked, err := p.attention.GetAttentionItem(p.ctx, domain.ProductionBlockedItemID(p.runID)); err == nil {
		t.Fatalf("a tip newer than the pass's checkout blocked the run: %#v", blocked.Item)
	}
	return tip
}

// TestHandoffAtATipNewerThanTheCheckoutRetries: a push to the target between
// the pass's fetch and the handoff's audit seals a tip that checkout lacks.
// The pass retries, and the next fetch holds the tip, so the handoff verifies
// the merge into it and publishes.
func TestHandoffAtATipNewerThanTheCheckoutRetries(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	reviews := p.recordReviews(t)
	tip := p.advanceBaseDuringThePass(t)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.reconcileUntilReady(t, 3)

	p.assertHandoffVerified(t, tip)
	p.assertReviewedOnce(t, reviews)
	if refs, prs := p.forge.Counts(); refs != 1 || prs != 1 {
		t.Fatalf("publication effects = %d refs, %d prs, want one of each", refs, prs)
	}
}

// TestHandoffAtARewrittenTipBlocks: a sealed tip the target branch no longer
// holds when a later pass fetches it was rewritten away. The tip is not
// re-resolved, so the run blocks.
func TestHandoffAtARewrittenTipBlocks(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	p.recordReviews(t)
	p.advanceBaseDuringThePass(t)
	runGit(t, p.baseDir, "reset", "-q", "--hard", "HEAD~1")
	p.advanceBase(t, "REWRITTEN.md", "rewritten upstream\n")
	p.audit.AuditedCommitSHA = runGit(t, p.baseDir, "rev-parse", "HEAD")
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	for range 2 {
		p.now = p.now.Add(time.Minute)
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatal(err)
		}
	}
	p.assertHandoffBlocked(t, domain.PublicationBlockBaseAdvanced)
}

// TestHandoffPublishesTheDispositionHistory: the run's one round declined a
// finding and deferred another at the admitted base; the base advanced
// before publication, and the handoff publishes that round's history
// unchanged.
func TestHandoffPublishesTheDispositionHistory(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	driver := p.configureAdjudicator(t)
	findings := []domain.Finding{
		{
			ID: "declined-finding", RunID: p.runID, Source: "codex_local", Severity: domain.FindingSeverityP2,
			Location: &domain.FindingLocation{Path: "README.md", StartLine: 1, EndLine: 1},
			Message:  "outside the approved task", RawText: "outside the approved task", CreatedAt: p.now,
		},
		{
			ID: "deferred-finding", RunID: p.runID, Source: "codex_local", Severity: domain.FindingSeverityP2,
			Location: &domain.FindingLocation{Path: "README.md", StartLine: 1, EndLine: 1},
			Message:  "worth a follow-up", RawText: "worth a follow-up", CreatedAt: p.now,
		},
	}
	p.startAndRecordExport(t)
	// The rebuilt engine scripts a clean first round, so the findings round
	// is scripted after it.
	advanced, _ := p.advanceBaseBeforePublication(t, "UPSTREAM.md", "upstream change\n")
	p.reviewer.Script(engine.ProductionReviewInvocationID(p.runID, 1), fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: p.baseSHA, HeadSHA: p.replay.HeadSHA,
			CompletedAt: p.now, CompletionEvidence: productionDigest([]byte("first round findings")),
			Findings: findings,
		},
	})
	// Low materiality keeps both findings off the remediation fast path, so
	// the adjudicator's routes decide them.
	classified := inferencefake.Script{Response: inference.Response{
		Output:       []byte(`{"materiality":"low","confidence":"high","note":"minor"}`),
		ComputeUnits: 3,
	}}
	driver.Script(inference.ClassifierSiteID, classified, classified)
	scriptAdjudication(t, driver, declineVerdict(findings[0].ID), deferVerdict(findings[1].ID))
	p.reconcileUntilReady(t, 4)

	p.assertHandoffVerified(t, advanced)
	if refs, prs := p.forge.Counts(); refs != 1 || prs != 1 {
		t.Fatalf("publication effects = %d refs, %d prs, want one of each", refs, prs)
	}
	body := p.forge.PullRequests()[0].Body
	for _, want := range []string{
		"### Review Round 1",
		string(findings[0].ID), string(findings[1].ID),
		string(domain.ReviewDispositionDeclined), string(domain.ReviewDispositionDeferred),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("published history lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "### Review Round 2") {
		t.Errorf("published history names a round the handoff never ran:\n%s", body)
	}
}

// TestHandoffAfterARemediationPublishes: a remediation continuation whose
// target advanced verifies the remediated head's merge into the tip and
// publishes it, keeping the rounds the remediation spent. Its remediation
// marker names the admitted base, so it authenticates instead of
// quarantining the task.
func TestHandoffAfterARemediationPublishes(t *testing.T) {
	t.Parallel()
	p, _ := prepareDispatchedRemediationPublicationTask(t)
	advanced, reviews := p.advanceBaseBeforePublication(t, "UPSTREAM.md", "upstream change\n")
	p.reconcileUntilReady(t, 8)

	p.assertHandoffVerified(t, advanced)
	p.assertNoReviewAtTheTip(t, reviews)
	ready := p.currentReadyItem(t)
	if ready.PRHeadSHA != p.replay.HeadSHA || ready.Status != domain.StatusOpen {
		t.Fatalf("ready item = %#v, want open at the remediated head", ready)
	}
	if refs, prs := p.forge.Counts(); refs != 1 || prs != 1 {
		t.Fatalf("forge effects = %d refs, %d PRs; want one each", refs, prs)
	}
	items, err := p.attention.ListAttentionItems(p.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if strings.HasPrefix(string(item.Item.ID), "remediation-marker-quarantined") {
			t.Fatalf("handoff quarantined its remediation marker: %#v", item.Item)
		}
	}
}

// queueRetiredReentry leaves the run as an earlier daemon would have: the
// first publication reviewed and unpublished, and a v5 re-entry task queued
// for a base advance in its place. With intent, that daemon also committed
// the re-entry's publication intent before it stopped.
func (p *productionPublicationHarness) queueRetiredReentry(
	t *testing.T, intent bool,
) (domain.PublicationSuccessor, *reviewedTree) {
	t.Helper()
	p.startAndRecordExport(t)
	advanced, reviews := p.advanceBaseBeforePublication(t, "UPSTREAM.md", "upstream change\n")
	// Stop the first publication after its handoff verification, as the
	// earlier daemon's publisher refusal left it: reviewed, unpublished, and
	// holding a bare reservation.
	p.workflow = p.newEngine(t, productionCrashSeams{
		afterVerification: func() error {
			if p.room.runs >= 2 {
				return errors.New("stop before the first publication publishes")
			}
			return nil
		},
	}, true)
	if result, err := p.reconcileLanes(); err == nil {
		t.Fatalf("seam did not interrupt the first publication: %#v", result)
	}

	successor := domain.PublicationSuccessor{
		Version: domain.PublicationUnpublishedReentryVersion, Origin: domain.PublicationSuccessorUnpublishedBaseAdvance,
		RunID: p.runID, PredecessorItemID: domain.ProductionReadyItemID(p.runID),
		PriorReviewInvocationID: engine.ProductionReviewInvocationID(p.runID, 1), ReviewRound: 2,
		Reentry: &domain.PublicationSuccessorReentry{
			Reason: domain.ReadinessInvalidationBaseAdvanced, BaseSHA: advanced, HeadSHA: p.replay.HeadSHA,
		},
	}
	if err := p.store.WriteInternal(p.ctx, func(tx *store.InternalTx) error {
		pending, err := tx.ListPendingOutbox(p.ctx, engine.KindProductionPublicationRequested)
		if err != nil {
			return err
		}
		if len(pending) != 1 {
			return fmt.Errorf("pending publication tasks = %d, want the first", len(pending))
		}
		first := pending[0]
		var task map[string]any
		if err := json.Unmarshal(first.Payload, &task); err != nil {
			return err
		}
		task["publication_invocation_id"] = string(successor.PublicationID())
		task["successor"] = successor
		task["authorizing_task_key"] = first.IdempotencyKey
		payload, err := json.Marshal(task)
		if err != nil {
			return err
		}
		authority, err := json.Marshal(successor)
		if err != nil {
			return err
		}
		if _, _, err := tx.EnqueueOutbox(p.ctx, successor.Key(), domain.PublicationSuccessorKind, authority); err != nil {
			return err
		}
		if err := tx.MarkOutboxDispatched(p.ctx, successor.Key()); err != nil {
			return err
		}
		reservation, err := publish.NewReservation(successor.PublicationID(), p.runID)
		if err != nil {
			return err
		}
		if err := publish.ClaimInvocation(p.ctx, tx, reservation); err != nil {
			return err
		}
		if _, _, err := tx.EnqueueOutbox(p.ctx, successor.TaskKey(),
			engine.KindProductionPublicationRequested, payload); err != nil {
			return err
		}
		if intent {
			// Committing settles the reservation row in place. The payload
			// stands in for the earlier daemon's intent: retirement reads
			// only that one is committed.
			key, err := publish.IntentKey(successor.PublicationID(), publish.IntentKindPublication)
			if err != nil {
				return err
			}
			reserved, err := tx.GetOutbox(p.ctx, key)
			if err != nil {
				return err
			}
			if _, promoted, err := tx.PromoteOutbox(p.ctx, key, publish.IntentKindReservation,
				publish.IntentKindPublication, reserved.Payload, []byte(`{}`)); err != nil || !promoted {
				return fmt.Errorf("commit the re-entry's intent: promoted %t: %w", promoted, err)
			}
		}
		return tx.MarkOutboxDispatched(p.ctx, first.IdempotencyKey)
	}); err != nil {
		t.Fatalf("queue the legacy re-entry: %v", err)
	}
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	return successor, reviews
}

// TestQueuedUnpublishedReentryIsRetired: a v5 re-entry task an earlier daemon
// queued for a base advance is never run. Its cycle ends on publish_blocked
// naming the retired rule, with nothing reviewed, verified, or published.
func TestQueuedUnpublishedReentryIsRetired(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	successor, reviews := p.queueRetiredReentry(t, false)
	runs, merges, requests := p.room.runs, p.transport.MergeCount(), len(reviews.requests)
	blocked := 0
	for range 3 {
		result, err := p.reconcileLanes()
		if err != nil {
			t.Fatal(err)
		}
		blocked += result.BlockedItemsCreated
	}
	if blocked != 1 {
		t.Fatalf("retired re-entry created %d blocked items, want 1", blocked)
	}
	item, err := p.attention.GetAttentionItem(p.ctx, successor.BlockedItemID())
	if err != nil {
		t.Fatal(err)
	}
	if item.Item.Type != domain.AttentionPublishBlocked || item.Item.Status != domain.StatusOpen ||
		item.Item.Reason != domain.PublicationBlockReentryRetired {
		t.Fatalf("retired re-entry item = %#v, want open publish_blocked: %s",
			item.Item, domain.PublicationBlockReentryRetired)
	}
	if p.room.runs != runs || p.transport.MergeCount() != merges || len(reviews.requests) != requests {
		t.Fatalf("retired re-entry ran work: verifications %d->%d, merges %d->%d, reviews %d->%d",
			runs, p.room.runs, merges, p.transport.MergeCount(), requests, len(reviews.requests))
	}
	if refs, prs := p.forge.Counts(); refs != 0 || prs != 0 {
		t.Fatalf("retired re-entry produced forge effects = %d refs, %d prs", refs, prs)
	}
}

// TestRetiredReentryWithACommittedIntentHolds: when the earlier daemon had
// already committed the retired cycle's publication intent, the run holds
// where a person can see it instead of blocking over a live intent, and
// nothing runs.
func TestRetiredReentryWithACommittedIntentHolds(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	successor, reviews := p.queueRetiredReentry(t, true)
	runs, merges, requests := p.room.runs, p.transport.MergeCount(), len(reviews.requests)
	for range 3 {
		p.now = p.now.Add(time.Minute)
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatal(err)
		}
	}
	item, err := p.attention.GetAttentionItem(p.ctx, successor.BlockedItemID())
	if err != nil {
		t.Fatal(err)
	}
	if item.Item.Status != domain.StatusOpen || item.Item.Reason == domain.PublicationBlockReentryRetired ||
		!strings.HasPrefix(item.Item.Reason, "Publication is durably held") {
		t.Fatalf("retired re-entry with an intent = %#v, want an open durable hold", item.Item)
	}
	if p.room.runs != runs || p.transport.MergeCount() != merges || len(reviews.requests) != requests {
		t.Fatalf("held re-entry ran work: verifications %d->%d, merges %d->%d, reviews %d->%d",
			runs, p.room.runs, merges, p.transport.MergeCount(), requests, len(reviews.requests))
	}
	if refs, prs := p.forge.Counts(); refs != 0 || prs != 0 {
		t.Fatalf("held re-entry produced forge effects = %d refs, %d prs", refs, prs)
	}
}

// TestHandoffRecoversAnIntentCommittedBeforeHandoffChecks: a publication
// intent an earlier daemon committed before handoff checks existed has no
// sealed tip. It is recovered under the decision it was made with, at the
// admitted base, even after the target moved: no tip is sealed and no merge
// is built.
func TestHandoffRecoversAnIntentCommittedBeforeHandoffChecks(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	p.transport.FailNextPush()
	if result, err := p.reconcileLanes(); err != nil || result != (engine.ReconcileResult{}) {
		t.Fatalf("contained push failure = %#v, %v", result, err)
	}
	raw, err := sql.Open("sqlite", p.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(p.ctx,
		`DELETE FROM publication_handoff_checks WHERE run_id = ?`, p.runID); err != nil {
		_ = raw.Close()
		t.Fatalf("drop the seal an earlier daemon never wrote: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	p.advanceBaseBeforePublication(t, "UPSTREAM.md", "upstream change\n")
	p.reconcileUntilReady(t, 3)

	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		publication, err := tx.CurrentPublicationInvocationID(p.ctx, p.runID)
		if err != nil {
			return err
		}
		_, err = tx.GetHandoffCheck(p.ctx, p.runID, publication)
		return err
	}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("recovered intent's handoff check = %v, want none sealed", err)
	}
	if p.transport.MergeCount() != 0 {
		t.Fatalf("recovered intent built %d merges, want none", p.transport.MergeCount())
	}
	if refs, prs := p.forge.Counts(); refs != 1 || prs != 1 {
		t.Fatalf("publication effects = %d refs, %d prs, want one of each", refs, prs)
	}
}

// TestInterruptedReentryRetirementRecovers: a retirement stopped after its
// block landed and before the task finished converges on the next pass, with
// the one block it wrote.
func TestInterruptedReentryRetirementRecovers(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	successor, _ := p.queueRetiredReentry(t, false)
	p.workflow = p.newEngine(t, productionCrashSeams{
		afterBlocked: func() error { return errors.New("stop after the retirement block") },
	}, true)
	if result, err := p.reconcileLanes(); err == nil {
		t.Fatalf("blocked seam did not interrupt the retirement: %#v", result)
	}
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	for range 2 {
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatal(err)
		}
	}
	item, err := p.attention.GetAttentionItem(p.ctx, successor.BlockedItemID())
	if err != nil {
		t.Fatal(err)
	}
	if item.Item.Status != domain.StatusOpen || item.Item.Reason != domain.PublicationBlockReentryRetired ||
		item.Item.ItemVersion != 1 {
		t.Fatalf("recovered retirement item = %#v, want open: %s", item.Item, domain.PublicationBlockReentryRetired)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		pending, err := tx.ListPendingOutbox(p.ctx, engine.KindProductionPublicationRequested)
		if err != nil {
			return err
		}
		if len(pending) != 0 {
			t.Errorf("recovered retirement left %d publication tasks pending", len(pending))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
