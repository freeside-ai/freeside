package domain_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

var observationAt = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

func validPoolUsageObservation() domain.PoolUsageObservation {
	return domain.PoolUsageObservation{
		Pool: "pool-claude", Collector: "enroll-a",
		LastAttempt: &domain.CollectionAttempt{Status: domain.CollectionAttemptSucceeded, At: observationAt},
		Enrollments: []domain.EnrollmentAvailability{
			{EnrollmentID: "enroll-a", Answer: domain.AvailabilitySupported, ObservedAt: observationAt},
			{EnrollmentID: "enroll-b", Answer: domain.AvailabilityUnsupported, ObservedAt: observationAt},
		},
		ModelWindows: []domain.ModelWindow{
			{RouteModelID: "model-a", Window: "seven_day_a"},
			{RouteModelID: "model-b", Window: "seven_day_b"},
		},
		Windows: []domain.WindowReading{
			observationReading("five_hour", domain.WindowStatusAllowed, nil),
			observationReading("seven_day_a", domain.WindowStatusAllowed, nil),
		},
		RefusalMarks: []domain.RefusalMark{
			{Window: "five_hour", InvocationID: "inv-1", SetAt: observationAt.Add(-time.Hour)},
		},
	}
}

func observationReading(window string, status domain.WindowStatus, resetsAt *time.Time) domain.WindowReading {
	return domain.WindowReading{
		Window: window, EnrollmentID: "enroll-a", Generation: 1, Status: status,
		ResetsAt: resetsAt, ObservedAt: observationAt,
	}
}

