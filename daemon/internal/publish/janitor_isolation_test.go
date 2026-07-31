package publish_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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

// errAuthorityUnavailable stands in for the state-directory authority store's
// refusal to serve a registration its operator-authored snapshot does not
// name, which is what an onboarding sequence produces before the operator
// writes the new entry.
var errAuthorityUnavailable = errors.New("authority snapshot names no such registration")

// faultyAuthoritySource denies the registrations in errs and serves the rest.
// heal drops a denial the way authoring the missing entry would.
type faultyAuthoritySource struct {
	mu      sync.Mutex
	entries installationAuthoritySource
	errs    map[int64]error
}

func (s *faultyAuthoritySource) InstallationAuthority(
	_ context.Context,
	registrationID int64,
) (publish.InstallationAuthority, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.errs[registrationID]; err != nil {
		return publish.InstallationAuthority{}, err
	}
	return s.entries[registrationID], nil
}

func (s *faultyAuthoritySource) heal(registrationID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.errs, registrationID)
}

// twoRegistrationKeystore holds two independent registrations so a failure can
// be attributed to one of them.
func twoRegistrationKeystore(t *testing.T) *publish.Keystore {
	t.Helper()
	ks := newTestKeystore(t)
	saveResolverApp(t, ks, "operator", 101, 501, publish.AppVisibilityPublic)
	saveResolverApp(t, ks, "partner", 202, 601, publish.AppVisibilityPublic)
	return ks
}

// twoRegistrationAuthority binds each registration to its own owner's single
// installation, holding exactly the repository the fake forge grants.
func twoRegistrationAuthority() installationAuthoritySource {
	return installationAuthoritySource{
		501: {
			TrustedOwners: []publish.TrustedOwner{{Login: "operator", ID: 101}},
			TrustedInstallations: []publish.TrustedInstallation{{
				RegistrationID: 501,
				InstallationID: 701,
				Account:        "operator",
				AccountID:      101,
				RepositoryIDs:  []int64{fixtureRepositoryID},
			}},
		},
		601: {
			TrustedOwners: []publish.TrustedOwner{{Login: "partner", ID: 202}},
			TrustedInstallations: []publish.TrustedInstallation{{
				RegistrationID: 601,
				InstallationID: 801,
				Account:        "partner",
				AccountID:      202,
				RepositoryIDs:  []int64{fixtureRepositoryID},
			}},
		},
	}
}

const (
	operatorInstallations = `[{"id":701,"app_id":501,"target_id":101,` +
		`"repository_selection":"selected","account":{"login":"operator","id":101}}]`
	partnerInstallations = `[{"id":801,"app_id":601,"target_id":202,` +
		`"repository_selection":"selected","account":{"login":"partner","id":202}}]`
)

// requestingRegistration reads the App ID out of the request's App JWT so one
// fake forge can answer for more than one registration. It reports 0 rather
// than failing the test, since it runs on the server's goroutine.
func requestingRegistration(t *testing.T, r *http.Request) int64 {
	t.Helper()
	parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
	if len(parts) != 3 {
		t.Errorf("authorization header is not an App JWT: %q", r.Header.Get("Authorization"))
		return 0
	}
	claims, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Errorf("decode App JWT claims: %v", err)
		return 0
	}
	var payload struct {
		Issuer string `json:"iss"`
	}
	if err := json.Unmarshal(claims, &payload); err != nil {
		t.Errorf("unmarshal App JWT claims: %v", err)
		return 0
	}
	registrationID, err := strconv.ParseInt(payload.Issuer, 10, 64)
	if err != nil {
		t.Errorf("App JWT issuer %q is not a registration ID", payload.Issuer)
		return 0
	}
	return registrationID
}

// awaitActive polls the runtime gate, which the always-on loop publishes from
// its own goroutine.
func awaitActive(t *testing.T, janitor *publish.InstallationJanitor, registrationID int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !janitor.ActiveFor(registrationID) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !janitor.ActiveFor(registrationID) {
		t.Fatalf("registration %d never became active", registrationID)
	}
}

// awaitFault polls for a published fault naming registrationID and returns it.
func awaitFault(
	t *testing.T,
	janitor *publish.InstallationJanitor,
	registrationID int64,
) publish.JanitorRegistrationFault {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, fault := range janitor.RegistrationFaults() {
			if fault.RegistrationID == registrationID {
				return fault
			}
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("registration %d never reported a fault", registrationID)
		}
		time.Sleep(time.Millisecond)
	}
}

func awaitChurn(
	t *testing.T,
	janitor *publish.InstallationJanitor,
	registrationID int64,
	consecutivePasses int,
) publish.JanitorRegistrationChurn {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, churn := range janitor.ChurningRegistrations() {
			if churn.RegistrationID == registrationID && churn.ConsecutivePasses >= consecutivePasses {
				return churn
			}
		}
		if !time.Now().Before(deadline) {
			t.Fatalf(
				"registration %d never reported %d consecutive removal passes",
				registrationID,
				consecutivePasses,
			)
		}
		time.Sleep(time.Millisecond)
	}
}

