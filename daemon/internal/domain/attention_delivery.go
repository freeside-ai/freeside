package domain

import (
	"fmt"
	"time"
)

// AttentionDelivery records one attempt to deliver an item to one device over
// one channel (plan §4). The three timestamps are the honest lifecycle:
// submitted (handed to the channel), channel-accepted (the provider took it,
// never "delivered"), and opened (a real device-level receipt). The product
// metric, open-to-decision time, is measured against these plus the item's
// resolution, not stored on the delivery.
type AttentionDelivery struct {
	ItemID            ItemID         `json:"item_id"`
	DeviceID          DeviceID       `json:"device_id"`
	Channel           string         `json:"channel"`
	Attempt           int            `json:"attempt"`
	SubmittedAt       time.Time      `json:"submitted_at"`
	ChannelAcceptedAt *time.Time     `json:"channel_accepted_at"`
	OpenedAt          *time.Time     `json:"opened_at"`
	Status            DeliveryStatus `json:"delivery_status"`
}

// Validate reports whether the delivery is structurally sound.
func (d AttentionDelivery) Validate() error {
	if d.ItemID == "" {
		return fmt.Errorf("delivery item_id: %w", ErrEmptyID)
	}
	if d.DeviceID == "" {
		return fmt.Errorf("delivery device_id: %w", ErrEmptyID)
	}
	// A delivery is explicitly per channel and per attempt (plan §4); those are
	// the keys retry and provider attribution join on, so neither may be blank.
	if d.Channel == "" {
		return fmt.Errorf("delivery channel: %w", ErrEmptyField)
	}
	if d.Attempt < 1 {
		return fmt.Errorf("delivery attempt %d: %w", d.Attempt, ErrNonPositive)
	}
	if !d.Status.valid() {
		return fmt.Errorf("delivery status %q: %w", d.Status, ErrInvalidDeliveryStatus)
	}
	if d.SubmittedAt.IsZero() {
		return fmt.Errorf("delivery submitted_at: %w", ErrEmptyField)
	}
	// The present receipt timestamps must correspond exactly to the status: a
	// status may neither outrun its receipts (opened needs opened_at) nor be
	// outrun by them (a submitted row carrying an opened_at would let timing
	// aggregates report an open the status denies). Both directions keep the
	// status honest (plan §4, decision 11). Behaviour dispatch on the enum, so
	// no default: a new status must decide its own receipts here.
	switch d.Status {
	case DeliverySubmitted:
		if d.ChannelAcceptedAt != nil || d.OpenedAt != nil {
			return fmt.Errorf("delivery status %q with a later receipt: %w", d.Status, ErrStatusTimestampTooStrong)
		}
	case DeliveryChannelAccepted:
		if d.ChannelAcceptedAt == nil {
			return fmt.Errorf("delivery status %q without channel_accepted_at: %w", d.Status, ErrStatusMissingTimestamp)
		}
		if d.OpenedAt != nil {
			return fmt.Errorf("delivery status %q with an opened_at: %w", d.Status, ErrStatusTimestampTooStrong)
		}
	case DeliveryOpened:
		if d.OpenedAt == nil {
			return fmt.Errorf("delivery status %q without opened_at: %w", d.Status, ErrStatusMissingTimestamp)
		}
	}
	// Present receipts must be monotonically ordered along the lifecycle
	// (submitted -> channel-accepted -> opened), so timing aggregates never
	// report a receipt before the submission that caused it (a negative
	// open-to-submit duration is corrupt telemetry, not honest data).
	if d.ChannelAcceptedAt != nil && d.ChannelAcceptedAt.Before(d.SubmittedAt) {
		return fmt.Errorf("delivery channel_accepted_at before submitted_at: %w", ErrTimestampOutOfOrder)
	}
	if d.OpenedAt != nil && d.OpenedAt.Before(d.SubmittedAt) {
		return fmt.Errorf("delivery opened_at before submitted_at: %w", ErrTimestampOutOfOrder)
	}
	if d.ChannelAcceptedAt != nil && d.OpenedAt != nil && d.OpenedAt.Before(*d.ChannelAcceptedAt) {
		return fmt.Errorf("delivery opened_at before channel_accepted_at: %w", ErrTimestampOutOfOrder)
	}
	return nil
}

// TimingSummary is the set of timing aggregates an AttentionItem derives from
// its delivery set (plan §4). Every field is computed by TimingAggregates; the
// item never sets them directly. Absent stages (never accepted, never opened)
// render as null.
type TimingSummary struct {
	DeliveryCount     int            `json:"delivery_count"`
	FirstSubmittedAt  *time.Time     `json:"first_submitted_at"`
	FirstAcceptedAt   *time.Time     `json:"first_accepted_at"`
	FirstOpenedAt     *time.Time     `json:"first_opened_at"`
	SubmitToFirstOpen *time.Duration `json:"submit_to_first_open"`
}

