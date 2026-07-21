package integration_test

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const (
	testRunID     domain.RunID     = "run-235"
	testProjectID domain.ProjectID = "project-235"
	deviceA       domain.DeviceID  = "device-a"
	deviceB       domain.DeviceID  = "device-b"
)

type workflowFixture struct {
	root   string
	store  *store.Store
	signet *signet.Service
	driver *fake.StageDriver
	engine *engine.Engine
}

func openWorkflowFixture(t *testing.T, root string) *workflowFixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(root, "freeside.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	blobs, err := signet.NewBlobStore(filepath.Join(root, "blobs"))
	if err != nil {
		_ = st.Close()
		t.Fatalf("signet.NewBlobStore: %v", err)
	}
	attention := signet.NewService(st, signet.WithBlobStore(blobs))
	driver, err := fake.NewStageDriverAt(filepath.Join(root, "driver"))
	if err != nil {
		_ = st.Close()
		t.Fatalf("fake.NewStageDriverAt: %v", err)
	}
	workflow, err := engine.New(st, attention, driver)
	if err != nil {
		_ = st.Close()
		t.Fatalf("engine.New: %v", err)
	}
	f := &workflowFixture{root: root, store: st, signet: attention, driver: driver, engine: workflow}
	t.Cleanup(func() {
		if f.store != nil {
			if err := f.store.Close(); err != nil {
				t.Errorf("store.Close: %v", err)
			}
		}
	})
	return f
}

func (f *workflowFixture) close(t *testing.T) {
	t.Helper()
	if err := f.store.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}
	f.store = nil
}

func (f *workflowFixture) seed(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.engine.StartFakeRun(ctx, engine.FakeRunSpec{
		RunID: testRunID, ProjectID: testProjectID,
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}); err != nil {
		t.Fatalf("StartFakeRun: %v", err)
	}
	pairedAt := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		for _, id := range []domain.DeviceID{deviceA, deviceB} {
			if err := tx.PutDevice(ctx, domain.Device{
				ID: id, DisplayName: string(id), Status: domain.DeviceActive, PairedAt: pairedAt,
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed devices: %v", err)
	}
}

func (f *workflowFixture) approve(t *testing.T) signet.CommandResult {
	t.Helper()
	ctx := context.Background()
	item, err := f.signet.GetAttentionItem(ctx, domain.ItemID("approval-"+string(testRunID)))
	if err != nil {
		t.Fatalf("get approval: %v", err)
	}
	command := signet.ClientCommand{
		CommandID: "approve-235", DeviceID: deviceA, ExpectedEntityVersion: item.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: item.Item.ID, Action: domain.ActionApprove, ItemVersion: item.Item.ItemVersion,
			PRHeadSHA: item.Item.PRHeadSHA, ArtifactDigests: item.Item.ArtifactDigests,
		},
	}
	result, err := f.signet.Submit(ctx, command)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	replayed, err := f.signet.Submit(ctx, command)
	if err != nil {
		t.Fatalf("approve replay: %v", err)
	}
	if !reflect.DeepEqual(replayed, result) {
		t.Fatalf("approve replay = %#v, want %#v", replayed, result)
	}
	return result
}

func (f *workflowFixture) openFeedback(t *testing.T) signet.AttentionItemSnapshot {
	t.Helper()
	if _, err := f.engine.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile after approval: %v", err)
	}
	item, err := f.signet.GetAttentionItem(context.Background(), domain.ItemID("feedback-"+string(testRunID)))
	if err != nil {
		t.Fatalf("get feedback: %v", err)
	}
	return item
}

func (f *workflowFixture) discuss(t *testing.T, item signet.AttentionItemSnapshot) domain.InvocationID {
	t.Helper()
	commandID := "discuss-235"
	_, err := f.signet.Submit(context.Background(), signet.ClientCommand{
		CommandID: commandID, DeviceID: deviceA, ExpectedEntityVersion: item.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: item.Item.ID, Action: domain.ActionDiscuss, ItemVersion: item.Item.ItemVersion,
			PRHeadSHA: item.Item.PRHeadSHA, ArtifactDigests: item.Item.ArtifactDigests,
			Message: "Why is this the next workflow state?",
		},
	})
	if err != nil {
		t.Fatalf("discuss: %v", err)
	}
	return domain.InvocationID("inv-" + commandID)
}

