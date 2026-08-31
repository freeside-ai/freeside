package signet

import (
	"context"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// stoppedNoticeID is the deterministic identity of the notice one accepted
// stop raises. Command-derived, not a singleton: item statuses are terminal,
// so after a resume resolves one notice the next stop needs a fresh identity,
// and the converge check below keeps concurrent stops from accumulating open
// duplicates.
func stoppedNoticeID(commandID string) domain.ItemID {
	return domain.ItemID("system-health-unattended-stopped-" + commandID)
}

// applyStopUnattended runs the stop transaction's steps inside the accepting
// Write (plan §4 stop_unattended; issue #319): conclude the decided item,
// append the durable stopped transition, and ensure exactly one open notice
// offers resume_unattended. All three commit atomically with the command
// record, so there is no window where the operator's stop "succeeded" while
// unattended admission still reads the old state — the failure the engine
// deliberately refused to offer this action under (its construction-time
// environment could not honour it).
func (s *Service) applyStopUnattended(
	ctx context.Context, tx *store.WriteTx,
	command domain.Command, item domain.AttentionItem, status domain.ItemStatus,
) error {
	now := s.now().UTC()
	if err := concludeItem(ctx, tx, item, status, now); err != nil {
		return err
	}
	commandID := command.CommandID
	if err := tx.RecordUnattendedOperationTransition(ctx, domain.UnattendedOperationTransition{
		State:     domain.UnattendedStopped,
		CommandID: &commandID,
		Reason: fmt.Sprintf("stop_unattended accepted on item %s from device %s",
			item.ID, command.DeviceID),
		OccurredAt: now,
	}); err != nil {
		return err
	}
	// Converge on an existing resume-offering notice: a second stop (another
	// open health item still offered the action) is a real decision worth
	// recording, but a second open notice would leave one still blocking
	// after the other resumed. The decided item is already concluded above,
	// so it cannot match its own scan.
	open, err := tx.ListOpenAttentionItems(ctx, domain.AttentionSystemHealth)
	if err != nil {
		return err
	}
	for _, existing := range open {
		if existing.Offers(domain.ActionResumeUnattended) {
			return nil
		}
	}
	posture := domain.HealthPostureBlocking
	subject := domain.Subject{Type: domain.SubjectSystem, ID: "daemon"}
	displayNames, err := tx.DisplayNamesFor(ctx, item.ProjectID, subject)
	if err != nil {
		return err
	}
	notice, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID:        stoppedNoticeID(command.CommandID),
		ProjectID: item.ProjectID,
		Subject:   subject,
		Type:      domain.AttentionSystemHealth,
		Priority:  domain.PriorityHigh,
		Reason: fmt.Sprintf(
			"Unattended operation is stopped by operator decision (item %s). "+
				"No new unattended work is admitted until resume_unattended is accepted.",
			item.ID),
		RequestedDecision: []domain.Action{domain.ActionResumeUnattended, domain.ActionAcknowledge},
		HealthDiagnostic: &domain.HealthDiagnostic{
			Code: "unattended_operation_stopped", Impairs: domain.ImpairedCapabilityUnattendedAdmission,
		},
		DisplayNames:      displayNames,
		ItemVersion:       1,
		InterruptionClass: domain.InterruptionExceptional,
		CreatedAt:         &now,
		Posture:           &posture,
		Status:            domain.StatusOpen,
	}, nil)
	if err != nil {
		return err
	}
	return tx.PutAttentionItem(ctx, notice)
}

// applyResumeUnattended concludes the stopped notice and appends the resumed
// transition in the accepting Write (issue #319). This command path is the
// only writer of "resumed": a restart alone never resumes. It re-checks
// nothing else — resume restores the operating state, and every subsequent
// admission still gates on its own merits in its own transaction.
func (s *Service) applyResumeUnattended(
	ctx context.Context, tx *store.WriteTx,
	command domain.Command, item domain.AttentionItem, status domain.ItemStatus,
) error {
	now := s.now().UTC()
	if err := concludeItem(ctx, tx, item, status, now); err != nil {
		return err
	}
	commandID := command.CommandID
	return tx.RecordUnattendedOperationTransition(ctx, domain.UnattendedOperationTransition{
		State:     domain.UnattendedResumed,
		CommandID: &commandID,
		Reason: fmt.Sprintf("resume_unattended accepted on item %s from device %s",
			item.ID, command.DeviceID),
		OccurredAt: now,
	})
}

// concludeItem applies a concluding decision's item side: the version bump,
// the terminal status, and the decision stamp, committed by the enclosing
// accepting transaction. Shared by the plain outcomeConcludes branch and the
// operating-state transactions so the concluding semantics (issue #171's
// exactly-once stamp among them) cannot drift between them.
func concludeItem(
	ctx context.Context, tx *store.WriteTx,
	item domain.AttentionItem, status domain.ItemStatus, decidedAt time.Time,
) error {
	next := item
	next.ItemVersion++
	next.Status = status
	// The concluding decision's accepted instant is the durable endpoint of
	// the open-to-decision metric (#171), stamped in the same transaction as
	// the command record and the status flip. Only concluding actions stamp,
	// and a replay never reaches a concluding branch, so the instant is set
	// exactly once — the item was open, so no earlier concluding command can
	// have stamped it.
	next, err := next.WithDecidedAt(decidedAt)
	if err != nil {
		return err
	}
	return tx.PutAttentionItem(ctx, next)
}
