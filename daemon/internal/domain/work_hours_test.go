package domain_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// workHoursClock builds the instants and records the work-hours tests share:
// minutes from t0 and one run's milestones.
type workHoursClock struct{ t0 time.Time }

func (c workHoursClock) at(minutes int) time.Time {
	return c.t0.Add(time.Duration(minutes) * time.Minute)
}

func (c workHoursClock) milestone(kind domain.RunMilestoneKind, invocation domain.InvocationID, minutes int) domain.RunMilestone {
	return domain.RunMilestone{RunID: "run-1", Kind: kind, InvocationID: &invocation, RecordedAt: c.at(minutes)}
}

// worked is a run submitted at submit whose one invocation worked from start
// to end.
func (c workHoursClock) worked(submit, start, end int) []domain.RunMilestone {
	return []domain.RunMilestone{
		c.milestone(domain.MilestoneRunSubmitted, "inv-submit", submit),
		c.milestone(domain.MilestoneInvocationStarted, "inv-work", start),
		c.milestone(domain.MilestoneExecutionExportRecorded, "inv-work", end),
	}
}

func (c workHoursClock) hold(reason domain.RunHoldReason, first int, cleared *int) domain.RunHoldSpan {
	s := domain.RunHoldSpan{RunID: "run-1", Reason: reason, FirstObservedAt: c.at(first), LastObservedAt: c.at(first)}
	if cleared != nil {
		s.ClearedAt = new(c.at(*cleared))
	}
	return s
}

// card is an attention item about run (nil names no run), created at
// created (nil for none), and open unless concluded.
func (c workHoursClock) card(run *domain.RunID, created *int, concluded *int) domain.AttentionItem {
	item := domain.AttentionItem{ID: "item-1", Subject: domain.Subject{RunID: run}, Status: domain.StatusOpen}
	if created != nil {
		item.CreatedAt = new(c.at(*created))
	}
	if concluded != nil {
		item.Status = domain.StatusResolved
		item.Conclusion = &domain.Conclusion{ConcludedAt: c.at(*concluded)}
	}
	return item
}

func oneDay(t0 time.Time) domain.WorkHoursPeriod {
	return domain.WorkHoursPeriod{From: t0, Days: 1, Location: time.UTC}
}

