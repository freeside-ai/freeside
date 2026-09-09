package integration_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/advisory"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	inferencefake "github.com/freeside-ai/freeside/daemon/internal/inference/fake"
	"github.com/freeside-ai/freeside/daemon/internal/publicationrecord"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestPublishedFeedbackRetryPreservesFailedAttemptAndInput(t *testing.T) {
	for _, name := range []string{"clean", "questions", "completed-before-failure", "completed-after-retry", "completed-before-retry-commit", "completed-after-sealing", "completed-after-successor-block", "attended-successor", "remediation", "reevaluation", "reevaluation-escalation", "reevaluation-continuation", "reevaluation-continuation-upgrade", "reevaluation-continuation-repeated", "reevaluation-continuation-completed-before-approve", "reevaluation-continuation-completed-after-approve", "reevaluation-continuation-completed-after-queue", "reevaluation-continuation-corrupt", "reevaluation-continuation-cyclic", "reevaluation-continuation-moved", "reevaluation-continuation-missing", "reevaluation-continuation-foreign", "reevaluation-continuation-closed", "reevaluation-continuation-stop", "reevaluation-continuation-discuss"} {
		t.Run(name, func(t *testing.T) { testPublishedFeedbackRetry(t, name) })
	}
}

func testPublishedFeedbackRetry(t *testing.T, scenario string) {
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
	p.startAndRecordExport(t)
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 1 {
		t.Fatalf("publish original: %#v, %v", result, err)
	}
	ready, err := p.attention.GetAttentionItem(p.ctx, domain.ProductionReadyItemID(p.runID))
	if err != nil {
		t.Fatal(err)
	}
	const deviceID domain.DeviceID = "retry-device"
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		return tx.PutDevice(p.ctx, domain.Device{ID: deviceID, DisplayName: "Retry device", Status: domain.DeviceActive, PairedAt: p.now})
	}); err != nil {
		t.Fatal(err)
	}
	const returnedCommand = "returned-feedback"
	failed := domain.InvocationID("inv-operator-feedback-returned-feedback")
	returnInput := signet.ClientCommand{
		CommandID: returnedCommand, DeviceID: deviceID, ExpectedEntityVersion: ready.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: ready.Item.ID, ItemVersion: ready.Item.ItemVersion,
			PRHeadSHA: ready.Item.PRHeadSHA, ArtifactDigests: ready.Item.ArtifactDigests,
			Action: domain.ActionReturnToAgent, Message: "Correct the note; preserve code and tests.",
		},
	}
	if _, err := p.attention.Submit(p.ctx, returnInput); err != nil {
		t.Fatal(err)
	}
	assertFeedbackRunProjection(t, p, domain.RunOutcomePending)
	if scenario == "questions" {
		for _, commandID := range []string{"answer-first-question", "answer-second-question"} {
			failed = answerFeedbackQuestion(t, p, failed, commandID, deviceID)
		}
	}
	if scenario == "completed-before-failure" {
		recordOriginalCompletion(t, p)
	}
	p.driver.Script(failed, fake.StageScript{
		Outcome: fake.OutcomeFail, Result: exec.StageResult{Summary: "feedback launch failed"},
	})
	if _, err := p.workflow.ReconcileProductionPublications(p.ctx); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatal(err)
		}
	}
	failure, err := p.attention.GetAttentionItem(p.ctx, domain.ItemID("execution-failure-"+string(failed)))
	if err != nil {
		t.Fatal(err)
	}
	if scenario == "completed-before-failure" {
		if failure.Item.Offers(domain.ActionRetry) {
			t.Fatal("completed work unit offered feedback Retry")
		}
		return
	}
	if !failure.Item.Offers(domain.ActionRetry) {
		_ = p.store.Read(p.ctx, func(tx *store.ReadTx) error {
			_, err := tx.OperatorFeedbackRetryParent(p.ctx, failure.Item)
			t.Logf("retry eligibility: %v", err)
			return nil
		})
		t.Fatalf("eligible failure has no Retry: %v", failure.Item.RequestedDecision)
	}
	if scenario == "clean" {
		assertRealRunCheckpoint(t, p, true, "retained")
		assertRealRunCheckpoint(t, p, false, "")
	}
	var oldOutcome domain.ExecutionOutcome
	var oldInput domain.AgentInvocation
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		oldOutcome, err = tx.GetExecutionOutcomeRecord(p.ctx, failed)
		if err != nil {
			return err
		}
		oldInput, err = tx.GetAgentInvocation(p.ctx, failed)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if scenario == "questions" && len(oldInput.InputIDs) != 4 {
		t.Fatalf("two answers did not accumulate after the root and returned feedback: %v", oldInput.InputIDs)
	}
	retryInput := signet.ClientCommand{
		CommandID: "retry-feedback", DeviceID: deviceID, ExpectedEntityVersion: failure.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: failure.Item.ID, ItemVersion: failure.Item.ItemVersion,
			PRHeadSHA: failure.Item.PRHeadSHA, ArtifactDigests: failure.Item.ArtifactDigests,
			Action: domain.ActionRetry,
		},
	}
	if _, err := p.attention.Submit(p.ctx, retryInput); err != nil {
		t.Fatal(err)
	}
	if scenario == "clean" {
		assertRealRunCheckpoint(t, p, true, "retained")
	}
	if scenario == "completed-after-retry" || scenario == "completed-before-retry-commit" {
		if scenario == "completed-after-retry" {
			recordOriginalCompletion(t, p)
		} else {
			p.workflow = p.newEngine(t, productionCrashSeams{transitionHook: func(transition engine.DurableTransition, at engine.DurableTransitionSide) error {
				if transition == engine.DurableTransitionOperatorFeedback && at == engine.DurableTransitionBefore {
					recordOriginalCompletion(t, p)
				}
				return nil
			}}, true)
		}
		for range 2 {
			if _, err := p.workflow.ReconcileProductionPublications(p.ctx); err != nil {
				t.Fatalf("accepted retry after completion stopped publication reconciliation: %v", err)
			}
			if _, err := p.workflow.Reconcile(p.ctx); err != nil {
				t.Fatalf("accepted retry after completion stopped the engine: %v", err)
			}
			p.workflow = p.newEngine(t, productionCrashSeams{}, true)
		}
		parked, err := p.attention.GetAttentionItem(p.ctx, "operator-feedback-undeliverable-retry-feedback")
		if err != nil || !parked.Item.Offers(domain.ActionAcknowledge) ||
			!strings.Contains(parked.Item.Reason, "work unit completed") {
			t.Fatalf("accepted retry did not retain a completion refusal: %#v, %v", parked, err)
		}
		if _, err := p.attention.Submit(p.ctx, retryInput); err != nil {
			t.Fatalf("accepted retry replay: %v", err)
		}
		if _, err := p.attention.Submit(p.ctx, signet.ClientCommand{
			CommandID: "ack-completed-retry", DeviceID: deviceID, ExpectedEntityVersion: parked.EntityVersion,
			Payload: signet.DecisionPayload{
				ItemID: parked.Item.ID, ItemVersion: parked.Item.ItemVersion,
				PRHeadSHA: parked.Item.PRHeadSHA, ArtifactDigests: parked.Item.ArtifactDigests,
				Action: domain.ActionAcknowledge,
			},
		}); err != nil {
			t.Fatal(err)
		}
		p.workflow = p.newEngine(t, productionCrashSeams{}, true)
		if _, err := p.workflow.ReconcileProductionPublications(p.ctx); err != nil {
			t.Fatal(err)
		}
		settled, err := p.attention.GetAttentionItem(p.ctx, parked.Item.ID)
		if err != nil || !reflect.DeepEqual(settled.Item, parked.Item) {
			t.Fatalf("restart changed acknowledged refusal: %#v, %v", settled, err)
		}
		if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
			if _, err := tx.GetAgentInvocation(p.ctx, "inv-operator-feedback-retry-feedback"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("completed retry created an invocation: %v", err)
			}
			outcome, err := tx.GetExecutionOutcomeRecord(p.ctx, failed)
			if err != nil || !reflect.DeepEqual(outcome, oldOutcome) {
				t.Fatalf("completed retry changed failure history: %#v, %v", outcome, err)
			}
			invocation, err := tx.GetAgentInvocation(p.ctx, failed)
			if err != nil || !reflect.DeepEqual(invocation, oldInput) {
				t.Fatalf("completed retry changed retained inputs: %#v, %v", invocation, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return
	}
	if scenario == "clean" {
		for _, side := range []engine.DurableTransitionSide{engine.DurableTransitionBefore, engine.DurableTransitionAfter} {
			interrupted := errors.New("stop at feedback retry transition")
			p.workflow = p.newEngine(t, productionCrashSeams{transitionHook: func(transition engine.DurableTransition, at engine.DurableTransitionSide) error {
				if transition == engine.DurableTransitionOperatorFeedback && at == side {
					return interrupted
				}
				return nil
			}}, true)
			if _, err := p.workflow.ReconcileProductionPublications(p.ctx); !errors.Is(err, interrupted) {
				t.Fatalf("retry transition %s did not interrupt: %v", side, err)
			}
			if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
				_, err := tx.GetAgentInvocation(p.ctx, "inv-operator-feedback-retry-feedback")
				if side == engine.DurableTransitionBefore && !errors.Is(err, store.ErrNotFound) || side == engine.DurableTransitionAfter && err != nil {
					t.Fatalf("retry atomicity at %s: %v", side, err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			p.restartDurableState(t)
		}
	}
	p.restartDurableState(t)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	for range 2 {
		if _, err := p.workflow.ReconcileProductionPublications(p.ctx); err != nil {
			t.Fatal(err)
		}
	}
	if scenario == "clean" {
		assertRealRunCheckpoint(t, p, true, "retained")
		key := "inv-operator-feedback-retry-feedback"
		original := readOutboxPayload(t, p, key)
		var altered map[string]any
		if err := json.Unmarshal(original, &altered); err != nil {
			t.Fatal(err)
		}
		altered["run_id"] = "missing-retry-run"
		body, err := json.Marshal(altered)
		if err != nil {
			t.Fatal(err)
		}
		writeOutboxPayload(t, p, key, body)
		assertRealRunCheckpoint(t, p, true, "")
		writeOutboxPayload(t, p, key, original)
	}
	if _, err := p.attention.Submit(p.ctx, returnInput); err != nil {
		t.Fatal(err)
	}
	if _, err := p.attention.Submit(p.ctx, retryInput); err != nil {
		t.Fatal(err)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		next, err := tx.GetAgentInvocation(p.ctx, "inv-operator-feedback-retry-feedback")
		if err != nil {
			return err
		}
		if next.ID == oldInput.ID || !reflect.DeepEqual(next.InputIDs, oldInput.InputIDs) {
			t.Fatalf("retry changed retained inputs: old=%#v next=%#v", oldInput, next)
		}
		outcome, err := tx.GetExecutionOutcomeRecord(p.ctx, failed)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(outcome, oldOutcome) {
			t.Fatalf("retry changed old failure: %#v", outcome)
		}
		request, err := tx.OperatorFeedbackIntent(p.ctx, next.ID)
		if err != nil {
			return err
		}
		if request.RunID != p.runID || request.Retry == nil || request.Retry.FailedInvocationID != failed {
			t.Fatalf("wrong retry lineage: %#v", request)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if completeFeedbackSuccessor(t, p, "inv-operator-feedback-retry-feedback", ready.Item.PRHeadSHA, scenario) {
		assertSuccessorCorruptionRejected(t, p)
	}
}

func recordOriginalCompletion(t *testing.T, p *productionPublicationHarness) {
	t.Helper()
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		binding, err := tx.GetWorkUnitPRBinding(p.ctx, p.declaration.ID)
		if err != nil {
			return err
		}
		pull := domain.PullMergeFact{
			Repo: binding.Repo, RepositoryID: binding.RepositoryID, PRNumber: binding.PRNumber,
			State: domain.PullRequestClosed, Merged: true, MergeCommitSHA: "deadbeef", BaseRef: binding.BaseRef, HeadSHA: binding.HeadSHA, ObservedAt: p.now,
		}
		issue := domain.IssueStateFact{
			Repo: binding.Repo, RepositoryID: binding.RepositoryID, IssueNumber: *p.declaration.BoundIssue,
			State: domain.IssueClosed, ClosedByCommitSHA: "deadbeef", ObservedAt: p.now,
		}
		if _, err := tx.AppendPullMergeFact(p.ctx, pull); err != nil {
			return err
		}
		if _, err := tx.AppendIssueStateFact(p.ctx, issue); err != nil {
			return err
		}
		completion, ok := domain.EvaluateWorkUnitCompletion(*p.declaration, binding, pull, &issue)
		if !ok {
			t.Fatal("fixture did not derive work-unit completion")
		}
		return tx.RecordWorkUnitCompletion(p.ctx, completion)
	}); err != nil {
		t.Fatal(err)
	}
}

func answerFeedbackQuestion(t *testing.T, p *productionPublicationHarness, source domain.InvocationID, commandID string, deviceID domain.DeviceID) domain.InvocationID {
	t.Helper()
	blocked := domain.BlockedOutcome{
		Version: domain.BlockedOutcomeEncodingVersion, Kind: domain.BlockedKindOwnerDecision,
		Decisions: []domain.Decision{{
			Question: "Keep the compatibility target?", WhyBlocking: "The correction needs an explicit target.",
			Options:        []domain.DecisionOption{{Label: "Keep", Tradeoffs: "Preserve compatibility."}, {Label: "Change", Tradeoffs: "Needs a separate scope decision."}},
			Recommendation: "Keep",
		}},
	}
	body, err := domain.EncodeBlockedOutcome(blocked)
	if err != nil {
		t.Fatal(err)
	}
	digest := domain.Digest(contentaddr.Sum(body))
	p.driver.Script(source, fake.StageScript{
		PendingInspects: 1, Outcome: fake.OutcomeBlocked,
		Result: exec.StageResult{Artifacts: []domain.Digest{digest}, Summary: exec.TruncateSummary(blocked.Decisions[0].Question)},
	})
	if _, err := p.workflow.ReconcileProductionPublications(p.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := p.workflow.Reconcile(p.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := p.blobs.Put(digest, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	claim := domain.AgentClaim{
		Label: export.BlockedEvidenceLabel, Artifact: domain.ArtifactID("blocked-" + source), Digest: digest,
		Provenance: domain.Provenance{ProducerClass: domain.ProducerAgent, ProducerInvocationID: source, HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal},
		Metadata:   domain.EvidenceMetadata{MediaType: domain.EvidenceMediaApplicationJSON, SizeBytes: int64(len(body)), CreatedAt: p.now, Source: domain.EvidenceSourceClaim, Availability: domain.EvidenceAvailable},
	}
	metadata := claim.Metadata
	metadata.Source = domain.EvidenceSourceRun
	artifact, err := domain.NewArtifact(domain.ArtifactInput{ID: claim.Artifact, Type: domain.ArtifactKindEvidence, Digest: digest, Provenance: claim.Provenance, Metadata: metadata}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		if err := tx.PutArtifact(p.ctx, artifact); err != nil {
			return err
		}
		return tx.PutAgentClaims(p.ctx, source, []domain.AgentClaim{claim})
	}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatal(err)
		}
	}
	question, err := p.attention.GetAttentionItem(p.ctx, domain.ItemID("question-"+source))
	if err != nil {
		t.Fatal(err)
	}
	route := domain.AnswerRouteRetryImplementation
	if _, err := p.attention.Submit(p.ctx, signet.ClientCommand{
		CommandID: commandID, DeviceID: deviceID, ExpectedEntityVersion: question.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: question.Item.ID, ItemVersion: question.Item.ItemVersion,
			PRHeadSHA: question.Item.PRHeadSHA, ArtifactDigests: question.Item.ArtifactDigests,
			Action: domain.ActionAnswerAndRetry, AnswerRoute: &route, Message: "Keep the existing compatibility target.",
		},
	}); err != nil {
		t.Fatal(err)
	}
	return domain.InvocationID("inv-operator-feedback-" + commandID)
}

func completeFeedbackSuccessor(t *testing.T, p *productionPublicationHarness, invocation domain.InvocationID, oldHead string, scenario string) bool {
	t.Helper()
	p.driver.Script(invocation, fake.StageScript{
		PendingInspects: 1, Outcome: fake.OutcomeComplete,
		Result: exec.StageResult{Summary: "Corrected note exported."},
	})
	if result, err := p.workflow.Reconcile(p.ctx); err != nil || result.InvocationsStarted != 1 {
		t.Fatalf("retry dispatch: %#v, %v", result, err)
	}
	var admission domain.ExecutionAdmission
	var run domain.Run
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		admission, err = tx.GetExecutionAdmissionRecord(p.ctx, invocation)
		if err != nil {
			return err
		}
		run, err = tx.GetRun(p.ctx, p.runID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	replay := buildProductionReplayWithContentAt(t, p.publicationHarness, p.runID, run.SpecDigest,
		submissionSpecification(string(p.runID)), p.declaration.BoundIssue, invocation, fakePublicationTime.Add(time.Minute),
		"production change\ncorrected decision note\n")
	exported, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: invocation, AdmissionID: admission.ID, ObservedBaseSHA: p.baseSHA,
		HeadSHA: replay.HeadSHA, ManifestDigest: replay.ManifestDigest, RecordedAt: replay.ImportOptions.CommitDate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.RecordExecutionExport(p.ctx, p.store, exported, replay); err != nil {
		t.Fatal(err)
	}
	assertRealRunCheckpoint(t, p, true, "retained")
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		successor, err := tx.CurrentPublicationSuccessor(p.ctx, p.runID)
		if err != nil || successor == nil {
			return fmt.Errorf("checkpoint fixture has no sealed successor: %w", err)
		}
		if _, err := readRealRunCheckpoint(p.ctx, tx, p.runID, false); err == nil {
			return fmt.Errorf("unpublished successor passed final verification")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	checkpoint := filepath.Join(t.TempDir(), "successor-checkpoint.db")
	if err := p.store.Checkpoint(p.ctx, checkpoint); err != nil {
		t.Fatal(err)
	}
	if _, err := p.store.Restore(p.ctx, checkpoint); err != nil {
		t.Fatal(err)
	}
	assertFeedbackRunProjection(t, p, domain.RunOutcomePending)
	p.replay = replay
	if scenario == "attended-successor" {
		p.workflow = p.newEngineForMode(t, productionCrashSeams{}, true, nil, domain.ModeAttendedDev, true)
		if _, err := p.workflow.ReconcileProductionPublications(p.ctx); err != nil {
			t.Fatal(err)
		}
		if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
			hold, found, err := tx.GetRunHold(p.ctx, p.runID)
			if err != nil || !found || hold.Reason != domain.HoldAttendedModeActive {
				t.Fatalf("pending successor lost attended hold: %#v, %t, %v", hold, found, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if scenario == "completed-after-sealing" || scenario == "completed-after-successor-block" {
		if scenario == "completed-after-successor-block" {
			p.room.fail = true
			p.workflow = p.newEngine(t, productionCrashSeams{}, true)
			if result, err := p.reconcileLanes(); err != nil || result.BlockedItemsCreated != 1 {
				t.Fatalf("successor verification block before original merge: %#v, %v", result, err)
			}
			assertFeedbackRunProjection(t, p, domain.RunOutcomeBlocked)
		}
		if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
			successor, err := tx.CurrentPublicationSuccessor(p.ctx, p.runID)
			if err != nil || successor == nil {
				t.Fatalf("successor not sealed: %#v, %v", successor, err)
			}
			binding, err := tx.EffectiveWorkUnitPRBinding(p.ctx, p.declaration.ID)
			if err != nil || binding.HeadSHA != oldHead {
				t.Fatalf("pending successor replaced published completion authority: %#v, %v", binding, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		recordOriginalCompletion(t, p)
		if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
			completion, err := tx.GetWorkUnitCompletion(p.ctx, p.declaration.ID)
			if err != nil {
				return err
			}
			invocation, err := tx.PublishedPublicationInvocationID(p.ctx, p.runID)
			if err != nil {
				return err
			}
			return tx.AppendRunMilestone(p.ctx, domain.RunMilestone{
				RunID: p.runID, Kind: domain.MilestoneWorkUnitCompleted,
				InvocationID: &invocation, RecordedAt: completion.RecordedAt,
			})
		}); err != nil {
			t.Fatal(err)
		}
		assertFeedbackRunProjection(t, p, domain.RunOutcomeCompleted)
		if conclusion := authenticatedProductionConclusion(t, p); conclusion.Outcome != domain.RunOutcomeCompleted || !conclusion.Final {
			t.Fatalf("completed predecessor conclusion: %#v", conclusion)
		}
		verifications := p.room.runs
		p.restartDurableState(t)
		p.workflow = p.newEngine(t, productionCrashSeams{}, true)
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatal(err)
		}
		assertFeedbackRunProjection(t, p, domain.RunOutcomeCompleted)
		if p.room.runs != verifications {
			t.Fatal("completed work unit continued successor verification")
		}
		if scenario == "completed-after-sealing" {
			foreign, reason := domain.ProductionPublicationInvocationID("unrelated-run"), domain.HoldTrustBlocked
			if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
				return tx.AppendRunMilestone(p.ctx, domain.RunMilestone{
					RunID: p.runID, Kind: domain.MilestonePublicationBlocked,
					InvocationID: &foreign, Reason: &reason, RecordedAt: p.now.Add(time.Minute),
				})
			}); err != nil {
				t.Fatal(err)
			}
			if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
				run, err := tx.GetRun(p.ctx, p.runID)
				if err != nil {
					return err
				}
				observation, err := tx.ObserveRun(p.ctx, p.runID)
				if err != nil {
					return err
				}
				_, err = engine.AuthenticatedProductionRunConclusion(p.ctx, tx, run, observation)
				return err
			}); !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("completion hid a foreign publication milestone: %v", err)
			}
		}
		return false
	}
	p.now = p.now.Add(time.Minute)
	p.reviewer.Script(engine.ProductionReviewInvocationID(p.runID, 2), fake.ReviewScript{
		Outcome: fake.OutcomeComplete, Result: exec.ReviewResult{
			BaseSHA: p.baseSHA, HeadSHA: replay.HeadSHA, Provider: "openai", ModelConfiguration: "codex/test", CostOwner: "test",
			CompletedAt: p.now, CompletionEvidence: productionDigest([]byte("clean successor review")),
		},
	})
	p.restartDurableState(t)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	wantVerifications := 2
	if scenario == "clean" {
		assertUnreadableSuccessorTaskHeld(t, p)
	}
	if strings.HasPrefix(scenario, "reevaluation") {
		p.room.fail = true
		if result, err := p.reconcileLanes(); err != nil || result.BlockedItemsCreated != 1 {
			t.Fatalf("successor verification block: %#v, %v", result, err)
		}
		assertFeedbackRunProjection(t, p, domain.RunOutcomeBlocked)
		if conclusion := authenticatedProductionConclusion(t, p); conclusion.Outcome != domain.RunOutcomeBlocked || !conclusion.Final {
			t.Fatalf("blocked successor conclusion: %#v", conclusion)
		}
		blocked, err := p.attention.GetAttentionItem(p.ctx, "production-blocked-feedback-returned-feedback")
		if err != nil {
			t.Fatal(err)
		}
		repairPublicationTrustProfile(t, p)
		submitPublicationRerun(t, p, blocked, "rerun-feedback-trust")
		assertFeedbackRunProjection(t, p, domain.RunOutcomePending)
		p.room.fail = false
		p.restartDurableState(t)
		p.workflow = p.newEngine(t, productionCrashSeams{}, true)
		wantVerifications = 3
		if scenario == "reevaluation-escalation" || strings.HasPrefix(scenario, "reevaluation-continuation") {
			scriptSuccessorFinding(t, p, replay)
			assertSuccessorReevaluationEscalates(t, p)
			if strings.HasPrefix(scenario, "reevaluation-continuation") {
				completePublicationContinuation(t, p, run, scenario)
			}
			return false
		}
	}
	if scenario == "remediation" {
		replay = remediateFeedbackSuccessor(t, p, replay, run)
		wantVerifications = 3
	} else {
		p.transport.successorUpdateFailure = &net.OpError{Op: "write", Net: "tcp", Err: syscall.ECONNRESET}
		if _, err := p.reconcileLanes(); err != nil && !errors.Is(err, syscall.ECONNRESET) {
			t.Fatal(err)
		}
		if p.transport.successorUpdateFailure != nil {
			t.Fatal("successor did not reach post-push interruption")
		}
		p.restartDurableState(t)
		p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	}
	var completed engine.ReconcileResult
	for range 4 {
		result, err := p.reconcileLanes()
		if err != nil {
			t.Fatal(err)
		}
		completed.PublicationTasksCompleted += result.PublicationTasksCompleted
		completed.ReadyItemsCreated += result.ReadyItemsCreated
		if completed.PublicationTasksCompleted > 0 {
			break
		}
	}
	if completed.PublicationTasksCompleted != 1 || completed.ReadyItemsCreated != 1 {
		if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
			items, err := tx.ListAttentionItems(p.ctx)
			for _, item := range items {
				t.Logf("item %s: %s: %s", item.Value.ID, item.Value.Status, item.Value.Reason)
			}
			return err
		}); err != nil {
			t.Log(err)
		}
		t.Fatalf("successor did not converge: %#v", completed)
	}
	if p.room.runs != wantVerifications {
		t.Fatalf("verification runs = %d, want %d", p.room.runs, wantVerifications)
	}
	if scenario == "clean" {
		if released := productionItemRecord(t, p, "production-task-quarantined-1-"+string(p.runID)); released.Status != domain.StatusSuperseded {
			t.Fatalf("recovered successor left an open quarantine notice: %#v", released)
		}
	}
	assertFeedbackRunProjection(t, p, domain.RunOutcomePublished)
	if conclusion := authenticatedProductionConclusion(t, p); conclusion.Outcome != domain.RunOutcomePublished || !conclusion.Final {
		t.Fatalf("successor supervision conclusion = %#v", conclusion)
	}
	if refs, prs := p.forge.counts(); refs != 1 || prs != 1 {
		t.Fatalf("successor created another resource: refs=%d prs=%d", refs, prs)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		old, err := tx.GetReadyItemPRBinding(p.ctx, domain.ProductionReadyItemID(p.runID))
		if err != nil {
			return err
		}
		currentID, err := tx.CurrentProductionReadyItemID(p.ctx, p.runID)
		if err != nil {
			return err
		}
		current, err := tx.GetReadyItemPRBinding(p.ctx, currentID)
		if err != nil {
			return err
		}
		checkpoint, err := readRealRunCheckpoint(p.ctx, tx, p.runID, false)
		if err != nil {
			return err
		}
		if checkpoint.State != "ready" || checkpoint.Binding != current {
			t.Fatal("live verifier selected historical publication instead of successor")
		}
		if current.ItemID == old.ItemID || current.HeadSHA != replay.HeadSHA || old.HeadSHA != oldHead || current.PRNumber != old.PRNumber {
			t.Fatalf("incorrect successor bindings: old=%#v current=%#v", old, current)
		}
		historical, err := tx.GetWorkUnitPRBinding(p.ctx, p.declaration.ID)
		if err != nil {
			return err
		}
		effective, err := tx.EffectiveWorkUnitPRBinding(p.ctx, p.declaration.ID)
		if err != nil {
			return err
		}
		if historical.HeadSHA != oldHead || effective.HeadSHA != current.HeadSHA || effective.PRNumber != historical.PRNumber {
			t.Fatalf("work-unit bindings: historical=%#v effective=%#v", historical, effective)
		}
		successor, err := tx.CurrentPublicationSuccessor(p.ctx, p.runID)
		if err != nil {
			return err
		}
		if err := tx.AuthenticateSuccessorProducer(p.ctx, *successor, old.ProducingInvocationID); err == nil {
			t.Fatal("successor accepted original producer")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return true
}

func assertFeedbackRunProjection(t *testing.T, p *productionPublicationHarness, want domain.RunOutcome) {
	t.Helper()
	runs, err := p.attention.ListRuns(p.ctx)
	if err != nil || len(runs) != 1 || runs[0].Run.Outcome != want {
		_, detailErr := p.attention.GetRun(p.ctx, p.runID)
		t.Logf("direct run projection: %v", detailErr)
		t.Fatalf("feedback projection = %#v, %v; want %s", runs, err, want)
	}
}

func assertSuccessorCorruptionRejected(t *testing.T, p *productionPublicationHarness) {
	t.Helper()
	var binding domain.ReadyItemPRBinding
	var intentEntry, outcomeEntry store.QueueEntry
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		id, err := tx.CurrentProductionReadyItemID(p.ctx, p.runID)
		if err != nil {
			return err
		}
		binding, err = tx.GetReadyItemPRBinding(p.ctx, id)
		if err != nil {
			return err
		}
		intentEntry, err = tx.GetOutbox(p.ctx, "publish/"+string(binding.PublicationInvocationID)+"/publish.publication")
		if err != nil {
			return err
		}
		outcomeEntry, err = tx.GetInbox(p.ctx, publicationrecord.OutcomeKey(binding.PublicationIdentity))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", p.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := raw.Close(); err != nil {
			t.Error(err)
		}
	}()
	write := func(intentBody, outcomeBody []byte) {
		t.Helper()
		if _, err := raw.ExecContext(p.ctx, "UPDATE outbox SET payload=?, payload_digest=? WHERE idempotency_key=?", intentBody, contentaddr.Sum(intentBody), intentEntry.IdempotencyKey); err != nil {
			t.Fatal(err)
		}
		if _, err := raw.ExecContext(p.ctx, "UPDATE inbox SET payload=? WHERE idempotency_key=?", outcomeBody, outcomeEntry.IdempotencyKey); err != nil {
			t.Fatal(err)
		}
	}
	intent, err := publicationrecord.DecodeIntent(intentEntry.Payload)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := publicationrecord.DecodeOutcome(outcomeEntry.Payload)
	if err != nil {
		t.Fatal(err)
	}
	intent.Successor.HeadSHA = strings.Repeat("f", 40)
	outcome.Successor.HeadSHA = intent.Successor.HeadSHA
	intentBody, err := intent.Encode()
	if err != nil {
		t.Fatal(err)
	}
	outcomeBody, err := outcome.Encode()
	if err != nil {
		t.Fatal(err)
	}
	write(intentBody, outcomeBody)
	err = p.store.Read(p.ctx, func(tx *store.ReadTx) error { _, err := tx.GetReadyItemPRBinding(p.ctx, binding.ItemID); return err })
	write(intentEntry.Payload, outcomeEntry.Payload)
	if err == nil {
		t.Fatal("matching forged predecessor claims passed ready reconstruction")
	}
	if err := p.store.WriteInternal(p.ctx, func(tx *store.InternalTx) error {
		key := "publication-successor/unrelated-run/publish-feedback-invalid"
		if _, _, err := tx.EnqueueOutbox(p.ctx, key, domain.PublicationSuccessorKind, []byte("invalid")); err != nil {
			return err
		}
		return tx.MarkOutboxDispatched(p.ctx, key)
	}); err != nil {
		t.Fatal(err)
	}
	assertFeedbackRunProjection(t, p, domain.RunOutcomePublished)
}

