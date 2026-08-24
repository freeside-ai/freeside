package ward

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/atomicfile"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const (
	codexAuthRefreshEndpoint      = "https://auth.openai.com/oauth/token"
	codexAuthRefreshClientID      = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexAuthRefreshLeaseDuration = 2 * time.Minute
	codexAuthRefreshResponseLimit = 1 << 20
	codexAuthStartTimeout         = 30 * time.Second
	codexAuthStartLeaseMargin     = time.Second
)

var (
	// ErrCodexAuthNeedsReenrollment is the durable configuration state raised
	// when a refresh chain can no longer be used safely.
	ErrCodexAuthNeedsReenrollment   = errors.New("codex auth identity needs re-enrollment")
	errCodexAuthRefreshIntentExists = errors.New("codex auth refresh intent already exists")
	// errCodexAuthRefreshPredecessorMayBeConsumed marks the only recovery
	// state an explicit re-enrollment may replace: the previous holder wrote
	// its durable predecessor intent but never persisted a response.  The
	// caller must still hold the exact mutation lease before removing it.
	errCodexAuthRefreshPredecessorMayBeConsumed = errors.New("codex auth refresh may have consumed its predecessor")
)

// CodexAuthRefreshTokens is the vendor's rotation result. Fields are never
// formatted into errors or logs; only the host-store transaction consumes it.
type CodexAuthRefreshTokens struct {
	IDToken      string
	AccessToken  string
	RefreshToken string
}

// CodexAuthRefresher is the host-only refresh boundary. The production client
// reaches auth.openai.com directly; the reviewer network never receives that
// endpoint.
type CodexAuthRefresher interface {
	RefreshCodexAuth(ctx context.Context, refreshToken string) (CodexAuthRefreshTokens, error)
}

// CodexAuthState owns the durable, identity-scoped re-enrollment marker.
// Implementations must authenticate the marker's complete AttentionItem shape,
// not infer identity from its free-text reason.
type CodexAuthState interface {
	NeedsCodexAuthReenrollment(ctx context.Context, id domain.AuthIdentityID) (bool, error)
	MarkCodexAuthNeedsReenrollment(
		ctx context.Context, runID domain.RunID, id domain.AuthIdentityID,
	) error
}

// CodexAuthRefreshError classifies a provider refusal. Error deliberately
// renders only the status: implementations outside this package can construct
// the exported value, so Code is data for classification, never safe text.
type CodexAuthRefreshError struct {
	StatusCode int
	Code       string
	Revoked    bool
}

func (e *CodexAuthRefreshError) Error() string {
	return fmt.Sprintf("Codex auth refresh failed with status %d", e.StatusCode)
}

type codexAuthHTTPRefresher struct {
	client   *http.Client
	endpoint string
}

