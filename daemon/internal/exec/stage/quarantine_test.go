package stage

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// testIntentEntry is testInvoke's intent file name in the driver directory.
var testIntentEntry = testRunIDFor(testInvoke) + ".json"

// reconcileClean runs Reconcile and fails the test unless the pass held and
// quarantined nothing, the outcome a nil Reconcile error meant before
// recovery isolated each intent (#386).
func reconcileClean(t *testing.T, ctx context.Context, d *Driver) {
	t.Helper()
	report, err := d.Reconcile(ctx, nil)
	if err != nil || len(report.Held) != 0 || len(report.Quarantined) != 0 {
		t.Fatalf("Reconcile = %+v, %v; want a clean pass", report, err)
	}
}

// reconcileHeld runs Reconcile and returns the error of the one intent the
// pass held, failing the test unless it held exactly one and quarantined
// none.
func reconcileHeld(t *testing.T, ctx context.Context, d *Driver) error {
	t.Helper()
	report, err := d.Reconcile(ctx, nil)
	if err != nil || len(report.Held) != 1 || len(report.Quarantined) != 0 {
		t.Fatalf("Reconcile = %+v, %v; want one held intent", report, err)
	}
	return report.Held[0].Err
}

// reconcileQuarantined runs Reconcile and returns the one intent the pass
// quarantined, failing the test unless it quarantined exactly one and held
// none.
func reconcileQuarantined(t *testing.T, ctx context.Context, d *Driver) IntentFailure {
	t.Helper()
	report, err := d.Reconcile(ctx, nil)
	if err != nil || len(report.Held) != 0 || len(report.Quarantined) != 1 {
		t.Fatalf("Reconcile = %+v, %v; want one quarantined intent", report, err)
	}
	return report.Quarantined[0]
}

// listClean lists the driver's intents and fails the test on any failure.
func listClean(t *testing.T, ctx context.Context, d *Driver) {
	t.Helper()
	if _, failures, err := d.listIntents(ctx); err != nil || len(failures) != 0 {
		t.Fatalf("listIntents = %+v, %v; want every intent enumerated", failures, err)
	}
}

// listFailure lists the driver's intents and returns the one failure the
// listing reports for entry, failing the test unless there is exactly one
// failure, it names entry, and no intent was enumerated from entry.
func listFailure(t *testing.T, ctx context.Context, d *Driver, entry string) intentListingFailure {
	t.Helper()
	intents, failures, err := d.listIntents(ctx)
	if err != nil || len(failures) != 1 || failures[0].entry != entry {
		t.Fatalf("listIntents = %+v, %v; want one failure for %s", failures, err, entry)
	}
	for _, in := range intents {
		if filepath.Base(d.intentPath(in.RunID)) == entry {
			t.Fatalf("listIntents enumerated the failed entry %s", entry)
		}
	}
	return failures[0]
}

// quarantineSecond is the second invocation the quarantine tests leave in the
// driver directory beside testInvoke.
const quarantineSecond = domain.InvocationID("inv-implement-run-2")

// quarantineOrders names each directory order the bad intent can take, so a
// test proves isolation whatever position the bad entry has.
var quarantineOrders = []string{"bad first", "bad last"}

// quarantinePair returns the bad and the good invocation for order, by the
// order os.ReadDir lists their intent files.
func quarantinePair(order string) (bad, good domain.InvocationID) {
	first, last := testInvoke, quarantineSecond
	if testRunIDFor(last) < testRunIDFor(first) {
		first, last = last, first
	}
	if order == "bad first" {
		return first, last
	}
	return last, first
}

func specFor(id domain.InvocationID) exec.StartSpec {
	spec := testStartSpec()
	spec.AttemptID = "attempt-" + domain.AttemptID(id)
	spec.Workspace = testWorkspaceFor(id)
	return spec
}

// lostRecoveryGate proves the loss of every run except the one in failRun,
// for which it returns failErr.
func lostRecoveryGate(failRun string, failErr error) *stubGate {
	return &stubGate{recoverFn: func(runID string, _ ward.HandoffSpec) (*ward.RecoveryResult, error) {
		if runID == failRun {
			return nil, failErr
		}
		return &ward.RecoveryResult{Outcome: ward.RecoveryLoss}, nil
	}}
}

