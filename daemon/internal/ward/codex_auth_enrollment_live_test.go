package ward_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
	"github.com/freeside-ai/freeside/daemon/internal/wardstore"
)

// FREESIDE_CODEX_ENROLLMENT_LIVE_TEST=1 deliberately spends the refresh token
// in INPUT_FILE and leaves the rotated credential at AUTH_STORE_PATH. It is
// CI-blind and must be invoked only with an operator-prepared disposable login.
func TestLiveCodexAuthEnrollmentSpendsAndReplaces(t *testing.T) {
	const requirements = "set FREESIDE_CODEX_ENROLLMENT_LIVE_TEST=1, FREESIDE_CODEX_ENROLLMENT_INPUT_ROOT, FREESIDE_CODEX_ENROLLMENT_INPUT_FILE, FREESIDE_CODEX_ENROLLMENT_AUTH_STORE_ROOT, and FREESIDE_CODEX_ENROLLMENT_AUTH_STORE_PATH (the test deliberately spends the input refresh token and leaves the rotated credential at the output path)"
	if os.Getenv("FREESIDE_CODEX_ENROLLMENT_LIVE_TEST") != "1" {
		t.Skip("live Codex enrollment test skipped: " + requirements)
	}
	inputRoot := os.Getenv("FREESIDE_CODEX_ENROLLMENT_INPUT_ROOT")
	inputFile := os.Getenv("FREESIDE_CODEX_ENROLLMENT_INPUT_FILE")
	storeRoot := os.Getenv("FREESIDE_CODEX_ENROLLMENT_AUTH_STORE_ROOT")
	storePath := os.Getenv("FREESIDE_CODEX_ENROLLMENT_AUTH_STORE_PATH")
	if inputRoot == "" || inputFile == "" || storeRoot == "" || storePath == "" {
		t.Fatal(requirements)
	}
	inputRefresh := liveCodexRefreshToken(t, inputFile)

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	adapters, err := wardstore.New(st)
	if err != nil {
		t.Fatal(err)
	}
	result, err := ward.EnrollCodexAuth(ctx, ward.CodexAuthEnrollmentConfig{
		InputRoot: inputRoot, InputFile: inputFile,
		AuthStoreRoot: storeRoot, AuthStorePath: storePath,
		AuthIdentityID: "codex-enrollment-live", ProjectID: "codex-enrollment-live",
		Journal: adapters.Enrollment, AuthStoreLeaser: adapters.Leaser,
		AuthRefresher: ward.NewCodexAuthHTTPRefresher(),
	})
	if err != nil {
		t.Fatal(err)
	}
	rotatedRefresh := liveCodexRefreshToken(t, storePath)
	if rotatedRefresh == inputRefresh {
		t.Fatal("live auth store retained the spent input refresh token")
	}
	if result.AuthIdentityID != domain.AuthIdentityID("codex-enrollment-live") ||
		result.AuthStoreDigest == "" || result.LeaseFence < 1 ||
		!result.AccessTokenExpiresAt.After(time.Now().UTC().Add(time.Hour)) ||
		result.AttentionItemID == "" {
		t.Fatalf("live enrollment returned incomplete non-secret evidence: %+v", result)
	}
}

func liveCodexRefreshToken(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path) //nolint:gosec // explicit operator-selected live-test credential path
	if err != nil {
		t.Fatal(err)
	}
	var auth struct {
		Tokens *struct {
			RefreshToken *string `json:"refresh_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(body, &auth); err != nil || auth.Tokens == nil ||
		auth.Tokens.RefreshToken == nil || *auth.Tokens.RefreshToken == "" {
		t.Fatal("live auth file is not a refreshable Codex login")
	}
	return *auth.Tokens.RefreshToken
}