// NewCodexAuthHTTPRefresher returns the pinned Codex 0.147.0 refresh client.
// The request shape and public OAuth client id mirror that CLI version.
func NewCodexAuthHTTPRefresher() CodexAuthRefresher {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &codexAuthHTTPRefresher{
		client: &http.Client{
			Transport: transport, Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		endpoint: codexAuthRefreshEndpoint,
	}
}

func (r *codexAuthHTTPRefresher) RefreshCodexAuth(
	ctx context.Context, refreshToken string,
) (CodexAuthRefreshTokens, error) {
	if r == nil || r.client == nil || r.endpoint == "" || refreshToken == "" {
		return CodexAuthRefreshTokens{}, errors.New("codex auth refresher is not configured")
	}
	body, err := json.Marshal(struct { //nolint:gosec // this host-only OAuth request intentionally carries the refresh credential
		ClientID  string `json:"client_id"`
		GrantType string `json:"grant_type"`
		Token     string `json:"refresh_token"`
	}{ClientID: codexAuthRefreshClientID, GrantType: "refresh_token", Token: refreshToken})
	if err != nil {
		return CodexAuthRefreshTokens{}, errors.New("encode Codex auth refresh request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(body))
	if err != nil {
		return CodexAuthRefreshTokens{}, errors.New("construct Codex auth refresh request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return CodexAuthRefreshTokens{}, fmt.Errorf("send Codex auth refresh request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // response teardown cannot change the refresh result
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, codexAuthRefreshResponseLimit+1))
	if err != nil {
		return CodexAuthRefreshTokens{}, errors.New("read Codex auth refresh response")
	}
	if len(responseBody) > codexAuthRefreshResponseLimit {
		return CodexAuthRefreshTokens{}, errors.New("codex auth refresh response exceeds its byte limit")
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		code, revoked := classifyCodexAuthRefreshFailure(responseBody)
		return CodexAuthRefreshTokens{}, &CodexAuthRefreshError{
			StatusCode: resp.StatusCode, Code: code, Revoked: revoked,
		}
	}
	var result struct {
		IDToken      *string `json:"id_token"`
		AccessToken  *string `json:"access_token"`
		RefreshToken *string `json:"refresh_token"`
	}
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return CodexAuthRefreshTokens{}, errors.New("decode Codex auth refresh response")
	}
	if result.AccessToken == nil || *result.AccessToken == "" ||
		result.RefreshToken == nil || *result.RefreshToken == "" {
		return CodexAuthRefreshTokens{}, &CodexAuthRefreshError{
			StatusCode: resp.StatusCode, Code: "incomplete_rotation", Revoked: true,
		}
	}
	tokens := CodexAuthRefreshTokens{
		AccessToken: *result.AccessToken, RefreshToken: *result.RefreshToken,
	}
	if result.IDToken != nil {
		tokens.IDToken = *result.IDToken
	}
	return tokens, nil
}

func classifyCodexAuthRefreshFailure(body []byte) (string, bool) {
	var envelope struct {
		Error any    `json:"error"`
		Code  string `json:"code"`
	}
	_ = json.Unmarshal(body, &envelope)
	code := strings.ToLower(strings.TrimSpace(envelope.Code))
	switch value := envelope.Error.(type) {
	case string:
		if code == "" {
			code = strings.ToLower(strings.TrimSpace(value))
		}
	case map[string]any:
		if nested, ok := value["code"].(string); ok {
			code = strings.ToLower(strings.TrimSpace(nested))
		}
	}
	revoked := false
	switch code {
	case "invalid_grant", "invalid_token", "refresh_token_expired",
		"refresh_token_reused", "refresh_token_invalidated":
		revoked = true
	default:
		// An untrusted response cannot choose text that will be formatted into
		// host errors. Unknown vendor codes retain their HTTP status only.
		code = ""
	}
	text := strings.ToLower(string(body))
	if strings.Contains(text, "refresh token was already used") ||
		strings.Contains(text, "refresh token has already been used") ||
		strings.Contains(text, "refresh token was revoked") ||
		strings.Contains(text, "refresh token has expired") {
		revoked = true
	}
	return code, revoked
}

func codexAuthRefreshThreshold(cfg CodexReviewConfig) time.Duration {
	if cfg.AccessTokenRefreshThreshold != 0 {
		return cfg.AccessTokenRefreshThreshold
	}
	return 2 * cfg.AccessTokenLifetimeFloor
}

type codexAuthLeaseGuard struct {
	cfg           CodexReviewConfig
	lease         domain.AuthStoreMutationLease
	holder        domain.InvocationID
	leaseDuration time.Duration
}

func (g *codexAuthLeaseGuard) reserveStart(
	ctx context.Context,
) (context.Context, context.CancelFunc, error) {
	if g == nil {
		startCtx, cancel := context.WithCancel(ctx)
		return startCtx, cancel, nil
	}
	now := g.cfg.Now()
	expiresAt := now.Add(g.leaseDuration)
	if expiresAt.Before(g.lease.ExpiresAt) {
		expiresAt = g.lease.ExpiresAt
	}
	renewed, err := g.cfg.AuthStoreLeaser.Renew(
		ctx, g.lease.AuthIdentityID, g.holder, g.lease.Fence, now, expiresAt,
	)
	if err != nil {
		return nil, nil, codexReviewOperationalCheckf(
			CheckAuthStoreMutationLease,
			"renew Codex auth launch lease for identity %q: %v",
			g.lease.AuthIdentityID, err,
		)
	}
	g.lease = renewed
	if err := verifyCodexAuthRefreshLeaseWindow(
		ctx, g.cfg, g.lease, g.lease.AuthIdentityID, g.holder,
		codexAuthStartTimeout+codexAuthStartLeaseMargin,
	); err != nil {
		return nil, nil, err
	}
	// Capture the process clock before the lease clock. If the goroutine is
	// suspended while sampling the lease clock, the mapped wall deadline only
	// becomes more conservative; it can never slide later by that suspension.
	wallNow := time.Now()
	remaining := g.lease.ExpiresAt.Sub(g.cfg.Now()) - codexAuthStartLeaseMargin
	if remaining <= 0 {
		return nil, nil, failf(
			CheckAuthStoreMutationLease,
			"Codex auth launch lease for identity %q has no start window",
			g.lease.AuthIdentityID,
		)
	}
	budget := min(codexAuthStartTimeout, remaining)
	// Bind the absolute process-clock deadline before returning so scheduler
	// delay between this verified fence and Start consumes the reserved window.
	startCtx, cancel := context.WithDeadline(ctx, wallNow.Add(budget))
	return startCtx, cancel, nil
}

func (g *codexAuthLeaseGuard) verify(ctx context.Context) error {
	if g == nil {
		return nil
	}
	return verifyCodexAuthRefreshLease(
		ctx, g.cfg, g.lease, g.lease.AuthIdentityID, g.holder,
	)
}

func (b *CodexReviewLifecycle) prepareCodexReviewAuth(
	ctx context.Context, cfg CodexReviewConfig, launch CodexReviewLaunchSpec,
) error {
	guard, err := b.acquireCodexReviewAuth(ctx, cfg, launch)
	if err != nil {
		return err
	}
	return b.releaseCodexReviewAuthLease(ctx, guard)
}

func (b *CodexReviewLifecycle) acquireCodexReviewAuth(
	ctx context.Context, cfg CodexReviewConfig, launch CodexReviewLaunchSpec,
) (_ *codexAuthLeaseGuard, retErr error) {
	if launch.AuthMode != CodexAuthSubscription {
		return nil, nil
	}
	if err := checkCodexAuthReenrollment(ctx, cfg, launch); err != nil {
		return nil, err
	}
	if cfg.AuthStoreLeaser == nil || cfg.AuthRefresher == nil || cfg.AuthState == nil {
		return nil, fmt.Errorf("%w: subscription host refresh dependencies are required", ErrInvalidCodexReviewSpec)
	}
	identity, err := cfg.AuthStoreLeaser.GetIdentity(ctx, launch.AuthIdentityID)
	if err != nil {
		return nil, codexReviewOperationalCheckf(
			CheckAuthStoreMutationLease, "load Codex auth identity %q: %v", launch.AuthIdentityID, err,
		)
	}
	if err := identity.Validate(); err != nil || identity.ID != launch.AuthIdentityID ||
		identity.Provider != "openai" ||
		!identity.AuthStoreMutationLease || !identity.Interim.SupportsReadOnlyAuthSnapshot {
		return nil, fmt.Errorf(
			"%w: Codex auth identity %q cannot support lease-held snapshot refresh",
			ErrInvalidCodexReviewSpec, launch.AuthIdentityID,
		)
	}
	owner, err := newOwnershipLabel()
	if err != nil {
		return nil, codexReviewOperationalCheckf(
			CheckAuthStoreMutationLease, "mint Codex auth refresh lease holder: %v", err,
		)
	}
	now := cfg.Now()
	holder := domain.InvocationID("codex-auth-refresh-" + owner.Value)
	leaseDuration := max(codexAuthRefreshLeaseDuration, b.cfg.HandoffTimeout)
	lease, err := cfg.AuthStoreLeaser.Acquire(
		ctx, launch.AuthIdentityID, holder, now, now.Add(leaseDuration),
	)
	if err != nil {
		return nil, codexReviewOperationalCheckf(
			CheckAuthStoreMutationLease, "acquire Codex auth refresh lease for identity %q: %v",
			launch.AuthIdentityID, err,
		)
	}
	guard := &codexAuthLeaseGuard{
		cfg: cfg, lease: lease, holder: holder, leaseDuration: leaseDuration,
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, b.releaseCodexReviewAuthLease(ctx, guard))
		}
	}()
	if err := verifyCodexAuthRefreshLease(ctx, cfg, lease, launch.AuthIdentityID, holder); err != nil {
		return nil, err
	}

	resolvedPath, body, predecessorMetadata, err := readCodexReviewInputWithMetadata(
		cfg.InputRoot, launch.AuthSnapshot, maxCodexAuthSnapshotBytes,
	)
	if err != nil {
		return nil, fmt.Errorf("%w: auth snapshot: %w", ErrInvalidCodexReviewSpec, err)
	}
	if identity.Interim.AuthStoreVolume != resolvedPath {
		return nil, fmt.Errorf(
			"%w: Codex auth identity %q is not bound to the configured host store",
			ErrInvalidCodexReviewSpec, launch.AuthIdentityID,
		)
	}
	mutate, err := codexAuthLeaseMutationGuard(ctx, cfg, lease)
	if err != nil {
		return nil, err
	}
	if _, err := recoverCodexAuthRefreshTransactionUnderLease(
		cfg.InputRoot, resolvedPath, launch.AuthIdentityID, body, predecessorMetadata,
		codexAuthRefreshThreshold(cfg), false, mutate,
	); err != nil {
		return nil, b.markCodexAuthReenrollment(ctx, cfg, launch, err)
	}
	_, body, predecessorMetadata, err = readCodexReviewInputWithMetadata(
		cfg.InputRoot, resolvedPath, maxCodexAuthSnapshotBytes,
	)
	if err != nil {
		return nil, b.markCodexAuthReenrollment(ctx, cfg, launch, err)
	}
	auth, expires, err := inspectCodexHostAuth(launch.AuthMode, body)
	if err != nil {
		return nil, b.markCodexAuthReenrollment(ctx, cfg, launch, err)
	}
	now = cfg.Now()
	if expires != nil && expires.Sub(now) >= codexAuthRefreshThreshold(cfg) {
		return guard, nil
	}
	if identity.Interim.RefreshStrategy != domain.RefreshOnDemand {
		if expires != nil && expires.Sub(now) < cfg.AccessTokenLifetimeFloor {
			return nil, codexAuthLifetimeRefusal(cfg, launch.AuthIdentityID, *expires, now)
		}
		return guard, nil
	}
	if auth.Tokens == nil || auth.Tokens.RefreshToken == nil || *auth.Tokens.RefreshToken == "" {
		return nil, b.markCodexAuthReenrollment(
			ctx, cfg, launch, errors.New("host auth store carries no refresh token"),
		)
	}
	if _, _, err := rotateCodexAuthStoreUnderLease(
		ctx, cfg, launch.AuthIdentityID, resolvedPath, body, predecessorMetadata,
		auth, lease, holder, false,
	); err != nil {
		var operational *codexAuthRefreshOperationalError
		if errors.As(err, &operational) {
			return nil, operational.err
		}
		return nil, b.markCodexAuthReenrollment(ctx, cfg, launch, err)
	}
	return guard, nil
}

