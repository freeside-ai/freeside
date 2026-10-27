package store

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

func TestWorkflowClosureAuthenticatesLegacyHeldWorkNotices(t *testing.T) {
	for _, prefix := range []string{"work-held-", "raised-work-held-"} {
		t.Run(prefix, func(t *testing.T) {
			st := openTestStore(t)
			ctx := t.Context()
			now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
			var taskID domain.TaskID
			var oldNotice, newNotice domain.AttentionItem
			if err := st.Write(ctx, func(tx *WriteTx) error {
				for i, id := range []domain.RunID{"legacy-first-run", "legacy-next-run"} {
					if err := tx.PutRun(ctx, domain.Run{
						ID: id, TaskID: taskID, ProjectID: "legacy-project",
						SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
					}); err != nil {
						return err
					}
					run, err := tx.GetRun(ctx, id)
					if err != nil {
						return err
					}
					taskID = run.TaskID
					var startErr error
					if i == 0 {
						startErr = tx.RecordTaskStart(ctx, id, now)
					} else {
						startErr = tx.AdmitTaskStart(ctx, id, now, 1)
					}
					if startErr != nil {
						return startErr
					}
					posture := domain.HealthPostureAdvisory
					suffix := now.Unix()
					if prefix == "raised-work-held-" {
						suffix = tx.asOfRevision
					}
					item, err := domain.NewAttentionItem(domain.AttentionItemInput{
						ID:        domain.ItemID(fmt.Sprintf("%s%s-%d", prefix, id, suffix)),
						ProjectID: run.ProjectID, Type: domain.AttentionSystemHealth,
						Subject:  domain.Subject{Type: domain.SubjectTask, ID: domain.SubjectID(taskID), TaskID: &taskID},
						Priority: domain.PriorityHigh,
						Reason: fmt.Sprintf("Run %s has been held since %s with hold reason %s. "+
							"Freeside keeps retrying it. "+
							"This notice resolves itself when the hold clears, its cause changes, or the task is stopped.",
							id, now.Format(time.RFC3339), domain.HoldAdmissionPolicyRefused),
						RequestedDecision: []domain.Action{domain.ActionAcknowledge}, ItemVersion: 1,
						InterruptionClass: domain.InterruptionExceptional, CreatedAt: &now,
						Posture: &posture, Status: domain.StatusOpen,
						HealthDiagnostic: &domain.HealthDiagnostic{
							Code: "work_held", Impairs: domain.ImpairedCapabilityUnattendedAdmission,
						},
					}, nil)
					if err != nil {
						return err
					}
					// Both pre-0100 producers used the ordinary writer, so
					// their original producing run has no private origin row.
					if err := tx.PutAttentionItem(ctx, item); err != nil {
						return err
					}
					if i == 0 {
						oldNotice = item
					} else {
						newNotice = item
					}
					if _, err := tx.AbandonTask(ctx, taskID, now); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			for _, statement := range []string{
				`ALTER TABLE run_hold_observations DROP COLUMN detail_check`,
				`ALTER TABLE run_hold_observations DROP COLUMN detail_agent`,
				`ALTER TABLE run_hold_observations DROP COLUMN detail_launch_stage`,
				`ALTER TABLE run_hold_observations DROP COLUMN detail_launch_digest`,
				`DROP TABLE work_unit_rebase_proofs`,
				`DROP TABLE task_attention_workflow_origins`,
				`DROP TABLE task_workflow_closures`,
				`ALTER TABLE execution_outcomes DROP COLUMN cause`,
				`DROP TABLE admitted_image_pointers`,
				`ALTER TABLE run_hold_observations DROP COLUMN upkeep_pending`,
				`ALTER TABLE run_hold_observations DROP COLUMN upkeep_refusal_id`,
				`ALTER TABLE run_hold_observations DROP COLUMN upkeep_item_id`,
				`ALTER TABLE run_hold_observations DROP COLUMN upkeep_task_id`,
				`DROP TABLE repository_tip_observations`,
				`DROP TABLE image_policy_adoptions`,
				`DROP TABLE image_upkeep_refusals`,
				`DELETE FROM schema_migrations WHERE name IN
					('0100_task_workflow_closures.sql', '0101_work_unit_rebase_proofs.sql',
					'0102_usage_observations_review.sql', '0103_run_hold_admission_detail.sql',
					'0104_execution_outcome_cause.sql',
					'0105_admitted_image_pointers.sql',
					'0106_schedules_image_upkeep.sql',
					'0107_run_hold_image_upkeep.sql',
					'0108_image_upkeep.sql',
					'0109_task_submission_origin.sql',
					'0110_image_policy_adoption.sql')`,
			} {
				if _, err := st.db.ExecContext(ctx, statement); err != nil {
					t.Fatal(err)
				}
			}
			if err := migrate(ctx, st.db, migrations.FS); err != nil {
				t.Fatal(err)
			}
			if err := st.Read(ctx, func(tx *ReadTx) error {
				closures, err := tx.ListTaskWorkflowClosures(ctx)
				if err == nil && len(closures) != 0 {
					t.Fatal("upgrade inferred closure from old abandonment")
				}
				for name, change := range map[string]func(*domain.AttentionItem){
					"unknown producer": func(item *domain.AttentionItem) { item.ID = "unknown-notice" },
					"missing run": func(item *domain.AttentionItem) {
						item.ID = domain.ItemID(fmt.Sprintf("%smissing-run-1", prefix))
					},
					"wrong run":     func(item *domain.AttentionItem) { item.ID = newNotice.ID },
					"wrong project": func(item *domain.AttentionItem) { item.ProjectID = "other-project" },
					"wrong task": func(item *domain.AttentionItem) {
						other := domain.TaskID("other-task")
						item.Subject.TaskID = &other
						item.Subject.ID = domain.SubjectID(other)
					},
					"wrong diagnostic": func(item *domain.AttentionItem) {
						item.HealthDiagnostic = &domain.HealthDiagnostic{
							Code: "other", Impairs: domain.ImpairedCapabilityUnattendedAdmission,
						}
					},
					"wrong body": func(item *domain.AttentionItem) { item.Reason = "Run legacy-next-run needs help." },
				} {
					t.Run(name, func(t *testing.T) {
						item := oldNotice
						change(&item)
						if _, _, err := tx.AttentionItemWorkflowEpisode(ctx, item); err == nil {
							t.Fatal("contradictory or ambiguous legacy notice acquired ownership")
						}
					})
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := st.Write(ctx, func(tx *WriteTx) error {
				_, _, err := tx.CloseTaskWorkflow(ctx, taskID, 1, now)
				return err
			}); err != nil {
				t.Fatalf("close upgraded first episode: %v", err)
			}
			if err := st.Read(ctx, func(tx *ReadTx) error {
				if err := tx.RequireItemWorkflowOpen(ctx, oldNotice); !errors.Is(err, ErrTaskWorkflowClosed) {
					t.Fatalf("legacy old notice = %v, want closed", err)
				}
				return tx.RequireItemWorkflowOpen(ctx, newNotice)
			}); err != nil {
				t.Fatalf("old repair affected newer notice: %v", err)
			}
		})
	}
}

func TestWorkflowClosureBindsEpisodeAndPreservesNewAdmission(t *testing.T) {
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

func TestWorkflowClosureMigrationDoesNotInferAuthority(t *testing.T) {
	st := openTestStore(t)
	ctx := t.Context()
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, domain.Run{ID: "old-run", ProjectID: "project", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy"}); err != nil {
			return err
		}
		if err := tx.RecordTaskStart(ctx, "old-run", time.Now().UTC()); err != nil {
			return err
		}
		run, err := tx.GetRun(ctx, "old-run")
		if err != nil {
			return err
		}
		_, err = tx.AbandonTask(ctx, run.TaskID, time.Now().UTC())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before, err := st.ServerState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`ALTER TABLE run_hold_observations DROP COLUMN detail_check`,
		`ALTER TABLE run_hold_observations DROP COLUMN detail_agent`,
		`ALTER TABLE run_hold_observations DROP COLUMN detail_launch_stage`,
		`ALTER TABLE run_hold_observations DROP COLUMN detail_launch_digest`,
		`DROP TABLE work_unit_rebase_proofs`,
		`DROP TABLE task_attention_workflow_origins`,
		`DROP TABLE task_workflow_closures`,
		`ALTER TABLE execution_outcomes DROP COLUMN cause`,
		`DROP TABLE admitted_image_pointers`,
		`ALTER TABLE run_hold_observations DROP COLUMN upkeep_pending`,
		`ALTER TABLE run_hold_observations DROP COLUMN upkeep_refusal_id`,
		`ALTER TABLE run_hold_observations DROP COLUMN upkeep_item_id`,
		`ALTER TABLE run_hold_observations DROP COLUMN upkeep_task_id`,
		`DROP TABLE repository_tip_observations`,
		`DROP TABLE image_policy_adoptions`,
		`DROP TABLE image_upkeep_refusals`,
		`DELETE FROM schema_migrations WHERE name IN
					('0100_task_workflow_closures.sql', '0101_work_unit_rebase_proofs.sql',
					'0102_usage_observations_review.sql', '0103_run_hold_admission_detail.sql',
					'0104_execution_outcome_cause.sql',
					'0105_admitted_image_pointers.sql',
					'0106_schedules_image_upkeep.sql',
					'0107_run_hold_image_upkeep.sql',
					'0108_image_upkeep.sql',
					'0109_task_submission_origin.sql',
					'0110_image_policy_adoption.sql')`,
	} {
		if _, err := st.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate(ctx, st.db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	after, err := st.ServerState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision {
		t.Fatal("schema migration changed lifecycle revision")
	}
	if err := st.Read(ctx, func(tx *ReadTx) error {
		closures, err := tx.ListTaskWorkflowClosures(ctx)
		if err != nil {
			return err
		}
		if len(closures) != 0 {
			t.Fatal("migration invented administrative closure")
		}
		return tx.RequireRunWorkflowOpen(ctx, "old-run")
	}); err != nil {
		t.Fatal(err)
	}
}
