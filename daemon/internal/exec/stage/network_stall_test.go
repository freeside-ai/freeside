package stage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// stallingStartAuthority holds the first AuthenticateStart of its invocation,
// as a commit-author check that reaches a stalled GitHub would, until release
// closes, and closes reached when it starts holding.
type stallingStartAuthority struct {
	stubAuthority
	id               domain.InvocationID
	reached, release chan struct{}
	once             sync.Once
}

func (a *stallingStartAuthority) AuthenticateStart(
	ctx context.Context, id domain.InvocationID, spec exec.StartSpec,
) error {
	if id == a.id {
		held := false
		a.once.Do(func() { held = true })
		if held {
			close(a.reached)
			<-a.release
		}
	}
	return a.stubAuthority.AuthenticateStart(ctx, id, spec)
}

// stallingImportAuthority is stallingStartAuthority for AuthenticateImport,
// the check a restarted exported intent with a current import start runs.
type stallingImportAuthority struct {
	stubAuthority
	id               domain.InvocationID
	reached, release chan struct{}
	once             sync.Once
}

func (a *stallingImportAuthority) AuthenticateImport(
	ctx context.Context, id domain.InvocationID, spec exec.StartSpec,
) error {
	if id == a.id {
		held := false
		a.once.Do(func() { held = true })
		if held {
			close(a.reached)
			<-a.release
		}
	}
	return a.stubAuthority.AuthenticateImport(ctx, id, spec)
}

// returnsWithin fails t when call does not return within ten seconds.
func returnsWithin(t *testing.T, what string, call func()) {
	t.Helper()
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		call()
	}()
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s waited on the stalled call", what)
	}
}

// TestStartAuthenticationHoldsNoDriverLock: while one start's first
// current-authority check hangs, another invocation's Inspect, which takes
// the driver's lock as every session commit does, still answers (#3215).
func TestStartAuthenticationHoldsNoDriverLock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newTestDriver(t, &stubGate{}, newStubExports())
	authority := &stallingStartAuthority{id: testInvoke, reached: make(chan struct{}), release: make(chan struct{})}
	d.authority = authority
	spec := testStartSpec()
	inputs := stageInputs(t, &spec)
	started := make(chan error, 1)
	go func() {
		started <- d.StartWithInputs(ctx, testInvoke, spec,
			func(context.Context) (exec.StageInputs, error) { return inputs, nil })
	}()
	<-authority.reached
	returnsWithin(t, "another invocation's Inspect", func() {
		if _, err := d.Inspect(ctx, "inv-implement-run-2"); !errors.Is(err, exec.ErrUnknownInvocation) {
			t.Errorf("Inspect of an unstarted invocation = %v, want ErrUnknownInvocation", err)
		}
	})
	close(authority.release)
	if err := <-started; err != nil {
		t.Fatalf("StartWithInputs: %v", err)
	}
	waitSessionDone(t, d, testInvoke)
}

// TestInspectRecoversOutsideTheCaller: while the recovery of an orphaned
// handoff hangs on ward, Inspect reports the invocation running rather than
// waiting, and a later Inspect reads the phase the recovery left (#3215).
func TestInspectRecoversOutsideTheCaller(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	gate := &stubGate{
		handoffFn: func(ward.HandoffSpec) (*ward.HandoffResult, error) {
			return nil, errors.New("handoff returned before teardown was proven")
		},
		recoverFn: func(string, ward.HandoffSpec) (*ward.RecoveryResult, error) {
			once.Do(func() { close(reached) })
			<-release
			return &ward.RecoveryResult{Outcome: ward.RecoveryLoss}, nil
		},
	}
	d := newTestDriver(t, gate, newStubExports())
	spec := testStartSpec()
	inputs := stageInputs(t, &spec)
	if err := d.StartWithInputs(ctx, testInvoke, spec,
		func(context.Context) (exec.StageInputs, error) { return inputs, nil },
	); err != nil {
		t.Fatalf("StartWithInputs: %v", err)
	}
	waitSessionDone(t, d, testInvoke)
	// With its session gone, the first Inspect runs the current-policy check
	// off the lock (#3268); the one after it starts recovery.
	if status, err := d.Inspect(ctx, testInvoke); err != nil || status.Status != exec.StatusRunning {
		t.Fatalf("Inspect starting the check = %+v, %v; want running", status, err)
	}
	d.recoveries.Wait()
	returnsWithin(t, "Inspect", func() {
		if status, err := d.Inspect(ctx, testInvoke); err != nil || status.Status != exec.StatusRunning {
			t.Errorf("Inspect while recovery runs = %+v, %v; want running", status, err)
		}
	})
	<-reached
	close(release)
	if status, err := inspectRecovered(t, ctx, d, testInvoke); err != nil || status.Status != exec.StatusGone {
		t.Fatalf("Inspect after recovery = %+v, %v; want gone", status, err)
	}
}

