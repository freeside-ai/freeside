package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// An operator's alternate-agent choice takes effect through one of two
// commands (issue #869). A retry_with_alternate_agent decision on an
// execution-failure card records the choice in the decision's transaction
// (AcceptAlternateAgentRetryTx), and the reconcile turns it into the
// campaign's next attempt, which cites the record so admission runs the
// chosen agent (attemptCard). A move_parked_task command on a task parked on
// an exhausted quota window records the choice and re-pins the task's lines
// to the chosen pool (MoveParkedTaskTx), and the park's resume gate releases
// the parked attempt (implementerMovedOffPool). Both re-derive the choice's
// rule against current state before they write.

// AcceptAlternateAgentRetryTx records the operator's alternate-agent choice
// on an execution-failure card, in the decision's transaction: the command's
// message names the choice (domain.ParseAlternateAgentChoice), whose
// enrollment must be one the card offers and still qualify now under the
// offer's pool and rule (requalifyAlternate), and whose model and effort the
// enrollment must serve (chosenSelection). Signet validates the command
// against the card before it calls this.
func (s AgentSelection) AcceptAlternateAgentRetryTx(
	ctx context.Context, tx *store.WriteTx, command domain.Command, item domain.AttentionItem, now time.Time,
) (domain.AlternateAgentRetry, error) {
	failure := item.ExecutionFailure
	if failure == nil || item.Subject.RunID == nil {
		return domain.AlternateAgentRetry{}, signet.ErrInvalidAlternateAgentDecisionPayload
	}
	choice, err := domain.ParseAlternateAgentChoice(command.Message)
	if err != nil {
		return domain.AlternateAgentRetry{}, fmt.Errorf("%w: %w", signet.ErrInvalidAlternateAgentDecisionPayload, err)
	}
	offer, ok := domain.OfferedEnrollment(item.AlternateAgents, choice.EnrollmentID)
	if !ok {
		return domain.AlternateAgentRetry{}, signet.ErrAlternateAgentNotOffered
	}
	run, err := tx.GetRun(ctx, *item.Subject.RunID)
	if err != nil {
		return domain.AlternateAgentRetry{}, err
	}
	if err := s.requalifyAlternate(ctx, &tx.ReadTx, alternateAgentFailure{
		taskID: run.TaskID, role: domain.RoleImplementer, cause: failure.Cause, invocationID: failure.InvocationID,
	}, offer, now); err != nil {
		return domain.AlternateAgentRetry{}, err
	}
	selection, err := s.chosenSelection(ctx, &tx.ReadTx, domain.RoleImplementer, choice)
	if err != nil {
		return domain.AlternateAgentRetry{}, err
	}
	record, err := domain.NewAlternateAgentRetry(domain.AlternateAgentRetry{
		TaskID: run.TaskID, Role: domain.RoleImplementer, FailedInvocationID: failure.InvocationID,
		ItemID: item.ID, CommandID: command.CommandID, Selection: &selection, Pool: offer.Pool,
		Qualification: offer.Qualification, RecordedAt: now,
	})
	if err != nil {
		return domain.AlternateAgentRetry{}, err
	}
	if err := tx.RecordAlternateAgentRetry(ctx, record); err != nil {
		return domain.AlternateAgentRetry{}, err
	}
	return record, nil
}

// reconcileAlternateAgentRetries turns each accepted alternate-agent choice
// into one campaign attempt that cites its record. As with a capability
// retry, the command id is persisted on that attempt, so the window between
// the item's conclusion and the allocation replays safely. The choice was
// re-derived at acceptance and is re-derived again at admission, so the
// allocation does not repeat it.
func (e *Engine) reconcileAlternateAgentRetries(ctx context.Context) (int, error) {
	var selected []domain.Command
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		selected, err = tx.ListCommandsForActions(ctx, domain.ActionRetryWithAlternateAgent)
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
		made, err := e.reconcileAlternateAgentRetry(ctx, candidate)
		if errors.Is(err, store.ErrTaskWorkflowClosed) {
			continue
		}
		if err != nil {
			joined = errors.Join(joined, fmt.Errorf("command %q: %w", candidate.CommandID, err))
			continue
		}
		created += boolCount(made)
	}
	return created, joined
}

