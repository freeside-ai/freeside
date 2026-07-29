package claude

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

type blockingSeeder struct {
	entered chan struct{}
	release chan struct{}
}

func (s blockingSeeder) FetchBase(context.Context, string, string, string, string) error {
	close(s.entered)
	<-s.release
	return errors.New("stop after observing recovered phase")
}

func (s blockingSeeder) FetchBaseWorktree(
	context.Context, string, string, string, string,
) error {
	close(s.entered)
	<-s.release
	return errors.New("stop after observing recovered phase")
}

// orphan writes a durable intent in the given phase with no live pipeline:
// exactly what a crashed daemon leaves behind.
func orphan(t *testing.T, d *Driver, ph phase, released *releasedExport) intent {
	t.Helper()
	spec := testStartSpec()
	inputs := stageInputs(t, &spec)
	instructions, err := ward.VendorInstructionsFromStageInputs(inputs)
	if err != nil {
		t.Fatalf("vendor instructions: %v", err)
	}
	prompt, err := renderPrompt(inputs)
	if err != nil {
		t.Fatalf("render prompt: %v", err)
	}
	in := intent{
		InvocationID: testInvoke, RunID: RunIDFor(testInvoke), Phase: ph, Spec: spec,
		Seed: filepath.Join(d.seedRoot, RunIDFor(testInvoke)), Prompt: prompt,
		Inputs: durableInputsFrom(inputs), Instructions: instructions, Export: released,
		RecordedAt: fixedNow, CommitDate: fixedNow,
	}
	if err := d.saveIntent(in); err != nil {
		t.Fatalf("save orphan intent: %v", err)
	}
	return in
}

// TestRecoveryErrorPreservesTheRunningIntent is the regression for treating
// an operational ward recovery error as a terminal stage failure. Until ward
// returns an explicit exported or loss outcome, the credential-bearing writer
// may still exist; committing a failure would skip every future teardown.
func TestRecoveryErrorPreservesTheRunningIntent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	recoveryErr := errors.New("temporary journal read failure")
	gate := &stubGate{
		recoverFn: func(string, ward.HandoffSpec) (*ward.RecoveryResult, error) {
			return nil, recoveryErr
		},
	}
	d := newTestDriver(t, gate, newStubExports())
	orphan(t, d, phaseRunning, nil)

	if err := d.Reconcile(ctx); !errors.Is(err, ErrRecoveryRetryable) ||
		!errors.Is(err, recoveryErr) {
		t.Fatalf("Reconcile error = %v, want retryable ward error", err)
	}
	reconstructed, err := d.loadIntent(ctx, testInvoke)
	if err != nil {
		t.Fatalf("load running intent: %v", err)
	}
	if reconstructed.Phase != phaseRunning || reconstructed.Result != nil {
		t.Fatalf("intent after recovery error = %#v, want uncommitted running phase", reconstructed)
	}
	if _, err := d.Collect(ctx, testInvoke); !errors.Is(err, exec.ErrResultNotReady) {
		t.Fatalf("Collect after recovery error = %v, want ErrResultNotReady", err)
	}
}

func TestRecoveredWriterFailureCommitsCanonicalOutcome(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	outcomes := newStubExports()
	gate := &stubGate{
		recoverFn: func(string, ward.HandoffSpec) (*ward.RecoveryResult, error) {
			return &ward.RecoveryResult{
				Outcome: ward.RecoveryFailed, FailureStatus: 7,
			}, nil
		},
	}
	d := newTestDriver(t, gate, outcomes)
	orphan(t, d, phaseRunning, nil)

	if err := d.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	result, err := d.Collect(ctx, testInvoke)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if result.Status != exec.StatusFailed || !strings.Contains(result.Summary, "status 7") {
		t.Fatalf("result = %+v, want failed status 7", result)
	}
	outcome, found, err := outcomes.LookupExecutionOutcome(ctx, testInvoke)
	if err != nil || !found {
		t.Fatalf("LookupExecutionOutcome = (%+v, %v, %v)", outcome, found, err)
	}
	if outcome.Status != domain.ExecutionOutcomeFailed {
		t.Fatalf("outcome = %+v, want failed", outcome)
	}
}

func TestRecoveredCancellationCommitsCanonicalOutcome(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	outcomes := newStubExports()
	gate := &stubGate{
		recoverFn: func(string, ward.HandoffSpec) (*ward.RecoveryResult, error) {
			return &ward.RecoveryResult{Outcome: ward.RecoveryCanceled}, nil
		},
	}
	d := newTestDriver(t, gate, outcomes)
	orphan(t, d, phaseRunning, nil)

	if err := d.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	result, err := d.Collect(ctx, testInvoke)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if result.Status != exec.StatusCanceled {
		t.Fatalf("result = %+v, want canceled", result)
	}
	outcome, found, err := outcomes.LookupExecutionOutcome(ctx, testInvoke)
	if err != nil || !found {
		t.Fatalf("LookupExecutionOutcome = (%+v, %v, %v)", outcome, found, err)
	}
	if outcome.Status != domain.ExecutionOutcomeCanceled {
		t.Fatalf("outcome = %+v, want canceled", outcome)
	}
}

