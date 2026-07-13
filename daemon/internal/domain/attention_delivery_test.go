package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func ptr[T any](v T) *T { return &v }

// TestTimingAggregates is acceptance criterion 2: item timing aggregates are
// derived from a delivery set (never settable directly), and a fixture of N
// deliveries produces the expected aggregates independent of slice order.
func TestTimingAggregates(t *testing.T) {
	base := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	accepted := base.Add(1 * time.Minute)
	opened := base.Add(5 * time.Minute)

	deliveries := []domain.AttentionDelivery{
		{
			ItemID: "i", DeviceID: "d1", Channel: "ntfy", Attempt: 1,
			SubmittedAt: base.Add(10 * time.Minute), Status: domain.DeliverySubmitted,
		},
		{
			ItemID: "i", DeviceID: "d2", Channel: "ntfy", Attempt: 1,
			SubmittedAt: base, ChannelAcceptedAt: ptr(accepted), OpenedAt: ptr(opened),
			Status: domain.DeliveryOpened,
		},
		{
			ItemID: "i", DeviceID: "d3", Channel: "ntfy", Attempt: 2,
			SubmittedAt: base.Add(2 * time.Minute), ChannelAcceptedAt: ptr(accepted.Add(time.Minute)),
			Status: domain.DeliveryChannelAccepted,
		},
	}

	gap := opened.Sub(base)
	want := domain.TimingSummary{
		DeliveryCount:     3,
		FirstSubmittedAt:  ptr(base),
		FirstAcceptedAt:   ptr(accepted),
		FirstOpenedAt:     ptr(opened),
		SubmitToFirstOpen: ptr(gap),
	}

	// Order independence: forward and reversed inputs must agree.
	got := domain.TimingAggregates(deliveries)
	assertTiming(t, "forward", got, want)

	reversed := append([]domain.AttentionDelivery(nil), deliveries...)
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	assertTiming(t, "reversed", domain.TimingAggregates(reversed), want)
}

func TestTimingAggregatesEmpty(t *testing.T) {
	got := domain.TimingAggregates(nil)
	if got.DeliveryCount != 0 || got.FirstSubmittedAt != nil || got.FirstOpenedAt != nil || got.SubmitToFirstOpen != nil {
		t.Errorf("empty delivery set produced non-zero aggregates: %+v", got)
	}
}

// TestWithTimingIsOnlyWriter checks that a freshly constructed item carries no
// timing (it is never settable through the constructor input) and that
// WithTiming is what fills it.
func TestWithTimingIsOnlyWriter(t *testing.T) {
	item, err := domain.NewAttentionItem(validItemInput(domain.AttentionSpecApproval), nil)
	if err != nil {
		t.Fatal(err)
	}
	if item.Timing.DeliveryCount != 0 || item.Timing.FirstSubmittedAt != nil {
		t.Fatalf("constructor set timing: %+v", item.Timing)
	}
	deliveries := []domain.AttentionDelivery{{
		ItemID: item.ID, DeviceID: "d", Channel: "ntfy", Attempt: 1,
		SubmittedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Status: domain.DeliverySubmitted,
	}}
	withTiming, err := item.WithTiming(deliveries)
	if err != nil {
		t.Fatal(err)
	}
	if withTiming.Timing.DeliveryCount != 1 {
		t.Errorf("WithTiming DeliveryCount = %d, want 1", withTiming.Timing.DeliveryCount)
	}
	if item.Timing.DeliveryCount != 0 {
		t.Error("WithTiming mutated the receiver; it must return a copy")
	}
}

