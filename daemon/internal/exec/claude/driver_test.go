package claude

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

var (
	fixedNow    = time.Date(2026, 7, 28, 18, 0, 0, 0, time.UTC)
	testImage   = domain.ImageRef("127.0.0.1:5014/freeside-agent-claude@sha256:" + strings.Repeat("ab", 32))
	testBase    = domain.BaseRevision{Repo: "freeside-ai/candidate", RepositoryID: 42, BaseRef: "refs/heads/main", BaseSHA: strings.Repeat("a", 40)}
	testInvoke  = domain.InvocationID("inv-implement-run-1")
	testAuthID  = domain.AuthIdentityID("auth-claude-owner")
	testAuthVol = "claude-owner-credentials"
)

// stubGate records the specs it is handed and returns scripted outcomes.
type stubGate struct {
	mu             sync.Mutex
	specs          []ward.HandoffSpec
	handoffFn      func(ward.HandoffSpec) (*ward.HandoffResult, error)
	handoffCtxFn   func(context.Context, ward.HandoffSpec) (*ward.HandoffResult, error)
	handoffStarted func(string) (bool, error)
	cancelFn       func(string) error
	recoverFn      func(string, ward.HandoffSpec) (*ward.RecoveryResult, error)
	authenticateFn func(string, string) error
}

func (g *stubGate) Handoff(ctx context.Context, hs ward.HandoffSpec) (*ward.HandoffResult, error) {
	g.mu.Lock()
	g.specs = append(g.specs, hs)
	g.mu.Unlock()
	if g.handoffCtxFn != nil {
		return g.handoffCtxFn(ctx, hs)
	}
	if g.handoffFn == nil {
		return nil, errors.New("no handoff scripted")
	}
	return g.handoffFn(hs)
}

func (g *stubGate) HandoffStarted(_ context.Context, runID string) (bool, error) {
	if g.handoffStarted == nil {
		return true, nil
	}
	return g.handoffStarted(runID)
}

func (g *stubGate) RequestCancellation(_ context.Context, runID string) error {
	if g.cancelFn == nil {
		return nil
	}
	return g.cancelFn(runID)
}

func (g *stubGate) Recover(_ context.Context, runID string, hs ward.HandoffSpec) (*ward.RecoveryResult, error) {
	if g.recoverFn == nil {
		return nil, errors.New("no recovery scripted")
	}
	return g.recoverFn(runID, hs)
}

func (g *stubGate) AuthenticateReleasedExport(
	_ context.Context, runID, exportDir string,
) error {
	if g.authenticateFn == nil {
		return nil
	}
	return g.authenticateFn(runID, exportDir)
}

func (g *stubGate) lastSpec(t *testing.T) ward.HandoffSpec {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.specs) == 0 {
		t.Fatal("gate received no handoff spec")
	}
	return g.specs[len(g.specs)-1]
}

type stubSeeder struct{ err error }

func (s stubSeeder) FetchBase(_ context.Context, _, _, _, _ string) error { return s.err }

func (s stubSeeder) FetchBaseWorktree(_ context.Context, _, _, _, _ string) error { return s.err }

// recordingSeeder separates the two Seeder methods so a test can prove which
// one a lane used. The distinction is load-bearing: only the worktree variant
// materializes files, and ward refuses a workspace seeded without them.
type recordingSeeder struct {
	mu       sync.Mutex
	plain    int
	worktree int
}

func (s *recordingSeeder) FetchBase(_ context.Context, _, _, _, dir string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.plain++
	return os.MkdirAll(dir, 0o700)
}

func (s *recordingSeeder) FetchBaseWorktree(_ context.Context, _, _, _, dir string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.worktree++
	return os.MkdirAll(dir, 0o700)
}

type cancelSeeder struct{ entered chan struct{} }

func (s cancelSeeder) FetchBase(ctx context.Context, _, _, _, _ string) error {
	close(s.entered)
	<-ctx.Done()
	return ctx.Err()
}

func (s cancelSeeder) FetchBaseWorktree(ctx context.Context, _, _, _, _ string) error {
	close(s.entered)
	<-ctx.Done()
	return ctx.Err()
}

type refuseOnCancelSeeder struct{ entered chan struct{} }

func (s refuseOnCancelSeeder) FetchBase(ctx context.Context, _, _, _, _ string) error {
	close(s.entered)
	<-ctx.Done()
	return errors.Join(ErrSeedRefused, ctx.Err(), errors.New("base absent"))
}

func (s refuseOnCancelSeeder) FetchBaseWorktree(
	ctx context.Context, _, _, _, _ string,
) error {
	close(s.entered)
	<-ctx.Done()
	return errors.Join(ErrSeedRefused, ctx.Err(), errors.New("base absent"))
}

type stubAuthority struct {
	err      error
	startErr error
}

func (a stubAuthority) AuthenticateAdmission(
	context.Context, domain.InvocationID, exec.StartSpec,
) error {
	return a.err
}

func (a stubAuthority) AuthenticateStart(
	context.Context, domain.InvocationID, exec.StartSpec,
) error {
	if a.startErr != nil {
		return a.startErr
	}
	return a.err
}

func (a stubAuthority) ImportOptions(
	_ context.Context, _ domain.InvocationID, _ exec.StartSpec, opts importer.Options,
) (importer.Options, error) {
	if a.startErr != nil {
		return opts, a.startErr
	}
	return opts, a.err
}

func (a stubAuthority) ImportOptionsRecord(
	_ context.Context, _ domain.InvocationID, _ exec.StartSpec, opts importer.Options,
) (importer.Options, error) {
	return opts, a.err
}

type stubVolumes struct {
	volume string
	err    error
}

func (v stubVolumes) AuthStoreVolume(context.Context, domain.AuthIdentityID) (string, error) {
	return v.volume, v.err
}

type secondLookupRefusingVolumes struct {
	mu      sync.Mutex
	calls   int
	err     error
	entered chan struct{}
	release chan struct{}
}

func (v *secondLookupRefusingVolumes) AuthStoreVolume(
	context.Context, domain.AuthIdentityID,
) (string, error) {
	v.mu.Lock()
	v.calls++
	call := v.calls
	v.mu.Unlock()
	if call == 2 {
		close(v.entered)
		<-v.release
		return "", v.err
	}
	return testAuthVol, nil
}

type stubExports struct {
	mu        sync.Mutex
	records   map[domain.InvocationID]domain.ExecutionExport
	replays   map[domain.InvocationID]ExecutionReplay
	outcomes  map[domain.InvocationID]domain.ExecutionOutcome
	lookupErr error
}

func newStubExports() *stubExports {
	return &stubExports{
		records:  map[domain.InvocationID]domain.ExecutionExport{},
		replays:  map[domain.InvocationID]ExecutionReplay{},
		outcomes: map[domain.InvocationID]domain.ExecutionOutcome{},
	}
}

func (e *stubExports) RecordExecutionExport(
	_ context.Context,
	record domain.ExecutionExport,
	replay ExecutionReplay,
) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.outcomes[record.InvocationID]; ok {
		return domain.ErrImmutableTransition
	}
	if stored, ok := e.records[record.InvocationID]; ok {
		if reflect.DeepEqual(stored, record) && reflect.DeepEqual(e.replays[record.InvocationID], replay) {
			return nil
		}
		return domain.ErrImmutableTransition
	}
	e.records[record.InvocationID] = record
	e.replays[record.InvocationID] = replay
	return nil
}

