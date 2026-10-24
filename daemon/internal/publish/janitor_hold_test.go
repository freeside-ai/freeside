package publish_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// The forge records credential effects in order and uses the same injected
// clock as the janitor, so expiry never depends on waiting in real time.
type heldTokenForge struct {
	mu                       sync.Mutex
	now                      time.Time
	mintStatus, revokeStatus int
	// dropMint closes installation 701's mint connection without a
	// response, so the daemon cannot know whether GitHub created a token.
	dropMint bool
	effects  []string
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
		if installation == "701" && f.dropMint {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack mint: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
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

// holdFixture is one forge and one database across daemon restarts: each
// start builds a fresh janitor over the same store, as a restarted daemon
// would, so only the durable hold carries over.
type holdFixture struct {
	forge  *heldTokenForge
	ks     *publish.Keystore
	srv    *httptest.Server
	dbPath string
	store  *store.Store
	holds  publish.InstallationMintHoldStore
}

func newHoldFixture(t *testing.T, f *heldTokenForge) *holdFixture {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "store.db")
	st := storetest.Open(t, dbPath, store.Options{})
	recorder, err := publish.NewStoreRecorder(st)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.serve(t, w, r) }))
	t.Cleanup(srv.Close)
	return &holdFixture{
		forge: f, ks: twoRegistrationKeystore(t), srv: srv,
		dbPath: dbPath, store: st, holds: recorder,
	}
}

func (h *holdFixture) start(t *testing.T) (*publish.InstallationJanitor, *publish.InstallationResolver) {
	t.Helper()
	j, err := publish.NewInstallationJanitor(h.ks, h.srv.Client(), h.srv.URL, twoRegistrationAuthority(),
		&removalRecorder{}, &captureMintRecorder{}, h.holds, h.forge.clock, 4)
	if err != nil {
		t.Fatal(err)
	}
	return j, publish.NewInstallationResolverWithJanitor(h.ks, h.srv.Client(), h.srv.URL, h.forge.clock, j)
}

func (h *holdFixture) hold(t *testing.T) (publish.InstallationMintHold, bool) {
	t.Helper()
	hold, found, err := h.holds.InstallationMintHold(701)
	if err != nil {
		t.Fatal(err)
	}
	return hold, found
}

func (h *holdFixture) seed(t *testing.T, heldUntil time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := h.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordInstallationMintHold(ctx, store.InstallationMintHold{
			InstallationID: 701, RegistrationID: 501,
			RequestedAt: heldUntil.Add(-69 * time.Minute), HeldUntil: heldUntil,
		})
	}); err != nil {
		t.Fatal(err)
	}
}

// assertNoTokenInDatabase scans the database and its write-ahead log for the
// held token's value, so "no token column" is checked as "no token anywhere".
func (h *holdFixture) assertNoTokenInDatabase(t *testing.T) {
	t.Helper()
	for _, path := range []string{h.dbPath, h.dbPath + "-wal"} {
		body, err := os.ReadFile(path) //nolint:gosec // test-owned database
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(body, []byte("fixture-701")) {
			t.Fatalf("%s contains the held token", filepath.Base(path))
		}
	}
}

func holdTestJanitor(t *testing.T, f *heldTokenForge) (*publish.InstallationJanitor, *publish.InstallationResolver) {
	t.Helper()
	return newHoldFixture(t, f).start(t)
}

