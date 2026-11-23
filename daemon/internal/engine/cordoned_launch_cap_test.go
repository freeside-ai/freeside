package engine

import (
	"fmt"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

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
