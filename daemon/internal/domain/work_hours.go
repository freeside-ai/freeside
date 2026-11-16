package domain

import (
	"cmp"
	"fmt"
	"slices"
	"time"
)

// MaxWorkHoursDays bounds a work-hours period: a quarter covers the days and
// weeks the work status view shows, with room to spare, and keeps the
// response small (issue #3241).
const MaxWorkHoursDays = 92

// WorkHoursPeriod names the calendar days a work-hours read covers: Days
// dates starting at From, each bounded in Location (WorkHoursOf).
type WorkHoursPeriod struct {
	// From is the first date; only its year, month, and day count.
	From     time.Time
	Days     int
	Location *time.Location
}

// Validate reports whether the period names 1 to MaxWorkHoursDays days in a
// time zone, from 0001-01-01 through 9999-12-30. The bounds keep every day's
// instants inside the years an RFC 3339 timestamp can carry.
func (p WorkHoursPeriod) Validate() error {
	if p.Days < 1 || p.Days > MaxWorkHoursDays {
		return fmt.Errorf("work hours days %d is outside 1 to %d: %w", p.Days, MaxWorkHoursDays, ErrInvalidWorkHoursPeriod)
	}
	if !WorkHoursPeriodInRange(p.From, p.Days) {
		return fmt.Errorf("work hours period from %s for %d days: %w",
			p.From.Format(time.DateOnly), p.Days, ErrInvalidWorkHoursPeriod)
	}
	if p.Location == nil {
		return fmt.Errorf("work hours time zone: %w", ErrInvalidWorkHoursPeriod)
	}
	return nil
}

// WorkHoursPeriodInRange reports whether days dates from from's date fall
// within 0001-01-01 through 9999-12-30, so that every day, bounded in any
// zone, starts and ends inside years 0 to 9999.
func WorkHoursPeriodInRange(from time.Time, days int) bool {
	year, month, day := from.Date()
	first := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	return first.Year() >= 1 && !first.AddDate(0, 0, days).After(time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC))
}

// WorkHoursRun is what WorkHoursOf reads of one run: the facts its timeline
// serves. An empty TaskID is a run with no task.
type WorkHoursRun struct {
	RunID      RunID
	TaskID     TaskID
	Milestones []RunMilestone
	// Finished is the run's lifecycle rule, as RunClocksOf reads it.
	Finished bool
	// Holds are the run's hold spans in the order they happened, ending
	// with its current hold as an open span.
	Holds []RunHoldSpan
}

// WorkHoursInput is everything a work-hours response is computed from.
type WorkHoursInput struct {
	Period WorkHoursPeriod
	AsOf   time.Time
	// ProjectID is the project the read was limited to, or nil for every
	// project. WorkHoursOf echoes it; the caller selects the runs.
	ProjectID        *ProjectID
	HoldHistorySince time.Time
	SkippedRunCount  int
	Runs             []WorkHoursRun
	// Items are attention items; each counts toward the run its subject
	// names, and an item that names no run adds nothing.
	Items []AttentionItem
}

// WorkHours is how runs' time divided over the days of a period (issue
// #3241). WorkHoursOf defines every figure.
type WorkHours struct {
	AsOf     time.Time `json:"as_of"`
	TimeZone string    `json:"time_zone"`
	// ProjectID is the project the read was limited to; nil for every
	// project, rendering explicit null.
	ProjectID *ProjectID `json:"project_id"`
	// HoldHistorySince is when the daemon began recording hold history
	// (#3235). Held time before it reads as other.
	HoldHistorySince time.Time `json:"hold_history_since"`
	// SkippedRunCount counts runs left out because their record failed the
	// read's integrity check; the figures are incomplete when it isn't zero.
	SkippedRunCount int             `json:"skipped_run_count"`
	Totals          WorkHoursTotals `json:"totals"`
	// Days has one entry per date, from the period's first date on. Never
	// nil.
	Days []WorkHoursDay `json:"days"`
}

// WorkHoursSeconds is the division of elapsed time into the four classes
// divideRunTime defines, each rounded down on its own.
type WorkHoursSeconds struct {
	ElapsedSeconds      int64 `json:"elapsed_seconds"`
	AgentWorkingSeconds int64 `json:"agent_working_seconds"`
	WaitingOnYouSeconds int64 `json:"waiting_on_you_seconds"`
	HeldSeconds         int64 `json:"held_seconds"`
	// HeldByReason lists only reasons with held time, largest first, then
	// by reason. Never nil, so it renders [].
	HeldByReason []WorkHoursHeld `json:"held_by_reason"`
	OtherSeconds int64           `json:"other_seconds"`
}

