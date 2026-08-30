package exec

import (
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// UsageMeasurement is a numbers-only measurement reported by a stage adapter
// or review source. Attribution is added from the stored admission when the
// measurement enters the usage ledger.
type UsageMeasurement struct {
	Source     domain.UsageSource          `json:"source"`
	Kind       domain.UsageMeasurementKind `json:"kind"`
	Metric     string                      `json:"metric"`
	Unit       string                      `json:"unit"`
	Quantity   int64                       `json:"quantity"`
	Sequence   int                         `json:"sequence"`
	ObservedAt time.Time                   `json:"observed_at"`
}

// Validate reports whether the measurement is well-formed.
func (m UsageMeasurement) Validate() error {
	observation := domain.UsageObservation{
		InvocationID:    "validation",
		RunID:           "validation",
		AgentDigest:     "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		LaunchDigest:    "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		TreatmentDigest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		PricingRevision: "validation",
		Source:          m.Source,
		Kind:            m.Kind,
		Metric:          m.Metric,
		Unit:            m.Unit,
		Quantity:        m.Quantity,
		Sequence:        m.Sequence,
		ObservedAt:      m.ObservedAt,
	}
	if err := observation.Validate(); err != nil {
		return fmt.Errorf("usage measurement: %w", err)
	}
	return nil
}
