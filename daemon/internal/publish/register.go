package publish

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"html/template"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Registration runs the GitHub App manifest flow (docs/plan.md §10):
// the daemon serves a one-shot local page whose form posts the manifest
// to GitHub, the user approves it in the browser, GitHub redirects back
// with a temporary code, and the conversion exchange returns the App's
// identity and private key. The key travels response → parsed key →
// keystore inside ExchangeCode: no temp file, no bare-string return, no
// rendering (the sensitive conversion fields decode into Secret).

const maxGitHubAppNameLength = 34

// Manifest is the App manifest the user approves on GitHub. Name,
// Public, and the permission set are set by ManifestForm from the
// registration target; RedirectURL is set by Register to the local
// callback for the same-machine convenience flow, or by the caller
// when relaying the form and one-time conversion code between machines.
type Manifest struct {
	Name               string      `json:"name"`
	URL                string      `json:"url"`
	RedirectURL        string      `json:"redirect_url,omitempty"`
	Public             bool        `json:"public"`
	DefaultPermissions Permissions `json:"default_permissions"`
}

// RegistrationTarget identifies the account that will own the App and the
// topology selected for it. Organization selects GitHub's organization-owned
// registration endpoint. Public is valid only for the personal-account
// default; a private personal registration remains valid for a work-account
// opt-in whose repository-owning account is personal.
type RegistrationTarget struct {
	Owner        string
	OwnerID      int64
	Organization bool
	Visibility   AppVisibility
}

func (t RegistrationTarget) validate() error {
	if err := validateOwnerLogin(t.Owner); err != nil {
		return fmt.Errorf("registration target: %w", err)
	}
	if _, err := appOwnerKey(t.OwnerID); err != nil {
		return fmt.Errorf("registration target: %w", err)
	}
	if !t.Visibility.valid() {
		return fmt.Errorf("registration target: invalid App visibility %q", t.Visibility)
	}
	if t.Organization && t.Visibility != AppVisibilityPrivate {
		return errors.New("registration target: organization-owned Apps require the private work-account posture")
	}
	return nil
}

// GeneratedAppName returns the suggested globally unique App name for one
// registration attempt. retry is zero for the initial suggestion; each
// collision increments it, producing -2, -3, and so on. Truncation drops the
// Freeside prefix first, then truncates the owning-account segment, and only
// truncates the operator login when that login cannot fit by itself. The
// conversion response remains canonical because GitHub may edit or reject the
// suggestion.
func GeneratedAppName(operator, owner string, retry int) (string, error) {
	if err := validateOwnerLogin(operator); err != nil {
		return "", fmt.Errorf("generate App name: operator: %w", err)
	}
	if err := validateOwnerLogin(owner); err != nil {
		return "", fmt.Errorf("generate App name: owner: %w", err)
	}
	if retry < 0 {
		return "", errors.New("generate App name: retry must not be negative")
	}

	suffix := ""
	if retry > 0 {
		suffix = "-" + strconv.FormatUint(uint64(retry)+1, 10)
	}
	limit := maxGitHubAppNameLength - len(suffix)
	if len(operator) > limit {
		return strings.TrimRight(operator[:limit], "-") + suffix, nil
	}

	account := ""
	if !strings.EqualFold(owner, operator) {
		account = owner + "-"
	}
	full := "freeside-" + account + operator
	if len(full) <= limit {
		return full + suffix, nil
	}

	withoutPrefix := account + operator
	if len(withoutPrefix) <= limit {
		return withoutPrefix + suffix, nil
	}
	accountLimit := limit - len(operator) - 1
	if accountLimit <= 0 {
		return operator + suffix, nil
	}
	return strings.TrimRight(owner[:accountLimit], "-") + "-" + operator + suffix, nil
}

// Registrar registers the GitHub App. The HTTP client and both base
// URLs are injected: apiBaseURL for the conversion exchange (real:
// https://api.github.com), webBaseURL for the form action the user's
// browser posts to (real: https://github.com).
type Registrar struct {
	keystore   *Keystore
	client     *http.Client
	apiBaseURL string
	webBaseURL string
}

// NewRegistrar wires a Registrar around the keystore that will hold
// the registration's key material. The injected client is wrapped to
// never follow redirects (see noRedirect): a followed redirect would
// send the code-bearing conversion URL as the Referer to the target,
// and the code is credential-equivalent until GitHub consumes it.
func NewRegistrar(ks *Keystore, client *http.Client, apiBaseURL, webBaseURL string) *Registrar {
	// Trailing slashes are trimmed here because every URL below concatenates an
	// absolute, leading-slash path onto the base; a raw trailing slash would
	// emit a doubled separator (`...//settings/apps/new`) that servers and
	// proxies may treat as a distinct path or redirect.
	return &Registrar{
		keystore:   ks,
		client:     noRedirect(client),
		apiBaseURL: strings.TrimRight(apiBaseURL, "/"),
		webBaseURL: strings.TrimRight(webBaseURL, "/"),
	}
}

