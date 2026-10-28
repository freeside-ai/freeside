package main

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
	blobs signet.EvidenceBlobWriter,
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
	upkeep := &engine.ImageUpkeep{
		Store: st, Rebuild: rebuild, RepositoryID: cfg.Claude.RepositoryID,
		Projects: imageUpkeepProjects(cfg, manualProjects), Blobs: blobs,
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
	}
	// A maintenance task runs at its refusal's tip only through per-attempt
	// image selection, which only the unattended composition has. Without it
	// the task would be admitted at -base-sha with hydration, so none is
	// submitted.
	if wiring.selectImage != nil {
		upkeep.SubmitMaintenance = func(ctx context.Context, refusal domain.ImageUpkeepRefusal) error {
			project, ok := imageRepairProject(manualProjects)
			if !ok {
				return fmt.Errorf("no project is configured for manual submission: %w", store.ErrNotFound)
			}
			_, err := attention.SubmitImageRepairTask(ctx, project, refusal.ID)
			return err
		}
		upkeep.MaintenancePending = func(ctx context.Context, refusal domain.ImageUpkeepRefusal) (domain.TaskID, bool, error) {
			return pendingImageRepairTask(ctx, st, refusal.ID)
		}
	}
	upkeep.AdoptionPending = upkeep.PendingAdoption
	return upkeep, nil
}

// imageRepairProject is the project a maintenance task is submitted to: the
// first configured manual-submission project in ProjectID order. Every
// configured project shares the daemon's one repository (imageUpkeepPolicy
// already assumes this), and the store's daemon-origin gate refuses a
// project on another repository.
func imageRepairProject(projects map[domain.ProjectID]engine.ManualInitiator) (domain.ProjectID, bool) {
	if len(projects) == 0 {
		return "", false
	}
	return slices.Min(slices.Collect(maps.Keys(projects))), true
}

// pendingImageRepairTask names the maintenance task submitted for a refusal
// while that task exists and has not been cancelled, so other writer
// attempts no image serves hold on it. A cancelled task resolves nothing,
// so they go back to the ordinary hold.
func pendingImageRepairTask(
	ctx context.Context, st *store.Store, refusalID domain.Digest,
) (domain.TaskID, bool, error) {
	var (
		taskID  domain.TaskID
		pending bool
	)
	err := st.Read(ctx, func(tx *store.ReadTx) error {
		submission, err := tx.GetTaskSubmission(ctx, domain.ImageUpkeepSubmissionIdentity(refusalID))
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		task, err := tx.GetTask(ctx, submission.TaskID)
		if err != nil {
			return err
		}
		taskID, pending = task.ID, task.Cancellation == nil
		return nil
	})
	return taskID, pending, err
}

// imageUpkeepProjects are the configured projects an adoption card may be
// raised in, in a stable order: the label-intake projects of the daemon's
// repository, then the manual-submission projects. Upkeep takes the first
// one registered for the repository.
func imageUpkeepProjects(cfg config, manualProjects map[domain.ProjectID]engine.ManualInitiator) []domain.ProjectID {
	var projects []domain.ProjectID
	for _, initiator := range cfg.IntakeInitiators {
		if initiator.RepositoryID == cfg.Claude.RepositoryID {
			projects = append(projects, initiator.ProjectID)
		}
	}
	manual := slices.Sorted(maps.Keys(manualProjects))
	for _, project := range manual {
		if !slices.Contains(projects, project) {
			projects = append(projects, project)
		}
	}
	return projects
}
