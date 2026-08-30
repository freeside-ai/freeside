package domain_test

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

func usageObservationFixture() domain.UsageObservation {
	return domain.UsageObservation{
		InvocationID:    "inv-1",
		RunID:           "run-1",
		AgentDigest:     "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		LaunchDigest:    "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		TreatmentDigest: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		PricingRevision: "pricing-2026-01",
		Source:          domain.UsageSourceAdapterTranscript,
		Kind:            domain.UsageMeasurementReportedUsage,
		Metric:          "input_tokens",
		Unit:            "tokens",
		Quantity:        100,
		Sequence:        1,
		ObservedAt:      time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

func usageProjectionFixture() []domain.UsageObservation {
	base := usageObservationFixture()
	rows := []domain.UsageObservation{base}

	later := base
	later.Quantity = 150
	later.Sequence = 2
	later.ObservedAt = base.ObservedAt.Add(time.Minute)
	rows = append(rows, later)

	zero := base
	zero.Metric = "output_tokens"
	zero.Quantity = 0
	rows = append(rows, zero)

	cost := base
	cost.Kind = domain.UsageMeasurementBillableCost
	cost.Metric = "total_cost"
	cost.Unit = "usd_micros"
	cost.Quantity = 1_250_000
	rows = append(rows, cost)

	secondInvocation := base
	secondInvocation.InvocationID = "inv-2"
	secondInvocation.Quantity = 50
	secondInvocation.PricingRevision = "pricing-2026-02"
	rows = append(rows, secondInvocation)

	secondTreatment := base
	secondTreatment.InvocationID = "inv-3"
	secondTreatment.TreatmentDigest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	secondTreatment.Quantity = 200
	rows = append(rows, secondTreatment)

	return rows
}

func TestUsageObservationValidate(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*domain.UsageObservation)
		want   error
	}{
		{"valid", func(*domain.UsageObservation) {}, nil},
		{"missing invocation", func(o *domain.UsageObservation) { o.InvocationID = "" }, domain.ErrEmptyID},
		{"invalid digest", func(o *domain.UsageObservation) { o.TreatmentDigest = "treatment" }, domain.ErrInvalidDigest},
		{"missing pricing", func(o *domain.UsageObservation) { o.PricingRevision = "" }, domain.ErrEmptyField},
		{"invalid source", func(o *domain.UsageObservation) { o.Source = "other" }, domain.ErrInvalidUsageSource},
		{"invalid kind", func(o *domain.UsageObservation) { o.Kind = "other" }, domain.ErrInvalidUsageMeasurementKind},
		{"invalid metric", func(o *domain.UsageObservation) { o.Metric = "Input Tokens" }, domain.ErrEmptyField},
		{"invalid unit", func(o *domain.UsageObservation) { o.Unit = "token/s" }, domain.ErrEmptyField},
		{"negative quantity", func(o *domain.UsageObservation) { o.Quantity = -1 }, domain.ErrNonPositive},
		{"zero sequence", func(o *domain.UsageObservation) { o.Sequence = 0 }, domain.ErrNonPositive},
		{"missing time", func(o *domain.UsageObservation) { o.ObservedAt = time.Time{} }, domain.ErrMissingTimestamp},
		{"non-UTC time", func(o *domain.UsageObservation) {
			o.ObservedAt = time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("other", 0))
		}, domain.ErrTimestampNotUTC},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			observation := usageObservationFixture()
			tc.mutate(&observation)
			if err := observation.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Validate() = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestUsageProjectionsKeepAbsenceDistinctFromZero(t *testing.T) {
	if got, err := domain.ProjectRunUsage(nil); err != nil || got != nil {
		t.Fatalf("empty run projection = %#v, want nil", got)
	}
	rows := usageProjectionFixture()
	runTotals, err := domain.ProjectRunUsage(rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(runTotals) != 3 {
		t.Fatalf("run totals = %#v, want three observed metrics", runTotals)
	}
	foundZero := false
	for _, total := range runTotals {
		if err := total.Validate(); err != nil {
			t.Fatalf("run total Validate() = %v", err)
		}
		if total.Metric == "output_tokens" && total.Quantity == 0 {
			foundZero = true
		}
	}
	if !foundZero {
		t.Fatalf("run totals = %#v, want observed zero", runTotals)
	}

	comparison, err := domain.ProjectUsageComparison(rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(comparison) != 2 {
		t.Fatalf("comparison = %#v, want two treatments", comparison)
	}
	for _, group := range comparison {
		if err := group.Validate(); err != nil {
			t.Fatalf("comparison group Validate() = %v", err)
		}
	}
}

func TestUsageProjectionsRejectQuantityOverflow(t *testing.T) {
	first := usageObservationFixture()
	first.Quantity = math.MaxInt64
	second := first
	second.InvocationID = "inv-2"
	second.Quantity = 1
	rows := []domain.UsageObservation{first, second}

	if _, err := domain.ProjectRunUsage(rows); !errors.Is(err, domain.ErrUsageQuantityOverflow) {
		t.Fatalf("run projection = %v, want %v", err, domain.ErrUsageQuantityOverflow)
	}
	if _, err := domain.ProjectUsageComparison(rows); !errors.Is(err, domain.ErrUsageQuantityOverflow) {
		t.Fatalf("comparison projection = %v, want %v", err, domain.ErrUsageQuantityOverflow)
	}
}

func TestUsageGoldens(t *testing.T) {
	runProjection, err := domain.ProjectRunUsage(usageProjectionFixture())
	if err != nil {
		t.Fatal(err)
	}
	comparison, err := domain.ProjectUsageComparison(usageProjectionFixture())
	if err != nil {
		t.Fatal(err)
	}
	fixtures := []struct {
		name  string
		value any
	}{
		{"usage_observation", usageObservationFixture()},
		{"usage_run_projection", runProjection},
		{"usage_comparison", comparison},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			body, err := json.MarshalIndent(fixture.value, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			golden.Assert(t, fixture.name, append(body, '\n'))
		})
	}
}