// requireAdopted checks that recovery ran for id's orphan: the scripted loss
// is now its durable terminal.
func requireAdopted(t *testing.T, ctx context.Context, d *Driver, id domain.InvocationID) {
	t.Helper()
	if status, err := d.Inspect(ctx, id); err != nil || status.Status != exec.StatusGone {
		t.Fatalf("Inspect of the valid orphan = %+v, %v; want gone", status, err)
	}
	if _, err := d.Collect(ctx, id); !errors.Is(err, exec.ErrNoResult) {
		t.Fatalf("Collect of the valid orphan = %v, want ErrNoResult", err)
	}
}

// requireQuarantinedRunning checks Inspect answers running, not live, for a
// quarantined invocation, without an error that would abort the engine pass.
func requireQuarantinedRunning(t *testing.T, ctx context.Context, d *Driver, id domain.InvocationID) {
	t.Helper()
	if status, err := d.Inspect(ctx, id); err != nil || status != (exec.Inspection{Status: exec.StatusRunning}) {
		t.Fatalf("Inspect of the quarantined invocation = %+v, %v; want running, not live", status, err)
	}
}

// TestReconcileQuarantinesACorruptIntentAndAdoptsTheRest is the regression
// for one unreadable intent costing every other orphan its recovery pass and
// stopping the daemon from starting (#386).
func TestReconcileQuarantinesACorruptIntentAndAdoptsTheRest(t *testing.T) {
	t.Parallel()
	for _, order := range quarantineOrders {
		t.Run(order, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			bad, good := quarantinePair(order)
			exports := newStubExports()
			d := newTestDriver(t, lostRecoveryGate("", nil), exports)
			orphanInvocation(t, d, good, phaseRunning, nil, specFor(good))
			corrupt := []byte(`{"invocation_id":"` + string(bad) + `","phase":"runn`)
			badPath := d.intentPath(testRunIDFor(bad))
			if err := os.WriteFile(badPath, corrupt, 0o600); err != nil {
				t.Fatal(err)
			}

			failure := reconcileQuarantined(t, ctx, d)
			if failure.Entry != testRunIDFor(bad)+".json" || failure.Path != badPath ||
				failure.Invocation != "" || failure.Cause != QuarantineUnreadable ||
				!strings.Contains(failure.Err.Error(), "decode driver intent") {
				t.Fatalf("quarantined %+v, want the corrupt file as unreadable", failure)
			}
			requireAdopted(t, ctx, d, good)
			requireQuarantinedRunning(t, ctx, d, bad)

			if body, err := os.ReadFile(d.intentPath(testRunIDFor(bad))); err != nil || !bytes.Equal(body, corrupt) {
				t.Fatalf("quarantined intent changed: %q, %v", body, err)
			}
			if _, recorded := exports.outcomes[bad]; recorded {
				t.Fatal("an outcome was committed for the quarantined intent")
			}
		})
	}
}