func (f *workflowFixture) scriptCompletion(id domain.InvocationID, outcome fake.Outcome) {
	f.driver.Script(id, fake.StageScript{
		Outcome: outcome,
		Result:  exec.StageResult{Summary: "The accepted feedback advances the fake workflow."},
	})
}

func (f *workflowFixture) recordDispatchedAttempt(t *testing.T, invocationID domain.InvocationID) {
	f.recordDispatchedAttemptWithID(t, invocationID, "attempt-"+domain.AttemptID(invocationID))
}

func (f *workflowFixture) recordDispatchedAttemptWithID(
	t *testing.T,
	invocationID domain.InvocationID,
	attemptID domain.AttemptID,
) {
	t.Helper()
	ctx := context.Background()
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		run, err := tx.GetRun(ctx, testRunID)
		if err != nil {
			return err
		}
		if len(run.Stages) != 1 {
			return errors.New("fake run does not have exactly one stage")
		}
		stage := run.Stages[0]
		stage.Attempts = append(stage.Attempts, domain.Attempt{
			ID: attemptID, StageID: stage.ID,
			Number: len(stage.Attempts) + 1, InvocationID: invocationID,
		})
		run.Stages[0] = stage
		return tx.PutRun(ctx, run)
	}); err != nil {
		t.Fatalf("record dispatched attempt: %v", err)
	}

	var pending []store.QueueEntry
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		pending, err = tx.ListPendingOutbox(ctx, "agent_invocation_requested")
		return err
	}); err != nil {
		t.Fatalf("list pending invocation: %v", err)
	}
	if len(pending) != 1 || pending[0].IdempotencyKey != string(invocationID) {
		t.Fatalf("pending invocation = %#v, want %q", pending, invocationID)
	}
	if err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.MarkOutboxDispatched(ctx, pending[0].IdempotencyKey)
	}); err != nil {
		t.Fatalf("mark invocation dispatched: %v", err)
	}
}

func assertCompletedOnce(t *testing.T, f *workflowFixture, invocationID domain.InvocationID) {
	t.Helper()
	ctx := context.Background()
	run, err := f.signet.GetRun(ctx, testRunID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if len(run.Run.Stages) != 1 || len(run.Run.Stages[0].Attempts) != 1 {
		t.Fatalf("run stages = %#v, want one stage with one attempt", run.Run.Stages)
	}
	if run.Run.Stages[0].Attempts[0].InvocationID != invocationID {
		t.Fatalf("attempt invocation = %q, want %q", run.Run.Stages[0].Attempts[0].InvocationID, invocationID)
	}
	feedback, err := f.signet.GetAttentionItem(ctx, domain.ItemID("feedback-"+string(testRunID)))
	if err != nil {
		t.Fatalf("get feedback: %v", err)
	}
	if feedback.Item.ConversationID == nil {
		t.Fatal("feedback has no conversation")
	}
	conversation, err := f.signet.GetConversation(ctx, *feedback.Item.ConversationID)
	if err != nil {
		t.Fatalf("get conversation: %v", err)
	}
	if len(conversation.Conversation.Messages) != 2 || conversation.Conversation.Status != domain.ConversationIdle {
		t.Fatalf("conversation = %#v, want two messages and idle", conversation.Conversation)
	}

	before, err := f.store.ServerState(ctx)
	if err != nil {
		t.Fatalf("ServerState before replay: %v", err)
	}
	result, err := f.engine.Reconcile(ctx)
	if err != nil {
		t.Fatalf("idempotent Reconcile: %v", err)
	}
	if result != (engine.ReconcileResult{}) {
		t.Fatalf("idempotent Reconcile result = %#v, want zero", result)
	}
	after, err := f.store.ServerState(ctx)
	if err != nil {
		t.Fatalf("ServerState after replay: %v", err)
	}
	if after != before {
		t.Fatalf("idempotent Reconcile moved server state %#v -> %#v", before, after)
	}
}

// TestWorkflowEngineFakeFlow is the 1A.0 walking skeleton: one device accepts
// the run, a second device refetches the same resolution, conversation feedback
// commits an invocation, and the fake result advances the durable run once.
func TestWorkflowEngineFakeFlow(t *testing.T) {
	f := openWorkflowFixture(t, t.TempDir())
	f.seed(t)
	ctx := context.Background()

	deviceBView, err := f.signet.GetAttentionItem(ctx, domain.ItemID("approval-"+string(testRunID)))
	if err != nil {
		t.Fatalf("device B initial read: %v", err)
	}
	decision := f.approve(t)
	converged, err := f.signet.GetAttentionItem(ctx, deviceBView.Item.ID)
	if err != nil {
		t.Fatalf("device B convergence read: %v", err)
	}
	if converged.Item.Status != domain.StatusResolved || converged.AsOfRevision != decision.Revision {
		t.Fatalf("device B converged snapshot = %#v, decision revision = %d", converged, decision.Revision)
	}

	feedback := f.openFeedback(t)
	invocationID := domain.InvocationID("inv-discuss-235")
	f.scriptCompletion(invocationID, fake.OutcomeComplete)
	if got := f.discuss(t, feedback); got != invocationID {
		t.Fatalf("discussion invocation = %q, want %q", got, invocationID)
	}
	result, err := f.engine.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile invocation: %v", err)
	}
	if result.InvocationsStarted != 1 || result.ResultsAccepted != 1 {
		t.Fatalf("Reconcile result = %#v, want one start and one acceptance", result)
	}
	assertCompletedOnce(t, f, invocationID)
}

