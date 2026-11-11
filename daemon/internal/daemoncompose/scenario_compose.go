package daemoncompose

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/daemonshell"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	fakeforge "github.com/freeside-ai/freeside/daemon/internal/exec/fake/forge"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/scheduler"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// ScenarioComposition is the production engine composed over the scenario's
// fakes: what run starts as daemon components and what it closes.
type ScenarioComposition struct {
	Workflow  *engine.Engine
	Scheduler *scheduler.Scheduler
	forge     *http.Server
	Listener  net.Listener
	// Agents is the lineup the scenario admits through, which the operator
	// hooks re-derive alternate-agent choices against, and Catalog its
	// projection for clients.
	Agents  *engine.AgentSelection
	Catalog signet.AgentCatalog
}

// The prompt packages a scenario run's stage inputs name. The materializer
// requires their bytes in the artifact store; no agent reads them.
var (
	scenarioPromptPackage              = []byte("Scenario implementer prompt package.\n")
	scenarioSpecificationPromptPackage = []byte("Scenario specifier prompt package.\n")
	scenarioRemediationPromptPackage   = []byte("<!-- freeside:render-prior-artifacts=v1 -->\nScenario remediator prompt package.\n")
)

// ScenarioParts are what run hands ComposeScenario. AdmissionFloor through
// AgentExpiryMargin are values the scenario does not compute for itself:
// all but Imports have their helpers in package main, and Imports is an
// internal/admissionauth Authority that run still builds. Run builds each
// but AgentExpiryMargin exactly as the scenario built it in package main,
// and passes the Claude composition's margin. The admission floor,
// validators, watches, and expiry margin come from what the Claude
// composition uses; the import authority and export recorder are the types
// it uses, built bare, without the Claude composition's path, author, or
// usage configuration.
type ScenarioParts struct {
	Store     *store.Store
	Attention *signet.Service
	Blobs     *signet.BlobStore
	Lock      *daemonlock.Lock
	Bound     ward.LaunchBound
	DBPath    string
	Logger    *slog.Logger
	Scenario  *Scenario

	// AdmissionFloor is the unattended admission floor.
	AdmissionFloor []exec.Capability
	// ImplementationDeliveryValidation and SpecificationDeliveryValidation
	// check that a stage's prompt inputs are deliverable, over one
	// production materializer.
	ImplementationDeliveryValidation func(context.Context, exec.StartSpec) error
	SpecificationDeliveryValidation  func(context.Context, exec.StartSpec) error
	// Imports and Exports are the store's import authority and export
	// recorder, which the scripted delivery uses the way the ward pipeline
	// does.
	Imports ScenarioImportAuthority
	Exports ScenarioExportLookup
	// PublicationWatches are the publication deadline registrations every
	// production scheduler runs.
	PublicationWatches map[domain.ScheduleKind]scheduler.Registration
	// AgentExpiryMargin is how far past an attempt's deadline an expiring
	// credential must last, which the scenario lineup admits with.
	AgentExpiryMargin time.Duration
}

