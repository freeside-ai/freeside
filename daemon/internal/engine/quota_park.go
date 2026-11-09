package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// A provider quota refusal parks the task on its task line until the refused
// window resets (plan §4, §5.4, revision 103). The refusal marks the pool's
// window exhausted and allocates the role's next attempt on the same
// campaign, in the transaction that records the refused terminal. The next
// attempt carries quotaParkReattemptReason, and dispatch holds it while the
// pool's record still marks the window its parent's terminal names.
//
// The gate reads only durable facts: the run's attempt reason, its parent's
// stored terminal, and the pool's record. The hold row is the operator's view
// of it, never the gate, because a milestone or another hold replaces that
// row.

// quotaParkReattemptReason is the attempt reason of a parked attempt. It
// marks the attempt as one dispatch holds until the window resets.
const quotaParkReattemptReason = "Provider quota window exhausted; resume after its reset."

// quotaRefusalPool returns the usage pool a refused invocation ran on: the
// pool its admitted identity meters. An admission with no identity, or one
// whose identity is gone, has no pool, so nothing can mark it.
func quotaRefusalPool(
	ctx context.Context, tx *store.ReadTx, invocationID domain.InvocationID,
) (domain.UsagePoolID, bool, error) {
	_, pool, found, err := admittedPool(ctx, tx, invocationID)
	return pool, found, err
}

// admittedPool returns an invocation's admission and the usage pool its
// admitted identity meters. found is false when the invocation has no
// admission, the admission no identity, or the identity is gone.
func admittedPool(
	ctx context.Context, tx *store.ReadTx, invocationID domain.InvocationID,
) (admission domain.ExecutionAdmission, pool domain.UsagePoolID, found bool, err error) {
	admission, err = tx.GetExecutionAdmissionRecord(ctx, invocationID)
	if errors.Is(err, store.ErrNotFound) {
		return domain.ExecutionAdmission{}, "", false, nil
	}
	if err != nil {
		return domain.ExecutionAdmission{}, "", false, err
	}
	if admission.AuthIdentityID == nil {
		return admission, "", false, nil
	}
	identity, err := tx.GetAuthIdentity(ctx, *admission.AuthIdentityID)
	if errors.Is(err, store.ErrNotFound) {
		return admission, "", false, nil
	}
	if err != nil {
		return domain.ExecutionAdmission{}, "", false, err
	}
	return admission, identity.EffectiveUsagePool(), true, nil
}

// quotaParkHold marks the refused window and returns the hold its next
// attempt waits under, or nil when the terminal keeps today's failure card.
// That is: no quota refusal on the run's root implementation invocation, no
// pool, no reset ahead from the refusal or the window's reading, no
// enrollment to own the mark, or no attempt to park (a closed or stopped
// task, a run outside a campaign or not its latest attempt). A refused
// remediation round or operator-feedback invocation keeps the card: a fresh
// attempt would discard the run's reviewed work, and the resume gate reads
// only the root's terminal. The mark is written
// whenever a reset and an enrollment exist, since it is a fact about the pool
// whatever happens to this task.
//
// Under agent selection the hold carries the agents the operator may move the
// task to (parkedTaskOffers).
func quotaParkHold(
	ctx context.Context, tx *store.WriteTx, selection *AgentSelection, run domain.Run,
	terminal productionTerminalRecord, now time.Time,
) (*domain.QuotaWindowHold, error) {
	refusal := terminal.QuotaRefusal
	if terminal.Status != exec.StatusFailed || terminal.Cause != domain.FailureCauseQuota || refusal == nil ||
		terminal.InvocationID != productionInvocationID(run.ID) {
		return nil, nil
	}
	pool, found, err := quotaRefusalPool(ctx, &tx.ReadTx, terminal.InvocationID)
	if err != nil || !found {
		return nil, err
	}
	record, err := tx.GetPoolUsageObservation(ctx, pool)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	resetsAt := refusal.ResetsAt
	if resetsAt == nil {
		resetsAt = record.ReadingReset(refusal.Window, now)
	}
	if resetsAt == nil || !resetsAt.After(now) {
		return nil, nil
	}
	err = tx.RecordWindowRefusalMark(ctx, pool, domain.RefusalMark{
		Window: refusal.Window, InvocationID: terminal.InvocationID, ResetsAt: resetsAt, SetAt: now,
	}, now)
	if errors.Is(err, store.ErrNotFound) {
		// The pool has no enrollment to own a record (DefaultPoolCollector),
		// so nothing could clear a park.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if record, err = tx.GetPoolUsageObservation(ctx, pool); err != nil {
		return nil, err
	}
	// The mark the record now derives may be a later one than this refusal.
	mark, marked := record.ExhaustedWindowMark(refusal.Window, now)
	if !marked || mark.ResetsAt == nil {
		return nil, nil
	}
	if parkable, err := productionAttemptParkable(ctx, tx, run); err != nil || !parkable {
		return nil, err
	}
	offers, err := parkedTaskOffers(ctx, &tx.ReadTx, selection, run.TaskID, terminal.InvocationID, now)
	if err != nil {
		return nil, err
	}
	return &domain.QuotaWindowHold{Pool: pool, ExhaustedMark: mark, AlternateAgents: offers}, nil
}

