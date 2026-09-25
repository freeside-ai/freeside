package engine

import (
	"encoding/json"
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
	answerBody, err := json.Marshal(specificationAnswerInput{
		Version:  specificationAnswerInputVersion,
		Question: *specificationQuestionFacts(firstID),
		Answer:   answer,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertSpecificationPriorArtifacts(t, prompt, []expectedSpecificationPriorArtifact{{
		role: "human_feedback", digest: answerArtifact.Digest, body: string(answerBody),
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
	run, err := f.run("specification-run")
	if err != nil {
		t.Fatal(err)
	}
	task := cancellationTask(t, f.store, run.TaskID)
	if task.Cancellation == nil || task.Cancellation.State != domain.TaskCancellationRequested || !domain.TaskWIP(task) {
		t.Fatalf("question Stop did not fence the held episode: %+v", task)
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

// TestSpecifierFixtureDistinguishesAssumptionFromOwnerDecision is the
// prompt-contract fixture: a repository-practice detail is a bounded
// assumption inside a returned specification, while a product question
// comes back as needs_decision and stops the run on a question item.
func TestSpecifierFixtureDistinguishesAssumptionFromOwnerDecision(t *testing.T) {
	for _, tc := range []struct {
		name     string
		output   specify.Output
		question bool
	}{
		{
			"bounded assumption", specify.Output{Specification: &specify.Specification{
				Summary:    "Assumes the existing table naming convention; no owner decision is open.",
				Body:       "# Specification\n\nAssumption: new tables follow the repository's snake_case convention.",
				Addressals: []specify.Addressal{},
			}}, false,
		},
		{"owner decision", specify.Output{Decisions: decisionsFixture()}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSpecificationFixture(t, false, 4)
			driver := f.newDriver(t)
			firstID := specificationInvocationID("specification-run", 1)
			if err := specifyfake.Script(driver, firstID, 0, 0, tc.output); err != nil {
				t.Fatal(err)
			}
			f.submit(t)
			if result, err := f.newEngine(t, driver).Reconcile(t.Context()); err != nil || result.ResultsAccepted != 1 {
				t.Fatalf("reconcile = %+v, %v", result, err)
			}
			questions := f.questionItemsForRun(t, "specification-run")
			_, implementationErr := f.run("implementation-run")
			if tc.question {
				if len(questions) != 1 || !errors.Is(implementationErr, store.ErrNotFound) {
					t.Fatalf("owner decision: questions = %d, implementation = %v", len(questions), implementationErr)
				}
				return
			}
			if len(questions) != 0 || implementationErr != nil {
				t.Fatalf("bounded assumption: questions = %d, implementation = %v", len(questions), implementationErr)
			}
		})
	}
}

func (f specificationFixture) assertItemAbsent(t *testing.T, id domain.ItemID) {
	t.Helper()
	err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		_, _, err := tx.GetAttentionItemSnapshot(t.Context(), id)
		return err
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("attention item %s = %v, want ErrNotFound", id, err)
	}
}

func (f specificationFixture) assertArtifactAbsent(t *testing.T, id domain.ArtifactID) {
	t.Helper()
	err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		_, err := tx.GetArtifact(t.Context(), id)
		return err
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("artifact %s = %v, want ErrNotFound", id, err)
	}
}

// sketchDecisionsFixture is the first-turn clarification a sketch produces: one
// question each for the task's outcome, scope, and non-goals, every one with a
// recommendation. It stands in for a source that leaves those three unresolved,
// which is what makes the round a sketch round; the scripted fake never reads
// the source body.
func sketchDecisionsFixture() []domain.Decision {
	return []domain.Decision{
		{
			Question:    "What outcome must the task achieve?",
			WhyBlocking: "The specification cannot fix behavior without the target outcome.",
			Options: []domain.DecisionOption{
				{Label: "Ship the importer", Tradeoffs: "Delivers the flow; more surface."},
				{Label: "Ship a stub", Tradeoffs: "Less code; defers the flow."},
			},
			Recommendation: "Ship the importer",
		},
		{
			Question:    "What scope does the task cover?",
			WhyBlocking: "The specification cannot bound the work without its scope.",
			Options: []domain.DecisionOption{
				{Label: "Daemon only", Tradeoffs: "Narrow; no client change."},
				{Label: "Daemon and app", Tradeoffs: "Wider; couples two components."},
			},
			Recommendation: "Daemon only",
		},
		{
			Question:    "What is explicitly out of scope?",
			WhyBlocking: "The specification cannot pin non-goals without them.",
			Options: []domain.DecisionOption{
				{Label: "No new API field", Tradeoffs: "Guidance only; no contract."},
				{Label: "Add an API field", Tradeoffs: "Explicit; a contract change."},
			},
			Recommendation: "No new API field",
		},
	}
}

// TestSpecificationSketchRoundAsksThenSpecifiesUnderBudgetOfTwo pins the sketch
// clarification round (#1329): a first-turn decisions result asks about the
// task's outcome, scope, and non-goals before any specification, the operator's
// answer returns as human_feedback, and the answered retry produces the
// specification and its approval item under a budget that allows no research.
//
// Iteration budget: the first-turn question is iteration 1 and the answered
// retry is iteration 2, so a budget of 2 covers the whole round without an
// iterations-exhausted failure. TestSpecificationNeedsDecisionAtIterationLimitRecordsFailure
// pins the opposite end, where a budget of 1 records that failure instead of
// asking; neither the engine's iteration counting nor specification.go changes.
//
// This test scripts the fake's second output directly, so it cannot observe
// what makes the retry proceed rather than re-ask: that the specifier's
// first-turn/resume rule fires only with no answering human_feedback. That
// prompt-driven exit is pinned by the "with no answering `human_feedback`"
// required phrase in exec/claude's TestPhase1ASummaryPromptContracts.
func TestSpecificationSketchRoundAsksThenSpecifiesUnderBudgetOfTwo(t *testing.T) {
	t.Run("answer_and_retry", func(t *testing.T) {
		f := newSpecificationFixture(t, true, 2)
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
		if err := specifyfake.Script(driver, firstID, 0, 0, specify.Output{
			Decisions: sketchDecisionsFixture(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := specifyfake.Script(driver, secondID, 0, 0, specify.Output{Specification: &specify.Specification{
			Summary: "The implementation plan is ready.", Body: "# Specification\n\nShip the importer, daemon only.",
			Addressals: []specify.Addressal{},
		}}); err != nil {
			t.Fatal(err)
		}
		f.submit(t)
		engine := f.newEngine(t, capturing)

		// First turn: the sketch produces exactly one clarification item, and no
		// specification artifact or approval item exists before the answer.
		if result, err := engine.Reconcile(t.Context()); err != nil || result.ResultsAccepted != 1 {
			t.Fatalf("first reconcile = %+v, %v", result, err)
		}
		if items := f.questionItemsForRun(t, "specification-run"); len(items) != 1 {
			t.Fatalf("agent_question items = %d, want 1", len(items))
		}
		approvalID := domain.ItemID("spec-approval-implementation-run-2")
		f.assertItemAbsent(t, approvalID)
		f.assertArtifactAbsent(t, "spec-implementation-run-2")

		// The operator's answer returns as human_feedback on the second turn.
		questionID := domain.ItemID("question-" + string(firstID))
		questionItem, _ := f.item(t, questionID)
		const answer = "Ship the importer, daemon only, no new API field."
		f.answerQuestion(t, questionID, "answer-sketch", answer)
		if created, err := engine.reconcileOperatorFeedback(t.Context()); err != nil || created != 1 {
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
		answerArtifact := f.artifact(t, "answer-answer-sketch")
		answerBody, err := json.Marshal(specificationAnswerInput{
			Version:  specificationAnswerInputVersion,
			Question: *questionItem.AgentQuestion,
			Answer:   answer,
		})
		if err != nil {
			t.Fatal(err)
		}
		assertSpecificationPriorArtifacts(t, prompt, []expectedSpecificationPriorArtifact{{
			role: "human_feedback", digest: answerArtifact.Digest, body: string(answerBody),
		}})

		// The answered retry yields the specification and opens its approval
		// item; the spec-approval gate holds implementation, and no
		// iterations-exhausted failure is recorded for either invocation.
		item, _ := f.item(t, approvalID)
		if item.Type != domain.AttentionSpecApproval || item.Status != domain.StatusOpen {
			t.Fatalf("approval item = %#v, want an open spec_approval", item)
		}
		f.assertItemAbsent(t, domain.ItemID("execution-failure-"+string(firstID)))
		f.assertItemAbsent(t, domain.ItemID("execution-failure-"+string(secondID)))
		if _, err := f.run("implementation-run"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("implementation run before approval = %v, want ErrNotFound", err)
		}
	})

	t.Run("stop", func(t *testing.T) {
		f := newSpecificationFixture(t, true, 2)
		driver := f.newDriver(t)
		firstID := specificationInvocationID("specification-run", 1)
		if err := specifyfake.Script(driver, firstID, 0, 0, specify.Output{
			Decisions: sketchDecisionsFixture(),
		}); err != nil {
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
			CommandID: "stop-sketch", DeviceID: "device-1", ExpectedEntityVersion: snapshot.EntityVersion,
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
		f.assertItemAbsent(t, domain.ItemID("spec-approval-implementation-run-2"))
		if _, err := f.run("implementation-run"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("implementation run after stop = %v, want ErrNotFound", err)
		}
	})
}

// taskName reads the specification run's task name, so a test can prove the
// rename that used to trip answer replay (#1556) really happened.
func (f specificationFixture) taskName(t *testing.T) domain.DisplayName {
	t.Helper()
	var name domain.DisplayName
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		run, err := tx.GetRun(t.Context(), "specification-run")
		if err != nil {
			return err
		}
		task, err := tx.GetTask(t.Context(), run.TaskID)
		name = task.Name
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return name
}

// assertAnswerReplayConverges runs the passes a daemon repeats after a
// consumed answer: none may fail, since a reconcile error is a durable stop,
// and none may create anything.
func assertAnswerReplayConverges(t *testing.T, engine *Engine, step string) {
	t.Helper()
	for pass := 1; pass <= 2; pass++ {
		if created, err := engine.reconcileOperatorFeedback(t.Context()); err != nil || created != 0 {
			t.Fatalf("%s: operator feedback pass %d = %d, %v", step, pass, created, err)
		}
		if _, err := engine.Reconcile(t.Context()); err != nil {
			t.Fatalf("%s: reconcile pass %d: %v", step, pass, err)
		}
	}
}

// answerRounds scripts one decisions turn per answer, answers each, and ends
// on the scripted specification, running the field sequence of #1556.
func answerRounds(t *testing.T, f specificationFixture, engine *Engine, answers int) {
	t.Helper()
	if result, err := engine.Reconcile(t.Context()); err != nil || result.ResultsAccepted != 1 {
		t.Fatalf("first reconcile = %+v, %v", result, err)
	}
	for round := 1; round <= answers; round++ {
		questionID := domain.ItemID("question-" + string(specificationInvocationID("specification-run", round)))
		f.answerQuestion(t, questionID, fmt.Sprintf("answer-%d", round), "Ship the importer, daemon only.")
		if created, err := engine.reconcileOperatorFeedback(t.Context()); err != nil || created != 1 {
			t.Fatalf("answer %d: reconcileOperatorFeedback = %d, %v", round, created, err)
		}
		if result, err := engine.Reconcile(t.Context()); err != nil || result.ResultsAccepted != 1 {
			t.Fatalf("answer %d: reconcile = %+v, %v", round, result, err)
		}
	}
}

// TestSpecificationAnswerReplaySurvivesTaskRename pins #1556: once a
// specification renames the task, every later pass still recognizes the
// consumed answers instead of rejecting them with ErrParentKeyMismatch,
// which escaped Reconcile as a durable stop.
func TestSpecificationAnswerReplaySurvivesTaskRename(t *testing.T) {
	const title = "Import the sketch"
	for _, tc := range []struct {
		name         string
		specApproval bool
		answers      int
		title        *string
		wantName     domain.DisplayName
	}{
		{
			name: "spec_approval titled one answer", specApproval: true, answers: 1, title: new(title),
			wantName: domain.DisplayName{Text: title, Source: domain.DisplayNameSourceAgent},
		},
		{
			name: "spec_approval titled two answers", specApproval: true, answers: 2, title: new(title),
			wantName: domain.DisplayName{Text: title, Source: domain.DisplayNameSourceAgent},
		},
		{
			name: "auto-approval heading one answer", specApproval: false, answers: 1,
			wantName: domain.DisplayName{Text: "Sketch Importer", Source: domain.DisplayNameSourceSpecification},
		},
		{
			name: "auto-approval heading two answers", specApproval: false, answers: 2,
			wantName: domain.DisplayName{Text: "Sketch Importer", Source: domain.DisplayNameSourceSpecification},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSpecificationFixture(t, tc.specApproval, 4)
			driver := f.newDriver(t)
			for round := 1; round <= tc.answers; round++ {
				if err := specifyfake.Script(driver, specificationInvocationID("specification-run", round), 0, 0,
					specify.Output{Decisions: sketchDecisionsFixture()}); err != nil {
					t.Fatal(err)
				}
			}
			if err := specifyfake.Script(driver, specificationInvocationID("specification-run", tc.answers+1), 0, 0,
				specify.Output{Specification: &specify.Specification{
					Title: tc.title, Summary: "The implementation plan is ready.",
					Body: "# Sketch Importer\n\nShip the importer, daemon only.", Addressals: []specify.Addressal{},
				}}); err != nil {
				t.Fatal(err)
			}
			f.submit(t)
			engine := f.newEngine(t, driver)
			answerRounds(t, f, engine, tc.answers)
			if tc.specApproval {
				approvalID := domain.ItemID(fmt.Sprintf("spec-approval-implementation-run-%d", tc.answers+1))
				if item, _ := f.item(t, approvalID); item.Status != domain.StatusOpen {
					t.Fatalf("approval item status = %s, want open", item.Status)
				}
			} else if _, err := f.run("implementation-run"); err != nil {
				t.Fatalf("auto-approved implementation = %v", err)
			}
			if got := f.taskName(t); got != tc.wantName {
				t.Fatalf("task name = %+v, want %+v", got, tc.wantName)
			}
			assertAnswerReplayConverges(t, engine, "after the specification")
		})
	}
}

// TestSpecificationAnswerReplaySurvivesApprovalRevision carries a consumed
// answer through request_changes, a revised specification, and approval
// (#1556), replaying reconcile after each step; the approval renames the task
// again from the approved heading.
func TestSpecificationAnswerReplaySurvivesApprovalRevision(t *testing.T) {
	f := newSpecificationFixture(t, true, 4)
	driver := f.newDriver(t)
	if err := specifyfake.Script(driver, specificationInvocationID("specification-run", 1), 0, 0,
		specify.Output{Decisions: sketchDecisionsFixture()}); err != nil {
		t.Fatal(err)
	}
	if err := specifyfake.Script(driver, specificationInvocationID("specification-run", 2), 0, 0,
		specify.Output{Specification: &specify.Specification{
			Title: new("Import the sketch"), Summary: "The implementation plan is ready.",
			Body: "# Sketch Importer\n\nShip the importer, daemon only.", Addressals: []specify.Addressal{},
		}}); err != nil {
		t.Fatal(err)
	}
	if err := specifyfake.Script(driver, specificationInvocationID("specification-run", 3), 0, 0,
		specify.Output{Specification: &specify.Specification{
			Summary: "The revised implementation plan is ready.",
			Body:    "# Approved Specification\n\nShip the importer and name its replay invariant.",
			Addressals: []specify.Addressal{{
				CommentID: "revise-answered-spec", Response: "Named the replay invariant.",
			}},
		}}); err != nil {
		t.Fatal(err)
	}
	f.submit(t)
	engine := f.newEngine(t, driver)
	answerRounds(t, f, engine, 1)
	assertAnswerReplayConverges(t, engine, "after the titled specification")

	approval, snapshot := f.item(t, "spec-approval-implementation-run-2")
	if _, err := f.signet.Submit(t.Context(), signet.ClientCommand{
		CommandID: "revise-answered-spec", DeviceID: "device-1", ExpectedEntityVersion: snapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: approval.ID, Action: domain.ActionRequestChanges, ItemVersion: approval.ItemVersion,
			ArtifactDigests: approval.ArtifactDigests, Message: "Name the replay invariant.",
		},
	}); err != nil {
		t.Fatal(err)
	}
	assertAnswerReplayConverges(t, engine, "after request_changes")

	revised, revisedSnapshot := f.item(t, "spec-approval-implementation-run-3")
	if revised.Status != domain.StatusOpen {
		t.Fatalf("revised approval status = %s, want open", revised.Status)
	}
	if _, err := f.signet.Submit(t.Context(), signet.ClientCommand{
		CommandID: "approve-answered-spec", DeviceID: "device-1", ExpectedEntityVersion: revisedSnapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: revised.ID, Action: domain.ActionApprove, ItemVersion: revised.ItemVersion,
			ArtifactDigests: revised.ArtifactDigests,
		},
	}); err != nil {
		t.Fatal(err)
	}
	assertAnswerReplayConverges(t, engine, "after approval")
	if _, err := f.run("implementation-run"); err != nil {
		t.Fatalf("approved implementation = %v", err)
	}
	if got := f.taskName(t); got.Text != "Approved Specification" || got.Source != domain.DisplayNameSourceSpecification {
		t.Fatalf("task name after approval = %+v", got)
	}
}
