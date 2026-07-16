package publish_test

import (
	"encoding/json"
	"fmt"
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
	key := fixtureKey(t)
	pemRaw, err := os.ReadFile("testdata/test-signing-key.pem")
	if err != nil {
		t.Fatal(err)
	}
	// A middle line of the PEM body: pure base64 key material.
	pemNeedle := strings.Split(strings.TrimSpace(string(pemRaw)), "\n")[3]

	needles := map[string]string{
		"pem base64":         pemNeedle,
		"private exponent":   key.D.String(),
		"prime factor":       key.Primes[0].String(),
		"installation token": fixtureTokenValue,
		"webhook secret":     "whsec_WEBHOOKWEBHOOK",
		"client secret":      "cs_CLIENTSECRETCLIENTSECRET",
	}

	creds := publish.AppCredentials{
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
		InstallationID: 777,
		Repo:           "evidence-repo",
		Requested:      publish.PublishPermissions,
		Granted:        publish.PublishPermissions,
		ExpiresAt:      fixtureTime.Add(time.Hour),
	}

	renderings := map[string]string{}
	for name, v := range map[string]any{
		"AppCredentials":    creds,
		"InstallationToken": token,
		"MintRecord":        record,
		"Secret":            creds.WebhookSecret,
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
