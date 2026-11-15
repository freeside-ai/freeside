package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const insertProjectSettingsRevisionSQL = `
INSERT INTO project_settings_revisions (project_id, revision, section, set_by, set_at, body)
VALUES (?, ?, ?, ?, ?, ?)`

// ErrProjectRepositoryMismatch is returned when a project's settings name a
// repository other than the one its projects row binds it to for good.
var ErrProjectRepositoryMismatch = errors.New("project settings name a repository other than the project's binding")

// AppendProjectSettingsRevision records a project's next settings revision,
// which the store numbers here, after that project's last. The domain
// constructor refuses a value its section's rules refuse and a clear of a
// required section. A repository section is refused when the project's
// projects row binds it to a different repository id; with no row, the
// append creates none, because label intake and admission write that
// binding. It is on InternalTx because project settings are daemon-internal
// and never synchronized to clients.
func (tx *InternalTx) AppendProjectSettingsRevision(
	ctx context.Context, in domain.ProjectSettingsInput, setAt time.Time,
) (domain.ProjectSettingsRevision, error) {
	var last int
	if err := tx.tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(revision), 0) FROM project_settings_revisions WHERE project_id = ?`,
		in.ProjectID).Scan(&last); err != nil {
		return domain.ProjectSettingsRevision{}, fmt.Errorf("append project %q settings revision: %w", in.ProjectID, err)
	}
	revision, err := domain.NewProjectSettingsRevision(in, last+1, setAt)
	if err != nil {
		return domain.ProjectSettingsRevision{}, fmt.Errorf("append project %q settings revision: %w", in.ProjectID, err)
	}
	if revision.Section == domain.ProjectSectionRepository {
		var repository domain.ProjectRepositorySettings
		if err := json.Unmarshal(revision.Value, &repository); err != nil {
			return domain.ProjectSettingsRevision{}, fmt.Errorf("append project %q settings revision: %w", in.ProjectID, err)
		}
		if err := tx.CheckProjectRepositoryBinding(ctx, in.ProjectID, repository.RepositoryID); err != nil {
			return domain.ProjectSettingsRevision{}, fmt.Errorf("append project %q settings revision: %w", in.ProjectID, err)
		}
	}
	body, err := encode(revision)
	if err != nil {
		return domain.ProjectSettingsRevision{}, fmt.Errorf("append project %q settings revision: %w", in.ProjectID, err)
	}
	if _, err := tx.tx.ExecContext(ctx, insertProjectSettingsRevisionSQL, revision.ProjectID, revision.Revision,
		revision.Section, revision.SetBy, formatTime(revision.SetAt), body); err != nil {
		return domain.ProjectSettingsRevision{}, fmt.Errorf("append project %q settings revision: %w", in.ProjectID, err)
	}
	return revision, nil
}

// CheckProjectRepositoryBinding refuses a repository id other than the one
// the project's projects row binds it to, with ErrProjectRepositoryMismatch.
// A project with no row passes: nothing binds it yet.
func (tx *ReadTx) CheckProjectRepositoryBinding(ctx context.Context, project domain.ProjectID, repositoryID int64) error {
	bound, err := tx.GetProject(ctx, project)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if bound.RepositoryID != repositoryID {
		return fmt.Errorf("project %q is bound to repository %d, not %d: %w",
			project, bound.RepositoryID, repositoryID, ErrProjectRepositoryMismatch)
	}
	return nil
}

// ProjectSettingsRevisions returns one project's settings revisions in
// revision order. A row that fails its decode, or whose columns disagree
// with its body, fails the read.
//
// The rows are not a grant: they hold authority-bearing values written under
// the policy of their day. The run settings loader in daemoncompose is the
// gate that re-checks them against current policy and the project's
// repository binding before a run uses them.
func (tx *ReadTx) ProjectSettingsRevisions(ctx context.Context, project domain.ProjectID) ([]domain.ProjectSettingsRevision, error) {
	revisions, err := queryAll(ctx, tx.tx, scanProjectSettingsRevision,
		`SELECT project_id, revision, section, body FROM project_settings_revisions
		 WHERE project_id = ? ORDER BY revision`, project)
	if err != nil {
		return nil, fmt.Errorf("project %q settings revisions: %w", project, err)
	}
	return revisions, nil
}

// ProjectSettingsRecord folds one project's settings revisions into the
// current value of each section; none set is an empty record at revision 0.
// Like ProjectSettingsRevisions, it is not a grant.
func (tx *ReadTx) ProjectSettingsRecord(ctx context.Context, project domain.ProjectID) (domain.ProjectSettingsRecord, error) {
	revisions, err := tx.ProjectSettingsRevisions(ctx, project)
	if err != nil {
		return domain.ProjectSettingsRecord{}, err
	}
	record, err := domain.NewProjectSettingsRecord(project, revisions)
	if err != nil {
		return domain.ProjectSettingsRecord{}, fmt.Errorf("project settings record: %w", err)
	}
	return record, nil
}

func scanProjectSettingsRevision(row scanner) (domain.ProjectSettingsRevision, error) {
	var (
		project  string
		revision int
		section  string
		body     []byte
	)
	if err := row.Scan(&project, &revision, &section, &body); err != nil {
		return domain.ProjectSettingsRevision{}, err
	}
	r, err := decode[domain.ProjectSettingsRevision](body)
	if err != nil {
		return domain.ProjectSettingsRevision{}, fmt.Errorf("%w: %w", errRowInconsistent, err)
	}
	if string(r.ProjectID) != project || r.Revision != revision || string(r.Section) != section {
		return domain.ProjectSettingsRevision{}, errRowInconsistent
	}
	return r, nil
}
