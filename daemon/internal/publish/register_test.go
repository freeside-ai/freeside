package publish_test

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

func testManifest() publish.Manifest {
	return publish.Manifest{
		URL: "https://github.com/freeside-ai/freeside",
	}
}

func testRegistrationTarget() publish.RegistrationTarget {
	return publish.RegistrationTarget{
		Owner:        "freeside-ai",
		OwnerID:      testOwnerID,
		Organization: true,
		Visibility:   publish.AppVisibilityPrivate,
	}
}

// conversionServer serves the recorded manifest-conversion fixture and
// captures the request path.
func conversionServer(t *testing.T, gotPath *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/apps/freeside-publish" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/app" {
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
				t.Error("authenticated App probe has no bearer token")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": fixtureAppID})
			return
		}
		*gotPath = r.Method + " " + r.URL.Path
		w.WriteHeader(http.StatusCreated)
		fixture, err := os.ReadFile(filepath.Join("testdata", "conversion-response.json"))
		if err != nil {
			t.Errorf("read fixture: %v", err)
		}
		_, _ = w.Write(fixture)
	}))
}

// TestExchangeCodeVerifiesVisibility proves a relayed code cannot substitute a
// public App for the private work-account posture or vice versa. A public
// observation is also bound back to the converted numeric App ID.
func TestExchangeCodeVerifiesVisibility(t *testing.T) {
	tests := []struct {
		name                string
		target              publish.RegistrationTarget
		ownerType           string
		publicStatus        int
		publicAppID         int64
		authenticatedStatus int
		authenticatedAppID  int64
		wantErr             bool
		wantVisibility      publish.AppVisibility
	}{
		{
			name:                "private organization hidden",
			target:              testRegistrationTarget(),
			ownerType:           "Organization",
			publicStatus:        http.StatusNotFound,
			authenticatedStatus: http.StatusOK,
			authenticatedAppID:  fixtureAppID,
			wantVisibility:      publish.AppVisibilityPrivate,
		},
		{
			name:         "public organization rejected",
			target:       testRegistrationTarget(),
			ownerType:    "Organization",
			publicStatus: http.StatusOK,
			publicAppID:  fixtureAppID,
			wantErr:      true,
		},
		{
			name: "public personal visible",
			target: publish.RegistrationTarget{
				Owner:      "freeside-ai",
				OwnerID:    testOwnerID,
				Visibility: publish.AppVisibilityPublic,
			},
			ownerType:      "User",
			publicStatus:   http.StatusOK,
			publicAppID:    fixtureAppID,
			wantVisibility: publish.AppVisibilityPublic,
		},
		{
			name: "private personal visible",
			target: publish.RegistrationTarget{
				Owner:      "freeside-ai",
				OwnerID:    testOwnerID,
				Visibility: publish.AppVisibilityPrivate,
			},
			ownerType:    "User",
			publicStatus: http.StatusOK,
			publicAppID:  fixtureAppID,
			wantErr:      true,
		},
		{
			name: "public personal hidden",
			target: publish.RegistrationTarget{
				Owner:      "freeside-ai",
				OwnerID:    testOwnerID,
				Visibility: publish.AppVisibilityPublic,
			},
			ownerType:           "User",
			publicStatus:        http.StatusNotFound,
			authenticatedStatus: http.StatusOK,
			authenticatedAppID:  fixtureAppID,
			wantErr:             true,
		},
		{
			name: "public metadata wrong App",
			target: publish.RegistrationTarget{
				Owner:      "freeside-ai",
				OwnerID:    testOwnerID,
				Visibility: publish.AppVisibilityPublic,
			},
			ownerType:    "User",
			publicStatus: http.StatusOK,
			publicAppID:  fixtureAppID + 1,
			wantErr:      true,
		},
		{
			name:                "private metadata wrong authenticated App",
			target:              testRegistrationTarget(),
			ownerType:           "Organization",
			publicStatus:        http.StatusNotFound,
			authenticatedStatus: http.StatusOK,
			authenticatedAppID:  fixtureAppID + 1,
			wantErr:             true,
		},
		{
			name:                "private metadata cannot authenticate",
			target:              testRegistrationTarget(),
			ownerType:           "Organization",
			publicStatus:        http.StatusNotFound,
			authenticatedStatus: http.StatusUnauthorized,
			wantErr:             true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					switch r.URL.Path {
					case "/apps/freeside-publish":
						w.WriteHeader(tt.publicStatus)
						if tt.publicStatus == http.StatusOK {
							_ = json.NewEncoder(w).Encode(map[string]any{"id": tt.publicAppID})
						}
					case "/app":
						if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
							t.Error("authenticated App probe has no bearer token")
						}
						w.WriteHeader(tt.authenticatedStatus)
						if tt.authenticatedStatus == http.StatusOK {
							_ = json.NewEncoder(w).Encode(map[string]any{"id": tt.authenticatedAppID})
						}
					default:
						t.Errorf("visibility path = %q", r.URL.Path)
						w.WriteHeader(http.StatusNotFound)
					}
					return
				}
				fixture, err := os.ReadFile(filepath.Join("testdata", "conversion-response.json"))
				if err != nil {
					t.Errorf("read fixture: %v", err)
				}
				var resp map[string]any
				if err := json.Unmarshal(fixture, &resp); err != nil {
					t.Errorf("decode fixture: %v", err)
				}
				resp["owner"].(map[string]any)["type"] = tt.ownerType
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(resp)
			}))
			defer srv.Close()

			ks := newTestKeystore(t)
			r := publish.NewRegistrar(ks, srv.Client(), srv.URL, "https://github.example")
			creds, err := r.ExchangeCode(context.Background(), "CODE123", tt.target)
			if tt.wantErr {
				if err == nil {
					t.Fatal("ExchangeCode accepted mismatched visibility")
				}
				if apps, listErr := ks.ListApps(); listErr != nil || len(apps) != 0 {
					t.Errorf("visibility mismatch changed the keystore: apps=%d err=%v", len(apps), listErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ExchangeCode: %v", err)
			}
			if creds.Visibility != tt.wantVisibility {
				t.Errorf("visibility = %q, want %q", creds.Visibility, tt.wantVisibility)
			}
		})
	}
}