// ManifestForm returns the owner-targeted form action and fields the user's
// browser posts to GitHub. Producing this form is independent of ExchangeCode,
// so an organization owner may complete the browser step on another machine
// and relay only the one-time code to the requesting daemon.
func (r *Registrar) ManifestForm(
	target RegistrationTarget,
	operator string,
	retry int,
	m Manifest,
) (action string, fields url.Values, err error) {
	if err := target.validate(); err != nil {
		return "", nil, fmt.Errorf("register: %w", err)
	}
	m.Name, err = GeneratedAppName(operator, target.Owner, retry)
	if err != nil {
		return "", nil, fmt.Errorf("register: %w", err)
	}
	// The registration boundary owns the App's authority. Callers choose
	// presentation only; the manifest always carries the topology's
	// visibility and the package's pinned minimum permission set.
	m.Public = target.Visibility == AppVisibilityPublic
	m.DefaultPermissions = PublishPermissions
	data, err := json.Marshal(m)
	if err != nil {
		return "", nil, fmt.Errorf("register: encode manifest: %w", err)
	}
	path := "/settings/apps/new"
	if target.Organization {
		path = "/organizations/" + url.PathEscape(target.Owner) + "/settings/apps/new"
	}
	return r.webBaseURL + path, url.Values{"manifest": {string(data)}}, nil
}

// conversionResponse is the manifest-conversion body; every sensitive
// field decodes directly into Secret.
type conversionResponse struct {
	ID            int64             `json:"id"`
	Name          string            `json:"name"`
	Slug          string            `json:"slug"`
	ClientID      string            `json:"client_id"`
	Permissions   map[string]string `json:"permissions"`
	Pem           Secret            `json:"pem"`
	WebhookSecret Secret            `json:"webhook_secret"`
	ClientSecret  Secret            `json:"client_secret"`
	Owner         struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
		Type  string `json:"type"`
	} `json:"owner"`
}