type codexAuthRefreshOperationalError struct{ err error }

func (e *codexAuthRefreshOperationalError) Error() string { return e.err.Error() }
func (e *codexAuthRefreshOperationalError) Unwrap() error { return e.err }

// rotateCodexAuthStoreUnderLease is the one host-owned refresh transaction
// shared by proactive review refresh and operator enrollment verification.
// The caller owns acquisition and release; this function continuously
// re-authenticates that exact holder and fence around the provider call and
// filesystem commit.
func rotateCodexAuthStoreUnderLease(
	ctx context.Context,
	cfg CodexReviewConfig,
	id domain.AuthIdentityID,
	resolvedPath string,
	body []byte,
	predecessorMetadata codexReviewInputMetadata,
	auth codexAuthFile,
	lease domain.AuthStoreMutationLease,
	holder domain.InvocationID,
	retainIntent bool,
) ([]byte, time.Time, error) {
	if auth.Tokens == nil || auth.Tokens.RefreshToken == nil || *auth.Tokens.RefreshToken == "" {
		return nil, time.Time{}, errors.New("host auth store carries no refresh token")
	}
	previousRefreshToken := *auth.Tokens.RefreshToken
	predecessor := newCodexAuthRefreshPredecessor(body, predecessorMetadata)
	if err := verifyCodexAuthRefreshLease(ctx, cfg, lease, id, holder); err != nil {
		return nil, time.Time{}, &codexAuthRefreshOperationalError{err: err}
	}
	mutate, err := codexAuthLeaseMutationGuard(ctx, cfg, lease)
	if err != nil {
		return nil, time.Time{}, &codexAuthRefreshOperationalError{err: err}
	}
	if err := mutate(func() error {
		return writeCodexAuthRefreshIntent(resolvedPath, id, predecessor, cfg.Now())
	}); err != nil {
		if errors.Is(err, errCodexAuthRefreshIntentExists) {
			return nil, time.Time{}, err
		}
		return nil, time.Time{}, &codexAuthRefreshOperationalError{err: codexReviewOperationalCheckf(
			CheckCredentialSeparation, "persist Codex auth refresh intent: %v", err,
		)}
	}
	if err := verifyCodexAuthRefreshLease(ctx, cfg, lease, id, holder); err != nil {
		return nil, time.Time{}, err
	}
	refreshCtx, cancel := context.WithDeadline(ctx, lease.ExpiresAt)
	defer cancel()
	rotated, err := cfg.AuthRefresher.RefreshCodexAuth(refreshCtx, previousRefreshToken)
	if err != nil {
		var refreshErr *CodexAuthRefreshError
		if errors.As(err, &refreshErr) && refreshErr != nil && refreshErr.Revoked {
			return nil, time.Time{}, errors.New("provider rejected the Codex auth refresh chain")
		}
		return nil, time.Time{}, errors.New("Codex auth refresh result is ambiguous") //nolint:staticcheck // surfaced sentence
	}
	if err := verifyCodexAuthRefreshLease(ctx, cfg, lease, id, holder); err != nil {
		return nil, time.Time{}, err
	}
	rotatedTokens := *auth.Tokens
	if rotated.IDToken != "" {
		rotatedTokens.IDToken = rotated.IDToken
	}
	rotatedTokens.AccessToken = rotated.AccessToken
	refreshToken := rotated.RefreshToken
	rotatedTokens.RefreshToken = &refreshToken
	validatedAt := cfg.Now()
	if err := validateCodexAuthRotation(
		auth.Tokens, &rotatedTokens, validatedAt, codexAuthRefreshThreshold(cfg),
	); err != nil {
		return nil, time.Time{}, err
	}
	*auth.Tokens = rotatedTokens
	lastRefresh, err := json.Marshal(validatedAt)
	if err != nil {
		return nil, time.Time{}, err
	}
	auth.LastRefresh = lastRefresh
	rotatedBody, err := json.Marshal(auth)
	if err != nil {
		return nil, time.Time{}, err
	}
	_, expiresAt, err := inspectCodexHostAuth(CodexAuthSubscription, rotatedBody)
	if err != nil || expiresAt == nil {
		return nil, time.Time{}, errors.New("provider returned an invalid Codex auth rotation")
	}
	if err := mutate(func() error {
		return bindCodexAuthRefreshIntent(
			cfg.InputRoot, resolvedPath, id, predecessor, rotatedBody, validatedAt,
		)
	}); err != nil {
		return nil, time.Time{}, err
	}
	observedIntent, err := readCodexAuthRefreshIntent(cfg.InputRoot, resolvedPath, id)
	if err != nil {
		return nil, time.Time{}, err
	}
	var pending string
	if err := mutate(func() error {
		var err error
		pending, err = stageCodexAuthStore(
			cfg.InputRoot, resolvedPath, id, predecessor, rotatedBody,
		)
		return err
	}); err != nil {
		return nil, time.Time{}, err
	}
	if err := mutate(func() error {
		if err := commitCodexAuthStore(
			cfg.InputRoot, resolvedPath, pending, predecessor, rotatedBody,
		); err != nil {
			return err
		}
		if retainIntent {
			return nil
		}
		return removeObservedCodexAuthRefreshIntent(
			cfg.InputRoot, resolvedPath, id, observedIntent,
		)
	}); err != nil {
		return nil, time.Time{}, &codexAuthRefreshOperationalError{err: codexReviewOperationalCheckf(
			CheckCredentialSeparation, "commit Codex auth rotation for recovery: %v", err,
		)}
	}
	return rotatedBody, expiresAt.UTC(), nil
}

