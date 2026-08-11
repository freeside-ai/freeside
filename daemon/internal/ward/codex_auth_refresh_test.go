package ward

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

type codexAuthRoundTripFunc func(*http.Request) (*http.Response, error)

func (f codexAuthRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type fakeCodexAuthRefresher struct {
	calls  int
	input  string
	tokens CodexAuthRefreshTokens
	err    error
}

func (f *fakeCodexAuthRefresher) RefreshCodexAuth(
	_ context.Context, refreshToken string,
) (CodexAuthRefreshTokens, error) {
	f.calls++
	f.input = refreshToken
	return f.tokens, f.err
}

type fakeCodexAuthState struct {
	needs      bool
	checks     int
	marks      int
	markedRun  domain.RunID
	markedID   domain.AuthIdentityID
	markCtxErr error
	err        error
}

func (s *fakeCodexAuthState) NeedsCodexAuthReenrollment(
	_ context.Context, _ domain.AuthIdentityID,
) (bool, error) {
	s.checks++
	return s.needs, s.err
}

func (s *fakeCodexAuthState) MarkCodexAuthNeedsReenrollment(
	ctx context.Context, runID domain.RunID, id domain.AuthIdentityID,
) error {
	s.marks++
	s.markedRun = runID
	s.markedID = id
	s.markCtxErr = ctx.Err()
	if s.err == nil {
		s.needs = true
	}
	return s.err
}

func TestCodexAuthHTTPRefresherMatchesPinnedCodexRequest(t *testing.T) {
	const oldRefresh = "old-family-token"
	client := &http.Client{Transport: codexAuthRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost || req.URL.String() != codexAuthRefreshEndpoint {
			t.Fatalf("request = %s %s", req.Method, req.URL)
		}
		if got := req.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q", got)
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]string
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{
			"client_id": codexAuthRefreshClientID, "grant_type": "refresh_token",
			"refresh_token": oldRefresh,
		}
		if len(got) != len(want) {
			t.Fatalf("request fields = %#v", got)
		}
		for key, value := range want {
			if got[key] != value {
				t.Fatalf("request %s = %q, want %q", key, got[key], value)
			}
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body: io.NopCloser(strings.NewReader(
				`{"id_token":"new-id","access_token":"new-access","refresh_token":"new-refresh"}`,
			)),
			Header: make(http.Header),
		}, nil
	})}
	refresher := &codexAuthHTTPRefresher{client: client, endpoint: codexAuthRefreshEndpoint}

	got, err := refresher.RefreshCodexAuth(context.Background(), oldRefresh)
	if err != nil {
		t.Fatal(err)
	}
	if got.IDToken != "new-id" || got.AccessToken != "new-access" || got.RefreshToken != "new-refresh" {
		t.Fatalf("tokens = %#v", got)
	}
}

func TestNewCodexAuthHTTPRefresherIsDirectAndRejectsRedirects(t *testing.T) {
	refresher, ok := NewCodexAuthHTTPRefresher().(*codexAuthHTTPRefresher)
	if !ok {
		t.Fatal("production refresher has an unexpected implementation")
	}
	transport, ok := refresher.client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatal("production refresher may route its refresh credential through an environment proxy")
	}
	redirect, err := http.NewRequest(http.MethodPost, "https://evil.example/token", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := refresher.client.CheckRedirect(redirect, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("redirect decision = %v, want refusal", err)
	}
}

func TestCodexAuthHTTPRefresherRedactsRevokedResponse(t *testing.T) {
	const secret = "spent-refresh-secret"
	client := &http.Client{Transport: codexAuthRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Body: io.NopCloser(strings.NewReader(
				`{"error":{"code":"refresh_token_reused","message":"` + secret + `"}}`,
			)),
			Header: make(http.Header),
		}, nil
	})}
	refresher := &codexAuthHTTPRefresher{client: client, endpoint: codexAuthRefreshEndpoint}

	_, err := refresher.RefreshCodexAuth(context.Background(), secret)
	var refreshErr *CodexAuthRefreshError
	if !errors.As(err, &refreshErr) || !refreshErr.Revoked || refreshErr.Code != "refresh_token_reused" {
		t.Fatalf("Refresh error = %v, want safe revoked classification", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("refresh error disclosed the vendor response or refresh token")
	}
	forged := (&CodexAuthRefreshError{StatusCode: http.StatusBadRequest, Code: secret}).Error()
	if strings.Contains(forged, secret) {
		t.Fatal("externally constructed refresh error disclosed its untrusted code")
	}
}