func (e *stubExports) LookupExecutionExport(
	_ context.Context, id domain.InvocationID,
) (domain.ExecutionExport, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lookupErr != nil {
		return domain.ExecutionExport{}, false, e.lookupErr
	}
	record, ok := e.records[id]
	if !ok {
		return domain.ExecutionExport{}, false, nil
	}
	return record, true, nil
}

func (e *stubExports) LookupExecutionExportRecord(
	ctx context.Context, id domain.InvocationID,
) (domain.ExecutionExport, bool, error) {
	return e.LookupExecutionExport(ctx, id)
}

func (e *stubExports) RecordExecutionOutcome(
	_ context.Context, record domain.ExecutionOutcome,
) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.records[record.InvocationID]; ok {
		return domain.ErrImmutableTransition
	}
	if _, ok := e.outcomes[record.InvocationID]; ok {
		return domain.ErrImmutableTransition
	}
	e.outcomes[record.InvocationID] = record
	return nil
}

func (e *stubExports) LookupExecutionOutcome(
	_ context.Context, id domain.InvocationID,
) (domain.ExecutionOutcome, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lookupErr != nil {
		return domain.ExecutionOutcome{}, false, e.lookupErr
	}
	record, ok := e.outcomes[id]
	return record, ok, nil
}

func (e *stubExports) LookupExecutionOutcomeRecord(
	ctx context.Context, id domain.InvocationID,
) (domain.ExecutionOutcome, bool, error) {
	return e.LookupExecutionOutcome(ctx, id)
}

func testStartSpec() exec.StartSpec {
	return exec.StartSpec{
		RunID: "run-1", StageID: "implement-run-1", AttemptID: "attempt-" + domain.AttemptID(testInvoke),
		InputDigest:  domain.Digest("sha256:" + strings.Repeat("11", 32)),
		SpecDigest:   domain.Digest("sha256:" + strings.Repeat("22", 32)),
		PolicyDigest: domain.Digest("sha256:" + strings.Repeat("33", 32)),
		Base:         testBase, Workspace: WorkspaceFor(testInvoke), ImageRef: testImage,
		CredentialMode: domain.CredentialSubscriptionContained,
		EgressProfile:  domain.EgressProviderOnly,
		AuthIdentityID: testAuthID,
		AdmissionID:    domain.Digest("sha256:" + strings.Repeat("44", 32)),
	}
}

// blobSource serves materialization by digest, standing in for the daemon's
// content-addressed artifact store.
type blobSource map[domain.Digest][]byte

func (b blobSource) OpenContext(_ context.Context, digest domain.Digest) (io.ReadCloser, error) {
	body, ok := b[digest]
	if !ok {
		return nil, errors.New("no blob for " + string(digest))
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

func digestOf(body []byte) domain.Digest {
	sum := sha256.Sum256(body)
	return domain.Digest("sha256:" + hex.EncodeToString(sum[:]))
}

// stageInputs builds the admitted snapshot and materializes it through the
// production materializer, so the driver's rendering path runs against real
// verified inputs rather than a hand-built bundle.
func stageInputs(t *testing.T, spec *exec.StartSpec) exec.StageInputs {
	t.Helper()
	return stageInputsWithBodies(
		t, spec,
		[]byte("# Work item\nDo the thing.\n"),
		[]byte("You are the Phase 1A implementer.\n"),
		[]byte(`[{"key":"paths","value":"daemon/**"}]`),
	)
}

func stageInputsWithBodies(
	t *testing.T, spec *exec.StartSpec, specBody, promptBody, policyBody []byte,
) exec.StageInputs {
	t.Helper()
	vendorBody := []byte("# Host instructions\n")
	vendorDigest := digestOf(vendorBody)
	snapshot, err := domain.NewStageInputSnapshot(domain.StageInputSnapshotInput{
		InputDigest:         spec.InputDigest,
		SpecificationDigest: digestOf(specBody),
		PromptPackageDigest: digestOf(promptBody),
		PolicyDigest:        digestOf(policyBody),
		VendorInstructions: &domain.VendorInstructionSnapshot{
			Vendor: domain.AgentVendorClaude, Digest: &vendorDigest,
		},
	})
	if err != nil {
		t.Fatalf("new stage input snapshot: %v", err)
	}
	spec.SpecDigest = snapshot.SpecificationDigest
	spec.PolicyDigest = snapshot.PolicyDigest
	spec.StageInputs = &snapshot

	materializer, err := exec.NewMaterializer(blobSource{
		snapshot.SpecificationDigest: specBody,
		snapshot.PromptPackageDigest: promptBody,
		snapshot.PolicyDigest:        policyBody,
		vendorDigest:                 vendorBody,
	}, exec.MaterializerOptions{MaxInputBytes: 1 << 20, MaxTotalBytes: 4 << 20})
	if err != nil {
		t.Fatalf("new materializer: %v", err)
	}
	inputs, err := materializer.Materialize(context.Background(), *spec)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	return inputs
}

func TestOversizedRenderedPromptCommitsFailureWithoutWedgingDispatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	gate := &stubGate{}
	records := newStubExports()
	d := newTestDriver(t, gate, records)
	spec := testStartSpec()
	inputs := stageInputsWithBodies(
		t, &spec,
		bytes.Repeat([]byte("s"), maxPromptBytes),
		[]byte("prompt"),
		[]byte(`[{"key":"paths","value":"daemon/**"}]`),
	)
	load := func(context.Context) (exec.StageInputs, error) { return inputs, nil }

	if err := d.StartWithInputs(ctx, testInvoke, spec, load); err != nil {
		t.Fatalf("oversized start must commit a terminal failure, got %v", err)
	}
	result, err := d.Collect(ctx, testInvoke)
	if err != nil {
		t.Fatalf("collect terminal refusal: %v", err)
	}
	if result.Status != exec.StatusFailed || !strings.Contains(result.Summary, "rendered prompt") {
		t.Fatalf("result = %#v, want an actionable failed result", result)
	}
	if len(gate.specs) != 0 {
		t.Fatal("oversized prompt reached the containment gate")
	}
	records.mu.Lock()
	delete(records.outcomes, testInvoke)
	records.mu.Unlock()
	if _, err := d.loadIntent(ctx, testInvoke); !errors.Is(err, ErrUnsupportedStart) {
		t.Fatalf("prompt refusal without durable outcome loaded with err %v, want ErrUnsupportedStart", err)
	}
}

func TestPromptLimitLeavesLinuxArgumentHeadroom(t *testing.T) {
	t.Parallel()
	// Apostrophes are shellQuote's worst case: each input byte expands to the
	// four-byte sequence that closes, escapes, and reopens the quoted word.
	command := agentCommand(
		strings.Repeat("'", maxPromptBytes),
		"00000000-0000-4000-8000-000000000000",
		"inv-headroom",
	)[2]
	if len(command) >= linuxMaxArgumentBytes {
		t.Fatalf("max prompt produces %d-byte sh argument, Linux limit is %d",
			len(command), linuxMaxArgumentBytes)
	}
}

func TestRenderPromptRejectsNonUTF8Inputs(t *testing.T) {
	t.Parallel()
	for _, field := range []struct {
		name   string
		mutate func(*durableInputs)
	}{
		{"prompt package", func(in *durableInputs) { in.PromptPackage = []byte{0xff} }},
		{"specification", func(in *durableInputs) { in.Specification = []byte{0xff} }},
		{"policy", func(in *durableInputs) { in.Policy = []byte{0xff} }},
	} {
		t.Run(field.name, func(t *testing.T) {
			inputs := durableInputs{
				PromptPackage: []byte("prompt"),
				Specification: []byte("specification"),
				Policy:        []byte("policy"),
			}
			field.mutate(&inputs)
			if _, err := renderPromptParts(inputs); !errors.Is(err, ErrUnsupportedStart) {
				t.Fatalf("renderPromptParts = %v, want ErrUnsupportedStart", err)
			}
		})
	}
}

// stubArtifacts records what the driver persisted, so a test can assert that
// a result names only artifacts that were actually stored.
type stubArtifacts struct {
	mu     sync.Mutex
	blobs  map[domain.Digest][]byte
	claims map[domain.InvocationID][]domain.AgentClaim
	err    error
}

func newStubArtifacts() *stubArtifacts {
	return &stubArtifacts{
		blobs:  map[domain.Digest][]byte{},
		claims: map[domain.InvocationID][]domain.AgentClaim{},
	}
}

func (a *stubArtifacts) PutBlob(_ context.Context, digest domain.Digest, body []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return a.err
	}
	a.blobs[digest] = body
	return nil
}

