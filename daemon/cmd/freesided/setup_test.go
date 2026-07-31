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
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
		strings.NewReader(""),
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
	const manifestCode = "FREESIDE_TEST_MANIFEST_CODE"
	var conversionCalls atomic.Int32
	var interruptAuthority atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost &&
			r.URL.Path == "/app-manifests/"+manifestCode+"/conversions":
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
		context.Background(), args, strings.NewReader(""), &firstOut, &firstErr,
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

	completionArgs := append(args, "-registration-code-stdin")
	interruptAuthority.Store(true)
	var interruptedOut, interruptedErr bytes.Buffer
	interruptedRunErr := runSetupCommand(
		context.Background(),
		completionArgs,
		strings.NewReader(manifestCode+"\n"),
		&interruptedOut,
		&interruptedErr,
		server.Client(),
		server.URL,
		"https://github.example",
	)
	if interruptedRunErr == nil {
		t.Fatal("registration completion survived blocked authority path")
	}
	assertSecretAbsent(t, manifestCode, interruptedOut.String(), interruptedErr.String(), interruptedRunErr.Error())
	assertSecretAbsentFromRegularFiles(t, root, manifestCode)
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
		args,
		strings.NewReader(""),
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
	assertSecretAbsentFromRegularFiles(t, root, manifestCode)
	assertSecretAbsent(t, manifestCode, secondOut.String(), secondErr.String())
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
		strings.NewReader(""),
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
			"-registration-code-stdin",
		},
		strings.NewReader("FREESIDE_UNRELATED_MANIFEST_CODE\n"),
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
		strings.NewReader(""),
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
		context.Background(), args, strings.NewReader(""), &firstOut, &firstErr,
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
		context.Background(), args, strings.NewReader(""), &replayOut, &replayErr,
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