// parkedTaskOffers returns the enrollments a task parked on its
// implementer's quota refusal may move to, nil when there is no agent
// selection or none qualifies. Only enrollments the routing policy in effect
// orders for the implementer are offered: a move re-pins the task's lines as
// the router would (MoveParkedTaskTx).
func parkedTaskOffers(
	ctx context.Context, tx *store.ReadTx, selection *AgentSelection, taskID domain.TaskID,
	refused domain.InvocationID, now time.Time,
) ([]domain.AlternateAgentOffer, error) {
	if selection == nil {
		return nil, nil
	}
	return selection.alternateAgentOffers(ctx, tx, parkedTaskFailure(taskID, refused), now)
}

// parkedTaskFailure is the failure a parked task's offers answer: its
// implementer's quota refusal on the parked attempt's parent.
func parkedTaskFailure(taskID domain.TaskID, refused domain.InvocationID) alternateAgentFailure {
	return alternateAgentFailure{
		taskID: taskID, role: domain.RoleImplementer, cause: domain.FailureCauseQuota,
		invocationID: refused, policyOnly: true,
	}
}

// productionAttemptParkable reports whether reattemptProductionRunTx can
// allocate the run's next attempt. It answers the refusals that would
// otherwise fail the terminal's whole transaction: the allocation writes
// before it checks the task's stop, and it refuses a parent that is not the
// campaign's latest attempt.
func productionAttemptParkable(ctx context.Context, tx *store.WriteTx, run domain.Run) (bool, error) {
	if run.CampaignID == "" || run.TaskID == "" {
		return false, nil
	}
	latest, err := tx.LatestProductionAttempt(ctx, run.CampaignID)
	if err != nil {
		return false, err
	}
	if latest.ImplementationRunID != run.ID {
		return false, nil
	}
	if err := tx.RequireRunWorkflowOpen(ctx, run.ID); errors.Is(err, store.ErrTaskWorkflowClosed) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	task, err := tx.GetTask(ctx, run.TaskID)
	if err != nil {
		return false, err
	}
	return task.Cancellation == nil, nil
}

// parkProductionAttempt allocates the refused run's next attempt and records
// its hold. It runs after the refused terminal's milestone, which the
// allocation reads to know the parent run is final.
func parkProductionAttempt(
	ctx context.Context, tx *store.WriteTx, run domain.Run, hold domain.QuotaWindowHold, now time.Time,
) error {
	next, err := reattemptProductionRunTx(ctx, tx, ProductionReattemptSpec{
		ParentRunID: run.ID, Reason: quotaParkReattemptReason,
	})
	if err != nil {
		return fmt.Errorf("park run %q: %w", run.ID, err)
	}
	return recordRunHold(ctx, tx, next.Run.Run.ID, next.Run.InvocationID,
		domain.HoldQuotaWindowExhausted, holdDetail{quotaWindow: &hold}, now)
}

