package publish

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// TokenSource supplies a usable installation token for an owner/name
// repository.
// The publisher and reconciler depend on this rather than on Minter so
// tests can inject tokens without a keystore, and so token reuse
// policy lives in one place.
type TokenSource interface {
	Token(ctx context.Context, repo string) (InstallationToken, error)
}

// tokenExpirySkew is how long before expiry a cached token stops being
// handed out: a token about to lapse mid-publication would fail the
// path halfway through its external effects.
const tokenExpirySkew = 2 * time.Minute

// CachedTokenSource reuses a minted installation token per resolved
// registration, installation, and canonical repository ID until
// tokenExpirySkew before its expiry, then mints a fresh one.
// Minting stays the audited slow path (one audit row per mint, not per
// request); the cache only bounds how often it runs.
type CachedTokenSource struct {
	minter *Minter
	now    func() time.Time

	// mu is held across a mint so concurrent callers converge on one
	// minted token (and one audit row) instead of racing mints.
	mu     sync.Mutex
	tokens map[tokenCacheKey]InstallationToken
}

type tokenCacheKey struct {
	registrationID int64
	installationID int64
	repositoryID   int64
}

// AuthorityInspectionTokenSource mints one repository-scoped, read-only token
// from an exact durable installation-authority binding. It exists for
// preflight observations that must authenticate private repositories while no
// publication janitor is running. Every token still passes the ordinary grant
// validation and durable mint recorder before it can reach the git transport.
type AuthorityInspectionTokenSource struct {
	minter         *Minter
	authority      InstallationAuthoritySource
	registrationID int64
	repositoryID   int64
}

var authorityInspectionPermissions = Permissions{Contents: "read", Metadata: "read"}

var authorityInspectionPermissionScopes = map[string]string{
	"contents": authorityInspectionPermissions.Contents,
	"metadata": authorityInspectionPermissions.Metadata,
}

// NewAuthorityInspectionTokenSource wires the narrow preflight token source.
func NewAuthorityInspectionTokenSource(
	minter *Minter,
	authority InstallationAuthoritySource,
	registrationID, repositoryID int64,
) *AuthorityInspectionTokenSource {
	return &AuthorityInspectionTokenSource{
		minter: minter, authority: authority,
		registrationID: registrationID, repositoryID: repositoryID,
	}
}

// Token revalidates the selected App and durable trusted installation on every
// call, then mints the minimum repository-read grant. Pending installation
// envelopes never authorize this source.
func (s *AuthorityInspectionTokenSource) Token(
	ctx context.Context,
	repo string,
) (InstallationToken, error) {
	if s == nil || s.minter == nil || s.minter.keystore == nil || s.minter.client == nil ||
		s.minter.baseURL == "" || s.minter.recorder == nil || s.minter.now == nil ||
		s.authority == nil || s.registrationID <= 0 || s.repositoryID <= 0 {
		return InstallationToken{}, errors.New("authority inspection token: nil or invalid dependency")
	}
	parsed, err := parseRepo(repo)
	if err != nil {
		return InstallationToken{}, fmt.Errorf("authority inspection token: %w", err)
	}
	apps, err := s.minter.keystore.ListApps()
	if err != nil {
		return InstallationToken{}, fmt.Errorf("authority inspection token: list registrations: %w", err)
	}
	var selected *AppCredentials
	for index := range apps {
		if apps[index].AppID != s.registrationID {
			continue
		}
		if selected != nil {
			return InstallationToken{}, errors.New("authority inspection token: selected registration is duplicated")
		}
		selected = &apps[index]
	}
	if selected == nil {
		return InstallationToken{}, errors.New("authority inspection token: selected registration is unavailable")
	}
	snapshot, err := s.authority.InstallationAuthority(ctx, s.registrationID)
	if err != nil {
		return InstallationToken{}, fmt.Errorf("authority inspection token: read authority: %w", err)
	}
	validated, err := validateInstallationAuthority(*selected, snapshot, s.minter.now().UTC())
	if err != nil {
		return InstallationToken{}, fmt.Errorf("authority inspection token: validate authority: %w", err)
	}
	var binding *InstallationBinding
	for installationID, candidate := range validated.trusted {
		if !slices.Contains(candidate.repositoryIDs, s.repositoryID) {
			continue
		}
		if candidate.account != strings.ToLower(parsed.owner) {
			return InstallationToken{}, errors.New("authority inspection token: repository owner differs from trusted installation")
		}
		if binding != nil {
			return InstallationToken{}, errors.New("authority inspection token: repository authority is ambiguous")
		}
		binding = &InstallationBinding{
			RegistrationID: s.registrationID, RegistrationOwner: selected.Owner,
			RegistrationOwnerID: selected.OwnerID, InstallationID: installationID,
			Account: candidate.account, AccountID: candidate.accountID,
		}
	}
	if binding == nil {
		return InstallationToken{}, errors.New("authority inspection token: repository has no durable trusted installation")
	}
	return s.minter.mintResolved(
		ctx, *binding, parsed, s.repositoryID,
		authorityInspectionPermissions, authorityInspectionPermissionScopes,
	)
}