// Validate reports whether the summary is a shape TimingAggregates could have
// produced: a non-negative count, first instants ordered along the lifecycle,
// and a submit-to-open gap present exactly when both endpoints are and equal to
// their difference. It is the backstop for a summary reconstructed from the
// store, which never passed through TimingAggregates.
func (s TimingSummary) Validate() error {
	if s.DeliveryCount < 0 {
		return fmt.Errorf("timing delivery_count %d: %w", s.DeliveryCount, ErrInconsistentTiming)
	}
	// A present endpoint pointer must carry a real instant: TimingAggregates
	// never records a zero time (earlier treats it as absent), so a zero behind
	// a non-nil pointer is corrupt reconstructed data, not "present".
	for _, e := range []struct {
		name string
		at   *time.Time
	}{{"first_submitted_at", s.FirstSubmittedAt}, {"first_accepted_at", s.FirstAcceptedAt}, {"first_opened_at", s.FirstOpenedAt}} {
		if e.at != nil && e.at.IsZero() {
			return fmt.Errorf("timing %s is zero: %w", e.name, ErrInconsistentTiming)
		}
	}
	// Count and endpoints must agree: no deliveries means no timing, and any
	// receipt aggregate implies the submission endpoint (every delivery is
	// submitted before it can be accepted or opened).
	hasReceipt := s.FirstAcceptedAt != nil || s.FirstOpenedAt != nil
	if s.DeliveryCount == 0 && (s.FirstSubmittedAt != nil || hasReceipt) {
		return fmt.Errorf("timing has no deliveries but carries a receipt: %w", ErrInconsistentTiming)
	}
	if s.DeliveryCount > 0 && s.FirstSubmittedAt == nil {
		return fmt.Errorf("timing has deliveries but no first_submitted_at: %w", ErrInconsistentTiming)
	}
	if hasReceipt && s.FirstSubmittedAt == nil {
		return fmt.Errorf("timing carries a receipt with no submission: %w", ErrInconsistentTiming)
	}
	// first_submitted is the earliest possible instant, so each receipt
	// aggregate falls on or after it. first_accepted and first_opened are
	// independent minima over *different* deliveries, though, so they carry no
	// order relative to each other (an opened delivery need not be one that was
	// channel-accepted).
	if s.FirstAcceptedAt != nil && s.FirstAcceptedAt.Before(*s.FirstSubmittedAt) {
		return fmt.Errorf("timing first_accepted_at before first_submitted_at: %w", ErrInconsistentTiming)
	}
	if s.FirstOpenedAt != nil && s.FirstOpenedAt.Before(*s.FirstSubmittedAt) {
		return fmt.Errorf("timing first_opened_at before first_submitted_at: %w", ErrInconsistentTiming)
	}
	bothEndpoints := s.FirstSubmittedAt != nil && s.FirstOpenedAt != nil
	switch {
	case s.SubmitToFirstOpen == nil && bothEndpoints:
		return fmt.Errorf("timing submit_to_first_open missing: %w", ErrInconsistentTiming)
	case s.SubmitToFirstOpen != nil && !bothEndpoints:
		return fmt.Errorf("timing submit_to_first_open without both endpoints: %w", ErrInconsistentTiming)
	case s.SubmitToFirstOpen != nil && *s.SubmitToFirstOpen != s.FirstOpenedAt.Sub(*s.FirstSubmittedAt):
		return fmt.Errorf("timing submit_to_first_open mismatched: %w", ErrInconsistentTiming)
	}
	return nil
}

// deliveryKey identifies one delivery attempt within an item: a delivery is
// per device, channel, and attempt (plan §4), so this is what a duplicate
// shares. Item id is not part of the key because callers require it fixed.
type deliveryKey struct {
	device  DeviceID
	channel string
	attempt int
}

// TimingAggregates derives an item's timing from its delivery set. It reduces
// the earliest submitted, channel-accepted, and opened instants across all
// deliveries, so the result is independent of slice order. The aggregates are
// only ever produced here: an item's TimingSummary has no other writer, which
// is what makes "never directly settable" (plan §4) a structural fact.
func TimingAggregates(deliveries []AttentionDelivery) TimingSummary {
	sum := TimingSummary{DeliveryCount: len(deliveries)}
	for _, d := range deliveries {
		sum.FirstSubmittedAt = earlier(sum.FirstSubmittedAt, &d.SubmittedAt)
		sum.FirstAcceptedAt = earlier(sum.FirstAcceptedAt, d.ChannelAcceptedAt)
		sum.FirstOpenedAt = earlier(sum.FirstOpenedAt, d.OpenedAt)
	}
	if sum.FirstSubmittedAt != nil && sum.FirstOpenedAt != nil {
		gap := sum.FirstOpenedAt.Sub(*sum.FirstSubmittedAt)
		sum.SubmitToFirstOpen = &gap
	}
	return sum
}

// earlier returns the earlier of two optional instants, treating nil and the
// zero time as absent. It never aliases its second argument, so a *time.Time
// pointing into a loop variable cannot leak into the result.
func earlier(cur, candidate *time.Time) *time.Time {
	if candidate == nil || candidate.IsZero() {
		return cur
	}
	if cur == nil || candidate.Before(*cur) {
		c := *candidate
		return &c
	}
	return cur
}