func assertRunning(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("the always-on loop stopped: %v", err)
	default:
	}
}

// awaitStop drives a pass that must stop the loop, and bounds the wait so a
// regression fails here instead of hanging the package until the test binary's
// own timeout.
func awaitStop(t *testing.T, janitor *publish.InstallationJanitor) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- janitor.Run(context.Background(), time.Hour) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the always-on loop returned success from a pass that must stop it")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the always-on loop kept running past a failure that must stop it")
	}
}

func assertShutdown(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run shutdown: %v", err)
	}
}

// TestInstallationJanitorIsolatesAnAuthorityFailure is the defect in #281: a
// registration its authority source cannot serve used to stop the loop, which
// denied every other registration until a human restarted the daemon.
func TestInstallationJanitorIsolatesAnAuthorityFailure(t *testing.T) {
	ks := twoRegistrationKeystore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/app/installations" {
			if requestingRegistration(t, r) == 501 {
				_, _ = io.WriteString(w, operatorInstallations)
				return
			}
			t.Errorf("enumerated a registration whose authority is unavailable")
			return
		}
		if !handleExactGrant(w, r, fixtureRepositoryID) {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	authority := &faultyAuthoritySource{
		entries: twoRegistrationAuthority(),
		errs:    map[int64]error{601: errAuthorityUnavailable},
	}
	janitor := newJanitor(t, ks, srv, authority, &removalRecorder{}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- janitor.Run(ctx, time.Millisecond) }()

	awaitActive(t, janitor, 501)
	if janitor.ActiveFor(601) {
		t.Fatal("a registration with no authority was covered")
	}
	if !janitor.AllowsRepository(501, 701, fixtureRepositoryID) {
		t.Fatal("the usable registration lost its mint allow-set")
	}
	faults := janitor.RegistrationFaults()
	if len(faults) != 1 || faults[0].RegistrationID != 601 {
		t.Fatalf("faults = %+v, want exactly registration 601", faults)
	}
	if !errors.Is(faults[0].Err, errAuthorityUnavailable) {
		t.Errorf("fault error = %v, want the authority source's own error", faults[0].Err)
	}
	assertRunning(t, done)

	assertShutdown(t, cancel, done)
	if len(janitor.RegistrationFaults()) != 0 {
		t.Errorf("faults survived shutdown: %+v", janitor.RegistrationFaults())
	}
}

