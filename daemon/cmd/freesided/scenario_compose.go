package main

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

	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
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

// scenarioComposition is the production engine composed over the scenario's
// fakes: what run starts as daemon components and what it closes.
type scenarioComposition struct {
	workflow  *engine.Engine
	scheduler *scheduler.Scheduler
	forge     *http.Server
	listener  net.Listener
}

// The prompt packages a scenario run's stage inputs name. The materializer
// requires their bytes in the artifact store; no agent reads them.
var (
	scenarioPromptPackage              = []byte("Scenario implementer prompt package.\n")
	scenarioSpecificationPromptPackage = []byte("Scenario specifier prompt package.\n")
	scenarioRemediationPromptPackage   = []byte("<!-- freeside:render-prior-artifacts=v1 -->\nScenario remediator prompt package.\n")
)

// composeScenario seeds the scenario's world and composes the production
// engine over it: the same admission, specification, delivery validation,
// task cancellation, and publication options the Claude branch uses, with
// the scripted stage driver and review source, an in-process forge on
// 127.0.0.1:0, and process-level verification. It runs unattended because
// production publication holds every other mode's work before publishing.
//
// run refuses a non-ephemeral environment before calling it: the fake
// backend's conformance record claims a containment no process here has.
func composeScenario(
	ctx context.Context, st *store.Store, attention *signet.Service, blobs *signet.BlobStore,
	lock *daemonlock.Lock, cfg config,
) (_ *scenarioComposition, err error) {
	now := func() time.Time { return time.Now().UTC() }
	sc := *cfg.Scenario
	world, err := seedScenario(ctx, st, cfg.DBPath, sc, now())
	if err != nil {
		return nil, err
	}
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
	vendorInstructions := filepath.Join(world.Dir, "CLAUDE.md")
	if err := os.WriteFile(vendorInstructions, []byte("Scenario vendor instructions.\n"), 0o600); err != nil {
		return nil, err
	}

	forge := fakeforge.New(fakeforge.Config{
		Repo: scenarioRepo, Token: scenarioForgeToken,
		Report: func(format string, args ...any) {
			if cfg.Logger != nil {
				cfg.Logger.Warn("scenario forge: " + fmt.Sprintf(format, args...))
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
		baseDir: world.BaseDir, workDir: filepath.Join(world.Dir, "deliveries"),
	}
	fakeReviews, err := fake.NewReviewSourceAt(filepath.Join(world.Dir, "review-source"))
	if err != nil {
		return nil, fmt.Errorf("open scenario review source: %w", err)
	}
	reviews := scenarioReviewSource{ReviewSource: fakeReviews}
	materializer, err := productionMaterializer(blobs)
	if err != nil {
		return nil, err
	}
	fetcher, err := specify.NewFetcher(st, blobs, nil)
	if err != nil {
		return nil, err
	}

	identity := scenarioIdentityID
	options := []engine.Option{
		engine.WithDaemonLock(lock),
		engine.WithAdmission(world.Backend, admissionFloor(domain.ModeUnattended), engine.AdmissionEnvironment{
			OperatingMode: domain.ModeUnattended, CredentialMode: domain.CredentialSubscriptionContained,
			EgressProfile: domain.EgressProviderOnly, ImageRef: world.Image.ImageRef,
			PromptPackageDigest:       digests["implementation"],
			ReviewConfigurationDigest: fake.DefaultReviewConfigurationDigest,
			VendorInstructions: engine.VendorInstructionConfig{
				Vendor: domain.AgentVendorClaude, Delivery: domain.VendorInstructionDeliveryAppendFile,
				HostPath: vendorInstructions,
			},
			AuthIdentityID: &identity,
		}, now),
		engine.WithAdmissionDerivation(func(context.Context, domain.InvocationID) (string, domain.BaseRevision, error) {
			return "scenario-workspace", domain.BaseRevision{
				Repo: scenarioRepo, RepositoryID: scenarioRepositoryID,
				BaseRef: scenarioBaseRef, BaseSHA: world.BaseSHA,
			}, nil
		}),
		engine.WithProductionDeliveryValidation(productionImplementationDeliveryValidator(materializer)),
		engine.WithSpecification(engine.SpecificationConfig{
			Fetcher: fetcher, Blobs: blobs, Now: now,
			PromptPackageDigest: digests["specification"],
			ValidateDelivery:    productionSpecificationDeliveryValidator(materializer),
		}),
		engine.WithTaskCancellationRuntime(engine.TaskCancellationRuntime{
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
		}),
		engine.WithProductionPublication(engine.ProductionPublicationConfig{
			WorkDir:   filepath.Join(world.Dir, "publication"),
			Transport: transport, Publisher: publisher, Artifacts: blobs,
			ApprovedRecipes:                map[domain.Digest]bool{verify.RecipeDigest(scenarioRecipe): true},
			RemediationPromptPackageDigest: digests["remediation"],
			ReviewSource:                   reviews,
			ReviewAdmission:                fake.AdmitReview,
			ReviewRecovery:                 func(context.Context) error { return nil },
			ReviewConfigurationDigest:      fake.DefaultReviewConfigurationDigest,
			Now:                            now,
			NewRoom: func(domain.ProjectImage, ward.ContainerSize) (engine.ProductionVerificationRoom, error) {
				home, err := os.MkdirTemp(world.Dir, "verification-home-")
				if err != nil {
					return nil, err
				}
				return &scenarioRoom{ProcRoom: verify.ProcRoom{Home: home}}, nil
			},
		}),
	}
	if cfg.Logger != nil {
		options = append(options, engine.WithLogger(cfg.Logger))
	}
	workflow, err := engine.New(st, attention, driver, options...)
	if err != nil {
		return nil, err
	}
	sched, err := scheduler.New(st, domain.ModeUnattended, now,
		publicationWatchRegistrations(st, staticBaseObserver, mergeCapture{}))
	if err != nil {
		return nil, err
	}
	sched.SetLogger(cfg.Logger)
	return &scenarioComposition{
		workflow: workflow, scheduler: sched, listener: listener,
		forge: &http.Server{Handler: forge, ReadHeaderTimeout: 5 * time.Second},
	}, nil
}

// serveForge serves the in-process forge until ctx ends.
func (c *scenarioComposition) serveForge(ctx context.Context, logger *slog.Logger) error {
	served := make(chan error, 1)
	go func() { served <- c.forge.Serve(c.listener) }()
	select {
	case err := <-served:
		return fmt.Errorf("scenario forge: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), serverShutdownBudget)
		defer cancel()
		if err := c.forge.Shutdown(shutdownCtx); err != nil && logger != nil {
			logger.Warn("scenario forge shutdown", "error", err)
		}
		return nil
	}
}