// TestWorkHoursDividesARunsTime covers the class order divideRunTime
// defines, over one UTC day that holds the whole run.
func TestWorkHoursDividesARunsTime(t *testing.T) {
	t.Parallel()
	c := workHoursClock{t0: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)}
	run1 := domain.RunID("run-1")
	other := domain.RunID("run-other")
	held := func(reason domain.RunHoldReason, seconds int64) domain.WorkHoursHeld {
		return domain.WorkHoursHeld{Reason: reason, Seconds: seconds}
	}
	cases := []struct {
		name     string
		run      domain.WorkHoursRun
		items    []domain.AttentionItem
		asOf     int
		want     domain.WorkHoursSeconds
		wantNone bool
	}{
		{
			name: "invocation overlapping an open card is agent working",
			run:  domain.WorkHoursRun{Milestones: c.worked(0, 10, 40)},
			// The card opens while the invocation works and stays open to
			// as_of.
			items: []domain.AttentionItem{c.card(&run1, new(20), nil)},
			asOf:  60,
			want: domain.WorkHoursSeconds{
				ElapsedSeconds: 3600, AgentWorkingSeconds: 1800, WaitingOnYouSeconds: 1200, OtherSeconds: 600,
			},
		},
		{
			name: "hold overlapping an open card is waiting on you",
			run: domain.WorkHoursRun{
				Milestones: c.worked(0, 10, 40),
				Holds:      []domain.RunHoldSpan{c.hold(domain.HoldMachineCapacity, 45, new(55))},
			},
			items: []domain.AttentionItem{c.card(&run1, new(50), nil)},
			asOf:  60,
			want: domain.WorkHoursSeconds{
				ElapsedSeconds: 3600, AgentWorkingSeconds: 1800, WaitingOnYouSeconds: 600,
				HeldSeconds: 300, HeldByReason: []domain.WorkHoursHeld{held(domain.HoldMachineCapacity, 300)},
				OtherSeconds: 900,
			},
		},
		{
			name: "the later of two overlapping holds wins",
			run: domain.WorkHoursRun{
				Milestones: c.worked(0, 10, 40),
				Holds: []domain.RunHoldSpan{
					c.hold(domain.HoldMachineCapacity, 42, new(58)),
					c.hold(domain.HoldOperationStopped, 45, new(50)),
				},
			},
			asOf: 60,
			want: domain.WorkHoursSeconds{
				ElapsedSeconds: 3600, AgentWorkingSeconds: 1800, HeldSeconds: 960,
				HeldByReason: []domain.WorkHoursHeld{
					held(domain.HoldMachineCapacity, 660), held(domain.HoldOperationStopped, 300),
				},
				OtherSeconds: 840,
			},
		},
		{
			name: "a hold past a finished run's end is cut at the end",
			run: domain.WorkHoursRun{
				Milestones: append(c.worked(0, 10, 40), c.milestone(domain.MilestonePublicationReady, "inv-publish", 50)),
				Finished:   true,
				Holds:      []domain.RunHoldSpan{c.hold(domain.HoldOperationStopped, 45, new(70))},
			},
			// An open card outlasts the run too; it ends with the run.
			items: []domain.AttentionItem{c.card(&run1, new(48), nil)},
			asOf:  90,
			want: domain.WorkHoursSeconds{
				ElapsedSeconds: 3000, AgentWorkingSeconds: 1800, WaitingOnYouSeconds: 120,
				HeldSeconds: 180, HeldByReason: []domain.WorkHoursHeld{held(domain.HoldOperationStopped, 180)},
				OtherSeconds: 900,
			},
		},
		{
			name: "a closed card counts to its conclusion",
			run:  domain.WorkHoursRun{Milestones: c.worked(0, 10, 40)},
			items: []domain.AttentionItem{
				c.card(&run1, new(45), new(50)),
				// Closed before conclusions existed: decided_at ends it.
				func() domain.AttentionItem {
					item := c.card(&run1, new(52), nil)
					item.Status, item.DecidedAt = domain.StatusResolved, new(c.at(54))
					return item
				}(),
				// Closed with neither instant: nothing to measure.
				func() domain.AttentionItem {
					item := c.card(&run1, new(56), nil)
					item.Status = domain.StatusResolved
					return item
				}(),
			},
			asOf: 60,
			want: domain.WorkHoursSeconds{
				ElapsedSeconds: 3600, AgentWorkingSeconds: 1800, WaitingOnYouSeconds: 420, OtherSeconds: 1380,
			},
		},
		{
			name: "an item naming no run, another run, or no creation adds nothing",
			run:  domain.WorkHoursRun{Milestones: c.worked(0, 10, 40)},
			items: []domain.AttentionItem{
				c.card(nil, new(0), nil),
				c.card(&other, new(0), nil),
				c.card(&run1, nil, nil),
			},
			asOf: 60,
			want: domain.WorkHoursSeconds{ElapsedSeconds: 3600, AgentWorkingSeconds: 1800, OtherSeconds: 1800},
		},
		{
			name: "time after as_of counts nowhere",
			// The newest milestone is after as_of, so the run's end is too;
			// only the part before as_of counts.
			run:  domain.WorkHoursRun{Milestones: c.worked(0, 10, 40)},
			asOf: 30,
			want: domain.WorkHoursSeconds{ElapsedSeconds: 1800, AgentWorkingSeconds: 1200, OtherSeconds: 600},
		},
		{
			name:     "a run with no milestone has no time",
			run:      domain.WorkHoursRun{},
			asOf:     60,
			wantNone: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.run.RunID, tc.run.TaskID = run1, "task-1"
			got, err := domain.WorkHoursOf(domain.WorkHoursInput{
				Period: oneDay(c.t0), AsOf: c.at(tc.asOf),
				Runs: []domain.WorkHoursRun{tc.run}, Items: tc.items,
			})
			if err != nil {
				t.Fatal(err)
			}
			want := tc.want
			if want.HeldByReason == nil {
				want.HeldByReason = []domain.WorkHoursHeld{}
			}
			if !reflect.DeepEqual(got.Totals.WorkHoursSeconds, want) {
				t.Errorf("totals = %+v, want %+v", got.Totals.WorkHoursSeconds, want)
			}
			if !reflect.DeepEqual(got.Days[0].WorkHoursSeconds, want) {
				t.Errorf("day = %+v, want the totals %+v", got.Days[0].WorkHoursSeconds, want)
			}
			wantTasks := []domain.WorkHoursTask{{TaskID: "task-1", ElapsedSeconds: want.ElapsedSeconds}}
			if tc.wantNone {
				wantTasks = []domain.WorkHoursTask{}
			}
			if !reflect.DeepEqual(got.Days[0].Tasks, wantTasks) {
				t.Errorf("tasks = %+v, want %+v", got.Days[0].Tasks, wantTasks)
			}
		})
	}
}

