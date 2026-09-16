package ward

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func codexAuthRenewalFixture(t *testing.T) (CodexAuthRenewalConfig, *fakeLeaser, *fakeCodexAuthState, *fakeCodexAuthRefresher, []byte) {
	t.Helper()
	enrollment, _, leaser, refresher, _, path := codexAuthEnrollmentFixture(t)
	body := codexHostAuthBody(t, "operator-refresh", codexReviewEpoch.Add(-time.Hour), codexReviewEpoch)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	leaser.identity = domain.AuthIdentity{
		ID: enrollment.AuthIdentityID, Provider: "openai", Enabled: false, AuthStoreMutationLease: true, MaxParallelExecutions: 1,
		Interim: domain.InterimClientFacts{AuthStoreVolume: resolved, RefreshStrategy: domain.RefreshOnDemand, SupportsReadOnlyAuthSnapshot: true},
	}
	state := &fakeCodexAuthState{}
	return CodexAuthRenewalConfig{
		AuthStoreRoot: enrollment.AuthStoreRoot, AuthStorePath: path, AuthIdentityID: enrollment.AuthIdentityID,
		AuthStoreLeaser: leaser, AuthState: state, AuthRefresher: refresher, Now: enrollment.Now,
	}, leaser, state, refresher, body
}

func TestCodexAuthRenewalRotatesExpiredStoreForLegacyDisabledIdentity(t *testing.T) {
	cfg, leaser, state, refresher, before := codexAuthRenewalFixture(t)
	result, err := RenewCodexAuth(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(cfg.AuthStorePath)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Rotated || refresher.calls != 1 || !leaser.released || state.marks != 0 || state.needs ||
		bytes.Equal(before, body) || result.AuthStoreDigest != domain.Digest(contentaddr.Sum(body)) {
		t.Fatal("renewal did not rotate exactly once under a released lease without a hold")
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"operator-refresh", "rotated-refresh", refresher.tokens.AccessToken, "rotated-id"} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatal("renewal result leaked a token")
		}
	}
}

func TestCodexAuthRenewalReadySkipsProvider(t *testing.T) {
	cfg, leaser, state, refresher, _ := codexAuthRenewalFixture(t)
	body := codexHostAuthBody(t, "operator-refresh", codexReviewEpoch.Add(2*time.Hour), codexReviewEpoch)
	if err := os.WriteFile(cfg.AuthStorePath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := RenewCodexAuth(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result.Rotated || refresher.calls != 0 || !leaser.released || state.marks != 0 || result.AuthStoreDigest != domain.Digest(contentaddr.Sum(body)) {
		t.Fatal("ready credential was changed or lease was not released")
	}
}

func TestCodexAuthRenewalRefusesHoldAndIdentityMismatch(t *testing.T) {
	for _, name := range []string{"hold", "provider", "lease", "path", "identity", "snapshot", "strategy"} {
		t.Run(name, func(t *testing.T) {
			cfg, leaser, state, refresher, _ := codexAuthRenewalFixture(t)
			switch name {
			case "hold":
				state.needs = true
			case "provider":
				leaser.identity.Provider = "claude"
			case "lease":
				leaser.identity.AuthStoreMutationLease = false
			case "path":
				leaser.identity.Interim.AuthStoreVolume = "/different/auth.json"
			case "identity":
				leaser.identity.ID = "other"
			case "snapshot":
				leaser.identity.Interim.SupportsReadOnlyAuthSnapshot = false
			case "strategy":
				leaser.identity.Interim.RefreshStrategy = domain.RefreshExternal
			}
			_, err := RenewCodexAuth(t.Context(), cfg)
			if err == nil || len(leaser.holders) != 0 || refresher.calls != 0 || state.marks != 0 {
				t.Fatal("refusal acquired a lease or called the provider")
			}
			if name == "hold" && !strings.Contains(err.Error(), "--recover-codex-credentials") {
				t.Fatal("hold lacks recovery command")
			}
		})
	}
}

func TestCodexAuthRenewalRevokedDoesNotRetryOrLeak(t *testing.T) {
	cfg, leaser, state, refresher, before := codexAuthRenewalFixture(t)
	refresher.err = &CodexAuthRefreshError{Revoked: true, Code: "operator-refresh"}
	for range 2 {
		_, err := RenewCodexAuth(t.Context(), cfg)
		if err == nil || !strings.Contains(err.Error(), "codex login") || !strings.Contains(err.Error(), "freesided enroll-codex") || strings.Contains(err.Error(), "operator-refresh") {
			t.Fatal("revoked chain error missing safe recovery instructions")
		}
	}
	body, err := os.ReadFile(cfg.AuthStorePath)
	if err != nil {
		t.Fatal(err)
	}
	if refresher.calls != 1 || !leaser.released || state.marks != 0 || !bytes.Equal(before, body) {
		t.Fatal("revoked chain was retried or store changed")
	}
}

func TestCodexAuthRenewalRecoversPendingRotation(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "committed"}[committed], func(t *testing.T) {
			cfg, leaser, state, refresher, before := codexAuthRenewalFixture(t)
			rotated := codexHostAuthBody(t, "crash-rotated", codexReviewEpoch.Add(4*time.Hour), codexReviewEpoch)
			seedCodexAuthEnrollmentCrash(t, cfg.AuthStoreRoot, cfg.AuthStorePath, cfg.AuthIdentityID, before, rotated, committed)
			result, err := RenewCodexAuth(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if !result.Rotated || refresher.calls != 0 || !leaser.released || state.marks != 0 || result.AuthStoreDigest != domain.Digest(contentaddr.Sum(rotated)) {
				t.Fatal("pending rotation was not recovered without a provider call")
			}
			for _, path := range []string{codexAuthRefreshIntentPath(cfg.AuthStorePath, cfg.AuthIdentityID), codexAuthRefreshPendingPath(cfg.AuthStorePath, cfg.AuthIdentityID)} {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("recovery left transaction files")
				}
			}
		})
	}
}

func TestCodexAuthRenewalLeaseLossPreservesStore(t *testing.T) {
	cfg, leaser, _, refresher, before := codexAuthRenewalFixture(t)
	leaser.onGet = func(current domain.AuthStoreMutationLease) (domain.AuthStoreMutationLease, error) {
		if refresher.calls != 0 {
			current.Fence++
		}
		return current, nil
	}
	_, err := RenewCodexAuth(context.Background(), cfg)
	if err == nil {
		t.Fatal("renewal committed after lease loss")
	}
	body, err := os.ReadFile(cfg.AuthStorePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, before) {
		t.Fatal("store changed after lease loss")
	}
}
