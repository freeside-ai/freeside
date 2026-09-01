package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// reconcileCapabilityRetries turns each accepted capability choice into one
// campaign attempt. The command id is persisted on that attempt, making the
// item-conclusion/allocation crash window replay-safe.
func (e *Engine) reconcileCapabilityRetries(ctx context.Context) (int, error) {
	var selected []domain.Command
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		selected, err = tx.ListCommandsForActions(ctx, domain.ActionRetryWithCapability)
		return err
	}); err != nil {
		return 0, err
	}
	created := 0
	seen := make(map[domain.ItemID]struct{}, len(selected))
	var joined error
	for _, candidate := range selected {
		if _, duplicate := seen[candidate.ItemID]; duplicate {
			continue
		}
		seen[candidate.ItemID] = struct{}{}
		made, err := e.reconcileCapabilityRetry(ctx, candidate)
		if err != nil {
			joined = errors.Join(joined, fmt.Errorf("command %q: %w", candidate.CommandID, err))
			continue
		}
		created += boolCount(made)
	}
	return created, joined
}

func (e *Engine) reconcileCapabilityRetry(
	ctx context.Context, candidate domain.Command,
) (bool, error) {
	var (
		item     domain.AttentionItem
		commands []domain.Command
		run      domain.Run
	)
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		item, err = tx.GetAttentionItemRecord(ctx, candidate.ItemID)
		if err != nil {
			return err
		}
		commands, err = tx.ListCommandsForItem(ctx, item.ID)
		if err != nil {
			return err
		}
		if item.Subject.RunID == nil {
			return domain.ErrParentKeyMismatch
		}
		run, err = tx.GetRun(ctx, *item.Subject.RunID)
		return err
	}); err != nil {
		return false, err
	}
	commands = slices.DeleteFunc(commands, func(command domain.Command) bool {
		return command.Action != domain.ActionRetryWithCapability ||
			!operatorFeedbackCommandMatchesItem(command, item)
	})
	if len(commands) == 0 {
		return false, nil
	}
	if len(commands) != 1 || item.Type != domain.AttentionExecutionFailure ||
		item.ExecutionFailure == nil || item.ExecutionFailure.Stage != domain.StageNameImplementation {
		return false, domain.ErrParentKeyMismatch
	}
	command := commands[0]
	manifestDigest := domain.Digest(command.Message)
	reason := "operator capability retry " + command.CommandID
	spec := ProductionReattemptSpec{
		ParentRunID:              run.ID,
		Reason:                   reason,
		OperatorCommandID:        command.CommandID,
		RetryOfInvocationID:      item.ExecutionFailure.InvocationID,
		CapabilityManifestDigest: manifestDigest,
	}
	var allocated bool
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		parent, err := tx.GetProductionAttemptByRun(ctx, run.ID)
		if err != nil {
			return err
		}
		latest, err := tx.LatestProductionAttempt(ctx, parent.CampaignID)
		if err != nil {
			return err
		}
		_, allocated, err = findOperatorRetryAttempt(
			ctx, tx, parent.CampaignID, latest.AttemptNumber, spec, reason,
		)
		return err
	}); err != nil {
		return false, err
	}
	if !allocated {
		manifest, err := e.capabilityManifestForRun(ctx, run, manifestDigest)
		if err != nil {
			return false, err
		}
		if !slices.ContainsFunc(item.ExecutionFailure.OfferedManifests,
			func(offer domain.CapabilityManifestOffer) bool {
				return offer == manifest.Offer()
			}) {
			return false, domain.ErrParentKeyMismatch
		}
		var failedAdmission domain.ExecutionAdmission
		if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			failedAdmission, err = tx.GetExecutionAdmissionRecord(
				ctx, item.ExecutionFailure.InvocationID)
			return err
		}); err != nil {
			return false, err
		}
		if failedAdmission.RunID != run.ID || failedAdmission.EgressProfile == manifest.EgressProfile {
			return false, domain.ErrParentKeyMismatch
		}
		spec.CapabilityManifestDigest = manifest.Digest
	}
	retry, err := ReattemptProductionRun(ctx, e.store, spec)
	if err != nil {
		return false, err
	}
	return retry.Created, nil
}
