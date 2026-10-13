package publish_test

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

// The forge records credential effects in order and uses the same injected
// clock as the janitor, so expiry never depends on waiting in real time.
type heldTokenForge struct {
	mu                       sync.Mutex
	now                      time.Time
	mintStatus, revokeStatus int
	effects                  []string
}

func (f *heldTokenForge) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *heldTokenForge) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

func (f *heldTokenForge) takeEffects() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	effects := strings.Join(f.effects, ",")
	f.effects = nil
	return effects
}

func (f *heldTokenForge) serve(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
		body := operatorInstallations
		if requestingRegistration(t, r) == 601 {
			body = partnerInstallations
		}
		_, _ = io.WriteString(w, body)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
		installation := "701"
		if strings.Contains(r.URL.Path, "/801/") {
			installation = "801"
		}
		f.effects = append(f.effects, "mint-"+installation)
		if installation == "701" && f.mintStatus != 0 {
			w.WriteHeader(f.mintStatus)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"token":"fixture-%s","expires_at":%q,"permissions":{"metadata":"read"},"repository_selection":"selected"}`,
			installation, f.now.Add(time.Hour).Format(time.RFC3339))
	case r.Method == http.MethodDelete && r.URL.Path == "/installation/token":
		installation := strings.TrimPrefix(r.Header.Get("Authorization"), "token fixture-")
		f.effects = append(f.effects, "revoke-"+installation)
		if installation == "701" && f.revokeStatus != 0 {
			w.WriteHeader(f.revokeStatus)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		if !handleExactGrant(w, r, fixtureRepositoryID) {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}
}

func holdTestJanitor(t *testing.T, f *heldTokenForge) (*publish.InstallationJanitor, *publish.InstallationResolver) {
	t.Helper()
	ks := twoRegistrationKeystore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.serve(t, w, r) }))
	t.Cleanup(srv.Close)
	j, err := publish.NewInstallationJanitor(ks, srv.Client(), srv.URL, twoRegistrationAuthority(), &removalRecorder{}, &captureMintRecorder{}, f.clock, 4)
	if err != nil {
		t.Fatal(err)
	}
	return j, publish.NewInstallationResolverWithJanitor(ks, srv.Client(), srv.URL, f.clock, j)
}

func TestJanitorMintHoldExpiresAndResolutionRecovers(t *testing.T) {
	f := &heldTokenForge{now: fixedNow(), mintStatus: http.StatusInternalServerError}
	j, resolver := holdTestJanitor(t, f)
	err := j.RunScheduledPass(t.Context())
	var apiErr *publish.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 500 {
		t.Fatalf("first pass = %v, want mint 500", err)
	}
	if got := f.takeEffects(); got != "mint-701" {
		t.Fatalf("first pass effects = %s", got)
	}
	f.mu.Lock()
	f.mintStatus = 0
	f.mu.Unlock()
	for _, advance := range []time.Duration{0, 69*time.Minute - time.Nanosecond} {
		f.advance(advance)
		if err := j.RunScheduledPass(t.Context()); err != nil {
			t.Fatal(err)
		}
		faults := j.RegistrationFaults()
		if len(faults) != 1 || faults[0].RegistrationID != 501 || !errors.Is(faults[0].Err, publish.ErrJanitorMintHeld) {
			t.Fatalf("faults = %v, want only held registration 501", faults)
		}
		if got := f.takeEffects(); got != "mint-801,revoke-801" {
			t.Fatalf("held pass effects = %s", got)
		}
		if j.ActiveFor(501) || !j.ActiveFor(601) {
			t.Fatal("held registration covered or sibling denied")
		}
		if _, err := resolver.Resolve(t.Context(), "operator"); !errors.Is(err, publish.ErrJanitorMintHeld) {
			t.Fatalf("held resolve = %v", err)
		}
	}
	f.advance(time.Nanosecond)
	if err := j.RunScheduledPass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := f.takeEffects(); got != "mint-701,revoke-701,mint-801,revoke-801" {
		t.Fatalf("expired hold effects = %s", got)
	}
	got, err := resolver.Resolve(t.Context(), "operator")
	if err != nil || got.InstallationID != 701 || !j.ActiveFor(501) {
		t.Fatalf("recovered resolve = %+v, %v", got, err)
	}
}

func TestJanitorRevokeHoldRetriesBeforeMintAndExpires(t *testing.T) {
	for _, recoverRevoke := range []bool{true, false} {
		t.Run(fmt.Sprintf("revoke-recovers-%t", recoverRevoke), func(t *testing.T) {
			f := &heldTokenForge{now: fixedNow(), revokeStatus: http.StatusInternalServerError}
			j, _ := holdTestJanitor(t, f)
			err := j.RunScheduledPass(t.Context())
			var apiErr *publish.APIError
			if !errors.As(err, &apiErr) || apiErr.Status != 500 || strings.Contains(err.Error(), "fixture-701") {
				t.Fatalf("first pass = %v", err)
			}
			if got := f.takeEffects(); got != "mint-701,revoke-701" {
				t.Fatalf("first pass effects = %s", got)
			}
			f.advance(68 * time.Minute)
			if err := j.RunScheduledPass(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := f.takeEffects(); got != "revoke-701,mint-801,revoke-801" {
				t.Fatalf("retry effects = %s", got)
			}
			faults := j.RegistrationFaults()
			if len(faults) != 1 || !errors.As(faults[0].Err, &apiErr) || strings.Contains(faults[0].Err.Error(), "fixture-701") {
				t.Fatalf("retry faults = %v", faults)
			}
			if j.ActiveFor(501) || !j.ActiveFor(601) {
				t.Fatal("retry coverage is wrong")
			}
			if recoverRevoke {
				f.mu.Lock()
				f.revokeStatus = 0
				f.mu.Unlock()
			} else {
				f.advance(time.Minute)
			}
			err = j.RunScheduledPass(t.Context())
			if recoverRevoke {
				if err != nil || !j.ActiveFor(501) {
					t.Fatalf("recovery = %v", err)
				}
				if got := f.takeEffects(); got != "revoke-701,mint-701,revoke-701,mint-801,revoke-801" {
					t.Fatalf("recovery effects = %s", got)
				}
			} else {
				if !errors.As(err, &apiErr) {
					t.Fatalf("next minted token's revoke = %v", err)
				}
				if got := f.takeEffects(); got != "mint-701,revoke-701" {
					t.Fatalf("expiry effects = %s", got)
				}
			}
		})
	}
}