func (a *stubArtifacts) RecordClaims(
	_ context.Context, id domain.InvocationID, claims []domain.AgentClaim,
) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return a.err
	}
	a.claims[id] = claims
	return nil
}

func newTestDriver(t *testing.T, gate *stubGate, exports *stubExports) *Driver {
	t.Helper()
	root := t.TempDir()
	d, err := New(Config{
		Lifetime: context.Background(),
		Dir:      filepath.Join(root, "driver"), SeedRoot: filepath.Join(root, "seeds"),
		ExportRoot: filepath.Clean(os.TempDir()),
		Gate:       gate, Seeder: stubSeeder{}, Exports: exports, Outcomes: exports,
		Authority: stubAuthority{},
		Artifacts: newStubArtifacts(),
		Volumes:   stubVolumes{volume: testAuthVol},
		Now:       func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

// The two Seeder methods must not collapse into one call. Ward's observer
// proves the workspace's raw worktree against HEAD, so seeding from the
// repository-only shape leaves every tracked path missing, reports the
// workspace dirty, and refuses the run before the writer starts. The import
// lane keeps the lighter shape deliberately, so this pins which lane calls
// which.
func TestWorkspaceSeedFetchesAMaterializedWorktree(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	seeder := &recordingSeeder{}
	d, err := New(Config{
		Lifetime: context.Background(),
		Dir:      filepath.Join(root, "driver"), SeedRoot: filepath.Join(root, "seeds"),
		ExportRoot: filepath.Clean(os.TempDir()),
		Gate:       &stubGate{}, Seeder: seeder,
		Exports: newStubExports(), Outcomes: newStubExports(),
		Authority: stubAuthority{},
		Artifacts: newStubArtifacts(),
		Volumes:   stubVolumes{volume: testAuthVol},
		Now:       func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = d.Close(context.Background()) })

	in := intent{
		InvocationID: testInvoke, RunID: RunIDFor(testInvoke),
		Spec: testStartSpec(), Seed: filepath.Join(root, "seeds", RunIDFor(testInvoke)),
	}
	if err := d.seedBase(context.Background(), in); err != nil {
		t.Fatalf("seedBase: %v", err)
	}
	if seeder.worktree != 1 || seeder.plain != 0 {
		t.Errorf("seedBase used worktree=%d plain=%d fetches, want the materialized worktree only",
			seeder.worktree, seeder.plain)
	}
}

func TestCloseCancelsAndAwaitsLiveSessions(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	canceled := make(chan struct{})
	cancellationRequested := make(chan struct{})
	gate := &stubGate{
		cancelFn: func(string) error {
			close(cancellationRequested)
			return nil
		},
		handoffCtxFn: func(ctx context.Context, _ ward.HandoffSpec) (*ward.HandoffResult, error) {
			close(entered)
			<-ctx.Done()
			close(canceled)
			return nil, ctx.Err()
		},
	}
	d := newTestDriver(t, gate, newStubExports())
	spec := testStartSpec()
	inputs := stageInputs(t, &spec)
	if err := d.StartWithInputs(context.Background(), testInvoke, spec,
		func(context.Context) (exec.StageInputs, error) { return inputs, nil },
	); err != nil {
		t.Fatalf("StartWithInputs: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("handoff did not start")
	}

	if err := d.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-cancellationRequested:
	default:
		t.Fatal("Close stopped the gate without first recording cancellation intent")
	}
	select {
	case <-canceled:
	default:
		t.Fatal("Close returned before the gate observed cancellation")
	}
	d.mu.Lock()
	_, live := d.running[testInvoke]
	d.mu.Unlock()
	if live {
		t.Fatal("Close returned with a live session")
	}
	if err := d.StartWithInputs(context.Background(), testInvoke, spec,
		func(context.Context) (exec.StageInputs, error) { return inputs, nil },
	); !errors.Is(err, ErrDriverClosed) {
		t.Fatalf("start after Close = %v, want ErrDriverClosed", err)
	}
}

func TestCloseDrainsEverySessionAfterCancellationAmendmentFailure(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{}, 2)
	canceled := make(chan struct{}, 2)
	cancelErr := errors.New("journal temporarily unavailable")
	gate := &stubGate{
		cancelFn: func(string) error { return cancelErr },
		handoffCtxFn: func(ctx context.Context, _ ward.HandoffSpec) (*ward.HandoffResult, error) {
			entered <- struct{}{}
			<-ctx.Done()
			canceled <- struct{}{}
			return nil, ctx.Err()
		},
	}
	d := newTestDriver(t, gate, newStubExports())
	ids := []domain.InvocationID{testInvoke, "inv-implement-run-2"}
	for _, id := range ids {
		spec := testStartSpec()
		spec.Workspace = WorkspaceFor(id)
		inputs := stageInputs(t, &spec)
		if err := d.StartWithInputs(
			context.Background(),
			id,
			spec,
			func(context.Context) (exec.StageInputs, error) { return inputs, nil },
		); err != nil {
			t.Fatalf("StartWithInputs(%s): %v", id, err)
		}
	}
	for range ids {
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("handoff did not start")
		}
	}

	if err := d.Close(context.Background()); !errors.Is(err, cancelErr) {
		t.Fatalf("Close = %v, want cancellation amendment error", err)
	}
	for range ids {
		select {
		case <-canceled:
		default:
			t.Fatal("Close returned before every gate observed cancellation")
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.running) != 0 {
		t.Fatalf("Close returned with %d live sessions", len(d.running))
	}
}

func TestClosePreservesCanceledSeedForRecovery(t *testing.T) {
	t.Parallel()
	records := newStubExports()
	d := newTestDriver(t, &stubGate{}, records)
	entered := make(chan struct{})
	d.seeder = cancelSeeder{entered: entered}
	spec := testStartSpec()
	inputs := stageInputs(t, &spec)
	if err := d.StartWithInputs(context.Background(), testInvoke, spec,
		func(context.Context) (exec.StageInputs, error) { return inputs, nil },
	); err != nil {
		t.Fatalf("StartWithInputs: %v", err)
	}
	<-entered
	if err := d.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	in, err := d.loadIntent(context.Background(), testInvoke)
	if err != nil {
		t.Fatalf("load canceled seed intent: %v", err)
	}
	if in.Phase != phaseSeeding || in.Result != nil {
		t.Fatalf("intent = %#v, want recoverable seeding phase", in)
	}
	if len(records.outcomes) != 0 {
		t.Fatalf("canceled seed recorded %d terminal outcomes", len(records.outcomes))
	}
}

func TestCancelMakesPreJournalSeedTerminal(t *testing.T) {
	t.Parallel()
	records := newStubExports()
	d := newTestDriver(t, &stubGate{
		cancelFn: func(string) error { return ward.ErrJournalRecordNotFound },
	}, records)
	entered := make(chan struct{})
	d.seeder = cancelSeeder{entered: entered}
	spec := testStartSpec()
	inputs := stageInputs(t, &spec)
	if err := d.StartWithInputs(context.Background(), testInvoke, spec,
		func(context.Context) (exec.StageInputs, error) { return inputs, nil },
	); err != nil {
		t.Fatalf("StartWithInputs: %v", err)
	}
	<-entered
	if err := d.Cancel(context.Background(), testInvoke); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	result, err := d.Collect(context.Background(), testInvoke)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if result.Status != exec.StatusCanceled {
		t.Fatalf("result = %#v, want canceled", result)
	}
	if len(records.outcomes) != 1 ||
		records.outcomes[testInvoke].Status != domain.ExecutionOutcomeCanceled {
		t.Fatalf("outcomes = %#v, want one canceled outcome", records.outcomes)
	}
}

func TestCancelRemainsAvailableAfterCurrentPolicyDrifts(t *testing.T) {
	t.Parallel()
	records := newStubExports()
	d := newTestDriver(t, &stubGate{
		cancelFn: func(string) error { return ward.ErrJournalRecordNotFound },
	}, records)
	entered := make(chan struct{})
	d.seeder = cancelSeeder{entered: entered}
	spec := testStartSpec()
	inputs := stageInputs(t, &spec)
	if err := d.StartWithInputs(context.Background(), testInvoke, spec,
		func(context.Context) (exec.StageInputs, error) { return inputs, nil },
	); err != nil {
		t.Fatalf("StartWithInputs: %v", err)
	}
	<-entered
	d.authority = stubAuthority{startErr: domain.ErrTrustProfileSuperseded}

	if err := d.Cancel(context.Background(), testInvoke); err != nil {
		t.Fatalf("Cancel after policy drift: %v", err)
	}
	result, err := d.Collect(context.Background(), testInvoke)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if result.Status != exec.StatusCanceled {
		t.Fatalf("result = %#v, want canceled", result)
	}
}

func TestPreJournalCancellationPersistenceFailureRetainsRetry(t *testing.T) {
	t.Parallel()
	records := newStubExports()
	d := newTestDriver(t, &stubGate{
		cancelFn: func(string) error { return ward.ErrJournalRecordNotFound },
	}, records)
	entered := make(chan struct{})
	d.seeder = cancelSeeder{entered: entered}
	spec := testStartSpec()
	inputs := stageInputs(t, &spec)
	if err := d.StartWithInputs(context.Background(), testInvoke, spec,
		func(context.Context) (exec.StageInputs, error) { return inputs, nil },
	); err != nil {
		t.Fatalf("StartWithInputs: %v", err)
	}
	<-entered
	if err := os.Chmod(d.dir, 0o500); err != nil { //nolint:gosec // adversarial owner-only fixture
		t.Fatal(err)
	}
	cancelErr := d.Cancel(context.Background(), testInvoke)
	if err := os.Chmod(d.dir, 0o700); err != nil { //nolint:gosec // restore private state directory
		t.Fatal(err)
	}
	if cancelErr == nil {
		t.Fatal("Cancel unexpectedly persisted through a read-only state directory")
	}
	d.mu.Lock()
	sess := d.running[testInvoke]
	pending := sess != nil && sess.pendingResult != nil &&
		sess.preJournalCancellation
	d.mu.Unlock()
	if !pending {
		t.Fatal("failed pre-journal cancellation discarded its durable retry")
	}

	status, err := d.Inspect(context.Background(), testInvoke)
	if err != nil {
		t.Fatalf("Inspect retry: %v", err)
	}
	if status.Status != exec.StatusCanceled {
		t.Fatalf("Inspect status = %q, want canceled", status.Status)
	}
	result, err := d.Collect(context.Background(), testInvoke)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if result.Status != exec.StatusCanceled {
		t.Fatalf("result = %#v, want canceled", result)
	}
}

func TestPostSeedAuthStoreCancellationPreservesSeedingForRecovery(t *testing.T) {
	t.Parallel()
	records := newStubExports()
	d := newTestDriver(t, &stubGate{}, records)
	volumes := &secondLookupRefusingVolumes{
		err: context.Canceled, entered: make(chan struct{}), release: make(chan struct{}),
	}
	d.volumes = volumes
	spec := testStartSpec()
	inputs := stageInputs(t, &spec)
	if err := d.StartWithInputs(context.Background(), testInvoke, spec,
		func(context.Context) (exec.StageInputs, error) { return inputs, nil },
	); err != nil {
		t.Fatalf("StartWithInputs: %v", err)
	}
	<-volumes.entered
	d.mu.Lock()
	done := d.running[testInvoke].done
	d.mu.Unlock()
	close(volumes.release)
	<-done

	in, err := d.loadIntent(context.Background(), testInvoke)
	if err != nil {
		t.Fatalf("load post-seed canceled intent: %v", err)
	}
	if in.Phase != phaseSeeding || in.Result != nil {
		t.Fatalf("intent = %#v, want recoverable seeding phase", in)
	}
	if len(records.outcomes) != 0 {
		t.Fatalf("post-seed cancellation recorded %d terminal outcomes", len(records.outcomes))
	}
}

func TestDefinitiveSeedRefusalCommitsFailure(t *testing.T) {
	t.Parallel()
	records := newStubExports()
	d := newTestDriver(t, &stubGate{}, records)
	d.seeder = stubSeeder{err: errors.Join(ErrSeedRefused, errors.New("base absent"))}
	spec := testStartSpec()
	inputs := stageInputs(t, &spec)
	if err := d.StartWithInputs(context.Background(), testInvoke, spec,
		func(context.Context) (exec.StageInputs, error) { return inputs, nil },
	); err != nil {
		t.Fatalf("StartWithInputs: %v", err)
	}
	d.mu.Lock()
	done := d.running[testInvoke].done
	d.mu.Unlock()
	<-done

	result, err := d.Collect(context.Background(), testInvoke)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if result.Status != exec.StatusFailed || !strings.Contains(result.Summary, "base absent") {
		t.Fatalf("result = %#v, want definitive seed failure", result)
	}
	if len(records.outcomes) != 1 {
		t.Fatalf("definitive seed refusal recorded %d outcomes, want 1", len(records.outcomes))
	}
}

func TestDefinitiveSeedRefusalOutranksConcurrentShutdown(t *testing.T) {
	t.Parallel()
	records := newStubExports()
	d := newTestDriver(t, &stubGate{}, records)
	entered := make(chan struct{})
	d.seeder = refuseOnCancelSeeder{entered: entered}
	spec := testStartSpec()
	inputs := stageInputs(t, &spec)
	if err := d.StartWithInputs(context.Background(), testInvoke, spec,
		func(context.Context) (exec.StageInputs, error) { return inputs, nil },
	); err != nil {
		t.Fatalf("StartWithInputs: %v", err)
	}
	<-entered
	if err := d.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	status, err := d.Inspect(context.Background(), testInvoke)
	if err != nil {
		t.Fatalf("Inspect after cleanup became unavailable: %v", err)
	}
	if status.Status != exec.StatusFailed {
		t.Fatalf("Inspect status = %q, want failed", status.Status)
	}
	result, err := d.Collect(context.Background(), testInvoke)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if result.Status != exec.StatusFailed {
		t.Fatalf("result status = %q, want failed definitive refusal", result.Status)
	}
}

func TestHandoffSpecBindsContainmentAndInstructions(t *testing.T) {
	t.Parallel()
	gate := &stubGate{}
	d := newTestDriver(t, gate, newStubExports())
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
		InvocationID: testInvoke, RunID: RunIDFor(testInvoke), Phase: phaseRunning,
		Spec: spec, Seed: filepath.Join(d.seedRoot, RunIDFor(testInvoke)),
		Prompt: prompt, Inputs: durableInputsFrom(inputs),
		Instructions: instructions, RecordedAt: fixedNow, CommitDate: fixedNow,
	}

	hs, err := d.handoffSpec(context.Background(), in)
	if err != nil {
		t.Fatalf("handoffSpec: %v", err)
	}
	if len(hs.Agent.CredentialMounts) != 1 {
		t.Fatalf("credential mounts = %#v, want exactly the leased one", hs.Agent.CredentialMounts)
	}
	mount := hs.Agent.CredentialMounts[0]
	if mount.Volume != testAuthVol || mount.Writable ||
		mount.Manifest != ward.CredentialManifestSetupToken {
		t.Errorf(
			"credential mount = %#v, want the manifest-verified trusted token volume read-only",
			mount,
		)
	}
	if mount.Target == "/root/.claude" || strings.HasPrefix(mount.Target, "/root/.claude/") {
		t.Errorf("credential mount target %q collides with the read-only instruction mount", mount.Target)
	}
	if hs.Agent.LaunchState != ward.LaunchStateClaudeClean {
		t.Errorf("launch state = %q, want clean lifecycle-scoped Claude state",
			hs.Agent.LaunchState)
	}
	command := strings.Join(hs.Agent.Command, " ")
	for _, required := range []string{
		"setpriv --reuid=" + agentUID,
		"--bounding-set=-all --no-new-privs",
		writerOutcomePath,
	} {
		if !strings.Contains(command, required) {
			t.Errorf("agent command omits privilege/outcome boundary %q", required)
		}
	}
	if filepath.Dir(writerOutcomePath) == filepath.Dir(transcriptPath) {
		t.Fatal("writer outcome shares the agent-writable transcript directory")
	}
	if hs.Agent.OutcomeMarkerPath != writerOutcomePath {
		t.Errorf("outcome marker = %q, want protected %q",
			hs.Agent.OutcomeMarkerPath, writerOutcomePath)
	}
	if hs.AuthStoreLease == nil || hs.AuthStoreLease.AuthIdentityID != testAuthID ||
		hs.AuthStoreLease.Holder != testInvoke {
		t.Errorf("auth store lease = %#v, want this invocation on the admitted identity", hs.AuthStoreLease)
	}
	if hs.Agent.VendorInstructions.Digest != instructions.Digest {
		t.Errorf("vendor instruction digest = %q, want the admitted %q",
			hs.Agent.VendorInstructions.Digest, instructions.Digest)
	}
	// No credential material rides the recorded container environment, which
	// is the surface ward inspects and the conformance check compares. It is
	// not a containment claim about the writer: the launcher reads the token
	// from its mounted file into the environment of the process it execs, so
	// the model and every tool it spawns can read it from their own environ.
	// Export credential scanning is the downstream control for that, not this.
	for _, env := range hs.Agent.Env {
		if strings.Contains(strings.ToUpper(env), "TOKEN") || strings.Contains(strings.ToUpper(env), "KEY") {
			t.Errorf("agent env %q looks like credential material", env)
		}
	}
}

func TestHandoffSpecRefusesUnsupportedContainment(t *testing.T) {
	t.Parallel()
	gate := &stubGate{}
	d := newTestDriver(t, gate, newStubExports())
	valid := testStartSpec()
	inputs := stageInputs(t, &valid)
	instructions, _ := ward.VendorInstructionsFromStageInputs(inputs)
	prompt, _ := renderPrompt(inputs)
	base := intent{
		InvocationID: testInvoke, RunID: RunIDFor(testInvoke), Phase: phaseRunning,
		Seed: filepath.Join(d.seedRoot, RunIDFor(testInvoke)), Prompt: prompt,
		Instructions: instructions, RecordedAt: fixedNow, CommitDate: fixedNow,
	}
	tests := []struct {
		name string
		edit func(*exec.StartSpec)
	}{
		{"api key isolated", func(s *exec.StartSpec) { s.CredentialMode = domain.CredentialAPIKeyIsolated }},
		{"web-read egress", func(s *exec.StartSpec) { s.EgressProfile = domain.EgressProviderWebRead }},
		{"no auth identity", func(s *exec.StartSpec) { s.AuthIdentityID = "" }},
		{"foreign workspace", func(s *exec.StartSpec) { s.Workspace = "freeside-handoff-someone-else-ws" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := valid
			tc.edit(&spec)
			in := base
			in.Spec = spec
			if _, err := d.handoffSpec(context.Background(), in); !errors.Is(err, ErrUnsupportedStart) {
				t.Fatalf("handoffSpec error = %v, want ErrUnsupportedStart", err)
			}
		})
	}
}

func TestStartRefusesDuplicateAndLeavesNoIntentOnRefusal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	gate := &stubGate{handoffFn: func(ward.HandoffSpec) (*ward.HandoffResult, error) {
		return nil, errors.New("scripted handoff failure")
	}}
	d := newTestDriver(t, gate, newStubExports())
	valid := testStartSpec()
	inputs := stageInputs(t, &valid)
	load := func(context.Context) (exec.StageInputs, error) { return inputs, nil }

	// A refused start commits nothing, so the id stays unknown and a later
	// start may still win it.
	unsupported := valid
	unsupported.CredentialMode = domain.CredentialAPIKeyIsolated
	if err := d.StartWithInputs(ctx, testInvoke, unsupported, load); !errors.Is(err, ErrUnsupportedStart) {
		t.Fatalf("refused start error = %v, want ErrUnsupportedStart", err)
	}
	if _, err := d.Inspect(ctx, testInvoke); !errors.Is(err, exec.ErrUnknownInvocation) {
		t.Fatalf("Inspect after refusal = %v, want ErrUnknownInvocation", err)
	}

	if err := d.StartWithInputs(ctx, testInvoke, valid, load); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := d.StartWithInputs(ctx, testInvoke, valid, load); !errors.Is(err, exec.ErrDuplicateStart) {
		t.Fatalf("duplicate start error = %v, want ErrDuplicateStart", err)
	}

	// A handoff error is not proof that teardown succeeded. Once the local
	// pipeline exits, the durable running phase remains for ward recovery.
	waitSessionDone(t, d, testInvoke)
	// The gate saw this run's spec, so the failure came from the scripted
	// handoff rather than from the driver never reaching the gate.
	if got := gate.lastSpec(t).RunID; got != RunIDFor(testInvoke) {
		t.Fatalf("gate run id = %q, want %q", got, RunIDFor(testInvoke))
	}
	in, err := d.loadIntent(ctx, testInvoke)
	if err != nil {
		t.Fatalf("load intent: %v", err)
	}
	if in.Phase != phaseRunning || in.Result != nil {
		t.Fatalf("handoff failure intent = %#v, want running recovery state", in)
	}
}

