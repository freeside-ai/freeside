package daemonshell

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// Serve runs the daemon until its context ends or a component fails, then
// tears it down.
//
// stop runs before teardown, not after. Until signal disposition is
// restored, signal.NotifyContext keeps absorbing SIGTERM, so an operator
// signalling during a slow teardown gets nothing: the second signal is
// swallowed, and the only thing left that ends the process is a SIGKILL
// landing mid-teardown, stranding the containers it was reaping. Once stop
// has run, a second SIGTERM terminates the process on the spot.
//
// This is the escape hatch that plan §5.2's unbounded credential-lease
// teardown needs, and the reason it is not a finite grace in disguise:
// nothing abandons the lease on a timer, and abandoning it is a human
// decision, taken deliberately, with the process still reporting what it
// was doing.
func Serve(ctx context.Context, stop func(), h *Shell) error {
	waitErr := h.Wait(ctx)
	stop()
	return errors.Join(waitErr, h.Close())
}

type Readiness struct {
	APIURL      string `json:"api_url"`
	PairingCode string `json:"pairing_code"`
	Environment string `json:"environment"`
	// RunID names one daemon process start, not a workflow run (domain.RunID).
	// It lets a script tell a fresh start from a stale file.
	RunID string `json:"run_id"`
}

// Shell runs the process run() composes: it serves the API and pairing
// control, tracks the long-running goroutines, supervises their exits,
// publishes readiness, and tears the process down.
type Shell struct {
	lock          *daemonlock.Lock
	store         *store.Store
	listener      net.Listener
	loopbackTwin  net.Listener
	server        *http.Server
	pairing       PairingControl
	cancel        context.CancelFunc
	sessionCloser SessionCloser
	errs          chan error
	wg            sync.WaitGroup
	closeOnce     sync.Once
	closeErr      error
	pairingCode   string
	environment   string
	runID         string
	logger        *slog.Logger
	now           func() time.Time
}

// PairingControl is the control socket the shell serves and closes. Its
// configuration, routes, and publication stay with the composition.
type PairingControl interface {
	Serve() error
	Close() error
}

// SessionCloser tears down the credential-bearing sessions. Close waits for
// it without a deadline (see Shell.Close).
type SessionCloser interface {
	Close(context.Context) error
}

// Parts are what the composition hands the shell. Pairing and SessionCloser
// may be nil: a shell without them serves and closes neither.
type Parts struct {
	Lock          *daemonlock.Lock
	Store         *store.Store
	Listener      net.Listener
	LoopbackTwin  net.Listener
	Server        *http.Server
	Pairing       PairingControl
	SessionCloser SessionCloser
	Cancel        context.CancelFunc
	Logger        *slog.Logger
	Now           func() time.Time
}

// New builds a shell from the composition's parts. It starts nothing.
func New(parts Parts) *Shell {
	return &Shell{
		lock:          parts.Lock,
		store:         parts.Store,
		listener:      parts.Listener,
		loopbackTwin:  parts.LoopbackTwin,
		server:        parts.Server,
		pairing:       parts.Pairing,
		sessionCloser: parts.SessionCloser,
		cancel:        parts.Cancel,
		errs:          make(chan error, 1),
		logger:        parts.Logger,
		now:           parts.Now,
	}
}

func (s *Shell) Readiness() Readiness {
	return Readiness{
		APIURL:      "http://" + s.listener.Addr().String(),
		PairingCode: s.pairingCode,
		Environment: s.environment,
		RunID:       s.runID,
	}
}

// newDaemonRunID mints the per-start readiness run id: 16 random bytes,
// hex-encoded.
func newDaemonRunID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("mint daemon run id: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func (s *Shell) sameHostReadiness() Readiness {
	ready := s.Readiness()
	if s.loopbackTwin != nil {
		ready.APIURL = "http://" + s.loopbackTwin.Addr().String()
	}
	return ready
}