func (e *Engine) reconcileAlternateAgentRetry(ctx context.Context, candidate domain.Command) (bool, error) {
	var (
		item     domain.AttentionItem
		commands []domain.Command
		record   domain.AlternateAgentRetry
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
		commands = slices.DeleteFunc(commands, func(command domain.Command) bool {
			return command.Action != domain.ActionRetryWithAlternateAgent ||
				!operatorFeedbackCommandMatchesItem(command, item)
		})
		if len(commands) != 1 {
			return nil
		}
		record, err = tx.GetAlternateAgentRetryByCommand(ctx, commands[0].CommandID)
		return err
	}); err != nil {
		return false, err
	}
	if len(commands) == 0 {
		return false, nil
	}
	if len(commands) != 1 || item.Type != domain.AttentionExecutionFailure || item.ExecutionFailure == nil ||
		item.ExecutionFailure.Stage != domain.StageNameImplementation || item.Subject.RunID == nil ||
		record.ItemID != item.ID {
		return false, domain.ErrParentKeyMismatch
	}
	command := commands[0]
	retry, err := ReattemptProductionRun(ctx, e.store, ProductionReattemptSpec{
		ParentRunID:           *item.Subject.RunID,
		Reason:                "operator alternate agent retry " + command.CommandID,
		OperatorCommandID:     command.CommandID,
		RetryOfInvocationID:   item.ExecutionFailure.InvocationID,
		AlternateAgentRetryID: record.ID,
	})
	if err != nil {
		return false, err
	}
	return retry.Created, nil
}

// MoveParkedTaskTx moves a task parked on its implementer's exhausted quota
// window to another enrollment, in the command's transaction. The request's
// choice must name an enrollment the park offers now (parkedTaskOffers),
// which re-derives its rule, with any model and effort it names one that
// enrollment serves (chosenSelection). The move records the choice, then re-pins the task's
// lines to the chosen enrollment as the router would (moveLines). It clears
// the parked attempt's hold row; the park's gate reads the moved line, not
// the row, to let the attempt start.
//
// The lines carry router-shaped provenance under the policy in effect, so
// requireRoutedPoolOpen holds them on the new pool's windows like a routed
// line, and admission anchors them to the record and its command's receipt.
// The command's receipt is recorded after this returns, in the same
// transaction, so a line never commits without it.
func (s AgentSelection) MoveParkedTaskTx(
	ctx context.Context, tx *store.WriteTx, request domain.ParkedTaskMoveRequest, now time.Time,
) (domain.ParkedTaskMoveReceipt, error) {
	if request.Choice == nil {
		return domain.ParkedTaskMoveReceipt{}, signet.ErrInvalidOperatorCommandPayload
	}
	parked, refused, found, err := parkedRun(ctx, &tx.ReadTx, request.TaskID)
	if err != nil {
		return domain.ParkedTaskMoveReceipt{}, err
	}
	if !found {
		return domain.ParkedTaskMoveReceipt{}, fmt.Errorf("task %s: %w", request.TaskID, signet.ErrTaskNotParked)
	}
	offers, err := parkedTaskOffers(ctx, &tx.ReadTx, &s, request.TaskID, refused, now)
	if err != nil {
		return domain.ParkedTaskMoveReceipt{}, err
	}
	offer, ok := domain.OfferedEnrollment(offers, request.Choice.EnrollmentID)
	if !ok {
		return domain.ParkedTaskMoveReceipt{}, signet.ErrAlternateAgentNotQualified
	}
	selection, err := s.chosenSelection(ctx, &tx.ReadTx, domain.RoleImplementer, *request.Choice)
	if err != nil {
		return domain.ParkedTaskMoveReceipt{}, err
	}
	policy, found, err := policyInEffect(ctx, &tx.ReadTx, now)
	if err != nil {
		return domain.ParkedTaskMoveReceipt{}, err
	}
	if !found {
		// parkedTaskOffers draws only on a policy in effect.
		return domain.ParkedTaskMoveReceipt{}, signet.ErrAlternateAgentNotQualified
	}
	record, err := domain.NewAlternateAgentRetry(domain.AlternateAgentRetry{
		TaskID: request.TaskID, Role: domain.RoleImplementer, FailedInvocationID: refused,
		CommandID: request.CommandID, Selection: &selection, Pool: offer.Pool,
		Qualification: offer.Qualification, RecordedAt: now,
	})
	if err != nil {
		return domain.ParkedTaskMoveReceipt{}, err
	}
	if err := tx.RecordAlternateAgentRetry(ctx, record); err != nil {
		return domain.ParkedTaskMoveReceipt{}, err
	}
	passedOver, err := s.movedPastMark(ctx, &tx.ReadTx, policy, parked.ID, refused)
	if err != nil {
		return domain.ParkedTaskMoveReceipt{}, err
	}
	inputs, err := s.moveLines(ctx, &tx.ReadTx, policy, record, passedOver)
	if err != nil {
		return domain.ParkedTaskMoveReceipt{}, err
	}
	lines, err := tx.RecordAlternateAgentMoveLines(ctx, record, inputs)
	if err != nil {
		return domain.ParkedTaskMoveReceipt{}, err
	}
	if err := tx.ClearRunHoldCause(ctx, parked.ID, domain.HoldQuotaWindowExhausted); err != nil {
		return domain.ParkedTaskMoveReceipt{}, err
	}
	return domain.ParkedTaskMoveReceipt{ParkedTaskMoveRequest: request, Retry: record, Lines: lines}, nil
}

