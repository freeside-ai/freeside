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

// stoppedNoticeDiagnostic is the typed finding code on the notice an operator
// stop raises. The sync projection recognises the notice by this code and its
// offered action, never by the item-ID convention above.
const stoppedNoticeDiagnostic = "unattended_operation_stopped"

// unattendedOperationSnapshot projects the gate's verdict for sync. The
// operator stop comes first, bound to its open resume notice; that notice is
// itself a blocking item, so it is reported once, as the operator stop,
// rather than a second time as an unrelated finding. Every other blocking
// item follows in the gate's item-id order.
func unattendedOperationSnapshot(gate domain.UnattendedOperationGate) UnattendedOperationSnapshot {
	out := UnattendedOperationSnapshot{
		Admission: domain.UnattendedAdmissionOpen,
		Stops:     []UnattendedStop{},
	}
	var notice *domain.ItemID
	if stop := gate.OperatorStop; stop != nil {
		for _, blocking := range gate.Blocking {
			if isStoppedNotice(blocking.Item) {
				notice = &blocking.Item.ID
				break
			}
		}
		out.Stops = append(out.Stops, UnattendedStop{
			Kind:      domain.UnattendedStopOperator,
			ItemID:    notice,
			CommandID: stop.CommandID,
			Since:     &stop.OccurredAt,
		})
	}
	for _, blocking := range gate.Blocking {
		if notice != nil && blocking.Item.ID == *notice {
			continue
		}
		out.Stops = append(out.Stops, UnattendedStop{
			Kind:   domain.UnattendedStopBlockingSystemHealth,
			ItemID: &blocking.Item.ID,
			Since:  blocking.Item.CreatedAt,
		})
	}
	if len(out.Stops) > 0 {
		out.Admission = domain.UnattendedAdmissionStopped
	}
	return out
}

func isStoppedNotice(item domain.AttentionItem) bool {
	return item.HealthDiagnostic != nil &&
		item.HealthDiagnostic.Code == stoppedNoticeDiagnostic &&
		item.Offers(domain.ActionResumeUnattended)
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
	if err := concludeItem(ctx, tx, command, item, status, now); err != nil {
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
			Code: stoppedNoticeDiagnostic, Impairs: domain.ImpairedCapabilityUnattendedAdmission,
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
	if err := concludeItem(ctx, tx, command, item, status, now); err != nil {
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
// the terminal status, the decision stamp, and the conclusion naming the
// command, committed by the enclosing accepting transaction. Shared by the
// plain outcomeConcludes branch and the operating-state transactions so the
// concluding semantics (issue #171's exactly-once stamp among them) cannot
// drift between them.
func concludeItem(
	ctx context.Context, tx *store.WriteTx, command domain.Command,
	item domain.AttentionItem, status domain.ItemStatus, decidedAt time.Time,
) error {
	decision, err := commandDecision(ctx, tx, command)
	if err != nil {
		return err
	}
	// The concluding decision's accepted instant is the durable endpoint of
	// the open-to-decision metric (#171), stamped in the same transaction as
	// the command record and the status flip. Only concluding actions stamp,
	// and a replay never reaches a concluding branch, so the instant is set
	// exactly once — the item was open, so no earlier concluding command can
	// have stamped it.
	next, err := item.ConcludeByCommand(status, decidedAt, decision)
	if err != nil {
		return err
	}
	next.ItemVersion++
	return tx.PutAttentionItem(ctx, next)
}

// commandDecision records command as the decision that concludes its item
// (#2097). The device's display name is read in the accepting transaction,
// so the record keeps the name the operator saw; a device the command was
// authenticated as but that has no row is an invariant breach and fails the
// command rather than recording a blank name. The per-finding routes come
// from the canonical message decisionMessage wrote for the two
// route-carrying actions.
func commandDecision(
	ctx context.Context, tx *store.WriteTx, command domain.Command,
) (domain.ConclusionDecision, error) {
	// The reserved unattended initiator has no device row to read a name
	// from; every other device must exist, and a missing one fails the
	// command rather than recording a blank name.
	name := unattendedInitiatorDisplayName
	if command.DeviceID != UnattendedInitiatorDeviceID {
		device, err := tx.GetDevice(ctx, command.DeviceID)
		if err != nil {
			return domain.ConclusionDecision{}, fmt.Errorf("conclude by command %q: device %q: %w",
				command.CommandID, command.DeviceID, err)
		}
		name = device.DisplayName
	}
	decision := domain.ConclusionDecision{
		CommandID:         command.CommandID,
		Action:            command.Action,
		DeviceID:          command.DeviceID,
		DeviceDisplayName: name,
		AnswerRoute:       command.AnswerRoute,
	}
	if command.Action == domain.ActionAdjudicateFindings || command.Action == domain.ActionChooseAlternativeRoute {
		routes, err := concludedRoutes(command)
		if err != nil {
			return domain.ConclusionDecision{}, fmt.Errorf("conclude by command %q: %w", command.CommandID, err)
		}
		decision.FindingRoutes = routes
	}
	return decision, nil
}

// concludedRoutes reads the routes a finding decision carried. The conclusion
// keeps each route and drops any reason: the reason travels in the
// adjudication artifact and the reply, not on the closed card.
func concludedRoutes(command domain.Command) ([]domain.ConcludedRoute, error) {
	if command.Action == domain.ActionAdjudicateFindings {
		choices, err := decodeFindingRoutes(command.Message)
		if err != nil {
			return nil, err
		}
		routes := make([]domain.ConcludedRoute, len(choices))
		for index, choice := range choices {
			routes[index] = domain.ConcludedRoute{FindingID: choice.FindingID, Route: choice.Route}
		}
		return routes, nil
	}
	choices, err := decodeAlternativeChoices(command.Message)
	if err != nil {
		return nil, err
	}
	routes := make([]domain.ConcludedRoute, len(choices))
	for index, choice := range choices {
		routes[index] = domain.ConcludedRoute{FindingID: choice.FindingID, Route: choice.Route}
	}
	return routes, nil
}
