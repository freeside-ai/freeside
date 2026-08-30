package domain

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// Usage observations are append-only, numbers-only telemetry. They describe
// what a route reported after an invocation; they never authorize admission,
// quota, or budget decisions. Each row is a cumulative snapshot for its
// (invocation, source, kind, metric) at Sequence. Projections use the greatest
// sequence and keep absence distinct from an observed zero quantity.

// UsageSource names the route that reported a measurement. The zero value is
// invalid by design.
type UsageSource string

const (
	UsageSourceAdapterTranscript UsageSource = "adapter_transcript"
	UsageSourceReviewSource      UsageSource = "review_source"
)

// AllUsageSources is the single registration point for usage sources.
var AllUsageSources = []UsageSource{
	UsageSourceAdapterTranscript,
	UsageSourceReviewSource,
}

func (s UsageSource) valid() bool {
	switch s {
	case UsageSourceAdapterTranscript, UsageSourceReviewSource:
		return true
	default:
		return false
	}
}

// UsageMeasurementKind preserves plan §8's deliberately distinct resource
// measures. The zero value is invalid by design.
type UsageMeasurementKind string

const (
	UsageMeasurementBillableCost     UsageMeasurementKind = "billable_cost"
	UsageMeasurementReportedUsage    UsageMeasurementKind = "reported_usage"
	UsageMeasurementQuotaConsumption UsageMeasurementKind = "quota_consumption"
)

// AllUsageMeasurementKinds is the single registration point for measurement
// kinds.
var AllUsageMeasurementKinds = []UsageMeasurementKind{
	UsageMeasurementBillableCost,
	UsageMeasurementReportedUsage,
	UsageMeasurementQuotaConsumption,
}

func (k UsageMeasurementKind) valid() bool {
	switch k {
	case UsageMeasurementBillableCost, UsageMeasurementReportedUsage,
		UsageMeasurementQuotaConsumption:
		return true
	default:
		return false
	}
}

// UsageObservation is one attributed, cumulative usage snapshot.
type UsageObservation struct {
	InvocationID    string               `json:"invocation_id"`
	RunID           string               `json:"run_id"`
	AgentDigest     Digest               `json:"agent_digest"`
	LaunchDigest    Digest               `json:"launch_digest"`
	TreatmentDigest Digest               `json:"treatment_digest"`
	PricingRevision string               `json:"pricing_revision"`
	Source          UsageSource          `json:"source"`
	Kind            UsageMeasurementKind `json:"kind"`
	Metric          string               `json:"metric"`
	Unit            string               `json:"unit"`
	Quantity        int64                `json:"quantity"`
	Sequence        int                  `json:"sequence"`
	ObservedAt      time.Time            `json:"observed_at"`
}

// Validate reports whether the observation is well-formed.
func (o UsageObservation) Validate() error {
	for field, id := range map[string]string{
		"invocation_id": o.InvocationID,
		"run_id":        o.RunID,
	} {
		if id == "" {
			return fmt.Errorf("usage observation %s: %w", field, ErrEmptyID)
		}
	}
	for field, digest := range map[string]Digest{
		"agent_digest":     o.AgentDigest,
		"launch_digest":    o.LaunchDigest,
		"treatment_digest": o.TreatmentDigest,
	} {
		if !contentaddr.Valid(string(digest)) {
			return fmt.Errorf("usage observation %s %q: %w", field, digest, ErrInvalidDigest)
		}
	}
	if o.PricingRevision == "" {
		return fmt.Errorf("usage observation pricing_revision: %w", ErrEmptyField)
	}
	if !o.Source.valid() {
		return fmt.Errorf("usage observation source %q: %w", o.Source, ErrInvalidUsageSource)
	}
	if !o.Kind.valid() {
		return fmt.Errorf("usage observation kind %q: %w", o.Kind, ErrInvalidUsageMeasurementKind)
	}
	if !validUsageToken(o.Metric) {
		return fmt.Errorf("usage observation metric %q: %w", o.Metric, ErrEmptyField)
	}
	if !validUsageToken(o.Unit) {
		return fmt.Errorf("usage observation unit %q: %w", o.Unit, ErrEmptyField)
	}
	if o.Quantity < 0 {
		return fmt.Errorf("usage observation quantity %d: %w", o.Quantity, ErrNonPositive)
	}
	if o.Sequence < 1 {
		return fmt.Errorf("usage observation sequence %d: %w", o.Sequence, ErrNonPositive)
	}
	if o.ObservedAt.IsZero() {
		return fmt.Errorf("usage observation observed_at: %w", ErrMissingTimestamp)
	}
	if o.ObservedAt.Location() != time.UTC {
		return fmt.Errorf("usage observation observed_at: %w", ErrTimestampNotUTC)
	}
	return nil
}

