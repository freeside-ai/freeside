package stage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// cordonProvider is testProvider with the cordoned launch hook.
type cordonProvider struct {
	testProvider
	mutate func(*CordonLaunch, *CordonExportRequest)
}

func (p cordonProvider) CordonLaunch(
	_ context.Context, in ProviderHandoffInput,
) (CordonLaunch, CordonExportRequest, error) {
	launch := CordonLaunch{
		InvocationID: in.InvocationID, RunID: in.RunID, Base: in.Spec.Base,
		Prompt: []byte(in.Prompt), Args: []string{"-p"},
	}
	req := CordonExportRequest{RunID: in.RunID, Transcript: export.EvidenceSource{
		Label: "agent-transcript", MediaType: "application/jsonl",
		Path: export.EvidenceWorkspaceDir + "/agent-transcript.jsonl", HeadBinding: export.EvidenceHeadIndependent,
		SensitivityClass: export.EvidenceSensitivitySensitive, ProducerInvocationID: string(in.InvocationID),
	}}
	if p.mutate != nil {
		p.mutate(&launch, &req)
	}
	return launch, req, nil
}

// fakeCordon is a scripted CordonGate that records what the driver asked.
type fakeCordon struct {
	mu        sync.Mutex
	runsRoot  string
	launchFn  func(context.Context, CordonLaunch) (CordonEnd, error)
	exportFn  func(CordonExportRequest) (CordonExport, error)
	startedFn func(string) (bool, error)
	recoverFn func(CordonExportRequest) (CordonRecovery, error)
	authFn    func(runID, dir string) error
	sendFn    func(runID string, ordinal int64, commandID, text string) error
	preJobErr error
	launches  []CordonLaunch
	cancels   []string
	releases  []string
	calls     int
}

func (c *fakeCordon) called() {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
}

func (c *fakeCordon) PreJob(context.Context, domain.InvocationID) error {
	c.called()
	return c.preJobErr
}

func (c *fakeCordon) Launch(ctx context.Context, launch CordonLaunch) (CordonEnd, error) {
	c.called()
	c.mu.Lock()
	c.launches = append(c.launches, launch)
	c.mu.Unlock()
	if c.launchFn == nil {
		return CordonEnd{EndedBy: CordonEndedByExited}, nil
	}
	return c.launchFn(ctx, launch)
}

func (c *fakeCordon) Export(_ context.Context, req CordonExportRequest) (CordonExport, error) {
	c.called()
	if c.exportFn == nil {
		return CordonExport{}, errors.New("no export scripted")
	}
	return c.exportFn(req)
}

func (c *fakeCordon) Started(_ context.Context, runID string) (bool, error) {
	c.called()
	if c.startedFn == nil {
		return true, nil
	}
	return c.startedFn(runID)
}

func (c *fakeCordon) RequestCancellation(_ context.Context, runID string) error {
	c.called()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancels = append(c.cancels, runID)
	return nil
}

func (c *fakeCordon) Recover(_ context.Context, req CordonExportRequest) (CordonRecovery, error) {
	c.called()
	if c.recoverFn == nil {
		return CordonRecovery{}, errors.New("no recovery scripted")
	}
	return c.recoverFn(req)
}

// AuthenticateReleasedExport defaults to the real backend's contract: it
// reads the export's members and its record in the run's directory, so it
// refuses once the driver removed the export or ReleaseFinished the run.
func (c *fakeCordon) AuthenticateReleasedExport(_ context.Context, runID, dir string) error {
	c.called()
	if c.authFn != nil {
		return c.authFn(runID, dir)
	}
	c.mu.Lock()
	released := slices.Contains(c.releases, runID)
	c.mu.Unlock()
	if _, err := os.Lstat(filepath.Join(dir, export.ManifestFilename)); released || err != nil {
		return fmt.Errorf("%w: no export record or manifest for %s", ErrUnsupportedStart, runID)
	}
	return nil
}

func (c *fakeCordon) SendMessage(_ context.Context, runID string, ordinal int64, commandID, text string) error {
	c.called()
	if c.sendFn == nil {
		return errors.New("no send scripted")
	}
	return c.sendFn(runID, ordinal, commandID, text)
}

