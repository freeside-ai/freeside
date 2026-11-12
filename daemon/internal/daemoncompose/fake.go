package daemoncompose

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

const (
	DefaultFakeRunID     domain.RunID     = "run-walking-skeleton"
	defaultFakeProjectID domain.ProjectID = "project-walking-skeleton"
)

// ComposeFakeWorkflow builds the fake-driver engine over driver. running is
// the daemon's count of in-process work.
func ComposeFakeWorkflow(
	st *store.Store, attention *signet.Service, driver exec.StageDriver,
	lock *daemonlock.Lock, bound ward.LaunchBound, running *engine.RunningWork, logger *slog.Logger,
) (*engine.Engine, error) {
	walkingSkeletonOptions := []engine.Option{
		engine.WithDaemonLock(lock), engine.WithMachineCapacity(bound), engine.WithRunningWork(running),
	}
	if logger != nil {
		walkingSkeletonOptions = append(walkingSkeletonOptions, engine.WithLogger(logger))
	}
	return engine.New(st, attention, driver, walkingSkeletonOptions...)
}

// SeedWalkingSkeleton starts the walking-skeleton sample run on workflow.
// Even explicit fake execution starts without sample work unless asked, so
// run calls it only under -seed-walking-skeleton.
func SeedWalkingSkeleton(parent context.Context, workflow *engine.Engine) error {
	if _, err := workflow.StartFakeRun(parent, engine.FakeRunSpec{
		RunID: DefaultFakeRunID, ProjectID: defaultFakeProjectID,
		SpecDigest: "sha256:walking-skeleton-spec", PolicyDigest: "sha256:walking-skeleton-policy",
	}); err != nil {
		return fmt.Errorf("seed walking-skeleton run: %w", err)
	}
	return nil
}
