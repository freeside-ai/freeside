package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const projectAuthorityBackfillMigration = "0081_project_authority_backfill.sql"

// backfillProjectAuthority registers the projects row that
// RecordExecutionAdmission now writes, for admissions recorded before it did.
// Each project takes the base repository its admissions name. A project whose
// admissions name two repositories fails the migration by name: the store
// already contradicts itself, and picking one would guess at authority. A
// project that already has a row is left alone; a disagreeing row surfaces at
// that project's next admission, not here. An admission, or the run it names,
// that does not reconstruct contributes nothing.
func backfillProjectAuthority(ctx context.Context, tx *sql.Tx) error {
	admissions, err := readableAdmissions(ctx, tx)
	if err != nil {
		return err
	}
	w := &InternalTx{ReadTx: ReadTx{tx: tx}}
	// Admissions are keyed per project in first-admission order, so the
	// registration order and any conflict report are deterministic.
	var order []domain.ProjectID
	bound := map[domain.ProjectID]domain.BaseRevision{}
	for _, admission := range admissions {
		// The project comes from the reconstructed run, never the copied
		// runs.project_id column: GetRun cross-checks every extracted column
		// against the canonical body, so a divergent column cannot mint an
		// immutable binding for a project the run does not belong to.
		run, err := w.GetRun(ctx, admission.RunID)
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("execution admission %q names run %q with no row: %w",
				admission.InvocationID, admission.RunID, ErrNotFound)
		}
		if err != nil {
			// Derive nothing from a run that fails its trust gate, as for an
			// unreadable admission; every read of that run still fails closed.
			continue
		}
		first, seen := bound[run.ProjectID]
		if !seen {
			order = append(order, run.ProjectID)
			bound[run.ProjectID] = admission.Base
			continue
		}
		if first.Repo != admission.Base.Repo || first.RepositoryID != admission.Base.RepositoryID {
			return fmt.Errorf("project %q is admitted against both %s (%d) and %s (%d): %w",
				run.ProjectID, first.Repo, first.RepositoryID,
				admission.Base.Repo, admission.Base.RepositoryID, ErrImmutableConflict)
		}
	}
	for _, projectID := range order {
		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM projects WHERE project_id = ?)`, projectID,
		).Scan(&exists); err != nil {
			return fmt.Errorf("read project %q: %w", projectID, err)
		}
		if exists {
			continue
		}
		if err := w.registerAdmittedProject(ctx, projectID, bound[projectID]); err != nil {
			return err
		}
	}
	return nil
}

// readableAdmissions returns every admission that reconstructs, in listing
// order. Like backfillTasks, it derives nothing from a row that does not (a
// column that will not convert, a body that fails decoding or its column
// cross-check) rather than refuse to open the store over it: authority never
// comes from unauthenticated bytes, and every read of that admission still
// fails closed. A database fault surfaces through rows.Err.
func readableAdmissions(ctx context.Context, tx *sql.Tx) ([]domain.ExecutionAdmission, error) {
	rows, err := tx.QueryContext(ctx, listExecutionAdmissionsSQL)
	if err != nil {
		return nil, fmt.Errorf("list execution admissions: %w", err)
	}
	var admissions []domain.ExecutionAdmission
	for rows.Next() {
		admission, err := scanExecutionAdmissionRecord(rows)
		if err != nil {
			continue
		}
		admissions = append(admissions, admission)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("list execution admissions: %w", err)
	}
	return admissions, nil
}