// movedPastMark names the enrollment a move leaves and the mark it was parked
// on: the enrollment the refused invocation's admission bound
// (failedBinding), with the parked run's hold's pool and mark. It is nil when
// the run holds no quota window or when the policy orders that enrollment for
// no role, since a move line's anchor holds what it passed over to the policy
// it cites (anchorMovePolicy).
func (s AgentSelection) movedPastMark(
	ctx context.Context, tx *store.ReadTx, policy domain.RoutingPolicy, runID domain.RunID,
	refused domain.InvocationID,
) (*domain.TaskLinePassedOverMark, error) {
	hold, held, err := tx.GetRunHold(ctx, runID)
	if err != nil || !held || hold.QuotaWindow == nil {
		return nil, err
	}
	binding, found, err := failedBinding(ctx, tx, refused)
	if err != nil || !found {
		return nil, err
	}
	if !slices.ContainsFunc(policy.Roles, func(role domain.RoutingPolicyRole) bool {
		return slices.Contains(roleOrder(s.Tree, policy, role.Role), binding.EnrollmentID)
	}) {
		return nil, nil
	}
	mark := hold.QuotaWindow
	return &domain.TaskLinePassedOverMark{
		Enrollment: binding.EnrollmentID, Pool: mark.Pool, Window: mark.Window, Source: mark.Source,
		MarkedAt: mark.SetAt.UTC(), ResetsAt: utcPtr(mark.ResetsAt),
	}, nil
}

// moveLines derives a move's lines: the record's role to the record's
// selection, then, in the policy's role order, each other role the policy
// orders the record's enrollment for (roleOrder) to the role's lineup model
// and effort on that enrollment (plan §4, revision 106). It
// skips a role whose newest line an operator set (or cleared), and a role
// whose lineup tuple the enrollment does not serve (lineupServes), which
// keeps its line. The provenance cites the policy in effect at the
// policy-order tier on the record's pool, and names passedOver, the
// enrollment the move left (movedPastMark), as the router names the
// enrollment it passed over.
//
// An escalated specifier (its task's triage record escalated and its newest
// line is the triage line or one with a provenance) moves to the escalated
// line the lineup names for the record's enrollment, or its standard line
// when the lineup names none, so the move keeps the tier where it can.
func (s AgentSelection) moveLines(
	ctx context.Context, tx *store.ReadTx, policy domain.RoutingPolicy, record domain.AlternateAgentRetry,
	passedOver *domain.TaskLinePassedOverMark,
) ([]domain.TaskLineInput, error) {
	heads, err := tx.TaskLineHeads(ctx, record.TaskID)
	if err != nil {
		return nil, err
	}
	choices := []domain.TaskLineChoice{{Role: record.Role, Selection: record.Selection}}
	others, err := s.movedRoleLines(ctx, tx, policy, record, heads)
	if err != nil {
		return nil, err
	}
	choices = append(choices, others...)
	provenance := &domain.TaskLineProvenance{
		PolicyRevision: policy.Revision, Tier: domain.TaskLineSelectionPolicyOrder, Pool: record.Pool,
		RankedBy: []domain.TaskLineRankingReading{}, PassedOver: passedOver,
	}
	inputs := make([]domain.TaskLineInput, 0, len(choices))
	for _, choice := range choices {
		inputs = append(inputs, domain.TaskLineInput{
			TaskID: record.TaskID, Role: choice.Role, Selection: choice.Selection,
			Source: domain.TaskLineSourceAlternateAgentMove, SetBy: string(record.ID), Provenance: provenance,
		})
	}
	return inputs, nil
}

