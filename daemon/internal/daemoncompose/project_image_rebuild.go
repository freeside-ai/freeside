package daemoncompose

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/operations"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// newProjectImageRebuild composes the builder the publication lane rebuilds a
// project image with (plan §5.7), or nil when the operator supplied no
// -base-build-ref: the lane then blocks a dependency change it cannot rebuild
// for.
//
// The builder holds no token source. A rebuild reads the candidate from the
// lane's own checkout and never reaches the forge, so it carries no GitHub
// authority at all.
func newProjectImageRebuild(
	cfg ClaudeDriverConfig, st *store.Store, memory *engine.DaemonMemory,
) (*engine.ProjectImageRebuild, error) {
	if cfg.Rebuild.BaseBuildRef == "" {
		return nil, nil
	}
	scratch := filepath.Join(cfg.SeedRoot, "project-image-rebuild")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return nil, fmt.Errorf("create project-image rebuild scratch: %w", err)
	}
	builder, err := projectimage.New(projectimage.Options{
		ContainerPath: cfg.ContainerBin, TempDir: scratch, Log: os.Stderr,
		Record: func(ctx context.Context, image domain.ProjectImage) error {
			return st.WriteInternal(ctx, func(tx *store.InternalTx) error {
				return tx.RecordProjectImage(ctx, image)
			})
		},
		LookupRecordedRef: func(ctx context.Context, ref string) (bool, error) {
			recorded := false
			err := st.Read(ctx, func(tx *store.ReadTx) error {
				var err error
				recorded, err = tx.ProjectImageRefRecorded(ctx, domain.ImageRef(ref))
				return err
			})
			return recorded, err
		},
		LookupRecordedBase: func(ctx context.Context, ref string) (bool, error) {
			found := false
			err := st.Read(ctx, func(tx *store.ReadTx) error {
				var err error
				_, found, err = tx.FindAdmittedAgentBaseByImage(ctx, domain.ImageRef(ref))
				return err
			})
			return found, err
		},
		Registry:      registryMemory{memory: memory},
		ManageBuilder: true,
	})
	if err != nil {
		return nil, fmt.Errorf("compose project-image rebuild: %w", err)
	}
	return &engine.ProjectImageRebuild{
		Builder: builder, Inputs: cfg.Rebuild, Memory: memory, BuildLimit: cfg.ImageBuildLimit,
		RecoverBuild: func(ctx context.Context, id domain.InvocationID) error {
			return projectimage.RecoverBuild(ctx, cfg.ContainerBin, id)
		},
	}, nil
}

// repositoryUpkeep is one repository's loop entry and the image upkeep that
// keeps that repository's image current.
type repositoryUpkeep struct {
	loop   repositoryLoop
	upkeep *engine.ImageUpkeep
}

// imageUpkeeps are the daemon's image upkeeps, one per served repository, in
// loop order. Every one shares the lane's ProjectImageRebuild, and with it
// the build limit and the builder VM turn.
type imageUpkeeps []repositoryUpkeep

// loops lists the upkeeps' loop entries in loop order.
func (u imageUpkeeps) loops() []repositoryLoop {
	loops := make([]repositoryLoop, 0, len(u))
	for _, served := range u {
		loops = append(loops, served.loop)
	}
	return loops
}

// all lists the upkeeps in loop order.
func (u imageUpkeeps) all() []*engine.ImageUpkeep {
	upkeeps := make([]*engine.ImageUpkeep, 0, len(u))
	for _, served := range u {
		upkeeps = append(upkeeps, served.upkeep)
	}
	return upkeeps
}

// pendingResolution is PendingResolution of the upkeep that serves
// repositoryID. No upkeep serves a repository the daemon does not, so
// nothing is pending there, and a base no image serves is refused as it
// would be without upkeep.
func (u imageUpkeeps) pendingResolution(ctx context.Context, repositoryID int64) (*domain.ImageUpkeepHold, error) {
	for _, served := range u {
		if served.loop.RepositoryID == repositoryID {
			return served.upkeep.PendingResolution(ctx)
		}
	}
	return nil, nil
}

