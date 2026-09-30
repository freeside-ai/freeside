package engine

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	execfake "github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	specifyfake "github.com/freeside-ai/freeside/daemon/internal/specify/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// withRaisedFloor reopens the fixture's store under an attended floor the
// recorded specification admission does not meet, so every admission
// re-gate refuses with a mutable ErrCapabilityBelowFloor until reopen
// restores the fixture's own floor.
func (f specificationFixture) withRaisedFloor(t *testing.T) specificationFixture {
	t.Helper()
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	raised := storetest.Open(t, f.dbPath, store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: domain.NewCapabilitySnapshot(
				domain.CapPostExitExport, domain.CapReadOnlyRemount),
		},
	})
	f.store = raised
	f.signet = signet.NewService(raised, signet.WithBlobStore(f.blobs),
		signet.WithClock(func() time.Time { return *f.now }))
	return f
}

func (f specificationFixture) runHold(t *testing.T) (domain.RunHoldObservation, bool) {
	t.Helper()
	var (
		hold  domain.RunHoldObservation
		found bool
	)
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		hold, found, err = tx.GetRunHold(t.Context(), "specification-run")
		return err
	}); err != nil {
		t.Fatalf("GetRunHold: %v", err)
	}
	return hold, found
}

func (f specificationFixture) requireHold(t *testing.T, want domain.RunHoldReason) {
	t.Helper()
	if hold, found := f.runHold(t); !found || hold.Reason != want {
		t.Fatalf("specification run hold = (%+v, found %t), want %s", hold, found, want)
	}
}

// scriptedSpecificationRefusal dispatches a specification attempt that
// reports running once and then completes, and holds its completion behind
// a raised admission floor. It returns the fixture on the raised floor with
// the refusal hold recorded, and the driver, which later passes must reuse:
// a fresh driver restarts the scripted inspection count.
func scriptedSpecificationRefusal(t *testing.T) (specificationFixture, *execfake.StageDriver) {
	t.Helper()
	f := newSpecificationFixture(t, true, 2)
	driver := f.newDriver(t)
	if err := specifyfake.Script(driver, specificationInvocationID("specification-run", 1), 0, 1, specify.Output{
		Specification: &specify.Specification{
			Summary: "The implementation contract is ready.", Body: "# Specification\n\nHold, then accept.",
			Addressals: []specify.Addressal{},
		},
	}); err != nil {
		t.Fatal(err)
	}
	f.submit(t)
	if _, err := f.newEngine(t, driver).Reconcile(t.Context()); err != nil {
		t.Fatalf("dispatch pass: %v", err)
	}
	if _, found := f.runHold(t); found {
		t.Fatal("a still-running attempt under a healthy policy recorded a hold")
	}

	f = f.withRaisedFloor(t)
	if _, err := f.newEngine(t, driver).Reconcile(t.Context()); err != nil {
		t.Fatalf("refused pass stopped the reconcile loop: %v", err)
	}
	f.requireHold(t, domain.HoldAdmissionPolicyRefused)
	return f, driver
}

// TestSpecificationAdmissionRefusalRecordsAndRecoveryClearsHold is issue
// #435's specification half: a completed attempt whose admission no longer
// clears current policy records the classified hold instead of an
// unexplained open run, and the pass that gets through after the policy
// recovers clears it and accepts the specification.
func TestSpecificationAdmissionRefusalRecordsAndRecoveryClearsHold(t *testing.T) {
	f, driver := scriptedSpecificationRefusal(t)

	f = f.reopen(t)
	if _, err := f.newEngine(t, driver).Reconcile(t.Context()); err != nil {
		t.Fatalf("recovered pass: %v", err)
	}
	if hold, found := f.runHold(t); found {
		t.Fatalf("hold after recovery = %+v, want none", hold)
	}
	if _, err := f.signet.GetAttentionItem(t.Context(), "spec-approval-implementation-run-1"); err != nil {
		t.Fatalf("recovered specification was not accepted: %v", err)
	}
}

// TestSpecificationRefusalRecoveryPreservesOtherHold: the recovery clear is
// scoped to the refusal class, so a hold with another cause keeps its row
// and its span.
func TestSpecificationRefusalRecoveryPreservesOtherHold(t *testing.T) {
	f, driver := scriptedSpecificationRefusal(t)
	otherFirst := f.now.Add(2 * time.Minute).UTC()
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.RecordRunHold(t.Context(), domain.RunHoldObservation{
			RunID: "specification-run", Reason: domain.HoldIdentityParallelism,
			FirstObservedAt: otherFirst, LastObservedAt: otherFirst,
		})
	}); err != nil {
		t.Fatalf("record other hold: %v", err)
	}

	f = f.reopen(t)
	if _, err := f.newEngine(t, driver).Reconcile(t.Context()); err != nil {
		t.Fatalf("recovered pass: %v", err)
	}
	hold, found := f.runHold(t)
	if !found || hold.Reason != domain.HoldIdentityParallelism || !hold.FirstObservedAt.Equal(otherFirst) {
		t.Fatalf("hold after recovery = (%+v, found %t), want HoldIdentityParallelism first-observed %s",
			hold, found, otherFirst)
	}
}

