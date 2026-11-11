package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const capacityBudgetMiB = 4096

// seedCapacityRuns records one task and run per name, in order, so each run's
// task is submitted before the next one's. Invocation ids sort the same way,
// so a tie in task creation time orders them the same.
func seedCapacityRuns(t *testing.T, st *Store, names ...string) {
	t.Helper()
	ctx := t.Context()
	if err := st.Write(ctx, func(tx *WriteTx) error {
		return tx.RegisterProject(ctx, domain.Project{
			ID: "proj-1", Repo: originTestRepository, RepositoryID: originTestRepositoryID,
		})
	}); err != nil {
		t.Fatal(err)
	}
	for i, name := range names {
		if err := st.Write(ctx, func(tx *WriteTx) error {
			task, err := tx.GetOrCreateTask(ctx, "proj-1", domain.SpecificationSource{
				Kind: domain.SpecificationSourceIssueSubject,
				IssueSubject: &domain.IssueSubjectRef{
					Repo: originTestRepository, RepositoryID: originTestRepositoryID, IssueNumber: i + 1,
				},
			})
			if err != nil {
				return err
			}
			return tx.PutRun(ctx, domain.Run{
				ID: capacityRun(name), ProjectID: "proj-1", TaskID: task.ID,
				SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy", Stages: []domain.Stage{},
			})
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func capacityRun(name string) domain.RunID { return domain.RunID("run-" + name) }

// verificationLaunch is a verification job of run name declaring memoryMiB,
// under Apple container 1.1.0's 256 MiB overhead.
func verificationLaunch(name string, memoryMiB int) domain.LaunchReservation {
	return domain.LaunchReservation{
		InvocationID: domain.InvocationID("inv-" + name), RunID: capacityRun(name),
		Kind: domain.ReservationVerification, MemoryMiB: memoryMiB, OverheadMiB: 256,
	}
}

func reserve(t *testing.T, st *Store, r domain.LaunchReservation) error {
	t.Helper()
	return st.Write(t.Context(), func(tx *WriteTx) error {
		return tx.RequireMachineCapacity(t.Context(), capacityBudgetMiB, r, slotAt)
	})
}

// refuseAndWait reserves r, wants a machine capacity refusal, and records the
// wait as the engine does after one.
func refuseAndWait(t *testing.T, st *Store, r domain.LaunchReservation) domain.MachineCapacityHold {
	t.Helper()
	var refusal *domain.MachineCapacityError
	if err := reserve(t, st, r); !errors.As(err, &refusal) {
		t.Fatalf("reserve %s = %v, want a machine capacity refusal", r.InvocationID, err)
	}
	if err := st.Write(t.Context(), func(tx *WriteTx) error {
		return tx.RecordLaunchWait(t.Context(), r.InvocationID, r.RunID, slotAt)
	}); err != nil {
		t.Fatal(err)
	}
	return refusal.Hold
}

func readCapacityRun(t *testing.T, st *Store, id domain.RunID) domain.Run {
	t.Helper()
	var run domain.Run
	if err := st.Read(t.Context(), func(tx *ReadTx) error {
		var err error
		run, err = tx.GetRun(t.Context(), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return run
}

func releaseVerification(t *testing.T, st *Store, r domain.LaunchReservation) {
	t.Helper()
	if err := st.Write(t.Context(), func(tx *WriteTx) error {
		return tx.ReleaseVerificationReservation(t.Context(), r.InvocationID, slotAt.Add(time.Minute))
	}); err != nil {
		t.Fatal(err)
	}
}

// TestMachineCapacityHoldsALaunchUntilMemoryIsReleased: a launch that does
// not fit waits, its hold names the three numbers, and the release of the
// memory ahead of it lets it start.
func TestMachineCapacityHoldsALaunchUntilMemoryIsReleased(t *testing.T) {
	t.Parallel()
	st := openSlotStore(t)
	seedCapacityRuns(t, st, "1-first", "2-second")
	first, second := verificationLaunch("1-first", 2048), verificationLaunch("2-second", 2048)
	if err := reserve(t, st, first); err != nil {
		t.Fatalf("first launch: %v", err)
	}
	want := domain.MachineCapacityHold{ReservationMiB: 2304, ReservedMiB: 2304, BudgetMiB: capacityBudgetMiB}
	if got := refuseAndWait(t, st, second); got != want {
		t.Fatalf("hold = %+v, want %+v", got, want)
	}
	releaseVerification(t, st, first)
	if err := reserve(t, st, second); err != nil {
		t.Fatalf("second launch after release: %v", err)
	}
	if err := st.Read(t.Context(), func(tx *ReadTx) error {
		waiting, err := tx.LaunchWaiting(t.Context(), second.InvocationID)
		if err == nil && waiting {
			err = errors.New("the granted launch still waits")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// TestMachineCapacityGrantsInTaskSubmissionOrder: a later task's launch that
// would fit does not pass an earlier task's launch that is waiting.
func TestMachineCapacityGrantsInTaskSubmissionOrder(t *testing.T) {
	t.Parallel()
	st := openSlotStore(t)
	seedCapacityRuns(t, st, "1-holder", "2-older", "3-newer")
	holder := verificationLaunch("1-holder", 2048)
	older, newer := verificationLaunch("2-older", 2048), verificationLaunch("3-newer", 1024)
	if err := reserve(t, st, holder); err != nil {
		t.Fatalf("holder: %v", err)
	}
	refuseAndWait(t, st, older)
	want := domain.MachineCapacityHold{ReservationMiB: 1280, ReservedMiB: 2304, BudgetMiB: capacityBudgetMiB, Ahead: 1}
	if got := refuseAndWait(t, st, newer); got != want {
		t.Fatalf("newer launch's hold = %+v, want %+v: it fits, but the older task waits ahead", got, want)
	}
	releaseVerification(t, st, holder)
	if err := reserve(t, st, older); err != nil {
		t.Fatalf("older launch after release: %v", err)
	}
	if err := reserve(t, st, newer); err != nil {
		t.Fatalf("newer launch once the older one holds: %v", err)
	}
}

// TestMachineCapacityRefusesALaunchThatCanNeverFit: a reservation over the
// whole budget is refused naming all three numbers and never waits.
func TestMachineCapacityRefusesALaunchThatCanNeverFit(t *testing.T) {
	t.Parallel()
	st := openSlotStore(t)
	seedCapacityRuns(t, st, "1-huge")
	var never *domain.LaunchNeverFitsError
	err := reserve(t, st, verificationLaunch("1-huge", capacityBudgetMiB))
	if !errors.As(err, &never) || errors.Is(err, domain.ErrMachineCapacity) {
		t.Fatalf("reserve = %v, want a never-fits refusal and no wait", err)
	}
	if *never != (domain.LaunchNeverFitsError{SizeMiB: capacityBudgetMiB, OverheadMiB: 256, BudgetMiB: capacityBudgetMiB}) {
		t.Fatalf("refusal = %+v", *never)
	}
}

// TestMachineCapacityReplaysAHeldReservation: a launch that already holds its
// reservation passes again under a full budget, and a different size for the
// same launch is a conflict, not a second reservation.
func TestMachineCapacityReplaysAHeldReservation(t *testing.T) {
	t.Parallel()
	st := openSlotStore(t)
	seedCapacityRuns(t, st, "1-held", "2-other")
	held := verificationLaunch("1-held", 2048)
	if err := reserve(t, st, held); err != nil {
		t.Fatal(err)
	}
	if err := reserve(t, st, verificationLaunch("2-other", 1536)); err != nil {
		t.Fatalf("second launch filling the budget: %v", err)
	}
	if err := reserve(t, st, held); err != nil {
		t.Fatalf("replay of a held reservation: %v", err)
	}
	resized := held
	resized.MemoryMiB = 1024
	if err := reserve(t, st, resized); !errors.Is(err, ErrImmutableConflict) {
		t.Fatalf("resized replay = %v, want %v", err, ErrImmutableConflict)
	}
}

// TestMachineCapacityLowerBudgetKeepsAHeldReservation: a budget lowered below
// a reservation already held limits only new launches. The held one replays
// without a refusal and still counts, and a new launch of the same size can
// never fit.
func TestMachineCapacityLowerBudgetKeepsAHeldReservation(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st := openSlotStore(t)
	seedCapacityRuns(t, st, "1-held", "2-new")
	held := verificationLaunch("1-held", 2048)
	if err := reserve(t, st, held); err != nil {
		t.Fatal(err)
	}
	const loweredMiB = 1024
	reserveUnder := func(r domain.LaunchReservation) error {
		return st.Write(ctx, func(tx *WriteTx) error {
			return tx.RequireMachineCapacity(ctx, loweredMiB, r, slotAt.Add(time.Second))
		})
	}
	if err := reserveUnder(held); err != nil {
		t.Fatalf("replay of a held reservation under a lower budget: %v", err)
	}
	var never *domain.LaunchNeverFitsError
	if err := reserveUnder(verificationLaunch("2-new", 2048)); !errors.As(err, &never) || never.BudgetMiB != loweredMiB {
		t.Fatalf("new launch under the lower budget = %v, want a never-fits refusal", err)
	}
	var refusal *domain.MachineCapacityError
	if err := reserveUnder(verificationLaunch("2-new", 512)); !errors.As(err, &refusal) ||
		refusal.Hold.ReservedMiB != held.TotalMiB() {
		t.Fatalf("small launch beside the held one = %v, want a wait counting its %d MiB", err, held.TotalMiB())
	}
}

// TestMachineCapacityDropsTheWaitOfARunThatCannotLaunch: a run whose task was
// stopped while it waited leaves the order, so it blocks nobody behind it.
func TestMachineCapacityDropsTheWaitOfARunThatCannotLaunch(t *testing.T) {
	t.Parallel()
	st := openSlotStore(t)
	seedCapacityRuns(t, st, "1-holder", "2-stopped", "3-newer")
	holder := verificationLaunch("1-holder", 2048)
	stopped, newer := verificationLaunch("2-stopped", 2048), verificationLaunch("3-newer", 1024)
	if err := reserve(t, st, holder); err != nil {
		t.Fatal(err)
	}
	refuseAndWait(t, st, stopped)
	run := readCapacityRun(t, st, stopped.RunID)
	state, err := st.ServerState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(t.Context(), func(tx *WriteTx) error {
		_, _, err := tx.StopTask(t.Context(), domain.StopTaskRequest{
			CommandID: "stop", DeviceID: "device", TaskID: run.TaskID, ProjectID: run.ProjectID,
			ExpectedSyncEpoch: state.SyncEpoch, ExpectedEntityVersion: state.Revision,
		}, slotAt)
		return err
	}); err != nil {
		t.Fatalf("stop the waiting task: %v", err)
	}
	if err := reserve(t, st, newer); err != nil {
		t.Fatalf("newer launch behind a stopped wait: %v", err)
	}
	if err := st.Read(t.Context(), func(tx *ReadTx) error {
		waiting, err := tx.LaunchWaiting(t.Context(), stopped.InvocationID)
		if err == nil && waiting {
			err = fmt.Errorf("stopped launch %s still waits", stopped.InvocationID)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func reserveAt(t *testing.T, st *Store, r domain.LaunchReservation, at time.Time) error {
	t.Helper()
	return st.Write(t.Context(), func(tx *WriteTx) error {
		return tx.RequireMachineCapacity(t.Context(), capacityBudgetMiB, r, at)
	})
}

func refreshWait(t *testing.T, st *Store, r domain.LaunchReservation, at time.Time) {
	t.Helper()
	if err := st.Write(t.Context(), func(tx *WriteTx) error {
		return tx.RecordLaunchWait(t.Context(), r.InvocationID, r.RunID, at)
	}); err != nil {
		t.Fatal(err)
	}
}

// TestMachineCapacityDropsALapsedWait: a wait its launch keeps refusing holds
// its place past the lease, and one nothing refuses any more, such as a
// review whose source failed for good while its run stays open, stops
// blocking the launches behind it once the lease lapses.
func TestMachineCapacityDropsALapsedWait(t *testing.T) {
	t.Parallel()
	st := openSlotStore(t)
	seedCapacityRuns(t, st, "1-holder", "2-older", "3-newer")
	holder := verificationLaunch("1-holder", 2048)
	older, newer := verificationLaunch("2-older", 2048), verificationLaunch("3-newer", 1024)
	if err := reserve(t, st, holder); err != nil {
		t.Fatal(err)
	}
	refuseAndWait(t, st, older)

	refreshed := slotAt.Add(launchWaitLease - time.Second)
	refreshWait(t, st, older, refreshed)
	var refusal *domain.MachineCapacityError
	if err := reserveAt(t, st, newer, slotAt.Add(launchWaitLease)); !errors.As(err, &refusal) || refusal.Hold.Ahead != 1 {
		t.Fatalf("newer launch behind a refreshed wait = %v, want a refusal with one ahead", err)
	}
	if err := reserveAt(t, st, newer, refreshed.Add(launchWaitLease)); err != nil {
		t.Fatalf("newer launch behind a lapsed wait: %v", err)
	}
	if err := st.Read(t.Context(), func(tx *ReadTx) error {
		waiting, err := tx.LaunchWaiting(t.Context(), older.InvocationID)
		if err == nil && waiting {
			err = errors.New("the lapsed wait was not dropped")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func keepWaits(t *testing.T, st *Store, since, at time.Time) {
	t.Helper()
	if err := st.WriteInternal(t.Context(), func(tx *InternalTx) error {
		return tx.KeepLaunchWaits(t.Context(), since, at)
	}); err != nil {
		t.Fatal(err)
	}
}

// TestMachineCapacityKeepsAKeptWait: a wait a publication pass keeps alive
// holds its place past its refusal's lease and lapses one lease after the
// keepalive, and the keepalive leaves the refusal the engine throttles by
// unchanged (#2953).
func TestMachineCapacityKeepsAKeptWait(t *testing.T) {
	t.Parallel()
	st := openSlotStore(t)
	seedCapacityRuns(t, st, "1-holder", "2-older", "3-newer")
	holder := verificationLaunch("1-holder", 2048)
	older, newer := verificationLaunch("2-older", 2048), verificationLaunch("3-newer", 1024)
	if err := reserve(t, st, holder); err != nil {
		t.Fatal(err)
	}
	refuseAndWait(t, st, older)

	kept := slotAt.Add(launchWaitLease - time.Second)
	keepWaits(t, st, slotAt, kept)
	if err := st.Read(t.Context(), func(tx *ReadTx) error {
		refusedAt, waiting, err := tx.LaunchWaitRefusedAt(t.Context(), older.InvocationID)
		if err == nil && (!waiting || !refusedAt.Equal(slotAt)) {
			err = fmt.Errorf("refused at %v (waiting %t), want %v", refusedAt, waiting, slotAt)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var refusal *domain.MachineCapacityError
	if err := reserveAt(t, st, newer, slotAt.Add(launchWaitLease+30*time.Second)); !errors.As(err, &refusal) ||
		refusal.Hold.Ahead != 1 {
		t.Fatalf("newer launch behind a kept wait = %v, want a refusal with one ahead", err)
	}
	if err := reserveAt(t, st, newer, kept.Add(launchWaitLease)); err != nil {
		t.Fatalf("newer launch behind a lapsed keepalive: %v", err)
	}
	if err := st.Read(t.Context(), func(tx *ReadTx) error {
		waiting, err := tx.LaunchWaiting(t.Context(), older.InvocationID)
		if err == nil && waiting {
			err = errors.New("the lapsed wait was not dropped")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// TestKeepLaunchWaitsSkipsAWaitNothingPursues: a wait last refused before the
// previous pass's start minus the lease is one the lane stopped pursuing, so
// a pass does not keep it and it lapses on its refusal's lease.
func TestKeepLaunchWaitsSkipsAWaitNothingPursues(t *testing.T) {
	t.Parallel()
	st := openSlotStore(t)
	seedCapacityRuns(t, st, "1-holder", "2-stale", "3-newer")
	holder := verificationLaunch("1-holder", 2048)
	stale, newer := verificationLaunch("2-stale", 2048), verificationLaunch("3-newer", 1024)
	if err := reserve(t, st, holder); err != nil {
		t.Fatal(err)
	}
	refuseAndWait(t, st, stale)

	since := slotAt.Add(launchWaitLease + time.Second)
	keepWaits(t, st, since, since.Add(LaunchWaitRefresh))
	if err := reserveAt(t, st, newer, slotAt.Add(launchWaitLease)); err != nil {
		t.Fatalf("newer launch behind an unkept wait past its lease: %v", err)
	}
}

// TestKeepLaunchWaitsLeavesALapsedWaitLapsed: a keepalive extends a live wait
// but never revives one whose lease already ran out, even when its refusal is
// recent enough for the pass to keep.
func TestKeepLaunchWaitsLeavesALapsedWaitLapsed(t *testing.T) {
	t.Parallel()
	st := openSlotStore(t)
	seedCapacityRuns(t, st, "1-holder", "2-lapsed", "3-newer")
	holder := verificationLaunch("1-holder", 2048)
	lapsed, newer := verificationLaunch("2-lapsed", 2048), verificationLaunch("3-newer", 1024)
	if err := reserve(t, st, holder); err != nil {
		t.Fatal(err)
	}
	refuseAndWait(t, st, lapsed)

	at := slotAt.Add(launchWaitLease)
	keepWaits(t, st, slotAt, at)
	if err := reserveAt(t, st, newer, at.Add(time.Second)); err != nil {
		t.Fatalf("newer launch behind a wait that lapsed before its keepalive: %v", err)
	}
}

// TestMachineCapacityReplaysUnderTheOverheadItReserved: a held launch replayed
// after the runtime's overhead changed keeps the reservation it holds rather
// than conflicting, which would fail every dispatch pass, and a released one
// reserved again takes the current overhead.
func TestMachineCapacityReplaysUnderTheOverheadItReserved(t *testing.T) {
	t.Parallel()
	st := openSlotStore(t)
	seedCapacityRuns(t, st, "1-held", "2-probe")
	held := verificationLaunch("1-held", 2048)
	if err := reserve(t, st, held); err != nil {
		t.Fatal(err)
	}
	upgraded := held
	upgraded.OverheadMiB = 512
	if err := reserve(t, st, upgraded); err != nil {
		t.Fatalf("replay under a changed overhead: %v", err)
	}
	probe := verificationLaunch("2-probe", 2048)
	var refusal *domain.MachineCapacityError
	if err := reserve(t, st, probe); !errors.As(err, &refusal) || refusal.Hold.ReservedMiB != 2304 {
		t.Fatalf("probe = %v, want a refusal counting the 256 MiB overhead the launch reserved", err)
	}
	releaseVerification(t, st, held)
	if err := reserve(t, st, upgraded); err != nil {
		t.Fatalf("re-reserve after release: %v", err)
	}
	if err := st.Read(t.Context(), func(tx *ReadTx) error {
		stored, _, err := tx.getLaunchReservation(t.Context(), held.InvocationID)
		if err == nil && stored.OverheadMiB != 512 {
			err = fmt.Errorf("reopened reservation overhead = %d, want 512", stored.OverheadMiB)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// TestMachineCapacityReviewReservationFollowsItsSlot: a review's memory is
// held exactly while its admission holds its usage-pool slot, so a launch the
// ward has not proved absent keeps it.
func TestMachineCapacityReviewReservationFollowsItsSlot(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		arrange func(context.Context, *WriteTx, domain.InvocationID) error
		held    bool
	}{
		"admitted, nothing launched yet": {
			func(context.Context, *WriteTx, domain.InvocationID) error { return nil }, true,
		},
		"released before a launch": {
			func(ctx context.Context, tx *WriteTx, id domain.InvocationID) error {
				return tx.ReleaseReviewSlot(ctx, id, slotAt)
			}, false,
		},
		"cancelled, containers not proved absent": {
			func(ctx context.Context, tx *WriteTx, id domain.InvocationID) error {
				if err := openReviewIntent(ctx, tx, id); err != nil {
					return err
				}
				return collectReviewOutcome(ctx, tx, id)
			}, true,
		},
		"cancelled, ready proves the containers absent": {
			func(ctx context.Context, tx *WriteTx, id domain.InvocationID) error {
				if err := openReviewIntent(ctx, tx, id); err != nil {
					return err
				}
				if err := collectReviewOutcome(ctx, tx, id); err != nil {
					return err
				}
				return tx.MarkCodexReviewOutcomeReady(ctx, string(id))
			}, false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			st, admission := reviewSlotFixture(t)
			review := domain.LaunchReservation{
				InvocationID: admission.InvocationID, RunID: admission.RunID,
				Kind: domain.ReservationReview, MemoryMiB: 2048, OverheadMiB: 256,
			}
			if err := reserve(t, st, review); err != nil {
				t.Fatalf("reserve the review: %v", err)
			}
			if err := st.Write(ctx, func(tx *WriteTx) error {
				return tc.arrange(ctx, tx, admission.InvocationID)
			}); err != nil {
				t.Fatal(err)
			}
			probe := domain.LaunchReservation{
				InvocationID: "verify-probe", RunID: admission.RunID,
				Kind: domain.ReservationVerification, MemoryMiB: 2048, OverheadMiB: 256,
			}
			err := reserve(t, st, probe)
			if held := errors.Is(err, domain.ErrMachineCapacity); held != tc.held || (!held && err != nil) {
				t.Fatalf("probe = %v, want the review's memory held %t", err, tc.held)
			}
		})
	}
}

// TestListOpenVerificationReservations: the list holds every verification
// reservation not yet released, in invocation order, and no other kind.
func TestListOpenVerificationReservations(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st, admission := reviewSlotFixture(t)
	if err := reserve(t, st, domain.LaunchReservation{
		InvocationID: admission.InvocationID, RunID: admission.RunID,
		Kind: domain.ReservationReview, MemoryMiB: 2048, OverheadMiB: 256,
	}); err != nil {
		t.Fatalf("reserve the review: %v", err)
	}
	launch := func(id domain.InvocationID) domain.LaunchReservation {
		return domain.LaunchReservation{
			InvocationID: id, RunID: admission.RunID,
			Kind: domain.ReservationVerification, MemoryMiB: 256, OverheadMiB: 256,
		}
	}
	for _, id := range []domain.InvocationID{"verify-b", "verify-a", "verify-c"} {
		if err := reserve(t, st, launch(id)); err != nil {
			t.Fatalf("reserve %s: %v", id, err)
		}
	}
	releaseVerification(t, st, launch("verify-c"))
	var got []domain.LaunchReservation
	if err := st.Read(ctx, func(tx *ReadTx) error {
		var err error
		got, err = tx.ListOpenVerificationReservations(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if want := []domain.LaunchReservation{launch("verify-a"), launch("verify-b")}; !slices.Equal(got, want) {
		t.Fatalf("open verification reservations = %+v, want %+v", got, want)
	}
}

// TestMachineCapacityConfirmedStopReleasesVerification: a confirmed task stop
// proves its verification rooms absent, so their memory frees with it.
func TestMachineCapacityConfirmedStopReleasesVerification(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st, admission := reviewSlotFixture(t)
	room := domain.LaunchReservation{
		InvocationID: "verify-room", RunID: admission.RunID,
		Kind: domain.ReservationVerification, MemoryMiB: 2048, OverheadMiB: 256,
	}
	if err := reserve(t, st, room); err != nil {
		t.Fatal(err)
	}
	run := readCapacityRun(t, st, admission.RunID)
	state, err := st.ServerState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		receipt, _, err := tx.StopTask(ctx, domain.StopTaskRequest{
			CommandID: "stop", DeviceID: "device", TaskID: run.TaskID, ProjectID: run.ProjectID,
			ExpectedSyncEpoch: state.SyncEpoch, ExpectedEntityVersion: state.Revision,
		}, slotAt)
		if err != nil {
			return err
		}
		c := receipt.Cancellation
		_, err = tx.AcknowledgeTaskCancellation(ctx, domain.TaskCancellationAcknowledgement{
			ID: "ack", RequestID: c.RequestID, TargetDigest: c.TargetDigest,
			State: domain.TaskCancellationConfirmed, EvidenceDigest: c.TargetDigest, RecordedAt: slotAt,
		})
		return err
	}); err != nil {
		t.Fatalf("stop and confirm: %v", err)
	}
	if err := st.Read(ctx, func(tx *ReadTx) error {
		open, err := tx.openLaunchReservations(ctx)
		if err == nil && len(open) != 0 {
			err = fmt.Errorf("open reservations after a confirmed stop = %+v", open)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