func TestDurableOutcomeRestoresBeforeMutablePolicy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		initial    phase
		outcome    domain.ExecutionOutcomeStatus
		summary    string
		wantPhase  phase
		wantStatus exec.Status
	}{
		{"failed", phaseSeeding, domain.ExecutionOutcomeFailed, "input materialization failed", phaseCommitted, exec.StatusFailed},
		{"canceled", phaseExported, domain.ExecutionOutcomeCanceled, "daemon stopped", phaseCommitted, exec.StatusCanceled},
		{"lost", phaseRunning, domain.ExecutionOutcomeLost, "", phaseLost, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			outcomes := newStubExports()
			d := newTestDriver(t, &stubGate{}, outcomes)
			var released *releasedExport
			if tc.initial == phaseExported {
				released = &releasedExport{Dir: filepath.Join(os.TempDir(),
					"freeside-handoff-"+RunIDFor(testInvoke)+"-out-stale")}
			}
			orphan(t, d, tc.initial, released)
			if err := outcomes.RecordExecutionOutcome(ctx, domain.ExecutionOutcome{
				InvocationID: testInvoke,
				AdmissionID:  testStartSpec().AdmissionID,
				Status:       tc.outcome,
				Summary:      tc.summary,
				RecordedAt:   fixedNow,
			}); err != nil {
				t.Fatal(err)
			}
			d.authority = stubAuthority{startErr: domain.ErrTrustProfileSuperseded}

			if err := d.Reconcile(ctx); err != nil {
				t.Fatalf("Reconcile outcome-written crash window: %v", err)
			}
			body, err := os.ReadFile(d.intentPath(testInvoke))
			if err != nil {
				t.Fatal(err)
			}
			reconstructed, err := decodeIntent(body)
			if err != nil {
				t.Fatal(err)
			}
			if reconstructed.Phase != tc.wantPhase || reconstructed.Export != nil {
				t.Fatalf("reconstructed intent = %#v, want phase %q without export", reconstructed, tc.wantPhase)
			}
			if tc.wantPhase == phaseCommitted &&
				(reconstructed.Result == nil ||
					reconstructed.Result.Status != tc.wantStatus ||
					reconstructed.Result.Summary != tc.summary) {
				t.Fatalf("reconstructed result = %#v, want %q", reconstructed.Result, tc.wantStatus)
			}
			if tc.wantPhase == phaseLost && reconstructed.Result != nil {
				t.Fatalf("lost intent carries result %#v", reconstructed.Result)
			}
			if _, err := d.loadIntent(ctx, testInvoke); err != nil {
				t.Fatalf("terminal history re-applied mutable policy: %v", err)
			}
		})
	}
}

