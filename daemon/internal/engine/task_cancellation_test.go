package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/stage"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func cancelledTaskFixture(t *testing.T, st *store.Store, number int, withRun bool) domain.TaskCancellation {
	t.Helper()
	ctx := t.Context()
	var task domain.Task
	err := st.Write(ctx, func(tx *store.WriteTx) error {
		var err error
		task, err = tx.GetOrCreateTask(ctx, "project-1", domain.SpecificationSource{
			Kind:         domain.SpecificationSourceIssueSubject,
			IssueSubject: &domain.IssueSubjectRef{Repo: "owner/repo", RepositoryID: 1, IssueNumber: number},
		})
		if err != nil || !withRun {
			return err
		}
		run := domain.Run{
			ID: domain.RunID(fmt.Sprintf("run-stop-%d", number)), TaskID: task.ID, ProjectID: task.ProjectID,
			SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy", Stages: []domain.Stage{},
		}
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		return tx.RecordTaskStart(ctx, run.ID, time.Now().UTC())
	})
	if err != nil {
		t.Fatal(err)
	}
	state, err := st.ServerState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var cancellation domain.TaskCancellation
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		receipt, _, err := tx.StopTask(ctx, domain.StopTaskRequest{
			CommandID: fmt.Sprintf("stop-%d", number), DeviceID: "device-1", TaskID: task.ID, ProjectID: task.ProjectID,
			ExpectedSyncEpoch: state.SyncEpoch, ExpectedEntityVersion: state.Revision,
		}, time.Now().UTC())
		cancellation = receipt.Cancellation
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return cancellation
}

func cancellationTask(t *testing.T, st *store.Store, id domain.TaskID) domain.Task {
	t.Helper()
	var task domain.Task
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		task, err = tx.GetTask(t.Context(), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return task
}

type cancellationReviewSource struct {
	exec.ReviewSource
	cancel func(domain.InvocationID) error
}

func (s cancellationReviewSource) CancelReview(_ context.Context, id domain.InvocationID) error {
	return s.cancel(id)
}

func TestTaskCancellationStopsBothReviewArms(t *testing.T) {
	for _, primaryFails := range []bool{false, true} {
		st := watchTestStore(t)
		review := domain.ReviewRequestRecord{InvocationID: "primary", RunID: "run", Round: 2}
		primaryCalled, shadowCalled := make(chan domain.InvocationID, 1), make(chan domain.InvocationID, 1)
		primary := cancellationReviewSource{cancel: func(id domain.InvocationID) error {
			primaryCalled <- id
			if primaryFails {
				return errors.New("primary cleanup failed")
			}
			return nil
		}}
		shadowFailure := errors.New("shadow still running")
		shadow := cancellationReviewSource{cancel: func(id domain.InvocationID) error {
			shadowCalled <- id
			return shadowFailure
		}}
		if err := StopTaskReviews(t.Context(), st, review, primary, shadow); !errors.Is(err, shadowFailure) {
			t.Fatalf("shadow failure did not retain cancellation uncertainty: %v", err)
		}
		if id := <-primaryCalled; id != review.InvocationID {
			t.Fatalf("primary cancellation targeted %s", id)
		}
		if id := <-shadowCalled; id != ProductionShadowReviewInvocationID(review.RunID, review.Round) {
			t.Fatalf("shadow cancellation targeted %s", id)
		}
	}
}

func TestTaskCancellationMissingReviewSourceNeedsExactJournalAbsence(t *testing.T) {
	st := watchTestStore(t)
	review := domain.ReviewRequestRecord{InvocationID: "primary", RunID: "run", Round: 1}
	primary := cancellationReviewSource{cancel: func(domain.InvocationID) error { return nil }}
	if err := StopTaskReviews(t.Context(), st, review, primary, nil); err != nil {
		t.Fatalf("never-enabled shadow arm blocked Stop: %v", err)
	}
	shadowID := ProductionShadowReviewInvocationID(review.RunID, review.Round)
	if err := st.WriteInternal(t.Context(), func(tx *store.InternalTx) error {
		return tx.RecordCodexReviewRequest(t.Context(), string(shadowID), []byte(`{"retained":"request"}`))
	}); err != nil {
		t.Fatal(err)
	}
	if err := StopTaskReviews(t.Context(), st, review, primary, nil); err == nil {
		t.Fatal("removed shadow source erased retained ownership uncertainty")
	}
	shadow := cancellationReviewSource{cancel: func(id domain.InvocationID) error {
		if id != shadowID {
			t.Errorf("restored adapter targeted %s", id)
		}
		return nil
	}}
	if err := StopTaskReviews(t.Context(), st, review, primary, shadow); err != nil {
		t.Fatalf("restored adapter could not supply proof: %v", err)
	}
}

func TestTaskCancellationRestartRetainsWIPForMissingShadowAdapter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := watchTestStore(t)
		c := cancelledTaskFixture(t, st, 1, true)
		review := domain.ReviewRequestRecord{InvocationID: "primary", RunID: c.Target.Runs[0].RunID, Round: 1}
		shadowID := ProductionShadowReviewInvocationID(review.RunID, review.Round)
		if err := st.WriteInternal(t.Context(), func(tx *store.InternalTx) error {
			return tx.RecordCodexReviewRequest(t.Context(), string(shadowID), []byte(`{"retained":"request"}`))
		}); err != nil {
			t.Fatal(err)
		}
		primary := cancellationReviewSource{cancel: func(domain.InvocationID) error { return nil }}
		shadow := cancellationReviewSource{cancel: func(id domain.InvocationID) error {
			if id != shadowID {
				t.Errorf("restored adapter targeted %s", id)
			}
			return nil
		}}
		for _, source := range []exec.ReviewSource{nil, nil, shadow} {
			// A fresh coordinator models restart with only durable ownership.
			e := &Engine{store: st}
			e.cancellation.runtime = TaskCancellationRuntime{Timeout: time.Second, StopRun: func(ctx context.Context, _ domain.Run, _ []domain.ReviewRequestRecord) error {
				return StopTaskReviews(ctx, st, review, primary, source)
			}}
			if err := e.ReconcileTaskCancellations(t.Context()); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
			if err := e.ReconcileTaskCancellations(t.Context()); err != nil {
				t.Fatal(err)
			}
			want := domain.TaskCancellationFailed
			if source != nil {
				want = domain.TaskCancellationConfirmed
			}
			task := cancellationTask(t, st, c.Target.TaskID)
			if task.Cancellation.State != want || domain.TaskWIP(task) != (source == nil) {
				t.Fatalf("restart lost shadow uncertainty: %+v", task)
			}
		}
	})
}

