// Command freesided serves the local control plane. Agent execution is disabled
// until a driver is selected; the permanent fake is an explicit demo/test mode.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/daemonshell"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
	"github.com/freeside-ai/freeside/daemon/internal/scheduler"
	"github.com/freeside-ai/freeside/daemon/internal/seedfixture"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/usagecollect"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

const defaultReconcileInterval = 100 * time.Millisecond

// Approval wakes the filer immediately; this tick bounds recovery delay.
const defaultFollowUpFilingInterval = time.Minute

const defaultNtfyURL = "https://ntfy.sh"

const defaultDoctorInterval = 24 * time.Hour

// defaultSchedulerInterval is the §5.16 scheduler's due-scan tick: how often
// durable schedules are checked for due fires, not any schedule's cadence.
const defaultSchedulerInterval = time.Second

// defaultAttentionDeliveryInterval is how often the freesided command looks
// for notifications it owes paired devices: the longest a new item waits
// before its first send. It is not the retry wait, which the sender owns.
const defaultAttentionDeliveryInterval = 5 * time.Second

const (
	defaultGitHubAPIBase       = "https://api.github.com"
	defaultGitHubRemoteBase    = "https://github.com"
	defaultJanitorInterval     = 30 * time.Second
	defaultJanitorRemovalBound = 10
)

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
// (resolveReviewSelection), so a command line that still passes
// -review-model, -review-reasoning-effort, -shadow-review-model, or
// -shadow-review-reasoning-effort is refused, not silently ignored.
type reviewFlagValues struct {
	Image, InputRoot, AuthMode, AuthSnapshot, Instructions string
	WorkspaceSizeMB                                        int64
	ShadowImage, ShadowAuthSnapshot                        string
	ShadowWorkspaceSizeMB                                  int64
	ShadowRate                                             float64
}

