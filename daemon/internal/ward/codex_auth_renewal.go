package ward

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Production admission and operator renewal must use the same lifetime gates.
const (
	CodexAuthProductionLifetimeFloor    = time.Hour
	CodexAuthProductionRefreshThreshold = 2 * time.Hour
)

// CodexAuthRenewalConfig identifies an existing, daemon-owned subscription
// store. Renewal cannot enroll an identity or accept a re-enrollment hold.
type CodexAuthRenewalConfig struct {
	AuthStoreRoot               string
	AuthStorePath               string
	AuthIdentityID              domain.AuthIdentityID
	AuthStoreLeaser             AuthStoreLeaser
	AuthState                   CodexAuthState
	AuthRefresher               CodexAuthRefresher
	Now                         func() time.Time
	LeaseDuration               time.Duration
	AccessTokenLifetimeFloor    time.Duration
	AccessTokenRefreshThreshold time.Duration
}

// CodexAuthRenewalResult exposes readiness coordinates, never credentials.
// Rotated includes a rotation recovered from an interrupted invocation.
type CodexAuthRenewalResult struct {
	AuthIdentityID       domain.AuthIdentityID `json:"auth_identity_id"`
	AuthStorePath        string                `json:"auth_store_path"`
	AuthStoreDigest      domain.Digest         `json:"auth_store_digest"`
	AccessTokenExpiresAt time.Time             `json:"access_token_expires_at"`
	Rotated              bool                  `json:"rotated"`
}

