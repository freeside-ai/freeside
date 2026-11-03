package domain

import (
	"cmp"
	"fmt"
	"slices"
	"time"
)

// Pool usage observations record what a provider reports about a subscription
// allowance (plan §5.4, usage observation). This is the per-pool record;
// UsageObservation is the unrelated per-invocation telemetry. The record is
// observation, never authority: admission does not read it. Plan §5.4 names
// two readers above admission: the router, which picks a new task's pool, and
// the quota park, which only times a parked attempt on its line.

// CollectionAttemptStatus is how the collector's last collection attempt
// ended. The zero value is invalid by design.
type CollectionAttemptStatus string

const (
	CollectionAttemptSucceeded   CollectionAttemptStatus = "succeeded"
	CollectionAttemptFailed      CollectionAttemptStatus = "failed"
	CollectionAttemptUnsupported CollectionAttemptStatus = "unsupported"
)

// AllCollectionAttemptStatuses is the single registration point for attempt
// statuses.
var AllCollectionAttemptStatuses = []CollectionAttemptStatus{
	CollectionAttemptSucceeded,
	CollectionAttemptFailed,
	CollectionAttemptUnsupported,
}

func (s CollectionAttemptStatus) valid() bool {
	switch s {
	case CollectionAttemptSucceeded, CollectionAttemptFailed, CollectionAttemptUnsupported:
		return true
	default:
		return false
	}
}

// AvailabilityAnswer is the provider's answer under one enrollment's own
// credential. The zero value is invalid by design.
type AvailabilityAnswer string

const (
	AvailabilitySupported   AvailabilityAnswer = "supported"
	AvailabilityUnsupported AvailabilityAnswer = "unsupported"
	AvailabilityFailed      AvailabilityAnswer = "failed"
)

// AllAvailabilityAnswers is the single registration point for availability
// answers.
var AllAvailabilityAnswers = []AvailabilityAnswer{
	AvailabilitySupported,
	AvailabilityUnsupported,
	AvailabilityFailed,
}

func (a AvailabilityAnswer) valid() bool {
	switch a {
	case AvailabilitySupported, AvailabilityUnsupported, AvailabilityFailed:
		return true
	default:
		return false
	}
}

// WindowStatus is what a reading says about a provider window. The zero value
// is invalid by design.
type WindowStatus string

const (
	WindowStatusAllowed   WindowStatus = "allowed"
	WindowStatusExhausted WindowStatus = "exhausted"
)

// AllWindowStatuses is the single registration point for window statuses.
var AllWindowStatuses = []WindowStatus{
	WindowStatusAllowed,
	WindowStatusExhausted,
}

func (s WindowStatus) valid() bool {
	switch s {
	case WindowStatusAllowed, WindowStatusExhausted:
		return true
	default:
		return false
	}
}

// ExhaustedMarkSource names what made a window exhausted. There is no
// fixture-only source: a mark stays honest about what set it. The zero value
// is invalid by design.
type ExhaustedMarkSource string

const (
	ExhaustedMarkProviderRefusal ExhaustedMarkSource = "provider_refusal"
	ExhaustedMarkUsageReading    ExhaustedMarkSource = "usage_reading"
)

// AllExhaustedMarkSources is the single registration point for mark sources.
var AllExhaustedMarkSources = []ExhaustedMarkSource{
	ExhaustedMarkProviderRefusal,
	ExhaustedMarkUsageReading,
}

func (s ExhaustedMarkSource) valid() bool {
	switch s {
	case ExhaustedMarkProviderRefusal, ExhaustedMarkUsageReading:
		return true
	default:
		return false
	}
}