// ExchangeCode converts the temporary manifest code and lands the credentials
// in the keystore before returning them. The canonical returned owner login
// and stable numeric ID must both match the expected owner, so a browser flow
// completed under the wrong personal or organization account, or under a
// renamed/login-reused account, cannot silently change credential ownership.
// The key's only existence outside GitHub is the protected storage write.
func (r *Registrar) ExchangeCode(
	ctx context.Context,
	code string,
	target RegistrationTarget,
) (AppCredentials, error) {
	if code == "" {
		return AppCredentials{}, ErrRegistrationDenied
	}
	if err := target.validate(); err != nil {
		return AppCredentials{}, fmt.Errorf("register: %w", err)
	}
	// The conversion URL embeds the code, which until GitHub consumes
	// it is credential-equivalent (it exchanges for the App key), so
	// every error on this path is scrubbed of the request URL: request
	// construction and transport failures wrap *url.Error, which
	// renders the full URL.
	path := "/app-manifests/" + url.PathEscape(code) + "/conversions"
	const redactedPath = "/app-manifests/{code}/conversions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.apiBaseURL+path, nil)
	if err != nil {
		return AppCredentials{}, fmt.Errorf("register: build request for %s: %w", redactedPath, redactURLError(err))
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := r.client.Do(req)
	if err != nil {
		return AppCredentials{}, fmt.Errorf("register: post %s: %w", redactedPath, redactURLError(err))
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode != http.StatusCreated {
		return AppCredentials{}, fmt.Errorf("register: %w", &APIError{Status: resp.StatusCode, RequestPath: redactedPath})
	}
	var conv conversionResponse
	if err := json.NewDecoder(resp.Body).Decode(&conv); err != nil {
		return AppCredentials{}, fmt.Errorf("register: decode conversion: %w", err)
	}
	// Returned-object trust boundary: a syntactically valid conversion
	// with no usable App identity must not overwrite the keystore —
	// credentials with issuer 0 would replace working ones and fail
	// every later mint.
	if conv.ID <= 0 {
		return AppCredentials{}, fmt.Errorf("register: conversion response carries app id %d", conv.ID)
	}
	if strings.TrimSpace(conv.Name) == "" || strings.TrimSpace(conv.Slug) == "" {
		return AppCredentials{}, errors.New("register: conversion response carries incomplete canonical App metadata")
	}
	if err := validateOwnerLogin(conv.Owner.Login); err != nil {
		return AppCredentials{}, errors.New("register: conversion response carries an invalid owner login")
	}
	if _, err := appOwnerKey(conv.Owner.ID); err != nil {
		return AppCredentials{}, fmt.Errorf("register: conversion response: %w", err)
	}
	expectedOwnerType := "User"
	if target.Organization {
		expectedOwnerType = "Organization"
	}
	if conv.Owner.Type != expectedOwnerType {
		return AppCredentials{}, errors.New("register: converted App owner type does not match the registration target")
	}
	if !strings.EqualFold(conv.Owner.Login, target.Owner) {
		return AppCredentials{}, errors.New("register: converted App owner does not match the expected owner")
	}
	if conv.Owner.ID != target.OwnerID {
		return AppCredentials{}, fmt.Errorf(
			"register: converted App owner id %d does not match expected owner id %d",
			conv.Owner.ID,
			target.OwnerID,
		)
	}
	if !maps.Equal(conv.Permissions, publishPermissionScopes) {
		return AppCredentials{}, errors.New("register: converted app permissions differ from the required set")
	}

	block, _ := pem.Decode([]byte(conv.Pem.Reveal()))
	if block == nil || block.Type != "RSA PRIVATE KEY" {
		return AppCredentials{}, errors.New("register: conversion pem is not an RSA PRIVATE KEY")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return AppCredentials{}, fmt.Errorf("register: parse conversion key: %w", err)
	}
	if err := r.verifyVisibility(ctx, conv.Slug, conv.ID, key, target.Visibility); err != nil {
		return AppCredentials{}, err
	}

	creds := AppCredentials{
		Owner:         conv.Owner.Login,
		OwnerID:       conv.Owner.ID,
		Visibility:    target.Visibility,
		AppID:         conv.ID,
		Name:          conv.Name,
		Slug:          conv.Slug,
		ClientID:      conv.ClientID,
		Key:           key,
		WebhookSecret: conv.WebhookSecret,
		ClientSecret:  conv.ClientSecret,
	}
	creds.KeyID, err = appKeyID(key)
	if err != nil {
		return AppCredentials{}, fmt.Errorf("register: %w", err)
	}
	if err := r.keystore.SaveApp(creds); err != nil {
		return AppCredentials{}, fmt.Errorf("register: %w", err)
	}
	return creds, nil
}

// verifyVisibility independently observes whether the converted App is
// publicly readable. GitHub's unauthenticated App endpoint returns the record
// only for public Apps; a private App is not found. The manifest conversion
// response does not carry visibility, so caller-supplied topology is not
// trusted without this post-conversion check.
func (r *Registrar) verifyVisibility(
	ctx context.Context,
	slug string,
	appID int64,
	key *rsa.PrivateKey,
	expected AppVisibility,
) error {
	const redactedPath = "/apps/{slug}"
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		r.apiBaseURL+"/apps/"+url.PathEscape(slug),
		nil,
	)
	if err != nil {
		return fmt.Errorf("register: build visibility request for %s: %w", redactedPath, redactURLError(err))
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("register: get %s: %w", redactedPath, redactURLError(err))
	}
	defer drainAndClose(resp.Body)

	actual := AppVisibilityPrivate
	switch resp.StatusCode {
	case http.StatusOK:
		var app struct {
			ID int64 `json:"id"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&app); err != nil {
			return fmt.Errorf("register: decode public App metadata: %w", err)
		}
		if app.ID != appID {
			return errors.New("register: public App metadata does not match the converted App")
		}
		actual = AppVisibilityPublic
	case http.StatusNotFound:
		// GitHub exposes public Apps through this unauthenticated endpoint
		// and hides private Apps as not found. Prove that this is the
		// converted App, rather than treating an ambiguous 404 as private.
		if err := r.verifyAuthenticatedApp(ctx, appID, key); err != nil {
			return err
		}
	default:
		return fmt.Errorf("register: verify visibility: %w", &APIError{
			Status:      resp.StatusCode,
			RequestPath: redactedPath,
		})
	}
	if actual != expected {
		return errors.New("register: converted App visibility does not match the registration target")
	}
	return nil
}

func (r *Registrar) verifyAuthenticatedApp(ctx context.Context, appID int64, key *rsa.PrivateKey) error {
	const path = "/app"
	jwt, err := AppJWT(key, appID, time.Now())
	if err != nil {
		return fmt.Errorf("register: authenticate converted App: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.apiBaseURL+path, nil)
	if err != nil {
		return fmt.Errorf("register: build authenticated App request: %w", redactURLError(err))
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+jwt.Reveal())
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("register: get authenticated App: %w", redactURLError(err))
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("register: verify private App identity: %w", &APIError{
			Status:      resp.StatusCode,
			RequestPath: path,
		})
	}
	var app struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&app); err != nil {
		return fmt.Errorf("register: decode authenticated App metadata: %w", err)
	}
	if app.ID != appID {
		return errors.New("register: authenticated App metadata does not match the converted App")
	}
	return nil
}

// redactURLError strips a *url.Error wrapper, keeping only its
// underlying cause: the wrapper renders the full request URL, which on
// the conversion path embeds the manifest code.
func redactURLError(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return uerr.Err
	}
	return err
}

// formPage auto-submits the manifest to GitHub; html/template escaping
// keeps the embedded JSON safe in the attribute.
var formPage = template.Must(template.New("form").Parse(`<!DOCTYPE html>
<html><body onload="document.forms[0].submit()">
<form action="{{.Action}}" method="post">
<input type="hidden" name="manifest" value="{{.Manifest}}">
<noscript><button type="submit">Register the Freeside GitHub App</button></noscript>
</form></body></html>`))

// Register orchestrates the interactive flow for the expected owner login and
// stable numeric ID on the supplied listener:
// it serves the auto-submitting form, hands the browser to openURL,
// waits for GitHub's redirect to deliver the temporary code (or the
// context to expire), and exchanges it. A redirect without a code
// (user cancelled) is ErrRegistrationDenied. The callback path embeds
// an unguessable per-attempt nonce, so only the redirect carrying this
// registration's manifest can deliver a code: an unrelated local
// request cannot inject a foreign code or abort the flow.
func (r *Registrar) Register(
	ctx context.Context,
	target RegistrationTarget,
	operator string,
	retry int,
	m Manifest,
	l net.Listener,
	openURL func(string) error,
) (AppCredentials, error) {
	if err := target.validate(); err != nil {
		return AppCredentials{}, fmt.Errorf("register: %w", err)
	}
	local, err := loopbackListenerURL(l)
	if err != nil {
		return AppCredentials{}, fmt.Errorf("register: %w", err)
	}
	nonces := make([]byte, 32)
	if _, err := rand.Read(nonces); err != nil {
		return AppCredentials{}, fmt.Errorf("register: nonces: %w", err)
	}
	callbackPath := "/callback/" + hex.EncodeToString(nonces[:16])
	// The form page embeds the manifest, whose redirect_url carries the
	// callback nonce, so the page itself must be undisclosable too: it
	// lives at its own unguessable path, and everything else is a 404.
	formPath := "/setup/" + hex.EncodeToString(nonces[16:])

	m.RedirectURL = local + callbackPath
	action, fields, err := r.ManifestForm(target, operator, retry, m)
	if err != nil {
		return AppCredentials{}, err
	}

	codeCh := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != formPath {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = formPage.Execute(w, struct{ Action, Manifest string }{action, fields.Get("manifest")})
	})
	mux.HandleFunc("/callback/", func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != callbackPath {
			http.NotFound(w, req)
			return
		}
		select {
		case codeCh <- req.URL.Query().Get("code"):
		default: // a repeated redirect changes nothing; first code wins
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintln(w, "Registration received; you can close this tab.")
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(l)   //nolint:errcheck // Serve always returns non-nil on Close; the flow's outcome travels via codeCh
	defer srv.Close() //nolint:errcheck // best-effort teardown of the one-shot listener

	if err := openURL(local + formPath); err != nil {
		return AppCredentials{}, fmt.Errorf("register: open browser: %w", err)
	}

	select {
	case code := <-codeCh:
		return r.ExchangeCode(ctx, code, target)
	case <-ctx.Done():
		return AppCredentials{}, fmt.Errorf("register: %w", ctx.Err())
	}
}

// loopbackListenerURL fails closed unless the caller supplied an
// explicit loopback TCP address. An unspecified address such as :0 or
// [::]:0 may listen successfully but is not a usable or contained
// browser callback target; rewriting its URL would still leave the
// listener exposed on non-loopback interfaces.
func loopbackListenerURL(l net.Listener) (string, error) {
	if l == nil || l.Addr() == nil {
		return "", errors.New("registration listener has no address")
	}
	host, port, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		return "", fmt.Errorf("registration listener address: %w", err)
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || !addr.IsLoopback() {
		return "", fmt.Errorf("registration listener %s is not explicit loopback", l.Addr())
	}
	return "http://" + net.JoinHostPort(addr.String(), port), nil
}