// TestLiveHandoffErrorRetriesRecoveryWithoutRestart closes the gap between
// preserving phaseRunning and actually driving it again. The engine calls
// Inspect on every pass; a transient ward error must hold the invocation
// running, and a later explicit loss must converge without restarting the
// daemon.
func TestLiveHandoffErrorRetriesRecoveryWithoutRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	recoveryCalls := 0
	gate := &stubGate{
		handoffFn: func(ward.HandoffSpec) (*ward.HandoffResult, error) {
			return nil, errors.New("handoff returned before teardown was proven")
		},
		recoverFn: func(string, ward.HandoffSpec) (*ward.RecoveryResult, error) {
			recoveryCalls++
			if recoveryCalls == 1 {
				return nil, errors.New("temporary journal read failure")
			}
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

	if status, err := d.Inspect(ctx, testInvoke); err != nil || status != exec.StatusRunning {
		t.Fatalf("Inspect after transient recovery = %q, %v; want running", status, err)
	}
	if recoveryCalls != 1 {
		t.Fatalf("recovery calls after first Inspect = %d, want 1", recoveryCalls)
	}
	if status, err := d.Inspect(ctx, testInvoke); err != nil || status != exec.StatusGone {
		t.Fatalf("Inspect after proven loss = %q, %v; want gone", status, err)
	}
	if _, err := d.Collect(ctx, testInvoke); !errors.Is(err, exec.ErrNoResult) {
		t.Fatalf("Collect after proven loss = %v, want ErrNoResult", err)
	}
}

// TestHandoffReturnPersistenceRetriesBeforeClosedJournalRecovery covers the
// last closed-journal crash window: once Handoff returns, its full return must
// survive before authentication or validation runs. A local state-write
// failure retains that exact exported intent in process; the next Inspect
// retries the write and never asks ward to recover its already-closed record.
func TestHandoffReturnPersistenceRetriesBeforeClosedJournalRecovery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	authErr := errors.New("temporary journal read failure")
	recovered := false
	var (
		d      *Driver
		outDir string
	)
	gate := &stubGate{
		handoffFn: func(ward.HandoffSpec) (*ward.HandoffResult, error) {
			manifest := export.Manifest{Version: export.ManifestVersion, Entries: []export.Entry{}}
			body, err := manifest.Encode()
			if err != nil {
				return nil, err
			}
			outDir, err = os.MkdirTemp("", "freeside-handoff-"+RunIDFor(testInvoke)+"-out-")
			if err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(outDir, export.ManifestFilename), body, 0o600); err != nil {
				return nil, err
			}
			if err := os.Chmod(d.dir, 0o500); err != nil { //nolint:gosec // G302: state directory, not a file
				return nil, err
			}
			return &ward.HandoffResult{
				ExportDir: outDir, Manifest: manifest,
				Workspace: ward.WorkspaceObservation{ObservedBaseSHA: testBase.BaseSHA},
			}, nil
		},
		recoverFn: func(string, ward.HandoffSpec) (*ward.RecoveryResult, error) {
			recovered = true
			return nil, errors.New("closed journal must not be recovered")
		},
		authenticateFn: func(string, string) error { return authErr },
	}
	d = newTestDriver(t, gate, newStubExports())
	t.Cleanup(func() {
		_ = os.Chmod(d.dir, 0o700) //nolint:gosec // G302: state directory, not a file
		if outDir != "" {
			_ = os.RemoveAll(outDir)
		}
	})
	spec := testStartSpec()
	inputs := stageInputs(t, &spec)
	if err := d.StartWithInputs(ctx, testInvoke, spec,
		func(context.Context) (exec.StageInputs, error) { return inputs, nil },
	); err != nil {
		t.Fatalf("StartWithInputs: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		d.mu.Lock()
		sess := d.running[testInvoke]
		pending := sess != nil && sess.pendingIntent != nil
		finished := false
		if sess != nil {
			select {
			case <-sess.done:
				finished = true
			default:
			}
		}
		d.mu.Unlock()
		if pending && finished {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("returned handoff was not retained after the state write failed")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if err := os.Chmod(d.dir, 0o700); err != nil { //nolint:gosec // G302: state directory, not a file
		t.Fatalf("restore driver state permissions: %v", err)
	}
	if status, err := d.Inspect(ctx, testInvoke); err != nil || status != exec.StatusRunning {
		t.Fatalf("Inspect after persistence retry = %q, %v; want running", status, err)
	}
	body, err := os.ReadFile(d.intentPath(testInvoke))
	if err != nil {
		t.Fatalf("read retried intent: %v", err)
	}
	in, err := decodeIntent(body)
	if err != nil {
		t.Fatalf("decode retried intent: %v", err)
	}
	if in.Phase != phaseExported || in.Export == nil || in.Export.Dir != outDir {
		t.Fatalf("retried intent = %#v, want exact exported handoff", in)
	}
	if recovered {
		t.Fatal("driver asked ward to recover a handoff whose return was retained")
	}
}

// TestRecoveryReturnPersistenceRetriesBeforeClosedJournalRecovery covers the
// sibling closed-journal window after ward recovery itself returns an export.
// The exact return is retained when its first exported-phase write fails, so
// the next inspection persists it without calling Recover a second time.
func TestRecoveryReturnPersistenceRetriesBeforeClosedJournalRecovery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	authErr := errors.New("temporary journal read failure")
	recoveryCalls := 0
	var (
		d      *Driver
		outDir string
	)
	gate := &stubGate{
		recoverFn: func(string, ward.HandoffSpec) (*ward.RecoveryResult, error) {
			recoveryCalls++
			manifest := export.Manifest{Version: export.ManifestVersion, Entries: []export.Entry{}}
			body, err := manifest.Encode()
			if err != nil {
				return nil, err
			}
			outDir, err = os.MkdirTemp("", "freeside-handoff-"+RunIDFor(testInvoke)+"-out-")
			if err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(outDir, export.ManifestFilename), body, 0o600); err != nil {
				return nil, err
			}
			if err := os.Chmod(d.dir, 0o500); err != nil { //nolint:gosec // G302: state directory, not a file
				return nil, err
			}
			return &ward.RecoveryResult{
				Outcome: ward.RecoveryExported, ExportDir: outDir, Manifest: manifest,
				Workspace: ward.WorkspaceObservation{ObservedBaseSHA: testBase.BaseSHA},
			}, nil
		},
		authenticateFn: func(string, string) error { return authErr },
	}
	d = newTestDriver(t, gate, newStubExports())
	t.Cleanup(func() {
		_ = os.Chmod(d.dir, 0o700) //nolint:gosec // G302: state directory, not a file
		if outDir != "" {
			_ = os.RemoveAll(outDir)
		}
	})
	orphan(t, d, phaseRunning, nil)

	if err := d.Reconcile(ctx); !errors.Is(err, ErrRecoveryRetryable) {
		t.Fatalf("Reconcile after state-write failure = %v, want retryable", err)
	}
	d.mu.Lock()
	sess := d.running[testInvoke]
	pending := sess != nil && sess.pendingIntent != nil
	d.mu.Unlock()
	if !pending {
		t.Fatal("recovery return was not retained after the state write failed")
	}

	if err := os.Chmod(d.dir, 0o700); err != nil { //nolint:gosec // G302: state directory, not a file
		t.Fatalf("restore driver state permissions: %v", err)
	}
	if status, err := d.Inspect(ctx, testInvoke); err != nil || status != exec.StatusRunning {
		t.Fatalf("Inspect after persistence retry = %q, %v; want running", status, err)
	}
	body, err := os.ReadFile(d.intentPath(testInvoke))
	if err != nil {
		t.Fatalf("read retried intent: %v", err)
	}
	in, err := decodeIntent(body)
	if err != nil {
		t.Fatalf("decode retried intent: %v", err)
	}
	if in.Phase != phaseExported || in.Export == nil || in.Export.Dir != outDir {
		t.Fatalf("retried recovery intent = %#v, want exact exported handoff", in)
	}
	if recoveryCalls != 1 {
		t.Fatalf("ward Recover calls = %d, want 1", recoveryCalls)
	}
}

