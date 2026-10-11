package signet

import (
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestOwedDeliveries pins the rule that decides who is notified of what, and
// when: each open item notifies each active device once, an item older than
// a device's pairing never does, and a failed attempt is retried on the
// 1, 2, 4, 8, 16 minute schedule until the sixth attempt.
func TestOwedDeliveries(t *testing.T) {
	paired := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	now := paired.Add(24 * time.Hour)
	before, after := paired.Add(-time.Second), paired.Add(time.Second)

	item := func(id domain.ItemID, status domain.ItemStatus, createdAt *time.Time) domain.AttentionItem {
		return domain.AttentionItem{ID: id, Status: status, CreatedAt: createdAt}
	}
	device := func(id domain.DeviceID, status domain.DeviceStatus) domain.Device {
		return domain.Device{ID: id, Status: status, PairedAt: paired}
	}
	attempt := func(n int, status domain.DeliveryStatus, submittedAt time.Time) domain.AttentionDelivery {
		return domain.AttentionDelivery{
			ItemID: "item", DeviceID: "phone", Channel: channelNtfy, Attempt: n,
			SubmittedAt: submittedAt, Status: status,
		}
	}
	// failures is n failed attempts, the last submitted at lastAt.
	failures := func(n int, lastAt time.Time) []domain.AttentionDelivery {
		var rows []domain.AttentionDelivery
		for i := 1; i <= n; i++ {
			rows = append(rows, attempt(i, domain.DeliverySubmitted, lastAt.Add(-time.Duration(n-i)*time.Hour)))
		}
		return rows
	}
	open := []domain.AttentionItem{item("item", domain.StatusOpen, &after)}
	phone := []domain.Device{device("phone", domain.DeviceActive)}
	owedOnce := []owedDelivery{{itemID: "item", deviceID: "phone"}}

	for name, tc := range map[string]struct {
		items      []domain.AttentionItem
		devices    []domain.Device
		deliveries []domain.AttentionDelivery
		want       []owedDelivery
	}{
		"an open item owes an active device": {items: open, devices: phone, want: owedOnce},
		"a resolved item owes nothing": {
			items: []domain.AttentionItem{item("item", domain.StatusResolved, &after)}, devices: phone,
		},
		"a superseded item owes nothing": {
			items: []domain.AttentionItem{item("item", domain.StatusSuperseded, &after)}, devices: phone,
		},
		"a revoked device is owed nothing": {
			items: open, devices: []domain.Device{device("phone", domain.DeviceRevoked)},
		},
		"an item created before the device paired owes nothing": {
			items: []domain.AttentionItem{item("item", domain.StatusOpen, &before)}, devices: phone,
		},
		"an item created as the device paired is owed": {
			items: []domain.AttentionItem{item("item", domain.StatusOpen, &paired)}, devices: phone, want: owedOnce,
		},
		"an item with no creation time is owed": {
			items: []domain.AttentionItem{item("item", domain.StatusOpen, nil)}, devices: phone, want: owedOnce,
		},
		"an accepted attempt settles the pair": {
			items: open, devices: phone,
			deliveries: []domain.AttentionDelivery{attempt(1, domain.DeliveryChannelAccepted, now.Add(-time.Hour))},
		},
		"an opened attempt settles the pair": {
			items: open, devices: phone,
			deliveries: []domain.AttentionDelivery{attempt(1, domain.DeliveryOpened, now.Add(-time.Hour))},
		},
		"an accepted retry settles the pair": {
			items: open, devices: phone,
			deliveries: []domain.AttentionDelivery{
				attempt(1, domain.DeliverySubmitted, now.Add(-2*time.Hour)),
				attempt(2, domain.DeliveryChannelAccepted, now.Add(-time.Hour)),
			},
		},
		"a failed attempt waits a minute":   {items: open, devices: phone, deliveries: failures(1, now.Add(-time.Minute+time.Second))},
		"then is retried":                   {items: open, devices: phone, deliveries: failures(1, now.Add(-time.Minute)), want: owedOnce},
		"a second failure waits two":        {items: open, devices: phone, deliveries: failures(2, now.Add(-2*time.Minute+time.Second))},
		"then is retried too":               {items: open, devices: phone, deliveries: failures(2, now.Add(-2*time.Minute)), want: owedOnce},
		"a third failure waits four":        {items: open, devices: phone, deliveries: failures(3, now.Add(-4*time.Minute+time.Second))},
		"a fourth failure waits eight":      {items: open, devices: phone, deliveries: failures(4, now.Add(-8*time.Minute+time.Second))},
		"a fifth failure waits sixteen":     {items: open, devices: phone, deliveries: failures(5, now.Add(-16*time.Minute+time.Second))},
		"then gets its last attempt":        {items: open, devices: phone, deliveries: failures(5, now.Add(-16*time.Minute)), want: owedOnce},
		"a sixth failure ends the attempts": {items: open, devices: phone, deliveries: failures(6, now.Add(-72*time.Hour))},
		"another channel's rows do not count": {
			items: open, devices: phone,
			deliveries: []domain.AttentionDelivery{{
				ItemID: "item", DeviceID: "phone", Channel: "apns", Attempt: 1,
				SubmittedAt: now.Add(-time.Hour), Status: domain.DeliveryChannelAccepted,
			}},
			want: owedOnce,
		},
		"each device is owed separately": {
			items:      open,
			devices:    []domain.Device{device("phone", domain.DeviceActive), device("tablet", domain.DeviceActive)},
			deliveries: []domain.AttentionDelivery{attempt(1, domain.DeliveryChannelAccepted, now.Add(-time.Hour))},
			want:       []owedDelivery{{itemID: "item", deviceID: "tablet"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := owedDeliveries(tc.items, tc.devices, tc.deliveries, now)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("owed = %v, want %v", got, tc.want)
			}
		})
	}
}
