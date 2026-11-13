// Command freesided serves the local control plane. Agent execution is disabled
// until a driver is selected; the permanent fake is an explicit demo/test mode.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/signal"
	"os/user"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/daemonshell"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
	"github.com/freeside-ai/freeside/daemon/internal/seedfixture"
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

// defaultAttentionDeliveryInterval is how often the freesided command looks
// for notifications it owes paired devices: the longest a new item waits
// before its first send. It is not the retry wait, which the sender owns.
const defaultAttentionDeliveryInterval = 5 * time.Second

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

// currentAccountHome comes from the operating system's account record rather
// than the caller-controlled HOME environment variable.
func currentAccountHome() (string, error) {
	account, err := user.Current()
	if err != nil {
		return "", err
	}
	if account.HomeDir == "" {
		return "", errors.New("current account has no home directory")
	}
	return account.HomeDir, nil
}

// reviewFlagValues are the daemon's review flags. No model and no effort is
// among them: each review role's agent names the pair its launch passes
// (ResolveReviewSelection), so a command line that still passes
// -review-model, -review-reasoning-effort, -shadow-review-model, or
// -shadow-review-reasoning-effort is refused, not silently ignored.
type reviewFlagValues struct {
	Image, InputRoot, AuthMode, AuthSnapshot, Instructions string
	WorkspaceSizeMB                                        int64
	ShadowImage, ShadowAuthSnapshot                        string
	ShadowWorkspaceSizeMB                                  int64
	ShadowRate                                             float64
}