// assertHeldAfterRestart starts a fresh janitor over the fixture's database
// and proves the hold taken at fixedNow survives the restart: no 701 mint or
// revoke before the 69-minute deadline, the sibling installation covered
// throughout, then a mint, a released hold, and a resolving owner after it.
func assertHeldAfterRestart(t *testing.T, h *holdFixture) {
	t.Helper()
	j, resolver := h.start(t)
	deadline := fixedNow().Add(69 * time.Minute)
	for _, at := range []time.Time{fixedNow(), deadline.Add(-time.Nanosecond)} {
		h.forge.mu.Lock()
		h.forge.now = at
		h.forge.mu.Unlock()
		if err := j.RunScheduledPass(t.Context()); err != nil {
			t.Fatal(err)
		}
		faults := j.RegistrationFaults()
		if len(faults) != 1 || faults[0].RegistrationID != 501 || !errors.Is(faults[0].Err, publish.ErrJanitorMintHeld) {
			t.Fatalf("faults at %s = %v, want only held registration 501", at, faults)
		}
		if got := h.forge.takeEffects(); got != "mint-801,revoke-801" {
			t.Fatalf("held pass effects at %s = %s", at, got)
		}
		if j.ActiveFor(501) || !j.ActiveFor(601) {
			t.Fatal("held registration covered or sibling denied")
		}
		if _, err := resolver.Resolve(t.Context(), "operator"); !errors.Is(err, publish.ErrJanitorMintHeld) {
			t.Fatalf("held resolve = %v", err)
		}
	}
	h.assertNoTokenInDatabase(t)
	h.forge.advance(time.Nanosecond)
	if err := j.RunScheduledPass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := h.forge.takeEffects(); got != "mint-701,revoke-701,mint-801,revoke-801" {
		t.Fatalf("expired hold effects = %s", got)
	}
	if _, found := h.hold(t); found {
		t.Fatal("hold survived a proven revoke")
	}
	got, err := resolver.Resolve(t.Context(), "operator")
	if err != nil || got.InstallationID != 701 || !j.ActiveFor(501) {
		t.Fatalf("recovered resolve = %+v, %v", got, err)
	}
}

// TestJanitorMintHoldSurvivesRestart covers each crash window around the
// mint: whatever the first daemon saw, a restarted daemon refuses to mint
// until the deadline and holds no token.
func TestJanitorMintHoldSurvivesRestart(t *testing.T) {
	cases := map[string]struct {
		mintStatus, revokeStatus int
		dropMint                 bool
		effects                  string
	}{
		"mint-500":         {mintStatus: http.StatusInternalServerError, effects: "mint-701"},
		"mint-dropped":     {dropMint: true, effects: "mint-701"},
		"revoke-after-201": {revokeStatus: http.StatusInternalServerError, effects: "mint-701,revoke-701"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := &heldTokenForge{
				now: fixedNow(), mintStatus: tc.mintStatus,
				revokeStatus: tc.revokeStatus, dropMint: tc.dropMint,
			}
			h := newHoldFixture(t, f)
			first, _ := h.start(t)
			err := first.RunScheduledPass(t.Context())
			if !errors.Is(err, publish.ErrJanitorUnsafeForTest) || strings.Contains(err.Error(), "fixture-701") {
				t.Fatalf("first pass = %v, want an unsafe credential failure", err)
			}
			if got := f.takeEffects(); got != tc.effects {
				t.Fatalf("first pass effects = %s, want %s", got, tc.effects)
			}
			hold, found := h.hold(t)
			if !found || hold.RegistrationID != 501 || !hold.HeldUntil.Equal(fixedNow().Add(69*time.Minute)) {
				t.Fatalf("hold after first pass = %+v, %v", hold, found)
			}
			h.assertNoTokenInDatabase(t)
			// The forge recovers; only the durable hold stands in the way.
			f.mu.Lock()
			f.mintStatus, f.revokeStatus, f.dropMint = 0, 0, false
			f.mu.Unlock()
			assertHeldAfterRestart(t, h)
		})
	}
}

// TestJanitorSeededMintHoldIsHonored: a hold row with nothing else, as a
// crash after the hold write and before the request leaves, or after a
// proven revoke and before the release, would leave it.
func TestJanitorSeededMintHoldIsHonored(t *testing.T) {
	f := &heldTokenForge{now: fixedNow()}
	h := newHoldFixture(t, f)
	h.seed(t, fixedNow().Add(69*time.Minute))
	assertHeldAfterRestart(t, h)
}

