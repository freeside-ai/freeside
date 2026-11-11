package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

func runSetMemoryBudget(t *testing.T, ctx context.Context, args ...string) (setMemoryBudgetResult, error) {
	t.Helper()
	var stdout bytes.Buffer
	err := runSetMemoryBudgetCommand(ctx, args, &stdout, io.Discard)
	var result setMemoryBudgetResult
	if err == nil {
		if decodeErr := json.Unmarshal(stdout.Bytes(), &result); decodeErr != nil {
			t.Fatalf("decode %q: %v", stdout.String(), decodeErr)
		}
	}
	return result, err
}

func recordedMemoryBudget(t *testing.T, st *store.Store) (int, bool) {
	t.Helper()
	var (
		budgetMiB int
		recorded  bool
	)
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		budgetMiB, recorded, err = tx.MemoryBudget(t.Context())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return budgetMiB, recorded
}

// TestSetMemoryBudgetRecordsOnlyTheBudget: the first budget reports no
// previous one, a change reports the one it replaces, a repeat writes
// nothing, and a budget below 1 MiB or a missing flag is refused and leaves
// the recorded one. No client syncs the budget, so none of it moves the
// revision.
func TestSetMemoryBudgetRecordsOnlyTheBudget(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	st := storetest.Open(t, dbPath, store.Options{})
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	revision := func() int64 {
		t.Helper()
		st := storetest.Open(t, dbPath, store.Options{})
		state, err := st.ServerState(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
		return state.Revision
	}

	before := revision()
	result, err := runSetMemoryBudget(t, t.Context(), "-db", dbPath, "-memory-budget-mib", "8192")
	if err != nil {
		t.Fatal(err)
	}
	if result != (setMemoryBudgetResult{MemoryBudgetMiB: 8192, Recorded: true}) {
		t.Fatalf("first result = %#v, want 8192 recorded with no previous budget", result)
	}
	if got := revision(); got != before {
		t.Fatalf("revision after the first budget = %d, want %d", got, before)
	}
	result, err = runSetMemoryBudget(t, t.Context(), "-db", dbPath, "-memory-budget-mib", "4096")
	if err != nil {
		t.Fatal(err)
	}
	if result.PreviousMemoryBudgetMiB == nil || *result.PreviousMemoryBudgetMiB != 8192 ||
		result.MemoryBudgetMiB != 4096 || !result.Recorded {
		t.Fatalf("change result = %#v, want 8192 replaced by 4096", result)
	}
	result, err = runSetMemoryBudget(t, t.Context(), "-db", dbPath, "-memory-budget-mib", "4096")
	if err != nil {
		t.Fatal(err)
	}
	if result.Recorded || result.PreviousMemoryBudgetMiB == nil || *result.PreviousMemoryBudgetMiB != 4096 {
		t.Fatalf("repeat result = %#v, want an unrecorded no-op at 4096", result)
	}

	for name, args := range map[string][]string{
		"zero budget":     {"-db", dbPath, "-memory-budget-mib", "0"},
		"negative budget": {"-db", dbPath, "-memory-budget-mib", "-1"},
		"missing budget":  {"-db", dbPath},
		"missing db":      {"-memory-budget-mib", "4096"},
		"positional":      {"-db", dbPath, "-memory-budget-mib", "4096", "extra"},
	} {
		if _, err := runSetMemoryBudget(t, t.Context(), args...); err == nil {
			t.Errorf("%s: set-memory-budget succeeded", name)
		}
	}
	st = storetest.Open(t, dbPath, store.Options{})
	if got, recorded := recordedMemoryBudget(t, st); !recorded || got != 4096 {
		t.Fatalf("recorded budget = %d, %t; want 4096", got, recorded)
	}
}

func TestSetMemoryBudgetUsesRunningDaemonStore(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	seedTemplateStore(t, dbPath)
	h, err := run(t.Context(), nil, config{
		Environment: environmentEphemeral, DBPath: dbPath, ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx := context.WithValue(t.Context(), directStoreOpenerKey{}, directStoreOpener(
		func(context.Context, string, store.Options, storeOpenMode) (*store.Store, error) {
			return nil, errors.New("unexpected direct store open")
		}))

	result, err := runSetMemoryBudget(t, ctx, "-db", dbPath, "-memory-budget-mib", "6144")
	if err != nil {
		t.Fatal(err)
	}
	if result != (setMemoryBudgetResult{MemoryBudgetMiB: 6144, Recorded: true}) {
		t.Fatalf("result = %#v", result)
	}
	if got, recorded := recordedMemoryBudget(t, h.Store); !recorded || got != 6144 {
		t.Fatalf("running daemon store budget = %d, %t; want 6144", got, recorded)
	}
}
