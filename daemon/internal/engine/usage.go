package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func billableCostSoFar(
	ctx context.Context, st *store.Store, runID domain.RunID,
) (*domain.CostSoFar, error) {
	projection, err := st.ProjectRunBillableCost(ctx, runID)
	if err != nil {
		return nil, err
	}
	if !projection.Present {
		return nil, nil
	}
	observedInvocations := make(map[domain.InvocationID]struct{})
	for _, invocationID := range projection.InvocationIDs {
		observedInvocations[invocationID] = struct{}{}
	}
	admittedInvocations := make(map[domain.InvocationID]struct{})
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		admissions, err := tx.ListRunExecutionAdmissionRecords(ctx, runID)
		if err != nil {
			return err
		}
		for _, admission := range admissions {
			admittedInvocations[admission.InvocationID] = struct{}{}
		}
		reviews, err := tx.ListReviewRecords(ctx, runID)
		if err != nil {
			return err
		}
		for _, review := range reviews {
			admittedInvocations[review.InvocationID] = struct{}{}
		}
		failures, err := tx.ListReviewFailures(ctx, runID)
		if err != nil {
			return err
		}
		for _, failure := range failures {
			admittedInvocations[failure.InvocationID] = struct{}{}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	complete := projection.CompleteUnits
	for invocationID := range admittedInvocations {
		if _, ok := observedInvocations[invocationID]; !ok {
			complete = false
			break
		}
	}
	return &domain.CostSoFar{
		Currency: "USD", Amount: usdMicrosDecimal(projection.USDMicros),
		Invocations: len(observedInvocations), Complete: complete,
	}, nil
}

func usdMicrosDecimal(micros int64) string {
	whole, fraction := micros/1_000_000, micros%1_000_000
	if fraction == 0 {
		return fmt.Sprintf("%d", whole)
	}
	fractionText := strings.TrimRight(fmt.Sprintf("%06d", fraction), "0")
	return fmt.Sprintf("%d.%s", whole, fractionText)
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
