package signet

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// This file is the delivery sender (#1924): what decides that an open item
// still owes a device a notification, and the loop that submits each one
// through SubmitDelivery. Nothing here is a new record. The delivery rows are
// the sender's only memory, so a restart resumes exactly where the rows say
// it stopped: an accepted attempt is never repeated, and a failed one is
// retried on the schedule its submitted_at implies.

const (
	// maxDeliveryAttempts bounds the retries of one item to one device. An
	// outage longer than the schedule below is the provider's, and the item
	// stays in the inbox either way: a notification is a hint (plan §4).
	maxDeliveryAttempts = 6
	// firstDeliveryRetryWait is the wait after the first failed attempt; each
	// further failure doubles it, so six attempts span 1+2+4+8+16 minutes.
	firstDeliveryRetryWait = time.Minute
)

// owedDelivery is one open item an active device has not yet been notified
// of, and may be notified of now.
type owedDelivery struct {
	itemID   domain.ItemID
	deviceID domain.DeviceID
}

// owedDeliveries decides which item and device pairs get an attempt at now,
// from the store's lists alone. A pair is owed when the item is open, the
// device is active, the item was not created before the device paired, and
// its ntfy attempts so far all failed, number fewer than
// maxDeliveryAttempts, and the last one's wait has passed. An accepted or
// opened attempt settles the pair for good: each item notifies each device
// once.
//
// An item with no CreatedAt is owed: the field is nil only on items that
// predate it, and those are old enough that the device's pairing instant
// says nothing about them. The caller leaves snoozed proposals out of items.
func owedDeliveries(
	items []domain.AttentionItem, devices []domain.Device, deliveries []domain.AttentionDelivery, now time.Time,
) []owedDelivery {
	type history struct {
		// attempts is the highest attempt number recorded, which is the count:
		// nextAttempt numbers them 1, 2, 3 without gaps.
		attempts      int
		lastSubmitted time.Time
		settled       bool
	}
	histories := map[owedDelivery]*history{}
	for _, d := range deliveries {
		if d.Channel != channelNtfy {
			continue
		}
		key := owedDelivery{itemID: d.ItemID, deviceID: d.DeviceID}
		h := histories[key]
		if h == nil {
			h = &history{}
			histories[key] = h
		}
		// A behaviour switch, no default: a new DeliveryStatus member must say
		// here whether it ends the retries.
		switch d.Status {
		case domain.DeliverySubmitted:
		case domain.DeliveryChannelAccepted, domain.DeliveryOpened:
			h.settled = true
		}
		if d.Attempt > h.attempts {
			h.attempts = d.Attempt
			h.lastSubmitted = d.SubmittedAt
		}
	}

	var owed []owedDelivery
	for _, item := range items {
		if item.Status != domain.StatusOpen {
			continue
		}
		for _, device := range devices {
			if device.Status != domain.DeviceActive {
				continue
			}
			if item.CreatedAt != nil && item.CreatedAt.Before(device.PairedAt) {
				continue
			}
			key := owedDelivery{itemID: item.ID, deviceID: device.ID}
			if h := histories[key]; h != nil {
				if h.settled || h.attempts >= maxDeliveryAttempts {
					continue
				}
				if now.Before(h.lastSubmitted.Add(deliveryRetryWait(h.attempts))) {
					continue
				}
			}
			owed = append(owed, key)
		}
	}
	return owed
}

// deliveryRetryWait is how long the sender waits after the failed-th
// consecutive failed attempt before the next: 1, 2, 4, 8, then 16 minutes.
func deliveryRetryWait(failed int) time.Duration {
	return firstDeliveryRetryWait << (failed - 1)
}

// DeliveryFailure is one notification the sender could not get accepted. Its
// text is safe to log: SubmitDelivery's errors carry the item, the device,
// and a status code or failure class, never the device's topic.
type DeliveryFailure struct {
	ItemID   domain.ItemID
	DeviceID domain.DeviceID
	// Attempt is the attempt that failed, or 0 when SubmitDelivery returned
	// no row: the failure came before an attempt was recorded, or while
	// recording its outcome.
	Attempt int
	// Final reports that this was the last attempt the sender will make for
	// this item and device.
	Final bool
	Err   error
}

