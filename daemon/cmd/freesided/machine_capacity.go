package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// resolveMemoryBudget is the memory budget, in MiB, every ward launch of this
// instance reserves against (plan §5.4). An explicit value wins. A prod
// instance with none takes half of physical memory, so it shares the machine
// with the dev and ephemeral instances beside it. A dev or ephemeral instance
// that launches containers must declare one, because only the operator knows
// what else the machine runs; one that launches none (disabled, fake,
// scenario) runs unbudgeted, and reserves like a real composition when given
// one. Zero means no budget.
func resolveMemoryBudget(
	env environment, launchesContainers bool, declaredMiB int, physicalBytes func() (uint64, error),
) (int, error) {
	if declaredMiB < 0 {
		return 0, fmt.Errorf("-memory-budget-mib %d is negative", declaredMiB)
	}
	if declaredMiB > 0 {
		return declaredMiB, nil
	}
	if env == environmentProd {
		total, err := physicalBytes()
		if err != nil {
			return 0, fmt.Errorf("read physical memory for the prod memory budget: %w", err)
		}
		half := int(total / 2 >> 20)
		if half < 1 {
			return 0, fmt.Errorf("physical memory %d bytes leaves no prod memory budget", total)
		}
		return half, nil
	}
	if launchesContainers {
		return 0, fmt.Errorf("-memory-budget-mib is required for a %s instance that launches containers", env)
	}
	return 0, nil
}

// startupLaunchBound resolves the bound this start runs under. A budget
// recorded with freesided set-memory-budget wins over the declared one (the
// flag, or prod's half of physical memory), and the start logs both when they
// differ. The start's checks run against the budget that wins, and a refusal
// names what sets it.
func (cfg config) startupLaunchBound(
	ctx context.Context, st *store.Store, manualProjects map[domain.ProjectID]engine.ManualInitiator,
) (ward.LaunchBound, error) {
	var (
		recordedMiB int
		recorded    bool
	)
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		recordedMiB, recorded, err = tx.MemoryBudget(ctx)
		return err
	}); err != nil {
		return ward.LaunchBound{}, err
	}
	source := "-memory-budget-mib"
	if recorded {
		if recordedMiB != cfg.MemoryBudgetMiB && cfg.Logger != nil {
			cfg.Logger.Info("recorded memory budget overrides the declared one",
				"recorded_budget_mib", recordedMiB, "declared_budget_mib", cfg.MemoryBudgetMiB,
				"change_with", "freesided set-memory-budget")
		}
		cfg.MemoryBudgetMiB = recordedMiB
		source = "recorded memory budget (change it with freesided set-memory-budget)"
	}
	bound, err := cfg.launchBound(ctx)
	if err == nil {
		err = requireManualProjectsFit(manualProjects, bound)
	}
	if errors.Is(err, domain.ErrLaunchNeverFits) {
		return ward.LaunchBound{}, fmt.Errorf("%s %d MiB: %w", source, cfg.MemoryBudgetMiB, err)
	}
	return bound, err
}

// launchBound pairs the budget with the container runtime whose per-VM
// overhead each reservation adds. Only the Claude composition starts VMs;
// every other composition reserves with no overhead, and carries that
// runtime even unbudgeted, so a budget recorded while it runs reserves under
// it. It reads the runtime version only under a budget, so an unbudgeted
// start runs no container command. A budget too small for a launch at its
// class's default size could run nothing of that class, so it refuses the
// start.
func (cfg config) launchBound(ctx context.Context) (ward.LaunchBound, error) {
	if cfg.MemoryBudgetMiB <= 0 {
		// An unbudgeted Claude composition cannot start (resolveMemoryBudget);
		// its zero runtime would refuse any launch rather than guess none.
		if cfg.Claude != nil {
			return ward.LaunchBound{}, nil
		}
		return ward.LaunchBound{RuntimeVersion: ward.RuntimeNone}, nil
	}
	bound := ward.LaunchBound{BudgetMiB: cfg.MemoryBudgetMiB, RuntimeVersion: ward.RuntimeNone}
	if cfg.Claude != nil {
		version, err := ward.NewCLIRuntime(cfg.Claude.ContainerBin).Version(ctx)
		if err != nil {
			return ward.LaunchBound{}, err
		}
		bound.RuntimeVersion = version
	}
	for _, class := range ward.AllLaunchClasses {
		if _, err := policySizeBound(bound).Reservation(ward.DefaultLaunchSize(class)); err != nil {
			return ward.LaunchBound{}, fmt.Errorf("a default %s launch: %w", class, err)
		}
	}
	return bound, nil
}

// logLaunchBound reports the budget beside the environment tier, so the
// operator can see what launches wait for. An unrecorded runtime version logs
// no overhead: its launches raise an item naming the version instead.
func logLaunchBound(logger *slog.Logger, bound ward.LaunchBound) {
	if logger == nil || !bound.Budgeted() {
		return
	}
	attrs := []any{"budget_mib", bound.BudgetMiB, "runtime", bound.RuntimeVersion}
	if overhead, ok := ward.VMOverheadMiB(bound.RuntimeVersion); ok {
		attrs = append(attrs, "vm_overhead_mib", overhead)
	} else {
		attrs = append(attrs, "vm_overhead_mib", "unrecorded")
	}
	logger.Info("machine capacity", attrs...)
}

// policySizeBound is the bound a policy's sizes are checked against before
// any launch. Under a runtime with no recorded overhead nothing can be
// checked yet; each launch raises the item naming the version instead of the
// start refusing.
func policySizeBound(bound ward.LaunchBound) ward.LaunchBound {
	if _, ok := ward.VMOverheadMiB(bound.RuntimeVersion); !ok {
		return ward.LaunchBound{}
	}
	return bound
}