// TestAuthenticateStartRunsTheStartCheckAlone: the driver's exported start
// check returns the authority's refusal wrapped as Start reports it, and a
// check that hangs takes no driver lock, so another invocation's Inspect
// still answers while it runs (#3224).
func TestAuthenticateStartRunsTheStartCheckAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	refusal := errors.New("commit author is not the App bot")
	refused := newTestDriver(t, &stubGate{}, newStubExports())
	refused.authority = stubAuthority{startErr: refusal}
	if err := refused.AuthenticateStart(ctx, testInvoke, testStartSpec()); !errors.Is(err, refusal) {
		t.Fatalf("AuthenticateStart = %v, want the authority's refusal", err)
	}

	d := newTestDriver(t, &stubGate{}, newStubExports())
	authority := &stallingStartAuthority{id: testInvoke, reached: make(chan struct{}), release: make(chan struct{})}
	d.authority = authority
	checked := make(chan error, 1)
	go func() { checked <- d.AuthenticateStart(ctx, testInvoke, testStartSpec()) }()
	<-authority.reached
	returnsWithin(t, "another invocation's Inspect", func() {
		if _, err := d.Inspect(ctx, "inv-implement-run-2"); !errors.Is(err, exec.ErrUnknownInvocation) {
			t.Errorf("Inspect of an unstarted invocation = %v, want ErrUnknownInvocation", err)
		}
	})
	close(authority.release)
	if err := <-checked; err != nil {
		t.Fatalf("AuthenticateStart: %v", err)
	}
	if _, err := d.Inspect(ctx, testInvoke); !errors.Is(err, exec.ErrUnknownInvocation) {
		t.Fatalf("Inspect after the check = %v, want ErrUnknownInvocation: the check commits nothing", err)
	}
}

// countRecoveryLaunches makes d's recovery launcher count each recovery it is
// asked to run and suppress it as workflow closure would, so the durable
// intent stays as it is and the count shows whether recovery started.
func countRecoveryLaunches(d *Driver) *atomic.Int32 {
	var launched atomic.Int32
	d.SetRecoveryLauncher(func(context.Context, domain.RunID, func(context.Context) error) error {
		launched.Add(1)
		return ErrWorkflowClosed
	})
	return &launched
}

// TestRestartCurrentCheckHoldsNoDriverLock: after a restart, while one
// preterminal invocation's current-policy re-check hangs, its own Inspect
// reports running, another invocation's Inspect still answers, and recovery
// waits for the check; once released, a later Inspect recovers (#3268).
func TestRestartCurrentCheckHoldsNoDriverLock(t *testing.T) {
	t.Parallel()
	for _, ph := range []phase{phaseSeeding, phaseRunning} {
		t.Run(string(ph), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			d := newTestDriver(t, &stubGate{}, newStubExports())
			launched := countRecoveryLaunches(d)
			orphan(t, d, ph, nil)
			orphanInvocation(t, d, quarantineSecond, phaseRunning, nil, specFor(quarantineSecond))
			authority := &stallingStartAuthority{id: testInvoke, reached: make(chan struct{}), release: make(chan struct{})}
			d.authority = authority

			returnsWithin(t, "the restarted invocation's Inspect", func() {
				if status, err := d.Inspect(ctx, testInvoke); err != nil || status.Status != exec.StatusRunning {
					t.Errorf("Inspect while its check starts = %+v, %v; want running", status, err)
				}
			})
			<-authority.reached
			returnsWithin(t, "another invocation's Inspect", func() {
				if _, err := d.Inspect(ctx, quarantineSecond); err != nil {
					t.Errorf("Inspect of another invocation = %v", err)
				}
			})
			returnsWithin(t, "the stalled invocation's Inspect", func() {
				if status, err := d.Inspect(ctx, testInvoke); err != nil || status.Status != exec.StatusRunning {
					t.Errorf("Inspect while its check hangs = %+v, %v; want running", status, err)
				}
			})
			if got := launched.Load(); got != 0 {
				t.Fatalf("recoveries launched while the check hangs = %d, want none", got)
			}

			close(authority.release)
			if status, err := inspectRecovered(t, ctx, d, testInvoke); err != nil || status.Status != exec.StatusRunning {
				t.Fatalf("Inspect after the check = %+v, %v; want running", status, err)
			}
			if got := launched.Load(); got != 1 {
				t.Fatalf("recoveries launched after the check = %d, want 1", got)
			}
		})
	}
}

