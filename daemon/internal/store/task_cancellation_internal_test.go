package store

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

func TestTaskCancellationMigrationPreservesHistory(t *testing.T) {
	ctx := t.Context()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0070_")
	dump, err := os.ReadFile(filepath.Join("testdata", "pre_tasks_started_label_intake.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, string(dump)); err != nil {
		t.Fatal(err)
	}
	migrateThrough(t, ctx, db, "0076_")
	queries := []string{`SELECT * FROM tasks ORDER BY id`, `SELECT * FROM runs ORDER BY id`, `SELECT * FROM commands ORDER BY command_id`, `SELECT * FROM task_submission_commands ORDER BY command_id`, `SELECT * FROM task_lifecycle_facts ORDER BY task_id, ordinal`, `SELECT * FROM work_unit_completions ORDER BY unit_id`, `SELECT * FROM server_state`}
	before := make([][][]any, len(queries))
	for i, q := range queries {
		before[i] = manualMigrationRows(t, db, q)
	}
	for range 2 {
		if err := migrate(ctx, db, migrations.FS); err != nil {
			t.Fatal(err)
		}
	}
	for i, q := range queries {
		if !reflect.DeepEqual(before[i], manualMigrationRows(t, db, q)) {
			t.Fatalf("changed historical state: %s", q)
		}
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM task_cancellations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("fabricated cancellation history")
	}
}

func TestTaskCancellationReconstructionRejectsCorruption(t *testing.T) {
	for _, query := range []string{
		`UPDATE task_cancellations SET body=json_set(body,'$.state','confirmed')`,
		`UPDATE task_cancellations SET body=json_set(body,'$.target.project_id','foreign')`,
		`UPDATE task_cancellations SET target_digest='sha256:forged'`,
		`UPDATE task_stop_commands SET body=json_set(body,'$.task_id','foreign')`,
		`UPDATE task_stop_commands SET as_of_revision=0`,
		`UPDATE task_cancellation_acknowledgements SET body=json_set(body,'$.request_id','foreign')`,
		`UPDATE task_cancellation_acknowledgements SET body=json_set(body,'$.target_digest','sha256:forged')`,
	} {
		t.Run(query, func(t *testing.T) {
			s := openTestStore(t)
			ctx := t.Context()
			var task domain.Task
			var receipt domain.StopTaskReceipt
			if err := s.Write(ctx, func(tx *WriteTx) error {
				var err error
				task, err = tx.getOrCreateTask(ctx, "project", "test", nil, time.Now().UTC())
				return err
			}); err != nil {
				t.Fatal(err)
			}
			state, _ := s.ServerState(ctx)
			if err := s.Write(ctx, func(tx *WriteTx) error {
				var err error
				receipt, _, err = tx.StopTask(ctx, domain.StopTaskRequest{CommandID: "stop", DeviceID: "device", TaskID: task.ID, ProjectID: task.ProjectID, ExpectedSyncEpoch: state.SyncEpoch, ExpectedEntityVersion: state.Revision}, time.Now().UTC())
				return err
			}); err != nil {
				t.Fatal(err)
			}
			c := receipt.Cancellation
			if err := s.Write(ctx, func(tx *WriteTx) error {
				_, err := tx.AcknowledgeTaskCancellation(ctx, domain.TaskCancellationAcknowledgement{ID: "ack", RequestID: c.RequestID, TargetDigest: c.TargetDigest, State: domain.TaskCancellationFailed, EvidenceDigest: c.TargetDigest, RecordedAt: c.RequestedAt.Add(time.Second)})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.ExecContext(ctx, query); err != nil {
				t.Fatal(err)
			}
			err := s.Read(ctx, func(tx *ReadTx) error { _, _, err := tx.GetStopTaskReceipt(ctx, "stop"); return err })
			if !errors.Is(err, ErrRowInconsistent) {
				t.Fatalf("corrupt row accepted: %v", err)
			}
		})
	}
}
