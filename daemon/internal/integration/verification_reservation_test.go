package integration_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// memoryHolder is the invocation of another launch's reservation that a test
// holds to fill the memory budget.
const memoryHolder domain.InvocationID = "inv-memory-holder"

var errProbeRolledBack = errors.New("memory probe rolled back")

// budgetMemory makes the next engine reserve under a recorded budget of
// budgetMiB, with no per-VM overhead, as a composition that starts no VM.
// Launches before it ran unbudgeted and reserved nothing.
func (p *productionPublicationHarness) budgetMemory(t *testing.T, budgetMiB int) {
	t.Helper()
	p.machineCapacity = &ward.LaunchBound{RuntimeVersion: ward.RuntimeNone}
	p.recordMemoryBudget(t, budgetMiB)
}

// recordMemoryBudget records the instance's budget as
// freesided set-memory-budget does; an engine bounded by machineCapacity
// applies it from its next pass.
func (p *productionPublicationHarness) recordMemoryBudget(t *testing.T, budgetMiB int) {
	t.Helper()
	if err := p.store.WriteInternal(p.ctx, func(tx *store.InternalTx) error {
		return tx.RecordMemoryBudget(p.ctx, budgetMiB, p.now.UTC())
	}); err != nil {
		t.Fatal(err)
	}
}

// holdMemory reserves memoryMiB for another launch, so a verification that
// needs more than what is left of budgetMiB waits.
func (p *productionPublicationHarness) holdMemory(t *testing.T, budgetMiB, memoryMiB int) {
	t.Helper()
	holder := domain.LaunchReservation{
		InvocationID: memoryHolder, RunID: p.runID, Kind: domain.ReservationVerification, MemoryMiB: memoryMiB,
	}
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		return tx.RequireMachineCapacity(p.ctx, budgetMiB, holder, p.now.UTC())
	}); err != nil {
		t.Fatal(err)
	}
}

// freeMemory ends the reservation holdMemory took.
func (p *productionPublicationHarness) freeMemory(t *testing.T) {
	t.Helper()
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		return tx.ReleaseVerificationReservation(p.ctx, memoryHolder, p.now.UTC())
	}); err != nil {
		t.Fatal(err)
	}
}

// memoryFits reports whether a launch of memoryMiB would be granted under
// budgetMiB now, beside every reservation held. The probe's own reservation
// is rolled back. It is meaningful only while no launch waits ahead of it.
func (p *productionPublicationHarness) memoryFits(t *testing.T, budgetMiB, memoryMiB int) bool {
	t.Helper()
	probe := domain.LaunchReservation{
		InvocationID: "inv-memory-probe", RunID: p.runID, Kind: domain.ReservationVerification, MemoryMiB: memoryMiB,
	}
	err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		if err := tx.RequireMachineCapacity(p.ctx, budgetMiB, probe, p.now.UTC()); err != nil {
			return err
		}
		return errProbeRolledBack
	})
	switch {
	case errors.Is(err, errProbeRolledBack):
		return true
	case errors.Is(err, domain.ErrMachineCapacity):
		return false
	}
	t.Fatalf("memory probe: %v", err)
	return false
}

// probeMemoryOnRead makes the room report, at each recipe read, whether a
// launch of 1 MiB still fit under budgetMiB: false means the room's job held
// the budget while its container ran.
func (p *productionPublicationHarness) probeMemoryOnRead(t *testing.T, budgetMiB int) *[]bool {
	t.Helper()
	var fits []bool
	recipe := p.room.recipe
	p.room.read = func(context.Context) ([]byte, error) {
		fits = append(fits, p.memoryFits(t, budgetMiB, 1))
		return bytes.Clone(recipe), nil
	}
	return &fits
}