// TestSpecificationStillRunningRecoveryClearsHold: a pass that finds the
// attempt still running, with the collect gate passing, clears a refusal
// hold an earlier collect recorded.
func TestSpecificationStillRunningRecoveryClearsHold(t *testing.T) {
	f := newSpecificationFixture(t, true, 2)
	base := f.newDriver(t)
	id := specificationInvocationID("specification-run", 1)
	base.Script(id, execfake.StageScript{RunningInspects: 1 << 20, Outcome: execfake.OutcomeComplete})
	f.submit(t)
	var inspects atomic.Int64
	refusing := inspectRefusingDriver{
		StageDriver: base, inspect: &inspects,
		err: fmt.Errorf("inspect: authenticate current intent %s: %w", id, store.ErrBackendNotConformant),
	}
	if _, err := f.newEngine(t, refusing).Reconcile(t.Context()); err != nil {
		t.Fatalf("refused pass: %v", err)
	}
	f.requireHold(t, domain.HoldBackendNotConformant)

	if _, err := f.newEngine(t, base).Reconcile(t.Context()); err != nil {
		t.Fatalf("still-running pass: %v", err)
	}
	if hold, found := f.runHold(t); found {
		t.Fatalf("hold after a healthy still-running pass = %+v, want none", hold)
	}
}

// TestSpecificationWriteRefusalRecordsHold covers the three acceptance
// writes that re-read the admission inside their own transaction. In
// production that refusal is a race with the admission re-check just before
// it; calling each write directly on the raised floor drives the same exit
// without the race.
func TestSpecificationWriteRefusalRecordsHold(t *testing.T) {
	cases := []struct {
		name   string
		accept func(*Engine, domain.Run, specificationRequest, specify.Policy) (bool, error)
	}{
		{"research", func(e *Engine, run domain.Run, request specificationRequest, settings specify.Policy) (bool, error) {
			return e.acceptResearchRequests(t.Context(), run, request,
				[]specify.FetchRequest{{URL: "https://docs.example/guide", Purpose: "Read the guide."}}, settings)
		}},
		{"specification", func(e *Engine, run domain.Run, request specificationRequest, settings specify.Policy) (bool, error) {
			return e.acceptSpecification(t.Context(), run, request, specify.Specification{
				Summary: "The implementation contract is ready.", Body: "# Specification\n\nRefused write.",
				Addressals: []specify.Addressal{},
			}, settings)
		}},
		{"decisions", func(e *Engine, run domain.Run, request specificationRequest, _ specify.Policy) (bool, error) {
			return e.acceptSpecificationDecisions(t.Context(), run, request, []domain.Decision{{
				Question:    "What scope does the task cover?",
				WhyBlocking: "The specification cannot bound the work without its scope.",
				Options: []domain.DecisionOption{
					{Label: "Daemon only", Tradeoffs: "Narrow; no client change."},
					{Label: "Daemon and app", Tradeoffs: "Wider; couples two components."},
				},
				Recommendation: "Daemon only",
			}})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, driver := scriptedSpecificationRefusal(t)
			if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
				return tx.ClearRunHold(t.Context(), "specification-run")
			}); err != nil {
				t.Fatal(err)
			}
			e := f.newEngine(t, driver)
			var entry store.QueueEntry
			var run domain.Run
			if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
				var err error
				if entry, err = tx.GetOutbox(t.Context(),
					string(specificationInvocationID("specification-run", 1))); err != nil {
					return err
				}
				run, err = tx.GetRun(t.Context(), "specification-run")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			request, _, err := e.loadSpecificationBinding(t.Context(), entry)
			if err != nil {
				t.Fatalf("load binding: %v", err)
			}
			settings, err := specify.ParsePolicy(f.policy)
			if err != nil {
				t.Fatal(err)
			}
			accepted, err := tc.accept(e, run, request, settings)
			if err != nil || accepted {
				t.Fatalf("refused write = accepted %t, %v; want false, nil", accepted, err)
			}
			f.requireHold(t, domain.HoldAdmissionPolicyRefused)
		})
	}
}