// TestWorkflowEngineIgnoresUnmarkedRun prevents the concrete 1A.0 state
// machine from claiming every Run in a shared store. Only StartFakeRun's
// deterministic approval item marks a run as belonging to this workflow.
func TestWorkflowEngineIgnoresUnmarkedRun(t *testing.T) {
	f := openWorkflowFixture(t, t.TempDir())
	ctx := context.Background()
	other := domain.Run{
		ID: "run-other", ProjectID: "project-other",
		SpecDigest: "sha256:other-spec", PolicyDigest: "sha256:other-policy",
		Stages: []domain.Stage{{
			ID: "feedback-run-other", RunID: "run-other", Name: "conversation_feedback",
			Attempts: []domain.Attempt{{
				ID: "attempt-inv-other", StageID: "feedback-run-other",
				Number: 1, InvocationID: "inv-other",
			}},
		}},
	}
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error { return tx.PutRun(ctx, other) }); err != nil {
		t.Fatalf("put unrelated run: %v", err)
	}
	if result, err := f.engine.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	} else if result != (engine.ReconcileResult{}) {
		t.Fatalf("Reconcile result = %#v, want no work for unrelated run", result)
	}
	if _, err := f.signet.GetAttentionItem(ctx, "approval-run-other"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unrelated run acquired an approval item: %v", err)
	}
}

// TestWorkflowEngineRejectsUnmarkedInvocation prevents a different workflow
// from gaining authority by reproducing only this engine's feedback item and
// stage shape. Dispatch requires StartFakeRun's initial approval marker too.
func TestWorkflowEngineRejectsUnmarkedInvocation(t *testing.T) {
	f := openWorkflowFixture(t, t.TempDir())
	ctx := context.Background()
	runID := domain.RunID("run-lookalike")
	projectID := domain.ProjectID("project-lookalike")
	run := domain.Run{
		ID: runID, ProjectID: projectID,
		SpecDigest: "sha256:lookalike-spec", PolicyDigest: "sha256:lookalike-policy",
		Stages: []domain.Stage{{
			ID: "feedback-" + domain.StageID(runID), RunID: runID,
			Name: "conversation_feedback", Attempts: []domain.Attempt{},
		}},
	}
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		return tx.PutDevice(ctx, domain.Device{
			ID: deviceA, DisplayName: "Device A", Status: domain.DeviceActive,
			PairedAt: time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC),
		})
	}); err != nil {
		t.Fatalf("seed lookalike workflow: %v", err)
	}
	runBinding := runID
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "feedback-" + domain.ItemID(runID), ProjectID: projectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runBinding},
		Type:    domain.AttentionSpecApproval, Priority: domain.PriorityNormal,
		Reason:            "Discuss the approved fake run with the agent.",
		RequestedDecision: []domain.Action{domain.ActionDiscuss}, ItemVersion: 1,
		InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	if err := f.signet.PutItem(ctx, item); err != nil {
		t.Fatalf("put lookalike feedback item: %v", err)
	}
	snapshot, err := f.signet.GetAttentionItem(ctx, item.ID)
	if err != nil {
		t.Fatalf("get lookalike feedback item: %v", err)
	}
	if _, err := f.signet.Submit(ctx, signet.ClientCommand{
		CommandID: "discuss-lookalike", DeviceID: deviceA, ExpectedEntityVersion: snapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: item.ID, Action: domain.ActionDiscuss, ItemVersion: item.ItemVersion,
			Message: "Do not let the fake workflow claim this run.",
		},
	}); err != nil {
		t.Fatalf("discuss lookalike item: %v", err)
	}
	invocationID := domain.InvocationID("inv-discuss-lookalike")
	f.scriptCompletion(invocationID, fake.OutcomeComplete)
	if result, err := f.engine.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile unmarked invocation: %v", err)
	} else if result != (engine.ReconcileResult{}) {
		t.Fatalf("Reconcile result = %#v, want unowned invocation skipped", result)
	}
	stored, err := f.signet.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("get lookalike run: %v", err)
	}
	if len(stored.Run.Stages[0].Attempts) != 0 {
		t.Fatalf("unmarked invocation was recorded: %#v", stored.Run.Stages[0].Attempts)
	}
}