// ReleaseFinished removes each run's directory, as the real backend does.
func (c *fakeCordon) ReleaseFinished(_ context.Context, runIDs []string) {
	c.called()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.releases = append(c.releases, runIDs...)
	for _, runID := range runIDs {
		_ = os.RemoveAll(filepath.Join(c.runsRoot, runID))
	}
}

func (c *fakeCordon) snapshot() (launches []CordonLaunch, cancels, releases []string, calls int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.launches), slices.Clone(c.cancels), slices.Clone(c.releases), c.calls
}

// newCordonDriver is newTestDriver with a cordon composed, its runs root a
// fresh directory, and a seeder over a real base for the import lane.
func newCordonDriver(t *testing.T, cordon *fakeCordon, gate *stubGate, exports *stubExports) (*Driver, string) {
	t.Helper()
	root := t.TempDir()
	cordon.runsRoot = filepath.Join(root, "runs")
	d, err := New(Config{
		ErrorPrefix: "test driver", DisplayName: "Test",
		Provider:        cordonProvider{testProvider: testProvider{volumes: stubVolumes{volume: testAuthVol}}},
		CredentialMount: testCredentialMountPolicy,
		Lifetime:        context.Background(),
		Dir:             filepath.Join(root, "driver"), SeedRoot: filepath.Join(root, "seeds"),
		ExportRoot: filepath.Clean(os.TempDir()),
		Gate:       gate, Seeder: stubSeeder{}, Exports: exports,
		Cordon: cordon, CordonRunsRoot: cordon.runsRoot,
		ImportStarts: exports, Outcomes: exports,
		Authority: stubAuthority{},
		Artifacts: newStubArtifacts(),
		Now:       func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	repo, base := emptyBaseRepo(t)
	d.seeder = recoveryGitSeeder{repo: repo}
	return d, base
}

func cordonSpec(base string) exec.StartSpec {
	spec := testStartSpec()
	spec.RunnerClass = domain.BackendCordonedHostProcess
	spec.EgressProfile = domain.EgressHostOpen
	spec.Base.BaseSHA = base
	return spec
}

// cordonExport writes a real export of one candidate file at the run's
// export directory, as the cordon would.
func cordonExport(t *testing.T, runsRoot, runID, base string) CordonExport {
	t.Helper()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "candidate.txt"), []byte("candidate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(runsRoot, runID, "export")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	manifest, err := export.Export(os.DirFS(workspace), dir, export.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return CordonExport{Dir: dir, Manifest: manifest, BaseSHA: base}
}

func startCordoned(t *testing.T, d *Driver, spec exec.StartSpec) error {
	t.Helper()
	inputs := stageInputs(t, &spec)
	return d.StartWithInputs(context.Background(), testInvoke, spec,
		func(context.Context) (exec.StageInputs, error) { return inputs, nil })
}

// awaitPipeline waits for the invocation's live pipeline to end.
func awaitPipeline(t *testing.T, d *Driver) {
	t.Helper()
	d.mu.Lock()
	sess := d.running[testInvoke]
	d.mu.Unlock()
	if sess != nil {
		<-sess.done
	}
}

func TestCordonedStartLaunchesExportsAndImports(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cordon := &fakeCordon{}
	gate := &stubGate{}
	exports := newStubExports()
	d, base := newCordonDriver(t, cordon, gate, exports)
	runID := testRunIDFor(testInvoke)
	cordon.exportFn = func(req CordonExportRequest) (CordonExport, error) {
		if req.RunID != runID {
			t.Errorf("export run = %q, want %q", req.RunID, runID)
		}
		return cordonExport(t, cordon.runsRoot, runID, base), nil
	}
	spec := cordonSpec(base)
	if err := startCordoned(t, d, spec); err != nil {
		t.Fatal(err)
	}
	awaitPipeline(t, d)
	result, err := d.Collect(ctx, testInvoke)
	if err != nil || result.Status != exec.StatusCompleted {
		t.Fatalf("Collect = %+v, %v; want completed", result, err)
	}
	if _, ok := exports.records[testInvoke]; !ok {
		t.Fatal("no execution export was recorded")
	}
	launches, _, releases, _ := cordon.snapshot()
	in, err := d.loadIntent(ctx, testInvoke)
	if err != nil {
		t.Fatal(err)
	}
	if len(launches) != 1 || string(launches[0].Prompt) != in.Prompt || launches[0].Base != spec.Base {
		t.Fatalf("launches = %+v, want one with the intent's prompt and base", launches)
	}
	if !slices.Contains(releases, runID) {
		t.Fatalf("cordon releases = %v, want %s once terminal", releases, runID)
	}
	// The committed intent still reads once its export and run are gone,
	// and after the cordon is unconfigured: its durable export is its
	// authority.
	if result, err := d.Collect(ctx, testInvoke); err != nil || result.Status != exec.StatusCompleted {
		t.Fatalf("Collect after release = %+v, %v; want completed", result, err)
	}
	d.cordon = nil
	if result, err := d.Collect(ctx, testInvoke); err != nil || result.Status != exec.StatusCompleted {
		t.Fatalf("Collect with no cordon = %+v, %v; want completed", result, err)
	}
	if len(gate.specs) != 0 || slices.Contains(gate.released(), runID) {
		t.Fatalf("a cordoned run reached the ward: specs %d, releases %v", len(gate.specs), gate.released())
	}
	if _, found, err := d.LiveTranscript(ctx, testInvoke, 0); err != nil || found {
		t.Fatalf("LiveTranscript = found %v, %v; a cordoned run has no live view", found, err)
	}
}

func TestCordonedWriterEndsThatDontExport(t *testing.T) {
	t.Parallel()
	body := []byte("the agent's last words\n")
	evidence := &ward.FailureEvidence{Body: body, Digest: contentaddr.Sum(body)}
	for _, tc := range []struct {
		name      string
		end       CordonEnd
		exportErr error
		status    exec.Status
		summary   string
	}{
		{
			"nonzero",
			CordonEnd{EndedBy: CordonEndedByExited, ExitCode: 3, Failure: evidence},
			nil,
			exec.StatusFailed, "exited with status 3",
		},
		{
			"stalled",
			CordonEnd{EndedBy: CordonEndedByStalled, ExitCode: -1, Failure: evidence},
			nil,
			exec.StatusFailed, "stalled",
		},
		{"canceled", CordonEnd{EndedBy: CordonEndedByCanceled, ExitCode: -1}, nil, exec.StatusCanceled, "canceled"},
		// A stop that raced the writer's clean exit: Launch saw exited 0, and
		// the cancellation landed before Export.
		{
			"canceled after a clean exit",
			CordonEnd{EndedBy: CordonEndedByExited},
			fmt.Errorf("%w: run", ErrCordonCanceled), exec.StatusCanceled, "canceled",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			cordon := &fakeCordon{
				launchFn: func(context.Context, CordonLaunch) (CordonEnd, error) {
					return tc.end, nil
				},
				exportFn: func(CordonExportRequest) (CordonExport, error) {
					return CordonExport{}, tc.exportErr
				},
			}
			d, base := newCordonDriver(t, cordon, &stubGate{}, newStubExports())
			if err := startCordoned(t, d, cordonSpec(base)); err != nil {
				t.Fatal(err)
			}
			awaitPipeline(t, d)
			result, err := d.Collect(ctx, testInvoke)
			if err != nil || result.Status != tc.status || !strings.Contains(result.Summary, tc.summary) {
				t.Fatalf("Collect = %+v, %v; want %s naming %q", result, err, tc.status, tc.summary)
			}
			if tc.end.Failure == nil {
				return
			}
			if !slices.Equal(result.Artifacts, []domain.Digest{domain.Digest(evidence.Digest)}) {
				t.Fatalf("artifacts = %v, want the failure transcript", result.Artifacts)
			}
			claims, _, err := d.artifacts.LookupClaims(ctx, testInvoke)
			if err != nil || len(claims) != 1 || claims[0].Provenance.SensitivityClass != domain.SensitivitySensitive {
				t.Fatalf("claims = %+v, %v; want one sensitive transcript claim", claims, err)
			}
		})
	}
}

// TestCordonedDetachLeavesTheIntentRunning shows a shutdown that detached
// the cordon mid-launch records nothing, and that Close asks the cordon to
// cancel nothing: the run is its supervisor's until the next Recover.
func TestCordonedDetachLeavesTheIntentRunning(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	launched := make(chan struct{})
	cordon := &fakeCordon{launchFn: func(ctx context.Context, _ CordonLaunch) (CordonEnd, error) {
		close(launched)
		<-ctx.Done()
		return CordonEnd{}, ErrCordonDetached
	}}
	exports := newStubExports()
	d, base := newCordonDriver(t, cordon, &stubGate{}, exports)
	if err := startCordoned(t, d, cordonSpec(base)); err != nil {
		t.Fatal(err)
	}
	<-launched
	if err := d.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	in, err := d.loadIntent(ctx, testInvoke)
	if err != nil || in.Phase != phaseRunning {
		t.Fatalf("intent = %q, %v; want running", in.Phase, err)
	}
	if len(exports.outcomes) != 0 {
		t.Fatalf("outcomes = %v, want none", exports.outcomes)
	}
	if _, cancels, _, _ := cordon.snapshot(); len(cancels) != 0 {
		t.Fatalf("Close requested cordon cancellation of %v", cancels)
	}
}

func TestCordonedLaunchErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		started bool
		phase   phase
	}{
		// Refused before the run's directory: nothing ran, so it fails.
		{"unstarted", false, phaseCommitted},
		// A directory exists, so only recovery may end the run.
		{"started", true, phaseRunning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			cordon := &fakeCordon{
				launchFn: func(context.Context, CordonLaunch) (CordonEnd, error) {
					return CordonEnd{}, errors.New("seed /agent/chosen/path failed")
				},
				startedFn: func(string) (bool, error) { return tc.started, nil },
			}
			exports := newStubExports()
			d, base := newCordonDriver(t, cordon, &stubGate{}, exports)
			if err := startCordoned(t, d, cordonSpec(base)); err != nil {
				t.Fatal(err)
			}
			awaitPipeline(t, d)
			in, err := d.loadIntent(ctx, testInvoke)
			if err != nil || in.Phase != tc.phase {
				t.Fatalf("intent = %q, %v; want %q", in.Phase, err, tc.phase)
			}
			if tc.phase != phaseCommitted {
				return
			}
			if in.Result.Status != exec.StatusFailed || strings.Contains(in.Result.Summary, "/agent/chosen/path") {
				t.Fatalf("result = %+v; want failed with a screened summary", in.Result)
			}
		})
	}
}

