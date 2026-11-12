package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	specifyfake "github.com/freeside-ai/freeside/daemon/internal/specify/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// recordBudget records the instance's memory budget as
// freesided set-memory-budget does.
func recordBudget(t *testing.T, st *store.Store, budgetMiB int) {
	t.Helper()
	if err := st.WriteInternal(t.Context(), func(tx *store.InternalTx) error {
		return tx.RecordMemoryBudget(t.Context(), budgetMiB, time.Now().UTC())
	}); err != nil {
		t.Fatal(err)
	}
}

func launchWaiting(t *testing.T, st *store.Store, id domain.InvocationID) bool {
	t.Helper()
	var waiting bool
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		waiting, err = tx.LaunchWaiting(t.Context(), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return waiting
}

// fillMemory reserves memoryMiB for another launch of runID, so the next
// launch that needs more than what is left waits.
func fillMemory(t *testing.T, st *store.Store, runID domain.RunID, budgetMiB, memoryMiB int, at time.Time) {
	t.Helper()
	holder := domain.LaunchReservation{
		InvocationID: "inv-holder", RunID: runID, Kind: domain.ReservationVerification, MemoryMiB: memoryMiB,
	}
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.RequireMachineCapacity(t.Context(), budgetMiB, holder, at)
	}); err != nil {
		t.Fatal(err)
	}
}

// memoryNotice returns the run's first launch memory notice.
func memoryNotice(t *testing.T, st *store.Store, runID domain.RunID) (domain.AttentionItem, bool) {
	t.Helper()
	item, found, err := readProductionQuarantineItem(t.Context(), st,
		productionQuarantineOccurrenceID(launchMemoryRefusedPrefix, runID, 1))
	if err != nil {
		t.Fatal(err)
	}
	return item, found
}

func scriptSpecification(t *testing.T, driver *fake.StageDriver, id domain.InvocationID, pendingInspects int) {
	t.Helper()
	if err := specifyfake.Script(driver, id, pendingInspects, 0, specify.Output{Specification: &specify.Specification{
		Summary: "The implementation plan is ready.", Body: "# Specification\n\nImplement.",
		Addressals: []specify.Addressal{},
	}}); err != nil {
		t.Fatal(err)
	}
}

// TestLoweredBudgetRefusesAWaitingDispatchForGood: a dispatch waiting for
// memory whose reservation a recorded budget no longer covers gets its notice
// at the next pass, with no restart, and leaves its hold and the memory
// order, so it holds back no launch behind it. A budget raised to fit it
// beside the memory held puts it back to waiting and retires the notice; one
// raised further admits it.
func TestLoweredBudgetRefusesAWaitingDispatchForGood(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newSpecificationFixture(t, false, 4)
	f.launchBound = ward.LaunchBound{BudgetMiB: 3072, RuntimeVersion: ward.RuntimeNone}
	driver := f.newDriver(t)
	runID := domain.RunID("specification-run")
	id := specificationInvocationID(runID, 1)
	scriptSpecification(t, driver, id, 0)
	f.submit(t)
	engine := f.newEngine(t, driver)
	fillMemory(t, f.store, runID, 3072, 2048, *f.now)
	if _, err := engine.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if hold := storedRunHold(t, f.store, runID); hold == nil || hold.Reason != domain.HoldMachineCapacity ||
		!launchWaiting(t, f.store, id) {
		t.Fatalf("hold = %+v, want the dispatch waiting for memory", hold)
	}

	recordBudget(t, f.store, 1024)
	if _, err := engine.Reconcile(ctx); err != nil {
		t.Fatalf("a launch the lowered budget refuses stopped the pass: %v", err)
	}
	item, found := memoryNotice(t, f.store, runID)
	if !found || item.Status != domain.StatusOpen ||
		!strings.Contains(item.Reason, "2048 MiB declared plus 0 MiB per-VM overhead exceeds the 1024 MiB budget") ||
		!strings.Contains(item.Reason, "freesided set-memory-budget") {
		t.Fatalf("notice = %+v, %t", item, found)
	}
	if hold := storedRunHold(t, f.store, runID); hold != nil {
		t.Fatalf("the refused dispatch still holds: %+v", hold)
	}
	if launchWaiting(t, f.store, id) {
		t.Fatal("the refused dispatch still holds a place in the memory order")
	}
	if _, started := driver.StartSpec(id); started {
		t.Fatal("the refused dispatch started")
	}

	// The same wait as the first pass, inside the hold's refresh interval:
	// the hold the notice cleared must not pace it away.
	recordBudget(t, f.store, 3072)
	if _, err := engine.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if hold := storedRunHold(t, f.store, runID); hold == nil || hold.Reason != domain.HoldMachineCapacity ||
		!launchWaiting(t, f.store, id) {
		t.Fatalf("hold = %+v, want the dispatch waiting for memory again", hold)
	}
	if item, _ := memoryNotice(t, f.store, runID); item.Status == domain.StatusOpen {
		t.Fatal("a dispatch the budget fits again left its can-never-fit notice open")
	}

	recordBudget(t, f.store, 8192)
	if _, err := engine.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, started := driver.StartSpec(id); !started {
		t.Fatal("the raised budget did not admit the dispatch")
	}
}