func validateCodexAuthRotation(
	predecessor, rotated *codexAuthTokens, validatedAt time.Time, refreshThreshold time.Duration,
) error {
	if predecessor == nil || predecessor.RefreshToken == nil ||
		*predecessor.RefreshToken == "" || rotated == nil || rotated.RefreshToken == nil {
		return errors.New("codex auth rotation is incomplete")
	}
	previousRefreshToken := *predecessor.RefreshToken
	rotatedRefreshToken := *rotated.RefreshToken
	if rotatedRefreshToken == "" || rotatedRefreshToken == previousRefreshToken ||
		codexAuthTokensExpose(predecessor, rotatedRefreshToken) {
		return errors.New("provider returned an unsafe Codex refresh credential")
	}
	rotatedExpires, err := jwtExpiry(rotated.AccessToken)
	if err != nil || validatedAt.IsZero() ||
		rotatedExpires.Sub(validatedAt) < refreshThreshold {
		return errors.New("provider returned an unusable Codex access credential")
	}
	if codexAuthTokensExpose(rotated, previousRefreshToken) ||
		codexAuthTokensExpose(rotated, rotatedRefreshToken) {
		return errors.New("provider aliased a refresh credential into a visible token")
	}
	return nil
}

func (b *CodexReviewLifecycle) releaseCodexReviewAuthLease(
	ctx context.Context, guard *codexAuthLeaseGuard,
) error {
	if guard == nil {
		return nil
	}
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.cfg.TeardownTimeout)
	defer cancel()
	err := guard.cfg.AuthStoreLeaser.Release(
		releaseCtx, guard.lease.AuthIdentityID, guard.lease.Holder,
		guard.lease.Fence, guard.cfg.Now(),
	)
	if err == nil || errors.Is(err, ErrLeaseWindowEnded) {
		return nil
	}
	return codexReviewOperationalCheckf(
		CheckAuthStoreMutationLease, "release Codex auth refresh lease for identity %q: %v",
		guard.lease.AuthIdentityID, err,
	)
}

func checkCodexAuthReenrollment(
	ctx context.Context, cfg CodexReviewConfig, launch CodexReviewLaunchSpec,
) error {
	if launch.AuthMode != CodexAuthSubscription || cfg.AuthState == nil {
		return nil
	}
	needsReenrollment, err := cfg.AuthState.NeedsCodexAuthReenrollment(
		ctx, launch.AuthIdentityID,
	)
	if err != nil {
		return codexReviewOperationalCheckf(
			CheckCredentialSeparation, "read Codex auth state for identity %q: %v",
			launch.AuthIdentityID, err,
		)
	}
	if needsReenrollment {
		return codexAuthReenrollmentRefusal(launch.AuthIdentityID)
	}
	return nil
}

func verifyCodexAuthLaunchAdmission(
	ctx context.Context,
	cfg CodexReviewConfig,
	launch CodexReviewLaunchSpec,
	guard *codexAuthLeaseGuard,
) error {
	if guard == nil {
		return nil
	}
	if err := checkCodexAuthReenrollment(ctx, cfg, launch); err != nil {
		return err
	}
	return guard.verify(ctx)
}

func reserveCodexAuthStartAdmission(
	ctx context.Context,
	cfg CodexReviewConfig,
	launch CodexReviewLaunchSpec,
	guard *codexAuthLeaseGuard,
) (context.Context, context.CancelFunc, error) {
	if guard == nil {
		startCtx, cancel := context.WithCancel(ctx)
		return startCtx, cancel, nil
	}
	if err := checkCodexAuthReenrollment(ctx, cfg, launch); err != nil {
		return nil, nil, err
	}
	return guard.reserveStart(ctx)
}

func verifyCodexAuthRefreshLease(
	ctx context.Context,
	cfg CodexReviewConfig,
	lease domain.AuthStoreMutationLease,
	id domain.AuthIdentityID,
	holder domain.InvocationID,
) error {
	return verifyCodexAuthRefreshLeaseWindow(ctx, cfg, lease, id, holder, 0)
}

