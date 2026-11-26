package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Task writer classes (migration 0149) hold a task's requirement that its
// writers run warded (#3471). The record decides which containment a
// writer runs under, so every read re-runs domain.TaskWriterClass.Validate:
// a stored row naming any class but the warded one fails the read rather
// than reaching the resolver. The rows are daemon-internal, so the writes
// live on InternalTx.

const setTaskWriterClassSQL = `
INSERT INTO task_writer_classes (task_id, class, set_by, set_at)
VALUES (?1, ?2, ?3, ?4)
ON CONFLICT (task_id) DO UPDATE SET class = excluded.class, set_by = excluded.set_by, set_at = excluded.set_at`

// SetTaskWriterClass stores the task's requirement, replacing any earlier
// one. It refuses a requirement Validate refuses, a task that doesn't exist
// (ErrNotFound), and a task whose project has no project image
// (domain.ErrWardedWithoutProjectImage): its settings hold a recipe_binding,
// and the warded writer runs in the project image. A binding set after the
// requirement isn't caught here; the writer-class resolver refuses that
// task's writer at admission.
func (tx *InternalTx) SetTaskWriterClass(ctx context.Context, requirement domain.TaskWriterClass) error {
	if err := requirement.Validate(); err != nil {
		return fmt.Errorf("set task writer class: %w", err)
	}
	task, err := tx.GetTask(ctx, requirement.TaskID)
	if err != nil {
		return fmt.Errorf("set task %q writer class: %w", requirement.TaskID, err)
	}
	settings, err := tx.ProjectSettingsRecord(ctx, task.ProjectID)
	if err != nil {
		return fmt.Errorf("set task %q writer class: %w", requirement.TaskID, err)
	}
	binding, err := settings.RecipeBinding()
	if err != nil {
		return fmt.Errorf("set task %q writer class: %w", requirement.TaskID, err)
	}
	if binding != nil {
		return fmt.Errorf("set task %q writer class %q: project %q: %w",
			requirement.TaskID, requirement.Class, task.ProjectID, domain.ErrWardedWithoutProjectImage)
	}
	if _, err := tx.tx.ExecContext(ctx, setTaskWriterClassSQL, requirement.TaskID, requirement.Class,
		requirement.SetBy, formatTime(requirement.SetAt)); err != nil {
		return fmt.Errorf("set task %q writer class: %w", requirement.TaskID, err)
	}
	return nil
}

// ClearTaskWriterClass removes the task's requirement and reports whether
// it had one. It refuses a task that doesn't exist (ErrNotFound).
func (tx *InternalTx) ClearTaskWriterClass(ctx context.Context, task domain.TaskID) (bool, error) {
	if _, err := tx.GetTask(ctx, task); err != nil {
		return false, fmt.Errorf("clear task %q writer class: %w", task, err)
	}
	result, err := tx.tx.ExecContext(ctx, `DELETE FROM task_writer_classes WHERE task_id = ?`, task)
	if err != nil {
		return false, fmt.Errorf("clear task %q writer class: %w", task, err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("clear task %q writer class: %w", task, err)
	}
	return removed > 0, nil
}

// TaskWriterClass returns the task's requirement, or nil when it has none.
// A stored row that fails validation fails the read.
func (tx *ReadTx) TaskWriterClass(ctx context.Context, task domain.TaskID) (*domain.TaskWriterClass, error) {
	var id, class, setBy, setAt string
	err := tx.tx.QueryRowContext(ctx,
		`SELECT task_id, class, set_by, set_at FROM task_writer_classes WHERE task_id = ?`, task).
		Scan(&id, &class, &setBy, &setAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get task %q writer class: %w", task, err)
	}
	at, err := parseTime(setAt)
	if err != nil {
		return nil, fmt.Errorf("task %q writer class set_at: %w", task, err)
	}
	requirement := domain.TaskWriterClass{
		TaskID: domain.TaskID(id), Class: domain.RunnerBackendClass(class), SetBy: setBy, SetAt: at,
	}
	if err := requirement.Validate(); err != nil {
		return nil, fmt.Errorf("get task %q writer class: %w", task, err)
	}
	return &requirement, nil
}