// PoolUsageObservation is the one usage record of a usage pool. The provider
// meters the pool, so the record attaches there and not to an identity.
//
// Exhausted state is derived by ExhaustedMarks, never stored as a flag.
// Credential expiry, usage reset, billing renewal, reset credit, and run
// consumption stay distinct: only the usage reset lives here, as a window's
// ResetsAt. Billing renewal and reset credit are reserved until a supported
// credential exposes them.
type PoolUsageObservation struct {
	Pool UsagePoolID `json:"pool"`
	// Plan is the provider's plan name where known.
	Plan *string `json:"plan"`
	// Collector is the one enrollment that collects for the pool. Collection
	// never fails over to another enrollment on its own.
	Collector ClientEnrollmentID `json:"collector"`
	// LastAttempt is the pool's refresh status; nil until the collector's first
	// attempt.
	LastAttempt *CollectionAttempt `json:"last_attempt"`
	// Enrollments holds each enrollment's availability answer, sorted by
	// enrollment id: what tells the operator which one could be the collector.
	Enrollments []EnrollmentAvailability `json:"enrollments"`
	// ModelWindows maps route model ids to the windows that limit them, sorted
	// by route model id then window. A window named in no entry is general and
	// applies to every model.
	ModelWindows []ModelWindow `json:"model_windows"`
	// Windows holds one reading per provider window, sorted by window.
	Windows []WindowReading `json:"windows"`
	// RefusalMarks holds at most one mark per window, sorted by window.
	RefusalMarks []RefusalMark `json:"refusal_marks"`
}

// CollectionAttempt is how and when the collector last tried to collect.
type CollectionAttempt struct {
	Status CollectionAttemptStatus `json:"status"`
	At     time.Time               `json:"at"`
	Detail string                  `json:"detail"`
}

// EnrollmentAvailability is one enrollment's availability answer.
type EnrollmentAvailability struct {
	EnrollmentID ClientEnrollmentID `json:"enrollment_id"`
	Answer       AvailabilityAnswer `json:"answer"`
	ObservedAt   time.Time          `json:"observed_at"`
}

// ModelWindow says that a window limits a route model id.
type ModelWindow struct {
	RouteModelID string `json:"route_model_id"`
	Window       string `json:"window"`
}

// WindowReading is one provider window as the collector last saw it. Unknown
// stays unknown: a reading with a status alone is complete, and a reset or
// length the provider didn't give stays null.
type WindowReading struct {
	Window       string             `json:"window"`
	EnrollmentID ClientEnrollmentID `json:"enrollment_id"`
	// Generation is the collector's enrollment generation the reading was
	// taken under. A reading keeps naming it after a later generation.
	Generation int          `json:"generation"`
	Status     WindowStatus `json:"status"`
	// UsedPercent and Quantity are the two forms a provider's report takes; a
	// reading carries at most one. Unit is set exactly when Quantity is.
	UsedPercent *int           `json:"used_percent"`
	Quantity    *int64         `json:"quantity"`
	Unit        *string        `json:"unit"`
	ResetsAt    *time.Time     `json:"resets_at"`
	Length      *time.Duration `json:"length"`
	ObservedAt  time.Time      `json:"observed_at"`
}

// RefusalMark is a provider quota refusal that marked a window exhausted.
type RefusalMark struct {
	Window       string       `json:"window"`
	InvocationID InvocationID `json:"invocation_id"`
	// ResetsAt is the reset the refusal gave, if any. A mark without one stays
	// until an allowed reading observed after it.
	ResetsAt *time.Time `json:"resets_at"`
	SetAt    time.Time  `json:"set_at"`
}

// QuotaRefusal is what a driver reports beside a quota failure: the provider
// window that refused the run and, when the provider said, when it resets.
type QuotaRefusal struct {
	Window   string     `json:"window"`
	ResetsAt *time.Time `json:"resets_at"`
}

// Validate reports whether the refusal names a window and a UTC reset.
func (r QuotaRefusal) Validate() error {
	if !validUsageToken(r.Window) {
		return fmt.Errorf("quota refusal window %q: %w", r.Window, ErrEmptyField)
	}
	return validateObservationReset("quota refusal "+r.Window+" resets_at", r.ResetsAt)
}

// ExhaustedMark is a derived fact: one reason the record holds a window
// exhausted, and what says so. It is computed from the record and never
// stored.
type ExhaustedMark struct {
	Window string              `json:"window"`
	Source ExhaustedMarkSource `json:"source"`
	// InvocationID is set exactly when Source is provider_refusal.
	InvocationID *InvocationID `json:"invocation_id"`
	// ReadingObservedAt is set exactly when Source is usage_reading.
	ReadingObservedAt *time.Time `json:"reading_observed_at"`
	ResetsAt          *time.Time `json:"resets_at"`
	SetAt             time.Time  `json:"set_at"`
}

