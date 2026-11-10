package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/admissionauth"
	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/daemonshell"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/operations"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/usagecollect"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

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
	late *daemoncompose.StartupBindings,
) *signet.Service {
	attention := signet.NewService(st,
		signet.WithTaskStopGuard(func(stopCtx context.Context, taskID domain.TaskID, commit func() error) error {
			if late.Workflow == nil {
				return commit()
			}
			return late.Workflow.CommitTaskStop(stopCtx, taskID, commit)
		}),
		signet.WithFollowUpFiler(func() signet.FollowUpFiler {
			return daemoncompose.FollowUpFilerBinding(late.ClaudeWiring)
		}),
		// An approved image-policy adoption wakes upkeep to build under it.
		signet.WithImageUpkeepWake(func() {
			if late.UpkeepTrigger != nil {
				late.UpkeepTrigger.Wake()
			}
		}),
		signet.WithPairingKey(pairingKey),
		signet.WithHostFacts(signet.HostFacts{DisplayName: hostName, ConnectionMode: connectionMode}),
		signet.WithClock(cfg.now),
		signet.WithLogger(cfg.Logger),
		signet.WithDoctorSchedule(daemoncompose.DoctorScheduleID, doctorAvailable.Load),
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
	lock *daemonlock.Lock, bound ward.LaunchBound, cfg config,
) (daemoncompose.ScenarioParts, error) {
	materializer, err := daemoncompose.ProductionMaterializer(blobs)
	if err != nil {
		return daemoncompose.ScenarioParts{}, err
	}
	return daemoncompose.ScenarioParts{
		Store: st, Attention: attention, Blobs: blobs, Lock: lock, Bound: bound,
		DBPath: cfg.DBPath, Logger: cfg.Logger, Scenario: cfg.Scenario,
		AdmissionFloor:                   daemoncompose.AdmissionFloor(domain.ModeUnattended),
		ImplementationDeliveryValidation: daemoncompose.ProductionImplementationDeliveryValidator(materializer),
		SpecificationDeliveryValidation:  daemoncompose.ProductionSpecificationDeliveryValidator(materializer),
		Imports:                          admissionauth.Authority{Store: st, Blobs: blobs},
		Exports:                          daemoncompose.ExportRecorder{Store: st},
		PublicationWatches:               daemoncompose.PublicationWatchRegistrations(st),
		AgentExpiryMargin:                daemoncompose.AgentExpiryMargin,
	}, nil
}

// newDoctorPass returns a doctor pass, which does nothing outside Claude
// mode. The startup pass takes a nil probe, so it reports recorded
// credential-integrity marks only and starting the daemon starts no
// observer container; scheduled passes take the live probe.
func newDoctorPass(
	cfg config, st *store.Store, attention *signet.Service, claudeWiring *daemoncompose.ClaudeComposition,
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
			ConfigurationDigest:       claudeWiring.Backend.ConfigurationDigest(),
			ReviewConfigurationDigest: claudeWiring.ReviewConfigurationDigest,
			Mode:                      cfg.Claude.OperatingMode,
			Now:                       cfg.now,
			CredentialIntegrityProbe:  replay,
			IdentityLabel:             credentialIdentityLabel,
		}).Run(runCtx)
		return err
	}
}

// startServing starts usage collection, model discovery, the API and
// control servers, the workflow loops, local backups, and attention
// deliveries. It cannot fail: run() registers the cleanup that stops
// these goroutines as soon as it returns, and a component that exits
// later reports through ComponentExited.
func (d *daemon) startServing(
	parent, ctx context.Context, cfg config, st *store.Store, attention *signet.Service,
	workflow *engine.Engine, claudeWiring *daemoncompose.ClaudeComposition, localBackups *store.LocalBackupProducer,
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
		refresh := newModelCatalogRefresh(st, *cfg.Claude, claudeWiring.ContainerBin,
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
	parent, ctx context.Context, cfg config, claudeWiring *daemoncompose.ClaudeComposition, workflow *engine.Engine,
	imageUpkeep *engine.ImageUpkeep, loops *daemoncompose.ClaudeLoops, doctorAvailable *atomic.Bool, logger *slog.Logger,
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
			daemoncompose.DefaultActiveResourceInterval, daemoncompose.OperatorActiveResourceInterval, cfg.Logger)
		d.ComponentExited(parent, ctx, daemonshell.ComponentActiveResource, err)
	})
	d.Go(func() {
		err := intakeReconcilerLoop.Run(ctx, daemoncompose.DefaultIntakeInterval, cfg.Logger)
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