func TestCordonedRecovery(t *testing.T) {
	t.Parallel()
	body := []byte("diagnostics\n")
	for _, tc := range []struct {
		name    string
		outcome CordonRecoveryOutcome
		end     CordonEnd
		status  domain.ExecutionOutcomeStatus
		// err is Recover's error in place of an outcome.
		err error
	}{
		// An exported run records its export, not an outcome.
		{"exported", CordonRecoveryExported, CordonEnd{EndedBy: CordonEndedByExited}, "", nil},
		{"failed", CordonRecoveryFailed, CordonEnd{
			EndedBy: CordonEndedByExited, ExitCode: 2,
			Failure: &ward.FailureEvidence{Body: body, Digest: contentaddr.Sum(body)},
		}, domain.ExecutionOutcomeFailed, nil},
		{
			"canceled", CordonRecoveryCanceled,
			CordonEnd{EndedBy: CordonEndedByCanceled, ExitCode: -1},
			domain.ExecutionOutcomeCanceled, nil,
		},
		{"lost", CordonRecoveryLost, CordonEnd{EndedBy: CordonEndedByLost, ExitCode: -1}, domain.ExecutionOutcomeLost, nil},
		// The cancellation landed between Recover's read of a clean end and
		// its export.
		{
			"canceled before the export", "",
			CordonEnd{},
			domain.ExecutionOutcomeCanceled,
			fmt.Errorf("%w: run", ErrCordonCanceled),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			cordon := &fakeCordon{}
			exports := newStubExports()
			d, base := newCordonDriver(t, cordon, &stubGate{}, exports)
			runID := testRunIDFor(testInvoke)
			var recovered atomic.Bool
			cordon.recoverFn = func(req CordonExportRequest) (CordonRecovery, error) {
				recovered.Store(true)
				if tc.err != nil {
					return CordonRecovery{}, tc.err
				}
				rec := CordonRecovery{Outcome: tc.outcome, End: tc.end, Reason: "no supervisor recorded its end"}
				if tc.outcome == CordonRecoveryExported {
					rec.Export = cordonExport(t, cordon.runsRoot, req.RunID, base)
				}
				return rec, nil
			}
			orphanWithSpec(t, d, phaseRunning, nil, cordonSpec(base))
			// Startup's pass never waits on a run that may still be alive; the
			// attempt's Inspect recovers it.
			if err := reconcileHeld(t, ctx, d); !errors.Is(err, ErrRecoveryRetryable) || recovered.Load() {
				t.Fatalf("Reconcile held %v, recovered %v; want a retryable hold and no Recover", err, recovered.Load())
			}
			if _, err := inspectRecovered(t, ctx, d, testInvoke); err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			if tc.status == "" {
				if _, ok := exports.records[testInvoke]; !ok {
					t.Fatal("the recovered export was not recorded")
				}
			} else if got, ok := exports.outcomes[testInvoke]; !ok || got.Status != tc.status {
				t.Fatalf("outcome = %+v (recorded %v), want %s", got, ok, tc.status)
			}
			// As for a warded run, the pass after a terminal commit releases it.
			reconcileClean(t, ctx, d)
			if _, _, releases, _ := cordon.snapshot(); !slices.Contains(releases, runID) {
				t.Fatalf("cordon releases = %v, want %s", releases, runID)
			}
			// The released intent still loads.
			reconcileClean(t, ctx, d)
		})
	}
}

