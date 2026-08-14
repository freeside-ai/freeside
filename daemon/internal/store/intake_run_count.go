package store

import (
	"context"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// CountActiveProjectRuns counts the runs of one project that have not yet
// reached a final outcome. Activity is the same milestone-derived conclusion
// the run-observation surface reports (domain.ConcludeRun): a run counts while
// its conclusion is non-final (pending), and drops out once it publishes,
// blocks definitively, fails, or is lost. Reusing ConcludeRun keeps this cap on
// the one canonical run-terminality predicate rather than a second, divergent
// notion of "done".
//
// This is the project run-WIP axis label-intake auto_start bounds itself by
// (intake.IntakePolicy.WIPCapExhausted). It is deliberately distinct from
// RequireIdentityExecutionCapacity, which limits one inference identity's
// concurrent executions; the #659 non-goal forbids reusing that
// inference-parallelism limit as a run cap. The caller derives this count under
// the store's write lock in the same decision that records the refusal or
// authors the start, so the count and its consequence cannot race.
func (tx *ReadTx) CountActiveProjectRuns(ctx context.Context, projectID domain.ProjectID) (int, error) {
	if projectID == "" {
		return 0, fmt.Errorf("count active project runs: %w", domain.ErrEmptyID)
	}
	ids, err := tx.projectRunIDs(ctx, projectID)
	if err != nil {
		return 0, fmt.Errorf("count active project runs: %w", err)
	}
	active := 0
	for _, id := range ids {
		observation, err := tx.ObserveRun(ctx, id)
		if err != nil {
			return 0, fmt.Errorf("count active project runs: observe %s: %w", id, err)
		}
		if !domain.ConcludeRun(observation).Final {
			active++
		}
	}
	return active, nil
}

// projectRunIDs reads every run id for one project. It fully drains and closes
// the result set before returning, so the caller can issue the per-run
// observation queries on the same transaction.
func (tx *ReadTx) projectRunIDs(ctx context.Context, projectID domain.ProjectID) ([]domain.RunID, error) {
	rows, err := tx.tx.QueryContext(ctx,
		`SELECT id FROM runs WHERE project_id = ? ORDER BY id`, string(projectID))
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read-only query
	var ids []domain.RunID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, domain.RunID(id))
	}
	return ids, rows.Err()
}