// WorkHoursTotals is the period's figures and the number of distinct tasks
// whose runs had elapsed time in it.
type WorkHoursTotals struct {
	WorkHoursSeconds
	TaskCount int `json:"task_count"`
}

// WorkHoursDay is one calendar date's figures: [StartsAt, EndsAt) is the
// date in the period's time zone, served as UTC instants.
type WorkHoursDay struct {
	Date     string    `json:"date"`
	StartsAt time.Time `json:"starts_at"`
	EndsAt   time.Time `json:"ends_at"`
	WorkHoursSeconds
	// Tasks are the tasks whose runs had elapsed time that day, largest
	// first, then by id. Never nil, so it renders [].
	Tasks []WorkHoursTask `json:"tasks"`
}

// WorkHoursHeld is the held time one hold reason accounts for.
type WorkHoursHeld struct {
	Reason  RunHoldReason `json:"reason"`
	Seconds int64         `json:"seconds"`
}

// WorkHoursTask is the elapsed time one task's runs had in a day.
type WorkHoursTask struct {
	TaskID         TaskID `json:"task_id"`
	ElapsedSeconds int64  `json:"elapsed_seconds"`
}

// WorkHoursOf divides each run's elapsed time into classes (divideRunTime)
// and adds it to the days of the period:
//
//   - A day runs from the start of its calendar date in the period's time
//     zone to the start of the next date, so it is 23 or 25 hours long
//     across a clock change. The daemon's own zone never applies.
//   - Time after AsOf counts nowhere, so the day holding AsOf is partial and
//     a later day is all zeros.
//   - A run's elapsed time on a day counts toward its task; a run with no
//     task adds hours but no task entry. Runs that overlap each count their
//     own time.
//   - Every seconds figure is computed from exact instants and rounded down
//     on its own, as RunClocksOf's are. So a day's classes can sum to less
//     than its elapsed seconds, and the days to less than the totals.
func WorkHoursOf(in WorkHoursInput) (WorkHours, error) {
	if err := in.Period.Validate(); err != nil {
		return WorkHours{}, err
	}
	waits := make(map[RunID][]AttentionItem)
	for _, item := range in.Items {
		if item.Subject.RunID != nil {
			waits[*item.Subject.RunID] = append(waits[*item.Subject.RunID], item)
		}
	}
	days := workHoursDays(in.Period)
	perDay := make([]classTime, len(days))
	taskTime := make([]map[TaskID]time.Duration, len(days))
	for _, run := range in.Runs {
		segments := divideRunTime(run, waits[run.RunID], in.AsOf)
		for i, day := range days {
			from, to := day.StartsAt, earlierOf(day.EndsAt, in.AsOf)
			var elapsed time.Duration
			for _, segment := range segments {
				overlap := earlierOf(segment.to, to).Sub(laterOf(segment.from, from))
				if overlap > 0 {
					perDay[i].add(segment, overlap)
					elapsed += overlap
				}
			}
			if elapsed > 0 && run.TaskID != "" {
				if taskTime[i] == nil {
					taskTime[i] = make(map[TaskID]time.Duration)
				}
				taskTime[i][run.TaskID] += elapsed
			}
		}
	}
	var total classTime
	tasks := make(map[TaskID]bool)
	for i := range days {
		total.addAll(perDay[i])
		days[i].WorkHoursSeconds = perDay[i].seconds()
		days[i].Tasks = workHoursTasks(taskTime[i])
		for task := range taskTime[i] {
			tasks[task] = true
		}
	}
	return WorkHours{
		AsOf: in.AsOf, TimeZone: in.Period.Location.String(), ProjectID: in.ProjectID,
		HoldHistorySince: in.HoldHistorySince, SkippedRunCount: in.SkippedRunCount,
		Totals: WorkHoursTotals{WorkHoursSeconds: total.seconds(), TaskCount: len(tasks)},
		Days:   days,
	}, nil
}

// workHoursDays bounds each date of the period in its time zone.
func workHoursDays(period WorkHoursPeriod) []WorkHoursDay {
	year, month, day := period.From.Date()
	days := make([]WorkHoursDay, period.Days)
	for i := range days {
		date := time.Date(year, month, day+i, 12, 0, 0, 0, time.UTC)
		days[i] = WorkHoursDay{
			Date:     date.Format(time.DateOnly),
			StartsAt: startOfDate(date, period.Location),
			EndsAt:   startOfDate(date.AddDate(0, 0, 1), period.Location),
		}
	}
	return days
}

