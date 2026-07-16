package signet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// This file is the delivery pipeline (plan §4, issue #69): the write side of
// AttentionDelivery. Rows are recorded by the daemon, never submitted by
// clients (api/openapi.yaml's delivery listing is read-only); the honest
// lifecycle is submitted → channel_accepted → opened, and "delivered" does
// not exist by design. The device-facing wire path for opened receipts is a
// deferred contract unit; until it lands, RecordDeliveryOpened is the
// in-process boundary.

// RecordDeliveryOpened records the device-level opened receipt on one
// delivery attempt and re-derives the item's timing aggregates in the same
// transaction. Opening an already-opened attempt is an idempotent replay: the
// recorded row returns unchanged and no revision is consumed. The item's
// lifecycle status is deliberately not a gate — a late open of a resolved
// item is honest telemetry, and its canonical rendering is §5.14 test 9's
// deep-link concern, not the receipt's. The active-device gate does apply: a
// revoked device produces no new server effect (the §5.14 test 15 posture).
func (s *Service) RecordDeliveryOpened(ctx context.Context, itemID domain.ItemID, deviceID domain.DeviceID, channel string, attempt int) (domain.AttentionDelivery, error) {
	var out domain.AttentionDelivery
	err := s.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := gateActiveDevice(ctx, tx, deviceID); err != nil {
			return err
		}
		row, err := tx.GetAttentionDelivery(ctx, itemID, deviceID, channel, attempt)
		if err != nil {
			return err
		}
		if row.Status == domain.DeliveryOpened {
			out = row
			return errReplay
		}
		now := s.now().UTC()
		row.OpenedAt = &now
		row.Status = domain.DeliveryOpened
		if err := tx.PutAttentionDelivery(ctx, row); err != nil {
			return err
		}
		if err := recomputeItemTiming(ctx, tx, itemID); err != nil {
			return err
		}
		out = row
		return nil
	})
	if err != nil && !errors.Is(err, errReplay) {
		return domain.AttentionDelivery{}, fmt.Errorf("record delivery %s/%s/%s/%d opened: %w",
			itemID, deviceID, channel, attempt, err)
	}
	return out, nil
}

// recomputeItemTiming re-derives the item's persisted timing aggregates from
// its full delivery set, inside the transaction that changed a delivery row.
// The aggregate is persisted rather than derived at read because timing is a
// required field of the wire item and one entity_version must always mean one
// body: sync invalidation and command acceptance both key on entity_version,
// so a body that drifted under an unchanged version would break them (the
// implementation decision #28's note deferred to this unit). The re-put
// happens only when the summary actually changed — a delivery event that
// moves no first_* instant and no count must not churn item versions or
// staleness-invalidate prepared commands for nothing.
func recomputeItemTiming(ctx context.Context, tx *store.WriteTx, itemID domain.ItemID) error {
	item, _, err := tx.GetAttentionItemSnapshot(ctx, itemID)
	if err != nil {
		return err
	}
	values, err := tx.ListAttentionDeliveries(ctx)
	if err != nil {
		return err
	}
	deliveries := make([]domain.AttentionDelivery, 0, len(values))
	for _, value := range values {
		if value.Value.ItemID == itemID {
			deliveries = append(deliveries, value.Value)
		}
	}
	next, err := item.WithTiming(deliveries)
	if err != nil {
		return err
	}
	same, err := timingEqual(item.Timing, next.Timing)
	if err != nil {
		return err
	}
	if same {
		return nil
	}
	next.ItemVersion++
	return tx.PutAttentionItem(ctx, next)
}

// timingEqual compares two summaries by their canonical JSON rendering. Both
// sides of the comparison derive from store-decoded values, so the rendering
// is stable; JSON equality sidesteps time.Time's monotonic-clock and location
// fields, which reflect.DeepEqual would treat as differences between one
// instant's two representations.
func timingEqual(a, b domain.TimingSummary) (bool, error) {
	aj, err := json.Marshal(a)
	if err != nil {
		return false, err
	}
	bj, err := json.Marshal(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(aj, bj), nil
}
