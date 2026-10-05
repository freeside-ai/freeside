package integration_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func externalMaintainer() domain.ExternalReviewer {
	return domain.ExternalReviewer{
		Forge: domain.ExternalReviewForgeGitHub, AccountID: 4101, Login: "maintainer",
		Authority: domain.ExternalReviewDriveRound,
	}
}

// listExternalReviewers records and activates the repository's trust profile
// again with reviewers as its allowlist and everything else unchanged, as a
// second onboarding pass does when the owner lists a reviewer.
func (p *productionPublicationHarness) listExternalReviewers(
	t *testing.T, reviewers ...domain.ExternalReviewer,
) domain.AutomationTrustProfile {
	t.Helper()
	current := p.profile
	revised, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo: current.Repo, RepositoryID: current.RepositoryID,
		PRExecution:                current.PRExecution,
		CandidateAutomationChanges: current.CandidateAutomationChanges,
		PRGitHubTokenPermissions:   current.PRGitHubTokenPermissions,
		AllowOIDC:                  current.AllowOIDC,
		AllowEnvironmentSecrets:    current.AllowEnvironmentSecrets,
		AllowSecretBearingPRJobs:   current.AllowSecretBearingPRJobs,
		AllowSelfHostedCI:          current.AllowSelfHostedCI,
		AllowPullRequestTarget:     current.AllowPullRequestTarget,
		AllowReusableWorkflows:     current.AllowReusableWorkflows,
		AllowPackagePublishing:     current.AllowPackagePublishing,
		AllowArtifactConsumers:     current.AllowArtifactConsumers,
		CommitPlan:                 current.CommitPlan,
		MessageRuleset:             current.MessageRuleset,
		WorkflowAuditDigest:        current.WorkflowAuditDigest,
		Review:                     current.Review,
		ProtectedPaths:             current.ProtectedPaths,
		ExternalReviewers:          reviewers,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.store.WriteInternal(p.ctx, func(tx *store.InternalTx) error {
		return tx.RecordTrustProfile(p.ctx, revised, p.now.Add(time.Minute))
	}); err != nil {
		t.Fatal(err)
	}
	return revised
}

