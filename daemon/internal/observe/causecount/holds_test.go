package causecount

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func intPtr(n int) *int     { return &n }
func secPtr(n int64) *int64 { return &n }
func days(d float64) int64  { return int64(d * 24 * 3600) }
func span(reason, run string, start, end float64) HoldSpan {
	return HoldSpan{Reason: reason, RunID: domain.RunID(run), Start: at(start), End: timePtr(at(end))}
}

// holdCount builds a count with both windows filled.
func holdCount(reason string, known bool, holds, runs int, held int64, prevHolds, prevRuns int, prevHeld int64) HoldCount {
	return HoldCount{
		Reason: reason, Known: known,
		Holds: intPtr(holds), Runs: intPtr(runs), HeldSeconds: secPtr(held),
		PreviousHolds: intPtr(prevHolds), PreviousRuns: intPtr(prevRuns), PreviousHeldSeconds: secPtr(prevHeld),
	}
}

func TestCoverageOf(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		recordedSince time.Time
		want          Coverage
	}{
		{period.Start.Add(-time.Hour), CoverageFull},
		{period.Start, CoverageFull},
		{at(3), CoveragePartial},
		{period.End, CoverageNone},
		{period.End.Add(time.Hour), CoverageNone},
	} {
		if got := CoverageOf(period, tc.recordedSince); got != tc.want {
			t.Errorf("CoverageOf(period, %s) = %s, want %s", tc.recordedSince, got, tc.want)
		}
	}
	for _, c := range AllCoverages {
		if !c.valid() {
			t.Errorf("coverage %q is not valid", c)
		}
	}
	if Coverage("").valid() {
		t.Error("the zero coverage is valid")
	}
}

func TestComputeHolds(t *testing.T) {
	t.Parallel()
	// The period is [at(7), at(0)) and the previous period [at(14), at(7)).
	spans := []HoldSpan{
		// One reason in several runs: three holds in two runs, 2.5 days held.
		span("machine_capacity", "run-1", 6, 5),
		span("machine_capacity", "run-2", 4, 3.5),
		span("machine_capacity", "run-1", 2, 1),
		// Across the period boundary: one hold in each window, a day in each.
		span("quota_window_exhausted", "run-3", 8, 6),
		// Still open: counted from at(1) to now.
		{Reason: "scope_conflict", RunID: "run-4", Start: at(1)},
		// A reason this build doesn't know, in the previous period only.
		span("solar_flare", "run-5", 10, 9),
		// An instant hold at the period's start counts there, with no time.
		span("update_drain", "run-6", 7, 7),
		// Ended where the previous period starts: in neither window.
		span("base_advanced", "run-7", 16, 14),
	}
	now := at(0.5)

	for name, tc := range map[string]struct {
		recordedSince time.Time
		want          Holds
	}{
		"both windows recorded": {
			recordedSince: at(20),
			want: Holds{
				PeriodCoverage: CoverageFull, PreviousPeriodCoverage: CoverageFull,
				Reasons: []HoldCount{
					holdCount("machine_capacity", true, 3, 2, days(2.5), 0, 0, 0),
					holdCount("quota_window_exhausted", true, 1, 1, days(1), 1, 1, days(1)),
					holdCount("scope_conflict", true, 1, 1, days(0.5), 0, 0, 0),
					holdCount("solar_flare", false, 0, 0, 0, 1, 1, days(1)),
					holdCount("update_drain", true, 1, 1, 0, 0, 0, 0),
				},
			},
		},
		// The previous period straddles recorded_since: its figures count what
		// was recorded, a lower bound.
		"previous period partial": {
			recordedSince: at(10),
			want: Holds{
				PeriodCoverage: CoverageFull, PreviousPeriodCoverage: CoveragePartial,
				Reasons: []HoldCount{
					holdCount("machine_capacity", true, 3, 2, days(2.5), 0, 0, 0),
					holdCount("quota_window_exhausted", true, 1, 1, days(1), 1, 1, days(1)),
					holdCount("scope_conflict", true, 1, 1, days(0.5), 0, 0, 0),
					holdCount("solar_flare", false, 0, 0, 0, 1, 1, days(1)),
					holdCount("update_drain", true, 1, 1, 0, 0, 0, 0),
				},
			},
		},
		// Recorded from the period's start: the previous period's figures are
		// null, and a reason held only then isn't listed.
		"previous period not recorded": {
			recordedSince: period.Start,
			want: Holds{
				PeriodCoverage: CoverageFull, PreviousPeriodCoverage: CoverageNone,
				Reasons: []HoldCount{
					{Reason: "machine_capacity", Known: true, Holds: intPtr(3), Runs: intPtr(2), HeldSeconds: secPtr(days(2.5))},
					{Reason: "quota_window_exhausted", Known: true, Holds: intPtr(1), Runs: intPtr(1), HeldSeconds: secPtr(days(1))},
					{Reason: "scope_conflict", Known: true, Holds: intPtr(1), Runs: intPtr(1), HeldSeconds: secPtr(days(0.5))},
					{Reason: "update_drain", Known: true, Holds: intPtr(1), Runs: intPtr(1), HeldSeconds: secPtr(0)},
				},
			},
		},
		"nothing recorded": {
			recordedSince: period.End,
			want:          Holds{PeriodCoverage: CoverageNone, PreviousPeriodCoverage: CoverageNone, Reasons: []HoldCount{}},
		},
	} {
		got := ComputeHolds(spans, period, tc.recordedSince, now)
		tc.want.RecordedSince = tc.recordedSince
		if !reflect.DeepEqual(got, tc.want) {
			gotJSON, _ := json.MarshalIndent(got, "", "  ")
			wantJSON, _ := json.MarshalIndent(tc.want, "", "  ")
			t.Errorf("%s: ComputeHolds =\n%s\nwant\n%s", name, gotJSON, wantJSON)
			continue
		}
		report := Report{
			Period: period, PreviousPeriod: period.Previous(), Causes: []Count{}, Holds: got, Skipped: []Skipped{},
		}
		if err := report.Validate(); err != nil {
			t.Errorf("%s: computed report is invalid: %v", name, err)
		}
	}
}

// TestComputeHoldsClipsOpenHoldToWindow pins an open hold's time when the
// period ended before now, and when it was first observed after now.
func TestComputeHoldsClipsOpenHoldToWindow(t *testing.T) {
	t.Parallel()
	spans := []HoldSpan{
		{Reason: "machine_capacity", RunID: "run-1", Start: at(2)},
		{Reason: "scope_conflict", RunID: "run-2", Start: at(1)},
	}
	got := ComputeHolds(spans, period, period.Start, periodEnd.Add(48*time.Hour)).Reasons
	if len(got) != 2 || *got[0].HeldSeconds != days(2) || *got[1].HeldSeconds != days(1) {
		t.Fatalf("open holds past the period end = %+v, want 2 and 1 days", got)
	}
	got = ComputeHolds(spans, period, period.Start, at(1.5)).Reasons
	if len(got) != 2 || *got[0].HeldSeconds != days(0.5) || *got[1].HeldSeconds != 0 || *got[1].Holds != 1 {
		t.Fatalf("open hold first observed after now = %+v, want 0.5 days and a hold with no time", got)
	}
}
