package ward

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// ErrUnadoptable marks an identity whose existing store cannot back an
// enrollment (plan §5.4): the store yields no account binding, or its account
// already belongs to another identity. Adoption records nothing for it, and
// the cutover retires it. Every other failure is an ordinary error the
// operator fixes and retries.
var ErrUnadoptable = errors.New("auth identity cannot be adopted")

// AuthAdoptionStore is the persistence port for adopting a flag-era
// identity. Begin and AppendGeneration are the first-enrollment writes
// `auth add` uses; Enrolled finds an enrollment the identity already holds.
type AuthAdoptionStore interface {
	ClaudeAuthEnrollmentStore
	// Enrolled returns the identity's enrollment for the client and its
	// current generation. found is false when there is no such enrollment or
	// it holds no generation yet; more than one enrollment for the client is
	// an error, because adoption cannot choose between them.
	Enrolled(
		ctx context.Context, identity domain.AuthIdentityID, client domain.HarnessClientKind,
	) (enrollment domain.ClientEnrollment, generation domain.EnrollmentGeneration, found bool, err error)
}

// AuthAdoptionConfig is what both adoption paths share. Identity is the
// stored declaration: adoption enrolls the store its interim facts already
// name, and writes no store bytes.
type AuthAdoptionConfig struct {
	Identity     domain.AuthIdentity
	EnrollmentID domain.ClientEnrollmentID
	Route        string
	CostOwner    string

	Store           AuthAdoptionStore
	AuthStoreLeaser AuthStoreLeaser
	Now             func() time.Time

	LeaseDuration   time.Duration
	TeardownTimeout time.Duration
}

// AuthAdoptionResult names the enrollment an adopted identity now runs under.
type AuthAdoptionResult struct {
	Enrollment domain.ClientEnrollment     `json:"enrollment"`
	Generation domain.EnrollmentGeneration `json:"generation"`
	// Reused is true when the identity was already enrolled and adoption
	// appended nothing.
	Reused bool `json:"reused"`
}

// ClaudeAuthAdoptionConfig adopts a flag-era Claude identity's existing
// setup-token volume.
type ClaudeAuthAdoptionConfig struct {
	AuthAdoptionConfig
	// AccountBinding is the operator's attestation of the subscription
	// account: a setup-token store carries no account of its own.
	AccountBinding string

	// ObserveVolume proves the volume's setup-token manifest and returns its
	// tree digest; production passes ObserveSetupTokenVolume.
	ObserveVolume func(ctx context.Context, volume string) (domain.Digest, error)
}

// ObserveSetupTokenVolume returns the observation AdoptClaudeAuth records:
// the networkless exporter proof over an existing setup-token volume, as the
// content address of its complete tree.
func ObserveSetupTokenVolume(
	runtime Runtime, exporterImage string, authorize RuntimeResourceAuthorizer,
) func(context.Context, string) (domain.Digest, error) {
	return func(ctx context.Context, volume string) (domain.Digest, error) {
		// The observer runs this image with the credential volume mounted,
		// so it is trusted compute under the same pin rule as ward.Config.
		if !digestPinnedImagePattern.MatchString(exporterImage) {
			return "", errors.New("exporter image is not digest-pinned")
		}
		hexDigest, err := observeCredentialVolume(
			ctx, runtime, exporterImage, volume, CredentialManifestSetupToken, authorize,
		)
		if err != nil {
			return "", err
		}
		digest, ok := contentaddr.FromHex(hexDigest)
		if !ok {
			return "", errors.New("credential volume proof carries no tree digest")
		}
		return domain.Digest(digest), nil
	}
}