// movedRoleLines is moveLines' re-pin of the roles other than the record's.
// A lineup that does not resolve re-pins none.
func (s AgentSelection) movedRoleLines(
	ctx context.Context, tx *store.ReadTx, policy domain.RoutingPolicy, record domain.AlternateAgentRetry,
	heads map[domain.RoleName]domain.TaskLine,
) ([]domain.TaskLineChoice, error) {
	lineup, err := s.Tree.ResolveLineup()
	if err != nil {
		return nil, nil
	}
	enrollmentID := record.Selection.EnrollmentID
	enrollment, err := tx.GetClientEnrollment(ctx, enrollmentID)
	if err != nil {
		return nil, err
	}
	catalog, err := readCatalog(ctx, tx, enrollmentID)
	if err != nil {
		return nil, err
	}
	escalated := false
	if head, ok := heads[domain.RoleSpecifier]; ok && record.Role != domain.RoleSpecifier &&
		(head.Source == domain.TaskLineSourceTriage || head.Source.CarriesProvenance()) {
		if escalated, err = taskEscalated(ctx, tx, record.TaskID); err != nil {
			return nil, err
		}
	}
	var choices []domain.TaskLineChoice
	for _, role := range policy.Roles {
		head, held := heads[role.Role]
		if role.Role == record.Role || held && !head.Source.CarriesProvenance() && head.Source != domain.TaskLineSourceTriage ||
			!slices.Contains(roleOrder(s.Tree, policy, role.Role), enrollmentID) {
			continue
		}
		line, ok := lineup.Line(role.Role)
		if !ok {
			continue
		}
		if counterpart, ok := lineup.EscalatedCounterpart(role.Role, string(enrollmentID)); ok &&
			role.Role == domain.RoleSpecifier && escalated {
			line = counterpart
		}
		client, err := lineupClient(ctx, tx, line)
		if err != nil {
			return nil, err
		}
		if !lineupServes(enrollment, catalog, line, client) {
			continue
		}
		selection := domain.TaskLineSelection{
			EnrollmentID: enrollmentID, ModelID: line.Names.Model, Effort: line.Names.Effort,
		}
		choices = append(choices, domain.TaskLineChoice{Role: role.Role, Selection: &selection})
	}
	return choices, nil
}

// parkedRun returns the task's attempt that is parked on its implementer's
// quota refusal and has not started, with the refused invocation its park
// answers. found is false when the task has none: the task was stopped, no
// attempt carries the park's reason, the parked attempt is no longer its
// campaign's latest or its workflow closed, its implementation invocation
// was already admitted, or its parent's terminal names no quota refusal.
func parkedRun(
	ctx context.Context, tx *store.ReadTx, taskID domain.TaskID,
) (domain.Run, domain.InvocationID, bool, error) {
	// A stop committed before its workflow closes leaves nothing to move:
	// the parked attempt would never launch (productionAttemptParkable).
	task, err := tx.GetTask(ctx, taskID)
	if err != nil || task.Cancellation != nil {
		return domain.Run{}, "", false, err
	}
	runIDs, err := tx.TaskRunIDs(ctx, taskID)
	if err != nil {
		return domain.Run{}, "", false, err
	}
	for _, runID := range runIDs {
		run, err := tx.GetRun(ctx, runID)
		if err != nil {
			return domain.Run{}, "", false, err
		}
		if run.AttemptReason != quotaParkReattemptReason || run.CampaignID == "" {
			continue
		}
		latest, err := tx.LatestProductionAttempt(ctx, run.CampaignID)
		if err != nil {
			return domain.Run{}, "", false, err
		}
		if latest.ImplementationRunID != run.ID {
			continue
		}
		if err := tx.RequireRunWorkflowOpen(ctx, run.ID); errors.Is(err, store.ErrTaskWorkflowClosed) {
			continue
		} else if err != nil {
			return domain.Run{}, "", false, err
		}
		if _, err := tx.GetExecutionAdmissionRecord(ctx, productionInvocationID(run.ID)); err == nil {
			continue
		} else if !errors.Is(err, store.ErrNotFound) {
			return domain.Run{}, "", false, err
		}
		parent, err := tx.GetRun(ctx, run.ParentRunID)
		if err != nil {
			return domain.Run{}, "", false, err
		}
		refused := productionInvocationID(parent.ID)
		entry, err := tx.GetInbox(ctx, string(refused))
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return domain.Run{}, "", false, err
		}
		terminal, err := decodeProductionTerminal(entry, parent)
		if err != nil {
			return domain.Run{}, "", false, err
		}
		if terminal.QuotaRefusal == nil {
			continue
		}
		return run, refused, true, nil
	}
	return domain.Run{}, "", false, nil
}
