package engine

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
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
	ctx := t.Context()
	f := newSpecificationFixture(t, false, 4)
	f.submit(t)
	runID := domain.RunID("specification-run")
	id := specificationInvocationID(runID, 1)
	bound := ward.LaunchBound{BudgetMiB: 4096, RuntimeVersion: ward.RuntimeNone}
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
	if err := recordLaunchOrder(ctx, f.store, bound, runID, id, memoryRefusal(0), start); err != nil {
		t.Fatal(err)
	}
	before := serverRevision(t, f.store)
	if err := recordLaunchOrder(ctx, f.store, bound, runID, id, memoryRefusal(0), start.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if after := serverRevision(t, f.store); after != before || !refusedAt().Equal(start) {
		t.Fatalf("a refusal inside the refresh moved the revision from %d to %d", before, after)
	}
	due := start.Add(store.LaunchWaitRefresh)
	if err := recordLaunchOrder(ctx, f.store, bound, runID, id, memoryRefusal(0), due); err != nil {
		t.Fatal(err)
	}
	if got := refusedAt(); !got.Equal(due) {
		t.Fatalf("refused_at after a due refusal = %s, want %s", got, due)
	}
	pool := &domain.UsagePoolExhaustedError{Pool: "identity:auth-1", Active: 1, Limit: 1}
	if err := recordLaunchOrder(ctx, f.store, bound, runID, id, pool, due.Add(time.Second)); err != nil {
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
	if err := noticeLaunchMemoryRefusal(ctx, f.store, f.signet, runID, run.ProjectID, never); err != nil {
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
	never := launchMemoryRefusalReason(&domain.LaunchNeverFitsError{SizeMiB: 8192, OverheadMiB: 256, BudgetMiB: 4096})
	if !strings.Contains(never, "8192 MiB declared plus 256 MiB per-VM overhead exceeds the 4096 MiB budget") {
		t.Fatalf("never-fits reason = %q", never)
	}
	unknown := launchMemoryRefusalReason(fmt.Errorf("intent: %w", &domain.RuntimeOverheadUnknownError{RuntimeVersion: "9.9.9"}))
	if !strings.Contains(unknown, `"9.9.9"`) {
		t.Fatalf("unrecorded runtime reason = %q", unknown)
	}
}
