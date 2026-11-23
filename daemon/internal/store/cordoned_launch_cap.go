package store

import (
	"context"
	"fmt"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// RequireCordonedLaunchCap bounds how many cordoned launches run at once
// (plan §5.4, Machine Capacity; issue #3471). A cordoned launch has no VM, so
// it reserves nothing against the memory budget; the instance's
// cordoned_launches.max_concurrent caps it instead. It runs on a new
// admission and on a supersession, never on an exact replay, and does nothing
// for an admission on another class or one that names no identity, which
// takes no pool slot and starts no writer, as the memory gate skips it.
//
// It refuses with domain.ErrCordonedLaunchCap when no cap is set, when the
// cordoned launches already running number the cap, or when an older task's
// cordoned launch waits ahead of this one. The cap is read from the stored
// record in this transaction, so a set applies from the next admission. A
// launch counts as running while its admission holds a pool slot and has no
// recorded export or outcome: a finished cordoned run has no process left on
// the host, so unlike the memory reservation it frees its place before its
// handoff closes. An admission that is already running, such as a supersession
// of one whose dispatch stands, passes without a new decision, under a cap
// lowered or cleared since, as a held memory reservation does. A pass ends the
// launch's cordoned_launch wait.
func (tx *WriteTx) RequireCordonedLaunchCap(ctx context.Context, admission domain.ExecutionAdmission) error {
	if admission.Backend != string(domain.BackendCordonedHostProcess) || admission.AuthIdentityID == nil {
		return nil
	}
	running, err := tx.runningCordonedLaunches(ctx)
	if err != nil {
		return fmt.Errorf("cordoned launch %q cap: %w", admission.InvocationID, err)
	}
	if !slices.Contains(running, admission.InvocationID) {
		if err := tx.requireCordonedPlace(ctx, admission, running); err != nil {
			return err
		}
	}
	if _, err := tx.tx.ExecContext(ctx, deleteLaunchWaitSQL, admission.InvocationID); err != nil {
		return fmt.Errorf("end cordoned launch %q wait: %w", admission.InvocationID, err)
	}
	return nil
}

// requireCordonedPlace is the cap's decision for a launch not yet running.
func (tx *WriteTx) requireCordonedPlace(
	ctx context.Context, admission domain.ExecutionAdmission, running []domain.InvocationID,
) error {
	record, err := tx.InstanceSettingsRecord(ctx)
	if err != nil {
		return fmt.Errorf("cordoned launch %q cap: %w", admission.InvocationID, err)
	}
	limit, err := record.CordonedLaunches()
	if err != nil {
		return fmt.Errorf("cordoned launch %q cap: %w", admission.InvocationID, err)
	}
	if limit == nil {
		return fmt.Errorf("cordoned launch %q: no cordoned_launches cap is set: %w",
			admission.InvocationID, domain.ErrCordonedLaunchCap)
	}
	if len(running) >= limit.MaxConcurrent {
		return fmt.Errorf("cordoned launch %q: %d of %d running: %w",
			admission.InvocationID, len(running), limit.MaxConcurrent, domain.ErrCordonedLaunchCap)
	}
	createdAt, err := tx.runTaskCreatedAt(ctx, admission.RunID)
	if err != nil {
		return fmt.Errorf("cordoned launch %q cap: %w", admission.InvocationID, err)
	}
	self := launchWait{
		invocation: admission.InvocationID, resource: LaunchWaitCordonedLaunch,
		run: admission.RunID, taskCreatedAt: createdAt,
	}
	ahead, err := tx.launchesAhead(ctx, LaunchWaitCordonedLaunch, self, running, admission.AdmittedAt)
	if err != nil {
		return fmt.Errorf("cordoned launch %q cap: %w", admission.InvocationID, err)
	}
	if ahead > 0 {
		return fmt.Errorf("cordoned launch %q: %d older cordoned launches wait ahead: %w",
			admission.InvocationID, ahead, domain.ErrCordonedLaunchCap)
	}
	return nil
}

// runningCordonedLaunches returns every cordoned execution admission that
// holds a pool slot and has no recorded export or outcome. The stored body is
// the authority for the class, as it is for the slot's identity.
func (tx *ReadTx) runningCordonedLaunches(ctx context.Context) ([]domain.InvocationID, error) {
	candidates, err := queryIDs[domain.InvocationID](ctx, tx, listSlotCandidateExecutionsSQL, "")
	if err != nil {
		return nil, fmt.Errorf("list slot-holding executions: %w", err)
	}
	var running []domain.InvocationID
	for _, id := range candidates {
		terminal, err := tx.executionTerminal(ctx, id)
		if err != nil {
			return nil, err
		}
		if terminal {
			continue
		}
		admission, err := tx.GetExecutionAdmissionRecord(ctx, id)
		if err != nil {
			return nil, err
		}
		if admission.Backend == string(domain.BackendCordonedHostProcess) {
			running = append(running, id)
		}
	}
	return running, nil
}
