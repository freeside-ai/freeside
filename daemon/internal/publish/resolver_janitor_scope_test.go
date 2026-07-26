package publish_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

// coveredJanitorStatus reports coverage per registration, the state a real
// always-on janitor reaches whenever one registration faults and its siblings
// keep passing (#281). Every other janitor stub in this package is uniform, so
// none of them can exercise the gate's scope.
type coveredJanitorStatus struct {
	covered map[int64]bool
}

func (s coveredJanitorStatus) ActiveFor(registrationID int64) bool {
	return s.covered[registrationID]
}

func (s coveredJanitorStatus) AllowsRepository(registrationID, _, _ int64) bool {
	return s.covered[registrationID]
}

// twoRegistrationForge answers /app/installations for both registrations of
// twoRegistrationKeystore, counting which ones were contacted so a test can
// assert what reached GitHub rather than infer it from the outcome.
func twoRegistrationForge(t *testing.T) (*publish.Keystore, *httptest.Server, func(int64) int) {
	t.Helper()
	ks := twoRegistrationKeystore(t)
	var mu sync.Mutex
	contacted := map[int64]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/app/installations" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			return
		}
		registrationID := requestingRegistration(t, r)
		mu.Lock()
		contacted[registrationID]++
		mu.Unlock()
		if registrationID == 601 {
			_, _ = io.WriteString(w, partnerInstallations)
			return
		}
		_, _ = io.WriteString(w, operatorInstallations)
	}))
	t.Cleanup(srv.Close)
	return ks, srv, func(registrationID int64) int {
		mu.Lock()
		defer mu.Unlock()
		return contacted[registrationID]
	}
}

// TestResolutionSurvivesAnInactiveNonMatchingRegistration drives #281's
// recorded consequence at its harm site: registration 601 faulted while 501
// kept passing, and the owner 501 serves could no longer mint at all. The gate
// proves cleanup ran for the registration a token is minted through, and 601
// never provides that token (#291).
func TestResolutionSurvivesAnInactiveNonMatchingRegistration(t *testing.T) {
	ks, srv, contacted := twoRegistrationForge(t)
	resolver := publish.NewInstallationResolverWithJanitor(
		ks, srv.Client(), srv.URL, fixedNow,
		coveredJanitorStatus{covered: map[int64]bool{501: true}},
	)

	got, err := resolver.Resolve(context.Background(), "operator")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.RegistrationID != 501 || got.InstallationID != 701 {
		t.Errorf("binding = %+v, want installation 701 of registration 501", got)
	}
	// The uncovered registration is still listed, deliberately: its accounts
	// are only knowable from the forge, and the match set has to be complete
	// for the ambiguity check below to be sound.
	if contacted(601) == 0 {
		t.Error("the uncovered registration was skipped, so its installations never entered the match set")
	}
}

