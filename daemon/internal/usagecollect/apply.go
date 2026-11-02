// Package usagecollect runs usage collection and refresh over each usage
// pool's observation record (plan §10, usage collection rules). It decides
// when the pool's one collector is asked and what of its answer reaches the
// record; the collector itself is a ward.UsageCollector.
//
// Collection only observes. It calls the collector, never another enrollment,
// and writes nothing but the observation record: it redeems no reset credit,
// refreshes no token, and writes no auth store. Its imports are held to that
// by containment_test.go.
package usagecollect

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// applyCollection returns record after its collector's full attempt at at. A
// supported answer replaces the window set, so a window the report no longer
// names is dropped. An unsupported answer clears the windows, which the record
// refuses under an unsupported attempt. A failed attempt keeps the readings
// and their observation times and records only the failure. Every answer
// updates the collector's own availability entry.
func applyCollection(
	record domain.PoolUsageObservation, result ward.UsageCollection, callErr error, at time.Time,
) domain.PoolUsageObservation {
	answer, detail := result.Availability, result.Detail
	switch {
	case callErr != nil:
		answer, detail = domain.AvailabilityFailed, callErr.Error()
	case !slices.Contains(domain.AllAvailabilityAnswers, answer):
		answer, detail = domain.AvailabilityFailed, fmt.Sprintf("collector returned availability %q", answer)
	}
	var status domain.CollectionAttemptStatus
	switch answer {
	case domain.AvailabilitySupported:
		status = domain.CollectionAttemptSucceeded
		record.Plan = result.Plan
		record.Windows = slices.SortedFunc(slices.Values(result.Windows), func(left, right domain.WindowReading) int {
			return cmp.Compare(left.Window, right.Window)
		})
		record.ModelWindows = slices.SortedFunc(slices.Values(result.ModelWindows), func(left, right domain.ModelWindow) int {
			return cmp.Or(cmp.Compare(left.RouteModelID, right.RouteModelID), cmp.Compare(left.Window, right.Window))
		})
	case domain.AvailabilityUnsupported:
		status = domain.CollectionAttemptUnsupported
		record.Windows = nil
	case domain.AvailabilityFailed:
		status = domain.CollectionAttemptFailed
	}
	record.LastAttempt = &domain.CollectionAttempt{Status: status, At: at, Detail: detail}
	record.Enrollments = withAvailability(record.Enrollments, domain.EnrollmentAvailability{
		EnrollmentID: record.Collector, Answer: answer, ObservedAt: at,
	})
	return record
}

// withAvailability returns entries with entry in place of any earlier answer
// from its enrollment, in enrollment order.
func withAvailability(
	entries []domain.EnrollmentAvailability, entry domain.EnrollmentAvailability,
) []domain.EnrollmentAvailability {
	entries = slices.DeleteFunc(slices.Clone(entries), func(held domain.EnrollmentAvailability) bool {
		return held.EnrollmentID == entry.EnrollmentID
	})
	entries = append(entries, entry)
	slices.SortFunc(entries, func(left, right domain.EnrollmentAvailability) int {
		return cmp.Compare(left.EnrollmentID, right.EnrollmentID)
	})
	return entries
}

// applyPartial returns record with reading in place of its window's reading,
// leaving every other window. It refuses a reading from anyone but the
// collector, and any reading before the collector's last attempt succeeded:
// the store accepts a new reading only under a succeeded attempt, and a
// partial update must not stand in for the full report that would set one.
func applyPartial(
	record domain.PoolUsageObservation, reading domain.WindowReading,
) (domain.PoolUsageObservation, bool) {
	if reading.EnrollmentID != record.Collector || record.LastAttempt == nil ||
		record.LastAttempt.Status != domain.CollectionAttemptSucceeded {
		return record, false
	}
	windows := slices.DeleteFunc(slices.Clone(record.Windows), func(held domain.WindowReading) bool {
		return held.Window == reading.Window
	})
	windows = append(windows, reading)
	slices.SortFunc(windows, func(left, right domain.WindowReading) int {
		return cmp.Compare(left.Window, right.Window)
	})
	record.Windows = windows
	return record, true
}

// reassigned returns record under a new collector. Every reading must come
// from the current collector, so the old collector's windows, model map, plan,
// and refresh status go; the availability answers and refusal marks stay,
// because they describe the pool, not the collector.
func reassigned(record domain.PoolUsageObservation, collector domain.ClientEnrollmentID) domain.PoolUsageObservation {
	record.Collector = collector
	record.Plan = nil
	record.LastAttempt = nil
	record.ModelWindows = nil
	record.Windows = nil
	return record
}

// nextReset returns the earliest reset after now that the record names, on a
// reading or a refusal mark, and false when there is none.
func nextReset(record domain.PoolUsageObservation, now time.Time) (time.Time, bool) {
	var resets []time.Time
	for _, reading := range record.Windows {
		if reading.ResetsAt != nil && reading.ResetsAt.After(now) {
			resets = append(resets, *reading.ResetsAt)
		}
	}
	for _, mark := range record.RefusalMarks {
		if mark.ResetsAt != nil && mark.ResetsAt.After(now) {
			resets = append(resets, *mark.ResetsAt)
		}
	}
	if len(resets) == 0 {
		return time.Time{}, false
	}
	return slices.MinFunc(resets, time.Time.Compare), true
}

// resetMissed reports whether a reset the record names passed with no
// collection landing since: a window's reset after its reading, or a refusal
// mark's reset after the collector's last answer or before a failed one.
func resetMissed(record domain.PoolUsageObservation, now time.Time) bool {
	for _, reading := range record.Windows {
		if reading.ResetsAt != nil && !reading.ResetsAt.After(now) && reading.ObservedAt.Before(*reading.ResetsAt) {
			return true
		}
	}
	last := record.LastAttempt
	for _, mark := range record.RefusalMarks {
		if mark.ResetsAt != nil && !mark.ResetsAt.After(now) &&
			(last == nil || last.At.Before(*mark.ResetsAt) || last.Status == domain.CollectionAttemptFailed) {
			return true
		}
	}
	return false
}
