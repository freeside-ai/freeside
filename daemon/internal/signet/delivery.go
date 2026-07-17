package signet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// This file is the delivery pipeline (plan §4, issue #69): the write side of
// AttentionDelivery. Rows are created and advanced by the daemon; the one
// client-reachable write is the opened receipt (#130), which advances an
// existing attempt and never creates one. The honest lifecycle is
// submitted → channel_accepted → opened, and "delivered" does not exist by
// design. RecordDeliveryOpened is the in-process boundary;
// ReportDeliveryOpened projects it to the wire as a resource snapshot.

// sendPhaseTimeout bounds the daemon-owned post-commit phase of a submission
// (the provider call plus the acceptance Write) once it no longer rides the
// caller's context.
const sendPhaseTimeout = 30 * time.Second

// ErrNotifierUnavailable is returned when delivery submission is exercised on
// a service composed without a usable notification channel; it fails closed
// before any write rather than record a submission no channel will carry.
var ErrNotifierUnavailable = errors.New("notification channel is not configured")

// ErrItemNotOpenForDelivery rejects a delivery submission for an item whose
// lifecycle has concluded: a notification is an interruption asking for a
// decision, and a concluded item has none to ask for. (Receipts on already
// recorded attempts are different — see RecordDeliveryOpened.)
var ErrItemNotOpenForDelivery = errors.New("item is not open for delivery")

// SubmitDelivery records one notification attempt for item to device over the
// ntfy channel. The pipeline is two transactions around the external call, so
// external I/O never runs inside a Write and every committed state is honest:
// the first Write records the submitted row (submitted_at only) and commits;
// the ntfy publish happens outside any transaction; on the provider's 2xx a
// second Write advances the row to channel_accepted. A crash or channel
// failure between them leaves a submitted-only row — exactly what is known to
// have happened — and a later retry is the next attempt number. The provider's
// acceptance populates channel_accepted_at and nothing stronger: "delivered"
// does not exist in this vocabulary (plan §4).
func (s *Service) SubmitDelivery(ctx context.Context, itemID domain.ItemID, deviceID domain.DeviceID) (domain.AttentionDelivery, error) {
	if s.ntfy == nil {
		return domain.AttentionDelivery{}, fmt.Errorf("submit delivery %s/%s: %w", itemID, deviceID, ErrNotifierUnavailable)
	}
	if err := s.ntfy.validate(); err != nil {
		return domain.AttentionDelivery{}, fmt.Errorf("submit delivery %s/%s: %w: %w", itemID, deviceID, err, ErrNotifierUnavailable)
	}

	var (
		row  domain.AttentionDelivery
		hint notification
	)
	err := s.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := gateActiveDevice(ctx, tx, deviceID); err != nil {
			return err
		}
		item, _, err := tx.GetAttentionItemSnapshot(ctx, itemID)
		if err != nil {
			return err
		}
		if item.Status != domain.StatusOpen {
			return fmt.Errorf("item %q is %s: %w", itemID, item.Status, ErrItemNotOpenForDelivery)
		}
		attempt, err := nextAttempt(ctx, tx, itemID, deviceID)
		if err != nil {
			return err
		}
		row = domain.AttentionDelivery{
			ItemID: itemID, DeviceID: deviceID, Channel: channelNtfy, Attempt: attempt,
			SubmittedAt: s.now().UTC(), Status: domain.DeliverySubmitted,
		}
		if err := tx.PutAttentionDelivery(ctx, row); err != nil {
			return err
		}
		if err := recomputeItemTiming(ctx, tx, itemID); err != nil {
			return err
		}
		hint = s.ntfy.notificationFor(item, deviceID)
		return nil
	})
	if err != nil {
		return domain.AttentionDelivery{}, fmt.Errorf("submit delivery %s/%s: %w", itemID, deviceID, err)
	}

	// Once the submitted row is durable, the provider call and its acceptance
	// record run under a daemon-owned bounded context: a caller abandoning its
	// request (the dev-harness control route passes the request context) must
	// not strand a committed attempt half-advanced, send a notification whose
	// acceptance is then never recorded, or seed a duplicate on retry.
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendPhaseTimeout)
	defer cancel()

	if err := s.ntfy.publish(sendCtx, hint); err != nil {
		// The submitted row stands: it claims only submitted_at, which is
		// true. The caller decides whether to retry as the next attempt.
		return row, fmt.Errorf("submit delivery %s/%s attempt %d: %w", itemID, deviceID, row.Attempt, err)
	}

	var accepted domain.AttentionDelivery
	err = s.store.Write(sendCtx, func(tx *store.WriteTx) error {
		current, err := tx.GetAttentionDelivery(sendCtx, itemID, deviceID, channelNtfy, row.Attempt)
		if err != nil {
			return err
		}
		if current.Status != domain.DeliverySubmitted {
			// An opened receipt raced in between the publish and this Write.
			// Receipts are immutable and status only advances, so the stronger
			// recorded state wins and the acceptance instant is not recorded.
			accepted = current
			return errReplay
		}
		now := s.now().UTC()
		current.ChannelAcceptedAt = &now
		current.Status = domain.DeliveryChannelAccepted
		if err := tx.PutAttentionDelivery(sendCtx, current); err != nil {
			return err
		}
		if err := recomputeItemTiming(sendCtx, tx, itemID); err != nil {
			return err
		}
		accepted = current
		return nil
	})
	if err != nil && !errors.Is(err, errReplay) {
		return domain.AttentionDelivery{}, fmt.Errorf("submit delivery %s/%s attempt %d: record acceptance: %w",
			itemID, deviceID, row.Attempt, err)
	}
	return accepted, nil
}

