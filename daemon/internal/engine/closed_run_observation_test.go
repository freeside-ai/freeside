package engine

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	execfake "github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// driverCallCounter counts the driver calls that reach a settled attempt.
type driverCallCounter struct {
	exec.StageDriver
	calls *atomic.Int64
}

func (d driverCallCounter) Inspect(ctx context.Context, id domain.InvocationID) (exec.Inspection, error) {
	d.calls.Add(1)
	return d.StageDriver.Inspect(ctx, id)
}

func (d driverCallCounter) Collect(ctx context.Context, id domain.InvocationID) (exec.StageResult, error) {
	d.calls.Add(1)
	return d.StageDriver.Collect(ctx, id)
}

// TestClosedRunSettledAttemptStaysQuiet: once a closed run's attempt has its
// closed-workflow-result row, later passes neither ask the driver about it
// nor move the run's or task's last activity, even from a fresh engine whose
// observation pace is empty, as after a daemon restart (issue #3452).
func TestClosedRunSettledAttemptStaysQuiet(t *testing.T) {
	t.Parallel()
	f := newFailedImplementationFixture(t, execfake.OutcomeFail, "Late provider failure.")
	closeWorkflowForTest(t, f.store, f.run.ID, true)
	if _, err := f.newEngine(t, f.driver).Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		_, err := tx.GetInbox(t.Context(), closedWorkflowResultKey(f.attempt.InvocationID))
		return err
	}); err != nil {
		t.Fatalf("closed-workflow-result row after the first pass: %v", err)
	}
	runBefore, taskBefore := f.lastActivity(t)

	calls := &atomic.Int64{}
	for range 2 {
		// A fresh engine each pass forgets the observation pace.
		e := f.newEngine(t, driverCallCounter{StageDriver: f.driver, calls: calls})
		if _, err := e.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("settled attempt reached the driver %d times, want 0", n)
	}
	runAfter, taskAfter := f.lastActivity(t)
	if !runAfter.Equal(runBefore) || !taskAfter.Equal(taskBefore) {
		t.Errorf("last activity moved: run %v -> %v, task %v -> %v",
			runBefore, runAfter, taskBefore, taskAfter)
	}
}

// streamFailingDriver fails every transcript read, so a specification
// discussion's reply cannot be decoded and accepted.
type streamFailingDriver struct{ exec.StageDriver }

func (streamFailingDriver) Stream(context.Context, domain.InvocationID) (io.ReadCloser, error) {
	return nil, errors.New("transcript unavailable")
}

// TestClosedRunReplyLandsBeforeSettling: a closed run's completed
// discussion reply lands before the attempt's closed-workflow-result row, so
// a pass whose reply fails leaves the attempt unsettled for the next pass,
// and the reply lands exactly once (issue #3452).
func TestClosedRunReplyLandsBeforeSettling(t *testing.T) {
	t.Parallel()
	f, driver, discussionID := discussionAwaitingReply(t, 0)
	refuseCollect(t, f, driver, discussionID) // Starts it without collecting.
	closeWorkflowForTest(t, f.store, "specification-run", false)
	item, _ := f.item(t, "spec-approval-implementation-run-1")
	settled := func() bool {
		t.Helper()
		err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
			_, err := tx.GetInbox(t.Context(), closedWorkflowResultKey(discussionID))
			return err
		})
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			t.Fatal(err)
		}
		return err == nil
	}
	replies := func() int {
		t.Helper()
		n := 0
		if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
			conversation, err := tx.GetConversation(t.Context(), *item.ConversationID)
			for _, m := range conversation.Messages {
				if m.Body == "The approved artifact bounds it." {
					n++
				}
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if _, err := f.newEngine(t, streamFailingDriver{driver}).Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if settled() || replies() != 0 {
		t.Fatalf("failed reply: settled=%v replies=%d, want unsettled with no reply", settled(), replies())
	}
	calls := &atomic.Int64{}
	for range 2 {
		if _, err := f.newEngine(t, driverCallCounter{StageDriver: driver, calls: calls}).Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if !settled() || replies() != 1 {
		t.Fatalf("retried reply: settled=%v replies=%d, want settled with one reply", settled(), replies())
	}
	// Inspect and Collect on the retry pass only; the settled pass adds none.
	if n := calls.Load(); n != 2 {
		t.Errorf("driver calls = %d, want 2 from the retry pass alone", n)
	}
}

// lastActivity reads the run's and its task's last activity as the app
// receives them.
func (f failedImplementationFixture) lastActivity(t *testing.T) (run, task time.Time) {
	t.Helper()
	snapshot, err := f.signet.Bootstrap(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var runTask domain.TaskID
	for _, r := range snapshot.Runs {
		if r.Run.ID == f.run.ID {
			if r.Run.LastActivityAt == nil {
				t.Fatal("closed run has no last activity")
			}
			run, runTask = *r.Run.LastActivityAt, r.Run.TaskID
		}
	}
	for _, s := range snapshot.Tasks {
		if runTask != "" && s.Task.ID == runTask {
			task = s.Task.LastActivityAt
		}
	}
	if run.IsZero() || task.IsZero() {
		t.Fatalf("run %s or its task missing from the snapshot", f.run.ID)
	}
	return run, task
}