func (p *productionPublicationHarness) launchWaiting(t *testing.T, id domain.InvocationID) bool {
	t.Helper()
	var waiting bool
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		waiting, err = tx.LaunchWaiting(p.ctx, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return waiting
}

// capacityHold returns the invocation the run's machine_capacity hold names,
// failing when the run holds for anything else or not at all.
func (p *productionPublicationHarness) capacityHold(t *testing.T) domain.InvocationID {
	t.Helper()
	var (
		hold    domain.RunHoldObservation
		present bool
	)
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		hold, present, err = tx.GetRunHold(p.ctx, p.runID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !present || hold.Reason != domain.HoldMachineCapacity || hold.InvocationID == nil {
		t.Fatalf("run hold = %+v, %t; want machine_capacity naming an invocation", hold, present)
	}
	return *hold.InvocationID
}

func (p *productionPublicationHarness) runHeld(t *testing.T) bool {
	t.Helper()
	var present bool
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		_, present, err = tx.GetRunHold(p.ctx, p.runID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return present
}

// memoryNotice reads the run's first can-never-fit notice.
func (p *productionPublicationHarness) memoryNotice(t *testing.T) (domain.AttentionItem, bool) {
	t.Helper()
	id := domain.ItemID(fmt.Sprintf("launch-memory-refused-1-%s", p.runID))
	item, err := p.attention.GetAttentionItem(p.ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return domain.AttentionItem{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return item.Item, true
}

// startHeadChangeReentry pushes to the published pull request and starts the
// re-entry that verifies the pushed head, scripting its clean review.
func (p *productionPublicationHarness) startHeadChangeReentry(t *testing.T) (domain.AttentionItem, string) {
	t.Helper()
	pushed := p.pushToPullRequest(t, "FOLLOWUP.md", "pushed after readiness\n")
	superseded, started := p.invalidateReady(t, domain.ReadinessInvalidation{
		Reason: domain.ReadinessInvalidationHeadChanged, Bound: p.replay.HeadSHA, Observed: pushed,
	})
	if !started {
		t.Fatal("a head change started no re-entry")
	}
	p.scriptCleanReview(2, p.baseSHA, pushed)
	return superseded, pushed
}

// TestReentryVerificationWaitsForMemory: a re-entry's verification reserves
// its memory before its room. With the budget full it builds no room and
// waits in the memory order under its own invocation, holding the run as
// machine_capacity rather than stopping the cycle; a budget lowered below its
// size refuses it for good without stopping the cycle either. Once memory
// frees it holds the reservation while its room runs and releases it after.
func TestReentryVerificationWaitsForMemory(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.publishReady(t)
	size := p.room.sizes[0].MemoryMiB
	p.budgetMemory(t, size)
	p.holdMemory(t, size, size)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	superseded, pushed := p.startHeadChangeReentry(t)
	rooms := len(p.room.sizes)

	result, err := p.reconcileLanes()
	if err != nil || result.ReadyItemsCreated != 0 || result.BlockedItemsCreated != 0 {
		t.Fatalf("full budget = %#v, %v; want the cycle left waiting", result, err)
	}
	if len(p.room.sizes) != rooms {
		t.Fatalf("a re-entry waiting for memory built %d rooms", len(p.room.sizes)-rooms)
	}
	id := p.capacityHold(t)
	if !strings.HasPrefix(string(id), "verify-reentry-") || !p.launchWaiting(t, id) {
		t.Fatalf("hold names %q (waiting %t), want the re-entry's own verification waiting",
			id, p.launchWaiting(t, id))
	}

	p.recordMemoryBudget(t, size-1)
	p.now = p.now.Add(time.Hour)
	result, err = p.reconcileLanes()
	if err != nil || result.ReadyItemsCreated != 0 || result.BlockedItemsCreated != 0 {
		t.Fatalf("lowered budget = %#v, %v; want the cycle left pending", result, err)
	}
	if item, found := p.memoryNotice(t); !found || item.Status != domain.StatusOpen {
		t.Fatalf("notice = %+v, %t; want an open can-never-fit notice", item, found)
	}
	if p.runHeld(t) || p.launchWaiting(t, id) {
		t.Fatal("the re-entry refused for good still holds, or still waits in the memory order")
	}

	p.recordMemoryBudget(t, size)
	fits := p.probeMemoryOnRead(t, size)
	p.freeMemory(t)
	p.now = p.now.Add(time.Hour)
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 1 {
		t.Fatalf("freed budget = %#v, %v", result, err)
	}
	p.assertReentered(t, superseded, 2, p.baseSHA, pushed)
	if item, _ := p.memoryNotice(t); item.Status == domain.StatusOpen {
		t.Fatal("a re-entry the budget fits again left its notice open")
	}
	if len(*fits) != 1 || (*fits)[0] {
		t.Fatalf("memory fit during the room's recipe read = %v, want the room holding it", *fits)
	}
	if !p.memoryFits(t, size, size) || p.launchWaiting(t, id) || p.runHeld(t) {
		t.Fatal("the re-entry kept memory, its wait, or its hold after verifying")
	}
}

// TestReentryVerificationKeepsMemoryUntilItsRoomIsProvedGone: a room that
// fails to read its recipe releases the reservation it took; one that cannot
// prove its container gone keeps it, and the next room that returns proves it
// and releases it.
func TestReentryVerificationKeepsMemoryUntilItsRoomIsProvedGone(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.publishReady(t)
	size := p.room.sizes[0].MemoryMiB
	p.budgetMemory(t, size)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.startHeadChangeReentry(t)

	p.room.read = func(context.Context) ([]byte, error) { return nil, errors.New("recipe read timed out") }
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 ||
		result.BlockedItemsCreated != 0 {
		t.Fatalf("failed recipe read = %#v, %v; want the cycle retried", result, err)
	}
	if !p.memoryFits(t, size, size) {
		t.Fatal("a room that failed to read its recipe kept its memory")
	}

	p.room.read = nil
	p.now = p.now.Add(time.Hour)
	p.room.runErr = fmt.Errorf("remove verification container: %w", ward.ErrVerificationAbsenceUnproven)
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 ||
		result.BlockedItemsCreated != 0 {
		t.Fatalf("unproven absence = %#v, %v; want the cycle retried", result, err)
	}
	if p.memoryFits(t, size, 1) {
		t.Fatal("a room whose container was not proved gone released its memory")
	}

	p.room.runErr = nil
	p.now = p.now.Add(time.Hour)
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 1 {
		t.Fatalf("proved absence = %#v, %v", result, err)
	}
	if !p.memoryFits(t, size, size) {
		t.Fatal("the room that proved its container gone kept the memory")
	}
}

// TestHandoffVerificationWaitsForMemory: a handoff reserves for its merge's
// verification under its own invocation, verify-handoff-<publication>. With
// the budget full it waits under that invocation; a budget lowered below its
// size refuses it for good and takes that invocation's wait out of the order;
// a raised budget lets it verify and publish.
func TestHandoffVerificationWaitsForMemory(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	advanced, _ := p.advanceBaseBeforePublication(t, "UPSTREAM.md", "upstream change\n")
	review := ward.DefaultLaunchSize(ward.LaunchReview).MemoryMiB
	var size, budget int
	// After the candidate's own verification, which ran unbudgeted, fill
	// the budget so the review still fits but no verification does.
	p.machineCapacity = &ward.LaunchBound{RuntimeVersion: ward.RuntimeNone}
	filled := false
	p.workflow = p.newEngine(t, productionCrashSeams{afterVerification: func() error {
		if filled {
			return nil
		}
		filled = true
		size = p.room.sizes[len(p.room.sizes)-1].MemoryMiB
		budget = size + review - 1
		p.recordMemoryBudget(t, budget)
		p.holdMemory(t, budget, review)
		return nil
	}}, true)

	var id domain.InvocationID
	for pass := 1; ; pass++ {
		p.now = p.now.Add(time.Minute)
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if p.runHeld(t) {
			id = p.capacityHold(t)
			break
		}
		if pass == 3 {
			t.Fatal("the handoff never waited for memory")
		}
	}
	var publication domain.InvocationID
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		publication, err = tx.CurrentPublicationInvocationID(p.ctx, p.runID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if want := domain.InvocationID("verify-handoff-" + string(publication)); id != want || !p.launchWaiting(t, id) {
		t.Fatalf("hold names %q (waiting %t), want the handoff's %q waiting", id, p.launchWaiting(t, id), want)
	}
	if len(p.room.sizes) != 1 {
		t.Fatalf("rooms built = %d, want only the candidate's", len(p.room.sizes))
	}
	if refs, prs := p.forge.Counts(); refs != 0 || prs != 0 {
		t.Fatalf("a waiting handoff published %d refs, %d prs", refs, prs)
	}

	p.recordMemoryBudget(t, size-1)
	p.now = p.now.Add(time.Minute)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	if item, found := p.memoryNotice(t); !found || item.Status != domain.StatusOpen {
		t.Fatalf("notice = %+v, %t; want an open can-never-fit notice", item, found)
	}
	if p.runHeld(t) || p.launchWaiting(t, id) {
		t.Fatal("the handoff refused for good still holds, or still waits in the memory order")
	}

	p.recordMemoryBudget(t, size+review)
	p.reconcileUntilReady(t, 3)
	p.assertHandoffVerified(t, advanced)
	if item, _ := p.memoryNotice(t); item.Status == domain.StatusOpen {
		t.Fatal("a handoff the budget fits again left its notice open")
	}
	if !p.memoryFits(t, size+review, size) {
		t.Fatal("the handoff kept its memory after verifying")
	}
}
