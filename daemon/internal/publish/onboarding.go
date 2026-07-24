package publish

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxImportedPEMBytes = 64 << 10

// AppRegistration is the non-secret identity needed to provision a distinct
// private key on another machine. Portable enrollment owns how this metadata
// reaches that machine; this package verifies it against GitHub before the
// imported key can replace local credentials.
type AppRegistration struct {
	Owner      string
	OwnerID    int64
	Visibility AppVisibility
	AppID      int64
	Name       string
	Slug       string
	ClientID   string
}

func (r AppRegistration) validate() error {
	if err := validateOwnerLogin(r.Owner); err != nil {
		return fmt.Errorf("app registration: %w", err)
	}
	if _, err := appOwnerKey(r.OwnerID); err != nil {
		return fmt.Errorf("app registration: %w", err)
	}
	if !r.Visibility.valid() {
		return fmt.Errorf("app registration: invalid visibility %q", r.Visibility)
	}
	if r.AppID <= 0 {
		return fmt.Errorf("app registration: app id %d is invalid", r.AppID)
	}
	if strings.TrimSpace(r.Name) == "" || strings.TrimSpace(r.Slug) == "" {
		return errors.New("app registration: canonical name and slug are required")
	}
	if strings.TrimSpace(r.Name) != r.Name || strings.TrimSpace(r.Slug) != r.Slug {
		return errors.New("app registration: canonical name or slug has surrounding whitespace")
	}
	if len(r.Slug) > maxGitHubAppNameLength {
		return errors.New("app registration: canonical slug exceeds GitHub's App-name limit")
	}
	for i, char := range r.Slug {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') {
			continue
		}
		if char == '-' && i > 0 && i < len(r.Slug)-1 {
			continue
		}
		return errors.New("app registration: canonical slug is not a GitHub App slug")
	}
	return nil
}

// Registration returns the credential's non-secret registration identity.
func (c AppCredentials) Registration() AppRegistration {
	return AppRegistration{
		Owner: c.Owner, OwnerID: c.OwnerID, Visibility: c.Visibility,
		AppID: c.AppID, Name: c.Name, Slug: c.Slug, ClientID: c.ClientID,
	}
}

// CredentialStepKind is the next operator action in the public per-user
// credential prerequisite.
type CredentialStepKind string

const (
	CredentialStepRegister CredentialStepKind = "register_app"
	CredentialStepKey      CredentialStepKind = "generate_machine_key"
	CredentialStepInstall  CredentialStepKind = "install_app"
	CredentialStepReady    CredentialStepKind = "ready"
)

// AllCredentialStepKinds is the single registration point for onboarding
// steps.
var AllCredentialStepKinds = []CredentialStepKind{
	CredentialStepRegister,
	CredentialStepKey,
	CredentialStepInstall,
	CredentialStepReady,
}

func (k CredentialStepKind) valid() bool {
	switch k {
	case CredentialStepRegister, CredentialStepKey, CredentialStepInstall, CredentialStepReady:
		return true
	default:
		return false
	}
}

func credentialStep(step CredentialStep) CredentialStep {
	if !step.Kind.valid() {
		panic("publish: invalid credential step kind")
	}
	return step
}

// CredentialStep describes one safe operator action. ManifestFields is present
// only for registration and contains no credential. URL is present for key
// generation or installation. Binding is present only when the prerequisite is
// complete.
type CredentialStep struct {
	Kind                            CredentialStepKind
	Registration                    *AppRegistration
	ManifestAction                  string
	ManifestFields                  url.Values
	URL                             string
	Guidance                        []string
	OrganizationApprovalMayBeNeeded bool
	Binding                         *InstallationBinding
}

// CredentialRequest supplies the operator identity, repository owner, and any
// already durable registration identity. A nil Registration means this is the
// first machine and no registration is known yet.
type CredentialRequest struct {
	Operator        string
	OperatorID      int64
	RepositoryOwner string
	Registration    *AppRegistration
	Retry           int
	Manifest        Manifest
}

