package cordonhost

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/cordon"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// shortDir is a directory short enough to hold a Unix socket, which
// t.TempDir's paths can outgrow. The cleanup removes only the directory
// MkdirTemp created.
func shortDir(t *testing.T) string {
	t.Helper()
	created, err := os.MkdirTemp("", "ch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(created) })
	dir, err := filepath.EvalSymlinks(created)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// fakeServed stands in for a backend and its suite.
type fakeServed struct {
	digest  domain.Digest
	launch  func(context.Context, cordon.LaunchRequest) (cordon.LaunchResult, error)
	recover func(context.Context, cordon.RecoverRequest) (cordon.RecoveryResult, error)
	full    func(context.Context) error

	mu       sync.Mutex
	launches int
	detached bool
}

func (f *fakeServed) Launch(ctx context.Context, req cordon.LaunchRequest) (cordon.LaunchResult, error) {
	f.mu.Lock()
	f.launches++
	f.mu.Unlock()
	return f.launch(ctx, req)
}

func (f *fakeServed) Started(context.Context, string) (bool, error)     { return true, nil }
func (f *fakeServed) RequestCancellation(context.Context, string) error { return nil }
func (f *fakeServed) Recover(ctx context.Context, req cordon.RecoverRequest) (cordon.RecoveryResult, error) {
	return f.recover(ctx, req)
}
func (f *fakeServed) ReleaseFinished(context.Context, []string) error { return nil }
func (f *fakeServed) Detach() {
	f.mu.Lock()
	f.detached = true
	f.mu.Unlock()
}

func (f *fakeServed) HostFacts() (cordon.HostFacts, error) {
	return cordon.HostFacts{DesktopSession: true}, nil
}
func (f *fakeServed) ConfigurationDigest() domain.Digest   { return f.digest }
func (f *fakeServed) Full(ctx context.Context) error       { return f.full(ctx) }
func (f *fakeServed) ProbeBeforeJob(context.Context) error { return nil }

type countingSeeder struct {
	mu    sync.Mutex
	calls int
}

func (s *countingSeeder) FetchBaseWorktree(_ context.Context, _, _, _, dir string) error {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "README"), []byte("the base\n"), 0o600)
}

type collectingRecorder struct {
	mu      sync.Mutex
	records []domain.BackendConformance
	err     error
}

func (r *collectingRecorder) RecordBackendConformance(_ context.Context, record domain.BackendConformance) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.records = append(r.records, record)
	return nil
}

type testLauncher struct {
	server *Server
	socket string
	seeds  string
	log    *syncBuffer
	mu     sync.Mutex
	seen   []kind
}