// TestManifestForm pins both owner endpoint forms and their topology-derived
// visibility, while proving caller-supplied authority fields are overwritten.
func TestManifestForm(t *testing.T) {
	ks := newTestKeystore(t)
	r := publish.NewRegistrar(ks, http.DefaultClient, "https://api.github.example", "https://github.example")

	tests := []struct {
		name       string
		target     publish.RegistrationTarget
		operator   string
		wantAction string
		wantName   string
		wantPublic bool
	}{
		{
			name: "personal default",
			target: publish.RegistrationTarget{
				Owner:      "bennelsonweiss",
				OwnerID:    testOwnerID,
				Visibility: publish.AppVisibilityPublic,
			},
			operator:   "bennelsonweiss",
			wantAction: "https://github.example/settings/apps/new",
			wantName:   "freeside-bennelsonweiss",
			wantPublic: true,
		},
		{
			name:       "organization work account",
			target:     testRegistrationTarget(),
			operator:   "bennelsonweiss",
			wantAction: "https://github.example/organizations/freeside-ai/settings/apps/new",
			wantName:   "freeside-ai-bennelsonweiss",
			wantPublic: false,
		},
		{
			name: "personal work account",
			target: publish.RegistrationTarget{
				Owner:      "bennelsonweiss",
				OwnerID:    testOwnerID,
				Visibility: publish.AppVisibilityPrivate,
			},
			operator:   "bennelsonweiss",
			wantAction: "https://github.example/settings/apps/new",
			wantName:   "freeside-bennelsonweiss",
			wantPublic: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := testManifest()
			input.Name = "caller-controlled"
			input.Public = !tt.wantPublic
			input.DefaultPermissions = publish.Permissions{}
			action, fields, err := r.ManifestForm(tt.target, tt.operator, 0, input)
			if err != nil {
				t.Fatal(err)
			}
			if action != tt.wantAction {
				t.Errorf("action = %q, want %q", action, tt.wantAction)
			}
			var got publish.Manifest
			if err := json.Unmarshal([]byte(fields.Get("manifest")), &got); err != nil {
				t.Fatalf("manifest field is not JSON: %v", err)
			}
			if got.Name != tt.wantName || got.Public != tt.wantPublic {
				t.Errorf("manifest identity = {Name:%q Public:%t}, want {Name:%q Public:%t}",
					got.Name, got.Public, tt.wantName, tt.wantPublic)
			}
			if got.URL != input.URL || got.DefaultPermissions != publish.PublishPermissions {
				t.Errorf("manifest = %+v, want URL %q and pinned permissions %+v",
					got, input.URL, publish.PublishPermissions)
			}
		})
	}

	publicOrganization := testRegistrationTarget()
	publicOrganization.Visibility = publish.AppVisibilityPublic
	if _, _, err := r.ManifestForm(publicOrganization, "bennelsonweiss", 0, testManifest()); err == nil {
		t.Error("ManifestForm accepted a public organization-owned registration")
	}
}