// TestRestartCurrentCheckRefusalBlocksRecovery is a Tier A gate test: the
// background re-check of a restarted invocation still refuses one whose
// current authority the authority refuses. A later Inspect returns that
// refusal as the locked re-check did, and recovery never starts (#3268).
func TestRestartCurrentCheckRefusalBlocksRecovery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	refusal := errors.New("commit author is not the App bot")
	d := newTestDriver(t, &stubGate{}, newStubExports())
	launched := countRecoveryLaunches(d)
	orphan(t, d, phaseRunning, nil)
	d.authority = stubAuthority{startErr: refusal}

	for range 2 {
		_, err := inspectChecked(t, ctx, d, testInvoke)
		if !errors.Is(err, refusal) || !strings.Contains(err.Error(), "authenticate current intent "+string(testInvoke)+": ") {
			t.Fatalf("Inspect after the refused check = %v, want the authority's refusal as the current intent check", err)
		}
	}
	if got := launched.Load(); got != 0 {
		t.Fatalf("recoveries launched for a refused invocation = %d, want none", got)
	}
}

// TestRestartImportCheckHoldsNoDriverLock: a restarted exported intent with
// a current import start re-checks its import authority, and while that
// check hangs another invocation's Inspect still answers (#3268).
func TestRestartImportCheckHoldsNoDriverLock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	exports := newStubExports()
	d := newTestDriver(t, &stubGate{}, exports)
	launched := countRecoveryLaunches(d)
	in := orphan(t, d, phaseExported, &releasedExport{
		Dir: filepath.Join(os.TempDir(),
			"freeside-handoff-"+testRunIDFor(testInvoke)+"-out-restart-import-check"),
		Manifest:        export.Manifest{Version: export.ManifestVersion, Entries: []export.Entry{}},
		ObservedBaseSHA: testBase.BaseSHA,
	})
	exports.importStarts[testInvoke] = domain.CurrentImportStart{
		InvocationID: testInvoke, AdmissionID: in.Spec.AdmissionID,
	}
	orphanInvocation(t, d, quarantineSecond, phaseRunning, nil, specFor(quarantineSecond))
	authority := &stallingImportAuthority{id: testInvoke, reached: make(chan struct{}), release: make(chan struct{})}
	d.authority = authority

	returnsWithin(t, "the restarted invocation's Inspect", func() {
		if status, err := d.Inspect(ctx, testInvoke); err != nil || status.Status != exec.StatusRunning {
			t.Errorf("Inspect while its import check starts = %+v, %v; want running", status, err)
		}
	})
	<-authority.reached
	returnsWithin(t, "another invocation's Inspect", func() {
		if _, err := d.Inspect(ctx, quarantineSecond); err != nil {
			t.Errorf("Inspect of another invocation = %v", err)
		}
	})
	if got := launched.Load(); got != 0 {
		t.Fatalf("recoveries launched while the import check hangs = %d, want none", got)
	}
	close(authority.release)
	d.recoveries.Wait()
	d.mu.Lock()
	check := d.currentChecks[testInvoke]
	d.mu.Unlock()
	if check.state != currentCheckPassed || check.phase != phaseExported {
		t.Fatalf("import check after release = %+v, want passed for the exported phase", check)
	}
}