func verifyCodexAuthRefreshLeaseWindow(
	ctx context.Context,
	cfg CodexReviewConfig,
	lease domain.AuthStoreMutationLease,
	id domain.AuthIdentityID,
	holder domain.InvocationID,
	minimumRemaining time.Duration,
) error {
	if err := lease.Validate(); err != nil || lease.AuthIdentityID != id || lease.Holder != holder {
		return failf(CheckAuthStoreMutationLease, "Codex auth refresh lease for identity %q is invalid", id)
	}
	current, err := cfg.AuthStoreLeaser.Get(ctx, id)
	if err != nil {
		return codexReviewOperationalCheckf(
			CheckAuthStoreMutationLease, "re-read Codex auth refresh lease for identity %q: %v", id, err,
		)
	}
	now := cfg.Now()
	if err := current.Validate(); err != nil || current.AuthIdentityID != lease.AuthIdentityID ||
		current.Holder != lease.Holder || current.Fence != lease.Fence ||
		!current.AcquiredAt.Equal(lease.AcquiredAt) || !current.ExpiresAt.Equal(lease.ExpiresAt) ||
		!current.HeldAt(now) || current.ExpiresAt.Sub(now) < minimumRemaining {
		return failf(CheckAuthStoreMutationLease, "Codex auth refresh lease for identity %q changed", id)
	}
	return nil
}

type codexAuthLeaseMutation func(func() error) error

func codexAuthLeaseMutationGuard(
	ctx context.Context, cfg CodexReviewConfig, lease domain.AuthStoreMutationLease,
) (codexAuthLeaseMutation, error) {
	guard, ok := cfg.AuthStoreLeaser.(AuthStoreLeaseMutationGuard)
	if !ok {
		return nil, failf(
			CheckAuthStoreMutationLease,
			"Codex auth refresh lease does not support atomic filesystem mutation",
		)
	}
	return func(mutation func() error) error {
		return guard.WithHeldLeaseMutation(ctx, lease, cfg.Now, mutation)
	}, nil
}

func (b *CodexReviewLifecycle) markCodexAuthReenrollment(
	ctx context.Context,
	cfg CodexReviewConfig,
	launch CodexReviewLaunchSpec,
	cause error,
) error {
	if cfg.AuthState != nil {
		persistCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx), b.cfg.TeardownTimeout,
		)
		defer cancel()
		if err := cfg.AuthState.MarkCodexAuthNeedsReenrollment(
			persistCtx, launch.WorkflowRunID, launch.AuthIdentityID,
		); err != nil {
			return errors.Join(
				codexAuthReenrollmentRefusal(launch.AuthIdentityID),
				codexReviewOperationalCheckf(
					CheckCredentialSeparation,
					"record re-enrollment attention for Codex auth identity %q: %v",
					launch.AuthIdentityID, err,
				),
			)
		}
	}
	return errors.Join(codexAuthReenrollmentRefusal(launch.AuthIdentityID), cause)
}

func codexAuthReenrollmentRefusal(id domain.AuthIdentityID) error {
	return fmt.Errorf(
		"%w: %w: identity %q requires operator re-enrollment",
		ErrInvalidCodexReviewSpec, ErrCodexAuthNeedsReenrollment, id,
	)
}

func codexAuthLifetimeRefusal(
	cfg CodexReviewConfig, id domain.AuthIdentityID, expires, now time.Time,
) error {
	return fmt.Errorf(
		"%w: identity %q access token has %s remaining, floor %s",
		ErrInvalidCodexReviewSpec, id, expires.Sub(now), cfg.AccessTokenLifetimeFloor,
	)
}

func codexAuthRefreshPendingPath(path string, id domain.AuthIdentityID) string {
	digest := contentaddr.Hex(contentaddr.Sum([]byte(id)))
	return filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".freeside-refresh-"+digest+".pending")
}

func codexAuthRefreshIntentPath(path string, id domain.AuthIdentityID) string {
	digest := contentaddr.Hex(contentaddr.Sum([]byte(id)))
	return filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".freeside-refresh-"+digest+".intent")
}

type codexAuthRefreshPredecessor struct {
	Digest string `json:"digest"`
	Mode   uint32 `json:"mode"`
	UID    uint32 `json:"uid"`
	GID    uint32 `json:"gid"`
	Device string `json:"device"`
	Inode  uint64 `json:"inode"`
}

func newCodexAuthRefreshPredecessor(
	body []byte, metadata codexReviewInputMetadata,
) codexAuthRefreshPredecessor {
	return codexAuthRefreshPredecessor{
		Digest: contentaddr.Sum(body), Mode: uint32(metadata.Mode.Perm()),
		UID: metadata.UID, GID: metadata.GID, Device: metadata.Device, Inode: metadata.Ino,
	}
}

func (p codexAuthRefreshPredecessor) matches(
	body []byte, metadata codexReviewInputMetadata,
) bool {
	return contentaddr.Valid(p.Digest) && p.Digest == contentaddr.Sum(body) &&
		p.Mode == uint32(metadata.Mode.Perm()) && p.UID == metadata.UID && p.GID == metadata.GID &&
		p.Device == metadata.Device && p.Inode == metadata.Ino
}

func (p codexAuthRefreshPredecessor) sameModeOwner(metadata codexReviewInputMetadata) bool {
	return p.Mode == uint32(metadata.Mode.Perm()) && p.UID == metadata.UID && p.GID == metadata.GID &&
		p.Device == metadata.Device
}

type codexAuthRefreshIntent struct {
	Version        string                      `json:"version"`
	AuthIdentityID domain.AuthIdentityID       `json:"auth_identity_id"`
	Predecessor    codexAuthRefreshPredecessor `json:"predecessor"`
	StartedAt      time.Time                   `json:"started_at"`
	PendingDigest  string                      `json:"pending_digest,omitempty"`
	ValidatedAt    time.Time                   `json:"validated_at,omitempty"`
}

func (i codexAuthRefreshIntent) validate(id domain.AuthIdentityID) error {
	if i.Version != "codex-auth-refresh-v1" || i.AuthIdentityID != id ||
		!contentaddr.Valid(i.Predecessor.Digest) || i.Predecessor.Mode == 0 ||
		i.StartedAt.IsZero() || i.StartedAt.Location() != time.UTC ||
		(i.PendingDigest == "") != i.ValidatedAt.IsZero() ||
		(i.PendingDigest != "" && (!contentaddr.Valid(i.PendingDigest) ||
			i.ValidatedAt.Location() != time.UTC || i.ValidatedAt.Before(i.StartedAt))) {
		return errors.New("codex auth refresh intent is invalid")
	}
	return nil
}