// TestGeneratedAppName pins the 34-character cap, the truncation precedence
// (prefix, then owner, then operator), and the increasing collision sequence.
func TestGeneratedAppName(t *testing.T) {
	tests := []struct {
		name     string
		operator string
		owner    string
		retry    int
		want     string
	}{
		{"personal", "alice", "alice", 0, "freeside-alice"},
		{"organization", "alice", "acme", 0, "freeside-acme-alice"},
		{"drop prefix first", "readable-operator", "owning-account", 0, "owning-account-readable-operator"},
		{"truncate owner second", "readable-operator", "extremely-long-owning-account-name", 0, "extremely-long-o-readable-operator"},
		{"truncate operator last", "extremely-long-operator-account-name", "org", 0, "extremely-long-operator-account-na"},
		{"first collision", "alice", "acme", 1, "freeside-acme-alice-2"},
		{"second collision", "alice", "acme", 2, "freeside-acme-alice-3"},
		{"collision reserves length", "extremely-long-operator-account-name", "org", 1, "extremely-long-operator-account-2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := publish.GeneratedAppName(tt.operator, tt.owner, tt.retry)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("GeneratedAppName() = %q, want %q", got, tt.want)
			}
			if len(got) > 34 {
				t.Errorf("GeneratedAppName() length = %d, want <= 34", len(got))
			}
		})
	}
}

// TestGeneratedAppNameBoundaries exhausts GitHub's login-length boundary over
// both owner legs and several collision widths. It is a refute-first check for
// truncation panics, over-cap names, and lost numeric retry suffixes.
func TestGeneratedAppNameBoundaries(t *testing.T) {
	for operatorLength := 1; operatorLength <= 39; operatorLength++ {
		operator := "u" + strings.Repeat("x", operatorLength-1)
		for ownerLength := 1; ownerLength <= 39; ownerLength++ {
			owner := "o" + strings.Repeat("y", ownerLength-1)
			for _, retry := range []int{0, 1, 8, 98} {
				got, err := publish.GeneratedAppName(operator, owner, retry)
				if err != nil {
					t.Fatalf("GeneratedAppName(%d, %d, %d): %v", operatorLength, ownerLength, retry, err)
				}
				if len(got) == 0 || len(got) > 34 {
					t.Fatalf("GeneratedAppName(%d, %d, %d) length = %d", operatorLength, ownerLength, retry, len(got))
				}
				if retry > 0 {
					wantSuffix := "-" + strconv.Itoa(retry+1)
					if !strings.HasSuffix(got, wantSuffix) {
						t.Fatalf("GeneratedAppName(%d, %d, %d) = %q, want suffix %q",
							operatorLength, ownerLength, retry, got, wantSuffix)
					}
				}
			}
		}
	}
	if _, err := publish.GeneratedAppName("alice", "acme", -1); err == nil {
		t.Error("GeneratedAppName accepted a negative retry")
	}
}

