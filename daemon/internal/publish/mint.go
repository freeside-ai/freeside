package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"time"
)

// Permissions is the GitHub App repository-permission set a mint requests and
// a grant reports. It is closed rather than map-shaped so tokens cannot carry
// authority the live audit and publication paths do not declare.
type Permissions struct {
	Actions        string `json:"actions,omitempty"`
	Administration string `json:"administration,omitempty"`
	Contents       string `json:"contents,omitempty"`
	Environments   string `json:"environments,omitempty"`
	PullRequests   string `json:"pull_requests,omitempty"`
	Metadata       string `json:"metadata,omitempty"`
}

// PublishPermissions is the minimum combined publish-and-live-audit set:
// contents:write and pull_requests:write perform publication; actions:read,
// administration:read, environments:read, contents access, and metadata:read
// inspect workflows, repository automation policy, environment secret names,
// local automation files, branch protection, and rulesets (#80, #182). The
// mint audit records both requested and granted values losslessly.
var PublishPermissions = Permissions{
	Actions:        "read",
	Administration: "read",
	Contents:       "write",
	Environments:   "read",
	PullRequests:   "write",
	Metadata:       "read",
}

// publishPermissionScopes is the lossless form the mint validates
// grants against: the response's permission object decodes into a map,
// so a grant carrying an unknown key (broader authority) cannot be
// silently dropped by a fixed struct before the comparison.
var publishPermissionScopes = map[string]string{
	"actions":        PublishPermissions.Actions,
	"administration": PublishPermissions.Administration,
	"contents":       PublishPermissions.Contents,
	"environments":   PublishPermissions.Environments,
	"pull_requests":  PublishPermissions.PullRequests,
	"metadata":       PublishPermissions.Metadata,
}

// InstallationToken is a short-lived, per-repository credential minted
// from the App JWT. Token is a Secret; Permissions is what GitHub
// actually granted.
type InstallationToken struct {
	Token          Secret
	ExpiresAt      time.Time
	RegistrationID int64
	InstallationID int64
	RepositoryID   int64
	Repo           string
	Permissions    Permissions
}

// Minter mints installation tokens against the GitHub API. The HTTP
// client, API base URL, audit recorder, and clock are all injected;
// the keystore is the only credential source.
type Minter struct {
	keystore *Keystore
	resolver *InstallationResolver
	trust    TrustSource
	client   *http.Client
	baseURL  string
	recorder Recorder
	now      func() time.Time
}

// NewMinter wires a Minter. baseURL is the GitHub API root (real:
// https://api.github.com; tests: an httptest server). The injected
// client is wrapped to never follow redirects (see noRedirect), so a
// caller-supplied CheckRedirect cannot carry the App JWT anywhere.
func NewMinter(ks *Keystore, client *http.Client, baseURL string, rec Recorder, trust TrustSource, now func() time.Time) *Minter {
	return &Minter{
		keystore: ks,
		resolver: NewInstallationResolver(ks, client, baseURL, now),
		trust:    trust,
		client:   noRedirect(client),
		baseURL:  baseURL,
		recorder: rec,
		now:      now,
	}
}

// NewMinterWithJanitor wires minting to the always-on janitor required by
// public registrations. NewMinter remains the private-registration path and
// fails closed if its keystore later contains a public registration.
func NewMinterWithJanitor(
	ks *Keystore,
	client *http.Client,
	baseURL string,
	rec Recorder,
	trust TrustSource,
	now func() time.Time,
	janitor JanitorStatus,
) *Minter {
	return &Minter{
		keystore: ks,
		resolver: NewInstallationResolverWithJanitor(ks, client, baseURL, now, janitor),
		trust:    trust,
		client:   noRedirect(client),
		baseURL:  baseURL,
		recorder: rec,
		now:      now,
	}
}