// startOfDate is the first instant, in UTC, whose calendar date in loc is
// date's. That is local midnight, unless a clock change skips midnight: then
// it is the change itself. time.Date can resolve a skipped midnight to the
// hour before it, on the previous date, so that case moves to the end of the
// zone offset in force there, which is the change. Where a change repeats
// midnight, time.Date picks the later one, so that case moves to midnight
// under the offset in force before the change.
func startOfDate(date time.Time, loc *time.Location) time.Time {
	year, month, day := date.Date()
	sameDate := func(t time.Time) bool {
		y, m, d := t.Date()
		return y == year && m == month && d == day
	}
	start := time.Date(year, month, day, 0, 0, 0, 0, loc)
	if !sameDate(start) {
		_, start = start.ZoneBounds()
		return start.UTC()
	}
	if changed, _ := start.ZoneBounds(); !changed.IsZero() {
		if before := changed.Add(-time.Nanosecond); sameDate(before) {
			_, offset := before.Zone()
			start = time.Date(year, month, day, 0, 0, 0, 0, time.FixedZone("", offset))
		}
	}
	return start.UTC()
}

func workHoursTasks(times map[TaskID]time.Duration) []WorkHoursTask {
	ids := make([]TaskID, 0, len(times))
	for id := range times {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b TaskID) int {
		return cmp.Or(cmp.Compare(times[b], times[a]), cmp.Compare(a, b))
	})
	tasks := make([]WorkHoursTask, 0, len(ids))
	for _, id := range ids {
		tasks = append(tasks, WorkHoursTask{TaskID: id, ElapsedSeconds: wholeSeconds(times[id])})
	}
	return tasks
}

// classTime is exact time per class, rounded only when served.
type classTime struct {
	working, waiting, held, other time.Duration
	heldBy                        map[RunHoldReason]time.Duration
}

func (c *classTime) add(segment runTimeSegment, d time.Duration) {
	switch segment.class {
	case runTimeAgentWorking:
		c.working += d
	case runTimeWaitingOnYou:
		c.waiting += d
	case runTimeHeld:
		c.held += d
		if c.heldBy == nil {
			c.heldBy = make(map[RunHoldReason]time.Duration)
		}
		c.heldBy[segment.holdReason] += d
	case runTimeOther:
		c.other += d
	}
}

func (c *classTime) addAll(o classTime) {
	c.working += o.working
	c.waiting += o.waiting
	c.held += o.held
	c.other += o.other
	for reason, d := range o.heldBy {
		if c.heldBy == nil {
			c.heldBy = make(map[RunHoldReason]time.Duration)
		}
		c.heldBy[reason] += d
	}
}

func (c classTime) seconds() WorkHoursSeconds {
	reasons := make([]RunHoldReason, 0, len(c.heldBy))
	for reason := range c.heldBy {
		reasons = append(reasons, reason)
	}
	slices.SortFunc(reasons, func(a, b RunHoldReason) int {
		return cmp.Or(cmp.Compare(c.heldBy[b], c.heldBy[a]), cmp.Compare(a, b))
	})
	held := make([]WorkHoursHeld, 0, len(reasons))
	for _, reason := range reasons {
		held = append(held, WorkHoursHeld{Reason: reason, Seconds: wholeSeconds(c.heldBy[reason])})
	}
	return WorkHoursSeconds{
		ElapsedSeconds:      wholeSeconds(c.working + c.waiting + c.held + c.other),
		AgentWorkingSeconds: wholeSeconds(c.working),
		WaitingOnYouSeconds: wholeSeconds(c.waiting),
		HeldSeconds:         wholeSeconds(c.held),
		HeldByReason:        held,
		OtherSeconds:        wholeSeconds(c.other),
	}
}

// runTimeClass is the class an instant of a run's elapsed time belongs to.
// It is never served or stored (the response carries one field per class),
// so it is not a string enum; the zero value is invalid.
type runTimeClass int

const (
	runTimeAgentWorking runTimeClass = iota + 1
	runTimeWaitingOnYou
	runTimeHeld
	runTimeOther
)

// runTimeSegment is a stretch [from, to) of one class; holdReason is set
// only on a held segment.
type runTimeSegment struct {
	from, to   time.Time
	class      runTimeClass
	holdReason RunHoldReason
}