// PublishReadiness mints this start's run ID, records the pairing code and
// environment it reports, and writes the readiness file under stateDir.
// The file's same-host app reader uses loopback; stdout and pairing keep the
// primary URL for remote devices (plan §5.2 Reachability).
func (s *Shell) PublishReadiness(stateDir, pairingCode, environment string) error {
	s.pairingCode = pairingCode
	runID, err := newDaemonRunID()
	if err != nil {
		return err
	}
	s.environment = environment
	s.runID = runID
	return publishReadiness(stateDir, s.sameHostReadiness())
}

// Go runs fn on its own goroutine, which Close and AbortStart wait for.
func (s *Shell) Go(fn func()) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		fn()
	}()
}

// StartServers serves the API on the primary listener and its loopback
// twin, and pairing control when the shell has it. A server that stops
// before ctx ends reports through ComponentExited.
func (s *Shell) StartServers(parent, ctx context.Context) {
	s.Go(func() {
		err := s.server.Serve(s.listener)
		if errors.Is(err, http.ErrServerClosed) {
			return
		}
		s.ComponentExited(parent, ctx, ComponentHTTP, err)
	})
	if s.loopbackTwin != nil {
		// The same s.server serves both listeners, so s.server.Shutdown closes
		// this one too; its exit is the same ComponentHTTP failure as the primary.
		s.Go(func() {
			err := s.server.Serve(s.loopbackTwin)
			if errors.Is(err, http.ErrServerClosed) {
				return
			}
			s.ComponentExited(parent, ctx, ComponentHTTP, err)
		})
	}
	if s.pairing != nil {
		s.Go(func() {
			if err := s.pairing.Serve(); !errors.Is(err, http.ErrServerClosed) {
				s.ComponentExited(parent, ctx, ComponentPairingControl, err)
			}
		})
	}
}

// AbortStart stops a start that failed after its goroutines began: it
// cancels their context, closes the servers, and waits for every goroutine
// Go started. The store and lock stay with the caller's own cleanup.
func (s *Shell) AbortStart() {
	s.cancel()
	_ = s.server.Close()
	_ = s.closePairing()
	s.wg.Wait()
}

// Wait returns when the process context is canceled or any long-running
// component exits. A nil component result is normal only during shutdown.
func (s *Shell) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return nil
	case err := <-s.errs:
		return err
	}
}

// Close tears the daemon down. It deliberately imposes no deadline on the
// credential-bearing session teardown: plan §5.2 closes the stop-wait fork
// on the unlimited side, because "any finite grace recreates
// SIGKILL-mid-lease, and a bounded credential-safe teardown is deferred
// hardening, not a tunable". The supervisor's exit timeout is effectively
// unlimited to match, so waiting here is the contract, not a hazard.
//
// What makes that wait safe is that it can no longer be indefinite by
// accident. The subprocess sites it runs through are bounded individually
// (internal/procbound), so a teardown that is still going is doing work
// rather than blocked on a pipe a descendant never closed; and serve
// restores signal disposition before calling this, so an operator who
// decides to abandon a lease can, with a second SIGTERM. That is a human
// choosing to, which is the distinction §5.2 draws.
func (s *Shell) Close() error {
	s.closeOnce.Do(func() {
		s.cancel()
		var driverErr error
		if s.sessionCloser != nil {
			// A credential-bearing external writer is more important than
			// bounded process-exit latency. Ward owns bounded teardown; do not
			// close its store or exit while a session still uses its lease.
			driverErr = s.sessionCloser.Close(context.Background())
		}
		ctx, cancel := context.WithTimeout(context.Background(), ServerShutdownBudget)
		defer cancel()
		shutdownErr := s.server.Shutdown(ctx)
		pairingErr := s.closePairing()
		s.wg.Wait()
		s.closeErr = errors.Join(driverErr, shutdownErr, pairingErr, s.store.Close(), s.lock.Close())
	})
	return s.closeErr
}

func (s *Shell) closePairing() error {
	if s.pairing == nil {
		return nil
	}
	return s.pairing.Close()
}

// ServerShutdownBudget bounds draining in-flight HTTP requests. Unlike the
// session teardown above, this phase holds no credential lease: the API is
// a read/write surface over the store, and abandoning a slow request loses
// nothing durable.
const ServerShutdownBudget = 5 * time.Second
