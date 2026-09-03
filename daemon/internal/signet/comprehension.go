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
func (s *Service) RecordComprehensionEvent(
	ctx context.Context, in domain.ComprehensionEventInput,
) (domain.ComprehensionEvent, error) {
	var out domain.ComprehensionEvent
	err := s.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		device, err := tx.GetDevice(ctx, in.DeviceID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("device %q: %w", in.DeviceID, ErrDeviceNotActive)
			}
			return err
		}
		if device.Status != domain.DeviceActive {
			return fmt.Errorf("device %q: %w", in.DeviceID, ErrDeviceNotActive)
		}
		if _, err := tx.GetAttentionItem(ctx, in.ItemID); err != nil {
			return err
		}
		event, err := domain.NewComprehensionEvent(in, s.now().UTC())
		if err != nil {
			return err
		}
		if event.Kind == domain.ComprehensionActionTaken ||
			event.Kind == domain.ComprehensionRecommendationOverride {
			command, err := tx.GetCommand(ctx, event.CommandID)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("event %q command %q: %w", event.EventID, event.CommandID, ErrComprehensionEventUnbacked)
				}
				return err
			}
			if command.DeviceID != event.DeviceID || command.ItemID != event.ItemID ||
				command.DecisionEvidence == nil || event.DecisionActionSurfaceDigest == nil ||
				command.DecisionEvidence.ActionSurfaceDigest != *event.DecisionActionSurfaceDigest {
				return fmt.Errorf("event %q: %w", event.EventID, ErrComprehensionEventUnbacked)
			}
		}
		recorded, err := tx.RecordComprehensionEvent(ctx, event)
		if err != nil {
			return err
		}
		out = recorded
		return nil
	})
	if err != nil {
		return domain.ComprehensionEvent{}, fmt.Errorf("record comprehension event %q/%q: %w", in.DeviceID, in.EventID, err)
	}
	return out, nil
}