func TestTaskCancellationNoStartAndReplay(t *testing.T) {
	st := watchTestStore(t)
	c := cancelledTaskFixture(t, st, 1, false)
	e := &Engine{store: st}
	if err := e.ReconcileTaskCancellations(t.Context()); err != nil {
		t.Fatal(err)
	}
	task := cancellationTask(t, st, c.Target.TaskID)
	if task.Cancellation.State != domain.TaskCancellationConfirmed || len(task.LifecycleFacts) != 0 {
		t.Fatalf("no-start cancellation invented lifecycle facts: %+v", task)
	}
	before, _ := st.ServerState(t.Context())
	if err := e.ReconcileTaskCancellations(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, _ := st.ServerState(t.Context())
	if before != after {
		t.Fatal("replayed reconciliation changed server revision")
	}
}

func TestTaskCancellationWithoutRuntimeProofKeepsWIP(t *testing.T) {
	st := watchTestStore(t)
	c := cancelledTaskFixture(t, st, 1, true)
	e := &Engine{store: st}
	if err := e.ReconcileTaskCancellations(t.Context()); err != nil {
		t.Fatal(err)
	}
	task := cancellationTask(t, st, c.Target.TaskID)
	if task.Cancellation.State != domain.TaskCancellationFailed || !domain.TaskWIP(task) {
		t.Fatalf("missing ownership proof released task: %+v", task)
	}
}

func TestTaskCancellationRetryAfterMissingProofReleasesOnlyBoundWIP(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := watchTestStore(t)
		c := cancelledTaskFixture(t, st, 1, true)
		e := &Engine{store: st}
		calls := 0
		e.cancellation.runtime = TaskCancellationRuntime{
			Timeout: time.Second,
			StopRun: func(context.Context, domain.Run, []domain.ReviewRequestRecord) error {
				calls++
				if calls == 1 {
					return errors.New("fixture owned-resource absence unproven")
				}
				return nil
			},
		}
		if err := e.ReconcileTaskCancellations(t.Context()); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if err := e.ReconcileTaskCancellations(t.Context()); err != nil {
			t.Fatal(err)
		}
		failed := cancellationTask(t, st, c.Target.TaskID)
		if failed.Cancellation.State != domain.TaskCancellationFailed || !domain.TaskWIP(failed) || len(failed.LifecycleFacts) != 1 {
			t.Fatalf("missing proof released or duplicated task work: %+v", failed)
		}
		if err := e.ReconcileTaskCancellations(t.Context()); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if calls != 1 {
			t.Fatalf("retried before the backoff ended: %d", calls)
		}
		time.Sleep(taskStopRetryDelay(1))
		if err := e.ReconcileTaskCancellations(t.Context()); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if err := e.ReconcileTaskCancellations(t.Context()); err != nil {
			t.Fatal(err)
		}
		confirmed := cancellationTask(t, st, c.Target.TaskID)
		if confirmed.Cancellation.State != domain.TaskCancellationConfirmed || domain.TaskWIP(confirmed) ||
			len(confirmed.LifecycleFacts) != 2 || confirmed.LifecycleFacts[1].Kind != domain.TaskLifecycleAbandoned || calls != 3 {
			t.Fatalf("fresh retry failed to release bound WIP once: task=%+v calls=%d", confirmed, calls)
		}
		if ack := confirmed.Cancellation.Acknowledgement; ack == nil || ack.RequestID != c.RequestID ||
			ack.TargetDigest != c.TargetDigest || ack.State != domain.TaskCancellationConfirmed {
			t.Fatalf("confirmed acknowledgement lost its request or target binding: %+v", ack)
		}
		if _, _, err := e.BeginRunWork(t.Context(), "run-stop-1"); !errors.Is(err, store.ErrTaskCancellationFenced) {
			t.Fatalf("stopped task admitted a successor: %v", err)
		}
		var other domain.Task
		if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
			var err error
			other, err = tx.GetOrCreateTask(t.Context(), "project-1", domain.SpecificationSource{
				Kind:         domain.SpecificationSourceIssueSubject,
				IssueSubject: &domain.IssueSubjectRef{Repo: "owner/repo", RepositoryID: 1, IssueNumber: 2},
			})
			if err != nil {
				return err
			}
			return tx.PutRun(t.Context(), domain.Run{
				ID: "run-other", TaskID: other.ID, ProjectID: other.ProjectID,
				SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
			})
		}); err != nil {
			t.Fatal(err)
		}
		_, finish, err := e.BeginRunWork(t.Context(), "run-other")
		if err != nil {
			t.Fatalf("another task was not admitted after release: %v", err)
		}
		finish()
		before, err := st.ServerState(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err := e.ReconcileTaskCancellations(t.Context()); err != nil {
			t.Fatal(err)
		}
		after, err := st.ServerState(t.Context())
		if err != nil || before != after {
			t.Fatalf("replayed proof changed receipt state: before=%+v after=%+v err=%v", before, after, err)
		}
	})
}

