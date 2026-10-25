package publish_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

// trustedIssueInspectionAuthority is a durable authority whose one trusted
// installation (777) holds the fixture repository under the fixture owner.
func trustedIssueInspectionAuthority() publish.InstallationAuthority {
	return publish.InstallationAuthority{
		TrustedOwners: []publish.TrustedOwner{{Login: "freeside-ai", ID: testOwnerID}},
		TrustedInstallations: []publish.TrustedInstallation{{
			RegistrationID: fixtureAppID, InstallationID: 777,
			Account: "freeside-ai", AccountID: testOwnerID,
			RepositoryIDs: []int64{fixtureRepositoryID},
		}},
	}
}

func TestIssueInspectionTokenSourceMintsOnlyIssueRead(t *testing.T) {
	t.Parallel()
	requests := 0
	server := newMintServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.URL.Path != "/app/installations/777/access_tokens" {
			t.Errorf("mint request = %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			RepositoryIDs []int64         `json:"repository_ids"`
			Permissions   json.RawMessage `json:"permissions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if len(body.RepositoryIDs) != 1 || body.RepositoryIDs[0] != fixtureRepositoryID ||
			string(body.Permissions) != `{"issues":"read","metadata":"read"}` {
			t.Errorf("mint body = %+v %s", body.RepositoryIDs, body.Permissions)
		}
		writeFilingTokenResponse(t, w, map[string]string{"issues": "read", "metadata": "read"})
	})
	defer server.Close()

	recorder := &captureRecorder{}
	minter := publish.NewMinter(newRegisteredKeystore(t), server.Client(), server.URL, recorder, nil, fixedNow)
	source := publish.NewIssueInspectionTokenSource(
		minter, &onboardingAuthoritySource{authority: trustedIssueInspectionAuthority()},
		fixtureAppID, fixtureRepositoryID,
	)
	token, err := source.Token(context.Background(), testTrustRepo)
	if err != nil {
		t.Fatal(err)
	}
	if token.Permissions != publish.IssueInspectionPermissions || token.RepositoryID != fixtureRepositoryID ||
		token.RegistrationID != fixtureAppID || token.InstallationID != 777 {
		t.Fatalf("issue inspection token = %+v", token)
	}
	if requests != 1 || len(recorder.records) != 1 ||
		recorder.records[0].Requested != publish.IssueInspectionPermissions ||
		recorder.records[0].Granted != publish.IssueInspectionPermissions {
		t.Fatalf("requests = %d, audit records = %+v", requests, recorder.records)
	}
	if rendered := fmt.Sprintf("%v %+v", source, source); strings.Contains(rendered, fixtureTokenValue) ||
		strings.Contains(rendered, "PRIVATE KEY") {
		t.Fatalf("source rendering leaked a credential: %s", rendered)
	}
}

// TestIssueInspectionTokenSourceFailsClosedBeforeMinting pins each refusal of
// the shared durable-binding resolution: none reaches the mint endpoint, and
// none carries a token.
func TestIssueInspectionTokenSourceFailsClosedBeforeMinting(t *testing.T) {
	t.Parallel()
	pendingOnly := publish.InstallationAuthority{
		TrustedOwners: []publish.TrustedOwner{{Login: "freeside-ai", ID: testOwnerID}},
		ActiveEpoch:   1, DurableIntentRevision: 2,
		Pending: &publish.PendingInstallationEnvelope{
			ActiveEpoch: 1, DurableIntentRevision: 2, RegistrationID: fixtureAppID,
			ExpectedAccount: "freeside-ai", ExpectedAccountID: testOwnerID,
			InstallationID: 777, ExpectedRepositoryIDs: []int64{fixtureRepositoryID},
			RequiredRepositoryMode: "selected", ExpiresAt: fixtureTime.Add(time.Hour),
		},
	}
	otherRepository := trustedIssueInspectionAuthority()
	otherRepository.TrustedInstallations[0].RepositoryIDs = []int64{fixtureRepositoryID + 1}
	ambiguous := trustedIssueInspectionAuthority()
	ambiguous.TrustedInstallations = append(ambiguous.TrustedInstallations, publish.TrustedInstallation{
		RegistrationID: fixtureAppID, InstallationID: 778,
		Account: "freeside-ai", AccountID: testOwnerID,
		RepositoryIDs: []int64{fixtureRepositoryID},
	})

	for name, tc := range map[string]struct {
		authority      publish.InstallationAuthority
		registrationID int64
		repo           string
		want           string
		// duplicate saves the fixture App ID a second time under another
		// owner: the keystore keys registrations by owner, so it lists both.
		duplicate bool
	}{
		"duplicated registration": {
			trustedIssueInspectionAuthority(), fixtureAppID, testTrustRepo, "selected registration is duplicated", true,
		},
		"pending-only authority": {
			pendingOnly, fixtureAppID, testTrustRepo, "repository has no durable trusted installation", false,
		},
		"no trusted installation for repo": {
			otherRepository, fixtureAppID, testTrustRepo, "repository has no durable trusted installation", false,
		},
		"owner mismatch": {
			trustedIssueInspectionAuthority(), fixtureAppID, "someone-else/evidence-repo",
			"repository owner differs from trusted installation", false,
		},
		// A private registration's authority validation refuses two
		// installations on one account before the resolver's own ambiguity
		// check is reached; either way the refusal precedes the mint.
		"ambiguous authority": {
			ambiguous, fixtureAppID, testTrustRepo, "account is bound to multiple installations", false,
		},
		"absent registration": {
			trustedIssueInspectionAuthority(), fixtureAppID + 1, testTrustRepo, "selected registration is unavailable", false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			requests := 0
			server := newMintServer(t, func(w http.ResponseWriter, _ *http.Request) {
				requests++
				writeFilingTokenResponse(t, w, map[string]string{"issues": "read", "metadata": "read"})
			})
			defer server.Close()
			recorder := &captureRecorder{}
			keystore := newRegisteredKeystore(t)
			if tc.duplicate {
				if err := keystore.SaveApp(publish.AppCredentials{
					Owner: "someone-else", OwnerID: testOwnerID + 1, Visibility: publish.AppVisibilityPrivate,
					AppID: fixtureAppID, Name: "freeside-publish-2", Slug: "freeside-publish-2",
					ClientID: "Iv1.cafecafecafecafe", Key: fixtureKey(t),
				}); err != nil {
					t.Fatal(err)
				}
			}
			minter := publish.NewMinter(keystore, server.Client(), server.URL, recorder, nil, fixedNow)
			token, err := publish.NewIssueInspectionTokenSource(
				minter, &onboardingAuthoritySource{authority: tc.authority},
				tc.registrationID, fixtureRepositoryID,
			).Token(context.Background(), tc.repo)
			if err == nil || !strings.HasPrefix(err.Error(), "issue inspection token: ") ||
				!strings.Contains(err.Error(), tc.want) ||
				strings.Contains(err.Error(), fixtureTokenValue) || token.Token.Reveal() != "" {
				t.Fatalf("result = %+v, %v", token, err)
			}
			if requests != 0 || len(recorder.records) != 0 {
				t.Fatalf("refusal reached the mint: requests = %d, records = %d", requests, len(recorder.records))
			}
		})
	}
}

func TestIssueInspectionTokenSourceRejectsDifferentGrants(t *testing.T) {
	t.Parallel()
	for name, scopes := range map[string]map[string]string{
		"missing issues":    {"metadata": "read"},
		"write issues":      {"issues": "write", "metadata": "read"},
		"publication scope": {"issues": "read", "metadata": "read", "contents": "write"},
		"research grant":    {"issues": "read", "pull_requests": "read", "metadata": "read"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := newMintServer(t, func(w http.ResponseWriter, _ *http.Request) { writeFilingTokenResponse(t, w, scopes) })
			defer server.Close()
			recorder := &captureRecorder{}
			minter := publish.NewMinter(newRegisteredKeystore(t), server.Client(), server.URL, recorder, nil, fixedNow)
			token, err := publish.NewIssueInspectionTokenSource(
				minter, &onboardingAuthoritySource{authority: trustedIssueInspectionAuthority()},
				fixtureAppID, fixtureRepositoryID,
			).Token(context.Background(), testTrustRepo)
			if !errors.Is(err, publish.ErrGrantMismatch) || token.Token.Reveal() != "" {
				t.Fatalf("different grant result = %v", err)
			}
			if len(recorder.records) != 0 {
				t.Fatal("rejected grant reached the successful-mint audit")
			}
		})
	}
}