// TestReconcileQuarantinesAnExportConflictAndAdoptsTheRest is the regression
// for errExportAuthorityConflict stopping every intent after it (#386). The
// conflicting intent, its export directory, and its export record stay as
// they were, and no outcome is committed for it.
func TestReconcileQuarantinesAnExportConflictAndAdoptsTheRest(t *testing.T) {
	t.Parallel()
	for _, order := range quarantineOrders {
		t.Run(order, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			bad, good := quarantinePair(order)
			exports := newStubExports()
			d := newTestDriver(t, lostRecoveryGate("", nil), exports)
			manifest := export.Manifest{Version: export.ManifestVersion, Entries: []export.Entry{}}
			manifestBody, err := manifest.Encode()
			if err != nil {
				t.Fatal(err)
			}
			record, err := domain.NewExecutionExport(domain.ExecutionExportInput{
				InvocationID: bad, AdmissionID: testStartSpec().AdmissionID,
				ObservedBaseSHA: testBase.BaseSHA, HeadSHA: strings.Repeat("c", 40),
				ManifestDigest: digestOf(append(manifestBody, '\n')), RecordedAt: fixedNow,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := exports.RecordExecutionExport(ctx, record, ExecutionReplay{}); err != nil {
				t.Fatal(err)
			}
			exportDir, err := os.MkdirTemp(os.TempDir(), "freeside-handoff-"+testRunIDFor(bad)+"-out-quarantine-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(exportDir) })
			sentinel := filepath.Join(exportDir, "work")
			if err := os.WriteFile(sentinel, []byte("agent work"), 0o600); err != nil {
				t.Fatal(err)
			}
			orphanInvocation(t, d, bad, phaseExported, &releasedExport{
				Dir: exportDir, Manifest: manifest, ObservedBaseSHA: testBase.BaseSHA,
			}, specFor(bad))
			orphanInvocation(t, d, good, phaseRunning, nil, specFor(good))
			before, err := os.ReadFile(d.intentPath(testRunIDFor(bad)))
			if err != nil {
				t.Fatal(err)
			}

			failure := reconcileQuarantined(t, ctx, d)
			if failure.Entry != testRunIDFor(bad)+".json" || failure.Invocation != bad ||
				failure.Cause != QuarantineExportConflict || !errors.Is(failure.Err, errExportAuthorityConflict) {
				t.Fatalf("quarantined %+v, want the export conflict", failure)
			}
			requireAdopted(t, ctx, d, good)
			requireQuarantinedRunning(t, ctx, d, bad)

			if after, err := os.ReadFile(d.intentPath(testRunIDFor(bad))); err != nil || !bytes.Equal(after, before) {
				t.Fatalf("quarantined intent changed: %v", err)
			}
			if body, err := os.ReadFile(sentinel); err != nil || string(body) != "agent work" { //nolint:gosec // the test's own file under its temporary export directory
				t.Fatalf("quarantined export directory changed: %q, %v", body, err)
			}
			if _, recorded := exports.outcomes[bad]; recorded {
				t.Fatal("an outcome was committed for the quarantined intent")
			}
			if got := exports.records[bad]; !reflect.DeepEqual(got, record) {
				t.Fatalf("export record = %+v, want %+v unchanged", got, record)
			}
		})
	}
}

// startRefusingAuthority refuses the current-policy start check for one
// invocation only, so a test can hold or quarantine it beside a valid orphan.
type startRefusingAuthority struct {
	stubAuthority
	refused domain.InvocationID
	err     error
}

func (a startRefusingAuthority) AuthenticateStart(
	ctx context.Context, id domain.InvocationID, spec exec.StartSpec,
) error {
	if id == a.refused {
		return a.err
	}
	return a.stubAuthority.AuthenticateStart(ctx, id, spec)
}

// TestReconcileHeldFailuresNoLongerStopThePass covers both held shapes: a
// retryable recovery and a failure the injected classifier accepts. Each
// stays held, unquarantined, while the pass still adopts the other orphan.
// The same refusal with no classifier is quarantined, so the classifier is
// what holds it.
func TestReconcileHeldFailuresNoLongerStopThePass(t *testing.T) {
	t.Parallel()
	policyRefusal := errors.New("mutable admission policy changed")
	for _, order := range quarantineOrders {
		t.Run(order+"/retryable", func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			bad, good := quarantinePair(order)
			wardErr := errors.New("temporary journal read failure")
			d := newTestDriver(t, lostRecoveryGate(testRunIDFor(bad), wardErr), newStubExports())
			orphanInvocation(t, d, bad, phaseRunning, nil, specFor(bad))
			orphanInvocation(t, d, good, phaseRunning, nil, specFor(good))

			if err := reconcileHeld(t, ctx, d); !errors.Is(err, ErrRecoveryRetryable) || !errors.Is(err, wardErr) {
				t.Fatalf("held error = %v, want the retryable ward error", err)
			}
			requireAdopted(t, ctx, d, good)
			if reconstructed, err := d.loadIntent(ctx, bad); err != nil || reconstructed.Phase != phaseRunning {
				t.Fatalf("held intent = %+v, %v; want it still running", reconstructed, err)
			}
		})
		for _, classified := range []bool{true, false} {
			name := order + "/policy refusal held"
			if !classified {
				name = order + "/policy refusal unclassified"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				ctx := t.Context()
				bad, good := quarantinePair(order)
				d := newTestDriver(t, lostRecoveryGate("", nil), newStubExports())
				orphanInvocation(t, d, bad, phaseSeeding, nil, specFor(bad))
				orphanInvocation(t, d, good, phaseRunning, nil, specFor(good))
				d.authority = startRefusingAuthority{refused: bad, err: policyRefusal}
				var held func(error) bool
				if classified {
					held = func(err error) bool { return errors.Is(err, policyRefusal) }
				}

				report, err := d.Reconcile(ctx, held)
				if err != nil {
					t.Fatal(err)
				}
				failures := report.Quarantined
				if classified {
					if len(report.Quarantined) != 0 {
						t.Fatalf("a held refusal was quarantined: %+v", report)
					}
					failures = report.Held
				} else if len(report.Held) != 0 {
					t.Fatalf("an unclassified refusal was held: %+v", report)
				}
				if len(failures) != 1 || failures[0].Invocation != bad ||
					failures[0].Cause != QuarantineUnauthenticated || !errors.Is(failures[0].Err, policyRefusal) {
					t.Fatalf("report = %+v, want the refused intent", report)
				}
				requireAdopted(t, ctx, d, good)
				if !classified {
					before, err := os.ReadFile(d.intentPath(testRunIDFor(bad)))
					if err != nil {
						t.Fatal(err)
					}
					requireQuarantinedRunning(t, ctx, d, bad)
					// Admission alone would authenticate this intent, so a
					// stop must be refused rather than commit an outcome.
					if err := d.CancelAndConfirm(ctx, bad); !errors.Is(err, errIntentQuarantined) {
						t.Fatalf("stop of the quarantined invocation = %v, want refused", err)
					}
					if after, err := os.ReadFile(d.intentPath(testRunIDFor(bad))); err != nil || !bytes.Equal(after, before) {
						t.Fatalf("quarantined intent changed: %v", err)
					}
					requireQuarantinedRunning(t, ctx, d, bad)
				}
			})
		}
	}
}

