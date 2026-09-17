package store_test

import (
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
