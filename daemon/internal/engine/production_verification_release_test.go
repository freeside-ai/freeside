package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/macverify"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

var errProbeRollback = errors.New("probe rollback")

// releaseFixture holds one task's verification reservation under a budget
// that fits a single verification job, so whether it is still held shows in
// whether a second job of that size fits beside it.
type releaseFixture struct {
	w      *productionPublicationWorkflow
	task   productionPublicationTask
	run    domain.Run
	budget int
	size   int
	at     time.Time
	// proved is what the fake ward proof reports for the invocation.
	proved error
	asked  []domain.InvocationID
}

func newReleaseFixture(t *testing.T, bound bool) *releaseFixture {
	t.Helper()
	f := newSpecificationFixture(t, false, 4)
	f.submit(t)
	runID := domain.RunID("specification-run")
	run, err := f.run(runID)
	if err != nil {
		t.Fatal(err)
	}
	size := ward.DefaultLaunchSize(ward.LaunchVerification).MemoryMiB
	rf := &releaseFixture{
		task: productionPublicationTask{RunID: runID, ProjectID: run.ProjectID, VerificationID: "inv-verification"},
		run:  run, budget: size + size/2, size: size, at: *f.now,
	}
	rf.w = &productionPublicationWorkflow{
		store: f.store, attention: f.signet, now: func() time.Time { return *f.now },
		launchBound: ward.LaunchBound{BudgetMiB: rf.budget, RuntimeVersion: ward.RuntimeNone},
	}
	if bound {
		rf.w.proveVerificationAbsent = func(ctx context.Context, got domain.Run, invocation domain.InvocationID) error {
			if got.ID != rf.run.ID || ctx.Err() != nil {
				t.Errorf("proof asked for run %q under a done context %v", got.ID, ctx.Err())
			}
			rf.asked = append(rf.asked, invocation)
			return rf.proved
		}
	}
	reservation := domain.LaunchReservation{
		InvocationID: rf.task.verificationInvocationID(), RunID: runID,
		Kind: domain.ReservationVerification, MemoryMiB: size,
	}
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.RequireMachineCapacity(t.Context(), rf.budget, reservation, rf.at)
	}); err != nil {
		t.Fatal(err)
	}
	return rf
}

// held probes, in a rolled-back write, whether a second verification job
// fits beside the task's.
func (rf *releaseFixture) held(t *testing.T) bool {
	t.Helper()
	probe := domain.LaunchReservation{
		InvocationID: "inv-probe", RunID: rf.task.RunID, Kind: domain.ReservationVerification, MemoryMiB: rf.size,
	}
	var refused error
	err := rf.w.store.Write(t.Context(), func(tx *store.WriteTx) error {
		refused = tx.RequireMachineCapacity(t.Context(), rf.budget, probe, rf.at)
		return errProbeRollback
	})
	if !errors.Is(err, errProbeRollback) {
		t.Fatal(err)
	}
	if refused != nil && !errors.Is(refused, domain.ErrMachineCapacity) {
		t.Fatal(refused)
	}
	return refused != nil
}

func (rf *releaseFixture) release(t *testing.T, roomErr error) error {
	t.Helper()
	// The pass's own context is already done, as at a shutdown mid-pass; the
	// proof must still run.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	return rf.w.releaseVerification(ctx, rf.task, rf.run, roomErr)
}

var unproven = ward.ErrVerificationAbsenceUnproven

// TestVerificationReleaseWaitsForEveryRoom: a clean retry does not release
// the reservation while the first pass's room is still unproven; the release
// comes once the proof covers that room too.
func TestVerificationReleaseWaitsForEveryRoom(t *testing.T) {
	t.Parallel()
	rf := newReleaseFixture(t, true)
	if !rf.held(t) {
		t.Fatal("the fixture's reservation is not held")
	}
	rf.proved = unproven
	if err := rf.release(t, unproven); err != nil || !rf.held(t) {
		t.Fatalf("unproven first pass: release = %v, held = %v", err, rf.held(t))
	}
	if err := rf.release(t, nil); err != nil || !rf.held(t) {
		t.Fatalf("clean retry over an unproven earlier room: release = %v, held = %v", err, rf.held(t))
	}
	rf.proved = nil
	if err := rf.release(t, nil); err != nil || rf.held(t) {
		t.Fatalf("clean retry once every room is proved: release = %v, held = %v", err, rf.held(t))
	}
	for _, got := range rf.asked {
		if got != rf.task.verificationInvocationID() {
			t.Fatalf("proof asked for %q, want the task's verification invocation", got)
		}
	}
}

