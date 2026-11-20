package stage

import (
	"context"
	"errors"
	"os"
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// requireReleasedRuns fails unless the gate was asked to release exactly
// want, in any order and across any number of calls.
func requireReleasedRuns(t *testing.T, gate *stubGate, want ...string) {
	t.Helper()
	got := gate.released()
	if len(got) != len(want) {
		t.Fatalf("released runs = %v, want %v", got, want)
	}
	for _, runID := range want {
		if !slices.Contains(got, runID) {
			t.Fatalf("released runs = %v, want %v", got, want)
		}
	}
}

// A start that returns after the pre-job hook bound the handoff namespace
// releases it unless a seeding intent may exist, which recovery would resume
// without running the hook again.
func TestStartReleasesHandoffNamesUnlessASeedingIntentMayExist(t *testing.T) {
	t.Parallel()
	loadErr := errors.New("fixture: input load failed")
	for _, test := range []struct {
		name    string
		arrange func(*testing.T, *Driver, *exec.StartSpec) exec.StageInputLoader
		refused bool
		release bool
	}{
		{
			name: "no intent saved",
			arrange: func(*testing.T, *Driver, *exec.StartSpec) exec.StageInputLoader {
				return func(context.Context) (exec.StageInputs, error) { return exec.StageInputs{}, loadErr }
			},
			refused: true,
			release: true,
		},
		{
			// An invalid adopted registry set is a refusal the start commits
			// as a terminal intent, which recovery never resumes.
			name: "terminal intent saved",
			arrange: func(t *testing.T, d *Driver, spec *exec.StartSpec) exec.StageInputLoader {
				d.adoptedHosts = func(context.Context, int64) ([]string, error) { return []string{"PyPI.org"}, nil }
				inputs := registryInputs(t, spec)
				return func(context.Context) (exec.StageInputs, error) { return inputs, nil }
			},
			release: true,
		},
		{
			name: "seeding save fails",
			arrange: func(t *testing.T, d *Driver, spec *exec.StartSpec) exec.StageInputLoader {
				// A failed atomic write may still land the file, so a directory
				// at the intent path stands in for any failed seeding save. The
				// loader runs after the duplicate-start read, which the
				// directory would otherwise fail.
				inputs := stageInputs(t, spec)
				return func(context.Context) (exec.StageInputs, error) {
					return inputs, os.Mkdir(d.intentPath(testRunIDFor(testInvoke)), 0o700)
				}
			},
			refused: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			gate := &stubGate{}
			d := newTestDriver(t, gate, newStubExports())
			bound := 0
			d.preJob = func(context.Context, domain.InvocationID, domain.ImageRef) error {
				bound++
				return nil
			}
			spec := testStartSpec()
			load := test.arrange(t, d, &spec)
			if err := d.StartWithInputs(context.Background(), testInvoke, spec, load); (err != nil) != test.refused {
				t.Fatalf("start = %v, want refused %t", err, test.refused)
			}
			if bound != 1 {
				t.Fatalf("pre-job hook ran %d times, want 1", bound)
			}
			if test.release {
				requireReleasedRuns(t, gate, testRunIDFor(testInvoke))
				return
			}
			requireReleasedRuns(t, gate)
		})
	}
}

// The pipeline releases its run's names once its intent is terminal and keeps
// them while a failed terminal write is retained, until a retry commits it.
// A refused seed is a pipeline failure that ends before ward ever opens a
// journal record, so no close of ward's releases these names.
func TestPipelineReleasesHandoffNamesOnceTerminal(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		retain bool
	}{
		{name: "terminal committed"},
		{name: "terminal write retained", retain: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			gate := &stubGate{}
			records := newStubExports()
			d := newTestDriver(t, gate, records)
			d.seeder = stubSeeder{err: errors.New("fixture: base fetch refused")}
			runID := testRunIDFor(testInvoke)
			var restore func()
			if test.retain {
				restore = holdIntentAfterOutcome(t, d, records, runID)
			}
			spec := testStartSpec()
			inputs := stageInputs(t, &spec)
			if err := d.StartWithInputs(ctx, testInvoke, spec, func(context.Context) (exec.StageInputs, error) {
				return inputs, nil
			}); err != nil {
				t.Fatal(err)
			}
			if !test.retain {
				waitSessionDone(t, d, testInvoke)
				if result, err := d.Collect(ctx, testInvoke); err != nil || result.Status != exec.StatusFailed {
					t.Fatalf("Collect = %+v, %v; want failed", result, err)
				}
				requireReleasedRuns(t, gate, runID)
				return
			}
			waitPipelineExited(t, d, testInvoke)
			requireReleasedRuns(t, gate)
			restore()
			// Collect retries the retained write; its commit releases.
			if result, err := d.Collect(ctx, testInvoke); err != nil || result.Status != exec.StatusFailed {
				t.Fatalf("Collect = %+v, %v; want failed", result, err)
			}
			requireReleasedRuns(t, gate, runID)
		})
	}
}

