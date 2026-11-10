package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/advisory"
	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/daemonshell"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/operations"
	"github.com/freeside-ai/freeside/daemon/internal/scheduler"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/usagecollect"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// startupBindings holds the values startup composes after the attention
// service, whose options read them later. Each field is assigned at the
// point startup composes its value and stays nil in a composition that
// has none, so an option that runs earlier, or in another composition,
// takes its unavailable answer. Options read a field through the
// pointer at call time; one that copied a field at construction would
// keep nil forever.
type startupBindings struct {
	workflow       *engine.Engine
	claudeWiring   *claudeComposition
	agentCatalog   *signet.AgentCatalog
	upkeepTrigger  *imageUpkeepTrigger
	scenarioWiring *daemoncompose.ScenarioComposition
}

// agents is the lineup the composed engine admits through: the Claude
// composition's, else the scenario composition's, else nil, which the
// operator hooks answer with signet.ErrOperatorSettingUnavailable.
func (b *startupBindings) agents() *engine.AgentSelection {
	switch {
	case b.claudeWiring != nil:
		return b.claudeWiring.env.Agents
	case b.scenarioWiring != nil:
		return b.scenarioWiring.Agents
	default:
		return nil
	}
}

// validateRunConfig checks the flag combinations that no default can
// repair. It runs before startup acquires anything, so a refused
// configuration leaves no lock, listener, or store behind.
func validateRunConfig(cfg config) error {
	if !cfg.Environment.valid() {
		return fmt.Errorf("environment %q is not prod, dev, or ephemeral", cfg.Environment)
	}
	if cfg.DBPath == "" {
		return errors.New("-db is required")
	}
	if cfg.SeedWalkingSkeleton && !cfg.FakeDriverEnabled {
		return errors.New("-seed-walking-skeleton requires -driver fake")
	}
	if cfg.Scenario != nil {
		// Before the lock or the store: a non-ephemeral store must never
		// receive the fake backend's conformance claim.
		if cfg.Environment != environmentEphemeral {
			return fmt.Errorf("-driver scenario requires -environment %s, not %q",
				environmentEphemeral, cfg.Environment)
		}
		if cfg.FakeDriverEnabled || cfg.Claude != nil {
			return errors.New("-driver scenario cannot run beside another driver")
		}
	}
	return nil
}