func (tr *integrationTransport) UpdateHead(_ context.Context, checkout engine.PublicationCheckout, update publish.GatedUpdate) (publish.PushResult, error) {
	sealed, ok := checkout.(integrationCheckout)
	if !ok || sealed.owner != tr || update.ExpectedOldHead() == "" || update.SourceHeadSHA() == "" {
		return publish.PushResult{}, engine.ErrForeignPublicationCheckout
	}
	tr.forge.mu.Lock()
	defer tr.forge.mu.Unlock()
	old := tr.forge.refs[update.Branch()]
	if old != update.ExpectedOldHead() && old != update.SourceHeadSHA() {
		return publish.PushResult{}, publish.ErrPublicationConflict
	}
	tr.forge.refs[update.Branch()] = update.SourceHeadSHA()
	for i := range tr.forge.prs {
		if tr.forge.prs[i].HeadRef == update.Branch() {
			tr.forge.prs[i].HeadSHA = update.SourceHeadSHA()
		}
	}
	if tr.successorUpdateFailure != nil {
		err := tr.successorUpdateFailure
		tr.successorUpdateFailure = nil
		return publish.PushResult{}, err
	}
	return publish.PushResult{}, nil
}

func configureSuccessorJudgments(t *testing.T, p *productionPublicationHarness) {
	t.Helper()
	classifier := inferencefake.New()
	classifier.Script(inference.ClassifierSiteID, inferencefake.Script{Response: inference.Response{
		Output:       []byte(`{"materiality":"high","confidence":"high","note":"actionable"}`),
		ComputeUnits: 3,
	}})
	advisoryStore, err := advisory.Open(
		filepath.Join(t.TempDir(), "advisory.json"), 20, 16<<10,
		advisory.WithClock(func() time.Time { return p.now }),
	)
	if err != nil {
		t.Fatal(err)
	}
	limits := inference.Limits{
		Calls: 10, ComputeUnits: 100_000, AttentionItems: 10, Starvation: time.Hour,
	}
	p.judgments, err = inference.New(inference.Config{
		StatePath: filepath.Join(t.TempDir(), "ledger.json"),
		Binding:   inference.Binding{Provider: "fake", Model: "classifier", Driver: classifier},
		Sites: []inference.Site{inference.ClassifierSite(inference.Budget{
			Window: time.Hour, Site: limits, Project: limits, Global: limits,
			MaxCallsPerRoot: 10, MaxStarvationPerRoot: time.Hour,
		})},
		Advisory: advisoryStore, Now: func() time.Time { return p.now },
	})
	if err != nil {
		t.Fatal(err)
	}
}