// noRedirect copies a client and disables redirect-following: the
// GitHub API endpoints this package calls never legitimately redirect,
// and following one would re-send a request whose URL, Referer, or
// same-host Authorization header can carry credential material to the
// target (a misconfigured base URL or interposed proxy). A 3xx
// surfaces as its status through the ordinary redacted APIError path.
func noRedirect(c *http.Client) *http.Client {
	if c == nil {
		return nil
	}
	nc := *c
	nc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &nc
}

// mintRequest is the API request body; field order is pinned by the
// request golden test, which is the durable statement of the minimum
// scope set.
type mintRequest struct {
	RepositoryIDs []int64     `json:"repository_ids"`
	Permissions   Permissions `json:"permissions"`
}

// mintResponse decodes the 201 body: the token lands directly in a
// Secret, never in a plain string, and the grant's scope fields decode
// losslessly (a permission map plus the repository selection) so the
// validation below sees exactly what GitHub granted, not a projection.
type mintResponse struct {
	Token               Secret            `json:"token"`
	ExpiresAt           Secret            `json:"expires_at"`
	Permissions         map[string]string `json:"permissions"`
	RepositorySelection string            `json:"repository_selection"`
	Repositories        []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"repositories"`
}

// MintInstallationToken resolves repo's owner to a known registration and
// installation, re-gates repo against the existing trusted-repository source,
// mints a token with the minimum publish permission set, and records the mint
// before returning it. No caller-supplied installation ID can bypass either
// gate.
func (m *Minter) MintInstallationToken(ctx context.Context, repo string) (InstallationToken, error) {
	binding, parsed, repositoryID, err := m.resolveTrusted(ctx, repo)
	if err != nil {
		return InstallationToken{}, err
	}
	return m.mintResolved(ctx, binding, parsed, repositoryID)
}

func (m *Minter) resolveTrusted(ctx context.Context, repo string) (InstallationBinding, repoRef, int64, error) {
	parsed, err := parseRepo(repo)
	if err != nil {
		return InstallationBinding{}, repoRef{}, 0, fmt.Errorf("mint: %w", err)
	}
	if m == nil || m.keystore == nil || m.resolver == nil || m.trust == nil ||
		m.client == nil || m.recorder == nil || m.now == nil {
		return InstallationBinding{}, repoRef{}, 0, errors.New("mint: nil or invalid dependency")
	}
	current, err := m.trust.CurrentTrust(ctx, repo)
	if err != nil {
		return InstallationBinding{}, repoRef{}, 0, fmt.Errorf("mint: read current trust: %w", err)
	}
	if current.Profile == nil {
		return InstallationBinding{}, repoRef{}, 0, fmt.Errorf("mint: no current trust profile for %s: %w",
			repo, ErrTrustProfileDrift)
	}
	if err := current.Profile.Validate(); err != nil {
		return InstallationBinding{}, repoRef{}, 0, fmt.Errorf("mint: current trust profile for %s: %w", repo, err)
	}
	if current.Profile.Repo != repo {
		return InstallationBinding{}, repoRef{}, 0, fmt.Errorf("mint: current trust profile names a different repository: %w",
			ErrTrustProfileDrift)
	}
	binding, err := m.resolver.Resolve(ctx, parsed.owner)
	if err != nil {
		return InstallationBinding{}, repoRef{}, 0, fmt.Errorf("mint: %w", err)
	}
	return binding, parsed, current.Profile.RepositoryID, nil
}

func (m *Minter) mintResolved(ctx context.Context, binding InstallationBinding, repo repoRef, repositoryID int64) (InstallationToken, error) {
	creds, err := m.keystore.LoadApp(binding.RegistrationOwnerID)
	if err != nil {
		return InstallationToken{}, fmt.Errorf("mint: load registration: %w", err)
	}
	if creds.AppID != binding.RegistrationID ||
		!strings.EqualFold(binding.Account, repo.owner) ||
		binding.InstallationID <= 0 || binding.AccountID <= 0 || repositoryID <= 0 {
		return InstallationToken{}, fmt.Errorf("mint: resolved installation binding changed: %w", ErrInstallationResolution)
	}
	jwt, err := AppJWT(creds.Key, creds.AppID, m.now())
	if err != nil {
		return InstallationToken{}, fmt.Errorf("mint: %w", err)
	}

	body, err := json.Marshal(mintRequest{RepositoryIDs: []int64{repositoryID}, Permissions: PublishPermissions})
	if err != nil {
		return InstallationToken{}, fmt.Errorf("mint: encode request: %w", err)
	}
	path := fmt.Sprintf("/app/installations/%d/access_tokens", binding.InstallationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return InstallationToken{}, fmt.Errorf("mint: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt.Reveal())
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.client.Do(req)
	if err != nil {
		return InstallationToken{}, fmt.Errorf("mint: %w", err)
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode != http.StatusCreated {
		return InstallationToken{}, fmt.Errorf("mint: %w", &APIError{Status: resp.StatusCode, RequestPath: path})
	}
	var minted mintResponse
	if err := decodeResponse(resp.Body, &minted); err != nil {
		return InstallationToken{}, fmt.Errorf("mint: decode response: %w", err)
	}
	expiresAt, err := time.Parse(time.RFC3339, minted.ExpiresAt.Reveal())
	if err != nil {
		// time.ParseError includes the rejected value. Treat the untrusted
		// field as secret-shaped and return no response content: a proxy
		// can place credential material in any string field.
		return InstallationToken{}, errors.New("mint: response carries an invalid token expiry")
	}
	// Fail closed on any grant that differs from the request, in either
	// direction: a narrower grant (an installation whose App
	// permissions were narrowed) would fail the publish path halfway,
	// and a broader one — an extra permission key, an all-repositories
	// selection, or a different repository set — would circulate more
	// authority than the audit row records. Scope names are not secret,
	// so the errors may carry them.
	if !maps.Equal(minted.Permissions, publishPermissionScopes) {
		return InstallationToken{}, fmt.Errorf("mint: granted permissions differ from the request: %w", ErrGrantMismatch)
	}
	if minted.RepositorySelection != "selected" || len(minted.Repositories) != 1 ||
		minted.Repositories[0].ID != repositoryID || minted.Repositories[0].Name != repo.name {
		return InstallationToken{}, fmt.Errorf("mint: granted repository scope differs from the request: %w", ErrGrantMismatch)
	}
	// Returned-object trust boundary: a syntactically valid 201 that
	// omits the token or carries an unusable expiry must not advance
	// the durable audit barrier or circulate as a credential.
	if minted.Token.Reveal() == "" {
		return InstallationToken{}, errors.New("mint: response carries no token")
	}
	if !expiresAt.After(m.now()) {
		return InstallationToken{}, errors.New("mint: token expiry is not usable")
	}

	// The validation above proved the grant identical to the request,
	// so the typed record and token carry PublishPermissions as both
	// requested and granted; any other grant never reaches this point.
	if err := m.recorder.RecordMint(MintRecord{
		MintedAt:       m.now().UTC(),
		RegistrationID: binding.RegistrationID,
		InstallationID: binding.InstallationID,
		Repo:           repo.path(),
		Requested:      PublishPermissions,
		Granted:        PublishPermissions,
		ExpiresAt:      expiresAt,
	}); err != nil {
		return InstallationToken{}, fmt.Errorf("mint: %w", err)
	}

	return InstallationToken{
		Token:          minted.Token,
		ExpiresAt:      expiresAt,
		RegistrationID: binding.RegistrationID,
		InstallationID: binding.InstallationID,
		RepositoryID:   repositoryID,
		Repo:           repo.path(),
		Permissions:    PublishPermissions,
	}, nil
}

// drainAndClose discards at most a small remainder so the connection
// can be reused, then closes. Response bodies are never retained or
// surfaced: an error body can echo credential material.
func drainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 4096))
	_ = body.Close()
}
