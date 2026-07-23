package publish_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

func saveResolverApp(t *testing.T, ks *publish.Keystore, owner string, ownerID, appID int64, visibility publish.AppVisibility) {
	t.Helper()
	if err := ks.SaveApp(publish.AppCredentials{
		Owner:         owner,
		OwnerID:       ownerID,
		Visibility:    visibility,
		AppID:         appID,
		Name:          owner + "-app",
		Slug:          owner + "-app",
		ClientID:      "Iv1.deadbeefdeadbeef",
		Key:           fixtureKey(t),
		WebhookSecret: publish.Secret("whsec_WEBHOOKWEBHOOK"),
		ClientSecret:  publish.Secret("cs_CLIENTSECRETCLIENTSECRET"),
	}); err != nil {
		t.Fatalf("SaveApp(%s): %v", owner, err)
	}
}

type activeJanitorStatus struct{}

func (activeJanitorStatus) ActiveFor(int64) bool { return true }

func newActiveResolver(
	ks *publish.Keystore,
	client *http.Client,
	baseURL string,
) *publish.InstallationResolver {
	return publish.NewInstallationResolverWithJanitor(
		ks,
		client,
		baseURL,
		fixedNow,
		activeJanitorStatus{},
	)
}

// TestInstallationResolverPublicRegistrationMatchesOwner proves one public
// registration may carry several installations and resolution selects by the
// canonical installation account rather than by registration owner.
func TestInstallationResolverPublicRegistrationMatchesOwner(t *testing.T) {
	ks := newTestKeystore(t)
	saveResolverApp(t, ks, "operator", 101, 501, publish.AppVisibilityPublic)
	wantJWT, err := publish.AppJWT(fixtureKey(t), 501, fixtureTime)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/app/installations" ||
			r.URL.Query().Get("per_page") != "100" || r.URL.Query().Get("page") != "1" {
			t.Errorf("request = %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+wantJWT.Reveal() {
			t.Error("discovery request does not carry the registration App JWT")
		}
		_, _ = io.WriteString(w, `[
			{"id":701,"app_id":501,"target_id":101,"repository_selection":"selected","account":{"login":"operator","id":101}},
			{"id":702,"app_id":501,"target_id":202,"repository_selection":"selected","account":{"login":"freeasinbird","id":202}},
			{"id":703,"app_id":501,"target_id":303,"repository_selection":"selected","account":{"login":"another-org","id":303}}
		]`)
	}))
	defer srv.Close()

	resolver := newActiveResolver(ks, srv.Client(), srv.URL)
	got, err := resolver.Resolve(context.Background(), "FreeAsInBird")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := publish.InstallationBinding{
		RegistrationID:      501,
		RegistrationOwner:   "operator",
		RegistrationOwnerID: 101,
		InstallationID:      702,
		Account:             "freeasinbird",
		AccountID:           202,
	}
	if got != want {
		t.Errorf("binding = %+v, want %+v", got, want)
	}
}

// TestInstallationResolverUnknownOwnerFailsClosed proves the resolver never
// falls back to an unrelated installation when no account matches.
func TestInstallationResolverUnknownOwnerFailsClosed(t *testing.T) {
	ks := newTestKeystore(t)
	saveResolverApp(t, ks, "operator", 101, 501, publish.AppVisibilityPublic)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"id":701,"app_id":501,"target_id":101,"repository_selection":"selected","account":{"login":"operator","id":101}}]`)
	}))
	defer srv.Close()

	resolver := newActiveResolver(ks, srv.Client(), srv.URL)
	if _, err := resolver.Resolve(context.Background(), "unknown-owner"); !errors.Is(err, publish.ErrNoInstallation) {
		t.Fatalf("err = %v, want ErrNoInstallation", err)
	}
}

// TestInstallationResolverRejectsPrivateOwnerMismatch covers the
// returned-object boundary and its auditable, non-reflective error carrier.
func TestInstallationResolverRejectsPrivateOwnerMismatch(t *testing.T) {
	ks := newTestKeystore(t)
	saveResolverApp(t, ks, "operator", 101, 501, publish.AppVisibilityPrivate)
	const untrustedOwner = "attacker-account"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `[{"id":701,"app_id":501,"target_id":202,"repository_selection":"selected","account":{"login":%q,"id":202}}]`, untrustedOwner)
	}))
	defer srv.Close()

	resolver := newActiveResolver(ks, srv.Client(), srv.URL)
	_, err := resolver.Resolve(context.Background(), "operator")
	if !errors.Is(err, publish.ErrInstallationResolution) {
		t.Fatalf("err = %v, want ErrInstallationResolution", err)
	}
	var failure *publish.ResolutionFailure
	if !errors.As(err, &failure) {
		t.Fatalf("err = %v, want *ResolutionFailure", err)
	}
	if failure.RegistrationID != 501 || failure.ExpectedOwner != "operator" {
		t.Errorf("failure coordinates = %+v", failure)
	}
	if strings.Contains(err.Error(), untrustedOwner) {
		t.Errorf("error reflects untrusted account text: %v", err)
	}
}

// TestInstallationResolverRejectsUnknownRegistrationIdentity proves an
// installation returned under a different App ID is not adopted by the only
// locally known registration.
func TestInstallationResolverRejectsUnknownRegistrationIdentity(t *testing.T) {
	ks := newTestKeystore(t)
	saveResolverApp(t, ks, "operator", 101, 501, publish.AppVisibilityPublic)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"id":701,"app_id":999,"target_id":202,"repository_selection":"selected","account":{"login":"freeasinbird","id":202}}]`)
	}))
	defer srv.Close()

	resolver := newActiveResolver(ks, srv.Client(), srv.URL)
	if _, err := resolver.Resolve(context.Background(), "freeasinbird"); !errors.Is(err, publish.ErrInstallationResolution) {
		t.Fatalf("err = %v, want ErrInstallationResolution", err)
	}
}