// TestPreHandoffCrashRerunsInsteadOfRecovering: with no gate call made, no
// ward object and no journal record exist, so the work is rerun-safe and
// must not be handed to a recovery that has nothing to find.
func TestPreHandoffCrashRerunsInsteadOfRecovering(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	recovered := false
	gate := &stubGate{
		handoffStarted: func(string) (bool, error) {
			return false, nil
		},
		recoverFn: func(string, ward.HandoffSpec) (*ward.RecoveryResult, error) {
			recovered = true
			return nil, errors.New("no journal record")
		},
		handoffFn: func(ward.HandoffSpec) (*ward.HandoffResult, error) {
			return nil, errors.New("handoff refused in this test")
		},
	}
	d := newTestDriver(t, gate, newStubExports())
	orphan(t, d, phaseSeeding, nil)

	if err := d.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	waitSessionDone(t, d, testInvoke)
	if recovered {
		t.Error("a pre-handoff intent was sent to gate recovery, which has no record to find")
	}
	if len(gate.specs) == 0 {
		t.Error("a pre-handoff intent was not re-run through the gate")
	}
	reconstructed, err := d.loadIntent(ctx, testInvoke)
	if err != nil {
		t.Fatalf("load rerun intent: %v", err)
	}
	if reconstructed.Phase != phaseRunning || reconstructed.Result != nil {
		t.Fatalf("rerun handoff failure = %#v, want running recovery state", reconstructed)
	}

	if status, err := d.Inspect(ctx, testInvoke); err != nil {
		t.Fatalf("Inspect pre-journal refusal: %v", err)
	} else if status != exec.StatusRunning {
		t.Fatalf("Inspect status = %q, want running rerun", status)
	}
	waitSessionDone(t, d, testInvoke)
	if recovered {
		t.Error("a confirmed pre-journal refusal was sent to gate recovery")
	}
	if len(gate.specs) < 2 {
		t.Fatalf("gate handoff calls = %d, want the refused handoff rerun", len(gate.specs))
	}
}

func TestOnlyPreterminalIntentsRequireCurrentConformance(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	drift := errors.New("current backend conformance changed")

	for _, preterminalPhase := range []phase{phaseSeeding, phaseRunning, phaseExported} {
		t.Run(string(preterminalPhase), func(t *testing.T) {
			t.Parallel()
			d := newTestDriver(t, &stubGate{}, newStubExports())
			var released *releasedExport
			if preterminalPhase == phaseExported {
				released = &releasedExport{
					Dir: filepath.Join(os.TempDir(),
						"freeside-handoff-"+RunIDFor(testInvoke)+"-out-unrecorded"),
					Manifest:        export.Manifest{Version: export.ManifestVersion, Entries: []export.Entry{}},
					ObservedBaseSHA: testBase.BaseSHA,
				}
			}
			orphan(t, d, preterminalPhase, released)
			d.authority = stubAuthority{startErr: drift}
			if _, err := d.listIntents(ctx); !errors.Is(err, drift) {
				t.Fatalf("list %s intent = %v, want current-conformance refusal",
					preterminalPhase, err)
			}
		})
	}

	for _, terminalPhase := range []phase{phaseCommitted, phaseLost} {
		t.Run(string(terminalPhase), func(t *testing.T) {
			t.Parallel()
			d := newTestDriver(t, &stubGate{}, newStubExports())
			switch terminalPhase {
			case phaseCommitted:
				orphan(t, d, phaseSeeding, nil)
				if err := d.commitResult(testInvoke, exec.StageResult{
					InvocationID: testInvoke,
					Status:       exec.StatusFailed,
					Summary:      "failed",
				}); err != nil {
					t.Fatalf("commit failed result: %v", err)
				}
			case phaseLost:
				orphan(t, d, phaseRunning, nil)
				if err := d.commitLost(ctx, testInvoke); err != nil {
					t.Fatalf("commit lost result: %v", err)
				}
			case phaseSeeding, phaseRunning, phaseExported:
				t.Fatalf("test table contains nonterminal phase %q", terminalPhase)
			}

			d.authority = stubAuthority{startErr: drift}
			if err := d.Reconcile(ctx); err != nil {
				t.Fatalf("Reconcile terminal intent after conformance drift: %v", err)
			}
			_, err := d.Collect(ctx, testInvoke)
			if terminalPhase == phaseLost {
				if !errors.Is(err, exec.ErrNoResult) {
					t.Fatalf("Collect lost intent = %v, want ErrNoResult", err)
				}
			} else if err != nil {
				t.Fatalf("Collect committed intent after conformance drift: %v", err)
			}
		})
	}
}