// newUsageCollection builds the usage coordinator, the age at which a
// reading goes stale, and the Claude capture sink, which stays a nil
// interface without the Claude collector. Collection starts in
// startServing.
func newUsageCollection(
	st *store.Store, cfg config,
) (time.Duration, *usagecollect.Coordinator, engine.ClaudeUsageSink) {
	// A reading is stale once one scheduled doctor pass, the collection's
	// cadence, has been missed. Collection and the bootstrap's stale_at share
	// the age, so a client sees a reading stale exactly when collection does.
	usageRefreshAge := 2 * cfg.DoctorInterval
	usageCollectors, claudeUsage := usageCollectorsFor(cfg.usageCollectors)
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
	st *store.Store, blobs *signet.BlobStore, cfg config, pairingKey []byte, hostName string,
	connectionMode domain.ConnectionMode, topicKey []byte,
	manualInitiator func(domain.ProjectID) (engine.ManualInitiator, bool),
	usage *usagecollect.Coordinator, usageRefreshAge time.Duration, doctorAvailable *atomic.Bool,
	late *startupBindings,
) *signet.Service {
	attention := signet.NewService(st,
		signet.WithTaskStopGuard(func(stopCtx context.Context, taskID domain.TaskID, commit func() error) error {
			if late.workflow == nil {
				return commit()
			}
			return late.workflow.CommitTaskStop(stopCtx, taskID, commit)
		}),
		signet.WithFollowUpFiler(func() signet.FollowUpFiler {
			return followUpFilerBinding(late.claudeWiring)
		}),
		// An approved image-policy adoption wakes upkeep to build under it.
		signet.WithImageUpkeepWake(func() {
			if late.upkeepTrigger != nil {
				late.upkeepTrigger.wake()
			}
		}),
		signet.WithPairingKey(pairingKey),
		signet.WithHostFacts(signet.HostFacts{DisplayName: hostName, ConnectionMode: connectionMode}),
		signet.WithClock(cfg.now),
		signet.WithLogger(cfg.Logger),
		signet.WithDoctorSchedule(doctorScheduleID, doctorAvailable.Load),
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
			agents := late.agents()
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
			agents := late.agents()
			if agents == nil {
				return domain.AlternateAgentRetry{}, signet.ErrOperatorSettingUnavailable
			}
			return agents.AcceptAlternateAgentRetryTx(ctx, tx, command, item, now)
		}),
		signet.WithParkedTaskMover(func(
			ctx context.Context, tx *store.WriteTx, request domain.ParkedTaskMoveRequest, now time.Time,
		) (domain.ParkedTaskMoveReceipt, error) {
			agents := late.agents()
			if agents == nil {
				return domain.ParkedTaskMoveReceipt{}, signet.ErrOperatorSettingUnavailable
			}
			return agents.MoveParkedTaskTx(ctx, tx, request, now)
		}),
		// The catalog, like the tree it projects, exists only after the
		// Claude or scenario composition; until then, and in the fake-driver
		// composition that loads no tree, clients see null.
		signet.WithAgentCatalog(func() *signet.AgentCatalog { return late.agentCatalog }),
		signet.WithUsageRefreshAge(usageRefreshAge),
		// A client's refresh or reassignment starts its collection only after
		// the command commits.
		signet.WithUsageOperator(usageOperator{coordinator: usage}),
		signet.WithNtfy(signet.NtfyConfig{
			BaseURL: cfg.NtfyURL, TopicKey: topicKey,
		}),
		// The effective digest exists only after the Claude composition
		// (composeClaudeWorkflow); the func indirection lets the
		// decision-time adoption gate read it then. The fake-driver
		// composition leaves it empty (gate absent); the scenario's review
		// source is the fake's default configuration.
		signet.WithEffectiveReviewConfiguration(func() domain.Digest {
			if late.scenarioWiring != nil {
				return fake.DefaultReviewConfigurationDigest
			}
			if late.claudeWiring == nil {
				return ""
			}
			return late.claudeWiring.reviewConfigurationDigest
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
	lock *daemonlock.Lock, bound ward.LaunchBound, cfg config,
) (daemoncompose.ScenarioParts, error) {
	materializer, err := productionMaterializer(blobs)
	if err != nil {
		return daemoncompose.ScenarioParts{}, err
	}
	return daemoncompose.ScenarioParts{
		Store: st, Attention: attention, Blobs: blobs, Lock: lock, Bound: bound,
		DBPath: cfg.DBPath, Logger: cfg.Logger, Scenario: cfg.Scenario,
		AdmissionFloor:                   admissionFloor(domain.ModeUnattended),
		ImplementationDeliveryValidation: productionImplementationDeliveryValidator(materializer),
		SpecificationDeliveryValidation:  productionSpecificationDeliveryValidator(materializer),
		Imports:                          storeAdmissionAuthority{store: st, blobs: blobs},
		Exports:                          exportRecorder{store: st},
		PublicationWatches:               publicationWatchRegistrations(st),
		AgentExpiryMargin:                agentExpiryMargin,
	}, nil
}

// claudeWorkflow is what the Claude composition hands the rest of startup.
type claudeWorkflow struct {
	claudeWiring *claudeComposition
	workflow     *engine.Engine
	imageUpkeep  *engine.ImageUpkeep
}

// composeClaudeWorkflow builds the Claude driver and the engine around it,
// then gives orphaned handoffs their first adoption attempt. It assigns
// *startupSessionCloser as soon as the driver exists, so run()'s cleanup
// awaits every credential-bearing session even when a later step here
// fails, and it publishes the wiring, the agent catalog, and the engine
// to late as it composes each.
func composeClaudeWorkflow(
	ctx context.Context, st *store.Store, attention *signet.Service, blobs *signet.BlobStore,
	lock *daemonlock.Lock, bound ward.LaunchBound, cfg config,
	manualProjects map[domain.ProjectID]engine.ManualInitiator, claudeUsageCaptures engine.ClaudeUsageSink,
	doctorConvergenceLock *sync.Mutex, startupSessionCloser *sessionCloser, late *startupBindings,
) (_ *claudeWorkflow, err error) {
	var (
		claudeWiring *claudeComposition
		workflow     *engine.Engine
		imageUpkeep  *engine.ImageUpkeep
	)
	claudeWiring, err = composeClaudeDriver(ctx, st, blobs, *cfg.Claude, claudeUsageCaptures, cfg.Logger)
	if err != nil {
		// A review role the lineup cannot fill stops startup: the review
		// sources cannot be composed without its identity. Leave the
		// item naming the role for the next start that gets far enough
		// to serve it.
		var roleFailure *roleAdmissionError
		if errors.As(err, &roleFailure) {
			err = errors.Join(err, reportRoleFailure(ctx, st, roleFailure, startupNow(cfg.now)))
		}
		return nil, err
	}
	late.claudeWiring = claudeWiring
	logEffectiveReviewConfiguration(cfg.Logger, claudeWiring.reviewConfigurationDigest)
	// Reconcile below can resume credential-bearing sessions before every
	// later startup step has succeeded. Register their awaited cleanup
	// immediately, while the store they need is still open.
	*startupSessionCloser = claudeWiring.closer
	// The tree already parsed, so a projection error is a daemon bug.
	catalog, err := signet.AgentCatalogOf(claudeWiring.env.Agents.Tree, claudeWiring.env.Agents.LineupRevision)
	if err != nil {
		return nil, err
	}
	late.agentCatalog = &catalog
	stageDriver, err := claudeWiring.stageDriver(blobs)
	if err != nil {
		return nil, err
	}
	production := daemoncompose.ProductionEngine{
		Lock: lock, Bound: bound,
		Backend: claudeWiring.backend, AdmissionFloor: admissionFloor(cfg.Claude.OperatingMode),
		Admission: claudeWiring.env, AdmissionNow: func() time.Time { return time.Now().UTC() },
		Derive:      claudeWiring.derive,
		SelectImage: claudeWiring.selectImage,
		Logger:      cfg.Logger,
		UsageSink:   claudeUsageCaptures,
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
	sites := judgmentSites(judgmentBudget)
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
	judgmentHealth := newJudgmentRoleHealth(st, func() time.Time { return time.Now().UTC() }, cfg.Logger)
	judgments, err := inference.New(inference.Config{
		Logger:     cfg.Logger,
		StatePath:  filepath.Join(cfg.StateDir, "inference-budget.json"),
		AnchorPath: cfg.DBPath + ".inference-budget-anchor",
		Roles: judgmentRoles{
			st: st, tree: claudeWiring.env.Agents.Tree, revision: claudeWiring.env.Agents.LineupRevision,
			runtime: judgmentRuntime,
			credentials: productionJudgmentCredentials(
				st, *cfg.Claude, ward.NewCLIRuntime(claudeWiring.containerBin), cfg.Logger),
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
	deliveryMaterializer, err := productionMaterializer(blobs)
	if err != nil {
		return nil, fmt.Errorf("compose specification delivery validator: %w", err)
	}
	production.DeliveryValidation = productionImplementationDeliveryValidator(deliveryMaterializer)
	production.Specification = engine.SpecificationConfig{
		Fetcher: researchFetcher, Blobs: blobs, Now: func() time.Time { return time.Now().UTC() },
		PromptPackageDigest: claudeWiring.specificationPromptPackage,
		ValidateDelivery:    productionSpecificationDeliveryValidator(deliveryMaterializer),
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
		filepath.Join(cfg.StateDir, "task-verification"), ward.NewCLIRuntime(claudeWiring.containerBin))
	if err != nil {
		return nil, err
	}
	coverage, err := openTaskRuntimeCoverage(ctx, st, cfg.StateDir)
	if err != nil {
		return nil, err
	}
	production.Cancellation = engine.TaskCancellationRuntime{
		Timeout: 2 * time.Minute,
		StopRun: func(stopCtx context.Context, run domain.Run, reviews []domain.ReviewRequestRecord) error {
			var joined error
			// Still request exact known teardown for an uncovered legacy task,
			// but never equate its missing private records with absence.
			if reason, err := unprovableTaskStop(stopCtx, st, coverage, run.TaskID); err != nil {
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
			return unprovableTaskStop(ctx, st, coverage, task)
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
		ReviewConfigurationDigest:       claudeWiring.reviewConfigurationDigest,
		ShadowReviewConfigurationDigest: claudeWiring.shadowReviewConfigurationDigest,
		ShadowReviewDefaultRate: enabledShadowReviewRate(
			claudeWiring.shadowReviewSource, claudeWiring.shadowReviewRate),
		ShadowReviewFailure:    shadowReviewFailure,
		ReviewHostInstructions: claudeWiring.reviewHostInstructions,
		HoldOnly:               cfg.Claude.OperatingMode != domain.ModeUnattended,
		RebuildProjectImage:    rebuild,
		ImageUpkeepPending:     imageUpkeep.PendingResolution,
		NewRoom: func(image domain.ProjectImage, size ward.ContainerSize) (engine.ProductionVerificationRoom, error) {
			return ward.NewProjectImageRoom(claudeWiring.containerBin, image, size, cfg.Logger)
		},
		NewBoundRoom: func(image domain.ProjectImage, size ward.ContainerSize, run domain.Run, invocation domain.InvocationID) (engine.ProductionVerificationRoom, error) {
			room, err := ward.NewProjectImageRoom(claudeWiring.containerBin, image, size, cfg.Logger)
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
	}
	if cfg.Claude.OperatingMode == domain.ModeUnattended {
		production.HealthRefresh = func(refreshCtx context.Context) error {
			doctorConvergenceLock.Lock()
			defer doctorConvergenceLock.Unlock()
			return (operations.Doctor{
				Store: st, Attention: attention,
				ProjectID:                 domain.ProjectID("project-system"),
				ReviewConfigurationDigest: claudeWiring.reviewConfigurationDigest,
				Mode:                      domain.ModeUnattended,
				Now:                       cfg.now,
			}).ConvergeReviewConfiguration(refreshCtx)
		}
	}
	workflow, err = engine.New(st, attention, stageDriver, production.Options()...)
	if err != nil {
		return nil, err
	}
	late.workflow = workflow
	// Before any loop admits: report why selection is not active, if it
	// is not.
	if err := activateAgentSelection(ctx, st, claudeWiring.selectionFailure, startupNow(cfg.now)); err != nil {
		return nil, fmt.Errorf("activate agent selection: %w", err)
	}
	// The same for the judgment roles: name each one the lineup cannot
	// fill before a site returns its fail-safe for it, and resolve the
	// item of a role fixed since the last start.
	if err := checkJudgmentRolesAtStartup(ctx, judgments, judgmentHealth, cfg.Logger); err != nil {
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
	if err := convergeStageIntentItems(ctx, st, recovery.Quarantined, startupNow(cfg.now)); err != nil {
		return nil, fmt.Errorf("report quarantined stage intents: %w", err)
	}
	return &claudeWorkflow{claudeWiring: claudeWiring, workflow: workflow, imageUpkeep: imageUpkeep}, nil
}

// newDoctorPass returns a doctor pass, which does nothing outside Claude
// mode. The startup pass takes a nil probe, so it reports recorded
// credential-integrity marks only and starting the daemon starts no
// observer container; scheduled passes take the live probe.
func newDoctorPass(
	cfg config, st *store.Store, attention *signet.Service, claudeWiring *claudeComposition,
	doctorConvergenceLock *sync.Mutex, probe credentialIntegrityProbe,
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
			ConfigurationDigest:       claudeWiring.backend.ConfigurationDigest(),
			ReviewConfigurationDigest: claudeWiring.reviewConfigurationDigest,
			Mode:                      cfg.Claude.OperatingMode,
			Now:                       cfg.now,
			CredentialIntegrityProbe:  replay,
			IdentityLabel:             credentialIdentityLabel,
		}).Run(runCtx)
		return err
	}
}

// claudeLoops holds the Claude composition's background loops between
// their construction and their start.
type claudeLoops struct {
	claudeSched          *scheduler.Scheduler
	activeReconciler     *activeResourceReconciler
	intakeReconcilerLoop *intakeReconciler
	upkeepTrigger        *imageUpkeepTrigger
}

// composeClaudeLoops builds the Claude loops without starting any, so it
// runs before startup makes durable-stop recovery available, and
// publishes the image-upkeep trigger to late.
func (d *daemon) composeClaudeLoops(
	parent context.Context, st *store.Store, attention *signet.Service, blobs *signet.BlobStore, cfg config,
	claudeWiring *claudeComposition, workflow *engine.Engine, imageUpkeep *engine.ImageUpkeep,
	runDoctor func(context.Context) error, late *startupBindings,
) (_ *claudeLoops, err error) {
	var (
		claudeSched          *scheduler.Scheduler
		activeReconciler     *activeResourceReconciler
		intakeReconcilerLoop *intakeReconciler
		upkeepTrigger        *imageUpkeepTrigger
	)
	// The doctor and janitor cadences live on the §5.16 durable scheduler;
	// their initial coverage remains the direct startup passes before this.
	claudeSched, err = newClaudeScheduler(st, cfg, claudeWiring, runDoctor, d.usage.CollectAll, imageUpkeep)
	if err != nil {
		return nil, err
	}
	claudeSched.SetLogger(cfg.Logger)
	if err := armTrustedConfigJobs(parent, claudeSched, cfg); err != nil {
		return nil, err
	}
	upkeepTrigger = newImageUpkeepTrigger()
	late.upkeepTrigger = upkeepTrigger
	activeReconciler = &activeResourceReconciler{
		store: st, pull: claudeWiring.observePull,
		issue:  claudeWiring.observeIssue,
		review: claudeWiring.observeReview,
		// The default automated reviewer is wired here, not in the domain
		// (plan §5.16, §7; AGENTS.md, Automated reviewer).
		reviewers:        map[string]bool{defaultNativeReviewerLogin: true},
		reviewInvalidate: claudeWiring.reviewInvalidate,
		evictConcluded:   claudeWiring.evictConcluded,
		tipWatch: repositoryTipWatch{
			Repo: cfg.Claude.Repo, RepositoryID: cfg.Claude.RepositoryID, Branch: cfg.Claude.BaseRef,
		},
		observeTip:       claudeWiring.observeRepositoryTip,
		upkeepTipAdvance: upkeepTrigger.tipAdvanced,
		now:              func() time.Time { return time.Now().UTC() },
	}
	intakeReconcilerLoop = &intakeReconciler{
		store: st, blobs: blobs, engine: workflow, attention: attention,
		observeLabel: claudeWiring.observeLabelIssues,
		observeIssue: claudeWiring.observeIssue,
		evictLabel:   claudeWiring.evictLabelIssues,
		initiators:   cfg.IntakeInitiators,
		now:          func() time.Time { return time.Now().UTC() },
	}
	return &claudeLoops{
		claudeSched: claudeSched, activeReconciler: activeReconciler,
		intakeReconcilerLoop: intakeReconcilerLoop, upkeepTrigger: upkeepTrigger,
	}, nil
}

// startServing starts usage collection, model discovery, the API and
// control servers, the workflow loops, local backups, and attention
// deliveries. It cannot fail: run() registers the cleanup that stops
// these goroutines as soon as it returns, and a component that exits
// later reports through ComponentExited.
func (d *daemon) startServing(
	parent, ctx context.Context, cfg config, st *store.Store, attention *signet.Service,
	workflow *engine.Engine, claudeWiring *claudeComposition, localBackups *store.LocalBackupProducer,
	logger *slog.Logger,
) {
	// Usage collection starts before the control socket serves and the
	// scheduler runs, either of which can trigger it. It only observes and
	// ends only at shutdown, so it is not a supervised component: nothing it
	// meets stops the daemon.
	d.usage.Start(ctx)
	d.Go(func() {
		d.usage.Wait()
	})
	// Model discovery observes like usage collection: it runs once in the
	// background, one container at a time, and nothing it meets stops the
	// daemon.
	if claudeWiring != nil {
		refresh := newModelCatalogRefresh(st, *cfg.Claude, claudeWiring.containerBin,
			cfg.claudeModelDiscoverer, cfg.codexModelDiscoverer, logger)
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

// startClaudeLoops starts the Claude loops after run() has registered the
// cleanup that stops them, and marks the doctor available before its
// scheduler starts.
func (d *daemon) startClaudeLoops(
	parent, ctx context.Context, cfg config, claudeWiring *claudeComposition, workflow *engine.Engine,
	imageUpkeep *engine.ImageUpkeep, loops *claudeLoops, doctorAvailable *atomic.Bool, logger *slog.Logger,
) {
	claudeSched := loops.claudeSched
	activeReconciler := loops.activeReconciler
	intakeReconcilerLoop := loops.intakeReconcilerLoop
	upkeepTrigger := loops.upkeepTrigger
	d.Go(func() {
		err := claudeWiring.followUpFiler.Run(ctx, defaultFollowUpFilingInterval, func(err error) {
			if cfg.Logger != nil {
				cfg.Logger.Error("follow-up filing pass failed", "error", err)
			}
		})
		d.ComponentExited(parent, ctx, daemonshell.ComponentFollowUpFiling, err)
	})
	doctorAvailable.Store(true)
	d.Go(func() {
		err := claudeWiring.reviewReplier.Run(ctx, time.Minute, func(err error) {
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
			defaultActiveResourceInterval, operatorActiveResourceInterval, cfg.Logger)
		d.ComponentExited(parent, ctx, daemonshell.ComponentActiveResource, err)
	})
	d.Go(func() {
		err := intakeReconcilerLoop.Run(ctx, defaultIntakeInterval, cfg.Logger)
		d.ComponentExited(parent, ctx, daemonshell.ComponentLabelIntake, err)
	})
	// The start and tip-advance triggers: an upgrade's rebuild can take
	// an hour, so start does not wait for it.
	d.Go(func() {
		upkeepTrigger.run(ctx, imageUpkeep, logger)
	})
}

// startScenarioLoops starts the scenario's forge, publication lane, and
// scheduler, at the point where the Claude loops would start.
func (d *daemon) startScenarioLoops(
	parent, ctx context.Context, cfg config, scenarioWiring *daemoncompose.ScenarioComposition, workflow *engine.Engine,
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
