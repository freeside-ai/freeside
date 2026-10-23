package publish_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

// TestNoCredentialValueRenders is the package-wide redaction sweep
// (issue #80 acceptance 4): every exported credential-bearing value is
// rendered through every fmt verb and encoding/json, and no needle —
// the key's PEM base64, its private exponent in the decimal form
// fmt.%+v gives big integers, the token, the webhook and client
// secrets — may appear in any rendering.
func TestNoCredentialValueRenders(t *testing.T) {
	t.Parallel()
	key := fixtureKey(t)
	pemRaw, err := os.ReadFile("testdata/test-signing-key.pem")
	if err != nil {
		t.Fatal(err)
	}
	// A middle line of the PEM body: pure base64 key material.
	pemNeedle := strings.Split(strings.TrimSpace(string(pemRaw)), "\n")[3]

	needles := map[string]string{ //nolint:gosec // leak-detection needles built from committed fixtures, not credentials
		"pem base64":         pemNeedle,
		"private exponent":   key.D.String(),
		"prime factor":       key.Primes[0].String(),
		"installation token": fixtureTokenValue,
		"webhook secret":     "whsec_WEBHOOKWEBHOOK",
		"client secret":      "cs_CLIENTSECRETCLIENTSECRET",
	}

	creds := publish.AppCredentials{
		Owner:         "freeside-ai",
		OwnerID:       testOwnerID,
		AppID:         fixtureAppID,
		Slug:          "freeside-publish",
		ClientID:      "Iv1.deadbeefdeadbeef",
		Key:           key,
		WebhookSecret: publish.Secret("whsec_WEBHOOKWEBHOOK"),
		ClientSecret:  publish.Secret("cs_CLIENTSECRETCLIENTSECRET"),
	}
	token := publish.InstallationToken{
		Token:       publish.Secret(fixtureTokenValue),
		ExpiresAt:   fixtureTime.Add(time.Hour),
		Repo:        "evidence-repo",
		Permissions: publish.PublishPermissions,
	}
	record := publish.MintRecord{
		MintedAt:       fixtureTime,
		RegistrationID: fixtureAppID,
		InstallationID: 777,
		RepositoryID:   fixtureRepositoryID,
		Repo:           "evidence-repo",
		Requested:      publish.PublishPermissions,
		Granted:        publish.PublishPermissions,
		ExpiresAt:      fixtureTime.Add(time.Hour),
	}

	// The issue-read source renders with a populated cache, so its cached
	// token goes through the sweep along with the source's own fields.
	srv := newMintServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeFilingTokenResponse(t, w, map[string]string{"issues": "read", "metadata": "read"})
	})
	defer srv.Close()
	issueRead := publish.NewIssueReadTokenSource(
		newCoveredMinter(newRegisteredKeystore(t), srv.Client(), srv.URL, &captureRecorder{}, conformantTrust(t), fixedNow),
		fixedNow,
	)
	issueReadToken, err := issueRead.Token(context.Background(), testTrustRepo)
	if err != nil {
		t.Fatal(err)
	}
	if issueReadToken.Token.Reveal() != fixtureTokenValue {
		t.Fatal("needle self-check failed: the issue-read source did not cache the fixture token")
	}

	renderings := map[string]string{}
	for name, v := range map[string]any{
		"AppCredentials":       creds,
		"InstallationToken":    token,
		"MintRecord":           record,
		"Secret":               creds.WebhookSecret,
		"IssueReadTokenSource": issueRead,
		"IssueReadToken":       issueReadToken,
	} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
			renderings[name+" "+verb] = fmt.Sprintf(verb, v)
		}
		j, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("json.Marshal(%s): %v", name, err)
		}
		renderings[name+" json"] = string(j)
	}
	renderings["APIError"] = (&publish.APIError{Status: 500, RequestPath: "/app/installations/777/access_tokens"}).Error()

	for where, rendered := range renderings {
		for what, needle := range needles {
			if strings.Contains(rendered, needle) {
				t.Errorf("%s leaks the %s", where, what)
			}
		}
	}

	// The sweep is only meaningful if the needles are real: the same
	// values rendered deliberately must contain them.
	if !strings.Contains(creds.WebhookSecret.Reveal(), "whsec_") {
		t.Error("needle self-check failed: Reveal does not return the value")
	}
}
