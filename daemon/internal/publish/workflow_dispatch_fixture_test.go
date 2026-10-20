package publish

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// RetireClosedForTest exposes the transactional retirement boundary so a
// winning dispatch can challenge it before the provider returns.
func (f *FollowUpFiler) RetireClosedForTest(ctx context.Context, id domain.ProposalInstanceID) (bool, error) {
	return f.retireClosed(ctx, id)
}

// CloseWorkflowAtDispatchForTest makes the next dispatch marker win before
// closure becomes visible. The closure is produced by the real API, then
// temporarily held outside its table and restored by a trigger after the
// marker write. This deterministically exercises the claim-to-send window
// without adding a production hook. The caller must reopen its closed store.
func CloseWorkflowAtDispatchForTest(
	t *testing.T, st *store.Store, path string, runID domain.RunID, at time.Time, filing bool,
) {
	t.Helper()
	taskID, ordinal := closeWorkflowForTest(t, st, runID, at)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE test_dispatch_closure AS
		SELECT * FROM task_workflow_closures WHERE task_id = ? AND start_ordinal = ?`,
		taskID, ordinal); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `DELETE FROM task_workflow_closures
		WHERE task_id = ? AND start_ordinal = ?`, taskID, ordinal); err != nil {
		t.Fatal(err)
	}
	event := `AFTER UPDATE OF status ON outbox
		WHEN NEW.status = 'dispatching' AND NEW.kind = 'publish.external_review_reply_intent'`
	if filing {
		event = "AFTER INSERT ON follow_up_filing_attempts"
	}
	if _, err := db.ExecContext(t.Context(), fmt.Sprintf(`CREATE TRIGGER test_close_after_dispatch %s
		BEGIN
			INSERT INTO task_workflow_closures SELECT * FROM test_dispatch_closure;
			DELETE FROM test_dispatch_closure;
		END`, event)); err != nil {
		t.Fatal(err)
	}
}

func closeWorkflowForTest(t *testing.T, st *store.Store, runID domain.RunID, at time.Time) (domain.TaskID, int) {
	t.Helper()
	var taskID domain.TaskID
	var ordinal int
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		run, err := tx.GetRun(t.Context(), runID)
		if err != nil {
			return err
		}
		taskID = run.TaskID
		task, err := tx.GetTask(t.Context(), taskID)
		if err != nil {
			return err
		}
		if task.CurrentStart() == nil {
			if err := tx.RecordTaskStart(t.Context(), runID, at); err != nil {
				return err
			}
			task, err = tx.GetTask(t.Context(), taskID)
			if err != nil {
				return err
			}
		}
		ordinal = task.CurrentStart().Ordinal
		_, err = tx.AbandonTask(t.Context(), taskID, at)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		_, _, err := tx.CloseTaskWorkflow(t.Context(), taskID, ordinal, at)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return taskID, ordinal
}
