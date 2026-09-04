package engine

import (
	"context"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// billableCostSoFar is the attention-card spend figure. The aggregate lives
// on the store read transaction (store.ReadTx.BillableCostSoFar) so the runs
// projection and the cards compute it the same way by construction.
func billableCostSoFar(
	ctx context.Context, st *store.Store, runID domain.RunID,
) (*domain.CostSoFar, error) {
	var cost *domain.CostSoFar
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		cost, err = tx.BillableCostSoFar(ctx, runID)
		return err
	}); err != nil {
		return nil, err
	}
	return cost, nil
}

func appendUsageObservations(
	ctx context.Context,
	st *store.Store,
	invocationID domain.InvocationID,
	measurements []exec.UsageMeasurement,
) error {
	if len(measurements) == 0 {
		return nil
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.AppendUsageObservations(ctx, invocationID, measurements)
		return err
	}); err != nil {
		return fmt.Errorf("persist usage observations for %q: %w", invocationID, err)
	}
	return nil
}

func reviewSourceFailureWithUsage(
	class domain.ReviewFailureClass,
	err error,
	measurements []exec.UsageMeasurement,
) *exec.ReviewSourceFailure {
	valid := make([]exec.UsageMeasurement, 0, len(measurements))
	for _, measurement := range measurements {
		if measurement.Validate() == nil {
			valid = append(valid, measurement)
		}
	}
	if len(valid) == 0 {
		valid = nil
	}
	return &exec.ReviewSourceFailure{Class: class, Err: err, Usage: valid}
}

func preserveReviewSourceFailureUsage(
	err error, measurements []exec.UsageMeasurement,
) *exec.ReviewSourceFailure {
	return reviewSourceFailureWithUsage(exec.ClassifyReviewSourceFailure(err), err, measurements)
}
