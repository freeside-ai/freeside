package store

import (
	"context"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// CountActiveProjectRuns counts runs using milestone-only terminality. It is a
// store-local primitive and cannot authenticate accepted signet commands. The
// production intake caller therefore uses its own resolution-aware count; this
// helper remains valid only where publication reevaluation is unreachable.
//
// It is deliberately distinct from RequireIdentityExecutionCapacity, which
// limits one inference identity's concurrent executions; the #659 non-goal
// forbids reusing that inference-parallelism limit as a run cap.
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
