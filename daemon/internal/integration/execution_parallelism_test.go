package integration_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func submitParallelismRun(
	t *testing.T, f *workflowFixture, runID string,
) engine.ProductionRun {
	t.Helper()
	spec, policy, resolved := registerSubmissionArtifacts(t, f.store, runID)
	submitted, err := engine.SubmitProductionRun(context.Background(), f.store, engine.ProductionRunSpec{
		RunID: domain.RunID(runID), ProjectID: "proj-parallelism",
		SpecArtifactID: spec.ID, PolicyArtifactID: policy.ID,
		ResolvedPolicy: resolved, Publication: productionPublicationMetadata(),
	})
	if err != nil {
		t.Fatalf("submit %s: %v", runID, err)
	}
	return submitted
}

func activeIdentityExecutions(
	t *testing.T, st *store.Store, identityID domain.AuthIdentityID,
) int {
	t.Helper()
	var active int
	if err := st.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		active, err = tx.ActiveIdentityExecutionCount(context.Background(), identityID)
		return err
	}); err != nil {
		t.Fatalf("count active executions for %s: %v", identityID, err)
	}
	return active
}

func acquireFixtureDaemonLock(t *testing.T, f *workflowFixture) {
	t.Helper()
	lock, err := daemonlock.Acquire(filepath.Join(f.root, "freeside.db"))
	if err != nil {
		t.Fatalf("acquire daemon lock: %v", err)
	}
	f.lock = lock
	t.Cleanup(func() { _ = lock.Close() })
}

func lockFixtureEngine(t *testing.T, f *workflowFixture, identity domain.AuthIdentity) {
	t.Helper()
	acquireFixtureDaemonLock(t, f)
	workflow, err := engine.New(f.store, f.signet, f.driver,
		append(unattendedProductionOptionsForIdentity(t, identity.ID), engine.WithDaemonLock(f.lock))...)
	if err != nil {
		t.Fatalf("compose locked engine: %v", err)
	}
	f.engine = workflow
}

// TestIdentityParallelismIgnoresUnstartedAdmissions keeps a transient input
// materialization refusal from reserving a slot that no provider execution
// ever received, so later healthy work can still start under a serial limit.
func TestIdentityParallelismIgnoresUnstartedAdmissions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	identity := testIdentity
	identity.MaxParallelExecutions = 1
	f := openUnattendedFixtureWithIdentity(t, identity)
	lockFixtureEngine(t, f, identity)
	held := submitParallelismRun(t, f, "run-parallel-unstarted")
	healthy := submitParallelismRun(t, f, "run-parallel-healthy")
	for _, run := range []engine.ProductionRun{held, healthy} {
		f.driver.Script(run.InvocationID, fake.StageScript{
			Outcome: fake.OutcomeComplete, RunningInspects: 20,
			Result: exec.StageResult{Summary: "parallelism fixture"},
		})
	}

	refusing, err := engine.New(
		f.store, f.signet,
		selectiveStartRefusingDriver{
			StageDriver: f.driver, invocationID: held.InvocationID,
			err: fmt.Errorf("materialize input: %w", exec.ErrInputUnavailable),
		},
		append(unattendedProductionOptionsForIdentity(t, identity.ID), engine.WithDaemonLock(f.lock))...,
	)
	if err != nil {
		t.Fatalf("compose refusing engine: %v", err)
	}
	if result, err := refusing.Reconcile(ctx); err != nil || result.InvocationsStarted != 1 {
		t.Fatalf("selective materialization reconcile = %#v, %v", result, err)
	}
	if _, started := f.driver.StartSpec(held.InvocationID); started {
		t.Fatal("input-unavailable invocation reached the driver")
	}
	if _, started := f.driver.StartSpec(healthy.InvocationID); !started {
		t.Fatal("healthy invocation was blocked by an unstarted admission")
	}
	if got := activeIdentityExecutions(t, f.store, identity.ID); got != 1 {
		t.Fatalf("active executions = %d, want only the healthy started invocation", got)
	}
}