func TestReconcileCleansSeedAfterTerminalSaveCrashWindow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newTestDriver(t, &stubGate{}, newStubExports())
	in := orphan(t, d, phaseSeeding, nil)
	result := exec.StageResult{
		InvocationID: testInvoke,
		Status:       exec.StatusFailed,
		Summary:      "durable failure",
	}
	if err := d.recordOutcome(ctx, in, result); err != nil {
		t.Fatalf("record outcome: %v", err)
	}
	in.Phase = phaseCommitted
	in.Result = &result
	if err := d.saveIntent(in); err != nil {
		t.Fatalf("save terminal intent: %v", err)
	}
	for _, path := range []string{in.Seed, in.Seed + "-import"} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	if err := d.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile terminal cleanup: %v", err)
	}
	for _, path := range []string{in.Seed, in.Seed + "-import"} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("restart cleanup left %s: %v", path, err)
		}
	}
}

// TestPostHandoffCrashFinishesFromTheRecordedExport: once Handoff returns,
// ward closes its journal and refuses to recover the record, so the driver
// carries the released facts and finishes the pipeline itself. With the
// released directory gone, the durable export record still names the head,
// which is enough to commit the same result rather than discard the work.
func TestPostHandoffCrashFinishesFromTheRecordedExport(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	exports := newStubExports()
	d := newTestDriver(t, &stubGate{}, exports)

	head := strings.Repeat("c", 40)
	manifest := export.Manifest{Version: export.ManifestVersion, Entries: []export.Entry{}}
	manifestBody, err := manifest.Encode()
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	artifactDigest := domain.Digest("sha256:" + strings.Repeat("66", 32))
	evidence := export.EvidenceManifest{
		Version: export.EvidenceManifestVersion,
		Entries: []export.EvidenceEntry{{
			Label: "agent-transcript", MediaType: "application/jsonl",
			Size: 1, Digest: export.Digest(artifactDigest),
			Provenance: export.EvidenceProvenance{
				ProducerClass:        export.EvidenceProducerAgent,
				ProducerInvocationID: string(testInvoke),
				HeadBinding:          export.EvidenceHeadIndependent,
				SensitivityClass:     export.EvidenceSensitivityNormal,
			},
		}},
	}
	evidenceBody, err := evidence.Encode()
	if err != nil {
		t.Fatalf("encode evidence: %v", err)
	}
	evidenceDigest := digestOf(evidenceBody)
	record, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: testInvoke, AdmissionID: domain.Digest("sha256:" + strings.Repeat("44", 32)),
		ObservedBaseSHA: testBase.BaseSHA, HeadSHA: head,
		ManifestDigest:         digestOf(manifestBody),
		EvidenceManifestDigest: &evidenceDigest,
		RecordedAt:             fixedNow,
	})
	if err != nil {
		t.Fatalf("new execution export: %v", err)
	}
	if err := exports.RecordExecutionExport(ctx, record); err != nil {
		t.Fatalf("record export: %v", err)
	}
	// The released directory did not survive the crash.
	orphan(t, d, phaseExported, &releasedExport{
		Dir: filepath.Join(os.TempDir(),
			"freeside-handoff-"+RunIDFor(testInvoke)+"-out-gone"),
		Manifest: manifest, Evidence: evidence, EvidencePresent: true,
		ObservedBaseSHA: testBase.BaseSHA,
	})

	if err := d.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	result, err := d.Collect(ctx, testInvoke)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if result.Status != exec.StatusCompleted || result.HeadSHA != head ||
		len(result.Artifacts) != 1 || result.Artifacts[0] != artifactDigest {
		t.Fatalf("recovered result = %#v, want the recorded head and evidence", result)
	}

	original, err := d.loadIntent(ctx, testInvoke)
	if err != nil {
		t.Fatalf("load completed intent: %v", err)
	}
	tamperings := []struct {
		name   string
		mutate func(*exec.StageResult)
	}{
		{"head", func(result *exec.StageResult) { result.HeadSHA = strings.Repeat("d", 40) }},
		{"artifacts", func(result *exec.StageResult) { result.Artifacts = nil }},
		{"status", func(result *exec.StageResult) { result.Status = exec.StatusFailed }},
	}
	for _, tc := range tamperings {
		t.Run("tampered committed "+tc.name, func(t *testing.T) {
			tampered := original
			copyResult := *original.Result
			copyResult.Artifacts = append([]domain.Digest(nil), original.Result.Artifacts...)
			tc.mutate(&copyResult)
			tampered.Result = &copyResult
			body, err := json.Marshal(tampered)
			if err != nil {
				t.Fatalf("encode tampered intent: %v", err)
			}
			if err := os.WriteFile(d.intentPath(testInvoke), body, 0o600); err != nil {
				t.Fatalf("write tampered intent: %v", err)
			}
			if _, err := d.Collect(ctx, testInvoke); !errors.Is(err, ErrUnsupportedStart) {
				t.Fatalf("Collect accepted tampered result: %v", err)
			}
		})
	}
	t.Run("tampered completed phase", func(t *testing.T) {
		tampered := original
		tampered.Phase = phaseLost
		tampered.Result = nil
		body, err := json.Marshal(tampered)
		if err != nil {
			t.Fatalf("encode tampered intent: %v", err)
		}
		if err := os.WriteFile(d.intentPath(testInvoke), body, 0o600); err != nil {
			t.Fatalf("write tampered intent: %v", err)
		}
		if _, err := d.Collect(ctx, testInvoke); !errors.Is(err, ErrUnsupportedStart) {
			t.Fatalf("Collect accepted a suppressed completed result: %v", err)
		}
	})
}

