package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// slowCloser stands in for a credential-bearing session teardown that is
// still working, and ignores the context it was given. A cooperative closer
// would prove nothing here: only one that refuses to stop on its own shows
// whether Close waits for the lease or for a clock.
type slowCloser struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *slowCloser) Close(context.Context) error {
	c.once.Do(func() { close(c.entered) })
	<-c.release
	return nil
}

func newSlowCloser() *slowCloser {
	return &slowCloser{entered: make(chan struct{}), release: make(chan struct{})}
}

func newTeardownDaemon(t *testing.T, closer sessionCloser) *daemon {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	return &daemon{
		store:         st,
		server:        &http.Server{ReadHeaderTimeout: time.Second},
		cancel:        func() {},
		sessionCloser: closer,
	}
}

// TestCloseWaitsOutTheCredentialBearingTeardown pins plan §5.2 against the
// obvious-looking fix. Bounding this phase is what the stop-wait fork
// explicitly rejected: any finite grace recreates SIGKILL-mid-lease, and a
// bounded credential-safe teardown is deferred hardening rather than a
// tunable. So Close must gate on the teardown finishing, never on a clock.
//
// The two halves matter together. Blocking alone could just be a long
// budget, and returning alone could be a timeout; gating completion on the
// release proves the wait tracks the lease.
func TestCloseWaitsOutTheCredentialBearingTeardown(t *testing.T) {
	closer := newSlowCloser()
	d := newTeardownDaemon(t, closer)

	done := make(chan error, 1)
	go func() { done <- d.Close() }()
	<-closer.entered
	select {
	case err := <-done:
		t.Fatalf("Close returned (%v) while the session teardown was still running; "+
			"plan §5.2 forbids a finite grace on a credential-bearing lease", err)
	case <-time.After(2 * time.Second):
		// Still waiting, as required. This cannot prove the wait is
		// unbounded, only that no short grace abandons the lease, which is
		// the shape a reintroduced budget would take.
	}

	close(closer.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Close did not return after the session teardown finished")
	}
}

// TestClosePropagatesATeardownFailure keeps the wait from swallowing what
// it exists to surface: a refused lease release is the daemon's problem to
// report, not to absorb into a clean exit.
func TestClosePropagatesATeardownFailure(t *testing.T) {
	sentinel := errors.New("lease release refused")
	d := newTeardownDaemon(t, closerFunc(func(context.Context) error { return sentinel }))
	if err := d.Close(); !errors.Is(err, sentinel) {
		t.Fatalf("Close err = %v, want the closer's own error", err)
	}
}

type closerFunc func(context.Context) error

func (f closerFunc) Close(ctx context.Context) error { return f(ctx) }

const (
	wedgedTeardownEnv    = "FREESIDE_WEDGED_TEARDOWN_HELPER"
	teardownReadyMarker  = "freeside-test: signals armed"
	teardownWedgedMarker = "freeside-test: teardown wedged"
	teardownDoneMarker   = "freeside-test: teardown returned"
)

// TestASecondSignalTerminatesAWedgedTeardown pins the sequencing that no
// in-process test can reach: signal disposition itself. While
// signal.NotifyContext is still installed it absorbs SIGTERM, so during a
// slow teardown a supervisor's escalation does nothing and the only thing
// left that stops the daemon is SIGKILL. The helper process runs the real
// serve sequence against a teardown that never finishes; moving stop()
// back after Close makes the second signal a no-op and hangs this test.
func TestASecondSignalTerminatesAWedgedTeardown(t *testing.T) {
	if os.Getenv(wedgedTeardownEnv) == "1" {
		runWedgedTeardownHelper(t)
		return
	}

	cmd := exec.Command( //nolint:gosec // G204: the test reexecutes its own binary with a fixed -test.run filter.
		os.Args[0], "-test.run=^TestASecondSignalTerminatesAWedgedTeardown$",
	)
	cmd.Env = append(os.Environ(), wedgedTeardownEnv+"=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	lines := make(chan string, 16)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	awaitMarker(t, lines, teardownReadyMarker)
	signalHelper(t, cmd, "first")
	awaitMarker(t, lines, teardownWedgedMarker)
	// Disposition is restored before the teardown the marker announces, so
	// this signal reaches the default handler rather than the context.
	signalHelper(t, cmd, "second")

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case <-waited:
	case <-time.After(30 * time.Second):
		t.Fatal("helper survived a second SIGTERM during teardown; the signal was swallowed")
	}
	if cmd.ProcessState.Success() {
		t.Fatalf("helper exited cleanly (%v); a signalled stop must not report success", cmd.ProcessState)
	}
}

func runWedgedTeardownHelper(t *testing.T) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	d := newTeardownDaemon(t, closerFunc(func(context.Context) error {
		fmt.Println(teardownWedgedMarker)
		select {}
	}))
	fmt.Println(teardownReadyMarker)
	_ = serve(ctx, stop, d)
	// Only reachable if the wedged teardown somehow returned, which would
	// mean the parent's second signal proved nothing.
	fmt.Println(teardownDoneMarker)
}

func awaitMarker(t *testing.T, lines <-chan string, marker string) {
	t.Helper()
	deadline := time.After(60 * time.Second)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("helper output ended before %q", marker)
			}
			if strings.Contains(line, teardownDoneMarker) {
				t.Fatalf("helper completed teardown on its own while waiting for %q", marker)
			}
			if strings.Contains(line, marker) {
				return
			}
		case <-deadline:
			t.Fatalf("helper never printed %q", marker)
		}
	}
}

func signalHelper(t *testing.T, cmd *exec.Cmd, which string) {
	t.Helper()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send %s SIGTERM: %v", which, err)
	}
}