// nextAttempt numbers the new submission after every attempt already recorded
// for this item, device, and channel, counting failed ones: the attempt
// sequence is the retry history, not the success history.
func nextAttempt(ctx context.Context, tx *store.WriteTx, itemID domain.ItemID, deviceID domain.DeviceID) (int, error) {
	values, err := tx.ListAttentionDeliveries(ctx)
	if err != nil {
		return 0, err
	}
	next := 1
	for _, value := range values {
		d := value.Value
		if d.ItemID == itemID && d.DeviceID == deviceID && d.Channel == channelNtfy && d.Attempt >= next {
			next = d.Attempt + 1
		}
	}
	return next, nil
}

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

// ReportDeliveryOpened is the device-facing wire boundary for opened receipts
// (#130): it delegates the write to RecordDeliveryOpened unchanged (idempotent
// replay, active-device gate, no open-item gate) and returns the recorded row
// as a wire resource snapshot. The snapshot is read after the write rather
// than captured inside it: the replay path rolls its transaction back, so an
// in-transaction snapshot would describe a state that never committed. The
// gap between write and read is benign — opened is terminal and receipts are
// immutable, so the row cannot change under the read; only as_of_revision can
// advance, which is ordinary partial-fetch staleness (plan §5.14).
func (s *Service) ReportDeliveryOpened(ctx context.Context, itemID domain.ItemID, deviceID domain.DeviceID, channel string, attempt int) (AttentionDeliverySnapshot, error) {
	if _, err := s.RecordDeliveryOpened(ctx, itemID, deviceID, channel, attempt); err != nil {
		return AttentionDeliverySnapshot{}, err
	}
	var out AttentionDeliverySnapshot
	err := s.store.Read(ctx, func(tx *store.ReadTx) error {
		state, err := tx.ServerState(ctx)
		if err != nil {
			return err
		}
		// Re-gate the parent item at this reconstruction boundary. The replay
		// path returns errReplay before recomputeItemTiming runs, so unlike
		// the write path this read never reconstructs the item; without this a
		// receipt whose item now fails the approved-recipe evidence gate would
		// return 200 here while the sibling ListAttentionItemDeliveries and
		// Bootstrap reject the same state.
		_, itemState, err := tx.GetAttentionItemSnapshot(ctx, itemID)
		if err != nil {
			return err
		}
		if err := validateSnapshot(state, itemState); err != nil {
			return err
		}
		values, err := tx.ListAttentionDeliveries(ctx)
		if err != nil {
			return err
		}
		for _, value := range values {
			d := value.Value
			if d.ItemID != itemID || d.DeviceID != deviceID || d.Channel != channel || d.Attempt != attempt {
				continue
			}
			if err := validateSnapshot(state, value.Snapshot); err != nil {
				return err
			}
			out = deliverySnapshot(d, value.Snapshot)
			return nil
		}
		return store.ErrNotFound
	})
	if err != nil {
		return AttentionDeliverySnapshot{}, fmt.Errorf("report delivery %s/%s/%s/%d opened: %w",
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
