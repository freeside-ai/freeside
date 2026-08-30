package engine

import (
	"context"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

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