// TestCloseWaitsForRestartCurrentCheck: Close returns only after a held
// current-policy check returns.
func TestCloseWaitsForRestartCurrentCheck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := newTestDriver(t, &stubGate{}, newStubExports())
		orphan(t, d, phaseRunning, nil)
		authority := &stallingStartAuthority{id: testInvoke, reached: make(chan struct{}), release: make(chan struct{})}
		d.authority = authority
		if status, err := d.Inspect(t.Context(), testInvoke); err != nil || status.Status != exec.StatusRunning {
			t.Fatalf("Inspect = %+v, %v; want running", status, err)
		}
		<-authority.reached
		closed := make(chan error, 1)
		go func() { closed <- d.Close(context.Background()) }()
		synctest.Wait()
		select {
		case err := <-closed:
			t.Fatalf("Close returned while a check is held: %v", err)
		default:
		}
		close(authority.release)
		if err := <-closed; err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// TestRestartCurrentCheckWithEndedContextRecordsNothing: a refusal from a
// check whose context ended is not kept for the next Inspect, which starts a
// new check instead of reporting it (#3268).
func TestRestartCurrentCheckWithEndedContextRecordsNothing(t *testing.T) {
	t.Parallel()
	refusal := errors.New("commit author is not the App bot")
	d := newTestDriver(t, &stubGate{}, newStubExports())
	orphan(t, d, phaseRunning, nil)
	authority := &stallingStartAuthority{
		stubAuthority: stubAuthority{startErr: refusal},
		id:            testInvoke, reached: make(chan struct{}), release: make(chan struct{}),
	}
	d.authority = authority
	ctx, cancel := context.WithCancel(context.Background())
	if status, err := d.Inspect(ctx, testInvoke); err != nil || status.Status != exec.StatusRunning {
		t.Fatalf("Inspect = %+v, %v; want running", status, err)
	}
	<-authority.reached
	cancel()
	close(authority.release)
	d.recoveries.Wait()
	if status, err := d.Inspect(context.Background(), testInvoke); err != nil || status.Status != exec.StatusRunning {
		t.Fatalf("Inspect after the ended check = %+v, %v; want running from a new check", status, err)
	}
	d.recoveries.Wait()
	if _, err := d.Inspect(context.Background(), testInvoke); !errors.Is(err, refusal) {
		t.Fatalf("Inspect after the new check = %v, want its refusal", err)
	}
}

// TestRecoveryAttemptRechecksOffTheLock: launching recovery consumes the
// passed check, because recovery can drop the authority's cached answer
// (import forgets it). The next Inspect re-checks off the lock, so a
// re-check that then hangs holds neither that Inspect nor another
// invocation's (#3268).
func TestRecoveryAttemptRechecksOffTheLock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newTestDriver(t, &stubGate{}, newStubExports())
	launched := countRecoveryLaunches(d)
	orphan(t, d, phaseRunning, nil)
	orphanInvocation(t, d, quarantineSecond, phaseRunning, nil, specFor(quarantineSecond))
	for _, step := range []string{"the restart check", "the first recovery"} {
		if status, err := d.Inspect(ctx, testInvoke); err != nil || status.Status != exec.StatusRunning {
			t.Fatalf("Inspect starting %s = %+v, %v; want running", step, status, err)
		}
		d.recoveries.Wait()
	}
	if got := launched.Load(); got != 1 {
		t.Fatalf("recoveries launched = %d, want 1", got)
	}
	authority := &stallingStartAuthority{id: testInvoke, reached: make(chan struct{}), release: make(chan struct{})}
	d.authority = authority
	returnsWithin(t, "Inspect after a recovery attempt", func() {
		if status, err := d.Inspect(ctx, testInvoke); err != nil || status.Status != exec.StatusRunning {
			t.Errorf("Inspect after a recovery attempt = %+v, %v; want running", status, err)
		}
	})
	<-authority.reached
	returnsWithin(t, "another invocation's Inspect", func() {
		if _, err := d.Inspect(ctx, quarantineSecond); err != nil {
			t.Errorf("Inspect of another invocation = %v", err)
		}
	})
	close(authority.release)
	d.recoveries.Wait()
}

