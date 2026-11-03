package store

import (
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// The bootstrap's usage_pools projection reads pools, identities, enrollments,
// and pool records, so each write to them raises the revision a heartbeat
// client compares (plan §5.14). One Write raises it by exactly one.

func storeRevision(t *testing.T, st *Store) int64 {
	t.Helper()
	state, err := st.ServerState(t.Context())
	if err != nil {
		t.Fatalf("server state: %v", err)
	}
	return state.Revision
}

func requireRevision(t *testing.T, st *Store, want int64) {
	t.Helper()
	if got := storeRevision(t, st); got != want {
		t.Fatalf("revision = %d, want %d", got, want)
	}
}

func TestPoolLimitChangeRaisesRevision(t *testing.T) {
	t.Parallel()
	st := openSlotStore(t)
	identity := slotIdentity("auth-a", "")
	recordSlotIdentity(t, st, identity, slotAt)

	before := storeRevision(t, st)
	setPoolLimit(t, st, identity.EffectiveUsagePool(), 3, slotAt.Add(time.Minute))
	requireRevision(t, st, before+1)
}

func TestNewIdentityRaisesRevision(t *testing.T) {
	t.Parallel()
	st := openSlotStore(t)
	identity := slotIdentity("auth-a", "")

	before := storeRevision(t, st)
	recordSlotIdentity(t, st, identity, slotAt)
	requireRevision(t, st, before+1)
	if got := readPool(t, st, identity.EffectiveUsagePool()); got.MaxParallelExecutions != 1 {
		t.Fatalf("new identity's pool = %+v, want a limit of one", got)
	}
}

// Moving an identity off its derived pool deletes that pool's record inside
// RecordAuthIdentity, and the same Write's one bump covers the delete.
func TestCharacterizationRaisesRevision(t *testing.T) {
	t.Parallel()
	st := openObservationStore(t)
	if err := writeObservation(st, t, collectedObservation(), observationAt); err != nil {
		t.Fatalf("record the shared pool: %v", err)
	}
	recordPoolEnrollment(t, st, "auth-moves", "enroll-moves", "", observationAt)
	moved := enrollmentIdentity()
	moved.ID = "auth-moves"
	moved.AccountBinding = "acct-auth-moves"
	moved.UsagePool = ""
	derived := moved.EffectiveUsagePool()
	if err := writeObservation(st, t, domain.PoolUsageObservation{Pool: derived, Collector: "enroll-moves"},
		observationAt); err != nil {
		t.Fatalf("record the derived pool: %v", err)
	}

	before := storeRevision(t, st)
	moved.UsagePool = string(observedPool)
	recordSlotIdentity(t, st, moved, observationAt.Add(time.Hour))
	requireRevision(t, st, before+1)
	if _, err := readObservation(st, t, derived); !errors.Is(err, ErrNotFound) {
		t.Fatalf("derived pool's record = %v, want %v", err, ErrNotFound)
	}
}

func TestClientEnrollmentRaisesRevision(t *testing.T) {
	t.Parallel()
	st := openEnrollmentStore(t)
	recordSlotIdentity(t, st, enrollmentIdentity(), observationAt)

	before := storeRevision(t, st)
	if err := st.Write(t.Context(), func(tx *WriteTx) error {
		return tx.RecordClientEnrollment(t.Context(), codexEnrollment(), observationAt)
	}); err != nil {
		t.Fatalf("record enrollment: %v", err)
	}
	requireRevision(t, st, before+1)
}