// NewCachedTokenSource wires a CachedTokenSource over a resolving minter.
func NewCachedTokenSource(m *Minter, now func() time.Time) *CachedTokenSource {
	return &CachedTokenSource{
		minter: m,
		now:    now,
		tokens: map[tokenCacheKey]InstallationToken{},
	}
}

// Token returns a cached token still comfortably inside its lifetime,
// or mints, caches, and returns a fresh one.
func (s *CachedTokenSource) Token(ctx context.Context, repo string) (InstallationToken, error) {
	if s == nil || s.minter == nil || s.now == nil {
		return InstallationToken{}, errors.New("token: nil dependency")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	binding, parsed, repositoryID, err := s.minter.resolveTrusted(ctx, repo)
	if err != nil {
		return InstallationToken{}, err
	}
	key := tokenCacheKey{
		registrationID: binding.RegistrationID,
		installationID: binding.InstallationID,
		repositoryID:   repositoryID,
	}
	if tok, ok := s.tokens[key]; ok && tok.ExpiresAt.After(s.now().Add(tokenExpirySkew)) {
		return tok, nil
	}
	tok, err := s.minter.mintResolved(
		ctx, binding, parsed, repositoryID,
		PublishPermissions, publishPermissionScopes,
	)
	if err != nil {
		return InstallationToken{}, err
	}
	s.tokens[key] = tok
	return tok, nil
}

// OnboardingGate exposes the janitor's two distinct reconciliation signals:
// trusted bindings may operate, while a pending binding may only mint the
// read-only audit token used to construct its one-time review.
type OnboardingGate interface {
	AllowsRepository(registrationID, installationID, repositoryID int64) bool
	PendingReady(PendingInstallationEnvelope) (int64, bool)
}

// OnboardingTokenSource mints only the read-only token needed to audit one
// repository during onboarding. It revalidates local authority and the
// janitor's current exact grant observation before every cache hit.
type OnboardingTokenSource struct {
	minter         *Minter
	authority      InstallationAuthoritySource
	gate           OnboardingGate
	registrationID int64
	repositoryID   int64
	now            func() time.Time

	mu     sync.Mutex
	tokens map[tokenCacheKey]InstallationToken
}

func NewOnboardingTokenSource(
	minter *Minter,
	authority InstallationAuthoritySource,
	gate OnboardingGate,
	registrationID, repositoryID int64,
	now func() time.Time,
) *OnboardingTokenSource {
	return &OnboardingTokenSource{
		minter: minter, authority: authority, gate: gate,
		registrationID: registrationID, repositoryID: repositoryID, now: now,
		tokens: map[tokenCacheKey]InstallationToken{},
	}
}

func (s *OnboardingTokenSource) Token(
	ctx context.Context,
	repo string,
) (InstallationToken, error) {
	if s == nil || s.minter == nil || s.minter.keystore == nil ||
		s.authority == nil || s.gate == nil || s.now == nil ||
		s.registrationID <= 0 || s.repositoryID <= 0 {
		return InstallationToken{}, errors.New("onboarding token: nil or invalid dependency")
	}
	parsed, err := parseRepo(repo)
	if err != nil {
		return InstallationToken{}, fmt.Errorf("onboarding token: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	binding, err := s.resolve(ctx, parsed)
	if err != nil {
		return InstallationToken{}, err
	}
	key := tokenCacheKey{
		registrationID: binding.RegistrationID,
		installationID: binding.InstallationID,
		repositoryID:   s.repositoryID,
	}
	if tok, ok := s.tokens[key]; ok &&
		tok.ExpiresAt.After(s.now().Add(tokenExpirySkew)) {
		return tok, nil
	}
	tok, err := s.minter.mintResolved(
		ctx, binding, parsed, s.repositoryID,
		WorkflowAuditPermissions, map[string]string{
			"actions":        WorkflowAuditPermissions.Actions,
			"administration": WorkflowAuditPermissions.Administration,
			"contents":       WorkflowAuditPermissions.Contents,
			"environments":   WorkflowAuditPermissions.Environments,
			"metadata":       WorkflowAuditPermissions.Metadata,
		},
	)
	if err != nil {
		return InstallationToken{}, err
	}
	s.tokens[key] = tok
	return tok, nil
}

func (s *OnboardingTokenSource) resolve(
	ctx context.Context,
	repo repoRef,
) (InstallationBinding, error) {
	apps, err := s.minter.keystore.ListApps()
	if err != nil {
		return InstallationBinding{}, fmt.Errorf("onboarding token: list registrations: %w", err)
	}
	var matches []AppCredentials
	for _, app := range apps {
		if app.AppID == s.registrationID {
			matches = append(matches, app)
		}
	}
	if len(matches) != 1 {
		return InstallationBinding{}, fmt.Errorf(
			"onboarding token: registration %d resolves to %d credentials",
			s.registrationID, len(matches),
		)
	}
	app := matches[0]
	authority, err := s.authority.InstallationAuthority(ctx, s.registrationID)
	if err != nil {
		return InstallationBinding{}, fmt.Errorf("onboarding token: read authority: %w", err)
	}
	validated, err := validateInstallationAuthority(app, authority, s.now().UTC())
	if err != nil {
		return InstallationBinding{}, fmt.Errorf("onboarding token: validate authority: %w", err)
	}
	for installationID, candidate := range validated.trusted {
		if !slices.Contains(candidate.repositoryIDs, s.repositoryID) {
			continue
		}
		if candidate.account != strings.ToLower(repo.owner) ||
			!s.gate.AllowsRepository(s.registrationID, installationID, s.repositoryID) {
			return InstallationBinding{}, errors.New(
				"onboarding token: trusted installation is not currently reconciled")
		}
		return InstallationBinding{
			RegistrationID: s.registrationID, RegistrationOwner: app.Owner,
			RegistrationOwnerID: app.OwnerID, InstallationID: installationID,
			Account: candidate.account, AccountID: candidate.accountID,
		}, nil
	}
	if validated.pending == nil || authority.Pending == nil ||
		validated.pending.account != strings.ToLower(repo.owner) ||
		!slices.Contains(validated.pending.repositoryIDs, s.repositoryID) ||
		slices.Contains(validated.pending.allowedRepositoryIDs, s.repositoryID) {
		return InstallationBinding{}, errors.New(
			"onboarding token: repository has no current exact onboarding authority")
	}
	installationID, ready := s.gate.PendingReady(*authority.Pending)
	if !ready || installationID <= 0 ||
		(validated.pending.installationID > 0 &&
			validated.pending.installationID != installationID) {
		return InstallationBinding{}, errors.New(
			"onboarding token: pending installation is not currently reconciled")
	}
	return InstallationBinding{
		RegistrationID: s.registrationID, RegistrationOwner: app.Owner,
		RegistrationOwnerID: app.OwnerID, InstallationID: installationID,
		Account: validated.pending.account, AccountID: validated.pending.accountID,
	}, nil
}
