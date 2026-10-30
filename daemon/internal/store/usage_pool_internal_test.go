package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

var slotAt = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

func slotIdentity(id domain.AuthIdentityID, pool string) domain.AuthIdentity {
	return domain.AuthIdentity{
		ID: id, Provider: "claude", AuthStoreMutationLease: true, UsagePool: pool,
		Interim: domain.InterimClientFacts{
			AuthStoreVolume: "volume-" + string(id), RefreshStrategy: domain.RefreshOnDemand,
		},
	}
}

func openSlotStore(t *testing.T) *Store {
	t.Helper()
	return openTemplateStoreAt(t, filepath.Join(t.TempDir(), "slots.db"), Options{})
}

func recordSlotIdentity(t *testing.T, st *Store, identity domain.AuthIdentity, at time.Time) {
	t.Helper()
	if err := st.WriteInternal(t.Context(), func(tx *InternalTx) error {
		return tx.RecordAuthIdentity(t.Context(), identity, at)
	}); err != nil {
		t.Fatalf("record %s: %v", identity.ID, err)
	}
}

func readPool(t *testing.T, st *Store, id domain.UsagePoolID) domain.UsagePool {
	t.Helper()
	var pool domain.UsagePool
	if err := st.Read(t.Context(), func(tx *ReadTx) error {
		var err error
		pool, err = tx.GetUsagePool(t.Context(), id)
		return err
	}); err != nil {
		t.Fatalf("read pool %s: %v", id, err)
	}
	return pool
}

func setPoolLimit(t *testing.T, st *Store, id domain.UsagePoolID, limit int, at time.Time) {
	t.Helper()
	pool := readPool(t, st, id)
	pool.MaxParallelExecutions = limit
	if err := st.WriteInternal(t.Context(), func(tx *InternalTx) error {
		return tx.RecordUsagePool(t.Context(), pool, at)
	}); err != nil {
		t.Fatalf("set pool %s limit: %v", id, err)
	}
}

