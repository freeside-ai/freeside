package main

import (
	"fmt"
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