func TestPoolUsageObservationValidate(t *testing.T) {
	t.Parallel()
	inKolkata := observationAt.In(time.FixedZone("IST", 5*60*60))

	for name, tc := range map[string]struct {
		mutate func(*domain.PoolUsageObservation)
		want   error
	}{
		"empty pool":      {func(o *domain.PoolUsageObservation) { o.Pool = "" }, domain.ErrEmptyID},
		"empty collector": {func(o *domain.PoolUsageObservation) { o.Collector = "" }, domain.ErrEmptyID},
		"empty plan":      {func(o *domain.PoolUsageObservation) { o.Plan = new("") }, domain.ErrEmptyField},
		"attempt status zero": {
			func(o *domain.PoolUsageObservation) { o.LastAttempt.Status = "" },
			domain.ErrInvalidCollectionAttemptStatus,
		},
		"attempt time zero": {
			func(o *domain.PoolUsageObservation) { o.LastAttempt.At = time.Time{} }, domain.ErrMissingTimestamp,
		},
		"attempt time not UTC": {
			func(o *domain.PoolUsageObservation) { o.LastAttempt.At = inKolkata }, domain.ErrTimestampNotUTC,
		},
		"availability answer zero": {
			func(o *domain.PoolUsageObservation) { o.Enrollments[0].Answer = "" },
			domain.ErrInvalidAvailabilityAnswer,
		},
		"availability enrollment empty": {
			func(o *domain.PoolUsageObservation) { o.Enrollments[0].EnrollmentID = "" }, domain.ErrEmptyID,
		},
		"availability time zero": {
			func(o *domain.PoolUsageObservation) { o.Enrollments[0].ObservedAt = time.Time{} },
			domain.ErrMissingTimestamp,
		},
		"availability out of order": {
			func(o *domain.PoolUsageObservation) { slices.Reverse(o.Enrollments) }, domain.ErrKeysNotCanonical,
		},
		"availability duplicate": {
			func(o *domain.PoolUsageObservation) { o.Enrollments[1] = o.Enrollments[0] }, domain.ErrKeysNotCanonical,
		},
		"map model empty": {
			func(o *domain.PoolUsageObservation) { o.ModelWindows[0].RouteModelID = "" }, domain.ErrEmptyField,
		},
		"map window not a token": {
			func(o *domain.PoolUsageObservation) { o.ModelWindows[0].Window = "Seven Day" }, domain.ErrEmptyField,
		},
		"map out of order": {
			func(o *domain.PoolUsageObservation) { slices.Reverse(o.ModelWindows) }, domain.ErrKeysNotCanonical,
		},
		"map duplicate": {
			func(o *domain.PoolUsageObservation) { o.ModelWindows[1] = o.ModelWindows[0] }, domain.ErrKeysNotCanonical,
		},
		"reading window not a token": {
			func(o *domain.PoolUsageObservation) { o.Windows[0].Window = "" }, domain.ErrEmptyField,
		},
		"reading out of order": {
			func(o *domain.PoolUsageObservation) { slices.Reverse(o.Windows) }, domain.ErrKeysNotCanonical,
		},
		"reading duplicate window": {
			func(o *domain.PoolUsageObservation) { o.Windows[1] = o.Windows[0] }, domain.ErrKeysNotCanonical,
		},
		"reading status zero": {
			func(o *domain.PoolUsageObservation) { o.Windows[0].Status = "" }, domain.ErrInvalidWindowStatus,
		},
		"reading no observation time": {
			func(o *domain.PoolUsageObservation) { o.Windows[0].ObservedAt = time.Time{} },
			domain.ErrMissingTimestamp,
		},
		"reading observation time not UTC": {
			func(o *domain.PoolUsageObservation) { o.Windows[0].ObservedAt = inKolkata }, domain.ErrTimestampNotUTC,
		},
		"reading reset not UTC": {
			func(o *domain.PoolUsageObservation) { o.Windows[0].ResetsAt = &inKolkata }, domain.ErrTimestampNotUTC,
		},
		"reading no generation": {
			func(o *domain.PoolUsageObservation) { o.Windows[0].Generation = 0 }, domain.ErrNonPositive,
		},
		"reading no enrollment": {
			func(o *domain.PoolUsageObservation) { o.Windows[0].EnrollmentID = "" }, domain.ErrEmptyID,
		},
		"reading from another enrollment": {
			func(o *domain.PoolUsageObservation) { o.Windows[0].EnrollmentID = "enroll-b" },
			domain.ErrReadingNotFromCollector,
		},
		"reading with share and quantity": {
			func(o *domain.PoolUsageObservation) {
				o.Windows[0].UsedPercent = new(10)
				o.Windows[0].Quantity = new(int64(1))
				o.Windows[0].Unit = new("messages")
			},
			domain.ErrWindowReadingMeasureConflict,
		},
		"reading quantity without unit": {
			func(o *domain.PoolUsageObservation) { o.Windows[0].Quantity = new(int64(1)) },
			domain.ErrWindowReadingMeasureConflict,
		},
		"reading unit without quantity": {
			func(o *domain.PoolUsageObservation) { o.Windows[0].Unit = new("messages") },
			domain.ErrWindowReadingMeasureConflict,
		},
		"reading unit not a token": {
			func(o *domain.PoolUsageObservation) {
				o.Windows[0].Quantity = new(int64(1))
				o.Windows[0].Unit = new("Messages")
			},
			domain.ErrEmptyField,
		},
		"reading negative share": {
			func(o *domain.PoolUsageObservation) { o.Windows[0].UsedPercent = new(-1) }, domain.ErrNonPositive,
		},
		"reading negative quantity": {
			func(o *domain.PoolUsageObservation) {
				o.Windows[0].Quantity = new(int64(-1))
				o.Windows[0].Unit = new("messages")
			},
			domain.ErrNonPositive,
		},
		"reading zero length": {
			func(o *domain.PoolUsageObservation) { o.Windows[0].Length = new(time.Duration(0)) },
			domain.ErrNonPositive,
		},
		"window before any attempt": {
			func(o *domain.PoolUsageObservation) { o.LastAttempt = nil }, domain.ErrSyntheticWindow,
		},
		"window after unsupported": {
			func(o *domain.PoolUsageObservation) { o.LastAttempt.Status = domain.CollectionAttemptUnsupported },
			domain.ErrSyntheticWindow,
		},
		"mark no invocation": {
			func(o *domain.PoolUsageObservation) { o.RefusalMarks[0].InvocationID = "" }, domain.ErrEmptyID,
		},
		"mark no set time": {
			func(o *domain.PoolUsageObservation) { o.RefusalMarks[0].SetAt = time.Time{} },
			domain.ErrMissingTimestamp,
		},
		"mark reset not UTC": {
			func(o *domain.PoolUsageObservation) { o.RefusalMarks[0].ResetsAt = &inKolkata },
			domain.ErrTimestampNotUTC,
		},
		"mark duplicate window": {
			func(o *domain.PoolUsageObservation) { o.RefusalMarks = append(o.RefusalMarks, o.RefusalMarks[0]) },
			domain.ErrKeysNotCanonical,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			observation := validPoolUsageObservation()
			if err := observation.Validate(); err != nil {
				t.Fatalf("valid fixture: %v", err)
			}
			tc.mutate(&observation)
			if err := observation.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Validate() = %v, want %v", err, tc.want)
			}
		})
	}
}