func TestPrepareCodexReviewAuthRotatesUnderLease(t *testing.T) {
	lifecycle, rt, cfg, launch, _ := testCodexReviewLifecycle(t)
	setCodexHostAuth(t, cfg, launch, "old-refresh", codexReviewEpoch.Add(90*time.Minute), codexReviewEpoch)
	leaser := &fakeLeaser{rt: rt, volume: launch.AuthSnapshot}
	refresher := &fakeCodexAuthRefresher{tokens: CodexAuthRefreshTokens{
		IDToken: "new-id", AccessToken: codexReviewJWT(t, codexReviewEpoch.Add(4*time.Hour)),
		RefreshToken: "new-refresh",
	}}
	state := &fakeCodexAuthState{}
	cfg.AuthStoreLeaser = leaser
	cfg.AuthRefresher = refresher
	cfg.AuthState = state
	cfg.AccessTokenRefreshThreshold = 2 * time.Hour
	before, err := os.Stat(launch.AuthSnapshot)
	if err != nil {
		t.Fatal(err)
	}

	if err := lifecycle.prepareCodexReviewAuth(context.Background(), cfg, launch); err != nil {
		t.Fatalf("prepareCodexReviewAuth = %v", err)
	}
	if refresher.calls != 1 || refresher.input != "old-refresh" {
		t.Fatalf("refresh calls = %d with %q", refresher.calls, refresher.input)
	}
	if !leaser.released {
		t.Fatal("refresh lease was not released")
	}
	assertCallOrder(t, rt.calls,
		"identity-get "+string(launch.AuthIdentityID),
		"lease-acquire "+string(launch.AuthIdentityID),
		"lease-get "+string(launch.AuthIdentityID),
		"lease-get "+string(launch.AuthIdentityID),
		"lease-get "+string(launch.AuthIdentityID),
		"lease-get "+string(launch.AuthIdentityID),
		"lease-get "+string(launch.AuthIdentityID),
		"lease-release "+string(launch.AuthIdentityID),
	)
	after, err := os.Stat(launch.AuthSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode().Perm() != after.Mode().Perm() {
		t.Fatalf("mode changed from %s to %s", before.Mode().Perm(), after.Mode().Perm())
	}
	body, err := os.ReadFile(launch.AuthSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte("old-refresh")) || !bytes.Contains(body, []byte("new-refresh")) {
		t.Fatal("host auth store did not atomically advance to the rotated family")
	}
	snapshot, _, err := codexReviewAgentAuthSnapshot(launch.AuthMode, body)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(snapshot, []byte("new-refresh")) {
		t.Fatal("agent snapshot carries the rotated refresh token")
	}
	if state.marks != 0 {
		t.Fatalf("re-enrollment marks = %d", state.marks)
	}
}

func TestPrepareCodexReviewAuthHealthyTokenStillSerializesWithRefresh(t *testing.T) {
	lifecycle, rt, cfg, launch, _ := testCodexReviewLifecycle(t)
	leaser := &fakeLeaser{rt: rt, volume: launch.AuthSnapshot}
	refresher := &fakeCodexAuthRefresher{}
	cfg.AuthStoreLeaser = leaser
	cfg.AuthRefresher = refresher
	state := &fakeCodexAuthState{}
	cfg.AuthState = state
	cfg.AccessTokenRefreshThreshold = 90 * time.Minute

	if err := lifecycle.prepareCodexReviewAuth(context.Background(), cfg, launch); err != nil {
		t.Fatalf("first healthy prepare = %v", err)
	}
	if err := lifecycle.prepareCodexReviewAuth(context.Background(), cfg, launch); err != nil {
		t.Fatalf("second healthy prepare = %v", err)
	}
	if refresher.calls != 0 || !leaser.released {
		t.Fatalf("provider calls = %d, lease released = %t", refresher.calls, leaser.released)
	}
	if len(leaser.holders) != 2 || leaser.holders[0] == leaser.holders[1] {
		t.Fatalf("lease holders = %v, want a unique holder per transaction", leaser.holders)
	}
	assertCallOrder(t, rt.calls,
		"identity-get "+string(launch.AuthIdentityID),
		"lease-acquire "+string(launch.AuthIdentityID),
		"lease-get "+string(launch.AuthIdentityID),
		"lease-release "+string(launch.AuthIdentityID),
	)
}

func TestAcquireCodexReviewAuthExcludesConcurrentSameIdentity(t *testing.T) {
	lifecycle, _, cfg, launch, _ := testCodexReviewLifecycle(t)
	leaser := &fakeLeaser{volume: launch.AuthSnapshot}
	cfg.AuthStoreLeaser = leaser
	cfg.AuthRefresher = &fakeCodexAuthRefresher{}
	state := &fakeCodexAuthState{}
	cfg.AuthState = state

	guard, err := lifecycle.acquireCodexReviewAuth(context.Background(), cfg, launch)
	if err != nil {
		t.Fatalf("first launch admission = %v", err)
	}
	leaser.onAcquire = func(
		id domain.AuthIdentityID, holder domain.InvocationID, now, expiresAt time.Time,
	) (domain.AuthStoreMutationLease, error) {
		if leaser.lease.HeldAt(now) && leaser.lease.Holder != holder {
			return domain.AuthStoreMutationLease{}, errors.New("fixture: identity lease held")
		}
		return domain.AuthStoreMutationLease{
			AuthIdentityID: id, Holder: holder, Fence: leaser.lease.Fence + 1,
			AcquiredAt: now, ExpiresAt: expiresAt,
		}, nil
	}
	if _, err := lifecycle.acquireCodexReviewAuth(context.Background(), cfg, launch); err == nil {
		t.Fatal("second same-identity launch entered while the first held admission")
	}
	state.needs = true
	if err := verifyCodexAuthLaunchAdmission(context.Background(), cfg, launch, guard); !errors.Is(err, ErrCodexAuthNeedsReenrollment) {
		t.Fatalf("final marker admission = %v, want re-enrollment refusal", err)
	}
	if err := lifecycle.releaseCodexReviewAuthLease(context.Background(), guard); err != nil {
		t.Fatalf("release first launch admission = %v", err)
	}
}

func TestCodexReviewHoldsAuthLeaseThroughContainerStart(t *testing.T) {
	lifecycle, rt, cfg, launchSpec, _ := testCodexReviewLifecycle(t)
	leaser, ok := cfg.AuthStoreLeaser.(*fakeLeaser)
	if !ok {
		t.Fatal("test lifecycle has an unexpected auth leaser")
	}
	startedUnderLease := false
	rt.onStart = func(id string) error {
		if id == codexReviewContainerName(launchSpec.RunID) {
			if leaser.released {
				t.Fatal("Codex review container started after the auth lease was released")
			}
			startedUnderLease = true
		}
		return nil
	}
	launch, err := lifecycle.CodexReview(context.Background(), cfg, launchSpec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = launch.Close() })
	if !startedUnderLease || !leaser.released {
		t.Fatalf("started under lease = %t, released after start = %t", startedUnderLease, leaser.released)
	}
}