// discussionAwaitingReply opens a discussion on the prepared specification
// and scripts its reply to report running the given number of times before
// it completes. It returns the driver, which later passes must reuse, and
// the discussion invocation, not yet dispatched.
func discussionAwaitingReply(
	t *testing.T, running int,
) (specificationFixture, *execfake.StageDriver, domain.InvocationID) {
	t.Helper()
	f := newSpecificationFixture(t, true, 4)
	driver := f.newDriver(t)
	if err := specifyfake.Script(driver, specificationInvocationID("specification-run", 1), 0, 0, specify.Output{
		Specification: &specify.Specification{
			Summary: "The bounded implementation plan is ready.", Body: "# Specification\n\nDiscuss it.",
			Addressals: []specify.Addressal{},
		},
	}); err != nil {
		t.Fatal(err)
	}
	f.submit(t)
	itemID := domain.ItemID("spec-approval-implementation-run-1")
	for pass := 1; pass <= 3; pass++ {
		if _, err := f.newEngine(t, driver).Reconcile(t.Context()); err != nil {
			t.Fatalf("prepare specification pass %d: %v", pass, err)
		}
		if _, err := f.signet.GetAttentionItem(t.Context(), itemID); err == nil {
			break
		}
	}
	item, snapshot := f.item(t, itemID)
	if _, err := f.signet.Submit(t.Context(), signet.ClientCommand{
		CommandID: "explain-hold", DeviceID: "device-1",
		ExpectedEntityVersion: snapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: item.ID, Action: domain.ActionDiscuss, ItemVersion: item.ItemVersion,
			ArtifactDigests: item.ArtifactDigests, Message: "Why is the workflow bounded?",
		},
	}); err != nil {
		t.Fatal(err)
	}
	reply := "The approved artifact bounds it."
	discussionID := specDiscussionInvocationID("explain-hold")
	if err := specifyfake.Script(driver, discussionID, 0, running, specify.Output{Reply: &reply}); err != nil {
		t.Fatal(err)
	}
	if _, found := f.runHold(t); found {
		t.Fatal("specification run holds before the discussion refuses")
	}
	return f, driver, discussionID
}

// refuseCollect runs passes whose driver refuses every inspection as
// non-conformant until one collect has run, and requires the hold it records.
func refuseCollect(t *testing.T, f specificationFixture, driver exec.StageDriver, id domain.InvocationID) {
	t.Helper()
	var inspects atomic.Int64
	refusing := inspectRefusingDriver{
		StageDriver: driver, inspect: &inspects,
		err: fmt.Errorf("inspect: authenticate current intent %s: %w", id, store.ErrBackendNotConformant),
	}
	for pass := 1; pass <= 2 && inspects.Load() == 0; pass++ {
		if _, err := f.newEngine(t, refusing).Reconcile(t.Context()); err != nil {
			t.Fatalf("refused pass %d: %v", pass, err)
		}
	}
	if inspects.Load() == 0 {
		t.Fatal("collect never ran: the hold assertion would be vacuous")
	}
	f.requireHold(t, domain.HoldBackendNotConformant)
}

// TestSpecificationDiscussionRefusalRecordsAndRecoveryClearsHold: a refused
// discussion collect records the hold, and a later pass that finds the reply
// still running clears it.
func TestSpecificationDiscussionRefusalRecordsAndRecoveryClearsHold(t *testing.T) {
	f, driver, discussionID := discussionAwaitingReply(t, 1)
	refuseCollect(t, f, driver, discussionID)

	if _, err := f.newEngine(t, driver).Reconcile(t.Context()); err != nil {
		t.Fatalf("still-running discussion pass: %v", err)
	}
	if hold, found := f.runHold(t); found {
		t.Fatalf("hold after a healthy discussion pass = %+v, want none", hold)
	}
}

// TestSpecificationDiscussionReplyClearsHold: when the first pass after a
// refused collect finds the reply complete, accepting it clears the hold.
func TestSpecificationDiscussionReplyClearsHold(t *testing.T) {
	f, driver, discussionID := discussionAwaitingReply(t, 0)
	refuseCollect(t, f, driver, discussionID)

	if _, err := f.newEngine(t, driver).Reconcile(t.Context()); err != nil {
		t.Fatalf("reply pass: %v", err)
	}
	if hold, found := f.runHold(t); found {
		t.Fatalf("hold after the reply was accepted = %+v, want none", hold)
	}
}

// TestSpecificationLostInvocationClearsHold: an attempt lost after a refused
// collect ends in an execution failure, and the refusal hold it no longer
// explains goes with it. The failure's terminal milestone clears it, as it
// clears every hold, so the lost path needs no refusal clear of its own.
func TestSpecificationLostInvocationClearsHold(t *testing.T) {
	f := newSpecificationFixture(t, true, 2)
	driver := f.newDriver(t)
	id := specificationInvocationID("specification-run", 1)
	driver.Script(id, execfake.StageScript{Outcome: execfake.OutcomeCrashBeforeResult})
	f.submit(t)
	refuseCollect(t, f, driver, id)

	if _, err := f.newEngine(t, driver).Reconcile(t.Context()); err != nil {
		t.Fatalf("lost-invocation pass: %v", err)
	}
	if _, err := f.signet.GetAttentionItem(t.Context(), domain.ItemID("execution-failure-"+string(id))); err != nil {
		t.Fatalf("lost invocation recorded no failure: %v", err)
	}
	if hold, found := f.runHold(t); found {
		t.Fatalf("hold after the lost invocation failed = %+v, want none", hold)
	}
}
