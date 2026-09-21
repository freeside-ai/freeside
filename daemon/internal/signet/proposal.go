package signet

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

type proposalSnoozeReader interface {
	ProposalSnoozed(context.Context, domain.ItemID, time.Time) (bool, error)
}

func proposalSnoozed(
	ctx context.Context,
	tx proposalSnoozeReader,
	item domain.AttentionItem,
	now time.Time,
) (bool, error) {
	if item.Type != domain.AttentionTaskProposal && item.Type != domain.AttentionEffectProposal {
		return false, nil
	}
	return tx.ProposalSnoozed(ctx, item.ID, now)
}

func (s *Service) currentProposal(
	ctx context.Context,
	tx *store.WriteTx,
	itemID domain.ItemID,
) (domain.ProposalInstance, domain.EffectProposal, error) {
	instance, proposal, err := tx.ProposalForItem(ctx, itemID)
	return instance, proposal, err
}

// UnattendedInitiatorDeviceID attributes a decision the daemon makes on its own
// initiative (label-initiator auto_start, #659), rather than an operator device.
// It is a reserved, non-operator attribution: commands under it are created
// directly through the store's item/binding/offered-action gates, never the
// device-gated operator Submit path, so no active device is implied.
const UnattendedInitiatorDeviceID = domain.DeviceID("daemon-label-intake")

// StartTaskProposalUnattended records a daemon-attributed start decision on an
// open task_proposal, through the same decision ledger an operator start uses
// (GQ2): it creates a reserved-device start command and applies it, resolving
// the item and recording the effect_proposal_decisions row. It reports whether
// it recorded the start: an item that is no longer open -- an operator declined
// or a departure superseded it between the caller's gate and this call, or a
// prior pass already decided it -- records no second decision and returns
// started=false, so the caller launches only a start this call actually made. A
// decided-start item is relaunched for convergence by the reconciler's
// already-decided path, not here. It does not launch the run; the caller (the
// label-intake loop) executes SubmitSpecificationRun after the decision is
// durable, exactly as it does for an operator-decided start.
func (s *Service) StartTaskProposalUnattended(
	ctx context.Context, itemID domain.ItemID, commandID string,
) (started bool, err error) {
	if commandID == "" {
		return false, fmt.Errorf("start task proposal unattended: %w", domain.ErrEmptyID)
	}
	err = s.store.Write(ctx, func(tx *store.WriteTx) error {
		item, err := tx.GetAttentionItem(ctx, itemID)
		if err != nil {
			return fmt.Errorf("start task proposal unattended: %w", err)
		}
		if item.Type != domain.AttentionTaskProposal {
			return fmt.Errorf("start task proposal unattended: item %q is a %q, not a task proposal: %w",
				itemID, item.Type, domain.ErrParentKeyMismatch)
		}
		if item.Status != domain.StatusOpen {
			// Already decided (a prior auto_start replay, an operator start or
			// decline, or a departure supersession). Record no second decision and
			// report started=false: the caller must not launch a proposal this call
			// did not start, so an explicit decline can never become a run. A genuine
			// decided-start is relaunched by the reconciler's already-decided path.
			return nil
		}
		command, err := domain.NewCommand(domain.CommandInput{
			CommandID: commandID, DeviceID: UnattendedInitiatorDeviceID, ItemID: itemID,
			ItemVersion: item.ItemVersion, PRHeadSHA: item.PRHeadSHA,
			ArtifactDigests: item.ArtifactDigests, Action: domain.ActionStart,
		})
		if err != nil {
			return fmt.Errorf("start task proposal unattended: %w", err)
		}
		if err := tx.PutCommand(ctx, command); err != nil {
			return fmt.Errorf("start task proposal unattended: %w", err)
		}
		if err := s.applyStartProposal(ctx, tx, command, item, s.now().UTC()); err != nil {
			return err
		}
		started = true
		return nil
	})
	return started, err
}

func (s *Service) applyStartProposal(
	ctx context.Context,
	tx *store.WriteTx,
	command domain.Command,
	item domain.AttentionItem,
	now time.Time,
) error {
	instance, proposal, err := s.currentProposal(ctx, tx, item.ID)
	if err != nil {
		return err
	}
	digest := proposal.Digest
	if err := tx.RecordProposalDecision(ctx, instance.ID, command.CommandID, command.Action, &digest, now); err != nil {
		return err
	}
	return concludeItem(ctx, tx, item, domain.StatusResolved, now)
}

