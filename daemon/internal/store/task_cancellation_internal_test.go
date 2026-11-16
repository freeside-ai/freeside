package store

import (
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestTaskCancellationReconstructionRejectsCorruption(t *testing.T) {
	t.Parallel()
	for _, query := range []string{
		`UPDATE task_cancellations SET body=json_set(body,'$.state','confirmed')`,
		`UPDATE task_cancellations SET body=json_set(body,'$.target.project_id','foreign')`,
		`UPDATE task_cancellations SET target_digest='sha256:forged'`,
		`UPDATE task_stop_commands SET body=json_set(body,'$.task_id','foreign')`,
		`UPDATE task_stop_commands SET as_of_revision=0`,
		`UPDATE task_stop_commands SET body=json_set(body,'$.expected_entity_version', as_of_revision)`,
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

// A pending Stop with no acknowledgement passes the identity, fence, and
// version checks even when its as_of_revision column is corrupted above the
// current server revision; only the re-gate against current server state fails
// it closed, so replay cannot return a fabricated future revision that no
// revision progress can settle.
func TestTaskCancellationReconstructionRejectsFutureRevision(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := t.Context()
	var task domain.Task
	if err := s.Write(ctx, func(tx *WriteTx) error {
		var err error
		task, err = tx.getOrCreateTask(ctx, "project", "test", nil, time.Now().UTC())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	state, _ := s.ServerState(ctx)
	if err := s.Write(ctx, func(tx *WriteTx) error {
		_, _, err := tx.StopTask(ctx, domain.StopTaskRequest{CommandID: "stop", DeviceID: "device", TaskID: task.ID, ProjectID: task.ProjectID, ExpectedSyncEpoch: state.SyncEpoch, ExpectedEntityVersion: state.Revision}, time.Now().UTC())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Read(ctx, func(tx *ReadTx) error { _, _, err := tx.GetStopTaskReceipt(ctx, "stop"); return err }); err != nil {
		t.Fatalf("clean receipt rejected: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE task_stop_commands SET as_of_revision=as_of_revision+1000`); err != nil {
		t.Fatal(err)
	}
	err := s.Read(ctx, func(tx *ReadTx) error { _, _, err := tx.GetStopTaskReceipt(ctx, "stop"); return err })
	if !errors.Is(err, ErrRowInconsistent) {
		t.Fatalf("future receipt revision accepted: %v", err)
	}
}
