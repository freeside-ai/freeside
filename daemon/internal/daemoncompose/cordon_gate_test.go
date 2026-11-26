package daemoncompose

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/cordon"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec/stage"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// fakeCordonLauncher scripts the backend and records what the gate sent.
type fakeCordonLauncher struct {
	mu       sync.Mutex
	launchFn func(context.Context, cordon.LaunchRequest) (cordon.LaunchResult, error)
	recover  cordon.RecoveryResult
	err      error
	requests []cordon.LaunchRequest
	detached chan struct{}
	once     sync.Once
}

func newFakeCordonLauncher() *fakeCordonLauncher {
	return &fakeCordonLauncher{detached: make(chan struct{})}
}

func (f *fakeCordonLauncher) Launch(ctx context.Context, req cordon.LaunchRequest) (cordon.LaunchResult, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	if f.launchFn != nil {
		return f.launchFn(ctx, req)
	}
	return cordon.LaunchResult{EndedBy: cordon.EndedByExited}, f.err
}

func (f *fakeCordonLauncher) Export(context.Context, cordon.ExportRequest) (cordon.ExportResult, error) {
	return cordon.ExportResult{}, f.err
}

func (f *fakeCordonLauncher) Started(context.Context, string) (bool, error) { return true, f.err }

func (f *fakeCordonLauncher) RequestCancellation(context.Context, string) error { return f.err }

func (f *fakeCordonLauncher) Recover(context.Context, cordon.RecoverRequest) (cordon.RecoveryResult, error) {
	return f.recover, f.err
}

func (f *fakeCordonLauncher) AuthenticateReleasedExport(context.Context, string, string) error {
	return f.err
}

func (f *fakeCordonLauncher) ReleaseFinished(context.Context, []string) error { return f.err }

func (f *fakeCordonLauncher) SendMessage(context.Context, string, int64, string, string) error {
	return f.err
}

func (f *fakeCordonLauncher) Detach() { f.once.Do(func() { close(f.detached) }) }

func (f *fakeCordonLauncher) launched() []cordon.LaunchRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]cordon.LaunchRequest(nil), f.requests...)
}

const testCordonInvocation = domain.InvocationID("inv-cordon-gate")

func testCordonAdmission() domain.ExecutionAdmission {
	return domain.ExecutionAdmission{
		Backend: string(domain.BackendCordonedHostProcess), OperatingMode: domain.ModeAttendedDev,
		AgentBinding: &domain.AdmissionAgentBinding{
			EnrollmentID: "enrollment-cordon", EnrollmentGeneration: 3,
		},
	}
}

// tokenRead records the enrollment and generation a gate asked for.
type tokenRead struct {
	enrollment domain.ClientEnrollmentID
	generation int
}

func newTestCordonGate(t *testing.T, backend *fakeCordonLauncher, unattended bool) (*cordonGate, *[]tokenRead) {
	t.Helper()
	var reads []tokenRead
	gate := &cordonGate{
		backend: backend,
		probe:   func(context.Context) error { return errors.New("the class is not declared") },
		admission: func(_ context.Context, id domain.InvocationID) (domain.ExecutionAdmission, error) {
			if id != testCordonInvocation {
				return domain.ExecutionAdmission{}, fmt.Errorf("no admission for %s", id)
			}
			return testCordonAdmission(), nil
		},
		readToken: func(_ context.Context, id domain.ClientEnrollmentID, generation int) (ward.SetupToken, error) {
			reads = append(reads, tokenRead{id, generation})
			return ward.NewSetupToken([]byte(authAddTestToken))
		},
		unattended: unattended,
		scanRoot:   filepath.Join(t.TempDir(), "scan"),
		logger:     slog.New(slog.DiscardHandler),
	}
	return gate, &reads
}

func testGateLaunch() stage.CordonLaunch {
	return stage.CordonLaunch{
		InvocationID: testCordonInvocation, RunID: "run-cordon-gate",
		Base: domain.BaseRevision{
			Repo: "freeside-ai/candidate", BaseRef: "refs/heads/main", BaseSHA: strings.Repeat("a", 40),
		},
		Prompt: []byte("do the work"), Args: []string{"-p"},
	}
}