// TestIdentityCharacterizationTakesTheLowerLimit walks the steps that decide an
// identity's pool. None of them raises anyone's effective concurrency, in either
// direction: a pool keeps a lower limit, and takes a lower one an identity
// brings.
func TestIdentityCharacterizationTakesTheLowerLimit(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		soloLimit, poolLimit, want int
	}{
		"identity brings the lower limit": {soloLimit: 1, poolLimit: 4, want: 1},
		"pool keeps the lower limit":      {soloLimit: 4, poolLimit: 2, want: 2},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			st := openSlotStore(t)
			recordSlotIdentity(t, st, slotIdentity("auth-member", "pool-shared"), slotAt)
			setPoolLimit(t, st, "pool-shared", tc.poolLimit, slotAt.Add(time.Minute))

			solo := slotIdentity("auth-solo", "")
			recordSlotIdentity(t, st, solo, slotAt)
			if got := readPool(t, st, solo.EffectiveUsagePool()).MaxParallelExecutions; got != 1 {
				t.Fatalf("new identity's pool = %d, want 1", got)
			}
			setPoolLimit(t, st, solo.EffectiveUsagePool(), tc.soloLimit, slotAt.Add(time.Minute))

			solo.UsagePool = "pool-shared"
			recordSlotIdentity(t, st, solo, slotAt.Add(time.Hour))
			if got := readPool(t, st, "pool-shared").MaxParallelExecutions; got != tc.want {
				t.Fatalf("pool limit = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestNewIdentityJoinsAPoolWithoutChangingIt: a new identity has no limit to
// bring, so a recorded pool keeps its own.
func TestNewIdentityJoinsAPoolWithoutChangingIt(t *testing.T) {
	t.Parallel()
	st := openSlotStore(t)
	recordSlotIdentity(t, st, slotIdentity("auth-member", "pool-shared"), slotAt)
	setPoolLimit(t, st, "pool-shared", 3, slotAt.Add(time.Minute))
	recordSlotIdentity(t, st, slotIdentity("auth-later", "pool-shared"), slotAt.Add(time.Hour))
	if got := readPool(t, st, "pool-shared").MaxParallelExecutions; got != 3 {
		t.Fatalf("pool limit = %d, want it unchanged", got)
	}
}

func TestIdentityCannotJoinAnotherProvidersPool(t *testing.T) {
	t.Parallel()
	st := openSlotStore(t)
	recordSlotIdentity(t, st, slotIdentity("auth-member", "pool-shared"), slotAt)
	other := slotIdentity("auth-other", "pool-shared")
	other.Provider = "openai"
	err := st.WriteInternal(t.Context(), func(tx *InternalTx) error {
		return tx.RecordAuthIdentity(t.Context(), other, slotAt)
	})
	if !errors.Is(err, domain.ErrImmutableTransition) {
		t.Fatalf("cross-provider join = %v, want %v", err, domain.ErrImmutableTransition)
	}
	if err := st.Read(t.Context(), func(tx *ReadTx) error {
		_, err := tx.GetAuthIdentity(t.Context(), other.ID)
		return err
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("refused identity read = %v, want it rolled back", err)
	}
}

// TestUsagePoolRevisionsOnlyMoveForward keeps a delayed older limit from
// reinstating a superseded one, the direction that raises concurrency past the
// latest safe result.
func TestUsagePoolRevisionsOnlyMoveForward(t *testing.T) {
	t.Parallel()
	st := openSlotStore(t)
	identity := slotIdentity("auth-1", "pool-shared")
	recordSlotIdentity(t, st, identity, slotAt)
	setPoolLimit(t, st, "pool-shared", 2, slotAt.Add(time.Hour))
	pool := readPool(t, st, "pool-shared")

	stale, conflicting := pool, pool
	stale.MaxParallelExecutions = 8
	conflicting.MaxParallelExecutions = 8
	for name, write := range map[string]func(*InternalTx) error{
		"older revision":    func(tx *InternalTx) error { return tx.RecordUsagePool(t.Context(), stale, slotAt.Add(time.Minute)) },
		"same instant, new": func(tx *InternalTx) error { return tx.RecordUsagePool(t.Context(), conflicting, slotAt.Add(time.Hour)) },
	} {
		if err := st.WriteInternal(t.Context(), write); !errors.Is(err, ErrStaleWrite) {
			t.Errorf("%s = %v, want %v", name, err, ErrStaleWrite)
		}
	}
	if err := st.WriteInternal(t.Context(), func(tx *InternalTx) error {
		return tx.RecordUsagePool(t.Context(), pool, slotAt.Add(time.Hour))
	}); err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	moved := pool
	moved.Provider = "openai"
	if err := st.WriteInternal(t.Context(), func(tx *InternalTx) error {
		return tx.RecordUsagePool(t.Context(), moved, slotAt.Add(2*time.Hour))
	}); !errors.Is(err, ErrImmutableConflict) {
		t.Fatalf("provider change = %v, want %v", err, ErrImmutableConflict)
	}
}

// TestUsagePoolFieldsCrossChecked fails a limit raised in one copy, which would
// otherwise admit more launches than anyone recorded.
func TestUsagePoolFieldsCrossChecked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, stmt := range map[string]string{
		"limit raised in the body alone":   `UPDATE usage_pools SET body = json_set(body, '$.pool.max_parallel_executions', 8)`,
		"limit raised in the column alone": `UPDATE usage_pools SET max_parallel_executions = 8`,
		"provider moved in the column":     `UPDATE usage_pools SET provider = 'openai'`,
		"revision moved in the column":     `UPDATE usage_pools SET recorded_at = '2020-01-01T00:00:00Z'`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			st := openSlotStore(t)
			recordSlotIdentity(t, st, slotIdentity("auth-1", "pool-shared"), slotAt)
			if _, err := st.db.ExecContext(ctx, stmt); err != nil {
				t.Fatal(err)
			}
			if err := st.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.ListUsagePools(ctx)
				return err
			}); !errors.Is(err, ErrRowInconsistent) {
				t.Fatalf("read = %v, want %v", err, ErrRowInconsistent)
			}
		})
	}
}

