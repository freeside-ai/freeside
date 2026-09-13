package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// GetOrCreateTask registers an intake key and a random identity in this write
// transaction. The source determines deduplication; it never determines the ID.
func (tx *WriteTx) GetOrCreateTask(ctx context.Context, projectID domain.ProjectID, source domain.SpecificationSource) (domain.Task, error) {
	key, err := tx.taskIntakeKey(ctx, source)
	if err != nil {
		return domain.Task{}, err
	}
	return tx.getOrCreateTask(ctx, projectID, key, &source, time.Now().UTC())
}

func (tx *ReadTx) taskIntakeKey(ctx context.Context, source domain.SpecificationSource) (string, error) {
	if err := source.Validate(); err != nil {
		return "", err
	}
	switch source.Kind {
	case domain.SpecificationSourceWorkItemArtifact:
		artifact, err := tx.GetArtifact(ctx, source.WorkItemArtifactID)
		if err != nil {
			return "", err
		}
		if artifact.Type != domain.ArtifactKindSpecification {
			return "", domain.ErrParentKeyMismatch
		}
		return "source:" + string(artifact.Digest), nil
	case domain.SpecificationSourceIssueSubject:
		return fmt.Sprintf("issue:%d:%d", source.IssueSubject.RepositoryID, source.IssueSubject.IssueNumber), nil
	}
	return "", domain.ErrInvalidSpecificationSourceKind
}

