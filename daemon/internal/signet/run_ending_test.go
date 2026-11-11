package signet_test

import (
	"context"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// seedStartedRun seeds a task whose started run has one submitted
// implementation invocation, so the run reads pending before anything ends it.
func seedStartedRun(t *testing.T, ctx context.Context, f fixture) (domain.TaskID, domain.Run) {
	t.Helper()
	runID := domain.RunID("run-ending")
	run := domain.Run{
		ID: runID, ProjectID: "project-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
		Stages: []domain.Stage{{
			ID: "stage-ending", RunID: runID, Name: string(domain.StageNameImplementation),
			Attempts: []domain.Attempt{{ID: "attempt-ending", StageID: "stage-ending", Number: 1, InvocationID: "inv-ending"}},
		}},
	}
	var taskID domain.TaskID
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		task, err := tx.GetOrCreateTask(ctx, "project-1", domain.SpecificationSource{
			Kind:         domain.SpecificationSourceIssueSubject,
			IssueSubject: &domain.IssueSubjectRef{Repo: "owner/repo", RepositoryID: 1, IssueNumber: 1},
		})
		if err != nil {
			return err
		}
		taskID, run.TaskID = task.ID, task.ID
		return tx.PutRun(ctx, run)
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	seedProductionSubmission(t, ctx, f, run, time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC))
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.RecordTaskStart(ctx, runID, time.Date(2026, 11, 10, 12, 0, 1, 0, time.UTC))
	}); err != nil {
		t.Fatalf("record start: %v", err)
	}
	return taskID, run
}

// assertRunReads checks the run API's outcome and lifecycle for one run in
// both the single-run read and the listing (#3043).
func assertRunReads(t *testing.T, ctx context.Context, f fixture, runID domain.RunID,
	outcome domain.RunOutcome, lifecycle domain.RunLifecycle,
) {
	t.Helper()
	got, err := f.service.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Run.Outcome != outcome || got.Run.Lifecycle != lifecycle {
		t.Errorf("GetRun = outcome %s lifecycle %s, want %s %s", got.Run.Outcome, got.Run.Lifecycle, outcome, lifecycle)
	}
	runs, err := f.service.ListRuns(ctx)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	for _, snapshot := range runs {
		if snapshot.Run.ID == runID {
			if snapshot.Run.Outcome != outcome || snapshot.Run.Lifecycle != lifecycle {
				t.Errorf("ListRuns = outcome %s lifecycle %s, want %s %s",
					snapshot.Run.Outcome, snapshot.Run.Lifecycle, outcome, lifecycle)
			}
			return
		}
	}
	t.Errorf("ListRuns is missing run %s", runID)
}

// TestRunAPISettlesConfirmedStop is the #3043 stop acceptance: a requested or
// failed cancellation leaves the run pending; a confirmed one concludes it
// stopped and finished.
func TestRunAPISettlesConfirmedStop(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newRunFixture(t)
	taskID, run := seedStartedRun(t, ctx, f)
	assertRunReads(t, ctx, f, run.ID, domain.RunOutcomePending, domain.RunLifecycleActive)

	accepted, err := f.service.Submit(ctx, stopCommand(t, f.store, taskID, "stop"))
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	assertRunReads(t, ctx, f, run.ID, domain.RunOutcomePending, domain.RunLifecycleActive)

	c := accepted.Stop.Cancellation
	ack := domain.TaskCancellationAcknowledgement{
		RequestID: c.RequestID, TargetDigest: c.TargetDigest, EvidenceDigest: c.TargetDigest,
	}
	for i, state := range []domain.TaskCancellationState{domain.TaskCancellationFailed, domain.TaskCancellationConfirmed} {
		ack.ID, ack.State = string(state), state
		ack.RecordedAt = time.Date(2026, 11, 10, 13, i, 0, 0, time.UTC)
		if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
			_, err := tx.AcknowledgeTaskCancellation(ctx, ack)
			return err
		}); err != nil {
			t.Fatalf("acknowledge %s: %v", state, err)
		}
		if state == domain.TaskCancellationFailed {
			assertRunReads(t, ctx, f, run.ID, domain.RunOutcomePending, domain.RunLifecycleActive)
		}
	}
	assertRunReads(t, ctx, f, run.ID, domain.RunOutcomeStopped, domain.RunLifecycleFinished)
}

// TestRunAPISettlesAbandonedRunOnceItsExecutionEnds is the #3043 abandon
// acceptance: abandonment doesn't stop a provider execution, so the run of a
// closed episode stays pending while an invocation is still running and
// concludes abandoned once none is.
func TestRunAPISettlesAbandonedRunOnceItsExecutionEnds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newRunFixture(t)
	taskID, run := seedStartedRun(t, ctx, f)
	invocation := run.Stages[0].Attempts[0].InvocationID
	observe := func(status domain.ObservedInvocationStatus, minute int) {
		t.Helper()
		if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
			return tx.RecordInvocationObservation(ctx, domain.InvocationObservation{
				InvocationID: invocation, RunID: run.ID, Status: status,
				Live:       status == domain.ObservedStatusRunning,
				ObservedAt: time.Date(2026, 11, 10, 12, minute, 0, 0, time.UTC),
			})
		}); err != nil {
			t.Fatalf("observe %s: %v", status, err)
		}
	}
	observe(domain.ObservedStatusRunning, 1)
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		task, err := tx.GetTask(ctx, taskID)
		if err != nil {
			return err
		}
		if _, err := tx.AbandonTask(ctx, taskID, time.Date(2026, 11, 10, 12, 2, 0, 0, time.UTC)); err != nil {
			return err
		}
		_, _, err = tx.CloseTaskWorkflow(ctx, taskID, task.CurrentStart().Ordinal,
			time.Date(2026, 11, 10, 12, 2, 0, 0, time.UTC))
		return err
	}); err != nil {
		t.Fatalf("abandon: %v", err)
	}
	assertRunReads(t, ctx, f, run.ID, domain.RunOutcomePending, domain.RunLifecycleActive)

	observe(domain.ObservedStatusFailed, 3)
	assertRunReads(t, ctx, f, run.ID, domain.RunOutcomeAbandoned, domain.RunLifecycleFinished)
}
