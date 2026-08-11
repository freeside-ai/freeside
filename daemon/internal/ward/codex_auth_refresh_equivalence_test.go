package ward

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

type legacyCodexAuthRotationOutcome string

const (
	legacyCodexAuthRotationSucceeded   legacyCodexAuthRotationOutcome = "succeeded"
	legacyCodexAuthRotationReenroll    legacyCodexAuthRotationOutcome = "reenroll"
	legacyCodexAuthRotationOperational legacyCodexAuthRotationOutcome = "operational"
)

// TestCodexAuthRefreshExtractionMatchesLegacyDecisionCorpus reconstructs the
// pre-extraction transaction from origin/main and compares its externally
// relevant decisions and committed bytes with the shared implementation over
// a deterministic adversarial token corpus.
func TestCodexAuthRefreshExtractionMatchesLegacyDecisionCorpus(t *testing.T) {
	for i := range 96 {
		validatedAt := codexReviewEpoch.Add(time.Duration(i) * time.Second)
		predecessorAccess := codexReviewJWT(t, validatedAt.Add(30*time.Minute))
		rotated := CodexAuthRefreshTokens{
			IDToken: "rotated-id", AccessToken: codexReviewJWT(t, validatedAt.Add(4*time.Hour)),
			RefreshToken: "rotated-refresh-" + time.Duration(i).String(),
		}
		switch i % 8 {
		case 1:
			rotated.RefreshToken = "predecessor-refresh"
		case 2:
			rotated.RefreshToken = ""
		case 3:
			rotated.RefreshToken = predecessorAccess
		case 4:
			rotated.AccessToken = "predecessor-refresh"
		case 5:
			rotated.AccessToken = codexReviewJWT(t, validatedAt.Add(30*time.Minute))
		case 6:
			rotated.AccessToken = ""
		case 7:
			rotated.IDToken = ""
		}

		legacyCfg, legacyPath, legacyAuth, legacyLease, legacyHolder := codexAuthRotationEquivalenceFixture(t, validatedAt, predecessorAccess, rotated)
		legacyOutcome, legacyBody := legacyCodexAuthRotation(
			context.Background(), legacyCfg, "codex-equivalence", legacyPath,
			legacyAuth, legacyLease, legacyHolder,
		)

		currentCfg, currentPath, currentAuth, currentLease, currentHolder := codexAuthRotationEquivalenceFixture(t, validatedAt, predecessorAccess, rotated)
		currentBody, _, currentErr := rotateCodexAuthStoreUnderLease(
			context.Background(), currentCfg, "codex-equivalence", currentPath,
			mustCodexAuthBody(t, currentCfg.InputRoot, currentPath),
			mustCodexAuthMetadata(t, currentCfg.InputRoot, currentPath),
			currentAuth, currentLease, currentHolder, false,
		)
		currentOutcome := legacyCodexAuthRotationSucceeded
		if currentErr != nil {
			var operational *codexAuthRefreshOperationalError
			if errors.As(currentErr, &operational) {
				currentOutcome = legacyCodexAuthRotationOperational
			} else {
				currentOutcome = legacyCodexAuthRotationReenroll
			}
		}
		if currentOutcome != legacyOutcome {
			t.Fatalf("case %d outcome = %s, legacy %s", i, currentOutcome, legacyOutcome)
		}
		if currentOutcome == legacyCodexAuthRotationSucceeded {
			if string(currentBody) != string(legacyBody) {
				t.Fatalf("case %d committed body diverged from legacy", i)
			}
			stored, err := os.ReadFile(currentPath) //nolint:gosec // fixture path is under t.TempDir
			if err != nil || string(stored) != string(legacyBody) {
				t.Fatalf("case %d stored body diverged from legacy: %v", i, err)
			}
		}
	}
}

func codexAuthRotationEquivalenceFixture(
	t *testing.T, now time.Time, predecessorAccess string, rotated CodexAuthRefreshTokens,
) (CodexReviewConfig, string, codexAuthFile, domain.AuthStoreMutationLease, domain.InvocationID) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "auth")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "auth.json")
	body := codexHostAuthBody(t, "predecessor-refresh", now.Add(30*time.Minute), now)
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	raw["tokens"].(map[string]any)["access_token"] = predecessorAccess
	body, _ = json.Marshal(raw)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	holder := domain.InvocationID("equivalence-holder")
	lease := domain.AuthStoreMutationLease{
		AuthIdentityID: "codex-equivalence", Holder: holder, Fence: 1,
		AcquiredAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
	leaser := &fakeLeaser{lease: lease}
	cfg := CodexReviewConfig{
		InputRoot: root, AuthStoreLeaser: leaser,
		AuthRefresher:               &fakeCodexAuthRefresher{tokens: rotated},
		AccessTokenRefreshThreshold: 2 * time.Hour,
		Now:                         func() time.Time { return now },
	}
	auth, _, err := inspectCodexHostAuth(CodexAuthSubscription, body)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, path, auth, lease, holder
}