func validateObservationTime(field string, at time.Time) error {
	if at.IsZero() {
		return fmt.Errorf("%s: %w", field, ErrMissingTimestamp)
	}
	if at.Location() != time.UTC {
		return fmt.Errorf("%s: %w", field, ErrTimestampNotUTC)
	}
	return nil
}

func validateObservationReset(field string, at *time.Time) error {
	if at == nil {
		return nil
	}
	return validateObservationTime(field, *at)
}

// Validate reports whether the attempt is well-formed.
func (a CollectionAttempt) Validate() error {
	if !a.Status.valid() {
		return fmt.Errorf("collection attempt status %q: %w", a.Status, ErrInvalidCollectionAttemptStatus)
	}
	return validateObservationTime("collection attempt at", a.At)
}

// Validate reports whether the availability answer is well-formed.
func (a EnrollmentAvailability) Validate() error {
	if a.EnrollmentID == "" {
		return fmt.Errorf("enrollment availability enrollment_id: %w", ErrEmptyID)
	}
	if !a.Answer.valid() {
		return fmt.Errorf("enrollment availability answer %q: %w", a.Answer, ErrInvalidAvailabilityAnswer)
	}
	return validateObservationTime("enrollment availability observed_at", a.ObservedAt)
}

// Validate reports whether the map entry is well-formed.
func (m ModelWindow) Validate() error {
	if m.RouteModelID == "" {
		return fmt.Errorf("model window route_model_id: %w", ErrEmptyField)
	}
	if !validUsageToken(m.Window) {
		return fmt.Errorf("model window window %q: %w", m.Window, ErrEmptyField)
	}
	return nil
}

// Validate reports whether the reading is well-formed. The record checks that
// it comes from the collector.
func (r WindowReading) Validate() error {
	if !validUsageToken(r.Window) {
		return fmt.Errorf("window reading window %q: %w", r.Window, ErrEmptyField)
	}
	if r.EnrollmentID == "" {
		return fmt.Errorf("window reading %s enrollment_id: %w", r.Window, ErrEmptyID)
	}
	if r.Generation < 1 {
		return fmt.Errorf("window reading %s generation %d: %w", r.Window, r.Generation, ErrNonPositive)
	}
	if !r.Status.valid() {
		return fmt.Errorf("window reading %s status %q: %w", r.Window, r.Status, ErrInvalidWindowStatus)
	}
	if r.UsedPercent != nil && r.Quantity != nil {
		return fmt.Errorf("window reading %s used_percent and quantity: %w", r.Window, ErrWindowReadingMeasureConflict)
	}
	if (r.Quantity != nil) != (r.Unit != nil) {
		return fmt.Errorf("window reading %s quantity and unit: %w", r.Window, ErrWindowReadingMeasureConflict)
	}
	if r.UsedPercent != nil && *r.UsedPercent < 0 {
		return fmt.Errorf("window reading %s used_percent %d: %w", r.Window, *r.UsedPercent, ErrNonPositive)
	}
	if r.Quantity != nil && *r.Quantity < 0 {
		return fmt.Errorf("window reading %s quantity %d: %w", r.Window, *r.Quantity, ErrNonPositive)
	}
	if r.Unit != nil && !validUsageToken(*r.Unit) {
		return fmt.Errorf("window reading %s unit %q: %w", r.Window, *r.Unit, ErrEmptyField)
	}
	if r.Length != nil && *r.Length <= 0 {
		return fmt.Errorf("window reading %s length %s: %w", r.Window, *r.Length, ErrNonPositive)
	}
	if err := validateObservationReset("window reading "+r.Window+" resets_at", r.ResetsAt); err != nil {
		return err
	}
	return validateObservationTime("window reading "+r.Window+" observed_at", r.ObservedAt)
}