// TestExchangeCodeFixture drives the conversion exchange against the
// recorded fixture (issue #80 acceptance 1): the credentials decode,
// the key parses, and the pem's only landing place is the keystore.
func TestExchangeCodeFixture(t *testing.T) {
	var gotPath string
	srv := conversionServer(t, &gotPath)
	defer srv.Close()

	ks := newTestKeystore(t)
	r := publish.NewRegistrar(ks, srv.Client(), srv.URL, "https://github.example")

	creds, err := r.ExchangeCode(context.Background(), "CODE123", testRegistrationTarget())
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if want := "POST /app-manifests/CODE123/conversions"; gotPath != want {
		t.Errorf("request = %q, want %q", gotPath, want)
	}
	if creds.Owner != "freeside-ai" || creds.OwnerID != testOwnerID || creds.Visibility != publish.AppVisibilityPrivate ||
		creds.AppID != fixtureAppID || creds.Name != "freeside-publish" ||
		creds.Slug != "freeside-publish" || creds.ClientID != "Iv1.deadbeefdeadbeef" {
		t.Errorf("credentials identity = %+v", creds)
	}
	if want := "SHA256:hV+YFKg8ua+92Bp5aukhasPJ9bU/H6vmoC3lMQPYVvc="; creds.KeyID != want {
		t.Errorf("key id = %q, want GitHub SHA-256 fingerprint %q", creds.KeyID, want)
	}
	if !creds.Key.Equal(fixtureKey(t)) {
		t.Error("conversion key does not match the fixture key")
	}
	if creds.WebhookSecret.Reveal() != "whsec_WEBHOOKWEBHOOK" || creds.ClientSecret.Reveal() != "cs_CLIENTSECRETCLIENTSECRET" {
		t.Error("conversion secrets do not match the fixture")
	}

	// The registration's key material is already in protected storage.
	loaded, err := ks.LoadApp(creds.OwnerID)
	if err != nil {
		t.Fatalf("LoadApp after exchange: %v", err)
	}
	if !loaded.Key.Equal(fixtureKey(t)) {
		t.Error("keystore does not hold the converted key")
	}
	requested, err := publish.GeneratedAppName("bennelsonweiss", "freeside-ai", 0)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Name != "freeside-publish" || loaded.Name == requested || loaded.Slug != "freeside-publish" {
		t.Errorf("stored metadata = {Name:%q Slug:%q}, want canonical conversion metadata rather than requested name %q",
			loaded.Name, loaded.Slug, requested)
	}
}

// TestRegistrationRelay proves form production and conversion redemption do
// not share browser or listener state. An organization owner can submit the
// form elsewhere and relay the resulting one-time code to this daemon.
func TestRegistrationRelay(t *testing.T) {
	var gotPath string
	api := conversionServer(t, &gotPath)
	defer api.Close()

	ks := newTestKeystore(t)
	producer := publish.NewRegistrar(ks, http.DefaultClient, "http://unreachable.invalid", "https://github.example")
	m := testManifest()
	m.RedirectURL = "http://127.0.0.1:4321/relay"
	action, fields, err := producer.ManifestForm(testRegistrationTarget(), "bennelsonweiss", 0, m)
	if err != nil {
		t.Fatalf("produce manifest form: %v", err)
	}
	if action != "https://github.example/organizations/freeside-ai/settings/apps/new" || fields.Get("manifest") == "" {
		t.Fatalf("produced form = action %q manifest-present %t", action, fields.Get("manifest") != "")
	}

	redeemer := publish.NewRegistrar(ks, api.Client(), api.URL, "http://unused.invalid")
	creds, err := redeemer.ExchangeCode(context.Background(), "RELAYED-CODE", testRegistrationTarget())
	if err != nil {
		t.Fatalf("redeem relayed code: %v", err)
	}
	if gotPath != "POST /app-manifests/RELAYED-CODE/conversions" || creds.AppID != fixtureAppID {
		t.Errorf("relay result = path %q app %d", gotPath, creds.AppID)
	}
}

