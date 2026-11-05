package integration_test

import (
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

// TestReturnToAgentAfterARemediationPublishes: feedback returned on a head a
// remediation produced is newer than that remediation, so the run's active
// remediation marker must authenticate it as a later producer rather than
// quarantine the run. The moved-tip case is the one the Wave 8 exit run hit
// (#2101): the target advanced before the remediated head published. The
// feedback then works at the admitted base, which the published pull request
// has left behind, so its cycle's handoff verifies the merge into the tip and
// updates that pull request without another review round (issue #2644).
func TestReturnToAgentAfterARemediationPublishes(t *testing.T) {
	t.Parallel()
	for _, advance := range []bool{false, true} {
		name := "published"
		if advance {
			name = "handed-off-at-a-moved-tip"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testReturnToAgentAfterRemediation(t, advance)
		})
	}
}

func testReturnToAgentAfterRemediation(t *testing.T, advance bool) {
	p, advanced, round, reviews := returnToAgentAfterRemediation(t, advance)
	var completed engine.ReconcileResult
	for range 8 {
		result, err := p.reconcileLanes()
		if err != nil {
			t.Fatal(err)
		}
		completed.PublicationTasksCompleted += result.PublicationTasksCompleted
		if completed.PublicationTasksCompleted > 0 {
			break
		}
	}
	items, err := p.attention.ListAttentionItems(p.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if strings.HasPrefix(string(item.Item.ID), "remediation-marker-quarantined") {
			t.Fatalf("feedback after a remediation quarantined the run: %s %s",
				item.Item.ID, item.Item.Status)
		}
	}
	if completed.PublicationTasksCompleted != 1 {
		for _, item := range items {
			t.Logf("item %s: %s: %s", item.Item.ID, item.Item.Status, item.Item.Reason)
		}
		if reviews != nil {
			for _, req := range reviews.requests {
				t.Logf("review round %d base %s head %s evaluated %s", req.Round, req.BaseSHA, req.HeadSHA, req.EvaluatedSHA)
			}
			t.Logf("pinned base %s returned head %s", p.baseSHA, p.replay.HeadSHA)
		}
		t.Fatalf("feedback successor did not publish: %#v", completed)
	}
	if advance {
		assertFeedbackHandoffPublished(t, p, advanced, round)
	}
}

// returnToAgentAfterRemediation publishes a remediated head, through a
// handoff at the moved tip advanced when advance is set, returns its ready
// item to the agent, and records the feedback's export at the admitted base.
// The feedback cycle's clean review is scripted for round, at that base; the
// cycle itself has not run.
func returnToAgentAfterRemediation(
	t *testing.T, advance bool,
) (p *productionPublicationHarness, advanced string, round int, reviews *reviewedTree) {
	t.Helper()
	p, _ = prepareDispatchedRemediationPublicationTask(t)
	round = 3
	if advance {
		advanced, reviews = p.advanceBaseBeforePublication(t, "UPSTREAM.md", "upstream change\n")
	}
	p.reconcileUntilReady(t, 8)
	ready, err := p.attention.GetAttentionItem(p.ctx, p.currentReadyItem(t).ID)
	if err != nil {
		t.Fatal(err)
	}
	if ready.Item.Status != domain.StatusOpen || !ready.Item.Offers(domain.ActionReturnToAgent) {
		t.Fatalf("remediated ready item = %s offering %v, want open with return to agent",
			ready.Item.Status, ready.Item.RequestedDecision)
	}
	const deviceID domain.DeviceID = "feedback-device"
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		return tx.PutDevice(p.ctx, domain.Device{
			ID: deviceID, DisplayName: "Feedback device", Status: domain.DeviceActive, PairedAt: p.now,
		})
	}); err != nil {
		t.Fatal(err)
	}
	const commandID = "return-remediated"
	if _, err := p.attention.Submit(p.ctx, signet.ClientCommand{
		CommandID: commandID, DeviceID: deviceID, ExpectedEntityVersion: ready.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: ready.Item.ID, ItemVersion: ready.Item.ItemVersion,
			PRHeadSHA: ready.Item.PRHeadSHA, ArtifactDigests: ready.Item.ArtifactDigests,
			Action: domain.ActionReturnToAgent, Message: "Match the README to the help text.",
		},
	}); err != nil {
		t.Fatal(err)
	}
	feedback := domain.InvocationID("inv-operator-feedback-" + commandID)
	p.driver.Script(feedback, fake.StageScript{
		PendingInspects: 1, Outcome: fake.OutcomeComplete,
		Result: exec.StageResult{Summary: "Feedback export completed."},
	})
	started := 0
	for range 3 {
		result, err := p.reconcileLanes()
		if err != nil {
			t.Fatal(err)
		}
		if started += result.InvocationsStarted; started > 0 {
			break
		}
	}
	if started != 1 {
		t.Fatalf("feedback dispatch started %d invocations, want 1", started)
	}
	var (
		run       domain.Run
		admission domain.ExecutionAdmission
	)
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		if run, err = tx.GetRun(p.ctx, p.runID); err != nil {
			return err
		}
		admission, err = tx.GetExecutionAdmissionRecord(p.ctx, feedback)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if advance {
		// The feedback works at the base the run was admitted at, as the
		// exit run's did; the builder exports from the base checkout.
		runGit(t, p.baseDir, "checkout", "-q", "--detach", p.baseSHA)
		defer runGit(t, p.baseDir, "checkout", "-q", "-")
	}
	returned := buildProductionReplayWithContentAt(
		t, p.publicationHarness, p.runID, run.SpecDigest,
		submissionSpecification(string(p.runID)), nil, feedback,
		fakePublicationTime.Add(2*time.Minute),
		"production change\nremediated review finding\nreturned feedback\n",
	)
	exported, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: feedback, AdmissionID: admission.ID,
		ObservedBaseSHA: p.baseSHA, HeadSHA: returned.HeadSHA,
		ManifestDigest: returned.ManifestDigest, RecordedAt: returned.ImportOptions.CommitDate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.RecordExecutionExport(p.ctx, p.store, exported, returned); err != nil {
		t.Fatal(err)
	}
	p.replay = returned
	p.now = returned.ImportOptions.CommitDate.Add(time.Minute)
	p.scriptCleanReview(round, p.baseSHA, returned.HeadSHA)
	return p, advanced, round, reviews
}

