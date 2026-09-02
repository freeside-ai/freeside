package engine

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	specifyfake "github.com/freeside-ai/freeside/daemon/internal/specify/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// questionItemsForRun counts the agent_question items bound to a run, so a
// replayed pass is proven to converge on one item rather than create more.
func (f specificationFixture) questionItemsForRun(t *testing.T, runID domain.RunID) []domain.AttentionItem {
	t.Helper()
	var items []domain.AttentionItem
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		all, err := tx.ListAttentionItems(t.Context())
		if err != nil {
			return err
		}
		for _, snapshot := range all {
			item := snapshot.Value
			if item.Type == domain.AttentionAgentQuestion && item.Subject.RunID != nil && *item.Subject.RunID == runID {
				items = append(items, item)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return items
}

func (f specificationFixture) pendingSpecificationIntents(t *testing.T) []store.QueueEntry {
	t.Helper()
	var pending []store.QueueEntry
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		pending, err = tx.ListPendingOutbox(t.Context(), KindSpecificationInvocationRequested)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return pending
}

// answerQuestion submits answer_and_retry on the open question item with the
// given message and returns the command id.
func (f specificationFixture) answerQuestion(t *testing.T, itemID domain.ItemID, commandID, answer string) {
	t.Helper()
	snapshot, err := f.signet.GetAttentionItem(t.Context(), itemID)
	if err != nil {
		t.Fatal(err)
	}
	item := snapshot.Item
	if _, err := f.signet.Submit(t.Context(), signet.ClientCommand{
		CommandID: commandID, DeviceID: "device-1", ExpectedEntityVersion: snapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: item.ID, Action: domain.ActionAnswerAndRetry,
			ItemVersion: item.ItemVersion, ArtifactDigests: item.ArtifactDigests,
			Message: answer,
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSpecificationNeedsDecisionCreatesOneQuestionUnderEitherGate(t *testing.T) {
	for _, specApproval := range []bool{true, false} {
		t.Run(fmt.Sprintf("spec_approval=%t", specApproval), func(t *testing.T) {
			f := newSpecificationFixture(t, specApproval, 4)
			driver := f.newDriver(t)
			firstID := specificationInvocationID("specification-run", 1)
			decisions := decisionsFixture()
			if err := specifyfake.Script(driver, firstID, 0, 0, specify.Output{Decisions: decisions}); err != nil {
				t.Fatal(err)
			}
			f.submit(t)
			engine := f.newEngine(t, driver)
			result, err := engine.Reconcile(t.Context())
			if err != nil || result.ResultsAccepted != 1 {
				t.Fatalf("reconcile = %+v, %v", result, err)
			}

			items := f.questionItemsForRun(t, "specification-run")
			if len(items) != 1 {
				t.Fatalf("agent_question items = %d, want 1", len(items))
			}
			item := items[0]
			artifactID := domain.ArtifactID("decisions-implementation-run-1")
			if item.ID != domain.ItemID("question-"+string(firstID)) ||
				item.Priority != domain.PriorityNormal ||
				item.InterruptionClass != domain.InterruptionExceptional ||
				item.Status != domain.StatusOpen ||
				!slices.Equal(item.RequestedDecision, []domain.Action{domain.ActionAnswerAndRetry, domain.ActionStop}) ||
				item.AgentQuestion == nil || item.AgentQuestion.Stage != domain.StageNameSpecification ||
				item.AgentQuestion.InvocationID != firstID || item.AgentQuestion.Kind != nil ||
				!reflect.DeepEqual(item.AgentQuestion.Decisions, decisions) ||
				len(item.AgentClaims) != 1 || item.AgentClaims[0].Label != domain.AgentQuestionClaimLabel ||
				item.AgentClaims[0].Artifact != artifactID ||
				item.AgentClaims[0].Provenance.ProducerInvocationID != firstID {
				t.Fatalf("question item = %#v", item)
			}
			artifact := f.artifact(t, artifactID)
			if artifact.Digest != item.AgentClaims[0].Digest || artifact.Provenance.ProducerClass != domain.ProducerAgent {
				t.Fatalf("decisions artifact = %#v", artifact)
			}
			if _, err := f.run("implementation-run"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("implementation run after needs_decision = %v, want ErrNotFound", err)
			}
			if pending := f.pendingSpecificationIntents(t); len(pending) != 0 {
				t.Fatalf("pending specification intents = %d, want none", len(pending))
			}

			// A replayed pass, including one after a restart, converges on the
			// same item and terminal without a second item or an implementation
			// start.
			_, before := f.item(t, item.ID)
			replay := func(reopened specificationFixture) {
				t.Helper()
				result, err := reopened.newEngine(t, reopened.newDriver(t)).Reconcile(t.Context())
				if err != nil || result.ResultsAccepted != 0 {
					t.Fatalf("replayed reconcile = %+v, %v", result, err)
				}
				if got := reopened.questionItemsForRun(t, "specification-run"); len(got) != 1 {
					t.Fatalf("agent_question items after replay = %d, want 1", len(got))
				}
				if _, after := reopened.item(t, item.ID); after.EntityVersion != before.EntityVersion {
					t.Fatalf("replay advanced the question item to version %d", after.EntityVersion)
				}
				if _, err := reopened.run("implementation-run"); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("implementation run after replay = %v, want ErrNotFound", err)
				}
			}
			replay(f)
			replay(f.reopen(t))
		})
	}
}

func TestSpecificationNeedsDecisionRejectsDecodedCredentialText(t *testing.T) {
	f := newSpecificationFixture(t, false, 4)
	driver := f.newDriver(t)
	firstID := specificationInvocationID("specification-run", 1)
	decisions := decisionsFixture()
	decisions[0].Options[0].Tradeoffs = `Use credential {"private_key_id":"` + strings.Repeat("a", 40) + `"}`
	if err := specifyfake.Script(driver, firstID, 0, 0, specify.Output{Decisions: decisions}); err != nil {
		t.Fatal(err)
	}
	f.submit(t)
	engine := f.newEngine(t, driver)
	if result, err := engine.Reconcile(t.Context()); err != nil || result.ResultsAccepted != 0 {
		t.Fatalf("reconcile credential-shaped decision = %+v, %v", result, err)
	}
	if items := f.questionItemsForRun(t, "specification-run"); len(items) != 0 {
		t.Fatalf("credential-shaped decision created %d question items", len(items))
	}
	failure, _ := f.item(t, domain.ItemID("execution-failure-"+string(firstID)))
	if failure.Type != domain.AttentionExecutionFailure || failure.Status != domain.StatusOpen ||
		!strings.Contains(failure.Reason, "credential-shaped content") {
		t.Fatalf("credential-shaped decision failure = %#v", failure)
	}
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		_, err := tx.GetArtifact(t.Context(), specificationDecisionArtifactID(specificationRequest{
			ImplementationRunID: "implementation-run", Iteration: 1,
		}))
		return err
	}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("credential-shaped decisions artifact = %v, want ErrNotFound", err)
	}
}

func TestSpecificationNeedsDecisionAnswerReinvokesSpecifierWithHumanFeedback(t *testing.T) {
	f := newSpecificationFixture(t, false, 4)
	driver := f.newDriver(t)
	materializer, err := exec.NewMaterializer(f.blobs, exec.MaterializerOptions{
		MaxInputBytes: exec.ProductionMaxInputBytes, MaxTotalBytes: exec.ProductionMaxTotalInputBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	capturing := &capturingSpecificationDriver{
		StageDriver: driver, materializer: materializer,
		prompts: make(map[domain.InvocationID]capturedSpecificationPrompt),
	}
	firstID := specificationInvocationID("specification-run", 1)
	secondID := specificationInvocationID("specification-run", 2)
	if err := specifyfake.Script(driver, firstID, 0, 0, specify.Output{Decisions: decisionsFixture()}); err != nil {
		t.Fatal(err)
	}
	if err := specifyfake.Script(driver, secondID, 0, 0, specify.Output{Specification: &specify.Specification{
		Summary: "The implementation plan is ready.", Body: "# Specification\n\nTarget both API versions.",
		Addressals: []specify.Addressal{},
	}}); err != nil {
		t.Fatal(err)
	}
	f.submit(t)
	engine := f.newEngine(t, capturing)
	if result, err := engine.Reconcile(t.Context()); err != nil || result.ResultsAccepted != 1 {
		t.Fatalf("first reconcile = %+v, %v", result, err)
	}
	questionID := domain.ItemID("question-" + string(firstID))
	const answer = "Target the current and immediately previous API versions."
	f.answerQuestion(t, questionID, "answer-question", answer)

	created, err := engine.reconcileOperatorFeedback(t.Context())
	if err != nil || created != 1 {
		t.Fatalf("reconcileOperatorFeedback = %d, %v", created, err)
	}
	if pending := f.pendingSpecificationIntents(t); len(pending) != 1 || pending[0].IdempotencyKey != string(secondID) {
		t.Fatalf("pending specification intents = %+v, want the second iteration", pending)
	}
	if result, err := engine.Reconcile(t.Context()); err != nil || result.ResultsAccepted != 1 {
		t.Fatalf("second reconcile = %+v, %v", result, err)
	}
	prompt, ok := capturing.prompt(secondID)
	if !ok {
		t.Fatal("second specification provider inputs were not captured")
	}
	answerArtifact := f.artifact(t, "answer-answer-question")
	assertSpecificationPriorArtifacts(t, prompt, []expectedSpecificationPriorArtifact{{
		role: "human_feedback", digest: answerArtifact.Digest, body: answer,
	}})
	if _, err := f.run("implementation-run"); err != nil {
		t.Fatalf("auto-approved implementation after the answered specification = %v", err)
	}
	if snapshot, err := f.signet.GetAttentionItem(t.Context(), questionID); err != nil ||
		snapshot.Item.Status != domain.StatusSuperseded {
		t.Fatalf("answered question = %+v, %v, want superseded", snapshot.Item.Status, err)
	}
}

func TestSpecificationNeedsDecisionAtIterationLimitRecordsFailure(t *testing.T) {
	f := newSpecificationFixture(t, false, 1)
	driver := f.newDriver(t)
	firstID := specificationInvocationID("specification-run", 1)
	if err := specifyfake.Script(driver, firstID, 0, 0, specify.Output{Decisions: decisionsFixture()}); err != nil {
		t.Fatal(err)
	}
	f.submit(t)
	engine := f.newEngine(t, driver)
	if result, err := engine.Reconcile(t.Context()); err != nil || result.ResultsAccepted != 0 {
		t.Fatalf("reconcile = %+v, %v", result, err)
	}
	if items := f.questionItemsForRun(t, "specification-run"); len(items) != 0 {
		t.Fatalf("agent_question items = %d, want none", len(items))
	}
	failure, _ := f.item(t, domain.ItemID("execution-failure-"+string(firstID)))
	if failure.Type != domain.AttentionExecutionFailure || failure.Status != domain.StatusOpen ||
		!strings.Contains(failure.Reason, ErrSpecificationIterationsExhausted.Error()) {
		t.Fatalf("iteration-limit failure = %#v", failure)
	}
	if pending := f.pendingSpecificationIntents(t); len(pending) != 0 {
		t.Fatalf("pending specification intents = %d, want none", len(pending))
	}
}

func TestSpecificationNeedsDecisionStopResolvesWithoutEnqueueing(t *testing.T) {
	f := newSpecificationFixture(t, true, 4)
	driver := f.newDriver(t)
	firstID := specificationInvocationID("specification-run", 1)
	if err := specifyfake.Script(driver, firstID, 0, 0, specify.Output{Decisions: decisionsFixture()}); err != nil {
		t.Fatal(err)
	}
	f.submit(t)
	engine := f.newEngine(t, driver)
	if result, err := engine.Reconcile(t.Context()); err != nil || result.ResultsAccepted != 1 {
		t.Fatalf("reconcile = %+v, %v", result, err)
	}
	questionID := domain.ItemID("question-" + string(firstID))
	snapshot, err := f.signet.GetAttentionItem(t.Context(), questionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.signet.Submit(t.Context(), signet.ClientCommand{
		CommandID: "stop-question", DeviceID: "device-1", ExpectedEntityVersion: snapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: questionID, Action: domain.ActionStop,
			ItemVersion: snapshot.Item.ItemVersion, ArtifactDigests: snapshot.Item.ArtifactDigests,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if stopped, err := f.signet.GetAttentionItem(t.Context(), questionID); err != nil ||
		stopped.Item.Status != domain.StatusResolved {
		t.Fatalf("stopped question = %+v, %v, want resolved", stopped.Item.Status, err)
	}
	if created, err := engine.reconcileOperatorFeedback(t.Context()); err != nil || created != 0 {
		t.Fatalf("stop reconciliation = %d, %v", created, err)
	}
	if result, err := engine.Reconcile(t.Context()); err != nil || result.ResultsAccepted != 0 {
		t.Fatalf("reconcile after stop = %+v, %v", result, err)
	}
	if pending := f.pendingSpecificationIntents(t); len(pending) != 0 {
		t.Fatalf("pending specification intents = %d, want none", len(pending))
	}
	if _, err := f.run("implementation-run"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("implementation run after stop = %v, want ErrNotFound", err)
	}
}
