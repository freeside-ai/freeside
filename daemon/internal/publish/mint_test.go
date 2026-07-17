package publish_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// fixtureTokenValue must match testdata/token-response.json.
const fixtureTokenValue = "ghs_FIXTURETOKENFIXTURETOKEN0000" //nolint:gosec // fixture value from the recorded response, not a credential

type captureRecorder struct {
	records []publish.MintRecord
	err     error
}

func (r *captureRecorder) RecordMint(rec publish.MintRecord) error {
	if r.err != nil {
		return r.err
	}
	r.records = append(r.records, rec)
	return nil
}

// newRegisteredKeystore returns a keystore already holding the fixture
// App credentials, as registration would leave it.
func newRegisteredKeystore(t *testing.T) *publish.Keystore {
	t.Helper()
	ks := newTestKeystore(t)
	if err := ks.SaveApp(publish.AppCredentials{
		AppID:         fixtureAppID,
		Slug:          "freeside-publish",
		ClientID:      "Iv1.deadbeefdeadbeef",
		Key:           fixtureKey(t),
		WebhookSecret: publish.Secret("whsec_WEBHOOKWEBHOOK"),
		ClientSecret:  publish.Secret("cs_CLIENTSECRETCLIENTSECRET"),
	}); err != nil {
		t.Fatalf("SaveApp: %v", err)
	}
	return ks
}

func fixedNow() time.Time { return fixtureTime }

// TestMintInstallationToken drives the full lifecycle against the
// recorded fixture (issue #80 acceptance 1): the request carries the
// App JWT and the pinned minimum scope body, the fixture response
// decodes into a redaction-safe token, and the mint lands one audit
// record with both requested and granted scopes (acceptance 3).
func TestMintInstallationToken(t *testing.T) {
	ks := newRegisteredKeystore(t)
	wantJWT, err := publish.AppJWT(fixtureKey(t), fixtureAppID, fixtureTime)
	if err != nil {
		t.Fatal(err)
	}

	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/app/installations/777/access_tokens" {
			t.Errorf("request = %s %s, want POST /app/installations/777/access_tokens", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+wantJWT.Reveal() {
			t.Error("Authorization header does not carry the App JWT")
		}
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Errorf("Accept = %q", got)
		}
		if got := r.Header.Get("X-GitHub-Api-Version"); got != "2022-11-28" {
			t.Errorf("X-GitHub-Api-Version = %q", got)
		}
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		fixture, err := os.ReadFile(filepath.Join("testdata", "token-response.json"))
		if err != nil {
			t.Errorf("read fixture: %v", err)
		}
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()

	rec := &captureRecorder{}
	m := publish.NewMinter(ks, srv.Client(), srv.URL, rec, fixedNow)
	tok, err := m.MintInstallationToken(context.Background(), 777, "evidence-repo")
	if err != nil {
		t.Fatalf("MintInstallationToken: %v", err)
	}

	// The request body golden is the durable statement of the minimum
	// scope set the publish path requests.
	golden.Assert(t, "mint-request", gotBody)

	if tok.Token.Reveal() != fixtureTokenValue {
		t.Error("token does not match the fixture response")
	}
	if want := time.Date(2026, 7, 16, 13, 0, 0, 0, time.UTC); !tok.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v", tok.ExpiresAt, want)
	}
	if tok.Repo != "evidence-repo" {
		t.Errorf("Repo = %q", tok.Repo)
	}
	if tok.Permissions != publish.PublishPermissions {
		t.Errorf("granted permissions = %+v, want %+v", tok.Permissions, publish.PublishPermissions)
	}

	if len(rec.records) != 1 {
		t.Fatalf("recorded %d mints, want 1", len(rec.records))
	}
	r := rec.records[0]
	if r.Requested != publish.PublishPermissions || r.Granted != publish.PublishPermissions {
		t.Errorf("record scopes = %+v", r)
	}
	if r.InstallationID != 777 || r.Repo != "evidence-repo" {
		t.Errorf("record identity = %+v", r)
	}
	if !r.MintedAt.Equal(fixtureTime) {
		t.Errorf("MintedAt = %v, want %v", r.MintedAt, fixtureTime)
	}

	// The audit shape cannot carry the token: no field exists for it.
	line, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(line), fixtureTokenValue) || strings.Contains(string(line), `"token"`) {
		t.Errorf("audit record carries token material: %s", line)
	}
}

