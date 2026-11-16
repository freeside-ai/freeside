package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const (
	putProjectReadinessSQL = `
INSERT INTO project_readiness (project_id, instance_revision, project_revision, evaluated_at, body)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (project_id, instance_revision, project_revision) DO UPDATE SET
    evaluated_at = excluded.evaluated_at,
    body = excluded.body`
	getProjectReadinessSQL = `
SELECT body FROM project_readiness
WHERE project_id = ? AND instance_revision = ? AND project_revision = ?`
)

// projectReadinessKey is the row key for a record's settings: revisions 0
// and 0 stand for a daemon composed from a settings file.
func projectReadinessKey(settings *domain.SettingsRevisions) (int, int) {
	if settings == nil {
		return 0, 0
	}
	return settings.Instance, settings.Project
}

// RecordProjectReadiness records a project's readiness under the settings
// revisions it was evaluated for, replacing the row for that key. It is on
// InternalTx because the record is daemon-internal and never synchronized.
func (tx *InternalTx) RecordProjectReadiness(ctx context.Context, readiness domain.ProjectReadiness) error {
	body, err := encode(readiness)
	if err != nil {
		return fmt.Errorf("put project %q readiness: %w", readiness.ProjectID, err)
	}
	instance, project := projectReadinessKey(readiness.Settings)
	if _, err := tx.tx.ExecContext(ctx, putProjectReadinessSQL, readiness.ProjectID, instance, project,
		formatTime(readiness.EvaluatedAt), body); err != nil {
		return fmt.Errorf("put project %q readiness: %w", readiness.ProjectID, err)
	}
	return nil
}

// ProjectReadiness reads the project's readiness record for the settings
// revisions given, nil for a daemon composed from a settings file. A missing
// row wraps ErrNotFound. The row is decoded with plain validation; it is an
// observation, never a grant, and the readiness gate decides what it means.
func (tx *ReadTx) ProjectReadiness(
	ctx context.Context, projectID domain.ProjectID, settings *domain.SettingsRevisions,
) (domain.ProjectReadiness, error) {
	instance, project := projectReadinessKey(settings)
	var body []byte
	err := tx.tx.QueryRowContext(ctx, getProjectReadinessSQL, projectID, instance, project).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ProjectReadiness{}, fmt.Errorf("project %q readiness at settings %d/%d: %w",
			projectID, instance, project, ErrNotFound)
	}
	if err != nil {
		return domain.ProjectReadiness{}, fmt.Errorf("project %q readiness: %w", projectID, err)
	}
	readiness, err := decode[domain.ProjectReadiness](body)
	if err != nil {
		return domain.ProjectReadiness{}, fmt.Errorf("project %q readiness: %w", projectID, err)
	}
	return readiness, nil
}