// reconcileTaskStopPasses drives the loop at its production interval and lets
// an attempt a pass started finish before the next pass.
func reconcileTaskStopPasses(t *testing.T, e *Engine, passes int) {
	t.Helper()
	for range passes {
		if err := e.ReconcileTaskCancellations(t.Context()); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		time.Sleep(100 * time.Millisecond)
	}
}

// taskStopLogEngine is a fresh coordinator on st, which models a restart with
// only durable ownership, and the buffer its log lines land in.
func taskStopLogEngine(st *store.Store, runtime TaskCancellationRuntime) (*Engine, *bytes.Buffer) {
	var logs bytes.Buffer
	e := &Engine{store: st, logger: slog.New(slog.NewTextHandler(&logs, nil))}
	e.cancellation.runtime = runtime
	return e, &logs
}

func TestTaskCancellationUnprovableStopIsAttemptedOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := watchTestStore(t)
		c := cancelledTaskFixture(t, st, 1, true)
		const refusal = "fixture task outside runtime coverage"
		calls := 0
		runtime := TaskCancellationRuntime{
			Timeout: time.Second,
			StopRun: func(context.Context, domain.Run, []domain.ReviewRequestRecord) error {
				calls++
				return errors.New(refusal)
			},
			Unprovable: func(context.Context, domain.TaskID) (string, error) { return refusal, nil },
		}
		for _, process := range []string{"first", "restarted"} {
			e, logs := taskStopLogEngine(st, runtime)
			// Ten seconds of passes spans several backoff delays, so a stop
			// that was only paced, not settled, would be attempted again.
			reconcileTaskStopPasses(t, e, 100)
			if calls != 1 {
				t.Fatalf("%s process: unprovable stop attempted %d times", process, calls)
			}
			if got := logs.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, "task stop cannot be proven; not retrying") {
				t.Fatalf("%s process: want one settled line, got:\n%s", process, got)
			}
			task := cancellationTask(t, st, c.Target.TaskID)
			if task.Cancellation.State != domain.TaskCancellationFailed || !domain.TaskWIP(task) {
				t.Fatalf("%s process: unprovable stop released or lost its task: %+v", process, task)
			}
		}
	})
}