// TestExchangeCodeRejectsOwnerMismatch keeps an App created through the wrong
// personal or organization settings page from being stored under that
// unexpected account.
func TestExchangeCodeRejectsOwnerMismatch(t *testing.T) {
	var gotPath string
	srv := conversionServer(t, &gotPath)
	defer srv.Close()

	ks := newTestKeystore(t)
	r := publish.NewRegistrar(ks, srv.Client(), srv.URL, "https://github.example")
	if _, err := r.ExchangeCode(
		context.Background(),
		"CODE123",
		publish.RegistrationTarget{
			Owner:        "bennelsonweiss",
			OwnerID:      testOwnerID,
			Organization: true,
			Visibility:   publish.AppVisibilityPrivate,
		},
	); err == nil {
		t.Fatal("ExchangeCode accepted an App owned by the wrong account")
	}
	if apps, err := ks.ListApps(); err != nil || len(apps) != 0 {
		t.Errorf("owner-mismatched conversion changed the keystore: apps=%d err=%v", len(apps), err)
	}
}

// TestExchangeCodeRejectsOwnerTypeMismatch binds a relayed conversion code to
// the personal or organization endpoint that produced its manifest. Matching
// owner login and numeric ID are insufficient because GitHub's namespaces and
// the private-only organization posture carry different policy.
func TestExchangeCodeRejectsOwnerTypeMismatch(t *testing.T) {
	tests := []struct {
		name       string
		actualType string
		target     publish.RegistrationTarget
	}{
		{
			name:       "organization returned for personal target",
			actualType: "Organization",
			target: publish.RegistrationTarget{
				Owner:      "freeside-ai",
				OwnerID:    testOwnerID,
				Visibility: publish.AppVisibilityPrivate,
			},
		},
		{
			name:       "user returned for organization target",
			actualType: "User",
			target:     testRegistrationTarget(),
		},
		{
			name:       "missing returned type",
			actualType: "",
			target:     testRegistrationTarget(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fixture, err := os.ReadFile(filepath.Join("testdata", "conversion-response.json"))
				if err != nil {
					t.Errorf("read fixture: %v", err)
				}
				var resp map[string]any
				if err := json.Unmarshal(fixture, &resp); err != nil {
					t.Errorf("decode fixture: %v", err)
				}
				owner := resp["owner"].(map[string]any)
				owner["type"] = tt.actualType
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(resp)
			}))
			defer srv.Close()

			ks := newTestKeystore(t)
			r := publish.NewRegistrar(ks, srv.Client(), srv.URL, "https://github.example")
			if _, err := r.ExchangeCode(context.Background(), "CODE123", tt.target); err == nil {
				t.Fatal("ExchangeCode accepted an owner-type mismatch")
			}
			if apps, err := ks.ListApps(); err != nil || len(apps) != 0 {
				t.Errorf("owner-type mismatch changed the keystore: apps=%d err=%v", len(apps), err)
			}
		})
	}
}

// TestExchangeCodeRedactsReturnedOwner keeps an untrusted conversion response
// from smuggling sensitive text into an error through its owner field.
func TestExchangeCodeRedactsReturnedOwner(t *testing.T) {
	const secret = "SECRET_RETURNED_OWNER_VALUE"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fixture, err := os.ReadFile(filepath.Join("testdata", "conversion-response.json"))
		if err != nil {
			t.Errorf("read fixture: %v", err)
		}
		var resp map[string]any
		if err := json.Unmarshal(fixture, &resp); err != nil {
			t.Errorf("decode fixture: %v", err)
		}
		resp["owner"] = map[string]any{"login": secret, "id": testOwnerID}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	ks := newTestKeystore(t)
	r := publish.NewRegistrar(ks, srv.Client(), srv.URL, "https://github.example")
	_, err := r.ExchangeCode(context.Background(), "CODE123", testRegistrationTarget())
	if err == nil {
		t.Fatal("ExchangeCode accepted an invalid returned owner")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("ExchangeCode error disclosed the returned owner: %v", err)
	}
}