// leaveExternalFinding stores one inline comment by reviewer on head as the
// reconciler's intake stores it. Comments are ordered by their number.
func (p *productionPublicationHarness) leaveExternalFinding(
	t *testing.T, comment int, reviewer domain.ExternalReviewer, head, text string,
) domain.Finding {
	t.Helper()
	finding, err := domain.NewExternalFinding(domain.ExternalFindingInput{
		RunID: p.runID, Forge: reviewer.Forge,
		ReviewerAccountID: reviewer.AccountID, ReviewerLogin: reviewer.Login,
		ThreadID: fmt.Sprintf("review_comment/%d", comment), HeadSHA: head,
		Location: &domain.FindingLocation{Path: "README.md", StartLine: 1, EndLine: 1},
		Severity: "P2", Message: text, RawText: text,
		CreatedAt: p.now.UTC().Add(time.Duration(comment) * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		return tx.PutExternalFinding(p.ctx, finding)
	}); err != nil {
		t.Fatal(err)
	}
	return finding
}

// startExternalReview runs the trigger for the run's current ready item as the
// active-resource reconciler does: the read-only probe, then the writer only
// when a cycle is due. It reports whether a cycle started.
func (p *productionPublicationHarness) startExternalReview(t *testing.T) bool {
	t.Helper()
	// The newest cycle's ready item may not exist yet; the trigger then finds
	// nothing to start, as the reconciler's would for an item it never lists.
	var (
		itemID domain.ItemID
		due    bool
	)
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		if itemID, err = tx.CurrentProductionReadyItemID(p.ctx, p.runID); err != nil {
			return err
		}
		due, err = engine.ExternalReviewReentryDue(p.ctx, tx, itemID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !due {
		return false
	}
	var started bool
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		var err error
		started, err = engine.StartExternalReviewReentry(p.ctx, tx, itemID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !started {
		t.Fatal("the probe reported a cycle due and the writer started none")
	}
	return true
}

// externalReviewEnd reads what an external review cycle leaves behind: the
// review items the run has open, and whether its cycle wrote a ready item or
// left a task pending.
type externalReviewEnd struct {
	reviewItems []domain.AttentionItem
	readyItem   bool
	pending     int
	successor   domain.PublicationSuccessor
}

func (p *productionPublicationHarness) externalReviewEnd(t *testing.T) externalReviewEnd {
	t.Helper()
	var end externalReviewEnd
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		items, err := tx.ListAttentionItems(p.ctx)
		if err != nil {
			return err
		}
		for _, snapshot := range items {
			item := snapshot.Value
			if item.Status == domain.StatusOpen &&
				(item.Type == domain.AttentionReviewDispute || item.Type == domain.AttentionReviewDiminishing) {
				end.reviewItems = append(end.reviewItems, item)
			}
		}
		successor, err := tx.CurrentPublicationSuccessor(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if successor == nil {
			return errors.New("the run has no successor authority")
		}
		end.successor = *successor
		_, err = tx.GetAttentionItem(p.ctx, successor.ReadyItemID())
		switch {
		case err == nil:
			end.readyItem = true
		case !errors.Is(err, store.ErrNotFound):
			return err
		}
		pending, err := tx.ListPendingOutbox(p.ctx, engine.KindProductionPublicationRequested)
		end.pending = len(pending)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return end
}

// TestExternalReviewCycleEndsOnAPerson drives issue #524's second part end to
// end: an admitted reviewer's finding on the published head withdraws
// readiness, the cycle verifies and reviews the unchanged head against the
// base its last review used, and the run ends on a review-dispute item that
// names every admitted finding and no other reviewer's.
func TestExternalReviewCycleEndsOnAPerson(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	first := p.publishReady(t)
	head := p.replay.HeadSHA
	pushes, body := p.transport.pushCount(), p.forge.pullRequests()[0].Body

	maintainer := externalMaintainer()
	stranger := domain.ExternalReviewer{
		Forge: domain.ExternalReviewForgeGitHub, AccountID: 4102, Login: "passer-by",
		Authority: domain.ExternalReviewDriveRound,
	}
	// Stored while nobody is listed, the findings start nothing.
	earliest := p.leaveExternalFinding(t, 1, maintainer, head, "this leaks the handle")
	p.leaveExternalFinding(t, 2, maintainer, head, "and this one races")
	p.leaveExternalFinding(t, 3, stranger, head, "an unlisted remark")
	if p.startExternalReview(t) {
		t.Fatal("a finding by an unlisted reviewer started a cycle")
	}
	profile := p.listExternalReviewers(t, maintainer)
	if !p.startExternalReview(t) {
		t.Fatal("an admitted finding on the published head started no cycle")
	}

	started := p.externalReviewEnd(t)
	successor := started.successor
	if successor.Version != domain.PublicationExternalReviewVersion ||
		successor.Origin != domain.PublicationSuccessorExternalReview ||
		successor.PredecessorItemID != first.ID || successor.ReviewRound != 2 ||
		successor.Reentry == nil || successor.Reentry.Reason != "" ||
		successor.Reentry.BaseSHA != p.baseSHA || successor.Reentry.HeadSHA != head ||
		successor.ExternalFindingID != earliest.ID ||
		successor.AdmittingProfileDigest != profile.ProfileDigest || started.pending != 1 {
		t.Fatalf("authority = %#v (re-entry %#v), %d tasks pending", successor, successor.Reentry, started.pending)
	}
	var withdrawn domain.AttentionItem
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		withdrawn, err = tx.GetAttentionItem(p.ctx, first.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if withdrawn.Status != domain.StatusSuperseded || withdrawn.ReadinessInvalidation != nil {
		t.Fatalf("withdrawn ready item = status %s, invalidation %#v", withdrawn.Status, withdrawn.ReadinessInvalidation)
	}

	reviews := p.recordReviews(t)
	p.scriptCleanReview(2, p.baseSHA, head)
	result, err := p.reconcileLanes()
	if err != nil || result.ReadyItemsCreated != 0 || result.BlockedItemsCreated != 0 {
		t.Fatalf("cycle result = %#v, %v", result, err)
	}
	// The pull request did not move, so the cycle reviews its head itself
	// against the base the last review used: no merge, nothing pushed.
	if len(reviews.requests) != 1 || reviews.requests[0].Round != 2 ||
		reviews.requests[0].BaseSHA != p.baseSHA || reviews.requests[0].HeadSHA != head ||
		reviews.requests[0].EvaluatedSHA != "" {
		t.Fatalf("review requests = %#v", reviews.requests)
	}
	if p.room.runs != 2 || p.transport.mergeCount() != 0 {
		t.Fatalf("verification runs = %d, merges = %d, want 2 and 0", p.room.runs, p.transport.mergeCount())
	}
	if p.transport.pushCount() != pushes || p.forge.pullRequests()[0].Body != body {
		t.Fatal("the cycle pushed or rewrote the pull request")
	}

	end := p.externalReviewEnd(t)
	if len(end.reviewItems) != 1 || end.readyItem || end.pending != 0 {
		t.Fatalf("cycle end = %d review items, ready item %t, %d tasks pending; want one item and nothing else",
			len(end.reviewItems), end.readyItem, end.pending)
	}
	item := end.reviewItems[0]
	if item.ID != productionReviewItemIDForTest(p.runID, 2) || item.Type != domain.AttentionReviewDispute ||
		item.PRHeadSHA != head {
		t.Fatalf("review item = %#v", item)
	}
	for _, want := range []string{
		"Freeside reviewed the published pull request again and found nothing blocking.",
		"An external reviewer's findings on " + head + " started this cycle",
		`maintainer on review_comment/1: "this leaks the handle"`,
		`maintainer on review_comment/2: "and this one races"`,
	} {
		if !strings.Contains(item.Reason, want) {
			t.Errorf("review item reason lacks %q:\n%s", want, item.Reason)
		}
	}
	if strings.Contains(item.Reason, "passer-by") || strings.Contains(item.Reason, "unlisted remark") {
		t.Errorf("review item names an unlisted reviewer's finding:\n%s", item.Reason)
	}

	// A later pass changes nothing, and nothing is left for a trigger to start.
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 {
		t.Fatalf("converged replay = %#v, %v", result, err)
	}
	if p.room.runs != 2 || len(reviews.requests) != 1 {
		t.Fatalf("converged replay ran verification %d times and requested %d reviews",
			p.room.runs, len(reviews.requests))
	}
	if again := p.externalReviewEnd(t); len(again.reviewItems) != 1 ||
		again.reviewItems[0].ItemVersion != item.ItemVersion {
		t.Fatalf("converged replay rewrote the review item: %#v", again.reviewItems)
	}
	if p.startExternalReview(t) {
		t.Fatal("a second cycle started with the first one's item open")
	}
}

// TestExternalReviewCycleReportsFreesideFindings: when Freeside's own review
// of the head also finds something, the cycle still ends on the one item, and
// the item says so beside the external finding.
func TestExternalReviewCycleReportsFreesideFindings(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.publishReady(t)
	head := p.replay.HeadSHA
	p.listExternalReviewers(t, externalMaintainer())
	p.leaveExternalFinding(t, 1, externalMaintainer(), head, "this leaks the handle")
	if !p.startExternalReview(t) {
		t.Fatal("an admitted finding started no cycle")
	}
	p.reviewer.Script(engine.ProductionReviewInvocationID(p.runID, 2), fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: p.baseSHA, HeadSHA: head,
			Provider: "openai", ModelConfiguration: "codex/test", CostOwner: "test",
			CompletedAt: p.now, CompletionEvidence: productionDigest([]byte("external cycle findings")),
			Findings: []domain.Finding{{
				ID: "external-cycle-finding-1", RunID: p.runID, Source: "codex_local", Severity: "P1",
				Location: &domain.FindingLocation{Path: "README.md", StartLine: 1, EndLine: 1},
				Message:  "unsafe on a second look", RawText: "unsafe on a second look",
				CreatedAt: p.now,
			}},
		},
	})
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 {
		t.Fatalf("cycle result = %#v, %v", result, err)
	}
	end := p.externalReviewEnd(t)
	if len(end.reviewItems) != 1 || end.readyItem || end.pending != 0 {
		t.Fatalf("cycle end = %#v", end)
	}
	reason := end.reviewItems[0].Reason
	if end.reviewItems[0].Type != domain.AttentionReviewDispute ||
		!strings.Contains(reason, "Freeside reviewed the published pull request again and found blocking findings of its own.") ||
		!strings.Contains(reason, `maintainer on review_comment/1: "this leaks the handle"`) {
		t.Fatalf("review item = %s: %s", end.reviewItems[0].Type, reason)
	}
}

// TestExternalReviewCycleConvergesAcrossRestarts interrupts the cycle at each
// durable boundary it crosses, restarts, and requires the same end: one
// review item, each review requested once, and no ready item.
func TestExternalReviewCycleConvergesAcrossRestarts(t *testing.T) {
	t.Parallel()
	for _, transition := range []engine.DurableTransition{
		engine.DurableTransitionVerificationEvidence,
		engine.DurableTransitionReviewRequest,
		engine.DurableTransitionReviewResult,
	} {
		for _, side := range engine.AllDurableTransitionSides {
			t.Run(string(transition)+"/"+string(side), func(t *testing.T) {
				t.Parallel()
				p := newProductionPublicationHarness(t, "")
				p.publishReady(t)
				head := p.replay.HeadSHA
				p.listExternalReviewers(t, externalMaintainer())
				p.leaveExternalFinding(t, 1, externalMaintainer(), head, "this leaks the handle")
				if !p.startExternalReview(t) {
					t.Fatal("an admitted finding started no cycle")
				}
				// A restart between the trigger and the cycle's first pass: the
				// task row alone carries it.
				p.restartDurableState(t)
				reviewCalls := &faultReviewSource{ReviewSource: p.reviewer}
				p.reviewSource = reviewCalls
				p.scriptCleanReview(2, p.baseSHA, head)
				injected := false
				p.workflow = p.newEngine(t, productionCrashSeams{
					transitionHook: func(
						got engine.DurableTransition, observed engine.DurableTransitionSide,
					) error {
						if !injected && got == transition && observed == side {
							injected = true
							return errors.New("injected process loss")
						}
						return nil
					},
				}, true)
				if _, err := p.reconcileLanes(); err == nil || !injected {
					t.Fatalf("%s/%s did not interrupt the cycle: %v", transition, side, err)
				}
				p.restartDurableState(t)
				round := 2
				if transition == engine.DurableTransitionReviewRequest &&
					side == engine.DurableTransitionAfter {
					// The interrupted request is abandoned for the next round,
					// as on every other cycle.
					round = 3
				}
				p.scriptCleanReview(2, p.baseSHA, head)
				p.scriptCleanReview(3, p.baseSHA, head)
				p.workflow = p.newEngine(t, productionCrashSeams{}, true)
				var end externalReviewEnd
				for pass := 1; pass <= 3 && len(end.reviewItems) == 0; pass++ {
					p.now = p.now.Add(time.Minute)
					if _, err := p.reconcileLanes(); err != nil {
						t.Fatalf("pass %d after the restart: %v", pass, err)
					}
					end = p.externalReviewEnd(t)
				}
				if len(end.reviewItems) != 1 || end.readyItem || end.pending != 0 {
					t.Fatalf("cycle end = %d review items, ready item %t, %d tasks pending",
						len(end.reviewItems), end.readyItem, end.pending)
				}
				item := end.reviewItems[0]
				if item.ID != productionReviewItemIDForTest(p.runID, round) ||
					item.Type != domain.AttentionReviewDispute ||
					!strings.Contains(item.Reason, `maintainer on review_comment/1: "this leaks the handle"`) {
					t.Fatalf("review item = %#v", item)
				}

				p.restartDurableState(t)
				p.workflow = p.newEngine(t, productionCrashSeams{}, true)
				for range 2 {
					if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 {
						t.Fatalf("converged replay = %#v, %v", result, err)
					}
				}
				again := p.externalReviewEnd(t)
				if len(again.reviewItems) != 1 || again.reviewItems[0].ID != item.ID ||
					again.reviewItems[0].ItemVersion != item.ItemVersion || again.readyItem || again.pending != 0 {
					t.Fatalf("restart changed the cycle's end: %#v", again)
				}
				for id, count := range reviewCalls.requestIDs {
					if count != 1 {
						t.Fatalf("restart requested review %s %d times, want 1", id, count)
					}
				}
				wantRuns := 2
				if transition == engine.DurableTransitionVerificationEvidence &&
					side == engine.DurableTransitionBefore {
					wantRuns = 3
				}
				if p.room.runs != wantRuns {
					t.Fatalf("verification runs = %d, want %d", p.room.runs, wantRuns)
				}
			})
		}
	}
}

// TestExternalReviewCycleStopsAtTheRoundLimit: an external cycle spends a
// review round like any other. A run already at its resolved hard limit ends
// on the existing exhaustion item, which names the reviewer's finding, and no
// review is requested.
func TestExternalReviewCycleStopsAtTheRoundLimit(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarnessWithPolicyKeys(t, "", []domain.PolicyKey{{
		Key: "review.hard_round_limit", Value: "1",
		Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride,
			Digest: submissionDigest("run-production-publication", "review-hard-round-limit"),
		},
	}})
	p.publishReady(t)
	head := p.replay.HeadSHA
	p.listExternalReviewers(t, externalMaintainer())
	p.leaveExternalFinding(t, 1, externalMaintainer(), head, "this leaks the handle")
	if !p.startExternalReview(t) {
		t.Fatal("an admitted finding started no cycle")
	}
	reviews := p.recordReviews(t)
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 {
		t.Fatalf("cycle result = %#v, %v", result, err)
	}
	end := p.externalReviewEnd(t)
	if len(end.reviewItems) != 1 || end.readyItem || end.pending != 0 {
		t.Fatalf("cycle end = %#v", end)
	}
	item := end.reviewItems[0]
	if item.Type != domain.AttentionReviewDiminishing ||
		!strings.HasPrefix(item.Reason, "Review exhausted the resolved hard limit of 1 rounds. ") ||
		!strings.Contains(item.Reason, `maintainer on review_comment/1: "this leaks the handle"`) {
		t.Fatalf("limit item = %s: %s", item.Type, item.Reason)
	}
	if len(reviews.requests) != 0 {
		t.Fatalf("a cycle past the round limit requested %d reviews", len(reviews.requests))
	}
}

// TestExternalReviewCycleAtTheRoundLimitKeepsItsOwnItem: the first cycle may
// have resolved an item under the round-limit identity on its way to ready.
// The external cycle's exhaustion item has its own identity, so it is raised
// open beside that item and never refused or dropped because of it.
func TestExternalReviewCycleAtTheRoundLimitKeepsItsOwnItem(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarnessWithPolicyKeys(t, "", []domain.PolicyKey{{
		Key: "review.hard_round_limit", Value: "1",
		Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride,
			Digest: submissionDigest("run-production-publication", "review-hard-round-limit"),
		},
	}})
	ready := p.publishReady(t)
	head := p.replay.HeadSHA
	occupied := productionReviewItemIDForTest(p.runID, 1)
	seeded, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: occupied, ProjectID: ready.ProjectID, Subject: ready.Subject,
		Type: domain.AttentionReviewDispute, Priority: domain.PriorityNormal,
		Reason:            "A person answered this in the last allowed round.",
		RequestedDecision: []domain.Action{domain.ActionDiscuss, domain.ActionStop},
		PRHeadSHA:         head, ItemVersion: 1,
		InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
		CreatedAt: &p.now, DisplayNames: ready.DisplayNames,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.attention.PutItem(p.ctx, seeded); err != nil {
		t.Fatalf("seed the round-limit item: %v", err)
	}
	seeded.ItemVersion, seeded.Status = 2, domain.StatusResolved
	if err := p.attention.PutItem(p.ctx, seeded); err != nil {
		t.Fatalf("resolve the round-limit item: %v", err)
	}

	p.listExternalReviewers(t, externalMaintainer())
	p.leaveExternalFinding(t, 1, externalMaintainer(), head, "this leaks the handle")
	if !p.startExternalReview(t) {
		t.Fatal("an admitted finding started no cycle")
	}
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 {
		t.Fatalf("cycle result = %#v, %v", result, err)
	}
	end := p.externalReviewEnd(t)
	if len(end.reviewItems) != 1 || end.readyItem || end.pending != 0 {
		t.Fatalf("cycle end = %#v", end)
	}
	item := end.reviewItems[0]
	want := domain.ItemID(fmt.Sprintf("production-external-review-exhaustion-%s-2", p.runID))
	if item.ID != want || item.ID == occupied || item.Type != domain.AttentionReviewDiminishing ||
		!strings.Contains(item.Reason, `maintainer on review_comment/1: "this leaks the handle"`) {
		t.Fatalf("limit item = %s (%s): %s", item.ID, item.Type, item.Reason)
	}
}

// TestExternalReviewCycleIsNotReadyWhileItsReviewRuns is the quarantine: an
// admitted finding withdraws readiness at once, and nothing gives it back
// while the cycle's review has not returned. The run has no open ready item,
// no review item, and one review request outstanding, on every pass.
func TestExternalReviewCycleIsNotReadyWhileItsReviewRuns(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	first := p.publishReady(t)
	head := p.replay.HeadSHA
	p.listExternalReviewers(t, externalMaintainer())
	p.leaveExternalFinding(t, 1, externalMaintainer(), head, "this leaks the handle")
	if !p.startExternalReview(t) {
		t.Fatal("an admitted finding started no cycle")
	}
	reviews := p.recordReviews(t)
	p.reviewer.Script(engine.ProductionReviewInvocationID(p.runID, 2), fake.ReviewScript{
		Outcome: fake.OutcomeComplete, PendingInspects: 1000,
		Result: exec.ReviewResult{
			BaseSHA: p.baseSHA, HeadSHA: head,
			Provider: "openai", ModelConfiguration: "codex/test", CostOwner: "test",
			CompletedAt: p.now.UTC(), CompletionEvidence: productionDigest([]byte("slow review")),
		},
	})
	for pass := 1; pass <= 3; pass++ {
		p.now = p.now.Add(time.Minute)
		result, err := p.reconcileLanes()
		if err != nil || result.ReadyItemsCreated != 0 || result.ReadyCleanItemsCreated != 0 {
			t.Fatalf("pass %d = %#v, %v", pass, result, err)
		}
		end := p.externalReviewEnd(t)
		if len(end.reviewItems) != 0 || end.readyItem || end.pending != 1 {
			t.Fatalf("pass %d: %d review items, ready item %t, %d tasks pending; want only the pending task",
				pass, len(end.reviewItems), end.readyItem, end.pending)
		}
		var withdrawn domain.AttentionItem
		if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
			var err error
			withdrawn, err = tx.GetAttentionItem(p.ctx, first.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if withdrawn.Status != domain.StatusSuperseded {
			t.Fatalf("pass %d: the withdrawn ready item is %s", pass, withdrawn.Status)
		}
	}
	if len(reviews.requests) != 1 || reviews.requests[0].Round != 2 {
		t.Fatalf("review requests = %#v, want one for round 2", reviews.requests)
	}
	var latest domain.ReviewRecord
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		latest, err = tx.LatestReviewRecord(p.ctx, p.runID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if latest.Round != 1 {
		t.Fatalf("latest review record is round %d with the cycle's review still running", latest.Round)
	}
}

// TestExternalReviewStartsNothingOnAForeignHead: a head someone else pushed
// was re-earned through a head-change cycle, and the store refuses an external
// review authority on it. The trigger decides that from reads, so the ready
// item stays open instead of failing the pass.
func TestExternalReviewStartsNothingOnAForeignHead(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.publishReady(t)
	p.listExternalReviewers(t, externalMaintainer())
	pushed := p.pushToPullRequest(t, "FOLLOWUP.md", "pushed after readiness\n")
	superseded, started := p.invalidateReady(t, domain.ReadinessInvalidation{
		Reason: domain.ReadinessInvalidationHeadChanged, Bound: p.replay.HeadSHA, Observed: pushed,
	})
	if !started {
		t.Fatal("a head change started no re-entry")
	}
	p.scriptCleanReview(2, p.baseSHA, pushed)
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 1 {
		t.Fatalf("re-entry result = %#v, %v", result, err)
	}
	ready := p.assertReentered(t, superseded, 2, p.baseSHA, pushed)

	p.leaveExternalFinding(t, 1, externalMaintainer(), pushed, "this leaks the handle")
	if p.startExternalReview(t) {
		t.Fatal("an external review cycle started on a head Freeside did not push")
	}
	if got := p.currentReadyItem(t); got.ID != ready.ID || got.Status != domain.StatusOpen ||
		got.ItemVersion != ready.ItemVersion {
		t.Fatalf("ready item on the foreign head = %#v", got)
	}
}

// TestExternalReviewCycleNamesFindingsWhenItsReviewFails: a cycle whose review
// stops on a quota failure ends on the existing review item. The cycle
// withdrew readiness for a reviewer's finding, so that item names it too.
func TestExternalReviewCycleNamesFindingsWhenItsReviewFails(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.publishReady(t)
	head := p.replay.HeadSHA
	p.listExternalReviewers(t, externalMaintainer())
	p.leaveExternalFinding(t, 1, externalMaintainer(), head, "this leaks the handle")
	if !p.startExternalReview(t) {
		t.Fatal("an admitted finding started no cycle")
	}
	p.scriptCleanReview(2, p.baseSHA, head)
	p.reviewSource = &faultReviewSource{
		ReviewSource: p.reviewer, failPollAt: 1,
		failPollWith: errors.Join(exec.ErrNoResult, &exec.ReviewSourceFailure{
			Class: domain.ReviewFailureQuota, Err: errors.New("review quota exhausted"),
		}),
	}
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	for range 4 {
		result, err := p.reconcileLanes()
		if err != nil {
			t.Fatal(err)
		}
		if result.PublicationTasksCompleted == 1 {
			break
		}
	}
	end := p.externalReviewEnd(t)
	if len(end.reviewItems) != 1 || end.readyItem || end.pending != 0 {
		t.Fatalf("cycle end = %#v", end)
	}
	item := end.reviewItems[0]
	if item.Type != domain.AttentionReviewDispute ||
		!strings.HasPrefix(item.Reason, "Codex review stopped because of a quota failure. ") ||
		!strings.Contains(item.Reason, `maintainer on review_comment/1: "this leaks the handle"`) {
		t.Fatalf("quota item = %s: %s", item.Type, item.Reason)
	}
}

// TestExternalReviewCycleAfterABaseAdvanceEndsOnAPerson: after a base advance
// the last review covered the head's merge into the new base, and the head
// itself does not descend from that base. The external review authority
// admits no prospective merge, so the cycle reviews nothing. It still ends on
// an item that names the reviewer's finding and says why no review ran.
func TestExternalReviewCycleAfterABaseAdvanceEndsOnAPerson(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.publishReady(t)
	head := p.replay.HeadSHA
	advanced := p.advanceBase(t, "UPSTREAM.md", "upstream change\n")
	superseded, started := p.invalidateReady(t, domain.ReadinessInvalidation{
		Reason: domain.ReadinessInvalidationBaseAdvanced, Bound: p.baseSHA, Observed: advanced,
	})
	if !started {
		t.Fatal("a base advance started no re-entry")
	}
	p.scriptCleanReview(2, advanced, head)
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 1 {
		t.Fatalf("re-entry result = %#v, %v", result, err)
	}
	p.assertReentered(t, superseded, 2, advanced, head)

	p.listExternalReviewers(t, externalMaintainer())
	p.leaveExternalFinding(t, 1, externalMaintainer(), head, "this leaks the handle")
	if !p.startExternalReview(t) {
		t.Fatal("an admitted finding started no cycle after a base advance")
	}
	reviews := p.recordReviews(t)
	result, err := p.reconcileLanes()
	if err != nil || result.BlockedItemsCreated != 1 || result.ReadyItemsCreated != 0 {
		t.Fatalf("cycle result = %#v, %v", result, err)
	}
	stop := p.reentryStopItem(t)
	for _, want := range []string{
		"Freeside did not review pull request head " + head + " again",
		"does not descend from the reviewed base " + advanced,
		"An external reviewer's findings on " + head + " started this cycle",
		`maintainer on review_comment/1: "this leaks the handle"`,
	} {
		if !strings.Contains(stop.Reason, want) {
			t.Errorf("stop item reason lacks %q:\n%s", want, stop.Reason)
		}
	}
	if len(reviews.requests) != 0 {
		t.Fatalf("a cycle with nothing it may review requested %d reviews", len(reviews.requests))
	}
	if end := p.externalReviewEnd(t); len(end.reviewItems) != 0 || end.readyItem || end.pending != 0 {
		t.Fatalf("cycle end = %#v", end)
	}
	// The stopped cycle is not retried: the finding is the one its authority
	// names, and the run has no open ready item for another to start from.
	if p.startExternalReview(t) {
		t.Fatal("a stopped external review cycle started another")
	}
}
