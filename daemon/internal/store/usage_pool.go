package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const (
	recordUsagePoolSQL = `
INSERT INTO usage_pools (id, provider, max_parallel_executions, recorded_at, body)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    max_parallel_executions = excluded.max_parallel_executions,
    recorded_at             = excluded.recorded_at,
    body                    = excluded.body`
	getUsagePoolSQL = `
SELECT provider, max_parallel_executions, recorded_at, body
FROM usage_pools WHERE id = ?`
	listUsagePoolIDsSQL = `SELECT id FROM usage_pools ORDER BY id`
	// The records that end an admission no ward launch stands behind.
	reviewSlotSettledSQL = `
SELECT EXISTS (SELECT 1 FROM review_records WHERE invocation_id = ?1)
    OR EXISTS (SELECT 1 FROM review_failures WHERE invocation_id = ?1)
    OR EXISTS (SELECT 1 FROM shadow_review_records WHERE invocation_id = ?1)
    OR EXISTS (SELECT 1 FROM review_slot_releases WHERE invocation_id = ?1)`
	// The select makes the release a no-op for an invocation never admitted.
	releaseReviewSlotSQL = `
INSERT INTO review_slot_releases (invocation_id, released_at)
SELECT invocation_id, ? FROM review_admissions WHERE invocation_id = ?
ON CONFLICT (invocation_id) DO NOTHING`
	listOpenHandoffHoldersSQL = `
SELECT COALESCE(lease_holder, read_hold_holder)
FROM handoff_journal_records
WHERE outcome IS NULL AND COALESCE(lease_holder, read_hold_holder) IS NOT NULL`
)

// usagePoolRecord is the stored body: the declaration plus the instant that
// orders its revisions.
type usagePoolRecord struct {
	Pool       domain.UsagePool `json:"pool"`
	RecordedAt time.Time        `json:"recorded_at"`
}

// Validate reports whether a stored body is a complete revision.
func (r usagePoolRecord) Validate() error {
	if r.RecordedAt.IsZero() {
		return fmt.Errorf("usage pool %q recorded_at: %w", r.Pool.ID, domain.ErrMissingTimestamp)
	}
	return r.Pool.Validate()
}

// RecordUsagePool persists a pool declaration, guarded by the domain
// transition rule. A running daemon reads the current revision at every
// admission, so a new limit applies to the next launch without a restart.
func (tx *InternalTx) RecordUsagePool(ctx context.Context, pool domain.UsagePool, recordedAt time.Time) error {
	if recordedAt.IsZero() {
		return fmt.Errorf("record usage pool %q: zero recorded_at", pool.ID)
	}
	if err := pool.Validate(); err != nil {
		return fmt.Errorf("record usage pool %q: %w", pool.ID, err)
	}
	body, err := encode(usagePoolRecord{Pool: pool, RecordedAt: recordedAt.UTC()})
	if err != nil {
		return fmt.Errorf("record usage pool %q: %w", pool.ID, err)
	}
	stored, storedAt, err := tx.getUsagePoolRecord(ctx, pool.ID)
	switch {
	case err == nil:
		if err := domain.ValidateUsagePoolTransition(stored, pool); err != nil {
			return fmt.Errorf("record usage pool %q: %w", pool.ID, mapTransition(err))
		}
		// As for an identity revision: a delayed older limit must not reinstate
		// a superseded one, and a shared instant carries no ordering evidence.
		if recordedAt.Before(storedAt) || (recordedAt.Equal(storedAt) && stored != pool) {
			return fmt.Errorf("record usage pool %q: revision stamped %s, stored revision is %s: %w",
				pool.ID, formatTime(recordedAt), formatTime(storedAt), ErrStaleWrite)
		}
	case !errors.Is(err, ErrNotFound):
		return fmt.Errorf("record usage pool %q: %w", pool.ID, err)
	}
	if _, err := tx.tx.ExecContext(ctx, recordUsagePoolSQL, pool.ID, pool.Provider,
		pool.MaxParallelExecutions, formatTime(recordedAt), body); err != nil {
		return fmt.Errorf("record usage pool %q: %w", pool.ID, err)
	}
	return nil
}