func TestCordonedUnstartedRunReturnsToSeeding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cordon := &fakeCordon{
		startedFn: func(string) (bool, error) { return false, nil },
		launchFn: func(context.Context, CordonLaunch) (CordonEnd, error) {
			return CordonEnd{EndedBy: CordonEndedByCanceled, ExitCode: -1}, nil
		},
	}
	d, base := newCordonDriver(t, cordon, &stubGate{}, newStubExports())
	orphanWithSpec(t, d, phaseRunning, nil, cordonSpec(base))
	if err := reconcileHeld(t, ctx, d); !errors.Is(err, ErrRecoveryRetryable) {
		t.Fatalf("Reconcile held %v, want a retryable hold", err)
	}
	if _, err := inspectRecovered(t, ctx, d, testInvoke); err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	awaitPipeline(t, d)
	if launches, _, _, _ := cordon.snapshot(); len(launches) != 1 {
		t.Fatalf("launches = %d, want the resumed launch", len(launches))
	}
}

func TestCordonedCancelGoesThroughThePort(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	launched := make(chan struct{})
	cordon := &fakeCordon{}
	cordon.launchFn = func(ctx context.Context, _ CordonLaunch) (CordonEnd, error) {
		close(launched)
		<-ctx.Done()
		return CordonEnd{EndedBy: CordonEndedByCanceled, ExitCode: -1}, nil
	}
	gate := &stubGate{cancelFn: func(string) error {
		t.Error("a cordoned cancellation reached the ward")
		return nil
	}}
	d, base := newCordonDriver(t, cordon, gate, newStubExports())
	if err := startCordoned(t, d, cordonSpec(base)); err != nil {
		t.Fatal(err)
	}
	<-launched
	if err := d.CancelAndConfirm(ctx, testInvoke); err != nil {
		t.Fatalf("CancelAndConfirm: %v", err)
	}
	if _, cancels, _, _ := cordon.snapshot(); !slices.Equal(cancels, []string{testRunIDFor(testInvoke)}) {
		t.Fatalf("cordon cancellations = %v", cancels)
	}
	if result, err := d.Collect(ctx, testInvoke); err != nil || result.Status != exec.StatusCanceled {
		t.Fatalf("Collect = %+v, %v; want canceled", result, err)
	}
}