func remediateFeedbackSuccessor(t *testing.T, p *productionPublicationHarness, replay engine.ProductionReplay, run domain.Run) engine.ProductionReplay {
	t.Helper()
	scriptSuccessorFinding(t, p, replay)
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 {
		t.Fatalf("successor adjudication: %#v, %v", result, err)
	}
	assertSuccessorCannotOmitReviewHistory(t, p, replay)
	p.restartDurableState(t)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	producer := domain.InvocationID("inv-remediate-2-" + string(p.runID))
	p.driver.Script(producer, fake.StageScript{PendingInspects: 1, Outcome: fake.OutcomeComplete, Result: exec.StageResult{Summary: "Successor finding fixed."}})
	if result, err := p.workflow.Reconcile(p.ctx); err != nil || result.InvocationsStarted != 1 {
		t.Fatalf("successor remediation dispatch: %#v, %v", result, err)
	}
	for range 2 {
		if _, err := p.workflow.ReconcileProductionPublications(p.ctx); err != nil {
			t.Fatalf("publication during successor remediation: %v", err)
		}
		if item, err := p.attention.GetAttentionItem(p.ctx, domain.ItemID("remediation-marker-quarantined-1-"+string(p.runID))); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("valid successor remediation was quarantined: %#v, %v", item, err)
		}
		p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	}
	var admission domain.ExecutionAdmission
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		admission, err = tx.GetExecutionAdmissionRecord(p.ctx, producer)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	replay = buildProductionReplayWithContentAt(t, p.publicationHarness, p.runID, run.SpecDigest,
		submissionSpecification(string(p.runID)), p.declaration.BoundIssue, producer, fakePublicationTime.Add(2*time.Minute),
		"production change\ncorrected decision note\nreview finding fixed\n")
	exported, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: producer, AdmissionID: admission.ID,
		ObservedBaseSHA: p.baseSHA, HeadSHA: replay.HeadSHA, ManifestDigest: replay.ManifestDigest, RecordedAt: replay.ImportOptions.CommitDate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.RecordProductionExecutionExport(p.ctx, p.store, exported, replay); err != nil {
		t.Fatal(err)
	}
	p.replay = replay
	p.now = p.now.Add(time.Minute)
	p.reviewer.Script(engine.ProductionReviewInvocationID(p.runID, 3), fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: p.baseSHA, HeadSHA: replay.HeadSHA, Provider: "openai", ModelConfiguration: "codex/test", CostOwner: "test",
			CompletedAt: p.now, CompletionEvidence: productionDigest([]byte("clean successor remediation review")),
		},
	})
	return replay
}

