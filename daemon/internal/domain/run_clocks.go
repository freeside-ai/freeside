package domain

import (
	"slices"
	"time"
)

// RunClocks is where a run's time went: its elapsed and active time and the
// active intervals behind the active time. RunClocksOf defines every field;
// the daemon derives the clocks at each read and never stores them.
type RunClocks struct {
	StartedAt time.Time `json:"started_at"`
	// EndedAt is the run's end; nil while the run isn't finished, rendering
	// explicit null.
	EndedAt        *time.Time `json:"ended_at"`
	ElapsedSeconds int64      `json:"elapsed_seconds"`
	ActiveSeconds  int64      `json:"active_seconds"`
	// ActiveIntervals has one entry per invocation_started milestone, in
	// record order. Entries can overlap; ActiveSeconds counts shared time
	// once. Never nil, so it renders [].
	ActiveIntervals []RunActiveInterval `json:"active_intervals"`
}

// RunActiveInterval is the span one invocation was working.
type RunActiveInterval struct {
	InvocationID InvocationID `json:"invocation_id"`
	StartedAt    time.Time    `json:"started_at"`
	// EndedAt is nil while the invocation is still working at the read's
	// asOf, rendering explicit null.
	EndedAt *time.Time `json:"ended_at"`
}

// RunClocksOf computes a run's clocks from its milestones in record order,
// whether the run is finished, and the instant of the read. It returns nil
// for a run with no milestone, because there is nothing to measure from.
// This comment is the one definition of the clocks (issue #3236):
//
//   - The run starts at its run_submitted milestone, or at its earliest
//     milestone when it has none.
//   - The run ends at its newest milestone once it is finished. Finished is
//     the rule that sets the run's lifecycle (LifecycleOf on the settled
//     conclusion), so a timeline and its run never disagree. A run that
//     isn't finished has no end: its clocks count to asOf, and never to
//     before its newest milestone.
//   - Elapsed time runs from the start to the end.
//   - An invocation is working from its invocation_started milestone to the
//     first later milestone of its own that is execution_export_recorded,
//     execution_outcome_recorded, or terminal_recorded. An invocation with
//     none of the three is working until the next invocation_started, or
//     until the run's end when there is none.
//   - Active time is the part of the elapsed time during which at least one
//     invocation is working. Two invocations working at once count once.
//   - A hold doesn't reduce active time. A hold records that the daemon
//     refused or put off the run's next step; it doesn't stop an invocation
//     that has already started, so where a hold overlaps an active interval
//     that time is active. The reasoning is in
//     devlog/2026-11-16-1845-run-clock-definition.md.
//   - Seconds are whole. Each total is computed from the exact instants and
//     rounded down. The instants in the intervals stay exact.
//
// Later and earlier mean record order, which pairs each start with its own
// ending even when two instants tie. The milestones must be valid, so every
// invocation_started names its invocation. These are the rules the app's
// run time strip applies to the milestones it holds (#3233).
func RunClocksOf(milestones []RunMilestone, finished bool, asOf time.Time) *RunClocks {
	if len(milestones) == 0 {
		return nil
	}
	start, newest := milestones[0].RecordedAt, milestones[0].RecordedAt
	for _, m := range milestones[1:] {
		start = earlierOf(start, m.RecordedAt)
		newest = laterOf(newest, m.RecordedAt)
	}
	if at := slices.IndexFunc(milestones, func(m RunMilestone) bool {
		return m.Kind == MilestoneRunSubmitted
	}); at >= 0 {
		start = milestones[at].RecordedAt
	}
	end := laterOf(asOf, newest)
	if finished {
		end = newest
	}
	// endedAt is a fresh copy of the run's end for a finished run, and nil
	// for one that isn't, so no two fields share a pointer.
	endedAt := func() *time.Time {
		if !finished {
			return nil
		}
		at := end
		return &at
	}
	clocks := &RunClocks{
		StartedAt:       start,
		EndedAt:         endedAt(),
		ElapsedSeconds:  wholeSeconds(end.Sub(start)),
		ActiveIntervals: []RunActiveInterval{},
	}
	for i, m := range milestones {
		if m.Kind != MilestoneInvocationStarted {
			continue
		}
		interval := RunActiveInterval{InvocationID: *m.InvocationID, StartedAt: m.RecordedAt, EndedAt: endedAt()}
		if ending, ok := invocationEnding(milestones[i+1:], *m.InvocationID); ok {
			// Record order decides which milestone ends the span, and
			// nothing keeps recorded instants in that order: a clock stepped
			// back serves a zero-length span, never one that reads
			// backwards.
			ending = laterOf(ending, m.RecordedAt)
			interval.EndedAt = &ending
		}
		clocks.ActiveIntervals = append(clocks.ActiveIntervals, interval)
	}
	clocks.ActiveSeconds = wholeSeconds(coveredTime(clocks.ActiveIntervals, start, end))
	return clocks
}

// invocationEnding is the instant that ends a working span among the
// milestones recorded after its start: the invocation's own first ending
// milestone, else the next invocation_started.
func invocationEnding(after []RunMilestone, invocation InvocationID) (time.Time, bool) {
	next := -1
	for i, m := range after {
		switch m.Kind {
		case MilestoneExecutionExportRecorded, MilestoneExecutionOutcomeRecorded, MilestoneTerminalRecorded:
			if *m.InvocationID == invocation {
				return m.RecordedAt, true
			}
		case MilestoneInvocationStarted:
			if next < 0 {
				next = i
			}
		case MilestoneRunSubmitted, MilestoneInvocationAdmitted, MilestonePublicationReady,
			MilestonePublicationBlocked, MilestoneWorkUnitCompleted:
		}
	}
	if next < 0 {
		return time.Time{}, false
	}
	return after[next].RecordedAt, true
}

// coveredTime is the length of the union of the intervals, each clipped to
// [start, end]; an interval with no end runs to end.
func coveredTime(intervals []RunActiveInterval, start, end time.Time) time.Duration {
	type span struct{ from, to time.Time }
	spans := make([]span, 0, len(intervals))
	for _, interval := range intervals {
		to := end
		if interval.EndedAt != nil {
			to = earlierOf(*interval.EndedAt, end)
		}
		from := laterOf(interval.StartedAt, start)
		if from.Before(to) {
			spans = append(spans, span{from, to})
		}
	}
	slices.SortFunc(spans, func(a, b span) int { return a.from.Compare(b.from) })
	var total time.Duration
	var reached time.Time
	for _, s := range spans {
		from := laterOf(s.from, reached)
		if from.Before(s.to) {
			total += s.to.Sub(from)
			reached = s.to
		}
	}
	return total
}

// wholeSeconds rounds a non-negative duration down to whole seconds.
func wholeSeconds(d time.Duration) int64 {
	return int64(d / time.Second)
}

func earlierOf(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}
