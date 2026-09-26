package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

func TestRenewCodexRecoveryWithExecutionDisabled(t *testing.T) {
	db := filepath.Join(t.TempDir(), "freeside.db")
	_, output, _, err := runEnrollCodexAgainst(t, db)
	if err != nil {
		t.Fatal(err)
	}
	var enrolled ward.CodexAuthEnrollmentResult
	if err := json.Unmarshal([]byte(output), &enrolled); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	h, err := run(ctx, nil, config{Environment: environmentEphemeral, DBPath: db, ListenAddr: "127.0.0.1:0"})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	if h.workflow != nil || h.driver != nil {
		t.Fatal("recovery started execution")
	}
	if err := h.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutDevice(ctx, domain.Device{ID: "recovery-device", DisplayName: "Recovery", Status: domain.DeviceActive, PairedAt: time.Now().UTC()})
	}); err != nil {
		t.Fatal(err)
	}
	item, err := h.attention.GetAttentionItem(ctx, enrolled.AttentionItemID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.attention.Submit(ctx, signet.ClientCommand{
		CommandID: "recover-codex", DeviceID: "recovery-device", ExpectedEntityVersion: item.EntityVersion,
		Payload: signet.DecisionPayload{ItemID: item.Item.ID, ItemVersion: item.Item.ItemVersion, Action: domain.ActionResolveReenrollment},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Read(ctx, func(tx *store.ReadTx) error {
		transition, found, err := tx.LatestCodexReenrollmentRecoveryTransition(ctx, enrolled.AuthIdentityID)
		if err != nil {
			return err
		}
		if !found || transition.CommandID == nil || *transition.CommandID != "recover-codex" {
			t.Fatal("recovery lacks a persisted device command")
		}
		tasks, err := tx.ListTasks(ctx)
		if len(tasks) != 0 {
			t.Fatal("recovery created a task")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func renewalCommandFixture(t *testing.T, dbPath string, approved map[domain.Digest]bool) []string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "auth")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "auth.json")
	if err := os.WriteFile(path, commandCodexAuth("operator-refresh"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, _, err := openStoreWithTopicKey(t.Context(), dbPath, store.Options{ApprovedRecipes: approved})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(t.Context(), func(tx *store.InternalTx) error {
		return tx.RecordAuthIdentity(t.Context(), domain.AuthIdentity{
			ID: "codex-primary", Provider: "openai", Enabled: false, AuthStoreMutationLease: true, MaxParallelExecutions: 1,
			Interim: domain.InterimClientFacts{AuthStoreVolume: path, RefreshStrategy: domain.RefreshOnDemand, SupportsReadOnlyAuthSnapshot: true},
		}, time.Now().UTC())
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return []string{"-db", dbPath, "-auth-identity", "codex-primary", "-auth-store-root", root, "-auth-store", path}
}

func TestRenewCodexCommandReadyWithoutHoldForLegacyDisabledIdentity(t *testing.T) {
	args := renewalCommandFixture(t, filepath.Join(t.TempDir(), "freeside.db"), nil)
	refresher := &commandCodexAuthRefresher{}
	var stdout, stderr bytes.Buffer
	if err := runRenewCodexCommandWithRefresher(t.Context(), args, &stdout, &stderr, refresher); err != nil {
		t.Fatal(err)
	}
	var result ward.CodexAuthRenewalResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if refresher.calls != 1 || !result.Rotated || result.AuthStoreDigest == "" || result.AuthIdentityID != "codex-primary" {
		t.Fatal("renewal did not report rotation")
	}
	for _, secret := range []string{"operator-refresh", "rotated-refresh", "rotated-id", commandCodexJWT(result.AccessTokenExpiresAt)} {
		if strings.Contains(stdout.String()+stderr.String(), secret) {
			t.Fatal("command output leaked a token")
		}
	}
	// The second invocation succeeds without a provider call, proving the first
	// released its lease and left no re-enrollment hold in the real store.
	if err := runRenewCodexCommandWithRefresher(t.Context(), args, &bytes.Buffer{}, &stderr, refresher); err != nil {
		t.Fatal(err)
	}
	if refresher.calls != 1 {
		t.Fatal("ready store refreshed twice")
	}
}

func TestRenewCodexCommandRefusesSchemaMigration(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	args := renewalCommandFixture(t, dbPath, nil)
	version, err := store.CurrentSchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close() //nolint:errcheck // test cleanup
	if _, err := raw.ExecContext(t.Context(), `DELETE FROM schema_migrations WHERE version = ?`, version); err != nil {
		t.Fatal(err)
	}
	refresher := &commandCodexAuthRefresher{}
	var stdout, stderr bytes.Buffer
	err = runRenewCodexCommandWithRefresher(t.Context(), args, &stdout, &stderr, refresher)
	if err == nil || !strings.Contains(err.Error(), "database schema version") || refresher.calls != 0 || stdout.Len() != 0 {
		t.Fatalf("renewal did not refuse the older schema before refresh: %v", err)
	}
	var after int
	if err := raw.QueryRowContext(t.Context(), `SELECT MAX(version) FROM schema_migrations`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != version-1 {
		t.Fatal("refused renewal migrated the database")
	}
}

func TestRenewCodexCommandFailsClosedWithoutApprovedRecipe(t *testing.T) {
	db := filepath.Join(t.TempDir(), "freeside.db")
	recipe := seedRecipeGatedEvidence(t, db)
	args := renewalCommandFixture(t, db, map[domain.Digest]bool{recipe: true})
	refresher := &commandCodexAuthRefresher{}
	var stdout, stderr bytes.Buffer
	err := runRenewCodexCommandWithRefresher(t.Context(), args, &stdout, &stderr, refresher)
	if !errors.Is(err, domain.ErrUnapprovedRecipe) || !strings.Contains(err.Error(), "-approved-recipe") || refresher.calls != 0 || stdout.Len() != 0 {
		t.Fatalf("recipe gate was not enforced before refresh: %v", err)
	}
	args = append(args, "-approved-recipe", string(recipe))
	if err := runRenewCodexCommandWithRefresher(t.Context(), args, &stdout, &stderr, refresher); err != nil {
		t.Fatal(err)
	}
}

func TestRenewCodexCommandFlagValidation(t *testing.T) {
	valid := []string{"-db", "db", "-auth-identity", "id", "-auth-store-root", "root", "-auth-store", "store"}
	for i := 0; i < len(valid); i += 2 {
		t.Run(valid[i], func(t *testing.T) {
			args := append(append([]string{}, valid[:i]...), valid[i+2:]...)
			err := runRenewCodexCommandWithRefresher(t.Context(), args, &bytes.Buffer{}, &bytes.Buffer{}, &commandCodexAuthRefresher{})
			if err == nil || !strings.Contains(err.Error(), valid[i]+" is required") {
				t.Fatalf("missing flag error = %v", err)
			}
		})
	}
	for _, extra := range [][]string{{"positional"}, {"-unknown"}, {"-approved-recipe", "invalid"}} {
		err := runRenewCodexCommandWithRefresher(t.Context(), append(valid, extra...), &bytes.Buffer{}, &bytes.Buffer{}, &commandCodexAuthRefresher{})
		if err == nil {
			t.Fatal("invalid flags accepted")
		}
	}
}