func TestSetupCommandKeepsRegistrationCodeOutOfProcessArgumentsAndOutput(t *testing.T) {
	if os.Getenv("FREESIDE_SETUP_PROCESS_HELPER") == "1" {
		err := runSetupCommand(
			context.Background(),
			setupProcessHelperArgs(t),
			os.Stdin,
			os.Stdout,
			os.Stderr,
			&http.Client{},
			os.Getenv("FREESIDE_SETUP_PROCESS_API"),
			"https://github.example",
		)
		if err != nil {
			t.Fatal(err)
		}
		return
	}

	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(setupTestKey(t)),
	})
	const manifestCode = "FREESIDE_PROCESS_LIST_MANIFEST_CODE"
	var conversionCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost &&
			r.URL.Path == "/app-manifests/"+manifestCode+"/conversions":
			conversionCalls.Add(1)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 91, "name": "Freeside Example", "slug": "freeside-example",
				"client_id": "Iv1.example",
				"permissions": map[string]string{
					"actions": "read", "administration": "read", "contents": "write",
					"environments": "read", "pull_requests": "write", "metadata": "read",
				},
				"pem":   string(keyPEM),
				"owner": map[string]any{"login": "example", "id": 42, "type": "User"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/apps/freeside-example":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 91})
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	configDir := filepath.Join(t.TempDir(), "freeside")
	cmd := exec.Command( //nolint:gosec // G204: the test reexecutes its own binary with fixed setup flags and a test temp path.
		os.Args[0],
		"-test.run=^TestSetupCommandKeepsRegistrationCodeOutOfProcessArgumentsAndOutput$",
		"--",
		"setup",
		"-config-dir", configDir,
		"-operator", "example",
		"-operator-id", "42",
		"-registration-code-stdin",
	)
	cmd.Env = append(os.Environ(),
		"FREESIDE_SETUP_PROCESS_HELPER=1",
		"FREESIDE_SETUP_PROCESS_API="+server.URL,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(stdin, manifestCode+"\n"); err != nil {
		t.Fatal(err)
	}
	commandLine, psErr := exec.Command( //nolint:gosec // G204: ps is fixed and the PID comes from the child process started above.
		"ps", "-ww", "-o", "command=", "-p", fmt.Sprint(cmd.Process.Pid),
	).CombinedOutput()
	if psErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("inspect setup process arguments: %v: %s", psErr, commandLine)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("setup process: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	if conversionCalls.Load() != 1 {
		t.Fatalf("manifest conversion calls = %d, want one", conversionCalls.Load())
	}
	assertSecretAbsent(t, manifestCode, string(commandLine), stdout.String(), stderr.String())
	assertSecretAbsentFromRegularFiles(t, configDir, manifestCode)
}

func TestSetupCommandRejectsArgvCodesWithoutRenderingThem(t *testing.T) {
	const manifestCode = "FREESIDE_LEGACY_ARGV_MANIFEST_CODE"
	tests := []struct {
		name string
		args []string
	}{
		{name: "separate-legacy-flag", args: []string{"-registration-code", manifestCode}},
		{name: "joined-legacy-flag", args: []string{"-registration-code=" + manifestCode}},
		{name: "joined-stdin-flag", args: []string{"-registration-code-stdin=" + manifestCode}},
		{name: "joined-double-dash-stdin-flag", args: []string{"--registration-code-stdin=" + manifestCode}},
		{name: "positional", args: []string{
			"-operator", "example", "-operator-id", "42", manifestCode,
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := runSetupCommand(
				context.Background(),
				tt.args,
				strings.NewReader(""),
				&stdout,
				&stderr,
				&http.Client{},
				"https://api.github.example",
				"https://github.example",
			)
			if err == nil {
				t.Fatal("setup accepted a code from process arguments")
			}
			assertSecretAbsent(t, manifestCode, stdout.String(), stderr.String(), err.Error())
		})
	}
}

func TestReadRegistrationCodeValidatesBoundedSingleLine(t *testing.T) {
	tests := []struct {
		name    string
		stdin   io.Reader
		want    string
		wantErr bool
	}{
		{name: "no-newline", stdin: strings.NewReader("code"), want: "code"},
		{name: "line-feed", stdin: strings.NewReader("code\n"), want: "code"},
		{name: "crlf", stdin: strings.NewReader("code\r\n"), want: "code"},
		{name: "empty", stdin: strings.NewReader(""), wantErr: true},
		{name: "multiple-lines", stdin: strings.NewReader("code\nother"), wantErr: true},
		{name: "oversized", stdin: strings.NewReader(strings.Repeat("x", maxRegistrationCodeBytes+1)), wantErr: true},
		{name: "unavailable", stdin: nil, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readRegistrationCode(tt.stdin)
			if (err != nil) != tt.wantErr {
				t.Fatalf("readRegistrationCode error = %v, wantErr %t", err, tt.wantErr)
			}
			if got != publish.Secret(tt.want) {
				t.Fatalf("readRegistrationCode = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReadRegistrationCodeRedactsReaderErrors(t *testing.T) {
	const manifestCode = "FREESIDE_READER_ERROR_MANIFEST_CODE"
	_, err := readRegistrationCode(errorReader{err: errors.New(manifestCode)})
	if err == nil {
		t.Fatal("readRegistrationCode accepted a failing reader")
	}
	assertSecretAbsent(t, manifestCode, err.Error())
}

type errorReader struct {
	err error
}

func (r errorReader) Read([]byte) (int, error) {
	return 0, r.err
}

func setupProcessHelperArgs(t *testing.T) []string {
	t.Helper()
	for i, arg := range os.Args {
		if arg != "--" {
			continue
		}
		if i+1 >= len(os.Args) || os.Args[i+1] != "setup" {
			t.Fatal("setup process helper received malformed arguments")
		}
		return os.Args[i+2:]
	}
	t.Fatal("setup process helper received no command arguments")
	return nil
}

func assertSecretAbsent(t *testing.T, secret string, values ...string) {
	t.Helper()
	for _, value := range values {
		if strings.Contains(value, secret) {
			t.Fatalf("secret appeared in output: %q", value)
		}
	}
}

func assertSecretAbsentFromRegularFiles(t *testing.T, root, secret string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type().IsRegular() {
			payload, err := os.ReadFile(path) //nolint:gosec // G304: WalkDir constrains paths to the test-owned config root.
			if err != nil {
				return err
			}
			if bytes.Contains(payload, []byte(secret)) {
				return fmt.Errorf("secret persisted in %s", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
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