func TestStartRunsPreJobAfterDuplicateArbitration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newTestDriver(t, &stubGate{
		handoffFn: func(ward.HandoffSpec) (*ward.HandoffResult, error) {
			return nil, errors.New("scripted handoff failure")
		},
	}, newStubExports())
	probes := 0
	refuse := true
	d.preJob = func(context.Context, domain.InvocationID) error {
		probes++
		if refuse {
			return errors.New("runtime unavailable")
		}
		return nil
	}
	spec := testStartSpec()
	inputs := stageInputs(t, &spec)
	loadCalls := 0
	load := func(context.Context) (exec.StageInputs, error) {
		loadCalls++
		return inputs, nil
	}

	if err := d.StartWithInputs(ctx, testInvoke, spec, load); !errors.Is(err, exec.ErrPreJobRefused) {
		t.Fatalf("pre-job refusal = %v, want ErrPreJobRefused", err)
	}
	if probes != 1 || loadCalls != 0 {
		t.Fatalf("refused probes = %d, loads = %d, want 1 and 0", probes, loadCalls)
	}
	if _, err := d.Inspect(ctx, testInvoke); !errors.Is(err, exec.ErrUnknownInvocation) {
		t.Fatalf("Inspect after pre-job refusal = %v, want unknown invocation", err)
	}

	refuse = false
	if err := d.StartWithInputs(ctx, testInvoke, spec, load); err != nil {
		t.Fatalf("start after healthy pre-job: %v", err)
	}
	if err := d.StartWithInputs(ctx, testInvoke, spec, load); !errors.Is(err, exec.ErrDuplicateStart) {
		t.Fatalf("duplicate start = %v, want duplicate", err)
	}
	if probes != 2 || loadCalls != 1 {
		t.Fatalf("final probes = %d, loads = %d, want 2 and 1", probes, loadCalls)
	}
	waitSessionDone(t, d, testInvoke)
}