// A statuses-only record is complete: the fixture path for an unsupported
// collector, and for a pool whose first write was a refusal mark.
func TestPoolUsageObservationValidateAcceptsNoWindows(t *testing.T) {
	t.Parallel()
	for name, attempt := range map[string]*domain.CollectionAttempt{
		"no attempt":  nil,
		"unsupported": {Status: domain.CollectionAttemptUnsupported, At: observationAt},
		"failed":      {Status: domain.CollectionAttemptFailed, At: observationAt},
	} {
		observation := domain.PoolUsageObservation{Pool: "pool", Collector: "enroll", LastAttempt: attempt}
		if err := observation.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestExhaustedMarkValidate(t *testing.T) {
	t.Parallel()
	invocation := domain.InvocationID("inv-1")
	for name, tc := range map[string]struct {
		mark domain.ExhaustedMark
		want error
	}{
		"refusal": {domain.ExhaustedMark{
			Window: "five_hour", Source: domain.ExhaustedMarkProviderRefusal,
			InvocationID: &invocation, SetAt: observationAt,
		}, nil},
		"reading": {domain.ExhaustedMark{
			Window: "five_hour", Source: domain.ExhaustedMarkUsageReading,
			ReadingObservedAt: &observationAt, SetAt: observationAt,
		}, nil},
		"source zero": {domain.ExhaustedMark{
			Window: "five_hour", InvocationID: &invocation, SetAt: observationAt,
		}, domain.ErrInvalidExhaustedMarkSource},
		"refusal without invocation": {domain.ExhaustedMark{
			Window: "five_hour", Source: domain.ExhaustedMarkProviderRefusal, SetAt: observationAt,
		}, domain.ErrExhaustedMarkInconsistent},
		"reading naming an invocation": {domain.ExhaustedMark{
			Window: "five_hour", Source: domain.ExhaustedMarkUsageReading, InvocationID: &invocation,
			ReadingObservedAt: &observationAt, SetAt: observationAt,
		}, domain.ErrExhaustedMarkInconsistent},
		"no set time": {domain.ExhaustedMark{
			Window: "five_hour", Source: domain.ExhaustedMarkProviderRefusal, InvocationID: &invocation,
		}, domain.ErrMissingTimestamp},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := tc.mark.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Validate() = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestPoolUsageObservationExhaustedMarks(t *testing.T) {
	t.Parallel()
	const hour = time.Hour
	at := func(offset time.Duration) *time.Time { return new(observationAt.Add(offset)) }
	refusal := func(setAt time.Duration, resetsAt *time.Time) domain.RefusalMark {
		return domain.RefusalMark{
			Window: "five_hour", InvocationID: "inv-1", ResetsAt: resetsAt, SetAt: observationAt.Add(setAt),
		}
	}
	reading := func(status domain.WindowStatus, observed time.Duration, resetsAt *time.Time) domain.WindowReading {
		r := observationReading("five_hour", status, resetsAt)
		r.ObservedAt = observationAt.Add(observed)
		return r
	}
	allowed, exhausted := domain.WindowStatusAllowed, domain.WindowStatusExhausted

	for name, tc := range map[string]struct {
		refusal *domain.RefusalMark
		reading *domain.WindowReading
		now     time.Duration
		// want is the source of the expected mark; empty means no mark.
		want       domain.ExhaustedMarkSource
		wantResets *time.Time
	}{
		"mark without reset stays": {
			refusal: new(refusal(0, nil)), now: 1000 * hour, want: domain.ExhaustedMarkProviderRefusal,
		},
		"mark without reset stays past an earlier allowed reading": {
			refusal: new(refusal(0, nil)), reading: new(reading(allowed, -hour, nil)), now: hour,
			want: domain.ExhaustedMarkProviderRefusal,
		},
		"mark without reset stays past an allowed reading at the same instant": {
			refusal: new(refusal(0, nil)), reading: new(reading(allowed, 0, nil)), now: hour,
			want: domain.ExhaustedMarkProviderRefusal,
		},
		"mark without reset clears on a later allowed reading": {
			refusal: new(refusal(0, nil)), reading: new(reading(allowed, time.Second, nil)), now: hour,
		},
		"mark with reset holds until the instant before": {
			refusal: new(refusal(0, at(hour))), now: hour - time.Nanosecond,
			want: domain.ExhaustedMarkProviderRefusal, wantResets: at(hour),
		},
		"mark with reset clears at that instant": {
			refusal: new(refusal(0, at(hour))), now: hour,
		},
		"mark does not apply before it was set": {
			refusal: new(refusal(hour, nil)), now: 0,
		},
		"exhausted reading holds until the instant before its reset": {
			reading: new(reading(exhausted, 0, at(hour))), now: hour - time.Nanosecond,
			want: domain.ExhaustedMarkUsageReading, wantResets: at(hour),
		},
		"exhausted reading clears at its reset": {
			reading: new(reading(exhausted, 0, at(hour))), now: hour,
		},
		"exhausted reading without reset stays": {
			reading: new(reading(exhausted, 0, nil)), now: 1000 * hour, want: domain.ExhaustedMarkUsageReading,
		},
		"allowed reading alone marks nothing": {
			reading: new(reading(allowed, 0, nil)), now: hour,
		},
		"later reading supplies the mark": {
			refusal: new(refusal(0, at(2*hour))), reading: new(reading(exhausted, hour, at(3*hour))), now: hour,
			want: domain.ExhaustedMarkUsageReading, wantResets: at(3 * hour),
		},
		"later refusal supplies the mark": {
			refusal: new(refusal(hour, at(2*hour))), reading: new(reading(exhausted, 0, at(3*hour))), now: hour,
			want: domain.ExhaustedMarkProviderRefusal, wantResets: at(2 * hour),
		},
		"refusal wins a tie": {
			refusal: new(refusal(0, at(2*hour))), reading: new(reading(exhausted, 0, at(3*hour))), now: hour,
			want: domain.ExhaustedMarkProviderRefusal, wantResets: at(2 * hour),
		},
		"refusal outlives an exhausted reading whose reset passed": {
			refusal: new(refusal(0, nil)), reading: new(reading(exhausted, 0, at(hour))), now: 2 * hour,
			want: domain.ExhaustedMarkProviderRefusal,
		},
		"reading outlives a refusal whose reset passed": {
			refusal: new(refusal(hour, at(2*hour))), reading: new(reading(exhausted, 0, nil)), now: 3 * hour,
			want: domain.ExhaustedMarkUsageReading,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			observation := domain.PoolUsageObservation{
				Pool: "pool", Collector: "enroll-a",
				LastAttempt: &domain.CollectionAttempt{Status: domain.CollectionAttemptSucceeded, At: observationAt},
			}
			if tc.refusal != nil {
				observation.RefusalMarks = []domain.RefusalMark{*tc.refusal}
			}
			if tc.reading != nil {
				observation.Windows = []domain.WindowReading{*tc.reading}
			}
			if err := observation.Validate(); err != nil {
				t.Fatalf("fixture: %v", err)
			}

			marks := observation.ExhaustedMarks(observationAt.Add(tc.now))
			if tc.want == "" {
				if len(marks) != 0 {
					t.Fatalf("marks = %+v, want none", marks)
				}
				return
			}
			if len(marks) != 1 {
				t.Fatalf("marks = %+v, want one from %s", marks, tc.want)
			}
			mark := marks[0]
			if err := mark.Validate(); err != nil {
				t.Fatalf("derived mark invalid: %v", err)
			}
			if mark.Source != tc.want {
				t.Fatalf("source = %s, want %s", mark.Source, tc.want)
			}
			switch {
			case tc.wantResets == nil && mark.ResetsAt != nil,
				tc.wantResets != nil && (mark.ResetsAt == nil || !mark.ResetsAt.Equal(*tc.wantResets)):
				t.Fatalf("resets_at = %v, want %v", mark.ResetsAt, tc.wantResets)
			}
		})
	}
}

func TestPoolUsageObservationModelScope(t *testing.T) {
	t.Parallel()
	observation := domain.PoolUsageObservation{
		Pool: "pool", Collector: "enroll-a",
		LastAttempt: &domain.CollectionAttempt{Status: domain.CollectionAttemptSucceeded, At: observationAt},
		ModelWindows: []domain.ModelWindow{
			{RouteModelID: "model-a", Window: "seven_day_scoped"},
			{RouteModelID: "model-b", Window: "seven_day_scoped"},
			{RouteModelID: "model-c", Window: "unread_scoped"},
		},
		Windows: []domain.WindowReading{
			observationReading("seven_day_scoped", domain.WindowStatusExhausted, nil),
			observationReading("weekly", domain.WindowStatusAllowed, nil),
		},
	}
	general := domain.RefusalMark{Window: "five_hour", InvocationID: "inv-1", SetAt: observationAt}

	for model, want := range map[string][]string{
		"model-a":  {"seven_day_scoped", "weekly"},
		"model-b":  {"seven_day_scoped", "weekly"},
		"model-c":  {"unread_scoped", "weekly"},
		"model-zz": {"weekly"},
	} {
		if got := observation.WindowsForModel(model); !slices.Equal(got, want) {
			t.Errorf("WindowsForModel(%s) = %v, want %v", model, got, want)
		}
	}

	windows := func(marks []domain.ExhaustedMark) []string {
		names := []string{}
		for _, mark := range marks {
			names = append(names, mark.Window)
		}
		return names
	}
	if got := windows(observation.ExhaustedMarksForModel("model-a", observationAt)); !slices.Equal(got, []string{"seven_day_scoped"}) {
		t.Errorf("scoped window for its model: %v", got)
	}
	if got := windows(observation.ExhaustedMarksForModel("model-c", observationAt)); len(got) != 0 {
		t.Errorf("scoped window counted for an unmapped model: %v", got)
	}

	// A general window counts for every model, a refusal's window included.
	observation.RefusalMarks = []domain.RefusalMark{general}
	for model, want := range map[string][]string{
		"model-a": {"five_hour", "seven_day_scoped"},
		"model-c": {"five_hour"},
	} {
		if got := windows(observation.ExhaustedMarksForModel(model, observationAt)); !slices.Equal(got, want) {
			t.Errorf("ExhaustedMarksForModel(%s) = %v, want %v", model, got, want)
		}
	}
}

func TestPoolUsageObservationWithoutClearedMarks(t *testing.T) {
	t.Parallel()
	observation := domain.PoolUsageObservation{
		Pool: "pool", Collector: "enroll-a",
		LastAttempt: &domain.CollectionAttempt{Status: domain.CollectionAttemptSucceeded, At: observationAt},
		Windows: []domain.WindowReading{
			observationReading("cleared", domain.WindowStatusAllowed, nil),
			observationReading("exhausted", domain.WindowStatusExhausted, nil),
			observationReading("tied", domain.WindowStatusAllowed, nil),
		},
		RefusalMarks: []domain.RefusalMark{
			{Window: "cleared", InvocationID: "inv-1", SetAt: observationAt.Add(-time.Second)},
			{Window: "exhausted", InvocationID: "inv-2", SetAt: observationAt.Add(-time.Second)},
			{Window: "tied", InvocationID: "inv-3", SetAt: observationAt},
			{Window: "unread", InvocationID: "inv-4", SetAt: observationAt.Add(-time.Second)},
		},
	}
	kept := observation.WithoutClearedMarks()
	names := []string{}
	for _, mark := range kept.RefusalMarks {
		names = append(names, mark.Window)
	}
	if want := []string{"exhausted", "tied", "unread"}; !slices.Equal(names, want) {
		t.Fatalf("kept %v, want %v", names, want)
	}
	if len(observation.RefusalMarks) != 4 {
		t.Fatalf("receiver's marks changed: %+v", observation.RefusalMarks)
	}

	// The revival this exists to stop: with the mark kept, replacing the allowed
	// reading would make a mark no reset ends active again.
	revived := kept
	revived.Windows = []domain.WindowReading{
		observationReading("cleared", domain.WindowStatusExhausted, new(observationAt.Add(time.Hour))),
	}
	if marks := revived.ExhaustedMarks(observationAt.Add(2 * time.Hour)); len(marks) != 3 {
		t.Fatalf("marks after the reset = %+v, want exhausted, tied, and unread alone", marks)
	}
}

func TestPoolUsageObservationWithRefusalMark(t *testing.T) {
	t.Parallel()
	mark := func(window string, invocation domain.InvocationID, setAt time.Duration) domain.RefusalMark {
		return domain.RefusalMark{Window: window, InvocationID: invocation, SetAt: observationAt.Add(setAt)}
	}
	observation := domain.PoolUsageObservation{Pool: "pool", Collector: "enroll-a"}.
		WithRefusalMark(mark("weekly", "inv-1", 0)).
		WithRefusalMark(mark("five_hour", "inv-2", 0))
	if err := observation.Validate(); err != nil {
		t.Fatalf("marks stay canonical: %v", err)
	}

	replaced := observation.WithRefusalMark(mark("weekly", "inv-3", time.Minute))
	if got := replaced.RefusalMarks[1].InvocationID; got != "inv-3" {
		t.Fatalf("later mark: holder = %s, want inv-3", got)
	}
	delayed := replaced.WithRefusalMark(mark("weekly", "inv-4", -time.Minute))
	if got := delayed.RefusalMarks[1].InvocationID; got != "inv-3" {
		t.Fatalf("delayed mark: holder = %s, want inv-3", got)
	}
	if got := observation.RefusalMarks[1].InvocationID; got != "inv-1" {
		t.Fatalf("receiver's mark changed to %s", got)
	}

	// A refusal reported after an allowed reading that outdates it is already
	// cleared, so recording it changes nothing the predicate can see.
	read := domain.PoolUsageObservation{
		Pool: "pool", Collector: "enroll-a",
		LastAttempt: &domain.CollectionAttempt{Status: domain.CollectionAttemptSucceeded, At: observationAt},
		Windows:     []domain.WindowReading{observationReading("weekly", domain.WindowStatusAllowed, nil)},
	}
	if got := read.WithRefusalMark(mark("weekly", "inv-5", -time.Minute)).RefusalMarks; len(got) != 0 {
		t.Fatalf("outdated mark kept: %+v", got)
	}
}

func TestPoolUsageObservationExhaustedWindowMark(t *testing.T) {
	t.Parallel()
	o := validPoolUsageObservation()
	o.Windows[0].ObservedAt = observationAt.Add(-2 * time.Hour)
	mark, ok := o.ExhaustedWindowMark("five_hour", observationAt)
	if !ok || mark.Source != domain.ExhaustedMarkProviderRefusal || mark.Window != "five_hour" {
		t.Fatalf("ExhaustedWindowMark(five_hour) = %+v, %t, want the refusal mark", mark, ok)
	}
	if _, ok := o.ExhaustedWindowMark("seven_day_a", observationAt); ok {
		t.Fatal("ExhaustedWindowMark(seven_day_a) reports an allowed window exhausted")
	}
	// The allowed reading observed after the refusal clears it.
	o.Windows[0].ObservedAt = observationAt
	if _, ok := o.ExhaustedWindowMark("five_hour", observationAt); ok {
		t.Fatal("ExhaustedWindowMark(five_hour) survives a later allowed reading")
	}
}

func TestPoolUsageObservationReadingReset(t *testing.T) {
	t.Parallel()
	resetsAt := observationAt.Add(time.Hour)
	o := validPoolUsageObservation()
	o.Windows[0].ResetsAt = &resetsAt
	if got := o.ReadingReset("five_hour", observationAt); got == nil || !got.Equal(resetsAt) {
		t.Fatalf("ReadingReset ahead of now = %v, want %s", got, resetsAt)
	}
	if got := o.ReadingReset("five_hour", resetsAt); got != nil {
		t.Fatalf("ReadingReset at the reset = %s, want none", got)
	}
	if got := o.ReadingReset("seven_day_a", observationAt); got != nil {
		t.Fatalf("ReadingReset for a reading with no reset = %s, want none", got)
	}
	if got := o.ReadingReset("seven_day_b", observationAt); got != nil {
		t.Fatalf("ReadingReset for an unread window = %s, want none", got)
	}
}
