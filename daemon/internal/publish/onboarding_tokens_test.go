package publish_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

type onboardingAuthoritySource struct {
	authority publish.InstallationAuthority
}

func (s *onboardingAuthoritySource) InstallationAuthority(
	context.Context,
	int64,
) (publish.InstallationAuthority, error) {
	return s.authority, nil
}

type onboardingGate struct {
	installationID int64
	pendingReady   bool
	trustedReady   bool
}

func (g *onboardingGate) AllowsRepository(int64, int64, int64) bool {
	return g.trustedReady
}

func (g *onboardingGate) PendingReady(
	publish.PendingInstallationEnvelope,
) (int64, bool) {
	return g.installationID, g.pendingReady
}

func TestOnboardingTokenSourceRegatesCachedReadOnlyMint(t *testing.T) {
	ks := newTestKeystore(t)
	if err := ks.SaveApp(publicFixtureCredentials(t)); err != nil {
		t.Fatal(err)
	}
	authority := &onboardingAuthoritySource{authority: publish.InstallationAuthority{
		ActiveEpoch: 1, DurableIntentRevision: 2,
		TrustedOwners: []publish.TrustedOwner{{
			Login: "freeside-ai", ID: testOwnerID,
		}},
		Pending: &publish.PendingInstallationEnvelope{
			ActiveEpoch: 1, DurableIntentRevision: 2,
			RegistrationID:  fixtureAppID,
			ExpectedAccount: "freeside-ai", ExpectedAccountID: testOwnerID,
			InstallationID:         777,
			CurrentRepositoryIDs:   []int64{},
			ExpectedRepositoryIDs:  []int64{fixtureRepositoryID},
			RequiredRepositoryMode: "selected",
			ExpiresAt:              fixtureTime.Add(time.Hour),
		},
	}}
	gate := &onboardingGate{installationID: 777, pendingReady: true}
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost ||
			r.URL.Path != "/app/installations/777/access_tokens" {
			t.Errorf("mint request = %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			RepositoryIDs []int64             `json:"repository_ids"`
			Permissions   publish.Permissions `json:"permissions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if len(body.RepositoryIDs) != 1 ||
			body.RepositoryIDs[0] != fixtureRepositoryID ||
			body.Permissions != publish.WorkflowAuditPermissions {
			t.Errorf("mint body = %+v", body)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{
			"token":"`+fixtureTokenValue+`",
			"expires_at":"2026-07-16T13:00:00Z",
			"permissions":{
				"actions":"read",
				"administration":"read",
				"contents":"read",
				"environments":"read",
				"metadata":"read"
			},
			"repository_selection":"selected",
			"repositories":[{"id":990011,"name":"evidence-repo"}]
		}`)
	}))
	defer server.Close()

	recorder := &captureRecorder{}
	minter := publish.NewMinterWithJanitor(
		ks, server.Client(), server.URL, recorder, nil, fixedNow, activeJanitorStatus{},
	)
	tokens := publish.NewOnboardingTokenSource(
		minter, authority, gate, fixtureAppID, fixtureRepositoryID, fixedNow,
	)
	for range 2 {
		token, err := tokens.Token(context.Background(), testTrustRepo)
		if err != nil {
			t.Fatal(err)
		}
		if token.Permissions != publish.WorkflowAuditPermissions {
			t.Fatalf("token permissions = %+v", token.Permissions)
		}
	}
	if requests != 1 || len(recorder.records) != 1 {
		t.Fatalf("requests = %d, records = %d, want one cached mint", requests, len(recorder.records))
	}
	if recorder.records[0].Requested != publish.WorkflowAuditPermissions ||
		recorder.records[0].Granted != publish.WorkflowAuditPermissions {
		t.Fatalf("audit permissions = %+v", recorder.records[0])
	}

	gate.pendingReady = false
	if _, err := tokens.Token(context.Background(), testTrustRepo); err == nil {
		t.Fatal("cached onboarding token survived withdrawal of pending readiness")
	}
	if requests != 1 {
		t.Fatalf("withdrawn gate caused another mint, requests = %d", requests)
	}

	gate.pendingReady = true
	authority.authority.Pending.ExpiresAt = fixtureTime
	if _, err := tokens.Token(context.Background(), testTrustRepo); err == nil ||
		strings.Contains(err.Error(), fixtureTokenValue) {
		t.Fatalf("expired authority result = %v", err)
	}
	if requests != 1 {
		t.Fatalf("expired authority caused another mint, requests = %d", requests)
	}

	authority.authority.Pending = nil
	authority.authority.TrustedInstallations = []publish.TrustedInstallation{{
		RegistrationID: fixtureAppID, InstallationID: 777,
		Account: "freeside-ai", AccountID: testOwnerID,
		RepositoryIDs: []int64{fixtureRepositoryID},
	}}
	gate.trustedReady = true
	if _, err := tokens.Token(context.Background(), testTrustRepo); err != nil {
		t.Fatalf("promoted trusted replay could not reuse the audit token: %v", err)
	}
	gate.trustedReady = false
	if _, err := tokens.Token(context.Background(), testTrustRepo); err == nil {
		t.Fatal("cached onboarding token survived withdrawal of trusted coverage")
	}
	if requests != 1 {
		t.Fatalf("trusted cache checks caused another mint, requests = %d", requests)
	}
}
