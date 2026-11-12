package engine

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

func memoryRefusal(ahead int) error {
	return &domain.MachineCapacityError{Hold: domain.MachineCapacityHold{
		ReservationMiB: 2304, ReservedMiB: 2304, BudgetMiB: 4096, Ahead: ahead,
	}}
}

// TestLaunchOrderRefreshesAWaitOnlyWhenDue: a launch refused for memory every
// pass refreshes its wait once per store.LaunchWaitRefresh, so the gate keeps
// counting it without each pass moving the revision, and a refusal for
// anything else leaves the order.
func TestLaunchOrderRefreshesAWaitOnlyWhenDue(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newSpecificationFixture(t, false, 4)
	f.submit(t)
	runID := domain.RunID("specification-run")
	id := specificationInvocationID(runID, 1)
	refusedAt := func() time.Time {
		t.Helper()
		var at time.Time
		if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
			var (
				waiting bool
				err     error
			)
			at, waiting, err = tx.LaunchWaitRefusedAt(ctx, id)
			if err == nil && !waiting {
				err = errors.New("the launch is not waiting")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return at
	}
	start := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	if err := recordLaunchOrder(ctx, f.store, f.signet, runID, id, memoryRefusal(0), start); err != nil {
		t.Fatal(err)
	}
	before := serverRevision(t, f.store)
	if err := recordLaunchOrder(ctx, f.store, f.signet, runID, id, memoryRefusal(0), start.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if after := serverRevision(t, f.store); after != before || !refusedAt().Equal(start) {
		t.Fatalf("a refusal inside the refresh moved the revision from %d to %d", before, after)
	}
	due := start.Add(store.LaunchWaitRefresh)
	if err := recordLaunchOrder(ctx, f.store, f.signet, runID, id, memoryRefusal(0), due); err != nil {
		t.Fatal(err)
	}
	if got := refusedAt(); !got.Equal(due) {
		t.Fatalf("refused_at after a due refusal = %s, want %s", got, due)
	}
	pool := &domain.UsagePoolExhaustedError{Pool: "identity:auth-1", Active: 1, Limit: 1}
	if err := recordLaunchOrder(ctx, f.store, f.signet, runID, id, pool, due.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		waiting, err := tx.LaunchWaiting(ctx, id)
		if err == nil && waiting {
			err = errors.New("a pool refusal kept the launch in the memory order")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// TestLaunchMemoryNoticeClearsAMachineCapacityHold: a run an earlier start
// held for memory shows the notice alone once a launch is refused for good.
func TestLaunchMemoryNoticeClearsAMachineCapacityHold(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newSpecificationFixture(t, false, 4)
	f.submit(t)
	runID := domain.RunID("specification-run")
	at := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		return recordRunHold(ctx, tx, runID, specificationInvocationID(runID, 1),
			domain.HoldMachineCapacity, holdDetailFor(memoryRefusal(1)), at)
	}); err != nil {
		t.Fatal(err)
	}
	var run domain.Run
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		run, err = tx.GetRun(ctx, runID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	never := &domain.LaunchNeverFitsError{SizeMiB: 8192, OverheadMiB: 256, BudgetMiB: 4096}
	if err := noticeLaunchMemoryRefusal(
		ctx, f.store, f.signet, &observationPace{}, runID, specificationInvocationID(runID, 1), run.ProjectID, never,
	); err != nil {
		t.Fatal(err)
	}
	if hold := storedRunHold(t, f.store, runID); hold != nil {
		t.Fatalf("hold beside the notice = %+v", hold)
	}
	if _, found, err := readProductionQuarantineItem(ctx, f.store,
		productionQuarantineOccurrenceID(launchMemoryRefusedPrefix, runID, 1)); err != nil || !found {
		t.Fatalf("notice = %v, %t", err, found)
	}
}

// TestExecutionMemoryReservesOnlyAnIdentity: an admission that names no
// identity starts no ward writer, so it reserves nothing and draws no notice
// even under a runtime with no recorded overhead.
func TestExecutionMemoryReservesOnlyAnIdentity(t *testing.T) {
	t.Parallel()
	e := &Engine{launchBound: ward.LaunchBound{BudgetMiB: 4096, RuntimeVersion: "9.9.9"}}
	memory, err := e.executionMemory(t.Context(), nil, domain.Run{ID: "run-1"},
		domain.ExecutionAdmission{InvocationID: "inv-1", RunID: "run-1"})
	if err != nil || memory != nil {
		t.Fatalf("executionMemory = %+v, %v; want nothing reserved", memory, err)
	}
}

// TestReviewUnderAnUnrecordedRuntimeIsRefusedForGood: a review admission
// under a runtime with no recorded overhead is a launch memory refusal, which
// the callers report under the notice, not a wait or a source failure, and it
// admits nothing.
func TestReviewUnderAnUnrecordedRuntimeIsRefusedForGood(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	runID := domain.RunID("run-review-runtime")
	run := domain.Run{ID: runID, ProjectID: "project-shadow", SpecDigest: shadowTestDigest("3"), PolicyDigest: shadowTestDigest("2")}
	if err := st.Write(ctx, func(tx *store.WriteTx) error { return tx.PutRun(ctx, run) }); err != nil {
		t.Fatal(err)
	}
	recordFakeReviewIdentities(t, st)
	w := &productionPublicationWorkflow{
		store: st, reviewAdmitter: fake.AdmitReview,
		now:         func() time.Time { return time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC) },
		launchBound: ward.LaunchBound{BudgetMiB: 4096, RuntimeVersion: "9.9.9"},
	}
	id := ProductionReviewInvocationID(runID, 1)
	_, err := w.ensureReviewAdmission(ctx, domain.RoleReviewer, runID, id)
	if !launchMemoryRefusal(err) || reviewCapacityRefusal(err) {
		t.Fatalf("admission = %v, want a launch memory refusal", err)
	}
	if _, found, err := w.storedReviewAdmission(ctx, domain.RoleReviewer, runID, id); err != nil || found {
		t.Fatalf("stored admission = %t, %v; want none", found, err)
	}
}

// TestLaunchMemoryNoticeNamesTheRefusal: the notice names the numbers of a
// launch that can never fit, or the runtime version with no recorded overhead.
func TestLaunchMemoryNoticeNamesTheRefusal(t *testing.T) {
	t.Parallel()
	never := launchMemoryRefusalReason(&domain.LaunchNeverFitsError{SizeMiB: 8192, OverheadMiB: 256, BudgetMiB: 4096})
	if !strings.Contains(never, "8192 MiB declared plus 256 MiB per-VM overhead exceeds the 4096 MiB budget") {
		t.Fatalf("never-fits reason = %q", never)
	}
	unknown := launchMemoryRefusalReason(fmt.Errorf("intent: %w", &domain.RuntimeOverheadUnknownError{RuntimeVersion: "9.9.9"}))
	if !strings.Contains(unknown, `"9.9.9"`) {
		t.Fatalf("unrecorded runtime reason = %q", unknown)
	}
}

// keptWaitFixture is an older task whose verification waits for memory behind
// a holder, and a newer task whose writer asks the gate for memory that fits
// beside the holder, so only the order can refuse it. Every time is the
// synctest bubble's clock.
type keptWaitFixture struct {
	store    *store.Store
	lane     *productionPublicationWorkflow
	keeper   launchWaitKeeper
	older    productionPublicationTask
	newerRun domain.RunID
}

const (
	keptWaitHolderMiB = 1024
	keptWaitWriterMiB = 1024
	// The budget fits the holder and the newer writer, not the holder and
	// the older verification (2048 MiB), which fits once the holder goes.
	keptWaitBudgetMiB = keptWaitHolderMiB + keptWaitWriterMiB + 512
)

func newKeptWaitFixture(t *testing.T) *keptWaitFixture {
	t.Helper()
	ctx := t.Context()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "state.db"), store.Options{})
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.RegisterProject(ctx, domain.Project{ID: closureStoreProject, Repo: closureStoreRepo, RepositoryID: closureStoreRepoID})
	}); err != nil {
		t.Fatal(err)
	}
	seedRun := func(id domain.RunID, issue int) {
		t.Helper()
		if err := st.Write(ctx, func(tx *store.WriteTx) error {
			task, err := tx.GetOrCreateTask(ctx, closureStoreProject, domain.SpecificationSource{
				Kind: domain.SpecificationSourceIssueSubject,
				IssueSubject: &domain.IssueSubjectRef{
					Repo: closureStoreRepo, RepositoryID: closureStoreRepoID, IssueNumber: issue,
				},
			})
			if err != nil {
				return err
			}
			return tx.PutRun(ctx, domain.Run{
				ID: id, ProjectID: closureStoreProject, TaskID: task.ID,
				SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy", Stages: []domain.Stage{},
			})
		}); err != nil {
			t.Fatal(err)
		}
	}
	seedRun("run-older", 1)
	time.Sleep(time.Second) // the newer task is submitted a second later
	seedRun("run-newer", 2)
	fillMemory(t, st, "run-older", keptWaitBudgetMiB, keptWaitHolderMiB, time.Now())
	return &keptWaitFixture{
		store: st,
		lane: &productionPublicationWorkflow{
			store: st, attention: signet.NewService(st), now: time.Now,
			launchBound: ward.LaunchBound{BudgetMiB: keptWaitBudgetMiB, RuntimeVersion: ward.RuntimeNone},
		},
		older:    productionPublicationTask{RunID: "run-older", ProjectID: closureStoreProject, VerificationID: "inv-verification"},
		newerRun: "run-newer",
	}
}