// CredentialOnboarder composes the registrar, keystore, and installation
// resolver into the prerequisite steps consumed by setup/onboard packaging.
type CredentialOnboarder struct {
	keystore   *Keystore
	registrar  *Registrar
	resolver   *InstallationResolver
	client     *http.Client
	apiBaseURL string
	webBaseURL string
	now        func() time.Time
}

// NewCredentialOnboarder constructs the public per-user onboarding flow.
// Janitor coverage remains mandatory because even installation discovery must
// not make a registration operational without reconciliation.
func NewCredentialOnboarder(
	ks *Keystore,
	client *http.Client,
	apiBaseURL, webBaseURL string,
	now func() time.Time,
	janitor JanitorStatus,
) *CredentialOnboarder {
	// Normalize once so every consumer (the onboarder's own URL builders and
	// the registrar/resolver it composes) shares the same trailing-slash-free
	// base; the leaf constructors trim again defensively for direct callers.
	apiBaseURL = strings.TrimRight(apiBaseURL, "/")
	webBaseURL = strings.TrimRight(webBaseURL, "/")
	return &CredentialOnboarder{
		keystore:   ks,
		registrar:  NewRegistrar(ks, client, apiBaseURL, webBaseURL),
		resolver:   NewInstallationResolverWithJanitor(ks, client, apiBaseURL, now, janitor),
		client:     noRedirect(client),
		apiBaseURL: apiBaseURL,
		webBaseURL: webBaseURL,
		now:        now,
	}
}