// divideRunTime divides a run's elapsed time into ordered, non-overlapping
// segments that cover it. This comment is the one definition of the classes
// (issue #3241); the run time strips (#3237) apply the same classes in the
// same order, so a day's bar and a run's strip agree.
//
// The elapsed time is RunClocksOf's: from the run's start to its end, or to
// asOf (never before its newest milestone) while it isn't finished. A run
// with no milestone has none. Every instant goes to exactly one class, the
// first of these that applies:
//
//  1. Agent working: inside one of the run's active intervals
//     (RunClocksOf).
//  2. Waiting on you: an attention item whose subject names the run is
//     open. It counts from the item's created_at to its conclusion's
//     concluded_at, or its decided_at on an item with no conclusion, or to
//     the run's end while the item is open. An item with no created_at, or
//     closed with neither instant, adds nothing. items must be the items
//     that name the run.
//  3. Held: inside one of the run's hold spans, from first_observed_at to
//     cleared_at, or to the run's end while the hold is current. The time
//     keeps the hold's reason; where two holds overlap, the one first
//     observed later wins, and of two observed at once the later span.
//  4. Other: what remains, such as queueing before admission,
//     verification, and publication.
//
// Every span is cut to the elapsed time, so a hold or an item that outlasts
// a finished run ends at the run's end.
func divideRunTime(run WorkHoursRun, items []AttentionItem, asOf time.Time) []runTimeSegment {
	if len(run.Milestones) == 0 {
		return nil
	}
	start, end := runBounds(run.Milestones, run.Finished, asOf)
	if !start.Before(end) {
		return nil
	}
	type span struct {
		from, to time.Time
		// reason and observed are set only on a hold.
		reason   RunHoldReason
		observed time.Time
	}
	// cut bounds a span to the elapsed time; a nil to runs to the run's end.
	cut := func(from time.Time, to *time.Time) (span, bool) {
		s := span{from: laterOf(from, start), to: end}
		if to != nil {
			s.to = earlierOf(*to, end)
		}
		return s, s.from.Before(s.to)
	}
	boundaries := []time.Time{start, end}
	collect := func(into *[]span, s span, ok bool) {
		if ok {
			*into = append(*into, s)
			boundaries = append(boundaries, s.from, s.to)
		}
	}
	var working, waiting, holds []span
	for _, interval := range RunClocksOf(run.Milestones, run.Finished, asOf).ActiveIntervals {
		s, ok := cut(interval.StartedAt, interval.EndedAt)
		collect(&working, s, ok)
	}
	for _, item := range items {
		if item.CreatedAt == nil {
			continue
		}
		var to *time.Time
		switch {
		case item.Conclusion != nil:
			to = &item.Conclusion.ConcludedAt
		case item.DecidedAt != nil:
			to = item.DecidedAt
		case item.Status != StatusOpen:
			continue
		}
		s, ok := cut(*item.CreatedAt, to)
		collect(&waiting, s, ok)
	}
	for _, hold := range run.Holds {
		s, ok := cut(hold.FirstObservedAt, hold.ClearedAt)
		s.reason, s.observed = hold.Reason, hold.FirstObservedAt
		collect(&holds, s, ok)
	}
	slices.SortFunc(boundaries, time.Time.Compare)
	boundaries = slices.CompactFunc(boundaries, time.Time.Equal)

	covers := func(s span, at time.Time) bool { return !at.Before(s.from) && at.Before(s.to) }
	classAt := func(at time.Time) (runTimeClass, RunHoldReason) {
		if slices.ContainsFunc(working, func(s span) bool { return covers(s, at) }) {
			return runTimeAgentWorking, ""
		}
		if slices.ContainsFunc(waiting, func(s span) bool { return covers(s, at) }) {
			return runTimeWaitingOnYou, ""
		}
		var latest *span
		for i := range holds {
			if covers(holds[i], at) && (latest == nil || !holds[i].observed.Before(latest.observed)) {
				latest = &holds[i]
			}
		}
		if latest != nil {
			return runTimeHeld, latest.reason
		}
		return runTimeOther, ""
	}
	// Every span starts and ends on a boundary, so one class holds from
	// each boundary to the next.
	var segments []runTimeSegment
	for i := 0; i+1 < len(boundaries); i++ {
		from, to := boundaries[i], boundaries[i+1]
		class, reason := classAt(from)
		if n := len(segments); n > 0 && segments[n-1].class == class && segments[n-1].holdReason == reason {
			segments[n-1].to = to
			continue
		}
		segments = append(segments, runTimeSegment{from: from, to: to, class: class, holdReason: reason})
	}
	return segments
}
