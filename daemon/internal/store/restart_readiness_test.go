package store_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func readRestartReadiness(t *testing.T, s *store.Store, now time.Time) domain.RestartReadiness {
	t.Helper()
	ctx := context.Background()
	var readiness domain.RestartReadiness
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		readiness, err = tx.RestartReadiness(ctx, now, nil)
		return err
	}); err != nil {
		t.Fatalf("RestartReadiness: %v", err)
	}
	return readiness
}

func drainNow(t *testing.T, s *store.Store) {
	t.Helper()
	recordDrain(t, s, domain.UpdateDrainTransition{
		State: domain.UpdateDrainDraining, Build: drainBuild, OccurredAt: admissionEpoch,
	})
}

// TestRestartReadinessNeedsADrain: with nothing held, the read is ready only
// while a drain is in force, and not once it is lifted.
func TestRestartReadinessNeedsADrain(t *testing.T) {
	t.Parallel()
	f := newAdmissionFixture(t, nil)
	s := openWithFixture(t, f, store.Options{AdmissionFloors: attendedFloors()})
	now := admissionEpoch.Add(time.Minute)

	undrained := readRestartReadiness(t, s, now)
	if undrained.Ready || undrained.Drain != nil || len(undrained.Blockers) != 0 || !undrained.AsOf.Equal(now) {
		t.Fatalf("readiness with no drain = %+v, want not ready with no blockers", undrained)
	}
	drainNow(t, s)
	drained := readRestartReadiness(t, s, now)
	if !drained.Ready || drained.Drain == nil || len(drained.Blockers) != 0 {
		t.Fatalf("readiness under a drain = %+v, want ready", drained)
	}
	recordDrain(t, s, domain.UpdateDrainTransition{
		State: domain.UpdateDrainLifted, Build: drainBuild, OccurredAt: now,
	})
	if lifted := readRestartReadiness(t, s, now); lifted.Ready || lifted.Drain != nil {
		t.Fatalf("readiness after the lift = %+v, want not ready with no drain", lifted)
	}
}

// TestRestartReadinessCountsHeldExecutionAndLeases: an execution admission
// holding its slot blocks, its own mutation lease isn't counted again, and a
// live lease no launch holds blocks as credential_lease until it expires.
func TestRestartReadinessCountsHeldExecutionAndLeases(t *testing.T) {
	t.Parallel()
	now := admissionEpoch.Add(time.Minute)
	expires := admissionEpoch.Add(time.Hour)

	t.Run("execution and its own lease", func(t *testing.T) {
		t.Parallel()
		f := newAdmissionFixture(t, nil)
		s := openWithFixture(t, f, store.Options{AdmissionFloors: attendedFloors()})
		drainNow(t, s)
		if err := recordAdmission(t, s, f.admission); err != nil {
			t.Fatal(err)
		}
		// The dispatched outbox row is what makes the admission hold its slot.
		if err := s.WriteInternal(context.Background(), func(tx *store.InternalTx) error {
			if _, _, err := tx.EnqueueOutbox(context.Background(), string(f.admission.InvocationID),
				string(domain.AgentInvocationRequestedKind), []byte("{}")); err != nil {
				return err
			}
			return tx.MarkOutboxDispatched(context.Background(), string(f.admission.InvocationID))
		}); err != nil {
			t.Fatal(err)
		}
		if err := acquireLease(t, s, f.admission.InvocationID, admissionEpoch, expires); err != nil {
			t.Fatal(err)
		}
		got := readRestartReadiness(t, s, now)
		want := []domain.RestartBlocker{{Kind: domain.RestartBlockerExecutionLaunch, Count: 1}}
		if got.Ready || !reflect.DeepEqual(got.Blockers, want) {
			t.Fatalf("readiness = %+v, want not ready with blockers %+v", got, want)
		}
	})

	t.Run("lease no launch holds", func(t *testing.T) {
		t.Parallel()
		f := newAdmissionFixture(t, nil)
		s := openWithFixture(t, f, store.Options{AdmissionFloors: attendedFloors()})
		drainNow(t, s)
		if err := acquireLease(t, s, "enrollment-refresh", admissionEpoch, expires); err != nil {
			t.Fatal(err)
		}
		got := readRestartReadiness(t, s, now)
		want := []domain.RestartBlocker{{Kind: domain.RestartBlockerCredentialLease, Count: 1}}
		if got.Ready || !reflect.DeepEqual(got.Blockers, want) {
			t.Fatalf("readiness = %+v, want not ready with blockers %+v", got, want)
		}
		if expired := readRestartReadiness(t, s, expires); !expired.Ready {
			t.Fatalf("readiness after the lease expired = %+v, want ready", expired)
		}
	})
}

// TestRestartReadinessChangesNothing: the read leaves the operator-stop gate
// and both transition logs as they were.
func TestRestartReadinessChangesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAdmissionFixture(t, nil)
	s := openWithFixture(t, f, store.Options{AdmissionFloors: attendedFloors()})
	recordTransition(t, s, stoppedAt(admissionEpoch, "cmd-stop-1"))
	drainNow(t, s)

	type state struct {
		gate   domain.UnattendedOperationGate
		stop   domain.UnattendedOperationTransition
		drain  *domain.UpdateDrainTransition
		server store.ServerState
	}
	snapshot := func() state {
		t.Helper()
		var out state
		if err := s.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			if out.gate, err = tx.UnattendedOperationGate(ctx); err != nil {
				return err
			}
			if out.stop, _, err = tx.LatestUnattendedOperationTransition(ctx); err != nil {
				return err
			}
			if out.drain, err = tx.UpdateDrainInForce(ctx); err != nil {
				return err
			}
			out.server, err = tx.ServerState(ctx)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := snapshot()
	readRestartReadiness(t, s, admissionEpoch.Add(time.Minute))
	if after := snapshot(); !reflect.DeepEqual(before, after) {
		t.Fatalf("state after the read = %+v, want %+v", after, before)
	}
}

// TestRestartReadinessAddsRunningWork: the in-process counts the caller
// passes join the store's kinds, so work only the process holds keeps a
// drained daemon from reading ready, and none running leaves it ready.
func TestRestartReadinessAddsRunningWork(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAdmissionFixture(t, nil)
	s := openWithFixture(t, f, store.Options{AdmissionFloors: attendedFloors()})
	drainNow(t, s)
	now := admissionEpoch.Add(time.Minute)

	var readiness domain.RestartReadiness
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		readiness, err = tx.RestartReadiness(ctx, now, map[domain.RestartBlockerKind]int{
			domain.RestartBlockerPublication:     1,
			domain.RestartBlockerVerificationJob: 2,
		})
		return err
	}); err != nil {
		t.Fatalf("RestartReadiness: %v", err)
	}
	want := []domain.RestartBlocker{
		{Kind: domain.RestartBlockerVerificationJob, Count: 2},
		{Kind: domain.RestartBlockerPublication, Count: 1},
	}
	if readiness.Ready || !reflect.DeepEqual(readiness.Blockers, want) {
		t.Fatalf("readiness with running work = %+v, want not ready with %+v", readiness, want)
	}
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		readiness, err = tx.RestartReadiness(ctx, now, map[domain.RestartBlockerKind]int{})
		return err
	}); err != nil {
		t.Fatalf("RestartReadiness: %v", err)
	}
	if !readiness.Ready {
		t.Fatalf("readiness with nothing running = %+v, want ready", readiness)
	}
}
