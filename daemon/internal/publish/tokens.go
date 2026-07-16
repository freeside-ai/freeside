package publish

import (
	"context"
	"errors"
	"sync"
	"time"
)

// TokenSource supplies a usable installation token for a repository.
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

// CachedTokenSource reuses a minted installation token per repository
// until tokenExpirySkew before its expiry, then mints a fresh one.
// Minting stays the audited slow path (one audit row per mint, not per
// request); the cache only bounds how often it runs.
type CachedTokenSource struct {
	minter         *Minter
	installationID int64
	now            func() time.Time

	// mu is held across a mint so concurrent callers converge on one
	// minted token (and one audit row) instead of racing mints.
	mu     sync.Mutex
	tokens map[string]InstallationToken
}

// NewCachedTokenSource wires a CachedTokenSource over the minter for
// one installation.
func NewCachedTokenSource(m *Minter, installationID int64, now func() time.Time) *CachedTokenSource {
	return &CachedTokenSource{
		minter:         m,
		installationID: installationID,
		now:            now,
		tokens:         map[string]InstallationToken{},
	}
}

// Token returns a cached token still comfortably inside its lifetime,
// or mints, caches, and returns a fresh one.
func (s *CachedTokenSource) Token(ctx context.Context, repo string) (InstallationToken, error) {
	if repo == "" {
		return InstallationToken{}, errors.New("token: empty repository name")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if tok, ok := s.tokens[repo]; ok && tok.ExpiresAt.After(s.now().Add(tokenExpirySkew)) {
		return tok, nil
	}
	tok, err := s.minter.MintInstallationToken(ctx, s.installationID, repo)
	if err != nil {
		return InstallationToken{}, err
	}
	s.tokens[repo] = tok
	return tok, nil
}