// TestCordonedSendMessageGoesThroughThePort shows a message reaches the
// port under the intent's run, and that a writer yet to start refuses as
// not started however the driver learns it.
func TestCordonedSendMessageGoesThroughThePort(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		phase   phase
		portErr error
		want    error
		reached bool
	}{
		{"running", phaseRunning, nil, nil, true},
		{"refused by the run", phaseRunning, ErrCordonInputClosed, ErrCordonInputClosed, true},
		{"seeding", phaseSeeding, nil, ErrCordonNotStarted, false},
		{"running before its launch claims a directory", phaseRunning, ErrCordonNoRun, ErrCordonNotStarted, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var sent []string
			cordon := &fakeCordon{sendFn: func(runID string, ordinal int64, commandID, text string) error {
				sent = append(sent, fmt.Sprintf("%s %d %s %s", runID, ordinal, commandID, text))
				return tc.portErr
			}}
			d, base := newCordonDriver(t, cordon, &stubGate{}, newStubExports())
			orphanWithSpec(t, d, tc.phase, nil, cordonSpec(base))
			err := d.SendMessage(context.Background(), testInvoke, 2, "cmd-2", "keep going")
			if !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
				t.Fatalf("SendMessage = %v, want %v", err, tc.want)
			}
			want := []string{testRunIDFor(testInvoke) + " 2 cmd-2 keep going"}
			if !tc.reached {
				want = nil
			}
			if !slices.Equal(sent, want) {
				t.Fatalf("the port got %q, want %q", sent, want)
			}
		})
	}
}