// TestInstallationJanitorIsolatesAReconcileFailure covers the other half of
// #281: a forge failure scoped to one registration denies that registration
// only.
func TestInstallationJanitorIsolatesAReconcileFailure(t *testing.T) {
	ks := twoRegistrationKeystore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/app/installations" {
			if requestingRegistration(t, r) == 501 {
				_, _ = io.WriteString(w, operatorInstallations)
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if !handleExactGrant(w, r, fixtureRepositoryID) {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	janitor := newJanitor(t, ks, srv, twoRegistrationAuthority(), &removalRecorder{}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- janitor.Run(ctx, time.Millisecond) }()

	awaitActive(t, janitor, 501)
	fault := awaitFault(t, janitor, 601)
	if janitor.ActiveFor(601) {
		t.Fatal("a registration the forge refused to enumerate was covered")
	}
	var apiErr *publish.APIError
	if !errors.As(fault.Err, &apiErr) || apiErr.Status != http.StatusInternalServerError {
		t.Errorf("fault error = %v, want the forge's 500", fault.Err)
	}
	assertRunning(t, done)
	assertShutdown(t, cancel, done)
}

// TestInstallationJanitorRecoversWhenAnAuthorityFaultClears proves the loop
// heals on its own: authoring the missing snapshot entry restores coverage
// without a daemon restart, which is the point of not stopping the loop.
func TestInstallationJanitorRecoversWhenAnAuthorityFaultClears(t *testing.T) {
	ks := twoRegistrationKeystore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/app/installations" {
			if requestingRegistration(t, r) == 501 {
				_, _ = io.WriteString(w, operatorInstallations)
				return
			}
			_, _ = io.WriteString(w, partnerInstallations)
			return
		}
		if !handleExactGrant(w, r, fixtureRepositoryID) {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	authority := &faultyAuthoritySource{
		entries: twoRegistrationAuthority(),
		errs:    map[int64]error{601: errAuthorityUnavailable},
	}
	janitor := newJanitor(t, ks, srv, authority, &removalRecorder{}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- janitor.Run(ctx, time.Millisecond) }()

	awaitFault(t, janitor, 601)
	authority.heal(601)
	awaitActive(t, janitor, 601)
	awaitActive(t, janitor, 501)
	if faults := janitor.RegistrationFaults(); len(faults) != 0 {
		t.Errorf("faults = %+v, want none once the authority entry exists", faults)
	}
	assertShutdown(t, cancel, done)
}

// TestInstallationJanitorStopsOnAKeystoreFailure holds the other side of the
// line: a failure that names no registration cannot be attributed to one, so
// it still stops the pass with every gate shut.
func TestInstallationJanitorStopsOnAKeystoreFailure(t *testing.T) {
	ks := twoRegistrationKeystore(t)
	// A directory that could never be a registration fails enumeration closed
	// for the whole keystore (#284).
	if err := os.Mkdir(filepath.Join(ks.Dir(), "github-app", "not-an-owner"), 0o700); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	janitor := newJanitor(t, ks, srv, twoRegistrationAuthority(), &removalRecorder{}, 4)
	err := janitor.Run(context.Background(), time.Hour)
	if !errors.Is(err, publish.ErrCredentialPermissions) {
		t.Fatalf("Run = %v, want the keystore enumeration failure", err)
	}
	if janitor.ActiveFor(501) || janitor.ActiveFor(601) {
		t.Error("a keystore failure left a registration covered")
	}
	if faults := janitor.RegistrationFaults(); len(faults) != 0 {
		t.Errorf("faults = %+v, want none for a whole-keystore failure", faults)
	}
}

// TestInstallationJanitorRemovalDoesNotDenyASibling covers the silent half of
// #281: a registration that removed something withholds only its own coverage
// until a later clean pass, and used to end the pass for everyone else too.
//
// The destructive registration is deliberately the one enumeration reaches
// first (owner 101 sorts before owner 202). A sibling reached before it was
// already covered under the old code, which appended coverage and only then
// ended the pass, so only a sibling reached after it pins this change.
func TestInstallationJanitorRemovalDoesNotDenyASibling(t *testing.T) {
	ks := twoRegistrationKeystore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			if requestingRegistration(t, r) == 601 {
				_, _ = io.WriteString(w, partnerInstallations)
				return
			}
			// The unsolicited installation outlives every delete, so this
			// registration is destructive on every pass.
			_, _ = io.WriteString(w, `[{"id":701,"app_id":501,"target_id":101,`+
				`"repository_selection":"selected","account":{"login":"operator","id":101}},`+
				`{"id":702,"app_id":501,"target_id":303,`+
				`"repository_selection":"selected","account":{"login":"stranger","id":303}}]`)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/app/installations/"):
			w.WriteHeader(http.StatusNoContent)
		default:
			if !handleExactGrant(w, r, fixtureRepositoryID) {
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			}
		}
	}))
	defer srv.Close()

	recorder := &removalRecorder{}
	janitor := newJanitor(t, ks, srv, twoRegistrationAuthority(), recorder, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	// Keep each published pass observable for substantially longer than the
	// polling cadence below; equal 1 ms windows can be missed indefinitely by
	// a fast janitor loop on a contended Linux runner.
	go func() { done <- janitor.Run(ctx, 25*time.Millisecond) }()

	awaitActive(t, janitor, 601)
	churn := awaitChurn(t, janitor, 501, 2)
	if churn.ConsecutivePasses < 2 {
		t.Errorf("churn = %+v, want repeated completed removal passes", churn)
	}
	if janitor.ActiveFor(501) {
		t.Fatal("a registration whose pass removed an installation was covered")
	}
	if faults := janitor.RegistrationFaults(); len(faults) != 0 {
		t.Errorf("faults = %+v, want none: a removal is not a failure", faults)
	}
	assertRunning(t, done)
	assertShutdown(t, cancel, done)
	if churn := janitor.ChurningRegistrations(); len(churn) != 0 {
		t.Errorf("churn survived shutdown: %+v", churn)
	}
	if len(recorder.snapshot()) == 0 {
		t.Error("the sibling's removals never ran")
	}
}