// Validate reports whether the mark is well-formed.
func (m RefusalMark) Validate() error {
	if !validUsageToken(m.Window) {
		return fmt.Errorf("refusal mark window %q: %w", m.Window, ErrEmptyField)
	}
	if m.InvocationID == "" {
		return fmt.Errorf("refusal mark %s invocation_id: %w", m.Window, ErrEmptyID)
	}
	if err := validateObservationReset("refusal mark "+m.Window+" resets_at", m.ResetsAt); err != nil {
		return err
	}
	return validateObservationTime("refusal mark "+m.Window+" set_at", m.SetAt)
}

// Validate reports whether the derived mark is a shape ExhaustedMarks could
// have produced.
func (m ExhaustedMark) Validate() error {
	if !validUsageToken(m.Window) {
		return fmt.Errorf("exhausted mark window %q: %w", m.Window, ErrEmptyField)
	}
	if !m.Source.valid() {
		return fmt.Errorf("exhausted mark source %q: %w", m.Source, ErrInvalidExhaustedMarkSource)
	}
	var consistent bool
	switch m.Source {
	case ExhaustedMarkProviderRefusal:
		consistent = m.InvocationID != nil && *m.InvocationID != "" && m.ReadingObservedAt == nil
	case ExhaustedMarkUsageReading:
		consistent = m.InvocationID == nil && m.ReadingObservedAt != nil
	}
	if !consistent {
		return fmt.Errorf("exhausted mark %s source %s: %w", m.Window, m.Source, ErrExhaustedMarkInconsistent)
	}
	if err := validateObservationReset("exhausted mark "+m.Window+" reading_observed_at", m.ReadingObservedAt); err != nil {
		return err
	}
	if err := validateObservationReset("exhausted mark "+m.Window+" resets_at", m.ResetsAt); err != nil {
		return err
	}
	return validateObservationTime("exhausted mark "+m.Window+" set_at", m.SetAt)
}

// Validate reports whether the record is well-formed and canonical. It cannot
// see whether an enrollment is on the pool, or whether a failed attempt's
// reading was carried from an earlier success; the store checks both.
func (o PoolUsageObservation) Validate() error {
	if o.Pool == "" {
		return fmt.Errorf("pool usage observation pool: %w", ErrEmptyID)
	}
	if o.Collector == "" {
		return fmt.Errorf("pool usage observation %s collector: %w", o.Pool, ErrEmptyID)
	}
	if o.Plan != nil && *o.Plan == "" {
		return fmt.Errorf("pool usage observation %s plan: %w", o.Pool, ErrEmptyField)
	}
	if o.LastAttempt != nil {
		if err := o.LastAttempt.Validate(); err != nil {
			return fmt.Errorf("pool usage observation %s: %w", o.Pool, err)
		}
	}
	if err := validateCanonical(o.Enrollments, func(left, right EnrollmentAvailability) int {
		return cmp.Compare(left.EnrollmentID, right.EnrollmentID)
	}); err != nil {
		return fmt.Errorf("pool usage observation %s enrollments: %w", o.Pool, err)
	}
	if err := validateCanonical(o.ModelWindows, func(left, right ModelWindow) int {
		return cmp.Or(cmp.Compare(left.RouteModelID, right.RouteModelID), cmp.Compare(left.Window, right.Window))
	}); err != nil {
		return fmt.Errorf("pool usage observation %s model_windows: %w", o.Pool, err)
	}
	if err := validateCanonical(o.Windows, func(left, right WindowReading) int {
		return cmp.Compare(left.Window, right.Window)
	}); err != nil {
		return fmt.Errorf("pool usage observation %s windows: %w", o.Pool, err)
	}
	if err := validateCanonical(o.RefusalMarks, func(left, right RefusalMark) int {
		return cmp.Compare(left.Window, right.Window)
	}); err != nil {
		return fmt.Errorf("pool usage observation %s refusal_marks: %w", o.Pool, err)
	}
	for _, reading := range o.Windows {
		if reading.EnrollmentID != o.Collector {
			return fmt.Errorf("pool usage observation %s window %s from %s: %w",
				o.Pool, reading.Window, reading.EnrollmentID, ErrReadingNotFromCollector)
		}
	}
	// A collector that never answered, or answered unsupported, holds statuses
	// and no window: a window here would be one the provider never reported.
	if len(o.Windows) > 0 && (o.LastAttempt == nil || o.LastAttempt.Status == CollectionAttemptUnsupported) {
		return fmt.Errorf("pool usage observation %s: %w", o.Pool, ErrSyntheticWindow)
	}
	return nil
}

