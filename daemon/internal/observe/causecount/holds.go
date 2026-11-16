package causecount

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Hold counts (issue #3239) tally each hold reason's holds, runs, and time
// held per window. Holds are recorded only from when their history began, so
// each window carries a coverage value, and a window with none prints null
// figures rather than zeros that would read as "no holds".

// HoldSpan is one hold: a cleared one from the hold history, or a run's
// current hold. End is nil while the hold is open. Reason is the stored
// string, so a reason this build doesn't know is still counted.
type HoldSpan struct {
	Reason string
	RunID  domain.RunID
	Start  time.Time
	End    *time.Time
}

// Coverage says how much of a window the hold history covers.
type Coverage string

// Coverage values.
const (
	// CoverageFull is a window that starts at or after the history began.
	CoverageFull Coverage = "full"
	// CoveragePartial is a window that straddles the history's start. Its
	// figures count only recorded holds, so they are a lower bound.
	CoveragePartial Coverage = "partial"
	// CoverageNone is a window that ends at or before the history began. Its
	// figures are null.
	CoverageNone Coverage = "none"
)

// AllCoverages lists every coverage value.
var AllCoverages = []Coverage{CoverageFull, CoveragePartial, CoverageNone}

func (c Coverage) valid() bool {
	switch c {
	case CoverageFull, CoveragePartial, CoverageNone:
		return true
	default:
		return false
	}
}

// CoverageOf is w's coverage by a history recorded since recordedSince.
func CoverageOf(w Window, recordedSince time.Time) Coverage {
	switch {
	case !w.Start.Before(recordedSince):
		return CoverageFull
	case !w.End.After(recordedSince):
		return CoverageNone
	default:
		return CoveragePartial
	}
}

// HoldCount is one hold reason's figures in the period and the previous
// period. A window's three figures are nil exactly when its coverage is none.
type HoldCount struct {
	Reason              string `json:"reason"`
	Known               bool   `json:"known"`
	Holds               *int   `json:"holds"`
	Runs                *int   `json:"runs"`
	HeldSeconds         *int64 `json:"held_seconds"`
	PreviousHolds       *int   `json:"previous_holds"`
	PreviousRuns        *int   `json:"previous_runs"`
	PreviousHeldSeconds *int64 `json:"previous_held_seconds"`
}

// Holds is the report's hold section. RecordedSince is when the hold history
// began; a hold that ended before it left no record.
type Holds struct {
	RecordedSince          time.Time   `json:"recorded_since"`
	PeriodCoverage         Coverage    `json:"period_coverage"`
	PreviousPeriodCoverage Coverage    `json:"previous_period_coverage"`
	Reasons                []HoldCount `json:"reasons"`
}

// holdTally accumulates one reason's figures in one window.
type holdTally struct {
	holds int
	runs  map[domain.RunID]bool
	held  time.Duration
}

// figures renders the tally, or nils when the window isn't covered. Held
// time is summed as a duration and truncated to whole seconds once.
func (t *holdTally) figures(covered bool) (holds, runs *int, held *int64) {
	if !covered {
		return nil, nil, nil
	}
	var h, r int
	var s int64
	if t != nil {
		h, r, s = t.holds, len(t.runs), int64(t.held/time.Second)
	}
	return &h, &r, &s
}

// ComputeHolds tallies each hold reason over period and the period before it.
// A hold counts in every window it overlaps, and only its time inside the
// window adds to that window's held time, so a hold across the boundary
// counts in both and splits its time. An open hold runs to now. A hold
// counts in a window when it starts inside it, or starts before it and ends
// after its start. A window whose coverage is none counts nothing. Reasons
// are listed when they have a hold in a counted window, sorted by reason.
func ComputeHolds(spans []HoldSpan, period Window, recordedSince, now time.Time) Holds {
	windows := [2]Window{period, period.Previous()}
	var covered [2]bool
	for i, w := range windows {
		covered[i] = CoverageOf(w, recordedSince) != CoverageNone
	}
	tallies := map[string]*[2]*holdTally{}
	for _, span := range spans {
		end := now
		if span.End != nil {
			end = *span.End
		}
		for i, w := range windows {
			if !covered[i] || !overlaps(span.Start, end, w) {
				continue
			}
			byWindow := tallies[span.Reason]
			if byWindow == nil {
				byWindow = &[2]*holdTally{}
				tallies[span.Reason] = byWindow
			}
			t := byWindow[i]
			if t == nil {
				t = &holdTally{runs: map[domain.RunID]bool{}}
				byWindow[i] = t
			}
			t.holds++
			if span.RunID != "" {
				t.runs[span.RunID] = true
			}
			t.held += clippedLength(span.Start, end, w)
		}
	}
	reasons := make([]HoldCount, 0, len(tallies))
	for reason, byWindow := range tallies {
		c := HoldCount{Reason: reason, Known: slices.Contains(domain.AllRunHoldReasons, domain.RunHoldReason(reason))}
		c.Holds, c.Runs, c.HeldSeconds = byWindow[0].figures(covered[0])
		c.PreviousHolds, c.PreviousRuns, c.PreviousHeldSeconds = byWindow[1].figures(covered[1])
		reasons = append(reasons, c)
	}
	slices.SortFunc(reasons, func(a, b HoldCount) int { return strings.Compare(a.Reason, b.Reason) })
	return Holds{
		RecordedSince:          recordedSince,
		PeriodCoverage:         CoverageOf(windows[0], recordedSince),
		PreviousPeriodCoverage: CoverageOf(windows[1], recordedSince),
		Reasons:                reasons,
	}
}

// overlaps reports whether the hold [start, end] counts in w. A hold that
// starts inside w counts even when it lasted no time, so an instant hold is
// never dropped at a window boundary.
func overlaps(start, end time.Time, w Window) bool {
	return w.contains(start) || (start.Before(w.Start) && end.After(w.Start))
}

// clippedLength is the length of [start, end] inside w, never negative: an
// open hold first observed after now has no time yet.
func clippedLength(start, end time.Time, w Window) time.Duration {
	from, to := start, end
	if from.Before(w.Start) {
		from = w.Start
	}
	if to.After(w.End) {
		to = w.End
	}
	if !to.After(from) {
		return 0
	}
	return to.Sub(from)
}

// validate checks the hold section against the report's windows: coverage
// matches recorded_since, a window's figures are null exactly when its
// coverage is none, runs never exceed holds, held time isn't negative, a
// window with no holds has no runs or time, and each listed reason is unique,
// sorted, non-empty, and has a hold in at least one window.
func (h Holds) validate(period, previous Window) error {
	switch {
	case h.RecordedSince.IsZero():
		return errors.New("recorded_since is not set")
	case !h.PeriodCoverage.valid() || !h.PreviousPeriodCoverage.valid():
		return errors.New("unknown coverage")
	case h.PeriodCoverage != CoverageOf(period, h.RecordedSince):
		return errors.New("period coverage does not match recorded_since")
	case h.PreviousPeriodCoverage != CoverageOf(previous, h.RecordedSince):
		return errors.New("previous period coverage does not match recorded_since")
	}
	for i, c := range h.Reasons {
		if i > 0 && h.Reasons[i-1].Reason >= c.Reason {
			return fmt.Errorf("hold reason %q: reasons are not unique and sorted", c.Reason)
		}
		if err := c.validate(h.PeriodCoverage, h.PreviousPeriodCoverage); err != nil {
			return fmt.Errorf("hold reason %q: %w", c.Reason, err)
		}
	}
	return nil
}

func (c HoldCount) validate(periodCoverage, previousCoverage Coverage) error {
	if c.Reason == "" {
		return errors.New("empty reason")
	}
	if err := validateHoldFigures(periodCoverage, c.Holds, c.Runs, c.HeldSeconds); err != nil {
		return fmt.Errorf("period: %w", err)
	}
	if err := validateHoldFigures(previousCoverage, c.PreviousHolds, c.PreviousRuns, c.PreviousHeldSeconds); err != nil {
		return fmt.Errorf("previous period: %w", err)
	}
	if deref(c.Holds)+deref(c.PreviousHolds) == 0 {
		return errors.New("no holds")
	}
	return nil
}

func validateHoldFigures(coverage Coverage, holds, runs *int, held *int64) error {
	unset := coverage == CoverageNone
	switch {
	case (holds == nil) != unset || (runs == nil) != unset || (held == nil) != unset:
		return errors.New("figures must be null exactly when coverage is none")
	case unset:
		return nil
	case *holds < 0 || *runs < 0 || *held < 0:
		return errors.New("negative figure")
	case *runs > *holds:
		return errors.New("more runs than holds")
	case *holds == 0 && (*runs != 0 || *held != 0):
		return errors.New("runs or time held with no holds")
	}
	return nil
}

func deref(n *int) int {
	if n == nil {
		return 0
	}
	return *n
}