// requireQuotaWindowReset holds a parked attempt's implementation invocation
// while the pool's record still marks the window its parent was refused on.
// Any other run or invocation passes. A window the record no longer marks, or
// marks with no reset, lets the attempt start: the provider decides then. So
// does a parent with no refused window or pool to wait on, as when an
// operator's reattempt happens to carry the park's reason: the gate holds
// only on a refusal it can read, and never stops dispatch over one it can't.
//
// Under agent selection, an implementer line that now resolves to another
// pool also lets it start: the operator moved the task off the refused pool
// (moveParkedTask) or changed its line, and the window no longer bounds it. A
// line that resolves to no pool keeps the hold. The hold it returns carries
// the offers current at now, so the hold row a client reads stays fresh.
func (e *Engine) requireQuotaWindowReset(
	ctx context.Context, run domain.Run, invocationID domain.InvocationID,
) error {
	if run.AttemptReason != quotaParkReattemptReason || invocationID != productionInvocationID(run.ID) {
		return nil
	}
	now := time.Now().UTC()
	return e.store.Read(ctx, func(tx *store.ReadTx) error {
		parent, err := tx.GetRun(ctx, run.ParentRunID)
		if err != nil {
			return err
		}
		parentInvocation := productionInvocationID(parent.ID)
		entry, err := tx.GetInbox(ctx, string(parentInvocation))
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("parked run %q parent terminal: %w", run.ID, err)
		}
		terminal, err := decodeProductionTerminal(entry, parent)
		if err != nil {
			return err
		}
		if terminal.QuotaRefusal == nil {
			return nil
		}
		pool, found, err := quotaRefusalPool(ctx, tx, parentInvocation)
		if err != nil || !found {
			return err
		}
		record, err := tx.GetPoolUsageObservation(ctx, pool)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		mark, marked := record.ExhaustedWindowMark(terminal.QuotaRefusal.Window, now)
		if !marked || mark.ResetsAt == nil {
			return nil
		}
		selection := e.agentSelection()
		if moved, err := implementerMovedOffPool(ctx, tx, selection, run.ID, pool); err != nil || moved {
			return err
		}
		offers, err := parkedTaskOffers(ctx, tx, selection, run.TaskID, parentInvocation, now)
		if err != nil {
			return err
		}
		return &QuotaWindowExhaustedError{Hold: domain.QuotaWindowHold{
			Pool: pool, ExhaustedMark: mark, AlternateAgents: offers,
		}}
	})
}

// implementerMovedOffPool reports whether the run's current implementer line
// runs a selection that resolves to a pool other than refused (lineRoute). No
// selection, no line, a line that fails its anchor, or a selection that no
// longer resolves reports false: the park holds, and admission refuses a bad
// line when the window resets.
func implementerMovedOffPool(
	ctx context.Context, tx *store.ReadTx, selection *AgentSelection, runID domain.RunID, refused domain.UsagePoolID,
) (bool, error) {
	if selection == nil {
		return false, nil
	}
	line, found, err := RunTaskLine(ctx, tx, selection.Tree, runID, domain.RoleImplementer)
	if err != nil || !found {
		return false, nil
	}
	candidate, ok, err := lineRoute(ctx, tx, line)
	if err != nil || !ok {
		return false, err
	}
	return candidate.pool != refused, nil
}

// agentSelection is the engine's agent selection, nil when it admits under
// one configured identity.
func (e *Engine) agentSelection() *AgentSelection {
	if e.admission == nil {
		return nil
	}
	return e.admission.environment.Agents
}