func TestJanitorExpiredMintHoldIsReleased(t *testing.T) {
	f := &heldTokenForge{now: fixedNow()}
	h := newHoldFixture(t, f)
	h.seed(t, fixedNow().Add(-time.Nanosecond))
	j, resolver := h.start(t)
	if err := j.RunScheduledPass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := f.takeEffects(); got != "mint-701,revoke-701,mint-801,revoke-801" {
		t.Fatalf("effects = %s", got)
	}
	if _, found := h.hold(t); found {
		t.Fatal("expired hold was not released")
	}
	if got, err := resolver.Resolve(t.Context(), "operator"); err != nil || got.InstallationID != 701 {
		t.Fatalf("resolve = %+v, %v", got, err)
	}
}

// TestJanitorMintRefusalReleasesHold: a 4xx proves no token was created, so
// the registration is not held for the token lifetime.
func TestJanitorMintRefusalReleasesHold(t *testing.T) {
	f := &heldTokenForge{now: fixedNow(), mintStatus: http.StatusForbidden}
	h := newHoldFixture(t, f)
	j, _ := h.start(t)
	if err := j.RunScheduledPass(t.Context()); err != nil {
		t.Fatal(err)
	}
	faults := j.RegistrationFaults()
	var apiErr *publish.APIError
	if len(faults) != 1 || !errors.As(faults[0].Err, &apiErr) || apiErr.Status != http.StatusForbidden ||
		errors.Is(faults[0].Err, publish.ErrJanitorMintHeld) {
		t.Fatalf("faults = %v, want the 403 refusal", faults)
	}
	if _, found := h.hold(t); found {
		t.Fatal("refused mint left a hold")
	}
	f.takeEffects()
	f.mu.Lock()
	f.mintStatus = 0
	f.mu.Unlock()
	if err := j.RunScheduledPass(t.Context()); err != nil || !j.ActiveFor(501) {
		t.Fatalf("next pass = %v, active %t", err, j.ActiveFor(501))
	}
	if got := f.takeEffects(); got != "mint-701,revoke-701,mint-801,revoke-801" {
		t.Fatalf("next pass effects = %s", got)
	}
}

func memoryHoldJanitor(t *testing.T, f *heldTokenForge, holds *memoryMintHolds) *publish.InstallationJanitor {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.serve(t, w, r) }))
	t.Cleanup(srv.Close)
	j, err := publish.NewInstallationJanitor(twoRegistrationKeystore(t), srv.Client(), srv.URL,
		twoRegistrationAuthority(), &removalRecorder{}, &captureMintRecorder{}, holds, f.clock, 4)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// TestJanitorMintHoldStoreFailuresFailClosed: a hold that cannot be recorded
// or read stops the pass as the daemon's own safety failure before any mint
// request, and a hold that cannot be released stays held.
func TestJanitorMintHoldStoreFailuresFailClosed(t *testing.T) {
	storeErr := errors.New("disk full")
	for _, failure := range []string{"record", "read"} {
		t.Run(failure, func(t *testing.T) {
			f := &heldTokenForge{now: fixedNow()}
			holds := newMemoryMintHolds()
			if failure == "record" {
				holds.recordErr = storeErr
			} else {
				holds.readErr = storeErr
			}
			err := memoryHoldJanitor(t, f, holds).RunScheduledPass(t.Context())
			if !errors.Is(err, publish.ErrJanitorUnsafeForTest) || !errors.Is(err, storeErr) {
				t.Fatalf("pass = %v, want the unsafe hold-%s failure", err, failure)
			}
			if got := f.takeEffects(); strings.Contains(got, "mint-") {
				t.Fatalf("effects = %s, want no mint request", got)
			}
		})
	}
	t.Run("release", func(t *testing.T) {
		f := &heldTokenForge{now: fixedNow()}
		holds := newMemoryMintHolds()
		holds.releaseErr = storeErr
		err := memoryHoldJanitor(t, f, holds).RunScheduledPass(t.Context())
		if !errors.Is(err, publish.ErrJanitorUnsafeForTest) || !errors.Is(err, storeErr) {
			t.Fatalf("pass = %v, want the unsafe release failure", err)
		}
		if got := f.takeEffects(); got != "mint-701,revoke-701" {
			t.Fatalf("effects = %s", got)
		}
		if _, found, _ := holds.InstallationMintHold(701); !found {
			t.Fatal("a failed release dropped the hold")
		}
	})
}