// TestInstallationJanitorPreservesSkippedRemovalChurn pins the latest
// per-registration state across a pass-wide bound. Registration 601 churns in
// pass one, is skipped when 501 spends pass two's bound, then churns again in
// pass three. The skipped pass neither clears nor increments its count.
func TestInstallationJanitorPreservesSkippedRemovalChurn(t *testing.T) {
	ks := twoRegistrationKeystore(t)
	third501 := make(chan struct{})
	releaseThird501 := make(chan struct{})
	fourth501 := make(chan struct{})
	releaseFourth501 := make(chan struct{})
	var callsMu sync.Mutex
	calls := map[int64]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/app/installations" {
			registrationID := requestingRegistration(t, r)
			callsMu.Lock()
			calls[registrationID]++
			call := calls[registrationID]
			callsMu.Unlock()
			switch registrationID {
			case 501:
				switch call {
				case 1:
					_, _ = io.WriteString(w, operatorInstallations)
				case 2:
					_, _ = io.WriteString(w, `[{"id":701,"app_id":501,"target_id":101,`+
						`"repository_selection":"selected","account":{"login":"operator","id":101}},`+
						`{"id":702,"app_id":501,"target_id":303,`+
						`"repository_selection":"selected","account":{"login":"stranger","id":303}},`+
						`{"id":703,"app_id":501,"target_id":304,`+
						`"repository_selection":"selected","account":{"login":"intruder","id":304}}]`)
				case 3:
					close(third501)
					<-releaseThird501
					_, _ = io.WriteString(w, operatorInstallations)
				case 4:
					close(fourth501)
					<-releaseFourth501
					_, _ = io.WriteString(w, operatorInstallations)
				default:
					_, _ = io.WriteString(w, operatorInstallations)
				}
			case 601:
				_, _ = io.WriteString(w, `[{"id":801,"app_id":601,"target_id":202,`+
					`"repository_selection":"selected","account":{"login":"partner","id":202}},`+
					`{"id":802,"app_id":601,"target_id":303,`+
					`"repository_selection":"selected","account":{"login":"stranger","id":303}}]`)
			}
			return
		}
		if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/app/installations/") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if !handleExactGrant(w, r, fixtureRepositoryID) {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	janitor := newJanitor(t, ks, srv, twoRegistrationAuthority(), &removalRecorder{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- janitor.Run(ctx, time.Millisecond) }()

	<-third501
	churn := janitor.ChurningRegistrations()
	if len(churn) != 2 ||
		churn[0] != (publish.JanitorRegistrationChurn{RegistrationID: 501, ConsecutivePasses: 1}) ||
		churn[1] != (publish.JanitorRegistrationChurn{RegistrationID: 601, ConsecutivePasses: 1}) {
		t.Errorf("churn after skipped pass = %+v, want 501:1 and preserved 601:1", churn)
	}
	callsMu.Lock()
	if calls[601] != 1 {
		t.Errorf("registration 601 was visited %d times before pass three, want once", calls[601])
	}
	callsMu.Unlock()

	close(releaseThird501)
	<-fourth501
	churn = janitor.ChurningRegistrations()
	if len(churn) != 1 ||
		churn[0] != (publish.JanitorRegistrationChurn{RegistrationID: 601, ConsecutivePasses: 2}) {
		t.Errorf("churn after the next completed pass = %+v, want only 601:2", churn)
	}
	callsMu.Lock()
	if calls[601] != 2 {
		t.Errorf("registration 601 was visited %d times before pass four, want twice", calls[601])
	}
	callsMu.Unlock()

	cancel()
	close(releaseFourth501)
	if err := <-done; err != nil {
		t.Fatalf("Run shutdown: %v", err)
	}
}

// TestInstallationJanitorStopsOnAnAuditBarrierFailure holds the line the
// refute pass found: the journal is one shared file, so its failure belongs to
// the host, not to whichever registration's drift reached it first. Faulting
// only that registration would let every other one keep minting on top of a
// durable withdrawal barrier the daemon has just proven it cannot write.
func TestInstallationJanitorStopsOnAnAuditBarrierFailure(t *testing.T) {
	ks := twoRegistrationKeystore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/app/installations" {
			if requestingRegistration(t, r) == 601 {
				_, _ = io.WriteString(w, partnerInstallations)
				return
			}
			_, _ = io.WriteString(w, `[{"id":701,"app_id":501,"target_id":101,`+
				`"repository_selection":"selected","account":{"login":"operator","id":101}},`+
				`{"id":702,"app_id":501,"target_id":303,`+
				`"repository_selection":"selected","account":{"login":"stranger","id":303}}]`)
			return
		}
		if !handleExactGrant(w, r, fixtureRepositoryID) {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	recorder := &removalRecorder{err: errors.New("journal: no space left on device")}
	janitor := newJanitor(t, ks, srv, twoRegistrationAuthority(), recorder, 4)
	awaitStop(t, janitor)
	if janitor.ActiveFor(601) || janitor.ActiveFor(501) {
		t.Error("a registration kept coverage while the audit barrier was down")
	}
}

// TestInstallationJanitorStopsOnAnUnrevokedToken is the credential-leak half of
// the same rule. A grant-read token the janitor minted and could not revoke
// stays live for an hour; faulting the registration would mint another every
// pass, so the pass stops instead.
func TestInstallationJanitorStopsOnAnUnrevokedToken(t *testing.T) {
	ks := twoRegistrationKeystore(t)
	var mintsMu sync.Mutex
	mints := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			if requestingRegistration(t, r) == 601 {
				_, _ = io.WriteString(w, partnerInstallations)
				return
			}
			_, _ = io.WriteString(w, operatorInstallations)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			mintsMu.Lock()
			mints++
			mintsMu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"token":"`+fixtureTokenValue+
				`","permissions":{"metadata":"read"},"repository_selection":"selected"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/installation/repositories":
			_, _ = io.WriteString(w, `{"total_count":1,"repositories":[{"id":990011}]}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/installation/token":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	janitor := newJanitor(t, ks, srv, twoRegistrationAuthority(), &removalRecorder{}, 4)
	awaitStop(t, janitor)
	mintsMu.Lock()
	defer mintsMu.Unlock()
	if mints != 1 {
		t.Errorf("minted %d tokens it could not revoke, want 1", mints)
	}
}

