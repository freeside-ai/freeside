package domain

import (
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// ClientEnrollment is one AuthIdentity × harness client × route × auth method
// (plan §5.4, admitted agents): the record an agent's `who` line resolves to.
// One identity has many enrollments — pi and the Codex CLI on one ChatGPT
// subscription are one identity, one lease, one budget, two enrollments with
// distinct sanitized stores — which is why the client facts that used to live
// on AuthIdentity (store locator, refresh strategy, snapshot support) live
// here and on the generations instead: a singular fact on the identity cannot
// name which client's store it describes once there is more than one.
//
// Enrollments are facts (records of what the operator enrolled), not
// configuration: agents and fragments live in the reviewed control-plane
// tree, enrollments live only here. A record of a past selection is never
// upgraded through current configuration.
type ClientEnrollment struct {
	ID             ClientEnrollmentID `json:"id"`
	AuthIdentityID AuthIdentityID     `json:"auth_identity_id"`
	HarnessClient  HarnessClientKind  `json:"harness_client"`
	// Route is the stable logical id of the route fragment this enrollment's
	// credential is valid for, deliberately not the route digest: an endpoint
	// or terms edit changes the route digest and every agent naming it, never
	// the enrollment (§5.4).
	Route          string         `json:"route"`
	AuthMethod     AuthMethod     `json:"auth_method"`
	CredentialMode CredentialMode `json:"credential_mode"`
	// RefreshStrategy and SupportsReadOnlyAuthSnapshot are client facts: how
	// this harness client's credential is kept fresh, and whether its store
	// tolerates a read-only snapshot mount.
	RefreshStrategy              RefreshStrategy `json:"refresh_strategy"`
	SupportsReadOnlyAuthSnapshot bool            `json:"supports_read_only_auth_snapshot"`
	// AccountBinding repeats the identity's account binding (the revision 36
	// rule §5.4 keeps): every enrollment and generation carries it, so a
	// credential whose account differs from its identity's is rejected at
	// enrollment, adoption, reconstruction, and admission rather than
	// attributing usage to the wrong account.
	AccountBinding string `json:"account_binding"`
}

// Validate reports whether the enrollment record is well-formed.
func (e ClientEnrollment) Validate() error {
	if e.ID == "" {
		return fmt.Errorf("client enrollment id: %w", ErrEmptyID)
	}
	if e.AuthIdentityID == "" {
		return fmt.Errorf("client enrollment %s auth_identity_id: %w", e.ID, ErrEmptyID)
	}
	if !e.HarnessClient.valid() {
		return fmt.Errorf("client enrollment %s harness_client %q: %w", e.ID, e.HarnessClient, ErrInvalidHarnessClientKind)
	}
	if e.Route == "" {
		return fmt.Errorf("client enrollment %s route: %w", e.ID, ErrEmptyField)
	}
	if !e.AuthMethod.valid() {
		return fmt.Errorf("client enrollment %s auth_method %q: %w", e.ID, e.AuthMethod, ErrInvalidAuthMethod)
	}
	if !e.CredentialMode.valid() {
		return fmt.Errorf("client enrollment %s credential_mode %q: %w", e.ID, e.CredentialMode, ErrInvalidCredentialMode)
	}
	if !e.RefreshStrategy.valid() {
		return fmt.Errorf("client enrollment %s refresh_strategy %q: %w", e.ID, e.RefreshStrategy, ErrInvalidRefreshStrategy)
	}
	if e.AccountBinding == "" {
		return fmt.Errorf("client enrollment %s account_binding: %w", e.ID, ErrEmptyField)
	}
	return nil
}

// EnrollmentGeneration is one immutable entry in an enrollment's append-only
// store history: every successful store mutation (login, refresh,
// re-enrollment) appends one and changes no agent (§5.4). Admission records
// the generation it mounted, so a run is bound to the exact store bytes it
// consumed, not to whatever the store holds later.
type EnrollmentGeneration struct {
	EnrollmentID ClientEnrollmentID `json:"enrollment_id"`
	// Ordinal is the 1-based append position: zero on an entry that has not
	// been persisted yet, and the row identity once it has. The store stamps
	// it at append and range-checks it at reconstruction; a caller-supplied
	// value is never trusted (the BackendConformance generation posture).
	Ordinal int `json:"ordinal"`
	// AuthStoreVolume is the exact store locator this generation's bytes live
	// at: the fact that moved here from AuthIdentity, because each enrollment
	// binds its own sanitized single-route store.
	AuthStoreVolume     string `json:"auth_store_volume"`
	StoreManifestDigest Digest `json:"store_manifest_digest"`
	// LeaseFence names the identity-lease fence the mutation that produced
	// this generation held (§5.4: the one lease fences every enrollment's
	// store, and every fence names the exact store it guarded).
	LeaseFence     int64  `json:"lease_fence"`
	AccountBinding string `json:"account_binding"`
	// TokenExpiry is present exactly where the enrollment's auth method
	// exposes one (§5.4 admission step 4): the Codex and pi OAuth tokens do,
	// the Claude setup token does not, and an expiry-less generation under
	// such a method is valid by design — no refresh exists, and an
	// authentication failure at use fails the attempt closed.
	TokenExpiry *time.Time `json:"token_expiry"`
	RecordedAt  time.Time  `json:"recorded_at"`
}

// Validate reports whether the generation entry is well-formed. Whether an
// expiry belongs on it depends on the enrollment's auth method, which this
// value does not carry; ValidateEnrollmentGenerationBinding holds that rule.
func (g EnrollmentGeneration) Validate() error {
	if g.EnrollmentID == "" {
		return fmt.Errorf("enrollment generation enrollment_id: %w", ErrEmptyID)
	}
	// Ordinal zero means not yet persisted; the store stamps and range-checks
	// the persisted value itself, so only a negative value is malformed.
	if g.Ordinal < 0 {
		return fmt.Errorf("enrollment generation %s ordinal %d: %w", g.EnrollmentID, g.Ordinal, ErrNonPositive)
	}
	if g.AuthStoreVolume == "" {
		return fmt.Errorf("enrollment generation %s auth_store_volume: %w", g.EnrollmentID, ErrEmptyField)
	}
	if !contentaddr.Valid(string(g.StoreManifestDigest)) {
		return fmt.Errorf("enrollment generation %s store_manifest_digest %q: %w",
			g.EnrollmentID, g.StoreManifestDigest, ErrInvalidDigest)
	}
	if g.LeaseFence < 1 {
		return fmt.Errorf("enrollment generation %s lease_fence %d: %w", g.EnrollmentID, g.LeaseFence, ErrNonPositive)
	}
	if g.AccountBinding == "" {
		return fmt.Errorf("enrollment generation %s account_binding: %w", g.EnrollmentID, ErrEmptyField)
	}
	if g.TokenExpiry != nil {
		if g.TokenExpiry.IsZero() {
			return fmt.Errorf("enrollment generation %s token_expiry: %w", g.EnrollmentID, ErrMissingTimestamp)
		}
		if g.TokenExpiry.Location() != time.UTC {
			return fmt.Errorf("enrollment generation %s token_expiry: %w", g.EnrollmentID, ErrTimestampNotUTC)
		}
	}
	if g.RecordedAt.IsZero() {
		return fmt.Errorf("enrollment generation %s recorded_at: %w", g.EnrollmentID, ErrMissingTimestamp)
	}
	if g.RecordedAt.Location() != time.UTC {
		return fmt.Errorf("enrollment generation %s recorded_at: %w", g.EnrollmentID, ErrTimestampNotUTC)
	}
	return nil
}

// ValidateEnrollmentIdentityBinding reports whether an enrollment belongs to
// the identity it names, under the account rule revision 36 hardened and §5.4
// keeps: the identity must carry an account binding, and the enrollment must
// carry exactly that one. Both directions fail closed — an unbound identity
// cannot hold enrollments (adoption binds the account first), and an
// enrollment can never re-home a credential onto a different account.
func ValidateEnrollmentIdentityBinding(identity AuthIdentity, e ClientEnrollment) error {
	if e.AuthIdentityID != identity.ID {
		return fmt.Errorf("client enrollment %s names identity %s, bound to %s: %w",
			e.ID, e.AuthIdentityID, identity.ID, ErrParentKeyMismatch)
	}
	if identity.AccountBinding == "" {
		return fmt.Errorf("client enrollment %s under identity %s with no account binding: %w",
			e.ID, identity.ID, ErrAccountBindingMismatch)
	}
	if e.AccountBinding != identity.AccountBinding {
		return fmt.Errorf("client enrollment %s account %q, identity %s account %q: %w",
			e.ID, e.AccountBinding, identity.ID, identity.AccountBinding, ErrAccountBindingMismatch)
	}
	return nil
}

// ValidateEnrollmentGenerationBinding reports whether a generation belongs to
// its enrollment: the parent key, the same account binding, and an expiry
// exactly where the enrollment's auth method exposes one. A generation
// recording an expiry the method cannot observe would be an invented fact;
// one omitting an observable expiry would defeat admission step 4's margin
// check (§5.4).
func ValidateEnrollmentGenerationBinding(e ClientEnrollment, g EnrollmentGeneration) error {
	if g.EnrollmentID != e.ID {
		return fmt.Errorf("enrollment generation names enrollment %s, bound to %s: %w",
			g.EnrollmentID, e.ID, ErrParentKeyMismatch)
	}
	if g.AccountBinding != e.AccountBinding {
		return fmt.Errorf("enrollment generation %s ordinal %d account %q, enrollment account %q: %w",
			g.EnrollmentID, g.Ordinal, g.AccountBinding, e.AccountBinding, ErrAccountBindingMismatch)
	}
	if e.AuthMethod.ExpiryObservable() {
		if g.TokenExpiry == nil {
			return fmt.Errorf("enrollment generation %s ordinal %d under auth method %q records no expiry: %w",
				g.EnrollmentID, g.Ordinal, e.AuthMethod, ErrGenerationExpiryInconsistent)
		}
	} else if g.TokenExpiry != nil {
		return fmt.Errorf("enrollment generation %s ordinal %d records an expiry auth method %q cannot observe: %w",
			g.EnrollmentID, g.Ordinal, e.AuthMethod, ErrGenerationExpiryInconsistent)
	}
	return nil
}

// ValidateGenerationExpiryMargin is admission step 4's credential window gate
// (§5.4): where the auth method observes expiry, the mounted generation's
// token must outlive the attempt deadline plus margin, and an admission
// against a shorter window fails closed with a typed error. Where the method
// observes no expiry there is nothing to check here — the daemon refreshes
// first where a refresh exists, and an authentication failure at use fails
// the attempt closed.
func ValidateGenerationExpiryMargin(
	e ClientEnrollment, g EnrollmentGeneration, deadline time.Time, margin time.Duration,
) error {
	if err := ValidateEnrollmentGenerationBinding(e, g); err != nil {
		return err
	}
	if !e.AuthMethod.ExpiryObservable() {
		return nil
	}
	if deadline.IsZero() {
		return fmt.Errorf("enrollment generation %s ordinal %d attempt deadline: %w",
			g.EnrollmentID, g.Ordinal, ErrMissingTimestamp)
	}
	if margin < 0 {
		return fmt.Errorf("enrollment generation %s ordinal %d expiry margin %s: %w",
			g.EnrollmentID, g.Ordinal, margin, ErrNonPositive)
	}
	if g.TokenExpiry.Before(deadline.Add(margin)) {
		return fmt.Errorf("enrollment generation %s ordinal %d expires %s, attempt deadline %s with margin %s: %w",
			g.EnrollmentID, g.Ordinal, g.TokenExpiry.Format(time.RFC3339Nano),
			deadline.Format(time.RFC3339Nano), margin, ErrGenerationExpiryInsufficient)
	}
	return nil
}

// HarnessClientKind names one harness client an enrollment binds a credential
// to (§5.4): the executable OAuth/token client, not the provider or the
// model. The two registered members are the harnesses whose behaviour the
// admitted-agent contract generalizes from; the pi adapter unit registers its
// own when it lands.
type HarnessClientKind string

const (
	HarnessClientClaudeCode HarnessClientKind = "claude_code"
	HarnessClientCodexCLI   HarnessClientKind = "codex_cli"
)

// AllHarnessClientKinds lists every valid HarnessClientKind.
var AllHarnessClientKinds = []HarnessClientKind{
	HarnessClientClaudeCode, HarnessClientCodexCLI,
}

func (k HarnessClientKind) valid() bool {
	switch k {
	case HarnessClientClaudeCode, HarnessClientCodexCLI:
		return true
	default:
		return false
	}
}

// AuthMethod names how an enrollment's credential authenticates, which is
// what decides whether a token expiry is observable (§5.4 admission step 4):
// the Codex and pi OAuth tokens expose one, the Claude setup token does not.
type AuthMethod string

const (
	// AuthMethodSetupToken is the long-lived opaque setup token (the Claude
	// baseline): no observable expiry, no refresh; failure at use fails the
	// attempt closed and raises the revoked-identity marker.
	AuthMethodSetupToken AuthMethod = "setup_token"
	// AuthMethodOAuth is an OAuth token pair with an observable access-token
	// expiry and a daemon-owned refresh under the identity lease.
	AuthMethodOAuth AuthMethod = "oauth"
)

// AllAuthMethods lists every valid AuthMethod.
var AllAuthMethods = []AuthMethod{AuthMethodSetupToken, AuthMethodOAuth}

func (m AuthMethod) valid() bool {
	switch m {
	case AuthMethodSetupToken, AuthMethodOAuth:
		return true
	default:
		return false
	}
}

// ExpiryObservable reports whether generations under this method record a
// token expiry. The switch dispatches behaviour and omits default, so a new
// method has to decide its expiry stance here; the trailing return covers the
// invalid zero value.
func (m AuthMethod) ExpiryObservable() bool {
	switch m {
	case AuthMethodSetupToken:
		return false
	case AuthMethodOAuth:
		return true
	}
	return false
}