func TestCodexReviewReleaseFailureAfterStartReapsBeforeHandoff(t *testing.T) {
	lifecycle, rt, cfg, launchSpec, journal := testCodexReviewLifecycle(t)
	leaser, ok := cfg.AuthStoreLeaser.(*fakeLeaser)
	if !ok {
		t.Fatal("test lifecycle has an unexpected auth leaser")
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	releases := 0
	leaser.onRelease = func(
		domain.AuthIdentityID, domain.InvocationID, int64, time.Time,
	) error {
		releases++
		if releases == 1 {
			cancel()
			return errors.New("fixture: auth lease release unavailable")
		}
		leaser.released = true
		return nil
	}

	launch, err := lifecycle.CodexReview(ctx, cfg, launchSpec)
	if err == nil || launch != nil {
		t.Fatalf("CodexReview = (%#v, %v), want reaped release failure", launch, err)
	}
	if _, exists := rt.ctrs[codexReviewContainerName(launchSpec.RunID)]; exists {
		t.Fatal("release failure stranded the started Codex review container")
	}
	if journal.intent == nil || journal.intent.State != CodexReviewIntentClosed {
		t.Fatalf("intent after release failure = %#v, want closed", journal.intent)
	}
	if releases != 2 || !leaser.released {
		t.Fatalf("auth lease releases = %d, eventually released = %t", releases, leaser.released)
	}
}

func TestCodexAuthStartReservationRejectsExpiryDuringLeaseRead(t *testing.T) {
	lifecycle, _, cfg, launch, _ := testCodexReviewLifecycle(t)
	now := codexReviewEpoch
	cfg.Now = func() time.Time { return now }
	leaser := &fakeLeaser{volume: launch.AuthSnapshot}
	cfg.AuthStoreLeaser = leaser
	cfg.AuthRefresher = &fakeCodexAuthRefresher{}
	cfg.AuthState = &fakeCodexAuthState{}

	guard, err := lifecycle.acquireCodexReviewAuth(context.Background(), cfg, launch)
	if err != nil {
		t.Fatal(err)
	}
	leaser.onRenew = func(
		current domain.AuthStoreMutationLease, _ time.Time, expiresAt time.Time,
	) (domain.AuthStoreMutationLease, error) {
		current.ExpiresAt = expiresAt
		leaser.lease = current
		return current, nil
	}
	leaser.onGet = func(current domain.AuthStoreMutationLease) (domain.AuthStoreMutationLease, error) {
		now = current.ExpiresAt
		return current, nil
	}
	if _, _, err := guard.reserveStart(context.Background()); err == nil {
		t.Fatal("start reservation accepted a lease that expired during its store read")
	}
	if err := lifecycle.releaseCodexReviewAuthLease(context.Background(), guard); err != nil {
		t.Fatal(err)
	}
}

func TestCodexAuthStartReservationBindsDeadlineBeforeFinalClockRead(t *testing.T) {
	lifecycle, _, cfg, launch, _ := testCodexReviewLifecycle(t)
	now := codexReviewEpoch
	clockReads := 0
	delayFinalRead := false
	cfg.Now = func() time.Time {
		clockReads++
		if delayFinalRead && clockReads == 3 {
			time.Sleep(50 * time.Millisecond)
		}
		return now
	}
	leaser := &fakeLeaser{volume: launch.AuthSnapshot}
	cfg.AuthStoreLeaser = leaser
	cfg.AuthRefresher = &fakeCodexAuthRefresher{}
	cfg.AuthState = &fakeCodexAuthState{}
	guard, err := lifecycle.acquireCodexReviewAuth(context.Background(), cfg, launch)
	if err != nil {
		t.Fatal(err)
	}
	clockReads = 0
	delayFinalRead = true
	before := time.Now()
	startCtx, cancel, err := guard.reserveStart(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	deadline, ok := startCtx.Deadline()
	if !ok || deadline.After(before.Add(codexAuthStartTimeout+10*time.Millisecond)) {
		t.Fatalf("start deadline = %v, want it bound before the delayed clock read", deadline)
	}
	if err := lifecycle.releaseCodexReviewAuthLease(context.Background(), guard); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareCodexReviewAuthKeepsPrecallIntentFailureRetryable(t *testing.T) {
	lifecycle, _, cfg, launch, _ := testCodexReviewLifecycle(t)
	setCodexHostAuth(t, cfg, launch, "old-refresh", codexReviewEpoch.Add(30*time.Minute), codexReviewEpoch)
	refresher := &fakeCodexAuthRefresher{tokens: CodexAuthRefreshTokens{
		AccessToken:  codexReviewJWT(t, codexReviewEpoch.Add(4*time.Hour)),
		RefreshToken: "new-refresh",
	}}
	state := &fakeCodexAuthState{}
	cfg.AuthStoreLeaser = &fakeLeaser{volume: launch.AuthSnapshot}
	cfg.AuthRefresher = refresher
	cfg.AuthState = state
	cfg.AccessTokenRefreshThreshold = 2 * time.Hour
	parent := filepath.Dir(launch.AuthSnapshot)
	if err := os.Chmod(parent, 0o500); err != nil { //nolint:gosec // test removes directory write permission
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chmod(parent, 0o700); err != nil { //nolint:gosec // restore private test directory access
			t.Error(err)
		}
	}()

	err := lifecycle.prepareCodexReviewAuth(context.Background(), cfg, launch)
	if err == nil || errors.Is(err, ErrCodexAuthNeedsReenrollment) ||
		state.marks != 0 || refresher.calls != 0 {
		t.Fatalf(
			"prepare = %v, marks %d, provider calls %d; want retryable pre-call refusal",
			err, state.marks, refresher.calls,
		)
	}
	if err := os.Chmod(parent, 0o700); err != nil { //nolint:gosec // restore private test directory access
		t.Fatal(err)
	}
	if err := lifecycle.prepareCodexReviewAuth(context.Background(), cfg, launch); err != nil {
		t.Fatalf("retry after intent persistence recovery = %v", err)
	}
	if state.marks != 0 || refresher.calls != 1 {
		t.Fatalf("retry marks %d, provider calls %d; want one clean rotation", state.marks, refresher.calls)
	}
}

func TestPrepareCodexReviewAuthRecoversPendingRotationWithoutProvider(t *testing.T) {
	lifecycle, _, cfg, launch, _ := testCodexReviewLifecycle(t)
	setCodexHostAuth(t, cfg, launch, "old-refresh", codexReviewEpoch.Add(90*time.Minute), codexReviewEpoch)
	pendingBody := codexHostAuthBody(
		t, "rotated-refresh", codexReviewEpoch.Add(4*time.Hour), codexReviewEpoch.Add(time.Minute),
	)
	_, currentBody, currentMetadata, err := readCodexReviewInputWithMetadata(
		cfg.InputRoot, launch.AuthSnapshot, maxCodexAuthSnapshotBytes,
	)
	if err != nil {
		t.Fatal(err)
	}
	predecessor := newCodexAuthRefreshPredecessor(currentBody, currentMetadata)
	if err := writeCodexAuthRefreshIntent(
		launch.AuthSnapshot, launch.AuthIdentityID, predecessor, codexReviewEpoch,
	); err != nil {
		t.Fatal(err)
	}
	if err := bindCodexAuthRefreshIntent(
		cfg.InputRoot, launch.AuthSnapshot, launch.AuthIdentityID, predecessor,
		pendingBody, codexReviewEpoch.Add(time.Minute),
	); err != nil {
		t.Fatal(err)
	}
	if err := writeCodexAuthRefreshFile(
		codexAuthRefreshPendingPath(launch.AuthSnapshot, launch.AuthIdentityID),
		pendingBody, currentMetadata,
	); err != nil {
		t.Fatal(err)
	}
	refresher := &fakeCodexAuthRefresher{}
	cfg.AuthStoreLeaser = &fakeLeaser{volume: launch.AuthSnapshot}
	cfg.AuthRefresher = refresher
	cfg.AuthState = &fakeCodexAuthState{}
	cfg.AccessTokenRefreshThreshold = 2 * time.Hour

	if err := lifecycle.prepareCodexReviewAuth(context.Background(), cfg, launch); err != nil {
		t.Fatalf("prepareCodexReviewAuth = %v", err)
	}
	if refresher.calls != 0 {
		t.Fatalf("provider calls = %d, want pending rotation recovery", refresher.calls)
	}
	committed, err := os.ReadFile(launch.AuthSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(committed, pendingBody) {
		t.Fatal("pending rotated response was not committed exactly")
	}
}

func TestRecoverCodexAuthRefreshTransactionRevalidatesPendingRotation(t *testing.T) {
	predecessorAccess := codexReviewJWT(t, codexReviewEpoch.Add(90*time.Minute))
	for _, tc := range []struct {
		name               string
		pendingRefresh     string
		pendingExpiry      time.Time
		pendingLastRefresh time.Time
		bindDifferent      bool
	}{
		{
			name: "empty replacement refresh token", pendingExpiry: codexReviewEpoch.Add(4 * time.Hour),
		},
		{
			name: "predecessor refresh token", pendingRefresh: "old-refresh",
			pendingExpiry: codexReviewEpoch.Add(4 * time.Hour),
		},
		{
			name: "predecessor access token", pendingRefresh: predecessorAccess,
			pendingExpiry: codexReviewEpoch.Add(4 * time.Hour),
		},
		{
			name: "access token below original refresh threshold", pendingRefresh: "rotated-refresh",
			pendingExpiry: codexReviewEpoch.Add(90 * time.Minute),
		},
		{
			name: "forged early validation instant", pendingRefresh: "rotated-refresh",
			pendingExpiry:      codexReviewEpoch.Add(90 * time.Minute),
			pendingLastRefresh: codexReviewEpoch.Add(-time.Hour),
		},
		{
			name: "pending bytes differ from durable binding", pendingRefresh: "rotated-refresh",
			pendingExpiry: codexReviewEpoch.Add(4 * time.Hour), bindDifferent: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, cfg, launch, _ := testCodexReviewLifecycle(t)
			setCodexHostAuth(
				t, cfg, launch, "old-refresh", codexReviewEpoch.Add(90*time.Minute), codexReviewEpoch,
			)
			_, predecessorBody, predecessorMetadata, err := readCodexReviewInputWithMetadata(
				cfg.InputRoot, launch.AuthSnapshot, maxCodexAuthSnapshotBytes,
			)
			if err != nil {
				t.Fatal(err)
			}
			predecessor := newCodexAuthRefreshPredecessor(predecessorBody, predecessorMetadata)
			if err := writeCodexAuthRefreshIntent(
				launch.AuthSnapshot, launch.AuthIdentityID, predecessor, codexReviewEpoch,
			); err != nil {
				t.Fatal(err)
			}
			pending := codexAuthRefreshPendingPath(launch.AuthSnapshot, launch.AuthIdentityID)
			pendingLastRefresh := tc.pendingLastRefresh
			if pendingLastRefresh.IsZero() {
				pendingLastRefresh = codexReviewEpoch.Add(time.Minute)
			}
			pendingBody := codexHostAuthBody(
				t, tc.pendingRefresh, tc.pendingExpiry, pendingLastRefresh,
			)
			boundBody := pendingBody
			if tc.bindDifferent {
				boundBody = codexHostAuthBody(
					t, "different-refresh", tc.pendingExpiry, codexReviewEpoch.Add(time.Minute),
				)
			}
			if err := bindCodexAuthRefreshIntent(
				cfg.InputRoot, launch.AuthSnapshot, launch.AuthIdentityID, predecessor,
				boundBody, codexReviewEpoch.Add(time.Minute),
			); err != nil {
				t.Fatal(err)
			}
			if err := writeCodexAuthRefreshFile(
				pending, pendingBody, predecessorMetadata,
			); err != nil {
				t.Fatal(err)
			}

			recovered, err := recoverCodexAuthRefreshTransaction(
				cfg.InputRoot, launch.AuthSnapshot, launch.AuthIdentityID,
				predecessorBody, predecessorMetadata, 2*time.Hour,
			)
			if err == nil || recovered {
				t.Fatalf("recover = %t, %v; want unsafe pending rotation refusal", recovered, err)
			}
			committed, readErr := os.ReadFile(launch.AuthSnapshot)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(committed, predecessorBody) {
				t.Fatal("unsafe pending rotation crossed the host-store trust boundary")
			}
		})
	}
}

func TestRecoverCodexAuthRefreshTransactionPreservesChangedPredecessor(t *testing.T) {
	_, _, cfg, launch, _ := testCodexReviewLifecycle(t)
	setCodexHostAuth(t, cfg, launch, "predecessor-refresh", codexReviewEpoch.Add(90*time.Minute), codexReviewEpoch)
	_, predecessorBody, predecessorMetadata, err := readCodexReviewInputWithMetadata(
		cfg.InputRoot, launch.AuthSnapshot, maxCodexAuthSnapshotBytes,
	)
	if err != nil {
		t.Fatal(err)
	}
	predecessor := newCodexAuthRefreshPredecessor(predecessorBody, predecessorMetadata)
	if err := writeCodexAuthRefreshIntent(
		launch.AuthSnapshot, launch.AuthIdentityID, predecessor, codexReviewEpoch,
	); err != nil {
		t.Fatal(err)
	}
	pendingPath := codexAuthRefreshPendingPath(launch.AuthSnapshot, launch.AuthIdentityID)
	if err := writeCodexAuthRefreshFile(
		pendingPath,
		codexHostAuthBody(t, "pending-refresh", codexReviewEpoch.Add(4*time.Hour), codexReviewEpoch.Add(time.Minute)),
		predecessorMetadata,
	); err != nil {
		t.Fatal(err)
	}
	external := codexHostAuthBody(
		t, "operator-reenrolled", codexReviewEpoch.Add(5*time.Hour), codexReviewEpoch.Add(2*time.Minute),
	)
	if err := os.WriteFile(launch.AuthSnapshot, external, 0o600); err != nil {
		t.Fatal(err)
	}

	_, externalBody, externalMetadata, err := readCodexReviewInputWithMetadata(
		cfg.InputRoot, launch.AuthSnapshot, maxCodexAuthSnapshotBytes,
	)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := recoverCodexAuthRefreshTransaction(
		cfg.InputRoot, launch.AuthSnapshot, launch.AuthIdentityID, externalBody, externalMetadata,
		2*time.Hour,
	)
	if err != nil || recovered {
		t.Fatalf("recover = %t, %v; want external replacement preserved", recovered, err)
	}
	committed, err := os.ReadFile(launch.AuthSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(committed, external) {
		t.Fatal("pending rotation overwrote the changed predecessor")
	}
	for _, path := range []string{
		pendingPath, codexAuthRefreshIntentPath(launch.AuthSnapshot, launch.AuthIdentityID),
	} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("transaction sidecar %q remains: %v", path, err)
		}
	}
}

func TestRecoverCodexAuthRefreshTransactionRefusesAmbiguousCall(t *testing.T) {
	_, _, cfg, launch, _ := testCodexReviewLifecycle(t)
	setCodexHostAuth(t, cfg, launch, "predecessor-refresh", codexReviewEpoch.Add(90*time.Minute), codexReviewEpoch)
	_, predecessorBody, predecessorMetadata, err := readCodexReviewInputWithMetadata(
		cfg.InputRoot, launch.AuthSnapshot, maxCodexAuthSnapshotBytes,
	)
	if err != nil {
		t.Fatal(err)
	}
	predecessor := newCodexAuthRefreshPredecessor(predecessorBody, predecessorMetadata)
	if err := writeCodexAuthRefreshIntent(
		launch.AuthSnapshot, launch.AuthIdentityID, predecessor, codexReviewEpoch,
	); err != nil {
		t.Fatal(err)
	}

	if _, err := recoverCodexAuthRefreshTransaction(
		cfg.InputRoot, launch.AuthSnapshot, launch.AuthIdentityID,
		predecessorBody, predecessorMetadata, 2*time.Hour,
	); err == nil {
		t.Fatal("ambiguous provider call was retried against its predecessor")
	}
}

func TestRecoverCodexAuthRefreshTransactionDiscardsPartialPendingAfterReenrollment(t *testing.T) {
	_, _, cfg, launch, _ := testCodexReviewLifecycle(t)
	setCodexHostAuth(t, cfg, launch, "predecessor-refresh", codexReviewEpoch.Add(90*time.Minute), codexReviewEpoch)
	_, predecessorBody, predecessorMetadata, err := readCodexReviewInputWithMetadata(
		cfg.InputRoot, launch.AuthSnapshot, maxCodexAuthSnapshotBytes,
	)
	if err != nil {
		t.Fatal(err)
	}
	predecessor := newCodexAuthRefreshPredecessor(predecessorBody, predecessorMetadata)
	if err := writeCodexAuthRefreshIntent(
		launch.AuthSnapshot, launch.AuthIdentityID, predecessor, codexReviewEpoch,
	); err != nil {
		t.Fatal(err)
	}
	partial := codexAuthRefreshPendingPath(launch.AuthSnapshot, launch.AuthIdentityID)
	if err := writeCodexAuthRefreshFile(partial, []byte(`{"tokens":`), predecessorMetadata); err != nil {
		t.Fatal(err)
	}
	external := codexHostAuthBody(
		t, "operator-reenrolled", codexReviewEpoch.Add(5*time.Hour), codexReviewEpoch.Add(time.Minute),
	)
	if err := os.WriteFile(launch.AuthSnapshot, external, 0o600); err != nil {
		t.Fatal(err)
	}
	_, externalBody, externalMetadata, err := readCodexReviewInputWithMetadata(
		cfg.InputRoot, launch.AuthSnapshot, maxCodexAuthSnapshotBytes,
	)
	if err != nil {
		t.Fatal(err)
	}
	if recovered, err := recoverCodexAuthRefreshTransaction(
		cfg.InputRoot, launch.AuthSnapshot, launch.AuthIdentityID, externalBody, externalMetadata,
		2*time.Hour,
	); err != nil || recovered {
		t.Fatalf("partial pending recovery = %t, %v", recovered, err)
	}
	if _, err := os.Stat(partial); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial pending remains: %v", err)
	}
}

func TestRecoverCodexAuthRefreshTransactionIgnoresPartialIntentStage(t *testing.T) {
	_, _, cfg, launch, _ := testCodexReviewLifecycle(t)
	_, body, metadata, err := readCodexReviewInputWithMetadata(
		cfg.InputRoot, launch.AuthSnapshot, maxCodexAuthSnapshotBytes,
	)
	if err != nil {
		t.Fatal(err)
	}
	stage := codexAuthRefreshIntentPath(launch.AuthSnapshot, launch.AuthIdentityID) + ".stage-crash"
	if err := writeCodexAuthRefreshFile(stage, []byte(`{"version":`), metadata); err != nil {
		t.Fatal(err)
	}
	if recovered, err := recoverCodexAuthRefreshTransaction(
		cfg.InputRoot, launch.AuthSnapshot, launch.AuthIdentityID, body, metadata, 2*time.Hour,
	); err != nil || recovered {
		t.Fatalf("partial intent stage recovery = %t, %v", recovered, err)
	}
	predecessor := newCodexAuthRefreshPredecessor(body, metadata)
	if err := writeCodexAuthRefreshIntent(
		launch.AuthSnapshot, launch.AuthIdentityID, predecessor, codexReviewEpoch,
	); err != nil {
		t.Fatalf("write intent after partial stage = %v", err)
	}
	if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale intent stage remains after retry: %v", err)
	}
}

