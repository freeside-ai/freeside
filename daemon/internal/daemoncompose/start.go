package daemoncompose

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/admissionauth"
	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/daemonshell"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/operations"
	"github.com/freeside-ai/freeside/daemon/internal/scheduler"
	"github.com/freeside-ai/freeside/daemon/internal/seedfixture"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/topicstore"
	"github.com/freeside-ai/freeside/daemon/internal/usagecollect"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// Approval wakes the filer immediately; this tick bounds recovery delay.
const defaultFollowUpFilingInterval = time.Minute

// StartSeams are the test seams a start reads. Production leaves each one
// unset.
type StartSeams struct {
	// AfterBackgroundStart is a test seam for startup failures after the base
	// background workers have launched. Production leaves it nil.
	AfterBackgroundStart func() error
	// BeforeStoreClose is a test seam for observing failed-start cleanup at
	// the store-close boundary. Production leaves it nil.
	BeforeStoreClose func()
	// UsageCollectors are the usage collectors by harness client. Production
	// leaves it nil and gets Claude's, which answers from writer turns'
	// usage captures; tests set a fake, and then no capture is recorded.
	UsageCollectors map[domain.HarnessClientKind]ward.UsageCollector
	// ClaudeModelDiscoverer runs the startup model discovery. Production
	// leaves it nil and launches the initialize probe; tests set a fake.
	ClaudeModelDiscoverer ClaudeModelDiscoverer
	// CodexModelDiscoverer runs the startup Codex model discovery. Production
	// leaves it nil and launches the model/list probe; tests set a fake.
	CodexModelDiscoverer CodexModelDiscoverer
	// Now is the attention service clock. Production leaves it nil for the
	// wall clock (run defaults it); tests can advance startup across
	// pairing-code expiry.
	Now func() time.Time
}

// StartOptions is what a start reads: main's configuration after run has
// validated and defaulted it, the files its flags name, and what only main
// supplies. Each field that shares a name with main's configuration means
// what that field means there.
type StartOptions struct {
	DBPath                    string
	StateDir                  string
	ListenAddr                string
	FakeDriverEnabled         bool
	FakeDriverDir             string
	NtfyURL                   string
	ReconcileInterval         time.Duration
	DoctorInterval            time.Duration
	SchedulerInterval         time.Duration
	AttentionDeliveryInterval time.Duration
	ApprovedRecipes           map[domain.Digest]bool
	SeedWalkingSkeleton       bool
	// SeedFixture is the parsed fixture to seed, or empty for none.
	SeedFixture      seedfixture.Name
	IntakeInitiators []IntakeInitiator
	MemoryBudgetMiB  int
	Logger           *slog.Logger
	Claude           *ClaudeDriverConfig
	Scenario         *Scenario
	// ManualProjects are the projects the manual-submission file configures,
	// and ManualInitiator resolves a client submission's policy from them.
	ManualProjects  map[domain.ProjectID]engine.ManualInitiator
	ManualInitiator func(domain.ProjectID) (engine.ManualInitiator, bool)
	// StoreOptions are the store's options before the start adds its backup
	// health source.
	StoreOptions store.Options
	// ReadinessEnvironment is the environment tier the readiness file names.
	ReadinessEnvironment string
	// Version is the build version the health response reports.
	Version string
	// IdentityLabel is the label the doctor shows for an auth identity.
	IdentityLabel func(domain.AuthIdentity) string
	// WrapFakeDriver wraps the fake stage driver the fake lane runs.
	WrapFakeDriver func(*fake.StageDriver) exec.StageDriver
	// OpenControl opens the control socket. The start calls it after taking
	// the database lock and closes what it returns if the start fails.
	OpenControl func(dbPath, stateDir string) (ControlSocket, error)
	StartSeams
}

// ControlSocket is the daemon's local control socket. The start configures
// it once the shell knows its API address, registers its routes over the
// composed parts, and publishes its address last.
type ControlSocket interface {
	daemonshell.PairingControl
	Configure(apiURL string, mint func(context.Context) (string, domain.PairingCode, error))
	RegisterRoutes(ControlRouteInputs)
	Publish() error
}

