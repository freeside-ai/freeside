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
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"os"
	osexec "os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/advisory"
	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/elaborate"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/claude"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/operations"
	"github.com/freeside-ai/freeside/daemon/internal/procbound"
	"github.com/freeside-ai/freeside/daemon/internal/scheduler"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

const defaultReconcileInterval = 100 * time.Millisecond

const defaultNtfyURL = "https://ntfy.sh"

const defaultDoctorInterval = 24 * time.Hour

// defaultSchedulerInterval is the §5.16 scheduler's due-scan tick: how often
// durable schedules are checked for due fires, not any schedule's cadence.
const defaultSchedulerInterval = time.Second

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
	// §10 verbs never collide with the original flag-mode invocation because
	// flags begin with '-'. Keeping dispatch here preserves compatibility with
	// the already-proven daemon command line while packaging operations.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "setup":
			runSetupMain(os.Args[2:])
			return
		case "onboard":
			runOnboardMain(os.Args[2:])
			return
		case "doctor":
			runDoctorMain(os.Args[2:])
			return
		case "submit":
			runSubmitMain(os.Args[2:])
			return
		case "follow":
			runFollowMain(os.Args[2:])
			return
		case "enroll-codex":
			runEnrollCodexMain(os.Args[2:])
			return
		}
	}
	flags := flag.NewFlagSet("freesided", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	dbPath := flags.String("db", "", "SQLite database path (required; created if absent)")
	driverDir := flags.String("fake-driver-dir", "", "permanent fake StageDriver state directory (defaults beside -db)")
	listenAddr := flags.String("listen", "127.0.0.1:0", "signet listener address (loopback or Tailscale-owned address only)")
	ntfyURL := flags.String("ntfy-url", defaultNtfyURL, "ntfy server URL for device notifications")
	logLevel := flags.String("log-level", defaultLogLevel,
		"loop log severity: debug, info, warn, or error (debug adds a record per loop iteration)")
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
	driverMode := flags.String("driver", "fake", "stage driver: fake (1A.0 walking skeleton) or claude (production, #237)")
	agentImage := flags.String("agent-image", "", "digest-pinned Claude agent image")
	exporterImage := flags.String("exporter-image", "", "digest-pinned export helper image")
	containerBin := flags.String("container-bin", "container", "Apple container CLI path")
	seedRoot := flags.String("seed-root", "", "daemon-owned exact-base checkout root")
	stateDir := flags.String("state-dir", "", "production driver state directory")
	providerEndpoints := flags.String("provider-endpoints", "api.anthropic.com:443", "comma-separated provider host:port allowlist")
	promptPackage := flags.String("prompt-package", "", "trusted prompt-package file (ingested into the artifact store at startup)")
	elaborationPromptPackage := flags.String("elaboration-prompt-package", "", "trusted elaborator prompt-package file (ingested into the artifact store at startup)")
	vendorInstructions := flags.String("vendor-instructions", "", "host vendor-instruction file (CLAUDE.md)")
	repo := flags.String("repo", "", "managed owner/name repository")
	baseRef := flags.String("base-ref", "", "managed repository base branch")
	baseSHA := flags.String("base-sha", "", "exact 40-character base commit work items run against")
	allowedPaths := flags.String("allowed-paths", "", "comma-separated candidate path allowlist (required in claude driver mode)")
	authIdentity := flags.String("auth-identity", "", "provider auth identity work items run under")
	reviewImage := flags.String("review-image", "", "digest-pinned Codex review image")
	reviewInputRoot := flags.String("review-input-root", "", "private root containing Codex review auth and instruction snapshots")
	reviewAuthMode := flags.String("review-auth-mode", "", "Codex review auth mode: subscription or api_key")
	reviewAuthIdentity := flags.String("review-auth-identity", "", "Codex review auth identity")
	reviewAuthSnapshot := flags.String("review-auth-snapshot", "", "Codex auth.json snapshot under review-input-root")
	reviewInstructions := flags.String("review-instructions", "", "operator-host Codex instruction source snapshotted with explicit absence")
	reviewModel := flags.String("review-model", "", "pinned Codex review model configuration")
	reviewReasoningEffort := flags.String("review-reasoning-effort", "", "Codex review reasoning effort")
	reviewCostOwner := flags.String("review-cost-owner", "", "account charged for Codex review")
	reviewWorkspaceSize := flags.Int64("review-workspace-size-mb", 8192, "Codex review workspace volume size")
	runConformance := flags.Bool("run-conformance", false,
		"run and durably record the full ward suite for this exact Claude configuration before admission")
	operatingMode := flags.String(
		"operating-mode", string(domain.ModeAttendedDev),
		"operating mode: attended_dev (default) or unattended")
	doctorInterval := flags.Duration(
		"doctor-interval", defaultDoctorInterval,
		"scheduled operational-health cadence in production driver mode")
	schedulerInterval := flags.Duration(
		"scheduler-interval", defaultSchedulerInterval,
		"durable-scheduler due-scan tick in production driver mode")
	approvedRecipes := digestSetFlag{}
	flags.Var(&approvedRecipes, "approved-recipe",
		"approved verification-recipe digest for persistence and doctor (repeatable)")
	var repositoryID repositoryIDFlag
	flags.Var(&repositoryID, "repository-id", "canonical numeric identity of the managed repository")
	var backupEncryptionWaiverRepositoryID repositoryIDFlag
	flags.Var(&backupEncryptionWaiverRepositoryID, "backup-encryption-waiver-repository-id",
		"retired Phase 1A.2 option; encrypted-checkpoint builds reject every supplied value")
	if err := flags.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	logger, err := newLogger(os.Stderr, *logLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, "freesided:", err)
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
	daemonConfig := config{
		DBPath: *dbPath, FakeDriverDir: *driverDir, StateDir: *stateDir,
		ListenAddr: *listenAddr, NtfyURL: *ntfyURL, ReconcileInterval: *interval,
		DoctorInterval:                     *doctorInterval,
		SchedulerInterval:                  *schedulerInterval,
		ApprovedRecipes:                    approvedRecipes,
		BackupEncryptionWaiverRepositoryID: backupEncryptionWaiverRepositoryID.Value(),
		Logger:                             logger,
	}
	mode, err := parseOperatingMode(*operatingMode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "freesided: -operating-mode %q is not attended_dev or unattended\n", *operatingMode)
		os.Exit(2)
	}
	switch *driverMode {
	case "fake":
	case "claude":
		id := int64(0)
		if v := repositoryID.Value(); v != nil {
			id = *v
		}
		var codexAuthMode ward.CodexAuthMode
		if mode == domain.ModeUnattended {
			switch *reviewAuthMode {
			case string(ward.CodexAuthSubscription):
				codexAuthMode = ward.CodexAuthSubscription
			case string(ward.CodexAuthAPIKey):
				codexAuthMode = ward.CodexAuthAPIKey
			default:
				fmt.Fprintf(os.Stderr, "freesided: -review-auth-mode %q is not subscription or api_key\n", *reviewAuthMode)
				os.Exit(2)
			}
		}
		daemonConfig.Claude = &claudeDriverConfig{
			AgentImage: domain.ImageRef(*agentImage), ExporterImage: *exporterImage,
			ContainerBin: *containerBin, SeedRoot: *seedRoot,
			StateDir:                     *stateDir,
			ProviderEndpoints:            strings.Split(*providerEndpoints, ","),
			PromptPackageFile:            *promptPackage,
			ElaborationPromptPackageFile: *elaborationPromptPackage,
			VendorInstructions:           *vendorInstructions,
			Repo:                         *repo, RepositoryID: id,
			BaseRef: *baseRef, BaseSHA: *baseSHA,
			AuthIdentityID: domain.AuthIdentityID(*authIdentity),
			AllowedPaths:   splitNonEmpty(*allowedPaths),
			RunConformance: *runConformance,
			StateRoot:      *publicationStateDir, CredentialsDir: *publicationCredentialsDir,
			OperatingMode: mode,
			ReviewImage:   *reviewImage, ReviewInputRoot: *reviewInputRoot,
			ReviewAuthMode:       codexAuthMode,
			ReviewAuthIdentityID: domain.AuthIdentityID(*reviewAuthIdentity),
			ReviewAuthSnapshot:   *reviewAuthSnapshot, ReviewInstructions: *reviewInstructions,
			ReviewModel: *reviewModel, ReviewReasoningEffort: *reviewReasoningEffort,
			ReviewCostOwner: *reviewCostOwner, ReviewWorkspaceSizeMB: *reviewWorkspaceSize,
		}
	default:
		fmt.Fprintf(os.Stderr, "freesided: -driver %q is not fake or claude\n", *driverMode)
		os.Exit(2)
	}
	h, err := run(ctx, stop, daemonConfig)
	if err != nil {
		fmt.Fprintln(os.Stderr, "freesided:", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(h.readiness()); err != nil {
		// Restore signal disposition before teardown here too, for the reason
		// serve documents: an unstopped Close keeps absorbing the operator's
		// SIGTERM, leaving SIGKILL as the only way out of a slow teardown.
		stop()
		_ = h.Close()
		fmt.Fprintln(os.Stderr, "freesided:", err)
		os.Exit(1)
	}

	if err := serve(ctx, stop, h); err != nil {
		fmt.Fprintln(os.Stderr, "freesided:", err)
		os.Exit(1)
	}
}

// serve runs the daemon until its context ends or a component fails, then
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
func serve(ctx context.Context, stop func(), h *daemon) error {
	waitErr := h.Wait(ctx)
	stop()
	return errors.Join(waitErr, h.Close())
}

// splitNonEmpty splits a comma-separated flag into its non-empty members,
// so an unset flag yields no members rather than one empty one.
func splitNonEmpty(value string) []string {
	out := []string{}
	for _, part := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

type config struct {
	DBPath                             string
	FakeDriverDir                      string
	StateDir                           string
	ListenAddr                         string
	NtfyURL                            string
	ReconcileInterval                  time.Duration
	DoctorInterval                     time.Duration
	SchedulerInterval                  time.Duration
	ApprovedRecipes                    map[domain.Digest]bool
	BackupEncryptionWaiverRepositoryID *int64
	// Logger is the process logger the long-running loops report through.
	// Nil discards their records, which keeps every test composition quiet
	// without each one having to build a handler.
	Logger *slog.Logger
	// afterBackgroundStart is a test seam for startup failures after the base
	// background workers have launched. Production leaves it nil.
	afterBackgroundStart func() error
	// beforeStoreClose is a test seam for observing failed-start cleanup at
	// the store-close boundary. Production leaves it nil.
	beforeStoreClose func()
	// now is the attention service clock. Production leaves it nil for the
	// wall clock; tests can advance startup across pairing-code expiry.
	now func() time.Time
	// Claude, when set, replaces the permanent fake stage driver with the
	// production Claude driver and its ward gate (#237). Nil keeps the 1A.0
	// walking-skeleton composition byte-for-byte.
	Claude *claudeDriverConfig
}

// defaultLogLevel emits what an unattended daemon's operator needs and
// nothing per loop iteration. The reconcile, scheduler, and active-resource
// loops all tick on fixed cadences forever, so their per-pass records live
// at debug: a page of "pass complete" every quarter hour is a log that gets
// filtered out wholesale, taking the 401 with it.
const defaultLogLevel = "info"

// newLogger builds the process logger over w. Text key=value rather than
// JSON because the LaunchAgent captures stderr straight to a file the
// operator greps; a line they can read beats one that needs a tool.
//
// The level parses through slog's own text unmarshaler, so the accepted
// spellings are exactly slog's (including "warn" and offsets like
// "error+1") rather than a second vocabulary that drifts from it.
func newLogger(w io.Writer, level string) (*slog.Logger, error) {
	var parsed slog.Level
	if err := parsed.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("-log-level %q is not debug, info, warn, or error", level)
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: parsed})), nil
}

