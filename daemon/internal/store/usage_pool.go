package store

import (
	"context"
	"encoding/json"
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
	getUsagePoolSQL     = `SELECT body FROM usage_pools WHERE id = ?`
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
	reviewSlotReleasedSQL     = `SELECT EXISTS (SELECT 1 FROM review_slot_releases WHERE invocation_id = ?)`
	reviewOutcomeExistsSQL    = `SELECT EXISTS (SELECT 1 FROM codex_review_outcomes WHERE invocation_id = ?)`
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
//
// Clients sync the pool through the bootstrap's usage_pools (plan §5.14), so
// it is written only in a revision-bumping transaction.
func (tx *WriteTx) RecordUsagePool(ctx context.Context, pool domain.UsagePool, recordedAt time.Time) error {
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
	if _, err := tx.tx.ExecContext(ctx, recordUsagePoolSQL, pool.ID, string(pool.Provider),
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
func (tx *WriteTx) settleIdentityUsagePool(
	ctx context.Context, previous *domain.AuthIdentity, identity domain.AuthIdentity, recordedAt time.Time,
) error {
	target := identity.EffectiveUsagePool()
	limit := 1
	if previous != nil {
		from := previous.EffectiveUsagePool()
		if from == target {
			return nil
		}
		// UsagePool is set once, so the pool left behind is the derived pool
		// of this identity alone.
		if err := tx.deletePoolUsageObservation(ctx, from); err != nil {
			return err
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

// GetUsagePool reconstructs one pool declaration from its body. The extracted
// columns aren't cross-checked against it: the pool is Tier B state whose
// limit gates concurrency and grants no authority, and nothing reads the
// columns (AGENTS.md, Assurance Tiers).
func (tx *ReadTx) GetUsagePool(ctx context.Context, id domain.UsagePoolID) (domain.UsagePool, error) {
	pool, _, err := tx.getUsagePoolRecord(ctx, id)
	return pool, err
}

func (tx *ReadTx) getUsagePoolRecord(
	ctx context.Context, id domain.UsagePoolID,
) (domain.UsagePool, time.Time, error) {
	var body []byte
	err := tx.tx.QueryRowContext(ctx, getUsagePoolSQL, id).Scan(&body)
	if err != nil {
		return domain.UsagePool{}, time.Time{}, fmt.Errorf("get usage pool %q: %w", id, notFoundOr(err))
	}
	record, err := decode[usagePoolRecord](body)
	if err != nil {
		return domain.UsagePool{}, time.Time{}, fmt.Errorf("get usage pool %q: %w", id, err)
	}
	return record.Pool, record.RecordedAt, nil
}

// ListUsagePools reconstructs every pool declaration in id order; one
// invalid row fails the whole listing.
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

// ActiveUsagePoolSlotCount derives a pool's current occupancy from the
// write-once execution and review records. There is no counter to leak across
// a crash.
func (tx *ReadTx) ActiveUsagePoolSlotCount(ctx context.Context, id domain.UsagePoolID) (int, error) {
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
	held, err := tx.executionSlotsHeld(ctx, exclude)
	if err != nil {
		return nil, err
	}
	var holders []domain.AuthIdentityID
	for _, id := range held {
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

// executionSlotsHeld returns every execution admission holding a slot under
// executionSlotHolders' rule, without reading whose identity holds it.
func (tx *ReadTx) executionSlotsHeld(
	ctx context.Context, exclude domain.InvocationID,
) ([]domain.InvocationID, error) {
	candidates, err := queryIDs[domain.InvocationID](ctx, tx, listSlotCandidateExecutionsSQL, exclude)
	if err != nil {
		return nil, fmt.Errorf("list slot-holding executions: %w", err)
	}
	open, err := queryIDs[domain.InvocationID](ctx, tx, listOpenHandoffHoldersSQL)
	if err != nil {
		return nil, fmt.Errorf("list open handoffs: %w", err)
	}
	var held []domain.InvocationID
	for _, id := range candidates {
		terminal, err := tx.executionTerminal(ctx, id)
		if err != nil {
			return nil, err
		}
		if terminal && !slices.Contains(open, id) {
			continue
		}
		held = append(held, id)
	}
	return held, nil
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
// proves nothing, and the ready marker or a closed intent behind an outcome
// proves every container absent. An admission with no running launch has
// nothing to prove absent, but it can launch again: a closed intent with no
// outcome is a launch that failed before it started, and Inspect relaunches its
// recorded request without passing the pool gate. It ends with its record, its
// release, or its run.
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
	admission, err := tx.GetReviewAdmission(ctx, id)
	if err != nil {
		return false, fmt.Errorf("review admission %q: %w", id, err)
	}
	return tx.reviewRunCanLaunch(ctx, admission.RunID)
}

// reviewLaunchState reads the ward's journal for a review. ended is the proof
// that no container of the launch remains and that the review will not launch
// again; launched reports an intent that has not yet reached that proof. A
// closed intent with no outcome is neither. A row is authenticated before it
// decides anything, so a rewritten one reads as an error, never as a release.
func (tx *ReadTx) reviewLaunchState(ctx context.Context, id domain.InvocationID) (ended, launched bool, err error) {
	intent, err := tx.GetCodexReviewIntent(ctx, string(id))
	opened := err == nil
	switch {
	case opened:
		if err := verifyReviewIntent(intent); err != nil {
			return false, false, err
		}
	case !errors.Is(err, ErrNotFound):
		return false, false, err
	}
	// Finished reviews are most of the table and every gate reads it, so this
	// branch leaves their large outcome bodies unread: behind an authenticated
	// closed intent no outcome state changes the answer, only whether one exists.
	if opened && intent.State == "closed" {
		var recorded bool
		if err := tx.tx.QueryRowContext(ctx, reviewOutcomeExistsSQL, id).Scan(&recorded); err != nil {
			return false, false, err
		}
		return recorded, false, nil
	}
	outcome, err := tx.GetCodexReviewOutcome(ctx, string(id))
	switch {
	case err == nil:
		if err := verifyReviewOutcome(outcome); err != nil {
			return false, false, err
		}
		if outcome.State == "ready" {
			return true, false, nil
		}
	case !errors.Is(err, ErrNotFound):
		return false, false, err
	}
	return false, opened, nil
}

// verifyReviewIntent checks a journal intent against its digest, and against
// the key and state in its body: the digest alone would pass a finished
// review's row copied onto a live one.
func verifyReviewIntent(record CodexReviewOpaqueRecord) error {
	var body struct {
		RunID string `json:"run_id"`
		State string `json:"state"`
	}
	if record.BodyDigest != codexReviewStateBodyDigest(record.State, record.Body) ||
		json.Unmarshal(record.Body, &body) != nil ||
		body.RunID != record.Key || body.State != record.State {
		return errRowInconsistent
	}
	return nil
}

// verifyReviewOutcome is verifyReviewIntent for a journal outcome, whose state
// is a column the digest covers and whose body carries the invocation.
func verifyReviewOutcome(record CodexReviewOpaqueRecord) error {
	var body struct {
		InvocationID string `json:"invocation_id"`
	}
	if record.BodyDigest != codexReviewStateBodyDigest(record.State, record.Body) ||
		json.Unmarshal(record.Body, &body) != nil || body.InvocationID != record.Key {
		return errRowInconsistent
	}
	return nil
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

// ReviewSlotReleased reports whether a review was given up for good: its
// admission stands, but no later pass may launch it past the pool gate.
func (tx *ReadTx) ReviewSlotReleased(ctx context.Context, id domain.InvocationID) (bool, error) {
	var released bool
	if err := tx.tx.QueryRowContext(ctx, reviewSlotReleasedSQL, id).Scan(&released); err != nil {
		return false, fmt.Errorf("review slot release %q: %w", id, err)
	}
	return released, nil
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