// TestCordonGateLaunchUsesTheAdmittedGeneration shows the launch carries
// the token of the generation the admission bound, and that the class's
// proof is required when the daemon or the admission is unattended.
func TestCordonGateLaunchUsesTheAdmittedGeneration(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		daemon       bool
		admission    domain.OperatingMode
		requireProof bool
	}{
		{"attended", false, domain.ModeAttendedDev, false},
		{"unattended daemon", true, domain.ModeAttendedDev, true},
		{"unattended admission", false, domain.ModeUnattended, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backend := newFakeCordonLauncher()
			gate, reads := newTestCordonGate(t, backend, tc.daemon)
			gate.admission = func(context.Context, domain.InvocationID) (domain.ExecutionAdmission, error) {
				admission := testCordonAdmission()
				admission.OperatingMode = tc.admission
				return admission, nil
			}
			end, err := gate.Launch(context.Background(), testGateLaunch())
			if err != nil || end.EndedBy != stage.CordonEndedByExited || end.ExitCode != 0 {
				t.Fatalf("Launch = %+v, %v; want a clean exit", end, err)
			}
			if want := []tokenRead{{"enrollment-cordon", 3}}; len(*reads) != 1 || (*reads)[0] != want[0] {
				t.Fatalf("token reads = %+v, want %+v", *reads, want)
			}
			requests := backend.launched()
			launch := testGateLaunch()
			if len(requests) != 1 {
				t.Fatalf("launches = %d, want 1", len(requests))
			}
			req := requests[0]
			if req.RunID != launch.RunID || req.Repository != launch.Base.Repo || req.BaseRef != launch.Base.BaseRef ||
				req.BaseSHA != launch.Base.BaseSHA || string(req.Prompt) != string(launch.Prompt) ||
				req.RequireProof != tc.requireProof {
				t.Fatalf("launch request = %+v, want the launch's run, base, and prompt, proof %v", req, tc.requireProof)
			}
			if err := gate.PreJob(context.Background(), testCordonInvocation); (err != nil) != tc.requireProof {
				t.Fatalf("PreJob = %v; it probes exactly when the launch requires the proof", err)
			}
		})
	}
}

func TestCordonGateRefusesAnUnboundAdmission(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		edit func(*domain.ExecutionAdmission)
	}{
		{"warded backend", func(a *domain.ExecutionAdmission) { a.Backend = "" }},
		{"no binding", func(a *domain.ExecutionAdmission) { a.AgentBinding = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backend := newFakeCordonLauncher()
			gate, reads := newTestCordonGate(t, backend, true)
			gate.admission = func(context.Context, domain.InvocationID) (domain.ExecutionAdmission, error) {
				admission := testCordonAdmission()
				tc.edit(&admission)
				return admission, nil
			}
			if _, err := gate.Launch(context.Background(), testGateLaunch()); err == nil {
				t.Fatal("Launch accepted an admission that binds no cordoned generation")
			}
			if len(*reads) != 0 || len(backend.launched()) != 0 {
				t.Fatalf("token reads %d, launches %d; want neither", len(*reads), len(backend.launched()))
			}
		})
	}
}

// TestCordonGateDetachKeepsTheRunAlive shows a caller's cancellation
// reaches the launch only until Detach begins: after it, the launch
// returns detached with its own context never canceled.
func TestCordonGateDetachKeepsTheRunAlive(t *testing.T) {
	t.Parallel()
	for _, detachFirst := range []bool{false, true} {
		t.Run(fmt.Sprint("detach first=", detachFirst), func(t *testing.T) {
			t.Parallel()
			backend := newFakeCordonLauncher()
			started := make(chan struct{})
			canceled := make(chan bool, 1)
			backend.launchFn = func(ctx context.Context, _ cordon.LaunchRequest) (cordon.LaunchResult, error) {
				close(started)
				select {
				case <-ctx.Done():
					canceled <- true
					return cordon.LaunchResult{EndedBy: cordon.EndedByCanceled, ExitCode: -1}, nil
				case <-backend.detached:
					canceled <- false
					return cordon.LaunchResult{}, fmt.Errorf("%w: run keeps running", cordon.ErrDetached)
				}
			}
			gate, _ := newTestCordonGate(t, backend, false)
			ctx, cancel := context.WithCancel(context.Background())
			lifetime := cordonDriverLifetime(ctx, gate.Detach)
			sessionCtx, cancelSession := context.WithCancel(lifetime)
			defer cancelSession()
			done := make(chan error, 1)
			go func() {
				_, err := gate.Launch(sessionCtx, testGateLaunch())
				done <- err
			}()
			<-started
			if detachFirst {
				// Shutdown: the process context ends, which detaches first.
				cancel()
			} else {
				defer cancel()
				cancelSession()
			}
			err := <-done
			if got := <-canceled; got == detachFirst {
				t.Fatalf("launch context canceled = %v with detach first %v", got, detachFirst)
			}
			if detachFirst != errors.Is(err, stage.ErrCordonDetached) {
				t.Fatalf("Launch = %v; want detached exactly when shutdown detached", err)
			}
		})
	}
}