func (f *DeliveryFailure) Error() string {
	return fmt.Sprintf("notify device %s of item %s: %v", f.DeviceID, f.ItemID, f.Err)
}

func (f *DeliveryFailure) Unwrap() error { return f.Err }

// DeliverOwed runs one pass: it reads the open items, the devices, and the
// delivery rows in a single transaction, then submits one attempt for each
// owed pair. A failed attempt is reported as a *DeliveryFailure and the pass
// goes on to the next pair, so one device's failing topic never holds back
// another's notification. The returned error is the pass itself failing (the
// store could not be read) or ctx ending.
func (s *Service) DeliverOwed(ctx context.Context, report func(error)) error {
	now := s.now().UTC()
	var (
		items      []domain.AttentionItem
		devices    []domain.Device
		deliveries []domain.AttentionDelivery
	)
	err := s.store.Read(ctx, func(tx *store.ReadTx) error {
		for _, itemType := range domain.AllAttentionTypes {
			open, err := tx.ListOpenAttentionItems(ctx, itemType)
			if err != nil {
				return err
			}
			for _, item := range open {
				// SubmitDelivery refuses a snoozed proposal too; leaving it
				// out here spares a refused write on every pass of the snooze.
				if snoozed, err := proposalSnoozed(ctx, tx, item, now); err != nil {
					return err
				} else if !snoozed {
					items = append(items, item)
				}
			}
		}
		listedDevices, err := tx.ListDevices(ctx)
		if err != nil {
			return err
		}
		for _, device := range listedDevices {
			devices = append(devices, device.Value)
		}
		listedDeliveries, err := tx.ListAttentionDeliveries(ctx)
		if err != nil {
			return err
		}
		for _, delivery := range listedDeliveries {
			deliveries = append(deliveries, delivery.Value)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("attention deliveries: read state: %w", err)
	}

	for _, owed := range owedDeliveries(items, devices, deliveries, now) {
		if err := ctx.Err(); err != nil {
			return err
		}
		row, err := s.SubmitDelivery(ctx, owed.itemID, owed.deviceID)
		switch {
		case err == nil:
		case errors.Is(err, ErrItemNotOpenForDelivery), errors.Is(err, ErrProposalSnoozed),
			errors.Is(err, ErrDeviceNotActive), errors.Is(err, store.ErrNotFound):
			// The item was decided, snoozed, or removed, or the device was
			// revoked, between the read above and this write: nothing is owed
			// any more, and that is not a failure.
		case ctx.Err() != nil && errors.Is(err, ctx.Err()):
			// ctx ended before the attempt was recorded. That is the pass
			// ending, not a failed send: the publish itself outlives ctx, so
			// a failure it returns is still reported below.
			return ctx.Err()
		case report != nil:
			report(&DeliveryFailure{
				ItemID: owed.itemID, DeviceID: owed.deviceID,
				Attempt: row.Attempt, Final: row.Attempt >= maxDeliveryAttempts, Err: err,
			})
		}
	}
	return nil
}

// RunDeliveries sends every owed notification at startup, then again on each
// interval, until ctx ends. Failures are reported and never stop the loop: a
// provider outage is the ordinary case, and the next pass retries what the
// delivery rows show is still owed.
//
// It returns ErrNotifierUnavailable at once when the channel is absent or
// misconfigured. That is a composition without notifications, not a failure
// of this loop, and the caller tells the two apart with errors.Is.
func (s *Service) RunDeliveries(ctx context.Context, interval time.Duration, report func(error)) error {
	if interval <= 0 {
		return errors.New("attention deliveries: interval must be positive")
	}
	if s.ntfy == nil {
		return fmt.Errorf("attention deliveries: %w", ErrNotifierUnavailable)
	}
	if err := s.ntfy.validate(); err != nil {
		return fmt.Errorf("attention deliveries: %w: %w", err, ErrNotifierUnavailable)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := s.DeliverOwed(ctx, report); err != nil && ctx.Err() == nil && report != nil {
			report(err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
