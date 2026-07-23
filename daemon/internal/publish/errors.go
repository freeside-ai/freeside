package publish

import (
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// ErrTrustProfileDrift is the class sentinel for a publication that fails the
// automation-trust drift gate (#169, plan §5.5): the candidate carries no
// trust-profile binding, its bound profile is no longer current, there is no
// current profile or complete fresh audit to compare against, or the fresh
// live audit exceeds the approved profile. It aliases the domain sentinel so
// the domain comparator's *TrustDriftError (recover the axis with errors.As)
// and the gate's own fail-closed cases all match one errors.Is target.
var ErrTrustProfileDrift = domain.ErrTrustProfileDrift

// ErrUnauthorizedPublication is the class sentinel for a publication that
// fails the candidate-authorization gate (#168, plan §5.6): the candidate
// carries no authorization binding, names no recorded authorization, the
// recorded authorization is malformed, it describes a different candidate, or
// it does not authorize publication (verification failed, or a publish-blocking
// importer/verifier finding lacks a trusted non-blocking disposition). The
// authorization is the daemon-authored record binding what publication may
// trust (#172); the gate fails closed before any external effect.
var ErrUnauthorizedPublication = errors.New("candidate authorization does not permit publication")

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

// ErrNoAppCredentials reports that no App has been registered yet (no key
// material on disk). Registration via the manifest flow is the only ordinary
// way to create it; after a checkpoint restore this is the expected state
// (recovery may require reauthentication, §5.10).
var ErrNoAppCredentials = errors.New("no GitHub App credentials in the keystore")

// ErrNoAppRegistration is returned by Keystore.LoadApp when the requested
// numeric owner has no registration. It is distinct from the entirely empty
// keystore state so resolution can fail closed without hiding which binding
// is absent.
var ErrNoAppRegistration = errors.New("no GitHub App registration for owner")

// ErrLegacyAppMigrationRequired is returned while the former singleton
// layout is present. Its metadata has no owner or visibility, so no load,
// enumeration, or new save may silently attribute it; MigrateLegacyApp
// requires both values explicitly before relocating the credential.
var ErrLegacyAppMigrationRequired = errors.New("legacy GitHub App credentials require explicit migration")

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

// ErrGrantMismatch is returned when GitHub grants an installation
// token whose permission or repository scope differs from the request
// in either direction: a narrower grant would fail the publish path
// halfway through its work, and a broader one (an extra permission,
// an all-repositories selection) would circulate more authority than
// the audit row records. The token is discarded rather than
// circulated.
var ErrGrantMismatch = errors.New("installation token grant does not match the request")

// ErrInstallationResolution is the class sentinel for installation metadata
// that fails the returned-object trust boundary. A *ResolutionFailure carries
// the safe registration and expected-owner coordinates for audit without
// retaining the untrusted response value.
var ErrInstallationResolution = errors.New("installation metadata failed validation")

// ErrNoInstallation reports that no known registration has an installation
// for the repository owner. Unknown owners never fall back to a caller-supplied
// installation ID.
var ErrNoInstallation = errors.New("no GitHub App installation for repository owner")

// ErrAmbiguousInstallation reports that more than one registration claims an
// installation for the same repository owner. Resolution cannot choose between
// credentials implicitly, so minting fails closed.
var ErrAmbiguousInstallation = errors.New("multiple GitHub App installations for repository owner")

// ErrHeadMismatch is returned when a head-bound artifact's
// source_head_sha differs from the candidate head being published:
// its evidence describes some other revision, and a new remediation
// head invalidates prior-head evidence (plan §5.15 rule 2), so the
// publication fails before any external effect.
var ErrHeadMismatch = errors.New("artifact head binding does not match the candidate head")

// ErrPublicationConflict is returned when an existing external
// resource under this publication's deterministic identity disagrees
// with the candidate: the branch exists at a different commit, the
// marker-matched PR is closed, or more than one PR claims the
// identity. Convergence never overwrites unknown external state; a
// human resolves the conflict.
var ErrPublicationConflict = errors.New("existing publication resource conflicts with the candidate")

// ErrForeignResource is returned when a pull request occupies the
// publication branch without carrying this publication's identity
// marker. It is not ours to converge, so the publication refuses
// rather than adopting or overwriting it.
var ErrForeignResource = errors.New("pull request on the publication branch does not carry the identity marker")

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