func TestTaskCancellationClearableFailureBacksOff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := watchTestStore(t)
		c := cancelledTaskFixture(t, st, 1, true)
		stage, review := errors.New("fixture stage still running"), errors.New("fixture review still running")
		// Children join in completion order, so the first two are one failure.
		failures := []error{
			errors.Join(stage, review),
			errors.Join(review, stage),
			errors.New("fixture child exit unobserved"),
		}
		calls := 0
		e, logs := taskStopLogEngine(st, TaskCancellationRuntime{
			Timeout: time.Second,
			StopRun: func(context.Context, domain.Run, []domain.ReviewRequestRecord) error {
				calls++
				if calls <= len(failures) {
					return failures[calls-1]
				}
				return nil
			},
			Unprovable: func(context.Context, domain.TaskID) (string, error) { return "", nil },
		})
		attempt := func() {
			t.Helper()
			for range 2 {
				if err := e.ReconcileTaskCancellations(t.Context()); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
			}
		}
		attempt()
		for failed := 1; failed <= len(failures); failed++ {
			if got := cancellationTask(t, st, c.Target.TaskID); calls != failed || got.Cancellation.State != domain.TaskCancellationFailed || !domain.TaskWIP(got) {
				t.Fatalf("failure %d: calls=%d task=%+v", failed, calls, got)
			}
			time.Sleep(taskStopRetryDelay(failed) - time.Nanosecond)
			attempt()
			if calls != failed {
				t.Fatalf("failure %d: retried before the backoff ended", failed)
			}
			time.Sleep(time.Nanosecond)
			attempt()
		}
		// The proving attempt observes each run twice.
		if got := cancellationTask(t, st, c.Target.TaskID); calls != len(failures)+2 || got.Cancellation.State != domain.TaskCancellationConfirmed || domain.TaskWIP(got) {
			t.Fatalf("retry did not confirm and release: calls=%d task=%+v", calls, got)
		}
		// Three failures carry two distinct errors, so two lines.
		if got := logs.String(); strings.Count(got, "\n") != 2 || strings.Count(got, "task stop proof failed") != 2 {
			t.Fatalf("want one line per distinct failure, got:\n%s", got)
		}
		attempt()
		if len(e.cancellation.retries) != 0 {
			t.Fatalf("confirmed request kept its retry record: %+v", e.cancellation.retries)
		}
		if got := taskStopRetryDelay(100); got != 256*time.Second {
			t.Fatalf("backoff is not capped: %s", got)
		}
	})
}

func TestTaskCancellationFailedUnprovableCheckKeepsTheStopRetryable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := watchTestStore(t)
		c := cancelledTaskFixture(t, st, 1, true)
		calls := 0
		runtime := TaskCancellationRuntime{
			Timeout: time.Second,
			StopRun: func(context.Context, domain.Run, []domain.ReviewRequestRecord) error {
				calls++
				return errors.New("fixture task outside runtime coverage")
			},
			// A check that cannot answer must never end retries for good.
			Unprovable: func(context.Context, domain.TaskID) (string, error) {
				return "", errors.New("fixture epoch read failed")
			},
		}
		for i, process := range []string{"first", "restarted"} {
			e, logs := taskStopLogEngine(st, runtime)
			// Attempts at 0s, 1s, 3s, and 7s fit in ten seconds of passes.
			reconcileTaskStopPasses(t, e, 100)
			if want := (i + 1) * 4; calls != want {
				t.Fatalf("%s process: want %d backed-off attempts, got %d", process, want, calls)
			}
			if got := logs.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, "task stop proof failed") ||
				!strings.Contains(got, "unprovable check: fixture epoch read failed") {
				t.Fatalf("%s process: want one retryable-failure line, got:\n%s", process, got)
			}
			if task := cancellationTask(t, st, c.Target.TaskID); task.Cancellation.State != domain.TaskCancellationFailed || !domain.TaskWIP(task) {
				t.Fatalf("%s process: failed check released or lost the task: %+v", process, task)
			}
		}
	})
}

