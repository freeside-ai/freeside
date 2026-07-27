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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

const defaultReconcileInterval = 100 * time.Millisecond

const defaultNtfyURL = "https://ntfy.sh"

const (
	defaultFakeRunID     domain.RunID     = "run-walking-skeleton"
	defaultFakeProjectID domain.ProjectID = "project-walking-skeleton"
)

type repositoryIDFlag struct {
	value *int64
}

func (f *repositoryIDFlag) Set(value string) error {
	repositoryID, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fmt.Errorf("parse repository ID %q: %w", value, err)
	}
	if repositoryID <= 0 {
		return fmt.Errorf("repository ID must be positive, got %d", repositoryID)
	}
	f.value = &repositoryID
	return nil
}

func (f *repositoryIDFlag) String() string {
	if f.value == nil {
		return ""
	}
	return strconv.FormatInt(*f.value, 10)
}

func (f *repositoryIDFlag) Value() *int64 {
	if f.value == nil {
		return nil
	}
	value := *f.value
	return &value
}

func main() {
	flags := flag.NewFlagSet("freesided", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	dbPath := flags.String("db", "", "SQLite database path (required; created if absent)")
	driverDir := flags.String("fake-driver-dir", "", "permanent fake StageDriver state directory (defaults beside -db)")
	listenAddr := flags.String("listen", "127.0.0.1:0", "signet listener address (loopback only)")
	ntfyURL := flags.String("ntfy-url", defaultNtfyURL, "ntfy server URL for device notifications")
	interval := flags.Duration("reconcile-interval", defaultReconcileInterval, "workflow reconciliation interval")
	fakePublication := flags.Bool("fake-publication", false, "run one explicit attended fake-candidate publication")
	publicationStateDir := flags.String("publication-state-dir", "", "GitHub App authority state directory")
	publicationCredentialsDir := flags.String("publication-credentials-dir", "", "GitHub App credential directory")
	publicationWorkDir := flags.String("publication-work-dir", "", "durable publication handoff directory")
	publicationWorkspace := flags.String("publication-workspace", "", "fake candidate workspace to export")
	publicationRecipe := flags.String("publication-recipe", "", "trusted JSON verification recipe")
	publicationRecipePath := flags.String("publication-recipe-path", verify.DefaultRecipePath, "repository-relative verification recipe path")
	publicationRepo := flags.String("publication-repo", "", "managed owner/name repository")
	publicationBaseRef := flags.String("publication-base-ref", "", "managed repository base branch")
	publicationBaseSHA := flags.String("publication-base-sha", "", "exact 40-character base commit")
	publicationAllowedPaths := flags.String("publication-allowed-paths", "**", "comma-separated candidate path allowlist")
	publicationRunID := flags.String("publication-run-id", "run-fake-publication", "durable publication run id")
	publicationProjectID := flags.String("publication-project-id", "project-fake-publication", "publication project id")
	publicationTitle := flags.String("publication-title", "Publish attended fake candidate", "pull request title")
	publicationBody := flags.String("publication-body", "", "pull request body")
	var backupEncryptionWaiverRepositoryID repositoryIDFlag
	flags.Var(&backupEncryptionWaiverRepositoryID, "backup-encryption-waiver-repository-id",
		"temporary Phase 1A.2 backup-encryption waiver for this exact trusted numeric repository ID")
	if err := flags.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if *fakePublication {
		result, err := runFakePublicationCommand(ctx, fakePublicationCommandConfig{
			DBPath: *dbPath, FakeDriverDir: *driverDir,
			StateDir: *publicationStateDir, CredentialsDir: *publicationCredentialsDir,
			WorkDir: *publicationWorkDir, WorkspaceDir: *publicationWorkspace,
			RecipeFile: *publicationRecipe, RecipeRepoPath: *publicationRecipePath,
			Repo:    *publicationRepo,
			BaseRef: *publicationBaseRef, BaseSHA: *publicationBaseSHA,
			AllowedPaths: strings.Split(*publicationAllowedPaths, ","),
			RunID:        domain.RunID(*publicationRunID), ProjectID: domain.ProjectID(*publicationProjectID),
			Title: *publicationTitle, Body: *publicationBody,
			ReconcileInterval: *interval,
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "freesided:", err)
			os.Exit(1)
		}
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			fmt.Fprintln(os.Stderr, "freesided:", err)
			os.Exit(1)
		}
		return
	}
	h, err := run(ctx, config{
		DBPath: *dbPath, FakeDriverDir: *driverDir,
		ListenAddr: *listenAddr, NtfyURL: *ntfyURL, ReconcileInterval: *interval,
		BackupEncryptionWaiverRepositoryID: backupEncryptionWaiverRepositoryID.Value(),
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
	DBPath                             string
	FakeDriverDir                      string
	ListenAddr                         string
	NtfyURL                            string
	ReconcileInterval                  time.Duration
	BackupEncryptionWaiverRepositoryID *int64
}

func (cfg config) storeOptions() (store.Options, error) {
	if cfg.BackupEncryptionWaiverRepositoryID == nil {
		return store.Options{}, nil
	}
	repositoryID := *cfg.BackupEncryptionWaiverRepositoryID
	if repositoryID <= 0 {
		return store.Options{}, fmt.Errorf(
			"-backup-encryption-waiver-repository-id must be positive, got %d", repositoryID)
	}
	return store.Options{BackupEncryptionWaiverRepositoryID: &repositoryID}, nil
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
	storeOptions, err := cfg.storeOptions()
	if err != nil {
		return nil, err
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

	blobs, err := signet.NewBlobStore(cfg.DBPath + ".blobs")
	if err != nil {
		return nil, fmt.Errorf("open attachment store: %w", err)
	}
	localBackupFiles, err := store.NewDefaultLocalBackupFiles(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	backupHealth, err := localBackupFiles.NewCheckpointHealthSource(
		blobs, storeOptions.ApprovedRecipes,
		map[string]store.BackupPayloadDigestExtractor{
			engine.FakePublicationTaskKind:            engine.FakePublicationBackupPayloadDigests,
			engine.FakePublicationInvocationOwnerKind: engine.FakePublicationInvocationOwnerBackupPayloadDigests,
			signet.AgentInvocationRequestedKind:       signet.AgentInvocationBackupPayloadDigests,
			publish.IntentKindReservation:             publish.ReservationBackupPayloadDigests,
			publish.IntentKindPublication:             publish.PublicationBackupPayloadDigests,
		})
	if err != nil {
		return nil, err
	}
	storeOptions.BackupHealthSource = backupHealth
	st, err := store.Open(parent, cfg.DBPath, storeOptions)
	if err != nil {
		return nil, err
	}
	defer func() {
		if !success {
			_ = st.Close()
		}
	}()
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
	localBackups, err := localBackupFiles.NewProducer(st)
	if err != nil {
		return nil, err
	}
	if err := localBackups.Maintain(parent); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(parent)
	d := &daemon{
		store: st, attention: attention, workflow: workflow, driver: driver,
		listener: listener, cancel: cancel, errs: make(chan error, 3), pairingCode: pairingCode,
		server: &http.Server{
			Handler:           signet.NewHTTPHandler(attention, signet.NewRequestAuthorizer(st)),
			ReadHeaderTimeout: 5 * time.Second,
		},
	}
	d.wg.Add(3)
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
	go func() {
		defer d.wg.Done()
		d.errs <- localBackups.Run(ctx)
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
	if err := killTestCheckpoint(killCheckpointBeforeIntentDispatch); err != nil {
		return err
	}
	err := d.StageDriver.Start(ctx, id, spec)
	if err == nil {
		return killTestCheckpoint(killCheckpointAfterIntentAccepted)
	}
	if !errors.Is(err, fake.ErrUnscripted) {
		return err
	}
	d.Script(id, fake.StageScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.StageResult{
			Summary: "The fake workflow invocation completed.",
		},
	})
	if err := d.StageDriver.Start(ctx, id, spec); err != nil {
		return err
	}
	return killTestCheckpoint(killCheckpointAfterIntentAccepted)
}

func (d autoScriptStageDriver) Inspect(ctx context.Context, id domain.InvocationID) (exec.Status, error) {
	status, err := d.StageDriver.Inspect(ctx, id)
	if err != nil {
		return "", err
	}
	if status == exec.StatusCompleted {
		if err := killTestCheckpoint(killCheckpointAfterResultCommitted); err != nil {
			return "", err
		}
	}
	return status, nil
}

func (d *daemon) readiness() readiness {
	return readiness{APIURL: "http://" + d.listener.Addr().String(), PairingCode: d.pairingCode}
}

// Wait returns when the process context is canceled or any long-running
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
