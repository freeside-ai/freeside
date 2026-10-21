package integration_test

import (
	"errors"
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
// quarantine the run. The re-entry case is the one the Wave 8 exit run hit
// (#2101): the remediated head published through a pre-publication re-entry.
// The feedback then works at the admitted base, which the published pull
// request has left behind, so its cycle re-enters at the newer base and
// updates that pull request (#2111).
func TestReturnToAgentAfterARemediationPublishes(t *testing.T) {
	t.Parallel()
	for _, reentry := range []bool{false, true} {
		name := "published"
		if reentry {
			name = "reentered-before-publication"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testReturnToAgentAfterRemediation(t, reentry)
		})
	}
}

func testReturnToAgentAfterRemediation(t *testing.T, reentry bool) {
	p, advanced, round, reviews := returnToAgentAfterRemediation(t, reentry)
	if reentry {
		p.scriptCleanReview(round+1, advanced, p.replay.HeadSHA)
	}

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
	if reentry {
		assertSuccessorReentryPublished(t, p, advanced, round+1)
	}
}

// TestSuccessorReentryRefusedAgainReentersAgain: the base advances once more
// while a successor re-entry (#2111) is in its cycle, so the publisher refuses
// it too. It seals the next successor re-entry at the newer base after itself,
// which reviews the kept head there and updates the pull request.
func TestSuccessorReentryRefusedAgainReentersAgain(t *testing.T) {
	t.Parallel()
	p, advanced, round, _ := returnToAgentAfterRemediation(t, true)
	p.scriptCleanReview(round+1, advanced, p.replay.HeadSHA)
	// Advance the base again in the pass after the first re-entry is sealed,
	// before its cycle audits the target.
	var first *domain.PublicationSuccessor
	var further string
	for pass := 1; first == nil; pass++ {
		if pass > 8 {
			t.Fatal("no successor re-entry was sealed")
		}
		p.now = p.now.Add(time.Minute)
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatal(err)
		}
		if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
			current, err := tx.CurrentPublicationSuccessor(p.ctx, p.runID)
			if err == nil && current != nil && current.Version == domain.PublicationSuccessorReentryVersion {
				first = current
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if prs := p.forge.pullRequests(); len(prs) != 1 || prs[0].HeadSHA == p.replay.HeadSHA {
		t.Fatalf("the first re-entry published before the further advance: %#v", prs)
	}
	further, _ = p.advanceBaseBeforePublication(t, "FURTHER.md", "further upstream change\n")
	p.scriptCleanReview(round+2, further, p.replay.HeadSHA)
	p.reconcileUntilReady(t, 12)
	if first == nil || first.Reentry.BaseSHA != advanced {
		t.Fatalf("first successor re-entry = %#v, want one at %s", first, advanced)
	}
	assertSuccessorReentryPublished(t, p, further, round+2)
	if current := p.successorReentry(t, further, round+2); current.PredecessorItemID != first.ReadyItemID() {
		t.Fatalf("re-entry predecessor = %s, want the refused re-entry's %s",
			current.PredecessorItemID, first.ReadyItemID())
	}
}

// returnToAgentAfterRemediation publishes a remediated head, through a
// pre-publication re-entry at advanced when reentry is set, returns its ready
// item to the agent, and records the feedback's export at the admitted base.
// The feedback cycle's clean review is scripted for round, at that base; the
// cycle itself has not run.
func returnToAgentAfterRemediation(
	t *testing.T, reentry bool,
) (p *productionPublicationHarness, advanced string, round int, reviews *reviewedTree) {
	t.Helper()
	p, _ = prepareDispatchedRemediationPublicationTask(t)
	readyID := domain.ProductionReadyItemID(p.runID)
	round = 3
	if reentry {
		advanced, reviews = p.advanceBaseBeforePublication(t, "UPSTREAM.md", "upstream change\n")
		p.scriptCleanReview(3, advanced, p.replay.HeadSHA)
		p.reconcileUntilReady(t, 8)
		readyID = p.unpublishedReentry(t, advanced).ReadyItemID()
		round = 4
	} else {
		p.reconcileUntilReady(t, 8)
	}
	ready, err := p.attention.GetAttentionItem(p.ctx, readyID)
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
	if reentry {
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

// assertSuccessorReentryPublished checks that the run's current cycle is a
// successor re-entry at baseSHA, sealed for review round, and that it updated
// the run's one pull request to the head it kept.
func assertSuccessorReentryPublished(t *testing.T, p *productionPublicationHarness, baseSHA string, round int) {
	t.Helper()
	var successor *domain.PublicationSuccessor
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		if successor, err = tx.CurrentPublicationSuccessor(p.ctx, p.runID); err != nil {
			return err
		}
		published, err := tx.PublishedPublicationInvocationID(p.ctx, p.runID)
		if err == nil && successor != nil && published != successor.PublicationID() {
			t.Errorf("published invocation = %s, want the successor re-entry's %s",
				published, successor.PublicationID())
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if successor == nil || successor.Version != domain.PublicationSuccessorReentryVersion ||
		successor.Origin != domain.PublicationSuccessorBaseAdvance || successor.Reentry == nil ||
		successor.Reentry.BaseSHA != baseSHA || successor.Reentry.HeadSHA != p.replay.HeadSHA ||
		successor.ReviewRound != round {
		t.Fatalf("current successor = %#v, want a successor re-entry at %s for round %d", successor, baseSHA, round)
	}
	if ready := p.currentReadyItem(t); ready.ID != successor.ReadyItemID() || ready.PRHeadSHA != p.replay.HeadSHA {
		t.Fatalf("ready item = %s at %s, want %s at %s", ready.ID, ready.PRHeadSHA,
			successor.ReadyItemID(), p.replay.HeadSHA)
	}
	prs := p.forge.pullRequests()
	if len(prs) != 1 || prs[0].HeadSHA != p.replay.HeadSHA {
		t.Fatalf("pull requests = %#v, want the one pull request at %s", prs, p.replay.HeadSHA)
	}
}

// TestRerunOfAReevaluatedBaseAdvanceBlockNeedsANewProfile: a rerun re-enters
// at a newer base only from its cycle's own base-advance block (#2111). A
// block a rerun's reevaluation recorded at an advanced base names that rerun's
// verification, which a re-entry does not reuse, so Signet still refuses its
// rerun under the trust profile that already evaluated the head, and the item
// stays open for a rerun under a new profile.
func TestRerunOfAReevaluatedBaseAdvanceBlockNeedsANewProfile(t *testing.T) {
	t.Parallel()
	p, _, _, _ := returnToAgentAfterRemediation(t, true)
	// The feedback cycle first blocks on a rerunnable refusal of its own.
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
	p.reconcileUntilBlocked(t, 8)
	blocked, err := p.attention.GetAttentionItem(p.ctx,
		signet.ReevaluatedBlockedItemID(p.runID, "rerun-at-admitted-base"))
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Item.Reason != domain.PublicationBlockBaseAdvanced || blocked.Item.Status != domain.StatusOpen ||
		!blocked.Item.Offers(domain.ActionRerunTrustEvaluation) {
		t.Fatalf("reevaluated block = %s %s offering %v, want an open base_advanced block with Rerun",
			blocked.Item.Status, blocked.Item.Reason, blocked.Item.RequestedDecision)
	}

	const deviceID = domain.DeviceID("device-rerun-after-base-advance")
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		return tx.PutDevice(p.ctx, domain.Device{
			ID: deviceID, DisplayName: "Publication reevaluation device",
			Status: domain.DeviceActive, PairedAt: p.now,
		})
	}); err != nil {
		t.Fatal(err)
	}
	_, err = p.attention.Submit(p.ctx, signet.ClientCommand{
		CommandID: "rerun-after-base-advance", DeviceID: deviceID,
		ExpectedEntityVersion: blocked.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: blocked.Item.ID, ItemVersion: blocked.Item.ItemVersion,
			PRHeadSHA: blocked.Item.PRHeadSHA, ArtifactDigests: blocked.Item.ArtifactDigests,
			Action: domain.ActionRerunTrustEvaluation,
		},
	})
	if !errors.Is(err, domain.ErrDuplicate) {
		t.Fatalf("rerun under the evaluating profile = %v, want ErrDuplicate", err)
	}
	after, err := p.attention.GetAttentionItem(p.ctx, blocked.Item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Item.Status != domain.StatusOpen || after.Item.ItemVersion != blocked.Item.ItemVersion {
		t.Fatalf("refused rerun moved the block to %s version %d", after.Item.Status, after.Item.ItemVersion)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		current, err := tx.CurrentPublicationSuccessor(p.ctx, p.runID)
		if err == nil && current.PublicationID() != feedback.PublicationID() {
			t.Errorf("refused rerun changed the cycle to %s", current.PublicationID())
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// successorReentry reads the run's current successor and requires it to be a
// successor re-entry at baseSHA, sealed for review round.
func (p *productionPublicationHarness) successorReentry(
	t *testing.T, baseSHA string, round int,
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
	if successor == nil || successor.Version != domain.PublicationSuccessorReentryVersion ||
		successor.Origin != domain.PublicationSuccessorBaseAdvance || successor.Reentry == nil ||
		successor.Reentry.BaseSHA != baseSHA || successor.Reentry.HeadSHA != p.replay.HeadSHA ||
		successor.ReviewRound != round {
		t.Fatalf("current successor = %#v, want a successor re-entry at %s for round %d", successor, baseSHA, round)
	}
	return *successor
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