func (s *testLauncher) kinds() []kind {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.seen)
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func serve(t *testing.T, build string, backend *fakeServed) *testLauncher {
	t.Helper()
	dir := shortDir(t)
	s := &testLauncher{socket: filepath.Join(dir, "l.sock"), seeds: filepath.Join(dir, "seeds"), log: &syncBuffer{}}
	server, err := newServer(build, backend, Account{Name: "run", UID: 501}, s.seeds, slog.New(slog.NewTextHandler(s.log, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server.awake.prevent = func() (func() error, error) { return func() error { return nil }, nil }
	server.onFrame = func(k kind) {
		s.mu.Lock()
		s.seen = append(s.seen, k)
		s.mu.Unlock()
	}
	ln, err := net.Listen("unix", s.socket)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(ln) }()
	t.Cleanup(server.Close)
	s.server = server
	return s
}

func newTestClient(t *testing.T, socket, build string, seeder cordon.Seeder, rec cordon.ConformanceRecorder) *Client {
	t.Helper()
	c, err := NewClient(ClientConfig{Socket: socket, Build: build, Seeder: seeder, Recorder: rec, ScratchRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Detach)
	return c
}

// fixtureToken is a setup token's shape, split so no tracked line matches
// the credential rules (scripts/check-credential-literals.sh).
const fixtureToken = "sk-ant-" + "oat01-cordonhost-fixture-0123456789abcdefghijklmnopqrstuvwxyz"

func launchRequest(t *testing.T, runID string) cordon.LaunchRequest {
	t.Helper()
	token, err := ward.NewSetupToken([]byte(fixtureToken))
	if err != nil {
		t.Fatal(err)
	}
	return cordon.LaunchRequest{
		RunID: runID, Repository: "owner/repo", BaseRef: "main", BaseSHA: "0123456789abcdef0123456789abcdef01234567",
		Prompt: []byte("the prompt"), Args: []string{"-p"}, Token: token,
	}
}

func digestOf(s string) domain.Digest { return domain.Digest(contentaddr.Sum([]byte(s))) }

// TestBuildsRefuseEachOtherAtTheHello shows a client and a launcher of
// different builds each refuse the other before a launch frame, and so the
// token, crosses, and before the client seeds anything.
func TestBuildsRefuseEachOtherAtTheHello(t *testing.T) {
	t.Parallel()
	backend := &fakeServed{digest: digestOf("config")}
	s := serve(t, "build-b", backend)
	seeder := &countingSeeder{}
	c := newTestClient(t, s.socket, "build-a", seeder, &collectingRecorder{})
	if _, err := c.Launch(t.Context(), launchRequest(t, "run-1")); !errors.Is(err, ErrBuildMismatch) {
		t.Fatalf("Launch = %v, want ErrBuildMismatch", err)
	}
	if got := s.kinds(); !slices.Equal(got, []kind{kindHello}) {
		t.Fatalf("the launcher read %v, want the hello alone", got)
	}
	if backend.launches != 0 || seeder.calls != 0 {
		t.Fatalf("launched %d and seeded %d times", backend.launches, seeder.calls)
	}

	// A launcher of another build that answers anyway: the client sends
	// nothing after the hellos.
	dir := shortDir(t)
	socket := filepath.Join(dir, "raw.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	next := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			next <- err
			return
		}
		defer func() { _ = conn.Close() }()
		if _, err := readFrame(conn, true); err != nil {
			next <- err
			return
		}
		_ = writeFrame(conn, frame{Kind: kindHello, Hello: &helloBody{
			Build: "build-b", ConfigurationDigest: digestOf("config"), Account: &Account{Name: "run", UID: 501},
		}})
		_, err = readFrame(conn, false)
		next <- err
	}()
	c = newTestClient(t, socket, "build-a", seeder, &collectingRecorder{})
	if _, err := c.Launch(t.Context(), launchRequest(t, "run-1")); !errors.Is(err, ErrBuildMismatch) {
		t.Fatalf("Launch against another build's hello = %v, want ErrBuildMismatch", err)
	}
	if err := <-next; err == nil {
		t.Fatal("the client sent a frame after refusing the launcher's hello")
	}
	if seeder.calls != 0 {
		t.Fatalf("seeded %d times", seeder.calls)
	}
}

// TestServedLaunchSeedsFromTheConnection shows the launch the backend gets
// seeds from what the client streamed, carries the token, and returns a
// failed end's transcript to the client; the staged seed is gone after,
// and the launcher's log holds no token.
func TestServedLaunchSeedsFromTheConnection(t *testing.T) {
	t.Parallel()
	runs := t.TempDir()
	backend := &fakeServed{digest: digestOf("config")}
	backend.launch = func(ctx context.Context, req cordon.LaunchRequest) (cordon.LaunchResult, error) {
		if string(req.Token.Reveal()) != fixtureToken {
			return cordon.LaunchResult{}, errors.New("the token did not arrive")
		}
		worktree := filepath.Join(runs, req.RunID)
		if err := req.Seeder.FetchBaseWorktree(ctx, req.Repository, req.BaseRef, req.BaseSHA, worktree); err != nil {
			return cordon.LaunchResult{}, err
		}
		if got, err := os.ReadFile(filepath.Join(worktree, "README")); err != nil || string(got) != "the base\n" { //nolint:gosec // G304: the test's own worktree
			return cordon.LaunchResult{}, fmt.Errorf("the seed: %q, %w", got, err)
		}
		transcript := filepath.Join(runs, "transcript")
		if err := os.WriteFile(transcript, []byte("it failed\n"), 0o600); err != nil {
			return cordon.LaunchResult{}, err
		}
		return cordon.LaunchResult{
			ExitCode: 3, EndedBy: cordon.EndedByExited, TranscriptPath: transcript,
			StderrPath: filepath.Join(runs, "absent"),
		}, nil
	}
	s := serve(t, "build", backend)
	c := newTestClient(t, s.socket, "build", &countingSeeder{}, &collectingRecorder{})
	res, err := c.Launch(t.Context(), launchRequest(t, "run-1"))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if res.EndedBy != cordon.EndedByExited || res.ExitCode != 3 {
		t.Fatalf("result %+v", res)
	}
	if got, err := os.ReadFile(res.TranscriptPath); err != nil || string(got) != "it failed\n" {
		t.Fatalf("transcript %q, %v", got, err)
	}
	if _, err := os.Lstat(res.StderrPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an absent stderr arrived: %v", err)
	}
	if entries, err := os.ReadDir(s.seeds); err != nil || len(entries) != 0 {
		t.Fatalf("the seed root keeps %v, %v", entries, err)
	}
	if strings.Contains(s.log.String(), fixtureToken) {
		t.Fatal("the launcher's log holds the token")
	}
	// Releasing the run removes the client's copy.
	if err := c.ReleaseFinished(t.Context(), []string{"run-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Dir(res.TranscriptPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the released run's transcript stayed: %v", err)
	}
}

// fifoSeeder seeds a tree holding a named pipe, which no seed carries.
type fifoSeeder struct{}

func (fifoSeeder) FetchBaseWorktree(_ context.Context, _, _, _, dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("the base\n"), 0o600); err != nil {
		return err
	}
	return syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o600)
}