// TestVerificationReleaseAfterMemoryLimitKill: a kill whose room cleanup
// failed is released when the proof's second cleanup attempt proves absence,
// and stays held when that attempt fails too, until a confirmed task stop.
func TestVerificationReleaseAfterMemoryLimitKill(t *testing.T) {
	t.Parallel()
	kill := errors.Join(ward.ErrMemoryLimit, unproven)
	for _, tc := range []struct {
		name   string
		proved error
		held   bool
	}{
		{"second-cleanup-proves", nil, false},
		{"second-cleanup-fails", errors.Join(unproven, errors.New("runtime refused the delete")), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rf := newReleaseFixture(t, true)
			rf.proved = tc.proved
			if err := rf.release(t, kill); err != nil {
				t.Fatal(err)
			}
			if got := rf.held(t); got != tc.held {
				t.Fatalf("held = %v, want %v", got, tc.held)
			}
		})
	}
}

// TestVerificationReleaseReturnsAProofFailure: a proof that fails for a
// reason other than absence keeps the reservation and reports the failure.
func TestVerificationReleaseReturnsAProofFailure(t *testing.T) {
	t.Parallel()
	rf := newReleaseFixture(t, true)
	rf.proved = domain.ErrEmptyID
	if err := rf.release(t, nil); !errors.Is(err, domain.ErrEmptyID) || !rf.held(t) {
		t.Fatalf("release = %v, held = %v", err, rf.held(t))
	}
}

// TestVerificationReleaseWithoutBoundRooms: unbound rooms write no records,
// so their own return is the proof.
func TestVerificationReleaseWithoutBoundRooms(t *testing.T) {
	t.Parallel()
	rf := newReleaseFixture(t, false)
	if err := rf.release(t, unproven); err != nil || !rf.held(t) {
		t.Fatalf("unproven room: release = %v, held = %v", err, rf.held(t))
	}
	if err := rf.release(t, errors.New("recipe failed")); err != nil || rf.held(t) {
		t.Fatalf("proved room: release = %v, held = %v", err, rf.held(t))
	}
}

// TestVerificationReleaseHoldsForAnUnprovenHostSlot: the host executor
// writes no ownership records, so a pass whose slot absence is unproven keeps
// the reservation even when ward proves every container gone, after ward's
// proof has run its cleanup. The hold covers that pass alone: a later pass
// whose own proofs pass releases it (#3000).
func TestVerificationReleaseHoldsForAnUnprovenHostSlot(t *testing.T) {
	t.Parallel()
	slot := fmt.Errorf("slot teardown: %w", macverify.ErrAbsenceUnproven)
	for name, bound := range map[string]bool{"bound": true, "unbound": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rf := newReleaseFixture(t, bound)
			if err := rf.release(t, slot); err != nil || !rf.held(t) {
				t.Fatalf("unproven slot: release = %v, held = %v", err, rf.held(t))
			}
			if bound && len(rf.asked) != 1 {
				t.Fatalf("ward proof ran %d times for the unproven slot's pass, want once", len(rf.asked))
			}
			if err := rf.release(t, nil); err != nil || rf.held(t) {
				t.Fatalf("later proved pass: release = %v, held = %v", err, rf.held(t))
			}
		})
	}
}

// fakeTransport and fakeArtifacts satisfy the config's nil checks only; the
// refusal comes before either is used.
type (
	fakeTransport struct{ PublicationTransport }
	fakeArtifacts struct{ ArtifactStore }
)

func TestProductionPublicationRefusesBoundRoomsWithoutAbsenceProof(t *testing.T) {
	t.Parallel()
	f := newSpecificationFixture(t, false, 4)
	newRoom := func(domain.ProjectImage, ward.ContainerSize) (ProductionVerificationRoom, error) {
		return nil, errors.New("unused")
	}
	cfg := ProductionPublicationConfig{
		WorkDir: t.TempDir(), Transport: fakeTransport{}, Publisher: &publish.Publisher{},
		Artifacts: fakeArtifacts{}, NewRoom: newRoom, HoldOnly: true,
		ReviewRecovery: func(context.Context) error { return nil },
		NewBoundRoom: func(domain.ProjectImage, ward.ContainerSize, domain.Run, domain.InvocationID) (ProductionVerificationRoom, error) {
			return nil, errors.New("unused")
		},
	}
	if _, err := newProductionPublicationWorkflow(f.store, f.signet, cfg); err == nil {
		t.Fatal("bound rooms without an absence proof were accepted")
	}
	cfg.ProveVerificationAbsent = func(context.Context, domain.Run, domain.InvocationID) error { return nil }
	if _, err := newProductionPublicationWorkflow(f.store, f.signet, cfg); err == nil {
		t.Fatal("bound rooms without the operator release's container-only proof were accepted")
	}
	cfg.ProveVerificationContainersAbsent = func(context.Context, domain.Run, domain.InvocationID) error { return nil }
	if _, err := newProductionPublicationWorkflow(f.store, f.signet, cfg); err != nil {
		t.Fatalf("bound rooms with an absence proof: %v", err)
	}
}