// TestIdentityParallelismRecoversDispatchReservation proves a daemon restart
// after recording the pre-start reservation can reclaim it and invoke the
// durable driver rather than leaving the identity permanently occupied.
func TestIdentityParallelismRecoversDispatchReservation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	identity := testIdentity
	identity.MaxParallelExecutions = 1
	f := openUnattendedFixtureWithIdentity(t, identity)
	lockFixtureEngine(t, f, identity)
	run := submitParallelismRun(t, f, "run-parallel-recover-reservation")
	f.driver.Script(run.InvocationID, fake.StageScript{
		Outcome: fake.OutcomeComplete, RunningInspects: 20,
		Result: exec.StageResult{Summary: "recovered dispatch reservation"},
	})
	refusing, err := engine.New(
		f.store, f.signet,
		startRefusingDriver{StageDriver: f.driver, err: fmt.Errorf("materialize input: %w", exec.ErrInputUnavailable)},
		append(unattendedProductionOptionsForIdentity(t, identity.ID), engine.WithDaemonLock(f.lock))...,
	)
	if err != nil {
		t.Fatalf("compose refusing engine: %v", err)
	}
	if result, err := refusing.Reconcile(ctx); err != nil || result.InvocationsStarted != 0 {
		t.Fatalf("create recoverable reservation = %#v, %v", result, err)
	}
	if err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.MarkOutboxDispatching(ctx, string(run.InvocationID))
	}); err != nil {
		t.Fatalf("simulate pre-start crash reservation: %v", err)
	}
	if result, err := f.engine.Reconcile(ctx); err != nil || result.InvocationsStarted != 1 {
		t.Fatalf("recover dispatch reservation = %#v, %v", result, err)
	}
	if _, started := f.driver.StartSpec(run.InvocationID); !started {
		t.Fatal("recovered reservation did not reach the driver")
	}
}

