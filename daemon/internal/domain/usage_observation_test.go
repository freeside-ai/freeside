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
		{"partial source", func(o *domain.UsageObservation) { o.Source = domain.UsageSourcePartialTranscript }, nil},
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

// invocationUsageFixture holds one invocation of each coverage: inv-complete
// reported a terminal result and also has a partial row its result
// supersedes, inv-partial ended early, and inv-missing reported nothing.
func invocationUsageFixture() ([]domain.InvocationID, []domain.UsageObservation) {
	base := usageObservationFixture()
	base.InvocationID = "inv-complete"
	completeOutput := base
	completeOutput.Metric = "output_tokens"
	completeOutput.Quantity = 40
	completeCost := base
	completeCost.Kind = domain.UsageMeasurementBillableCost
	completeCost.Metric = "total_cost"
	completeCost.Unit = "usd_micros"
	completeCost.Quantity = 1_250_000
	superseded := base
	superseded.Source = domain.UsageSourcePartialTranscript
	superseded.Quantity = 60

	partialInput := base
	partialInput.InvocationID = "inv-partial"
	partialInput.Source = domain.UsageSourcePartialTranscript
	partialInput.Quantity = 306
	partialCacheRead := partialInput
	partialCacheRead.Metric = "cache_read_input_tokens"
	partialCacheRead.Quantity = 30034

	admitted := []domain.InvocationID{"inv-partial", "inv-missing", "inv-complete"}
	return admitted, []domain.UsageObservation{
		base, completeOutput, completeCost, superseded, partialInput, partialCacheRead,
	}
}

func TestProjectInvocationUsageClassifiesCoverage(t *testing.T) {
	got, err := domain.ProjectInvocationUsage(invocationUsageFixture())
	if err != nil {
		t.Fatal(err)
	}
	type summary struct {
		id       domain.InvocationID
		coverage domain.UsageCoverage
		tokens   map[string]int64
	}
	want := []summary{
		{"inv-complete", domain.UsageCoverageComplete, map[string]int64{"input_tokens": 100, "output_tokens": 40}},
		{"inv-missing", domain.UsageCoverageMissing, map[string]int64{}},
		{"inv-partial", domain.UsageCoveragePartial, map[string]int64{"cache_read_input_tokens": 30034, "input_tokens": 306}},
	}
	if len(got) != len(want) {
		t.Fatalf("ProjectInvocationUsage() = %#v, want %d entries", got, len(want))
	}
	for i, entry := range got {
		if err := entry.Validate(); err != nil {
			t.Fatalf("entry %d Validate() = %v", i, err)
		}
		tokens := map[string]int64{}
		for _, total := range entry.Tokens {
			tokens[total.Metric] = total.Quantity
		}
		if entry.InvocationID != want[i].id || entry.Coverage != want[i].coverage ||
			len(tokens) != len(want[i].tokens) {
			t.Fatalf("entry %d = %#v, want %#v", i, entry, want[i])
		}
		for metric, quantity := range want[i].tokens {
			if tokens[metric] != quantity {
				t.Fatalf("entry %d %s = %d, want %d", i, metric, tokens[metric], quantity)
			}
		}
	}
}

func TestProjectInvocationUsageIncludesObservedInvocationsOutsideAdmitted(t *testing.T) {
	got, err := domain.ProjectInvocationUsage(nil, []domain.UsageObservation{usageObservationFixture()})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].InvocationID != "inv-1" || got[0].Coverage != domain.UsageCoverageComplete {
		t.Fatalf("ProjectInvocationUsage() = %#v, want inv-1 complete", got)
	}
}

func TestInvocationUsageValidate(t *testing.T) {
	valid := func() domain.InvocationUsage {
		return domain.InvocationUsage{
			InvocationID: "inv-1", Coverage: domain.UsageCoveragePartial,
			Tokens: []domain.UsageTotal{
				{Kind: domain.UsageMeasurementReportedUsage, Metric: "input_tokens", Unit: "tokens", Quantity: 1},
				{Kind: domain.UsageMeasurementReportedUsage, Metric: "output_tokens", Unit: "tokens", Quantity: 2},
			},
		}
	}
	cases := []struct {
		name   string
		mutate func(*domain.InvocationUsage)
		want   error
	}{
		{"valid", func(*domain.InvocationUsage) {}, nil},
		{"missing invocation", func(u *domain.InvocationUsage) { u.InvocationID = "" }, domain.ErrEmptyID},
		{"invalid coverage", func(u *domain.InvocationUsage) { u.Coverage = "other" }, domain.ErrInvalidUsageCoverage},
		{"nil tokens", func(u *domain.InvocationUsage) { u.Tokens = nil }, domain.ErrEmptyField},
		{"cost total", func(u *domain.InvocationUsage) {
			u.Tokens[0].Kind = domain.UsageMeasurementBillableCost
		}, domain.ErrInvalidUsageMeasurementKind},
		{"unsorted tokens", func(u *domain.InvocationUsage) {
			u.Tokens[0], u.Tokens[1] = u.Tokens[1], u.Tokens[0]
		}, domain.ErrKeysNotCanonical},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usage := valid()
			tc.mutate(&usage)
			if err := usage.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Validate() = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestUsageGoldens(t *testing.T) {
	runProjection, err := domain.ProjectRunUsage(usageProjectionFixture())
	if err != nil {
		t.Fatal(err)
	}
	invocationUsage, err := domain.ProjectInvocationUsage(invocationUsageFixture())
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
		{"usage_invocation_projection", invocationUsage},
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