func bindCodexAuthRefreshIntent(
	root, path string, id domain.AuthIdentityID, predecessor codexAuthRefreshPredecessor,
	pendingBody []byte, validatedAt time.Time,
) error {
	intentPath := codexAuthRefreshIntentPath(path, id)
	_, originalBody, metadata, err := readCodexReviewInputWithMetadata(
		root, intentPath, maxCodexAuthSnapshotBytes,
	)
	if err != nil {
		return errors.New("read Codex auth refresh intent before response binding")
	}
	var intent codexAuthRefreshIntent
	if err := strictjson.Decode(
		originalBody, &intent, strictjson.TolerateInvalidUTF8,
		strictjson.Limit(maxCodexAuthSnapshotBytes),
	); err != nil || intent.validate(id) != nil || intent.Predecessor != predecessor ||
		intent.PendingDigest != "" || !intent.ValidatedAt.IsZero() ||
		!predecessor.sameModeOwner(metadata) {
		return errors.New("codex auth refresh intent changed before response binding")
	}
	intent.PendingDigest = contentaddr.Sum(pendingBody)
	intent.ValidatedAt = validatedAt
	if err := intent.validate(id); err != nil {
		return err
	}
	boundBody, err := json.Marshal(intent)
	if err != nil {
		return errors.New("encode bound Codex auth refresh intent")
	}
	owner, err := newOwnershipLabel()
	if err != nil {
		return errors.New("mint bound Codex auth refresh intent stage")
	}
	stage := intentPath + ".stage-" + owner.Value
	if err := writeCodexAuthRefreshFile(stage, boundBody, metadata); err != nil {
		_ = os.Remove(stage)
		return errors.New("stage bound Codex auth refresh intent")
	}
	_, currentBody, currentMetadata, err := readCodexReviewInputWithMetadata(
		root, intentPath, maxCodexAuthSnapshotBytes,
	)
	if err != nil || !bytes.Equal(currentBody, originalBody) ||
		!predecessor.sameModeOwner(currentMetadata) {
		_ = os.Remove(stage)
		return errors.New("codex auth refresh intent changed during response binding")
	}
	if err := os.Rename(stage, intentPath); err != nil {
		_ = os.Remove(stage)
		return errors.New("commit bound Codex auth refresh intent")
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return errors.New("sync bound Codex auth refresh intent")
	}
	return nil
}

func writeCodexAuthRefreshIntent(
	path string, id domain.AuthIdentityID, predecessor codexAuthRefreshPredecessor, startedAt time.Time,
) error {
	intent := codexAuthRefreshIntent{
		Version: "codex-auth-refresh-v1", AuthIdentityID: id,
		Predecessor: predecessor, StartedAt: startedAt,
	}
	if err := intent.validate(id); err != nil {
		return err
	}
	body, err := json.Marshal(intent)
	if err != nil {
		return errors.New("encode Codex auth refresh intent")
	}
	intentPath := codexAuthRefreshIntentPath(path, id)
	if err := removeCodexAuthRefreshIntentStages(intentPath); err != nil {
		return err
	}
	metadata := codexReviewInputMetadata{
		Mode: os.FileMode(predecessor.Mode), UID: predecessor.UID, GID: predecessor.GID,
		Device: predecessor.Device, Ino: predecessor.Inode,
	}
	owner, err := newOwnershipLabel()
	if err != nil {
		return errors.New("mint Codex auth refresh intent stage")
	}
	stage := intentPath + ".stage-" + owner.Value
	if err := writeCodexAuthRefreshFile(stage, body, metadata); err != nil {
		if cleanupErr := os.Remove(stage); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
			return errors.New("clean up failed Codex auth refresh intent stage")
		}
		return err
	}
	if err := atomicfile.RenameNoReplace(stage, intentPath); err != nil {
		cleanupErr := os.Remove(stage)
		if errors.Is(err, os.ErrExist) {
			if cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
				return fmt.Errorf(
					"%w: clean up losing Codex auth refresh intent stage",
					errCodexAuthRefreshIntentExists,
				)
			}
			return errCodexAuthRefreshIntentExists
		}
		if cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
			return errors.New("clean up uncommitted Codex auth refresh intent stage")
		}
		return errors.New("commit Codex auth refresh intent")
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		// Leave the exclusive intent in place. Rolling its fixed path back
		// after a failed directory sync could unlink a successor's replacement
		// if this holder lost its lease while descheduled. Recovery can safely
		// authenticate the retained intent before deciding what to do with it.
		return fmt.Errorf("%w: sync Codex auth refresh intent: %w", errCodexAuthRefreshIntentExists, err)
	}
	return nil
}

func removeCodexAuthRefreshIntentStages(intentPath string) error {
	dir := filepath.Dir(intentPath)
	prefix := filepath.Base(intentPath) + ".stage-"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return errors.New("inspect Codex auth refresh intent stages")
	}
	stages := make([]string, 0)
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		if len(stages) == 64 {
			return errors.New("too many stale Codex auth refresh intent stages")
		}
		stages = append(stages, filepath.Join(dir, entry.Name()))
	}
	for _, stage := range stages {
		if err := os.Remove(stage); err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.New("remove stale Codex auth refresh intent stage")
		}
	}
	if len(stages) != 0 {
		return syncDirectory(dir)
	}
	return nil
}

type codexAuthRefreshIntentObservation struct {
	Intent   codexAuthRefreshIntent
	Body     []byte
	Metadata codexReviewInputMetadata
}

