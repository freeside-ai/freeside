package publish

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

// tokenCache holds minted installation tokens per tokenCacheKey and lets one
// caller per key mint at a time, so concurrent callers for one key converge
// on one minted token and one audit row. Its mutex guards only the maps and
// is never held across a network call: a caller resolves its installation
// authority before it reaches the cache, and a caller that finds another
// minting its key waits on that mint alone, so one repository's slow GitHub
// call never delays another's token.
type tokenCache struct {
	now func() time.Time

	mu sync.Mutex
	// tokens holds pointers so a holder that keeps this cache in an
	// unexported field cannot print a cached token. Under a verb a
	// pointer does not support (%s, %q, %t), fmt cannot call Format
	// through that field; it reprints the pointer under %v, which prints a
	// pointer below the top level as an address, so each entry shows only
	// its address. Format still guards direct rendering. get returns a
	// copy, so no caller can mutate an entry.
	tokens map[tokenCacheKey]*InstallationToken
	// minting holds the done channel of the mint running for a key.
	minting map[tokenCacheKey]chan struct{}
}

func newTokenCache(now func() time.Time) *tokenCache {
	return &tokenCache{
		now: now, tokens: map[tokenCacheKey]*InstallationToken{}, minting: map[tokenCacheKey]chan struct{}{},
	}
}

// Format renders the cache without its tokens.
func (*tokenCache) Format(f fmt.State, _ rune) {
	io.WriteString(f, "publish.tokenCache") //nolint:errcheck,gosec // fmt.State writes cannot be usefully handled
}

// get returns key's cached token while it is still comfortably inside its
// lifetime, or mints, caches, and returns a fresh one. The caller has already
// resolved key from current authority; the cache never stands in for that
// check. A caller that waited on another's mint reads the cache again, and
// mints itself if that mint failed, as it would have under one mutex.
func (c *tokenCache) get(
	ctx context.Context, key tokenCacheKey, mint func() (InstallationToken, error),
) (InstallationToken, error) {
	for {
		c.mu.Lock()
		if tok, ok := c.tokens[key]; ok && tok.ExpiresAt.After(c.now().Add(tokenExpirySkew)) {
			c.mu.Unlock()
			return *tok, nil
		}
		if running, ok := c.minting[key]; ok {
			c.mu.Unlock()
			select {
			case <-running:
				continue
			case <-ctx.Done():
				return InstallationToken{}, ctx.Err()
			}
		}
		done := make(chan struct{})
		c.minting[key] = done
		c.mu.Unlock()

		tok, err := mint()

		c.mu.Lock()
		delete(c.minting, key)
		if err == nil {
			c.tokens[key] = &tok
		}
		close(done)
		c.mu.Unlock()
		return tok, err
	}
}
