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

// cordonedImplementer is a specification fixture whose run has passed its
// specification and whose implementer, under unattended with no writer
// class set, resolves to a fake cordon holding a passing proof. Its launch
// bound is 3072 MiB and its implementer fails after ten running inspects.
type cordonedImplementer struct {
	specificationFixture
	driver *execfake.StageDriver
	engine *Engine
	cordon execfake.RunnerBackend
	runID  domain.RunID
	id     domain.InvocationID
}

func newCordonedImplementer(t *testing.T) cordonedImplementer {
	t.Helper()
	ctx := t.Context()
	f := newSpecificationFixture(t, false, 4)
	f.launchBound = ward.LaunchBound{BudgetMiB: 3072, RuntimeVersion: ward.RuntimeNone}
	driver := f.newDriver(t)
	scriptSpecification(t, driver, specificationInvocationID("specification-run", 1), 0)
	f.submit(t)
	engine := f.newEngine(t, driver)
	if result, err := engine.Reconcile(ctx); err != nil || result.ResultsAccepted != 1 {
		t.Fatalf("specification reconcile = %+v, %v", result, err)
	}
	// Production dispatch is unattended. The project sets no writer class,
	// so the implementer resolves to the cordon, which holds a passing
	// proof of its configuration.
	engine.admission.environment.OperatingMode = domain.ModeUnattended
	engine.admission.environment.ReviewConfigurationDigest = "sha256:review-config"
	setWriterClass(engine, nil)
	seedUnattendedAdmissionTrust(t, f.store, engine.admission.environment.Base)
	cordon := execfake.RunnerBackend{
		BackendName: string(domain.BackendCordonedHostProcess),
		Caps:        exec.NewCapabilitySet(domain.CordonedCapabilities()...),
	}
	engine.admission.cordon, engine.admission.cordonConfigurationDigest = cordon, cordon.ConfigurationDigest()
	proof, err := domain.NewBackendConformance(domain.BackendConformanceInput{
		Backend: domain.BackendCordonedHostProcess, Outcome: domain.ConformancePassed,
		ConfigurationDigest: cordon.ConfigurationDigest(), Capabilities: domain.CordonedCapabilities(),
		ProvedAt: unattendedExportEpoch,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.RecordBackendConformance(ctx, domain.DaemonHostID, proof)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	runID := domain.RunID("implementation-run")
	id := productionInvocationID(runID)
	driver.Script(id, execfake.StageScript{RunningInspects: 10, Outcome: execfake.OutcomeFail})
	return cordonedImplementer{
		specificationFixture: f, driver: driver, engine: engine, cordon: cordon, runID: runID, id: id,
	}
}

// TestCordonedDispatchWaitsForACapAndReservesNoMemory: with no cap set an
// implementer the admitter places on the cordon stays pending under a
// cordoned_launch_cap hold, in the cordoned launch order; once a cap is set
// it starts, though the memory budget is full, because a cordoned launch
// reserves no memory, and starting clears the hold and the wait (#3471,
// #3472).
func TestCordonedDispatchWaitsForACapAndReservesNoMemory(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := newCordonedImplementer(t)
	f, driver, engine, cordon, runID, id := c.specificationFixture, c.driver, c.engine, c.cordon, c.runID, c.id
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
	spec, started := driver.StartSpec(id)
	if !started {
		t.Fatal("a cordoned dispatch under the cap did not start beside a full memory budget")
	}
	if spec.RunnerClass != domain.BackendCordonedHostProcess {
		t.Fatalf("started with runner class %q, want the cordon", spec.RunnerClass)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		admission, err := tx.GetExecutionAdmissionRecord(ctx, id)
		if err == nil && (admission.BackendConfigurationDigest != cordon.ConfigurationDigest() ||
			admission.EgressProfile != domain.EgressHostOpen) {
			t.Errorf("admission binds %s with %s, want the cordon's %s with host_open",
				admission.BackendConfigurationDigest, admission.EgressProfile, cordon.ConfigurationDigest())
		}
		return err
	}); err != nil {
		t.Fatal(err)
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