// settleIdentityUsagePool keeps the pool an identity counts against recorded,
// and never lets a step raise anyone's concurrency. A new identity's pool
// starts at one. An identity characterized into a pool brings the limit of the
// pool it leaves, and the pool takes the lower of the two. previous is nil for
// a new identity.
func (tx *InternalTx) settleIdentityUsagePool(
	ctx context.Context, previous *domain.AuthIdentity, identity domain.AuthIdentity, recordedAt time.Time,
) error {
	target := identity.EffectiveUsagePool()
	limit := 1
	if previous != nil {
		from := previous.EffectiveUsagePool()
		if from == target {
			return nil
		}
		left, err := tx.GetUsagePool(ctx, from)
		if err != nil {
			return err
		}
		limit = left.MaxParallelExecutions
	}
	current, err := tx.GetUsagePool(ctx, target)
	switch {
	case errors.Is(err, ErrNotFound):
		return tx.RecordUsagePool(ctx, domain.UsagePool{
			ID: target, Provider: identity.Provider, MaxParallelExecutions: limit,
		}, recordedAt)
	case err != nil:
		return err
	case current.Provider != identity.Provider:
		return fmt.Errorf("usage pool %q belongs to provider %q, identity is %q: %w",
			target, current.Provider, identity.Provider, domain.ErrImmutableTransition)
	case previous == nil || current.MaxParallelExecutions <= limit:
		return nil
	}
	current.MaxParallelExecutions = limit
	return tx.RecordUsagePool(ctx, current, recordedAt)
}

// GetUsagePool reconstructs one pool declaration, cross-checking the extracted
// columns against the decoded body.
func (tx *ReadTx) GetUsagePool(ctx context.Context, id domain.UsagePoolID) (domain.UsagePool, error) {
	pool, _, err := tx.getUsagePoolRecord(ctx, id)
	return pool, err
}

func (tx *ReadTx) getUsagePoolRecord(
	ctx context.Context, id domain.UsagePoolID,
) (domain.UsagePool, time.Time, error) {
	var (
		provider   string
		limit      int
		recordedAt string
		body       []byte
	)
	err := tx.tx.QueryRowContext(ctx, getUsagePoolSQL, id).Scan(&provider, &limit, &recordedAt, &body)
	if err != nil {
		return domain.UsagePool{}, time.Time{}, fmt.Errorf("get usage pool %q: %w", id, notFoundOr(err))
	}
	record, err := decode[usagePoolRecord](body)
	if err != nil {
		return domain.UsagePool{}, time.Time{}, fmt.Errorf("get usage pool %q: %w", id, err)
	}
	pool := record.Pool
	if pool.ID != id || pool.Provider != provider || pool.MaxParallelExecutions != limit ||
		!timeColumnEqual(recordedAt, record.RecordedAt) {
		return domain.UsagePool{}, time.Time{}, fmt.Errorf("get usage pool %q: %w", id, errRowInconsistent)
	}
	return pool, record.RecordedAt, nil
}

// ListUsagePools reconstructs every pool declaration in id order; one
// inconsistent row fails the whole listing.
func (tx *ReadTx) ListUsagePools(ctx context.Context) ([]domain.UsagePool, error) {
	ids, err := queryIDs[domain.UsagePoolID](ctx, tx, listUsagePoolIDsSQL)
	if err != nil {
		return nil, fmt.Errorf("list usage pools: %w", err)
	}
	pools := make([]domain.UsagePool, 0, len(ids))
	for _, id := range ids {
		pool, err := tx.GetUsagePool(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("list usage pools: %w", err)
		}
		pools = append(pools, pool)
	}
	return pools, nil
}

// ActiveUsagePoolExecutionCount derives a pool's current occupancy from the
// write-once execution and review records. There is no counter to leak across
// a crash.
func (tx *ReadTx) ActiveUsagePoolExecutionCount(ctx context.Context, id domain.UsagePoolID) (int, error) {
	if _, err := tx.GetUsagePool(ctx, id); err != nil {
		return 0, fmt.Errorf("count active launches on usage pool %q: %w", id, err)
	}
	return tx.activeUsagePoolSlots(ctx, id, "")
}

// RequireUsagePoolCapacity is the transactional scheduling gate. The store
// begins writes with SQLite's immediate lock, so this derived count and the
// admission insert serialize as one decision across concurrent callers. A
// durable pre-start reservation or driver-accepted handoff counts, closing the
// cross-daemon interval between admission and Start. A synchronous driver
// refusal releases that reservation, so an input-materialization refusal does
// not consume a slot. A replay excludes its own invocation so the write-once
// convergence contract remains intact. Provider-free clean verification
// carries no identity and is outside this limit by definition.
func (tx *ReadTx) RequireUsagePoolCapacity(ctx context.Context, admission domain.ExecutionAdmission) error {
	if admission.AuthIdentityID == nil {
		return nil
	}
	return tx.requireUsagePoolSlot(ctx, *admission.AuthIdentityID, admission.InvocationID)
}

// RequireReviewUsagePoolCapacity is the same gate for a review admission, whose
// reviewer draws on its identity's pool like any other ward role.
func (tx *ReadTx) RequireReviewUsagePoolCapacity(ctx context.Context, admission domain.ReviewAdmission) error {
	return tx.requireUsagePoolSlot(ctx, admission.AuthIdentityID, admission.InvocationID)
}