func TestTaskCancellationRefusedAcknowledgementSettlesTheRequest(t *testing.T) {
	for _, tc := range []struct {
		name            string
		stopErr         error
		outlivesTimeout bool
	}{
		{name: "proved"},
		{name: "failed", stopErr: errors.New("fixture child still running")},
		{name: "outlives timeout", outlivesTimeout: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				st := watchTestStore(t)
				c := cancelledTaskFixture(t, st, 1, true)
				// A new epoch moves the task's target off the one the Stop bound.
				if _, err := st.NewEpoch(t.Context()); err != nil {
					t.Fatal(err)
				}
				finish := make(chan struct{})
				if !tc.outlivesTimeout {
					close(finish)
				}
				calls := 0
				runtime := TaskCancellationRuntime{
					Timeout: time.Second,
					StopRun: func(context.Context, domain.Run, []domain.ReviewRequestRecord) error {
						calls++
						<-finish
						return tc.stopErr
					},
				}
				// One attempt per process; a proving attempt observes each run twice.
				perProcess := 2
				if tc.stopErr != nil {
					perProcess = 1
				}
				for i, process := range []string{"first", "restarted"} {
					e, logs := taskStopLogEngine(st, runtime)
					reconcileTaskStopPasses(t, e, 100)
					if tc.outlivesTimeout && process == "first" {
						if calls != 1 || strings.Count(logs.String(), "\n") != 1 {
							t.Fatalf("refusal at the deadline: calls=%d logs:\n%s", calls, logs)
						}
						close(finish)
						reconcileTaskStopPasses(t, e, 100)
						if len(e.cancellation.attempts) != 0 {
							t.Fatal("returned worker stayed registered")
						}
					}
					if want := (i + 1) * perProcess; calls != want {
						t.Fatalf("%s process: want %d StopRun calls, got %d", process, want, calls)
					}
					if got := logs.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, "task stop acknowledgement refused") {
						t.Fatalf("%s process: want one refusal line, got:\n%s", process, got)
					}
					if task := cancellationTask(t, st, c.Target.TaskID); task.Cancellation.State != domain.TaskCancellationRequested || !domain.TaskWIP(task) {
						t.Fatalf("%s process: refused acknowledgement changed the request: %+v", process, task)
					}
				}
			})
		})
	}
}

func TestTaskCancellationTimeoutDoesNotBlockDiscoveryOrConfirm(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := watchTestStore(t)
		active := cancelledTaskFixture(t, st, 1, true)
		e := &Engine{store: st}
		finish := make(chan struct{})
		calls := 0
		e.cancellation.runtime = TaskCancellationRuntime{
			Timeout: time.Second,
			StopRun: func(context.Context, domain.Run, []domain.ReviewRequestRecord) error {
				calls++
				<-finish
				return nil
			},
		}
		if err := e.ReconcileTaskCancellations(t.Context()); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		queued := cancelledTaskFixture(t, st, 2, false)
		time.Sleep(time.Second)
		if err := e.ReconcileTaskCancellations(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := cancellationTask(t, st, active.Target.TaskID); got.Cancellation.State != domain.TaskCancellationFailed || !domain.TaskWIP(got) {
			t.Fatalf("timeout claimed termination: %+v", got)
		}
		if got := cancellationTask(t, st, queued.Target.TaskID); got.Cancellation.State != domain.TaskCancellationConfirmed {
			t.Fatalf("blocked discovery of another Stop: %+v", got)
		}
		if err := e.ReconcileTaskCancellations(t.Context()); err != nil {
			t.Fatal(err)
		}
		if calls != 1 {
			t.Fatalf("repeated teardown of an unresponsive child: %d", calls)
		}
		close(finish)
		synctest.Wait()
		if err := e.ReconcileTaskCancellations(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := cancellationTask(t, st, active.Target.TaskID); got.Cancellation.State != domain.TaskCancellationConfirmed || domain.TaskWIP(got) {
			t.Fatalf("actual quiescence did not release the bound WIP: %+v", got)
		}
	})
}

