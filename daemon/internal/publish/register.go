package publish

import (
	"context"
	"crypto/rand"
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
	"time"
)

// Registration runs the GitHub App manifest flow (docs/plan.md §10):
// the daemon serves a one-shot local page whose form posts the manifest
// to GitHub, the user approves it in the browser, GitHub redirects back
// with a temporary code, and the conversion exchange returns the App's
// identity and private key. The key travels response → parsed key →
// keystore inside ExchangeCode: no temp file, no bare-string return, no
// rendering (the sensitive conversion fields decode into Secret).

// Manifest is the App manifest the user approves on GitHub. The
// permission set is the same pinned minimum the minter requests;
// RedirectURL is set by Register to the local callback.
type Manifest struct {
	Name               string      `json:"name"`
	URL                string      `json:"url"`
	RedirectURL        string      `json:"redirect_url,omitempty"`
	Public             bool        `json:"public"`
	DefaultPermissions Permissions `json:"default_permissions"`
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
	return &Registrar{keystore: ks, client: noRedirect(client), apiBaseURL: apiBaseURL, webBaseURL: webBaseURL}
}

// ManifestForm returns the form action and fields the user's browser
// posts to GitHub: a single "manifest" field carrying the JSON.
func (r *Registrar) ManifestForm(m Manifest) (action string, fields url.Values, err error) {
	// The registration boundary owns the App's authority. Callers choose
	// identity and presentation only; the manifest always carries the
	// package's pinned minimum permission set.
	m.DefaultPermissions = PublishPermissions
	data, err := json.Marshal(m)
	if err != nil {
		return "", nil, fmt.Errorf("register: encode manifest: %w", err)
	}
	return r.webBaseURL + "/settings/apps/new", url.Values{"manifest": {string(data)}}, nil
}

// conversionResponse is the manifest-conversion body; every sensitive
// field decodes directly into Secret.
type conversionResponse struct {
	ID            int64             `json:"id"`
	Slug          string            `json:"slug"`
	ClientID      string            `json:"client_id"`
	Permissions   map[string]string `json:"permissions"`
	Pem           Secret            `json:"pem"`
	WebhookSecret Secret            `json:"webhook_secret"`
	ClientSecret  Secret            `json:"client_secret"`
}

// ExchangeCode converts the temporary manifest code and lands the
// credentials in the keystore before returning them: the key's only
// existence outside GitHub is the protected storage write.
func (r *Registrar) ExchangeCode(ctx context.Context, code string) (AppCredentials, error) {
	if code == "" {
		return AppCredentials{}, ErrRegistrationDenied
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

	creds := AppCredentials{
		AppID:         conv.ID,
		Slug:          conv.Slug,
		ClientID:      conv.ClientID,
		Key:           key,
		WebhookSecret: conv.WebhookSecret,
		ClientSecret:  conv.ClientSecret,
	}
	if err := r.keystore.SaveApp(creds); err != nil {
		return AppCredentials{}, fmt.Errorf("register: %w", err)
	}
	return creds, nil
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

// Register orchestrates the interactive flow on the supplied listener:
// it serves the auto-submitting form, hands the browser to openURL,
// waits for GitHub's redirect to deliver the temporary code (or the
// context to expire), and exchanges it. A redirect without a code
// (user cancelled) is ErrRegistrationDenied. The callback path embeds
// an unguessable per-attempt nonce, so only the redirect carrying this
// registration's manifest can deliver a code: an unrelated local
// request cannot inject a foreign code or abort the flow.
func (r *Registrar) Register(ctx context.Context, m Manifest, l net.Listener, openURL func(string) error) (AppCredentials, error) {
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
	action, fields, err := r.ManifestForm(m)
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
		fmt.Fprintln(w, "Registration received; you can close this tab.")
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(l)   //nolint:errcheck // Serve always returns non-nil on Close; the flow's outcome travels via codeCh
	defer srv.Close() //nolint:errcheck // best-effort teardown of the one-shot listener

	if err := openURL(local + formPath); err != nil {
		return AppCredentials{}, fmt.Errorf("register: open browser: %w", err)
	}

	select {
	case code := <-codeCh:
		return r.ExchangeCode(ctx, code)
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