func (tx *WriteTx) getOrCreateTask(ctx context.Context, projectID domain.ProjectID, key string, source *domain.SpecificationSource, createdAt time.Time) (domain.Task, error) {
	id := domain.TaskID("task-" + rand.Text())
	result, err := tx.tx.ExecContext(ctx, `INSERT INTO task_intake_keys (project_id, intake_key, task_id)
		VALUES (?, ?, ?) ON CONFLICT (project_id, intake_key) DO NOTHING`, projectID, key, id)
	if err != nil {
		return domain.Task{}, fmt.Errorf("register task intake: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return domain.Task{}, err
	}
	if inserted != 0 {
		task := domain.Task{
			ID: id, ProjectID: projectID, Source: source,
			Name:      domain.DisplayName{Text: string(id), Source: domain.DisplayNameSourceIdentifier},
			CreatedAt: createdAt, CampaignIDs: []domain.CampaignID{},
		}
		body, err := encode(task)
		if err != nil {
			return domain.Task{}, err
		}
		if _, err := tx.tx.ExecContext(ctx, `INSERT INTO tasks (id, project_id, entity_version, as_of_revision, body)
			VALUES (?, ?, 1, ?, ?)`, id, projectID, tx.asOfRevision, body); err != nil {
			return domain.Task{}, err
		}
	}
	if err := tx.tx.QueryRowContext(ctx, `SELECT task_id FROM task_intake_keys WHERE project_id = ? AND intake_key = ?`, projectID, key).Scan(&id); err != nil {
		return domain.Task{}, err
	}
	return tx.GetTask(ctx, id)
}

func (tx *ReadTx) GetTask(ctx context.Context, id domain.TaskID) (domain.Task, error) {
	snapshot, err := tx.GetTaskSnapshot(ctx, id)
	return snapshot.Value, err
}

func (tx *ReadTx) GetTaskSnapshot(ctx context.Context, id domain.TaskID) (Snapshotted[domain.Task], error) {
	var projectID domain.ProjectID
	var body []byte
	var snapshot Snapshot
	if err := tx.tx.QueryRowContext(ctx, `SELECT project_id, entity_version, as_of_revision, body FROM tasks WHERE id = ?`, id).
		Scan(&projectID, &snapshot.EntityVersion, &snapshot.AsOfRevision, &body); err != nil {
		return Snapshotted[domain.Task]{}, notFoundOr(err)
	}
	task, err := decode[domain.Task](body)
	if err != nil {
		return Snapshotted[domain.Task]{}, err
	}
	if task.ID != id || task.ProjectID != projectID || snapshot.EntityVersion < 1 || snapshot.AsOfRevision < 1 {
		return Snapshotted[domain.Task]{}, errRowInconsistent
	}
	var key string
	if err := tx.tx.QueryRowContext(ctx, `SELECT intake_key FROM task_intake_keys WHERE task_id = ? AND project_id = ?`, id, projectID).Scan(&key); err != nil {
		return Snapshotted[domain.Task]{}, notFoundOr(err)
	}
	if task.Source != nil {
		want, err := tx.taskIntakeKey(ctx, *task.Source)
		if err != nil {
			return Snapshotted[domain.Task]{}, err
		}
		if key != want {
			return Snapshotted[domain.Task]{}, errRowInconsistent
		}
	}
	return Snapshotted[domain.Task]{Value: task, Snapshot: snapshot}, nil
}

func (tx *ReadTx) ListTasks(ctx context.Context) ([]Snapshotted[domain.Task], error) {
	rows, err := tx.tx.QueryContext(ctx, `SELECT id FROM tasks ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var ids []domain.TaskID
	for rows.Next() {
		var id domain.TaskID
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	out := make([]Snapshotted[domain.Task], 0, len(ids))
	for _, id := range ids {
		task, err := tx.GetTaskSnapshot(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, task)
	}
	return out, nil
}

// AssignTask binds a new run before persistence and preserves a stored binding
// on replay. Production creators provide their intake source; source-less
// legacy/demo constructors receive an orphan identity. Neither path changes
// the content-addressed run or campaign IDs.
func (tx *WriteTx) AssignTask(ctx context.Context, run *domain.Run, source *domain.SpecificationSource) error {
	if run.TaskID == "" {
		err := tx.tx.QueryRowContext(ctx, `SELECT task_id FROM runs WHERE id = ?`, run.ID).Scan(&run.TaskID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	expected, sourceDigest, err := tx.campaignTask(ctx, *run)
	if err != nil {
		return err
	}
	if expected != "" {
		if run.TaskID != "" && run.TaskID != expected {
			return domain.ErrParentKeyMismatch
		}
		run.TaskID = expected
	}
	if run.TaskID == "" && source == nil && sourceDigest != "" {
		task, err := tx.getOrCreateTask(ctx, run.ProjectID, "source:"+string(sourceDigest), nil, time.Now().UTC())
		if err != nil {
			return err
		}
		run.TaskID = task.ID
	}
	if run.TaskID == "" {
		var task domain.Task
		var err error
		if source != nil {
			task, err = tx.GetOrCreateTask(ctx, run.ProjectID, *source)
		} else {
			task, err = tx.getOrCreateTask(ctx, run.ProjectID, "orphan:"+string(run.ID), nil, time.Now().UTC())
		}
		if err != nil {
			return err
		}
		run.TaskID = task.ID
	}
	task, err := tx.GetTask(ctx, run.TaskID)
	if err != nil {
		return err
	}
	if task.ProjectID != run.ProjectID {
		return domain.ErrParentKeyMismatch
	}
	return nil
}

func (tx *WriteTx) recordTaskRun(ctx context.Context, run domain.Run) error {
	if _, err := tx.tx.ExecContext(ctx, `INSERT INTO task_runs (task_id, ordinal, run_id)
		SELECT ?, COALESCE(MAX(ordinal), 0) + 1, ? FROM task_runs WHERE task_id = ?
		ON CONFLICT (run_id) DO NOTHING`, run.TaskID, run.ID, run.TaskID); err != nil {
		return err
	}
	task, err := tx.GetTask(ctx, run.TaskID)
	if err != nil {
		return err
	}
	if run.CampaignID != "" && !slices.Contains(task.CampaignIDs, run.CampaignID) {
		task.CampaignIDs = append(task.CampaignIDs, run.CampaignID)
	}
	return tx.updateTask(ctx, task)
}

func (tx *WriteTx) updateTask(ctx context.Context, task domain.Task) error {
	body, err := encode(task)
	if err != nil {
		return err
	}
	_, err = tx.tx.ExecContext(ctx, `UPDATE tasks SET body = ?, entity_version = entity_version + 1, as_of_revision = ? WHERE id = ?`, body, tx.asOfRevision, task.ID)
	return err
}

// SetTaskName applies the task-name source order. Approval can replace an
// identifier or agent name; an operator or approved name is permanent. The
// engine limits agent refinement to the task's first accepted specification.
func (tx *WriteTx) SetTaskName(ctx context.Context, id domain.TaskID, name domain.DisplayName) error {
	task, err := tx.GetTask(ctx, id)
	if err != nil {
		return err
	}
	if task.Name == name || task.Name.Source == domain.DisplayNameSourceOperator || task.Name.Source == domain.DisplayNameSourceSpecification {
		return nil
	}
	if name.Source == domain.DisplayNameSourceIdentifier {
		return domain.ErrImmutableTransition
	}
	task.Name = name
	if err := tx.updateTask(ctx, task); err != nil {
		return err
	}
	// Attention snapshots project the current task name. Advance their own
	// versions atomically so a delayed read cannot restore the previous name.
	_, err = tx.tx.ExecContext(ctx, `UPDATE attention_items
		SET entity_version = entity_version + 1, as_of_revision = ?
		WHERE subject_task_id = ?`, tx.asOfRevision, id)
	return err
}

func (tx *ReadTx) TaskRunIDs(ctx context.Context, id domain.TaskID) ([]domain.RunID, error) {
	rows, err := tx.tx.QueryContext(ctx, `SELECT run_id FROM task_runs WHERE task_id = ? ORDER BY ordinal`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	ids := []domain.RunID{}
	for rows.Next() {
		var runID domain.RunID
		if err := rows.Scan(&runID); err != nil {
			return nil, err
		}
		ids = append(ids, runID)
	}
	return ids, rows.Err()
}