// TestIdentityParallelismKeepsReservationDuringConcurrentInputRetry proves
// that only the dispatcher that claimed an invocation's reservation enters
// the driver. A concurrent reconcile therefore cannot release the first
// caller's reservation while it is materializing inputs, and a later
// same-identity invocation stays below the serial limit.
func TestIdentityParallelismKeepsReservationDuringConcurrentInputRetry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	identity := testIdentity
	identity.MaxParallelExecutions = 1
	f := openUnattendedFixtureWithIdentity(t, identity)
	lockFixtureEngine(t, f, identity)
	first := submitParallelismRun(t, f, "run-parallel-racing-retry")
	later := submitParallelismRun(t, f, "run-parallel-racing-later")
	for _, run := range []engine.ProductionRun{first, later} {
		f.driver.Script(run.InvocationID, fake.StageScript{
			Outcome: fake.OutcomeComplete, RunningInspects: 20,
			Result: exec.StageResult{Summary: "parallelism retry fixture"},
		})
	}

	refusing := &blockingOnceRefusingDriver{
		StageDriver: f.driver, invocationID: first.InvocationID,
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	racing, err := engine.New(
		f.store, f.signet, refusing,
		append(unattendedProductionOptionsForIdentity(t, identity.ID), engine.WithDaemonLock(f.lock))...,
	)
	if err != nil {
		t.Fatalf("compose racing engine: %v", err)
	}
	firstDone := make(chan error, 1)
	go func() {
		_, err := racing.Reconcile(ctx)
		firstDone <- err
	}()
	select {
	case <-refusing.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first dispatcher did not enter the driver")
	}
	secondDone := make(chan error, 1)
	go func() {
		_, err := racing.Reconcile(ctx)
		secondDone <- err
	}()
	select {
	case err := <-secondDone:
		t.Fatalf("concurrent reconcile returned before the active pass: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(refusing.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("serialized reconcile: %v", err)
	}
	started := 0
	for _, invocationID := range []domain.InvocationID{first.InvocationID, later.InvocationID} {
		if _, ok := f.driver.StartSpec(invocationID); ok {
			started++
		}
	}
	if started != 1 {
		t.Fatalf("starts after concurrent retry = %d, want exactly one", started)
	}
	if got := activeIdentityExecutions(t, f.store, identity.ID); got != 1 {
		t.Fatalf("active executions after reservation release = %d, want 1", got)
	}
}

type blockingOnceRefusingDriver struct {
	exec.StageDriver
	invocationID domain.InvocationID
	entered      chan struct{}
	release      chan struct{}

	mu      sync.Mutex
	refused bool
}

func (d *blockingOnceRefusingDriver) Start(
	ctx context.Context, invocationID domain.InvocationID, spec exec.StartSpec,
) error {
	d.mu.Lock()
	refuse := invocationID == d.invocationID && !d.refused
	if refuse {
		d.refused = true
	}
	d.mu.Unlock()
	if !refuse {
		return d.StageDriver.Start(ctx, invocationID, spec)
	}
	close(d.entered)
	<-d.release
	return fmt.Errorf("materialize input: %w", exec.ErrInputUnavailable)
}

// TestIdentityParallelismRechecksReplayedAdmissionsAfterLimitReduction proves
// that durable pre-start admissions do not let a restored lower limit launch
// more than one replay in the same pass.
func TestIdentityParallelismRechecksReplayedAdmissionsAfterLimitReduction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	identity := testIdentity
	identity.MaxParallelExecutions = 2
	f := openUnattendedFixtureWithIdentity(t, identity)
	runs := []engine.ProductionRun{
		submitParallelismRun(t, f, "run-parallel-replay-one"),
		submitParallelismRun(t, f, "run-parallel-replay-two"),
	}
	for _, run := range runs {
		f.driver.Script(run.InvocationID, fake.StageScript{
			Outcome: fake.OutcomeComplete, RunningInspects: 20,
			Result: exec.StageResult{Summary: "parallelism replay fixture"},
		})
	}
	refusing, err := engine.New(
		f.store, f.signet,
		startRefusingDriver{StageDriver: f.driver, err: fmt.Errorf("materialize input: %w", exec.ErrInputUnavailable)},
		unattendedProductionOptionsForIdentity(t, identity.ID)...,
	)
	if err != nil {
		t.Fatalf("compose refusing engine: %v", err)
	}
	if result, err := refusing.Reconcile(ctx); err != nil || result.InvocationsStarted != 0 {
		t.Fatalf("pre-reduction reconcile = %#v, %v", result, err)
	}
	lower := identity
	lower.MaxParallelExecutions = 1
	if err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordAuthIdentity(ctx, lower, admittedAt.Add(time.Hour))
	}); err != nil {
		t.Fatalf("restore lower identity limit: %v", err)
	}
	if result, err := f.engine.Reconcile(ctx); err != nil || result.InvocationsStarted != 1 {
		t.Fatalf("replay under restored limit = %#v, %v", result, err)
	}
	if got := activeIdentityExecutions(t, f.store, identity.ID); got != 1 {
		t.Fatalf("active executions after replay = %d, want 1", got)
	}
	started := 0
	for _, run := range runs {
		if _, ok := f.driver.StartSpec(run.InvocationID); ok {
			started++
		}
	}
	if started != 1 {
		t.Fatalf("replayed starts = %d, want restored limit 1", started)
	}
}