// TestResolutionSurvivesARealJanitorFault closes the loop #281 left open. Its
// isolation tests prove the always-on loop keeps covering the healthy
// registration when a sibling's authority entry is missing, but none of them
// resolved anything, so the coverage they restored still could not mint. This
// drives the same fault through the resolver the minter uses.
func TestResolutionSurvivesARealJanitorFault(t *testing.T) {
	ks := twoRegistrationKeystore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/app/installations" {
			if requestingRegistration(t, r) == 601 {
				_, _ = io.WriteString(w, partnerInstallations)
				return
			}
			_, _ = io.WriteString(w, operatorInstallations)
			return
		}
		if !handleExactGrant(w, r, fixtureRepositoryID) {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	// The onboarding order #276 describes: the keystore record for 601 exists
	// before the operator has authored its authority-snapshot entry.
	authority := &faultyAuthoritySource{
		entries: twoRegistrationAuthority(),
		errs:    map[int64]error{601: errAuthorityUnavailable},
	}
	janitor := newJanitor(t, ks, srv, authority, &removalRecorder{}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	// One pass, then a wait long enough that coverage cannot be withdrawn for
	// the next one underneath the resolution below.
	go func() { done <- janitor.Run(ctx, time.Hour) }()

	awaitActive(t, janitor, 501)
	awaitFault(t, janitor, 601)

	resolver := publish.NewInstallationResolverWithJanitor(ks, srv.Client(), srv.URL, fixedNow, janitor)
	got, err := resolver.Resolve(context.Background(), "operator")
	if err != nil {
		t.Fatalf("Resolve while a sibling registration is faulted: %v", err)
	}
	if got.RegistrationID != 501 || got.InstallationID != 701 {
		t.Errorf("binding = %+v, want installation 701 of registration 501", got)
	}
	if _, err := resolver.Resolve(context.Background(), "partner"); !errors.Is(err, publish.ErrJanitorInactive) {
		t.Errorf("err = %v, want the faulted registration to still deny its own owner", err)
	}
	assertShutdown(t, cancel, done)
}

// TestResolutionDeniesWhenTheMatchingRegistrationIsInactive is the inverse of
// the isolation above: coverage of a sibling never substitutes for coverage of
// the registration the binding comes from.
func TestResolutionDeniesWhenTheMatchingRegistrationIsInactive(t *testing.T) {
	ks, srv, _ := twoRegistrationForge(t)
	resolver := publish.NewInstallationResolverWithJanitor(
		ks, srv.Client(), srv.URL, fixedNow,
		coveredJanitorStatus{covered: map[int64]bool{601: true}},
	)

	_, err := resolver.Resolve(context.Background(), "operator")
	if !errors.Is(err, publish.ErrJanitorInactive) {
		t.Fatalf("err = %v, want ErrJanitorInactive", err)
	}
	if !strings.Contains(err.Error(), "registration 501") {
		t.Errorf("err = %v, want it to name the denying registration 501", err)
	}
}

// TestResolutionDeniesAnAmbiguousMatchWithOneInactiveRegistration is the
// safety property the narrowing had to preserve: an owner installed under two
// registrations must never resolve to whichever one the janitor happens to
// cover. Skipping the uncovered candidate would turn this denial into a
// confident single match under 501 (keystore.go's enumeration rule, #279).
func TestResolutionDeniesAnAmbiguousMatchWithOneInactiveRegistration(t *testing.T) {
	// Both orders run: the uncovered registration is reached last in one and
	// first in the other. Only asserting the last one lets a gate that checks
	// just the final match pass, and a multi-match denial would still look
	// right, because it falls through to ErrAmbiguousInstallation.
	for _, active := range []int64{501, 502} {
		t.Run("covers-"+strconv.FormatInt(active, 10), func(t *testing.T) {
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
					_, _ = io.WriteString(w,
						`[{"id":701,"app_id":501,"target_id":202,"repository_selection":"selected","account":{"login":"freeasinbird","id":202}}]`)
				case "Bearer " + secondJWT.Reveal():
					_, _ = io.WriteString(w,
						`[{"id":702,"app_id":502,"target_id":202,"repository_selection":"selected","account":{"login":"freeasinbird","id":202}}]`)
				default:
					t.Error("unexpected registration JWT")
				}
			}))
			defer srv.Close()

			resolver := publish.NewInstallationResolverWithJanitor(
				ks, srv.Client(), srv.URL, fixedNow,
				coveredJanitorStatus{covered: map[int64]bool{active: true}},
			)

			denied := int64(502)
			if active == 502 {
				denied = 501
			}
			binding, err := resolver.Resolve(context.Background(), "freeasinbird")
			if err == nil {
				t.Fatalf("binding = %+v, want a denial: the owner matches two registrations", binding)
			}
			if !errors.Is(err, publish.ErrJanitorInactive) {
				t.Fatalf("err = %v, want ErrJanitorInactive for the uncovered matching registration", err)
			}
			if want := "registration " + strconv.FormatInt(denied, 10); !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, want it to name the uncovered matching registration %d", err, denied)
			}
		})
	}
}