// AdoptClaudeAuth enrolls the Claude Code client over the identity's existing
// credential volume. Unlike EnrollClaudeSetupToken it captures no token and
// authors no volume: generation one records the volume as it stands, under
// the tree digest the manifest observer proves.
func AdoptClaudeAuth(ctx context.Context, cfg ClaudeAuthAdoptionConfig) (AuthAdoptionResult, error) {
	if err := normalizeAuthAdoptionConfig(&cfg.AuthAdoptionConfig); err != nil {
		return AuthAdoptionResult{}, err
	}
	volume := cfg.Identity.Interim.AuthStoreVolume
	if cfg.Identity.Provider != "claude" || volume == "" || !cfg.Identity.AuthStoreMutationLease {
		return AuthAdoptionResult{}, fmt.Errorf(
			"auth identity %s is not a leased Claude identity with a credential volume", cfg.Identity.ID)
	}
	if reused, found, err := reuseEnrollment(ctx, cfg.AuthAdoptionConfig, domain.HarnessClientClaudeCode); err != nil || found {
		return reused, err
	}
	if cfg.AccountBinding == "" {
		return AuthAdoptionResult{}, fmt.Errorf(
			"auth identity %s: a setup-token store carries no account and none was attested: %w",
			cfg.Identity.ID, ErrUnadoptable)
	}
	if cfg.ObserveVolume == nil {
		return AuthAdoptionResult{}, errors.New("claude auth adoption needs a volume observer")
	}
	digest, err := cfg.ObserveVolume(ctx, volume)
	if err != nil {
		return AuthAdoptionResult{}, fmt.Errorf("credential volume %q failed its manifest proof: %w", volume, err)
	}
	if !contentaddr.Valid(string(digest)) {
		return AuthAdoptionResult{}, errors.New("credential volume observation is not a content address")
	}
	return adoptStore(ctx, cfg.AuthAdoptionConfig, domain.ClientEnrollment{
		ID: cfg.EnrollmentID, AuthIdentityID: cfg.Identity.ID,
		HarnessClient: domain.HarnessClientClaudeCode, Route: cfg.Route,
		AuthMethod:      domain.AuthMethodSetupToken,
		CredentialMode:  domain.CredentialSubscriptionContained,
		RefreshStrategy: domain.RefreshExternal, SupportsReadOnlyAuthSnapshot: true,
		AccountBinding: cfg.AccountBinding,
	}, volume, digest, nil, "claude-auth-adoption-")
}

// CodexAuthAdoptionConfig adopts a flag-era Codex identity's live store.
type CodexAuthAdoptionConfig struct {
	AuthAdoptionConfig
	// AuthStoreRoot is the private directory the identity's store lives in.
	AuthStoreRoot string
}

// AdoptCodexAuth enrolls the Codex CLI client over the identity's existing
// live store. Unlike EnrollCodexAuth it replaces and refreshes nothing:
// generation one records the store as it stands, bound to the account the
// store's own tokens name.
func AdoptCodexAuth(ctx context.Context, cfg CodexAuthAdoptionConfig) (AuthAdoptionResult, error) {
	if err := normalizeAuthAdoptionConfig(&cfg.AuthAdoptionConfig); err != nil {
		return AuthAdoptionResult{}, err
	}
	interim := cfg.Identity.Interim
	if cfg.Identity.Provider != "openai" || interim.AuthStoreVolume == "" || !cfg.Identity.AuthStoreMutationLease {
		return AuthAdoptionResult{}, fmt.Errorf(
			"auth identity %s is not a leased Codex identity with a live store", cfg.Identity.ID)
	}
	if reused, found, err := reuseEnrollment(ctx, cfg.AuthAdoptionConfig, domain.HarnessClientCodexCLI); err != nil || found {
		return reused, err
	}
	storeRoot, err := resolvePrivateCodexAuthRoot(cfg.AuthStoreRoot)
	if err != nil {
		return AuthAdoptionResult{}, fmt.Errorf("auth-store root: %w", err)
	}
	storePath, err := resolveCodexAuthStoreTarget(storeRoot, interim.AuthStoreVolume)
	if err != nil {
		return AuthAdoptionResult{}, err
	}
	if storePath != interim.AuthStoreVolume {
		return AuthAdoptionResult{}, fmt.Errorf(
			"auth identity %s: its recorded store path does not resolve to itself", cfg.Identity.ID)
	}
	_, body, err := readCodexReviewInput(storeRoot, storePath, maxCodexAuthSnapshotBytes)
	if err != nil {
		return AuthAdoptionResult{}, fmt.Errorf("read live auth store: %w", err)
	}
	auth, expiresAt, err := inspectCodexHostAuth(CodexAuthSubscription, body)
	if err != nil || expiresAt == nil || auth.Tokens == nil ||
		auth.Tokens.AccountID == nil || *auth.Tokens.AccountID == "" {
		// The cause stays out of the message's wrap chain on purpose: it
		// describes store contents, and the sentinel is what callers match.
		return AuthAdoptionResult{}, fmt.Errorf(
			"auth identity %s: its store is not a subscription login that names its account: %w",
			cfg.Identity.ID, ErrUnadoptable)
	}
	expiry := expiresAt.UTC()
	return adoptStore(ctx, cfg.AuthAdoptionConfig, domain.ClientEnrollment{
		ID: cfg.EnrollmentID, AuthIdentityID: cfg.Identity.ID,
		HarnessClient: domain.HarnessClientCodexCLI, Route: cfg.Route,
		AuthMethod:      domain.AuthMethodOAuth,
		CredentialMode:  domain.CredentialSubscriptionContained,
		RefreshStrategy: interim.RefreshStrategy, SupportsReadOnlyAuthSnapshot: interim.SupportsReadOnlyAuthSnapshot,
		AccountBinding: *auth.Tokens.AccountID,
	}, storePath, domain.Digest(contentaddr.Sum(body)), &expiry, "codex-auth-adoption-")
}