// TestRestartCurrentCheckRefusalIsBoundToItsPhase: a refusal is reported
// only while the intent stays in the phase its check examined. Once the
// phase moves to one whose gate the refusal does not apply to, here a
// released export with no current import start, the next Inspect checks
// the intent as it is instead of reporting the stale refusal (#3268).
func TestRestartCurrentCheckRefusalIsBoundToItsPhase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	refusal := errors.New("commit author is not the App bot")
	d := newTestDriver(t, &stubGate{}, newStubExports())
	orphan(t, d, phaseRunning, nil)
	d.authority = stubAuthority{startErr: refusal}
	if status, err := d.Inspect(ctx, testInvoke); err != nil || status.Status != exec.StatusRunning {
		t.Fatalf("Inspect = %+v, %v; want running", status, err)
	}
	d.recoveries.Wait()
	orphan(t, d, phaseExported, &releasedExport{
		Dir: filepath.Join(os.TempDir(),
			"freeside-handoff-"+testRunIDFor(testInvoke)+"-out-phase-moved"),
		Manifest:        export.Manifest{Version: export.ManifestVersion, Entries: []export.Entry{}},
		ObservedBaseSHA: testBase.BaseSHA,
	})
	if status, err := d.Inspect(ctx, testInvoke); err != nil || status.Status != exec.StatusRunning {
		t.Fatalf("Inspect after the phase moved = %+v, %v; want running from a new check", status, err)
	}
	d.recoveries.Wait()
	d.mu.Lock()
	check := d.currentChecks[testInvoke]
	d.mu.Unlock()
	if check.state != currentCheckPassed || check.phase != phaseExported {
		t.Fatalf("check after the phase moved = %+v, want passed for the exported phase", check)
	}
}

// TestRestartCurrentCheckRetriesAnUnreachableAuthority: while the authority
// can't be reached, Inspect keeps a held restarted intent running and
// launches no recovery; once the authority answers, a later Inspect
// recovers it (#3381).
func TestRestartCurrentCheckRetriesAnUnreachableAuthority(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newTestDriver(t, &stubGate{}, newStubExports())
	launched := countRecoveryLaunches(d)
	orphan(t, d, phaseRunning, nil)
	d.authority = stubAuthority{startErr: fmt.Errorf("resolve App bot identity: %w", ErrAuthorityUnavailable)}

	if err := reconcileHeld(t, ctx, d); !errors.Is(err, ErrRecoveryRetryable) {
		t.Fatalf("held error = %v, want retryable", err)
	}
	for range 3 {
		if status, err := inspectChecked(t, ctx, d, testInvoke); err != nil || status.Status != exec.StatusRunning {
			t.Fatalf("Inspect while the authority is unreachable = %+v, %v; want running", status, err)
		}
	}
	if got := launched.Load(); got != 0 {
		t.Fatalf("recoveries launched while the authority is unreachable = %d, want none", got)
	}

	// inspectChecked waited for the last check, so no check reads the
	// authority while it is replaced.
	d.authority = stubAuthority{}
	if status, err := inspectRecovered(t, ctx, d, testInvoke); err != nil || status.Status != exec.StatusRunning {
		t.Fatalf("Inspect once the authority answers = %+v, %v; want running", status, err)
	}
	if got := launched.Load(); got != 1 {
		t.Fatalf("recoveries launched once the authority answers = %d, want 1", got)
	}
}

// TestReconcileCurrentCheckHoldsNoDriverLock: while startup Reconcile's
// current-policy check of one intent hangs, an Inspect of another
// invocation still answers, because the check runs off the driver's lock
// (#3381).
func TestReconcileCurrentCheckHoldsNoDriverLock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newTestDriver(t, &stubGate{}, newStubExports())
	launched := countRecoveryLaunches(d)
	orphan(t, d, phaseRunning, nil)
	authority := &stallingStartAuthority{id: testInvoke, reached: make(chan struct{}), release: make(chan struct{})}
	d.authority = authority
	release := sync.OnceFunc(func() { close(authority.release) })
	t.Cleanup(release)

	type outcome struct {
		report RecoveryReport
		err    error
	}
	reconciled := make(chan outcome, 1)
	go func() {
		report, err := d.Reconcile(ctx, nil)
		reconciled <- outcome{report, err}
	}()
	<-authority.reached
	returnsWithin(t, "another invocation's Inspect", func() {
		if _, err := d.Inspect(ctx, quarantineSecond); !errors.Is(err, exec.ErrUnknownInvocation) {
			t.Errorf("Inspect of another invocation = %v, want unknown", err)
		}
	})

	release()
	got := <-reconciled
	if got.err != nil || len(got.report.Held) != 0 || len(got.report.Quarantined) != 0 {
		t.Fatalf("Reconcile = %+v, %v; want a clean pass", got.report, got.err)
	}
	if n := launched.Load(); n != 1 {
		t.Fatalf("recoveries launched after the check = %d, want 1", n)
	}
}
