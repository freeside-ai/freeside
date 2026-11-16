package causecount

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

var (
	periodEnd = time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	period    = Window{Start: periodEnd.Add(-7 * 24 * time.Hour), End: periodEnd}
)

func at(daysBeforeEnd float64) time.Time {
	return periodEnd.Add(-time.Duration(daysBeforeEnd * float64(24*time.Hour)))
}

func timePtr(t time.Time) *time.Time { return &t }

func TestPreviousWindowIsAdjacentAndSameLength(t *testing.T) {
	t.Parallel()
	got := period.Previous()
	want := Window{Start: periodEnd.Add(-14 * 24 * time.Hour), End: period.Start}
	if got != want {
		t.Fatalf("Previous() = %+v, want %+v", got, want)
	}
}

func TestComputeCountsPeriodAndPreviousPeriod(t *testing.T) {
	t.Parallel()
	occurrences := []Occurrence{
		// Two quota failures in one run and one in another: 3 occurrences, 2 runs.
		{Source: SourceFailureCause, Value: "quota", RunID: "run-1", At: at(6)},
		{Source: SourceFailureCause, Value: "quota", RunID: "run-1", At: at(2)},
		{Source: SourceFailureCause, Value: "quota", RunID: "run-2", At: at(4)},
		// One in the previous period.
		{Source: SourceFailureCause, Value: "quota", RunID: "run-0", At: at(10)},
		// The period's start is inside it; its end belongs to the next period.
		{Source: SourceTerminal, Value: "completed", RunID: "run-1", At: period.Start},
		{Source: SourceTerminal, Value: "completed", RunID: "run-3", At: periodEnd},
		// The previous period's start is inside it; anything older is ignored.
		{Source: SourceTerminal, Value: "failed", RunID: "run-0", At: period.Previous().Start},
		{Source: SourceTerminal, Value: "failed", RunID: "run-0", At: at(15)},
		// A value this build doesn't know is counted, not dropped.
		{Source: SourceReviewFailure, Value: "solar_flare", RunID: "run-2", At: at(1)},
		// An item with no subject run counts an occurrence and no run.
		{Source: SourceAttentionConclusion, Value: "expired", At: at(3)},
	}
	got := Compute(occurrences, period)
	want := []Count{
		{
			Source: SourceFailureCause, Cause: "quota", Known: true, Occurrences: 3, Runs: 2,
			FirstSeen: timePtr(at(6)), LastSeen: timePtr(at(2)), PreviousOccurrences: 1,
		},
		{
			Source: SourceTerminal, Cause: "completed", Known: true, Occurrences: 1, Runs: 1,
			FirstSeen: timePtr(period.Start), LastSeen: timePtr(period.Start),
		},
		{Source: SourceTerminal, Cause: "failed", Known: true, PreviousOccurrences: 1},
		{
			Source: SourceReviewFailure, Cause: "solar_flare", Known: false, Occurrences: 1, Runs: 1,
			FirstSeen: timePtr(at(1)), LastSeen: timePtr(at(1)),
		},
		{
			Source: SourceAttentionConclusion, Cause: "expired", Known: true, Occurrences: 1, Runs: 0,
			FirstSeen: timePtr(at(3)), LastSeen: timePtr(at(3)),
		},
	}
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.MarshalIndent(got, "", "  ")
		wantJSON, _ := json.MarshalIndent(want, "", "  ")
		t.Fatalf("Compute =\n%s\nwant\n%s", gotJSON, wantJSON)
	}
	report := Report{Period: period, PreviousPeriod: period.Previous(), Causes: got, Skipped: []Skipped{}}
	if err := report.Validate(); err != nil {
		t.Fatalf("computed report is invalid: %v", err)
	}
}

func TestComputeWithNoOccurrencesIsAnEmptyList(t *testing.T) {
	t.Parallel()
	got := Compute(nil, period)
	if got == nil || len(got) != 0 {
		t.Fatalf("Compute(nil) = %#v, want an empty non-nil slice so it renders []", got)
	}
}

