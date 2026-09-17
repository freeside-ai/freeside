package store

import (
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestCancellationCompletionOrderAndHistoricalProjection(t *testing.T) {
	for _, order := range []string{"complete-first", "stop-first", "historical-confirmation", "late-old-result", "migration-pair"} {
		t.Run(order, func(t *testing.T) {
			s := openTestStore(t)
			ctx := t.Context()
			now := time.Now().UTC()
			var id domain.TaskID
			if err := s.Write(ctx, func(tx *WriteTx) error {
				task, err := tx.getOrCreateTask(ctx, "project", "fixture", nil, now)
				if err != nil {
					return err
				}
				id = task.ID
				for _, runID := range []domain.RunID{"spec", "implementation", "retry"} {
					if err := tx.PutRun(ctx, domain.Run{ID: runID, ProjectID: "project", TaskID: id, SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy", Stages: []domain.Stage{}}); err != nil {
						return err
					}
				}
				if order == "migration-pair" {
					return tx.RecordTaskLifecycleFact(ctx, id, domain.TaskLifecycleFact{Kind: domain.TaskLifecycleStarted, RunID: "retry", SourceID: "migration:start", RecordedAt: now})
				}
				return tx.RecordTaskStart(ctx, "spec", now)
			}); err != nil {
				t.Fatal(err)
			}
			// Only the lifecycle reconstruction's authoritative completion binding
			// is exercised here; full publication evidence has its own store tests.
			if _, err := s.db.ExecContext(ctx, `INSERT INTO work_unit_declarations VALUES ('workunit-implementation', 'implementation', 'project', '{}', ?)`, formatTime(now)); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.ExecContext(ctx, `INSERT INTO work_unit_completions VALUES ('workunit-implementation', '{}', ?)`, formatTime(now)); err != nil {
				t.Fatal(err)
			}
			complete := func() {
				t.Helper()
				if err := s.Write(ctx, func(tx *WriteTx) error {
					if order == "migration-pair" {
						return tx.RecordTaskLifecycleFact(ctx, id, domain.TaskLifecycleFact{Kind: domain.TaskLifecycleCompleted, RunID: "implementation", SourceID: "migration:complete", BindingUnitID: new(domain.WorkUnitID("workunit-implementation")), RecordedAt: now})
					}
					return tx.RecordTaskCompletion(ctx, "implementation", "workunit-implementation", now)
				}); err != nil {
					t.Fatal(err)
				}
			}
			if order == "late-old-result" {
				if err := s.Write(ctx, func(tx *WriteTx) error {
					if _, err := tx.AbandonTask(ctx, id, now); err != nil {
						return err
					}
					return tx.AdmitTaskStart(ctx, "retry", now, 1)
				}); err != nil {
					t.Fatal(err)
				}
				complete()
				if err := s.Read(ctx, func(tx *ReadTx) error {
					task, err := tx.GetTask(ctx, id)
					if err != nil {
						return err
					}
					if !domain.TaskWIP(task) || task.LifecycleFacts[3].EpisodeOrdinal != 1 {
						t.Fatalf("late completion released new episode: %+v", task.LifecycleFacts)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				return
			}
			if order == "complete-first" || order == "migration-pair" {
				complete()
			}
			state, _ := s.ServerState(ctx)
			var receipt domain.StopTaskReceipt
			if err := s.Write(ctx, func(tx *WriteTx) error {
				var err error
				receipt, _, err = tx.StopTask(ctx, domain.StopTaskRequest{CommandID: "stop", DeviceID: "device", TaskID: id, ProjectID: "project", ExpectedSyncEpoch: state.SyncEpoch, ExpectedEntityVersion: state.Revision}, now)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			c := receipt.Cancellation
			ack := domain.TaskCancellationAcknowledgement{ID: "ack", RequestID: c.RequestID, TargetDigest: c.TargetDigest, State: domain.TaskCancellationConfirmed, EvidenceDigest: c.TargetDigest, RecordedAt: now}
			if order == "historical-confirmation" {
				body, err := encode(ack)
				if err != nil {
					t.Fatal(err)
				}
				// #1367 could persist helper-created confirmations before lifecycle
				// release existed. Upgrade reads them without fabricating new facts.
				if _, err := s.db.ExecContext(ctx, `INSERT INTO task_cancellation_acknowledgements VALUES (?, ?, ?, ?)`, ack.ID, ack.RequestID, c.FenceRevision, body); err != nil {
					t.Fatal(err)
				}
			} else if err := s.Write(ctx, func(tx *WriteTx) error { _, err := tx.AcknowledgeTaskCancellation(ctx, ack); return err }); err != nil {
				t.Fatal(err)
			}
			if order == "stop-first" {
				complete()
			}
			if err := s.Read(ctx, func(tx *ReadTx) error {
				task, err := tx.GetTask(ctx, id)
				if err != nil {
					return err
				}
				wantFacts := map[string]int{"complete-first": 2, "stop-first": 3, "historical-confirmation": 1, "migration-pair": 2}[order]
				wantLifecycle := domain.TaskFinished
				if order == "historical-confirmation" {
					wantLifecycle = domain.TaskStopped
				}
				if domain.TaskWIP(task) || len(task.LifecycleFacts) != wantFacts || *task.DisplayLifecycle(new(domain.RunLifecycleActive)) != wantLifecycle {
					t.Fatalf("order %s: %+v", order, task)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := s.Write(ctx, func(tx *WriteTx) error { return tx.AdmitTaskStart(ctx, "retry", now, 1) }); !errors.Is(err, ErrTaskCancellationFenced) {
				t.Fatalf("confirmed readmission: %v", err)
			}
		})
	}
}