func TestWriteCodexAuthRefreshIntentDoesNotReplaceExistingIntent(t *testing.T) {
	_, _, cfg, launch, _ := testCodexReviewLifecycle(t)
	_, body, metadata, err := readCodexReviewInputWithMetadata(
		cfg.InputRoot, launch.AuthSnapshot, maxCodexAuthSnapshotBytes,
	)
	if err != nil {
		t.Fatal(err)
	}
	predecessor := newCodexAuthRefreshPredecessor(body, metadata)
	if err := writeCodexAuthRefreshIntent(
		launch.AuthSnapshot, launch.AuthIdentityID, predecessor, codexReviewEpoch,
	); err != nil {
		t.Fatal(err)
	}
	intentPath := codexAuthRefreshIntentPath(launch.AuthSnapshot, launch.AuthIdentityID)
	before, err := os.ReadFile(intentPath) //nolint:gosec // test path derives from a hardened temporary auth store
	if err != nil {
		t.Fatal(err)
	}
	err = writeCodexAuthRefreshIntent(
		launch.AuthSnapshot, launch.AuthIdentityID, predecessor, codexReviewEpoch.Add(time.Minute),
	)
	if !errors.Is(err, errCodexAuthRefreshIntentExists) {
		t.Fatalf("second intent = %v, want atomic no-replace refusal", err)
	}
	after, err := os.ReadFile(intentPath) //nolint:gosec // test path derives from a hardened temporary auth store
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("second intent replaced the first ambiguous transaction")
	}
}