// reserveOlder runs the older verification's memory reservation as the lane
// does, which records its wait when memory refuses it.
func (f *keptWaitFixture) reserveOlder(t *testing.T) error {
	t.Helper()
	_, err := f.lane.reserveVerification(t.Context(), f.older, productionBinding{})
	return err
}

// reserveNewer asks the gate for the newer task's writer reservation now.
func (f *keptWaitFixture) reserveNewer(t *testing.T) error {
	t.Helper()
	return f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.RequireMachineCapacity(t.Context(), keptWaitBudgetMiB, domain.LaunchReservation{
			InvocationID: "inv-newer-writer", RunID: f.newerRun,
			Kind: domain.ReservationExecution, MemoryMiB: keptWaitWriterMiB,
		}, time.Now())
	})
}

// pass runs a publication pass that works on another task for busy.
func (f *keptWaitFixture) pass(t *testing.T, busy time.Duration, during func()) {
	t.Helper()
	f.keeper.keepDuring(t.Context(), f.store, time.Now, slog.New(slog.DiscardHandler), func() {
		time.Sleep(busy)
		if during != nil {
			during()
		}
	})
}

func wantBehindOne(t *testing.T, err error, what string) {
	t.Helper()
	var refusal *domain.MachineCapacityError
	if !errors.As(err, &refusal) || refusal.Hold.Ahead != 1 {
		t.Fatalf("%s = %v, want a memory refusal with one launch ahead", what, err)
	}
}

