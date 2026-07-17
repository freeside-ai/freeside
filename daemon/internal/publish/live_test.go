package publish_test

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

// parseTestPEM parses the PKCS#1 PEM GitHub issues for App keys.
func parseTestPEM(raw []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("key is not PEM")
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

// TestLiveMintInstallationToken exercises the App JWT and
// installation-token lifecycle against the real GitHub API. It is
// opt-in (issue #80 acceptance 1): CI documents it as Not run. It
// needs an already-registered App (registration requires a browser and
// is not live-tested).
func TestLiveMintInstallationToken(t *testing.T) {
	if os.Getenv("FREESIDE_PUBLISH_LIVE_TEST") != "1" {
		t.Skip("live GitHub integration is opt-in: set FREESIDE_PUBLISH_LIVE_TEST=1, " +
			"FREESIDE_PUBLISH_LIVE_APP_ID, FREESIDE_PUBLISH_LIVE_KEY_PATH, " +
			"FREESIDE_PUBLISH_LIVE_INSTALLATION_ID, FREESIDE_PUBLISH_LIVE_REPO")
	}

	appID, err := strconv.ParseInt(os.Getenv("FREESIDE_PUBLISH_LIVE_APP_ID"), 10, 64)
	if err != nil {
		t.Fatalf("FREESIDE_PUBLISH_LIVE_APP_ID: %v", err)
	}
	installationID, err := strconv.ParseInt(os.Getenv("FREESIDE_PUBLISH_LIVE_INSTALLATION_ID"), 10, 64)
	if err != nil {
		t.Fatalf("FREESIDE_PUBLISH_LIVE_INSTALLATION_ID: %v", err)
	}
	repo := os.Getenv("FREESIDE_PUBLISH_LIVE_REPO")
	if repo == "" {
		t.Fatal("FREESIDE_PUBLISH_LIVE_REPO is empty")
	}

	// G304: the key path is supplied by the operator running the
	// opt-in test against their own App.
	keyPEM, err := os.ReadFile(os.Getenv("FREESIDE_PUBLISH_LIVE_KEY_PATH")) //nolint:gosec // operator-supplied path for the opt-in live test
	if err != nil {
		t.Fatalf("read live key: %v", err)
	}
	key, err := parseTestPEM(keyPEM)
	if err != nil {
		t.Fatalf("parse live key: %v", err)
	}

	base := t.TempDir()
	ks, err := publish.NewKeystore(filepath.Join(base, "credentials"), filepath.Join(base, "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ks.SaveApp(publish.AppCredentials{AppID: appID, Key: key}); err != nil {
		t.Fatal(err)
	}
	// The live mint audits through the production store-backed path, so
	// the opt-in run exercises the same recorder the daemon composes.
	rec, err := publish.NewStoreRecorder(newTestStore(t))
	if err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	m := publish.NewMinter(ks, client, "https://api.github.com", rec, time.Now)
	tok, err := m.MintInstallationToken(context.Background(), installationID, repo)
	if err != nil {
		t.Fatalf("live mint: %v", err)
	}
	if tok.Token.Reveal() == "" {
		t.Error("live mint returned an empty token")
	}
	if !tok.ExpiresAt.After(time.Now()) {
		t.Errorf("live token already expired at %v", tok.ExpiresAt)
	}
}
