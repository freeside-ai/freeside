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
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

func testManifest() publish.Manifest {
	return publish.Manifest{
		Name:               "requested-app-name",
		URL:                "https://github.com/freeside-ai/freeside",
		Public:             false,
		DefaultPermissions: publish.PublishPermissions,
	}
}

// conversionServer serves the recorded manifest-conversion fixture and
// captures the request path.
func conversionServer(t *testing.T, gotPath *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotPath = r.Method + " " + r.URL.Path
		w.WriteHeader(http.StatusCreated)
		fixture, err := os.ReadFile(filepath.Join("testdata", "conversion-response.json"))
		if err != nil {
			t.Errorf("read fixture: %v", err)
		}
		_, _ = w.Write(fixture)
	}))
}

// TestManifestForm pins the form action and checks the manifest field
// round-trips as JSON.
func TestManifestForm(t *testing.T) {
	ks := newTestKeystore(t)
	r := publish.NewRegistrar(ks, http.DefaultClient, "https://api.github.example", "https://github.example")

	input := testManifest()
	input.DefaultPermissions = publish.Permissions{}
	action, fields, err := r.ManifestForm(input)
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://github.example/settings/apps/new"; action != want {
		t.Errorf("action = %q, want %q", action, want)
	}
	var got publish.Manifest
	if err := json.Unmarshal([]byte(fields.Get("manifest")), &got); err != nil {
		t.Fatalf("manifest field is not JSON: %v", err)
	}
	if got != testManifest() {
		t.Errorf("manifest round-trip = %+v, want %+v", got, testManifest())
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

	creds, err := r.ExchangeCode(context.Background(), "CODE123", "freeside-ai", testOwnerID, publish.AppVisibilityPrivate)
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
	if loaded.Name != "freeside-publish" || loaded.Name == testManifest().Name {
		t.Errorf("stored name = %q, want canonical conversion name rather than requested %q", loaded.Name, testManifest().Name)
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
		"bennelsonweiss",
		testOwnerID,
		publish.AppVisibilityPrivate,
	); err == nil {
		t.Fatal("ExchangeCode accepted an App owned by the wrong account")
	}
	if apps, err := ks.ListApps(); err != nil || len(apps) != 0 {
		t.Errorf("owner-mismatched conversion changed the keystore: apps=%d err=%v", len(apps), err)
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
		"freeside-ai",
		testOwnerID+1,
		publish.AppVisibilityPrivate,
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
	_, err := r.ExchangeCode(context.Background(), "EXPIRED", "freeside-ai", testOwnerID, publish.AppVisibilityPrivate)
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
	_, err := r.ExchangeCode(context.Background(), "SECRETMANIFESTCODE123", "freeside-ai", testOwnerID, publish.AppVisibilityPrivate)
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
	_, err := r.ExchangeCode(context.Background(), "SECRETMANIFESTCODE123", "freeside-ai", testOwnerID, publish.AppVisibilityPrivate)
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
	if _, err := r.ExchangeCode(context.Background(), "CODE123", "freeside-ai", testOwnerID, publish.AppVisibilityPrivate); err == nil {
		t.Error("conversion without app id accepted, want error")
	}
	if apps, err := ks.ListApps(); err != nil || len(apps) != 0 {
		t.Error("rejected conversion left credentials in the keystore")
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
	if _, err := r.ExchangeCode(context.Background(), "CODE123", "freeside-ai", testOwnerID, publish.AppVisibilityPrivate); err == nil {
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

	creds, err := r.Register(context.Background(), "freeside-ai", testOwnerID, testManifest(), l, openURL)
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
		_, err := r.Register(context.Background(), "freeside-ai", testOwnerID, testManifest(), l, func(string) error {
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

	_, err = r.Register(context.Background(), "freeside-ai", testOwnerID, testManifest(), l, openURL)
	if !errors.Is(err, publish.ErrRegistrationDenied) {
		t.Errorf("Register without code = %v, want ErrRegistrationDenied", err)
	}
}
