package publish

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// AppBotIdentity is the public Git attribution GitHub binds to one App.
// It carries no credential or publication authority.
type AppBotIdentity struct {
	AppSlug   string
	BotUserID int64
}

// GitHubAppBotIdentityResolver derives commit attribution from the same
// trusted App registration selected by the repository token source.
type GitHubAppBotIdentityResolver struct {
	tokens   TokenSource
	keystore *Keystore
	client   *http.Client
	baseURL  string
	now      func() time.Time

	// mu is held through authority resolution and a possible GitHub bot lookup
	// so concurrent invocation paths cannot burst either endpoint.
	mu    sync.Mutex
	cache map[string]appBotIdentityCacheEntry
}

type appBotIdentityCacheEntry struct {
	repo           string
	registrationID int64
	installationID int64
	repositoryID   int64
	appSlug        string
	tokenExpiresAt time.Time
	identity       AppBotIdentity
}

// NewGitHubAppBotIdentityResolver constructs a resolver over the production
// token and credential authorities. The client never follows redirects so its
// installation token cannot be forwarded to another endpoint.
func NewGitHubAppBotIdentityResolver(
	tokens TokenSource,
	keystore *Keystore,
	client *http.Client,
	baseURL string,
	now func() time.Time,
) (*GitHubAppBotIdentityResolver, error) {
	if tokens == nil || keystore == nil || client == nil || strings.TrimSpace(baseURL) == "" || now == nil {
		return nil, errors.New("app bot identity resolver: nil or empty dependency")
	}
	return &GitHubAppBotIdentityResolver{
		tokens: tokens, keystore: keystore, client: noRedirect(client),
		baseURL: strings.TrimRight(baseURL, "/"), now: now,
		cache: map[string]appBotIdentityCacheEntry{},
	}, nil
}

// Resolve authenticates the current token and registration binding, then
// reuses the canonical bot observation while that exact token lease remains
// usable. Invocation-level callers decide when this current-authority check is
// required; a cache hit here never bypasses it.
func (r *GitHubAppBotIdentityResolver) Resolve(
	ctx context.Context,
	repo string,
) (AppBotIdentity, error) {
	if r == nil || r.tokens == nil || r.keystore == nil || r.client == nil || r.now == nil {
		return AppBotIdentity{}, errors.New("app bot identity resolver: nil dependency")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.revalidateLocked(ctx, repo)
}

// Revalidate names the import boundary's explicit current-binding check.
func (r *GitHubAppBotIdentityResolver) Revalidate(
	ctx context.Context,
	repo string,
) (AppBotIdentity, error) {
	if r == nil || r.tokens == nil || r.keystore == nil || r.client == nil || r.now == nil {
		return AppBotIdentity{}, errors.New("app bot identity resolver: nil dependency")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.revalidateLocked(ctx, repo)
}

// revalidateLocked authenticates the selected registration/token binding and
// only then trusts or refreshes the returned GitHub bot object. r.mu is held so
// concurrent cold callers cannot burst either authority or identity endpoints.
func (r *GitHubAppBotIdentityResolver) revalidateLocked(
	ctx context.Context,
	repo string,
) (AppBotIdentity, error) {
	token, err := r.tokens.Token(ctx, repo)
	if err != nil {
		return AppBotIdentity{}, fmt.Errorf("resolve App bot identity: %w", err)
	}
	if token.RegistrationID <= 0 || token.InstallationID <= 0 ||
		token.RepositoryID <= 0 || token.Repo != repo || token.Token.Reveal() == "" {
		return AppBotIdentity{}, fmt.Errorf(
			"resolve App bot identity: selected token has invalid coordinates: %w",
			ErrAppBotIdentityMismatch,
		)
	}
	apps, err := r.keystore.ListApps()
	if err != nil {
		return AppBotIdentity{}, fmt.Errorf("resolve App bot identity: list registrations: %w", err)
	}
	var selected *AppCredentials
	for index := range apps {
		if apps[index].AppID != token.RegistrationID {
			continue
		}
		if selected != nil {
			return AppBotIdentity{}, fmt.Errorf(
				"resolve App bot identity: duplicate selected registration: %w",
				ErrAppBotIdentityMismatch,
			)
		}
		selected = &apps[index]
	}
	if selected == nil {
		return AppBotIdentity{}, fmt.Errorf(
			"resolve App bot identity: selected registration is unavailable: %w",
			ErrAppBotIdentityMismatch,
		)
	}
	login := selected.Slug + "[bot]"
	if cached, ok := r.cache[repo]; ok &&
		cached.repo == repo && cached.registrationID == token.RegistrationID &&
		cached.installationID == token.InstallationID &&
		cached.repositoryID == token.RepositoryID && cached.appSlug == selected.Slug &&
		cached.tokenExpiresAt.Equal(token.ExpiresAt) &&
		r.now().Before(cached.tokenExpiresAt.Add(-tokenExpirySkew)) {
		return cached.identity, nil
	}
	path := "/users/" + url.PathEscape(login)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.baseURL+path, nil)
	if err != nil {
		return AppBotIdentity{}, fmt.Errorf("resolve App bot identity: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token.Token.Reveal())
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := r.client.Do(req)
	if err != nil {
		return AppBotIdentity{}, fmt.Errorf("resolve App bot identity: %w", err)
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return AppBotIdentity{}, fmt.Errorf(
			"resolve App bot identity: %w",
			&APIError{Status: resp.StatusCode, RequestPath: path},
		)
	}
	var user struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
		Type  string `json:"type"`
	}
	if err := decodeResponse(resp.Body, &user); err != nil {
		return AppBotIdentity{}, fmt.Errorf("resolve App bot identity: decode response: %w", err)
	}
	if user.Login != login || user.ID <= 0 || user.Type != "Bot" {
		return AppBotIdentity{}, fmt.Errorf(
			"resolve App bot identity: canonical account disagrees with selected registration: %w",
			ErrAppBotIdentityMismatch,
		)
	}
	identity := AppBotIdentity{AppSlug: selected.Slug, BotUserID: user.ID}
	if r.now().Before(token.ExpiresAt.Add(-tokenExpirySkew)) {
		r.cache[repo] = appBotIdentityCacheEntry{
			repo: repo, registrationID: token.RegistrationID,
			installationID: token.InstallationID, repositoryID: token.RepositoryID,
			appSlug: selected.Slug, tokenExpiresAt: token.ExpiresAt, identity: identity,
		}
	} else {
		delete(r.cache, repo)
	}
	return identity, nil
}