// TestWorkflowEngineRejectsMalformedAttemptIdentity proves reconstructed Run
// history cannot retarget a legitimate invocation by changing only the
// attempt's supposedly deterministic identity.
func TestWorkflowEngineRejectsMalformedAttemptIdentity(t *testing.T) {
	root := t.TempDir()
	f := openWorkflowFixture(t, root)
	f.seed(t)
	f.approve(t)
	feedback := f.openFeedback(t)
	invocationID := domain.InvocationID("inv-discuss-235")
	f.scriptCompletion(invocationID, fake.OutcomeCrashAfterResult)
	f.discuss(t, feedback)
	if err := f.driver.Start(context.Background(), invocationID, exec.StartSpec{
		RunID: testRunID, StageID: domain.StageID("feedback-" + string(testRunID)),
	}); err != nil {
		t.Fatalf("driver.Start: %v", err)
	}
	if _, err := f.driver.Inspect(context.Background(), invocationID); err != nil {
		t.Fatalf("driver.Inspect: %v", err)
	}
	f.recordDispatchedAttemptWithID(t, invocationID, "attempt-retargeted")
	f.close(t)

	restarted := openWorkflowFixture(t, root)
	if _, err := restarted.engine.Reconcile(context.Background()); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("Reconcile error = %v, want ErrParentKeyMismatch", err)
	}
}

// TestWorkflowEngineRecoversBeforeDispatch closes the durable store after the
// discuss transaction but before the engine starts the fake. Restart scans the
// pending outbox intent and completes the workflow once.
func TestWorkflowEngineRecoversBeforeDispatch(t *testing.T) {
	root := t.TempDir()
	f := openWorkflowFixture(t, root)
	f.seed(t)
	f.approve(t)
	feedback := f.openFeedback(t)
	invocationID := domain.InvocationID("inv-discuss-235")
	f.scriptCompletion(invocationID, fake.OutcomeComplete)
	f.discuss(t, feedback)
	f.close(t)

	restarted := openWorkflowFixture(t, root)
	result, err := restarted.engine.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("restart Reconcile: %v", err)
	}
	if result.InvocationsStarted != 1 || result.ResultsAccepted != 1 {
		t.Fatalf("restart result = %#v, want one start and one acceptance", result)
	}
	assertCompletedOnce(t, restarted, invocationID)
}

// TestWorkflowEngineRecoversCommittedResult closes the store after the fake
// commits a result but before local acceptance. Reconstructed fake state serves
// the result by invocation id; inbox acceptance and run advancement occur once.
func TestWorkflowEngineRecoversCommittedResult(t *testing.T) {
	root := t.TempDir()
	f := openWorkflowFixture(t, root)
	f.seed(t)
	f.approve(t)
	feedback := f.openFeedback(t)
	invocationID := domain.InvocationID("inv-discuss-235")
	f.scriptCompletion(invocationID, fake.OutcomeCrashAfterResult)
	f.discuss(t, feedback)

	if err := f.driver.Start(context.Background(), invocationID, exec.StartSpec{
		RunID: testRunID, StageID: domain.StageID("feedback-" + string(testRunID)),
	}); err != nil {
		t.Fatalf("driver.Start: %v", err)
	}
	status, err := f.driver.Inspect(context.Background(), invocationID)
	if err != nil {
		t.Fatalf("driver.Inspect: %v", err)
	}
	if status != exec.StatusGone {
		t.Fatalf("driver status = %q, want gone with a committed result", status)
	}
	f.close(t)

	restarted := openWorkflowFixture(t, root)
	result, err := restarted.engine.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("restart Reconcile: %v", err)
	}
	if result.InvocationsStarted != 0 || result.ResultsAccepted != 1 {
		t.Fatalf("restart result = %#v, want recovered acceptance without a new start", result)
	}
	assertCompletedOnce(t, restarted, invocationID)
}