// TestRaisedBudgetAdmitsAWaitingDispatch: a dispatch waiting for memory starts
// at the next pass once a recorded budget fits it beside the memory already
// held, and leaves its hold and the order.
func TestRaisedBudgetAdmitsAWaitingDispatch(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newSpecificationFixture(t, false, 4)
	f.launchBound = ward.LaunchBound{BudgetMiB: 3072, RuntimeVersion: ward.RuntimeNone}
	driver := f.newDriver(t)
	runID := domain.RunID("specification-run")
	id := specificationInvocationID(runID, 1)
	scriptSpecification(t, driver, id, 0)
	f.submit(t)
	engine := f.newEngine(t, driver)
	fillMemory(t, f.store, runID, 3072, 2048, *f.now)
	if _, err := engine.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if !launchWaiting(t, f.store, id) {
		t.Fatal("the dispatch did not wait for memory")
	}

	recordBudget(t, f.store, 4096)
	if _, err := engine.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, started := driver.StartSpec(id); !started {
		t.Fatal("the raised budget did not admit the dispatch")
	}
	if hold := storedRunHold(t, f.store, runID); hold != nil {
		t.Fatalf("the admitted dispatch still holds: %+v", hold)
	}
	if launchWaiting(t, f.store, id) {
		t.Fatal("the admitted dispatch still waits for memory")
	}
}

// TestLoweredBudgetKeepsARunningDispatch: a budget lowered below a running
// execution's reservation revokes nothing. The running launch raises no
// notice, its replayed admission still passes the gate, and its memory still
// counts, so the next launch waits until it frees.
func TestLoweredBudgetKeepsARunningDispatch(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newSpecificationFixture(t, false, 4)
	f.launchBound = ward.LaunchBound{BudgetMiB: 4096, RuntimeVersion: ward.RuntimeNone}
	driver := f.newDriver(t)
	runID := domain.RunID("specification-run")
	id := specificationInvocationID(runID, 1)
	// Pending inspects keep the invocation running, holding its slot and its
	// reservation, across the passes below.
	scriptSpecification(t, driver, id, 100)
	f.submit(t)
	engine := f.newEngine(t, driver)
	if _, err := engine.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, started := driver.StartSpec(id); !started {
		t.Fatal("the dispatch did not start under the startup budget")
	}

	recordBudget(t, f.store, 1024)
	if _, err := engine.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, found := memoryNotice(t, f.store, runID); found {
		t.Fatal("the lowered budget raised a notice on running work")
	}
	// The gate a dispatch replayed after a crash runs (dispatchAdmittedIntent).
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		stored, found, err := tx.LookupExecutionAdmission(ctx, id)
		if err != nil || !found {
			return errors.Join(err, errors.New("no recorded admission"))
		}
		run, err := tx.GetRun(ctx, runID)
		if err != nil {
			return err
		}
		memory, err := engine.executionMemory(ctx, &tx.ReadTx, run, stored)
		if err != nil {
			return err
		}
		if memory == nil || memory.BudgetMiB != 1024 {
			return errors.New("the replay was not gated under the recorded budget")
		}
		return tx.RequireExecutionMemory(ctx, stored, memory)
	}); err != nil {
		t.Fatalf("replayed admission under the lowered budget: %v", err)
	}
	next := domain.LaunchReservation{
		InvocationID: "inv-next", RunID: runID, Kind: domain.ReservationVerification, MemoryMiB: 512,
	}
	var refusal *domain.MachineCapacityError
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.RequireMachineCapacity(ctx, 1024, next, *f.now)
	}); !errors.As(err, &refusal) || refusal.Hold.ReservedMiB != 2048 {
		t.Fatalf("next launch = %v, want a wait counting the running 2048 MiB", err)
	}
}

