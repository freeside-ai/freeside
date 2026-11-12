package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

const drainBuild domain.Digest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

func recordDrain(t *testing.T, s *store.Store, transition domain.UpdateDrainTransition) {
	t.Helper()
	ctx := context.Background()
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordUpdateDrainTransition(ctx, transition)
	}); err != nil {
		t.Fatalf("RecordUpdateDrainTransition: %v", err)
	}
}

func drainInForce(t *testing.T, s *store.Store) *domain.UpdateDrainTransition {
	t.Helper()
	ctx := context.Background()
	var drain *domain.UpdateDrainTransition
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		drain, err = tx.UpdateDrainInForce(ctx)
		return err
	}); err != nil {
		t.Fatalf("UpdateDrainInForce: %v", err)
	}
	return drain
}

// TestUpdateDrainLatestRowWins: an empty log is no drain, and each appended
// row replaces the state the one before it set.
func TestUpdateDrainLatestRowWins(t *testing.T) {
	t.Parallel()
	s := storetest.Open(t, tempDBPath(t), store.Options{})
	if drain := drainInForce(t, s); drain != nil {
		t.Fatalf("empty log reported drain %+v", drain)
	}
	first := domain.UpdateDrainTransition{
		State: domain.UpdateDrainDraining, Build: drainBuild, OccurredAt: admissionEpoch,
	}
	recordDrain(t, s, first)
	recordDrain(t, s, domain.UpdateDrainTransition{
		State: domain.UpdateDrainLifted, Build: drainBuild, OccurredAt: admissionEpoch.Add(time.Minute),
	})
	if drain := drainInForce(t, s); drain != nil {
		t.Fatalf("drain after lift = %+v, want none", drain)
	}
	second := domain.UpdateDrainTransition{
		State: domain.UpdateDrainDraining, Build: drainBuild, Forced: true,
		OccurredAt: admissionEpoch.Add(2 * time.Minute),
	}
	recordDrain(t, s, second)
	if drain := drainInForce(t, s); drain == nil || *drain != second {
		t.Fatalf("drain after second request = %+v, want %+v", drain, second)
	}
}

// TestUpdateDrainRefusesInvalidRow: the write validates before it reaches the
// table.
func TestUpdateDrainRefusesInvalidRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storetest.Open(t, tempDBPath(t), store.Options{})
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordUpdateDrainTransition(ctx, domain.UpdateDrainTransition{
			State: domain.UpdateDrainLifted, Build: drainBuild, Forced: true, OccurredAt: admissionEpoch,
		})
	}); err == nil {
		t.Fatal("a forced lift was recorded")
	}
	if drain := drainInForce(t, s); drain != nil {
		t.Fatalf("refused write left drain %+v", drain)
	}
}

// TestUpdateDrainLeavesOperatorStop: the drain and the operator stop never
// share a row, so with a stop in force neither a drain nor its lift changes
// what the operator-stop gate reports.
func TestUpdateDrainLeavesOperatorStop(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAdmissionFixture(t, nil)
	s := openWithFixture(t, f, store.Options{AdmissionFloors: attendedFloors()})
	recordTransition(t, s, stoppedAt(admissionEpoch, "cmd-stop-1"))

	assertStopped := func(label string) {
		t.Helper()
		var gate domain.UnattendedOperationGate
		if err := s.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			gate, err = tx.UnattendedOperationGate(ctx)
			return err
		}); err != nil {
			t.Fatalf("%s: UnattendedOperationGate: %v", label, err)
		}
		if gate.OperatorStop == nil || *gate.OperatorStop.CommandID != "cmd-stop-1" {
			t.Fatalf("%s: operator stop = %+v, want the stop from cmd-stop-1", label, gate.OperatorStop)
		}
	}
	recordDrain(t, s, domain.UpdateDrainTransition{
		State: domain.UpdateDrainDraining, Build: drainBuild, OccurredAt: admissionEpoch.Add(time.Minute),
	})
	assertStopped("after draining")
	recordDrain(t, s, domain.UpdateDrainTransition{
		State: domain.UpdateDrainLifted, Build: drainBuild, OccurredAt: admissionEpoch.Add(2 * time.Minute),
	})
	assertStopped("after lifted")
}
