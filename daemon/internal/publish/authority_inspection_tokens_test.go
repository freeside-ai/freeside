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

func TestAuthorityInspectionTokenSourceUsesDurableReadOnlyAuthority(t *testing.T) {
	t.Parallel()
	ks := newRegisteredKeystore(t)
	authority := &onboardingAuthoritySource{authority: publish.InstallationAuthority{
		TrustedOwners: []publish.TrustedOwner{{Login: "freeside-ai", ID: testOwnerID}},
		TrustedInstallations: []publish.TrustedInstallation{{
			RegistrationID: fixtureAppID, InstallationID: 777,
			Account: "freeside-ai", AccountID: testOwnerID,
			RepositoryIDs: []int64{fixtureRepositoryID},
		}},
	}}
	readOnly := publish.Permissions{Contents: "read", Metadata: "read"}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.URL.Path != "/app/installations/777/access_tokens" {
			t.Errorf("mint request = %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			RepositoryIDs []int64             `json:"repository_ids"`
			Permissions   publish.Permissions `json:"permissions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if len(body.RepositoryIDs) != 1 || body.RepositoryIDs[0] != fixtureRepositoryID ||
			body.Permissions != readOnly {
			t.Errorf("mint body = %+v", body)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{
			"token":"`+fixtureTokenValue+`",
			"expires_at":"2026-07-16T13:00:00Z",
			"permissions":{"contents":"read","metadata":"read"},
			"repository_selection":"selected",
			"repositories":[{"id":990011,"name":"evidence-repo"}]
		}`)
	}))
	defer server.Close()

	recorder := &captureRecorder{}
	minter := publish.NewMinter(ks, server.Client(), server.URL, recorder, nil, fixedNow)
	tokens := publish.NewAuthorityInspectionTokenSource(
		minter, authority, fixtureAppID, fixtureRepositoryID,
	)
	token, err := tokens.Token(context.Background(), testTrustRepo)
	if err != nil {
		t.Fatal(err)
	}
	if token.Permissions != readOnly || token.RepositoryID != fixtureRepositoryID ||
		token.RegistrationID != fixtureAppID || token.InstallationID != 777 {
		t.Fatalf("inspection token = %+v", token)
	}
	if requests != 1 || len(recorder.records) != 1 ||
		recorder.records[0].Requested != readOnly || recorder.records[0].Granted != readOnly {
		t.Fatalf("requests = %d, audit records = %+v", requests, recorder.records)
	}

	authority.authority.TrustedInstallations = nil
	authority.authority.ActiveEpoch = 1
	authority.authority.DurableIntentRevision = 2
	authority.authority.Pending = &publish.PendingInstallationEnvelope{
		ActiveEpoch: 1, DurableIntentRevision: 2, RegistrationID: fixtureAppID,
		ExpectedAccount: "freeside-ai", ExpectedAccountID: testOwnerID,
		InstallationID: 777, ExpectedRepositoryIDs: []int64{fixtureRepositoryID},
		RequiredRepositoryMode: "selected", ExpiresAt: fixtureTime.Add(time.Hour),
	}
	if _, err := tokens.Token(context.Background(), testTrustRepo); err == nil ||
		strings.Contains(err.Error(), fixtureTokenValue) {
		t.Fatalf("pending-only authority result = %v", err)
	}
	if requests != 1 {
		t.Fatalf("pending-only authority reached mint endpoint, requests = %d", requests)
	}
}
