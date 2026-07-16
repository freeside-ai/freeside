package signet_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// seedSubmittedDelivery plants a submitted-only delivery row directly in the
// store, bypassing the pipeline: the item's persisted timing is deliberately
// left stale so tests can observe exactly which pipeline event recomputes it.
func seedSubmittedDelivery(t *testing.T, f fixture, deviceID domain.DeviceID, attempt int, at time.Time) domain.AttentionDelivery {
	t.Helper()
	row := domain.AttentionDelivery{
		ItemID: f.item.ID, DeviceID: deviceID, Channel: "ntfy", Attempt: attempt,
		SubmittedAt: at, Status: domain.DeliverySubmitted,
	}
	if err := f.store.Write(context.Background(), func(tx *store.WriteTx) error {
		return tx.PutAttentionDelivery(context.Background(), row)
	}); err != nil {
		t.Fatalf("seed delivery: %v", err)
	}
	return row
}

// TestRecordDeliveryOpenedAdvancesTiming: the opened receipt lands on the row
// and the item's persisted timing aggregates move in the same transaction —
// FirstOpenedAt and SubmitToFirstOpen appear, and the item's entity_version
// advances so cached clients invalidate.
func TestRecordDeliveryOpenedAdvancesTiming(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	submittedAt := *f.now
	seedSubmittedDelivery(t, f, f.device.ID, 1, submittedAt)
	_, before := f.itemSnapshot(t)

	openedAt := submittedAt.Add(3 * time.Minute)
	*f.now = openedAt
	row, err := f.service.RecordDeliveryOpened(ctx, f.item.ID, f.device.ID, "ntfy", 1)
	if err != nil {
		t.Fatalf("RecordDeliveryOpened: %v", err)
	}
	if row.Status != domain.DeliveryOpened {
		t.Errorf("row status = %q, want opened", row.Status)
	}
	if row.OpenedAt == nil || !row.OpenedAt.Equal(openedAt) {
		t.Errorf("row opened_at = %v, want %v", row.OpenedAt, openedAt)
	}
	if row.ChannelAcceptedAt != nil {
		t.Errorf("row channel_accepted_at = %v, want nil (never claimed)", row.ChannelAcceptedAt)
	}

	item, after := f.itemSnapshot(t)
	if item.Timing.DeliveryCount != 1 {
		t.Errorf("timing delivery_count = %d, want 1", item.Timing.DeliveryCount)
	}
	if item.Timing.FirstOpenedAt == nil || !item.Timing.FirstOpenedAt.Equal(openedAt) {
		t.Errorf("timing first_opened_at = %v, want %v", item.Timing.FirstOpenedAt, openedAt)
	}
	if item.Timing.SubmitToFirstOpen == nil || *item.Timing.SubmitToFirstOpen != 3*time.Minute {
		t.Errorf("timing submit_to_first_open = %v, want 3m", item.Timing.SubmitToFirstOpen)
	}
	if after.EntityVersion <= before.EntityVersion {
		t.Errorf("item entity_version %d → %d, want an advance", before.EntityVersion, after.EntityVersion)
	}
	if item.ItemVersion <= f.item.ItemVersion {
		t.Errorf("item_version %d → %d, want an advance", f.item.ItemVersion, item.ItemVersion)
	}
}

// TestRecordDeliveryOpenedReplayNoNewEffect: opening an already-opened attempt
// returns the recorded row and consumes no revision, mirroring the command
// path's idempotent-replay posture.
func TestRecordDeliveryOpenedReplayNoNewEffect(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	seedSubmittedDelivery(t, f, f.device.ID, 1, *f.now)
	openedAt := f.now.Add(time.Minute)
	*f.now = openedAt
	first, err := f.service.RecordDeliveryOpened(ctx, f.item.ID, f.device.ID, "ntfy", 1)
	if err != nil {
		t.Fatalf("RecordDeliveryOpened: %v", err)
	}
	before := f.revision(t)

	*f.now = openedAt.Add(time.Hour)
	replay, err := f.service.RecordDeliveryOpened(ctx, f.item.ID, f.device.ID, "ntfy", 1)
	if err != nil {
		t.Fatalf("RecordDeliveryOpened replay: %v", err)
	}
	if replay.OpenedAt == nil || !replay.OpenedAt.Equal(*first.OpenedAt) {
		t.Errorf("replay opened_at = %v, want the recorded %v", replay.OpenedAt, first.OpenedAt)
	}
	if after := f.revision(t); after != before {
		t.Errorf("replay moved the revision %d → %d", before, after)
	}
}