func TestTaskCancellationOrdersLaunchRegistrationAndStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newSpecificationFixture(t, true, 4)
		f.submit(t)
		e := f.newEngine(t, f.newDriver(t))
		run, err := f.run("specification-run")
		if err != nil {
			t.Fatal(err)
		}
		work, finish, err := e.BeginRunWork(t.Context(), run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.CommitTaskStop(t.Context(), run.TaskID, func() error {
			return f.store.Write(t.Context(), func(tx *store.WriteTx) error {
				state, err := tx.ServerState(t.Context())
				if err != nil {
					return err
				}
				_, _, err = tx.StopTask(t.Context(), domain.StopTaskRequest{
					CommandID: "launch-stop", DeviceID: "device-1", TaskID: run.TaskID, ProjectID: run.ProjectID,
					ExpectedSyncEpoch: state.SyncEpoch, ExpectedEntityVersion: state.Revision,
				}, time.Now().UTC())
				return err
			})
		}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if work.Err() != context.Canceled {
			t.Fatal("registered work did not receive Stop")
		}
		joined := make(chan error, 1)
		go func() { joined <- e.stopTaskWork(t.Context(), run.TaskID) }()
		synctest.Wait()
		select {
		case <-joined:
			t.Fatal("cancel signal was mistaken for a joined call")
		default:
		}
		finish()
		if err := <-joined; err != nil {
			t.Fatal(err)
		}
		if _, _, err := e.BeginRunWork(t.Context(), run.ID); !errors.Is(err, store.ErrTaskCancellationFenced) {
			t.Fatalf("new work crossed fence: %v", err)
		}
		if err := e.ResumeTaskInvocation(t.Context(), run.ID, func(context.Context) error {
			t.Fatal("recovery relaunched fenced work")
			return nil
		}); !errors.Is(err, stage.ErrTaskCancelled) {
			t.Fatalf("recovery bypassed fence: %v", err)
		}
		if result, err := e.Reconcile(t.Context()); err != nil || result.InvocationsStarted != 0 {
			t.Fatalf("queued intent crossed fence: %+v, %v", result, err)
		}
	})
}

type stopDuringStartDriver struct {
	exec.StageDriver
	start func(context.Context) error
}

func (d stopDuringStartDriver) Start(ctx context.Context, _ domain.InvocationID, _ exec.StartSpec) error {
	return d.start(ctx)
}

func TestTaskCancellationDuringStartDoesNotFailSharedReconcile(t *testing.T) {
	f := newSpecificationFixture(t, true, 4)
	f.submit(t)
	run, err := f.run("specification-run")
	if err != nil {
		t.Fatal(err)
	}
	var e *Engine
	entered := false
	driver := stopDuringStartDriver{StageDriver: f.newDriver(t), start: func(ctx context.Context) error {
		entered = true
		if err := e.CommitTaskStop(t.Context(), run.TaskID, func() error {
			return f.store.Write(t.Context(), func(tx *store.WriteTx) error {
				state, err := tx.ServerState(t.Context())
				if err != nil {
					return err
				}
				_, _, err = tx.StopTask(t.Context(), domain.StopTaskRequest{
					CommandID: "stop-during-start", DeviceID: "device-1", TaskID: run.TaskID, ProjectID: run.ProjectID,
					ExpectedSyncEpoch: state.SyncEpoch, ExpectedEntityVersion: state.Revision,
				}, time.Now().UTC())
				return err
			})
		}); err != nil {
			t.Fatal(err)
		}
		<-ctx.Done()
		return ctx.Err()
	}}
	e = f.newEngine(t, driver)
	if _, err := e.Reconcile(t.Context()); err != nil {
		t.Fatalf("task cancellation failed shared reconciliation: %v", err)
	}
	if !entered {
		t.Fatal("fixture did not reach driver Start")
	}
	if _, err := e.Reconcile(t.Context()); err != nil {
		t.Fatalf("subsequent reconciliation failed: %v", err)
	}
}