func (s *Service) applyDeclineProposal(
	ctx context.Context,
	tx *store.WriteTx,
	command domain.Command,
	item domain.AttentionItem,
	now time.Time,
) error {
	instance, _, err := s.currentProposal(ctx, tx, item.ID)
	if err != nil {
		return err
	}
	if err := tx.RecordProposalDecision(ctx, instance.ID, command.CommandID, command.Action, nil, now); err != nil {
		return err
	}
	return concludeItem(ctx, tx, item, domain.StatusDismissed, now)
}

func (s *Service) applySnoozeProposal(
	ctx context.Context,
	tx *store.WriteTx,
	command domain.Command,
	item domain.AttentionItem,
	now time.Time,
) error {
	instance, _, err := s.currentProposal(ctx, tx, item.ID)
	if err != nil {
		return err
	}
	until, err := time.Parse(time.RFC3339Nano, command.Message)
	if err != nil || until.Location() != time.UTC || until.Format(time.RFC3339Nano) != command.Message || !until.After(now) {
		return ErrInvalidProposalDecisionPayload
	}
	if err := tx.RecordProposalSnooze(ctx, instance.ID, command.CommandID, until, now); err != nil {
		return err
	}
	next := item
	next.ItemVersion++
	return tx.PutAttentionItem(ctx, next)
}

func (s *Service) applyStartProposalWithChanges(
	ctx context.Context,
	tx *store.WriteTx,
	command domain.Command,
	item domain.AttentionItem,
	now time.Time,
) error {
	instance, prior, err := s.currentProposal(ctx, tx, item.ID)
	if err != nil {
		return err
	}
	// A task proposal revises through start_with_changes and carries no merge; a
	// closure proposal revises through approve_with_changes, keeps the item's
	// prospective merge, and rebuilds the revised body in the store (which owns
	// the closable-source determination). The digest-change and re-gate checks
	// are shared below.
	var revised domain.EffectProposal
	var merge *domain.ProspectiveMerge
	switch prior.Kind {
	case domain.EffectTaskProposal:
		revised, err = s.reviseTaskProposal(ctx, tx, command, prior, item)
	case domain.EffectSourceIssueClosure:
		revised, err = tx.ReviseClosureProposal(ctx, prior, command.Message)
		if err == nil {
			merge, err = tx.ProspectiveMergeForItem(ctx, item.ID)
			if err == nil && merge == nil {
				err = fmt.Errorf("%w: closure item lacks a merge", ErrInvalidProposalDecisionPayload)
			}
		}
	default:
		return ErrInvalidProposalDecisionPayload
	}
	if err != nil {
		return err
	}
	if revised.Digest == prior.Digest {
		return ErrInvalidProposalDecisionPayload
	}
	if err := tx.PutProposalRevision(ctx, instance, prior, revised, command.CommandID, now); err != nil {
		return err
	}
	revisedInstance := instance
	revisedInstance.Proposal = revised
	artifact, err := revisedInstance.EvidenceArtifact()
	if err != nil {
		return err
	}
	if err := tx.PutArtifact(ctx, artifact); err != nil {
		return err
	}

	superseded := item
	superseded.ItemVersion++
	superseded.Status = domain.StatusSuperseded
	if err := tx.PutAttentionItem(ctx, superseded); err != nil {
		return err
	}
	replacement, err := proposalReplacementItem(item, instance, revised, artifact, command.CommandID, merge, now)
	if err != nil {
		return err
	}
	if err := tx.PutAttentionItem(ctx, replacement); err != nil {
		return err
	}
	if err := tx.BindProposalItem(ctx, replacement.ID, instance.ID, revised.Digest, merge); err != nil {
		return err
	}
	digest := revised.Digest
	return tx.RecordProposalDecision(ctx, instance.ID, command.CommandID, command.Action, &digest, now)
}