func (cfg config) storeOptions() (store.Options, error) {
	opts := store.Options{ApprovedRecipes: maps.Clone(cfg.ApprovedRecipes)}
	if cfg.BackupEncryptionWaiverRepositoryID != nil {
		return store.Options{}, fmt.Errorf(
			"-backup-encryption-waiver-repository-id: %w",
			domain.ErrBackupEncryptionWaiverUnsupported)
	}
	if cfg.Claude == nil {
		return opts, nil
	}
	// The store re-gates every recorded admission against the operator's
	// policy, so a claude-mode daemon has to configure that policy or its own
	// admissions are refused at persistence: an unset floor is "no policy
	// configured", which fails closed, not "no minimum". The floor here is
	// the same mode-specific minimum the engine admits against.
	opts.AdmissionFloors = map[domain.OperatingMode]domain.CapabilitySnapshot{
		cfg.Claude.OperatingMode: admissionCapabilitySnapshot(cfg.Claude.OperatingMode),
	}
	opts.ApprovedCredentialModes = []domain.CredentialMode{domain.CredentialSubscriptionContained}
	return opts, nil
}

func runScheduledDoctorPass(
	ctx context.Context,
	runConformance, runDoctor func(context.Context) error,
) error {
	conformanceErr := runConformance(ctx)
	doctorErr := runDoctor(ctx)
	if ctx.Err() != nil {
		return nil
	}
	return errors.Join(conformanceErr, doctorErr)
}