// TestIdentityParallelismEnforcesLimitsAndCompletionRelease exercises the
// production dispatcher with the permanent fake at both a serial limit and a
// wider experimentally established limit. The N+1th intent stays pending with
// a typed scheduling hold until an export terminal releases one derived slot.
func TestIdentityParallelismEnforcesLimitsAndCompletionRelease(t *testing.T) {
	for _, limit := range []int{1, 2} {
		t.Run(fmt.Sprintf("limit_%d", limit), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			identity := testIdentity
			identity.MaxParallelExecutions = limit
			f := openUnattendedFixtureWithIdentity(t, identity)

			runs := make([]engine.ProductionRun, limit+1)
			for i := range runs {
				runs[i] = submitParallelismRun(t, f, fmt.Sprintf("run-parallel-%d-%d", limit, i))
				f.driver.Script(runs[i].InvocationID, fake.StageScript{
					Outcome: fake.OutcomeComplete, RunningInspects: 20,
					Result: exec.StageResult{Summary: "parallelism fixture"},
				})
			}

			result, err := f.engine.Reconcile(ctx)
			if err != nil {
				t.Fatalf("initial reconcile: %v", err)
			}
			if result.InvocationsStarted != limit {
				t.Fatalf("initial starts = %d, want limit %d", result.InvocationsStarted, limit)
			}
			if got := activeIdentityExecutions(t, f.store, identity.ID); got != limit {
				t.Fatalf("active executions = %d, want %d", got, limit)
			}
			held := runs[len(runs)-1]
			if _, started := f.driver.StartSpec(held.InvocationID); started {
				t.Fatalf("invocation %s started past limit %d", held.InvocationID, limit)
			}
			observation := observeProductionRun(t, f.store, held.Run.ID)
			if observation.Hold == nil || observation.Hold.Reason != domain.HoldIdentityParallelism {
				t.Fatalf("held observation = %#v, want identity_parallelism", observation.Hold)
			}

			var admission domain.ExecutionAdmission
			if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
				var err error
				admission, err = tx.GetExecutionAdmissionRecord(ctx, runs[0].InvocationID)
				return err
			}); err != nil {
				t.Fatalf("read admitted execution: %v", err)
			}
			recordParallelismCompletion(t, f.store, admission)
			if got := activeIdentityExecutions(t, f.store, identity.ID); got != limit-1 {
				t.Fatalf("active executions after completion = %d, want %d", got, limit-1)
			}

			resumed, err := f.engine.Reconcile(ctx)
			if err != nil {
				t.Fatalf("resume after completion: %v", err)
			}
			if resumed.InvocationsStarted != 1 {
				t.Fatalf("starts after completion = %d, want held invocation", resumed.InvocationsStarted)
			}
			if _, started := f.driver.StartSpec(held.InvocationID); !started {
				t.Fatalf("held invocation %s did not resume", held.InvocationID)
			}
			if observation := observeProductionRun(t, f.store, held.Run.ID); observation.Hold != nil {
				t.Fatalf("resumed run retained stale hold %#v", observation.Hold)
			}
		})
	}
}

// TestIdentityParallelismFailureRelease proves the driver's non-export
// terminal authority also releases capacity without decrement bookkeeping.
func TestIdentityParallelismFailureRelease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	identity := testIdentity
	identity.MaxParallelExecutions = 1
	f := openUnattendedFixtureWithIdentity(t, identity)
	failed := submitParallelismRun(t, f, "run-parallel-failed")
	held := submitParallelismRun(t, f, "run-parallel-after-failure")
	f.driver.Script(failed.InvocationID, fake.StageScript{
		Outcome: fake.OutcomeFail, RunningInspects: 20,
		Result: exec.StageResult{Summary: "expected fixture failure"},
	})
	f.driver.Script(held.InvocationID, fake.StageScript{
		Outcome: fake.OutcomeComplete, RunningInspects: 20,
		Result: exec.StageResult{Summary: "resumed after failure"},
	})

	if first, err := f.engine.Reconcile(ctx); err != nil || first.InvocationsStarted != 1 {
		t.Fatalf("initial reconcile = %#v, %v", first, err)
	}
	var admission domain.ExecutionAdmission
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		admission, err = tx.GetExecutionAdmissionRecord(ctx, failed.InvocationID)
		return err
	}); err != nil {
		t.Fatalf("read failed execution admission: %v", err)
	}
	if err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordExecutionOutcome(ctx, domain.ExecutionOutcome{
			InvocationID: failed.InvocationID, AdmissionID: admission.ID,
			Status: domain.ExecutionOutcomeFailed, Summary: "expected fixture failure",
			RecordedAt: admittedAt.Add(time.Minute),
		})
	}); err != nil {
		t.Fatalf("record failed execution outcome: %v", err)
	}
	if got := activeIdentityExecutions(t, f.store, identity.ID); got != 0 {
		t.Fatalf("active executions after failure = %d, want 0", got)
	}
	if resumed, err := f.engine.Reconcile(ctx); err != nil || resumed.InvocationsStarted != 1 {
		t.Fatalf("post-failure reconcile = %#v, %v", resumed, err)
	}
	if _, started := f.driver.StartSpec(held.InvocationID); !started {
		t.Fatal("held invocation did not resume after failure")
	}
}