func TestRecordedExportOutranksAStaleReleasedDirectory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	exports := newStubExports()
	d := newTestDriver(t, &stubGate{}, exports)

	head := strings.Repeat("c", 40)
	manifest := export.Manifest{Version: export.ManifestVersion, Entries: []export.Entry{}}
	manifestBody, err := manifest.Encode()
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	record, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: testInvoke, AdmissionID: testStartSpec().AdmissionID,
		ObservedBaseSHA: testBase.BaseSHA, HeadSHA: head,
		ManifestDigest: digestOf(manifestBody), RecordedAt: fixedNow,
	})
	if err != nil {
		t.Fatalf("new execution export: %v", err)
	}
	if err := exports.RecordExecutionExport(ctx, record); err != nil {
		t.Fatalf("record export: %v", err)
	}
	dir, err := os.MkdirTemp("", "freeside-handoff-"+RunIDFor(testInvoke)+"-out-")
	if err != nil {
		t.Fatalf("create stale release: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.WriteFile(
		filepath.Join(dir, export.ManifestFilename), []byte("tampered"), 0o600,
	); err != nil {
		t.Fatalf("write stale manifest: %v", err)
	}
	orphan(t, d, phaseExported, &releasedExport{
		Dir: dir, Manifest: manifest, ObservedBaseSHA: testBase.BaseSHA,
	})
	d.authority = stubAuthority{startErr: errors.New("current conformance lapsed")}

	if err := d.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	result, err := d.Collect(ctx, testInvoke)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if result.Status != exec.StatusCompleted || result.HeadSHA != head {
		t.Fatalf("recovered result = %#v, want completed head %s", result, head)
	}
	if len(exports.outcomes) != 0 {
		t.Fatalf("recorded %d outcomes beside completed export, want none", len(exports.outcomes))
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale released directory remains with error %v", err)
	}
}

func TestConflictingDurableExportFailsRecoveryWithoutTerminalizing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	exports := newStubExports()
	d := newTestDriver(t, &stubGate{}, exports)
	manifest := export.Manifest{
		Version: export.ManifestVersion,
		Entries: []export.Entry{},
	}
	manifestBody, err := manifest.Encode()
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	record, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: testInvoke, AdmissionID: testStartSpec().AdmissionID,
		ObservedBaseSHA: testBase.BaseSHA, HeadSHA: strings.Repeat("c", 40),
		ManifestDigest: digestOf(append(manifestBody, '\n')), RecordedAt: fixedNow,
	})
	if err != nil {
		t.Fatalf("new conflicting export: %v", err)
	}
	if err := exports.RecordExecutionExport(ctx, record); err != nil {
		t.Fatalf("record conflicting export: %v", err)
	}
	orphan(t, d, phaseExported, &releasedExport{
		Dir: filepath.Join(os.TempDir(),
			"freeside-handoff-"+RunIDFor(testInvoke)+"-out-gone-conflict"),
		Manifest: manifest, ObservedBaseSHA: testBase.BaseSHA,
	})

	if err := d.Reconcile(ctx); !errors.Is(err, errExportAuthorityConflict) {
		t.Fatalf("Reconcile error = %v, want durable-authority conflict", err)
	}
	if len(exports.outcomes) != 0 {
		t.Fatalf("authority conflict recorded %d contradictory outcomes", len(exports.outcomes))
	}
	body, err := os.ReadFile(d.intentPath(testInvoke))
	if err != nil {
		t.Fatalf("read preserved conflict: %v", err)
	}
	reconstructed, err := decodeIntent(body)
	if err != nil {
		t.Fatalf("decode preserved conflict: %v", err)
	}
	if reconstructed.Phase != phaseExported || reconstructed.Result != nil {
		t.Fatalf("intent = %#v, want preserved exported conflict", reconstructed)
	}
}