// TestRecordedBudgetBoundsAnUnbudgetedInstance: an instance started with no
// budget, as a fake composition may, reserves under one recorded while it
// runs, from the next pass, with no per-VM overhead: a budget of exactly the
// launch's size admits it.
func TestRecordedBudgetBoundsAnUnbudgetedInstance(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newSpecificationFixture(t, false, 4)
	f.launchBound = ward.LaunchBound{RuntimeVersion: ward.RuntimeNone}
	driver := f.newDriver(t)
	runID := domain.RunID("specification-run")
	id := specificationInvocationID(runID, 1)
	scriptSpecification(t, driver, id, 0)
	f.submit(t)
	engine := f.newEngine(t, driver)
	recordBudget(t, f.store, 1024)
	if _, err := engine.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if item, found := memoryNotice(t, f.store, runID); !found ||
		!strings.Contains(item.Reason, "2048 MiB declared plus 0 MiB per-VM overhead exceeds the 1024 MiB budget") {
		t.Fatalf("notice = %+v, %t; want a refusal under the recorded budget", item, found)
	}

	recordBudget(t, f.store, 2048)
	if _, err := engine.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, started := driver.StartSpec(id); !started {
		t.Fatal("a recorded budget of exactly the launch's size did not admit it")
	}
}

// TestLoweredBudgetRefusesAWaitingVerificationForGood: a verification job
// waiting for memory that a lowered budget can no longer fit raises its
// notice, leaving its hold and the order. A budget it fits beside the memory
// held puts it back to waiting and retires the notice; one raised further
// reserves it.
func TestLoweredBudgetRefusesAWaitingVerificationForGood(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newSpecificationFixture(t, false, 4)
	f.submit(t)
	runID := domain.RunID("specification-run")
	run, err := f.run(runID)
	if err != nil {
		t.Fatal(err)
	}
	size := ward.DefaultLaunchSize(ward.LaunchVerification).MemoryMiB
	budget := size + 512
	w := &productionPublicationWorkflow{
		store: f.store, attention: f.signet, now: func() time.Time { return *f.now },
		launchBound: ward.LaunchBound{BudgetMiB: budget, RuntimeVersion: ward.RuntimeNone},
	}
	task := productionPublicationTask{RunID: runID, ProjectID: run.ProjectID, VerificationID: "inv-verification"}
	id := task.verificationInvocationID()
	fillMemory(t, f.store, runID, budget, 1024, *f.now)
	reserve := func() error {
		t.Helper()
		_, err := w.reserveVerification(ctx, task, productionBinding{})
		if err != nil {
			if holdErr := w.recordRetryableHold(ctx, task, err); holdErr != nil {
				t.Fatal(holdErr)
			}
		}
		return err
	}
	if err := reserve(); !errors.Is(err, domain.ErrMachineCapacity) || !launchWaiting(t, f.store, id) {
		t.Fatalf("reserve = %v, want the job waiting for memory", err)
	}
	if hold := storedRunHold(t, f.store, runID); hold == nil || hold.Reason != domain.HoldMachineCapacity {
		t.Fatalf("hold = %+v, want machine_capacity", hold)
	}

	recordBudget(t, f.store, size-1)
	if err := reserve(); !launchMemoryRefusal(err) {
		t.Fatalf("reserve under the lowered budget = %v, want a refusal for good", err)
	}
	if _, found := memoryNotice(t, f.store, runID); !found {
		t.Fatal("the lowered budget raised no notice")
	}
	if hold := storedRunHold(t, f.store, runID); hold != nil {
		t.Fatalf("the refused job still holds: %+v", hold)
	}
	if launchWaiting(t, f.store, id) {
		t.Fatal("the refused job still holds a place in the memory order")
	}

	recordBudget(t, f.store, budget)
	if err := reserve(); !errors.Is(err, domain.ErrMachineCapacity) || !launchWaiting(t, f.store, id) {
		t.Fatalf("reserve = %v, want the job waiting for memory again", err)
	}
	if item, _ := memoryNotice(t, f.store, runID); item.Status == domain.StatusOpen {
		t.Fatal("a job the budget fits again left its can-never-fit notice open")
	}

	recordBudget(t, f.store, 2*budget)
	if err := reserve(); err != nil {
		t.Fatalf("reserve under the raised budget: %v", err)
	}
}