// TestWorkHoursBucketsByDay covers how a day is bounded: in the named zone,
// across a clock change, and against as_of.
func TestWorkHoursBucketsByDay(t *testing.T) {
	t.Parallel()
	load := func(name string) *time.Location {
		loc, err := time.LoadLocation(name)
		if err != nil {
			t.Fatal(err)
		}
		return loc
	}
	newYork := load("America/New_York")
	santiago := load("America/Santiago")
	amman := load("Asia/Amman")
	utc := func(s string) time.Time {
		at, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return at
	}
	type day struct {
		date, startsAt, endsAt string
		elapsed, working       int64
	}
	cases := []struct {
		name   string
		period domain.WorkHoursPeriod
		t0     time.Time
		run    func(c workHoursClock) []domain.RunMilestone
		asOf   int
		want   []day
	}{
		{
			name:   "an interval that crosses midnight splits between the days",
			period: domain.WorkHoursPeriod{From: utc("2026-11-16T00:00:00Z"), Days: 2, Location: time.UTC},
			t0:     utc("2026-11-16T23:30:00Z"),
			run:    func(c workHoursClock) []domain.RunMilestone { return c.worked(0, 10, 50) },
			asOf:   90,
			want: []day{
				{"2026-11-16", "2026-11-16T00:00:00Z", "2026-11-17T00:00:00Z", 1800, 1200},
				{"2026-11-17", "2026-11-17T00:00:00Z", "2026-11-18T00:00:00Z", 3600, 1200},
			},
		},
		{
			name:   "the day a clock moves forward is 23 hours",
			period: domain.WorkHoursPeriod{From: utc("2026-03-08T00:00:00Z"), Days: 1, Location: newYork},
			t0:     utc("2026-03-07T12:00:00Z"),
			// Submitted the day before, never started, read the day after.
			run:  func(c workHoursClock) []domain.RunMilestone { return c.worked(0, 3000, 3000)[:1] },
			asOf: 2 * 24 * 60,
			want: []day{{"2026-03-08", "2026-03-08T05:00:00Z", "2026-03-09T04:00:00Z", 23 * 3600, 0}},
		},
		{
			name:   "the day a clock moves back is 25 hours",
			period: domain.WorkHoursPeriod{From: utc("2026-11-01T00:00:00Z"), Days: 1, Location: newYork},
			t0:     utc("2026-10-31T12:00:00Z"),
			run:    func(c workHoursClock) []domain.RunMilestone { return c.worked(0, 3000, 3000)[:1] },
			asOf:   2 * 24 * 60,
			want:   []day{{"2026-11-01", "2026-11-01T04:00:00Z", "2026-11-02T05:00:00Z", 25 * 3600, 0}},
		},
		{
			name: "a skipped midnight starts the day at the change",
			// Santiago moves from 00:00 -04 to 01:00 -03 on 6 September.
			period: domain.WorkHoursPeriod{From: utc("2026-09-05T00:00:00Z"), Days: 2, Location: santiago},
			t0:     utc("2026-09-05T00:00:00Z"),
			run:    func(c workHoursClock) []domain.RunMilestone { return c.worked(0, 3000, 3000)[:1] },
			asOf:   4 * 24 * 60,
			want: []day{
				{"2026-09-05", "2026-09-05T04:00:00Z", "2026-09-06T04:00:00Z", 24 * 3600, 0},
				{"2026-09-06", "2026-09-06T04:00:00Z", "2026-09-07T03:00:00Z", 23 * 3600, 0},
			},
		},
		{
			name: "a repeated midnight starts the day at the first one",
			// Amman moves from 01:00 +03 back to 00:00 +02 on 29 October 2021.
			period: domain.WorkHoursPeriod{From: utc("2021-10-28T00:00:00Z"), Days: 2, Location: amman},
			t0:     utc("2021-10-27T00:00:00Z"),
			run:    func(c workHoursClock) []domain.RunMilestone { return c.worked(0, 6000, 6000)[:1] },
			asOf:   4 * 24 * 60,
			want: []day{
				{"2021-10-28", "2021-10-27T21:00:00Z", "2021-10-28T21:00:00Z", 24 * 3600, 0},
				{"2021-10-29", "2021-10-28T21:00:00Z", "2021-10-29T22:00:00Z", 25 * 3600, 0},
			},
		},
		{
			name:   "today is partial and a future day is zero",
			period: domain.WorkHoursPeriod{From: utc("2026-11-16T00:00:00Z"), Days: 2, Location: time.UTC},
			t0:     utc("2026-11-16T09:00:00Z"),
			run:    func(c workHoursClock) []domain.RunMilestone { return c.worked(0, 10, 40)[:2] },
			asOf:   60,
			want: []day{
				{"2026-11-16", "2026-11-16T00:00:00Z", "2026-11-17T00:00:00Z", 3600, 3000},
				{"2026-11-17", "2026-11-17T00:00:00Z", "2026-11-18T00:00:00Z", 0, 0},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := workHoursClock{t0: tc.t0}
			got, err := domain.WorkHoursOf(domain.WorkHoursInput{
				Period: tc.period, AsOf: c.at(tc.asOf),
				Runs: []domain.WorkHoursRun{{RunID: "run-1", TaskID: "task-1", Milestones: tc.run(c)}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Days) != len(tc.want) {
				t.Fatalf("days = %d, want %d", len(got.Days), len(tc.want))
			}
			var elapsed int64
			for i, want := range tc.want {
				d := got.Days[i]
				gotDay := day{
					d.Date, d.StartsAt.Format(time.RFC3339), d.EndsAt.Format(time.RFC3339),
					d.ElapsedSeconds, d.AgentWorkingSeconds,
				}
				if gotDay != want {
					t.Errorf("day %d = %+v, want %+v", i, gotDay, want)
				}
				elapsed += d.ElapsedSeconds
			}
			if got.Totals.ElapsedSeconds != elapsed {
				t.Errorf("totals elapsed = %d, want the days' %d", got.Totals.ElapsedSeconds, elapsed)
			}
		})
	}
}

// TestWorkHoursGroupsTasks covers the per-day task list and the period's
// task count: largest first, a run with no task adding hours only, and two
// runs of one task adding up.
func TestWorkHoursGroupsTasks(t *testing.T) {
	t.Parallel()
	c := workHoursClock{t0: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)}
	got, err := domain.WorkHoursOf(domain.WorkHoursInput{
		Period: domain.WorkHoursPeriod{From: c.t0, Days: 2, Location: time.UTC}, AsOf: c.at(60),
		Runs: []domain.WorkHoursRun{
			{RunID: "run-a1", TaskID: "task-a", Milestones: c.worked(30, 40, 50)},
			{RunID: "run-b", TaskID: "task-b", Milestones: c.worked(0, 10, 20)},
			{RunID: "run-a2", TaskID: "task-a", Milestones: c.worked(50, 55, 58)},
			{RunID: "run-none", Milestones: c.worked(0, 10, 20)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantTasks := []domain.WorkHoursTask{{TaskID: "task-b", ElapsedSeconds: 3600}, {TaskID: "task-a", ElapsedSeconds: 2400}}
	if !reflect.DeepEqual(got.Days[0].Tasks, wantTasks) {
		t.Errorf("tasks = %+v, want %+v", got.Days[0].Tasks, wantTasks)
	}
	if got.Totals.TaskCount != 2 || got.Totals.ElapsedSeconds != 3*3600-1200 {
		t.Errorf("totals = %+v, want 2 tasks and every run's elapsed time", got.Totals)
	}
	if len(got.Days[1].Tasks) != 0 || got.Days[1].Tasks == nil {
		t.Errorf("future day tasks = %#v, want an empty list", got.Days[1].Tasks)
	}
}

// TestWorkHoursRoundsEachFigureDown pins that every figure rounds down on
// its own, so classes can sum to less than the elapsed seconds.
func TestWorkHoursRoundsEachFigureDown(t *testing.T) {
	t.Parallel()
	c := workHoursClock{t0: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)}
	milestones := c.worked(0, 0, 0)
	milestones[1].RecordedAt = c.t0.Add(1500 * time.Millisecond)
	got, err := domain.WorkHoursOf(domain.WorkHoursInput{
		Period: oneDay(c.t0), AsOf: c.t0.Add(3 * time.Second),
		Runs: []domain.WorkHoursRun{{RunID: "run-1", Milestones: milestones[:2]}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if s := got.Totals; s.ElapsedSeconds != 3 || s.AgentWorkingSeconds != 1 || s.OtherSeconds != 1 {
		t.Errorf("totals = %+v, want 3 elapsed of 1.5 working and 1.5 other, each rounded down", s)
	}
}

func TestWorkHoursRefusesAnInvalidPeriod(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	for _, period := range []domain.WorkHoursPeriod{
		{From: from, Days: 0, Location: time.UTC},
		{From: from, Days: domain.MaxWorkHoursDays + 1, Location: time.UTC},
		{From: from, Days: 1},
		{From: time.Date(0, 12, 31, 0, 0, 0, 0, time.UTC), Days: 1, Location: time.UTC},
		{From: time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC), Days: 1, Location: time.UTC},
		{From: time.Date(9999, 12, 1, 0, 0, 0, 0, time.UTC), Days: 31, Location: time.UTC},
	} {
		if _, err := domain.WorkHoursOf(domain.WorkHoursInput{Period: period, AsOf: from}); !errors.Is(err, domain.ErrInvalidWorkHoursPeriod) {
			t.Errorf("period %+v: err = %v, want ErrInvalidWorkHoursPeriod", period, err)
		}
	}
	// The outermost periods serve days whose instants every zone keeps
	// inside the years a timestamp can carry.
	for _, period := range []domain.WorkHoursPeriod{
		{From: time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC), Days: 1, Location: time.FixedZone("", 14*3600)},
		{From: time.Date(9999, 12, 1, 0, 0, 0, 0, time.UTC), Days: 30, Location: time.FixedZone("", -12*3600)},
	} {
		hours, err := domain.WorkHoursOf(domain.WorkHoursInput{Period: period, AsOf: from})
		if err != nil {
			t.Fatalf("period %+v: %v", period, err)
		}
		if _, err := json.Marshal(hours); err != nil {
			t.Errorf("period %+v: marshal: %v", period, err)
		}
	}
}
