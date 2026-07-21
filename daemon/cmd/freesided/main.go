// Command freesided is the Freeside daemon. The Phase 1A.0 composition serves
// signet on loopback and drives the workflow engine with the permanent fake
// StageDriver. Later Wave 2 units replace the driver and add operational
// surfaces without changing the engine's durable reconciliation loop.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const defaultReconcileInterval = 100 * time.Millisecond

const defaultNtfyURL = "https://ntfy.sh"

const (
	defaultFakeRunID     domain.RunID     = "run-walking-skeleton"
	defaultFakeProjectID domain.ProjectID = "project-walking-skeleton"
)

func main() {
	flags := flag.NewFlagSet("freesided", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	dbPath := flags.String("db", "", "SQLite database path (required; created if absent)")
	driverDir := flags.String("fake-driver-dir", "", "permanent fake StageDriver state directory (defaults beside -db)")
	listenAddr := flags.String("listen", "127.0.0.1:0", "signet listener address (loopback only)")
	ntfyURL := flags.String("ntfy-url", defaultNtfyURL, "ntfy server URL for device notifications")
	interval := flags.Duration("reconcile-interval", defaultReconcileInterval, "workflow reconciliation interval")
	if err := flags.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	h, err := run(ctx, config{
		DBPath: *dbPath, FakeDriverDir: *driverDir,
		ListenAddr: *listenAddr, NtfyURL: *ntfyURL, ReconcileInterval: *interval,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "freesided:", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(h.readiness()); err != nil {
		_ = h.Close()
		fmt.Fprintln(os.Stderr, "freesided:", err)
		os.Exit(1)
	}

	waitErr := h.Wait(ctx)
	closeErr := h.Close()
	if err := errors.Join(waitErr, closeErr); err != nil {
		fmt.Fprintln(os.Stderr, "freesided:", err)
		os.Exit(1)
	}
}

type config struct {
	DBPath            string
	FakeDriverDir     string
	ListenAddr        string
	NtfyURL           string
	ReconcileInterval time.Duration
}

type readiness struct {
	APIURL      string `json:"api_url"`
	PairingCode string `json:"pairing_code"`
}

type daemon struct {
	store       *store.Store
	attention   *signet.Service
	workflow    *engine.Engine
	driver      *fake.StageDriver
	listener    net.Listener
	server      *http.Server
	cancel      context.CancelFunc
	errs        chan error
	wg          sync.WaitGroup
	closeOnce   sync.Once
	closeErr    error
	pairingCode string
}

func run(parent context.Context, cfg config) (_ *daemon, err error) {
	if cfg.DBPath == "" {
		return nil, errors.New("-db is required")
	}
	if cfg.FakeDriverDir == "" {
		cfg.FakeDriverDir = cfg.DBPath + ".fake-stage-driver"
	}
	if cfg.ReconcileInterval == 0 {
		cfg.ReconcileInterval = defaultReconcileInterval
	}
	if cfg.ReconcileInterval < 0 {
		return nil, fmt.Errorf("negative reconcile interval %s", cfg.ReconcileInterval)
	}
	if cfg.NtfyURL == "" {
		cfg.NtfyURL = defaultNtfyURL
	}
	_, statErr := os.Stat(cfg.DBPath)
	storePreexisting := statErr == nil
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		return nil, fmt.Errorf("stat store path: %w", statErr)
	}
	topicKey, err := loadOrCreateTopicKey(cfg.DBPath, storePreexisting)
	if err != nil {
		return nil, err
	}
	pairingKey := make([]byte, 32)
	if _, err := rand.Read(pairingKey); err != nil {
		return nil, fmt.Errorf("generate pairing key: %w", err)
	}

	listener, err := listenLoopback(cfg.ListenAddr)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			_ = listener.Close()
		}
	}()

	st, err := store.Open(parent, cfg.DBPath, store.Options{})
	if err != nil {
		return nil, err
	}
	defer func() {
		if !success {
			_ = st.Close()
		}
	}()
	blobs, err := signet.NewBlobStore(cfg.DBPath + ".blobs")
	if err != nil {
		return nil, fmt.Errorf("open attachment store: %w", err)
	}
	driver, err := fake.NewStageDriverAt(cfg.FakeDriverDir)
	if err != nil {
		return nil, fmt.Errorf("open fake stage driver: %w", err)
	}
	attention := signet.NewService(st,
		signet.WithPairingKey(pairingKey),
		signet.WithBlobStore(blobs),
		signet.WithNtfy(signet.NtfyConfig{
			BaseURL: cfg.NtfyURL, TopicKey: topicKey,
			ClickBaseURL: "http://" + listener.Addr().String(),
		}),
	)
	pairingCode, _, err := attention.MintPairingCode(parent)
	if err != nil {
		return nil, fmt.Errorf("mint startup pairing code: %w", err)
	}
	workflow, err := engine.New(st, attention, autoScriptStageDriver{StageDriver: driver})
	if err != nil {
		return nil, err
	}
	if _, err := workflow.StartFakeRun(parent, engine.FakeRunSpec{
		RunID: defaultFakeRunID, ProjectID: defaultFakeProjectID,
		SpecDigest: "sha256:walking-skeleton-spec", PolicyDigest: "sha256:walking-skeleton-policy",
	}); err != nil {
		return nil, fmt.Errorf("seed walking-skeleton run: %w", err)
	}

	ctx, cancel := context.WithCancel(parent)
	d := &daemon{
		store: st, attention: attention, workflow: workflow, driver: driver,
		listener: listener, cancel: cancel, errs: make(chan error, 2), pairingCode: pairingCode,
		server: &http.Server{
			Handler:           signet.NewHTTPHandler(attention, signet.NewRequestAuthorizer(st)),
			ReadHeaderTimeout: 5 * time.Second,
		},
	}
	d.wg.Add(2)
	go func() {
		defer d.wg.Done()
		err := d.server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		d.errs <- err
	}()
	go func() {
		defer d.wg.Done()
		d.errs <- workflow.Run(ctx, cfg.ReconcileInterval)
	}()
	success = true
	return d, nil
}

