package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// TestResolveMemoryBudget: a declared budget wins, prod defaults to half of
// physical memory, and a dev or ephemeral instance refuses to start without
// one only when it launches containers.
func TestResolveMemoryBudget(t *testing.T) {
	t.Parallel()
	physical := func() (uint64, error) { return 64 << 30, nil }
	for name, tc := range map[string]struct {
		env        environment
		containers bool
		declared   int
		want       int
		refusal    string
	}{
		"declared wins over the prod default":  {env: environmentProd, containers: true, declared: 4096, want: 4096},
		"prod takes half of physical memory":   {env: environmentProd, containers: true, want: 32768},
		"prod without containers still bounds": {env: environmentProd, want: 32768},
		"dev declares its own":                 {env: environmentDev, containers: true, declared: 8192, want: 8192},
		"dev with containers must declare":     {env: environmentDev, containers: true, refusal: "-memory-budget-mib is required for a dev instance"},
		"ephemeral with containers must declare": {
			env: environmentEphemeral, containers: true, refusal: "-memory-budget-mib is required for a ephemeral instance",
		},
		"ephemeral without containers runs unbudgeted": {env: environmentEphemeral},
		"ephemeral without containers may declare":     {env: environmentEphemeral, declared: 2048, want: 2048},
		"negative is refused":                          {env: environmentProd, declared: -1, refusal: "negative"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveMemoryBudget(tc.env, tc.containers, tc.declared, physical)
			if tc.refusal != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refusal) {
					t.Fatalf("resolve = %d, %v; want a refusal naming %q", got, err, tc.refusal)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("resolve = %d, %v; want %d", got, err, tc.want)
			}
		})
	}
}

func TestResolveMemoryBudgetFailsWhenProdCannotReadPhysicalMemory(t *testing.T) {
	t.Parallel()
	unreadable := errors.New("sysctl refused")
	_, err := resolveMemoryBudget(environmentProd, true, 0, func() (uint64, error) { return 0, unreadable })
	if !errors.Is(err, unreadable) {
		t.Fatalf("resolve = %v, want the read failure", err)
	}
}

// TestLaunchBoundOfAComposition: a composition that starts no VM reserves
// with no overhead, and an unbudgeted one reads no runtime at all.
func TestLaunchBoundOfAComposition(t *testing.T) {
	t.Parallel()
	bound, err := config{MemoryBudgetMiB: 4096}.launchBound(t.Context())
	if err != nil || bound != (ward.LaunchBound{BudgetMiB: 4096, RuntimeVersion: ward.RuntimeNone}) {
		t.Fatalf("scenario bound = %+v, %v", bound, err)
	}
	// A budget no default writer launch fits could run no writer at all.
	if _, err := (config{MemoryBudgetMiB: 1024}).launchBound(t.Context()); !errors.Is(err, domain.ErrLaunchNeverFits) {
		t.Fatalf("1024 MiB bound = %v, want a never-fits refusal", err)
	}
	// A Claude composition with no budget never runs the container binary,
	// which does not exist here.
	bound, err = config{Claude: &claudeDriverConfig{ContainerBin: "/nonexistent/container"}}.launchBound(t.Context())
	if err != nil || bound.Budgeted() {
		t.Fatalf("unbudgeted bound = %+v, %v", bound, err)
	}
}

// TestPolicySizeBoundSkipsAnUnrecordedRuntime: with no recorded overhead the
// start checks nothing, leaving the launch to raise the item naming the
// version.
func TestPolicySizeBoundSkipsAnUnrecordedRuntime(t *testing.T) {
	t.Parallel()
	known := ward.LaunchBound{BudgetMiB: 4096, RuntimeVersion: "apple-container 1.1.0"}
	if got := policySizeBound(known); got != known {
		t.Fatalf("known runtime bound = %+v", got)
	}
	if got := policySizeBound(ward.LaunchBound{BudgetMiB: 4096, RuntimeVersion: "apple-container 9.9.9"}); got.Budgeted() {
		t.Fatalf("unrecorded runtime bound = %+v, want none", got)
	}
}
