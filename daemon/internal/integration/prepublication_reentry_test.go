package integration_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	inferencefake "github.com/freeside-ai/freeside/daemon/internal/inference/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// advanceBaseBeforePublication moves the target branch after the run was
// admitted and before its first publication, and rebuilds the engine so the
// publisher's fresh audit observes the new tip.
func (p *productionPublicationHarness) advanceBaseBeforePublication(
	t *testing.T, name, content string,
) (string, *reviewedTree) {
	t.Helper()
	advanced := p.advanceBase(t, name, content)
	p.audit.AuditedCommitSHA = advanced
	return advanced, p.recordReviews(t)
}

// unpublishedReentry reads the run's current successor and requires it to be
// a re-entry before publication against baseSHA.
func (p *productionPublicationHarness) unpublishedReentry(
	t *testing.T, baseSHA string,
) domain.PublicationSuccessor {
	t.Helper()
	var successor *domain.PublicationSuccessor
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		successor, err = tx.CurrentPublicationSuccessor(p.ctx, p.runID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if successor == nil || !successor.RepublishesUnpublished() || successor.Reentry == nil ||
		successor.Reentry.Reason != domain.ReadinessInvalidationBaseAdvanced ||
		successor.Reentry.BaseSHA != baseSHA || successor.Reentry.HeadSHA != p.replay.HeadSHA {
		t.Fatalf("current successor = %#v, want a re-entry before publication at %s", successor, baseSHA)
	}
	return *successor
}

// assertUnpublishedReentryBlocked checks that the re-entry stopped with a
// publish_blocked item naming reason, and that nothing reached the forge.
func (p *productionPublicationHarness) assertUnpublishedReentryBlocked(
	t *testing.T, successor domain.PublicationSuccessor, reason string,
) {
	t.Helper()
	blocked, err := p.attention.GetAttentionItem(p.ctx, successor.BlockedItemID())
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Item.Type != domain.AttentionPublishBlocked || blocked.Item.Reason != reason ||
		blocked.Item.Status != domain.StatusOpen {
		t.Fatalf("re-entry blocked item = %#v, want open publish_blocked: %s", blocked.Item, reason)
	}
	if refs, prs := p.forge.Counts(); refs != 0 || prs != 0 {
		t.Fatalf("blocked re-entry produced forge effects = %d refs, %d prs", refs, prs)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetAttentionItem(p.ctx, successor.ReadyItemID()); err == nil {
			t.Error("a blocked re-entry wrote a ready item")
		}
		if hold, held, err := tx.GetRunHold(p.ctx, p.runID); err != nil || held {
			t.Errorf("a blocked re-entry left run hold %#v, %v", hold, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestUnpublishedRunReentersAfterBaseAdvance: plan revision 89. A run whose
// target base advanced before its first publication re-enters against the
// new base instead of blocking: the unchanged head is reviewed as its merge
// into the new base, verified there, and published once, on the same run.
func TestUnpublishedRunReentersAfterBaseAdvance(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	advanced, reviews := p.advanceBaseBeforePublication(t, "UPSTREAM.md", "upstream change\n")
	p.scriptCleanReview(2, advanced, p.replay.HeadSHA)

	sealed, err := p.reconcileLanes()
	if err != nil {
		t.Fatal(err)
	}
	if sealed.ReadyItemsCreated != 0 || sealed.BlockedItemsCreated != 0 {
		t.Fatalf("base-advance pass = %#v, want a re-entry and no item", sealed)
	}
	if refs, prs := p.forge.Counts(); refs != 0 || prs != 0 {
		t.Fatalf("the refused publication reached the forge: %d refs, %d prs", refs, prs)
	}
	successor := p.unpublishedReentry(t, advanced)
	if successor.ReviewRound != 2 {
		t.Fatalf("re-entry review round = %d, want 2", successor.ReviewRound)
	}

	p.reconcileUntilReady(t, 3)
	if len(reviews.requests) != 2 {
		t.Fatalf("review requests = %d, want the first round and the re-review", len(reviews.requests))
	}
	request, tree := reviews.requests[1], reviews.files[1]
	if request.Round != 2 || request.BaseSHA != advanced || request.HeadSHA != p.replay.HeadSHA ||
		request.EvaluatedSHA == "" || request.EvaluatedSHA == p.replay.HeadSHA ||
		request.EvaluatedSHA == advanced {
		t.Fatalf("re-review request = %#v, want the head's merge into %s", request, advanced)
	}
	if tree["README.md"] != "production change\n" || tree["UPSTREAM.md"] != "upstream change\n" {
		t.Fatalf("re-reviewed tree = %#v, want the head's change on the advanced base", tree)
	}
	ready := p.currentReadyItem(t)
	if ready.ID != successor.ReadyItemID() || ready.PRHeadSHA != p.replay.HeadSHA {
		t.Fatalf("ready item = %#v, want %s at the unchanged head", ready, successor.ReadyItemID())
	}
	if refs, prs := p.forge.Counts(); refs != 1 || prs != 1 {
		t.Fatalf("publication effects = %d refs, %d prs, want one of each", refs, prs)
	}
	pr := p.forge.PullRequests()[0]
	if pr.HeadSHA != p.replay.HeadSHA {
		t.Fatalf("published head = %s, want the unchanged head %s", pr.HeadSHA, p.replay.HeadSHA)
	}
	if p.room.runs != 2 || p.transport.MergeCount() != 1 {
		t.Fatalf("verification runs = %d, merges = %d, want 2 and 1",
			p.room.runs, p.transport.MergeCount())
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		review, err := tx.LatestReviewRecord(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if review.Round != 2 || review.Outcome != domain.ReviewClean ||
			review.BaseSHA != advanced || review.HeadSHA != p.replay.HeadSHA {
			t.Errorf("re-review record = %#v", review)
		}
		published, err := tx.PublishedPublicationInvocationID(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if published != successor.PublicationID() {
			t.Errorf("published invocation = %s, want the re-entry's %s", published, successor.PublicationID())
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

// TestUnpublishedReentryBindsTheClosureToTheNewBase: the first cycle bound
// its policy-approved close to the merge into the admitted base. The
// re-entry publishes against the newer base, so the approval that lets it
// write Closes must name that base, never the admitted one.
func TestUnpublishedReentryBindsTheClosureToTheNewBase(t *testing.T) {
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
	driver := scriptPublicationSites(t, p, explain, &propose)
	// Each cycle is its own publication, so each asks both sites once.
	driver.Script(inference.PublicationAuthorExplainSiteID, explain, explain)
	driver.Script(inference.PublicationAuthorProposeSiteID, propose, propose)
	p.startAndRecordExport(t)
	advanced, _ := p.advanceBaseBeforePublication(t, "UPSTREAM.md", "upstream change\n")
	p.scriptCleanReview(2, advanced, p.replay.HeadSHA)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	successor := p.unpublishedReentry(t, advanced)
	p.reconcileUntilReady(t, 3)

	prs := p.forge.PullRequests()
	if len(prs) != 1 || !strings.Contains(prs[0].Body, "Closes #82") {
		t.Fatalf("re-entry did not publish the policy-approved close: %d PRs", len(prs))
	}
	approvalBase := func(publication domain.InvocationID) string {
		t.Helper()
		var base string
		if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
			entry, err := tx.GetInbox(p.ctx, "production-closure/"+string(p.runID)+"/"+string(publication))
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
		return base
	}
	if got := approvalBase(successor.PublicationID()); got != advanced {
		t.Fatalf("re-entry closure approval binds base %s, want the advanced base %s", got, advanced)
	}
}

// TestUnpublishedReentryConflictBlocks: a head that conflicts with the new
// base cannot be re-reviewed there, so the re-entry blocks naming the
// conflict, with nothing published.
func TestUnpublishedReentryConflictBlocks(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	advanced, reviews := p.advanceBaseBeforePublication(t, "README.md", "conflicting upstream change\n")
	for range 3 {
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatal(err)
		}
	}
	successor := p.unpublishedReentry(t, advanced)
	p.assertUnpublishedReentryBlocked(t, successor, domain.PublicationBlockBaseAdvancedConflict)
	if len(reviews.requests) != 1 || p.room.runs != 1 {
		t.Fatalf("conflicting re-entry reviewed %d times, verified %d times, want 1 and 1",
			len(reviews.requests), p.room.runs)
	}
}

// TestUnpublishedReentryAtTheRoundLimitBlocks: the re-review counts toward
// hard_round_limit, so a run that spent its last round before the advance
// blocks naming the limit instead of re-entering.
func TestUnpublishedReentryAtTheRoundLimitBlocks(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarnessWithPolicyKeys(t, "", []domain.PolicyKey{{
		Key: "review.hard_round_limit", Value: fmt.Sprint(1),
		Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride,
			Digest: submissionDigest("run-production-publication", "review-hard-round-limit"),
		},
	}})
	p.startAndRecordExport(t)
	p.advanceBaseBeforePublication(t, "UPSTREAM.md", "upstream change\n")
	result, err := p.reconcileLanes()
	if err != nil {
		t.Fatal(err)
	}
	if result.BlockedItemsCreated != 1 || result.ReadyItemsCreated != 0 {
		t.Fatalf("round-limit result = %#v", result)
	}
	blocked, err := p.attention.GetAttentionItem(p.ctx,
		domain.ItemID("production-publish-blocked-"+string(p.runID)))
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Item.Reason != domain.PublicationBlockBaseAdvancedRoundLimit {
		t.Fatalf("round-limit block reason = %q", blocked.Item.Reason)
	}
	if refs, prs := p.forge.Counts(); refs != 0 || prs != 0 {
		t.Fatalf("round-limit block produced forge effects = %d refs, %d prs", refs, prs)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		chain, err := tx.PublicationSuccessorChain(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if len(chain) != 0 {
			t.Errorf("an exhausted run sealed %d successors", len(chain))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestUnpublishedReentrySealsAgainAfterASecondAdvance: a base that moves
// again while the re-entry cycle runs refuses its publication the same way,
// and the run re-enters once more against the newer tip, spending one more
// review round, before it publishes there.
func TestUnpublishedReentrySealsAgainAfterASecondAdvance(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	first, _ := p.advanceBaseBeforePublication(t, "UPSTREAM.md", "upstream change\n")
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	firstReentry := p.unpublishedReentry(t, first)

	second, _ := p.advanceBaseBeforePublication(t, "FOLLOWUP.md", "second upstream change\n")
	p.scriptCleanReview(2, first, p.replay.HeadSHA)
	p.scriptCleanReview(3, second, p.replay.HeadSHA)
	p.reconcileUntilReady(t, 4)

	secondReentry := p.unpublishedReentry(t, second)
	if secondReentry.ReviewRound != 3 || secondReentry.PublicationID() == firstReentry.PublicationID() {
		t.Fatalf("second re-entry = %#v, want round 3 under a new publication", secondReentry)
	}
	if ready := p.currentReadyItem(t); ready.ID != secondReentry.ReadyItemID() {
		t.Fatalf("ready item = %s, want the second re-entry's %s", ready.ID, secondReentry.ReadyItemID())
	}
	if refs, prs := p.forge.Counts(); refs != 1 || prs != 1 {
		t.Fatalf("publication effects = %d refs, %d prs, want one of each", refs, prs)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		chain, err := tx.PublicationSuccessorChain(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if len(chain) != 2 {
			t.Errorf("successor chain = %d links, want 2", len(chain))
		}
		review, err := tx.LatestReviewRecord(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if review.Round != 3 || review.BaseSHA != second || review.Outcome != domain.ReviewClean {
			t.Errorf("final review = %#v", review)
		}
		published, err := tx.PublishedPublicationInvocationID(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if published != secondReentry.PublicationID() {
			t.Errorf("published invocation = %s, want %s", published, secondReentry.PublicationID())
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestUnpublishedReentryVerificationFailureBlocks: the merge into the new
// base is verified before it is re-reviewed; a failing merge blocks naming
// verification, with nothing published.
func TestUnpublishedReentryVerificationFailureBlocks(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	advanced, reviews := p.advanceBaseBeforePublication(t, "UPSTREAM.md", "upstream change\n")
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	successor := p.unpublishedReentry(t, advanced)
	p.room.fail = true
	for range 3 {
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatal(err)
		}
	}
	p.assertUnpublishedReentryBlocked(t, successor, domain.PublicationBlockBaseAdvancedVerification)
	if len(reviews.requests) != 1 || p.room.runs != 2 {
		t.Fatalf("failed re-entry reviewed %d times, verified %d times, want 1 and 2",
			len(reviews.requests), p.room.runs)
	}
}

// TestUnpublishedReentryFindingsGoToAPerson: a remediation is admitted at
// the run's admitted base, not the advanced one, so a re-review finding
// routed to a fix starts no remediation. The round goes to a person on a
// review dispute, and nothing is published.
func TestUnpublishedReentryFindingsGoToAPerson(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	configureSuccessorJudgments(t, p)
	p.startAndRecordExport(t)
	advanced, _ := p.advanceBaseBeforePublication(t, "UPSTREAM.md", "upstream change\n")
	finding := domain.Finding{
		ID: "reentry-finding", RunID: p.runID, Source: "codex_local", Severity: domain.FindingSeverityP1,
		Location: &domain.FindingLocation{Path: "README.md", StartLine: 1, EndLine: 1},
		Message:  "the change needs one more line on the new base",
		RawText:  "the change needs one more line on the new base", CreatedAt: p.now,
	}
	p.reviewer.Script(engine.ProductionReviewInvocationID(p.runID, 2), fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: advanced, HeadSHA: p.replay.HeadSHA,
			CompletedAt: p.now, CompletionEvidence: productionDigest([]byte("re-entry review findings")),
			Findings: []domain.Finding{finding},
		},
	})
	started := 0
	for range 4 {
		result, err := p.reconcileLanes()
		if err != nil || result.ReadyItemsCreated != 0 {
			t.Fatalf("re-entry with a finding = %#v, %v", result, err)
		}
		started += result.InvocationsStarted
	}
	if started != 0 {
		t.Fatalf("re-entry started %d invocations, want no remediation", started)
	}
	p.unpublishedReentry(t, advanced)
	dispute, err := p.attention.GetAttentionItem(p.ctx, productionReviewItemIDForTest(p.runID, 2))
	if err != nil {
		t.Fatal(err)
	}
	if dispute.Item.Type != domain.AttentionReviewDispute || dispute.Item.Status != domain.StatusOpen ||
		!strings.Contains(dispute.Item.Reason, "starts no remediation") {
		t.Fatalf("re-entry dispute = %#v", dispute.Item)
	}
	if refs, prs := p.forge.Counts(); refs != 0 || prs != 0 {
		t.Fatalf("escalated re-entry produced forge effects = %d refs, %d prs", refs, prs)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetOutbox(p.ctx, "inv-remediate-2-"+string(p.runID)); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("re-entry queued a remediation: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestUnpublishedReentryPublishesTheDispositionHistory: the run's first round
// declined one finding and deferred another at the admitted base; the base
// advanced before publication, and the clean re-review at the new base
// publishes with both rounds, and both dispositions, in the history.
func TestUnpublishedReentryPublishesTheDispositionHistory(t *testing.T) {
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
	p.scriptCleanReview(2, advanced, p.replay.HeadSHA)
	p.reconcileUntilReady(t, 4)

	successor := p.unpublishedReentry(t, advanced)
	if ready := p.currentReadyItem(t); ready.ID != successor.ReadyItemID() {
		t.Fatalf("ready item = %s, want %s", ready.ID, successor.ReadyItemID())
	}
	if refs, prs := p.forge.Counts(); refs != 1 || prs != 1 {
		t.Fatalf("publication effects = %d refs, %d prs, want one of each", refs, prs)
	}
	body := p.forge.PullRequests()[0].Body
	for _, want := range []string{
		"### Review Round 1", "### Review Round 2",
		string(findings[0].ID), string(findings[1].ID),
		string(domain.ReviewDispositionDeclined), string(domain.ReviewDispositionDeferred),
		advanced,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("published history lacks %q:\n%s", want, body)
		}
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		for i, want := range []domain.ReviewDisposition{
			domain.ReviewDispositionDeclined, domain.ReviewDispositionDeferred,
		} {
			disposition, err := tx.GetFindingDisposition(p.ctx, findings[i].ID, 1)
			if err != nil {
				return err
			}
			if disposition.Disposition != want {
				t.Errorf("%s disposition = %s, want %s", findings[i].ID, disposition.Disposition, want)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A run that remediated before its base advanced re-enters with the
// remediated head. Its remediation marker names the base the run was admitted
// at, which the re-entry task still carries in its replay, so the marker
// authenticates instead of quarantining the task.
func TestUnpublishedReentryAfterARemediationPublishes(t *testing.T) {
	p, _ := prepareDispatchedRemediationPublicationTask(t)
	advanced, _ := p.advanceBaseBeforePublication(t, "UPSTREAM.md", "upstream change\n")
	p.scriptCleanReview(3, advanced, p.replay.HeadSHA)
	p.reconcileUntilReady(t, 8)

	successor := p.unpublishedReentry(t, advanced)
	ready, err := p.attention.GetAttentionItem(p.ctx, successor.ReadyItemID())
	if err != nil {
		t.Fatal(err)
	}
	if ready.Item.PRHeadSHA != p.replay.HeadSHA || ready.Item.Status != domain.StatusOpen {
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
			t.Fatalf("re-entry quarantined its remediation marker: %#v", item.Item)
		}
	}
}
