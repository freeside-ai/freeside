package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

func setPoolLimitIdentity(id domain.AuthIdentityID, provider string) domain.AuthIdentity {
	return domain.AuthIdentity{
		ID: id, Provider: provider, AuthStoreMutationLease: true,
		// Non-zero operator fields, so a revision that touched any of them
		// would show in the whole-record comparisons below.
		AccountBinding: "account-" + string(id), UsagePool: "pool-" + string(id), Budget: 500,
		Enabled: true, CostOwner: "owner-" + string(id),
		Interim: domain.InterimClientFacts{
			AuthStoreVolume: "freeside-auth-" + string(id), RefreshStrategy: domain.RefreshOnDemand,
		},
	}
}

func recordSetPoolLimitIdentities(t *testing.T, st *store.Store, identities ...domain.AuthIdentity) {
	t.Helper()
	if err := st.WriteInternal(t.Context(), func(tx *store.InternalTx) error {
		for _, identity := range identities {
			if err := tx.RecordAuthIdentity(t.Context(), identity, time.Now().UTC().Add(-time.Minute)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func runSetPoolLimit(t *testing.T, ctx context.Context, args ...string) (setPoolLimitResult, error) {
	t.Helper()
	var stdout bytes.Buffer
	err := runSetPoolLimitCommand(ctx, args, &stdout, io.Discard)
	var result setPoolLimitResult
	if err == nil {
		if decodeErr := json.Unmarshal(stdout.Bytes(), &result); decodeErr != nil {
			t.Fatalf("decode %q: %v", stdout.String(), decodeErr)
		}
	}
	return result, err
}

func TestSetPoolLimitRecordsOnlyTheLimit(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	st := storetest.Open(t, dbPath, store.Options{})
	writer := setPoolLimitIdentity("claude-writer", "claude")
	reviewer := setPoolLimitIdentity("codex-reviewer", "openai")
	recordSetPoolLimitIdentities(t, st, writer, reviewer)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	result, err := runSetPoolLimit(t, t.Context(),
		"-db", dbPath, "-pool", "pool-claude-writer", "-max-parallel-executions", "4")
	if err != nil {
		t.Fatal(err)
	}
	want := setPoolLimitResult{
		Pool: "pool-claude-writer", PreviousMaxParallelExecutions: 1, MaxParallelExecutions: 4, Recorded: true,
	}
	if result != want {
		t.Fatalf("result = %#v, want %#v", result, want)
	}
	// Repeating the same limit writes no revision.
	result, err = runSetPoolLimit(t, t.Context(),
		"-db", dbPath, "-pool", "pool-claude-writer", "-max-parallel-executions", "4")
	if err != nil {
		t.Fatal(err)
	}
	if result.Recorded || result.PreviousMaxParallelExecutions != 4 {
		t.Fatalf("repeat result = %#v, want an unrecorded no-op at 4", result)
	}

	for name, args := range map[string][]string{
		"unknown pool":  {"-db", dbPath, "-pool", "missing", "-max-parallel-executions", "4"},
		"zero limit":    {"-db", dbPath, "-pool", "pool-claude-writer", "-max-parallel-executions", "0"},
		"missing limit": {"-db", dbPath, "-pool", "pool-claude-writer"},
		"missing pool":  {"-db", dbPath, "-max-parallel-executions", "4"},
		"missing db":    {"-pool", "pool-claude-writer", "-max-parallel-executions", "4"},
	} {
		if _, err := runSetPoolLimit(t, t.Context(), args...); err == nil {
			t.Errorf("%s: set-pool-limit succeeded", name)
		}
	}

	st = storetest.Open(t, dbPath, store.Options{})
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		gotWriter, err := tx.GetAuthIdentity(t.Context(), writer.ID)
		if err != nil {
			return err
		}
		if gotWriter != writer {
			t.Errorf("writer = %#v, want it unchanged", gotWriter)
		}
		gotPool, err := tx.GetUsagePool(t.Context(), writer.EffectiveUsagePool())
		if err != nil {
			return err
		}
		if want := (domain.UsagePool{
			ID: writer.EffectiveUsagePool(), Provider: "claude", MaxParallelExecutions: 4,
		}); gotPool != want {
			t.Errorf("pool = %#v, want %#v", gotPool, want)
		}
		other, err := tx.GetUsagePool(t.Context(), reviewer.EffectiveUsagePool())
		if err != nil {
			return err
		}
		if other.MaxParallelExecutions != 1 {
			t.Errorf("other pool limit = %d, want it unchanged", other.MaxParallelExecutions)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestSetPoolLimitTakesTheIDAuthListPrints: an identity with no pool of its own
// is limited through its derived pool, which auth list names for the operator.
func TestSetPoolLimitTakesTheIDAuthListPrints(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	st := storetest.Open(t, dbPath, store.Options{})
	solo := setPoolLimitIdentity("claude-solo", "claude")
	solo.UsagePool = ""
	recordSetPoolLimitIdentities(t, st, solo)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	listed := runAuthListForTest(t, dbPath).Identities
	if len(listed) != 1 || listed[0].UsagePool != "identity:claude-solo" || listed[0].MaxParallelExecutions != 1 {
		t.Fatalf("auth list = %#v", listed)
	}
	if _, err := runSetPoolLimit(t, t.Context(), "-db", dbPath,
		"-pool", string(listed[0].UsagePool), "-max-parallel-executions", "3"); err != nil {
		t.Fatal(err)
	}
	listed = runAuthListForTest(t, dbPath).Identities
	if len(listed) != 1 || listed[0].MaxParallelExecutions != 3 {
		t.Fatalf("auth list after the limit = %#v", listed)
	}
}

func TestSetPoolLimitUsesRunningDaemonStore(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
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
	recordSetPoolLimitIdentities(t, h.store,
		setPoolLimitIdentity("claude-writer", "claude"), setPoolLimitIdentity("codex-reviewer", "openai"))
	ctx := context.WithValue(t.Context(), directStoreOpenerKey{}, directStoreOpener(
		func(context.Context, string, store.Options, storeOpenMode) (*store.Store, error) {
			return nil, errors.New("unexpected direct store open")
		}))

	result, err := runSetPoolLimit(t, ctx,
		"-db", dbPath, "-pool", "pool-claude-writer", "-max-parallel-executions", "8")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Recorded || result.MaxParallelExecutions != 8 {
		t.Fatalf("result = %#v", result)
	}
	if _, err := runSetPoolLimit(t, ctx,
		"-db", dbPath, "-pool", "missing", "-max-parallel-executions", "8",
	); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("forwarded refusal = %v", err)
	}
	if err := h.store.Read(t.Context(), func(tx *store.ReadTx) error {
		pool, err := tx.GetUsagePool(t.Context(), "pool-claude-writer")
		if err == nil && pool.MaxParallelExecutions != 8 {
			t.Errorf("running daemon store limit = %d, want 8", pool.MaxParallelExecutions)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