func TestNonExportTerminalStateRequiresDurableOutcome(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	records := newStubExports()
	d := newTestDriver(t, &stubGate{}, records)
	orphan(t, d, phaseSeeding, nil)
	if err := d.commitResult(testInvoke, exec.StageResult{
		InvocationID: testInvoke,
		Status:       exec.StatusFailed,
		Summary:      "materialization failed",
	}); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(d.intentPath(testInvoke))
	if err != nil {
		t.Fatalf("read intent: %v", err)
	}
	in, err := decodeIntent(body)
	if err != nil {
		t.Fatalf("decode intent: %v", err)
	}
	originalSummary := in.Result.Summary
	in.Result.Summary = "forged failure"
	if err := d.saveIntent(in); err != nil {
		t.Fatalf("write tampered intent: %v", err)
	}
	if _, err := d.loadIntent(ctx, testInvoke); !errors.Is(err, ErrUnsupportedStart) {
		t.Fatalf("tampered failure loaded with err %v, want ErrUnsupportedStart", err)
	}

	records.mu.Lock()
	delete(records.outcomes, testInvoke)
	records.mu.Unlock()
	in.Result.Summary = originalSummary
	if err := d.saveIntent(in); err != nil {
		t.Fatalf("write unauthenticated intent: %v", err)
	}
	if _, err := d.loadIntent(ctx, testInvoke); !errors.Is(err, ErrUnsupportedStart) {
		t.Fatalf("failure without durable outcome loaded with err %v, want ErrUnsupportedStart", err)
	}
}