// TestExchangeCodeRejectsOwnerIDMismatch prevents a renamed or login-reused
// account from being accepted as the expected registration owner.
func TestExchangeCodeRejectsOwnerIDMismatch(t *testing.T) {
	var gotPath string
	srv := conversionServer(t, &gotPath)
	defer srv.Close()

	ks := newTestKeystore(t)
	r := publish.NewRegistrar(ks, srv.Client(), srv.URL, "https://github.example")
	if _, err := r.ExchangeCode(
		context.Background(),
		"CODE123",
		publish.RegistrationTarget{
			Owner:        "freeside-ai",
			OwnerID:      testOwnerID + 1,
			Organization: true,
			Visibility:   publish.AppVisibilityPrivate,
		},
	); err == nil {
		t.Fatal("ExchangeCode accepted an App owned by the wrong numeric account")
	}
	if apps, err := ks.ListApps(); err != nil || len(apps) != 0 {
		t.Errorf("owner-ID-mismatched conversion changed the keystore: apps=%d err=%v", len(apps), err)
	}
}

// TestExchangeCodeAPIError asserts a failed conversion surfaces as the
// APIError carrier and writes nothing to the keystore.
func TestExchangeCodeAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	}))
	defer srv.Close()

	ks := newTestKeystore(t)
	r := publish.NewRegistrar(ks, srv.Client(), srv.URL, "https://github.example")
	_, err := r.ExchangeCode(context.Background(), "EXPIRED", testRegistrationTarget())
	if !errors.Is(err, publish.ErrGitHubAPI) {
		t.Fatalf("err = %v, want ErrGitHubAPI", err)
	}
	if apps, err := ks.ListApps(); err != nil || len(apps) != 0 {
		t.Error("failed exchange left credentials in the keystore")
	}
}

// TestExchangeCodeTransportErrorRedactsCode asserts a transport
// failure never renders the manifest code: an unconsumed code is
// credential-equivalent (it exchanges for the App key), and Go wraps
// transport errors in *url.Error, which carries the full request URL.
func TestExchangeCodeTransportErrorRedactsCode(t *testing.T) {
	ks := newTestKeystore(t)
	// A closed port: the connect fails before any request leaves.
	r := publish.NewRegistrar(ks, &http.Client{Timeout: time.Second}, "http://127.0.0.1:1", "https://github.example")
	_, err := r.ExchangeCode(context.Background(), "SECRETMANIFESTCODE123", testRegistrationTarget())
	if err == nil {
		t.Fatal("ExchangeCode against a closed port succeeded")
	}
	if strings.Contains(err.Error(), "SECRETMANIFESTCODE123") {
		t.Errorf("transport error carries the manifest code: %v", err)
	}
}

// TestExchangeCodeRefusesRedirect asserts a 3xx from the conversion
// endpoint is never followed: following it would send the code-bearing
// URL as the Referer to the redirect target. The 3xx surfaces as the
// redacted APIError.
func TestExchangeCodeRefusesRedirect(t *testing.T) {
	var targetHits int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer srv.Close()

	ks := newTestKeystore(t)
	r := publish.NewRegistrar(ks, srv.Client(), srv.URL, "https://github.example")
	_, err := r.ExchangeCode(context.Background(), "SECRETMANIFESTCODE123", testRegistrationTarget())
	var apiErr *publish.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusFound {
		t.Errorf("err = %v, want *APIError with status 302", err)
	}
	if targetHits != 0 {
		t.Errorf("redirect target received %d requests, want 0", targetHits)
	}
}

// fetchRedirectURL plays the browser far enough to learn this
// attempt's callback: it fetches the form page and extracts the
// manifest's redirect_url, exactly what GitHub would receive and
// redirect to.
func fetchRedirectURL(t *testing.T, formURL string) string {
	t.Helper()
	resp, err := http.Get(formURL) //nolint:gosec // scripted test browser fetching the local one-shot page
	if err != nil {
		t.Errorf("fetch form page: %v", err)
		return ""
	}
	page, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(page), "/settings/apps/new") {
		t.Errorf("form page does not target the manifest endpoint: %s", page)
	}
	match := regexp.MustCompile(`name="manifest" value="([^"]*)"`).FindStringSubmatch(string(page))
	if match == nil {
		t.Errorf("form page carries no manifest field: %s", page)
		return ""
	}
	var m publish.Manifest
	if err := json.Unmarshal([]byte(html.UnescapeString(match[1])), &m); err != nil {
		t.Errorf("manifest field is not JSON: %v", err)
		return ""
	}
	return m.RedirectURL
}