// TestALaunchWhoseSeedFailsToSendEnds shows a seed the client can't finish
// sending ends the launch at once, with no launch on the launcher and no
// seed left staged, instead of each side waiting on the other.
func TestALaunchWhoseSeedFailsToSendEnds(t *testing.T) {
	t.Parallel()
	backend := &fakeServed{digest: digestOf("config")}
	backend.launch = func(context.Context, cordon.LaunchRequest) (cordon.LaunchResult, error) {
		return cordon.LaunchResult{}, errors.New("a launch whose seed never arrived started")
	}
	s := serve(t, "build", backend)
	c := newTestClient(t, s.socket, "build", fifoSeeder{}, &collectingRecorder{})
	done := make(chan error, 1)
	go func() {
		_, err := c.Launch(t.Context(), launchRequest(t, "run-1"))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || errors.Is(err, cordon.ErrDetached) || !strings.Contains(err.Error(), "seed") {
			t.Fatalf("Launch = %v, want the seed's refusal", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Launch still waits after its seed failed to send")
	}
	if entries, err := os.ReadDir(s.seeds); err != nil || len(entries) != 0 {
		t.Fatalf("the seed root keeps %v, %v", entries, err)
	}
}

// TestALaunchEndsOnlyOnItsCancellation shows a client that detaches leaves
// its launch running on the launcher, and one whose context ends cancels
// it.
func TestALaunchEndsOnlyOnItsCancellation(t *testing.T) {
	t.Parallel()
	started := make(chan *connection, 1)
	release := make(chan struct{})
	backend := &fakeServed{digest: digestOf("config")}
	canceledEarly := make(chan bool, 1)
	backend.launch = func(ctx context.Context, req cordon.LaunchRequest) (cordon.LaunchResult, error) {
		conn, _ := ctx.Value(connKey{}).(*connection)
		started <- conn
		if req.RunID == "run-canceled" {
			<-ctx.Done()
			return cordon.LaunchResult{EndedBy: cordon.EndedByCanceled, ExitCode: -1}, nil
		}
		select {
		case <-release:
			canceledEarly <- ctx.Err() != nil
			return cordon.LaunchResult{EndedBy: cordon.EndedByExited}, nil
		case <-ctx.Done():
			return cordon.LaunchResult{EndedBy: cordon.EndedByCanceled, ExitCode: -1}, nil
		}
	}
	s := serve(t, "build", backend)

	c := newTestClient(t, s.socket, "build", &countingSeeder{}, &collectingRecorder{})
	done := make(chan error, 1)
	go func() {
		_, err := c.Launch(t.Context(), launchRequest(t, "run-detached"))
		done <- err
	}()
	conn := <-started
	c.Detach()
	if err := <-done; !errors.Is(err, cordon.ErrDetached) {
		t.Fatalf("a detached Launch = %v, want ErrDetached", err)
	}
	<-conn.gone
	close(release)
	if <-canceledEarly {
		t.Fatal("a client that detached canceled its launch")
	}

	c = newTestClient(t, s.socket, "build", &countingSeeder{}, &collectingRecorder{})
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		res, err := c.Launch(ctx, launchRequest(t, "run-canceled"))
		if err == nil && res.EndedBy != cordon.EndedByCanceled {
			err = fmt.Errorf("ended %s", res.EndedBy)
		}
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("a canceled Launch: %v", err)
	}
}

// TestRecordsCrossToTheClientsRecorder shows each record the launcher's
// suite writes reaches the client's recorder, and one the client refuses,
// bound to another digest or failing in its recorder, fails the host's
// record step.
func TestRecordsCrossToTheClientsRecorder(t *testing.T) {
	t.Parallel()
	digest := digestOf("config")
	var boundTo domain.Digest
	backend := &fakeServed{digest: digest}
	backend.full = func(ctx context.Context) error {
		for _, outcome := range []domain.ConformanceOutcome{domain.ConformanceSuperseded, domain.ConformancePassed} {
			in := domain.BackendConformanceInput{
				Backend: domain.BackendCordonedHostProcess, Outcome: outcome, ConfigurationDigest: boundTo, ProvedAt: time.Now(),
			}
			if outcome == domain.ConformancePassed {
				in.Capabilities = domain.CordonedCapabilities()
			}
			record, err := domain.NewBackendConformance(in)
			if err != nil {
				return err
			}
			if err := ConnectionRecorder().RecordBackendConformance(ctx, record); err != nil {
				return fmt.Errorf("%w: %w", cordon.ErrConformanceRecord, err)
			}
		}
		return nil
	}
	s := serve(t, "build", backend)

	boundTo = digest
	rec := &collectingRecorder{}
	if err := newTestClient(t, s.socket, "build", &countingSeeder{}, rec).Full(t.Context()); err != nil {
		t.Fatalf("Full: %v", err)
	}
	if len(rec.records) != 2 || rec.records[1].Outcome != domain.ConformancePassed || rec.records[1].ConfigurationDigest != digest {
		t.Fatalf("recorded %+v", rec.records)
	}

	boundTo = digestOf("another")
	rec = &collectingRecorder{}
	if err := newTestClient(t, s.socket, "build", &countingSeeder{}, rec).Full(t.Context()); !errors.Is(err, cordon.ErrConformanceRecord) {
		t.Fatalf("Full with another digest's record = %v, want ErrConformanceRecord", err)
	}
	if len(rec.records) != 0 {
		t.Fatalf("recorded another digest's record: %+v", rec.records)
	}

	boundTo = digest
	rec = &collectingRecorder{err: errors.New("the store is gone")}
	err := newTestClient(t, s.socket, "build", &countingSeeder{}, rec).Full(t.Context())
	if !errors.Is(err, cordon.ErrConformanceRecord) || !strings.Contains(err.Error(), "the store is gone") {
		t.Fatalf("Full with a failing recorder = %v", err)
	}
}

// TestRecorderGivesUpOnASilentClient shows a record a client never
// answers fails after recordTimeout, so it can't hold the suite's guard.
func TestRecorderGivesUpOnASilentClient(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		server, client := net.Pipe()
		defer func() { _ = client.Close() }()
		defer func() { _ = server.Close() }()
		go func() {
			for {
				if _, err := readFrame(client, false); err != nil {
					return
				}
			}
		}()
		c := &connection{conn: server, acks: make(chan recordedBody, 1), gone: make(chan struct{})}
		record, err := domain.NewBackendConformance(domain.BackendConformanceInput{
			Backend: domain.BackendCordonedHostProcess, Outcome: domain.ConformanceFailed,
			ConfigurationDigest: digestOf("config"), ProvedAt: time.Now(),
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.WithValue(t.Context(), connKey{}, c)
		if err := ConnectionRecorder().RecordBackendConformance(ctx, record); err == nil || !strings.Contains(err.Error(), "in time") {
			t.Fatalf("a silent client's record = %v", err)
		}
	})
}

// TestRecoverCarriesAFailedEndAndRefusesAnExport shows a recovered failure
// returns its transcript, and a recovery that would export, an export, and
// an authentication are refused with ErrExportUnsupported.
func TestRecoverCarriesAFailedEndAndRefusesAnExport(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stderr := filepath.Join(dir, "stderr")
	if err := os.WriteFile(stderr, []byte("stalled\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	backend := &fakeServed{digest: digestOf("config")}
	backend.recover = func(_ context.Context, req cordon.RecoverRequest) (cordon.RecoveryResult, error) {
		switch req.RunID {
		case "run-clean":
			return cordon.RecoveryResult{Outcome: cordon.RecoveryExported, EndedBy: cordon.EndedByExited}, nil
		case "run-scanned":
			return cordon.RecoveryResult{}, &cordon.ScanRefusalError{Reason: "rule x matched item y"}
		case "run-canceled":
			return cordon.RecoveryResult{}, fmt.Errorf("%w: %w: run-canceled", cordon.ErrCanceled, cordon.ErrNotExportable)
		}
		return cordon.RecoveryResult{
			Outcome: cordon.RecoveryFailed, EndedBy: cordon.EndedByStalled, ExitCode: -1,
			TranscriptPath: filepath.Join(dir, "absent"), StderrPath: stderr,
		}, nil
	}
	s := serve(t, "build", backend)
	c := newTestClient(t, s.socket, "build", &countingSeeder{}, &collectingRecorder{})
	res, err := c.Recover(t.Context(), cordon.RecoverRequest{RunID: "run-1", Export: cordon.ExportRequest{RunID: "run-1"}})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if res.Outcome != cordon.RecoveryFailed || res.EndedBy != cordon.EndedByStalled {
		t.Fatalf("recovered %+v", res)
	}
	if got, err := os.ReadFile(res.StderrPath); err != nil || string(got) != "stalled\n" {
		t.Fatalf("stderr %q, %v", got, err)
	}
	if _, err := c.Recover(t.Context(), cordon.RecoverRequest{RunID: "run-clean", Export: cordon.ExportRequest{RunID: "run-clean"}}); !errors.Is(err, ErrExportUnsupported) {
		t.Fatalf("a clean recovery = %v, want ErrExportUnsupported", err)
	}
	if _, err := c.Recover(t.Context(), cordon.RecoverRequest{RunID: "run-scanned", Export: cordon.ExportRequest{RunID: "run-scanned"}}); !errors.Is(err, ErrExportUnsupported) {
		t.Fatalf("a recovery whose export the scan refused = %v, want ErrExportUnsupported", err)
	}
	// A run canceled between its end record's read and its export ended
	// canceled; it is not an export the launcher refused.
	_, err = c.Recover(t.Context(), cordon.RecoverRequest{RunID: "run-canceled", Export: cordon.ExportRequest{RunID: "run-canceled"}})
	if !errors.Is(err, cordon.ErrCanceled) || errors.Is(err, ErrExportUnsupported) {
		t.Fatalf("a recovery canceled before its export = %v, want ErrCanceled alone", err)
	}
	if _, err := c.Export(t.Context(), cordon.ExportRequest{RunID: "run-1"}); !errors.Is(err, ErrExportUnsupported) {
		t.Fatalf("Export = %v, want ErrExportUnsupported", err)
	}
	if err := c.AuthenticateReleasedExport(t.Context(), "run-1", "/x"); !errors.Is(err, ErrExportUnsupported) {
		t.Fatalf("AuthenticateReleasedExport = %v, want ErrExportUnsupported", err)
	}
}

// TestNewServerRemovesOnlyStaleSeeds shows a restarted launcher removes the
// seeds an earlier one staged, which only it names, and nothing else.
func TestNewServerRemovesOnlyStaleSeeds(t *testing.T) {
	t.Parallel()
	seeds := t.TempDir()
	for _, name := range []string{"run-stale", ".keep"} {
		if err := os.Mkdir(filepath.Join(seeds, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := newServer("build", &fakeServed{}, Account{Name: "run"}, seeds, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(seeds)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != ".keep" {
		t.Fatalf("the seed root holds %v", entries)
	}
}

func TestAwakeHolderHoldsWhileAnyWorkRuns(t *testing.T) {
	t.Parallel()
	var taken, released int
	a := &awakeHolder{logger: slog.New(slog.DiscardHandler), prevent: func() (func() error, error) {
		taken++
		return func() error { released++; return nil }, nil
	}}
	first := a.hold()
	second := a.hold()
	first()
	first()
	if taken != 1 || released != 0 {
		t.Fatalf("taken %d, released %d with one hold left", taken, released)
	}
	second()
	if released != 1 {
		t.Fatalf("released %d once nothing held it", released)
	}
	a.hold()()
	if taken != 2 || released != 2 {
		t.Fatalf("taken %d, released %d after a second round", taken, released)
	}
}

// TestClientRefusesAMessageAsNotStreamed: until the protocol carries a
// message (#3702), the client refuses one without dialing, as a refusal
// the gate settles not_streamed rather than an error it would retry.
func TestClientRefusesAMessageAsNotStreamed(t *testing.T) {
	t.Parallel()
	err := (&Client{}).SendMessage(t.Context(), "run-1", 1, "cmd-1", "steer")
	if !errors.Is(err, ErrMessageUnsupported) || !errors.Is(err, cordon.ErrNotStreamed) {
		t.Fatalf("SendMessage = %v, want ErrMessageUnsupported wrapping cordon.ErrNotStreamed", err)
	}
}