// TestInstallationJanitorStopsOnAnUnaccountableMint enumerates the outcomes of
// the grant-read mint, which is not idempotent. A refusal proves GitHub created
// nothing and is this registration's own state; every other outcome leaves a
// token that may be live for an hour and whose value this daemon never
// learned, so retrying it every pass would accumulate credentials nothing can
// revoke.
func TestInstallationJanitorStopsOnAnUnaccountableMint(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		hangUp     bool
		wantStops  bool
		wantRemint bool
	}{
		{
			name:      "refused",
			status:    http.StatusForbidden,
			body:      `{"message":"forbidden"}`,
			wantStops: false,
		},
		{
			name:      "server error",
			status:    http.StatusInternalServerError,
			body:      `{"message":"boom"}`,
			wantStops: true,
		},
		{
			name:      "response lost",
			hangUp:    true,
			wantStops: true,
		},
		{
			name:      "created but undecodable",
			status:    http.StatusCreated,
			body:      `{"token":`,
			wantStops: true,
		},
		{
			name:      "created but no token",
			status:    http.StatusCreated,
			body:      `{"permissions":{"metadata":"read"},"repository_selection":"selected"}`,
			wantStops: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ks := publicJanitorKeystore(t)
			var mintsMu sync.Mutex
			mints := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
					_, _ = io.WriteString(w, operatorInstallations)
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
					mintsMu.Lock()
					mints++
					mintsMu.Unlock()
					if tc.hangUp {
						// The request reached the forge; the response does not
						// come back, so the outcome is unknown.
						conn, _, err := http.NewResponseController(w).Hijack()
						if err != nil {
							t.Errorf("hijack: %v", err)
							return
						}
						_ = conn.Close()
						return
					}
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.body)
				case r.Method == http.MethodDelete && r.URL.Path == "/installation/token":
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
			}))
			defer srv.Close()

			janitor := newJanitor(t, ks, srv, trustedPublicBinding(fixtureRepositoryID), &removalRecorder{}, 4)
			if tc.wantStops {
				awaitStop(t, janitor)
				mintsMu.Lock()
				defer mintsMu.Unlock()
				if mints != 1 {
					t.Errorf("minted %d unaccountable tokens, want 1", mints)
				}
				return
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- janitor.Run(ctx, time.Millisecond) }()
			awaitFault(t, janitor, 501)
			if janitor.ActiveFor(501) {
				t.Error("a registration whose mint was refused kept coverage")
			}
			assertShutdown(t, cancel, done)
		})
	}
}

// TestInstallationJanitorBoundsDestructiveAttempts keeps the operator's removal
// bound honest. It used to be spent on completed removals, which was safe only
// because a failed one ended the pass; now that the pass continues, every
// registration would otherwise get one destructive request beyond the bound.
// A suspend is already a completed, account-visible effect.
func TestInstallationJanitorBoundsDestructiveAttempts(t *testing.T) {
	ks := twoRegistrationKeystore(t)
	var effectsMu sync.Mutex
	var suspends []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			// Each registration's own trusted installation drifts its
			// repository selection, which quarantines: suspend, then delete.
			if requestingRegistration(t, r) == 601 {
				_, _ = io.WriteString(w, `[{"id":801,"app_id":601,"target_id":202,`+
					`"repository_selection":"all","account":{"login":"partner","id":202}}]`)
				return
			}
			_, _ = io.WriteString(w, `[{"id":701,"app_id":501,"target_id":101,`+
				`"repository_selection":"all","account":{"login":"operator","id":101}}]`)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/suspended"):
			effectsMu.Lock()
			suspends = append(suspends, r.URL.Path)
			effectsMu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/app/installations/"):
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	janitor := newJanitor(t, ks, srv, twoRegistrationAuthority(), &removalRecorder{}, 1)
	cycle, err := janitor.RunCycle(context.Background())
	if err == nil {
		t.Fatal("RunCycle hid the failed removal")
	}
	effectsMu.Lock()
	defer effectsMu.Unlock()
	if len(suspends) != 1 {
		t.Errorf("suspended %v under a bound of one destructive request", suspends)
	}
	if cycle.Removed != 0 {
		t.Errorf("cycle.Removed = %d, want 0 completed removals", cycle.Removed)
	}
}

// TestInstallationJanitorWithdrawsCoverageFromADuplicatedRegistration covers
// the ambiguity the refute pass found: enumeration is keyed by owner, so two
// keystore records can carry one registration ID. The record that reconciles
// must not open the gate for an ID whose sibling record could not be
// validated.
func TestInstallationJanitorWithdrawsCoverageFromADuplicatedRegistration(t *testing.T) {
	ks := newTestKeystore(t)
	saveResolverApp(t, ks, "operator", 101, 501, publish.AppVisibilityPublic)
	saveResolverApp(t, ks, "partner", 202, 501, publish.AppVisibilityPublic)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/app/installations" {
			_, _ = io.WriteString(w, operatorInstallations)
			return
		}
		if !handleExactGrant(w, r, fixtureRepositoryID) {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	janitor := newJanitor(t, ks, srv, twoRegistrationAuthority(), &removalRecorder{}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- janitor.Run(ctx, time.Millisecond) }()

	awaitFault(t, janitor, 501)
	if janitor.ActiveFor(501) {
		t.Fatal("a registration ID that faulted in the same pass kept coverage")
	}
	if janitor.AllowsRepository(501, 701, fixtureRepositoryID) {
		t.Fatal("a duplicated registration kept its mint allow-set")
	}
	assertShutdown(t, cancel, done)
}

