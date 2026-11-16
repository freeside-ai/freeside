// Package causecount counts how often each typed cause occurred over a period,
// so a cause that keeps coming back is visible without an analysis (issue
// #3238). It is pure: the operator command reads the stored rows through the
// store and hands them here as occurrences, and nothing in this package reaches
// the store. The counts are advisory (Tier C) and grant nothing.
package causecount

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Source names where a cause is stored. Each source's values are one domain
// cause type, which known checks against.
type Source string

// Sources counted. The comment on each names its stored column.
const (
	// SourceFailureCause is a failed execution outcome's cause
	// (execution_outcomes.cause, a domain.FailureCause).
	SourceFailureCause Source = "failure_cause"
	// SourceTerminal is the terminal recorded for one of a run's invocations,
	// completed included (run_milestones.terminal, a
	// domain.ObservedInvocationStatus). A run whose terminal a later invocation
	// clears records another, so occurrences can exceed runs.
	SourceTerminal Source = "terminal"
	// SourcePublicationBlocked is the reason publication was blocked for one
	// of a run's invocations (run_milestones.reason, a domain.RunHoldReason).
	SourcePublicationBlocked Source = "publication_blocked"
	// SourceReviewFailure is a failed review invocation's class
	// (review_failures.failure_class, a domain.ReviewFailureClass).
	SourceReviewFailure Source = "review_failure"
	// SourceAttentionConclusion is how an attention item closed (the
	// conclusion's cause in attention_items.body, a domain.ConclusionCause).
	SourceAttentionConclusion Source = "attention_conclusion"
)

// AllSources lists every source in report order.
var AllSources = []Source{
	SourceFailureCause, SourceTerminal, SourcePublicationBlocked,
	SourceReviewFailure, SourceAttentionConclusion,
}

func (s Source) valid() bool {
	switch s {
	case SourceFailureCause, SourceTerminal, SourcePublicationBlocked,
		SourceReviewFailure, SourceAttentionConclusion:
		return true
	default:
		return false
	}
}

// known reports whether value is a member of the source's cause type in this
// build. A stored value a newer or older build wrote is still counted, under
// its stored string, so nothing silently drops out of the report.
func known(source Source, value string) bool {
	switch source {
	case SourceFailureCause:
		return slices.Contains(domain.AllFailureCauses, domain.FailureCause(value))
	case SourceTerminal:
		return slices.Contains(domain.AllObservedInvocationStatuses, domain.ObservedInvocationStatus(value))
	case SourcePublicationBlocked:
		return slices.Contains(domain.AllRunHoldReasons, domain.RunHoldReason(value))
	case SourceReviewFailure:
		return slices.Contains(domain.AllReviewFailureClasses, domain.ReviewFailureClass(value))
	case SourceAttentionConclusion:
		return slices.Contains(domain.AllConclusionCauses, domain.ConclusionCause(value))
	}
	return false
}

// Occurrence is one stored cause. An empty RunID means the row names no run,
// so it adds to Occurrences but not to Runs.
type Occurrence struct {
	Source Source
	Value  string
	RunID  domain.RunID
	At     time.Time
}

// Window is a half-open interval of time, [Start, End).
type Window struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

func (w Window) contains(at time.Time) bool {
	return !at.Before(w.Start) && at.Before(w.End)
}

// Previous is the window of the same length that ends where w starts.
func (w Window) Previous() Window {
	return Window{Start: w.Start.Add(-w.End.Sub(w.Start)), End: w.Start}
}

// Count is one cause's tally. FirstSeen and LastSeen bound its occurrences in
// the period and are nil when it occurred only in the previous period.
type Count struct {
	Source              Source     `json:"source"`
	Cause               string     `json:"cause"`
	Known               bool       `json:"known"`
	Occurrences         int        `json:"occurrences"`
	Runs                int        `json:"runs"`
	FirstSeen           *time.Time `json:"first_seen"`
	LastSeen            *time.Time `json:"last_seen"`
	PreviousOccurrences int        `json:"previous_occurrences"`
}