// TestCordonGateShutdownDuringTheTokenRead shows a launch whose token read
// a shutdown cuts short returns detached, so the driver leaves the intent
// for recovery rather than recording it canceled.
func TestCordonGateShutdownDuringTheTokenRead(t *testing.T) {
	t.Parallel()
	backend := newFakeCordonLauncher()
	gate, _ := newTestCordonGate(t, backend, false)
	gate.readToken = func(context.Context, domain.ClientEnrollmentID, int) (ward.SetupToken, error) {
		gate.Detach()
		return ward.SetupToken{}, context.Canceled
	}
	if _, err := gate.Launch(context.Background(), testGateLaunch()); !errors.Is(err, stage.ErrCordonDetached) {
		t.Fatalf("Launch = %v, want detached", err)
	}
	if len(backend.launched()) != 0 {
		t.Fatal("a detached gate launched a run")
	}
}

// TestCordonDriverLifetimeEndsAfterDetach pins the shutdown order: the
// driver's lifetime is still live while detach runs.
func TestCordonDriverLifetimeEndsAfterDetach(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	var lifetime context.Context
	liveAtDetach := make(chan bool, 1)
	lifetime = cordonDriverLifetime(ctx, func() { liveAtDetach <- lifetime.Err() == nil })
	cancel()
	<-lifetime.Done()
	if !<-liveAtDetach {
		t.Fatal("the driver's lifetime ended before the cordon detached")
	}
}

func TestCordonGateFailureEvidence(t *testing.T) {
	t.Parallel()
	credential := "remote: https://ghp_" + strings.Repeat("b", 36) + "@github.com/x/y\n"
	for _, tc := range []struct {
		name        string
		transcript  *string
		stderr      string
		end         cordon.EndedBy
		exit        int
		want        string
		unavailable bool
	}{
		{
			"nonzero exit keeps transcript and stderr", ptr("{\"type\":\"result\"}\n"), "boom\n",
			cordon.EndedByExited, 2, "{\"type\":\"result\"}\nboom\n", false,
		},
		{"stall keeps the transcript", ptr("partial\n"), "", cordon.EndedByStalled, -1, "partial\n", false},
		{"no transcript", nil, "boom\n", cordon.EndedByExited, 1, "", true},
		{"a credential withholds it", ptr(credential), "", cordon.EndedByExited, 1, "", true},
		{"invalid UTF-8", ptr("\xff\xfe"), "", cordon.EndedByExited, 1, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			transcript, stderr := filepath.Join(dir, "transcript"), filepath.Join(dir, "stderr")
			if tc.transcript != nil {
				writeFixture(t, transcript, *tc.transcript)
			}
			writeFixture(t, stderr, tc.stderr)
			backend := newFakeCordonLauncher()
			backend.launchFn = func(context.Context, cordon.LaunchRequest) (cordon.LaunchResult, error) {
				return cordon.LaunchResult{
					EndedBy: tc.end, ExitCode: tc.exit, TranscriptPath: transcript, StderrPath: stderr,
				}, nil
			}
			gate, _ := newTestCordonGate(t, backend, false)
			end, err := gate.Launch(context.Background(), testGateLaunch())
			if err != nil || end.Failure == nil {
				t.Fatalf("Launch = %+v, %v; want failure evidence", end, err)
			}
			if tc.unavailable {
				if !end.Failure.Unavailable || len(end.Failure.Body) != 0 || end.Failure.Digest != "" {
					t.Fatalf("evidence = %+v, want unavailable", end.Failure)
				}
				return
			}
			if string(end.Failure.Body) != tc.want || end.Failure.Digest != contentaddr.Sum([]byte(tc.want)) {
				t.Fatalf("evidence = %q (%s), want %q", end.Failure.Body, end.Failure.Digest, tc.want)
			}
		})
	}
}