// reviseTaskProposal decodes the bounded task-proposal delta from the command,
// re-resolves the fixed subject, re-gates its scope against the current
// declaration, and constructs the revised proposal. The opaque subject handle
// stays bound to the reviewed proposal, never taken from the client.
func (s *Service) reviseTaskProposal(
	ctx context.Context,
	tx *store.WriteTx,
	command domain.Command,
	prior domain.EffectProposal,
	item domain.AttentionItem,
) (domain.EffectProposal, error) {
	var revision TaskProposalRevisionInput
	if err := strictjson.Decode(
		[]byte(command.Message), &revision, strictjson.RejectInvalidUTF8, domain.MaxEffectProposalBytes,
	); err != nil {
		return domain.EffectProposal{}, fmt.Errorf("%w: %w", ErrInvalidProposalDecisionPayload, err)
	}
	if err := revision.validate(); err != nil {
		return domain.EffectProposal{}, fmt.Errorf("%w: %w", ErrInvalidProposalDecisionPayload, err)
	}
	parameters := domain.TaskProposalParameters{
		SubjectHandle: prior.TaskProposal.SubjectHandle, Intent: revision.Intent,
		ExpectedCostUnits: revision.ExpectedCostUnits, Scope: revision.Scope,
	}
	declaration, policy, err := tx.ResolveProposalSubject(ctx, parameters.SubjectHandle)
	if err != nil {
		return domain.EffectProposal{}, err
	}
	if declaration.ProjectID != item.ProjectID {
		return domain.EffectProposal{}, domain.ErrTransitionCommandMismatch
	}
	if err := domain.GateTaskProposalScope(parameters.Scope, declaration); err != nil {
		return domain.EffectProposal{}, fmt.Errorf("%w: proposal scope differs from durable declaration: %w",
			ErrInvalidProposalDecisionPayload, err)
	}
	revised, err := domain.NewEffectProposal(domain.EffectTaskProposal, parameters, policy)
	if err != nil {
		return domain.EffectProposal{}, fmt.Errorf("%w: %w", ErrInvalidProposalDecisionPayload, err)
	}
	return revised, nil
}

// proposalReplacementItem builds the resolved replacement item a revise decision
// leaves behind. A task revision produces a task_proposal item with no merge; a
// closure revision produces an effect_proposal item carrying the same
// prospective merge and candidate head as the item it replaces (approve with
// changes flips only resolves, never the merge).
func proposalReplacementItem(
	priorItem domain.AttentionItem,
	instance domain.ProposalInstance,
	revised domain.EffectProposal,
	artifact domain.Artifact,
	commandID string,
	merge *domain.ProspectiveMerge,
	now time.Time,
) (domain.AttentionItem, error) {
	if commandID == "" {
		return domain.AttentionItem{}, errors.New("revised proposal is incomplete")
	}
	input := domain.AttentionItemInput{
		ID:                domain.ItemID(string(instance.ID) + "/revision/" + commandID),
		ProjectID:         priorItem.ProjectID,
		Subject:           priorItem.Subject,
		Priority:          priorItem.Priority,
		EvidenceSnapshot:  []domain.Artifact{artifact},
		ItemVersion:       priorItem.ItemVersion + 1,
		DisplayNames:      priorItem.DisplayNames,
		InterruptionClass: priorItem.InterruptionClass,
		Status:            domain.StatusResolved,
		PRHeadSHA:         priorItem.PRHeadSHA,
		CreatedAt:         &now,
	}
	switch revised.Kind {
	case domain.EffectTaskProposal:
		if revised.TaskProposal == nil || merge != nil {
			return domain.AttentionItem{}, errors.New("revised task proposal is incomplete")
		}
		input.Type = domain.AttentionTaskProposal
		input.Reason = "Start the revised daemon-enumerated work subject"
		input.RequestedDecision = []domain.Action{
			domain.ActionStart, domain.ActionStartWithChanges, domain.ActionDecline, domain.ActionSnooze,
		}
	case domain.EffectSourceIssueClosure:
		if revised.ClosureProposal == nil || merge == nil {
			return domain.AttentionItem{}, errors.New("revised closure proposal is incomplete")
		}
		input.Type = domain.AttentionEffectProposal
		input.Reason = "Decide the revised proposed effect on the source issue"
		input.RequestedDecision = []domain.Action{
			domain.ActionApprove, domain.ActionApproveWithChanges, domain.ActionDecline, domain.ActionSnooze,
		}
	default:
		return domain.AttentionItem{}, errors.New("revised proposal has an unregistered kind")
	}
	replacement, err := domain.NewAttentionItem(input, map[domain.Digest]bool{domain.EffectProposalRecipeDigest: true})
	if err != nil {
		return domain.AttentionItem{}, err
	}
	return replacement.WithDecidedAt(now)
}
