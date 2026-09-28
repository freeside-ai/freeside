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

func setIdentityLimitIdentity(id domain.AuthIdentityID, provider string) domain.AuthIdentity {
	return domain.AuthIdentity{
		ID: id, Provider: provider, AuthStoreMutationLease: true, MaxParallelExecutions: 1,
		// Non-zero operator fields, so a revision that dropped any of them
		// would show in the whole-record comparisons below.
		AccountBinding: "account-" + string(id), UsagePool: "pool-" + string(id), Budget: 500,
		Enabled: true, CostOwner: "owner-" + string(id),
		Interim: domain.InterimClientFacts{
			AuthStoreVolume: "freeside-auth-" + string(id), RefreshStrategy: domain.RefreshOnDemand,
		},
	}
}

func recordSetIdentityLimitIdentities(t *testing.T, st *store.Store, identities ...domain.AuthIdentity) {
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

func runSetIdentityLimit(t *testing.T, ctx context.Context, args ...string) (setIdentityLimitResult, error) {
	t.Helper()
	var stdout bytes.Buffer
	err := runSetIdentityLimitCommand(ctx, args, &stdout, io.Discard)
	var result setIdentityLimitResult
	if err == nil {
		if decodeErr := json.Unmarshal(stdout.Bytes(), &result); decodeErr != nil {
			t.Fatalf("decode %q: %v", stdout.String(), decodeErr)
		}
	}
	return result, err
}

func TestSetIdentityLimitRecordsOnlyTheLimit(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	st := storetest.Open(t, dbPath, store.Options{})
	writer := setIdentityLimitIdentity("claude-writer", "claude")
	reviewer := setIdentityLimitIdentity("codex-reviewer", "openai")
	recordSetIdentityLimitIdentities(t, st, writer, reviewer)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	result, err := runSetIdentityLimit(t, t.Context(),
		"-db", dbPath, "-identity", "claude-writer", "-max-parallel-executions", "4")
	if err != nil {
		t.Fatal(err)
	}
	want := setIdentityLimitResult{
		Identity: "claude-writer", PreviousMaxParallelExecutions: 1, MaxParallelExecutions: 4, Recorded: true,
	}
	if result != want {
		t.Fatalf("result = %#v, want %#v", result, want)
	}
	// Repeating the same limit writes no revision.
	result, err = runSetIdentityLimit(t, t.Context(),
		"-db", dbPath, "-identity", "claude-writer", "-max-parallel-executions", "4")
	if err != nil {
		t.Fatal(err)
	}
	if result.Recorded || result.PreviousMaxParallelExecutions != 4 {
		t.Fatalf("repeat result = %#v, want an unrecorded no-op at 4", result)
	}

	for name, args := range map[string][]string{
		"codex identity":   {"-db", dbPath, "-identity", "codex-reviewer", "-max-parallel-executions", "4"},
		"unknown identity": {"-db", dbPath, "-identity", "missing", "-max-parallel-executions", "4"},
		"zero limit":       {"-db", dbPath, "-identity", "claude-writer", "-max-parallel-executions", "0"},
		"missing limit":    {"-db", dbPath, "-identity", "claude-writer"},
		"missing identity": {"-db", dbPath, "-max-parallel-executions", "4"},
		"missing db":       {"-identity", "claude-writer", "-max-parallel-executions", "4"},
	} {
		if _, err := runSetIdentityLimit(t, t.Context(), args...); err == nil {
			t.Errorf("%s: set-identity-limit succeeded", name)
		}
	}

	st = storetest.Open(t, dbPath, store.Options{})
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		gotWriter, err := tx.GetAuthIdentity(t.Context(), writer.ID)
		if err != nil {
			return err
		}
		writer.MaxParallelExecutions = 4
		if gotWriter != writer {
			t.Errorf("writer = %#v, want %#v", gotWriter, writer)
		}
		gotReviewer, err := tx.GetAuthIdentity(t.Context(), reviewer.ID)
		if err != nil {
			return err
		}
		if gotReviewer != reviewer {
			t.Errorf("reviewer = %#v, want it unchanged", gotReviewer)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSetIdentityLimitUsesRunningDaemonStore(t *testing.T) {
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
	recordSetIdentityLimitIdentities(t, h.store,
		setIdentityLimitIdentity("claude-writer", "claude"), setIdentityLimitIdentity("codex-reviewer", "openai"))
	ctx := context.WithValue(t.Context(), directStoreOpenerKey{}, directStoreOpener(
		func(context.Context, string, store.Options, storeOpenMode) (*store.Store, error) {
			return nil, errors.New("unexpected direct store open")
		}))

	result, err := runSetIdentityLimit(t, ctx,
		"-db", dbPath, "-identity", "claude-writer", "-max-parallel-executions", "8")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Recorded || result.MaxParallelExecutions != 8 {
		t.Fatalf("result = %#v", result)
	}
	for _, identity := range []string{"missing", "codex-reviewer"} {
		if _, err := runSetIdentityLimit(t, ctx,
			"-db", dbPath, "-identity", identity, "-max-parallel-executions", "8",
		); err == nil || !strings.Contains(err.Error(), identity) {
			t.Fatalf("forwarded refusal for %s = %v", identity, err)
		}
	}
	if err := h.store.Read(t.Context(), func(tx *store.ReadTx) error {
		identity, err := tx.GetAuthIdentity(t.Context(), "claude-writer")
		if err == nil && identity.MaxParallelExecutions != 8 {
			t.Errorf("running daemon store limit = %d, want 8", identity.MaxParallelExecutions)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
