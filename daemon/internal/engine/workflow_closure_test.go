package engine

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/advisory"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	execfake "github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	inferencefake "github.com/freeside-ai/freeside/daemon/internal/inference/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

type workflowClosurePreparationStore struct {
	ArtifactStore
	digest  domain.Digest
	entered chan struct{}
	release chan struct{}
}

func (s *workflowClosurePreparationStore) Open(digest domain.Digest) (io.ReadCloser, error) {
	if digest == s.digest {
		close(s.entered)
		<-s.release
	}
	return s.ArtifactStore.Open(digest)
}

func TestWorkflowClosureDuringAdjudicationPreparationPreventsProviderDispatch(t *testing.T) {
	f := newFindingAdjudicationFixture(t, domain.FindingSeverityP2,
		&domain.FindingLocation{Path: "daemon/a.go", StartLine: 1, EndLine: 1}, "low", "high")
	f.writePath(t, f.headRoot)
	if err := f.store.Write(f.ctx, func(tx *store.WriteTx) error {
		return tx.RecordTaskStart(f.ctx, f.task.RunID, time.Now().UTC())
	}); err != nil {
		t.Fatal(err)
	}
	ownedDriver, err := ward.NewTaskJudgments(filepath.Join(t.TempDir(), "judgments"), f.driver)
	if err != nil {
		t.Fatal(err)
	}
	limits := inference.Limits{Calls: 10, ComputeUnits: 100_000, AttentionItems: 10, Starvation: time.Hour}
	advisoryStore, err := advisory.Open(filepath.Join(t.TempDir(), "advisory.json"), 20, 16<<10)
	if err != nil {
		t.Fatal(err)
	}
	client, err := inference.New(inference.Config{
		Advisory:  advisoryStore,
		StatePath: filepath.Join(t.TempDir(), "inference.json"),
		Roles:     inferencefake.Roles{Provider: "fake", Model: "test", Driver: ownedDriver},
		Sites: []inference.Site{inference.AdjudicatorSite(inference.Budget{
			Window: time.Hour, Site: limits, Project: limits, Global: limits,
			MaxCallsPerRoot: 10, MaxStarvationPerRoot: time.Hour,
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	preparation := &workflowClosurePreparationStore{
		ArtifactStore: f.artifacts, digest: f.binding.run.SpecDigest,
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	e := &Engine{store: f.store}
	f.workflow.findingAdjudicator = &productionFindingAdjudicator{
		client: client, store: f.store, artifacts: preparation, beginTaskWork: e.BeginRunWork,
		claimDispatch: e.claimRunDispatch,
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.workflow.reconcileFindingAdjudication(
			f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot)
		done <- err
	}()
	select {
	case <-preparation.entered:
	case err := <-done:
		t.Fatalf("adjudication did not reach preparation read: %v", err)
	}
	// BeginRunWork has succeeded, but the provider has not been entered.
	// Closure must return promptly without canceling existing work contexts.
	closeWorkflowThroughEngineForTest(t, e, f.task.RunID)
	close(preparation.release)
	if err := <-done; !errors.Is(err, store.ErrTaskWorkflowClosed) {
		t.Fatalf("preparation refusal = %v, want workflow closure", err)
	}
	if got := len(f.driver.Requests()); got != 0 {
		t.Fatalf("provider dispatched %d call(s) after workflow closure during preparation", got)
	}
}

func TestWorkflowClosureDuringDiscussionPreparationDoesNotInventFallbackReply(t *testing.T) {
	driver := inferencefake.New()
	f := newAttentionDiscussionFixture(t, domain.AttentionExecutionFailure, driver)
	f.submit(t)
	e := f.engine(t, true)
	var entry store.QueueEntry
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetOutbox(t.Context(), "inv-discuss-1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	discussion, err := e.loadAttentionDiscussion(t.Context(), entry)
	if err != nil || discussion == nil {
		t.Fatalf("prepare discussion: %+v, %v", discussion, err)
	}
	// Engine preparation may finish before closure. The provider's private
	// role preparation is after a winning claim and is not a refusal boundary.
	closeWorkflowForTest(t, f.store, "run-discussion", false)
	if _, err := e.produceAttentionDiscussionReply(t.Context(), *discussion); !errors.Is(err, store.ErrTaskWorkflowClosed) {
		t.Fatalf("discussion dispatch refusal = %v", err)
	}
	if _, err := e.reconcileAttentionDiscussions(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := len(driver.Requests()); got != 0 {
		t.Fatalf("closed discussion dispatched %d calls", got)
	}
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		conversation, err := tx.GetConversation(t.Context(), *discussion.item.ConversationID)
		if err != nil {
			return err
		}
		if len(conversation.Messages) != 1 || conversation.Status != domain.ConversationIdle {
			t.Fatalf("dispatch refusal invented history or stayed pending: %+v", conversation)
		}
		intent, err := tx.GetOutbox(t.Context(), entry.IdempotencyKey)
		if err == nil && !intent.Dispatched() {
			t.Fatal("closed discussion intent remains pending")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWorkflowClosureAfterDiscussionClaimAllowsPrivatePreparation(t *testing.T) {
	driver := inferencefake.New()
	driver.Script(inference.AttentionDiscussionSiteID, inferencefake.Script{
		Response: inference.Response{Output: []byte(`{"reply":"The claimed reply is history."}`), ComputeUnits: 1},
	})
	ownedDriver, err := ward.NewTaskJudgments(filepath.Join(t.TempDir(), "judgments"), driver)
	if err != nil {
		t.Fatal(err)
	}
	f := newAttentionDiscussionFixture(t, domain.AttentionExecutionFailure, ownedDriver)
	f.submit(t)
	entered, release := make(chan struct{}), make(chan struct{})
	armed := false
	limits := inference.Limits{Calls: 20, ComputeUnits: 100_000, AttentionItems: 20, Starvation: time.Hour}
	f.inference, err = inference.New(inference.Config{
		StatePath: filepath.Join(t.TempDir(), "inference.json"),
		Advisory:  f.advisory, Now: func() time.Time { return f.now },
		Roles: inferencefake.Roles{
			Provider: "fake", Model: "test", Driver: ownedDriver,
			Edit: func(role domain.RoleName, _ *inference.RoleCall) error {
				if armed && role == domain.RoleAttentionDiscussion {
					close(entered)
					<-release
				}
				return nil
			},
		},
		Sites: []inference.Site{inference.DiscussionSite(inference.Budget{
			Window: time.Hour, Site: limits, Project: limits, Global: limits,
			MaxCallsPerRoot: 20, MaxStarvationPerRoot: time.Hour,
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	e := f.engine(t, true)
	armed = true
	done := make(chan error, 1)
	go func() {
		_, err := e.reconcileAttentionDiscussions(t.Context())
		done <- err
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("discussion did not reach role preparation: %v", err)
	}
	closeWorkflowThroughEngineForTest(t, e, "run-discussion")
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := len(driver.Requests()); got != 1 {
		t.Fatalf("winning claim dispatched %d calls, want one", got)
	}
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		item, err := tx.GetAttentionItem(t.Context(), f.item.ID)
		if err != nil {
			return err
		}
		conversation, err := tx.GetConversation(t.Context(), *item.ConversationID)
		if err != nil {
			return err
		}
		if len(conversation.Messages) != 2 || conversation.Messages[1].Body != "The claimed reply is history." {
			t.Fatalf("winning dispatch lost its real reply: %+v", conversation.Messages)
		}
		intent, err := tx.GetOutbox(t.Context(), "inv-discuss-1")
		if err == nil && !intent.Dispatched() {
			t.Fatal("closed discussion intent remains pending")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func closeWorkflowThroughEngineForTest(t *testing.T, e *Engine, runID domain.RunID) {
	t.Helper()
	var run domain.Run
	if err := e.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		run, err = tx.GetRun(t.Context(), runID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.CommitTaskWorkflowClosure(t.Context(), run.TaskID, func() error {
		closeWorkflowForTest(t, e.store, runID, false)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWorkflowClosureAfterStageClaimDoesNotRevokeLaunch(t *testing.T) {
	f := newAttentionDiscussionFixture(t, domain.AttentionExecutionFailure, nil)
	e := f.engine(t, true)
	launched := false
	if err := e.ResumeTaskInvocation(t.Context(), "run-discussion", func(ctx context.Context) error {
		// The claim has won. Stage-owned asynchronous preparation may still be
		// pending when closure commits, without holding the engine mutex.
		closeWorkflowThroughEngineForTest(t, e, "run-discussion")
		if err := ctx.Err(); err != nil {
			t.Fatalf("closure cancelled claimed launch: %v", err)
		}
		launched = true
		return nil
	}); err != nil || !launched {
		t.Fatalf("claimed launch = %v, launched=%v", err, launched)
	}
	if err := e.ResumeTaskInvocation(t.Context(), "run-discussion", func(context.Context) error {
		t.Fatal("retry reused earlier claim")
		return nil
	}); !errors.Is(err, store.ErrTaskWorkflowClosed) {
		t.Fatalf("fresh retry claim = %v", err)
	}
}

func closeWorkflowForTest(t *testing.T, st *store.Store, runID domain.RunID, cleanup bool) store.TaskWorkflowClosure {
	t.Helper()
	ctx := t.Context()
	var task domain.Task
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		run, err := tx.GetRun(ctx, runID)
		if err != nil {
			return err
		}
		task, err = tx.GetTask(ctx, run.TaskID)
		if err != nil {
			return err
		}
		if task.CurrentStart() == nil {
			if err := tx.RecordTaskStart(ctx, runID, time.Now().UTC()); err != nil {
				return err
			}
			task, err = tx.GetTask(ctx, run.TaskID)
			if err != nil {
				return err
			}
		}
		_, err = tx.AbandonTask(ctx, task.ID, time.Now().UTC())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var closure store.TaskWorkflowClosure
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		var err error
		closure, _, err = tx.CloseTaskWorkflow(ctx, task.ID, task.CurrentStart().Ordinal, time.Now().UTC())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if cleanup {
		if err := st.Write(ctx, func(tx *store.WriteTx) error {
			_, err := RetireClosedTaskWorkflowItems(ctx, tx, task.ID, closure.StartOrdinal)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	return closure
}

func TestWorkflowClosureRetiresBothSpecificationDiscussionIntents(t *testing.T) {
	f, driver, id := discussionAwaitingReply(t, 0)
	e := f.newEngine(t, driver)
	// Materialize the specification-specific marker without dispatching it.
	if _, err := e.reconcileSpecificationGates(t.Context()); err != nil {
		t.Fatal(err)
	}
	closeWorkflowForTest(t, f.store, "specification-run", true)
	for range 2 {
		if _, err := f.newEngine(t, driver).Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		for _, key := range []string{string(id), "inv-explain-hold"} {
			entry, err := tx.GetOutbox(t.Context(), key)
			if err != nil {
				return err
			}
			if !entry.Dispatched() {
				t.Fatalf("discussion intent %s remains pending", key)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWorkflowClosureCollectsLateExecutionWithoutSuccessor(t *testing.T) {
	f := newFailedImplementationFixture(t, execfake.OutcomeFail, "Late provider failure.")
	closeWorkflowForTest(t, f.store, f.run.ID, true)
	for range 2 {
		e := f.newEngine(t, f.driver)
		if _, err := e.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		if _, err := tx.GetInbox(t.Context(), "closed-workflow-result/"+string(f.attempt.InvocationID)); err != nil {
			return err
		}
		if _, err := tx.GetAttentionItem(t.Context(), f.failureItemID()); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("late execution created a recovery decision: %v", err)
		}
		run, err := tx.GetRun(t.Context(), f.run.ID)
		if err != nil {
			return err
		}
		task, err := tx.GetTask(t.Context(), run.TaskID)
		if err == nil && domain.TaskWIP(task) {
			t.Fatalf("late execution advanced task: %+v", task)
		}
		for _, fact := range task.LifecycleFacts {
			if fact.Kind == domain.TaskLifecycleCompleted {
				t.Fatal("late execution recorded task completion")
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWorkflowClosureAllowsRegisteredObservationButRefusesResume(t *testing.T) {
	f := newAttentionDiscussionFixture(t, domain.AttentionExecutionFailure, nil)
	closeWorkflowForTest(t, f.store, "run-discussion", true)
	e := f.engine(t, true)
	observed := false
	if err := e.RecoverTaskInvocation(t.Context(), "run-discussion", func(ctx context.Context) error {
		if ctx.Err() != nil {
			t.Fatal("closure cancelled observation")
		}
		observed = true
		return nil
	}); err != nil || !observed {
		t.Fatalf("closed observation = %v, observed=%v", err, observed)
	}
	if err := e.ResumeTaskInvocation(t.Context(), "run-discussion", func(context.Context) error {
		t.Fatal("closed workflow resumed")
		return nil
	}); !errors.Is(err, store.ErrTaskWorkflowClosed) {
		t.Fatalf("closed Resume = %v", err)
	}
}

func TestWorkflowClosureRetiresUnstartedDiscussionAcrossRestart(t *testing.T) {
	driver := inferencefake.New()
	f := newAttentionDiscussionFixture(t, domain.AttentionExecutionFailure, driver)
	f.submit(t)
	closeWorkflowForTest(t, f.store, "run-discussion", false)
	f.blockUnattended(t) // Cleanup does not wait for a launch gate to reopen.
	for range 2 {
		if result, err := f.engine(t, true).Reconcile(t.Context()); err != nil || result.ResultsAccepted != 0 {
			t.Fatalf("closed reconcile = %+v, %v", result, err)
		}
	}
	if len(driver.Requests()) != 0 {
		t.Fatal("closed discussion called inference")
	}
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		item, err := tx.GetAttentionItem(t.Context(), f.item.ID)
		if err != nil {
			return err
		}
		conversation, err := tx.GetConversation(t.Context(), *item.ConversationID)
		if err != nil {
			return err
		}
		entry, err := tx.GetOutbox(t.Context(), "inv-discuss-1")
		if err != nil {
			return err
		}
		if item.Status != domain.StatusExpired || item.DecidedAt != nil ||
			conversation.Status != domain.ConversationIdle || len(conversation.Messages) != 1 || !entry.Dispatched() {
			t.Fatalf("unstarted discussion was not settled: item=%+v conversation=%+v entry=%+v", item, conversation, entry)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWorkflowClosureRetainsInflightDiscussionWithoutDecisionAdvance(t *testing.T) {
	for _, cleanup := range []bool{false, true} {
		t.Run(map[bool]string{false: "cleanup pending", true: "cleanup committed"}[cleanup], func(t *testing.T) {
			var f *attentionDiscussionFixture
			calls := 0
			driver := taskNamingDriverFunc(func(ctx context.Context, _ inference.Request, _ inference.Secret) (inference.Response, error) {
				calls++
				closeWorkflowForTest(t, f.store, "run-discussion", cleanup)
				if ctx.Err() != nil {
					t.Fatal("administrative closure cancelled the in-flight call")
				}
				return inference.Response{Output: []byte(`{"reply":"The already-started reply is history."}`), ComputeUnits: 1}, nil
			})
			f = newAttentionDiscussionFixture(t, domain.AttentionExecutionFailure, driver)
			f.submit(t)
			e := f.engine(t, true)
			if result, err := e.Reconcile(t.Context()); err != nil || result.ResultsAccepted != 1 {
				t.Fatalf("late reply = %+v, %v", result, err)
			}
			if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
				item, err := tx.GetAttentionItem(t.Context(), f.item.ID)
				if err != nil {
					return err
				}
				conversation, err := tx.GetConversation(t.Context(), *item.ConversationID)
				wantVersion := 2
				if cleanup {
					wantVersion++
				}
				if err == nil && (item.ItemVersion != wantVersion || item.DecidedAt != nil ||
					len(conversation.Messages) != 2 || conversation.Status != domain.ConversationIdle) {
					t.Fatalf("late reply advanced decision: item=%+v conversation=%+v", item, conversation)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.engine(t, true).Reconcile(t.Context()); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("reply calls = %d, want 1", calls)
			}
			launched := false
			if err := e.ResumeTaskInvocation(t.Context(), "run-discussion", func(context.Context) error {
				launched = true
				return nil
			}); !errors.Is(err, store.ErrTaskWorkflowClosed) || launched {
				t.Fatalf("closed Resume = %v, launched=%v", err, launched)
			}
		})
	}
}