// TestJanitorCanceledBeforeMintReleasesHold: a pass canceled after the hold
// write sends no request, so it must not leave the installation held.
func TestJanitorCanceledBeforeMintReleasesHold(t *testing.T) {
	f := &heldTokenForge{now: fixedNow()}
	ctx, cancel := context.WithCancel(t.Context())
	holds := &cancelOnRecordHolds{memoryMintHolds: newMemoryMintHolds(), cancel: cancel}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.serve(t, w, r) }))
	t.Cleanup(srv.Close)
	j, err := publish.NewInstallationJanitor(twoRegistrationKeystore(t), srv.Client(), srv.URL,
		twoRegistrationAuthority(), &removalRecorder{}, &captureMintRecorder{}, holds, f.clock, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.RunScheduledPass(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("pass = %v, want cancellation", err)
	}
	if got := f.takeEffects(); strings.Contains(got, "mint-") {
		t.Fatalf("effects = %s, want no mint request", got)
	}
	if _, found, _ := holds.InstallationMintHold(701); found {
		t.Fatal("a canceled pass left a hold although nothing was sent")
	}
}

// cancelOnRecordHolds cancels the pass the moment the hold commits, the
// narrowest window between the hold write and the request.
type cancelOnRecordHolds struct {
	*memoryMintHolds
	cancel context.CancelFunc
}

func (c *cancelOnRecordHolds) RecordInstallationMintHold(hold publish.InstallationMintHold) error {
	err := c.memoryMintHolds.RecordInstallationMintHold(hold)
	c.cancel()
	return err
}

// TestJanitorCanceledAtMintDispatchFinishesRequest: a shutdown that lands
// after the pre-request check but before the mint is sent must not fail the
// request unsent, which reads as an unknown outcome and holds the
// installation across the restart with no token in existence. The request
// finishes, the token is revoked, and the hold is released.
func TestJanitorCanceledAtMintDispatchFinishesRequest(t *testing.T) {
	f := &heldTokenForge{now: fixedNow()}
	ctx, cancel := context.WithCancel(t.Context())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.serve(t, w, r) }))
	t.Cleanup(srv.Close)
	client := srv.Client()
	client.Transport = cancelAtMintTransport{next: client.Transport, cancel: cancel}
	holds := newMemoryMintHolds()
	j, err := publish.NewInstallationJanitor(twoRegistrationKeystore(t), client, srv.URL,
		twoRegistrationAuthority(), &removalRecorder{}, &captureMintRecorder{}, holds, f.clock, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.RunScheduledPass(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("pass = %v, want cancellation", err)
	}
	if got := f.takeEffects(); !strings.Contains(got, "mint-701") || !strings.Contains(got, "revoke-701") {
		t.Fatalf("effects = %s, want the mint sent and its token revoked", got)
	}
	if _, found, _ := holds.InstallationMintHold(701); found {
		t.Fatal("a pass canceled at dispatch left a hold although its token was revoked")
	}
}

// cancelAtMintTransport cancels the pass as a mint request reaches the
// transport, before it is sent: the window between the janitor's last
// cancellation check and the dispatch.
type cancelAtMintTransport struct {
	next   http.RoundTripper
	cancel context.CancelFunc
}

func (c cancelAtMintTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens") {
		c.cancel()
	}
	return c.next.RoundTrip(r)
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
