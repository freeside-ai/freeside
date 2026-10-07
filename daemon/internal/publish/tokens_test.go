package publish_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestFollowUpFilingTokenSourceScopesCacheAndAudit(t *testing.T) {
	t.Parallel()
	clock, mints := fixtureTime, 0
	want := publish.Permissions{Issues: "write", Metadata: "read"}
	srv := newMintServer(t, func(w http.ResponseWriter, r *http.Request) {
		mints++
		var request struct {
			RepositoryIDs []int64           `json:"repository_ids"`
			Permissions   map[string]string `json:"permissions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if len(request.RepositoryIDs) != 1 || request.RepositoryIDs[0] != fixtureRepositoryID ||
			len(request.Permissions) != 2 || request.Permissions["issues"] != "write" || request.Permissions["metadata"] != "read" {
			t.Errorf("filing mint request = %+v", request)
		}
		writeFilingTokenResponse(t, w, map[string]string{"issues": "write", "metadata": "read"})
	})
	defer srv.Close()
	s := newTestStore(t)
	recorder, err := publish.NewStoreRecorder(s)
	if err != nil {
		t.Fatal(err)
	}
	now := func() time.Time { return clock }
	m := newCoveredMinter(newRegisteredKeystore(t), srv.Client(), srv.URL, recorder, conformantTrust(t), now)
	source := publish.NewFollowUpFilingTokenSource(m, now)
	for _, offset := range []time.Duration{0, 30 * time.Minute, 59 * time.Minute} {
		clock = fixtureTime.Add(offset)
		token, err := source.Token(context.Background(), testTrustRepo)
		if err != nil {
			t.Fatal(err)
		}
		if token.Permissions != want {
			t.Errorf("permissions = %+v, want %+v", token.Permissions, want)
		}
	}
	if mints != 2 {
		t.Fatalf("mints = %d, want 2", mints)
	}
	if err := s.Read(context.Background(), func(tx *store.ReadTx) error {
		audits, err := tx.ListMintAudits(context.Background())
		if err != nil {
			return err
		}
		if len(audits) != 2 {
			t.Fatalf("audits = %d, want 2", len(audits))
		}
		for _, audit := range audits {
			requested := publish.Permissions{
				Actions: audit.RequestedActions, Administration: audit.RequestedAdministration,
				Contents: audit.RequestedContents, Environments: audit.RequestedEnvironments, Issues: audit.RequestedIssues,
				PullRequests: audit.RequestedPullRequests, Metadata: audit.RequestedMetadata,
			}
			granted := publish.Permissions{
				Actions: audit.GrantedActions, Administration: audit.GrantedAdministration,
				Contents: audit.GrantedContents, Environments: audit.GrantedEnvironments, Issues: audit.GrantedIssues,
				PullRequests: audit.GrantedPullRequests, Metadata: audit.GrantedMetadata,
			}
			if requested != want || granted != want {
				t.Errorf("audit scopes requested=%+v granted=%+v", requested, granted)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func writeFilingTokenResponse(t *testing.T, w http.ResponseWriter, scopes map[string]string) {
	t.Helper()
	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(map[string]any{
		"token": fixtureTokenValue, "expires_at": fixtureTime.Add(time.Hour),
		"permissions": scopes, "repository_selection": "selected",
		"repositories": []map[string]any{{"id": fixtureRepositoryID, "name": "evidence-repo"}},
	}); err != nil {
		t.Error(err)
	}
}

func TestFollowUpFilingTokenSourceRejectsDifferentGrants(t *testing.T) {
	t.Parallel()
	for name, scopes := range map[string]map[string]string{
		"missing issues":    {"metadata": "read"},
		"read only issues":  {"issues": "read", "metadata": "read"},
		"publication scope": {"issues": "write", "metadata": "read", "contents": "write"},
		"unknown scope":     {"issues": "write", "metadata": "read", "unexpected": "read"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newMintServer(t, func(w http.ResponseWriter, _ *http.Request) { writeFilingTokenResponse(t, w, scopes) })
			defer srv.Close()
			recorder := &captureRecorder{}
			m := newCoveredMinter(newRegisteredKeystore(t), srv.Client(), srv.URL, recorder, conformantTrust(t), fixedNow)
			token, err := publish.NewFollowUpFilingTokenSource(m, fixedNow).Token(context.Background(), testTrustRepo)
			if !errors.Is(err, publish.ErrGrantMismatch) || token.Token.Reveal() != "" {
				t.Fatal("different grant returned a token or lacked ErrGrantMismatch")
			}
			if len(recorder.records) != 0 {
				t.Fatal("rejected grant reached the successful-mint audit")
			}
		})
	}
}

func TestFollowUpFilingTokenSourceRevalidatesInstallationBeforeCacheHit(t *testing.T) {
	t.Parallel()
	discoveries, mints := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/app/installations" {
			discoveries++
			if discoveries == 1 {
				_, _ = fmt.Fprintf(w, `[{"id":777,"app_id":%d,"target_id":%d,"repository_selection":"selected","account":{"login":"freeside-ai","id":%d}}]`, fixtureAppID, testOwnerID, testOwnerID)
			} else {
				_, _ = fmt.Fprint(w, `[]`)
			}
			return
		}
		mints++
		writeFilingTokenResponse(t, w, map[string]string{"issues": "write", "metadata": "read"})
	}))
	defer srv.Close()
	m := newCoveredMinter(newRegisteredKeystore(t), srv.Client(), srv.URL, &captureRecorder{}, conformantTrust(t), fixedNow)
	source := publish.NewFollowUpFilingTokenSource(m, fixedNow)
	if _, err := source.Token(context.Background(), testTrustRepo); err != nil {
		t.Fatal(err)
	}
	if token, err := source.Token(context.Background(), testTrustRepo); !errors.Is(err, publish.ErrNoInstallation) || token.Token.Reveal() != "" {
		t.Fatal("cached token survived installation removal")
	}
	if mints != 1 {
		t.Errorf("mints = %d, want 1", mints)
	}
}

// newMintCountingSource stands up a fixture-backed mint endpoint and a
// CachedTokenSource over it, returning the source and a pointer to the
// mint count. The fixture token expires at 13:00; now starts at
// fixtureTime (12:00) and is advanced through the clock pointer.
func newMintCountingSource(t *testing.T, clock *time.Time) (*publish.CachedTokenSource, *int) {
	t.Helper()
	mints := 0
	srv := newMintServer(t, func(w http.ResponseWriter, _ *http.Request) {
		mints++
		w.WriteHeader(http.StatusCreated)
		fixture, err := os.ReadFile(filepath.Join("testdata", "token-response.json"))
		if err != nil {
			t.Errorf("read fixture: %v", err)
		}
		_, _ = w.Write(fixture)
	})
	t.Cleanup(srv.Close)

	m := newCoveredMinter(newRegisteredKeystore(t), srv.Client(), srv.URL, &captureRecorder{}, conformantTrust(t), func() time.Time { return *clock })
	return publish.NewCachedTokenSource(m, func() time.Time { return *clock }), &mints
}

// TestCachedTokenSourceReusesUntilExpiry: repeated Token calls inside
// the token's lifetime mint exactly once (one audit row per mint, not
// per request).
func TestCachedTokenSourceReusesUntilExpiry(t *testing.T) {
	t.Parallel()
	clock := fixtureTime
	src, mints := newMintCountingSource(t, &clock)

	first, err := src.Token(context.Background(), testTrustRepo)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	clock = fixtureTime.Add(30 * time.Minute)
	second, err := src.Token(context.Background(), testTrustRepo)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if *mints != 1 {
		t.Errorf("minted %d times, want 1", *mints)
	}
	if first.Token.Reveal() != second.Token.Reveal() {
		t.Error("cached call returned a different token")
	}
}

// TestCachedTokenSourceRemintsNearExpiry: once inside the expiry skew
// the cached token is no longer handed out — a token about to lapse
// mid-publication would fail the path halfway.
func TestCachedTokenSourceRemintsNearExpiry(t *testing.T) {
	t.Parallel()
	clock := fixtureTime
	src, mints := newMintCountingSource(t, &clock)

	if _, err := src.Token(context.Background(), testTrustRepo); err != nil {
		t.Fatalf("Token: %v", err)
	}
	// 12:59, one minute from the fixture's 13:00 expiry: inside the
	// two-minute skew.
	clock = fixtureTime.Add(59 * time.Minute)
	if _, err := src.Token(context.Background(), testTrustRepo); err != nil {
		t.Fatalf("Token near expiry: %v", err)
	}
	if *mints != 2 {
		t.Errorf("minted %d times, want 2 (re-mint inside the skew)", *mints)
	}
}

// TestCachedTokenSourceRevalidatesInstallationBeforeCacheHit proves a cached
// credential is not handed out after the registration no longer reports the
// owner installation.
func TestCachedTokenSourceRevalidatesInstallationBeforeCacheHit(t *testing.T) {
	t.Parallel()
	discoveries := 0
	mints := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/app/installations" {
			discoveries++
			if discoveries == 1 {
				_, _ = fmt.Fprintf(w, `[{"id":777,"app_id":%d,"target_id":%d,"repository_selection":"selected","account":{"login":"freeside-ai","id":%d}}]`,
					fixtureAppID, testOwnerID, testOwnerID)
			} else {
				_, _ = fmt.Fprint(w, `[]`)
			}
			return
		}
		mints++
		w.WriteHeader(http.StatusCreated)
		fixture, _ := os.ReadFile(filepath.Join("testdata", "token-response.json"))
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()

	m := newCoveredMinter(newRegisteredKeystore(t), srv.Client(), srv.URL, &captureRecorder{}, conformantTrust(t), fixedNow)
	src := publish.NewCachedTokenSource(m, fixedNow)
	if _, err := src.Token(context.Background(), testTrustRepo); err != nil {
		t.Fatalf("first Token: %v", err)
	}
	if _, err := src.Token(context.Background(), testTrustRepo); !errors.Is(err, publish.ErrNoInstallation) {
		t.Fatalf("second Token err = %v, want ErrNoInstallation", err)
	}
	if mints != 1 {
		t.Errorf("minted %d times, want the first mint only", mints)
	}
}

type repositoryTrustSource map[string]domain.AutomationTrustProfile

func (s repositoryTrustSource) CurrentTrust(_ context.Context, repo string) (publish.CurrentTrust, error) {
	profile, ok := s[repo]
	if !ok {
		return publish.CurrentTrust{}, nil
	}
	return publish.CurrentTrust{Profile: &profile}, nil
}

// TestCachedTokenSourceIsolatesRegistrations covers the cache identity with two
// private registrations whose installations deliberately share a synthetic
// installation ID. Each registration mints and caches independently.
func TestCachedTokenSourceIsolatesRegistrations(t *testing.T) {
	t.Parallel()
	const (
		secondOwner   = "freeasinbird"
		secondOwnerID = testOwnerID + 1
		secondAppID   = fixtureAppID + 1
		secondRepoID  = fixtureRepositoryID + 1
		secondRepo    = secondOwner + "/evidence-repo"
	)
	ks := newRegisteredKeystore(t)
	if err := ks.SaveApp(publish.AppCredentials{
		Owner:      secondOwner,
		OwnerID:    secondOwnerID,
		Visibility: publish.AppVisibilityPrivate,
		AppID:      secondAppID,
		Name:       "second-app",
		Key:        fixtureKey(t),
	}); err != nil {
		t.Fatalf("SaveApp(second): %v", err)
	}
	firstJWT, err := publish.AppJWT(fixtureKey(t), fixtureAppID, fixtureTime)
	if err != nil {
		t.Fatal(err)
	}
	secondJWT, err := publish.AppJWT(fixtureKey(t), secondAppID, fixtureTime)
	if err != nil {
		t.Fatal(err)
	}
	mints := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if r.Method == http.MethodGet && r.URL.Path == "/app/installations" {
			switch auth {
			case "Bearer " + firstJWT.Reveal():
				_, _ = fmt.Fprintf(w, `[{"id":777,"app_id":%d,"target_id":%d,"repository_selection":"selected","account":{"login":"freeside-ai","id":%d}}]`,
					fixtureAppID, testOwnerID, testOwnerID)
			case "Bearer " + secondJWT.Reveal():
				_, _ = fmt.Fprintf(w, `[{"id":777,"app_id":%d,"target_id":%d,"repository_selection":"selected","account":{"login":"%s","id":%d}}]`,
					secondAppID, secondOwnerID, secondOwner, secondOwnerID)
			default:
				t.Errorf("unexpected discovery authorization")
			}
			return
		}
		mints[auth]++
		w.WriteHeader(http.StatusCreated)
		fixture, _ := os.ReadFile(filepath.Join("testdata", "token-response.json"))
		if auth == "Bearer "+secondJWT.Reveal() {
			fixture = bytes.Replace(fixture, []byte(`"id": 990011`), []byte(`"id": 990012`), 1)
		}
		_, _ = w.Write(fixture)
	}))
	defer srv.Close()

	firstProfile := trustProfileForRepo(t, testTrustRepo)
	secondProfile := trustProfileForRepoID(t, secondRepo, secondRepoID)
	trust := repositoryTrustSource{testTrustRepo: firstProfile, secondRepo: secondProfile}
	m := newCoveredMinter(ks, srv.Client(), srv.URL, &captureRecorder{}, trust, fixedNow)
	src := publish.NewCachedTokenSource(m, fixedNow)

	var tokens []publish.InstallationToken
	for _, repo := range []string{testTrustRepo, secondRepo, testTrustRepo, secondRepo} {
		token, err := src.Token(context.Background(), repo)
		if err != nil {
			t.Fatalf("Token(%s): %v", repo, err)
		}
		tokens = append(tokens, token)
	}
	if got := mints["Bearer "+firstJWT.Reveal()]; got != 1 {
		t.Errorf("first registration minted %d times, want 1", got)
	}
	if got := mints["Bearer "+secondJWT.Reveal()]; got != 1 {
		t.Errorf("second registration minted %d times, want 1", got)
	}
	for _, index := range []int{0, 2} {
		if tokens[index].RegistrationID != fixtureAppID || tokens[index].InstallationID != 777 ||
			tokens[index].RepositoryID != fixtureRepositoryID {
			t.Errorf("first registration token %d binding = %+v", index, tokens[index])
		}
	}
	for _, index := range []int{1, 3} {
		if tokens[index].RegistrationID != secondAppID || tokens[index].InstallationID != 777 ||
			tokens[index].RepositoryID != secondRepoID {
			t.Errorf("second registration token %d binding = %+v", index, tokens[index])
		}
	}
}

// TestCachedTokenSourceValidation covers the fail-fast argument check.
func TestCachedTokenSourceValidation(t *testing.T) {
	t.Parallel()
	clock := fixtureTime
	src, _ := newMintCountingSource(t, &clock)
	if _, err := src.Token(context.Background(), ""); err == nil {
		t.Error("empty repo accepted, want error")
	}
}
