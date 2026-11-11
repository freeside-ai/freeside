package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
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

// TestRunRefusesARecordedBudgetNoLaunchFits: the daemon's start reads the
// recorded budget from its store, so a restart keeps it over the flag.
func TestRunRefusesARecordedBudgetNoLaunchFits(t *testing.T) {
	t.Parallel()
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
