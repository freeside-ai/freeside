package store

import (
	"context"
	"errors"
	"fmt"
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
// write-once execution records. There is no counter to leak across a crash.
func (tx *ReadTx) ActiveUsagePoolExecutionCount(ctx context.Context, id domain.UsagePoolID) (int, error) {
	if _, err := tx.GetUsagePool(ctx, id); err != nil {
		return 0, fmt.Errorf("count active executions on usage pool %q: %w", id, err)
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
	identity, err := tx.GetAuthIdentity(ctx, *admission.AuthIdentityID)
	if err != nil {
		return fmt.Errorf("load auth identity %q usage pool: %w", *admission.AuthIdentityID, err)
	}
	pool, err := tx.GetUsagePool(ctx, identity.EffectiveUsagePool())
	if err != nil {
		return fmt.Errorf("load auth identity %q usage pool: %w", identity.ID, err)
	}
	active, err := tx.activeUsagePoolSlots(ctx, pool.ID, admission.InvocationID)
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

// activeUsagePoolSlots counts the executions holding a slot on one pool. An
// admission holds one from its durable pre-start reservation or driver-accepted
// handoff until either mutually exclusive terminal row exists. Each is
// attributed through its authenticated body to the identity's current effective
// pool, so an identity characterized into a shared pool brings its running
// launches with it; reconstruction fails the gate closed on an indexed identity
// that disagrees with the body (#731).
func (tx *ReadTx) activeUsagePoolSlots(
	ctx context.Context, id domain.UsagePoolID, exclude domain.InvocationID,
) (int, error) {
	candidates, err := queryIDs[domain.InvocationID](ctx, tx, listSlotCandidateExecutionsSQL, exclude)
	if err != nil {
		return 0, fmt.Errorf("count active executions on usage pool %q: %w", id, err)
	}
	pools := make(map[domain.AuthIdentityID]domain.UsagePoolID)
	count := 0
	for _, invocation := range candidates {
		terminal, err := tx.executionTerminal(ctx, invocation)
		if err != nil {
			return 0, err
		}
		if terminal {
			continue
		}
		admission, err := tx.GetExecutionAdmissionRecord(ctx, invocation)
		if err != nil {
			return 0, err
		}
		if admission.AuthIdentityID == nil {
			continue
		}
		holder := *admission.AuthIdentityID
		pool, known := pools[holder]
		if !known {
			identity, err := tx.GetAuthIdentity(ctx, holder)
			if err != nil {
				return 0, fmt.Errorf("count active executions on usage pool %q: %w", id, err)
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
