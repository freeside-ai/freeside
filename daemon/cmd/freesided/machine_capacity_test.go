package main

import (
	"bytes"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
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
	// An unbudgeted composition that starts no VM still names its runtime,
	// so a budget recorded while it runs reserves with no overhead.
	bound, err = config{}.launchBound(t.Context())
	if err != nil || bound != (ward.LaunchBound{RuntimeVersion: ward.RuntimeNone}) {
		t.Fatalf("unbudgeted scenario bound = %+v, %v", bound, err)
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

// TestStartupLaunchBoundPrefersTheRecordedBudget: a budget recorded with
// set-memory-budget wins over the declared one, the start logs both, and the
// start's checks run against it, naming the command when it refuses.
func TestStartupLaunchBoundPrefersTheRecordedBudget(t *testing.T) {
	t.Parallel()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	declared := config{MemoryBudgetMiB: 4096}
	bound, err := declared.startupLaunchBound(t.Context(), st, nil)
	if err != nil || bound.BudgetMiB != 4096 {
		t.Fatalf("bound with none recorded = %+v, %v; want the declared 4096", bound, err)
	}
	recordBudget := func(budgetMiB int) {
		t.Helper()
		if err := st.WriteInternal(t.Context(), func(tx *store.InternalTx) error {
			return tx.RecordMemoryBudget(t.Context(), budgetMiB, time.Now().UTC())
		}); err != nil {
			t.Fatal(err)
		}
	}
	recordBudget(8192)
	var logs bytes.Buffer
	declared.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	bound, err = declared.startupLaunchBound(t.Context(), st, nil)
	if err != nil || bound != (ward.LaunchBound{BudgetMiB: 8192, RuntimeVersion: ward.RuntimeNone}) {
		t.Fatalf("bound = %+v, %v; want the recorded 8192", bound, err)
	}
	if line := logs.String(); !strings.Contains(line, "recorded_budget_mib=8192") ||
		!strings.Contains(line, "declared_budget_mib=4096") {
		t.Fatalf("start log = %q, want both budgets", line)
	}
	// An unbudgeted composition takes a recorded budget too.
	if bound, err := (config{}).startupLaunchBound(t.Context(), st, nil); err != nil || bound.BudgetMiB != 8192 {
		t.Fatalf("unbudgeted start bound = %+v, %v; want the recorded 8192", bound, err)
	}

	// A recorded budget no default launch fits refuses the start, naming the
	// command, even when the declared one would fit.
	recordBudget(1024)
	_, err = declared.startupLaunchBound(t.Context(), st, nil)
	if !errors.Is(err, domain.ErrLaunchNeverFits) || !strings.Contains(err.Error(), "freesided set-memory-budget") {
		t.Fatalf("recorded 1024 MiB start = %v, want a never-fits refusal naming the command", err)
	}
	// So does a configured project's size under the recorded budget.
	recordBudget(4096)
	projects := map[domain.ProjectID]engine.ManualInitiator{
		"project-big": {PolicyKeys: []domain.PolicyKey{{Key: ward.PolicyWriterMemoryMiB, Value: "8192"}}},
	}
	_, err = (config{MemoryBudgetMiB: 16384}).startupLaunchBound(t.Context(), st, projects)
	if !errors.Is(err, domain.ErrLaunchNeverFits) || !strings.Contains(err.Error(), "freesided set-memory-budget") ||
		!strings.Contains(err.Error(), "project-big") {
		t.Fatalf("recorded 4096 MiB start with an 8192 MiB project = %v", err)
	}
}

// TestRunRefusesARecordedBudgetNoLaunchFits: the daemon's start reads the
// recorded budget from its store, so a restart keeps it over the flag.
func TestRunRefusesARecordedBudgetNoLaunchFits(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	seedTemplateStore(t, dbPath)
	first, err := run(t.Context(), nil, config{
		Environment: environmentEphemeral, DBPath: dbPath, ListenAddr: "127.0.0.1:0", MemoryBudgetMiB: 8192,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	st := storetest.Open(t, dbPath, store.Options{})
	if err := st.WriteInternal(t.Context(), func(tx *store.InternalTx) error {
		return tx.RecordMemoryBudget(t.Context(), 1024, time.Now().UTC())
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	h, err := run(t.Context(), nil, config{
		Environment: environmentEphemeral, DBPath: dbPath, ListenAddr: "127.0.0.1:0", MemoryBudgetMiB: 8192,
	})
	if err == nil {
		_ = h.Close()
		t.Fatal("a start under a recorded budget no launch fits succeeded")
	}
	if !errors.Is(err, domain.ErrLaunchNeverFits) || !strings.Contains(err.Error(), "freesided set-memory-budget") {
		t.Fatalf("start = %v, want a never-fits refusal naming the command", err)
	}
}