// TestPublicationPassKeepsAnOlderVerificationAhead: an older task's
// verification waits for memory while the serial publication lane spends 45
// minutes, far past the wait's lease, on another task's verification. A newer
// task's writer that asks meanwhile, and again once memory frees after the
// pass, still waits behind it, and the older launch reserves first (#2953).
func TestPublicationPassKeepsAnOlderVerificationAhead(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newKeptWaitFixture(t)
		if err := f.reserveOlder(t); !errors.Is(err, domain.ErrMachineCapacity) {
			t.Fatalf("older verification = %v, want it waiting for memory", err)
		}
		f.pass(t, 45*time.Minute, func() {
			wantBehindOne(t, f.reserveNewer(t), "newer writer during the long pass")
		})
		if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
			return tx.ReleaseVerificationReservation(t.Context(), "inv-holder", time.Now())
		}); err != nil {
			t.Fatal(err)
		}
		wantBehindOne(t, f.reserveNewer(t), "newer writer once memory frees")
		if err := f.reserveOlder(t); err != nil {
			t.Fatalf("older verification once memory frees: %v", err)
		}
	})
}

// TestPublicationPassKeepsAWaitFirstRefusedDuringIt: the lane refuses an
// older verification partway through a pass, as it does when the pass reaches
// that task first, and then spends the rest of the pass on another task. The
// keepalive picks the wait up from its next tick, so a newer writer still
// waits behind it.
func TestPublicationPassKeepsAWaitFirstRefusedDuringIt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newKeptWaitFixture(t)
		f.pass(t, 2*time.Minute, func() {
			if err := f.reserveOlder(t); !errors.Is(err, domain.ErrMachineCapacity) {
				t.Fatalf("older verification = %v, want it waiting for memory", err)
			}
			time.Sleep(45 * time.Minute)
			wantBehindOne(t, f.reserveNewer(t), "newer writer late in the pass")
		})
	})
}

