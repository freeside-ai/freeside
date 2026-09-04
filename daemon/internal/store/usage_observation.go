package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

// runBillableCost is the render-only billable projection of one run's usage
// rows. Unknown units make completeUnits false and are never added to
// usdMicros.
type runBillableCost struct {
	usdMicros     int64
	invocationIDs []domain.InvocationID
	present       bool
	completeUnits bool
}

// projectRunBillableCost aggregates the usage rows through the transaction's
// own connection. The store opens one connection (Open sets
// SetMaxOpenConns(1)), so an aggregate a Read callback needs must run inside
// that same transaction rather than through a nested ReadUsage.
func (tx *ReadTx) projectRunBillableCost(
	ctx context.Context, runID domain.RunID,
) (runBillableCost, error) {
	rows, err := tx.tx.QueryContext(ctx, listRunUsageObservationsSQL, runID)
	if err != nil {
		return runBillableCost{}, fmt.Errorf("list run usage observations %q: %w", runID, err)
	}
	observations, err := scanUsageObservations(rows, fmt.Sprintf("list run usage observations %q", runID))
	if err != nil {
		return runBillableCost{}, err
	}
	totals, err := domain.ProjectRunUsage(observations)
	if err != nil {
		return runBillableCost{}, err
	}
	projection := runBillableCost{completeUnits: true}
	for _, total := range totals {
		if total.Kind != domain.UsageMeasurementBillableCost {
			continue
		}
		projection.present = true
		if total.Unit != "usd_micros" {
			projection.completeUnits = false
			continue
		}
		if total.Quantity > math.MaxInt64-projection.usdMicros {
			return runBillableCost{}, domain.ErrUsageQuantityOverflow
		}
		projection.usdMicros += total.Quantity
	}
	invocations := make(map[domain.InvocationID]struct{})
	for _, observation := range observations {
		if observation.Kind == domain.UsageMeasurementBillableCost {
			invocations[domain.InvocationID(observation.InvocationID)] = struct{}{}
		}
	}
	projection.invocationIDs = make([]domain.InvocationID, 0, len(invocations))
	for invocationID := range invocations {
		projection.invocationIDs = append(projection.invocationIDs, invocationID)
	}
	slices.Sort(projection.invocationIDs)
	return projection, nil
}

