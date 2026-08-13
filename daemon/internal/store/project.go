package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const (
	registerProjectSQL = `
INSERT INTO projects (project_id, repository_id, body)
VALUES (?, ?, ?)
ON CONFLICT (project_id) DO NOTHING`
	getProjectSQL     = `SELECT project_id, repository_id, body FROM projects WHERE project_id = ?`
	getProjectBodySQL = `SELECT body FROM projects WHERE project_id = ?`
)

// RegisterProject records one project↔repository authority binding write-once
// (issue #740). Project authority is a daemon-internal record, not synchronized
// client state, so it rides InternalTx like RecordProjectImage and does not bump
// the server revision. A byte-identical replay of the same binding converges on
// the existing row; any different repository for the same project_id is an
// ErrImmutableConflict, so a project's repository can never be silently rebound.
// There is no update or delete path: the authority row is durable for the
// project's life, which is what lets the label-intake read boundary require it.
//
// It is register-or-verify: a replay that converges must leave a row the read
// boundary can actually reconstruct. putImmutable compares only the body, so a
// converging replay over a row whose copied repository_id column has been
// corrupted would otherwise report success while GetProject and intake admission
// reject the row. Reconstruct the existing row on the converge path (GetProject
// re-runs the column/body cross-check and re-validation) so copied-column
// corruption fails closed here, not silently at the next admission.
func (tx *InternalTx) RegisterProject(ctx context.Context, project domain.Project) error {
	body, err := encode(project)
	if err != nil {
		return fmt.Errorf("register project %q: %w", project.ID, err)
	}
	inserted, err := tx.putImmutableInserted(ctx, registerProjectSQL,
		[]any{project.ID, project.RepositoryID, body},
		getProjectBodySQL, []any{project.ID}, body)
	if err != nil {
		return fmt.Errorf("register project %q: %w", project.ID, err)
	}
	if !inserted {
		if _, err := tx.GetProject(ctx, project.ID); err != nil {
			return fmt.Errorf("register project %q verify: %w", project.ID, err)
		}
	}
	return nil
}

// GetProject reconstructs one project↔repository authority binding, reporting
// ErrNotFound when absent. decode re-validates the body (Project.Validate is the
// fail-closed reconstruction gate) and the extracted repository_id is
// cross-checked against the decoded body, so a body-only tamper that rebinds the
// project to a different repository is refused as inconsistent.
func (tx *ReadTx) GetProject(ctx context.Context, id domain.ProjectID) (domain.Project, error) {
	project, err := scanProject(tx.tx.QueryRowContext(ctx, getProjectSQL, id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Project{}, fmt.Errorf("get project %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return domain.Project{}, fmt.Errorf("get project %q: %w", id, err)
	}
	if project.ID != id {
		return domain.Project{}, fmt.Errorf("get project %q: %w", id, errRowInconsistent)
	}
	return project, nil
}

// scanProject decodes a projects row and cross-checks its extracted columns
// against the canonical body: project_id (the primary key) and repository_id
// (the trust-bearing lookup column). A row whose body disagrees with either
// column is refused as inconsistent, so a direct-SQL tamper of one side alone
// cannot rebind the project.
func scanProject(row scanner) (domain.Project, error) {
	var (
		projectID    string
		repositoryID int64
		body         []byte
	)
	if err := row.Scan(&projectID, &repositoryID, &body); err != nil {
		// A Scan failure is a transient read fault (context cancellation, a DB
		// operational error) or sql.ErrNoRows, not row corruption; propagate it
		// unwrapped so the caller can tell it apart from a durable-corruption row.
		return domain.Project{}, err
	}
	project, err := decode[domain.Project](body)
	if err != nil {
		// A stored body that will not decode or re-validate is durable row
		// corruption, the same trust class as the column cross-check below, so it
		// carries errRowInconsistent (cause preserved) rather than surfacing as an
		// undifferentiated error a caller might mistake for a transient fault.
		return domain.Project{}, fmt.Errorf("%w: %w", errRowInconsistent, err)
	}
	if string(project.ID) != projectID || project.RepositoryID != repositoryID {
		return domain.Project{}, errRowInconsistent
	}
	return project, nil
}