// TestLoweredBudgetRefusesAWaitingReviewForGood: a review waiting for memory
// that a lowered budget can no longer fit is refused for good, and the
// notice takes it out of its hold and the order. A budget it fits beside the
// memory held puts it back to waiting and retires the notice; one raised
// further admits it.
func TestLoweredBudgetRefusesAWaitingReviewForGood(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newSpecificationFixture(t, false, 4)
	f.submit(t)
	runID := domain.RunID("specification-run")
	run, err := f.run(runID)
	if err != nil {
		t.Fatal(err)
	}
	recordFakeReviewIdentities(t, f.store)
	size := ward.DefaultLaunchSize(ward.LaunchReview).MemoryMiB
	budget := size + 512
	w := &productionPublicationWorkflow{
		store: f.store, attention: f.signet, reviewAdmitter: fake.AdmitReview,
		now:         func() time.Time { return *f.now },
		launchBound: ward.LaunchBound{BudgetMiB: budget, RuntimeVersion: ward.RuntimeNone},
	}
	id := ProductionReviewInvocationID(runID, 1)
	fillMemory(t, f.store, runID, budget, 1024, *f.now)
	_, err = w.ensureReviewAdmission(ctx, domain.RoleReviewer, runID, id)
	if !reviewCapacityRefusal(err) {
		t.Fatalf("admission = %v, want a wait for memory", err)
	}
	if err := w.holdReviewForCapacity(ctx, runID, id, err); err != nil {
		t.Fatal(err)
	}
	if !launchWaiting(t, f.store, id) {
		t.Fatal("the review did not join the memory order")
	}

	recordBudget(t, f.store, size-1)
	_, err = w.ensureReviewAdmission(ctx, domain.RoleReviewer, runID, id)
	if !launchMemoryRefusal(err) || reviewCapacityRefusal(err) {
		t.Fatalf("admission under the lowered budget = %v, want a refusal for good", err)
	}
	// reconcileReviewGate's handling of a refusal for good.
	if err := noticeLaunchMemoryRefusal(ctx, f.store, f.signet, &w.holdPace, runID, id, run.ProjectID, err); err != nil {
		t.Fatal(err)
	}
	if hold := storedRunHold(t, f.store, runID); hold != nil {
		t.Fatalf("the refused review still holds: %+v", hold)
	}
	if launchWaiting(t, f.store, id) {
		t.Fatal("the refused review still holds a place in the memory order")
	}
	if _, found := memoryNotice(t, f.store, runID); !found {
		t.Fatal("the lowered budget raised no notice")
	}

	// The same wait as before, inside the hold's refresh interval: the hold
	// the notice cleared must not pace it away.
	recordBudget(t, f.store, budget)
	_, err = w.ensureReviewAdmission(ctx, domain.RoleReviewer, runID, id)
	if !errors.Is(err, domain.ErrMachineCapacity) {
		t.Fatalf("admission = %v, want a wait for memory again", err)
	}
	if err := w.holdReviewForCapacity(ctx, runID, id, err); err != nil {
		t.Fatal(err)
	}
	if hold := storedRunHold(t, f.store, runID); hold == nil || hold.Reason != domain.HoldMachineCapacity ||
		!launchWaiting(t, f.store, id) {
		t.Fatalf("hold = %+v, want the review waiting for memory again", hold)
	}
	if item, _ := memoryNotice(t, f.store, runID); item.Status == domain.StatusOpen {
		t.Fatal("a review the budget fits again left its can-never-fit notice open")
	}

	recordBudget(t, f.store, 2*budget)
	if _, err := w.ensureReviewAdmission(ctx, domain.RoleReviewer, runID, id); err != nil {
		t.Fatalf("admission under the raised budget: %v", err)
	}
}