// TestCommitResultPreservesTheDurableExportedIntent is the regression for a
// successful fresh handoff whose pipeline caller still held its original
// seeding-phase value. A terminal commit must advance the durable exported
// intent, not overwrite it with an older caller copy that has no Export.
func TestCommitResultPreservesTheDurableExportedIntent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	exports := newStubExports()
	d := newTestDriver(t, &stubGate{}, exports)

	head := strings.Repeat("c", 40)
	manifest := export.Manifest{Version: export.ManifestVersion, Entries: []export.Entry{}}
	manifestBody, err := manifest.Encode()
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	record, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: testInvoke, AdmissionID: testStartSpec().AdmissionID,
		ObservedBaseSHA: testBase.BaseSHA, HeadSHA: head,
		ManifestDigest: digestOf(manifestBody), RecordedAt: fixedNow,
	})
	if err != nil {
		t.Fatalf("new execution export: %v", err)
	}
	if err := exports.RecordExecutionExport(ctx, record); err != nil {
		t.Fatalf("record export: %v", err)
	}
	orphan(t, d, phaseExported, &releasedExport{
		Dir: filepath.Join(os.TempDir(),
			"freeside-handoff-"+RunIDFor(testInvoke)+"-out-consumed"),
		Manifest: manifest, ObservedBaseSHA: testBase.BaseSHA,
	})

	if err := d.commitResult(testInvoke, exec.StageResult{
		InvocationID: testInvoke, Status: exec.StatusCompleted, HeadSHA: head,
		Summary: "completed",
	}); err != nil {
		t.Fatal(err)
	}
	result, err := d.Collect(ctx, testInvoke)
	if err != nil {
		t.Fatalf("Collect rejected the committed success: %v", err)
	}
	if result.Status != exec.StatusCompleted || result.HeadSHA != head {
		t.Fatalf("result = %#v, want completed head %s", result, head)
	}
}

func TestRecoveredExportPhaseIsDurableBeforeImportResumes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in := intent{}
	var outDir string
	gate := &stubGate{recoverFn: func(string, ward.HandoffSpec) (*ward.RecoveryResult, error) {
		manifest := export.Manifest{Version: export.ManifestVersion, Entries: []export.Entry{}}
		body, err := manifest.Encode()
		if err != nil {
			return nil, err
		}
		outDir, err = os.MkdirTemp("", "freeside-handoff-"+in.RunID+"-out-")
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(outDir, export.ManifestFilename), body, 0o600); err != nil {
			return nil, err
		}
		return &ward.RecoveryResult{
			Outcome: ward.RecoveryExported, ExportDir: outDir, Manifest: manifest,
			Workspace: ward.WorkspaceObservation{ObservedBaseSHA: testBase.BaseSHA},
		}, nil
	}}
	d := newTestDriver(t, gate, newStubExports())
	seeder := blockingSeeder{entered: make(chan struct{}), release: make(chan struct{})}
	d.seeder = seeder
	in = orphan(t, d, phaseRunning, nil)

	done := make(chan error, 1)
	go func() { done <- d.recoverIntent(ctx, in) }()
	<-seeder.entered
	reconstructed, err := d.loadIntent(ctx, testInvoke)
	if err != nil {
		t.Fatalf("load intent while recovered import is blocked: %v", err)
	}
	if reconstructed.Phase != phaseExported || reconstructed.Export == nil {
		t.Fatalf("recovered intent = %#v, want durable exported phase", reconstructed)
	}
	close(seeder.release)
	if err := <-done; !errors.Is(err, ErrRecoveryRetryable) {
		t.Fatalf("recover intent = %v, want retryable finish failure", err)
	}
	if _, err := os.Stat(outDir); err != nil {
		t.Fatalf("recovered export was not preserved: %v", err)
	}
}

func TestExportLookupErrorLeavesRecoveryRetryable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	exports := newStubExports()
	exports.lookupErr = errors.New("temporary store failure")
	d := newTestDriver(t, &stubGate{}, exports)
	in := orphan(t, d, phaseExported, &releasedExport{
		Dir: filepath.Join(os.TempDir(),
			"freeside-handoff-"+RunIDFor(testInvoke)+"-out-gone"),
		Manifest:        export.Manifest{Version: export.ManifestVersion, Entries: []export.Entry{}},
		ObservedBaseSHA: testBase.BaseSHA,
	})

	if err := d.Reconcile(ctx); !errors.Is(err, ErrRecoveryRetryable) {
		t.Fatalf("Reconcile error = %v, want retryable recovery", err)
	}
	exports.lookupErr = nil
	reconstructed, err := d.loadIntent(ctx, testInvoke)
	if err != nil {
		t.Fatalf("load retryable intent: %v", err)
	}
	if reconstructed.Phase != phaseExported {
		t.Fatalf("phase = %q, want exported", reconstructed.Phase)
	}
	if err := d.Reconcile(ctx); err != nil {
		t.Fatalf("retry Reconcile: %v", err)
	}
	if _, err := d.Collect(ctx, in.InvocationID); !errors.Is(err, exec.ErrNoResult) {
		t.Fatalf("Collect after confirmed absence = %v, want ErrNoResult", err)
	}
}

