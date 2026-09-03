package signet

import (
	"context"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// RegisterClientCapability records the set of decision actions the device's app
// build can present and submit (plan §5.14, §8). It gates the active device and
// skips the write, avoiding a needless revision bump, when the stored contract
// already carries the same content-address digest.
func (s *Service) RegisterClientCapability(
	ctx context.Context, deviceID domain.DeviceID, actions []domain.Action,
) (domain.ClientCapabilityContract, error) {
	contract, err := domain.NewClientCapabilityContract(deviceID, actions)
	if err != nil {
		return domain.ClientCapabilityContract{}, fmt.Errorf("register capability %q: %w", deviceID, err)
	}
	var out domain.ClientCapabilityContract
	err = s.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := gateActiveDevice(ctx, tx, deviceID); err != nil {
			return err
		}
		existing, err := tx.GetDeviceCapabilityContract(ctx, deviceID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err == nil && existing.Digest == contract.Digest {
			out = existing
			return errReplay
		}
		if err := tx.PutDeviceCapabilityContract(ctx, contract, s.now().UTC()); err != nil {
			return err
		}
		out = contract
		return nil
	})
	if err != nil && !errors.Is(err, errReplay) {
		return domain.ClientCapabilityContract{}, fmt.Errorf("register capability %q: %w", deviceID, err)
	}
	return out, nil
}

// DeriveActionSurface derives, and persists on first sight, the action surface
// for a device and an item's current decision surface (plan §5.14, §8): the
// intersection of the item's requested decisions with the device's capability
// contract. It is telemetry evidence and never widens the item's actions. A
// device that has registered no capability contract earns
// ErrCapabilityNotRegistered. The write, and its revision bump, is skipped when
// the surface's content-address digest is already recorded.
func (s *Service) DeriveActionSurface(
	ctx context.Context, deviceID domain.DeviceID, itemID domain.ItemID,
) (domain.DecisionActionSurface, error) {
	var out domain.DecisionActionSurface
	err := s.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := gateActiveDevice(ctx, tx, deviceID); err != nil {
			return err
		}
		contract, err := tx.GetDeviceCapabilityContract(ctx, deviceID)
		if errors.Is(err, store.ErrNotFound) {
			return ErrCapabilityNotRegistered
		}
		if err != nil {
			return err
		}
		// A gated item read backs the surface (404 for a missing item), and the
		// decision-surface row is authenticated against its own digest.
		if _, err := tx.GetAttentionItem(ctx, itemID); err != nil {
			return err
		}
		surface, err := tx.DecisionSurface(ctx, itemID)
		if err != nil {
			return err
		}
		derived, err := domain.DeriveDecisionActionSurface(deviceID, surface, contract)
		if err != nil {
			return err
		}
		if _, err := tx.GetDecisionActionSurface(ctx, derived.Digest); err == nil {
			out = derived
			return errReplay
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		stored, err := tx.PutDecisionActionSurface(ctx, derived, s.now().UTC())
		if err != nil {
			return err
		}
		out = stored
		return nil
	})
	if err != nil && !errors.Is(err, errReplay) {
		return domain.DecisionActionSurface{}, fmt.Errorf("derive action surface %q/%q: %w", deviceID, itemID, err)
	}
	return out, nil
}

// stampDecisionEvidence revalidates a referenced action surface, when the
// client sent one, and stamps the daemon-authored decision evidence onto the
// command: the accepted surface digest and the item's recommendation at
// acceptance. The client's referenced surface is never trusted — a foreign
// device, a different item, a stale capability contract, an unknown digest, or
// a selected action the surface does not offer is ErrActionSurfaceMismatch,
// while a stale item decision surface is the stale-version class. When neither
// a surface nor a recommendation is present, the command carries no evidence.
func (s *Service) stampDecisionEvidence(
	ctx context.Context, tx *store.WriteTx, command domain.Command,
	item domain.AttentionItem, digest *domain.Digest, snap store.Snapshot,
) (domain.Command, error) {
	var evidence domain.CommandDecisionEvidence
	if item.Recommendation != nil {
		action := item.Recommendation.Action
		source := item.Recommendation.Source
		evidence.RecommendedAction = &action
		evidence.RecommendationSource = &source
	}
	if digest != nil {
		surface, err := tx.GetDecisionActionSurface(ctx, *digest)
		if errors.Is(err, store.ErrNotFound) {
			return domain.Command{}, fmt.Errorf("submit command %q: %w", command.CommandID, ErrActionSurfaceMismatch)
		}
		if err != nil {
			return domain.Command{}, err
		}
		current, err := tx.DecisionSurface(ctx, item.ID)
		if err != nil {
			return domain.Command{}, err
		}
		// A surface bound to a superseded item decision surface is stale: the
		// client rendered an older epoch, so it re-renders the replacement.
		if surface.ItemDecisionSurfaceDigest != current.Digest {
			return domain.Command{}, &StaleVersionError{
				CommandID: command.CommandID, Replacement: item, Snapshot: snap,
			}
		}
		contract, err := tx.GetDeviceCapabilityContract(ctx, command.DeviceID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return domain.Command{}, fmt.Errorf("submit command %q: %w", command.CommandID, ErrActionSurfaceMismatch)
			}
			return domain.Command{}, err
		}
		if surface.DeviceID != command.DeviceID || surface.ItemID != item.ID ||
			surface.ClientCapabilityDigest != contract.Digest || !surface.Offers(command.Action) {
			return domain.Command{}, fmt.Errorf("submit command %q: %w", command.CommandID, ErrActionSurfaceMismatch)
		}
		evidence.ActionSurfaceDigest = *digest
	}
	if evidence.ActionSurfaceDigest == "" &&
		evidence.RecommendedAction == nil && evidence.RecommendationSource == nil {
		return command, nil
	}
	stamped, err := command.WithDecisionEvidence(evidence)
	if err != nil {
		return domain.Command{}, fmt.Errorf("submit command %q: %w", command.CommandID, err)
	}
	return stamped, nil
}