// TestIdentityParallelismIsScopedPerIdentity proves an occupied identity does
// not consume another identity's capacity. The second engine models a distinct
// configured provider identity over the same durable scheduler state.
func TestIdentityParallelismIsScopedPerIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	firstIdentity := testIdentity
	firstIdentity.MaxParallelExecutions = 1
	f := openUnattendedFixtureWithIdentity(t, firstIdentity)
	first := submitParallelismRun(t, f, "run-parallel-identity-a")
	f.driver.Script(first.InvocationID, fake.StageScript{
		Outcome: fake.OutcomeComplete, RunningInspects: 20,
		Result: exec.StageResult{Summary: "identity A remains active"},
	})
	if result, err := f.engine.Reconcile(ctx); err != nil || result.InvocationsStarted != 1 {
		t.Fatalf("start identity A = %#v, %v", result, err)
	}

	secondIdentity := firstIdentity
	secondIdentity.ID = "auth-claude-secondary"
	secondIdentity.AuthStoreVolume = "claude-secondary-credentials"
	if err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordAuthIdentity(ctx, secondIdentity, admittedAt)
	}); err != nil {
		t.Fatalf("record second identity: %v", err)
	}
	second := submitParallelismRun(t, f, "run-parallel-identity-b")
	f.driver.Script(second.InvocationID, fake.StageScript{
		Outcome: fake.OutcomeComplete, RunningInspects: 20,
		Result: exec.StageResult{Summary: "identity B starts independently"},
	})
	other, err := engine.New(
		f.store, f.signet, f.driver,
		unattendedProductionOptionsForIdentity(t, secondIdentity.ID)...,
	)
	if err != nil {
		t.Fatalf("compose second-identity engine: %v", err)
	}
	if result, err := other.Reconcile(ctx); err != nil || result.InvocationsStarted != 1 {
		t.Fatalf("start identity B beside A = %#v, %v", result, err)
	}
	if got := activeIdentityExecutions(t, f.store, secondIdentity.ID); got != 1 {
		t.Fatalf("identity B active executions = %d, want 1", got)
	}
}

func recordParallelismCompletion(
	t *testing.T, st *store.Store, admission domain.ExecutionAdmission,
) {
	t.Helper()
	executionExport, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: admission.InvocationID, AdmissionID: admission.ID,
		ObservedBaseSHA: admission.Base.BaseSHA, HeadSHA: "cafe1234",
		ManifestDigest: submissionDigest(string(admission.RunID), "parallelism-manifest"),
		RecordedAt:     admittedAt.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("new completion export: %v", err)
	}
	if err := st.WriteInternal(context.Background(), func(tx *store.InternalTx) error {
		return tx.RecordExecutionExportRecord(context.Background(), executionExport)
	}); err != nil {
		t.Fatalf("record completion export: %v", err)
	}
}