// runScheduled is the doctor schedule's pass over every repository. Each
// repository's pass runs on its own goroutine and a failed one is logged, so
// one repository's slow or failed pass never delays another's upkeep; the
// result joins every failure, or is the context's error once it ends.
func (u imageUpkeeps) runScheduled(ctx context.Context, logger *slog.Logger) error {
	failed := make([]error, len(u))
	var passes sync.WaitGroup
	for at, served := range u {
		passes.Go(func() {
			if err := served.upkeep.RunScheduled(ctx); err != nil {
				failed[at] = fmt.Errorf("repository %s: %w", served.loop.Repo, err)
				if ctx.Err() == nil {
					logger.Warn("scheduled image upkeep pass failed", "repository", served.loop.Repo, "error", err)
				}
			}
		})
	}
	passes.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.Join(failed...)
}

// composeImageUpkeeps composes one image-upkeep job (plan §5.7,
// Project-image upkeep) per repository the daemon serves. They share
// rebuild with the publication lane, so all of them take one build gate; a
// nil rebuild leaves upkeep able to keep a current image and record what it
// cannot rebuild. Each repository's host-fault item converges under the
// doctor's lock, as every doctor convergence does.
func composeImageUpkeeps(
	ctx context.Context,
	st *store.Store,
	attention *signet.Service,
	blobs signet.EvidenceBlobWriter,
	cfg ClaudeStartupConfig,
	wiring *ClaudeComposition,
	rebuild *engine.ProjectImageRebuild,
	manualProjects map[domain.ProjectID]engine.ManualInitiator,
	doctorLock *sync.Mutex,
) (imageUpkeeps, error) {
	bound, err := bindManualProjects(ctx, st, manualProjects)
	if err != nil {
		return nil, err
	}
	loops, err := repositoryLoops([]loopRepository{{
		Repo: cfg.Claude.Repo, RepositoryID: cfg.Claude.RepositoryID, BaseRef: cfg.Claude.BaseRef,
		Project: cfg.Claude.ProjectID,
	}}, cfg.IntakeInitiators, bound)
	if err != nil {
		return nil, err
	}
	workDir := filepath.Join(cfg.Claude.SeedRoot, "image-upkeep")
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return nil, fmt.Errorf("create image upkeep scratch: %w", err)
	}
	keep := make([]domain.ProjectID, 0, len(loops))
	for _, loop := range loops {
		keep = append(keep, loop.cardProject())
	}
	cards := &orphanUpkeepCards{store: st, attention: attention, cfg: cfg, keep: keep}
	upkeeps := make(imageUpkeeps, 0, len(loops))
	for _, loop := range loops {
		upkeeps = append(upkeeps, repositoryUpkeep{
			loop: loop, upkeep: composeImageUpkeep(st, attention, blobs, cfg, wiring, rebuild, loop, workDir, doctorLock, cards),
		})
	}
	return upkeeps, nil
}