func TestInstallationJanitorWithdrawsChurningCoverageFromADuplicatedRegistration(t *testing.T) {
	ks := newTestKeystore(t)
	saveResolverApp(t, ks, "operator", 101, 501, publish.AppVisibilityPublic)
	saveResolverApp(t, ks, "partner", 202, 501, publish.AppVisibilityPublic)
	authority := installationAuthoritySource{
		501: {
			TrustedOwners: []publish.TrustedOwner{
				{Login: "operator", ID: 101},
				{Login: "partner", ID: 202},
			},
			TrustedInstallations: []publish.TrustedInstallation{{
				RegistrationID: 501,
				InstallationID: 701,
				Account:        "operator",
				AccountID:      101,
				RepositoryIDs:  []int64{fixtureRepositoryID},
			}},
		},
	}
	var callsMu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/app/installations" {
			callsMu.Lock()
			calls++
			call := calls
			callsMu.Unlock()
			if call%2 == 1 {
				_, _ = io.WriteString(w, operatorInstallations)
				return
			}
			_, _ = io.WriteString(w, `[{"id":701,"app_id":501,"target_id":101,`+
				`"repository_selection":"selected","account":{"login":"operator","id":101}},`+
				`{"id":702,"app_id":501,"target_id":303,`+
				`"repository_selection":"selected","account":{"login":"stranger","id":303}}]`)
			return
		}
		if r.Method == http.MethodDelete && r.URL.Path == "/app/installations/702" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if !handleExactGrant(w, r, fixtureRepositoryID) {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	janitor := newJanitor(t, ks, srv, authority, &removalRecorder{}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- janitor.Run(ctx, time.Millisecond) }()

	awaitChurn(t, janitor, 501, 1)
	if janitor.ActiveFor(501) {
		t.Fatal("a registration ID that churned in the same pass kept coverage")
	}
	if janitor.AllowsRepository(501, 701, fixtureRepositoryID) {
		t.Fatal("a duplicated churning registration kept its mint allow-set")
	}
	assertShutdown(t, cancel, done)
}

func TestInstallationJanitorWithdrawsIncompleteDuplicateCoverage(t *testing.T) {
	t.Run("later duplicate is skipped", func(t *testing.T) {
		testInstallationJanitorWithdrawsIncompleteDuplicateCoverage(
			t,
			`[{"id":801,"app_id":601,"target_id":150,`+
				`"repository_selection":"selected","account":{"login":"middle","id":150}},`+
				`{"id":802,"app_id":601,"target_id":303,`+
				`"repository_selection":"selected","account":{"login":"stranger","id":303}},`+
				`{"id":803,"app_id":601,"target_id":304,`+
				`"repository_selection":"selected","account":{"login":"intruder","id":304}}]`,
			operatorInstallations,
			1,
		)
	})
	t.Run("later duplicate is reached after the last attempt", func(t *testing.T) {
		testInstallationJanitorWithdrawsIncompleteDuplicateCoverage(
			t,
			`[{"id":801,"app_id":601,"target_id":150,`+
				`"repository_selection":"selected","account":{"login":"middle","id":150}},`+
				`{"id":802,"app_id":601,"target_id":303,`+
				`"repository_selection":"selected","account":{"login":"stranger","id":303}}]`,
			`[{"id":701,"app_id":501,"target_id":101,`+
				`"repository_selection":"selected","account":{"login":"operator","id":101}},`+
				`{"id":702,"app_id":501,"target_id":303,`+
				`"repository_selection":"selected","account":{"login":"stranger","id":303}}]`,
			2,
		)
	})
}