// TestWithTimingRejectsUntrustedDeliveries checks that timing is derived only
// from valid deliveries that belong to the item: a malformed delivery or one
// naming another item is rejected rather than counted as this item's history.
func TestWithTimingRejectsUntrustedDeliveries(t *testing.T) {
	item, err := domain.NewAttentionItem(validItemInput(domain.AttentionSpecApproval), nil)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	foreign := domain.AttentionDelivery{
		ItemID: "other-item", DeviceID: "d", Channel: "ntfy", Attempt: 1,
		SubmittedAt: base, Status: domain.DeliverySubmitted,
	}
	if _, err := item.WithTiming([]domain.AttentionDelivery{foreign}); !errors.Is(err, domain.ErrForeignDelivery) {
		t.Errorf("WithTiming accepted a foreign delivery: %v", err)
	}
	malformed := domain.AttentionDelivery{
		ItemID: item.ID, DeviceID: "d", SubmittedAt: base, Status: domain.DeliverySubmitted, // no channel/attempt
	}
	if _, err := item.WithTiming([]domain.AttentionDelivery{malformed}); !errors.Is(err, domain.ErrEmptyField) {
		t.Errorf("WithTiming accepted a malformed delivery: %v", err)
	}
	// A duplicated attempt (same device/channel/attempt) must not be counted twice.
	one := domain.AttentionDelivery{ItemID: item.ID, DeviceID: "d", Channel: "ntfy", Attempt: 1, SubmittedAt: base, Status: domain.DeliverySubmitted}
	if _, err := item.WithTiming([]domain.AttentionDelivery{one, one}); !errors.Is(err, domain.ErrDuplicate) {
		t.Errorf("WithTiming counted a duplicate delivery attempt: %v", err)
	}
}

// TestWithTimingCrossDeliveryValidates is the regression for the round-9 fix:
// timing derived from individually valid deliveries whose aggregate opened
// precedes aggregate accepted (independent minima) must survive Validate.
func TestWithTimingCrossDeliveryValidates(t *testing.T) {
	item, err := domain.NewAttentionItem(validItemInput(domain.AttentionSpecApproval), nil)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	openedOnly := domain.AttentionDelivery{
		ItemID: item.ID, DeviceID: "d1", Channel: "ntfy", Attempt: 1,
		SubmittedAt: base, OpenedAt: ptr(base.Add(2 * time.Minute)), Status: domain.DeliveryOpened,
	}
	acceptedLater := domain.AttentionDelivery{
		ItemID: item.ID, DeviceID: "d2", Channel: "ntfy", Attempt: 1,
		SubmittedAt: base.Add(time.Minute), ChannelAcceptedAt: ptr(base.Add(9 * time.Minute)), Status: domain.DeliveryChannelAccepted,
	}
	withTiming, err := item.WithTiming([]domain.AttentionDelivery{openedOnly, acceptedLater})
	if err != nil {
		t.Fatal(err)
	}
	if err := withTiming.Validate(); err != nil {
		t.Fatalf("item with valid cross-delivery timing failed Validate: %v", err)
	}
}

