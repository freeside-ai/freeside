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
	Token       Secret
	ExpiresAt   time.Time
	Repo        string
	Permissions Permissions
}

// Minter mints installation tokens against the GitHub API. The HTTP
// client, API base URL, audit recorder, and clock are all injected;
// the keystore is the only credential source.
type Minter struct {
	keystore *Keystore
	client   *http.Client
	baseURL  string
	recorder Recorder
	now      func() time.Time
}

// NewMinter wires a Minter. baseURL is the GitHub API root (real:
// https://api.github.com; tests: an httptest server). The injected
// client is wrapped to never follow redirects (see noRedirect), so a
// caller-supplied CheckRedirect cannot carry the App JWT anywhere.
func NewMinter(ks *Keystore, client *http.Client, baseURL string, rec Recorder, now func() time.Time) *Minter {
	return &Minter{keystore: ks, client: noRedirect(client), baseURL: baseURL, recorder: rec, now: now}
}

// noRedirect copies a client and disables redirect-following: the
// GitHub API endpoints this package calls never legitimately redirect,
// and following one would re-send a request whose URL, Referer, or
// same-host Authorization header can carry credential material to the
// target (a misconfigured base URL or interposed proxy). A 3xx
// surfaces as its status through the ordinary redacted APIError path.
func noRedirect(c *http.Client) *http.Client {
	nc := *c
	nc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &nc
}

// mintRequest is the API request body; field order is pinned by the
// request golden test, which is the durable statement of the minimum
// scope set.
type mintRequest struct {
	Repositories []string    `json:"repositories"`
	Permissions  Permissions `json:"permissions"`
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
		Name string `json:"name"`
	} `json:"repositories"`
}

// MintInstallationToken mints a token for exactly one repository (repo
// is the repository name within the installation's account, per the
// GitHub access_tokens API) with the minimum publish permission set,
// and records the mint for audit before the token is returned: a mint
// whose audit row cannot be written fails.
func (m *Minter) MintInstallationToken(ctx context.Context, installationID int64, repo string) (InstallationToken, error) {
	if installationID <= 0 {
		return InstallationToken{}, fmt.Errorf("mint: invalid installation id %d", installationID)
	}
	if repo == "" {
		return InstallationToken{}, errors.New("mint: empty repository name")
	}

	apps, err := m.keystore.ListApps()
	if err != nil {
		return InstallationToken{}, fmt.Errorf("mint: %w", err)
	}
	if len(apps) == 0 {
		return InstallationToken{}, fmt.Errorf("mint: %w", ErrNoAppCredentials)
	}
	if len(apps) != 1 {
		return InstallationToken{}, errors.New("mint: multiple App registrations require owner resolution")
	}
	creds := apps[0]
	jwt, err := AppJWT(creds.Key, creds.AppID, m.now())
	if err != nil {
		return InstallationToken{}, fmt.Errorf("mint: %w", err)
	}

	body, err := json.Marshal(mintRequest{Repositories: []string{repo}, Permissions: PublishPermissions})
	if err != nil {
		return InstallationToken{}, fmt.Errorf("mint: encode request: %w", err)
	}
	path := fmt.Sprintf("/app/installations/%d/access_tokens", installationID)
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
	if err := json.NewDecoder(resp.Body).Decode(&minted); err != nil {
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
	if minted.RepositorySelection != "selected" || len(minted.Repositories) != 1 || minted.Repositories[0].Name != repo {
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
		InstallationID: installationID,
		Repo:           repo,
		Requested:      PublishPermissions,
		Granted:        PublishPermissions,
		ExpiresAt:      expiresAt,
	}); err != nil {
		return InstallationToken{}, fmt.Errorf("mint: %w", err)
	}

	return InstallationToken{
		Token:       minted.Token,
		ExpiresAt:   expiresAt,
		Repo:        repo,
		Permissions: PublishPermissions,
	}, nil
}

// drainAndClose discards at most a small remainder so the connection
// can be reused, then closes. Response bodies are never retained or
// surfaced: an error body can echo credential material.
func drainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 4096))
	_ = body.Close()
}