// TestQuarantineNamesOnlyTheEntrysOwnInvocation checks that a stale copy
// naming a healthy invocation is quarantined without naming that
// invocation, which is adopted from its own file and never held.
func TestQuarantineNamesOnlyTheEntrysOwnInvocation(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	d := newTestDriver(t, lostRecoveryGate("", nil), newStubExports())
	orphan(t, d, phaseRunning, nil)
	body, err := os.ReadFile(d.intentPath(testRunIDFor(testInvoke)))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.dir, "stale-copy.json"), body, 0o600); err != nil { //nolint:gosec // fixed test filename under the driver's private temporary state directory
		t.Fatal(err)
	}

	failure := reconcileQuarantined(t, ctx, d)
	if failure.Entry != "stale-copy.json" || failure.Invocation != "" || failure.Cause != QuarantineUnreadable ||
		!errors.Is(failure.Err, ErrUnsupportedStart) {
		t.Fatalf("quarantined %+v, want the stale copy with no invocation named", failure)
	}
	requireAdopted(t, ctx, d, testInvoke)
}

// TestReconcileFailsWhenTheIntentDirectoryCannotBeListed keeps the one
// whole-pass failure startup still refuses on.
func TestReconcileFailsWhenTheIntentDirectoryCannotBeListed(t *testing.T) {
	t.Parallel()
	d := newTestDriver(t, &stubGate{}, newStubExports())
	if err := os.RemoveAll(d.dir); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Reconcile(t.Context(), nil); err == nil || !strings.Contains(err.Error(), "list driver intents") {
		t.Fatalf("Reconcile over an unlistable directory = %v, want the listing error", err)
	}
}

func TestQuarantineCausesAreRegistered(t *testing.T) {
	t.Parallel()
	for _, cause := range AllQuarantineCauses {
		if !cause.valid() {
			t.Fatalf("registered cause %q is invalid", cause)
		}
	}
	if QuarantineCause("").valid() {
		t.Fatal("the zero cause is valid")
	}
}
