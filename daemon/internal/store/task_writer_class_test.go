package store_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

var taskWriterClassSetAt = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

func readTaskWriterClass(t *testing.T, s *store.Store, task domain.TaskID) (*domain.TaskWriterClass, error) {
	t.Helper()
	var got *domain.TaskWriterClass
	err := s.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		got, err = tx.TaskWriterClass(context.Background(), task)
		return err
	})
	return got, err
}

// TestTaskWriterClassSetsReadsAndClears sets a task's requirement, reads it
// back, replaces it, and clears it; a task with none reads as nil.
func TestTaskWriterClassSetsReadsAndClears(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStoreAt(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	taskID, _ := seedTaskSubmissionTarget(t, ctx, s)
	if got, err := readTaskWriterClass(t, s, taskID); err != nil || got != nil {
		t.Fatalf("before any set: %+v, %v; want nil, nil", got, err)
	}
	requirement := domain.TaskWriterClass{
		TaskID: taskID, Class: domain.BackendFreshVMReadOnlyVolumeHandoff, SetBy: "cli:operator", SetAt: taskWriterClassSetAt,
	}
	for _, setBy := range []string{"cli:operator", "cli:other"} {
		requirement.SetBy = setBy
		if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
			return tx.SetTaskWriterClass(ctx, requirement)
		}); err != nil {
			t.Fatal(err)
		}
		got, err := readTaskWriterClass(t, s, taskID)
		if err != nil || got == nil || *got != requirement {
			t.Fatalf("after set by %s: %+v, %v; want %+v", setBy, got, err, requirement)
		}
	}
	for _, want := range []bool{true, false} {
		var removed bool
		if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
			var err error
			removed, err = tx.ClearTaskWriterClass(ctx, taskID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if removed != want {
			t.Fatalf("clear removed %t, want %t", removed, want)
		}
	}
	if got, err := readTaskWriterClass(t, s, taskID); err != nil || got != nil {
		t.Fatalf("after clear: %+v, %v; want nil, nil", got, err)
	}
}

// TestTaskWriterClassRefusesAMissingTaskAndAnotherClass refuses a set or
// clear on a task that doesn't exist and a set naming the cordon.
func TestTaskWriterClassRefusesAMissingTaskAndAnotherClass(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStoreAt(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	taskID, _ := seedTaskSubmissionTarget(t, ctx, s)
	missing := domain.TaskWriterClass{
		TaskID: "task-missing", Class: domain.BackendFreshVMReadOnlyVolumeHandoff, SetBy: "cli:operator", SetAt: taskWriterClassSetAt,
	}
	err := s.WriteInternal(ctx, func(tx *store.InternalTx) error { return tx.SetTaskWriterClass(ctx, missing) })
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("set on a missing task: %v, want ErrNotFound", err)
	}
	err = s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.ClearTaskWriterClass(ctx, "task-missing")
		return err
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("clear on a missing task: %v, want ErrNotFound", err)
	}
	cordoned := missing
	cordoned.TaskID, cordoned.Class = taskID, domain.BackendCordonedHostProcess
	err = s.WriteInternal(ctx, func(tx *store.InternalTx) error { return tx.SetTaskWriterClass(ctx, cordoned) })
	if !errors.Is(err, domain.ErrTaskWriterClassNotWarded) {
		t.Fatalf("set naming the cordon: %v, want ErrTaskWriterClassNotWarded", err)
	}
}

// TestTaskWriterClassRefusesATaskOfAProjectWithNoImage refuses the warded
// requirement for a task whose project binds its recipe with no image.
func TestTaskWriterClassRefusesATaskOfAProjectWithNoImage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStoreAt(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	taskID, _ := seedTaskSubmissionTarget(t, ctx, s)
	binding := `{"repository":"acme/widgets","repository_id":42,"commit_sha":"` + hex64[:40] +
		`","recipe_digest":"sha256:` + hex64 + `"}`
	if _, err := appendProjectSettings(t, s, "project", domain.ProjectSectionRecipeBinding, binding); err != nil {
		t.Fatal(err)
	}
	requirement := domain.TaskWriterClass{
		TaskID: taskID, Class: domain.BackendFreshVMReadOnlyVolumeHandoff, SetBy: "cli:operator", SetAt: taskWriterClassSetAt,
	}
	err := s.WriteInternal(ctx, func(tx *store.InternalTx) error { return tx.SetTaskWriterClass(ctx, requirement) })
	if !errors.Is(err, domain.ErrWardedWithoutProjectImage) {
		t.Fatalf("set on a no-image project's task: %v, want ErrWardedWithoutProjectImage", err)
	}
}

// TestTaskWriterClassReadFailsOnAStoredRowNamingAnotherClass rewrites a
// stored requirement to the cordon: the read refuses it rather than hand
// the resolver a class a task may not require.
func TestTaskWriterClassReadFailsOnAStoredRowNamingAnotherClass(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "freeside.db")
	s := openStoreAt(t, path, store.Options{})
	taskID, _ := seedTaskSubmissionTarget(t, ctx, s)
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.SetTaskWriterClass(ctx, domain.TaskWriterClass{
			TaskID: taskID, Class: domain.BackendFreshVMReadOnlyVolumeHandoff, SetBy: "cli:operator", SetAt: taskWriterClassSetAt,
		})
	}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, `UPDATE task_writer_classes SET class = 'cordoned_host_process'`); err != nil {
		t.Fatal(err)
	}
	if _, err := readTaskWriterClass(t, s, taskID); !errors.Is(err, domain.ErrTaskWriterClassNotWarded) {
		t.Fatalf("read after rewrite: %v, want ErrTaskWriterClassNotWarded", err)
	}
}
