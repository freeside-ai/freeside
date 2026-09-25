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
	renameProjectSQL = `UPDATE projects SET body = ? WHERE project_id = ? AND repository_id = ?`
	getProjectSQL    = `SELECT project_id, repository_id, body FROM projects WHERE project_id = ?`
)

// RegisterProject records one project↔repository authority binding (issue
// #740). Project authority is a daemon-internal record, not synchronized
// client state, so it rides InternalTx like RecordProjectImage and does not bump
// the server revision.
//
// The repository id is the binding's identity and never changes: a different
// repository id for the same project_id is an ErrImmutableConflict, so a
// project's repository can never be silently rebound. The name follows the
// repository (issue #1537): a same-id registration under a new name is a
// rename and rewrites the row's name, and one under the stored name converges.
// There is no other update and no delete path: the authority row is durable
// for the project's life, which is what lets the label-intake read boundary
// require it.
//
// It is register-or-verify: a call that finds an existing row must leave a row
// the read boundary can actually reconstruct. Reconstruct the existing row with
// GetProject, which re-runs the column/body cross-check and re-validation, so
// copied-column corruption fails closed here, not silently at the next
// admission.
func (tx *InternalTx) RegisterProject(ctx context.Context, project domain.Project) error {
	return tx.bindProject(ctx, project, true)
}

// registerAdmittedProject binds a run's project to the repository its
// execution admission names. It is the one production writer outside label
// intake, and the store's backfill replays it for admissions recorded before
// it existed. A new admission carries the daemon's current configured name, so
// it follows a rename; a replayed one (rename false) may carry a name from
// before the rename, so it verifies the repository id and never renames the
// row back.
func (tx *InternalTx) registerAdmittedProject(
	ctx context.Context, projectID domain.ProjectID, base domain.BaseRevision, rename bool,
) error {
	project, err := domain.NewProject(projectID, base.Repo, base.RepositoryID)
	if err != nil {
		return fmt.Errorf("admitted project %q: %w", projectID, err)
	}
	return tx.bindProject(ctx, project, rename)
}

// bindProject inserts the binding when the project has no row and otherwise
// verifies the stored row names the same repository id. With rename, a stored
// name that differs is rewritten to the given one; without it, the stored name
// stays.
func (tx *InternalTx) bindProject(ctx context.Context, project domain.Project, rename bool) error {
	body, err := encode(project)
	if err != nil {
		return fmt.Errorf("register project %q: %w", project.ID, err)
	}
	res, err := tx.tx.ExecContext(ctx, registerProjectSQL, project.ID, project.RepositoryID, body)
	if err != nil {
		return fmt.Errorf("register project %q: %w", project.ID, err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("register project %q: %w", project.ID, err)
	}
	if inserted > 0 {
		return nil
	}
	stored, err := tx.GetProject(ctx, project.ID)
	if err != nil {
		return fmt.Errorf("register project %q verify: %w", project.ID, err)
	}
	if stored.RepositoryID != project.RepositoryID {
		return fmt.Errorf("register project %q: bound to repository %d, not %d: %w",
			project.ID, stored.RepositoryID, project.RepositoryID, ErrImmutableConflict)
	}
	if stored.Repo == project.Repo || !rename {
		return nil
	}
	res, err = tx.tx.ExecContext(ctx, renameProjectSQL, body, project.ID, project.RepositoryID)
	if err != nil {
		return fmt.Errorf("rename project %q: %w", project.ID, err)
	}
	renamed, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rename project %q: %w", project.ID, err)
	}
	// GetProject just matched this row in the same transaction, so anything
	// but one changed row means the store is not what it reported.
	if renamed != 1 {
		return fmt.Errorf("rename project %q: %d rows changed: %w", project.ID, renamed, errRowInconsistent)
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