func mustCodexAuthBody(t *testing.T, root, path string) []byte {
	t.Helper()
	_, body, _, err := readCodexReviewInputWithMetadata(root, path, maxCodexAuthSnapshotBytes)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func mustCodexAuthMetadata(t *testing.T, root, path string) codexReviewInputMetadata {
	t.Helper()
	_, _, metadata, err := readCodexReviewInputWithMetadata(root, path, maxCodexAuthSnapshotBytes)
	if err != nil {
		t.Fatal(err)
	}
	return metadata
}

func legacyCodexAuthRotation(
	ctx context.Context,
	cfg CodexReviewConfig,
	id domain.AuthIdentityID,
	path string,
	auth codexAuthFile,
	lease domain.AuthStoreMutationLease,
	holder domain.InvocationID,
) (legacyCodexAuthRotationOutcome, []byte) {
	body := mustCodexAuthBodyNoTest(cfg.InputRoot, path)
	metadata, ok := codexAuthMetadataNoTest(cfg.InputRoot, path)
	if body == nil || !ok || auth.Tokens == nil || auth.Tokens.RefreshToken == nil {
		return legacyCodexAuthRotationReenroll, nil
	}
	predecessor := newCodexAuthRefreshPredecessor(body, metadata)
	if verifyCodexAuthRefreshLease(ctx, cfg, lease, id, holder) != nil {
		return legacyCodexAuthRotationOperational, nil
	}
	if err := writeCodexAuthRefreshIntent(path, id, predecessor, cfg.Now()); err != nil {
		if errors.Is(err, errCodexAuthRefreshIntentExists) {
			return legacyCodexAuthRotationReenroll, nil
		}
		return legacyCodexAuthRotationOperational, nil
	}
	if verifyCodexAuthRefreshLease(ctx, cfg, lease, id, holder) != nil {
		return legacyCodexAuthRotationReenroll, nil
	}
	rotated, err := cfg.AuthRefresher.RefreshCodexAuth(ctx, *auth.Tokens.RefreshToken)
	if err != nil || verifyCodexAuthRefreshLease(ctx, cfg, lease, id, holder) != nil {
		return legacyCodexAuthRotationReenroll, nil
	}
	rotatedTokens := *auth.Tokens
	if rotated.IDToken != "" {
		rotatedTokens.IDToken = rotated.IDToken
	}
	rotatedTokens.AccessToken = rotated.AccessToken
	rotatedTokens.RefreshToken = &rotated.RefreshToken
	validatedAt := cfg.Now()
	if validateCodexAuthRotation(auth.Tokens, &rotatedTokens, validatedAt, codexAuthRefreshThreshold(cfg)) != nil {
		return legacyCodexAuthRotationReenroll, nil
	}
	*auth.Tokens = rotatedTokens
	lastRefresh, err := json.Marshal(validatedAt)
	if err != nil {
		return legacyCodexAuthRotationReenroll, nil
	}
	auth.LastRefresh = lastRefresh
	rotatedBody, err := json.Marshal(auth)
	if err != nil {
		return legacyCodexAuthRotationReenroll, nil
	}
	if _, _, err := inspectCodexHostAuth(CodexAuthSubscription, rotatedBody); err != nil {
		return legacyCodexAuthRotationReenroll, nil
	}
	if bindCodexAuthRefreshIntent(
		cfg.InputRoot, path, id, predecessor, rotatedBody, validatedAt,
	) != nil {
		return legacyCodexAuthRotationReenroll, nil
	}
	pending, err := stageCodexAuthStore(cfg.InputRoot, path, id, predecessor, rotatedBody)
	if err != nil {
		return legacyCodexAuthRotationReenroll, nil
	}
	if verifyCodexAuthRefreshLease(ctx, cfg, lease, id, holder) != nil {
		return legacyCodexAuthRotationOperational, nil
	}
	if commitCodexAuthStore(cfg.InputRoot, path, pending, predecessor, rotatedBody) != nil ||
		os.Remove(codexAuthRefreshIntentPath(path, id)) != nil {
		return legacyCodexAuthRotationOperational, nil
	}
	return legacyCodexAuthRotationSucceeded, rotatedBody
}

func mustCodexAuthBodyNoTest(root, path string) []byte {
	_, body, _, err := readCodexReviewInputWithMetadata(root, path, maxCodexAuthSnapshotBytes)
	if err != nil {
		return nil
	}
	return body
}

func codexAuthMetadataNoTest(root, path string) (codexReviewInputMetadata, bool) {
	_, _, metadata, err := readCodexReviewInputWithMetadata(root, path, maxCodexAuthSnapshotBytes)
	return metadata, err == nil
}