// TestOperationalFinishFailurePreservesTheExport is the regression for
// deleting phaseExported replay state when fetch, import, persistence, or
// shutdown failed after the gate had already returned completed work.
func TestOperationalFinishFailurePreservesTheExport(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	operationalErr := errors.New("temporary import-base fetch failure")
	exports := newStubExports()
	d := newTestDriver(t, &stubGate{}, exports)
	d.seeder = stubSeeder{err: operationalErr}

	manifest := export.Manifest{
		Version: export.ManifestVersion,
		Entries: []export.Entry{},
	}
	body, err := manifest.Encode()
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	dir, err := os.MkdirTemp("", "freeside-handoff-"+RunIDFor(testInvoke)+"-out-")
	if err != nil {
		t.Fatalf("create released export: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.WriteFile(filepath.Join(dir, export.ManifestFilename), body, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	orphan(t, d, phaseExported, &releasedExport{
		Dir: dir, Manifest: manifest, ObservedBaseSHA: testBase.BaseSHA,
	})

	if err := d.Reconcile(ctx); !errors.Is(err, ErrRecoveryRetryable) ||
		!errors.Is(err, operationalErr) {
		t.Fatalf("Reconcile error = %v, want retryable finish failure", err)
	}
	if _, err := os.Stat(filepath.Join(dir, export.ManifestFilename)); err != nil {
		t.Fatalf("released export was not preserved: %v", err)
	}
	reconstructed, err := d.loadIntent(ctx, testInvoke)
	if err != nil {
		t.Fatalf("load retryable intent: %v", err)
	}
	if reconstructed.Phase != phaseExported || reconstructed.Result != nil {
		t.Fatalf("intent = %#v, want replayable exported phase", reconstructed)
	}
	if len(exports.outcomes) != 0 {
		t.Fatalf("operational failure recorded %d terminal outcomes", len(exports.outcomes))
	}
}

// TestManifestsArePersistedBeforeTheExportIsRemoved: the ExecutionExport
// names both manifest digests, so both byte streams must outlive the export
// directory or the audit record asserts objects nothing can fetch.
func TestManifestsArePersistedBeforeTheExportIsRemoved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	artifacts := newStubArtifacts()
	d := newTestDriver(t, &stubGate{}, newStubExports())
	d.artifacts = artifacts

	dir := t.TempDir()
	manifestBody := []byte(`{"version":"freeside.export.manifest/v1","entries":[]}`)
	evidenceBody := []byte(`{"version":"freeside.export.evidence/v1","entries":[]}`)
	if err := os.WriteFile(filepath.Join(dir, export.ManifestFilename), manifestBody, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, export.EvidenceFilename), evidenceBody, 0o600); err != nil {
		t.Fatalf("write evidence manifest: %v", err)
	}
	manifestDigest := digestOf(manifestBody)
	evidenceDigest := digestOf(evidenceBody)
	record, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: testInvoke, AdmissionID: domain.Digest("sha256:" + strings.Repeat("44", 32)),
		ObservedBaseSHA: testBase.BaseSHA, HeadSHA: strings.Repeat("c", 40),
		ManifestDigest: manifestDigest, EvidenceManifestDigest: &evidenceDigest,
		RecordedAt: fixedNow,
	})
	if err != nil {
		t.Fatalf("new execution export: %v", err)
	}
	if err := d.persistManifests(ctx, exportOutcome{dir: dir}, record); err != nil {
		t.Fatalf("persistManifests: %v", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("remove export dir: %v", err)
	}
	for name, digest := range map[string]domain.Digest{
		"repo manifest": manifestDigest, "evidence manifest": evidenceDigest,
	} {
		if _, ok := artifacts.blobs[digest]; !ok {
			t.Errorf("%s the export record names did not survive the export directory", name)
		}
	}
}

// TestManifestDigestMismatchFailsTheStage: a released manifest that does not
// hash to the digest the record names is never stored under it.
func TestManifestDigestMismatchFailsTheStage(t *testing.T) {
	t.Parallel()
	d := newTestDriver(t, &stubGate{}, newStubExports())
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, export.ManifestFilename), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	record := domain.ExecutionExport{
		InvocationID:   testInvoke,
		ManifestDigest: domain.Digest("sha256:" + strings.Repeat("ab", 32)),
	}
	if err := d.persistManifests(context.Background(), exportOutcome{dir: dir}, record); err == nil {
		t.Fatal("a manifest that does not match its recorded digest was stored")
	}
}