// TestWorkflowEngineRecoversDispatchedResult reconstructs the exact durable
// state after Start and the dispatch marker commit but before local acceptance.
// The pending outbox is empty, so recovery must discover the committed fake
// result through the Run attempt index.
func TestWorkflowEngineRecoversDispatchedResult(t *testing.T) {
	root := t.TempDir()
	f := openWorkflowFixture(t, root)
	f.seed(t)
	f.approve(t)
	feedback := f.openFeedback(t)
	invocationID := domain.InvocationID("inv-discuss-235")
	f.scriptCompletion(invocationID, fake.OutcomeCrashAfterResult)
	f.discuss(t, feedback)

	if err := f.driver.Start(context.Background(), invocationID, exec.StartSpec{
		RunID: testRunID, StageID: domain.StageID("feedback-" + string(testRunID)),
	}); err != nil {
		t.Fatalf("driver.Start: %v", err)
	}
	status, err := f.driver.Inspect(context.Background(), invocationID)
	if err != nil {
		t.Fatalf("driver.Inspect: %v", err)
	}
	if status != exec.StatusGone {
		t.Fatalf("driver status = %q, want gone with a committed result", status)
	}
	f.recordDispatchedAttempt(t, invocationID)
	f.close(t)

	restarted := openWorkflowFixture(t, root)
	result, err := restarted.engine.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("restart Reconcile: %v", err)
	}
	if result.InvocationsStarted != 0 || result.ResultsAccepted != 1 {
		t.Fatalf("restart result = %#v, want attempt-indexed acceptance without dispatch", result)
	}
	assertCompletedOnce(t, restarted, invocationID)
}

type foreignResultDriver struct{}

func (foreignResultDriver) Start(context.Context, domain.InvocationID, exec.StartSpec) error {
	return nil
}

func (foreignResultDriver) Inspect(context.Context, domain.InvocationID) (exec.Status, error) {
	return exec.StatusCompleted, nil
}

func (foreignResultDriver) Stream(context.Context, domain.InvocationID) (io.ReadCloser, error) {
	return nil, errors.New("not used")
}
func (foreignResultDriver) Cancel(context.Context, domain.InvocationID) error { return nil }
func (foreignResultDriver) Collect(context.Context, domain.InvocationID) (exec.StageResult, error) {
	return exec.StageResult{
		InvocationID: "inv-foreign", Status: exec.StatusCompleted, Summary: "foreign result",
	}, nil
}

// TestWorkflowEngineRejectsForeignDriverResult is the returned-object trust
// boundary: a driver cannot advance this run by returning another invocation's
// otherwise-valid result. The legitimate invocation intent stays recorded,
// but no agent message or completion transition is accepted.
func TestWorkflowEngineRejectsForeignDriverResult(t *testing.T) {
	f := openWorkflowFixture(t, t.TempDir())
	f.seed(t)
	f.approve(t)
	feedback := f.openFeedback(t)
	f.discuss(t, feedback)

	workflow, err := engine.New(f.store, f.signet, foreignResultDriver{})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	_, err = workflow.Reconcile(context.Background())
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("Reconcile error = %v, want ErrParentKeyMismatch", err)
	}
	run, getErr := f.signet.GetRun(context.Background(), testRunID)
	if getErr != nil {
		t.Fatalf("get run: %v", getErr)
	}
	if len(run.Run.Stages) != 1 || len(run.Run.Stages[0].Attempts) != 1 {
		t.Fatalf("legitimate invocation intent was not recorded once: %#v", run.Run)
	}
	item, getErr := f.signet.GetAttentionItem(context.Background(), feedback.Item.ID)
	if getErr != nil {
		t.Fatalf("get feedback: %v", getErr)
	}
	if item.Item.ItemVersion != 2 || item.Item.ConversationID == nil {
		t.Fatalf("foreign result advanced feedback item: %#v", item.Item)
	}
	conversation, getErr := f.signet.GetConversation(context.Background(), *item.Item.ConversationID)
	if getErr != nil {
		t.Fatalf("get conversation: %v", getErr)
	}
	if len(conversation.Conversation.Messages) != 1 ||
		conversation.Conversation.Status != domain.ConversationAwaitingAgent {
		t.Fatalf("foreign result entered conversation: %#v", conversation.Conversation)
	}
}