// reuseEnrollment returns the enrollment the identity already holds for the
// client. An enrolled identity keeps its enrollment and adoption appends
// nothing, but the cost owner given must still be the stored one.
func reuseEnrollment(
	ctx context.Context, cfg AuthAdoptionConfig, client domain.HarnessClientKind,
) (AuthAdoptionResult, bool, error) {
	enrollment, generation, found, err := cfg.Store.Enrolled(ctx, cfg.Identity.ID, client)
	if err != nil || !found {
		return AuthAdoptionResult{}, false, err
	}
	if cfg.Identity.CostOwner != cfg.CostOwner {
		return AuthAdoptionResult{}, false, fmt.Errorf("auth identity %s has cost owner %q, not %q",
			cfg.Identity.ID, cfg.Identity.CostOwner, cfg.CostOwner)
	}
	return AuthAdoptionResult{Enrollment: enrollment, Generation: generation, Reused: true}, true, nil
}

// adoptStore records the enrollment and its first generation over a store
// that already exists. Begin binds the account and cost owner onto the
// identity, records the enrollment, and takes the bound lease in one
// transaction, so an account another identity holds records nothing.
func adoptStore(
	ctx context.Context, cfg AuthAdoptionConfig, enrollment domain.ClientEnrollment,
	volume string, digest domain.Digest, expiry *time.Time, holderPrefix string,
) (_ AuthAdoptionResult, retErr error) {
	identity := cfg.Identity
	identity.AccountBinding = enrollment.AccountBinding
	identity.CostOwner = cfg.CostOwner
	owner, err := newOwnershipLabel()
	if err != nil {
		return AuthAdoptionResult{}, errors.New("mint auth adoption holder")
	}
	holder := domain.InvocationID(holderPrefix + owner.Value)
	now := cfg.Now()
	lease, err := cfg.Store.Begin(ctx, identity, EnrollmentBootstrap{
		Enrollment: enrollment,
		Binding: domain.LeaseGenerationBinding{
			EnrollmentID: enrollment.ID, AuthStoreVolume: volume, StoreManifestDigest: digest,
		},
	}, holder, now, now.Add(cfg.LeaseDuration))
	if errors.Is(err, domain.ErrAccountBindingTaken) {
		// The store's error names the account binding, and this message is
		// reported, so it is replaced rather than wrapped.
		return AuthAdoptionResult{}, fmt.Errorf(
			"auth identity %s: its account is already bound to another identity: %w", cfg.Identity.ID, ErrUnadoptable)
	}
	if err != nil {
		return AuthAdoptionResult{}, fmt.Errorf("begin auth adoption: %w", err)
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.TeardownTimeout)
		defer cancel()
		releaseErr := cfg.AuthStoreLeaser.Release(releaseCtx, cfg.Identity.ID, holder, lease.Fence, cfg.Now())
		if releaseErr != nil && !errors.Is(releaseErr, ErrLeaseWindowEnded) {
			retErr = errors.Join(retErr, fmt.Errorf("release auth adoption lease: %w", releaseErr))
		}
	}()
	generation, err := cfg.Store.AppendGeneration(ctx, domain.EnrollmentGeneration{
		EnrollmentID: enrollment.ID, AuthStoreVolume: volume,
		StoreManifestDigest: digest, LeaseFence: lease.Fence,
		AccountBinding: enrollment.AccountBinding, TokenExpiry: expiry, RecordedAt: cfg.Now(),
	}, cfg.Now())
	if err != nil {
		return AuthAdoptionResult{}, fmt.Errorf("record adopted enrollment generation: %w", err)
	}
	return AuthAdoptionResult{Enrollment: enrollment, Generation: generation}, nil
}

func normalizeAuthAdoptionConfig(cfg *AuthAdoptionConfig) error {
	if cfg.Identity.ID == "" || cfg.EnrollmentID == "" || cfg.Route == "" || cfg.CostOwner == "" ||
		cfg.Store == nil || cfg.AuthStoreLeaser == nil {
		return errors.New("auth adoption configuration is incomplete")
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.LeaseDuration == 0 {
		cfg.LeaseDuration = defaultClaudeAuthEnrollmentLeaseDuration
	}
	if cfg.TeardownTimeout == 0 {
		cfg.TeardownTimeout = defaultClaudeAuthEnrollmentTeardown
	}
	if cfg.LeaseDuration <= 0 || cfg.TeardownTimeout <= 0 {
		return errors.New("auth adoption durations are invalid")
	}
	if now := cfg.Now(); now.IsZero() || now.Location() != time.UTC {
		return errors.New("auth adoption clock must return nonzero UTC instants")
	}
	return nil
}
