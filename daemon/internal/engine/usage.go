package engine

import (
	"context"
	"errors"
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

// appendUsageObservations records measurements in their own client-visible
// transaction, so the revision moves as soon as the spend figure does (#1145).
// A collect repeats for the same terminal, including to authenticate one
// already recorded, so a replay that inserts nothing rolls back rather than
// raising the revision on every pass.
func appendUsageObservations(
	ctx context.Context,
	st *store.Store,
	invocationID domain.InvocationID,
	measurements []exec.UsageMeasurement,
) error {
	if len(measurements) == 0 {
		return nil
	}
	err := st.Write(ctx, func(tx *store.WriteTx) error {
		inserted, err := tx.AppendUsageObservations(ctx, invocationID, measurements)
		if err != nil {
			return err
		}
		if inserted == 0 {
			return errReplay
		}
		return nil
	})
	if err != nil && !errors.Is(err, errReplay) {
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