// TestKnownCoversEveryDomainValue pins each source to its domain type: every
// value the type lists is known under its own source.
func TestKnownCoversEveryDomainValue(t *testing.T) {
	t.Parallel()
	values := map[Source][]string{}
	for _, v := range domain.AllFailureCauses {
		values[SourceFailureCause] = append(values[SourceFailureCause], string(v))
	}
	for _, v := range domain.AllObservedInvocationStatuses {
		values[SourceTerminal] = append(values[SourceTerminal], string(v))
	}
	for _, v := range domain.AllRunHoldReasons {
		values[SourcePublicationBlocked] = append(values[SourcePublicationBlocked], string(v))
	}
	for _, v := range domain.AllReviewFailureClasses {
		values[SourceReviewFailure] = append(values[SourceReviewFailure], string(v))
	}
	for _, v := range domain.AllConclusionCauses {
		values[SourceAttentionConclusion] = append(values[SourceAttentionConclusion], string(v))
	}
	for _, source := range AllSources {
		if !source.valid() {
			t.Errorf("source %q is not valid", source)
		}
		if len(values[source]) == 0 {
			t.Errorf("source %q has no domain values in this test", source)
		}
		for _, v := range values[source] {
			if !known(source, v) {
				t.Errorf("known(%q, %q) = false", source, v)
			}
		}
		if known(source, "solar_flare") {
			t.Errorf("known(%q, solar_flare) = true", source)
		}
	}
	if Source("").valid() {
		t.Error("the zero source is valid")
	}
}

func goldenReport() Report {
	project := domain.ProjectID("proj-1")
	return Report{
		ProjectID: &project, Period: period, PreviousPeriod: period.Previous(),
		Causes: []Count{
			{
				Source: SourceFailureCause, Cause: "quota", Known: true, Occurrences: 3, Runs: 2,
				FirstSeen: timePtr(at(6)), LastSeen: timePtr(at(2)), PreviousOccurrences: 1,
			},
			{Source: SourceTerminal, Cause: "failed", Known: true, PreviousOccurrences: 2},
			{
				Source: SourceReviewFailure, Cause: "solar_flare", Known: false, Occurrences: 1, Runs: 1,
				FirstSeen: timePtr(at(1)), LastSeen: timePtr(at(1)),
			},
		},
		Skipped: []Skipped{{
			Table: "run_milestones", Row: "42",
			Error: `recorded_at: parse RFC3339Nano timestamp: parsing time "yesterday" as "2006-01-02T15:04:05.999999999Z07:00": cannot parse "yesterday" as "2006"`,
		}},
	}
}

func TestReportGolden(t *testing.T) {
	t.Parallel()
	report := goldenReport()
	if err := report.Validate(); err != nil {
		t.Fatalf("golden fixture is invalid: %v", err)
	}
	got, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "report", append(got, '\n'))
}

func TestReportValidateRefusesBrokenInvariants(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*Report){
		"empty period":      func(r *Report) { r.Period.End = r.Period.Start },
		"detached previous": func(r *Report) { r.PreviousPeriod.End = r.PreviousPeriod.End.Add(time.Hour) },
		"unknown source":    func(r *Report) { r.Causes[0].Source = "" },
		"empty cause":       func(r *Report) { r.Causes[0].Cause = "" },
		"no occurrences":    func(r *Report) { r.Causes[1].PreviousOccurrences = 0 },
		"too many runs":     func(r *Report) { r.Causes[0].Runs = 4 },
		"missing first":     func(r *Report) { r.Causes[0].FirstSeen = nil },
		"stray last":        func(r *Report) { r.Causes[1].LastSeen = timePtr(at(1)) },
		"last before first": func(r *Report) { r.Causes[0].LastSeen = timePtr(at(7)) },
		"seen outside":      func(r *Report) { r.Causes[0].FirstSeen = timePtr(at(8)) },
	} {
		report := goldenReport()
		mutate(&report)
		if err := report.Validate(); err == nil {
			t.Errorf("%s: Validate accepted the report", name)
		}
	}
}