func testInstallationJanitorWithdrawsIncompleteDuplicateCoverage(
	t *testing.T,
	middleInstallations string,
	laterDuplicateInstallations string,
	want501Calls int,
) {
	t.Helper()
	ks := newTestKeystore(t)
	saveResolverApp(t, ks, "operator", 101, 501, publish.AppVisibilityPublic)
	saveResolverApp(t, ks, "middle", 150, 601, publish.AppVisibilityPublic)
	saveResolverApp(t, ks, "partner", 202, 501, publish.AppVisibilityPublic)
	authority := installationAuthoritySource{
		501: {
			TrustedOwners: []publish.TrustedOwner{
				{Login: "operator", ID: 101},
				{Login: "partner", ID: 202},
			},
			TrustedInstallations: []publish.TrustedInstallation{{
				RegistrationID: 501,
				InstallationID: 701,
				Account:        "operator",
				AccountID:      101,
				RepositoryIDs:  []int64{fixtureRepositoryID},
			}},
		},
		601: {
			TrustedOwners: []publish.TrustedOwner{{Login: "middle", ID: 150}},
			TrustedInstallations: []publish.TrustedInstallation{{
				RegistrationID: 601,
				InstallationID: 801,
				Account:        "middle",
				AccountID:      150,
				RepositoryIDs:  []int64{fixtureRepositoryID},
			}},
		},
	}
	var callsMu sync.Mutex
	calls501 := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/app/installations" {
			switch requestingRegistration(t, r) {
			case 501:
				callsMu.Lock()
				calls501++
				call := calls501
				callsMu.Unlock()
				if call == 1 {
					_, _ = io.WriteString(w, operatorInstallations)
				} else {
					_, _ = io.WriteString(w, laterDuplicateInstallations)
				}
			case 601:
				_, _ = io.WriteString(w, middleInstallations)
			}
			return
		}
		if r.Method == http.MethodDelete && r.URL.Path == "/app/installations/802" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if !handleExactGrant(w, r, fixtureRepositoryID) {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	janitor := newJanitor(t, ks, srv, authority, &removalRecorder{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- janitor.Run(ctx, time.Hour) }()

	awaitChurn(t, janitor, 601, 1)
	if janitor.ActiveFor(501) {
		t.Fatal("the first clean owner record covered an App ID whose later duplicate was skipped")
	}
	if janitor.AllowsRepository(501, 701, fixtureRepositoryID) {
		t.Fatal("a partially visited duplicate App ID kept its mint allow-set")
	}
	churn := janitor.ChurningRegistrations()
	if len(churn) != 1 ||
		churn[0] != (publish.JanitorRegistrationChurn{RegistrationID: 601, ConsecutivePasses: 1}) {
		t.Errorf("churn = %+v, want only the bound-spending registration 601", churn)
	}
	callsMu.Lock()
	if calls501 != want501Calls {
		t.Errorf("registration 501 was reconciled %d times, want %d", calls501, want501Calls)
	}
	callsMu.Unlock()

	assertShutdown(t, cancel, done)
}

// TestInstallationJanitorFaultsOutliveTheGateTheyExplain keeps the diagnosis
// readable. Coverage is withdrawn before every pass, but clearing faults with
// it would report a registration that has failed for hours as merely unvisited
// for as long as each pass takes.
func TestInstallationJanitorFaultsOutliveTheGateTheyExplain(t *testing.T) {
	ks := twoRegistrationKeystore(t)
	blocked := make(chan struct{})
	release := make(chan struct{})
	var callsMu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/app/installations" {
			callsMu.Lock()
			calls++
			call := calls
			callsMu.Unlock()
			if call == 2 {
				close(blocked)
				<-release
			}
			_, _ = io.WriteString(w, operatorInstallations)
			return
		}
		if !handleExactGrant(w, r, fixtureRepositoryID) {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	authority := &faultyAuthoritySource{
		entries: twoRegistrationAuthority(),
		errs:    map[int64]error{601: errAuthorityUnavailable},
	}
	janitor := newJanitor(t, ks, srv, authority, &removalRecorder{}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- janitor.Run(ctx, time.Millisecond) }()

	awaitFault(t, janitor, 601)
	<-blocked
	if janitor.ActiveFor(501) {
		t.Error("coverage survived into the next pass")
	}
	awaited := make(chan bool, 1)
	go func() {
		awaited <- janitor.AwaitAllowsRepository(501, 701, fixtureRepositoryID)
	}()
	faulted := make(chan bool, 1)
	go func() {
		faulted <- janitor.AwaitAllowsRepository(601, 801, fixtureRepositoryID)
	}()
	pending := make(chan bool, 1)
	go func() {
		_, ready := janitor.AwaitPendingReady(publish.PendingInstallationEnvelope{
			ActiveEpoch: 1, DurableIntentRevision: 1, RegistrationID: 501,
			InstallationID: 701, ExpectedRepositoryIDs: []int64{fixtureRepositoryID},
		})
		pending <- ready
	}()
	select {
	case <-awaited:
		t.Fatal("coordinated onboarding gate observed transient pass withdrawal")
	case <-time.After(20 * time.Millisecond):
	}
	faults := janitor.RegistrationFaults()
	if len(faults) != 1 || faults[0].RegistrationID != 601 {
		t.Errorf("faults = %+v mid-pass, want the last pass's fault for 601", faults)
	}
	close(release)
	if allowed := <-awaited; !allowed {
		t.Fatal("coordinated onboarding gate rejected the newly completed clean pass")
	}
	if allowed := <-faulted; allowed {
		t.Fatal("coordinated onboarding gate retained coverage after a registration fault")
	}
	if ready := <-pending; ready {
		t.Fatal("coordinated pending gate invented readiness after a completed pass")
	}
	assertShutdown(t, cancel, done)
}

// TestInstallationJanitorOrdersFaultsAndReportsCancellation pins the two
// contracts a single-fault test cannot reach: faults are ordered by
// registration ID, and a canceled pass reports the cancellation rather than a
// fault for every registration it did not reach.
func TestInstallationJanitorOrdersFaultsAndReportsCancellation(t *testing.T) {
	ks := twoRegistrationKeystore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	authority := &faultyAuthoritySource{
		entries: twoRegistrationAuthority(),
		errs: map[int64]error{
			501: errAuthorityUnavailable,
			601: errAuthorityUnavailable,
		},
	}
	janitor := newJanitor(t, ks, srv, authority, &removalRecorder{}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- janitor.Run(ctx, time.Millisecond) }()

	awaitFault(t, janitor, 601)
	faults := janitor.RegistrationFaults()
	if len(faults) != 2 || faults[0].RegistrationID != 501 || faults[1].RegistrationID != 601 {
		t.Errorf("faults = %+v, want 501 then 601", faults)
	}
	assertShutdown(t, cancel, done)

	canceled, stop := context.WithCancel(context.Background())
	stop()
	if _, err := janitor.RunCycle(canceled); !errors.Is(err, context.Canceled) {
		t.Errorf("RunCycle on a canceled context = %v, want the cancellation", err)
	}
}

// TestInstallationJanitorRemovalLimitStopsThePass keeps the cycle-wide removal
// budget whole: it is spent by the pass, not by one registration, so a later
// registration cannot be reconciled within it either.
func TestInstallationJanitorRemovalLimitStopsThePass(t *testing.T) {
	ks := twoRegistrationKeystore(t)
	examined := map[int64]int{}
	var examinedMu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			registrationID := requestingRegistration(t, r)
			examinedMu.Lock()
			examined[registrationID]++
			examinedMu.Unlock()
			if registrationID == 501 {
				_, _ = io.WriteString(w, `[{"id":701,"app_id":501,"target_id":101,`+
					`"repository_selection":"selected","account":{"login":"operator","id":101}},`+
					`{"id":702,"app_id":501,"target_id":303,`+
					`"repository_selection":"selected","account":{"login":"stranger","id":303}},`+
					`{"id":703,"app_id":501,"target_id":304,`+
					`"repository_selection":"selected","account":{"login":"intruder","id":304}}]`)
				return
			}
			_, _ = io.WriteString(w, partnerInstallations)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/app/installations/"):
			w.WriteHeader(http.StatusNoContent)
		default:
			if !handleExactGrant(w, r, fixtureRepositoryID) {
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			}
		}
	}))
	defer srv.Close()

	recorder := &removalRecorder{}
	janitor := newJanitor(t, ks, srv, twoRegistrationAuthority(), recorder, 1)
	cycle, err := janitor.RunCycle(context.Background())
	if err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	if !cycle.RemovalLimitReached || cycle.Removed != 1 {
		t.Fatalf("cycle = %+v, want one removal and the bound reached", cycle)
	}
	examinedMu.Lock()
	defer examinedMu.Unlock()
	if examined[501] != 1 {
		t.Fatalf("enumeration reached registration 501 %d times, want the budget spent there first", examined[501])
	}
	if examined[601] != 0 {
		t.Errorf("the pass kept examining registrations after spending its removal budget")
	}
}

