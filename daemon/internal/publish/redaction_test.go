package publish_test

import (
	"context"
	"encoding/hex"
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
		// %x hex-encodes the strings it reaches, so a token leaked under
		// %x appears only in this form.
		"hex installation token": hex.EncodeToString([]byte(fixtureTokenValue)),
		"webhook secret":         "whsec_WEBHOOKWEBHOOK",
		"client secret":          "cs_CLIENTSECRETCLIENTSECRET",
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

	// Each caching token source renders with a populated cache, so its
	// cached token goes through the sweep along with the source's own
	// fields. Each mint server answers with the grant its source validates.
	coveredMinter := func(scopes map[string]string) *publish.Minter {
		srv := newMintServer(t, func(w http.ResponseWriter, _ *http.Request) {
			writeFilingTokenResponse(t, w, scopes)
		})
		t.Cleanup(srv.Close)
		return newCoveredMinter(newRegisteredKeystore(t), srv.Client(), srv.URL, &captureRecorder{}, conformantTrust(t), fixedNow)
	}
	publication := publish.NewCachedTokenSource(coveredMinter(map[string]string{
		"actions": "read", "administration": "read", "contents": "write",
		"environments": "read", "metadata": "read", "pull_requests": "write",
	}), fixedNow)
	filing := publish.NewFollowUpFilingTokenSource(
		coveredMinter(map[string]string{"issues": "write", "metadata": "read"}), fixedNow)
	issueRead := publish.NewIssueReadTokenSource(coveredMinter(issueReadGrant), fixedNow)
	handBackLabel := publish.NewHandBackLabelTokenSource(
		coveredMinter(map[string]string{"pull_requests": "write", "metadata": "read"}), fixedNow)

	onboardingKeystore := newTestKeystore(t)
	if err := onboardingKeystore.SaveApp(publicFixtureCredentials(t)); err != nil {
		t.Fatal(err)
	}
	onboardingServer := newMintServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeFilingTokenResponse(t, w, map[string]string{
			"actions": "read", "administration": "read", "contents": "read",
			"environments": "read", "metadata": "read",
		})
	})
	t.Cleanup(onboardingServer.Close)
	onboarding := newOnboardingSource(t, onboardingKeystore, onboardingServer, &captureRecorder{})

	sources := map[string]publish.TokenSource{
		"CachedTokenSource":         publication,
		"FollowUpFilingTokenSource": filing,
		"IssueReadTokenSource":      issueRead,
		"HandBackLabelTokenSource":  handBackLabel,
		"OnboardingTokenSource":     onboarding,
	}
	var issueReadToken publish.InstallationToken
	for name, source := range sources {
		cached, err := source.Token(context.Background(), testTrustRepo)
		if err != nil {
			t.Fatalf("%s.Token: %v", name, err)
		}
		if cached.Token.Reveal() != fixtureTokenValue {
			t.Fatalf("needle self-check failed: %s did not cache the fixture token", name)
		}
		if name == "IssueReadTokenSource" {
			issueReadToken = cached
		}
	}

	renderings := map[string]string{}
	values := map[string]any{
		"AppCredentials":    creds,
		"InstallationToken": token,
		"MintRecord":        record,
		"Secret":            creds.WebhookSecret,
		"IssueReadToken":    issueReadToken,
	}
	// A holder keeps its source in an unexported field, as Transport and
	// the publish forge do. Under a verb a pointer does not support, fmt
	// prints the source's struct without calling its Format, so the
	// source's own fields must not reach a token either.
	type tokenSourceHolder struct{ source publish.TokenSource }
	for name, source := range sources {
		values[name] = source
		values[name+" holder"] = tokenSourceHolder{source}
	}
	for name, v := range values {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d", "%t"} {
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