// TestMintRejectsGrantMismatch asserts any grant differing from the
// request fails closed with the token discarded and nothing audited:
// a narrower grant would fail the publish path halfway, and a broader
// one (extra permission key, all-repositories selection, different
// repository set) would circulate more authority than the audit
// records. The permission object decodes losslessly, so an unknown
// key cannot be dropped before the comparison.
func TestMintRejectsGrantMismatch(t *testing.T) {
	const (
		expiry    = `"expires_at":"2026-07-16T13:00:00Z"`
		wantRepos = `"repository_selection":"selected","repositories":[{"name":"evidence-repo"}]`
		wantPerms = `"permissions":{"contents":"write","pull_requests":"write","metadata":"read"}`
	)
	cases := []struct {
		name string
		body string
	}{
		{"narrower permissions", `{"token":"` + fixtureTokenValue + `",` + expiry + `,` +
			`"permissions":{"contents":"write","metadata":"read"},` + wantRepos + `}`},
		{"extra permission key", `{"token":"` + fixtureTokenValue + `",` + expiry + `,` +
			`"permissions":{"contents":"write","pull_requests":"write","metadata":"read","issues":"write"},` + wantRepos + `}`},
		{"credential-shaped permission", `{"token":"` + fixtureTokenValue + `",` + expiry + `,` +
			`"permissions":{"contents":"` + fixtureTokenValue + `","pull_requests":"write","metadata":"read"},` + wantRepos + `}`},
		{"all repositories", `{"token":"` + fixtureTokenValue + `",` + expiry + `,` +
			wantPerms + `,"repository_selection":"all","repositories":[]}`},
		{"credential-shaped repository selection", `{"token":"` + fixtureTokenValue + `",` + expiry + `,` +
			wantPerms + `,"repository_selection":"` + fixtureTokenValue + `","repositories":[]}`},
		{"different repository", `{"token":"` + fixtureTokenValue + `",` + expiry + `,` +
			wantPerms + `,"repository_selection":"selected","repositories":[{"name":"other-repo"}]}`},
		{"credential-shaped repository", `{"token":"` + fixtureTokenValue + `",` + expiry + `,` +
			wantPerms + `,"repository_selection":"selected","repositories":[{"name":"` + fixtureTokenValue + `"}]}`},
		{"extra repository", `{"token":"` + fixtureTokenValue + `",` + expiry + `,` +
			wantPerms + `,"repository_selection":"selected","repositories":[{"name":"evidence-repo"},{"name":"other-repo"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ks := newRegisteredKeystore(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			rec := &captureRecorder{}
			m := publish.NewMinter(ks, srv.Client(), srv.URL, rec, fixedNow)
			_, err := m.MintInstallationToken(context.Background(), 777, "evidence-repo")
			if !errors.Is(err, publish.ErrGrantMismatch) {
				t.Fatalf("err = %v, want ErrGrantMismatch", err)
			}
			if strings.Contains(err.Error(), fixtureTokenValue) {
				t.Errorf("error carries the token: %v", err)
			}
			if len(rec.records) != 0 {
				t.Errorf("rejected mint recorded %d audit rows, want 0", len(rec.records))
			}
		})
	}
}

// TestMintRejectsUnusableToken covers the returned-object boundary: a
// syntactically valid 201 whose token is missing or already expired
// must not advance the audit barrier or circulate as a credential.
func TestMintRejectsUnusableToken(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty token", `{"token":"","expires_at":"2026-07-16T13:00:00Z",` +
			`"permissions":{"contents":"write","pull_requests":"write","metadata":"read"},` +
			`"repository_selection":"selected","repositories":[{"name":"evidence-repo"}]}`},
		{"expired token", `{"token":"` + fixtureTokenValue + `","expires_at":"2026-07-16T11:00:00Z",` +
			`"permissions":{"contents":"write","pull_requests":"write","metadata":"read"},` +
			`"repository_selection":"selected","repositories":[{"name":"evidence-repo"}]}`},
		{"credential-shaped invalid expiry", `{"token":"` + fixtureTokenValue + `","expires_at":"` + fixtureTokenValue + `",` +
			`"permissions":{"contents":"write","pull_requests":"write","metadata":"read"},` +
			`"repository_selection":"selected","repositories":[{"name":"evidence-repo"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ks := newRegisteredKeystore(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			rec := &captureRecorder{}
			m := publish.NewMinter(ks, srv.Client(), srv.URL, rec, fixedNow)
			_, err := m.MintInstallationToken(context.Background(), 777, "evidence-repo")
			if err == nil {
				t.Error("unusable token accepted, want error")
			}
			for _, needle := range []string{fixtureTokenValue, "2026-07-16T11:00:00Z"} {
				if strings.Contains(err.Error(), needle) {
					t.Errorf("unusable-token error carries response value %q: %v", needle, err)
				}
			}
			if len(rec.records) != 0 {
				t.Errorf("unusable mint recorded %d audit rows, want 0", len(rec.records))
			}
		})
	}
}

// TestMintAPIError asserts a non-201 surfaces as the APIError carrier
// with no audit record and no body content in the error.
func TestMintAPIError(t *testing.T) {
	ks := newRegisteredKeystore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"message":"boom ghs_LEAKYBODYVALUE"}`)
	}))
	defer srv.Close()

	rec := &captureRecorder{}
	m := publish.NewMinter(ks, srv.Client(), srv.URL, rec, fixedNow)
	_, err := m.MintInstallationToken(context.Background(), 777, "evidence-repo")
	if !errors.Is(err, publish.ErrGitHubAPI) {
		t.Fatalf("err = %v, want ErrGitHubAPI", err)
	}
	var apiErr *publish.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusInternalServerError {
		t.Errorf("err = %v, want *APIError with status 500", err)
	}
	if strings.Contains(err.Error(), "LEAKYBODYVALUE") {
		t.Errorf("error carries response body: %v", err)
	}
	if len(rec.records) != 0 {
		t.Errorf("failed mint recorded %d audit rows, want 0", len(rec.records))
	}
}

// TestMintFailsWhenRecorderFails: an unauditable token must not
// circulate.
func TestMintFailsWhenRecorderFails(t *testing.T) {
	ks := newRegisteredKeystore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fixture, _ := os.ReadFile(filepath.Join("testdata", "token-response.json"))
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()

	rec := &captureRecorder{err: errors.New("audit disk full")}
	m := publish.NewMinter(ks, srv.Client(), srv.URL, rec, fixedNow)
	if _, err := m.MintInstallationToken(context.Background(), 777, "evidence-repo"); err == nil {
		t.Error("mint succeeded with a failing recorder, want error")
	}
}

// TestMintValidation covers the fail-fast argument checks.
func TestMintValidation(t *testing.T) {
	ks := newRegisteredKeystore(t)
	m := publish.NewMinter(ks, http.DefaultClient, "http://unreachable.invalid", &captureRecorder{}, fixedNow)
	if _, err := m.MintInstallationToken(context.Background(), 0, "repo"); err == nil {
		t.Error("installation id 0 accepted, want error")
	}
	if _, err := m.MintInstallationToken(context.Background(), 777, ""); err == nil {
		t.Error("empty repo accepted, want error")
	}
}

// newTestStore opens a real temp-file store; SQLite needs a file (a
// pooled :memory: DSN would give each connection its own database).
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "store.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	return s
}

// TestStoreRecorder drives a full mint through the production audit
// path (issue #107 acceptance 2): the record lands on the store-owned
// SQLite surface and reads back field-identical.
func TestStoreRecorder(t *testing.T) {
	ks := newRegisteredKeystore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fixture, _ := os.ReadFile(filepath.Join("testdata", "token-response.json"))
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()

	s := newTestStore(t)
	rec, err := publish.NewStoreRecorder(s)
	if err != nil {
		t.Fatalf("NewStoreRecorder: %v", err)
	}
	m := publish.NewMinter(ks, srv.Client(), srv.URL, rec, fixedNow)
	if _, err := m.MintInstallationToken(context.Background(), 777, "evidence-repo"); err != nil {
		t.Fatalf("MintInstallationToken: %v", err)
	}

	var audits []store.MintAudit
	err = s.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		audits, err = tx.ListMintAudits(context.Background())
		return err
	})
	if err != nil {
		t.Fatalf("ListMintAudits: %v", err)
	}
	if len(audits) != 1 {
		t.Fatalf("recorded %d audits, want 1", len(audits))
	}
	got := audits[0]
	if got.InstallationID != 777 || got.Repo != "evidence-repo" {
		t.Errorf("audit identity = %+v", got)
	}
	if !got.MintedAt.Equal(fixtureTime) {
		t.Errorf("MintedAt = %v, want %v", got.MintedAt, fixtureTime)
	}
	want := publish.PublishPermissions
	if got.RequestedContents != want.Contents || got.RequestedPullRequests != want.PullRequests ||
		got.RequestedMetadata != want.Metadata || got.GrantedContents != want.Contents ||
		got.GrantedPullRequests != want.PullRequests || got.GrantedMetadata != want.Metadata {
		t.Errorf("audit scopes = %+v, want %+v for requested and granted", got, want)
	}
}

// TestStoreRecorderFailsClosed: the #80 invariant against the real
// recorder rather than a fake — an audit write that cannot commit
// fails the mint, and a nil store fails at construction.
func TestStoreRecorderFailsClosed(t *testing.T) {
	if _, err := publish.NewStoreRecorder(nil); err == nil {
		t.Error("NewStoreRecorder(nil) succeeded, want error")
	}

	ks := newRegisteredKeystore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fixture, _ := os.ReadFile(filepath.Join("testdata", "token-response.json"))
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()

	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "store.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	rec, err := publish.NewStoreRecorder(s)
	if err != nil {
		t.Fatalf("NewStoreRecorder: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}

	m := publish.NewMinter(ks, srv.Client(), srv.URL, rec, fixedNow)
	if _, err := m.MintInstallationToken(context.Background(), 777, "evidence-repo"); err == nil {
		t.Error("mint succeeded with an unwritable audit store, want error")
	}
}