type readiness struct {
	APIURL      string `json:"api_url"`
	PairingCode string `json:"pairing_code"`
}

type sessionCloser interface {
	Close(context.Context) error
}

type daemon struct {
	lock          *daemonlock.Lock
	store         *store.Store
	attention     *signet.Service
	workflow      *engine.Engine
	driver        *fake.StageDriver
	listener      net.Listener
	server        *http.Server
	cancel        context.CancelFunc
	sessionCloser sessionCloser
	errs          chan error
	wg            sync.WaitGroup
	closeOnce     sync.Once
	closeErr      error
	pairingCode   string
	logger        *slog.Logger
}

func run(parent context.Context, stop func(), cfg config) (_ *daemon, err error) {
	startedAt := time.Now().UTC()
	if cfg.DBPath == "" {
		return nil, errors.New("-db is required")
	}
	lock, err := daemonlock.Acquire(cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("acquire database daemon lock: %w", err)
	}
	lockTransferred := false
	defer func() {
		if !lockTransferred {
			_ = lock.Close()
		}
	}()
	if cfg.StateDir == "" && cfg.Claude != nil {
		cfg.StateDir = cfg.Claude.StateDir
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
	if cfg.DoctorInterval == 0 {
		cfg.DoctorInterval = defaultDoctorInterval
	}
	if cfg.DoctorInterval < time.Second || cfg.DoctorInterval%time.Second != 0 {
		// Durable schedules carry whole-second cadences (§5.16): a
		// sub-second doctor interval was never meaningful for a pass that
		// runs conformance, and a fractional one would be silently truncated
		// at arming.
		return nil, fmt.Errorf("doctor interval %s must be a whole number of seconds", cfg.DoctorInterval)
	}
	if cfg.SchedulerInterval == 0 {
		cfg.SchedulerInterval = defaultSchedulerInterval
	}
	if cfg.SchedulerInterval < 0 {
		return nil, fmt.Errorf("negative scheduler interval %s", cfg.SchedulerInterval)
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	if cfg.NtfyURL == "" {
		cfg.NtfyURL = defaultNtfyURL
	}
	storeOptions, err := cfg.storeOptions()
	if err != nil {
		return nil, err
	}
	listener, err := listenPrivileged(cfg.ListenAddr)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			_ = listener.Close()
		}
	}()
	ctx, cancel := context.WithCancel(parent)
	defer func() {
		if !success {
			cancel()
		}
	}()

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
		backupPayloadExtractors())
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
			if cfg.beforeStoreClose != nil {
				cfg.beforeStoreClose()
			}
			_ = st.Close()
		}
	}()
	var startupSessionCloser sessionCloser
	defer func() {
		err = errors.Join(err, closeStartupSessions(success, startupSessionCloser, stop))
	}()
	// Backup evidence is maintained before anything can admit work. Orphan
	// reconciliation below resumes writers through the unattended admission
	// gate, which reads backup health, so a pass that has not yet scanned the
	// live database would let that gate see a stale checkpoint's verdict.
	localBackups, err := localBackupFiles.NewProducer(st)
	if err != nil {
		return nil, err
	}
	if err := localBackups.Maintain(parent); err != nil {
		// A durable row this binary cannot reconstruct is reported, never
		// fatal: startup is the one pass an operator cannot retry past, and
		// the dominant cause is a downgrade that starting is what lets them
		// undo. Backup health carries the refusal (store.ErrBackupClosureIncomplete).
		if !errors.Is(err, store.ErrBackupClosureIncomplete) {
			return nil, err
		}
		fmt.Fprintln(os.Stderr, "freesided:", err)
	}
	// The fake driver's state directory is only claimed in fake mode: the
	// production composition must not require walking-skeleton state on a
	// fresh operator machine.
	var driver *fake.StageDriver
	if cfg.Claude == nil {
		driver, err = fake.NewStageDriverAt(cfg.FakeDriverDir)
		if err != nil {
			return nil, fmt.Errorf("open fake stage driver: %w", err)
		}
	}
	var (
		workflow     *engine.Engine
		claudeWiring *claudeComposition
	)
	attention := signet.NewService(st,
		signet.WithPairingKey(pairingKey),
		signet.WithClock(cfg.now),
		signet.WithBlobStore(blobs),
		signet.WithNtfy(signet.NtfyConfig{
			BaseURL: cfg.NtfyURL, TopicKey: topicKey,
			ClickBaseURL: "http://" + listener.Addr().String(),
		}),
		// The effective digest exists only after the Claude composition below;
		// the func indirection lets the decision-time adoption gate read it
		// then. The fake-driver composition leaves it empty (gate absent).
		signet.WithEffectiveReviewConfiguration(func() domain.Digest {
			if claudeWiring == nil {
				return ""
			}
			return claudeWiring.reviewConfigurationDigest
		}),
	)
	if cfg.Claude == nil {
		walkingSkeletonOptions := []engine.Option{engine.WithDaemonLock(lock)}
		if cfg.Logger != nil {
			walkingSkeletonOptions = append(walkingSkeletonOptions, engine.WithLogger(cfg.Logger))
		}
		workflow, err = engine.New(
			st, attention, autoScriptStageDriver{StageDriver: driver}, walkingSkeletonOptions...)
		if err != nil {
			return nil, err
		}
		if _, err := workflow.StartFakeRun(parent, engine.FakeRunSpec{
			RunID: defaultFakeRunID, ProjectID: defaultFakeProjectID,
			SpecDigest: "sha256:walking-skeleton-spec", PolicyDigest: "sha256:walking-skeleton-policy",
		}); err != nil {
			return nil, fmt.Errorf("seed walking-skeleton run: %w", err)
		}
	} else {
		claudeWiring, err = composeClaudeDriver(ctx, st, blobs, *cfg.Claude, cfg.Logger)
		if err != nil {
			return nil, err
		}
		// Reconcile below can resume credential-bearing sessions before every
		// later startup step has succeeded. Register their awaited cleanup
		// immediately, while the store they need is still open.
		startupSessionCloser = claudeWiring.closer
		stageDriver, err := claudeWiring.stageDriver(blobs)
		if err != nil {
			return nil, err
		}
		engineOptions := []engine.Option{
			engine.WithDaemonLock(lock),
			engine.WithAdmission(claudeWiring.backend, admissionFloor(cfg.Claude.OperatingMode),
				claudeWiring.env, func() time.Time { return time.Now().UTC() }),
			engine.WithAdmissionDerivation(claudeWiring.derive),
		}
		advisoryStore, advisoryErr := advisory.Open(
			filepath.Join(cfg.StateDir, "advisory.json"), 2_000, 64<<10,
		)
		var advisoryWriter inference.AdvisoryWriter = advisoryStore
		if advisoryErr != nil {
			advisoryWriter = advisory.Unavailable(advisoryErr)
			if cfg.Logger != nil {
				cfg.Logger.Warn("inference advisory store unavailable; judgment calls will fail safe", "error", advisoryErr)
			}
		}
		judgmentBudget := inference.Budget{
			Window: 24 * time.Hour,
			Site: inference.Limits{
				Calls: 100, ComputeUnits: 1_000_000, AttentionItems: 25, Starvation: time.Hour,
			},
			Project: inference.Limits{
				Calls: 500, ComputeUnits: 5_000_000, AttentionItems: 100, Starvation: 4 * time.Hour,
			},
			Global: inference.Limits{
				Calls: 1_000, ComputeUnits: 10_000_000, AttentionItems: 200, Starvation: 8 * time.Hour,
			},
			MaxCallsPerRoot: 20, MaxStarvationPerRoot: 10 * time.Minute,
		}
		judgments, err := inference.New(inference.Config{
			StatePath:  filepath.Join(cfg.StateDir, "inference-budget.json"),
			AnchorPath: cfg.DBPath + ".inference-budget-anchor",
			Binding:    inference.Binding{Provider: "unavailable", Model: "unbound"},
			Sites: []inference.Site{
				inference.ClassifierSite(judgmentBudget), inference.DiagnosticSite(judgmentBudget),
			},
			Advisory: advisoryWriter, Now: func() time.Time { return time.Now().UTC() },
		})
		if err != nil {
			return nil, fmt.Errorf("compose inference boundary: %w", err)
		}
		engineOptions = append(engineOptions, engine.WithInference(judgments))
		researchFetcher, err := elaborate.NewFetcher(st, blobs, nil)
		if err != nil {
			return nil, fmt.Errorf("compose research fetcher: %w", err)
		}
		deliveryMaterializer, err := productionMaterializer(blobs)
		if err != nil {
			return nil, fmt.Errorf("compose elaboration delivery validator: %w", err)
		}
		engineOptions = append(engineOptions, engine.WithElaboration(engine.ElaborationConfig{
			Fetcher: researchFetcher, Blobs: blobs, Now: func() time.Time { return time.Now().UTC() },
			PromptPackageDigest: claudeWiring.elaborationPromptPackage,
			ValidateDelivery:    productionElaborationDeliveryValidator(deliveryMaterializer),
		}))
		engineOptions = append(engineOptions, engine.WithProductionPublication(engine.ProductionPublicationConfig{
			WorkDir:   filepath.Join(cfg.Claude.SeedRoot, "production-publication"),
			Transport: claudeWiring.publicationTransport,
			Publisher: claudeWiring.publisher, Artifacts: blobs,
			ApprovedRecipes:           cfg.ApprovedRecipes,
			ReviewSource:              claudeWiring.reviewSource,
			ReviewRecovery:            claudeWiring.reviewRecovery,
			ReviewConfigurationDigest: claudeWiring.reviewConfigurationDigest,
			ReviewHostInstructions:    claudeWiring.reviewHostInstructions,
			HoldOnly:                  cfg.Claude.OperatingMode != domain.ModeUnattended,
			NewRoom: func(image domain.ProjectImage) (engine.ProductionVerificationRoom, error) {
				return ward.NewProjectImageRoom(claudeWiring.containerBin, image)
			},
		}))
		if cfg.Logger != nil {
			engineOptions = append(engineOptions, engine.WithLogger(cfg.Logger))
		}
		workflow, err = engine.New(st, attention, stageDriver, engineOptions...)
		if err != nil {
			return nil, err
		}
		// Give every orphan its first adoption attempt before the engine loop.
		// A permanent reconstruction failure stops startup; an operational
		// retry leaves the intent running so the attempt's later Inspect pass
		// drives the same recovery again without a process restart.
		if err := holdRetryableClaudeRecovery(claudeWiring.driver.Reconcile(ctx)); err != nil {
			return nil, fmt.Errorf("reconcile orphaned handoffs: %w", err)
		}
	}
	runDoctor := func(runCtx context.Context) error {
		if cfg.Claude == nil {
			return nil
		}
		_, err := (operations.Doctor{
			Store: st, Attention: attention,
			ProjectID:           domain.ProjectID("project-system"),
			Backend:             domain.BackendFreshVMReadOnlyVolumeHandoff,
			ConfigurationDigest: claudeWiring.backend.ConfigurationDigest(),
			Mode:                cfg.Claude.OperatingMode,
		}).Run(runCtx)
		return err
	}
	if err := runDoctor(parent); err != nil {
		return nil, fmt.Errorf("initial doctor pass: %w", err)
	}
	if err := workflow.ConvergeLegacyFakePublicationPolicies(parent); err != nil {
		return nil, fmt.Errorf("converge legacy fake-publication policies: %w", err)
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	d := &daemon{
		lock:  lock,
		store: st, attention: attention, workflow: workflow, driver: driver,
		listener: listener, cancel: cancel, errs: make(chan error, 1),
		logger: logger,
		server: &http.Server{
			Handler: signet.NewHTTPHandler(attention, signet.NewRequestAuthorizer(st), signet.HealthResponse{
				Status: "ok", Version: buildVersion(), StartedAt: startedAt,
			}),
			ReadHeaderTimeout: 5 * time.Second,
		},
	}
	if claudeWiring != nil {
		d.sessionCloser = claudeWiring.closer
	}
	var fakeSched *scheduler.Scheduler
	var claudeSched *scheduler.Scheduler
	var activeReconciler *activeResourceReconciler
	if claudeWiring == nil {
		// Construct every loop before making restart-gated recovery available.
		// None is running yet, so no operator can resume into a partial start.
		fakeSched, err = newFakeScheduler(st)
		if err != nil {
			return nil, err
		}
		fakeSched.SetLogger(cfg.Logger)
	} else {
		// The doctor and janitor cadences live on the §5.16 durable scheduler;
		// their initial coverage remains the direct calls above.
		claudeSched, err = newClaudeScheduler(st, cfg, claudeWiring, runDoctor)
		if err != nil {
			return nil, err
		}
		claudeSched.SetLogger(cfg.Logger)
		if err := armTrustedConfigJobs(parent, claudeSched, cfg); err != nil {
			return nil, err
		}
		activeReconciler = &activeResourceReconciler{
			store: st, pull: claudeWiring.observePull,
			issue:  claudeWiring.observeIssue,
			review: claudeWiring.observeReview,
			// The default automated reviewer is wired here, not in the domain
			// (plan §5.16, §7; AGENTS.md, Automated reviewer).
			reviewers:        map[string]bool{defaultNativeReviewerLogin: true},
			reviewInvalidate: claudeWiring.reviewInvalidate,
			evictConcluded:   claudeWiring.evictConcluded,
			now:              func() time.Time { return time.Now().UTC() },
		}
	}
	if err := d.enableDurableStopRecovery(parent); err != nil {
		return nil, fmt.Errorf("enable durable-stop recovery: %w", err)
	}

	d.wg.Add(3)
	go func() {
		defer d.wg.Done()
		err := d.server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			return
		}
		d.componentExited(parent, ctx, componentHTTP, err)
	}()
	go func() {
		defer d.wg.Done()
		d.componentExited(parent, ctx, componentWorkflow, workflow.Run(ctx, cfg.ReconcileInterval))
	}()
	go func() {
		defer d.wg.Done()
		d.componentExited(parent, ctx, componentLocalBackups, localBackups.Run(ctx))
	}()
	defer func() {
		if !success {
			if stop != nil {
				stop()
			}
			cancel()
			_ = d.server.Close()
			d.wg.Wait()
		}
	}()
	if cfg.afterBackgroundStart != nil {
		if err := cfg.afterBackgroundStart(); err != nil {
			return nil, err
		}
	}
	if fakeSched != nil {
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			d.componentExited(parent, ctx, componentScheduler,
				fakeSched.Run(ctx, cfg.SchedulerInterval))
		}()
	}
	if claudeSched != nil {
		d.wg.Add(3)
		// The production publication lane gets its own loop: one task holds a
		// clone, a containerized verification, and GitHub calls for minutes,
		// which inside the reconcile loop would stall every other run,
		// invocation, and attention item for that whole span (issue #425).
		go func() {
			defer d.wg.Done()
			d.componentExited(parent, ctx, componentProductionPublication,
				workflow.RunProductionPublications(ctx, cfg.ReconcileInterval))
		}()
		go func() {
			defer d.wg.Done()
			d.componentExited(parent, ctx, componentScheduler,
				claudeSched.Run(ctx, cfg.SchedulerInterval))
		}()
		go func() {
			defer d.wg.Done()
			err := activeReconciler.Run(ctx,
				defaultActiveResourceInterval, operatorActiveResourceInterval, cfg.Logger)
			d.componentExited(parent, ctx, componentActiveResource, err)
		}()
	}
	pairingCode, _, err := attention.MintPairingCode(parent)
	if err != nil {
		return nil, fmt.Errorf("mint startup pairing code: %w", err)
	}
	d.pairingCode = pairingCode
	if err := publishReadiness(cfg.StateDir, d.readiness()); err != nil {
		return nil, err
	}
	success = true
	lockTransferred = true
	return d, nil
}