// TestDeliveryStatusRequiresReceipt checks the honesty invariant: a status may
// not claim a receipt the row cannot prove. Opened needs opened_at,
// channel-accepted needs channel_accepted_at; submitted needs only submitted_at.
func TestDeliveryStatusRequiresReceipt(t *testing.T) {
	base := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	tests := []struct {
		name    string
		mutate  func(*domain.AttentionDelivery)
		wantErr error
	}{
		{"opened without opened_at", func(d *domain.AttentionDelivery) {
			d.Status = domain.DeliveryOpened
		}, domain.ErrStatusMissingTimestamp},
		{"channel_accepted without accepted_at", func(d *domain.AttentionDelivery) {
			d.Status = domain.DeliveryChannelAccepted
		}, domain.ErrStatusMissingTimestamp},
		{"opened with opened_at", func(d *domain.AttentionDelivery) {
			d.Status = domain.DeliveryOpened
			d.ChannelAcceptedAt = ptr(base.Add(time.Minute))
			d.OpenedAt = ptr(base.Add(2 * time.Minute))
		}, nil},
		{"submitted needs only submitted_at", func(d *domain.AttentionDelivery) {
			d.Status = domain.DeliverySubmitted
		}, nil},
		{"submitted with opened_at is too strong", func(d *domain.AttentionDelivery) {
			d.Status = domain.DeliverySubmitted
			d.OpenedAt = ptr(base.Add(time.Minute))
		}, domain.ErrStatusTimestampTooStrong},
		{"submitted with channel_accepted_at is too strong", func(d *domain.AttentionDelivery) {
			d.Status = domain.DeliverySubmitted
			d.ChannelAcceptedAt = ptr(base.Add(time.Minute))
		}, domain.ErrStatusTimestampTooStrong},
		{"channel_accepted with opened_at is too strong", func(d *domain.AttentionDelivery) {
			d.Status = domain.DeliveryChannelAccepted
			d.ChannelAcceptedAt = ptr(base.Add(time.Minute))
			d.OpenedAt = ptr(base.Add(2 * time.Minute))
		}, domain.ErrStatusTimestampTooStrong},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := domain.AttentionDelivery{
				ItemID: "i", DeviceID: "dev", Channel: "ntfy", Attempt: 1, SubmittedAt: base,
			}
			tt.mutate(&d)
			err := d.Validate()
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// TestDeliveryRequiredFields checks the per-channel, per-attempt keys are
// required: a delivery with an empty channel or a non-positive attempt is
// rejected before its telemetry can be attributed.
func TestDeliveryRequiredFields(t *testing.T) {
	base := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	valid := domain.AttentionDelivery{
		ItemID: "i", DeviceID: "dev", Channel: "ntfy", Attempt: 1,
		SubmittedAt: base, Status: domain.DeliverySubmitted,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid delivery rejected: %v", err)
	}
	tests := []struct {
		name    string
		mutate  func(*domain.AttentionDelivery)
		wantErr error
	}{
		{"empty channel", func(d *domain.AttentionDelivery) { d.Channel = "" }, domain.ErrEmptyField},
		{"zero attempt", func(d *domain.AttentionDelivery) { d.Attempt = 0 }, domain.ErrNonPositive},
		{"negative attempt", func(d *domain.AttentionDelivery) { d.Attempt = -1 }, domain.ErrNonPositive},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := valid
			tt.mutate(&d)
			if err := d.Validate(); !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// TestDeliveryTimestampOrdering checks receipts are monotonically ordered along
// the lifecycle, so timing can never report a receipt before submission.
func TestDeliveryTimestampOrdering(t *testing.T) {
	base := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	tests := []struct {
		name    string
		mutate  func(*domain.AttentionDelivery)
		wantErr error
	}{
		{"opened before submitted", func(d *domain.AttentionDelivery) {
			d.Status = domain.DeliveryOpened
			d.OpenedAt = ptr(base.Add(-time.Minute))
		}, domain.ErrTimestampOutOfOrder},
		{"channel_accepted before submitted", func(d *domain.AttentionDelivery) {
			d.Status = domain.DeliveryChannelAccepted
			d.ChannelAcceptedAt = ptr(base.Add(-time.Minute))
		}, domain.ErrTimestampOutOfOrder},
		{"opened before channel_accepted", func(d *domain.AttentionDelivery) {
			d.Status = domain.DeliveryOpened
			d.ChannelAcceptedAt = ptr(base.Add(2 * time.Minute))
			d.OpenedAt = ptr(base.Add(time.Minute))
		}, domain.ErrTimestampOutOfOrder},
		{"in order", func(d *domain.AttentionDelivery) {
			d.Status = domain.DeliveryOpened
			d.ChannelAcceptedAt = ptr(base.Add(time.Minute))
			d.OpenedAt = ptr(base.Add(2 * time.Minute))
		}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := domain.AttentionDelivery{ItemID: "i", DeviceID: "dev", Channel: "ntfy", Attempt: 1, SubmittedAt: base}
			tt.mutate(&d)
			err := d.Validate()
			if tt.wantErr == nil && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// TestTimingSummaryValidate is the reconstruction backstop for the timing
// shape: a summary the store decodes must be one TimingAggregates could produce.
func TestTimingSummaryValidate(t *testing.T) {
	base := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	sub, acc, opn := base, base.Add(time.Minute), base.Add(5*time.Minute)
	gap := opn.Sub(sub)

	good := domain.TimingSummary{DeliveryCount: 2, FirstSubmittedAt: &sub, FirstAcceptedAt: &acc, FirstOpenedAt: &opn, SubmitToFirstOpen: &gap}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid timing rejected: %v", err)
	}
	if err := (domain.TimingSummary{}).Validate(); err != nil {
		t.Fatalf("zero timing rejected: %v", err)
	}
	// Valid: first_opened (an opened-only delivery) precedes first_accepted (a
	// different, later channel-accepted delivery). These are independent minima,
	// so the shape is legitimate and must not be rejected.
	cross := domain.TimingSummary{DeliveryCount: 2, FirstSubmittedAt: &sub, FirstAcceptedAt: ptr(base.Add(9 * time.Minute)), FirstOpenedAt: &opn, SubmitToFirstOpen: &gap}
	if err := cross.Validate(); err != nil {
		t.Fatalf("valid cross-delivery timing (opened before accepted) rejected: %v", err)
	}

	badGap := -time.Minute
	beforeSub := base.Add(-time.Minute)
	tests := map[string]domain.TimingSummary{
		"negative count":               {DeliveryCount: -1},
		"opened before submit":         {DeliveryCount: 1, FirstSubmittedAt: &sub, FirstOpenedAt: &beforeSub, SubmitToFirstOpen: durPtr(beforeSub.Sub(sub))},
		"accepted before submit":       {DeliveryCount: 1, FirstSubmittedAt: &sub, FirstAcceptedAt: &beforeSub},
		"negative gap":                 {DeliveryCount: 1, FirstSubmittedAt: &opn, FirstOpenedAt: &sub, SubmitToFirstOpen: &badGap},
		"gap without endpoints":        {DeliveryCount: 1, SubmitToFirstOpen: &gap},
		"endpoints without gap":        {DeliveryCount: 1, FirstSubmittedAt: &sub, FirstOpenedAt: &opn},
		"mismatched gap":               {DeliveryCount: 1, FirstSubmittedAt: &sub, FirstOpenedAt: &opn, SubmitToFirstOpen: durPtr(time.Second)},
		"zero submitted endpoint":      {DeliveryCount: 1, FirstSubmittedAt: &time.Time{}},
		"zero opened endpoint":         {DeliveryCount: 1, FirstSubmittedAt: &sub, FirstOpenedAt: &time.Time{}, SubmitToFirstOpen: durPtr(time.Time{}.Sub(sub))},
		"count zero with receipt":      {DeliveryCount: 0, FirstOpenedAt: &opn},
		"count zero with submission":   {DeliveryCount: 0, FirstSubmittedAt: &sub},
		"count positive no submission": {DeliveryCount: 3},
		"receipt without submission":   {DeliveryCount: 1, FirstAcceptedAt: &acc},
	}
	for name, ts := range tests {
		t.Run(name, func(t *testing.T) {
			if err := ts.Validate(); !errors.Is(err, domain.ErrInconsistentTiming) {
				t.Fatalf("Validate() = %v, want ErrInconsistentTiming", err)
			}
		})
	}
}

func durPtr(d time.Duration) *time.Duration { return &d }

func assertTiming(t *testing.T, label string, got, want domain.TimingSummary) {
	t.Helper()
	if got.DeliveryCount != want.DeliveryCount {
		t.Errorf("%s: DeliveryCount = %d, want %d", label, got.DeliveryCount, want.DeliveryCount)
	}
	assertTimePtr(t, label+" FirstSubmittedAt", got.FirstSubmittedAt, want.FirstSubmittedAt)
	assertTimePtr(t, label+" FirstAcceptedAt", got.FirstAcceptedAt, want.FirstAcceptedAt)
	assertTimePtr(t, label+" FirstOpenedAt", got.FirstOpenedAt, want.FirstOpenedAt)
	switch {
	case got.SubmitToFirstOpen == nil && want.SubmitToFirstOpen == nil:
	case got.SubmitToFirstOpen == nil || want.SubmitToFirstOpen == nil:
		t.Errorf("%s: SubmitToFirstOpen = %v, want %v", label, got.SubmitToFirstOpen, want.SubmitToFirstOpen)
	case *got.SubmitToFirstOpen != *want.SubmitToFirstOpen:
		t.Errorf("%s: SubmitToFirstOpen = %v, want %v", label, *got.SubmitToFirstOpen, *want.SubmitToFirstOpen)
	}
}

func assertTimePtr(t *testing.T, label string, got, want *time.Time) {
	t.Helper()
	switch {
	case got == nil && want == nil:
	case got == nil || want == nil:
		t.Errorf("%s = %v, want %v", label, got, want)
	case !got.Equal(*want):
		t.Errorf("%s = %v, want %v", label, *got, *want)
	}
}