// TestUsagePoolCountRefusesDivergentIndexedIdentity is the #731 regression. The
// count once selected admissions by the indexed identity, so a row whose column
// disagrees with its authenticated body dropped out of the count silently and
// freed a slot nothing had released. The count now attributes by the body and
// fails the gate on the disagreement.
func TestUsagePoolCountRefusesDivergentIndexedIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, admission := seedAdmission(t, nil)
	if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
		if err := tx.RecordAuthIdentity(ctx, slotIdentity("auth-2", ""), admission.AdmittedAt); err != nil {
			return err
		}
		if _, _, err := tx.EnqueueOutbox(ctx, string(admission.InvocationID),
			string(domain.AgentInvocationRequestedKind), []byte("{}")); err != nil {
			return err
		}
		return tx.MarkOutboxDispatched(ctx, string(admission.InvocationID))
	}); err != nil {
		t.Fatal(err)
	}
	count := func(pool domain.UsagePoolID) (int, error) {
		var got int
		err := s.Read(ctx, func(tx *ReadTx) error {
			var err error
			got, err = tx.ActiveUsagePoolExecutionCount(ctx, pool)
			return err
		})
		return got, err
	}
	if got, err := count("identity:auth-1"); err != nil || got != 1 {
		t.Fatalf("count = %d, %v, want the dispatched admission", got, err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE execution_admissions SET auth_identity_id = 'auth-2' WHERE invocation_id = ?`,
		admission.InvocationID); err != nil {
		t.Fatal(err)
	}
	for _, pool := range []domain.UsagePoolID{"identity:auth-1", "identity:auth-2"} {
		if _, err := count(pool); !errors.Is(err, ErrRowInconsistent) {
			t.Errorf("count on %s = %v, want %v", pool, err, ErrRowInconsistent)
		}
	}
	err := s.Read(ctx, func(tx *ReadTx) error {
		next := admission
		next.InvocationID = "inv-next"
		return tx.RequireUsagePoolCapacity(ctx, next)
	})
	if !errors.Is(err, ErrRowInconsistent) {
		t.Fatalf("gate = %v, want it to fail closed with %v", err, ErrRowInconsistent)
	}
}

// TestRunHoldRoundTripsItsUsagePool reads a pool hold back with its pool, and a
// stored identity_parallelism hold with none. A pool without its reason has no
// reading the contract can express, so it fails rather than decoding.
func TestRunHoldRoundTripsItsUsagePool(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, admission := seedAdmission(t, nil)
	pool := domain.UsagePoolID("pool-shared")
	read := func() (domain.RunHoldObservation, error) {
		var hold domain.RunHoldObservation
		err := s.Read(ctx, func(tx *ReadTx) error {
			var err error
			hold, _, err = tx.GetRunHold(ctx, admission.RunID)
			return err
		})
		return hold, err
	}
	write := func(reason domain.RunHoldReason, usagePool *domain.UsagePoolID) {
		t.Helper()
		if err := s.Write(ctx, func(tx *WriteTx) error {
			return tx.RecordRunHold(ctx, domain.RunHoldObservation{
				RunID: admission.RunID, Reason: reason, UsagePool: usagePool,
				FirstObservedAt: slotAt, LastObservedAt: slotAt,
			})
		}); err != nil {
			t.Fatal(err)
		}
	}

	write(domain.HoldUsagePoolExhausted, &pool)
	hold, err := read()
	if err != nil || hold.UsagePool == nil || *hold.UsagePool != pool {
		t.Fatalf("pool hold = %#v, %v", hold, err)
	}
	write(domain.HoldIdentityParallelism, nil)
	hold, err = read()
	if err != nil || hold.Reason != domain.HoldIdentityParallelism || hold.UsagePool != nil {
		t.Fatalf("stored identity hold = %#v, %v, want it readable with no pool", hold, err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE run_hold_observations SET usage_pool = 'pool-shared'`); err != nil {
		t.Fatal(err)
	}
	if _, err := read(); !errors.Is(err, domain.ErrHoldDetailMismatch) {
		t.Fatalf("pool beside another reason = %v, want %v", err, domain.ErrHoldDetailMismatch)
	}
}