// TestExchangeCodeRejectsMissingAppID covers the conversion's
// returned-object boundary: a valid PEM with no app id must not
// overwrite the keystore, since issuer-0 credentials would replace
// working ones and fail every later mint.
func TestExchangeCodeRejectsMissingAppID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fixture, err := os.ReadFile(filepath.Join("testdata", "conversion-response.json"))
		if err != nil {
			t.Errorf("read fixture: %v", err)
		}
		var resp map[string]any
		if err := json.Unmarshal(fixture, &resp); err != nil {
			t.Errorf("decode fixture: %v", err)
		}
		delete(resp, "id")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	ks := newTestKeystore(t)
	r := publish.NewRegistrar(ks, srv.Client(), srv.URL, "https://github.example")
	if _, err := r.ExchangeCode(context.Background(), "CODE123", testRegistrationTarget()); err == nil {
		t.Error("conversion without app id accepted, want error")
	}
	if apps, err := ks.ListApps(); err != nil || len(apps) != 0 {
		t.Error("rejected conversion left credentials in the keystore")
	}
}

// TestExchangeCodeRejectsMissingCanonicalMetadata keeps an incomplete
// returned App identity from becoming the owner-keyed registration record.
func TestExchangeCodeRejectsMissingCanonicalMetadata(t *testing.T) {
	for _, field := range []string{"name", "slug"} {
		t.Run(field, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fixture, err := os.ReadFile(filepath.Join("testdata", "conversion-response.json"))
				if err != nil {
					t.Errorf("read fixture: %v", err)
				}
				var resp map[string]any
				if err := json.Unmarshal(fixture, &resp); err != nil {
					t.Errorf("decode fixture: %v", err)
				}
				delete(resp, field)
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(resp)
			}))
			defer srv.Close()

			ks := newTestKeystore(t)
			r := publish.NewRegistrar(ks, srv.Client(), srv.URL, "https://github.example")
			if _, err := r.ExchangeCode(context.Background(), "CODE123", testRegistrationTarget()); err == nil {
				t.Errorf("conversion without %s accepted, want error", field)
			}
			if apps, err := ks.ListApps(); err != nil || len(apps) != 0 {
				t.Errorf("conversion without %s changed the keystore: apps=%d err=%v", field, len(apps), err)
			}
		})
	}
}

// TestExchangeCodeRejectsPermissionMismatch verifies the converted App
// reports exactly the pinned manifest permissions before its one-time
// credentials can replace the keystore.
func TestExchangeCodeRejectsPermissionMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fixture, err := os.ReadFile(filepath.Join("testdata", "conversion-response.json"))
		if err != nil {
			t.Errorf("read fixture: %v", err)
		}
		var resp map[string]any
		if err := json.Unmarshal(fixture, &resp); err != nil {
			t.Errorf("decode fixture: %v", err)
		}
		resp["permissions"] = map[string]string{"metadata": "read"}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	ks := newTestKeystore(t)
	r := publish.NewRegistrar(ks, srv.Client(), srv.URL, "https://github.example")
	if _, err := r.ExchangeCode(context.Background(), "CODE123", testRegistrationTarget()); err == nil {
		t.Error("conversion with mismatched permissions accepted, want error")
	}
	if apps, err := ks.ListApps(); err != nil || len(apps) != 0 {
		t.Error("rejected conversion left credentials in the keystore")
	}
}