// TestWorkflowEngineDoesNotAcceptFailedResult proves a terminal driver failure
// cannot be rendered as an agent answer. The attempt stays durable for later
// failure policy, while the conversation and item remain unadvanced.
func TestWorkflowEngineDoesNotAcceptFailedResult(t *testing.T) {
	f := openWorkflowFixture(t, t.TempDir())
	f.seed(t)
	f.approve(t)
	feedback := f.openFeedback(t)
	invocationID := domain.InvocationID("inv-discuss-235")
	f.scriptCompletion(invocationID, fake.OutcomeFail)
	f.discuss(t, feedback)

	_, err := f.engine.Reconcile(context.Background())
	if !errors.Is(err, engine.ErrInvocationUnsuccessful) {
		t.Fatalf("Reconcile error = %v, want ErrInvocationUnsuccessful", err)
	}
	run, getErr := f.signet.GetRun(context.Background(), testRunID)
	if getErr != nil {
		t.Fatalf("get run: %v", getErr)
	}
	if len(run.Run.Stages) != 1 || len(run.Run.Stages[0].Attempts) != 1 {
		t.Fatalf("failed invocation intent was not recorded once: %#v", run.Run)
	}
	item, getErr := f.signet.GetAttentionItem(context.Background(), feedback.Item.ID)
	if getErr != nil {
		t.Fatalf("get feedback: %v", getErr)
	}
	conversation, getErr := f.signet.GetConversation(context.Background(), *item.Item.ConversationID)
	if getErr != nil {
		t.Fatalf("get conversation: %v", getErr)
	}
	if item.Item.ItemVersion != 2 || len(conversation.Conversation.Messages) != 1 ||
		conversation.Conversation.Status != domain.ConversationAwaitingAgent {
		t.Fatalf("failed result advanced workflow: item=%#v conversation=%#v",
			item.Item, conversation.Conversation)
	}
}

// TestWorkflowEngineKeepsPriorAttemptsAccepted exercises a second discuss
// round. The append-only Run history must not let the older accepted attempt
// block reconciliation merely because the conversation is awaiting a newer
// invocation.
func TestWorkflowEngineKeepsPriorAttemptsAccepted(t *testing.T) {
	f := openWorkflowFixture(t, t.TempDir())
	f.seed(t)
	f.approve(t)
	feedback := f.openFeedback(t)
	firstID := domain.InvocationID("inv-discuss-235")
	f.scriptCompletion(firstID, fake.OutcomeComplete)
	f.discuss(t, feedback)
	if _, err := f.engine.Reconcile(context.Background()); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}

	feedback, err := f.signet.GetAttentionItem(context.Background(), feedback.Item.ID)
	if err != nil {
		t.Fatalf("get replacement feedback: %v", err)
	}
	secondID := domain.InvocationID("inv-discuss-236")
	f.scriptCompletion(secondID, fake.OutcomeComplete)
	_, err = f.signet.Submit(context.Background(), signet.ClientCommand{
		CommandID: "discuss-236", DeviceID: deviceA, ExpectedEntityVersion: feedback.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: feedback.Item.ID, Action: domain.ActionDiscuss, ItemVersion: feedback.Item.ItemVersion,
			PRHeadSHA: feedback.Item.PRHeadSHA, ArtifactDigests: feedback.Item.ArtifactDigests,
			Message: "One more question before the next state.",
		},
	})
	if err != nil {
		t.Fatalf("second discuss: %v", err)
	}
	result, err := f.engine.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if result.InvocationsStarted != 1 || result.ResultsAccepted != 1 {
		t.Fatalf("second Reconcile result = %#v", result)
	}
	run, err := f.signet.GetRun(context.Background(), testRunID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if len(run.Run.Stages[0].Attempts) != 2 ||
		run.Run.Stages[0].Attempts[1].InvocationID != secondID {
		t.Fatalf("run attempts = %#v, want two ordered attempts", run.Run.Stages[0].Attempts)
	}
	feedback, err = f.signet.GetAttentionItem(context.Background(), feedback.Item.ID)
	if err != nil {
		t.Fatalf("get completed feedback: %v", err)
	}
	conversation, err := f.signet.GetConversation(context.Background(), *feedback.Item.ConversationID)
	if err != nil {
		t.Fatalf("get conversation: %v", err)
	}
	if len(conversation.Conversation.Messages) != 4 ||
		conversation.Conversation.Status != domain.ConversationIdle {
		t.Fatalf("conversation = %#v, want two user/agent rounds", conversation.Conversation)
	}
}