func TestTerminalIntentPersistenceFailureRetainsAnInProcessRetry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newTestDriver(t, &stubGate{}, newStubExports())
	orphan(t, d, phaseSeeding, nil)
	seed := filepath.Join(d.seedRoot, RunIDFor(testInvoke))
	if err := os.MkdirAll(seed, 0o700); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	close(done)
	d.mu.Lock()
	d.running[testInvoke] = &session{cancel: func() {}, done: done}
	d.mu.Unlock()

	if err := os.Chmod(d.dir, 0o500); err != nil { //nolint:gosec // adversarial fixture makes its private state directory read-only
		t.Fatal(err)
	}
	result := exec.StageResult{
		InvocationID: testInvoke,
		Status:       exec.StatusFailed,
		Summary:      "materialization failed",
	}
	commitErr := d.commitResult(testInvoke, result)
	if err := os.Chmod(d.dir, 0o700); err != nil { //nolint:gosec // restore the private directory's owner-only permissions
		t.Fatal(err)
	}
	if commitErr == nil {
		t.Fatal("terminal commit unexpectedly succeeded in a read-only state directory")
	}
	d.mu.Lock()
	pending := d.running[testInvoke] != nil &&
		d.running[testInvoke].pendingResult != nil
	d.mu.Unlock()
	if !pending {
		t.Fatal("failed terminal commit discarded its in-process retry")
	}
	if _, err := os.Stat(seed); err != nil {
		t.Fatalf("failed terminal persistence removed the recoverable seed: %v", err)
	}

	status, err := d.Inspect(ctx, testInvoke)
	if err != nil {
		t.Fatalf("Inspect did not retry the terminal commit: %v", err)
	}
	if status.Status != exec.StatusFailed {
		t.Fatalf("Inspect status = %q, want failed", status.Status)
	}
	if _, err := d.Collect(ctx, testInvoke); err != nil {
		t.Fatalf("Collect after retry: %v", err)
	}
	d.mu.Lock()
	_, live := d.running[testInvoke]
	d.mu.Unlock()
	if live {
		t.Fatal("successful terminal retry left the completed session registered")
	}
	if _, err := os.Stat(seed); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("terminal retry left seed behind: %v", err)
	}
}