func TestPrepareCodexReviewAuthRevocationMarksAndRefusesFast(t *testing.T) {
	lifecycle, _, cfg, launch, _ := testCodexReviewLifecycle(t)
	setCodexHostAuth(t, cfg, launch, "spent-refresh", codexReviewEpoch.Add(30*time.Minute), codexReviewEpoch)
	refresher := &fakeCodexAuthRefresher{err: &CodexAuthRefreshError{
		StatusCode: http.StatusBadRequest, Code: "refresh_token_reused", Revoked: true,
	}}
	state := &fakeCodexAuthState{}
	leaser := &fakeLeaser{volume: launch.AuthSnapshot}
	cfg.AuthStoreLeaser = leaser
	cfg.AuthRefresher = refresher
	cfg.AuthState = state
	cfg.AccessTokenRefreshThreshold = 2 * time.Hour

	err := lifecycle.prepareCodexReviewAuth(context.Background(), cfg, launch)
	if !errors.Is(err, ErrCodexAuthNeedsReenrollment) {
		t.Fatalf("first prepare = %v, want re-enrollment refusal", err)
	}
	if state.marks != 1 || state.markedRun != launch.WorkflowRunID || state.markedID != launch.AuthIdentityID {
		t.Fatalf("mark = count %d, run %q, identity %q", state.marks, state.markedRun, state.markedID)
	}
	if refresher.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", refresher.calls)
	}

	err = lifecycle.prepareCodexReviewAuth(context.Background(), cfg, launch)
	if !errors.Is(err, ErrCodexAuthNeedsReenrollment) {
		t.Fatalf("second prepare = %v, want fast re-enrollment refusal", err)
	}
	if refresher.calls != 1 {
		t.Fatalf("provider calls = %d after durable marker, want 1", refresher.calls)
	}
	if state.marks != 1 {
		t.Fatalf("re-enrollment marks = %d, want 1", state.marks)
	}
}