// TestInstallationResolverRejectsBroadRepositorySelection proves the
// pre-token gate accepts only a selected-repositories installation. Missing,
// all-repositories, and unknown modes fail as the same closed, auditable
// returned-object class without reflecting the observed value.
func TestInstallationResolverRejectsBroadRepositorySelection(t *testing.T) {
	for _, selection := range []string{"", "all", "future-mode"} {
		t.Run(selection, func(t *testing.T) {
			ks := newTestKeystore(t)
			saveResolverApp(t, ks, "operator", 101, 501, publish.AppVisibilityPublic)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprintf(w,
					`[{"id":701,"app_id":501,"target_id":202,"repository_selection":%q,"account":{"login":"freeasinbird","id":202}}]`,
					selection)
			}))
			defer srv.Close()

			resolver := newActiveResolver(ks, srv.Client(), srv.URL)
			_, err := resolver.Resolve(context.Background(), "freeasinbird")
			var failure *publish.ResolutionFailure
			if !errors.As(err, &failure) {
				t.Fatalf("err = %v, want *ResolutionFailure", err)
			}
			if failure.Reason != publish.ResolutionRepositorySelectionMismatch {
				t.Fatalf("reason = %q, want %q", failure.Reason, publish.ResolutionRepositorySelectionMismatch)
			}
			if selection == "future-mode" && strings.Contains(err.Error(), selection) {
				t.Errorf("error reflects untrusted repository selection %q: %v", selection, err)
			}
		})
	}
}

// TestInstallationResolverRejectsAmbiguousRegistrations proves a repository
// owner installed under two locally known public registrations is never chosen
// by registration order.
func TestInstallationResolverRejectsAmbiguousRegistrations(t *testing.T) {
	ks := newTestKeystore(t)
	saveResolverApp(t, ks, "operator-one", 101, 501, publish.AppVisibilityPublic)
	saveResolverApp(t, ks, "operator-two", 102, 502, publish.AppVisibilityPublic)
	firstJWT, err := publish.AppJWT(fixtureKey(t), 501, fixtureTime)
	if err != nil {
		t.Fatal(err)
	}
	secondJWT, err := publish.AppJWT(fixtureKey(t), 502, fixtureTime)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer " + firstJWT.Reveal():
			_, _ = io.WriteString(w, `[{"id":701,"app_id":501,"target_id":202,"repository_selection":"selected","account":{"login":"freeasinbird","id":202}}]`)
		case "Bearer " + secondJWT.Reveal():
			_, _ = io.WriteString(w, `[{"id":702,"app_id":502,"target_id":202,"repository_selection":"selected","account":{"login":"freeasinbird","id":202}}]`)
		default:
			t.Error("unexpected registration JWT")
		}
	}))
	defer srv.Close()

	resolver := newActiveResolver(ks, srv.Client(), srv.URL)
	if _, err := resolver.Resolve(context.Background(), "freeasinbird"); !errors.Is(err, publish.ErrAmbiguousInstallation) {
		t.Fatalf("err = %v, want ErrAmbiguousInstallation", err)
	}
}