func readCodexAuthRefreshIntent(
	root, path string, id domain.AuthIdentityID,
) (codexAuthRefreshIntentObservation, error) {
	_, body, metadata, err := readCodexReviewInputWithMetadata(
		root, codexAuthRefreshIntentPath(path, id), maxCodexAuthSnapshotBytes,
	)
	if err != nil {
		return codexAuthRefreshIntentObservation{}, errors.New("codex auth refresh intent failed hardening verification")
	}
	var intent codexAuthRefreshIntent
	if err := strictjson.Decode(
		body, &intent, strictjson.TolerateInvalidUTF8, strictjson.Limit(maxCodexAuthSnapshotBytes),
	); err != nil {
		return codexAuthRefreshIntentObservation{}, errors.New("codex auth refresh intent is malformed")
	}
	if err := intent.validate(id); err != nil {
		return codexAuthRefreshIntentObservation{}, err
	}
	if !intent.Predecessor.sameModeOwner(metadata) {
		return codexAuthRefreshIntentObservation{}, errors.New("codex auth refresh intent metadata diverges")
	}
	return codexAuthRefreshIntentObservation{
		Intent: intent, Body: body, Metadata: metadata,
	}, nil
}

func (o codexAuthRefreshIntentObservation) matches(
	body []byte, metadata codexReviewInputMetadata,
) bool {
	return bytes.Equal(o.Body, body) && o.Metadata.Mode == metadata.Mode &&
		o.Metadata.UID == metadata.UID && o.Metadata.GID == metadata.GID &&
		o.Metadata.Device == metadata.Device && o.Metadata.Ino == metadata.Ino
}

// removeObservedCodexAuthRefreshIntent must run inside a
// codexAuthLeaseMutation. Its body-and-inode check binds the deletion to the
// transaction's observed intent; the store transaction prevents a successor
// from taking the lease before the unlink completes.
func removeObservedCodexAuthRefreshIntent(
	root, path string, id domain.AuthIdentityID,
	observed codexAuthRefreshIntentObservation,
) error {
	intentPath := codexAuthRefreshIntentPath(path, id)
	_, body, metadata, err := readCodexReviewInputWithMetadata(
		root, intentPath, maxCodexAuthSnapshotBytes,
	)
	if err != nil || !observed.matches(body, metadata) {
		return errors.New("codex auth refresh intent changed before removal")
	}
	if err := os.Remove(intentPath); err != nil {
		return errors.New("remove Codex auth refresh intent")
	}
	return syncDirectory(filepath.Dir(path))
}

func stageCodexAuthStore(
	root, path string, id domain.AuthIdentityID, predecessor codexAuthRefreshPredecessor, body []byte,
) (string, error) {
	_, currentBody, metadata, err := readCodexReviewInputWithMetadata(
		root, path, maxCodexAuthSnapshotBytes,
	)
	if err != nil {
		return "", err
	}
	if !predecessor.matches(currentBody, metadata) {
		return "", errors.New("codex auth store changed before rotation staging")
	}
	pending := codexAuthRefreshPendingPath(path, id)
	if err := writeCodexAuthRefreshFile(pending, body, metadata); err != nil {
		return "", err
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return "", err
	}
	return pending, nil
}