func parseOperatingMode(raw string) (domain.OperatingMode, error) {
	switch domain.OperatingMode(raw) {
	case domain.ModeAttendedDev:
		return domain.ModeAttendedDev, nil
	case domain.ModeUnattended:
		return domain.ModeUnattended, nil
	default:
		return "", fmt.Errorf("invalid operating mode %q", raw)
	}
}

func closeStartupSessions(success bool, closer sessionCloser, stop func()) error {
	if success || closer == nil {
		return nil
	}
	// Restore signal disposition before this teardown, for the reason serve
	// documents: startupSessionCloser can block on credential-bearing lease
	// cleanup, and until stop runs signal.NotifyContext keeps absorbing the
	// operator's SIGTERM, leaving SIGKILL as the only way out. stop is nil
	// only in tests that never register signal handling.
	if stop != nil {
		stop()
	}
	return closer.Close(context.Background())
}

func holdRetryableClaudeRecovery(err error) error {
	if errors.Is(err, claude.ErrRecoveryRetryable) ||
		engine.MutableAdmissionPolicyRefusal(err) {
		return nil
	}
	return err
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

func (d autoScriptStageDriver) Inspect(ctx context.Context, id domain.InvocationID) (exec.Inspection, error) {
	inspection, err := d.StageDriver.Inspect(ctx, id)
	if err != nil {
		return exec.Inspection{}, err
	}
	if inspection.Status == exec.StatusCompleted {
		if err := killTestCheckpoint(killCheckpointAfterResultCommitted); err != nil {
			return exec.Inspection{}, err
		}
	}
	return inspection, nil
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
func (d *daemon) Close() error {
	d.closeOnce.Do(func() {
		d.cancel()
		var driverErr error
		if d.sessionCloser != nil {
			// A credential-bearing external writer is more important than
			// bounded process-exit latency. Ward owns bounded teardown; do not
			// close its store or exit while a session still uses its lease.
			driverErr = d.sessionCloser.Close(context.Background())
		}
		ctx, cancel := context.WithTimeout(context.Background(), serverShutdownBudget)
		defer cancel()
		shutdownErr := d.server.Shutdown(ctx)
		d.wg.Wait()
		d.closeErr = errors.Join(driverErr, shutdownErr, d.store.Close(), d.lock.Close())
	})
	return d.closeErr
}

// serverShutdownBudget bounds draining in-flight HTTP requests. Unlike the
// session teardown above, this phase holds no credential lease: the API is
// a read/write surface over the store, and abandoning a slow request loses
// nothing durable.
const serverShutdownBudget = 5 * time.Second

var (
	tailscaleIPv4 = netip.MustParsePrefix("100.64.0.0/10")
	tailscaleIPv6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

const tailscaleIPTimeout = 5 * time.Second

func listenPrivileged(addr string) (net.Listener, error) {
	return listenPrivilegedWith(
		addr,
		func(network string, resolved *net.TCPAddr) (net.Listener, error) {
			return net.ListenTCP(network, resolved)
		},
		readTailscaleIPs,
	)
}

func listenPrivilegedWith(
	addr string,
	bind func(string, *net.TCPAddr) (net.Listener, error),
	tailscaleIPs func() ([]netip.Addr, error),
) (net.Listener, error) {
	resolved, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("resolve listen address %q: %w", addr, err)
	}
	if resolved.IP == nil || resolved.IP.IsUnspecified() || resolved.Zone != "" {
		return nil, fmt.Errorf("listen %q resolved to an unsupported address %q", addr, resolved)
	}
	if !resolved.IP.IsLoopback() {
		if !isTailscaleIP(resolved.IP) {
			return nil, fmt.Errorf("listen %q resolved to non-loopback, non-Tailscale address %q", addr, resolved)
		}
		addrs, err := tailscaleIPs()
		if err != nil {
			return nil, fmt.Errorf("query Tailscale addresses for listener %q: %w", addr, err)
		}
		if !tailscaleOwnsIP(resolved.IP, addrs) {
			return nil, fmt.Errorf("listen %q resolved to address %q not reported by Tailscale", addr, resolved)
		}
	}
	network := "tcp6"
	if resolved.IP.To4() != nil {
		network = "tcp4"
	}
	listener, err := bind(network, resolved)
	if err != nil {
		return nil, fmt.Errorf("listen %q: %w", addr, err)
	}
	bound, ok := listener.Addr().(*net.TCPAddr)
	if !ok || !bound.IP.Equal(resolved.IP) {
		_ = listener.Close()
		return nil, fmt.Errorf("listen %q bound unexpected address %q", addr, listener.Addr())
	}
	return listener, nil
}

func isTailscaleIP(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	return isTailscaleAddr(addr)
}

func readTailscaleIPs() ([]netip.Addr, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tailscaleIPTimeout)
	defer cancel()

	// The executable name and arguments are fixed; only the trusted supervisor
	// controls the daemon's PATH.
	cmd := osexec.CommandContext(ctx, "tailscale", "ip") //nolint:gosec // G204 has no untrusted command input.
	procbound.Bind(cmd, procbound.DefaultWaitDelay)
	defer procbound.Reap(cmd)
	output, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("tailscale ip: %w", ctx.Err())
		}
		return nil, fmt.Errorf("tailscale ip: %w", err)
	}
	return parseTailscaleIPs(string(output))
}

func parseTailscaleIPs(output string) ([]netip.Addr, error) {
	fields := strings.Fields(output)
	if len(fields) == 0 {
		return nil, errors.New("tailscale ip returned no addresses")
	}
	addrs := make([]netip.Addr, 0, len(fields))
	for _, field := range fields {
		addr, err := netip.ParseAddr(field)
		if err != nil || addr.Zone() != "" || !isTailscaleAddr(addr) {
			return nil, fmt.Errorf("tailscale ip returned unsupported address %q", field)
		}
		addrs = append(addrs, addr.Unmap())
	}
	return addrs, nil
}

func tailscaleOwnsIP(ip net.IP, addrs []netip.Addr) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	for _, assigned := range addrs {
		if assigned.Unmap() == addr {
			return true
		}
	}
	return false
}

func isTailscaleAddr(addr netip.Addr) bool {
	addr = addr.Unmap()
	return tailscaleIPv4.Contains(addr) || tailscaleIPv6.Contains(addr)
}