// assertFeedbackHandoffPublished checks that the run's current cycle is the
// feedback successor, still at review round, whose handoff verified the
// merge of the returned head into tip and updated the run's one pull request
// to that head.
func assertFeedbackHandoffPublished(t *testing.T, p *productionPublicationHarness, tip string, round int) {
	t.Helper()
	var successor *domain.PublicationSuccessor
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		if successor, err = tx.CurrentPublicationSuccessor(p.ctx, p.runID); err != nil {
			return err
		}
		published, err := tx.PublishedPublicationInvocationID(p.ctx, p.runID)
		if err == nil && successor != nil && published != successor.PublicationID() {
			t.Errorf("published invocation = %s, want the feedback cycle's %s",
				published, successor.PublicationID())
		}
		review, err := tx.LatestReviewRecord(p.ctx, p.runID)
		if err == nil && (review.Round != round || review.BaseSHA != p.baseSHA) {
			t.Errorf("latest review = round %d at %s, want round %d at the admitted base",
				review.Round, review.BaseSHA, round)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if successor == nil || successor.EffectiveOrigin() != domain.PublicationSuccessorFeedback ||
		successor.ReviewRound != round {
		t.Fatalf("current successor = %#v, want the feedback cycle for round %d", successor, round)
	}
	p.assertHandoffVerified(t, tip)
	if ready := p.currentReadyItem(t); ready.ID != successor.ReadyItemID() || ready.PRHeadSHA != p.replay.HeadSHA {
		t.Fatalf("ready item = %s at %s, want %s at %s", ready.ID, ready.PRHeadSHA,
			successor.ReadyItemID(), p.replay.HeadSHA)
	}
	prs := p.forge.PullRequests()
	if len(prs) != 1 || prs[0].HeadSHA != p.replay.HeadSHA {
		t.Fatalf("pull requests = %#v, want the one pull request at %s", prs, p.replay.HeadSHA)
	}
}

// TestRerunOfAFeedbackBlockPublishesThroughTheHandoff: a feedback cycle that
// blocked on its own verification at the admitted base is rerun under a new
// trust profile. The rerun's publication meets the moved tip at its handoff,
// verifies the merge into it, and updates the pull request; it never
// re-enters the run (issue #2644).
func TestRerunOfAFeedbackBlockPublishesThroughTheHandoff(t *testing.T) {
	t.Parallel()
	p, advanced, _, _ := returnToAgentAfterRemediation(t, true)
	p.room.fail = true
	p.reconcileUntilBlocked(t, 8)
	var feedback *domain.PublicationSuccessor
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		feedback, err = tx.CurrentPublicationSuccessor(p.ctx, p.runID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	first, err := p.attention.GetAttentionItem(p.ctx, feedback.BlockedItemID())
	if err != nil {
		t.Fatal(err)
	}
	p.room.fail = false
	repairPublicationTrustProfile(t, p)
	submitPublicationRerun(t, p, first, "rerun-at-admitted-base")
	for pass := 0; ; pass++ {
		if pass == 8 {
			t.Fatal("the rerun did not update the pull request")
		}
		p.now = p.now.Add(time.Minute)
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatal(err)
		}
		if prs := p.forge.PullRequests(); len(prs) == 1 && prs[0].HeadSHA == p.replay.HeadSHA {
			break
		}
	}
	p.assertHandoffVerified(t, advanced)
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		current, err := tx.CurrentPublicationSuccessor(p.ctx, p.runID)
		if err == nil && current.PublicationID() != feedback.PublicationID() {
			t.Errorf("rerun changed the cycle to %s", current.PublicationID())
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// reconcileUntilBlocked runs lane passes until one creates a blocked item,
// and fails after maxPasses.
func (p *productionPublicationHarness) reconcileUntilBlocked(t *testing.T, maxPasses int) {
	t.Helper()
	for pass := 1; pass <= maxPasses; pass++ {
		p.now = p.now.Add(time.Minute)
		result, err := p.reconcileLanes()
		if err != nil {
			t.Fatalf("pass %d/%d: %v", pass, maxPasses, err)
		}
		if result.BlockedItemsCreated > 0 {
			return
		}
	}
	t.Fatalf("no blocked item after %d passes", maxPasses)
}