// ComposeScenario seeds the scenario's world and composes the production
// engine over it through the builder the Claude branch uses
// (ProductionEngine), with the scripted stage driver and review source, an
// in-process forge on 127.0.0.1:0, and process-level verification. Each part
// it leaves out is an explicit zero value with its reason. It runs
// unattended because production publication holds every other mode's work
// before publishing.
//
// run refuses a non-ephemeral environment before calling it: the fake
// backend's conformance record claims a containment no process here has.
func ComposeScenario(ctx context.Context, parts ScenarioParts) (_ *ScenarioComposition, err error) {
	st, blobs := parts.Store, parts.Blobs
	now := func() time.Time { return time.Now().UTC() }
	sc := *parts.Scenario
	digests := map[string]domain.Digest{}
	for name, body := range map[string][]byte{
		"implementation": scenarioPromptPackage, "specification": scenarioSpecificationPromptPackage,
		"remediation": scenarioRemediationPromptPackage,
	} {
		digest := scenarioDigest(body)
		if _, err := blobs.Put(digest, bytes.NewReader(body)); err != nil {
			return nil, fmt.Errorf("store scenario %s prompt package: %w", name, err)
		}
		digests[name] = digest
	}
	agents, err := scenarioAgentSelection(sc, digests, parts.AgentExpiryMargin)
	if err != nil {
		return nil, err
	}
	catalog, err := signet.AgentCatalogOf(agents.Tree, agents.LineupRevision)
	if err != nil {
		return nil, fmt.Errorf("scenario agent catalog: %w", err)
	}
	world, err := seedScenario(ctx, st, parts.DBPath, sc, agents.LineupRevision, now())
	if err != nil {
		return nil, err
	}
	vendorInstructions := filepath.Join(world.Dir, "CLAUDE.md")
	if err := os.WriteFile(vendorInstructions, []byte("Scenario vendor instructions.\n"), 0o600); err != nil {
		return nil, err
	}

	forge := fakeforge.New(fakeforge.Config{
		Repo: scenarioRepo, Token: scenarioForgeToken,
		Report: func(format string, args ...any) {
			if parts.Logger != nil {
				parts.Logger.Warn("scenario forge: " + fmt.Sprintf(format, args...))
			}
		},
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for scenario forge: %w", err)
	}
	defer func() {
		if err != nil {
			_ = listener.Close()
		}
	}()
	// The transport runs git inside HeadsDir, so it must exist; a restart
	// reinitializes the bare repository there in place.
	headsDir := filepath.Join(world.Dir, "heads")
	if err := os.MkdirAll(headsDir, 0o750); err != nil {
		return nil, err
	}
	transport := &fakeforge.Transport{
		BaseDir: world.BaseDir, BaseRef: scenarioBaseRef, Forge: forge, HeadsDir: headsDir,
	}
	ledger, err := publish.NewStoreLedger(st)
	if err != nil {
		return nil, err
	}
	trust, err := publish.NewStoreTrustSource(st)
	if err != nil {
		return nil, err
	}
	authorizations, err := publish.NewStoreAuthorizationSource(st)
	if err != nil {
		return nil, err
	}
	publisher := publish.NewPublisher(
		fakeforge.TokenSource{Secret: scenarioForgeToken, Repo: scenarioRepo, Now: now},
		&http.Client{Timeout: 30 * time.Second}, "http://"+listener.Addr().String(),
		fakeforge.Auditor{Result: world.Audit}, ledger, trust, authorizations,
	)

	fakeStages, err := fake.NewStageDriverAt(filepath.Join(world.Dir, "stage-driver"))
	if err != nil {
		return nil, fmt.Errorf("open scenario stage driver: %w", err)
	}
	driver := scenarioStageDriver{
		StageDriver: fakeStages, scenario: sc, store: st, blobs: blobs,
		imports: parts.Imports, exports: parts.Exports,
		baseDir: world.BaseDir, workDir: filepath.Join(world.Dir, "deliveries"),
	}
	fakeReviews, err := fake.NewReviewSourceAt(filepath.Join(world.Dir, "review-source"))
	if err != nil {
		return nil, fmt.Errorf("open scenario review source: %w", err)
	}
	reviews := scenarioReviewSource{ReviewSource: fakeReviews}
	fetcher, err := specify.NewFetcher(st, blobs, nil)
	if err != nil {
		return nil, err
	}

	production := ProductionEngine{
		Lock: parts.Lock, Bound: parts.Bound,
		Backend: world.Backend, AdmissionFloor: parts.AdmissionFloor,
		Admission: engine.AdmissionEnvironment{
			OperatingMode: domain.ModeUnattended, CredentialMode: domain.CredentialSubscriptionContained,
			EgressProfile:             domain.EgressProviderOnly,
			EnforceableEgressProfiles: []domain.EgressProfile{domain.EgressProviderOnly},
			ImageRef:                  world.Image.ImageRef,
			PromptPackageDigest:       digests["implementation"],
			ReviewConfigurationDigest: fake.DefaultReviewConfigurationDigest,
			VendorInstructions: engine.VendorInstructionConfig{
				Vendor: domain.AgentVendorClaude, Delivery: domain.VendorInstructionDeliveryAppendFile,
				HostPath: vendorInstructions,
			},
			// Every stage admits through the lineup, as the Claude
			// composition's do, so a park or a failure card can offer
			// another agent.
			AuthIdentityID: nil,
			Agents:         &agents,
		},
		AdmissionNow: now,
		Derive: func(context.Context, domain.InvocationID) (string, domain.BaseRevision, error) {
			return "scenario-workspace", domain.BaseRevision{
				Repo: scenarioRepo, RepositoryID: scenarioRepositoryID,
				BaseRef: scenarioBaseRef, BaseSHA: world.BaseSHA,
			}, nil
		},
		DeliveryValidation: parts.ImplementationDeliveryValidation,
		Specification: engine.SpecificationConfig{
			Fetcher: fetcher, Blobs: blobs, Now: now,
			PromptPackageDigest: digests["specification"],
			ValidateDelivery:    parts.SpecificationDeliveryValidation,
		},
		Cancellation: engine.TaskCancellationRuntime{
			Timeout: time.Minute,
			StopRun: func(stopCtx context.Context, run domain.Run, requests []domain.ReviewRequestRecord) error {
				var joined error
				for _, stage := range run.Stages {
					for _, attempt := range stage.Attempts {
						// A fake invocation runs no process, so one the driver
						// no longer knows (never started, or lost to a
						// restart) has nothing left to stop.
						err := fakeStages.Cancel(stopCtx, attempt.InvocationID)
						if err != nil && !errors.Is(err, exec.ErrUnknownInvocation) {
							joined = errors.Join(joined, err)
						}
					}
				}
				for _, request := range requests {
					joined = errors.Join(joined, engine.StopTaskReviews(stopCtx, st, request, reviews, nil))
				}
				return errors.Join(joined, publisher.ReconcileCancelledRun(stopCtx, run.ID))
			},
			Unprovable: func(context.Context, domain.TaskID) (string, error) { return "", nil },
		},
		Publication: engine.ProductionPublicationConfig{
			WorkDir:   filepath.Join(world.Dir, "publication"),
			Transport: transport, Publisher: publisher, Artifacts: blobs,
			ApprovedRecipes: map[domain.Digest]bool{verify.RecipeDigest(ScenarioRecipe): true},
			// The scenario approves its one recipe above and records no
			// image-policy adoption, so no adoption's recipes can apply. A
			// scenario that seeds an adoption must set scenarioRepositoryID.
			RepositoryID:                   0,
			RemediationPromptPackageDigest: digests["remediation"],
			ReviewSource:                   reviews,
			ReviewAdmission:                fake.AdmitReview,
			ReviewRecovery:                 func(context.Context) error { return nil },
			ReviewConfigurationDigest:      fake.DefaultReviewConfigurationDigest,
			// No shadow review source is composed, so the engine requires
			// none of the other shadow fields.
			ShadowReviewSource:              nil,
			ShadowReviewConfigurationDigest: "",
			ShadowReviewDefaultRate:         0,
			ShadowReviewFailure:             nil,
			// The fake review source reads no operator-host instructions;
			// the zero value records the source as absent.
			ReviewHostInstructions: engine.ReviewHostInstructions{},
			// The scenario runs unattended (see the doc comment), so the
			// publication queue advances.
			HoldOnly: false,
			// The image is fixed at seeding, so a dependency-changing
			// candidate is refused as rebuild_not_configured.
			RebuildProjectImage: nil,
			// No image upkeep is composed, so nothing is pending.
			ImageUpkeepPending: nil,
			Now:                now,
			TransitionHook:     sc.PublicationTransitionHook,
			NewRoom: func(domain.ProjectImage, ward.ContainerSize) (engine.ProductionVerificationRoom, error) {
				home, err := os.MkdirTemp(world.Dir, "verification-home-")
				if err != nil {
					return nil, err
				}
				return &scenarioRoom{ProcRoom: verify.ProcRoom{Home: home}}, nil
			},
			// Verification runs as a host process (verify.ProcRoom), with no
			// container to bind to task ownership, so NewRoom serves.
			NewBoundRoom: nil,
		},
		// The scenario admits the one image it seeded, world.Image.ImageRef.
		SelectImage: nil,
		// No judgment runtime is composed; every judgment site takes its
		// fail-safe.
		Inference: nil,
		// The refresh converges the doctor's review-configuration health,
		// and no doctor runs without a Claude config (newDoctorPass); the
		// scenario's review configuration is the fake's fixed default.
		HealthRefresh: nil,
		Logger:        parts.Logger,
		// The scripted stage driver runs no Claude writer turn, so there is
		// no usage capture to sink.
		UsageSink: nil,
	}
	workflow, err := engine.New(st, parts.Attention, driver, production.Options()...)
	if err != nil {
		return nil, err
	}
	sched, err := scheduler.New(st, domain.ModeUnattended, now, parts.PublicationWatches)
	if err != nil {
		return nil, err
	}
	sched.SetLogger(parts.Logger)
	return &ScenarioComposition{
		Workflow: workflow, Scheduler: sched, Listener: listener,
		forge:  &http.Server{Handler: forge, ReadHeaderTimeout: 5 * time.Second},
		Agents: &agents, Catalog: catalog,
	}, nil
}

// The scenario lineup's writer models. They name no real model: the
// scripted stage driver runs no agent.
const (
	ScenarioWriterModel    = "claude-scenario"
	scenarioSpecifierModel = "claude-scenario-specifier"
)

// scenarioAgentSelection is the baseline lineup on the scenario identity's
// enrollment, with a second subscription's enrollment when the scenario
// seeds one, for the prompt packages the scenario stores. digests is keyed
// by stage: specification, implementation, and remediation. expiryMargin is
// the Claude composition's, which run passes in ScenarioParts.
func scenarioAgentSelection(
	sc Scenario, digests map[string]domain.Digest, expiryMargin time.Duration,
) (engine.AgentSelection, error) {
	subscriptions := sc.Subscriptions()
	input := agentbaseline.TreeInput{
		ClaudeEnrollment: subscriptions[0].Enrollment,
		Writers: map[domain.RoleName]agentbaseline.Selection{
			// The specifier asks its own model, so its lineup line differs
			// from the implementer's.
			domain.RoleSpecifier:   {Model: scenarioSpecifierModel, Effort: domain.EffortHigh},
			domain.RoleImplementer: {Model: ScenarioWriterModel, Effort: domain.EffortHigh},
			domain.RoleRemediator:  {Model: ScenarioWriterModel, Effort: domain.EffortHigh},
		},
		TermsBasisDate: "2026-07-01",
		Prompts: map[domain.RoleName]agentbaseline.Prompt{
			domain.RoleSpecifier:   {Name: "specifier", Digest: digests["specification"]},
			domain.RoleImplementer: {Name: "implementer", Digest: digests["implementation"]},
			domain.RoleRemediator:  {Name: "remediator", Digest: digests["remediation"]},
		},
	}
	if len(subscriptions) > 1 {
		input.SecondClaudeEnrollment = &subscriptions[1].Enrollment
	}
	tree, err := agentbaseline.Tree(input)
	if err != nil {
		return engine.AgentSelection{}, fmt.Errorf("scenario agent tree: %w", err)
	}
	// The round trip through the tree's files gives the revision the
	// Claude composition reads from a committed tree.
	files, err := agenttree.Render(tree)
	if err != nil {
		return engine.AgentSelection{}, fmt.Errorf("render scenario agent tree: %w", err)
	}
	revision, err := agenttree.Revision(files)
	if err != nil {
		return engine.AgentSelection{}, fmt.Errorf("scenario agent tree revision: %w", err)
	}
	if tree, err = agenttree.Parse(files); err != nil {
		return engine.AgentSelection{}, fmt.Errorf("parse scenario agent tree: %w", err)
	}
	return engine.AgentSelection{
		Tree: tree, LineupRevision: revision, Launch: agentbaseline.RoleLaunch,
		EffectiveEgress: []string{"api.anthropic.com:443"},
		// The scenario policy's budgets.stage_active_time.
		AttemptBudget: time.Hour, ExpiryMargin: expiryMargin,
		// Seeding records every writer subscription before the engine
		// starts, so nothing holds admission.
		Gate: nil,
	}, nil
}

// ServeForge serves the in-process forge until ctx ends.
func (c *ScenarioComposition) ServeForge(ctx context.Context, logger *slog.Logger) error {
	served := make(chan error, 1)
	go func() { served <- c.forge.Serve(c.Listener) }()
	select {
	case err := <-served:
		return fmt.Errorf("scenario forge: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), daemonshell.ServerShutdownBudget)
		defer cancel()
		if err := c.forge.Shutdown(shutdownCtx); err != nil && logger != nil {
			logger.Warn("scenario forge shutdown", "error", err)
		}
		return nil
	}
}
