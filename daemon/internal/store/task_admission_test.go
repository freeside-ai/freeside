package store_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestTaskAdmissionRejectsLateMilestoneAfterSlotIsTaken(t *testing.T) {
	s := openStore(t, store.Options{})
	ctx := t.Context()
	now := time.Now().UTC()
	var first, second domain.TaskID
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		for i, id := range []domain.RunID{"first", "second"} {
			source := taskIssueSource()
			source.IssueSubject.IssueNumber += i
			task, err := tx.GetOrCreateTask(ctx, "project", source)
			if err != nil {
				return err
			}
			if i == 0 {
				first = task.ID
			} else {
				second = task.ID
			}
			if err := tx.PutRun(ctx, domain.Run{ID: id, ProjectID: "project", TaskID: task.ID, SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy", Stages: []domain.Stage{}}); err != nil {
				return err
			}
		}
		if err := tx.AdmitTaskStart(ctx, "first", now, 1); err != nil {
			return err
		}
		if _, err := tx.AbandonTask(ctx, first, now); err != nil {
			return err
		}
		return tx.AdmitTaskStart(ctx, "second", now, 1)
	}); err != nil {
		t.Fatal(err)
	}
	for _, admitted := range []bool{false, true} {
		want := store.ErrTaskAdmissionRequired
		if admitted {
			want = store.ErrTaskWIPCapExhausted
		}
		err := s.Write(ctx, func(tx *store.WriteTx) error {
			if err := tx.PutRun(ctx, domain.Run{ID: "late", ProjectID: "project", TaskID: first, SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy", Stages: []domain.Stage{}}); err != nil {
				return err
			}
			if admitted {
				return tx.AdmitTaskStart(ctx, "late", now, 1)
			}
			return tx.RecordTaskStart(ctx, "late", now)
		})
		if !errors.Is(err, want) {
			t.Fatalf("admitted=%v: %v", admitted, err)
		}
	}
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		if _, err := tx.AbandonTask(ctx, second, now); err != nil {
			return err
		}
		if err := tx.PutRun(ctx, domain.Run{ID: "retry", ProjectID: "project", TaskID: first, SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy", Stages: []domain.Stage{}}); err != nil {
			return err
		}
		return tx.AdmitTaskStart(ctx, "retry", now, 1)
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		task, err := tx.GetTask(ctx, first)
		if err != nil {
			return err
		}
		if !domain.TaskWIP(task) || len(task.LifecycleFacts) != 3 || task.CurrentStart().RunID != "retry" {
			t.Fatalf("readmission: %+v", task)
		}
		_, err = tx.GetRun(ctx, "late")
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatal("rejected admission persisted late run")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// releasedRunFixture opens a store holding one task with the runs "released"
// and "other", then admits and abandons "released": the state label intake
// leaves when it releases an undecided admitted start (#1770).
func releasedRunFixture(t *testing.T, path string) (*store.Store, domain.TaskID) {
	t.Helper()
	s := openStoreAt(t, path, store.Options{})
	ctx := t.Context()
	now := time.Now().UTC()
	var taskID domain.TaskID
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		task, err := tx.GetOrCreateTask(ctx, "project", taskIssueSource())
		if err != nil {
			return err
		}
		taskID = task.ID
		for _, id := range []domain.RunID{"released", "other"} {
			if err := tx.PutRun(ctx, domain.Run{ID: id, ProjectID: "project", TaskID: task.ID, SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy", Stages: []domain.Stage{}}); err != nil {
				return err
			}
		}
		if err := tx.AdmitTaskStart(ctx, "released", now, 1); err != nil {
			return err
		}
		_, err = tx.AbandonTask(ctx, task.ID, now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return s, taskID
}

func readTask(t *testing.T, s *store.Store, id domain.TaskID) domain.Task {
	t.Helper()
	var task domain.Task
	if err := s.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		task, err = tx.GetTask(t.Context(), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return task
}

// TestRecordTaskStartAfterReleaseRestartsReleasedRun covers the restart and
// its episode: the run takes its slot again under a fresh source id, a replay
// records nothing, the restart owns the run's workflow episode, and a
// completion of the run releases the restarted slot.
func TestRecordTaskStartAfterReleaseRestartsReleasedRun(t *testing.T) {
	t.Parallel()
	path := tempDBPath(t)
	s, taskID := releasedRunFixture(t, path)
	ctx := t.Context()
	now := time.Now().UTC()
	for range 2 {
		if err := s.Write(ctx, func(tx *store.WriteTx) error {
			return tx.RecordTaskStartAfterRelease(ctx, "released", now)
		}); err != nil {
			t.Fatal(err)
		}
	}
	task := readTask(t, s, taskID)
	if !domain.TaskWIP(task) || len(task.LifecycleFacts) != 3 {
		t.Fatalf("restart: WIP = %v, facts = %+v", domain.TaskWIP(task), task.LifecycleFacts)
	}
	first, restart := task.LifecycleFacts[0], task.LifecycleFacts[2]
	if restart.Kind != domain.TaskLifecycleStarted || restart.RunID != "released" ||
		restart.SourceID == first.SourceID || restart.SourceID != "start:released:after:2" {
		t.Fatalf("restart fact = %+v, want a fresh start of the released run", restart)
	}
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		_, episode, err := tx.TaskWorkflowEpisode(ctx, "released")
		if err != nil {
			return err
		}
		if episode == nil || episode.Ordinal != restart.Ordinal {
			t.Fatalf("released run's episode = %+v, want the restart (ordinal %d)", episode, restart.Ordinal)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Only the completion binding is exercised here; publication evidence has
	// its own tests.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	for _, stmt := range []string{
		`INSERT INTO work_unit_declarations VALUES ('workunit-released', 'released', 'project', '{}', ?)`,
		`INSERT INTO work_unit_completions VALUES ('workunit-released', '{}', ?)`,
	} {
		if _, err := raw.ExecContext(ctx, stmt, now.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.RecordTaskCompletion(ctx, "released", "workunit-released", now)
	}); err != nil {
		t.Fatal(err)
	}
	if task := readTask(t, s, taskID); domain.TaskWIP(task) || task.LifecycleFacts[3].EpisodeOrdinal != restart.Ordinal {
		t.Fatalf("completion after restart: WIP = %v, facts = %+v", domain.TaskWIP(task), task.LifecycleFacts)
	}
}

// TestRecordTaskStartAfterReleaseRefusesOtherStates covers the refusals: a
// restart needs the run's own newest start followed by that start's bound
// abandonment, an open workflow episode, and no cancellation fence.
func TestRecordTaskStartAfterReleaseRefusesOtherStates(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	for _, tc := range []struct {
		name  string
		setup func(ctx context.Context, tx *store.WriteTx, taskID domain.TaskID) error
		run   domain.RunID
		want  error
	}{
		{"run never started", nil, "other", store.ErrTaskAdmissionRequired},
		{"newest start is another run's", func(ctx context.Context, tx *store.WriteTx, taskID domain.TaskID) error {
			if err := tx.AdmitTaskStart(ctx, "other", now, 1); err != nil {
				return err
			}
			_, err := tx.AbandonTask(ctx, taskID, now)
			return err
		}, "released", store.ErrTaskAdmissionRequired},
		{"newest start has no bound abandonment", func(ctx context.Context, tx *store.WriteTx, taskID domain.TaskID) error {
			if err := tx.RecordTaskStartAfterRelease(ctx, "released", now); err != nil {
				return err
			}
			// An abandonment not bound to the restart still releases the slot,
			// but it is not the released state a restart follows.
			return tx.RecordTaskLifecycleFact(ctx, taskID, domain.TaskLifecycleFact{
				Kind: domain.TaskLifecycleAbandoned, RunID: "released", SourceID: "abandon:unbound", RecordedAt: now,
			})
		}, "released", store.ErrTaskAdmissionRequired},
		{"operator closed the released episode", func(ctx context.Context, tx *store.WriteTx, taskID domain.TaskID) error {
			_, _, err := tx.CloseTaskWorkflow(ctx, taskID, 1, now)
			return err
		}, "released", store.ErrTaskWorkflowClosed},
		{"cancellation fences the task", func(ctx context.Context, tx *store.WriteTx, taskID domain.TaskID) error {
			state, err := tx.ServerState(ctx)
			if err != nil {
				return err
			}
			_, _, err = tx.StopTask(ctx, domain.StopTaskRequest{
				CommandID: "stop", DeviceID: "device", TaskID: taskID, ProjectID: "project",
				ExpectedSyncEpoch: state.SyncEpoch, ExpectedEntityVersion: state.Revision,
			}, now)
			return err
		}, "released", store.ErrTaskCancellationFenced},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, taskID := releasedRunFixture(t, tempDBPath(t))
			ctx := t.Context()
			if tc.setup != nil {
				if err := s.Write(ctx, func(tx *store.WriteTx) error { return tc.setup(ctx, tx, taskID) }); err != nil {
					t.Fatal(err)
				}
			}
			before := readTask(t, s, taskID)
			err := s.Write(ctx, func(tx *store.WriteTx) error {
				return tx.RecordTaskStartAfterRelease(ctx, tc.run, now)
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("restart %q: err = %v, want %v", tc.run, err, tc.want)
			}
			if after := readTask(t, s, taskID); len(after.LifecycleFacts) != len(before.LifecycleFacts) {
				t.Fatalf("refused restart recorded facts: %+v", after.LifecycleFacts)
			}
		})
	}
}