// validateCanonical validates each entry and requires a strictly ascending
// order, which also refuses a duplicate.
func validateCanonical[T interface{ Validate() error }](entries []T, compare func(left, right T) int) error {
	for i, entry := range entries {
		if err := entry.Validate(); err != nil {
			return err
		}
		if i > 0 && compare(entries[i-1], entry) >= 0 {
			return ErrKeysNotCanonical
		}
	}
	return nil
}

// WindowsForModel returns the windows that apply to a route model id, sorted:
// those mapped to it, and every general window the record names.
func (o PoolUsageObservation) WindowsForModel(routeModelID string) []string {
	windows := make([]string, 0, len(o.ModelWindows)+len(o.Windows)+len(o.RefusalMarks))
	for _, entry := range o.ModelWindows {
		windows = append(windows, entry.Window)
	}
	windows = append(windows, o.observedWindows()...)
	slices.Sort(windows)
	return slices.DeleteFunc(slices.Compact(windows), func(window string) bool {
		return !o.windowApplies(window, routeModelID)
	})
}

// windowApplies reports whether a window limits a model: it is mapped to that
// model, or it is mapped to none.
func (o PoolUsageObservation) windowApplies(window, routeModelID string) bool {
	mapped := false
	for _, entry := range o.ModelWindows {
		if entry.Window != window {
			continue
		}
		if entry.RouteModelID == routeModelID {
			return true
		}
		mapped = true
	}
	return !mapped
}

// observedWindows returns every window a reading or a refusal mark names,
// sorted and unique.
func (o PoolUsageObservation) observedWindows() []string {
	windows := make([]string, 0, len(o.Windows)+len(o.RefusalMarks))
	for _, reading := range o.Windows {
		windows = append(windows, reading.Window)
	}
	for _, mark := range o.RefusalMarks {
		windows = append(windows, mark.Window)
	}
	slices.Sort(windows)
	return slices.Compact(windows)
}

// RecordedExhaustedMarks returns every exhausted mark the record holds, with
// no clock: a window's refusal mark unless an allowed reading observed after
// it cleared it, and its reading when the collector reported it exhausted.
// One window can carry both. The marks come in window order, then in
// AllExhaustedMarkSources order.
//
// The answer changes only when the record is written, so two reads of one
// record agree whenever they run. A mark lapses once ResetsAt is reached, and a
// mark with a nil ResetsAt holds until a write replaces or clears it; a mark
// that has lapsed is still returned, and the reader judges lapsing by its own
// clock. A window is exhausted while any of its marks hasn't lapsed.
func (o PoolUsageObservation) RecordedExhaustedMarks() []ExhaustedMark {
	var marks []ExhaustedMark
	for _, window := range o.observedWindows() {
		if refusal := o.refusal(window); refusal != nil && !o.refusalCleared(*refusal) {
			marks = append(marks, ExhaustedMark{
				Window: window, Source: ExhaustedMarkProviderRefusal,
				InvocationID: new(refusal.InvocationID),
				ResetsAt:     refusal.ResetsAt, SetAt: refusal.SetAt,
			})
		}
		if reading := o.reading(window); reading != nil && reading.Status == WindowStatusExhausted {
			marks = append(marks, ExhaustedMark{
				Window: window, Source: ExhaustedMarkUsageReading,
				ReadingObservedAt: new(reading.ObservedAt),
				ResetsAt:          reading.ResetsAt, SetAt: reading.ObservedAt,
			})
		}
	}
	return marks
}