func TestTerminalOutcomeCommitsAfterCurrentPolicyDrifts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	records := newStubExports()
	d := newTestDriver(t, &stubGate{}, records)
	orphan(t, d, phaseSeeding, nil)
	d.authority = stubAuthority{startErr: domain.ErrTrustProfileSuperseded}
	result := exec.StageResult{
		InvocationID: testInvoke,
		Status:       exec.StatusFailed,
		Summary:      "materialization failed",
	}

	if err := d.commitResult(testInvoke, result); err != nil {
		t.Fatalf("commitResult after policy drift: %v", err)
	}
	collected, err := d.Collect(ctx, testInvoke)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if collected.InvocationID != result.InvocationID ||
		collected.Status != result.Status ||
		collected.Summary != result.Summary {
		t.Fatalf("result = %#v, want %#v", collected, result)
	}
	if len(records.outcomes) != 1 ||
		records.outcomes[testInvoke].Status != domain.ExecutionOutcomeFailed {
		t.Fatalf("outcomes = %#v, want one failed outcome", records.outcomes)
	}
}

func TestTerminalSeedCleanupIsRootScopedAndPhaseGated(t *testing.T) {
	t.Parallel()
	for _, ph := range []phase{
		"", "future", phaseSeeding, phaseRunning, phaseExported, phaseCommitted, phaseLost,
	} {
		t.Run(string(ph), func(t *testing.T) {
			t.Parallel()
			d := newTestDriver(t, &stubGate{}, newStubExports())
			runID := RunIDFor(testInvoke)
			seed := filepath.Join(d.seedRoot, runID)
			importSeed := filepath.Join(d.seedRoot, runID+"-import")
			if err := os.MkdirAll(seed, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(importSeed, 0o700); err != nil {
				t.Fatal(err)
			}
			in := intent{
				InvocationID: testInvoke,
				Phase:        ph,
				Seed:         filepath.Join(t.TempDir(), "forged-foreign-seed"),
			}
			err := d.cleanupTerminalSeed(in)
			terminal := ph == phaseCommitted || ph == phaseLost
			if terminal && err != nil {
				t.Fatalf("cleanup terminal seed: %v", err)
			}
			if !terminal && err == nil {
				t.Fatal("cleanup accepted a preterminal phase")
			}
			for _, path := range []string{seed, importSeed} {
				_, statErr := os.Stat(path)
				if terminal && !errors.Is(statErr, os.ErrNotExist) {
					t.Errorf("terminal cleanup left %s: %v", path, statErr)
				}
				if !terminal && statErr != nil {
					t.Errorf("preterminal cleanup changed %s: %v", path, statErr)
				}
			}
		})
	}

	t.Run("child symlink", func(t *testing.T) {
		t.Parallel()
		d := newTestDriver(t, &stubGate{}, newStubExports())
		outside := t.TempDir()
		sentinel := filepath.Join(outside, "keep")
		if err := os.WriteFile(sentinel, []byte("outside"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(d.seedRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(d.seedRoot, RunIDFor(testInvoke))
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
		if err := d.cleanupTerminalSeed(intent{
			InvocationID: testInvoke, Phase: phaseCommitted, Seed: outside,
		}); err != nil {
			t.Fatalf("cleanup symlink fixture: %v", err)
		}
		body, err := os.ReadFile(sentinel) //nolint:gosec // adversarial test-owned path
		if err != nil || string(body) != "outside" {
			t.Fatalf("root-scoped cleanup changed outside sentinel: %q, %v", body, err)
		}
		if _, err := os.Lstat(link); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("root-scoped cleanup left child symlink: %v", err)
		}
	})

	t.Run("root replacement", func(t *testing.T) {
		t.Parallel()
		d := newTestDriver(t, &stubGate{}, newStubExports())
		runID := RunIDFor(testInvoke)
		pinnedRoot := d.seedRoot + "-pinned"
		if err := os.Rename(d.seedRoot, pinnedRoot); err != nil {
			t.Fatal(err)
		}
		originalSeed := filepath.Join(pinnedRoot, runID)
		if err := os.MkdirAll(originalSeed, 0o700); err != nil {
			t.Fatal(err)
		}
		outside := t.TempDir()
		outsideSeed := filepath.Join(outside, runID)
		if err := os.MkdirAll(outsideSeed, 0o700); err != nil {
			t.Fatal(err)
		}
		sentinel := filepath.Join(outsideSeed, "keep")
		if err := os.WriteFile(sentinel, []byte("outside"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, d.seedRoot); err != nil {
			t.Fatal(err)
		}

		if err := d.cleanupTerminalSeed(intent{
			InvocationID: testInvoke, Phase: phaseCommitted,
		}); err != nil {
			t.Fatalf("cleanup after root replacement: %v", err)
		}
		if _, err := os.Stat(originalSeed); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("pinned root cleanup left original seed: %v", err)
		}
		body, err := os.ReadFile(sentinel) //nolint:gosec // adversarial test-owned path
		if err != nil || string(body) != "outside" {
			t.Fatalf("root replacement redirected deletion: %q, %v", body, err)
		}
	})
}

func waitSessionDone(t *testing.T, d *Driver, id domain.InvocationID) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		_, live := d.running[id]
		d.mu.Unlock()
		if !live {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("pipeline session did not exit")
}

// TestExportConvergenceAcceptsAnIdenticalReplay is the regression for a
// pointer-comparison convergence check: ExecutionExport carries the optional
// evidence digest as a pointer, and the constructor and the store's decode
// each allocate their own, so comparing the structs directly reports every
// evidence-carrying replay as a conflict — turning the crash window the
// convergence exists to close into a durable failure.
func TestExportConvergenceAcceptsAnIdenticalReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	exports := newStubExports()
	d := newTestDriver(t, &stubGate{}, exports)

	evidence := domain.Digest("sha256:" + strings.Repeat("ee", 32))
	build := func() domain.ExecutionExport {
		// Built twice, as a replay does: two independent allocations of the
		// same optional digest.
		digest := evidence
		record, err := domain.NewExecutionExport(domain.ExecutionExportInput{
			InvocationID: testInvoke, AdmissionID: domain.Digest("sha256:" + strings.Repeat("44", 32)),
			ObservedBaseSHA: testBase.BaseSHA, HeadSHA: strings.Repeat("c", 40),
			ManifestDigest:         domain.Digest("sha256:" + strings.Repeat("55", 32)),
			EvidenceManifestDigest: &digest, RecordedAt: fixedNow,
		})
		if err != nil {
			t.Fatalf("new execution export: %v", err)
		}
		return record
	}
	if err := d.recordExport(ctx, build(), ExecutionReplay{}); err != nil {
		t.Fatalf("first record: %v", err)
	}
	if err := d.recordExport(ctx, build(), ExecutionReplay{}); err != nil {
		t.Fatalf("identical replay was rejected: %v", err)
	}

	// A genuinely different head for the same invocation is still a conflict.
	conflicting := build()
	conflicting.HeadSHA = strings.Repeat("d", 40)
	if err := d.recordExport(ctx, conflicting, ExecutionReplay{}); !errors.Is(err, domain.ErrImmutableTransition) ||
		!errors.Is(err, errExportAuthorityConflict) {
		t.Fatalf("conflicting export error = %v, want durable-authority conflict", err)
	}
}

// TestOutcomeRefusesExistingExportWithoutRetrying proves a contradictory
// terminal result stops for operator repair. Treating the store's refusal as
// an ordinary write failure would retain a pending result and retry forever.
func TestOutcomeRefusesExistingExportWithoutRetrying(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	authorities := newStubExports()
	authorities.records[testInvoke] = domain.ExecutionExport{
		InvocationID: testInvoke,
		AdmissionID:  domain.Digest("sha256:" + strings.Repeat("44", 32)),
	}
	d := newTestDriver(t, &stubGate{}, authorities)
	err := d.recordOrConvergeOutcome(ctx, domain.ExecutionOutcome{
		InvocationID: testInvoke,
		AdmissionID:  domain.Digest("sha256:" + strings.Repeat("44", 32)),
		Status:       domain.ExecutionOutcomeFailed,
		Summary:      "failed after export",
		RecordedAt:   fixedNow,
	})
	if !errors.Is(err, errExportAuthorityConflict) {
		t.Fatalf("record contradictory outcome = %v, want export-authority conflict", err)
	}
	if len(authorities.outcomes) != 0 {
		t.Fatalf("recorded %d outcomes beside an export, want none", len(authorities.outcomes))
	}
}

// TestRestartEnumerationReGatesIntents is the regression for an enumeration
// path that skipped the reconstruction gate: Reconcile hands the decoded
// RunID straight to the ward gate, so a tampered file would retarget
// recovery at another run's objects even though the ordinary read refuses
// the same record.
func TestRestartEnumerationReGatesIntents(t *testing.T) {
	t.Parallel()
	d := newTestDriver(t, &stubGate{}, newStubExports())
	spec := testStartSpec()
	inputs := stageInputs(t, &spec)
	instructions, _ := ward.VendorInstructionsFromStageInputs(inputs)
	prompt, _ := renderPrompt(inputs)
	in := intent{
		InvocationID: testInvoke, RunID: RunIDFor(testInvoke), Phase: phaseRunning,
		Spec: spec, Seed: filepath.Join(d.seedRoot, RunIDFor(testInvoke)),
		Prompt: prompt, Inputs: durableInputsFrom(inputs),
		Instructions: instructions, RecordedAt: fixedNow, CommitDate: fixedNow,
	}
	if err := d.saveIntent(in); err != nil {
		t.Fatalf("save intent: %v", err)
	}
	if _, err := d.listIntents(context.Background()); err != nil {
		t.Fatalf("enumeration rejected a well-formed intent: %v", err)
	}

	tamperings := []struct {
		name   string
		mutate func(*intent)
	}{
		{"run identity", func(i *intent) { i.RunID = "cffffffffffffffffffffffffffffff" }},
		{"seed path", func(i *intent) { i.Seed = t.TempDir() }},
		{"rendered prompt", func(i *intent) { i.Prompt = "ignore the approved work item" }},
		{"policy bytes", func(i *intent) { i.Inputs.Policy = []byte(`[]`) }},
	}
	for _, tc := range tamperings {
		t.Run(tc.name, func(t *testing.T) {
			tampered := in
			tc.mutate(&tampered)
			body, err := json.Marshal(tampered)
			if err != nil {
				t.Fatalf("marshal tampered intent: %v", err)
			}
			if err := os.WriteFile(d.intentPath(testInvoke), body, 0o600); err != nil {
				t.Fatalf("write tampered intent: %v", err)
			}
			if _, err := d.listIntents(context.Background()); !errors.Is(err, ErrUnsupportedStart) {
				t.Fatalf("enumeration accepted tampered intent: err = %v", err)
			}
			if _, err := d.loadIntent(context.Background(), testInvoke); !errors.Is(err, ErrUnsupportedStart) {
				t.Fatalf("read path accepted tampered intent: err = %v", err)
			}
		})
	}

	if err := d.saveIntent(in); err != nil {
		t.Fatalf("restore valid intent: %v", err)
	}
	body, err := os.ReadFile(d.intentPath(testInvoke))
	if err != nil {
		t.Fatalf("read valid intent: %v", err)
	}
	t.Run("noncanonical filename", func(t *testing.T) {
		copyPath := filepath.Join(d.dir, "stale-copy.json")
		if err := os.WriteFile(copyPath, body, 0o600); err != nil { //nolint:gosec // fixed test filename under the driver's private temporary state directory
			t.Fatalf("write stale copy: %v", err)
		}
		defer func() { _ = os.Remove(copyPath) }()
		if _, err := d.listIntents(context.Background()); !errors.Is(err, ErrUnsupportedStart) {
			t.Fatalf("enumeration accepted a stale copy under another name: %v", err)
		}
	})

	malformed := []struct {
		name string
		body []byte
	}{
		{"unknown field", append(append([]byte{}, body[:len(body)-1]...), []byte(`,"unexpected":true}`)...)},
		{"duplicate field", append([]byte(`{"phase":"seeding",`), body[1:]...)},
		{"trailing value", append(append([]byte{}, body...), []byte("\n{}")...)},
	}
	for _, tc := range malformed {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(d.intentPath(testInvoke), tc.body, 0o600); err != nil {
				t.Fatalf("write malformed intent: %v", err)
			}
			if _, err := d.loadIntent(context.Background(), testInvoke); err == nil {
				t.Fatal("read path accepted malformed intent")
			}
			if _, err := d.listIntents(context.Background()); err == nil {
				t.Fatal("enumeration accepted malformed intent")
			}
		})
	}
}