func validUsageToken(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

// UsageTotal is the sum of the latest observations for one measurement.
type UsageTotal struct {
	Kind     UsageMeasurementKind `json:"kind"`
	Metric   string               `json:"metric"`
	Unit     string               `json:"unit"`
	Quantity int64                `json:"quantity"`
}

// Validate reports whether the total is well-formed.
func (t UsageTotal) Validate() error {
	if !t.Kind.valid() {
		return fmt.Errorf("usage total kind %q: %w", t.Kind, ErrInvalidUsageMeasurementKind)
	}
	if !validUsageToken(t.Metric) {
		return fmt.Errorf("usage total metric %q: %w", t.Metric, ErrEmptyField)
	}
	if !validUsageToken(t.Unit) {
		return fmt.Errorf("usage total unit %q: %w", t.Unit, ErrEmptyField)
	}
	if t.Quantity < 0 {
		return fmt.Errorf("usage total quantity %d: %w", t.Quantity, ErrNonPositive)
	}
	return nil
}

// UsageTreatmentGroup is the comparison projection for one treatment.
type UsageTreatmentGroup struct {
	TreatmentDigest  Digest       `json:"treatment_digest"`
	Invocations      int          `json:"invocations"`
	PricingRevisions []string     `json:"pricing_revisions"`
	Totals           []UsageTotal `json:"totals"`
}

// Validate reports whether the treatment group is well-formed and canonical.
func (g UsageTreatmentGroup) Validate() error {
	if !contentaddr.Valid(string(g.TreatmentDigest)) {
		return fmt.Errorf("usage treatment group treatment_digest %q: %w", g.TreatmentDigest, ErrInvalidDigest)
	}
	if g.Invocations < 1 {
		return fmt.Errorf("usage treatment group invocations %d: %w", g.Invocations, ErrNonPositive)
	}
	if len(g.PricingRevisions) == 0 {
		return fmt.Errorf("usage treatment group pricing_revisions: %w", ErrEmptyField)
	}
	for i, revision := range g.PricingRevisions {
		if revision == "" {
			return fmt.Errorf("usage treatment group pricing_revisions: %w", ErrEmptyField)
		}
		if i > 0 && g.PricingRevisions[i-1] >= revision {
			return fmt.Errorf("usage treatment group pricing_revisions: %w", ErrKeysNotCanonical)
		}
	}
	for i, total := range g.Totals {
		if err := total.Validate(); err != nil {
			return fmt.Errorf("usage treatment group total %d: %w", i, err)
		}
		if i > 0 && !usageTotalLess(g.Totals[i-1], total) {
			return fmt.Errorf("usage treatment group totals: %w", ErrKeysNotCanonical)
		}
	}
	return nil
}

type usageSnapshotKey struct {
	InvocationID string
	Source       UsageSource
	Kind         UsageMeasurementKind
	Metric       string
}

type usageTotalKey struct {
	Kind   UsageMeasurementKind
	Metric string
	Unit   string
}

// ProjectRunUsage returns per-run totals from the latest snapshot for each
// invocation, source, kind, and metric. Empty input returns nil so a caller can
// render telemetry absence as null instead of zero.
func ProjectRunUsage(rows []UsageObservation) ([]UsageTotal, error) {
	latest := latestUsageObservations(rows)
	return sumUsage(latest)
}

// ProjectUsageComparison groups the latest observations by treatment digest.
func ProjectUsageComparison(rows []UsageObservation) ([]UsageTreatmentGroup, error) {
	byTreatment := make(map[Digest][]UsageObservation)
	for _, row := range rows {
		byTreatment[row.TreatmentDigest] = append(byTreatment[row.TreatmentDigest], row)
	}
	if len(byTreatment) == 0 {
		return nil, nil
	}

	groups := make([]UsageTreatmentGroup, 0, len(byTreatment))
	for treatment, treatmentRows := range byTreatment {
		invocations := make(map[string]struct{})
		pricing := make(map[string]struct{})
		for _, row := range treatmentRows {
			invocations[row.InvocationID] = struct{}{}
			pricing[row.PricingRevision] = struct{}{}
		}
		revisions := make([]string, 0, len(pricing))
		for revision := range pricing {
			revisions = append(revisions, revision)
		}
		slices.Sort(revisions)
		totals, err := sumUsage(latestUsageObservations(treatmentRows))
		if err != nil {
			return nil, err
		}
		groups = append(groups, UsageTreatmentGroup{
			TreatmentDigest:  treatment,
			Invocations:      len(invocations),
			PricingRevisions: revisions,
			Totals:           totals,
		})
	}
	sort.Slice(groups, func(i, j int) bool {
		return groups[i].TreatmentDigest < groups[j].TreatmentDigest
	})
	return groups, nil
}

func latestUsageObservations(rows []UsageObservation) []UsageObservation {
	latest := make(map[usageSnapshotKey]UsageObservation)
	for _, row := range rows {
		key := usageSnapshotKey{
			InvocationID: row.InvocationID,
			Source:       row.Source,
			Kind:         row.Kind,
			Metric:       row.Metric,
		}
		if current, ok := latest[key]; !ok || row.Sequence > current.Sequence {
			latest[key] = row
		}
	}
	result := make([]UsageObservation, 0, len(latest))
	for _, row := range latest {
		result = append(result, row)
	}
	return result
}

func sumUsage(rows []UsageObservation) ([]UsageTotal, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	totals := make(map[usageTotalKey]int64)
	for _, row := range rows {
		key := usageTotalKey{Kind: row.Kind, Metric: row.Metric, Unit: row.Unit}
		if row.Quantity > math.MaxInt64-totals[key] {
			return nil, fmt.Errorf("usage projection %s/%s/%s: %w",
				row.Kind, row.Metric, row.Unit, ErrUsageQuantityOverflow)
		}
		totals[key] += row.Quantity
	}
	result := make([]UsageTotal, 0, len(totals))
	for key, quantity := range totals {
		result = append(result, UsageTotal{
			Kind: key.Kind, Metric: key.Metric, Unit: key.Unit, Quantity: quantity,
		})
	}
	sort.Slice(result, func(i, j int) bool { return usageTotalLess(result[i], result[j]) })
	return result, nil
}

func usageTotalLess(left, right UsageTotal) bool {
	if left.Kind != right.Kind {
		return left.Kind < right.Kind
	}
	if left.Metric != right.Metric {
		return left.Metric < right.Metric
	}
	return left.Unit < right.Unit
}