// TestRegisterOrchestration runs the whole local flow with a scripted
// "browser": fetch the form page, then follow GitHub's redirect to the
// per-attempt callback with the temporary code. A request to a wrong
// callback path first must be ignored, not abort the flow.
func TestRegisterOrchestration(t *testing.T) {
	var gotPath string
	api := conversionServer(t, &gotPath)
	defer api.Close()

	ks := newTestKeystore(t)
	r := publish.NewRegistrar(ks, api.Client(), api.URL, "https://github.example")

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	browsed := make(chan error, 1)
	openURL := func(u string) error {
		go func() {
			browsed <- func() error {
				redirect := fetchRedirectURL(t, u)
				if redirect == "" {
					return errors.New("no redirect URL")
				}
				// An unrelated local request must learn nothing and
				// change nothing: the root discloses no manifest (and
				// so no callback nonce), and a guessed callback path
				// is ignored rather than injecting a code.
				for _, probe := range []string{"/", "/callback/0000?code=EVIL"} {
					resp, err := http.Get("http://" + l.Addr().String() + probe)
					if err != nil {
						return err
					}
					_ = resp.Body.Close()
					if resp.StatusCode != http.StatusNotFound {
						t.Errorf("probe %s = %d, want 404", probe, resp.StatusCode)
					}
				}
				// GitHub's post-approval redirect:
				resp, err := http.Get(redirect + "?code=CODE123")
				if err != nil {
					return err
				}
				_ = resp.Body.Close()
				return nil
			}()
		}()
		return nil
	}

	creds, err := r.Register(
		context.Background(),
		testRegistrationTarget(),
		"bennelsonweiss",
		0,
		testManifest(),
		l,
		openURL,
	)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := <-browsed; err != nil {
		t.Fatalf("scripted browser: %v", err)
	}
	if creds.AppID != fixtureAppID {
		t.Errorf("AppID = %d, want %d", creds.AppID, fixtureAppID)
	}
	if _, err := ks.LoadApp(creds.OwnerID); err != nil {
		t.Errorf("keystore empty after Register: %v", err)
	}
}

type addrOnlyListener struct {
	addr net.Addr
}

func (l *addrOnlyListener) Accept() (net.Conn, error) { return nil, errors.New("unexpected Accept") }
func (l *addrOnlyListener) Close() error              { return nil }
func (l *addrOnlyListener) Addr() net.Addr            { return l.addr }

// TestRegisterRejectsNonLoopbackListener keeps the manifest and its
// callback nonce off wildcard and externally reachable listeners. A
// successful bind is not enough; the advertised callback must be an
// explicit loopback address.
func TestRegisterRejectsNonLoopbackListener(t *testing.T) {
	ks := newTestKeystore(t)
	r := publish.NewRegistrar(ks, http.DefaultClient, "https://api.github.example", "https://github.example")
	for _, ip := range []net.IP{net.IPv4zero, net.IPv6zero, net.ParseIP("192.0.2.1")} {
		l := &addrOnlyListener{addr: &net.TCPAddr{IP: ip, Port: 4321}}
		opened := false
		_, err := r.Register(context.Background(), testRegistrationTarget(), "bennelsonweiss", 0, testManifest(), l, func(string) error {
			opened = true
			return nil
		})
		if err == nil {
			t.Errorf("Register on %s succeeded, want non-loopback rejection", l.Addr())
		}
		if opened {
			t.Errorf("Register on %s opened a browser before rejection", l.Addr())
		}
	}
}

// TestRegisterDenied covers the cancelled flow: a redirect without a
// code must not reach the conversion API.
func TestRegisterDenied(t *testing.T) {
	ks := newTestKeystore(t)
	r := publish.NewRegistrar(ks, http.DefaultClient, "http://unreachable.invalid", "https://github.example")

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	openURL := func(u string) error {
		go func() {
			redirect := fetchRedirectURL(t, u)
			if redirect == "" {
				return
			}
			resp, err := http.Get(redirect) //nolint:gosec // scripted test browser following the manifest redirect
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
		return nil
	}

	_, err = r.Register(context.Background(), testRegistrationTarget(), "bennelsonweiss", 0, testManifest(), l, openURL)
	if !errors.Is(err, publish.ErrRegistrationDenied) {
		t.Errorf("Register without code = %v, want ErrRegistrationDenied", err)
	}
}