// ControlRouteInputs are the composed parts the control routes serve over.
type ControlRouteInputs struct {
	Store            *store.Store
	Blobs            *signet.BlobStore
	LocalBackupFiles *store.LocalBackupFiles
	// ApprovedRecipes is the store's recipe set.
	ApprovedRecipes map[domain.Digest]bool
	IntegrityProbe  CredentialIntegrityProbe
	Usage           *usagecollect.Coordinator
	Attention       *signet.Service
	// CommitWorkflowClosure commits a task's workflow closure through the
	// engine, or directly when no engine runs.
	CommitWorkflowClosure func(context.Context, domain.TaskID, func() error) error
	// CommitTaskStop commits a task's Stop through the engine, ordered
	// against invocation launch as a client's Stop is, or directly when no
	// engine runs.
	CommitTaskStop func(context.Context, domain.TaskID, func() error) error
}

// Daemon is what start composes: the shell that runs the process,
// and the composition results that startup and the tests reach.
type Daemon struct {
	*daemonshell.Shell
	Store     *store.Store
	Attention *signet.Service
	Workflow  *engine.Engine
	Driver    *fake.StageDriver
	Usage     *usagecollect.Coordinator
}

// Start acquires and composes everything a daemon runs, and starts it. A
// start that fails releases everything it acquired, in reverse order.
func Start(parent context.Context, stop func(), cfg StartOptions) (_ *Daemon, err error) {
	startedAt := time.Now().UTC()
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
	pairing, err := cfg.OpenControl(cfg.DBPath, cfg.StateDir)
	if err != nil {
		return nil, err
	}
	defer func() {
		if !lockTransferred {
			_ = pairing.Close()
		}
	}()
	storeOptions := cfg.StoreOptions
	listener, err := daemonshell.ListenPrivileged(cfg.ListenAddr)
	if err != nil {
		return nil, err
	}
	// A Tailscale-bound daemon also serves the identical API on 127.0.0.1 at the
	// same port, so a same-host client always has a path a host VPN cannot drop
	// (#1449; devlog 2026-09-20-1257-same-host-loopback-listener.md). A loopback
	// primary needs no twin. A twin bind failure is fatal: the daemon must never
	// run reachable only over the filterable Tailscale address.
	var loopbackTwin net.Listener
	success := false
	defer func() {
		if !success {
			_ = listener.Close()
			if loopbackTwin != nil {
				_ = loopbackTwin.Close()
			}
		}
	}()
	loopbackTwin, err = daemonshell.ListenLoopbackTwin(
		listener,
		func(network string, resolved *net.TCPAddr) (net.Listener, error) {
			return net.ListenTCP(network, resolved)
		},
	)
	if err != nil {
		return nil, err
	}
	// The pairing surface's process-fixed facts (plan §5.14): the connection
	// mode the bound listener implies and the host's name as a display label.
	// Both are read once here; a host that cannot name itself must not pair
	// devices under an empty name, so that failure is fatal at startup.
	connectionMode, err := daemonshell.ConnectionModeOf(listener.Addr())
	if err != nil {
		return nil, err
	}
	hostName, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("read host name for pairing facts: %w", err)
	}
	if hostName == "" {
		return nil, errors.New("read host name for pairing facts: empty host name")
	}
	ctx, cancel := context.WithCancel(parent)
	defer func() {
		if !success {
			cancel()
		}
	}()

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
		engine.BackupPayloadExtractors())
	if err != nil {
		return nil, err
	}
	storeOptions.BackupHealthSource = backupHealth
	st, topicKey, err := topicstore.Open(parent, cfg.DBPath, storeOptions)
	if err != nil {
		return nil, err
	}
	defer func() {
		if !success {
			if cfg.BeforeStoreClose != nil {
				cfg.BeforeStoreClose()
			}
			_ = st.Close()
		}
	}()
	// The budget's checks wait for the store, because a budget recorded there
	// wins over the declared one.
	bound, err := cfg.startupLaunchBound(parent, st, cfg.ManualProjects)
	if err != nil {
		return nil, err
	}
	logLaunchBound(cfg.Logger, bound)
	var startupSessionCloser SessionCloser
	defer func() {
		err = errors.Join(err, closeStartupSessions(success, startupSessionCloser, stop))
	}()
	if cfg.SeedFixture != "" {
		if err := SeedStartupFixture(parent, st, cfg.SeedFixture, cfg.DBPath, cfg.Logger); err != nil {
			return nil, err
		}
	}
	localBackups, err := MaintainLocalBackups(parent, st, blobs, localBackupFiles, cfg.Logger)
	if err != nil {
		return nil, err
	}
	// The fake driver's state directory is only claimed in fake mode: the
	// production composition must not require walking-skeleton state on a
	// fresh operator machine.
	var driver *fake.StageDriver
	if cfg.FakeDriverEnabled {
		driver, err = fake.NewStageDriverAt(cfg.FakeDriverDir)
		if err != nil {
			return nil, fmt.Errorf("open fake stage driver: %w", err)
		}
	}
	var (
		workflow              *engine.Engine
		claudeWiring          *ClaudeComposition
		imageUpkeep           *engine.ImageUpkeep
		scenarioWiring        *ScenarioComposition
		doctorConvergenceLock sync.Mutex
		doctorAvailable       atomic.Bool
		late                  = &StartupBindings{}
		claudeStartup         = cfg.claudeStartup()
	)
	usageRefreshAge, usage, claudeUsageCaptures := newUsageCollection(st, cfg)
	attention := newAttentionService(st, blobs, cfg, pairingKey, hostName, connectionMode, topicKey,
		cfg.ManualInitiator, usage, usageRefreshAge, &doctorAvailable, late)
	if cfg.FakeDriverEnabled {
		workflow, err = ComposeFakeWorkflow(
			st, attention, cfg.WrapFakeDriver(driver), lock, bound, cfg.Logger)
		if err != nil {
			return nil, err
		}
		// Published before the seed, so an attention option the seed's
		// writes reach reads this engine, not nil.
		late.Workflow = workflow
		if cfg.SeedWalkingSkeleton {
			if err := SeedWalkingSkeleton(parent, workflow); err != nil {
				return nil, err
			}
		}
	} else if cfg.Claude != nil {
		var claude *ClaudeWorkflow
		claude, err = ComposeClaudeWorkflow(ctx, st, attention, blobs, lock, bound, claudeStartup, cfg.ManualProjects,
			claudeUsageCaptures, &doctorConvergenceLock, &startupSessionCloser, late)
		if err != nil {
			return nil, err
		}
		claudeWiring, workflow, imageUpkeep = claude.ClaudeWiring, claude.Workflow, claude.ImageUpkeep
	} else if cfg.Scenario != nil {
		var parts ScenarioParts
		parts, err = newScenarioParts(st, attention, blobs, lock, bound, cfg)
		if err != nil {
			return nil, fmt.Errorf("compose scenario %s: %w", cfg.Scenario.Name, err)
		}
		scenarioWiring, err = ComposeScenario(ctx, parts)
		if err != nil {
			return nil, fmt.Errorf("compose scenario %s: %w", cfg.Scenario.Name, err)
		}
		late.ScenarioWiring, late.AgentCatalog = scenarioWiring, &scenarioWiring.Catalog
		// The forge serves only once its loop starts; a start that fails
		// before then still owns the listener.
		defer func() {
			if err != nil {
				_ = scenarioWiring.Listener.Close()
			}
		}()
		workflow = scenarioWiring.Workflow
		late.Workflow = workflow
	}
	// The live credential-integrity probe exists only in Claude mode, where
	// the daemon has a runtime and a pinned exporter image to observe with.
	var integrityProbe CredentialIntegrityProbe
	if cfg.Claude != nil {
		integrityProbe = NewCredentialIntegrityProbe(st,
			ProductionCredentialIntegrityObservers(*cfg.Claude, ward.NewCLIRuntime(claudeWiring.ContainerBin)),
			cfg.Now, cfg.Logger)
	}
	// The startup pass reports recorded marks only, so starting the daemon
	// starts no observer container. Scheduled passes run the live probe.
	if err := newDoctorPass(cfg, st, attention, claudeWiring, &doctorConvergenceLock, nil)(parent); err != nil {
		return nil, fmt.Errorf("initial doctor pass: %w", err)
	}
	runDoctor := newDoctorPass(cfg, st, attention, claudeWiring, &doctorConvergenceLock, integrityProbe)
	if workflow != nil {
		if err := workflow.ConvergeLegacyFakePublicationPolicies(parent); err != nil {
			return nil, fmt.Errorf("converge legacy fake-publication policies: %w", err)
		}
	}
	if err := convergeExecutionConfiguration(parent, st, workflow != nil, cfg.Now); err != nil {
		return nil, fmt.Errorf("report execution configuration: %w", err)
	}
	if err := concludeInvocationStallNotices(parent, st); err != nil {
		return nil, fmt.Errorf("conclude stale invocation stall notices: %w", err)
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if loopbackTwin != nil {
		logger.Info("serving same-host loopback twin listener",
			"primary", listener.Addr().String(), "loopback", loopbackTwin.Addr().String())
	}
	// A completion recorded before migration 0066 gains its
	// work_unit_completed milestone once (#1134); a store already carrying
	// the milestones is a no-op.
	if err := reconcileWorkUnitCompletionMilestones(parent, st, logger); err != nil {
		return nil, err
	}
	server := &http.Server{
		Handler: signet.NewHTTPHandler(attention, signet.NewRequestAuthorizer(st), signet.HealthResponse{
			Status: "ok", Version: cfg.Version, StartedAt: startedAt,
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	// Shutdown releases open revision long-polls, so draining the server
	// stays inside ServerShutdownBudget instead of waiting out their bound.
	server.RegisterOnShutdown(attention.Shutdown)
	var sessions daemonshell.SessionCloser
	if claudeWiring != nil {
		sessions = claudeWiring.Closer
	}
	d := &Daemon{
		Shell: daemonshell.New(daemonshell.Parts{
			Lock: lock, Store: st, Listener: listener, LoopbackTwin: loopbackTwin, Server: server,
			Pairing: pairing, SessionCloser: sessions, Cancel: cancel, Logger: logger, Now: cfg.Now,
		}),
		Store: st, Attention: attention, Workflow: workflow, Driver: driver, Usage: usage,
	}
	pairing.Configure(d.Readiness().APIURL, attention.MintPairingCode)
	pairing.RegisterRoutes(ControlRouteInputs{
		Store: st, Blobs: blobs, LocalBackupFiles: localBackupFiles, ApprovedRecipes: storeOptions.ApprovedRecipes,
		IntegrityProbe: integrityProbe, Usage: d.Usage, Attention: attention,
		CommitWorkflowClosure: func(ctx context.Context, id domain.TaskID, commit func() error) error {
			if workflow == nil {
				return commit()
			}
			return workflow.CommitTaskWorkflowClosure(ctx, id, commit)
		},
		CommitTaskStop: func(ctx context.Context, id domain.TaskID, commit func() error) error {
			if workflow == nil {
				return commit()
			}
			return workflow.CommitTaskStop(ctx, id, commit)
		},
	})
	var fakeSched *scheduler.Scheduler
	var loops *ClaudeLoops
	if cfg.FakeDriverEnabled {
		// Construct every loop before making restart-gated recovery available.
		// None is running yet, so no operator can resume into a partial start.
		fakeSched, err = NewFakeScheduler(st)
		if err != nil {
			return nil, err
		}
		fakeSched.SetLogger(cfg.Logger)
	} else if claudeWiring != nil {
		loops, err = ComposeClaudeLoops(parent, st, attention, blobs, claudeStartup, claudeWiring, workflow, imageUpkeep,
			runDoctor, d.Usage.CollectAll, late)
		if err != nil {
			return nil, err
		}
	}
	if workflow != nil {
		if err := d.EnableDurableStopRecovery(parent); err != nil {
			return nil, fmt.Errorf("enable durable-stop recovery: %w", err)
		}
	}

	d.startServing(parent, ctx, cfg, st, attention, workflow, claudeWiring, localBackups, logger)
	defer func() {
		if !success {
			if stop != nil {
				stop()
			}
			d.AbortStart()
		}
	}()
	if cfg.AfterBackgroundStart != nil {
		if err := cfg.AfterBackgroundStart(); err != nil {
			return nil, err
		}
	}
	if fakeSched != nil {
		d.Go(func() {
			d.ComponentExited(parent, ctx, daemonshell.ComponentScheduler,
				fakeSched.Run(ctx, cfg.SchedulerInterval))
		})
	}
	if loops != nil {
		d.startClaudeLoops(parent, ctx, cfg, claudeWiring, workflow, imageUpkeep, loops, &doctorAvailable, logger)
	}
	if scenarioWiring != nil {
		d.startScenarioLoops(parent, ctx, cfg, scenarioWiring, workflow)
	}
	pairingCode, _, err := attention.MintPairingCode(parent)
	if err != nil {
		return nil, fmt.Errorf("mint startup pairing code: %w", err)
	}
	if err := d.PublishReadiness(cfg.StateDir, pairingCode, cfg.ReadinessEnvironment); err != nil {
		return nil, err
	}
	if pairing != nil {
		if err := pairing.Publish(); err != nil {
			return nil, err
		}
	}
	success = true
	lockTransferred = true
	return d, nil
}

func closeStartupSessions(success bool, closer SessionCloser, stop func()) error {
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

// claudeStartup is the configuration the Claude composition reads.
func (cfg StartOptions) claudeStartup() ClaudeStartupConfig {
	return ClaudeStartupConfig{
		Claude: cfg.Claude, Logger: cfg.Logger, StateDir: cfg.StateDir, DBPath: cfg.DBPath,
		ApprovedRecipes: cfg.ApprovedRecipes, IntakeInitiators: cfg.IntakeInitiators,
		DoctorInterval: cfg.DoctorInterval, Now: cfg.Now,
	}
}

// reportAttentionDelivery logs one thing the notification sender could not
// do. A send that failed is a warning with its item, device, and attempt; the
// provider being down is ordinary, and the sender retries on its own
// schedule. The error names a status code or a failure class and never the
// device's topic (signet.ChannelTransportError).
func reportAttentionDelivery(logger *slog.Logger, err error) {
	if logger == nil {
		return
	}
	var failure *signet.DeliveryFailure
	if errors.As(err, &failure) {
		logger.Warn("phone notification not sent",
			"item_id", failure.ItemID, "device_id", failure.DeviceID,
			"attempt", failure.Attempt, "last_attempt", failure.Final, "error", failure.Err)
		return
	}
	logger.Warn("phone notification pass failed", "error", err)
}

// newUsageCollection builds the usage coordinator, the age at which a
// reading goes stale, and the Claude capture sink, which stays a nil
// interface without the Claude collector. Collection starts in
// startServing.
func newUsageCollection(
	st *store.Store, cfg StartOptions,
) (time.Duration, *usagecollect.Coordinator, engine.ClaudeUsageSink) {
	// A reading is stale once one scheduled doctor pass, the collection's
	// cadence, has been missed. Collection and the bootstrap's stale_at share
	// the age, so a client sees a reading stale exactly when collection does.
	usageRefreshAge := 2 * cfg.DoctorInterval
	usageCollectors, claudeUsage := usageCollectorsFor(cfg.UsageCollectors)
	usage := usagecollect.New(st, usageCollectors, usagecollect.Config{
		RefreshAge: usageRefreshAge, Logger: cfg.Logger,
	})
	// One sink serves both capture paths: a collected terminal (the engine)
	// and an unattended export (the driver's export recorder). It stays a nil
	// interface without the Claude collector, never a typed nil.
	var claudeUsageCaptures engine.ClaudeUsageSink
	if claudeUsage != nil {
		claudeUsageCaptures = claudeUsageSink{collector: claudeUsage, usage: usage}
	}
	return usageRefreshAge, usage, claudeUsageCaptures
}

// newAttentionService builds the attention service before any workflow
// exists. Options that answer from a later composition read it through
// late at call time.
func newAttentionService(
	st *store.Store, blobs *signet.BlobStore, cfg StartOptions, pairingKey []byte, hostName string,
	connectionMode domain.ConnectionMode, topicKey []byte,
	manualInitiator func(domain.ProjectID) (engine.ManualInitiator, bool),
	usage *usagecollect.Coordinator, usageRefreshAge time.Duration, doctorAvailable *atomic.Bool,
	late *StartupBindings,
) *signet.Service {
	attention := signet.NewService(st,
		signet.WithTaskStopGuard(func(stopCtx context.Context, taskID domain.TaskID, commit func() error) error {
			if late.Workflow == nil {
				return commit()
			}
			return late.Workflow.CommitTaskStop(stopCtx, taskID, commit)
		}),
		signet.WithFollowUpFiler(func() signet.FollowUpFiler {
			return FollowUpFilerBinding(late.ClaudeWiring)
		}),
		// An approved image-policy adoption wakes upkeep to build under it.
		signet.WithImageUpkeepWake(func() {
			if late.UpkeepTrigger != nil {
				late.UpkeepTrigger.Wake()
			}
		}),
		signet.WithPairingKey(pairingKey),
		signet.WithHostFacts(signet.HostFacts{DisplayName: hostName, ConnectionMode: connectionMode}),
		signet.WithClock(cfg.Now),
		signet.WithLogger(cfg.Logger),
		signet.WithDoctorSchedule(DoctorScheduleID, doctorAvailable.Load),
		signet.WithBlobStore(blobs),
		// Client task submission (plan §5.11) resolves each project's policy from
		// its operator-configured startup snapshot, independently of label intake.
		// An omitted manual-submission file disables new client tasks.
		signet.WithTaskSubmitter(engine.NewTaskSubmitter(blobs, manualInitiator)),
		// A client line change runs the engine's rule inside the command's
		// transaction, so the receipt commits with the line.
		signet.WithTaskLineChanger(func(
			ctx context.Context, tx *store.WriteTx, change signet.TaskLineChange, now time.Time,
		) (signet.TaskLineChangeOutcome, error) {
			result, err := engine.ChangeTaskLineTx(ctx, tx, engine.ChangeTaskLineRequest{
				TaskID: change.TaskID, Role: change.Role, Selection: change.Selection, Clear: change.Clear,
				Source: change.Source,
			}, now)
			return signet.TaskLineChangeOutcome{Recorded: result.Recorded, Line: result.Line}, err
		}),
		// A policy orders enrollments on the admitted lineup, which exists
		// only after the Claude or scenario composition; without one the
		// command fails closed. Signet has already translated the wire's
		// agent names to enrollments.
		signet.WithRoutingPolicySetter(func(
			ctx context.Context, tx *store.WriteTx, change signet.RoutingPolicyChange, now time.Time,
		) (signet.RoutingPolicyOutcome, error) {
			agents := late.Agents()
			if agents == nil {
				return signet.RoutingPolicyOutcome{}, signet.ErrOperatorSettingUnavailable
			}
			result, err := engine.SetRoutingPolicyTx(ctx, tx, agents.Tree, agents.LineupRevision,
				engine.SetRoutingPolicyRequest{
					Roles: change.Roles, DrainFirstAccount: change.DrainFirstAccount,
					Source: change.Source, SetBy: change.SetBy,
				}, now)
			return signet.RoutingPolicyOutcome{Recorded: result.Recorded, Policy: result.Policy}, err
		}),
		// An alternate-agent choice re-derives its rule against the admitted
		// lineup and the daemon's admission settings, which exist only after
		// the Claude or scenario composition; without one both commands
		// fail closed.
		signet.WithAlternateAgentRetrier(func(
			ctx context.Context, tx *store.WriteTx, command domain.Command, item domain.AttentionItem, now time.Time,
		) (domain.AlternateAgentRetry, error) {
			agents := late.Agents()
			if agents == nil {
				return domain.AlternateAgentRetry{}, signet.ErrOperatorSettingUnavailable
			}
			return agents.AcceptAlternateAgentRetryTx(ctx, tx, command, item, now)
		}),
		signet.WithParkedTaskMover(func(
			ctx context.Context, tx *store.WriteTx, request domain.ParkedTaskMoveRequest, now time.Time,
		) (domain.ParkedTaskMoveReceipt, error) {
			agents := late.Agents()
			if agents == nil {
				return domain.ParkedTaskMoveReceipt{}, signet.ErrOperatorSettingUnavailable
			}
			return agents.MoveParkedTaskTx(ctx, tx, request, now)
		}),
		// The catalog, like the tree it projects, exists only after the
		// Claude or scenario composition; until then, and in the fake-driver
		// composition that loads no tree, clients see null.
		signet.WithAgentCatalog(func() *signet.AgentCatalog { return late.AgentCatalog }),
		signet.WithUsageRefreshAge(usageRefreshAge),
		// A client's refresh or reassignment starts its collection only after
		// the command commits.
		signet.WithUsageOperator(usageOperator{coordinator: usage}),
		signet.WithNtfy(signet.NtfyConfig{
			BaseURL: cfg.NtfyURL, TopicKey: topicKey,
		}),
		// The effective digest exists only after the Claude composition
		// (ComposeClaudeWorkflow); the func indirection lets the
		// decision-time adoption gate read it then. The fake-driver
		// composition leaves it empty (gate absent); the scenario's review
		// source is the fake's default configuration.
		signet.WithEffectiveReviewConfiguration(func() domain.Digest {
			if late.ScenarioWiring != nil {
				return fake.DefaultReviewConfigurationDigest
			}
			if late.ClaudeWiring == nil {
				return ""
			}
			return late.ClaudeWiring.ReviewConfigurationDigest
		}),
	)
	return attention
}

// newScenarioParts gathers what ComposeScenario takes, including the values
// it needs from package main's helpers: the unattended admission floor,
// both delivery validators over one production materializer, the store's
// import authority and export recorder, the publication watches, and the
// Claude composition's agent expiry margin. Each but the margin is the value
// the scenario computed for itself before it left package main; the margin
// is the Claude composition's own constant.
func newScenarioParts(
	st *store.Store, attention *signet.Service, blobs *signet.BlobStore,
	lock *daemonlock.Lock, bound ward.LaunchBound, cfg StartOptions,
) (ScenarioParts, error) {
	materializer, err := ProductionMaterializer(blobs)
	if err != nil {
		return ScenarioParts{}, err
	}
	return ScenarioParts{
		Store: st, Attention: attention, Blobs: blobs, Lock: lock, Bound: bound,
		DBPath: cfg.DBPath, Logger: cfg.Logger, Scenario: cfg.Scenario,
		AdmissionFloor:                   AdmissionFloor(domain.ModeUnattended),
		ImplementationDeliveryValidation: ProductionImplementationDeliveryValidator(materializer),
		SpecificationDeliveryValidation:  ProductionSpecificationDeliveryValidator(materializer),
		Imports:                          admissionauth.Authority{Store: st, Blobs: blobs},
		Exports:                          ExportRecorder{Store: st},
		PublicationWatches:               PublicationWatchRegistrations(st),
		AgentExpiryMargin:                AgentExpiryMargin,
	}, nil
}

// newDoctorPass returns a doctor pass, which does nothing outside Claude
// mode. The startup pass takes a nil probe, so it reports recorded
// credential-integrity marks only and starting the daemon starts no
// observer container; scheduled passes take the live probe.
func newDoctorPass(
	cfg StartOptions, st *store.Store, attention *signet.Service, claudeWiring *ClaudeComposition,
	doctorConvergenceLock *sync.Mutex, probe CredentialIntegrityProbe,
) func(context.Context) error {
	return func(runCtx context.Context) error {
		if cfg.Claude == nil {
			return nil
		}
		// The probe runs before the lock is taken. It lasts as long as
		// its observer containers do, and the unattended engine waits on
		// this lock before every dispatch.
		replay := probe.runOnce(runCtx)
		doctorConvergenceLock.Lock()
		defer doctorConvergenceLock.Unlock()
		_, err := (operations.Doctor{
			Store: st, Attention: attention,
			ProjectID:                 domain.ProjectID("project-system"),
			Backend:                   domain.BackendFreshVMReadOnlyVolumeHandoff,
			ConfigurationDigest:       claudeWiring.Backend.ConfigurationDigest(),
			ReviewConfigurationDigest: claudeWiring.ReviewConfigurationDigest,
			Mode:                      cfg.Claude.OperatingMode,
			Now:                       cfg.Now,
			CredentialIntegrityProbe:  replay,
			IdentityLabel:             cfg.IdentityLabel,
		}).Run(runCtx)
		return err
	}
}

// startServing starts usage collection, model discovery, the API and
// control servers, the workflow loops, local backups, and attention
// deliveries. It cannot fail: start registers the cleanup that stops
// these goroutines as soon as it returns, and a component that exits
// later reports through ComponentExited.
func (d *Daemon) startServing(
	parent, ctx context.Context, cfg StartOptions, st *store.Store, attention *signet.Service,
	workflow *engine.Engine, claudeWiring *ClaudeComposition, localBackups *store.LocalBackupProducer,
	logger *slog.Logger,
) {
	// Usage collection starts before the control socket serves and the
	// scheduler runs, either of which can trigger it. It only observes and
	// ends only at shutdown, so it is not a supervised component: nothing it
	// meets stops the daemon.
	d.Usage.Start(ctx)
	d.Go(func() {
		d.Usage.Wait()
	})
	// Model discovery observes like usage collection: it runs once in the
	// background, one container at a time, and nothing it meets stops the
	// daemon.
	if claudeWiring != nil {
		refresh := newModelCatalogRefresh(st, *cfg.Claude, claudeWiring.ContainerBin,
			cfg.ClaudeModelDiscoverer, cfg.CodexModelDiscoverer, logger)
		d.Go(func() {
			refresh.run(ctx)
		})
	}
	d.StartServers(parent, ctx)
	if workflow != nil {
		d.Go(func() {
			d.ComponentExited(parent, ctx, daemonshell.ComponentWorkflow, workflow.RunTaskCancellations(ctx, cfg.ReconcileInterval))
		})
		d.Go(func() {
			d.ComponentExited(parent, ctx, daemonshell.ComponentWorkflow, workflow.Run(ctx, cfg.ReconcileInterval))
		})
	}
	d.Go(func() {
		d.ComponentExited(parent, ctx, daemonshell.ComponentLocalBackups, localBackups.Run(ctx))
	})
	if cfg.AttentionDeliveryInterval > 0 {
		d.Go(func() {
			err := attention.RunDeliveries(ctx, cfg.AttentionDeliveryInterval, func(err error) {
				reportAttentionDelivery(cfg.Logger, err)
			})
			if errors.Is(err, signet.ErrNotifierUnavailable) {
				// A channel that cannot publish is a daemon without
				// notifications, not a failed component: pairing already
				// fails closed on the same check, and the inbox still works.
				if cfg.Logger != nil {
					cfg.Logger.Warn("phone notifications are off", "error", err)
				}
				return
			}
			d.ComponentExited(parent, ctx, daemonshell.ComponentAttentionDeliveries, err)
		})
	}
}

// startClaudeLoops starts the Claude loops after start has registered the
// cleanup that stops them, and marks the doctor available before its
// scheduler starts.
func (d *Daemon) startClaudeLoops(
	parent, ctx context.Context, cfg StartOptions, claudeWiring *ClaudeComposition, workflow *engine.Engine,
	imageUpkeep *engine.ImageUpkeep, loops *ClaudeLoops, doctorAvailable *atomic.Bool, logger *slog.Logger,
) {
	claudeSched := loops.ClaudeSched
	activeReconciler := loops.ActiveReconciler
	intakeReconcilerLoop := loops.IntakeReconcilerLoop
	upkeepTrigger := loops.UpkeepTrigger
	d.Go(func() {
		err := claudeWiring.FollowUpFiler.Run(ctx, defaultFollowUpFilingInterval, func(err error) {
			if cfg.Logger != nil {
				cfg.Logger.Error("follow-up filing pass failed", "error", err)
			}
		})
		d.ComponentExited(parent, ctx, daemonshell.ComponentFollowUpFiling, err)
	})
	doctorAvailable.Store(true)
	d.Go(func() {
		err := claudeWiring.ReviewReplier.Run(ctx, time.Minute, func(err error) {
			if cfg.Logger != nil {
				cfg.Logger.Warn("external review reply sweep", "error", err)
			}
		})
		d.ComponentExited(parent, ctx, daemonshell.ComponentReviewReplies, err)
	})

	// The production publication lane gets its own loop: one task holds a
	// clone, a containerized verification, and GitHub calls for minutes,
	// which inside the reconcile loop would stall every other run,
	// invocation, and attention item for that whole span (issue #425).
	d.Go(func() {
		d.ComponentExited(parent, ctx, daemonshell.ComponentProductionPublication,
			workflow.RunProductionPublications(ctx, cfg.ReconcileInterval))
	})
	d.Go(func() {
		err := claudeSched.Run(ctx, cfg.SchedulerInterval)
		doctorAvailable.Store(false)
		d.ComponentExited(parent, ctx, daemonshell.ComponentScheduler, err)
	})
	d.Go(func() {
		err := activeReconciler.Run(ctx,
			DefaultActiveResourceInterval, OperatorActiveResourceInterval, cfg.Logger)
		d.ComponentExited(parent, ctx, daemonshell.ComponentActiveResource, err)
	})
	d.Go(func() {
		err := intakeReconcilerLoop.Run(ctx, DefaultIntakeInterval, cfg.Logger)
		d.ComponentExited(parent, ctx, daemonshell.ComponentLabelIntake, err)
	})
	// The start and tip-advance triggers: an upgrade's rebuild can take
	// an hour, so start does not wait for it.
	d.Go(func() {
		upkeepTrigger.Run(ctx, imageUpkeep, logger)
	})
}

// startScenarioLoops starts the scenario's forge, publication lane, and
// scheduler, at the point where the Claude loops would start.
func (d *Daemon) startScenarioLoops(
	parent, ctx context.Context, cfg StartOptions, scenarioWiring *ScenarioComposition, workflow *engine.Engine,
) {
	// The scenario's own loops: the in-process forge publication talks
	// to, the publication lane, and the durable scheduler's publication
	// watches. The forge is an HTTP server like the API, so its exit is
	// the same component failure.
	d.Go(func() {
		d.ComponentExited(parent, ctx, daemonshell.ComponentHTTP, scenarioWiring.ServeForge(ctx, cfg.Logger))
	})
	d.Go(func() {
		d.ComponentExited(parent, ctx, daemonshell.ComponentProductionPublication,
			workflow.RunProductionPublications(ctx, cfg.ReconcileInterval))
	})
	d.Go(func() {
		d.ComponentExited(parent, ctx, daemonshell.ComponentScheduler,
			scenarioWiring.Scheduler.Run(ctx, cfg.SchedulerInterval))
	})
}