func commitCodexAuthStore(
	root, path, pending string, predecessor codexAuthRefreshPredecessor, body []byte,
) error {
	_, currentBody, metadata, err := readCodexReviewInputWithMetadata(
		root, path, maxCodexAuthSnapshotBytes,
	)
	if err != nil {
		return err
	}
	if !predecessor.matches(currentBody, metadata) {
		return errors.New("codex auth store changed before rotation commit")
	}
	_, pendingBody, pendingMetadata, err := readCodexReviewInputWithMetadata(
		root, pending, maxCodexAuthSnapshotBytes,
	)
	if err != nil || !bytes.Equal(pendingBody, body) || !predecessor.sameModeOwner(pendingMetadata) {
		return errors.New("pending Codex auth rotation changed before commit")
	}
	if err := os.Rename(pending, path); err != nil {
		return errors.New("commit pending Codex auth rotation")
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	_, committed, err := readCodexReviewInput(root, path, maxCodexAuthSnapshotBytes)
	if err != nil || !bytes.Equal(committed, body) {
		return errors.New("replaced Codex auth store failed hardening verification")
	}
	return nil
}

func writeCodexAuthRefreshFile(
	path string, body []byte, metadata codexReviewInputMetadata,
) (retErr error) {
	if len(body) == 0 || len(body) > maxCodexAuthSnapshotBytes || metadata.Mode.Perm() == 0 {
		return errors.New("pending Codex auth rotation is invalid")
	}
	f, err := os.OpenFile( //nolint:gosec // caller derives the sibling path from a hardened input under InputRoot
		path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, metadata.Mode.Perm(),
	)
	if err != nil {
		return errors.New("create pending Codex auth rotation")
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil {
			retErr = errors.Join(retErr, errors.New("close pending Codex auth rotation"))
		}
	}()
	if err := f.Chown(int(metadata.UID), int(metadata.GID)); err != nil {
		return errors.New("preserve pending Codex auth rotation ownership")
	}
	if err := f.Chmod(metadata.Mode.Perm()); err != nil {
		return errors.New("preserve pending Codex auth rotation mode")
	}
	if _, err := io.Copy(f, bytes.NewReader(body)); err != nil {
		return errors.New("write pending Codex auth rotation")
	}
	if err := f.Sync(); err != nil {
		return errors.New("sync pending Codex auth rotation")
	}
	return nil
}

func recoverCodexAuthRefreshTransactionUnderLease(
	root, path string, id domain.AuthIdentityID, currentBody []byte,
	currentMetadata codexReviewInputMetadata, refreshThreshold time.Duration,
	retainIntent bool, mutate codexAuthLeaseMutation,
) (bool, error) {
	return recoverCodexAuthRefreshTransactionWithIntent(
		root, path, id, currentBody, currentMetadata, refreshThreshold,
		retainIntent, mutate,
	)
}

// discardCodexAuthRefreshIntentForReenrollmentUnderLease discards an
// unbound refresh intent only after a new explicit enrollment holder has
// verified its exact lease. A missing pending response means the predecessor
// may have been spent, so normal refresh recovery must stop; a fresh operator
// login is the one deliberate replacement authority for that terminal state.
func discardCodexAuthRefreshIntentForReenrollmentUnderLease(
	root, path string, id domain.AuthIdentityID, currentBody []byte,
	currentMetadata codexReviewInputMetadata, mutate codexAuthLeaseMutation,
) error {
	observed, err := readCodexAuthRefreshIntent(root, path, id)
	if err != nil {
		return err
	}
	intent := observed.Intent
	if !intent.Predecessor.matches(currentBody, currentMetadata) {
		return errors.New("codex auth refresh intent no longer matches its predecessor")
	}
	pending := codexAuthRefreshPendingPath(path, id)
	if _, err := os.Lstat(pending); err == nil {
		return errors.New("codex auth refresh intent has a pending response")
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("inspect pending Codex auth rotation")
	}
	return mutate(func() error {
		if _, err := os.Lstat(pending); err == nil {
			return errors.New("codex auth refresh intent gained a pending response")
		} else if !errors.Is(err, os.ErrNotExist) {
			return errors.New("inspect pending Codex auth rotation")
		}
		return removeObservedCodexAuthRefreshIntent(root, path, id, observed)
	})
}

func recoverCodexAuthRefreshTransactionWithIntent(
	root, path string, id domain.AuthIdentityID, currentBody []byte,
	currentMetadata codexReviewInputMetadata, refreshThreshold time.Duration,
	retainIntent bool, mutate codexAuthLeaseMutation,
) (bool, error) {
	intentPath := codexAuthRefreshIntentPath(path, id)
	pending := codexAuthRefreshPendingPath(path, id)
	_, intentErr := os.Lstat(intentPath)
	_, pendingErr := os.Lstat(pending)
	intentExists := intentErr == nil
	pendingExists := pendingErr == nil
	if intentErr != nil && !errors.Is(intentErr, os.ErrNotExist) {
		return false, errors.New("inspect Codex auth refresh intent")
	}
	if pendingErr != nil && !errors.Is(pendingErr, os.ErrNotExist) {
		return false, errors.New("inspect pending Codex auth rotation")
	}
	if !intentExists && !pendingExists {
		return false, nil
	}
	if !intentExists {
		return false, errors.New("pending Codex auth rotation has no predecessor intent")
	}
	observed, err := readCodexAuthRefreshIntent(root, path, id)
	if err != nil {
		return false, err
	}
	intent := observed.Intent
	currentIsPredecessor := intent.Predecessor.matches(currentBody, currentMetadata)
	if !currentIsPredecessor {
		currentIsCommittedTarget := !pendingExists && intent.PendingDigest != "" &&
			contentaddr.Sum(currentBody) == intent.PendingDigest &&
			intent.Predecessor.sameModeOwner(currentMetadata)
		if currentIsCommittedTarget {
			currentAuth, _, err := inspectCodexHostAuth(CodexAuthSubscription, currentBody)
			var validatedAt time.Time
			if err != nil || len(currentAuth.LastRefresh) == 0 ||
				bytes.Equal(currentAuth.LastRefresh, []byte("null")) ||
				json.Unmarshal(currentAuth.LastRefresh, &validatedAt) != nil ||
				!validatedAt.Equal(intent.ValidatedAt) {
				return false, errors.New("committed Codex auth rotation diverges from its response binding")
			}
			if !retainIntent {
				if err := mutate(func() error {
					return removeObservedCodexAuthRefreshIntent(root, path, id, observed)
				}); err != nil {
					return false, err
				}
			}
			return true, nil
		}
		// The exact body/inode/owner/mode binding proves another writer
		// replaced the predecessor after this request began. Preserve that
		// newer authority even when a crash left an incomplete pending file.
		if err := mutate(func() error {
			currentObserved, err := readCodexAuthRefreshIntent(root, path, id)
			if err != nil || !observed.matches(currentObserved.Body, currentObserved.Metadata) {
				return errors.New("codex auth refresh intent changed before superseded cleanup")
			}
			if pendingExists {
				if err := os.Remove(pending); err != nil {
					return errors.New("discard superseded Codex auth rotation")
				}
			}
			return removeObservedCodexAuthRefreshIntent(root, path, id, observed)
		}); err != nil {
			return false, err
		}
		return false, nil
	}
	if !pendingExists {
		return false, errCodexAuthRefreshPredecessorMayBeConsumed
	}
	if intent.PendingDigest == "" || intent.ValidatedAt.IsZero() {
		return false, errors.New("pending Codex auth rotation has no durable response binding")
	}
	_, pendingBody, pendingMetadata, err := readCodexReviewInputWithMetadata(
		root, pending, maxCodexAuthSnapshotBytes,
	)
	if err != nil {
		return false, errors.New("pending Codex auth rotation failed hardening verification")
	}
	if contentaddr.Sum(pendingBody) != intent.PendingDigest {
		return false, errors.New("pending Codex auth rotation diverges from its response binding")
	}
	predecessorAuth, _, err := inspectCodexHostAuth(CodexAuthSubscription, currentBody)
	if err != nil {
		return false, errors.New("codex auth rotation predecessor is invalid")
	}
	pendingAuth, _, err := inspectCodexHostAuth(CodexAuthSubscription, pendingBody)
	if err != nil {
		return false, errors.New("pending Codex auth rotation is incomplete")
	}
	var validatedAt time.Time
	if len(pendingAuth.LastRefresh) == 0 || bytes.Equal(pendingAuth.LastRefresh, []byte("null")) ||
		json.Unmarshal(pendingAuth.LastRefresh, &validatedAt) != nil || validatedAt.IsZero() ||
		!validatedAt.Equal(intent.ValidatedAt) {
		return false, errors.New("pending Codex auth rotation has no validation instant")
	}
	if err := validateCodexAuthRotation(
		predecessorAuth.Tokens, pendingAuth.Tokens, validatedAt, refreshThreshold,
	); err != nil {
		return false, errors.New("pending Codex auth rotation failed returned-token validation")
	}
	if !intent.Predecessor.sameModeOwner(pendingMetadata) {
		return false, errors.New("pending Codex auth rotation metadata diverges")
	}
	if err := mutate(func() error {
		if err := commitCodexAuthStore(
			root, path, pending, intent.Predecessor, pendingBody,
		); err != nil {
			return err
		}
		if retainIntent {
			return nil
		}
		return removeObservedCodexAuthRefreshIntent(root, path, id, observed)
	}); err != nil {
		return false, err
	}
	return true, nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path) //nolint:gosec // caller passes the validated auth-store parent directory
	if err != nil {
		return errors.New("open Codex auth store directory for sync")
	}
	defer dir.Close() //nolint:errcheck // the preceding fsync is the durability signal
	if err := dir.Sync(); err != nil {
		return errors.New("sync Codex auth store directory")
	}
	return nil
}