func TestRecoveryOfLostHandoffReportsNoResult(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	gate := &stubGate{recoverFn: func(string, ward.HandoffSpec) (*ward.RecoveryResult, error) {
		return &ward.RecoveryResult{Outcome: ward.RecoveryLoss}, nil
	}}
	d := newTestDriver(t, gate, newStubExports())
	spec := testStartSpec()
	inputs := stageInputs(t, &spec)
	instructions, _ := ward.VendorInstructionsFromStageInputs(inputs)
	prompt, _ := renderPrompt(inputs)
	// An orphan: a durable running intent with no live pipeline, exactly what
	// a daemon restart leaves behind.
	in := intent{
		InvocationID: testInvoke, RunID: RunIDFor(testInvoke), Phase: phaseRunning,
		Spec: spec, Seed: filepath.Join(d.seedRoot, RunIDFor(testInvoke)),
		Prompt: prompt, Inputs: durableInputsFrom(inputs),
		Instructions: instructions, RecordedAt: fixedNow, CommitDate: fixedNow,
	}
	if err := d.saveIntent(in); err != nil {
		t.Fatalf("save orphan intent: %v", err)
	}
	if status, err := d.Inspect(ctx, testInvoke); err != nil || status.Status != exec.StatusGone {
		t.Fatalf("orphan Inspect = %q, %v; want gone", status.Status, err)
	}
	if err := d.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, err := d.Collect(ctx, testInvoke); !errors.Is(err, exec.ErrNoResult) {
		t.Fatalf("Collect after loss = %v, want ErrNoResult", err)
	}
}
