package publish

import (
	"errors"
	"fmt"
)

// The publish boundary's error vocabulary. GitHub API failures are
// carried by *APIError, which records only the status code and request
// path: response bodies can echo request credentials or carry
// token-shaped content, so they never travel inside an error (issue #80
// acceptance 4).

// ErrCredentialsInsideStateDir is returned by NewKeystore when the
// credentials directory and the state directory overlap. The state
// directory is the future checkpoint/backup surface (docs/plan.md
// §5.10 excludes App keys from checkpoints), so the keystore refuses to
// exist anywhere a checkpoint could reach; the check fails closed on
// any path ambiguity.
var ErrCredentialsInsideStateDir = errors.New("credentials directory overlaps the state directory")

// ErrNoAppCredentials is returned by Keystore.LoadApp when no App has
// been registered yet (no key material on disk). Registration via the
// manifest flow is the only way to create it; after a checkpoint
// restore this is the expected state (recovery may require
// reauthentication, §5.10).
var ErrNoAppCredentials = errors.New("no GitHub App credentials in the keystore")

// ErrCredentialPermissions is returned when on-disk credential files or
// directories are reachable by group or other. The keystore re-asserts
// containment on every load and fails closed rather than narrowing the
// permissions itself, since widened permissions mean the value must be
// treated as exposed.
var ErrCredentialPermissions = errors.New("credential path permissions are too permissive")

// ErrRegistrationDenied is returned when the manifest-flow redirect
// comes back without a temporary code, i.e. the user cancelled or
// GitHub rejected the manifest.
var ErrRegistrationDenied = errors.New("app manifest registration returned no code")

// ErrGitHubAPI is the class sentinel for any non-success GitHub API
// response; it is carried by *APIError. Match the class with errors.Is
// and recover the status with errors.As.
var ErrGitHubAPI = errors.New("github api request failed")

// APIError reports a non-success GitHub API response. It carries the
// status code and the request path only — never the response body,
// which may contain credential material.
type APIError struct {
	Status      int
	RequestPath string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github api: %s returned status %d", e.RequestPath, e.Status)
}

// Is lets errors.Is(err, ErrGitHubAPI) match the class while errors.As
// recovers the status and path.
func (e *APIError) Is(target error) bool { return target == ErrGitHubAPI }