// TestRecordDeliveryOpenedGatesDevice: a revoked device produces no new
// server effect (the §5.14 test 15 posture applied to receipts), and an
// unknown delivery row is a loud not-found, never an implicit create.
func TestRecordDeliveryOpenedGatesDevice(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	seedSubmittedDelivery(t, f, f.device.ID, 1, *f.now)
	if _, err := f.service.Revoke(ctx, f.device.ID, f.device.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	before := f.revision(t)
	if _, err := f.service.RecordDeliveryOpened(ctx, f.item.ID, f.device.ID, "ntfy", 1); !errors.Is(err, signet.ErrDeviceNotActive) {
		t.Fatalf("RecordDeliveryOpened error = %v, want ErrDeviceNotActive", err)
	}
	if after := f.revision(t); after != before {
		t.Errorf("gated receipt moved the revision %d → %d", before, after)
	}

	f.seedDevice(t, "device-2")
	if _, err := f.service.RecordDeliveryOpened(ctx, f.item.ID, "device-2", "ntfy", 9); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("RecordDeliveryOpened(unknown attempt) error = %v, want ErrNotFound", err)
	}
}

// TestTimingReputSkippedWhenUnchanged: a receipt that moves no aggregate (a
// second device's later open, when the first open already set every first_*
// instant) writes the delivery row but leaves the item untouched — no
// entity_version churn, no staleness-invalidation of prepared commands.
func TestTimingReputSkippedWhenUnchanged(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.seedDevice(t, "device-2")
	submittedAt := *f.now
	seedSubmittedDelivery(t, f, f.device.ID, 1, submittedAt)
	seedSubmittedDelivery(t, f, "device-2", 1, submittedAt)

	*f.now = submittedAt.Add(time.Minute)
	if _, err := f.service.RecordDeliveryOpened(ctx, f.item.ID, f.device.ID, "ntfy", 1); err != nil {
		t.Fatalf("RecordDeliveryOpened(first): %v", err)
	}
	item, before := f.itemSnapshot(t)
	if item.Timing.DeliveryCount != 2 || item.Timing.FirstOpenedAt == nil {
		t.Fatalf("timing after first open = %+v, want count 2 with first_opened_at", item.Timing)
	}

	*f.now = submittedAt.Add(2 * time.Minute)
	if _, err := f.service.RecordDeliveryOpened(ctx, f.item.ID, "device-2", "ntfy", 1); err != nil {
		t.Fatalf("RecordDeliveryOpened(second): %v", err)
	}
	second, err := readDelivery(f, f.item.ID, "device-2", "ntfy", 1)
	if err != nil {
		t.Fatalf("GetAttentionDelivery: %v", err)
	}
	if second.Status != domain.DeliveryOpened {
		t.Errorf("second row status = %q, want opened", second.Status)
	}
	if _, after := f.itemSnapshot(t); after.EntityVersion != before.EntityVersion {
		t.Errorf("aggregate-neutral receipt churned entity_version %d → %d", before.EntityVersion, after.EntityVersion)
	}
}

// TestOpenToDecisionDerivableFromDeliveries is issue #69's acceptance 3: the
// §8 product metric, open-to-decision time, is computable from the delivery
// rows' opened_at and the decision that concluded the item — no dashboard,
// just the honest endpoints.
func TestOpenToDecisionDerivableFromDeliveries(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	seedSubmittedDelivery(t, f, f.device.ID, 1, *f.now)

	openedAt := f.now.Add(2 * time.Minute)
	*f.now = openedAt
	if _, err := f.service.RecordDeliveryOpened(ctx, f.item.ID, f.device.ID, "ntfy", 1); err != nil {
		t.Fatalf("RecordDeliveryOpened: %v", err)
	}

	decidedAt := openedAt.Add(5 * time.Minute)
	*f.now = decidedAt
	item, snap := f.itemSnapshot(t)
	cmd := f.command("cmd-decide", domain.ActionStop)
	cmd.Payload.ItemVersion = item.ItemVersion
	cmd.ExpectedEntityVersion = snap.EntityVersion
	if _, err := f.service.Submit(ctx, cmd); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	rows, err := f.service.ListAttentionItemDeliveries(ctx, f.item.ID)
	if err != nil {
		t.Fatalf("ListAttentionItemDeliveries: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("deliveries = %d rows, want 1", len(rows))
	}
	got := rows[0].Delivery.OpenedAt
	if got == nil || !got.Equal(openedAt) {
		t.Fatalf("opened_at = %v, want %v", got, openedAt)
	}
	if openToDecision := decidedAt.Sub(*got); openToDecision != 5*time.Minute {
		t.Errorf("open-to-decision = %v, want 5m", openToDecision)
	}
}

func readDelivery(f fixture, itemID domain.ItemID, deviceID domain.DeviceID, channel string, attempt int) (domain.AttentionDelivery, error) {
	var row domain.AttentionDelivery
	err := f.store.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		row, err = tx.GetAttentionDelivery(context.Background(), itemID, deviceID, channel, attempt)
		return err
	})
	return row, err
}