func reviewFlags(flags *flag.FlagSet, v *reviewFlagValues) {
	flags.StringVar(&v.Image, "review-image", "", "digest-pinned review image for the reviewer line's harness")
	flags.StringVar(&v.InputRoot, "review-input-root", "", "private root containing review auth and instruction snapshots")
	flags.StringVar(&v.AuthMode, "review-auth-mode", "", "review auth mode: subscription or api_key (Codex), setup_token (Claude)")
	flags.StringVar(&v.AuthSnapshot, "review-auth-snapshot", "", "reviewer credential snapshot under review-input-root (Codex auth.json or Claude setup token)")
	flags.StringVar(&v.Instructions, "review-instructions", "", "operator-host review instruction source snapshotted with explicit absence")
	flags.Int64Var(&v.WorkspaceSizeMB, "review-workspace-size-mb", 8192, "review workspace volume size")
	flags.StringVar(&v.ShadowImage, "shadow-review-image", "", "digest-pinned Claude shadow review image (enables the shadow arm)")
	flags.StringVar(&v.ShadowAuthSnapshot, "shadow-review-auth-snapshot", "", "Claude setup-token snapshot under review-input-root")
	flags.Int64Var(&v.ShadowWorkspaceSizeMB, "shadow-review-workspace-size-mb", 8192, "Claude shadow review workspace volume size")
	flags.Float64Var(&v.ShadowRate, "shadow-review-rate", 0.2, "fallback Claude shadow review sampling rate in [0,1]")
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
		case "preflight":
			runPreflightMain(os.Args[2:])
			return
		case "submit":
			runSubmitMain(os.Args[2:])
			return
		case "reattempt":
			runReattemptMain(os.Args[2:])
			return
		case "abandon":
			runAbandonMain(os.Args[2:])
			return
		case "stop":
			runStopMain(os.Args[2:])
			return
		case "drain":
			runDrainMain(os.Args[2:])
			return
		case "raise-item":
			runRaiseItemMain(os.Args[2:])
			return
		case "resume":
			runResumeMain(os.Args[2:])
			return
		case "follow":
			runFollowMain(os.Args[2:])
			return
		case "inspect":
			runInspectMain(os.Args[2:])
			return
		case "pairing-code":
			runPairingCodeMain(os.Args[2:])
			return
		case "notify-test":
			runNotifyTestMain(os.Args[2:])
			return
		case "renew-codex":
			runRenewCodexMain(os.Args[2:])
			return
		case "enroll-codex":
			runEnrollCodexMain(os.Args[2:])
			return
		case "approve-shadow-review":
			runApproveShadowReviewMain(os.Args[2:])
			return
		case "auth":
			runAuthMain(os.Args[2:])
			return
		case "set-pool-limit":
			runSetPoolLimitMain(os.Args[2:])
			return
		case "set-memory-budget":
			runSetMemoryBudgetMain(os.Args[2:])
			return
		case "release-verification-memory":
			runReleaseVerificationMemoryMain(os.Args[2:])
			return
		case "task-line":
			runTaskLineMain(os.Args[2:])
			return
		case "routing":
			runRoutingMain(os.Args[2:])
			return
		case "rig":
			runRigMain(os.Args[2:])
			return
		case "macverify":
			runMacverifyMain(os.Args[2:])
			return
		case "deputy":
			runDeputyMain(os.Args[2:])
			return
		case "prelaunch":
			runPrelaunchMain(os.Args[2:])
			return
		case "probe-lineup":
			runProbeLineupMain(os.Args[2:])
			return
		case "comprehension":
			runComprehensionMain(os.Args[2:])
			return
		case "snapshot":
			runSnapshotMain(os.Args[2:])
			return
		case "check-database":
			runCheckDatabaseMain(os.Args[2:])
			return
		case "publication-formats":
			runPublicationFormatsMain(os.Args[2:])
			return
		case "swift-cache":
			runSwiftCacheMain(os.Args[2:])
			return
		case "go-cache":
			runGoCacheMain(os.Args[2:])
			return
		}
	}
	flags := flag.NewFlagSet("freesided", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	dbPath := flags.String("db", "", "SQLite database path (required; created if absent)")
	manualSubmissionConfigPath := flags.String("manual-submission-config", "", "operator JSON project policies for new client tasks (loaded once at startup)")
	driverDir := flags.String("fake-driver-dir", "", "permanent fake StageDriver state directory (defaults beside -db)")
	listenAddr := flags.String("listen", "127.0.0.1:0", "signet listener address (loopback or Tailscale-owned address only; a Tailscale address also opens 127.0.0.1 at the same port for same-host clients)")
	ntfyURL := flags.String("ntfy-url", defaultNtfyURL, "ntfy server URL for device notifications")
	logLevel := flags.String("log-level", defaultLogLevel,
		"loop log severity: debug, info, warn, or error (debug adds a record per loop iteration)")
	interval := flags.Duration("reconcile-interval", defaultReconcileInterval, "workflow reconciliation interval")
	publicationStateDir := flags.String("publication-state-dir", "", "GitHub App authority state directory")
	publicationCredentialsDir := flags.String("publication-credentials-dir", "", "GitHub App credential directory")
	driverMode := flags.String("driver", "disabled",
		"execution: disabled (setup only), fake (explicit demo), claude (real agents), "+
			"or scenario (a scripted run on the production pipeline; ephemeral only)")
	scenarioName := flags.String("scenario", "", "with -driver scenario: the scenario to play (see -list-scenarios)")
	listScenariosOnly := flags.Bool("list-scenarios", false, "print the scenarios -driver scenario plays, one per line, and exit")
	var judgmentsConfig daemoncompose.JudgmentConfig
	judgmentFlags(flags, &judgmentsConfig)
	flags.StringVar(&judgmentsConfig.ExpectedDigest, "judgment-configuration-digest", "", "required preflight digest when subscription judgments are enabled")
	seedWalkingSkeleton := flags.Bool("seed-walking-skeleton", false,
		"seed the 1A.0 walking-skeleton demo run under the fake driver (off by default so a production store stays empty, #1127)")
	seedFixture := flags.String("seed-fixture", "",
		"load a named development fixture into an empty ephemeral store (requires -driver disabled): representative")
	agentImage := flags.String("agent-image", "", "digest-pinned Claude agent image")
	exporterImage := flags.String("exporter-image", "", "digest-pinned export helper image")
	containerBin := flags.String("container-bin", "container", "Apple container CLI path")
	seedRoot := flags.String("seed-root", "", "daemon-owned exact-base checkout root")
	baseBuildRef := flags.String("base-build-ref", "",
		"local tag of the approved agent base image; enables rebuilding a project image "+
			"when a candidate changes its dependencies within the project's declared policy")
	buildProxy := flags.String("build-proxy", "",
		"with -base-build-ref: build-only HTTP proxy URL without credentials, overriding the managed build proxy")
	var buildDNS repeatedStringFlag
	flags.Var(&buildDNS, "build-dns", "with -base-build-ref: build DNS server; repeatable")
	writerStopTimeout := flags.Duration("writer-stop-timeout", 0,
		"max time the implementation writer container may run before it must reach stopped; 0 uses the ward default (10m)")
	writerStallInterval := flags.Duration("writer-stall-interval", 0,
		"time a running writer may go without a provider response before an advisory invocation_stalled notice is raised; "+
			"never changes -writer-stop-timeout; 0 uses the ward default (5m)")
	stateDir := flags.String("state-dir", "", "production driver state directory")
	rigTokenFile := flags.String("rig-token-file", "", "production rig acquisition file (optional)")
	providerEndpoints := flags.String("provider-endpoints", "api.anthropic.com:443", "comma-separated provider host:port allowlist")
	promptPackage := flags.String("prompt-package", "", "trusted prompt-package file (ingested into the artifact store at startup)")
	specificationPromptPackage := flags.String("specification-prompt-package", "", "trusted specifier prompt-package file (ingested into the artifact store at startup)")
	remediationPromptPackage := flags.String("remediation-prompt-package", "", "trusted remediator prompt-package file (ingested into the artifact store at startup)")
	vendorInstructions := flags.String("vendor-instructions", "", "host vendor-instruction file (CLAUDE.md)")
	repo := flags.String("repo", "", "managed owner/name repository")
	baseRef := flags.String("base-ref", "", "managed repository base branch")
	baseSHA := flags.String("base-sha", "", "exact 40-character base commit tasks run against")
	allowedPaths := flags.String("allowed-paths", "", "comma-separated candidate path allowlist (required in claude driver mode)")
	var review reviewFlagValues
	reviewFlags(flags, &review)
	agentTree := flags.String("agent-tree", "",
		"local checkout holding the admitted-agent tree under policy/; its lineup selects the agent and identity every role runs under (required in claude driver mode)")
	agentTreeCommit := flags.String("agent-tree-commit", "",
		"exact commit of -agent-tree whose tree is read; the working tree is never read")
	runConformance := flags.Bool("run-conformance", false,
		"run and durably record the full ward suite for this exact Claude configuration before admission")
	hostVerification := macverifyExecutorFlags(flags)
	operatingMode := flags.String(
		"operating-mode", string(domain.ModeAttendedDev),
		"operating mode: attended_dev (default) or unattended")
	environmentFlag := flags.String("environment", string(defaultEnvironment),
		"environment tier: prod, dev, or ephemeral (default; refuses supervised roots and ports)")
	memoryBudget := flags.Int("memory-budget-mib", 0,
		"memory budget in MiB every container launch reserves against; prod defaults to half of physical memory, and dev or ephemeral requires it with -driver claude")
	prodAppAuthority := flags.Bool("prod-app-authority", false,
		"ephemeral only: use prod's own App directories as -publication-state-dir and -publication-credentials-dir while holding the production rig lease (-rig-token-file)")
	prodDaemon := flags.String("prod-daemon", "",
		"with -prod-app-authority: the installed prod freesided whose publication formats this build must match")
	doctorInterval := flags.Duration(
		"doctor-interval", defaultDoctorInterval,
		"scheduled operational-health cadence in production driver mode")
	schedulerInterval := flags.Duration(
		"scheduler-interval", defaultSchedulerInterval,
		"durable-scheduler due-scan tick in production driver mode")
	attentionDeliveryInterval := flags.Duration(
		"attention-delivery-interval", defaultAttentionDeliveryInterval,
		"how often to send the notifications owed to paired devices (0 turns the sender off)")
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
	if *listScenariosOnly {
		if err := daemoncompose.ListScenarios(os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "freesided:", err)
			os.Exit(1)
		}
		return
	}
	logger, err := newLogger(os.Stderr, *logLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, "freesided:", err)
		os.Exit(2)
	}
	env, err := parseEnvironment(*environmentFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "freesided:", err)
		os.Exit(2)
	}
	execution := executionMode(*driverMode)
	if !execution.valid() {
		fmt.Fprintf(os.Stderr, "freesided: -driver %q is not disabled, fake, claude, or scenario\n", *driverMode)
		os.Exit(2)
	}
	memoryBudgetMiB, err := resolveMemoryBudget(env, execution == executionClaude, *memoryBudget, physicalMemoryBytes)
	if err != nil {
		fmt.Fprintln(os.Stderr, "freesided:", err)
		os.Exit(2)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "freesided:", err)
		os.Exit(2)
	}
	paths := environmentPaths{
		DB:                        *dbPath,
		StateDir:                  *stateDir,
		PublicationStateDir:       *publicationStateDir,
		PublicationCredentialsDir: *publicationCredentialsDir,
		ReviewInputRoot:           review.InputRoot,
		ProdAppAuthority:          *prodAppAuthority,
	}
	if *prodDaemon != "" && !*prodAppAuthority {
		fmt.Fprintln(os.Stderr, "freesided: -prod-daemon requires -prod-app-authority")
		os.Exit(2)
	}
	if *prodAppAuthority {
		accountHome, err := currentAccountHome()
		if err != nil {
			fmt.Fprintln(os.Stderr, "freesided:", err)
			os.Exit(2)
		}
		leaseRoot, err := daemonlock.DefaultRigLeaseRoot()
		if err != nil {
			fmt.Fprintln(os.Stderr, "freesided:", err)
			os.Exit(2)
		}
		if err := checkProdAppAuthority(context.Background(), env, home, accountHome, *rigTokenFile, leaseRoot, *prodDaemon); err != nil {
			fmt.Fprintln(os.Stderr, "freesided:", err)
			os.Exit(2)
		}
	}
	if err := checkEnvironment(env, home, paths, *listenAddr); err != nil {
		fmt.Fprintln(os.Stderr, "freesided:", err)
		os.Exit(2)
	}
	if env == environmentEphemeral {
		accountHome, err := currentAccountHome()
		if err != nil {
			fmt.Fprintln(os.Stderr, "freesided:", err)
			os.Exit(2)
		}
		if err := checkEnvironment(env, accountHome, paths, *listenAddr); err != nil {
			fmt.Fprintln(os.Stderr, "freesided:", err)
			os.Exit(2)
		}
	}
	if *prodAppAuthority {
		err := checkProdDriverless(context.Background(), newProductionRigHost().ProdExecution, paths.PublicationStateDir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "freesided:", err)
			os.Exit(2)
		}
	}
	if env == environmentProd {
		// Before anything opens the database: a real-work run may be using
		// prod's App authority (#1583), which only a driver would open (#2080).
		leaseRoot, err := daemonlock.DefaultRigLeaseRoot()
		if err != nil {
			fmt.Fprintln(os.Stderr, "freesided:", err)
			os.Exit(2)
		}
		if err := checkProdDriverUnderLease(leaseRoot, execution); err != nil {
			fmt.Fprintln(os.Stderr, "freesided:", err)
			os.Exit(2)
		}
	}
	logger.Info("environment", "tier", env)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	daemonConfig := config{
		Environment:                env,
		ManualSubmissionConfigPath: *manualSubmissionConfigPath,
		DBPath:                     *dbPath, FakeDriverDir: *driverDir, StateDir: *stateDir,
		ListenAddr: *listenAddr, NtfyURL: *ntfyURL, ReconcileInterval: *interval,
		DoctorInterval:                     *doctorInterval,
		SchedulerInterval:                  *schedulerInterval,
		AttentionDeliveryInterval:          *attentionDeliveryInterval,
		ApprovedRecipes:                    approvedRecipes,
		BackupEncryptionWaiverRepositoryID: backupEncryptionWaiverRepositoryID.Value(),
		SeedWalkingSkeleton:                *seedWalkingSkeleton,
		SeedFixture:                        *seedFixture,
		MemoryBudgetMiB:                    memoryBudgetMiB,
		Logger:                             logger,
	}
	mode, err := parseOperatingMode(*operatingMode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "freesided: -operating-mode %q is not attended_dev or unattended\n", *operatingMode)
		os.Exit(2)
	}
	played, err := scenarioFromFlags(execution, *scenarioName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "freesided:", err)
		os.Exit(2)
	}
	daemonConfig.Scenario = played
	switch execution {
	case executionDisabled:
	case executionFake:
		daemonConfig.FakeDriverEnabled = true
	case executionScenario:
		// scenarioFromFlags set daemonConfig.Scenario above.
	case executionClaude:
		id := int64(0)
		if v := repositoryID.Value(); v != nil {
			id = *v
		}
		var reviewAuthMode ward.CodexAuthMode
		if mode == domain.ModeUnattended {
			reviewAuthMode = ward.CodexAuthMode(review.AuthMode)
			if !slices.Contains(daemoncompose.ReviewAuthModes, reviewAuthMode) {
				fmt.Fprintf(os.Stderr,
					"freesided: -review-auth-mode %q is not subscription, api_key, or setup_token\n", review.AuthMode)
				os.Exit(2)
			}
		}
		daemonConfig.Claude = &daemoncompose.ClaudeDriverConfig{
			AgentImage: domain.ImageRef(*agentImage), ExporterImage: *exporterImage,
			ContainerBin: *containerBin, SeedRoot: *seedRoot,
			WriterStopTimeout: *writerStopTimeout, WriterStallInterval: *writerStallInterval,
			Rebuild: projectimage.BuildInputs{
				BaseBuildRef: *baseBuildRef, BuildProxy: *buildProxy, DNS: buildDNS,
			},
			StateDir: *stateDir, RigTokenFile: *rigTokenFile, Judgments: judgmentsConfig,
			ProviderEndpoints:              strings.Split(*providerEndpoints, ","),
			PromptPackageFile:              *promptPackage,
			SpecificationPromptPackageFile: *specificationPromptPackage,
			RemediationPromptPackageFile:   *remediationPromptPackage,
			VendorInstructions:             *vendorInstructions,
			Repo:                           *repo, RepositoryID: id,
			BaseRef: *baseRef, BaseSHA: *baseSHA,
			AgentTreeCheckout: *agentTree, AgentTreeCommit: *agentTreeCommit,
			AllowedPaths:   engine.SplitNonEmpty(*allowedPaths),
			RunConformance: *runConformance,
			StateRoot:      *publicationStateDir, CredentialsDir: *publicationCredentialsDir,
			OperatingMode: mode,
			ReviewImage:   review.Image, ReviewInputRoot: review.InputRoot,
			ReviewAuthMode:     reviewAuthMode,
			ReviewAuthSnapshot: review.AuthSnapshot, ReviewInstructions: review.Instructions,
			ReviewWorkspaceSizeMB:       review.WorkspaceSizeMB,
			ShadowReviewImage:           review.ShadowImage,
			ShadowReviewAuthSnapshot:    review.ShadowAuthSnapshot,
			ShadowReviewWorkspaceSizeMB: review.ShadowWorkspaceSizeMB,
			ShadowReviewRate:            review.ShadowRate,
			HostVerification:            hostVerification(),
		}
	}
	h, err := run(ctx, stop, daemonConfig)
	if err != nil {
		fmt.Fprintln(os.Stderr, "freesided:", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(h.Readiness()); err != nil {
		// Restore signal disposition before teardown here too, for the reason
		// serve documents: an unstopped Close keeps absorbing the operator's
		// SIGTERM, leaving SIGKILL as the only way out of a slow teardown.
		stop()
		_ = h.Close()
		fmt.Fprintln(os.Stderr, "freesided:", err)
		os.Exit(1)
	}

	if err := daemonshell.Serve(ctx, stop, h.Shell); err != nil {
		fmt.Fprintln(os.Stderr, "freesided:", err)
		os.Exit(1)
	}
}

// executionMode is what a daemon's -driver lets it run. Every daemon reports
// it on its control socket (GET /execution), so a real-work run can prove the
// prod it shares an App authority with runs nothing (#2080).
type executionMode string

const (
	executionDisabled executionMode = "disabled"
	executionFake     executionMode = "fake"
	executionClaude   executionMode = "claude"
	executionScenario executionMode = "scenario"
)

var AllExecutionModes = []executionMode{
	executionDisabled,
	executionFake,
	executionClaude,
	executionScenario,
}

func (mode executionMode) valid() bool {
	switch mode {
	case executionDisabled, executionFake, executionClaude, executionScenario:
		return true
	default:
		return false
	}
}

type config struct {
	// Environment is the tier this daemon runs as. It is stamped into the
	// readiness handshake so an app of another tier refuses to follow it
	// (#1504); the zero value is rejected so no start publishes an empty stamp.
	// It also gates SeedFixture and the ephemeral driverless admission floor
	// in storeOptions.
	Environment                        environment
	ManualSubmissionConfigPath         string
	DBPath                             string
	FakeDriverEnabled                  bool
	FakeDriverDir                      string
	StateDir                           string
	ListenAddr                         string
	NtfyURL                            string
	ReconcileInterval                  time.Duration
	DoctorInterval                     time.Duration
	SchedulerInterval                  time.Duration
	ApprovedRecipes                    map[domain.Digest]bool
	BackupEncryptionWaiverRepositoryID *int64
	// SeedWalkingSkeleton seeds the 1A.0 walking-skeleton demo run at startup
	// under an explicitly selected fake driver. Seeding alone never enables it.
	SeedWalkingSkeleton bool
	// SeedFixture names a seedfixture loaded into an empty store at startup
	// (#1503). It is refused outside the ephemeral tier and with any driver,
	// so fixture state never reaches a supervised store and the seeding start
	// runs no engine. The fixture's dispatch intents are recorded already
	// dispatched, so a later driver start on the store has none to execute.
	SeedFixture string
	// IntakeInitiators are the configured label initiators the label-intake
	// reconciler observes (#659). Empty leaves the loop supervised but idle; the
	// rein resolver and workflow-definition parsing that populate it are a later
	// unit, so a composition (or a test) supplies them directly for now.
	IntakeInitiators []daemoncompose.IntakeInitiator
	// MemoryBudgetMiB is the declared budget for every ward launch's memory
	// (resolveMemoryBudget). A budget recorded with freesided
	// set-memory-budget wins over it (startupLaunchBound). Zero, with none
	// recorded, runs unbudgeted: nothing is reserved and no launch waits for
	// memory.
	MemoryBudgetMiB int
	// AttentionDeliveryInterval is how often the daemon sends the
	// notifications it owes paired devices (#1924). Zero leaves the sender
	// off, so a test that pairs a device neither publishes to the hosted
	// default server nor has its items' entity versions move under the
	// commands it prepares.
	//
	// The freesided command sets it from -attention-delivery-interval, so
	// the sender runs there unless the operator passes 0. A delivery changes
	// an item's timing and entity_version and leaves its item_version alone,
	// so engine work bound to the item stays bound
	// (devlog/2026-10-11-1750-timing-keeps-item-version.md).
	AttentionDeliveryInterval time.Duration
	// Logger is the process logger the long-running loops report through.
	// Nil discards their records, which keeps every test composition quiet
	// without each one having to build a handler.
	Logger *slog.Logger
	// seams are the start's test seams (startSeams). Production leaves them
	// unset.
	seams daemoncompose.StartSeams
	// Claude configures real execution and its ward gate. Without it, explicit
	// FakeDriverEnabled, or Scenario, the daemon serves setup and state
	// without an engine.
	Claude *daemoncompose.ClaudeDriverConfig
	// Scenario plays one scripted run on the production engine composition
	// over fakes (-driver scenario). It is refused outside the ephemeral tier,
	// because the fake backend's conformance record claims a containment
	// nothing provides.
	Scenario *daemoncompose.Scenario
	// DeputyLaunch is set only by deputy: the composition this daemon serves
	// under, which every new submission must bind (deputyLaunch.admit).
	DeputyLaunch *deputyLaunch
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

// execution is the mode this configuration runs: a Claude driver, the fake
// driver, or neither.
func (cfg config) execution() executionMode {
	switch {
	case cfg.Claude != nil:
		return executionClaude
	case cfg.FakeDriverEnabled:
		return executionFake
	case cfg.Scenario != nil:
		return executionScenario
	default:
		return executionDisabled
	}
}

func (cfg config) storeOptions() (store.Options, error) {
	opts := store.Options{
		ApprovedRecipes:  maps.Clone(cfg.ApprovedRecipes),
		ExclusiveLocking: cfg.Environment.locksDatabaseExclusively(),
	}
	if cfg.BackupEncryptionWaiverRepositoryID != nil {
		return store.Options{}, fmt.Errorf(
			"-backup-encryption-waiver-repository-id: %w",
			domain.ErrBackupEncryptionWaiverUnsupported)
	}
	if cfg.Scenario != nil {
		// The scenario runs unattended, so its store re-gates admissions
		// against the unattended floor, as a Claude-mode store does.
		opts.AdmissionFloors = map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeUnattended: daemoncompose.AdmissionCapabilitySnapshot(domain.ModeUnattended),
		}
		opts.ApprovedCredentialModes = []domain.CredentialMode{domain.CredentialSubscriptionContained}
		if opts.ApprovedRecipes == nil {
			opts.ApprovedRecipes = map[domain.Digest]bool{}
		}
		opts.ApprovedRecipes[verify.RecipeDigest(daemoncompose.ScenarioRecipe)] = true
		return opts, nil
	}
	if cfg.Claude == nil {
		if cfg.Environment == environmentEphemeral && !cfg.FakeDriverEnabled {
			// An ephemeral daemon without a driver admits nothing itself, so
			// the only admissions in its store are seeded fixture records.
			// The attended_dev floor lets those read back, on the seeding
			// start and on every restart of the same store; without it the
			// re-gate fails them closed. Decision:
			// devlog/2026-09-25-1608-seed-fixture-admission-floor.md.
			opts.AdmissionFloors = map[domain.OperatingMode]domain.CapabilitySnapshot{
				domain.ModeAttendedDev: daemoncompose.AdmissionCapabilitySnapshot(domain.ModeAttendedDev),
			}
		}
		return opts, nil
	}
	// The store re-gates every recorded admission against the operator's
	// policy, so a claude-mode daemon has to configure that policy or its own
	// admissions are refused at persistence: an unset floor is "no policy
	// configured", which fails closed, not "no minimum". The floor here is
	// the same mode-specific minimum the engine admits against.
	opts.AdmissionFloors = map[domain.OperatingMode]domain.CapabilitySnapshot{
		cfg.Claude.OperatingMode: daemoncompose.AdmissionCapabilitySnapshot(cfg.Claude.OperatingMode),
	}
	opts.ApprovedCredentialModes = []domain.CredentialMode{domain.CredentialSubscriptionContained}
	return opts, nil
}

// daemon is what run() starts: the started daemon, and the pairing control
// its control socket serves, which a test reads.
type daemon struct {
	*daemoncompose.Daemon
	pairing *pairingControl
}

// run translates the configuration into start options and starts the
// daemon. It validates and defaults cfg and reads the files the flags name
// before start acquires anything, so a refused configuration leaves no
// lock, listener, or store behind. Everything a start acquires, and every
// cleanup of a failed start, belongs to start.
func run(parent context.Context, stop func(), cfg config) (*daemon, error) {
	if err := validateRunConfig(cfg); err != nil {
		return nil, err
	}
	var fixture seedfixture.Name
	if cfg.SeedFixture != "" {
		var err error
		if fixture, err = parseSeedFixture(cfg); err != nil {
			return nil, err
		}
	}
	manualProjects, err := readManualSubmissionProjects(cfg.ManualSubmissionConfigPath, cfg.Logger)
	if err != nil {
		return nil, err
	}
	manualInitiator := cfg.DeputyLaunch.gateManualInitiator(manualInitiatorLookup(manualProjects), cfg.Logger)
	if cfg.StateDir == "" && cfg.Claude != nil {
		cfg.StateDir = cfg.Claude.StateDir
	}
	if cfg.FakeDriverEnabled && cfg.FakeDriverDir == "" {
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
	if cfg.AttentionDeliveryInterval < 0 {
		return nil, fmt.Errorf("negative attention delivery interval %s", cfg.AttentionDeliveryInterval)
	}
	if cfg.seams.Now == nil {
		cfg.seams.Now = time.Now
	}
	if cfg.NtfyURL == "" {
		cfg.NtfyURL = defaultNtfyURL
	}
	storeOptions, err := cfg.storeOptions()
	if err != nil {
		return nil, err
	}
	// The control routes approve the store's recipe set, which a scenario
	// extends with its own recipe, and the recipes adopted for the Claude
	// driver's repository; with no Claude driver there is none.
	var repositoryID int64
	if cfg.Claude != nil {
		repositoryID = cfg.Claude.RepositoryID
	}
	var pairing *pairingControl
	started, err := daemoncompose.Start(parent, stop, daemoncompose.StartOptions{
		DBPath: cfg.DBPath, StateDir: cfg.StateDir, ListenAddr: cfg.ListenAddr,
		FakeDriverEnabled: cfg.FakeDriverEnabled, FakeDriverDir: cfg.FakeDriverDir, NtfyURL: cfg.NtfyURL,
		ReconcileInterval: cfg.ReconcileInterval, DoctorInterval: cfg.DoctorInterval,
		SchedulerInterval: cfg.SchedulerInterval, AttentionDeliveryInterval: cfg.AttentionDeliveryInterval,
		ApprovedRecipes: cfg.ApprovedRecipes, SeedWalkingSkeleton: cfg.SeedWalkingSkeleton, SeedFixture: fixture,
		IntakeInitiators: cfg.IntakeInitiators, MemoryBudgetMiB: cfg.MemoryBudgetMiB, Logger: cfg.Logger,
		Claude: cfg.Claude, Scenario: cfg.Scenario,
		ManualProjects: manualProjects, ManualInitiator: manualInitiator, StoreOptions: storeOptions,
		ReadinessEnvironment: string(cfg.Environment), Version: buildVersion(),
		IdentityLabel: credentialIdentityLabel,
		WrapFakeDriver: func(driver *fake.StageDriver) exec.StageDriver {
			return autoScriptStageDriver{StageDriver: driver}
		},
		OpenControl: func(dbPath, stateDir string) (daemoncompose.ControlSocket, error) {
			opened, err := newPairingControl(dbPath, stateDir)
			if err != nil {
				return nil, err
			}
			pairing = opened
			return pairingControlSocket{
				pairing: opened, repositoryID: repositoryID, environment: cfg.Environment,
				execution: cfg.execution(), launch: cfg.DeputyLaunch,
			}, nil
		},
		StartSeams: cfg.seams,
	})
	if err != nil {
		return nil, err
	}
	return &daemon{Daemon: started, pairing: pairing}, nil
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
