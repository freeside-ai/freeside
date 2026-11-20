package readyshare

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/observe/causecount"
)

var (
	periodEnd = time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	period    = causecount.Window{Start: periodEnd.Add(-7 * 24 * time.Hour), End: periodEnd}
)

func day(d, hour int) time.Time { return time.Date(2026, 11, d, hour, 0, 0, 0, time.UTC) }

const (
	ready       = string(domain.AttentionReadyForFinalReview)
	failure     = string(domain.AttentionExecutionFailure)
	spec        = string(domain.AttentionSpecApproval)
	health      = string(domain.AttentionSystemHealth)
	planned     = string(domain.InterruptionPlannedGate)
	exceptional = string(domain.InterruptionExceptional)
)

// fixtureItems covers each definition in Compute's comment; the comment on
// each task says what it exercises. The period is [Nov 9, Nov 16).
func fixtureItems() []Item {
	item := func(id, task, itemType, class string, at time.Time) Item {
		return Item{
			ItemID: domain.ItemID(id), TaskID: domain.TaskID(task), Type: itemType, Class: class, CreatedAt: at,
		}
	}
	return []Item{
		// task-a raised nothing before ready. An item at the ready instant and
		// one after it are not before ready.
		item("a-ready", "task-a", ready, planned, day(12, 0)),
		item("a-same", "task-a", failure, exceptional, day(12, 0)),
		item("a-after", "task-a", failure, exceptional, day(13, 0)),
		// task-b raised one item of each class first, one of them before the
		// period: only the ready time must fall in it. Its second ready item,
		// from a later cycle, doesn't move the ready time.
		item("b-spec", "task-b", spec, planned, day(1, 0)),
		item("b-fail", "task-b", failure, exceptional, day(13, 0)),
		item("b-ready", "task-b", ready, planned, day(14, 0)),
		item("b-ready-again", "task-b", ready, planned, day(15, 0)),
		// task-c raised several items first, one of a type this build doesn't
		// know.
		item("c-fail-2", "task-c", failure, exceptional, day(10, 10)),
		item("c-fail-1", "task-c", failure, exceptional, day(10, 0)),
		item("c-flare", "task-c", "solar_flare", exceptional, day(10, 11)),
		item("c-ready", "task-c", ready, planned, day(11, 0)),
		// task-d first reached ready before the period, so its ready item in
		// the period doesn't count it.
		item("d-fail", "task-d", failure, exceptional, day(4, 0)),
		item("d-ready", "task-d", ready, planned, day(5, 0)),
		item("d-ready-again", "task-d", ready, planned, day(13, 0)),
		// task-e never reached ready.
		item("e-fail", "task-e", failure, exceptional, day(12, 0)),
		// task-f reached ready at the period's start, which is inside it, with
		// two ready items at one instant: the lower item ID is the first.
		item("f-ready-2", "task-f", ready, planned, period.Start),
		item("f-ready-1", "task-f", ready, planned, period.Start),
		// task-g reached ready at the period's end, which is outside it.
		item("g-fail", "task-g", failure, exceptional, day(15, 0)),
		item("g-ready", "task-g", ready, planned, periodEnd),
		// Task-less items count only when created in the period, and never
		// as a task.
		item("health-in", "", health, exceptional, day(12, 0)),
		item("health-out", "", health, exceptional, day(2, 0)),
	}
}

func share(f float64) *float64 { return &f }

func fixtureReport() Report {
	return Report{
		Period:     period,
		TasksReady: 4, TasksReadyWithoutItems: 2, ShareWithoutItems: share(0.5),
		ByClass: []ClassCount{
			{Class: planned, Known: true, Items: 1, Tasks: 1},
			{Class: exceptional, Known: true, Items: 4, Tasks: 2},
		},
		ByType: []TypeCount{
			{Type: failure, Class: exceptional, Known: true, Items: 3, Tasks: 2},
			{Type: "solar_flare", Class: exceptional, Known: false, Items: 1, Tasks: 1},
			{Type: spec, Class: planned, Known: true, Items: 1, Tasks: 1},
		},
		Tasks: []Task{
			{TaskID: "task-f", ReadyItemID: "f-ready-1", ReadyAt: period.Start, Items: []ItemRef{}},
			{TaskID: "task-c", ReadyItemID: "c-ready", ReadyAt: day(11, 0), Items: []ItemRef{
				{ItemID: "c-fail-1", Type: failure, Class: exceptional, CreatedAt: day(10, 0)},
				{ItemID: "c-fail-2", Type: failure, Class: exceptional, CreatedAt: day(10, 10)},
				{ItemID: "c-flare", Type: "solar_flare", Class: exceptional, CreatedAt: day(10, 11)},
			}},
			{TaskID: "task-a", ReadyItemID: "a-ready", ReadyAt: day(12, 0), Items: []ItemRef{}},
			{TaskID: "task-b", ReadyItemID: "b-ready", ReadyAt: day(14, 0), Items: []ItemRef{
				{ItemID: "b-spec", Type: spec, Class: planned, CreatedAt: day(1, 0)},
				{ItemID: "b-fail", Type: failure, Class: exceptional, CreatedAt: day(13, 0)},
			}},
		},
		TaskLess: []TaskLessCount{{Type: health, Class: exceptional, Known: true, Items: 1}},
	}
}

func assertReport(t *testing.T, got, want Report) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.MarshalIndent(got, "", "  ")
		wantJSON, _ := json.MarshalIndent(want, "", "  ")
		t.Fatalf("Compute =\n%s\nwant\n%s", gotJSON, wantJSON)
	}
}

func TestComputeCountsItemsBeforeFirstReady(t *testing.T) {
	t.Parallel()
	assertReport(t, Compute(fixtureItems(), period), fixtureReport())
}

func TestComputeWithNoItems(t *testing.T) {
	t.Parallel()
	// The share is null, not zero, when nothing reached ready, and every
	// known class is still listed so the report reads the same each time.
	assertReport(t, Compute(nil, period), Report{
		Period: period,
		ByClass: []ClassCount{
			{Class: planned, Known: true},
			{Class: exceptional, Known: true},
		},
		ByType: []TypeCount{}, Tasks: []Task{}, TaskLess: []TaskLessCount{},
	})
}

func TestComputeListsUnknownClassAfterKnownOnes(t *testing.T) {
	t.Parallel()
	got := Compute([]Item{
		{ItemID: "x-fail", TaskID: "task-x", Type: failure, Class: "urgent", CreatedAt: day(10, 0)},
		{ItemID: "x-ready", TaskID: "task-x", Type: ready, Class: planned, CreatedAt: day(11, 0)},
	}, period)
	want := []ClassCount{
		{Class: planned, Known: true},
		{Class: exceptional, Known: true},
		{Class: "urgent", Known: false, Items: 1, Tasks: 1},
	}
	if !reflect.DeepEqual(got.ByClass, want) {
		t.Fatalf("ByClass = %+v, want %+v", got.ByClass, want)
	}
}

func TestReportGolden(t *testing.T) {
	t.Parallel()
	project := domain.ProjectID("proj-1")
	report := fixtureReport()
	report.ProjectID = &project
	report.Skipped = []causecount.Skipped{{Table: "attention_items", Row: "item-bad", Error: "no created_at"}}
	got, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "report", append(got, '\n'))
}