func TestCordonGateRecoverReadsAFailedRunsEvidence(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	transcript := filepath.Join(dir, "transcript")
	writeFixture(t, transcript, "last words\n")
	backend := newFakeCordonLauncher()
	backend.recover = cordon.RecoveryResult{
		Outcome: cordon.RecoveryFailed, EndedBy: cordon.EndedByExited, ExitCode: 4,
		TranscriptPath: transcript, StderrPath: filepath.Join(dir, "absent"),
	}
	gate, _ := newTestCordonGate(t, backend, false)
	got, err := gate.Recover(context.Background(), stage.CordonExportRequest{RunID: "run-cordon-gate"})
	if err != nil || got.Outcome != stage.CordonRecoveryFailed || got.End.ExitCode != 4 ||
		got.End.Failure == nil || string(got.End.Failure.Body) != "last words\n" {
		t.Fatalf("Recover = %+v, %v; want a failed end with its transcript", got, err)
	}
}

func TestCordonPortError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		err     error
		is      error
		refusal bool
	}{
		{"detached", fmt.Errorf("%w: run", cordon.ErrDetached), stage.ErrCordonDetached, false},
		{"end unproven", &cordon.BlockedError{RunID: "r", Err: cordon.ErrRunEndUnproven}, stage.ErrCordonEndUnproven, false},
		{"no run", fmt.Errorf("%w: r", cordon.ErrNoRun), stage.ErrCordonNoRun, false},
		{"scan", &cordon.ScanRefusalError{Reason: "github_token in candidate.txt"}, ward.ErrOutputScanRefused, true},
		{"worktree replaced", cordon.ErrWorktreeReplaced, cordon.ErrWorktreeReplaced, true},
		{"not exportable", fmt.Errorf("%w: r stalled", cordon.ErrNotExportable), cordon.ErrNotExportable, true},
		{"canceled", fmt.Errorf("%w: %w: r", cordon.ErrCanceled, cordon.ErrNotExportable), stage.ErrCordonCanceled, false},
		{"already exported", cordon.ErrAlreadyExported, cordon.ErrAlreadyExported, false},
		{"run ended", fmt.Errorf("%w: r", cordon.ErrRunEnded), stage.ErrCordonRunEnded, false},
		{"input closed", fmt.Errorf("%w: r", cordon.ErrInputClosed), stage.ErrCordonInputClosed, false},
		{"not streamed", fmt.Errorf("%w: r", cordon.ErrNotStreamed), stage.ErrCordonNotStreamed, false},
		{"not taken", fmt.Errorf("%w: r", cordon.ErrNotTaken), stage.ErrCordonNotTaken, false},
		{"not started", fmt.Errorf("%w: r", cordon.ErrNotStarted), stage.ErrCordonNotStarted, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := cordonPortError(tc.err)
			var refusal *stage.CordonExportRefusal
			if !errors.Is(err, tc.is) || errors.As(err, &refusal) != tc.refusal {
				t.Fatalf("cordonPortError(%v) = %v; want %v, refusal %v", tc.err, err, tc.is, tc.refusal)
			}
		})
	}
	backend := newFakeCordonLauncher()
	backend.err = fmt.Errorf("%w: dir", cordon.ErrExportUnauthenticated)
	gate, _ := newTestCordonGate(t, backend, false)
	if err := gate.AuthenticateReleasedExport(context.Background(), "r", "/x"); !errors.Is(err, stage.ErrUnsupportedStart) {
		t.Fatalf("AuthenticateReleasedExport = %v, want a refusal that never retries", err)
	}
}

func ptr(s string) *string { return &s }

func writeFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