func TestCordonedReleasedExportPath(t *testing.T) {
	t.Parallel()
	runs := t.TempDir()
	in := intent{RunID: testRunIDFor(testInvoke), Spec: cordonSpec("")}
	roots := releasedExportRoots{ward: filepath.Clean(os.TempDir()), cordonRuns: runs}
	valid := filepath.Join(runs, in.RunID, "export")
	if err := validateReleasedExportPath(roots, in, valid); err != nil {
		t.Fatalf("the run's own export: %v", err)
	}
	for _, dir := range []string{
		filepath.Join(roots.ward, "freeside-handoff-"+in.RunID+"-out-x"),
		filepath.Join(runs, "another-run", "export"),
		filepath.Join(runs, in.RunID, "export", "nested"),
		filepath.Join(runs, in.RunID, "run", "worktree"),
	} {
		if err := validateReleasedExportPath(roots, in, dir); !errors.Is(err, ErrUnsupportedStart) {
			t.Errorf("accepted %q: %v", dir, err)
		}
	}
}

// TestCordonedExportAuthenticationAfterRemoval covers a crash partway
// through the driver's removal of a released export: the partial directory
// stays refused until a durable export record shows the import finished,
// and a directory that is gone has nothing left to read.
func TestCordonedExportAuthenticationAfterRemoval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cordon := &fakeCordon{}
	exports := newStubExports()
	d, base := newCordonDriver(t, cordon, &stubGate{}, exports)
	exp := cordonExport(t, cordon.runsRoot, testRunIDFor(testInvoke), base)
	in := orphanWithSpec(t, d, phaseImportPending, &releasedExport{
		Dir: exp.Dir, Manifest: exp.Manifest, ObservedBaseSHA: base,
	}, cordonSpec(base))
	if err := os.Remove(filepath.Join(exp.Dir, export.ManifestFilename)); err != nil {
		t.Fatal(err)
	}
	if err := d.authenticateReleasedExport(ctx, in, exp.Dir); !errors.Is(err, ErrUnsupportedStart) {
		t.Fatalf("partial export with no record = %v, want refused", err)
	}
	exports.records[testInvoke] = domain.ExecutionExport{}
	if err := d.authenticateReleasedExport(ctx, in, exp.Dir); err != nil {
		t.Fatalf("partial export with a durable record = %v, want accepted", err)
	}
	delete(exports.records, testInvoke)
	if err := os.RemoveAll(exp.Dir); err != nil {
		t.Fatal(err)
	}
	if err := d.authenticateReleasedExport(ctx, in, exp.Dir); err != nil {
		t.Fatalf("removed export = %v, want accepted", err)
	}
}

// TestCordonedUnauthenticatedExportIsRefusedForGood shows an export the
// cordon won't authenticate is never imported, and the next reconcile
// quarantines its intent instead of holding it for a retry.
func TestCordonedUnauthenticatedExportIsRefusedForGood(t *testing.T) {
	t.Parallel()
	cordon := &fakeCordon{}
	exports := newStubExports()
	d, base := newCordonDriver(t, cordon, &stubGate{}, exports)
	runID := testRunIDFor(testInvoke)
	cordon.exportFn = func(CordonExportRequest) (CordonExport, error) {
		return cordonExport(t, cordon.runsRoot, runID, base), nil
	}
	cordon.authFn = func(string, string) error {
		return errors.Join(ErrUnsupportedStart, errors.New("the export does not match its record"))
	}
	if err := startCordoned(t, d, cordonSpec(base)); err != nil {
		t.Fatal(err)
	}
	awaitPipeline(t, d)
	if _, ok := exports.records[testInvoke]; ok {
		t.Fatal("an unauthenticated export was recorded")
	}
	report, err := d.Reconcile(context.Background(), nil)
	if err != nil || len(report.Held) != 0 || len(report.Quarantined) != 1 ||
		report.Quarantined[0].Cause != QuarantineUnauthenticated {
		t.Fatalf("Reconcile = %+v, %v; want the intent quarantined as unauthenticated", report, err)
	}
}

func TestCordonedStartRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		cordon bool
		mutate func(*exec.StartSpec)
	}{
		{"unknown class", true, func(s *exec.StartSpec) {
			s.RunnerClass = domain.BackendFreshVMReadOnlyVolumeHandoff
		}},
		{"no cordon composed", false, func(*exec.StartSpec) {}},
		{"cordoned under a ward profile", true, func(s *exec.StartSpec) {
			s.EgressProfile = domain.EgressProviderOnly
		}},
		{"warded under the host profile", true, func(s *exec.StartSpec) {
			s.RunnerClass = ""
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cordon := &fakeCordon{}
			d, base := newCordonDriver(t, cordon, &stubGate{}, newStubExports())
			if !tc.cordon {
				d.cordon = nil
			}
			spec := cordonSpec(base)
			tc.mutate(&spec)
			if err := startCordoned(t, d, spec); !errors.Is(err, ErrUnsupportedStart) {
				t.Fatalf("start = %v, want an unsupported start", err)
			}
			if _, err := d.loadIntent(context.Background(), testInvoke); !errors.Is(err, exec.ErrUnknownInvocation) {
				t.Fatalf("intent after refusal: %v, want none", err)
			}
			if launches, _, _, _ := cordon.snapshot(); len(launches) != 0 {
				t.Fatalf("a refused start launched %d times", len(launches))
			}
		})
	}
}

// The cordon has no input volume, so a launch that would drop the stage's
// attachments is refused; start's dry run builds the same launch.
func TestCordonedLaunchRefusesAttachments(t *testing.T) {
	t.Parallel()
	d, base := newCordonDriver(t, &fakeCordon{}, &stubGate{}, newStubExports())
	spec := cordonSpec(base)
	spec.StageInputs = &domain.StageInputSnapshot{Attachments: testAttachments()}
	in := intent{RunID: testRunIDFor(testInvoke), Spec: spec}
	if _, _, err := d.cordonLaunch(context.Background(), in); !errors.Is(err, ErrUnsupportedStart) {
		t.Fatalf("cordonLaunch = %v, want an unsupported start", err)
	}
}

func TestCordonedPreJobRefusalHolds(t *testing.T) {
	t.Parallel()
	cordon := &fakeCordon{preJobErr: errors.New("probe: the class is not declared")}
	d, base := newCordonDriver(t, cordon, &stubGate{}, newStubExports())
	if err := startCordoned(t, d, cordonSpec(base)); !errors.Is(err, exec.ErrPreJobRefused) {
		t.Fatalf("start = %v, want a pre-job refusal", err)
	}
}

// TestWardedStartNeverTouchesTheCordon runs a warded start on a driver
// that composes a cordon.
func TestWardedStartNeverTouchesTheCordon(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cordon := &fakeCordon{}
	repo, base := emptyBaseRepo(t)
	released := timestampTestExport(t, base)
	gate := &stubGate{handoffFn: func(ward.HandoffSpec) (*ward.HandoffResult, error) {
		return &ward.HandoffResult{
			ExportDir: released.Dir, Manifest: released.Manifest,
			Workspace: ward.WorkspaceObservation{ObservedBaseSHA: base},
		}, nil
	}}
	d, _ := newCordonDriver(t, cordon, gate, newStubExports())
	d.seeder = recoveryGitSeeder{repo: repo}
	spec := testStartSpec()
	spec.Base.BaseSHA = base
	if err := startCordoned(t, d, spec); err != nil {
		t.Fatal(err)
	}
	awaitPipeline(t, d)
	if result, err := d.Collect(ctx, testInvoke); err != nil || result.Status != exec.StatusCompleted {
		t.Fatalf("Collect = %+v, %v; want completed", result, err)
	}
	if _, _, _, calls := cordon.snapshot(); calls != 0 {
		t.Fatalf("a warded start made %d cordon calls", calls)
	}
}