func (tx *ReadTx) requireUsagePoolSlot(
	ctx context.Context, identityID domain.AuthIdentityID, invocation domain.InvocationID,
) error {
	identity, err := tx.GetAuthIdentity(ctx, identityID)
	if err != nil {
		return fmt.Errorf("load auth identity %q usage pool: %w", identityID, err)
	}
	pool, err := tx.GetUsagePool(ctx, identity.EffectiveUsagePool())
	if err != nil {
		return fmt.Errorf("load auth identity %q usage pool: %w", identityID, err)
	}
	active, err := tx.activeUsagePoolSlots(ctx, pool.ID, invocation)
	if err != nil {
		return err
	}
	if active >= pool.MaxParallelExecutions {
		return &domain.UsagePoolExhaustedError{
			Pool: pool.ID, Active: active, Limit: pool.MaxParallelExecutions,
		}
	}
	return nil
}

// activeUsagePoolSlots counts the launches holding a slot on one pool. Each
// admission is attributed through its authenticated identity to that
// identity's current effective pool, so an identity characterized into a shared
// pool brings its running launches with it.
func (tx *ReadTx) activeUsagePoolSlots(
	ctx context.Context, id domain.UsagePoolID, exclude domain.InvocationID,
) (int, error) {
	executions, err := tx.executionSlotHolders(ctx, exclude)
	if err != nil {
		return 0, fmt.Errorf("count active launches on usage pool %q: %w", id, err)
	}
	reviews, err := tx.reviewSlotHolders(ctx, exclude)
	if err != nil {
		return 0, fmt.Errorf("count active launches on usage pool %q: %w", id, err)
	}
	pools := make(map[domain.AuthIdentityID]domain.UsagePoolID)
	count := 0
	for _, holder := range append(executions, reviews...) {
		pool, known := pools[holder]
		if !known {
			identity, err := tx.GetAuthIdentity(ctx, holder)
			if err != nil {
				return 0, fmt.Errorf("count active launches on usage pool %q: %w", id, err)
			}
			pool = identity.EffectiveUsagePool()
			pools[holder] = pool
		}
		if pool == id {
			count++
		}
	}
	return count, nil
}

// executionSlotHolders returns the identity of every execution admission
// holding a slot. An admission holds one from its durable pre-start reservation
// or driver-accepted handoff until both its terminal record exists (either
// mutually exclusive export or outcome row) and no open handoff journal record
// stands behind it. The journal closes only after every runtime object is
// proved absent, and a cancelled launch can record its outcome first; a launch
// with no journal record (a fake backend, a cancellation before the handoff
// opened one) released on its terminal record alone.
func (tx *ReadTx) executionSlotHolders(
	ctx context.Context, exclude domain.InvocationID,
) ([]domain.AuthIdentityID, error) {
	candidates, err := queryIDs[domain.InvocationID](ctx, tx, listSlotCandidateExecutionsSQL, exclude)
	if err != nil {
		return nil, fmt.Errorf("list slot-holding executions: %w", err)
	}
	open, err := queryIDs[domain.InvocationID](ctx, tx, listOpenHandoffHoldersSQL)
	if err != nil {
		return nil, fmt.Errorf("list open handoffs: %w", err)
	}
	var holders []domain.AuthIdentityID
	for _, id := range candidates {
		terminal, err := tx.executionTerminal(ctx, id)
		if err != nil {
			return nil, err
		}
		if terminal && !slices.Contains(open, id) {
			continue
		}
		// The body is the authority for who holds the slot; its reconstruction
		// fails the gate closed on an indexed identity that disagrees (#731).
		admission, err := tx.GetExecutionAdmissionRecord(ctx, id)
		if err != nil {
			return nil, err
		}
		if admission.AuthIdentityID != nil {
			holders = append(holders, *admission.AuthIdentityID)
		}
	}
	return holders, nil
}

func (tx *ReadTx) executionTerminal(ctx context.Context, id domain.InvocationID) (bool, error) {
	if _, err := tx.GetExecutionExportRecord(ctx, id); err == nil {
		return true, nil
	} else if !errors.Is(err, ErrNotFound) {
		return false, fmt.Errorf("validate execution export %q: %w", id, err)
	}
	if _, err := tx.GetExecutionOutcomeRecord(ctx, id); err == nil {
		return true, nil
	} else if !errors.Is(err, ErrNotFound) {
		return false, fmt.Errorf("validate execution outcome %q: %w", id, err)
	}
	return false, nil
}