// reviewAuthModes are the -review-auth-mode values: Codex delivers a
// subscription or an API key, Claude a setup token. Which of them suits the
// reviewer is checked against its line's harness (requireReviewAuthMode).
var reviewAuthModes = []ward.CodexAuthMode{
	ward.CodexAuthSubscription, ward.CodexAuthAPIKey, ward.CodexAuthSetupToken,
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
		case "publication-formats":
			runPublicationFormatsMain(os.Args[2:])
			return
		case "swift-cache":
			runSwiftCacheMain(os.Args[2:])
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
	var judgmentsConfig judgmentConfig
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
		if err := listScenarios(os.Stdout); err != nil {
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
			if !slices.Contains(reviewAuthModes, reviewAuthMode) {
				fmt.Fprintf(os.Stderr,
					"freesided: -review-auth-mode %q is not subscription, api_key, or setup_token\n", review.AuthMode)
				os.Exit(2)
			}
		}
		daemonConfig.Claude = &claudeDriverConfig{
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
	IntakeInitiators []intakeInitiator
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
	// afterBackgroundStart is a test seam for startup failures after the base
	// background workers have launched. Production leaves it nil.
	afterBackgroundStart func() error
	// beforeStoreClose is a test seam for observing failed-start cleanup at
	// the store-close boundary. Production leaves it nil.
	beforeStoreClose func()
	// usageCollectors are the usage collectors by harness client. Production
	// leaves it nil and gets Claude's, which answers from writer turns'
	// usage captures; tests set a fake, and then no capture is recorded.
	usageCollectors map[domain.HarnessClientKind]ward.UsageCollector
	// claudeModelDiscoverer runs the startup model discovery. Production
	// leaves it nil and launches the initialize probe; tests set a fake.
	claudeModelDiscoverer claudeModelDiscoverer
	// codexModelDiscoverer runs the startup Codex model discovery. Production
	// leaves it nil and launches the model/list probe; tests set a fake.
	codexModelDiscoverer codexModelDiscoverer
	// now is the attention service clock. Production leaves it nil for the
	// wall clock; tests can advance startup across pairing-code expiry.
	now func() time.Time
	// Claude configures real execution and its ward gate. Without it, explicit
	// FakeDriverEnabled, or Scenario, the daemon serves setup and state
	// without an engine.
	Claude *claudeDriverConfig
	// Scenario plays one scripted run on the production engine composition
	// over fakes (-driver scenario). It is refused outside the ephemeral tier,
	// because the fake backend's conformance record claims a containment
	// nothing provides.
	Scenario *scenario
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

func logEffectiveReviewConfiguration(logger *slog.Logger, digest domain.Digest) {
	if logger == nil || digest == "" {
		return
	}
	logger.Info("effective reviewer configuration", "digest", digest)
}

func enabledShadowReviewRate(source exec.ReviewSource, configured float64) float64 {
	if source == nil {
		return 0
	}
	return configured
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
			domain.ModeUnattended: admissionCapabilitySnapshot(domain.ModeUnattended),
		}
		opts.ApprovedCredentialModes = []domain.CredentialMode{domain.CredentialSubscriptionContained}
		if opts.ApprovedRecipes == nil {
			opts.ApprovedRecipes = map[domain.Digest]bool{}
		}
		opts.ApprovedRecipes[verify.RecipeDigest(scenarioRecipe)] = true
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
				domain.ModeAttendedDev: admissionCapabilitySnapshot(domain.ModeAttendedDev),
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

type sessionCloser interface {
	Close(context.Context) error
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

// daemon is what run() composes: the shell that runs the process, and the
// composition results that startup and the tests reach.
type daemon struct {
	*daemonshell.Shell
	store     *store.Store
	attention *signet.Service
	workflow  *engine.Engine
	driver    *fake.StageDriver
	pairing   *pairingControl
	usage     *usagecollect.Coordinator
}

func run(parent context.Context, stop func(), cfg config) (_ *daemon, err error) {
	startedAt := time.Now().UTC()
	if err := validateRunConfig(cfg); err != nil {
		return nil, err
	}
	var fixture seedfixture.Name
	if cfg.SeedFixture != "" {
		if fixture, err = parseSeedFixture(cfg); err != nil {
			return nil, err
		}
	}
	manualProjects, err := readManualSubmissionProjects(cfg.ManualSubmissionConfigPath)
	if err != nil {
		return nil, err
	}
	manualInitiator := cfg.DeputyLaunch.gateManualInitiator(manualInitiatorLookup(manualProjects), cfg.Logger)
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
	pairing, err := newPairingControl(cfg.DBPath, cfg.StateDir)
	if err != nil {
		return nil, err
	}
	defer func() {
		if !lockTransferred {
			_ = pairing.Close()
		}
	}()
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
	st, topicKey, err := openStoreWithTopicKey(parent, cfg.DBPath, storeOptions)
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
	// The budget's checks wait for the store, because a budget recorded there
	// wins over the declared one.
	bound, err := cfg.startupLaunchBound(parent, st, manualProjects)
	if err != nil {
		return nil, err
	}
	logLaunchBound(cfg.Logger, bound)
	var startupSessionCloser sessionCloser
	defer func() {
		err = errors.Join(err, closeStartupSessions(success, startupSessionCloser, stop))
	}()
	if fixture != "" {
		if err := seedStartupFixture(parent, st, fixture, cfg); err != nil {
			return nil, err
		}
	}
	localBackups, err := maintainLocalBackups(parent, st, blobs, localBackupFiles, cfg)
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
		claudeWiring          *claudeComposition
		imageUpkeep           *engine.ImageUpkeep
		scenarioWiring        *scenarioComposition
		doctorConvergenceLock sync.Mutex
		doctorAvailable       atomic.Bool
		late                  = &startupBindings{}
	)
	usageRefreshAge, usage, claudeUsageCaptures := newUsageCollection(st, cfg)
	attention := newAttentionService(st, blobs, cfg, pairingKey, hostName, connectionMode, topicKey,
		manualInitiator, usage, usageRefreshAge, &doctorAvailable, late)
	if cfg.FakeDriverEnabled {
		workflow, err = composeFakeWorkflow(parent, st, attention, driver, lock, bound, cfg, late)
		if err != nil {
			return nil, err
		}
	} else if cfg.Claude != nil {
		var claude *claudeWorkflow
		claude, err = composeClaudeWorkflow(ctx, st, attention, blobs, lock, bound, cfg, manualProjects,
			claudeUsageCaptures, &doctorConvergenceLock, &startupSessionCloser, late)
		if err != nil {
			return nil, err
		}
		claudeWiring, workflow, imageUpkeep = claude.claudeWiring, claude.workflow, claude.imageUpkeep
	} else if cfg.Scenario != nil {
		scenarioWiring, err = composeScenario(ctx, st, attention, blobs, lock, cfg, bound)
		if err != nil {
			return nil, fmt.Errorf("compose scenario %s: %w", cfg.Scenario.Name, err)
		}
		late.scenarioWiring = scenarioWiring
		// The forge serves only once its loop starts; a start that fails
		// before then still owns the listener.
		defer func() {
			if err != nil {
				_ = scenarioWiring.listener.Close()
			}
		}()
		workflow = scenarioWiring.workflow
		late.workflow = workflow
	}
	// The live credential-integrity probe exists only in Claude mode, where
	// the daemon has a runtime and a pinned exporter image to observe with.
	var integrityProbe credentialIntegrityProbe
	if cfg.Claude != nil {
		integrityProbe = newCredentialIntegrityProbe(st,
			productionCredentialIntegrityObservers(*cfg.Claude, ward.NewCLIRuntime(claudeWiring.containerBin)),
			cfg.now, cfg.Logger)
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
	if err := convergeExecutionConfiguration(parent, st, workflow != nil, cfg.now); err != nil {
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
			Status: "ok", Version: buildVersion(), StartedAt: startedAt,
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	// Shutdown releases open revision long-polls, so draining the server
	// stays inside ServerShutdownBudget instead of waiting out their bound.
	server.RegisterOnShutdown(attention.Shutdown)
	var sessions daemonshell.SessionCloser
	if claudeWiring != nil {
		sessions = claudeWiring.closer
	}
	d := &daemon{
		Shell: daemonshell.New(daemonshell.Parts{
			Lock: lock, Store: st, Listener: listener, LoopbackTwin: loopbackTwin, Server: server,
			Pairing: pairing, SessionCloser: sessions, Cancel: cancel, Logger: logger, Now: cfg.now,
		}),
		store: st, attention: attention, workflow: workflow, driver: driver, pairing: pairing, usage: usage,
	}
	d.pairing.configure(d.Readiness().APIURL, attention.MintPairingCode)
	// The control routes approve the store's recipe set, which a scenario
	// extends with its own recipe, and the recipes adopted for the Claude
	// driver's repository; with no Claude driver there is none.
	var repositoryID int64
	if cfg.Claude != nil {
		repositoryID = cfg.Claude.RepositoryID
	}
	d.pairing.registerControlRoutes(
		d.pairing.mux, st, blobs, localBackupFiles, storeOptions.ApprovedRecipes, repositoryID, integrityProbe, d.usage,
		cfg.Environment,
		cfg.execution(), attention, cfg.DeputyLaunch, func(ctx context.Context, id domain.TaskID, commit func() error) error {
			if workflow == nil {
				return commit()
			}
			return workflow.CommitTaskWorkflowClosure(ctx, id, commit)
		})
	var fakeSched *scheduler.Scheduler
	var loops *claudeLoops
	if cfg.FakeDriverEnabled {
		// Construct every loop before making restart-gated recovery available.
		// None is running yet, so no operator can resume into a partial start.
		fakeSched, err = newFakeScheduler(st)
		if err != nil {
			return nil, err
		}
		fakeSched.SetLogger(cfg.Logger)
	} else if claudeWiring != nil {
		loops, err = d.composeClaudeLoops(parent, st, attention, blobs, cfg, claudeWiring, workflow, imageUpkeep,
			runDoctor, late)
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
	if cfg.afterBackgroundStart != nil {
		if err := cfg.afterBackgroundStart(); err != nil {
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
	if err := d.PublishReadiness(cfg.StateDir, pairingCode, string(cfg.Environment)); err != nil {
		return nil, err
	}
	if d.pairing != nil {
		if err := d.pairing.publish(); err != nil {
			return nil, err
		}
	}
	success = true
	lockTransferred = true
	return d, nil
}

// taskRuntimeCoverageFile is the coverage record's name under a daemon's
// state directory.
const taskRuntimeCoverageFile = "task-runtime-coverage.json"

// openTaskRuntimeCoverage opens stateDir's task runtime coverage record. On
// a root's first open it writes the record, listing the tasks then in st as
// uncovered; later opens read it unchanged. The caller holds the database
// daemon lock, so no task is saved between the listing and the write. run()
// and a deputy start both open the record here, so they read one file with
// the same inputs.
func openTaskRuntimeCoverage(ctx context.Context, st *store.Store, stateDir string) (ward.CancellationCoverage, error) {
	var existingTasks []domain.TaskID
	var epoch string
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		state, err := tx.ServerState(ctx)
		if err != nil {
			return err
		}
		epoch = state.SyncEpoch
		tasks, err := tx.ListTasks(ctx)
		for _, task := range tasks {
			existingTasks = append(existingTasks, task.Value.ID)
		}
		return err
	}); err != nil {
		return ward.CancellationCoverage{}, err
	}
	return ward.OpenCancellationCoverage(filepath.Join(stateDir, taskRuntimeCoverageFile), epoch, existingTasks)
}

// unprovableTaskStop reports why no stop attempt can prove that task's
// runtime work is absent: the coverage refusal. The record is written once and
// freesided never changes the sync epoch while it runs, so the answer holds
// for as long as the state root does. A failed epoch read is an error, never a
// reason, because a reason ends retries of the stop for good.
func unprovableTaskStop(ctx context.Context, st *store.Store, coverage ward.CancellationCoverage, task domain.TaskID) (string, error) {
	state, err := st.ServerState(ctx)
	if err != nil {
		return "", fmt.Errorf("server state: %w", err)
	}
	if err := coverage.Covers(task, state.SyncEpoch); err != nil {
		return "runtime coverage: " + err.Error(), nil
	}
	return "", nil
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