// Next returns the next prerequisite action without performing an interactive
// browser effect. The packaging layer owns opening the emitted URL or posting
// the manifest form.
func (o *CredentialOnboarder) Next(ctx context.Context, req CredentialRequest) (CredentialStep, error) {
	if o == nil || o.keystore == nil || o.registrar == nil || o.resolver == nil ||
		o.client == nil || o.now == nil || o.apiBaseURL == "" || o.webBaseURL == "" {
		return CredentialStep{}, errors.New("credential onboarding: nil or invalid dependency")
	}
	if err := validateOwnerLogin(req.Operator); err != nil {
		return CredentialStep{}, fmt.Errorf("credential onboarding: operator: %w", err)
	}
	if _, err := appOwnerKey(req.OperatorID); err != nil {
		return CredentialStep{}, fmt.Errorf("credential onboarding: operator: %w", err)
	}
	if err := validateOwnerLogin(req.RepositoryOwner); err != nil {
		return CredentialStep{}, fmt.Errorf("credential onboarding: repository owner: %w", err)
	}
	if req.Retry < 0 {
		return CredentialStep{}, errors.New("credential onboarding: retry must not be negative")
	}

	registration := req.Registration
	if registration == nil {
		apps, err := o.keystore.ListApps()
		if err != nil {
			return CredentialStep{}, fmt.Errorf("credential onboarding: %w", err)
		}
		switch len(apps) {
		case 0:
			target := RegistrationTarget{
				Owner: req.Operator, OwnerID: req.OperatorID,
				Visibility: AppVisibilityPublic,
			}
			action, fields, err := o.registrar.ManifestForm(target, req.Operator, req.Retry, req.Manifest)
			if err != nil {
				return CredentialStep{}, fmt.Errorf("credential onboarding: %w", err)
			}
			return credentialStep(CredentialStep{
				Kind:           CredentialStepRegister,
				ManifestAction: action,
				ManifestFields: fields,
				Guidance: []string{
					"Create the public App on your personal GitHub account.",
					"The generated App name is a suggestion; Freeside records GitHub's canonical App ID and slug.",
				},
			}), nil
		case 1:
			inferred := apps[0].Registration()
			registration = &inferred
		default:
			return CredentialStep{}, fmt.Errorf("credential onboarding: %w", ErrAmbiguousAppRegistration)
		}
	}
	if err := registration.validate(); err != nil {
		return CredentialStep{}, fmt.Errorf("credential onboarding: %w", err)
	}
	if registration.Visibility != AppVisibilityPublic ||
		registration.OwnerID != req.OperatorID ||
		!strings.EqualFold(registration.Owner, req.Operator) {
		return CredentialStep{}, errors.New("credential onboarding: registration is not the operator's public personal App")
	}

	creds, err := o.keystore.LoadApp(registration.OwnerID)
	if errors.Is(err, ErrNoAppRegistration) {
		keyURL, urlErr := o.KeySettingsURL(*registration)
		if urlErr != nil {
			return CredentialStep{}, urlErr
		}
		return credentialStep(CredentialStep{
			Kind:         CredentialStepKey,
			Registration: registration,
			URL:          keyURL,
			Guidance: []string{
				"Generate a new private key for this machine and import the downloaded PEM.",
				"Do not copy a PEM from another machine; each machine must have a separately revocable key.",
			},
		}), nil
	}
	if err != nil {
		return CredentialStep{}, fmt.Errorf("credential onboarding: %w", err)
	}
	if !sameRegistration(creds.Registration(), *registration) {
		return CredentialStep{}, fmt.Errorf("credential onboarding: %w", ErrAppRegistrationMismatch)
	}

	binding, err := o.resolver.ResolveRegistration(
		ctx,
		req.RepositoryOwner,
		registration.AppID,
	)
	// A temporarily inactive janitor (starting up, or mid-pass with coverage
	// deliberately cleared) is not-ready, not a fatal error: coverage cannot
	// yet confirm an installation, so surface the install/resume step just as
	// a confirmed absence does. WaitForInstallation treats the two conditions
	// identically; Next stays symmetric.
	if errors.Is(err, ErrNoInstallation) || errors.Is(err, ErrJanitorInactive) {
		installURL, urlErr := o.InstallationURL(*registration)
		if urlErr != nil {
			return CredentialStep{}, urlErr
		}
		return credentialStep(CredentialStep{
			Kind:         CredentialStepInstall,
			Registration: registration,
			URL:          installURL,
			Guidance: []string{
				"Select only the repository being onboarded.",
				"For an organization, GitHub may route the request to an owner for approval; resume after approval.",
			},
			OrganizationApprovalMayBeNeeded: true,
		}), nil
	}
	if err != nil {
		return CredentialStep{}, fmt.Errorf("credential onboarding: %w", err)
	}
	return credentialStep(CredentialStep{
		Kind: CredentialStepReady, Registration: registration, Binding: &binding,
	}), nil
}

// CompleteRegistration exchanges the manifest code for the operator's public
// personal registration and lands its initial key directly in the keystore.
func (o *CredentialOnboarder) CompleteRegistration(
	ctx context.Context,
	code, operator string,
	operatorID int64,
) (AppCredentials, error) {
	if o == nil || o.registrar == nil {
		return AppCredentials{}, errors.New("credential onboarding: nil registrar")
	}
	return o.registrar.ExchangeCode(ctx, code, RegistrationTarget{
		Owner: operator, OwnerID: operatorID, Visibility: AppVisibilityPublic,
	})
}

// KeySettingsURL is the personal-account App settings page containing the
// private-key generator. The public default never uses an organization-owned
// registration.
func (o *CredentialOnboarder) KeySettingsURL(registration AppRegistration) (string, error) {
	if o == nil || o.webBaseURL == "" {
		return "", errors.New("credential onboarding: empty web base URL")
	}
	if err := registration.validate(); err != nil {
		return "", fmt.Errorf("credential onboarding: %w", err)
	}
	if registration.Visibility != AppVisibilityPublic {
		return "", errors.New("credential onboarding: key settings URL is only defined for the public personal default")
	}
	return o.webBaseURL + "/settings/apps/" + url.PathEscape(registration.Slug), nil
}