// reviewSlotHolders returns the identity of every review admission holding a
// slot.
func (tx *ReadTx) reviewSlotHolders(
	ctx context.Context, exclude domain.InvocationID,
) ([]domain.AuthIdentityID, error) {
	ids, err := queryIDs[domain.InvocationID](ctx, tx,
		`SELECT invocation_id FROM review_admissions WHERE invocation_id <> ?`, exclude)
	if err != nil {
		return nil, fmt.Errorf("list review admissions: %w", err)
	}
	var holders []domain.AuthIdentityID
	for _, id := range ids {
		held, err := tx.reviewSlotHeld(ctx, id)
		if err != nil {
			return nil, err
		}
		if !held {
			continue
		}
		admission, err := tx.GetReviewAdmission(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("review admission %q: %w", id, err)
		}
		holders = append(holders, admission.AuthIdentityID)
	}
	return holders, nil
}

// reviewSlotHeld applies the release rule to one review admission. For a
// launch the ward journalled, only the ward's proof ends the slot: a cancelled
// review records its outcome before its containers stop, so the outcome alone
// proves nothing, and the ready marker or the closed intent proves every
// container absent. An admission with no launch has nothing to prove absent; it
// ends with its record, its release, or its run.
func (tx *ReadTx) reviewSlotHeld(ctx context.Context, id domain.InvocationID) (bool, error) {
	ended, launched, err := tx.reviewLaunchState(ctx, id)
	if err != nil {
		return false, fmt.Errorf("review launch %q: %w", id, err)
	}
	if ended {
		return false, nil
	}
	if launched {
		return true, nil
	}
	var settled bool
	if err := tx.tx.QueryRowContext(ctx, reviewSlotSettledSQL, id).Scan(&settled); err != nil {
		return false, fmt.Errorf("review admission %q: %w", id, err)
	}
	if settled {
		return false, nil
	}
	var runID domain.RunID
	if err := tx.tx.QueryRowContext(ctx,
		`SELECT run_id FROM review_admissions WHERE invocation_id = ?`, id).Scan(&runID); err != nil {
		return false, fmt.Errorf("review admission %q: %w", id, notFoundOr(err))
	}
	return tx.reviewRunCanLaunch(ctx, runID)
}

// reviewLaunchState reads the ward's journal for a review. ended is the proof
// that no container of the launch remains; launched reports an intent that has
// not yet reached that proof. The stored state is authenticated against its
// digest, so a rewritten row reads as an error, never as a release.
func (tx *ReadTx) reviewLaunchState(ctx context.Context, id domain.InvocationID) (ended, launched bool, err error) {
	outcome, err := tx.GetCodexReviewOutcome(ctx, string(id))
	switch {
	case err == nil:
		if outcome.BodyDigest != codexReviewStateBodyDigest(outcome.State, outcome.Body) {
			return false, false, errRowInconsistent
		}
		if outcome.State == "ready" {
			return true, false, nil
		}
	case !errors.Is(err, ErrNotFound):
		return false, false, err
	}
	intent, err := tx.GetCodexReviewIntent(ctx, string(id))
	switch {
	case errors.Is(err, ErrNotFound):
		return false, false, nil
	case err != nil:
		return false, false, err
	case intent.BodyDigest != codexReviewStateBodyDigest(intent.State, intent.Body):
		return false, false, errRowInconsistent
	}
	return intent.State == "closed", intent.State != "closed", nil
}

// reviewRunCanLaunch reports whether the run may still launch an admitted
// review: neither a cancelled task nor a closed workflow will.
func (tx *ReadTx) reviewRunCanLaunch(ctx context.Context, runID domain.RunID) (bool, error) {
	run, err := tx.GetRun(ctx, runID)
	if err != nil {
		return false, err
	}
	// Launching passes the task gate, which a run without a task never does.
	if run.TaskID == "" {
		return false, nil
	}
	task, err := tx.GetTask(ctx, run.TaskID)
	if err != nil {
		return false, err
	}
	if task.Cancellation != nil {
		return false, nil
	}
	if err := tx.RequireRunWorkflowOpen(ctx, runID); errors.Is(err, ErrTaskWorkflowClosed) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, nil
}

// ReleaseReviewSlot ends the slot of a review admission that will never launch,
// such as a shadow review abandoned before its request. It does not outrank the
// ward: a launched review still holds its slot until the ward's proof. Releasing
// an invocation never admitted does nothing.
func (tx *InternalTx) ReleaseReviewSlot(ctx context.Context, id domain.InvocationID, at time.Time) error {
	if _, err := tx.tx.ExecContext(ctx, releaseReviewSlotSQL, formatTime(at), id); err != nil {
		return fmt.Errorf("release review slot %q: %w", id, err)
	}
	return nil
}