func TestPrepareCodexReviewAuthPersistsReenrollmentAfterCancellation(t *testing.T) {
	lifecycle, _, cfg, launch, _ := testCodexReviewLifecycle(t)
	setCodexHostAuth(t, cfg, launch, "spent-refresh", codexReviewEpoch.Add(30*time.Minute), codexReviewEpoch)
	cfg.AuthStoreLeaser = &fakeLeaser{volume: launch.AuthSnapshot}
	cfg.AuthRefresher = &fakeCodexAuthRefresher{err: &CodexAuthRefreshError{
		StatusCode: http.StatusBadRequest, Code: "spent-refresh", Revoked: true,
	}}
	state := &fakeCodexAuthState{}
	cfg.AuthState = state
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := lifecycle.prepareCodexReviewAuth(ctx, cfg, launch)
	if !errors.Is(err, ErrCodexAuthNeedsReenrollment) || state.marks != 1 || state.markCtxErr != nil {
		t.Fatalf("prepare = %v, marks %d, marker context %v", err, state.marks, state.markCtxErr)
	}
	if strings.Contains(err.Error(), "spent-refresh") {
		t.Fatal("externally supplied refresh error disclosed the credential")
	}
}

func TestCodexReviewReenrollmentMarkerRefusesBeforeInputReads(t *testing.T) {
	lifecycle, _, cfg, launch, _ := testCodexReviewLifecycle(t)
	cfg.AuthState = &fakeCodexAuthState{needs: true}
	if err := os.Remove(launch.AuthSnapshot); err != nil {
		t.Fatal(err)
	}

	_, err := lifecycle.codexReview(context.Background(), cfg, launch)
	if !errors.Is(err, ErrCodexAuthNeedsReenrollment) {
		t.Fatalf("codexReview = %v, want identity-scoped fast refusal", err)
	}
}