// BillableCostSoFar returns the run's render-only spend figure, or nil while
// no billable observation exists. It is the one aggregate the ordinary read
// transaction exposes over usage rows: the rows themselves stay behind
// UsageReadTx, so admission and policy callers still cannot reach them. The
// figure is complete only when every admitted invocation (execution
// admissions, review records, and review failures) has a billable
// observation and every observed unit is usd_micros; an admitted invocation
// with no observation, or an unknown unit, marks the amount a lower bound.
func (tx *ReadTx) BillableCostSoFar(
	ctx context.Context, runID domain.RunID,
) (*domain.CostSoFar, error) {
	projection, err := tx.projectRunBillableCost(ctx, runID)
	if err != nil {
		return nil, err
	}
	if !projection.present {
		return nil, nil
	}
	observedInvocations := make(map[domain.InvocationID]struct{})
	for _, invocationID := range projection.invocationIDs {
		observedInvocations[invocationID] = struct{}{}
	}
	admittedInvocations := make(map[domain.InvocationID]struct{})
	admissions, err := tx.ListRunExecutionAdmissionRecords(ctx, runID)
	if err != nil {
		return nil, err
	}
	for _, admission := range admissions {
		admittedInvocations[admission.InvocationID] = struct{}{}
	}
	reviews, err := tx.ListReviewRecords(ctx, runID)
	if err != nil {
		return nil, err
	}
	for _, review := range reviews {
		admittedInvocations[review.InvocationID] = struct{}{}
	}
	failures, err := tx.ListReviewFailures(ctx, runID)
	if err != nil {
		return nil, err
	}
	for _, failure := range failures {
		admittedInvocations[failure.InvocationID] = struct{}{}
	}
	complete := projection.completeUnits
	for invocationID := range admittedInvocations {
		if _, ok := observedInvocations[invocationID]; !ok {
			complete = false
			break
		}
	}
	return &domain.CostSoFar{
		Currency: "USD", Amount: usdMicrosDecimal(projection.usdMicros),
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

const (
	appendUsageObservationSQL = `INSERT INTO usage_observations
		(invocation_id, run_id, agent_digest, launch_digest, treatment_digest,
		 pricing_revision, source, kind, metric, unit, quantity, sequence, observed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING`
	selectUsageObservationContentSQL = `SELECT unit, quantity, observed_at
		FROM usage_observations
		WHERE invocation_id = ? AND source = ? AND kind = ? AND metric = ? AND sequence = ?`
	listRunUsageObservationsSQL = `SELECT invocation_id, run_id, agent_digest,
		launch_digest, treatment_digest, pricing_revision, source, kind, metric,
		unit, quantity, sequence, observed_at
		FROM usage_observations WHERE run_id = ?
		ORDER BY invocation_id, source, kind, metric, sequence`
	listUsageObservationsByTreatmentSQL = `SELECT invocation_id, run_id,
		agent_digest, launch_digest, treatment_digest, pricing_revision, source,
		kind, metric, unit, quantity, sequence, observed_at
		FROM usage_observations
		ORDER BY invocation_id, source, kind, metric, sequence`
)

// AppendUsageObservations attributes and appends measurements for one stored
// admission. A missing admission or one without an agent binding records
// nothing because it cannot supply the required agent, launch, treatment, and
// pricing identity.
func (tx *InternalTx) AppendUsageObservations(
	ctx context.Context,
	invocationID domain.InvocationID,
	measurements []exec.UsageMeasurement,
) (int, error) {
	if len(measurements) == 0 {
		return 0, nil
	}
	admission, err := tx.GetExecutionAdmissionRecord(ctx, invocationID)
	if errors.Is(err, ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("append usage observations %q: %w", invocationID, err)
	}
	if admission.AgentBinding == nil {
		return 0, nil
	}

	inserted := 0
	for i, measurement := range measurements {
		if err := measurement.Validate(); err != nil {
			return 0, fmt.Errorf("append usage observations %q measurement %d: %w",
				invocationID, i, err)
		}
		observation := domain.UsageObservation{
			InvocationID:    string(admission.InvocationID),
			RunID:           string(admission.RunID),
			AgentDigest:     admission.AgentBinding.AgentDigest,
			LaunchDigest:    admission.AgentBinding.LaunchDigest,
			TreatmentDigest: admission.AgentBinding.TreatmentDigest,
			PricingRevision: admission.AgentBinding.PricingRevision,
			Source:          measurement.Source,
			Kind:            measurement.Kind,
			Metric:          measurement.Metric,
			Unit:            measurement.Unit,
			Quantity:        measurement.Quantity,
			Sequence:        measurement.Sequence,
			ObservedAt:      measurement.ObservedAt,
		}
		if err := observation.Validate(); err != nil {
			return 0, fmt.Errorf("append usage observations %q measurement %d: %w",
				invocationID, i, err)
		}
		result, err := tx.tx.ExecContext(ctx, appendUsageObservationSQL,
			observation.InvocationID, observation.RunID, observation.AgentDigest,
			observation.LaunchDigest, observation.TreatmentDigest,
			observation.PricingRevision, observation.Source, observation.Kind,
			observation.Metric, observation.Unit, observation.Quantity,
			observation.Sequence, formatTime(observation.ObservedAt))
		if err != nil {
			return 0, fmt.Errorf("append usage observation %q %s/%s/%s/%d: %w",
				invocationID, observation.Source, observation.Kind,
				observation.Metric, observation.Sequence, err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("append usage observation %q %s/%s/%s/%d: %w",
				invocationID, observation.Source, observation.Kind,
				observation.Metric, observation.Sequence, err)
		}
		if changed == 1 {
			inserted++
			continue
		}
		if err := tx.requireMatchingUsageObservation(ctx, observation); err != nil {
			return 0, err
		}
	}
	return inserted, nil
}

func (tx *InternalTx) requireMatchingUsageObservation(
	ctx context.Context, observation domain.UsageObservation,
) error {
	var unit, observedAt string
	var quantity int64
	err := tx.tx.QueryRowContext(ctx, selectUsageObservationContentSQL,
		observation.InvocationID, observation.Source, observation.Kind,
		observation.Metric, observation.Sequence).Scan(&unit, &quantity, &observedAt)
	if err != nil {
		return fmt.Errorf("read existing usage observation %q %s/%s/%s/%d: %w",
			observation.InvocationID, observation.Source, observation.Kind,
			observation.Metric, observation.Sequence, err)
	}
	if unit != observation.Unit || quantity != observation.Quantity ||
		observedAt != formatTime(observation.ObservedAt) {
		return fmt.Errorf("append usage observation %q %s/%s/%s/%d: %w",
			observation.InvocationID, observation.Source, observation.Kind,
			observation.Metric, observation.Sequence, domain.ErrUsageObservationConflict)
	}
	return nil
}

// UsageReadTx is the dedicated observation-only read surface. It intentionally
// does not embed ReadTx, so admission and policy callers cannot reach usage
// rows through their ordinary transaction handle.
type UsageReadTx struct {
	tx *sql.Tx
}

// ListRunUsageObservations lists the append-only observations for one run.
func (tx *UsageReadTx) ListRunUsageObservations(
	ctx context.Context, runID domain.RunID,
) ([]domain.UsageObservation, error) {
	rows, err := tx.tx.QueryContext(ctx, listRunUsageObservationsSQL, runID)
	if err != nil {
		return nil, fmt.Errorf("list run usage observations %q: %w", runID, err)
	}
	return scanUsageObservations(rows, fmt.Sprintf("list run usage observations %q", runID))
}

// ListUsageObservationsByTreatment lists all observations in primary-key
// order for the treatment comparison projection.
func (tx *UsageReadTx) ListUsageObservationsByTreatment(
	ctx context.Context,
) ([]domain.UsageObservation, error) {
	rows, err := tx.tx.QueryContext(ctx, listUsageObservationsByTreatmentSQL)
	if err != nil {
		return nil, fmt.Errorf("list usage observations by treatment: %w", err)
	}
	return scanUsageObservations(rows, "list usage observations by treatment")
}

func scanUsageObservations(rows *sql.Rows, operation string) ([]domain.UsageObservation, error) {
	defer func() { _ = rows.Close() }()
	var observations []domain.UsageObservation
	for rows.Next() {
		var observation domain.UsageObservation
		var observedAt string
		if err := rows.Scan(
			&observation.InvocationID, &observation.RunID,
			&observation.AgentDigest, &observation.LaunchDigest,
			&observation.TreatmentDigest, &observation.PricingRevision,
			&observation.Source, &observation.Kind, &observation.Metric,
			&observation.Unit, &observation.Quantity, &observation.Sequence,
			&observedAt,
		); err != nil {
			return nil, fmt.Errorf("%s: %w", operation, err)
		}
		parsed, err := parseTime(observedAt)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", operation, err)
		}
		observation.ObservedAt = parsed
		if err := observation.Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", operation, err)
		}
		observations = append(observations, observation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", operation, err)
	}
	return observations, nil
}
