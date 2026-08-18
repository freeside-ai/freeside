package publish_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

var appBotIdentityNow = time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)

type appBotIdentityTokenSource struct {
	mu    sync.Mutex
	token publish.InstallationToken
	calls int
}

func (s *appBotIdentityTokenSource) Token(_ context.Context, _ string) (publish.InstallationToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.token, nil
}

func (s *appBotIdentityTokenSource) setToken(token publish.InstallationToken) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = token
}

func (s *appBotIdentityTokenSource) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func appBotIdentityKeystore(t *testing.T) *publish.Keystore {
	t.Helper()
	root := t.TempDir()
	keystore, err := publish.NewKeystore(
		filepath.Join(root, "credentials"), filepath.Join(root, "state"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := keystore.SaveApp(testCredentials()); err != nil {
		t.Fatal(err)
	}
	return keystore
}

func appBotIdentityToken() publish.InstallationToken {
	return publish.InstallationToken{
		Token:          publish.Secret("github_pat_app_identity_test"),
		ExpiresAt:      appBotIdentityNow.Add(time.Hour),
		RegistrationID: testCredentials().AppID,
		InstallationID: 777,
		RepositoryID:   fixtureRepositoryID,
		Repo:           testTrustRepo,
	}
}

func TestGitHubAppBotIdentityResolverBindsSelectedRegistration(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/users/freeside-publish[bot]" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.URL.EscapedPath() != "/users/freeside-publish%5Bbot%5D" {
			t.Errorf("escaped request path = %q", r.URL.EscapedPath())
		}
		if r.Header.Get("Authorization") != "Bearer github_pat_app_identity_test" {
			t.Error("request did not use the selected installation token")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"login":"freeside-publish[bot]","id":308829240,"type":"Bot"}`))
	}))
	t.Cleanup(server.Close)
	resolver, err := publish.NewGitHubAppBotIdentityResolver(
		fixedTokenSource{token: appBotIdentityToken()},
		appBotIdentityKeystore(t), server.Client(), server.URL, func() time.Time { return appBotIdentityNow },
	)
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolver.Resolve(t.Context(), testTrustRepo)
	if err != nil {
		t.Fatal(err)
	}
	if got.AppSlug != "freeside-publish" || got.BotUserID != 308829240 {
		t.Fatalf("resolved identity = %#v", got)
	}
}

func TestInspectAppBotIdentityUsesPublicRegistrationLookup(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.EscapedPath() != "/users/freeside-publish%5Bbot%5D" {
			t.Errorf("request = %s %s", r.Method, r.URL.EscapedPath())
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("public App identity lookup unexpectedly carried authorization")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"login":"freeside-publish[bot]","id":308829240,"type":"Bot"}`))
	}))
	t.Cleanup(server.Close)
	identity, err := publish.InspectAppBotIdentity(
		t.Context(), testCredentials(), server.Client(), server.URL,
	)
	if err != nil || identity.AppSlug != "freeside-publish" || identity.BotUserID != 308829240 {
		t.Fatalf("identity = %+v, error = %v", identity, err)
	}
}

func TestGitHubAppBotIdentityResolverCachesOneTokenBinding(t *testing.T) {
	t.Parallel()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"login":"freeside-publish[bot]","id":308829240,"type":"Bot"}`))
	}))
	t.Cleanup(server.Close)
	now := appBotIdentityNow
	source := &appBotIdentityTokenSource{token: appBotIdentityToken()}
	resolver, err := publish.NewGitHubAppBotIdentityResolver(
		source, appBotIdentityKeystore(t), server.Client(), server.URL,
		func() time.Time { return now },
	)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := resolver.Resolve(t.Context(), testTrustRepo); err != nil {
			t.Fatal(err)
		}
	}
	if requests.Load() != 1 || source.callCount() != 2 {
		t.Fatalf("stable binding made %d bot requests and %d token checks, want 1 and 2",
			requests.Load(), source.callCount())
	}
	if _, err := resolver.Revalidate(t.Context(), testTrustRepo); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 || source.callCount() != 3 {
		t.Fatalf("revalidated binding made %d bot requests and %d token checks, want 1 and 3",
			requests.Load(), source.callCount())
	}
	token := appBotIdentityToken()
	token.ExpiresAt = token.ExpiresAt.Add(time.Hour)
	source.setToken(token)
	if _, err := resolver.Revalidate(t.Context(), testTrustRepo); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 {
		t.Fatalf("renewed token lease used %d bot requests, want 2", requests.Load())
	}
	token.InstallationID++
	source.setToken(token)
	if _, err := resolver.Revalidate(t.Context(), testTrustRepo); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 3 {
		t.Fatalf("changed installation used %d bot requests, want 3", requests.Load())
	}
	now = token.ExpiresAt.Add(-2 * time.Minute)
	if _, err := resolver.Resolve(t.Context(), testTrustRepo); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 4 {
		t.Fatalf("expired token lease used %d bot requests, want 4", requests.Load())
	}
}

func TestGitHubAppBotIdentityResolverCoalescesColdCallers(t *testing.T) {
	t.Parallel()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"login":"freeside-publish[bot]","id":308829240,"type":"Bot"}`))
	}))
	t.Cleanup(server.Close)
	source := &appBotIdentityTokenSource{token: appBotIdentityToken()}
	resolver, err := publish.NewGitHubAppBotIdentityResolver(
		source, appBotIdentityKeystore(t), server.Client(), server.URL,
		func() time.Time { return appBotIdentityNow },
	)
	if err != nil {
		t.Fatal(err)
	}
	const callers = 8
	errCh := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := resolver.Resolve(t.Context(), testTrustRepo)
			errCh <- err
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("concurrent cold callers made %d bot requests, want 1", requests.Load())
	}
}

func TestGitHubAppBotIdentityResolverRejectsUnboundIdentity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
	}{
		{"wrong login", `{"login":"another-app[bot]","id":308829240,"type":"Bot"}`},
		{"wrong type", `{"login":"freeside-publish[bot]","id":308829240,"type":"User"}`},
		{"missing id", `{"login":"freeside-publish[bot]","id":0,"type":"Bot"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(server.Close)
			resolver, err := publish.NewGitHubAppBotIdentityResolver(
				fixedTokenSource{token: appBotIdentityToken()},
				appBotIdentityKeystore(t), server.Client(), server.URL, func() time.Time { return appBotIdentityNow },
			)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if _, err := resolver.Resolve(t.Context(), testTrustRepo); !errors.Is(
					err, publish.ErrAppBotIdentityMismatch,
				) {
					t.Fatalf("Resolve error = %v, want ErrAppBotIdentityMismatch", err)
				}
			}
			if requests.Load() != 2 {
				t.Fatalf("rejected identity made %d requests, want 2", requests.Load())
			}
		})
	}
}

func TestGitHubAppBotIdentityResolverRejectsTokenForUnknownRegistration(t *testing.T) {
	t.Parallel()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests++
	}))
	t.Cleanup(server.Close)
	token := appBotIdentityToken()
	token.RegistrationID++
	resolver, err := publish.NewGitHubAppBotIdentityResolver(
		fixedTokenSource{token: token}, appBotIdentityKeystore(t), server.Client(), server.URL,
		func() time.Time { return appBotIdentityNow },
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Resolve(t.Context(), testTrustRepo); !errors.Is(
		err, publish.ErrAppBotIdentityMismatch,
	) {
		t.Fatalf("Resolve error = %v, want ErrAppBotIdentityMismatch", err)
	}
	if requests != 0 {
		t.Fatalf("unknown registration reached GitHub %d time(s)", requests)
	}
}