func TestCodexReviewOutcomeFencePrecedesAuthRefresh(t *testing.T) {
	lifecycle, _, cfg, launch, journal := testCodexReviewLifecycle(t)
	setCodexHostAuth(t, cfg, launch, "old-refresh", codexReviewEpoch.Add(30*time.Minute), codexReviewEpoch)
	refresher := &fakeCodexAuthRefresher{tokens: CodexAuthRefreshTokens{
		AccessToken:  codexReviewJWT(t, codexReviewEpoch.Add(4*time.Hour)),
		RefreshToken: "new-refresh",
	}}
	leaser := &fakeLeaser{volume: launch.AuthSnapshot}
	cfg.AuthStoreLeaser = leaser
	cfg.AuthRefresher = refresher
	journal.outcomes = map[string]CodexReviewSourceOutcome{launch.RunID: {
		InvocationID: domain.InvocationID(launch.RunID),
		FailureClass: domain.ReviewFailureTransient, Failure: "already terminal",
	}}

	if _, err := lifecycle.codexReview(context.Background(), cfg, launch); err == nil {
		t.Fatal("terminal outcome allowed a second launch")
	}
	if refresher.calls != 0 || len(leaser.holders) != 0 {
		t.Fatalf("outcome fence followed auth mutation: provider %d, lease holders %v", refresher.calls, leaser.holders)
	}
}

func TestPrepareCodexReviewAuthLeaseRefusalDoesNotCallProvider(t *testing.T) {
	lifecycle, _, cfg, launch, _ := testCodexReviewLifecycle(t)
	setCodexHostAuth(t, cfg, launch, "old-refresh", codexReviewEpoch.Add(30*time.Minute), codexReviewEpoch)
	refresher := &fakeCodexAuthRefresher{}
	leaser := &fakeLeaser{volume: launch.AuthSnapshot, onAcquire: func(
		domain.AuthIdentityID, domain.InvocationID, time.Time, time.Time,
	) (domain.AuthStoreMutationLease, error) {
		return domain.AuthStoreMutationLease{}, errors.New("lease held elsewhere")
	}}
	cfg.AuthStoreLeaser = leaser
	cfg.AuthRefresher = refresher
	cfg.AuthState = &fakeCodexAuthState{}
	cfg.AccessTokenRefreshThreshold = 2 * time.Hour

	err := lifecycle.prepareCodexReviewAuth(context.Background(), cfg, launch)
	wantCheckFailure(t, err, CheckAuthStoreMutationLease)
	if refresher.calls != 0 {
		t.Fatalf("provider calls = %d, want none without the lease", refresher.calls)
	}
}

func TestPrepareCodexReviewAuthRejectsNonOpenAIIdentity(t *testing.T) {
	lifecycle, _, cfg, launch, _ := testCodexReviewLifecycle(t)
	setCodexHostAuth(t, cfg, launch, "old-refresh", codexReviewEpoch.Add(30*time.Minute), codexReviewEpoch)
	refresher := &fakeCodexAuthRefresher{}
	leaser := &fakeLeaser{identity: domain.AuthIdentity{
		ID: launch.AuthIdentityID, Provider: "claude", AuthStoreMutationLease: true,
		AuthStoreVolume: launch.AuthSnapshot, MaxParallelExecutions: 1,
		RefreshStrategy: domain.RefreshOnDemand, SupportsReadOnlyAuthSnapshot: true,
	}}
	cfg.AuthStoreLeaser = leaser
	cfg.AuthRefresher = refresher
	cfg.AuthState = &fakeCodexAuthState{}
	cfg.AccessTokenRefreshThreshold = 2 * time.Hour

	err := lifecycle.prepareCodexReviewAuth(context.Background(), cfg, launch)
	if !errors.Is(err, ErrInvalidCodexReviewSpec) {
		t.Fatalf("prepare = %v, want provider-binding refusal", err)
	}
	if refresher.calls != 0 {
		t.Fatalf("provider calls = %d, want none for a non-OpenAI identity", refresher.calls)
	}
}

func TestPrepareCodexReviewAuthRejectsIdentityBoundToAnotherStore(t *testing.T) {
	lifecycle, _, cfg, launch, _ := testCodexReviewLifecycle(t)
	setCodexHostAuth(t, cfg, launch, "old-refresh", codexReviewEpoch.Add(30*time.Minute), codexReviewEpoch)
	refresher := &fakeCodexAuthRefresher{}
	cfg.AuthStoreLeaser = &fakeLeaser{volume: filepath.Join(cfg.InputRoot, "another-auth.json")}
	cfg.AuthRefresher = refresher
	cfg.AuthState = &fakeCodexAuthState{}

	err := lifecycle.prepareCodexReviewAuth(context.Background(), cfg, launch)
	if !errors.Is(err, ErrInvalidCodexReviewSpec) {
		t.Fatalf("prepare = %v, want identity/store binding refusal", err)
	}
	if refresher.calls != 0 {
		t.Fatalf("provider calls = %d, want none for a mismatched store binding", refresher.calls)
	}
}

func TestCodexReviewSubscriptionRequiresHostRefreshDependencies(t *testing.T) {
	_, _, cfg, launch, _ := testCodexReviewLifecycle(t)
	for _, tc := range []struct {
		name   string
		remove func(*CodexReviewConfig)
	}{
		{"leaser", func(cfg *CodexReviewConfig) { cfg.AuthStoreLeaser = nil }},
		{"refresher", func(cfg *CodexReviewConfig) { cfg.AuthRefresher = nil }},
		{"state", func(cfg *CodexReviewConfig) { cfg.AuthState = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			broken := cfg
			tc.remove(&broken)
			if err := validateCodexReviewLaunchStructure(broken, launch); !errors.Is(err, ErrInvalidCodexReviewSpec) {
				t.Fatalf("validation = %v, want configuration refusal", err)
			}
		})
	}
}

