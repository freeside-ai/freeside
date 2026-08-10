package publish

import (
	"errors"
	"fmt"
	"time"
)

// GitHub issues every installation access token with the same fixed
// one-hour lifetime, so the returned expiry is a checkable part of the
// grant rather than free-form data. The plan requires the expected
// bounded expiry to be verified before a worker can receive the token
// (docs/plan.md §5.5), which is only meaningful against one declared
// lifetime; this is it.
//
// installationTokenSkew is the entire slack allowed on top of that
// lifetime. It absorbs request/response latency and a local clock
// running behind GitHub's, the two honest reasons an expires_at lands
// later than local now plus one hour.
//
// It is jwtLifetime because that is where the App JWT stops being
// accepted: GitHub rejects a JWT whose exp has passed, so a clock at
// least jwtLifetime behind GitHub's already fails authentication and
// never reaches this gate. A smaller value would refuse every mint on
// a clock that still authenticates fine, making this the first thing a
// drifting clock breaks; a larger one would widen the accepted lifetime
// past any skew that can still mint.
const (
	installationTokenLifetime = time.Hour
	installationTokenSkew     = jwtLifetime
)

// ErrTokenExpiry rejects an installation-token expiry that is missing,
// unparsable, already lapsed, or longer than the declared lifetime
// allows. It is one sentinel because it is one decision: the response
// did not carry the bounded expiry the mint asked for. Callers key seed
// permanence on it so the refused credential does not enter a retry loop.
var ErrTokenExpiry = errors.New("installation token expiry is not the expected bounded lifetime")

// checkInstallationTokenExpiry decodes an untrusted expires_at and
// bounds it against now, returning the UTC instant only when the value
// is usable.
//
// raw is response text a compromised proxy or a regressed API chooses
// freely, so none of it reaches the returned error: callers render that
// error into operator output beside audit records, and time.ParseError
// would carry the rejected value verbatim.
//
// The lower bound is only "after now" on purpose: a shorter-than-
// expected lifetime narrows the authority in circulation rather than
// extending it, and a token too short to finish the work fails that
// attempt loudly. Nothing downstream enforces a minimum, since
// CachedTokenSource's tokenExpirySkew gates cache hits rather than the
// token a fresh mint hands back.
func checkInstallationTokenExpiry(raw Secret, now time.Time) (time.Time, error) {
	if raw.Reveal() == "" {
		return time.Time{}, fmt.Errorf("%w: response carries no expiry", ErrTokenExpiry)
	}
	expiresAt, err := time.Parse(time.RFC3339, raw.Reveal())
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: response expiry is unparsable", ErrTokenExpiry)
	}
	if !expiresAt.After(now) {
		return time.Time{}, fmt.Errorf("%w: response expiry is not in the future", ErrTokenExpiry)
	}
	if bound := now.Add(installationTokenLifetime + installationTokenSkew); expiresAt.After(bound) {
		return time.Time{}, fmt.Errorf("%w: response expiry is more than %s away",
			ErrTokenExpiry, installationTokenLifetime+installationTokenSkew)
	}
	return expiresAt.UTC(), nil
}
