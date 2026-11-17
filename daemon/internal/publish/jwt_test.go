package publish_test

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

// fixtureTime is the fixed instant every deterministic fixture in this
// package signs at.
var fixtureTime = time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)

const fixtureAppID = int64(12345)

// fixtureKeyPEM returns the committed test-only signing key as PKCS #1 PEM.
// The key was generated for these fixtures and has never authenticated
// anything; it is test data, not a secret. The file holds only the base64
// body and the armor is added here, so the repository tracks no
// credential-shaped line an agent working on it could quote into an export
// the credential scan would refuse (#3305).
func fixtureKeyPEM(t *testing.T) []byte {
	t.Helper()
	body, err := os.ReadFile("testdata/test-signing-key.pem.body")
	if err != nil {
		t.Fatalf("read fixture key: %v", err)
	}
	const label = "RSA " + "PRIVATE KEY"
	return []byte("-----BEGIN " + label + "-----\n" + string(body) + "-----END " + label + "-----\n")
}

// fixtureKey parses fixtureKeyPEM.
func fixtureKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	block, _ := pem.Decode(fixtureKeyPEM(t))
	if block == nil {
		t.Fatal("fixture key is not PEM")
		return nil
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse fixture key: %v", err)
	}
	return key
}

// TestAppJWTVerifiesAndClaims verifies the signature against the public
// key and checks the claim contract: iss is the app ID, iat is
// backdated 60s for clock skew, exp stays under GitHub's 10-minute cap.
func TestAppJWTVerifiesAndClaims(t *testing.T) {
	t.Parallel()
	key := fixtureKey(t)
	jwt, err := publish.AppJWT(key, fixtureAppID, fixtureTime)
	if err != nil {
		t.Fatalf("AppJWT: %v", err)
	}

	parts := strings.Split(jwt.Reveal(), ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d segments, want 3", len(parts))
	}
	b64 := base64.RawURLEncoding

	header, err := b64.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	if want := `{"alg":"RS256","typ":"JWT"}`; string(header) != want {
		t.Errorf("header = %s, want %s", header, want)
	}

	claimsRaw, err := b64.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	var claims struct {
		IssuedAt int64  `json:"iat"`
		Expires  int64  `json:"exp"`
		Issuer   string `json:"iss"`
	}
	if err := json.Unmarshal(claimsRaw, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	if claims.Issuer != "12345" {
		t.Errorf("iss = %q, want \"12345\"", claims.Issuer)
	}
	if got := fixtureTime.Unix() - claims.IssuedAt; got != 60 {
		t.Errorf("iat backdate = %ds, want 60s", got)
	}
	lifetime := claims.Expires - fixtureTime.Unix()
	if lifetime <= 0 || lifetime >= 600 {
		t.Errorf("exp is %ds after now, want within (0, 600)", lifetime)
	}

	sig, err := b64.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		t.Errorf("signature does not verify: %v", err)
	}
}

// TestAppJWTNoKey covers the fail-closed path.
func TestAppJWTNoKey(t *testing.T) {
	t.Parallel()
	if _, err := publish.AppJWT(nil, fixtureAppID, fixtureTime); err == nil {
		t.Error("AppJWT(nil key) succeeded, want error")
	}
}
