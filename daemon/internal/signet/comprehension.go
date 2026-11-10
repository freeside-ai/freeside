package signet

import (
	"context"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// RecordComprehensionEvent ingests one typed decision-path event (plan §8). It
// follows the delivery-receipt discipline: it records the fact on the internal
// (non-synchronized) path, so recording never advances the sync revision, and
// it has no version precondition. It gates the active device and requires the
// item to exist. For the action-bearing kinds it additionally requires the
// referenced command to exist, belong to this device and item, and carry the
// same action surface digest in its stamped evidence. A replay of the client's
// (device, event) idempotency key returns the recorded row unchanged.
//
// The one exception is the first card_opened for an (item, decision-surface
// digest): it also records an AttentionItemView stamped with the event's
// received_at, and that write commits on the synchronized path, one revision
// bump, so every device's inbox learns the item was viewed (issue #2930). The
// event and the view commit together. A repeat open of a surface already
// viewed, and every other kind, stays on the internal path. Two devices
// opening the same new surface at once can both take the synchronized path;
// the view is insert-only, so the earlier commit's time stands and the cost is
// one extra revision.
func (s *Service) RecordComprehensionEvent(
	ctx context.Context, in domain.ComprehensionEventInput,
) (domain.ComprehensionEvent, error) {
	var out domain.ComprehensionEvent
	firstView, err := s.isFirstSurfaceView(ctx, in)
	if err != nil {
		return domain.ComprehensionEvent{}, fmt.Errorf("record comprehension event %q/%q: %w", in.DeviceID, in.EventID, err)
	}
	if firstView {
		err = s.store.Write(ctx, func(tx *store.WriteTx) error {
			recorded, err := s.recordComprehensionEvent(ctx, &tx.InternalTx, in)
			if err != nil {
				return err
			}
			out = recorded
			return tx.PutAttentionItemView(ctx, domain.AttentionItemView{
				ItemID:                recorded.ItemID,
				DecisionSurfaceDigest: recorded.ItemDecisionSurfaceDigest,
				FirstOpenedAt:         recorded.ReceivedAt,
			})
		})
	} else {
		err = s.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
			recorded, err := s.recordComprehensionEvent(ctx, tx, in)
			if err != nil {
				return err
			}
			out = recorded
			return nil
		})
	}
	if err != nil {
		return domain.ComprehensionEvent{}, fmt.Errorf("record comprehension event %q/%q: %w", in.DeviceID, in.EventID, err)
	}
	return out, nil
}

// isFirstSurfaceView reports whether in is a card_opened for a decision
// surface no device has opened yet. It reads outside the recording
// transaction, so the answer can be stale by the time that commits; the
// insert-only view absorbs the race (RecordComprehensionEvent).
func (s *Service) isFirstSurfaceView(ctx context.Context, in domain.ComprehensionEventInput) (bool, error) {
	if in.Kind != domain.ComprehensionCardOpened {
		return false, nil
	}
	first := false
	err := s.store.Read(ctx, func(tx *store.ReadTx) error {
		_, _, err := tx.GetAttentionItemView(ctx, in.ItemID, in.ItemDecisionSurfaceDigest)
		switch {
		case errors.Is(err, store.ErrNotFound):
			first = true
			return nil
		case err != nil:
			// A malformed view row reads as viewed: the event still records on
			// the internal path, and the item serves nil (withSurfaceView).
			s.logger.Warn("treat unreadable attention item view as present",
				"item_id", in.ItemID, "error", err)
			return nil
		default:
			return nil
		}
	})
	return first, err
}

// recordComprehensionEvent validates and records one event in tx, the shared
// body of both recording paths.
func (s *Service) recordComprehensionEvent(
	ctx context.Context, tx *store.InternalTx, in domain.ComprehensionEventInput,
) (domain.ComprehensionEvent, error) {
	device, err := tx.GetDevice(ctx, in.DeviceID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return domain.ComprehensionEvent{}, fmt.Errorf("device %q: %w", in.DeviceID, ErrDeviceNotActive)
		}
		return domain.ComprehensionEvent{}, err
	}
	if device.Status != domain.DeviceActive {
		return domain.ComprehensionEvent{}, fmt.Errorf("device %q: %w", in.DeviceID, ErrDeviceNotActive)
	}
	if _, err := tx.GetAttentionItem(ctx, in.ItemID); err != nil {
		return domain.ComprehensionEvent{}, err
	}
	event, err := domain.NewComprehensionEvent(in, s.now().UTC())
	if err != nil {
		return domain.ComprehensionEvent{}, err
	}
	if event.Kind == domain.ComprehensionActionTaken ||
		event.Kind == domain.ComprehensionRecommendationOverride {
		command, err := tx.GetCommand(ctx, event.CommandID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return domain.ComprehensionEvent{}, fmt.Errorf("event %q command %q: %w", event.EventID, event.CommandID, ErrComprehensionEventUnbacked)
			}
			return domain.ComprehensionEvent{}, err
		}
		if command.DeviceID != event.DeviceID || command.ItemID != event.ItemID ||
			command.DecisionEvidence == nil || event.DecisionActionSurfaceDigest == nil ||
			command.DecisionEvidence.ActionSurfaceDigest != *event.DecisionActionSurfaceDigest {
			return domain.ComprehensionEvent{}, fmt.Errorf("event %q: %w", event.EventID, ErrComprehensionEventUnbacked)
		}
	}
	return tx.RecordComprehensionEvent(ctx, event)
}