// autoScriptStageDriver gives the standalone 1A.0 daemon a complete fake
// workflow while preserving explicitly registered fixture scripts. The first
// Start is side-effect free when it reports ErrUnscripted, so registering the
// deterministic fallback and retrying cannot duplicate an invocation intent.
type autoScriptStageDriver struct {
	*fake.StageDriver
}

func (d autoScriptStageDriver) Start(ctx context.Context, id domain.InvocationID, spec exec.StartSpec) error {
	err := d.StageDriver.Start(ctx, id, spec)
	if !errors.Is(err, fake.ErrUnscripted) {
		return err
	}
	d.Script(id, fake.StageScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.StageResult{
			Summary: "The fake workflow invocation completed.",
		},
	})
	return d.StageDriver.Start(ctx, id, spec)
}

func (d *daemon) readiness() readiness {
	return readiness{APIURL: "http://" + d.listener.Addr().String(), PairingCode: d.pairingCode}
}

// Wait returns when the process context is canceled or either long-running
// component exits. A nil component result is normal only during shutdown.
func (d *daemon) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return nil
	case err := <-d.errs:
		return err
	}
}

func (d *daemon) Close() error {
	d.closeOnce.Do(func() {
		d.cancel()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownErr := d.server.Shutdown(ctx)
		d.wg.Wait()
		d.closeErr = errors.Join(shutdownErr, d.store.Close())
	})
	return d.closeErr
}

func listenLoopback(addr string) (net.Listener, error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen %q: %w", addr, err)
	}
	tcpAddr, ok := listener.Addr().(*net.TCPAddr)
	if !ok || !tcpAddr.IP.IsLoopback() {
		_ = listener.Close()
		return nil, fmt.Errorf("listen %q resolved to non-loopback address %q", addr, listener.Addr())
	}
	return listener, nil
}