// TestKeptWaitLapsesOnceNothingKeepsIt: a kept wait still lapses, so a dead
// one cannot block newer launches. With no pass running it lapses one lease
// after its last keepalive; and while passes keep running long, one the lane
// stopped pursuing drops out of the kept set after two passes and lapses.
func TestKeptWaitLapsesOnceNothingKeepsIt(t *testing.T) {
	t.Parallel()
	// The lease is five refreshes (store.launchWaitLease).
	const lease = 5 * store.LaunchWaitRefresh
	t.Run("no-pass-running", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			f := newKeptWaitFixture(t)
			if err := f.reserveOlder(t); !errors.Is(err, domain.ErrMachineCapacity) {
				t.Fatalf("older verification = %v, want it waiting for memory", err)
			}
			// Keepalives at one, two, and three minutes; the pass returns
			// before the fourth.
			f.pass(t, 3*store.LaunchWaitRefresh+30*time.Second, nil)
			lastKept := time.Now().Add(-30 * time.Second)
			time.Sleep(time.Until(lastKept.Add(lease - time.Second)))
			wantBehindOne(t, f.reserveNewer(t), "newer writer just inside the keepalive's lease")
			time.Sleep(time.Second)
			if err := f.reserveNewer(t); err != nil {
				t.Fatalf("newer writer once the keepalive lapsed: %v", err)
			}
			if launchWaiting(t, f.store, f.older.verificationInvocationID()) {
				t.Fatal("the lapsed wait was not dropped")
			}
		})
	})
	t.Run("lane-stopped-pursuing", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			f := newKeptWaitFixture(t)
			if err := f.reserveOlder(t); !errors.Is(err, domain.ErrMachineCapacity) {
				t.Fatalf("older verification = %v, want it waiting for memory", err)
			}
			// No pass retries the older verification again. The second pass
			// still keeps it, since it was refused after the first pass's
			// start; the third does not.
			f.pass(t, 45*time.Minute, nil)
			f.pass(t, 45*time.Minute, func() {
				wantBehindOne(t, f.reserveNewer(t), "newer writer during the second pass")
			})
			f.pass(t, 45*time.Minute, func() {
				if err := f.reserveNewer(t); err != nil {
					t.Fatalf("newer writer behind a wait nothing pursues: %v", err)
				}
			})
			if launchWaiting(t, f.store, f.older.verificationInvocationID()) {
				t.Fatal("the lapsed wait was not dropped")
			}
		})
	})
}