// composeImageUpkeep composes one repository's image upkeep. Its reads and
// fetches name that repository and its branch, its adoption and repair
// cards go to that repository's projects, and its host-fault card is filed
// under loop.cardProject, so one repository's healthy pass never clears
// another's card.
func composeImageUpkeep(
	st *store.Store,
	attention *signet.Service,
	blobs signet.EvidenceBlobWriter,
	cfg ClaudeStartupConfig,
	wiring *ClaudeComposition,
	rebuild *engine.ProjectImageRebuild,
	loop repositoryLoop,
	workDir string,
	doctorLock *sync.Mutex,
	orphans *orphanUpkeepCards,
) *engine.ImageUpkeep {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	cardProject := loop.cardProject()
	upkeep := &engine.ImageUpkeep{
		Store: st, Rebuild: rebuild, RepositoryID: loop.RepositoryID,
		Projects: loop.Projects, Blobs: blobs,
		ReadTip: func(ctx context.Context) (string, error) {
			return wiring.observeRepositoryTip(ctx, loop.Repo, loop.BaseRef)
		},
		Checkout: func(ctx context.Context, tipSHA, dir string) (int64, error) {
			return wiring.fetchTip(ctx, loop.Repo, loop.BaseRef, tipSHA, dir)
		},
		NewRoom: func(image domain.ProjectImage, size ward.ContainerSize) (engine.ProductionVerificationRoom, error) {
			return ward.NewProjectImageRoom(wiring.ContainerBin, image, size, cfg.Logger)
		},
		Policy:            loop.Policy,
		PreparationDigest: projectimage.PreparationDigest(),
		WorkDir:           workDir,
		Health: func(ctx context.Context, fault string) error {
			doctorLock.Lock()
			defer doctorLock.Unlock()
			if err := orphans.conclude(ctx); err != nil {
				return err
			}
			return (operations.Doctor{
				Store: st, Attention: attention, ProjectID: cardProject,
				Mode: cfg.Claude.OperatingMode, Now: cfg.Now,
			}).ConvergeImageUpkeep(ctx, fault)
		},
		Now:    cfg.Now,
		Logger: logger.With("subsystem", "image_upkeep", "repository", loop.Repo),
	}
	// A maintenance task runs at its refusal's tip only through per-attempt
	// image selection, which only the unattended composition has. Without it
	// the task would be admitted at its run's base with hydration, so none is
	// submitted.
	if wiring.selectImage != nil {
		upkeep.SubmitMaintenance = func(ctx context.Context, refusal domain.ImageUpkeepRefusal) error {
			project, ok := ImageRepairProject(loop.Manual)
			if !ok {
				return fmt.Errorf("no project on repository %s is configured for manual submission: %w",
					loop.Repo, store.ErrNotFound)
			}
			_, err := attention.SubmitImageRepairTask(ctx, project, refusal.ID)
			return err
		}
		upkeep.MaintenancePending = func(
			ctx context.Context, refusal domain.ImageUpkeepRefusal,
		) (domain.Digest, domain.TaskID, bool, error) {
			return PendingImageRepairTask(ctx, st, refusal)
		}
	}
	upkeep.AdoptionPending = upkeep.PendingAdoption
	// The agent base is one daemon-wide pointer built from the one repository
	// the agent_base setting may name, the daemon's configured repository, so
	// only that repository's upkeep keeps it. Another repository's upkeep has
	// no keeper and leaves its image on the base it has. The daemon's builder
	// builds agent bases too; a test double that cannot leaves it off.
	if rebuild != nil && loop.RepositoryID == cfg.Claude.RepositoryID {
		if builder, ok := rebuild.Builder.(engine.AgentBaseBuilder); ok {
			upkeep.AgentBase = &engine.AgentBaseUpkeep{
				Builder: builder, Source: agentBaseSourceSetting(st),
				Repository: loop.Repo, HarnessBuild: agentbaseline.ClaudeCodeHarnessBuild,
				LocalDigest: func(ctx context.Context, ref string) (string, error) {
					return projectimage.InspectImageDigest(ctx, cfg.Claude.ContainerBin, ref)
				},
			}
		}
	}
	return upkeep
}

// orphanUpkeepCards concludes the image-upkeep host-fault cards filed under
// a project no served repository's card goes under: the scheduler's system
// project, where a daemon filed the one card before each repository's card
// went under a project on it, or a project the daemon no longer serves. No
// repository's healthy pass would conclude such a card. The first
// convergence that succeeds concludes them, once per start; its caller holds
// the doctor's lock.
type orphanUpkeepCards struct {
	store     *store.Store
	attention *signet.Service
	cfg       ClaudeStartupConfig
	// keep is every served repository's card project (repositoryLoop.cardProject).
	keep      []domain.ProjectID
	concluded bool
}

func (c *orphanUpkeepCards) conclude(ctx context.Context) error {
	if c.concluded {
		return nil
	}
	if err := (operations.Doctor{
		Store: c.store, Attention: c.attention, Mode: c.cfg.Claude.OperatingMode, Now: c.cfg.Now,
	}).ConcludeImageUpkeepExcept(ctx, c.keep); err != nil {
		return fmt.Errorf("conclude image upkeep cards no served repository keeps: %w", err)
	}
	c.concluded = true
	return nil
}

// ImageRepairProject is the project a repository's maintenance task is
// submitted to: the first of that repository's manual-submission projects in
// ProjectID order. The store's daemon-origin gate refuses a project on
// another repository.
func ImageRepairProject(projects map[domain.ProjectID]engine.ManualInitiator) (domain.ProjectID, bool) {
	if len(projects) == 0 {
		return "", false
	}
	return slices.Min(slices.Collect(maps.Keys(projects))), true
}

// PendingImageRepairTask names the open maintenance task for the image a
// refusal was measured from, and the refusal that task was opened for, so
// other writer attempts no image serves hold on it. A cancelled, completed,
// or abandoned task resolves nothing, so they go back to the ordinary hold.
func PendingImageRepairTask(
	ctx context.Context, st *store.Store, refusal domain.ImageUpkeepRefusal,
) (domain.Digest, domain.TaskID, bool, error) {
	var (
		open    signet.ImageRepairTask
		pending bool
	)
	err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		open, pending, err = signet.OpenImageRepairTask(ctx, tx, refusal)
		return err
	})
	return open.RefusalID, open.TaskID, pending, err
}
