package engine

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	execfake "github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

func recordCordonedCap(t *testing.T, st *store.Store, maxConcurrent int, at time.Time) {
	t.Helper()
	value, err := json.Marshal(domain.CordonedLaunchSettings{MaxConcurrent: maxConcurrent})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(t.Context(), func(tx *store.InternalTx) error {
		_, err := tx.AppendInstanceSettingsRevision(t.Context(), domain.InstanceSettingsInput{
			Section: domain.InstanceSectionCordonedLaunches, Value: value, SetBy: "cli:operator",
		}, at)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// TestCordonedDispatchWaitsForACapAndReservesNoMemory: with no cap set a
// cordoned dispatch stays pending under a cordoned_launch_cap hold, in the
// cordoned launch order; once a cap is set it starts, though the memory
// budget is full, because a cordoned launch reserves no memory, and starting
// clears the hold and the wait (#3471).
func TestCordonedDispatchWaitsForACapAndReservesNoMemory(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newSpecificationFixture(t, false, 4)
	f.backend = execfake.RunnerBackend{
		BackendName: string(domain.BackendCordonedHostProcess), Caps: exec.NewCapabilitySet(exec.CapPostExitExport),
	}
	f.launchBound = ward.LaunchBound{BudgetMiB: 3072, RuntimeVersion: ward.RuntimeNone}
	driver := f.newDriver(t)
	runID := domain.RunID("specification-run")
	id := specificationInvocationID(runID, 1)
	scriptSpecification(t, driver, id, 0)
	f.submit(t)
	engine := f.newEngine(t, driver)
	fillMemory(t, f.store, runID, 3072, 3072, *f.now)
	if _, err := engine.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if hold := storedRunHold(t, f.store, runID); hold == nil || hold.Reason != domain.HoldCordonedLaunchCap {
		t.Fatalf("hold = %+v, want cordoned_launch_cap", hold)
	}
	if _, started := driver.StartSpec(id); started {
		t.Fatal("a cordoned dispatch started with no cap set")
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		_, resource, waiting, err := tx.LaunchWait(ctx, id)
		if err == nil && (!waiting || resource != store.LaunchWaitCordonedLaunch) {
			t.Errorf("wait = %q, %t; want the cordoned launch order", resource, waiting)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	recordCordonedCap(t, f.store, 1, *f.now)
	if _, err := engine.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, started := driver.StartSpec(id); !started {
		t.Fatal("a cordoned dispatch under the cap did not start beside a full memory budget")
	}
	if hold := storedRunHold(t, f.store, runID); hold != nil {
		t.Fatalf("hold after the start = %+v", hold)
	}
	if launchWaiting(t, f.store, id) {
		t.Fatal("the started dispatch still waits")
	}
}

// TestLaunchOrderWaitsOnTheRefusingResource: a cap refusal joins the
// cordoned launch order and a memory refusal the memory order, and a launch
// whose refusal changes resource moves at once, inside the refresh interval.
func TestLaunchOrderWaitsOnTheRefusingResource(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newSpecificationFixture(t, false, 4)
	f.submit(t)
	runID := domain.RunID("specification-run")
	id := specificationInvocationID(runID, 1)
	waitsOn := func() store.LaunchWaitResource {
		t.Helper()
		var resource store.LaunchWaitResource
		if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
			var (
				waiting bool
				err     error
			)
			_, resource, waiting, err = tx.LaunchWait(ctx, id)
			if err == nil && !waiting {
				t.Error("the launch is not waiting")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return resource
	}
	start := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	capRefusal := fmt.Errorf("admit: %w", domain.ErrCordonedLaunchCap)
	if err := recordLaunchOrder(ctx, f.store, f.signet, runID, id, capRefusal, start); err != nil {
		t.Fatal(err)
	}
	if got := waitsOn(); got != store.LaunchWaitCordonedLaunch {
		t.Fatalf("after a cap refusal the launch waits on %q", got)
	}
	if err := recordLaunchOrder(ctx, f.store, f.signet, runID, id, memoryRefusal(0), start.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := waitsOn(); got != store.LaunchWaitMemory {
		t.Fatalf("after a memory refusal inside the refresh the launch waits on %q", got)
	}
}