// waitPipelineExited waits for a session whose terminal write is retained,
// so it stays registered after its pipeline returns.
func waitPipelineExited(t *testing.T, d *Driver, id domain.InvocationID) {
	t.Helper()
	d.mu.Lock()
	sess := d.running[id]
	d.mu.Unlock()
	if sess == nil {
		t.Fatal("no pipeline session registered")
	}
	<-sess.done
}

// A Cancel that commits before ward opened a journal record releases the
// run's names; one that finds the handoff started leaves its disposition, and
// the names, to ward.
func TestPreJournalCancellationReleasesHandoffNamesOnceCommitted(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		started bool
	}{
		{name: "journal absent"},
		{name: "journal opened", started: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			gate := &stubGate{
				cancelFn:       func(string) error { return ward.ErrJournalRecordNotFound },
				handoffStarted: func(string) (bool, error) { return test.started, nil },
			}
			d := newTestDriver(t, gate, newStubExports())
			in := orphan(t, d, phaseRunning, nil)
			if err := d.Cancel(context.Background(), testInvoke); err != nil {
				t.Fatal(err)
			}
			if test.started {
				requireReleasedRuns(t, gate)
				return
			}
			requireReleasedRuns(t, gate, in.RunID)
		})
	}
}

// Startup reconcile releases every terminal intent's names in one call, which
// covers a crash between a close or terminal commit and its release, and
// leaves a preterminal intent's names to its own recovery.
func TestReconcileReleasesTerminalIntentsInOneCall(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	gate := &stubGate{}
	d := newTestDriver(t, gate, newStubExports())
	committed := domain.InvocationID("inv-implement-committed")
	lost := domain.InvocationID("inv-implement-lost")
	running := domain.InvocationID("inv-implement-running")
	for _, id := range []domain.InvocationID{committed, lost} {
		in := orphanInvocation(t, d, id, phaseSeeding, nil, specFor(id))
		in.Phase = phaseLost
		record := d.recordLost
		if id == committed {
			result := exec.StageResult{InvocationID: id, Status: exec.StatusFailed, Summary: "failed"}
			in.Phase, in.Result = phaseCommitted, &result
			record = func(ctx context.Context, in intent, _ *domain.HandoffCheckFailure) error {
				return d.recordOutcome(ctx, in, result)
			}
		}
		if err := record(ctx, in, nil); err != nil {
			t.Fatal(err)
		}
		if err := d.saveIntent(in); err != nil {
			t.Fatal(err)
		}
	}
	// A running handoff whose ward recovery fails stays held.
	orphanInvocation(t, d, running, phaseRunning, nil, specFor(running))

	report, err := d.Reconcile(ctx, nil)
	if err != nil || len(report.Held) != 1 || report.Held[0].Invocation != running {
		t.Fatalf("Reconcile = %+v, %v; want only the running intent held", report, err)
	}
	if calls := gate.releaseCalls(); calls != 1 {
		t.Fatalf("release calls = %d, want one batch", calls)
	}
	requireReleasedRuns(t, gate, testRunIDFor(committed), testRunIDFor(lost))
}

// Recovery of an intent copy read before a Cancel made the intent terminal
// leaves the terminal record alone and starts no handoff, so a terminal
// release stays final.
func TestReconcileIntentSkipsACopyThatBecameTerminal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	gate := &stubGate{
		cancelFn:       func(string) error { return ward.ErrJournalRecordNotFound },
		handoffStarted: func(string) (bool, error) { return false, nil },
	}
	d := newTestDriver(t, gate, newStubExports())
	stale := orphan(t, d, phaseRunning, nil)
	if err := d.Cancel(ctx, testInvoke); err != nil {
		t.Fatal(err)
	}
	if err := d.reconcileIntent(ctx, stale); err != nil {
		t.Fatal(err)
	}
	waitSessionDone(t, d, testInvoke)
	if result, err := d.Collect(ctx, testInvoke); err != nil || result.Status != exec.StatusCanceled {
		t.Fatalf("Collect = %+v, %v; want the committed cancellation", result, err)
	}
	gate.mu.Lock()
	handoffs := len(gate.specs)
	gate.mu.Unlock()
	if handoffs != 0 {
		t.Fatalf("stale recovery started %d handoffs, want none", handoffs)
	}
}
