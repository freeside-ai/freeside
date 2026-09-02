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
	if item.Type != domain.AttentionRunProposal {
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

// StartRunProposalUnattended records a daemon-attributed start decision on an
// open run_proposal, through the same decision ledger an operator start uses
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
func (s *Service) StartRunProposalUnattended(
	ctx context.Context, itemID domain.ItemID, commandID string,
) (started bool, err error) {
	if commandID == "" {
		return false, fmt.Errorf("start run proposal unattended: %w", domain.ErrEmptyID)
	}
	err = s.store.Write(ctx, func(tx *store.WriteTx) error {
		item, err := tx.GetAttentionItem(ctx, itemID)
		if err != nil {
			return fmt.Errorf("start run proposal unattended: %w", err)
		}
		if item.Type != domain.AttentionRunProposal {
			return fmt.Errorf("start run proposal unattended: item %q is a %q, not a run proposal: %w",
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
			return fmt.Errorf("start run proposal unattended: %w", err)
		}
		if err := tx.PutCommand(ctx, command); err != nil {
			return fmt.Errorf("start run proposal unattended: %w", err)
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
	var revision RunProposalRevisionInput
	if err := strictjson.Decode(
		[]byte(command.Message), &revision, strictjson.RejectInvalidUTF8, domain.MaxEffectProposalBytes,
	); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidProposalDecisionPayload, err)
	}
	if err := revision.validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidProposalDecisionPayload, err)
	}
	parameters := domain.RunProposalParameters{
		SubjectHandle: prior.RunProposal.SubjectHandle, Intent: revision.Intent,
		ExpectedCostUnits: revision.ExpectedCostUnits, Scope: revision.Scope,
	}
	declaration, policy, err := tx.ResolveProposalSubject(ctx, parameters.SubjectHandle)
	if err != nil {
		return err
	}
	if declaration.ProjectID != item.ProjectID {
		return domain.ErrTransitionCommandMismatch
	}
	if err := domain.GateRunProposalScope(parameters.Scope, declaration); err != nil {
		return fmt.Errorf("%w: proposal scope differs from durable declaration: %w",
			ErrInvalidProposalDecisionPayload, err)
	}
	revised, err := domain.NewEffectProposal(domain.EffectRunProposal, parameters, policy)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidProposalDecisionPayload, err)
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
	replacement, err := proposalReplacementItem(item, instance, revised, artifact, command.CommandID, now)
	if err != nil {
		return err
	}
	if err := tx.PutAttentionItem(ctx, replacement); err != nil {
		return err
	}
	if err := tx.BindProposalItem(ctx, replacement.ID, instance.ID, revised.Digest); err != nil {
		return err
	}
	digest := revised.Digest
	return tx.RecordProposalDecision(ctx, instance.ID, command.CommandID, command.Action, &digest, now)
}

func proposalReplacementItem(
	priorItem domain.AttentionItem,
	instance domain.ProposalInstance,
	revised domain.EffectProposal,
	artifact domain.Artifact,
	commandID string,
	now time.Time,
) (domain.AttentionItem, error) {
	if revised.RunProposal == nil || commandID == "" {
		return domain.AttentionItem{}, errors.New("revised run proposal is incomplete")
	}
	replacement, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID:        domain.ItemID(string(instance.ID) + "/revision/" + commandID),
		ProjectID: priorItem.ProjectID, Subject: priorItem.Subject,
		Type: domain.AttentionRunProposal, Priority: priorItem.Priority,
		Reason: "Start the revised daemon-enumerated work subject",
		RequestedDecision: []domain.Action{
			domain.ActionStart, domain.ActionStartWithChanges, domain.ActionDecline, domain.ActionSnooze,
		},
		EvidenceSnapshot: []domain.Artifact{artifact}, ItemVersion: priorItem.ItemVersion + 1,
		DisplayNames:      priorItem.DisplayNames,
		InterruptionClass: priorItem.InterruptionClass, Status: domain.StatusResolved,
		CreatedAt: &now,
	}, map[domain.Digest]bool{domain.EffectProposalRecipeDigest: true})
	if err != nil {
		return domain.AttentionItem{}, err
	}
	return replacement.WithDecidedAt(now)
}
