package store

import (
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestWorkflowClosureBindsEpisodeAndPreservesNewAdmission(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	var taskID domain.TaskID
	putRun := func(tx *WriteTx, id domain.RunID) error {
		return tx.PutRun(ctx, domain.Run{
			ID: id, TaskID: taskID, ProjectID: "project",
			SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy", Stages: []domain.Stage{},
		})
	}
	if err := s.Write(ctx, func(tx *WriteTx) error {
		if err := putRun(tx, "first"); err != nil {
			return err
		}
		run, err := tx.GetRun(ctx, "first")
		if err != nil {
			return err
		}
		taskID = run.TaskID
		if err := tx.RecordTaskStart(ctx, "first", now); err != nil {
			return err
		}
		if err := putRun(tx, "continuation"); err != nil {
			return err
		}
		return tx.RecordTaskStart(ctx, "continuation", now)
	}); err != nil {
		t.Fatal(err)
	}
	taskNotice := func(id domain.ItemID) domain.AttentionItem {
		t.Helper()
		posture := domain.HealthPostureAdvisory
		item, err := domain.NewAttentionItem(domain.AttentionItemInput{
			ID: id, ProjectID: "project", Type: domain.AttentionSystemHealth,
			Subject:  domain.Subject{Type: domain.SubjectTask, ID: domain.SubjectID(taskID), TaskID: &taskID},
			Priority: domain.PriorityNormal, Reason: "The producing run needs attention.",
			RequestedDecision: []domain.Action{domain.ActionAcknowledge}, ItemVersion: 1,
			InterruptionClass: domain.InterruptionExceptional, Status: domain.StatusOpen,
			HealthDiagnostic: &domain.HealthDiagnostic{Code: "work_held", Impairs: domain.ImpairedCapabilityNone},
			Posture:          &posture,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return item
	}
	oldNotice := taskNotice("old-notice")
	if err := s.Write(ctx, func(tx *WriteTx) error {
		return tx.PutTaskAttentionItem(ctx, oldNotice, "continuation")
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(ctx, func(tx *WriteTx) error {
		_, _, err := tx.CloseTaskWorkflow(ctx, taskID, 1, now)
		return err
	}); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("closure without abandonment = %v", err)
	}
	if err := s.Write(ctx, func(tx *WriteTx) error {
		released, err := tx.AbandonTask(ctx, taskID, now)
		if !released {
			t.Error("first abandonment did not release its slot")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Shared abandonment callers still release only a slot.
	if err := s.Read(ctx, func(tx *ReadTx) error { return tx.RequireRunWorkflowOpen(ctx, "first") }); err != nil {
		t.Fatal(err)
	}
	var original TaskWorkflowClosure
	if err := s.Write(ctx, func(tx *WriteTx) error {
		closure, created, err := tx.CloseTaskWorkflow(ctx, taskID, 1, now)
		if !created || len(closure.RunIDs) != 2 {
			t.Fatalf("closure = %+v, created = %v", closure, created)
		}
		original = closure
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Intake can admit a run reserved before closure without calling PutRun
	// again. Its new start must not remove that run from the captured episode.
	if err := s.Write(ctx, func(tx *WriteTx) error {
		return tx.AdmitTaskStart(ctx, "continuation", now.Add(time.Second), 1)
	}); !errors.Is(err, ErrTaskWorkflowClosed) {
		t.Fatalf("reserved run escaped closed episode: %v", err)
	}
	if err := s.Read(ctx, func(tx *ReadTx) error {
		if _, err := tx.GetTaskWorkflowClosure(ctx, taskID, 1); err != nil {
			return err
		}
		task, err := tx.GetTask(ctx, taskID)
		if err == nil && domain.TaskWIP(task) {
			t.Fatal("refused admission acquired a slot")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(ctx, func(tx *WriteTx) error {
		return putRun(tx, "ordinary-submission")
	}); !errors.Is(err, ErrTaskWorkflowClosed) {
		t.Fatalf("ordinary submission extended closed membership: %v", err)
	}
	if err := s.Write(ctx, func(tx *WriteTx) error {
		if err := putRun(tx, "new-episode"); err != nil {
			return err
		}
		return tx.AdmitTaskStart(ctx, "new-episode", now.Add(time.Second), 1)
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Read(ctx, func(tx *ReadTx) error {
		for _, id := range []domain.RunID{"first", "continuation"} {
			if err := tx.RequireRunWorkflowOpen(ctx, id); !errors.Is(err, ErrTaskWorkflowClosed) {
				t.Fatalf("old run %s = %v", id, err)
			}
		}
		return tx.RequireRunWorkflowOpen(ctx, "new-episode")
	}); err != nil {
		t.Fatal(err)
	}
	newNotice := taskNotice("new-notice")
	if err := s.Write(ctx, func(tx *WriteTx) error {
		return tx.PutTaskAttentionItem(ctx, newNotice, "new-episode")
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Read(ctx, func(tx *ReadTx) error {
		if err := tx.RequireItemWorkflowOpen(ctx, oldNotice); !errors.Is(err, ErrTaskWorkflowClosed) {
			t.Fatalf("old task-only notice lost origin: %v", err)
		}
		return tx.RequireItemWorkflowOpen(ctx, newNotice)
	}); err != nil {
		t.Fatal(err)
	}
	// Delayed repair uses the old ordinal, never the current episode.
	if err := s.Write(ctx, func(tx *WriteTx) error {
		closure, created, err := tx.CloseTaskWorkflow(ctx, taskID, 1, now.Add(time.Hour))
		if created || closure.Revision != original.Revision || !closure.ClosedAt.Equal(original.ClosedAt) {
			t.Fatalf("repair changed closure: %+v", closure)
		}
		task, readErr := tx.GetTask(ctx, taskID)
		if readErr != nil {
			return readErr
		}
		if !domain.TaskWIP(task) || task.CurrentStart().RunID != "new-episode" {
			t.Fatal("repair affected newer admission")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE task_attention_workflow_origins
		SET run_id = 'new-episode' WHERE item_id = 'old-notice'`); err != nil {
		t.Fatal(err)
	}
	if err := s.Read(ctx, func(tx *ReadTx) error {
		return tx.RequireItemWorkflowOpen(ctx, oldNotice)
	}); err == nil || errors.Is(err, ErrTaskWorkflowClosed) {
		t.Fatalf("rewritten task-only origin accepted: %v", err)
	}
}

func TestWorkflowClosureRejectsCorruptAuthority(t *testing.T) {
	t.Parallel()
	for _, change := range []string{
		`UPDATE task_workflow_closures SET start_ordinal = 2`,
		`UPDATE task_workflow_closures SET start_run_id = 'foreign'`,
		`UPDATE task_workflow_closures SET revision = revision + 1`,
		`UPDATE task_workflow_closures SET body = json_set(body, '$.task_id', 'foreign')`,
		`UPDATE task_workflow_closures SET body = json_set(body, '$.run_ids[0]', 'foreign')`,
		`UPDATE task_lifecycle_facts SET source_id = 'unbound-abandonment' WHERE kind = 'abandoned'`,
	} {
		t.Run(change, func(t *testing.T) {
			s := openTestStore(t)
			ctx := t.Context()
			var taskID domain.TaskID
			if err := s.Write(ctx, func(tx *WriteTx) error {
				for _, id := range []domain.RunID{"owned", "foreign"} {
					if err := tx.PutRun(ctx, domain.Run{ID: id, ProjectID: "project", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy"}); err != nil {
						return err
					}
				}
				run, err := tx.GetRun(ctx, "owned")
				if err != nil {
					return err
				}
				taskID = run.TaskID
				if err := tx.RecordTaskStart(ctx, run.ID, time.Now().UTC()); err != nil {
					return err
				}
				if _, err := tx.AbandonTask(ctx, taskID, time.Now().UTC()); err != nil {
					return err
				}
				_, _, err = tx.CloseTaskWorkflow(ctx, taskID, 1, time.Now().UTC())
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.ExecContext(ctx, change); err != nil {
				t.Fatal(err)
			}
			if err := s.Read(ctx, func(tx *ReadTx) error {
				return tx.RequireRunWorkflowOpen(ctx, "owned")
			}); err == nil || errors.Is(err, ErrTaskWorkflowClosed) {
				t.Fatalf("corruption accepted as ordinary closure: %v", err)
			}
		})
	}
}
