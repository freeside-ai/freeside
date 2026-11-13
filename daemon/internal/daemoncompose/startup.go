package daemoncompose

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/advisory"
	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/operations"
	"github.com/freeside-ai/freeside/daemon/internal/scheduler"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

const (
	DefaultGitHubAPIBase       = "https://api.github.com"
	DefaultGitHubRemoteBase    = "https://github.com"
	defaultJanitorInterval     = 30 * time.Second
	DefaultJanitorRemovalBound = 10
)

// ReviewAuthModes are the -review-auth-mode values: Codex delivers a
// subscription or an API key, Claude a setup token. Which of them suits the
// reviewer is checked against its line's harness (RequireReviewAuthMode).
var ReviewAuthModes = []ward.CodexAuthMode{
	ward.CodexAuthSubscription, ward.CodexAuthAPIKey, ward.CodexAuthSetupToken,
}

func LogEffectiveReviewConfiguration(logger *slog.Logger, digest domain.Digest) {
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

type SessionCloser interface {
	Close(context.Context) error
}

// TaskRuntimeCoverageFile is the coverage record's name under a daemon's
// state directory.
const TaskRuntimeCoverageFile = "task-runtime-coverage.json"

// OpenTaskRuntimeCoverage opens stateDir's task runtime coverage record. On
// a root's first open it writes the record, listing the tasks then in st as
// uncovered; later opens read it unchanged. The caller holds the database
// daemon lock, so no task is saved between the listing and the write. run()
// and a deputy start both open the record here, so they read one file with
// the same inputs.
func OpenTaskRuntimeCoverage(ctx context.Context, st *store.Store, stateDir string) (ward.CancellationCoverage, error) {
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
	return ward.OpenCancellationCoverage(filepath.Join(stateDir, TaskRuntimeCoverageFile), epoch, existingTasks)
}

// UnprovableTaskStop reports why no stop attempt can prove that task's
// runtime work is absent: the coverage refusal. The record is written once and
// freesided never changes the sync epoch while it runs, so the answer holds
// for as long as the state root does. A failed epoch read is an error, never a
// reason, because a reason ends retries of the stop for good.
func UnprovableTaskStop(ctx context.Context, st *store.Store, coverage ward.CancellationCoverage, task domain.TaskID) (string, error) {
	state, err := st.ServerState(ctx)
	if err != nil {
		return "", fmt.Errorf("server state: %w", err)
	}
	if err := coverage.Covers(task, state.SyncEpoch); err != nil {
		return "runtime coverage: " + err.Error(), nil
	}
	return "", nil
}

// StartupBindings holds the values startup composes after the attention
// service, whose options read them later. Each field is assigned at the
// point startup composes its value and stays nil in a composition that
// has none, so an option that runs earlier, or in another composition,
// takes its unavailable answer. Options read a field through the
// pointer at call time; one that copied a field at construction would
// keep nil forever.
type StartupBindings struct {
	Workflow       *engine.Engine
	ClaudeWiring   *ClaudeComposition
	AgentCatalog   *signet.AgentCatalog
	UpkeepTrigger  *imageUpkeepTrigger
	ScenarioWiring *ScenarioComposition
}

// Agents is the lineup the composed engine admits through: the Claude
// composition's, else the scenario composition's, else nil, which the
// operator hooks answer with signet.ErrOperatorSettingUnavailable.
func (b *StartupBindings) Agents() *engine.AgentSelection {
	switch {
	case b.ClaudeWiring != nil:
		return b.ClaudeWiring.env.Agents
	case b.ScenarioWiring != nil:
		return b.ScenarioWiring.Agents
	default:
		return nil
	}
}

// ClaudeStartupConfig is the part of the daemon's configuration the
// Claude composition and its loops read. Each field means what the
// config field of the same name does.
type ClaudeStartupConfig struct {
	Claude           *ClaudeDriverConfig
	Logger           *slog.Logger
	StateDir         string
	DBPath           string
	ApprovedRecipes  map[domain.Digest]bool
	IntakeInitiators []IntakeInitiator
	DoctorInterval   time.Duration
	// Now is the attention service clock; nil is the wall clock.
	Now func() time.Time
}

// ClaudeWorkflow is what the Claude composition hands the rest of startup.
type ClaudeWorkflow struct {
	ClaudeWiring *ClaudeComposition
	Workflow     *engine.Engine
	ImageUpkeep  *engine.ImageUpkeep
}

// ComposeClaudeWorkflow builds the Claude driver and the engine around it,
// then gives orphaned handoffs their first adoption attempt. It assigns
// *startupSessionCloser as soon as the driver exists, so run()'s cleanup
// awaits every credential-bearing session even when a later step here
// fails, and it publishes the wiring, the agent catalog, and the engine
// to late as it composes each.
func ComposeClaudeWorkflow(
	ctx context.Context, st *store.Store, attention *signet.Service, blobs *signet.BlobStore,
	lock *daemonlock.Lock, bound ward.LaunchBound, cfg ClaudeStartupConfig,
	manualProjects map[domain.ProjectID]engine.ManualInitiator, claudeUsageCaptures engine.ClaudeUsageSink,
	doctorConvergenceLock *sync.Mutex, startupSessionCloser *SessionCloser, late *StartupBindings,
	running *engine.RunningWork,
) (_ *ClaudeWorkflow, err error) {
	var (
		claudeWiring *ClaudeComposition
		workflow     *engine.Engine
		imageUpkeep  *engine.ImageUpkeep
	)
	claudeWiring, err = composeClaudeDriver(ctx, st, blobs, *cfg.Claude, claudeUsageCaptures, cfg.Logger)
	if err != nil {
		// A review role the lineup cannot fill stops startup: the review
		// sources cannot be composed without its identity. Leave the
		// item naming the role for the next start that gets far enough
		// to serve it.
		var roleFailure *RoleAdmissionError
		if errors.As(err, &roleFailure) {
			err = errors.Join(err, ReportRoleFailure(ctx, st, roleFailure, startupNow(cfg.Now)))
		}
		return nil, err
	}
	late.ClaudeWiring = claudeWiring
	LogEffectiveReviewConfiguration(cfg.Logger, claudeWiring.ReviewConfigurationDigest)
	// Reconcile below can resume credential-bearing sessions before every
	// later startup step has succeeded. Register their awaited cleanup
	// immediately, while the store they need is still open.
	*startupSessionCloser = claudeWiring.Closer
	// The tree already parsed, so a projection error is a daemon bug.
	catalog, err := signet.AgentCatalogOf(claudeWiring.env.Agents.Tree, claudeWiring.env.Agents.LineupRevision)
	if err != nil {
		return nil, err
	}
	late.AgentCatalog = &catalog
	stageDriver, err := claudeWiring.stageDriver(blobs)
	if err != nil {
		return nil, err
	}
	production := ProductionEngine{
		Lock: lock, Bound: bound,
		Backend: claudeWiring.Backend, AdmissionFloor: AdmissionFloor(cfg.Claude.OperatingMode),
		Admission: claudeWiring.env, AdmissionNow: func() time.Time { return time.Now().UTC() },
		Derive:           claudeWiring.derive,
		SelectImage:      claudeWiring.selectImage,
		HostVerification: claudeWiring.hostVerification,
		Logger:           cfg.Logger,
		UsageSink:        claudeUsageCaptures,
		Running:          running,
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
		// The ledger reserves each site's full bound per call, so the
		// per-root allowance is the call ceiling at the largest bound
		// (20 calls at the adjudicator's and drift auditor's 120 seconds)
		// and never parks a root the ceiling still admits, whatever the
		// mix of sites.
		MaxCallsPerRoot: 20, MaxStarvationPerRoot: 40 * time.Minute,
	}
	sites := JudgmentSites(judgmentBudget)
	advisoryStore, advisoryErr := openAdvisoryStore(cfg.StateDir, sites)
	var advisoryWriter inference.AdvisoryWriter = advisoryStore
	if advisoryErr != nil {
		advisoryWriter = advisory.Unavailable(advisoryErr)
		if cfg.Logger != nil {
			cfg.Logger.Warn("inference advisory store unavailable; judgment calls will fail safe", "error", advisoryErr)
		}
	}
	judgmentRuntime, ownedJudgments, err := composeProductionJudgments(cfg.StateDir, cfg.Claude.Judgments)
	if err != nil {
		return nil, err
	}
	if judgmentRuntime.Driver == nil && cfg.Logger != nil {
		cfg.Logger.Info("subscription judgments are off; every judgment site returns its fail-safe")
	}
	judgmentHealth := NewJudgmentRoleHealth(st, func() time.Time { return time.Now().UTC() }, cfg.Logger)
	judgments, err := inference.New(inference.Config{
		Logger:     cfg.Logger,
		StatePath:  filepath.Join(cfg.StateDir, "inference-budget.json"),
		AnchorPath: cfg.DBPath + ".inference-budget-anchor",
		Roles: JudgmentRoles{
			Store: st, Tree: claudeWiring.env.Agents.Tree, Revision: claudeWiring.env.Agents.LineupRevision,
			Runtime: judgmentRuntime,
			Credentials: productionJudgmentCredentials(
				st, *cfg.Claude, ward.NewCLIRuntime(claudeWiring.ContainerBin), cfg.Logger),
		},
		Health:   judgmentHealth,
		Sites:    sites,
		Advisory: advisoryWriter, Now: func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		return nil, fmt.Errorf("compose inference boundary: %w", err)
	}
	production.Inference = judgments
	researchFetcher, err := specify.NewFetcherWithIssueTokens(st, blobs, nil, claudeWiring.issueReadTokens)
	if err != nil {
		return nil, fmt.Errorf("compose research fetcher: %w", err)
	}
	deliveryMaterializer, err := ProductionMaterializer(blobs)
	if err != nil {
		return nil, fmt.Errorf("compose specification delivery validator: %w", err)
	}
	production.DeliveryValidation = ProductionImplementationDeliveryValidator(deliveryMaterializer)
	production.Specification = engine.SpecificationConfig{
		Fetcher: researchFetcher, Blobs: blobs, Now: func() time.Time { return time.Now().UTC() },
		PromptPackageDigest: claudeWiring.specificationPromptPackage,
		ValidateDelivery:    ProductionSpecificationDeliveryValidator(deliveryMaterializer),
	}
	var shadowReviewFailure func(domain.RunID, int, domain.ReviewFailureClass, error)
	if claudeWiring.shadowReviewSource != nil {
		shadowReviewFailure = func(runID domain.RunID, round int, class domain.ReviewFailureClass, err error) {
			if cfg.Logger != nil {
				cfg.Logger.Warn("shadow review abandoned", "run_id", runID, "round", round,
					"failure_class", class, "error", err)
			}
		}
	}
	verificationOwnership, err := ward.NewVerificationOwnership(
		filepath.Join(cfg.StateDir, "task-verification"), ward.NewCLIRuntime(claudeWiring.ContainerBin))
	if err != nil {
		return nil, err
	}
	coverage, err := OpenTaskRuntimeCoverage(ctx, st, cfg.StateDir)
	if err != nil {
		return nil, err
	}
	production.Cancellation = engine.TaskCancellationRuntime{
		Timeout: 2 * time.Minute,
		StopRun: func(stopCtx context.Context, run domain.Run, reviews []domain.ReviewRequestRecord) error {
			var joined error
			// Still request exact known teardown for an uncovered legacy task,
			// but never equate its missing private records with absence.
			if reason, err := UnprovableTaskStop(stopCtx, st, coverage, run.TaskID); err != nil {
				joined = errors.Join(joined, err)
			} else if reason != "" {
				joined = errors.Join(joined, errors.New(reason))
			}
			var mu sync.Mutex
			var children sync.WaitGroup
			stop := func(owner string, call func() error) {
				children.Go(func() {
					err := call()
					mu.Lock()
					if err != nil {
						joined = errors.Join(joined, fmt.Errorf("%s: %w", owner, err))
					}
					mu.Unlock()
				})
			}
			for _, stage := range run.Stages {
				for _, attempt := range stage.Attempts {
					stop("stage "+string(attempt.InvocationID), func() error {
						return claudeWiring.driver.CancelAndConfirm(stopCtx, attempt.InvocationID)
					})
				}
			}
			for _, review := range reviews {
				stop("review "+string(review.InvocationID), func() error {
					return engine.StopTaskReviews(stopCtx, st, review, claudeWiring.reviewSource, claudeWiring.shadowReviewSource)
				})
			}
			stop("verification", func() error { return verificationOwnership.StopRun(stopCtx, run.TaskID, run.ID) })
			stop("judgments", func() error { return ownedJudgments.ConfirmRun(stopCtx, run.TaskID, run.ID) })
			stop("publication", func() error { return claudeWiring.publisher.ReconcileCancelledRun(stopCtx, run.ID) })
			children.Wait()
			return joined
		},
		Unprovable: func(ctx context.Context, task domain.TaskID) (string, error) {
			return UnprovableTaskStop(ctx, st, coverage, task)
		},
	}
	rebuild, err := newProjectImageRebuild(*cfg.Claude, st)
	if err != nil {
		return nil, err
	}
	// Upkeep shares the lane's rebuild, and with it the one build gate.
	imageUpkeep, err = composeImageUpkeep(st, attention, blobs, cfg, claudeWiring, rebuild, manualProjects,
		doctorConvergenceLock)
	if err != nil {
		return nil, err
	}
	imageUpkeep.Running = running
	if claudeWiring.bindImageUpkeep != nil {
		claudeWiring.bindImageUpkeep(imageUpkeep.PendingResolution)
	}
	production.Publication = engine.ProductionPublicationConfig{
		WorkDir:   filepath.Join(cfg.Claude.SeedRoot, "production-publication"),
		Transport: claudeWiring.publicationTransport,
		Publisher: claudeWiring.publisher, Artifacts: blobs,
		ApprovedRecipes:                 cfg.ApprovedRecipes,
		RepositoryID:                    cfg.Claude.RepositoryID,
		RemediationPromptPackageDigest:  claudeWiring.remediationPromptPackage,
		ReviewSource:                    claudeWiring.reviewSource,
		ShadowReviewSource:              claudeWiring.shadowReviewSource,
		ReviewAdmission:                 claudeWiring.reviewAdmission,
		ReviewRecovery:                  claudeWiring.reviewRecovery,
		ReviewConfigurationDigest:       claudeWiring.ReviewConfigurationDigest,
		ShadowReviewConfigurationDigest: claudeWiring.shadowReviewConfigurationDigest,
		ShadowReviewDefaultRate: enabledShadowReviewRate(
			claudeWiring.shadowReviewSource, claudeWiring.shadowReviewRate),
		ShadowReviewFailure:    shadowReviewFailure,
		ReviewHostInstructions: claudeWiring.reviewHostInstructions,
		HoldOnly:               cfg.Claude.OperatingMode != domain.ModeUnattended,
		RebuildProjectImage:    rebuild,
		ImageUpkeepPending:     imageUpkeep.PendingResolution,
		OpenIssues:             claudeWiring.openIssues,
		NewRoom: func(image domain.ProjectImage, size ward.ContainerSize) (engine.ProductionVerificationRoom, error) {
			return ward.NewProjectImageRoom(claudeWiring.ContainerBin, image, size, cfg.Logger)
		},
		NewBoundRoom: func(image domain.ProjectImage, size ward.ContainerSize, run domain.Run, invocation domain.InvocationID) (engine.ProductionVerificationRoom, error) {
			room, err := ward.NewProjectImageRoom(claudeWiring.ContainerBin, image, size, cfg.Logger)
			if err != nil {
				return nil, err
			}
			if err := verificationOwnership.Bind(room, run.TaskID, run.ID, invocation); err != nil {
				return nil, err
			}
			return room, nil
		},
		ProveVerificationAbsent: func(ctx context.Context, run domain.Run, invocation domain.InvocationID) error {
			return verificationOwnership.ProveInvocationAbsent(ctx, run.TaskID, run.ID, invocation)
		},
		ProveVerificationContainersAbsent: func(ctx context.Context, run domain.Run, invocation domain.InvocationID) error {
			return verificationOwnership.ProveInvocationContainersAbsent(ctx, run.TaskID, run.ID, invocation)
		},
	}
	if cfg.Claude.OperatingMode == domain.ModeUnattended {
		production.HealthRefresh = func(refreshCtx context.Context) error {
			doctorConvergenceLock.Lock()
			defer doctorConvergenceLock.Unlock()
			return (operations.Doctor{
				Store: st, Attention: attention,
				ProjectID:                 domain.ProjectID("project-system"),
				ReviewConfigurationDigest: claudeWiring.ReviewConfigurationDigest,
				Mode:                      domain.ModeUnattended,
				Now:                       cfg.Now,
			}).ConvergeReviewConfiguration(refreshCtx)
		}
	}
	workflow, err = engine.New(st, attention, stageDriver, production.Options()...)
	if err != nil {
		return nil, err
	}
	late.Workflow = workflow
	// Before any loop admits: report why selection is not active, if it
	// is not.
	if err := ActivateAgentSelection(ctx, st, claudeWiring.selectionFailure, startupNow(cfg.Now)); err != nil {
		return nil, fmt.Errorf("activate agent selection: %w", err)
	}
	// The same for the judgment roles: name each one the lineup cannot
	// fill before a site returns its fail-safe for it, and resolve the
	// item of a role fixed since the last start.
	if err := CheckJudgmentRolesAtStartup(ctx, judgments, judgmentHealth, cfg.Logger); err != nil {
		return nil, fmt.Errorf("check judgment roles: %w", err)
	}
	claudeWiring.driver.SetRecoveryLauncher(workflow.ResumeTaskInvocation)
	claudeWiring.driver.SetRecoveryObserver(workflow.RecoverTaskInvocation)
	for _, source := range []exec.ReviewSource{claudeWiring.reviewSource, claudeWiring.shadowReviewSource} {
		if guarded, ok := source.(interface {
			SetTaskWorkGuard(func(context.Context, domain.RunID) (context.Context, func(), error))
		}); ok {
			guarded.SetTaskWorkGuard(workflow.BeginRunWork)
		}
	}
	// Give every orphan its first adoption attempt before the engine loop.
	// An operational retry or a mutable policy refusal holds the intent,
	// so the attempt's later Inspect passes drive the same recovery again
	// without a process restart. Any other failure quarantines only
	// that intent and raises its item, and the daemon still starts (#386).
	// An intent directory that cannot be listed, or an item that cannot
	// be written, still stops startup.
	recovery, reconcileErr := claudeWiring.driver.Reconcile(ctx, heldClaudeRecovery)
	if reconcileErr != nil {
		return nil, fmt.Errorf("reconcile orphaned handoffs: %w", reconcileErr)
	}
	if err := convergeStageIntentItems(ctx, st, recovery.Quarantined, startupNow(cfg.Now)); err != nil {
		return nil, fmt.Errorf("report quarantined stage intents: %w", err)
	}
	return &ClaudeWorkflow{ClaudeWiring: claudeWiring, Workflow: workflow, ImageUpkeep: imageUpkeep}, nil
}

// ClaudeLoops holds the Claude composition's background loops between
// their construction and their start.
type ClaudeLoops struct {
	ClaudeSched          *scheduler.Scheduler
	ActiveReconciler     *ActiveResourceReconciler
	IntakeReconcilerLoop *intakeReconciler
	UpkeepTrigger        *imageUpkeepTrigger
}

// ComposeClaudeLoops builds the Claude loops without starting any, so it
// runs before startup makes durable-stop recovery available, and
// publishes the image-upkeep trigger to late. collectUsage is the usage
// coordinator's collection pass, which the scheduled doctor pass starts.
func ComposeClaudeLoops(
	parent context.Context, st *store.Store, attention *signet.Service, blobs *signet.BlobStore, cfg ClaudeStartupConfig,
	claudeWiring *ClaudeComposition, workflow *engine.Engine, imageUpkeep *engine.ImageUpkeep,
	runDoctor func(context.Context) error, collectUsage func(context.Context) error, late *StartupBindings,
) (_ *ClaudeLoops, err error) {
	var (
		claudeSched          *scheduler.Scheduler
		activeReconciler     *ActiveResourceReconciler
		intakeReconcilerLoop *intakeReconciler
		upkeepTrigger        *imageUpkeepTrigger
	)
	// The doctor and janitor cadences live on the §5.16 durable scheduler;
	// their initial coverage remains the direct startup passes before this.
	claudeSched, err = newClaudeScheduler(st, cfg, claudeWiring, runDoctor, collectUsage, imageUpkeep)
	if err != nil {
		return nil, err
	}
	claudeSched.SetLogger(cfg.Logger)
	if err := armTrustedConfigJobs(parent, claudeSched, cfg); err != nil {
		return nil, err
	}
	upkeepTrigger = newImageUpkeepTrigger()
	late.UpkeepTrigger = upkeepTrigger
	activeReconciler = &ActiveResourceReconciler{
		Store: st, Pull: claudeWiring.observePull,
		issue:  claudeWiring.observeIssue,
		Review: claudeWiring.observeReview,
		// The default automated reviewer is wired here, not in the domain
		// (plan §5.16, §7; AGENTS.md, Automated reviewer).
		Reviewers:        map[string]bool{defaultNativeReviewerLogin: true},
		reviewInvalidate: claudeWiring.reviewInvalidate,
		appBotIdentity:   claudeWiring.appBotIdentity,
		handBackLabel:    cfg.Claude.HandBackLabel,
		mergeBase:        claudeWiring.mergeBase,
		evictConcluded:   claudeWiring.evictConcluded,
		tipWatch: repositoryTipWatch{
			Repo: cfg.Claude.Repo, RepositoryID: cfg.Claude.RepositoryID, Branch: cfg.Claude.BaseRef,
		},
		observeTip:       claudeWiring.observeRepositoryTip,
		upkeepTipAdvance: upkeepTrigger.tipAdvanced,
		Now:              func() time.Time { return time.Now().UTC() },
	}
	intakeReconcilerLoop = &intakeReconciler{
		store: st, blobs: blobs, engine: workflow, attention: attention,
		observeLabel: claudeWiring.observeLabelIssues,
		observeIssue: claudeWiring.observeIssue,
		evictLabel:   claudeWiring.evictLabelIssues,
		initiators:   cfg.IntakeInitiators,
		now:          func() time.Time { return time.Now().UTC() },
	}
	return &ClaudeLoops{
		ClaudeSched: claudeSched, ActiveReconciler: activeReconciler,
		IntakeReconcilerLoop: intakeReconcilerLoop, UpkeepTrigger: upkeepTrigger,
	}, nil
}
