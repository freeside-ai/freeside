package publish

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// jwtHeader is fixed: GitHub App JWTs are always RS256.
const jwtHeader = `{"alg":"RS256","typ":"JWT"}`

// GitHub caps App JWT lifetime at 10 minutes and rejects tokens issued
// in the future; the 60s backdate absorbs clock skew (both from
// GitHub's App JWT documentation) and 9m stays safely under the cap.
const (
	jwtBackdate = time.Minute
	jwtLifetime = 9 * time.Minute
)

// AppJWT constructs the GitHub App JWT that authenticates the App
// itself (installation tokens are minted with it; see Minter). RS256
// over stdlib crypto only: PKCS#1 v1.5 signatures are deterministic,
// so a fixed key and time yield a byte-stable token the golden test
// pins. The caller injects now; this package never reads the clock.
func AppJWT(key *rsa.PrivateKey, appID int64, now time.Time) (Secret, error) {
	if key == nil {
		return "", errors.New("jwt: no signing key")
	}
	claims, err := json.Marshal(struct {
		IssuedAt int64  `json:"iat"`
		Expires  int64  `json:"exp"`
		Issuer   string `json:"iss"`
	}{
		IssuedAt: now.Add(-jwtBackdate).Unix(),
		Expires:  now.Add(jwtLifetime).Unix(),
		Issuer:   strconv.FormatInt(appID, 10),
	})
	if err != nil {
		return "", fmt.Errorf("jwt: encode claims: %w", err)
	}

	b64 := base64.RawURLEncoding
	signingInput := b64.EncodeToString([]byte(jwtHeader)) + "." + b64.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(nil, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("jwt: sign: %w", err)
	}
	return Secret(signingInput + "." + b64.EncodeToString(sig)), nil
}
