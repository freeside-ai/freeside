package usagecollect

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

var applyAt = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

func heldRecord() domain.PoolUsageObservation {
	return domain.PoolUsageObservation{
		Pool: sharedPool, Collector: enrollFirst, Plan: new("max"),
		LastAttempt: &domain.CollectionAttempt{Status: domain.CollectionAttemptSucceeded, At: applyAt},
		Enrollments: []domain.EnrollmentAvailability{
			{EnrollmentID: enrollSecond, Answer: domain.AvailabilityUnsupported, ObservedAt: applyAt},
		},
		ModelWindows: []domain.ModelWindow{{RouteModelID: "model-a", Window: "seven_day"}},
		Windows: []domain.WindowReading{{
			Window: "five_hour", EnrollmentID: enrollFirst, Generation: 1,
			Status: domain.WindowStatusAllowed, ResetsAt: new(applyAt.Add(time.Hour)), ObservedAt: applyAt,
		}},
		RefusalMarks: []domain.RefusalMark{{
			Window: "seven_day", InvocationID: "inv-1", ResetsAt: new(applyAt.Add(30 * time.Minute)), SetAt: applyAt,
		}},
	}
}

// TestFreshnessShowsAStaleReadingAsObserved covers stale after reset and
// after refresh age: the reading keeps its own times and is never shown as
// replenished.
func TestFreshnessShowsAStaleReadingAsObserved(t *testing.T) {
	t.Parallel()
	record := heldRecord()
	age := 24 * time.Hour
	for _, tc := range []struct {
		name string
		now  time.Time
		want Freshness
	}{
		{"fresh", applyAt.Add(time.Minute), FreshnessFresh},
		{"at its reset", applyAt.Add(time.Hour), FreshnessStaleReset},
		{"reset passed first", applyAt.Add(48 * time.Hour), FreshnessStaleReset},
	} {
		got := windowsFreshness(record, tc.now, age)
		if len(got) != 1 || got[0].Freshness != tc.want || !got[0].ObservedAt.Equal(applyAt) {
			t.Errorf("%s: %+v, want %s observed at %s", tc.name, got, tc.want, applyAt)
		}
	}
	record.Windows[0].ResetsAt = nil
	if got := windowsFreshness(record, applyAt.Add(age+time.Second), age); got[0].Freshness != FreshnessStaleAge {
		t.Errorf("past the refresh age: %+v", got)
	}
	if !reflect.DeepEqual(record.Windows, func() []domain.WindowReading {
		held := heldRecord().Windows
		held[0].ResetsAt = nil
		return held
	}()) {
		t.Error("freshness changed the reading")
	}
}

// TestApplyCollectionAnswers covers each answer's effect on what the record
// holds.
func TestApplyCollectionAnswers(t *testing.T) {
	t.Parallel()
	at := applyAt.Add(time.Hour)
	unsupported := applyCollection(heldRecord(), ward.UsageCollection{
		Availability: domain.AvailabilityUnsupported, Detail: "no usage endpoint",
	}, nil, at)
	if unsupported.Windows != nil || unsupported.LastAttempt.Status != domain.CollectionAttemptUnsupported ||
		unsupported.RefusalMarks == nil || unsupported.Plan == nil {
		t.Errorf("unsupported = %+v", unsupported)
	}
	if err := unsupported.Validate(); err != nil {
		t.Errorf("unsupported record invalid: %v", err)
	}

	invalid := applyCollection(heldRecord(), ward.UsageCollection{}, nil, at)
	if invalid.LastAttempt.Status != domain.CollectionAttemptFailed ||
		!reflect.DeepEqual(invalid.Windows, heldRecord().Windows) {
		t.Errorf("an answer with no availability = %+v", invalid)
	}

	failed := applyCollection(heldRecord(), ward.UsageCollection{}, errors.New("boom"), at)
	want := heldRecord()
	want.LastAttempt = &domain.CollectionAttempt{Status: domain.CollectionAttemptFailed, At: at, Detail: "boom"}
	want.Enrollments = []domain.EnrollmentAvailability{
		{EnrollmentID: enrollFirst, Answer: domain.AvailabilityFailed, ObservedAt: at},
		want.Enrollments[0],
	}
	if !reflect.DeepEqual(failed, want) {
		t.Errorf("failed = %+v, want %+v", failed, want)
	}

	sorted := applyCollection(heldRecord(), ward.UsageCollection{
		Availability: domain.AvailabilitySupported,
		Windows: []domain.WindowReading{
			{Window: "seven_day", EnrollmentID: enrollFirst, Generation: 1, Status: domain.WindowStatusAllowed, ObservedAt: at},
			{Window: "five_hour", EnrollmentID: enrollFirst, Generation: 1, Status: domain.WindowStatusAllowed, ObservedAt: at},
		},
	}, nil, at)
	if err := sorted.Validate(); err != nil || sorted.Plan != nil || sorted.ModelWindows != nil {
		t.Errorf("supported = %+v, %v", sorted, err)
	}
}