// InstallationURL is GitHub's native install-or-request-approval page.
func (o *CredentialOnboarder) InstallationURL(registration AppRegistration) (string, error) {
	if o == nil || o.webBaseURL == "" {
		return "", errors.New("credential onboarding: empty web base URL")
	}
	if err := registration.validate(); err != nil {
		return "", fmt.Errorf("credential onboarding: %w", err)
	}
	return o.webBaseURL + "/apps/" + url.PathEscape(registration.Slug) + "/installations/new", nil
}

// ImportKey verifies a downloaded PEM against the canonical App and its
// visibility before it can replace local credentials. The bounded PEM bytes
// are cleared after parsing; only the parsed key reaches SaveApp.
func (o *CredentialOnboarder) ImportKey(
	ctx context.Context,
	registration AppRegistration,
	pemReader io.Reader,
) (AppCredentials, error) {
	if o == nil || o.keystore == nil || o.client == nil || o.now == nil || pemReader == nil {
		return AppCredentials{}, errors.New("credential onboarding: nil key-import dependency")
	}
	if err := registration.validate(); err != nil {
		return AppCredentials{}, fmt.Errorf("credential onboarding: %w", err)
	}
	raw, err := io.ReadAll(io.LimitReader(pemReader, maxImportedPEMBytes+1))
	if err != nil {
		return AppCredentials{}, fmt.Errorf("credential onboarding: read private key: %w", err)
	}
	defer clear(raw)
	if len(raw) > maxImportedPEMBytes {
		return AppCredentials{}, errors.New("credential onboarding: private key PEM exceeds the size limit")
	}
	trimmed := bytes.TrimSpace(raw)
	if !bytes.HasPrefix(trimmed, []byte("-----BEGIN RSA PRIVATE KEY-----")) {
		return AppCredentials{}, errors.New("credential onboarding: downloaded key is not one RSA PRIVATE KEY PEM")
	}
	block, rest := pem.Decode(trimmed)
	if block == nil || block.Type != "RSA PRIVATE KEY" || len(rest) != 0 {
		return AppCredentials{}, errors.New("credential onboarding: downloaded key is not one RSA PRIVATE KEY PEM")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return AppCredentials{}, fmt.Errorf("credential onboarding: parse private key: %w", err)
	}
	if err := key.Validate(); err != nil {
		return AppCredentials{}, fmt.Errorf("credential onboarding: validate private key: %w", err)
	}
	if err := o.verifyRegistration(ctx, registration, key); err != nil {
		return AppCredentials{}, fmt.Errorf("credential onboarding: %w", err)
	}

	creds := AppCredentials{
		Owner: registration.Owner, OwnerID: registration.OwnerID,
		Visibility: registration.Visibility, AppID: registration.AppID,
		Name: registration.Name, Slug: registration.Slug, ClientID: registration.ClientID,
		Key: key,
	}
	existing, err := o.keystore.LoadApp(registration.OwnerID)
	if err == nil {
		if !sameRegistration(existing.Registration(), registration) {
			return AppCredentials{}, fmt.Errorf("credential onboarding: %w", ErrAppRegistrationMismatch)
		}
		// Additional-key downloads do not reissue the one-time manifest
		// secrets. Preserve them when this is an in-place machine-key
		// rotation instead of erasing still-valid local metadata.
		creds.WebhookSecret = existing.WebhookSecret
		creds.ClientSecret = existing.ClientSecret
	} else if !errors.Is(err, ErrNoAppRegistration) {
		return AppCredentials{}, fmt.Errorf("credential onboarding: inspect existing registration: %w", err)
	}
	creds.KeyID, err = appKeyID(key)
	if err != nil {
		return AppCredentials{}, fmt.Errorf("credential onboarding: %w", err)
	}
	if err := o.keystore.SaveApp(creds); err != nil {
		return AppCredentials{}, fmt.Errorf("credential onboarding: %w", err)
	}
	return creds, nil
}