func scriptSuccessorFinding(t *testing.T, p *productionPublicationHarness, replay engine.ProductionReplay) {
	t.Helper()
	configureSuccessorJudgments(t, p)
	finding := domain.Finding{
		ID: "successor-finding", RunID: p.runID, Source: "codex_local", Severity: domain.FindingSeverityP1,
		Location: &domain.FindingLocation{Path: "README.md", StartLine: 1, EndLine: 1},
		Message:  "The returned feedback needs one more correction.", RawText: "The returned feedback needs one more correction.", CreatedAt: p.now,
	}
	p.reviewer.Script(engine.ProductionReviewInvocationID(p.runID, 2), fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: p.baseSHA, HeadSHA: replay.HeadSHA, Provider: "openai", ModelConfiguration: "codex/test", CostOwner: "test",
			CompletedAt: p.now, CompletionEvidence: productionDigest([]byte("successor review findings")), Findings: []domain.Finding{finding},
		},
	})
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
}

func assertSuccessorReevaluationEscalates(t *testing.T, p *productionPublicationHarness) {
	t.Helper()
	if result, err := p.reconcileLanes(); err != nil || result.PublicationTasksCompleted != 1 || result.ReadyItemsCreated != 0 {
		t.Fatalf("successor reevaluation escalation: %#v, %v", result, err)
	}
	p.restartDurableState(t)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	item := productionItemRecord(t, p, "successor-reevaluation-review-rerun-feedback-trust")
	if item.Type != domain.AttentionReviewDispute || item.Status != domain.StatusOpen || !strings.Contains(item.Reason, "Approve starts one remediation continuation") ||
		!reflect.DeepEqual(item.RequestedDecision, []domain.Action{domain.ActionApprove, domain.ActionDiscuss, domain.ActionStop}) {
		t.Fatalf("missing actionable escalation: %#v", item)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetAgentInvocation(p.ctx, domain.InvocationID("inv-remediate-2-"+string(p.runID))); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("escalated cycle created remediation: %v", err)
		}
		review, err := tx.LatestReviewRecord(p.ctx, p.runID)
		if err != nil || len(review.FindingIDs) != 1 {
			t.Fatalf("escalation lost findings: %#v, %v", review, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if refs, prs := p.forge.counts(); refs != 1 || prs != 1 {
		t.Fatalf("escalation changed resources: refs=%d prs=%d", refs, prs)
	}
}

func assertSuccessorCannotOmitReviewHistory(t *testing.T, p *productionPublicationHarness, replay engine.ProductionReplay) {
	t.Helper()
	var successor *domain.PublicationSuccessor
	var target publicationrecord.SuccessorTarget
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		successor, err = tx.CurrentPublicationSuccessor(p.ctx, p.runID)
		if err != nil {
			return err
		}
		target, err = tx.PublicationSuccessorTarget(p.ctx, p.runID, successor.PublicationID())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	called := false
	_, err := p.newPublisher(t).PublishSuccessorAfterGateAndFinalize(p.ctx, publish.ExecutionCandidate{
		Candidate: publish.Candidate{
			Repo: fakePublicationRepo, BaseRef: "main", HeadSHA: replay.HeadSHA,
			Title: "Successor without review history", RunID: p.runID,
			InvocationID: successor.PublicationID(), Successor: &target, Branch: target.Branch,
		},
		ProducingInvocationID: successor.FeedbackInvocationID,
	}, map[domain.Digest]bool{p.image.RecipeDigest: true}, func(context.Context, publish.GatedUpdate) error {
		called = true
		return nil
	})
	if called || err == nil || !strings.Contains(err.Error(), "execution candidate carries no disposition history") {
		t.Fatalf("successor without review history reached publication: callback=%t, err=%v", called, err)
	}
}

func assertUnreadableSuccessorTaskHeld(t *testing.T, p *productionPublicationHarness) {
	t.Helper()
	var successor *domain.PublicationSuccessor
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		successor, err = tx.CurrentPublicationSuccessor(p.ctx, p.runID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	key := successor.TaskKey()
	original := readOutboxPayload(t, p, key)
	writeOutboxPayload(t, p, key, []byte(`{"version":"freeside.production-publication/v9"}`))
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatalf("unreadable successor ended reconciliation: %v", err)
	}
	id := "production-task-quarantined-1-" + string(p.runID)
	if held := productionItemRecord(t, p, id); held.Status != domain.StatusOpen {
		t.Fatalf("unreadable successor was not held: %#v", held)
	}
	if refs, prs := p.forge.counts(); refs != 1 || prs != 1 || p.room.runs != 1 {
		t.Fatalf("unreadable successor advanced: refs=%d prs=%d verification=%d", refs, prs, p.room.runs)
	}
	writeOutboxPayload(t, p, key, original)
}

func assertRealRunCheckpoint(t *testing.T, p *productionPublicationHarness, retained bool, state string) {
	t.Helper()
	err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		checkpoint, err := readRealRunCheckpoint(p.ctx, tx, p.runID, retained)
		if err != nil {
			return err
		}
		if checkpoint.State != state {
			return fmt.Errorf("checkpoint state %q, want %q", checkpoint.State, state)
		}
		return nil
	})
	if state == "" {
		if err == nil {
			t.Fatal("unready publication passed final verification")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
}
