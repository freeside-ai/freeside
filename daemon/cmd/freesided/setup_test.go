package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/operations"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

func TestSetupCommandCreatesCorrectedFakeDriverDirectory(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root := filepath.Join(t.TempDir(), "freeside")
	if err := runSetup(
		context.Background(),
		[]string{
			"-config-dir", root,
			"-operator", "example",
			"-operator-id", "42",
		},
		&stdout,
		&stderr,
	); err != nil {
		t.Fatalf("runSetup: %v\nstderr: %s", err, stderr.String())
	}
	var layout operations.Layout
	if err := json.Unmarshal(stdout.Bytes(), &layout); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if layout.FakeDriverDir != layout.DBPath+".fake-stage-driver" {
		t.Fatalf("fake driver = %q, want %q", layout.FakeDriverDir, layout.DBPath+".fake-stage-driver")
	}
}

func TestSetupResultGoldens(t *testing.T) {
	layout := operations.Layout{
		ConfigDir:      "/home/operator/.freeside",
		DBPath:         "/home/operator/.freeside/state/freeside.db",
		StateDir:       "/home/operator/.freeside/state",
		CredentialsDir: "/home/operator/.freeside/credentials",
		FakeDriverDir:  "/home/operator/.freeside/state/freeside.db.fake-stage-driver",
		AuthorityPath:  "/home/operator/.freeside/state/installation-authority.json",
	}
	registration := publish.AppRegistration{
		Owner: "operator", OwnerID: 42, Visibility: publish.AppVisibilityPublic,
		AppID: 91, Name: "Freeside Operator", Slug: "freeside-operator",
		ClientID: "Iv1.example",
	}
	cases := []struct {
		name   string
		result setupResult
	}{
		{
			name: "setup-registration-required",
			result: setupResult{
				Layout: layout, Status: "registration_required",
				ManifestAction: "https://github.com/settings/apps/new",
				ManifestFields: map[string][]string{
					"manifest": {`{"name":"freeside-operator"}`},
				},
			},
		},
		{
			name: "setup-complete",
			result: setupResult{
				Layout: layout, Status: "complete", Registration: &registration,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.MarshalIndent(tc.result, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			golden.Assert(t, tc.name, append(body, '\n'))
		})
	}
}

func TestSetupCommandRegistersAppAndInitializesAuthority(t *testing.T) {
	key := setupTestKey(t)
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	root := filepath.Join(t.TempDir(), "freeside")
	authorityPath := filepath.Join(root, "state", "installation-authority.json")
	savedAuthorityPath := authorityPath + ".saved"
	var conversionCalls atomic.Int32
	var interruptAuthority atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost &&
			r.URL.Path == "/app-manifests/manifest-code/conversions":
			if conversionCalls.Add(1) != 1 {
				http.Error(w, "manifest code already consumed", http.StatusGone)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 91, "name": "Freeside Example", "slug": "freeside-example",
				"client_id": "Iv1.example",
				"permissions": map[string]string{
					"actions": "read", "administration": "read", "contents": "write",
					"environments": "read", "pull_requests": "write", "metadata": "read",
				},
				"pem": string(keyPEM),
				"owner": map[string]any{
					"login": "example", "id": 42, "type": "User",
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/apps/freeside-example":
			if interruptAuthority.Swap(false) {
				if err := os.Rename(authorityPath, savedAuthorityPath); err != nil {
					t.Errorf("set aside authority: %v", err)
					http.Error(w, "fixture failure", http.StatusInternalServerError)
					return
				}
				if err := os.Mkdir(authorityPath, 0o700); err != nil {
					t.Errorf("block authority path: %v", err)
					http.Error(w, "fixture failure", http.StatusInternalServerError)
					return
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 91})
		default:
			http.Error(w, fmt.Sprintf("unexpected %s %s", r.Method, r.URL.Path), http.StatusNotFound)
		}
	}))
	defer server.Close()

	args := []string{
		"-config-dir", root,
		"-operator", "example",
		"-operator-id", "42",
	}
	var firstOut, firstErr bytes.Buffer
	if err := runSetupCommand(
		context.Background(), args, &firstOut, &firstErr,
		server.Client(), server.URL, "https://github.example",
	); err != nil {
		t.Fatalf("registration form: %v", err)
	}
	var first setupResult
	if err := json.Unmarshal(firstOut.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if first.Status != "registration_required" ||
		first.ManifestAction != "https://github.example/settings/apps/new" ||
		first.ManifestFields.Get("manifest") == "" {
		t.Fatalf("registration form = %+v", first)
	}

	completionArgs := append(args, "-registration-code", "manifest-code")
	interruptAuthority.Store(true)
	var interruptedOut, interruptedErr bytes.Buffer
	if err := runSetupCommand(
		context.Background(),
		completionArgs,
		&interruptedOut,
		&interruptedErr,
		server.Client(),
		server.URL,
		"https://github.example",
	); err == nil {
		t.Fatal("registration completion survived blocked authority path")
	}
	keystore, err := publish.NewKeystore(first.CredentialsDir, first.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	apps, listErr := keystore.ListAppsIncludingPendingAuthority()
	if listErr != nil || len(apps) != 1 || !apps[0].AuthorityPending {
		t.Fatalf("credentials after interrupted completion = (%d, %v), want one", len(apps), listErr)
	}
	if _, listErr := keystore.ListApps(); !errors.Is(listErr, publish.ErrPendingAppAuthority) {
		t.Fatalf("runtime credential enumeration = %v, want ErrPendingAppAuthority", listErr)
	}
	if err := os.Remove(authorityPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(savedAuthorityPath, authorityPath); err != nil {
		t.Fatal(err)
	}

	var secondOut, secondErr bytes.Buffer
	if err := runSetupCommand(
		context.Background(),
		completionArgs,
		&secondOut,
		&secondErr,
		server.Client(),
		server.URL,
		"https://github.example",
	); err != nil {
		t.Fatalf("registration completion retry: %v", err)
	}
	if conversionCalls.Load() != 1 {
		t.Fatalf("manifest conversion calls = %d, want one", conversionCalls.Load())
	}
	if apps, listErr := keystore.ListApps(); listErr != nil ||
		len(apps) != 1 || apps[0].AuthorityPending {
		t.Fatalf("credentials after recovery = (%+v, %v), want finalized", apps, listErr)
	}
	var second setupResult
	if err := json.Unmarshal(secondOut.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if second.Status != "complete" ||
		second.Registration == nil ||
		second.Registration.AppID != 91 ||
		second.Registration.Owner != "example" {
		t.Fatalf("registration result = %+v", second)
	}
	if info, err := os.Stat(second.AuthorityPath); err != nil ||
		info.Mode().Perm() != 0o600 {
		t.Fatalf("authority file info = %v, err = %v", info, err)
	}
	authority, err := publish.NewInstallationAuthorityStore(second.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	document, err := authority.Document(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(document.Registrations) != 1 ||
		document.Registrations[0].RegistrationID != 91 ||
		len(document.Registrations[0].TrustedOwners) != 1 ||
		document.Registrations[0].TrustedOwners[0].Login != "example" ||
		document.Registrations[0].TrustedInstallations == nil {
		t.Fatalf("authority document = %+v", document)
	}
	installationURL, err := installationURLForRegistration(
		second.CredentialsDir, second.StateDir, second.Registration.AppID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if installationURL != "https://github.com/apps/freeside-example/installations/new" {
		t.Fatalf("installation URL = %q", installationURL)
	}
}

func TestSetupCommandDoesNotCreateAuthorityForExistingCredentials(t *testing.T) {
	root := filepath.Join(t.TempDir(), "freeside")
	layout, err := operations.Setup(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	keystore, err := publish.NewKeystore(layout.CredentialsDir, layout.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := publish.NewInstallationAuthorityStore(layout.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := keystore.SaveApp(publish.AppCredentials{
		Owner: "example", OwnerID: 42, Visibility: publish.AppVisibilityPublic,
		AppID: 91, Name: "Freeside Example", Slug: "freeside-example",
		ClientID: "Iv1.example", Key: setupTestKey(t),
	}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err = runSetupCommand(
		context.Background(),
		[]string{
			"-config-dir", root,
			"-operator", "example",
			"-operator-id", "42",
		},
		&stdout,
		&stderr,
		&http.Client{},
		"https://api.github.example",
		"https://github.example",
	)
	if err == nil {
		t.Fatal("setup accepted existing credentials without explicit authority")
	}
	if _, statErr := os.Stat(layout.AuthorityPath); !os.IsNotExist(statErr) {
		t.Fatalf("setup created authority before refusing existing credentials: %v", statErr)
	}

	if err := authority.InitializeDocument(context.Background()); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	err = runSetupCommand(
		context.Background(),
		[]string{
			"-config-dir", root,
			"-operator", "example",
			"-operator-id", "42",
			"-registration-code", "unrelated-code",
		},
		&stdout,
		&stderr,
		&http.Client{},
		"https://api.github.example",
		"https://github.example",
	)
	if err == nil {
		t.Fatal("setup authorized unmarked credentials with an unrelated registration code")
	}
	document, documentErr := authority.Document(context.Background())
	if documentErr != nil {
		t.Fatal(documentErr)
	}
	if len(document.Registrations) != 0 {
		t.Fatalf("setup authorized unmarked existing credentials: %+v", document)
	}
}

func TestSetupCommandSelectsOperatorRegistrationFromMultipleApps(t *testing.T) {
	root := filepath.Join(t.TempDir(), "freeside")
	layout, err := operations.Setup(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	keystore, err := publish.NewKeystore(layout.CredentialsDir, layout.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := publish.NewInstallationAuthorityStore(layout.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.InitializeDocument(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, credentials := range []publish.AppCredentials{
		{
			Owner: "other", OwnerID: 41, Visibility: publish.AppVisibilityPublic,
			AppID: 90, Name: "Freeside Other", Slug: "freeside-other",
			ClientID: "Iv1.other", Key: setupTestKey(t),
		},
		{
			Owner: "example", OwnerID: 42, Visibility: publish.AppVisibilityPublic,
			AppID: 91, Name: "Freeside Example", Slug: "freeside-example",
			ClientID: "Iv1.example", Key: setupTestKey(t),
		},
	} {
		if err := keystore.SaveApp(credentials); err != nil {
			t.Fatal(err)
		}
		if err := authority.InitializeRegistration(context.Background(), credentials.Registration()); err != nil {
			t.Fatal(err)
		}
	}
	var stdout, stderr bytes.Buffer
	if err := runSetupCommand(
		context.Background(),
		[]string{
			"-config-dir", root,
			"-operator", "example",
			"-operator-id", "42",
		},
		&stdout,
		&stderr,
		&http.Client{},
		"https://api.github.example",
		"https://github.example",
	); err != nil {
		t.Fatalf("setup replay: %v", err)
	}
	var result setupResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "complete" || result.Registration == nil ||
		result.Registration.AppID != 91 {
		t.Fatalf("setup selected registration = %+v, want App 91", result.Registration)
	}
}

func TestSetupCommandNeverOverwritesMalformedAuthority(t *testing.T) {
	root := filepath.Join(t.TempDir(), "freeside")
	args := []string{
		"-config-dir", root,
		"-operator", "example",
		"-operator-id", "42",
	}
	var firstOut, firstErr bytes.Buffer
	if err := runSetupCommand(
		context.Background(), args, &firstOut, &firstErr,
		&http.Client{}, "https://api.github.example", "https://github.example",
	); err != nil {
		t.Fatal(err)
	}
	var first setupResult
	if err := json.Unmarshal(firstOut.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	const malformed = "{not-json"
	if err := os.WriteFile(first.AuthorityPath, []byte(malformed), 0o600); err != nil {
		t.Fatal(err)
	}
	var replayOut, replayErr bytes.Buffer
	if err := runSetupCommand(
		context.Background(), args, &replayOut, &replayErr,
		&http.Client{}, "https://api.github.example", "https://github.example",
	); err == nil {
		t.Fatal("setup accepted malformed existing authority")
	}
	payload, err := os.ReadFile(first.AuthorityPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != malformed {
		t.Fatalf("setup replaced malformed authority with %q", payload)
	}
}

func setupTestKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}