func TestPrepareCodexReviewAuthRejectsInvalidReturnedTokensBeforeMutation(t *testing.T) {
	aliasedNew := codexReviewJWT(t, codexReviewEpoch.Add(4*time.Hour))
	aliasedOld := codexReviewJWT(t, codexReviewEpoch.Add(5*time.Hour))
	predecessorAccess := codexReviewJWT(t, codexReviewEpoch.Add(30*time.Minute))
	predecessorID := strings.Join([]string{"fixture", "id", "token"}, "-")
	for _, tc := range []struct {
		name       string
		oldRefresh string
		tokens     CodexAuthRefreshTokens
	}{
		{
			name: "malformed access token",
			tokens: CodexAuthRefreshTokens{
				AccessToken: "not-a-jwt", RefreshToken: "untrusted-rotation",
			},
		},
		{
			name: "refresh token aliased into access token",
			tokens: CodexAuthRefreshTokens{
				AccessToken: aliasedNew, RefreshToken: aliasedNew,
			},
		},
		{
			name: "prior refresh token aliased into access token", oldRefresh: aliasedOld,
			tokens: CodexAuthRefreshTokens{ //nolint:gosec // adversarially aliases the prior refresh credential
				AccessToken:  aliasedOld,
				RefreshToken: "untrusted-new-rotation",
			},
		},
		{
			name: "refresh token was not rotated", oldRefresh: aliasedNew,
			tokens: CodexAuthRefreshTokens{
				AccessToken:  aliasedOld,
				RefreshToken: aliasedNew,
			},
		},
		{
			name: "replacement refresh token is empty",
			tokens: CodexAuthRefreshTokens{
				AccessToken: aliasedNew,
			},
		},
		{
			name: "replacement refresh token was a predecessor access token",
			tokens: CodexAuthRefreshTokens{
				AccessToken: aliasedNew, RefreshToken: predecessorAccess,
			},
		},
		{
			name: "replacement refresh token was an overwritten predecessor ID token",
			tokens: CodexAuthRefreshTokens{
				IDToken: "replacement-id", AccessToken: aliasedNew, RefreshToken: predecessorID,
			},
		},
		{
			name: "returned access token is below the launch floor",
			tokens: CodexAuthRefreshTokens{
				AccessToken:  codexReviewJWT(t, codexReviewEpoch.Add(30*time.Minute)),
				RefreshToken: "untrusted-below-floor-rotation",
			},
		},
		{
			name: "returned access token is below the refresh threshold",
			tokens: CodexAuthRefreshTokens{
				AccessToken:  codexReviewJWT(t, codexReviewEpoch.Add(90*time.Minute)),
				RefreshToken: "untrusted-below-threshold-rotation",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lifecycle, _, cfg, launch, _ := testCodexReviewLifecycle(t)
			oldRefresh := tc.oldRefresh
			if oldRefresh == "" {
				oldRefresh = "old-refresh"
			}
			setCodexHostAuth(t, cfg, launch, oldRefresh, codexReviewEpoch.Add(30*time.Minute), codexReviewEpoch)
			before, readErr := os.ReadFile(launch.AuthSnapshot)
			if readErr != nil {
				t.Fatal(readErr)
			}
			refresher := &fakeCodexAuthRefresher{tokens: tc.tokens}
			state := &fakeCodexAuthState{}
			cfg.AuthStoreLeaser = &fakeLeaser{volume: launch.AuthSnapshot}
			cfg.AuthRefresher = refresher
			cfg.AuthState = state
			cfg.AccessTokenRefreshThreshold = 2 * time.Hour

			err := lifecycle.prepareCodexReviewAuth(context.Background(), cfg, launch)
			if !errors.Is(err, ErrCodexAuthNeedsReenrollment) || state.marks != 1 {
				t.Fatalf("prepare = %v, marks %d; want re-enrollment", err, state.marks)
			}
			committed, readErr := os.ReadFile(launch.AuthSnapshot)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(committed, before) {
				t.Fatal("invalid returned token set crossed the host-store trust boundary")
			}
			if _, statErr := os.Stat(codexAuthRefreshIntentPath(
				launch.AuthSnapshot, launch.AuthIdentityID,
			)); statErr != nil {
				t.Fatalf("refresh intent = %v, want durable ambiguous-call record", statErr)
			}
			if _, statErr := os.Stat(codexAuthRefreshPendingPath(
				launch.AuthSnapshot, launch.AuthIdentityID,
			)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("pending rotation = %v, want none for rejected tokens", statErr)
			}
			secondErr := lifecycle.prepareCodexReviewAuth(context.Background(), cfg, launch)
			if !errors.Is(secondErr, ErrCodexAuthNeedsReenrollment) || refresher.calls != 1 {
				t.Fatalf(
					"second prepare = %v, provider calls %d; want fast refusal",
					secondErr, refresher.calls,
				)
			}
		})
	}
}

func codexHostAuthBody(
	t *testing.T, refresh string, expires, lastRefresh time.Time,
) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"OPENAI_API_KEY": nil,
		"tokens": map[string]any{
			"id_token": "fixture-id-token", "access_token": codexReviewJWT(t, expires),
			"refresh_token": refresh,
		},
		"last_refresh": lastRefresh.Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func setCodexHostAuth(
	t *testing.T, cfg CodexReviewConfig, launch CodexReviewLaunchSpec,
	refresh string, expires, lastRefresh time.Time,
) {
	t.Helper()
	body := codexHostAuthBody(t, refresh, expires, lastRefresh)
	if err := os.WriteFile(launch.AuthSnapshot, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readCodexReviewInput(
		cfg.InputRoot, launch.AuthSnapshot, maxCodexAuthSnapshotBytes,
	); err != nil {
		t.Fatalf("test host auth hardening: %v", err)
	}
}

func assertCallOrder(t *testing.T, calls []string, ordered ...string) {
	t.Helper()
	next := 0
	for _, call := range calls {
		if next < len(ordered) && call == ordered[next] {
			next++
		}
	}
	if next != len(ordered) {
		t.Fatalf("calls = %v, want ordered subsequence %v", calls, ordered)
	}
}