// Skipped is a stored row the read couldn't decode. The report carries it,
// rather than a log line, because the control route discards stderr.
type Skipped struct {
	Table string `json:"table"`
	Row   string `json:"row"`
	Error string `json:"error"`
}

// Report is the printed shape of `freesided cause-counts`. ProjectID is nil
// when every project counts.
type Report struct {
	ProjectID      *domain.ProjectID `json:"project_id"`
	Period         Window            `json:"period"`
	PreviousPeriod Window            `json:"previous_period"`
	Causes         []Count           `json:"causes"`
	Skipped        []Skipped         `json:"skipped"`
}

// Compute tallies every (source, value) that occurs in period or in the
// period before it, sorted by source in AllSources order, then by cause.
// Occurrences outside both windows are ignored.
func Compute(occurrences []Occurrence, period Window) []Count {
	previous := period.Previous()
	type key struct {
		source Source
		value  string
	}
	type tally struct {
		count Count
		runs  map[domain.RunID]bool
	}
	tallies := map[key]*tally{}
	for _, o := range occurrences {
		current := period.contains(o.At)
		if !current && !previous.contains(o.At) {
			continue
		}
		k := key{o.Source, o.Value}
		t := tallies[k]
		if t == nil {
			t = &tally{
				count: Count{Source: o.Source, Cause: o.Value, Known: known(o.Source, o.Value)},
				runs:  map[domain.RunID]bool{},
			}
			tallies[k] = t
		}
		if !current {
			t.count.PreviousOccurrences++
			continue
		}
		t.count.Occurrences++
		if o.RunID != "" {
			t.runs[o.RunID] = true
		}
		at := o.At
		if t.count.FirstSeen == nil || at.Before(*t.count.FirstSeen) {
			t.count.FirstSeen = &at
		}
		if t.count.LastSeen == nil || at.After(*t.count.LastSeen) {
			t.count.LastSeen = &at
		}
	}
	counts := make([]Count, 0, len(tallies))
	for _, t := range tallies {
		t.count.Runs = len(t.runs)
		counts = append(counts, t.count)
	}
	slices.SortFunc(counts, func(a, b Count) int {
		return cmp.Or(
			cmp.Compare(slices.Index(AllSources, a.Source), slices.Index(AllSources, b.Source)),
			cmp.Compare(a.Cause, b.Cause))
	})
	return counts
}

// Validate checks the invariants Compute guarantees for a report: a non-empty
// period with its previous period adjacent, and per cause a known source, a
// non-empty value, at least one occurrence across the two periods, no more
// runs than occurrences, and first and last seen set, ordered, and inside the
// period exactly when the cause occurred in it.
func (r Report) Validate() error {
	if !r.Period.Start.Before(r.Period.End) {
		return errors.New("period is empty")
	}
	previous := r.Period.Previous()
	if !r.PreviousPeriod.Start.Equal(previous.Start) || !r.PreviousPeriod.End.Equal(previous.End) {
		return errors.New("previous period does not end where the period starts")
	}
	for _, c := range r.Causes {
		if err := c.validate(r.Period); err != nil {
			return fmt.Errorf("cause %s/%q: %w", c.Source, c.Cause, err)
		}
	}
	return nil
}

func (c Count) validate(period Window) error {
	switch {
	case !c.Source.valid():
		return errors.New("unknown source")
	case c.Cause == "":
		return errors.New("empty cause")
	case c.Occurrences < 0 || c.PreviousOccurrences < 0 || c.Occurrences+c.PreviousOccurrences == 0:
		return errors.New("no occurrences")
	case c.Runs < 0 || c.Runs > c.Occurrences:
		return errors.New("more runs than occurrences")
	case (c.FirstSeen == nil) != (c.Occurrences == 0) || (c.LastSeen == nil) != (c.Occurrences == 0):
		return errors.New("first and last seen must be set exactly when the cause occurred in the period")
	case c.FirstSeen != nil && (!period.contains(*c.FirstSeen) || !period.contains(*c.LastSeen) ||
		c.LastSeen.Before(*c.FirstSeen)):
		return errors.New("first and last seen are out of order or outside the period")
	}
	return nil
}
