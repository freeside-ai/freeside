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
// quarantine the run. The re-entry case is the one the Wave 8 exit run hit
// (#2101): the remediated head published through a pre-publication re-entry.
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
	p, _ := prepareDispatchedRemediationPublicationTask(t)
	readyID := domain.ProductionReadyItemID(p.runID)
	round := 3
	var reviews *reviewedTree
	if reentry {
		var advanced string
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
			t.Logf("pinned base %s returned head %s", p.baseSHA, returned.HeadSHA)
		}
		t.Fatalf("feedback successor did not publish: %#v", completed)
	}
}
