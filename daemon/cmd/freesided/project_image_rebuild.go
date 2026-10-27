package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

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
func newProjectImageRebuild(cfg claudeDriverConfig, st *store.Store) (*engine.ProjectImageRebuild, error) {
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
	})
	if err != nil {
		return nil, fmt.Errorf("compose project-image rebuild: %w", err)
	}
	return &engine.ProjectImageRebuild{Builder: builder, Inputs: cfg.Rebuild}, nil
}

// composeImageUpkeep composes the image-upkeep job (plan §5.7, Project-image
// upkeep) for the configured repository. It shares rebuild with the
// publication lane, so the two take one build gate; a nil rebuild leaves
// upkeep able to keep a current image and record what it cannot rebuild.
// Its host-fault item converges under the doctor's lock, as every doctor
// convergence does.
func composeImageUpkeep(
	st *store.Store,
	attention *signet.Service,
	cfg config,
	wiring *claudeComposition,
	rebuild *engine.ProjectImageRebuild,
	manualProjects map[domain.ProjectID]engine.ManualInitiator,
	doctorLock *sync.Mutex,
) (*engine.ImageUpkeep, error) {
	policy, err := imageUpkeepPolicy(manualProjects)
	if err != nil {
		return nil, err
	}
	workDir := filepath.Join(cfg.Claude.SeedRoot, "image-upkeep")
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return nil, fmt.Errorf("create image upkeep scratch: %w", err)
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	watch := domain.ScheduleBaseWatch{Repo: cfg.Claude.Repo, BaseRef: cfg.Claude.BaseRef}
	return &engine.ImageUpkeep{
		Store: st, Rebuild: rebuild, RepositoryID: cfg.Claude.RepositoryID,
		ReadTip: func(ctx context.Context) (string, error) {
			return wiring.observeBaseTip(ctx, watch)
		},
		Checkout: wiring.fetchTip,
		NewRoom: func(image domain.ProjectImage, size ward.ContainerSize) (engine.ProductionVerificationRoom, error) {
			return ward.NewProjectImageRoom(wiring.containerBin, image, size, cfg.Logger)
		},
		Policy:            policy,
		PreparationDigest: projectimage.PreparationDigest(),
		WorkDir:           workDir,
		Health: func(ctx context.Context, fault string) error {
			doctorLock.Lock()
			defer doctorLock.Unlock()
			return (operations.Doctor{
				Store: st, Attention: attention, ProjectID: schedulerSystemProjectID,
				Mode: cfg.Claude.OperatingMode, Now: cfg.now,
			}).ConvergeImageUpkeep(ctx, fault)
		},
		Now:    cfg.now,
		Logger: logger.With("subsystem", "image_upkeep"),
	}, nil
}
