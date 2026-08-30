package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

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
// admission. An admission without an agent binding records nothing because it
// cannot supply the required agent, launch, treatment, and pricing identity.
func (tx *InternalTx) AppendUsageObservations(
	ctx context.Context,
	invocationID domain.InvocationID,
	measurements []exec.UsageMeasurement,
) (int, error) {
	admission, err := tx.GetExecutionAdmissionRecord(ctx, invocationID)
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
	if err := rows.Err(); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%s: %w", operation, err)
	}
	return observations, nil
}
