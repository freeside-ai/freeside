package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

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
