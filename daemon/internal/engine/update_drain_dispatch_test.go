package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	specifyfake "github.com/freeside-ai/freeside/daemon/internal/specify/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// drainDispatchFixture is a specification fixture whose writer attempt the
// test drains, before or after it is recorded.
type drainDispatchFixture struct {
	fixture      specificationFixture
	engine       *Engine
	invocationID domain.InvocationID
	drain        UpdateDrain
}

func newDrainDispatchFixture(t *testing.T, driver func(exec.StageDriver) exec.StageDriver) *drainDispatchFixture {
	t.Helper()
	f := newSpecificationFixture(t, false, 4)
	fake := f.newDriver(t)
	invocationID := specificationInvocationID("specification-run", 1)
	if err := specifyfake.Script(fake, invocationID, 0, 0, specify.Output{Specification: &specify.Specification{
		Summary: "ready", Body: "# Specification\n\nImplement.", Addressals: []specify.Addressal{},
	}}); err != nil {
		t.Fatal(err)
	}
	var stageDriver exec.StageDriver = fake
	if driver != nil {
		stageDriver = driver(fake)
	}
	return &drainDispatchFixture{
		fixture: f, engine: f.newEngine(t, stageDriver), invocationID: invocationID,
		drain: UpdateDrain{Store: f.store, Build: drainOldBuild, Now: func() time.Time { return *f.now }},
	}
}

func (h *drainDispatchFixture) dispatch(t *testing.T) int {
	t.Helper()
	started, err := h.engine.dispatchPendingInvocations(t.Context())
	if err != nil {
		t.Fatalf("dispatch = %v, want the pass to continue", err)
	}
	return started
}

func (h *drainDispatchFixture) attemptRecorded(t *testing.T) bool {
	t.Helper()
	var recorded bool
	if err := h.fixture.store.Read(t.Context(), func(tx *store.ReadTx) error {
		run, err := tx.GetRun(t.Context(), "specification-run")
		recorded = err == nil && attemptRecorded(run, h.invocationID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return recorded
}

// TestUpdateDrainHoldsANewWriterLaunch: in both operating modes a drain holds
// a writer attempt not yet recorded, as an update_drain hold on its run, and
// a submission made while draining is accepted. The lift ends the hold, and
// the next pass starts the attempt.
func TestUpdateDrainHoldsANewWriterLaunch(t *testing.T) {
	t.Parallel()
	for _, mode := range []domain.OperatingMode{domain.ModeAttendedDev, domain.ModeUnattended} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			h := newDrainDispatchFixture(t, nil)
			h.engine.admission.environment.OperatingMode = mode
			if _, err := h.drain.Request(t.Context()); err != nil {
				t.Fatal(err)
			}
			h.fixture.submit(t)
			if started := h.dispatch(t); started != 0 {
				t.Fatalf("started %d while draining", started)
			}
			if hold := storedRunHold(t, h.fixture.store, "specification-run"); hold == nil ||
				hold.Reason != domain.HoldUpdateDrain {
				t.Fatalf("hold = %+v, want update_drain", hold)
			}
			if h.attemptRecorded(t) {
				t.Fatal("a drained dispatch recorded an attempt")
			}

			if _, err := h.drain.Cancel(t.Context()); err != nil {
				t.Fatal(err)
			}
			if hold := storedRunHold(t, h.fixture.store, "specification-run"); hold != nil {
				t.Fatalf("hold after the lift = %+v, want none", hold)
			}
			if mode == domain.ModeUnattended {
				// The unattended gate has its own conditions; the drain's
				// part ends with the lift.
				return
			}
			if started := h.dispatch(t); started != 1 {
				t.Fatalf("started %d after the lift, want 1", started)
			}
		})
	}
}

// failFirstStart refuses its first Start, leaving an attempt recorded and
// not started, as a daemon that crashed between the two would.
type failFirstStart struct {
	exec.StageDriver
	refused atomic.Bool
}

func (d *failFirstStart) Start(ctx context.Context, id domain.InvocationID, spec exec.StartSpec) error {
	if d.refused.CompareAndSwap(false, true) {
		return errors.New("injected start failure")
	}
	return d.StageDriver.Start(ctx, id, spec)
}

// TestUpdateDrainStartsAnAttemptRecordedBeforeIt: an attempt recorded before
// the drain is running work, so a pass under the drain still starts it.
func TestUpdateDrainStartsAnAttemptRecordedBeforeIt(t *testing.T) {
	t.Parallel()
	h := newDrainDispatchFixture(t, func(driver exec.StageDriver) exec.StageDriver {
		return &failFirstStart{StageDriver: driver}
	})
	h.fixture.submit(t)
	if _, err := h.engine.dispatchPendingInvocations(t.Context()); err == nil {
		t.Fatal("the injected start failure did not surface")
	}
	if !h.attemptRecorded(t) {
		t.Fatal("the failed start left no recorded attempt")
	}
	if _, err := h.drain.Request(t.Context()); err != nil {
		t.Fatal(err)
	}
	if started := h.dispatch(t); started != 1 {
		t.Fatalf("started %d under the drain, want the recorded attempt to start", started)
	}
	if hold := storedRunHold(t, h.fixture.store, "specification-run"); hold != nil {
		t.Fatalf("hold = %+v, want none", hold)
	}
}

// TestUpdateDrainBackstopsTheAttemptWrite: a drain committed after
// dispatch's read and before the attempt's write still appends no attempt,
// because the write's own transaction checks the drain again.
func TestUpdateDrainBackstopsTheAttemptWrite(t *testing.T) {
	t.Parallel()
	h := newDrainDispatchFixture(t, nil)
	h.fixture.submit(t)
	run, err := h.fixture.run("specification-run")
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Stages) == 0 {
		t.Fatal("the submitted run has no stage")
	}
	if _, err := h.drain.Request(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, _, _, err = h.engine.recordAttempt(
		t.Context(), run.ID, run.Stages[0].ID, h.invocationID, nil, "pending", nil)
	if !errors.Is(err, store.ErrUpdateDrainInForce) {
		t.Fatalf("attempt write under a drain = %v, want the drain refusal", err)
	}
	if h.attemptRecorded(t) {
		t.Fatal("the attempt write appended an attempt under a drain")
	}
}
