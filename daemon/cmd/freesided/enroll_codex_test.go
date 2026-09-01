package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

type commandCodexAuthRefresher struct {
	calls int
	input string
}

func (f *commandCodexAuthRefresher) RefreshCodexAuth(
	_ context.Context, refreshToken string,
) (ward.CodexAuthRefreshTokens, error) {
	f.calls++
	f.input = refreshToken
	return ward.CodexAuthRefreshTokens{
		IDToken: "rotated-id", AccessToken: commandCodexJWT(time.Now().UTC().Add(4 * time.Hour)),
		RefreshToken: "rotated-refresh",
	}, nil
}

func TestEnrollCodexCommandRunsVerifiedEnrollment(t *testing.T) {
	root := t.TempDir()
	inputRoot := filepath.Join(root, "input")
	storeRoot := filepath.Join(root, "store")
	for _, path := range []string{inputRoot, storeRoot} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	inputPath := filepath.Join(inputRoot, "auth.json")
	storePath := filepath.Join(storeRoot, "auth.json")
	if err := os.WriteFile(inputPath, commandCodexAuth("operator-refresh"), 0o600); err != nil {
		t.Fatal(err)
	}
	refresher := &commandCodexAuthRefresher{}
	var stdout, stderr bytes.Buffer
	err := runEnrollCodexCommandWithRefresher(context.Background(), []string{
		"-db", filepath.Join(root, "freeside.db"),
		"-project", "project-1",
		"-auth-identity", "codex-primary",
		"-input-root", inputRoot,
		"-input-file", inputPath,
		"-auth-store-root", storeRoot,
		"-auth-store", storePath,
	}, &stdout, &stderr, refresher)
	if err != nil {
		t.Fatalf("run enrollment: %v; stderr = %s", err, stderr.String())
	}
	if refresher.calls != 1 || refresher.input != "operator-refresh" {
		t.Fatalf("refresh calls = %d, input = %q", refresher.calls, refresher.input)
	}
	var result ward.CodexAuthEnrollmentResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode result %q: %v", stdout.String(), err)
	}
	if result.AuthIdentityID != "codex-primary" || result.AuthStorePath == "" ||
		result.LeaseFence != 1 || result.AuthStoreDigest == "" ||
		result.AttentionItemID == "" || result.AttentionItemVersion != 2 {
		t.Fatalf("result = %+v", result)
	}
	body, err := os.ReadFile(storePath) //nolint:gosec // test path is under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	var auth struct {
		Tokens struct {
			RefreshToken string `json:"refresh_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(body, &auth); err != nil || auth.Tokens.RefreshToken != "rotated-refresh" {
		t.Fatalf("stored auth was not rotated: %v", err)
	}
}

// seedRecipeGatedEvidence writes one AttentionItem carrying a verifier-produced
// evidence artifact into a fresh store at dbPath, and returns the approved
// recipe digest the artifact was produced under. Every production store holds
// such recipe-gated evidence, so enroll-codex must open the store with this
// digest or the ListAttentionItems re-gate fails closed on the row (#759). The
// seed open carries the approved set because PutAttentionItem runs the same
// gate on write.
func seedRecipeGatedEvidence(t *testing.T, dbPath string) domain.Digest {
	t.Helper()
	recipe := domain.Digest("sha256:" + strings.Repeat("a", 64))
	approved := map[domain.Digest]bool{recipe: true}
	art, err := domain.NewArtifact(domain.ArtifactInput{
		ID: "art-evidence", Type: domain.ArtifactKindVerificationReport, Digest: "sha256:evidence",
		Provenance: domain.Provenance{
			ProducerClass:            domain.ProducerVerifier,
			ProducerInvocationID:     "inv-verify",
			HeadBinding:              domain.HeadIndependent,
			VerificationRecipeDigest: &recipe,
			SensitivityClass:         domain.SensitivityNormal,
		},
		Metadata: testRunEvidenceMetadata(1),
	}, approved)
	if err != nil {
		t.Fatalf("NewArtifact: %v", err)
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-evidence", ProjectID: "project-1",
		Subject:           domain.Subject{Type: domain.SubjectSystem, ID: "sys-1"},
		Type:              domain.AttentionExecutionFailure,
		Priority:          domain.PriorityNormal,
		Reason:            "recipe-gated evidence present",
		InterruptionClass: domain.InterruptionExceptional,
		EvidenceSnapshot:  []domain.Artifact{art},
		ItemVersion:       1,
		Status:            domain.StatusOpen,
	}, approved)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	ctx := context.Background()
	st, _, err := openStoreWithTopicKey(
		ctx, dbPath, store.Options{ApprovedRecipes: approved},
	)
	if err != nil {
		t.Fatalf("open seed store: %v", err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatalf("seed attention item: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}
	return recipe
}

// runEnrollCodexAgainst runs the enroll-codex command against an existing store
// at dbPath, using the same operator-login fixtures as the happy-path test and
// appending extraArgs (e.g. -approved-recipe <digest>).
func runEnrollCodexAgainst(
	t *testing.T, dbPath string, extraArgs ...string,
) (*commandCodexAuthRefresher, string, string, error) {
	t.Helper()
	root := t.TempDir()
	inputRoot := filepath.Join(root, "input")
	storeRoot := filepath.Join(root, "store")
	for _, path := range []string{inputRoot, storeRoot} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	inputPath := filepath.Join(inputRoot, "auth.json")
	storePath := filepath.Join(storeRoot, "auth.json")
	if err := os.WriteFile(inputPath, commandCodexAuth("operator-refresh"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := append([]string{
		"-db", dbPath,
		"-project", "project-1",
		"-auth-identity", "codex-primary",
		"-input-root", inputRoot,
		"-input-file", inputPath,
		"-auth-store-root", storeRoot,
		"-auth-store", storePath,
	}, extraArgs...)
	refresher := &commandCodexAuthRefresher{}
	var stdout, stderr bytes.Buffer
	err := runEnrollCodexCommandWithRefresher(context.Background(), args, &stdout, &stderr, refresher)
	return refresher, stdout.String(), stderr.String(), err
}

func TestEnrollCodexCommandFailsClosedWithoutApprovedRecipe(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	seedRecipeGatedEvidence(t, dbPath)

	_, _, stderr, err := runEnrollCodexAgainst(t, dbPath)
	if err == nil {
		t.Fatalf("enroll-codex without -approved-recipe succeeded; stderr = %s", stderr)
	}
	if !errors.Is(err, domain.ErrUnapprovedRecipe) {
		t.Fatalf("error is not ErrUnapprovedRecipe: %v", err)
	}
	if !strings.Contains(err.Error(), "-approved-recipe") {
		t.Fatalf("error does not name -approved-recipe: %v", err)
	}
}

func TestEnrollCodexCommandEnrollsWithApprovedRecipe(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	recipe := seedRecipeGatedEvidence(t, dbPath)

	refresher, stdout, stderr, err := runEnrollCodexAgainst(t, dbPath, "-approved-recipe", string(recipe))
	if err != nil {
		t.Fatalf("enroll-codex with -approved-recipe: %v; stderr = %s", err, stderr)
	}
	if refresher.calls != 1 || refresher.input != "operator-refresh" {
		t.Fatalf("refresh calls = %d, input = %q", refresher.calls, refresher.input)
	}
	var result ward.CodexAuthEnrollmentResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("decode result %q: %v", stdout, err)
	}
	if result.AuthIdentityID != "codex-primary" || result.AuthStorePath == "" ||
		result.LeaseFence != 1 || result.AuthStoreDigest == "" || result.AttentionItemID == "" {
		t.Fatalf("result = %+v", result)
	}
}

func TestEnrollCodexCommandRequiresEveryBinding(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := runEnrollCodexCommandWithRefresher(
		context.Background(), nil, &stdout, &stderr, &commandCodexAuthRefresher{},
	)
	if err == nil || err.Error() != "-db is required" {
		t.Fatalf("missing flags = %v", err)
	}
}

func commandCodexAuth(refresh string) []byte {
	body, _ := json.Marshal(map[string]any{
		"OPENAI_API_KEY": nil,
		"tokens": map[string]any{
			"id_token": "operator-id", "access_token": commandCodexJWT(time.Now().UTC().Add(30 * time.Minute)),
			"refresh_token": refresh,
		},
		"last_refresh": time.Now().UTC().Format(time.RFC3339Nano),
	})
	return body
}

func commandCodexJWT(expires time.Time) string {
	payload, _ := json.Marshal(map[string]int64{"exp": expires.Unix()})
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}