// RenewCodexAuth uses review launch's durable refresh transaction under the
// identity's mutation lease. A revoked or ambiguous chain requires enrollment;
// renewal never creates or clears the run-bound re-enrollment marker.
func RenewCodexAuth(ctx context.Context, cfg CodexAuthRenewalConfig) (_ CodexAuthRenewalResult, retErr error) {
	if cfg.AuthIdentityID == "" || cfg.AuthStoreLeaser == nil || cfg.AuthState == nil || cfg.AuthRefresher == nil {
		return CodexAuthRenewalResult{}, errors.New("codex renewal requires an identity, lease store, auth state, and refresher")
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.LeaseDuration == 0 {
		cfg.LeaseDuration = defaultCodexAuthEnrollmentLeaseDuration
	}
	if cfg.AccessTokenLifetimeFloor == 0 {
		cfg.AccessTokenLifetimeFloor = CodexAuthProductionLifetimeFloor
	}
	if cfg.AccessTokenRefreshThreshold == 0 {
		cfg.AccessTokenRefreshThreshold = CodexAuthProductionRefreshThreshold
	}
	if cfg.LeaseDuration < 0 || cfg.AccessTokenLifetimeFloor < 0 || cfg.AccessTokenRefreshThreshold < cfg.AccessTokenLifetimeFloor {
		return CodexAuthRenewalResult{}, errors.New("invalid Codex renewal lease or lifetime gates")
	}
	root, err := resolvePrivateCodexAuthRoot(cfg.AuthStoreRoot)
	if err != nil {
		return CodexAuthRenewalResult{}, err
	}
	path, err := resolveCodexAuthStoreTarget(root, cfg.AuthStorePath)
	if err != nil {
		return CodexAuthRenewalResult{}, err
	}
	needs, err := cfg.AuthState.NeedsCodexAuthReenrollment(ctx, cfg.AuthIdentityID)
	if err != nil {
		return CodexAuthRenewalResult{}, fmt.Errorf("inspect Codex re-enrollment hold: %w", err)
	}
	if needs {
		return CodexAuthRenewalResult{}, errors.New("codex identity has a re-enrollment hold; run scripts/run-real-work.sh --recover-codex-credentials and resolve it from a paired client")
	}
	identity, err := cfg.AuthStoreLeaser.GetIdentity(ctx, cfg.AuthIdentityID)
	if err != nil {
		return CodexAuthRenewalResult{}, err
	}
	if err := identity.Validate(); err != nil || identity.ID != cfg.AuthIdentityID ||
		identity.Provider != "openai" || !identity.AuthStoreMutationLease ||
		!identity.Interim.SupportsReadOnlyAuthSnapshot || identity.Interim.RefreshStrategy != domain.RefreshOnDemand ||
		identity.Interim.AuthStoreVolume != path {
		return CodexAuthRenewalResult{}, errors.New("codex identity cannot renew the configured leased auth store; see freesided enroll-codex")
	}
	owner, err := newOwnershipLabel()
	if err != nil {
		return CodexAuthRenewalResult{}, errors.New("mint Codex renewal lease holder")
	}
	holder := domain.InvocationID("codex-auth-renewal-" + owner.Value)
	now := cfg.Now()
	lease, err := cfg.AuthStoreLeaser.Acquire(ctx, cfg.AuthIdentityID, holder, now, now.Add(cfg.LeaseDuration))
	if err != nil {
		return CodexAuthRenewalResult{}, err
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultCodexAuthEnrollmentTeardown)
		defer cancel()
		err := cfg.AuthStoreLeaser.Release(releaseCtx, cfg.AuthIdentityID, holder, lease.Fence, cfg.Now())
		if err != nil && !errors.Is(err, ErrLeaseWindowEnded) {
			retErr = errors.Join(retErr, fmt.Errorf("release Codex renewal lease: %w", err))
		}
	}()
	refreshCfg := CodexReviewConfig{
		InputRoot: root, Now: cfg.Now, AuthStoreLeaser: cfg.AuthStoreLeaser, AuthRefresher: cfg.AuthRefresher,
		AccessTokenLifetimeFloor: cfg.AccessTokenLifetimeFloor, AccessTokenRefreshThreshold: cfg.AccessTokenRefreshThreshold,
	}
	if err := verifyCodexAuthRefreshLease(ctx, refreshCfg, lease, cfg.AuthIdentityID, holder); err != nil {
		return CodexAuthRenewalResult{}, err
	}
	_, body, metadata, err := readCodexReviewInputWithMetadata(root, path, maxCodexAuthSnapshotBytes)
	if err != nil {
		return CodexAuthRenewalResult{}, err
	}
	mutate, err := codexAuthLeaseMutationGuard(ctx, refreshCfg, lease)
	if err != nil {
		return CodexAuthRenewalResult{}, err
	}
	rotated, err := recoverCodexAuthRefreshTransactionUnderLease(root, path, cfg.AuthIdentityID, body, metadata, cfg.AccessTokenRefreshThreshold, false, mutate)
	if err != nil {
		return CodexAuthRenewalResult{}, codexAuthRenewalRecoveryError(err)
	}
	_, body, metadata, err = readCodexReviewInputWithMetadata(root, path, maxCodexAuthSnapshotBytes)
	if err != nil {
		return CodexAuthRenewalResult{}, err
	}
	auth, expires, err := inspectCodexHostAuth(CodexAuthSubscription, body)
	if err != nil {
		return CodexAuthRenewalResult{}, codexAuthRenewalRecoveryError(err)
	}
	if expires == nil || expires.Sub(cfg.Now()) < cfg.AccessTokenRefreshThreshold {
		body, _, err = rotateCodexAuthStoreUnderLease(ctx, refreshCfg, cfg.AuthIdentityID, path, body, metadata, auth, lease, holder, false)
		if err != nil {
			return CodexAuthRenewalResult{}, codexAuthRenewalRecoveryError(err)
		}
		rotated = true
	}
	_, expires, err = InspectCodexAuthReadiness(root, path, CodexAuthSubscription, cfg.AuthIdentityID, cfg.Now(), cfg.AccessTokenLifetimeFloor, cfg.AccessTokenRefreshThreshold, true)
	if err != nil {
		return CodexAuthRenewalResult{}, err
	}
	if err := verifyCodexAuthRefreshLease(ctx, refreshCfg, lease, cfg.AuthIdentityID, holder); err != nil {
		return CodexAuthRenewalResult{}, err
	}
	return CodexAuthRenewalResult{
		AuthIdentityID: cfg.AuthIdentityID, AuthStorePath: path, AuthStoreDigest: domain.Digest(contentaddr.Sum(body)),
		AccessTokenExpiresAt: expires.UTC(), Rotated: rotated,
	}, nil
}

func codexAuthRenewalRecoveryError(err error) error {
	return fmt.Errorf("codex renewal failed: %w; rerun renew-codex to recover a pending rotation; if the chain is rejected or unavailable, use codex login then freesided enroll-codex and scripts/run-real-work.sh --recover-codex-credentials", err)
}