// ExhaustedMarks derives the windows exhausted at now, one mark per window in
// window order, from RecordedExhaustedMarks.
//
// A refusal mark is active from SetAt until its reset, or until an allowed
// reading observed after SetAt clears it. A reading the collector reported as
// exhausted is active until its reset. A reset, once reached, clears on its
// own, and an exhausted reading with no reset stays until the next reading
// replaces it. When a refusal and a reading are both active on one window, the
// later of the two supplies the mark, and the refusal wins a tie.
func (o PoolUsageObservation) ExhaustedMarks(now time.Time) []ExhaustedMark {
	var marks []ExhaustedMark
	for _, mark := range o.RecordedExhaustedMarks() {
		if resetReached(mark.ResetsAt, now) ||
			(mark.Source == ExhaustedMarkProviderRefusal && mark.SetAt.After(now)) {
			continue
		}
		// A window's refusal comes before its reading, so a reading replaces the
		// refusal only when strictly later, and the refusal keeps a tie.
		if last := len(marks) - 1; last >= 0 && marks[last].Window == mark.Window {
			if mark.SetAt.After(marks[last].SetAt) {
				marks[last] = mark
			}
			continue
		}
		marks = append(marks, mark)
	}
	return marks
}

// ExhaustedMarksForModel is ExhaustedMarks restricted to the windows that
// apply to a route model id. An empty result means the pool is not known to be
// exhausted for that model at now; it does not mean allowance remains.
func (o PoolUsageObservation) ExhaustedMarksForModel(routeModelID string, now time.Time) []ExhaustedMark {
	return slices.DeleteFunc(o.ExhaustedMarks(now), func(mark ExhaustedMark) bool {
		return !o.windowApplies(mark.Window, routeModelID)
	})
}

// ExhaustedWindowMark returns the mark ExhaustedMarks derives for one window
// at now, if the window is exhausted then.
func (o PoolUsageObservation) ExhaustedWindowMark(window string, now time.Time) (ExhaustedMark, bool) {
	for _, mark := range o.ExhaustedMarks(now) {
		if mark.Window == window {
			return mark, true
		}
	}
	return ExhaustedMark{}, false
}

// ReadingReset returns the reset the window's current reading gives, when one
// is still ahead of now. A refusal that names no reset takes it from here.
func (o PoolUsageObservation) ReadingReset(window string, now time.Time) *time.Time {
	reading := o.reading(window)
	if reading == nil || reading.ResetsAt == nil || !reading.ResetsAt.After(now) {
		return nil
	}
	return new(*reading.ResetsAt)
}

// WithoutClearedMarks returns the record without the refusal marks an allowed
// reading observed after them has cleared. Clearing is derived from the
// window's current reading, and a later reading replaces that one, so a store
// write drops a cleared mark: an exhausted reading must not revive it.
func (o PoolUsageObservation) WithoutClearedMarks() PoolUsageObservation {
	o.RefusalMarks = slices.DeleteFunc(slices.Clone(o.RefusalMarks), o.refusalCleared)
	return o
}

// WithRefusalMark returns the record with mark in place of any earlier mark on
// its window. A mark older than the one held changes nothing: a delayed report
// of a refusal must not replace what a later refusal said about the reset.
func (o PoolUsageObservation) WithRefusalMark(mark RefusalMark) PoolUsageObservation {
	if held := o.refusal(mark.Window); held != nil && held.SetAt.After(mark.SetAt) {
		return o
	}
	marks := slices.DeleteFunc(slices.Clone(o.RefusalMarks), func(held RefusalMark) bool {
		return held.Window == mark.Window
	})
	marks = append(marks, mark)
	slices.SortFunc(marks, func(left, right RefusalMark) int {
		return cmp.Compare(left.Window, right.Window)
	})
	o.RefusalMarks = marks
	return o.WithoutClearedMarks()
}

func (o PoolUsageObservation) refusalCleared(mark RefusalMark) bool {
	reading := o.reading(mark.Window)
	return reading != nil && reading.Status == WindowStatusAllowed && reading.ObservedAt.After(mark.SetAt)
}

func (o PoolUsageObservation) reading(window string) *WindowReading {
	for i := range o.Windows {
		if o.Windows[i].Window == window {
			return &o.Windows[i]
		}
	}
	return nil
}

func (o PoolUsageObservation) refusal(window string) *RefusalMark {
	for i := range o.RefusalMarks {
		if o.RefusalMarks[i].Window == window {
			return &o.RefusalMarks[i]
		}
	}
	return nil
}

func resetReached(resetsAt *time.Time, now time.Time) bool {
	return resetsAt != nil && !resetsAt.After(now)
}