// TestApplyCollectionBoundsDetail covers the stored detail's bound: a long
// error or answer detail keeps its start and its end within
// maxCollectionDetailBytes, and a short one is stored as it came.
func TestApplyCollectionBoundsDetail(t *testing.T) {
	t.Parallel()
	at := applyAt.Add(time.Hour)
	long := "collect: " + strings.Repeat("x", 8<<20) + ": unknown window"
	supported := ward.UsageCollection{
		Availability: domain.AvailabilitySupported,
		Windows: []domain.WindowReading{
			{Window: "five_hour", EnrollmentID: enrollFirst, Generation: 1, Status: domain.WindowStatusAllowed, ObservedAt: at},
		},
	}
	for _, tc := range []struct {
		name   string
		result ward.UsageCollection
		err    error
	}{
		{"failed collection", ward.UsageCollection{}, errors.New(long)},
		{"supported answer", func() ward.UsageCollection { r := supported; r.Detail = long; return r }(), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			detail := applyCollection(heldRecord(), tc.result, tc.err, at).LastAttempt.Detail
			if len(detail) > maxCollectionDetailBytes {
				t.Fatalf("detail is %d bytes, more than %d", len(detail), maxCollectionDetailBytes)
			}
			if !strings.HasPrefix(detail, "collect: xxx") || !strings.HasSuffix(detail, "xxx: unknown window") ||
				!strings.Contains(detail, collectionDetailMarker) {
				t.Fatalf("detail lost its start or end: %q", detail)
			}
		})
	}

	// A multi-byte rune across either cut point is kept whole or dropped. The
	// shift on both ends moves each cut through every offset of a 3-byte rune.
	for shift := range 3 {
		pad := strings.Repeat("a", shift)
		detail := boundedDetail(pad + strings.Repeat("€", 1000) + pad)
		if len(detail) > maxCollectionDetailBytes || !utf8.ValidString(detail) {
			t.Errorf("shift %d: detail is %d bytes, valid UTF-8 %t", shift, len(detail), utf8.ValidString(detail))
		}
	}

	exact := strings.Repeat("z", maxCollectionDetailBytes)
	if got := boundedDetail(exact); got != exact {
		t.Errorf("a detail of exactly %d bytes was cut to %d", maxCollectionDetailBytes, len(got))
	}
	if got := boundedDetail(exact + "z"); len(got) > maxCollectionDetailBytes || !strings.Contains(got, collectionDetailMarker) {
		t.Errorf("a detail one byte over the bound = %d bytes, %q", len(got), got)
	}
}

// TestReassignedKeepsPoolFacts covers what a reassignment keeps.
func TestReassignedKeepsPoolFacts(t *testing.T) {
	t.Parallel()
	got := reassigned(heldRecord(), enrollSecond)
	want := domain.PoolUsageObservation{
		Pool: sharedPool, Collector: enrollSecond,
		Enrollments: heldRecord().Enrollments, RefusalMarks: heldRecord().RefusalMarks,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("reassigned = %+v, want %+v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("reassigned record invalid: %v", err)
	}
}

// TestNextResetIsTheEarliestFutureReset covers the reset timer's target.
func TestNextResetIsTheEarliestFutureReset(t *testing.T) {
	t.Parallel()
	record := heldRecord()
	if got, ok := nextReset(record, applyAt); !ok || !got.Equal(applyAt.Add(30*time.Minute)) {
		t.Errorf("next reset = %s, %v", got, ok)
	}
	if got, ok := nextReset(record, applyAt.Add(30*time.Minute)); !ok || !got.Equal(applyAt.Add(time.Hour)) {
		t.Errorf("after the mark's reset = %s, %v", got, ok)
	}
	if _, ok := nextReset(record, applyAt.Add(time.Hour)); ok {
		t.Error("a passed reset armed a timer")
	}
}

// TestEnumsRegisterEveryValue keeps each registration slice and validity
// predicate in step.
func TestEnumsRegisterEveryValue(t *testing.T) {
	t.Parallel()
	for _, freshness := range AllFreshness {
		if !freshness.valid() {
			t.Errorf("freshness %q invalid", freshness)
		}
	}
	for _, outcome := range AllOutcomes {
		if !outcome.valid() {
			t.Errorf("outcome %q invalid", outcome)
		}
	}
	if Freshness("").valid() || Outcome("").valid() {
		t.Error("a zero value is valid")
	}
}
