package publish

import (
	"context"
	"errors"
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
	tok, err := s.minter.mintResolved(ctx, binding, parsed, repositoryID)
	if err != nil {
		return InstallationToken{}, err
	}
	s.tokens[key] = tok
	return tok, nil
}