// WaitForInstallation polls canonical App-authenticated discovery until the
// selected installation appears or the context ends.
func (o *CredentialOnboarder) WaitForInstallation(
	ctx context.Context,
	registrationID int64,
	repositoryOwner string,
	interval time.Duration,
) (InstallationBinding, error) {
	if o == nil || o.resolver == nil {
		return InstallationBinding{}, errors.New("credential onboarding: nil installation resolver")
	}
	if interval <= 0 {
		return InstallationBinding{}, errors.New("credential onboarding: installation poll interval must be positive")
	}
	for {
		binding, err := o.resolver.ResolveRegistration(ctx, repositoryOwner, registrationID)
		if err == nil {
			return binding, nil
		}
		// The always-on janitor deliberately clears coverage while each new
		// exhaustive pass is in flight. Both no installation and temporarily
		// inactive coverage mean "keep observing", never "operate anyway".
		if !errors.Is(err, ErrNoInstallation) && !errors.Is(err, ErrJanitorInactive) {
			return InstallationBinding{}, fmt.Errorf("credential onboarding: wait for installation: %w", err)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return InstallationBinding{}, fmt.Errorf("credential onboarding: wait for installation: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

type authenticatedAppResponse struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Slug     string `json:"slug"`
	ClientID string `json:"client_id"`
	Owner    struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
	} `json:"owner"`
}

func (o *CredentialOnboarder) verifyRegistration(
	ctx context.Context,
	registration AppRegistration,
	key *rsa.PrivateKey,
) error {
	jwt, err := AppJWT(key, registration.AppID, o.now())
	if err != nil {
		return fmt.Errorf("authenticate imported key: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.apiBaseURL+"/app", nil)
	if err != nil {
		return fmt.Errorf("build authenticated App request: %w", redactURLError(err))
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+jwt.Reveal())
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := o.client.Do(req)
	if err != nil {
		return fmt.Errorf("get authenticated App: %w", redactURLError(err))
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return &APIError{Status: resp.StatusCode, RequestPath: "/app"}
	}
	var app authenticatedAppResponse
	if err := decodeResponse(resp.Body, &app); err != nil {
		return fmt.Errorf("decode authenticated App metadata: %w", err)
	}
	if app.ID != registration.AppID ||
		app.Owner.ID != registration.OwnerID ||
		!strings.EqualFold(app.Owner.Login, registration.Owner) ||
		app.Name != registration.Name ||
		app.Slug != registration.Slug ||
		(registration.ClientID != "" && app.ClientID != registration.ClientID) {
		return ErrAppRegistrationMismatch
	}

	req, err = http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		o.apiBaseURL+"/apps/"+url.PathEscape(registration.Slug),
		nil,
	)
	if err != nil {
		return fmt.Errorf("build public App request: %w", redactURLError(err))
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err = o.client.Do(req)
	if err != nil {
		return fmt.Errorf("get public App metadata: %w", redactURLError(err))
	}
	defer drainAndClose(resp.Body)

	actual := AppVisibilityPrivate
	switch resp.StatusCode {
	case http.StatusOK:
		var publicApp struct {
			ID int64 `json:"id"`
		}
		if err := decodeResponse(resp.Body, &publicApp); err != nil {
			return fmt.Errorf("decode public App metadata: %w", err)
		}
		if publicApp.ID != registration.AppID {
			return ErrAppRegistrationMismatch
		}
		actual = AppVisibilityPublic
	case http.StatusNotFound:
	default:
		return &APIError{Status: resp.StatusCode, RequestPath: "/apps/{slug}"}
	}
	if actual != registration.Visibility {
		return ErrAppVisibilityMismatch
	}
	return nil
}

func sameRegistration(a, b AppRegistration) bool {
	return a.OwnerID == b.OwnerID &&
		strings.EqualFold(a.Owner, b.Owner) &&
		a.Visibility == b.Visibility &&
		a.AppID == b.AppID &&
		a.Name == b.Name &&
		a.Slug == b.Slug &&
		a.ClientID == b.ClientID
}
