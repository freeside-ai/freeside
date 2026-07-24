package publish_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

const fixtureRepositoryOwnerID = int64(98765)

type credentialForge struct {
	t *testing.T

	mu           sync.Mutex
	installed    bool
	public       bool
	appID        int64
	trailing     bool
	installedFor int64
	authenticate bool
}

func newCredentialForge(t *testing.T) *credentialForge {
	t.Helper()
	return &credentialForge{
		t: t, public: true, appID: fixtureAppID, installedFor: fixtureAppID,
	}
}

func (f *credentialForge) setInstalled(installed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installed = installed
}

func (f *credentialForge) setPublic(public bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.public = public
}

func (f *credentialForge) setAuthenticate(authenticate bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authenticate = authenticate
}

func (f *credentialForge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	installed, public, appID, trailing, installedFor, authenticate := f.installed, f.public, f.appID, f.trailing, f.installedFor, f.authenticate
	f.mu.Unlock()

	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/app-manifests/CODE123/conversions":
		raw, err := os.ReadFile(filepath.Join("testdata", "conversion-response.json"))
		if err != nil {
			f.t.Errorf("read conversion fixture: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var response map[string]any
		if err := json.Unmarshal(raw, &response); err != nil {
			f.t.Errorf("decode conversion fixture: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		response["owner"].(map[string]any)["type"] = "User"
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(response)
	case r.Method == http.MethodGet && r.URL.Path == "/app":
		if r.Header.Get("Authorization") == "" {
			f.t.Error("authenticated App request has no authorization")
		}
		if authenticate && requestJWTIssuer(f.t, r) != appID {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": appID, "name": "freeside-publish", "slug": "freeside-publish",
			"client_id": "Iv1.deadbeefdeadbeef",
			"owner":     map[string]any{"login": "freeside-ai", "id": testOwnerID},
		})
		if trailing {
			_, _ = io.WriteString(w, "garbage")
		}
	case r.Method == http.MethodGet && r.URL.Path == "/apps/freeside-publish":
		if !public {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": appID})
	case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
		if r.URL.Query().Get("per_page") != "100" || r.URL.Query().Get("page") != "1" {
			f.t.Errorf("installation query = %q", r.URL.RawQuery)
		}
		if !installed || requestJWTIssuer(f.t, r) != installedFor {
			_, _ = io.WriteString(w, "[]")
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"id": 777, "app_id": installedFor,
			"target_id":            fixtureRepositoryOwnerID,
			"repository_selection": "selected",
			"account": map[string]any{
				"login": "repo-org", "id": fixtureRepositoryOwnerID,
			},
		}})
	default:
		f.t.Errorf("unexpected forge request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func requestJWTIssuer(t *testing.T, r *http.Request) int64 {
	t.Helper()
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Errorf("authorization is not a JWT")
		return 0
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Errorf("decode JWT claims: %v", err)
		return 0
	}
	var claims struct {
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Errorf("decode JWT claims JSON: %v", err)
		return 0
	}
	issuer, err := strconv.ParseInt(claims.Iss, 10, 64)
	if err != nil {
		t.Errorf("parse JWT issuer: %v", err)
		return 0
	}
	return issuer
}

func publicFixtureRegistration() publish.AppRegistration {
	return publish.AppRegistration{
		Owner: "freeside-ai", OwnerID: testOwnerID,
		Visibility: publish.AppVisibilityPublic,
		AppID:      fixtureAppID, Name: "freeside-publish", Slug: "freeside-publish",
		ClientID: "Iv1.deadbeefdeadbeef",
	}
}

func publicFixtureCredentials(t *testing.T) publish.AppCredentials {
	t.Helper()
	registration := publicFixtureRegistration()
	return publish.AppCredentials{
		Owner: registration.Owner, OwnerID: registration.OwnerID,
		Visibility: registration.Visibility, AppID: registration.AppID,
		Name: registration.Name, Slug: registration.Slug, ClientID: registration.ClientID,
		Key: fixtureKey(t),
	}
}

func newTestOnboarder(
	t *testing.T,
	ks *publish.Keystore,
	janitor publish.JanitorStatus,
) (*publish.CredentialOnboarder, *credentialForge, *httptest.Server) {
	t.Helper()
	forge := newCredentialForge(t)
	srv := httptest.NewServer(forge)
	t.Cleanup(srv.Close)
	return publish.NewCredentialOnboarder(
		ks,
		srv.Client(),
		srv.URL,
		"https://github.example",
		fixedNow,
		janitor,
	), forge, srv
}

func TestCredentialOnboardingRegistrationAndInstallationFlow(t *testing.T) {
	ks := newTestKeystore(t)
	onboarder, forge, _ := newTestOnboarder(t, ks, activeJanitorStatus{})
	request := publish.CredentialRequest{
		Operator: "freeside-ai", OperatorID: testOwnerID,
		RepositoryOwner: "repo-org",
		Manifest:        publish.Manifest{URL: "https://freeside.example"},
	}

	step, err := onboarder.Next(context.Background(), request)
	if err != nil {
		t.Fatalf("Next without registration: %v", err)
	}
	if step.Kind != publish.CredentialStepRegister ||
		step.ManifestAction != "https://github.example/settings/apps/new" {
		t.Fatalf("registration step = %+v", step)
	}
	var manifest publish.Manifest
	if err := json.Unmarshal([]byte(step.ManifestFields.Get("manifest")), &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if !manifest.Public || manifest.Name != "freeside-freeside-ai" {
		t.Errorf("public manifest = %+v", manifest)
	}

	creds, err := onboarder.CompleteRegistration(
		context.Background(),
		"CODE123",
		request.Operator,
		request.OperatorID,
	)
	if err != nil {
		t.Fatalf("CompleteRegistration: %v", err)
	}
	if creds.Visibility != publish.AppVisibilityPublic || creds.KeyID == "" {
		t.Errorf("registered credentials = %+v", creds)
	}

	step, err = onboarder.Next(context.Background(), request)
	if err != nil {
		t.Fatalf("Next without installation: %v", err)
	}
	if step.Kind != publish.CredentialStepInstall ||
		step.URL != "https://github.example/apps/freeside-publish/installations/new" ||
		!step.OrganizationApprovalMayBeNeeded {
		t.Fatalf("installation step = %+v", step)
	}

	go func() {
		time.Sleep(20 * time.Millisecond)
		forge.setInstalled(true)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	binding, err := onboarder.WaitForInstallation(
		ctx,
		fixtureAppID,
		"repo-org",
		5*time.Millisecond,
	)
	if err != nil {
		t.Fatalf("WaitForInstallation: %v", err)
	}
	if binding.RegistrationID != fixtureAppID ||
		binding.InstallationID != 777 ||
		binding.AccountID != fixtureRepositoryOwnerID {
		t.Errorf("binding = %+v", binding)
	}
}

func TestCredentialOnboardingImportsPerMachineKey(t *testing.T) {
	ks := newTestKeystore(t)
	onboarder, _, _ := newTestOnboarder(t, ks, activeJanitorStatus{})
	registration := publicFixtureRegistration()
	request := publish.CredentialRequest{
		Operator: "freeside-ai", OperatorID: testOwnerID,
		RepositoryOwner: "repo-org", Registration: &registration,
	}

	step, err := onboarder.Next(context.Background(), request)
	if err != nil {
		t.Fatalf("Next without machine key: %v", err)
	}
	if step.Kind != publish.CredentialStepKey ||
		step.URL != "https://github.example/settings/apps/freeside-publish" {
		t.Fatalf("machine-key step = %+v", step)
	}

	raw, err := os.ReadFile(filepath.Join("testdata", "test-signing-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	creds, err := onboarder.ImportKey(context.Background(), registration, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ImportKey: %v", err)
	}
	if creds.KeyID == "" {
		t.Fatal("ImportKey recorded no key fingerprint")
	}
	stored, err := ks.LoadApp(testOwnerID)
	if err != nil {
		t.Fatalf("LoadApp: %v", err)
	}
	if stored.KeyID != creds.KeyID || stored.AppID != registration.AppID {
		t.Errorf("stored credentials = %+v, imported = %+v", stored, creds)
	}
}

func TestCredentialOnboardingScopesInstallationToSelectedRegistration(t *testing.T) {
	ks := newTestKeystore(t)
	if err := ks.SaveApp(publicFixtureCredentials(t)); err != nil {
		t.Fatal(err)
	}
	other := publish.AppCredentials{
		Owner: "other-operator", OwnerID: testOwnerID + 1,
		Visibility: publish.AppVisibilityPublic,
		AppID:      fixtureAppID + 1, Name: "other-app", Slug: "other-app",
		ClientID: "Iv1.other", Key: fixtureKey(t),
	}
	if err := ks.SaveApp(other); err != nil {
		t.Fatal(err)
	}
	onboarder, forge, _ := newTestOnboarder(t, ks, activeJanitorStatus{})
	forge.installedFor = other.AppID
	forge.setInstalled(true)
	registration := publicFixtureRegistration()
	step, err := onboarder.Next(context.Background(), publish.CredentialRequest{
		Operator: "freeside-ai", OperatorID: testOwnerID,
		RepositoryOwner: "repo-org", Registration: &registration,
	})
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if step.Kind != publish.CredentialStepInstall {
		t.Fatalf("step kind = %q, want selected registration's install prerequisite", step.Kind)
	}
}

func TestCredentialOnboardingKeyRotationPreservesManifestSecrets(t *testing.T) {
	ks := newTestKeystore(t)
	existing := publicFixtureCredentials(t)
	existing.WebhookSecret = publish.Secret("webhook-existing")
	existing.ClientSecret = publish.Secret("client-existing")
	if err := ks.SaveApp(existing); err != nil {
		t.Fatal(err)
	}
	onboarder, _, _ := newTestOnboarder(t, ks, activeJanitorStatus{})
	raw, err := os.ReadFile(filepath.Join("testdata", "test-signing-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := onboarder.ImportKey(
		context.Background(),
		publicFixtureRegistration(),
		bytes.NewReader(raw),
	); err != nil {
		t.Fatalf("ImportKey: %v", err)
	}
	stored, err := ks.LoadApp(testOwnerID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.WebhookSecret.Reveal() != "webhook-existing" ||
		stored.ClientSecret.Reveal() != "client-existing" {
		t.Error("machine-key rotation erased one-time manifest secrets")
	}
}

func TestCredentialOnboardingRejectsUnsafeSlug(t *testing.T) {
	ks := newTestKeystore(t)
	onboarder, _, _ := newTestOnboarder(t, ks, activeJanitorStatus{})
	registration := publicFixtureRegistration()
	registration.Slug = ".."
	if _, err := onboarder.KeySettingsURL(registration); err == nil {
		t.Fatal("KeySettingsURL accepted a path-relative App slug")
	}
}

func TestCredentialOnboardingRejectsUntrustedKeyOrAppMetadata(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*credentialForge)
		pemPrefix string
		pemSuffix string
		want      error
	}{
		{
			name: "wrong canonical App",
			mutate: func(f *credentialForge) {
				f.appID = fixtureAppID + 1
			},
			want: publish.ErrAppRegistrationMismatch,
		},
		{
			name: "visibility mismatch",
			mutate: func(f *credentialForge) {
				f.setPublic(false)
			},
			want: publish.ErrAppVisibilityMismatch,
		},
		{
			name:      "leading PEM content",
			mutate:    func(*credentialForge) {},
			pemPrefix: "not-the-key\n",
		},
		{
			name:      "trailing PEM content",
			mutate:    func(*credentialForge) {},
			pemSuffix: "\nnot-a-second-key",
		},
		{
			name: "trailing App response",
			mutate: func(f *credentialForge) {
				f.trailing = true
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ks := newTestKeystore(t)
			onboarder, forge, _ := newTestOnboarder(t, ks, activeJanitorStatus{})
			tt.mutate(forge)
			raw, err := os.ReadFile(filepath.Join("testdata", "test-signing-key.pem"))
			if err != nil {
				t.Fatal(err)
			}
			raw = append([]byte(tt.pemPrefix), raw...)
			raw = append(raw, tt.pemSuffix...)
			_, err = onboarder.ImportKey(
				context.Background(),
				publicFixtureRegistration(),
				bytes.NewReader(raw),
			)
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("ImportKey error = %v, want %v", err, tt.want)
			}
			if tt.want == nil && err == nil {
				t.Fatal("ImportKey accepted malformed PEM")
			}
			if _, loadErr := ks.LoadApp(testOwnerID); !errors.Is(loadErr, publish.ErrNoAppRegistration) {
				t.Fatalf("rejected import changed keystore: %v", loadErr)
			}
		})
	}
}

type inactiveJanitorStatus struct{}

func (inactiveJanitorStatus) ActiveFor(int64) bool { return false }
func (inactiveJanitorStatus) AllowsRepository(int64, int64, int64) bool {
	return false
}

type mutableJanitorStatus struct {
	mu     sync.Mutex
	active bool
}

func (s *mutableJanitorStatus) ActiveFor(int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

func (*mutableJanitorStatus) AllowsRepository(int64, int64, int64) bool {
	return true
}

func (s *mutableJanitorStatus) setActive(active bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active = active
}

func TestCredentialOnboardingWaitsThroughJanitorPass(t *testing.T) {
	ks := newTestKeystore(t)
	if err := ks.SaveApp(publicFixtureCredentials(t)); err != nil {
		t.Fatal(err)
	}
	janitor := &mutableJanitorStatus{}
	onboarder, forge, _ := newTestOnboarder(t, ks, janitor)
	forge.setInstalled(true)
	go func() {
		time.Sleep(20 * time.Millisecond)
		janitor.setActive(true)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := onboarder.WaitForInstallation(
		ctx,
		fixtureAppID,
		"repo-org",
		5*time.Millisecond,
	); err != nil {
		t.Fatalf("WaitForInstallation during janitor pass: %v", err)
	}
}

func TestCredentialOnboardingNormalizesTrailingSlashBaseURLs(t *testing.T) {
	ks := newTestKeystore(t)
	forge := newCredentialForge(t)
	srv := httptest.NewServer(forge)
	t.Cleanup(srv.Close)
	// Trailing slashes on both bases must not survive into emitted URLs: the
	// registrar and the onboarder's own builders concatenate leading-slash
	// paths, so a raw trailing slash would produce a doubled separator.
	onboarder := publish.NewCredentialOnboarder(
		ks, srv.Client(), srv.URL+"/", "https://github.example/", fixedNow, activeJanitorStatus{},
	)
	step, err := onboarder.Next(context.Background(), publish.CredentialRequest{
		Operator: "freeside-ai", OperatorID: testOwnerID,
		RepositoryOwner: "repo-org",
		Manifest:        publish.Manifest{URL: "https://freeside.example"},
	})
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if step.ManifestAction != "https://github.example/settings/apps/new" {
		t.Fatalf("manifest action = %q, want no doubled slash", step.ManifestAction)
	}
	registration := publicFixtureRegistration()
	keyURL, err := onboarder.KeySettingsURL(registration)
	if err != nil {
		t.Fatalf("KeySettingsURL: %v", err)
	}
	if keyURL != "https://github.example/settings/apps/freeside-publish" {
		t.Fatalf("key settings URL = %q, want no doubled slash", keyURL)
	}
	installURL, err := onboarder.InstallationURL(registration)
	if err != nil {
		t.Fatalf("InstallationURL: %v", err)
	}
	if installURL != "https://github.example/apps/freeside-publish/installations/new" {
		t.Fatalf("installation URL = %q, want no doubled slash", installURL)
	}
}

func TestCredentialOnboardingNextTreatsInactiveJanitorAsNotReady(t *testing.T) {
	ks := newTestKeystore(t)
	if err := ks.SaveApp(publicFixtureCredentials(t)); err != nil {
		t.Fatal(err)
	}
	onboarder, forge, _ := newTestOnboarder(t, ks, inactiveJanitorStatus{})
	// Even with an installation present on the forge, an inactive janitor must
	// route to the install/resume step rather than a fatal error: coverage has
	// not yet confirmed the binding.
	forge.setInstalled(true)
	registration := publicFixtureRegistration()
	step, err := onboarder.Next(context.Background(), publish.CredentialRequest{
		Operator: "freeside-ai", OperatorID: testOwnerID,
		RepositoryOwner: "repo-org", Registration: &registration,
	})
	if err != nil {
		t.Fatalf("Next with inactive janitor: %v", err)
	}
	if step.Kind != publish.CredentialStepInstall {
		t.Fatalf("step kind = %q, want install prerequisite while janitor is inactive", step.Kind)
	}
}

func TestCredentialDoctorDetections(t *testing.T) {
	t.Run("missing key", func(t *testing.T) {
		ks := newTestKeystore(t)
		onboarder, _, _ := newTestOnboarder(t, ks, activeJanitorStatus{})
		findings, err := publish.NewCredentialDoctor(onboarder, activeJanitorStatus{}).
			Check(context.Background(), []publish.AppRegistration{publicFixtureRegistration()})
		if err != nil {
			t.Fatal(err)
		}
		assertFinding(t, findings, publish.CredentialFindingMissingKey)
	})

	t.Run("bad modes", func(t *testing.T) {
		ks := newTestKeystore(t)
		if err := ks.SaveApp(publicFixtureCredentials(t)); err != nil {
			t.Fatal(err)
		}
		keyPath := filepath.Join(
			ks.Dir(), "github-app", strconv.FormatInt(testOwnerID, 10), "app.pem",
		)
		if err := os.Chmod(keyPath, 0o644); err != nil { //nolint:gosec // deliberately exposed doctor fixture
			t.Fatal(err)
		}
		onboarder, _, _ := newTestOnboarder(t, ks, activeJanitorStatus{})
		findings, err := publish.NewCredentialDoctor(onboarder, activeJanitorStatus{}).
			Check(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		assertFinding(t, findings, publish.CredentialFindingBadPermissions)
	})

	t.Run("visibility mismatch", func(t *testing.T) {
		ks := newTestKeystore(t)
		if err := ks.SaveApp(publicFixtureCredentials(t)); err != nil {
			t.Fatal(err)
		}
		onboarder, forge, _ := newTestOnboarder(t, ks, activeJanitorStatus{})
		forge.setPublic(false)
		findings, err := publish.NewCredentialDoctor(onboarder, activeJanitorStatus{}).
			Check(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		assertFinding(t, findings, publish.CredentialFindingVisibilityMismatch)
	})

	t.Run("janitor inactive", func(t *testing.T) {
		ks := newTestKeystore(t)
		if err := ks.SaveApp(publicFixtureCredentials(t)); err != nil {
			t.Fatal(err)
		}
		onboarder, _, _ := newTestOnboarder(t, ks, inactiveJanitorStatus{})
		findings, err := publish.NewCredentialDoctor(onboarder, inactiveJanitorStatus{}).
			Check(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		assertFinding(t, findings, publish.CredentialFindingJanitorInactive)
	})

	t.Run("private janitor inactive", func(t *testing.T) {
		ks := newTestKeystore(t)
		credentials := publicFixtureCredentials(t)
		credentials.Visibility = publish.AppVisibilityPrivate
		if err := ks.SaveApp(credentials); err != nil {
			t.Fatal(err)
		}
		onboarder, forge, _ := newTestOnboarder(t, ks, inactiveJanitorStatus{})
		forge.setPublic(false)
		findings, err := publish.NewCredentialDoctor(onboarder, inactiveJanitorStatus{}).
			Check(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		assertFinding(t, findings, publish.CredentialFindingJanitorInactive)
	})

	t.Run("legacy singleton", func(t *testing.T) {
		ks := newTestKeystore(t)
		legacyDir := filepath.Join(ks.Dir(), "github-app")
		if err := os.MkdirAll(legacyDir, 0o700); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join("testdata", "test-signing-key.pem"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(legacyDir, "app.pem"), raw, 0o600); err != nil { //nolint:gosec // fixed test-only keystore path
			t.Fatal(err)
		}
		metadata := fmt.Sprintf(
			`{"app_id":%d,"slug":"freeside-publish","client_id":"Iv1.test","webhook_secret":"x","client_secret":"y"}`,
			fixtureAppID,
		)
		if err := os.WriteFile(filepath.Join(legacyDir, "app.json"), []byte(metadata), 0o600); err != nil {
			t.Fatal(err)
		}
		onboarder, _, _ := newTestOnboarder(t, ks, activeJanitorStatus{})
		findings, err := publish.NewCredentialDoctor(onboarder, activeJanitorStatus{}).
			Check(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		assertFinding(t, findings, publish.CredentialFindingLegacyLayout)
	})

	t.Run("reused machine key", func(t *testing.T) {
		ks := newTestKeystore(t)
		if err := ks.SaveApp(publicFixtureCredentials(t)); err != nil {
			t.Fatal(err)
		}
		second := publish.AppCredentials{
			Owner: "second-owner", OwnerID: testOwnerID + 1,
			Visibility: publish.AppVisibilityPublic,
			AppID:      fixtureAppID + 1, Name: "second-app", Slug: "second-app",
			ClientID: "Iv1.second", Key: fixtureKey(t),
		}
		if err := ks.SaveApp(second); err != nil {
			t.Fatal(err)
		}
		onboarder, forge, _ := newTestOnboarder(t, ks, activeJanitorStatus{})
		forge.setAuthenticate(true)
		findings, err := publish.NewCredentialDoctor(onboarder, activeJanitorStatus{}).
			Check(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		assertFinding(t, findings, publish.CredentialFindingReusedMachineKey)
	})
}

func assertFinding(t *testing.T, findings []publish.CredentialFinding, want publish.CredentialFindingCode) {
	t.Helper()
	for _, finding := range findings {
		if finding.Code == want {
			return
		}
	}
	t.Fatalf("findings = %+v, want code %q", findings, want)
}