func TestInstallationJanitorFailedAttemptLimitStopsThePass(t *testing.T) {
	ks := twoRegistrationKeystore(t)
	examined := map[int64]int{}
	var examinedMu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			registrationID := requestingRegistration(t, r)
			examinedMu.Lock()
			examined[registrationID]++
			examinedMu.Unlock()
			if registrationID != 501 {
				t.Error("the pass examined a later registration after a failed attempt spent its bound")
				_, _ = io.WriteString(w, partnerInstallations)
				return
			}
			_, _ = io.WriteString(w, `[{"id":701,"app_id":501,"target_id":101,`+
				`"repository_selection":"selected","account":{"login":"operator","id":101}},`+
				`{"id":702,"app_id":501,"target_id":303,`+
				`"repository_selection":"selected","account":{"login":"stuck","id":303}},`+
				`{"id":703,"app_id":501,"target_id":304,`+
				`"repository_selection":"selected","account":{"login":"later","id":304}}]`)
		case r.Method == http.MethodDelete && r.URL.Path == "/app/installations/702":
			http.Error(w, "installation cannot be deleted", http.StatusConflict)
		default:
			if !handleExactGrant(w, r, fixtureRepositoryID) {
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			}
		}
	}))
	defer srv.Close()

	janitor := newJanitor(t, ks, srv, twoRegistrationAuthority(), &removalRecorder{}, 1)
	cycle, err := janitor.RunCycle(context.Background())
	if err == nil {
		t.Fatal("RunCycle hid the failed removal")
	}
	if !cycle.RemovalLimitReached || cycle.Removed != 0 {
		t.Errorf("cycle = %+v, want the failed attempt to spend the pass-wide bound", cycle)
	}
	examinedMu.Lock()
	defer examinedMu.Unlock()
	if examined[501] != 1 || examined[601] != 0 {
		t.Errorf("enumerations = %v, want only registration 501", examined)
	}
}
