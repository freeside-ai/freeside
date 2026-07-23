package publish

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	installationPageSize = 100
	installationMaxPages = 100
)

// InstallationBinding is the non-secret identity selected for a repository
// owner. RegistrationID is GitHub's stable numeric App ID; OwnerID identifies
// the account that owns the registration; InstallationID and AccountID name
// the selected installation and its account.
type InstallationBinding struct {
	RegistrationID      int64
	RegistrationOwner   string
	RegistrationOwnerID int64
	InstallationID      int64
	Account             string
	AccountID           int64
}

// ResolutionFailureReason is the safe, closed vocabulary retained for an
// installation object that failed validation.
type ResolutionFailureReason string

const (
	ResolutionPrivateMultipleInstallations ResolutionFailureReason = "private_registration_has_multiple_installations"
	ResolutionIdentityMismatch             ResolutionFailureReason = "installation_identity_mismatch"
	ResolutionRepositorySelectionMismatch  ResolutionFailureReason = "installation_repository_selection_mismatch"
	ResolutionDuplicateInstallation        ResolutionFailureReason = "duplicate_installation_identity"
	ResolutionPrivateOwnerMismatch         ResolutionFailureReason = "private_installation_owner_mismatch"
)

// AllResolutionFailureReasons is the single registration point for auditable
// installation-resolution failure reasons.
var AllResolutionFailureReasons = []ResolutionFailureReason{
	ResolutionPrivateMultipleInstallations,
	ResolutionIdentityMismatch,
	ResolutionRepositorySelectionMismatch,
	ResolutionDuplicateInstallation,
	ResolutionPrivateOwnerMismatch,
}

func (r ResolutionFailureReason) valid() bool {
	switch r {
	case ResolutionPrivateMultipleInstallations,
		ResolutionIdentityMismatch,
		ResolutionRepositorySelectionMismatch,
		ResolutionDuplicateInstallation,
		ResolutionPrivateOwnerMismatch:
		return true
	default:
		return false
	}
}

// ResolutionFailure records safe coordinates for a returned installation
// object that failed validation. It deliberately omits the observed account
// text: every decoded string is untrusted and may contain credential material.
type ResolutionFailure struct {
	RegistrationID int64
	ExpectedOwner  string
	Reason         ResolutionFailureReason
}

func (e *ResolutionFailure) Error() string {
	if !e.Reason.valid() {
		return fmt.Sprintf("installation resolution: registration %d for owner %q: invalid failure reason",
			e.RegistrationID, e.ExpectedOwner)
	}
	return fmt.Sprintf("installation resolution: registration %d for owner %q: %s",
		e.RegistrationID, e.ExpectedOwner, e.Reason)
}

func (e *ResolutionFailure) Is(target error) bool { return target == ErrInstallationResolution }

// InstallationResolver discovers the installation for a repository owner by
// enumerating every locally trusted App registration. Installation IDs are
// observations, never configuration: each Resolve re-reads GitHub and validates
// the returned App and account identities before selecting one.
type InstallationResolver struct {
	keystore *Keystore
	client   *http.Client
	baseURL  string
	now      func() time.Time
}

// NewInstallationResolver wires owner resolution to the registration keystore
// and GitHub's App-authenticated installation endpoint.
func NewInstallationResolver(ks *Keystore, client *http.Client, baseURL string, now func() time.Time) *InstallationResolver {
	return &InstallationResolver{keystore: ks, client: noRedirect(client), baseURL: baseURL, now: now}
}

type installationResponse struct {
	ID                  int64  `json:"id"`
	AppID               int64  `json:"app_id"`
	TargetID            int64  `json:"target_id"`
	RepositorySelection string `json:"repository_selection"`
	Account             struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
	} `json:"account"`
}

// Resolve returns the unique registration and installation whose canonical
// installation account matches owner. A private registration may have only
// its owner's single installation. No match or more than one match fails
// closed.
func (r *InstallationResolver) Resolve(ctx context.Context, owner string) (InstallationBinding, error) {
	if err := validateOwnerLogin(owner); err != nil {
		return InstallationBinding{}, fmt.Errorf("installation resolution: %w", err)
	}
	if r == nil || r.keystore == nil || r.client == nil || r.now == nil {
		return InstallationBinding{}, errors.New("installation resolution: nil dependency")
	}
	apps, err := r.keystore.ListApps()
	if err != nil {
		return InstallationBinding{}, fmt.Errorf("installation resolution: %w", err)
	}
	if len(apps) == 0 {
		return InstallationBinding{}, fmt.Errorf("installation resolution: %w", ErrNoAppCredentials)
	}

	var matches []InstallationBinding
	for _, app := range apps {
		installations, err := r.installations(ctx, app)
		if err != nil {
			return InstallationBinding{}, err
		}
		if app.Visibility == AppVisibilityPrivate && len(installations) > 1 {
			return InstallationBinding{}, &ResolutionFailure{
				RegistrationID: app.AppID,
				ExpectedOwner:  app.Owner,
				Reason:         ResolutionPrivateMultipleInstallations,
			}
		}
		seen := make(map[int64]struct{}, len(installations))
		for _, installation := range installations {
			if installation.RepositorySelection != "selected" {
				return InstallationBinding{}, &ResolutionFailure{
					RegistrationID: app.AppID,
					ExpectedOwner:  expectedInstallationOwner(app, owner),
					Reason:         ResolutionRepositorySelectionMismatch,
				}
			}
			if installation.ID <= 0 || installation.AppID != app.AppID ||
				installation.Account.ID <= 0 || installation.TargetID != installation.Account.ID ||
				validateOwnerLogin(installation.Account.Login) != nil {
				return InstallationBinding{}, &ResolutionFailure{
					RegistrationID: app.AppID,
					ExpectedOwner:  expectedInstallationOwner(app, owner),
					Reason:         ResolutionIdentityMismatch,
				}
			}
			if _, exists := seen[installation.ID]; exists {
				return InstallationBinding{}, &ResolutionFailure{
					RegistrationID: app.AppID,
					ExpectedOwner:  expectedInstallationOwner(app, owner),
					Reason:         ResolutionDuplicateInstallation,
				}
			}
			seen[installation.ID] = struct{}{}

			if app.Visibility == AppVisibilityPrivate &&
				(!strings.EqualFold(installation.Account.Login, app.Owner) || installation.Account.ID != app.OwnerID) {
				return InstallationBinding{}, &ResolutionFailure{
					RegistrationID: app.AppID,
					ExpectedOwner:  app.Owner,
					Reason:         ResolutionPrivateOwnerMismatch,
				}
			}
			if !strings.EqualFold(installation.Account.Login, owner) {
				continue
			}
			matches = append(matches, InstallationBinding{
				RegistrationID:      app.AppID,
				RegistrationOwner:   app.Owner,
				RegistrationOwnerID: app.OwnerID,
				InstallationID:      installation.ID,
				Account:             installation.Account.Login,
				AccountID:           installation.Account.ID,
			})
		}
	}

	switch len(matches) {
	case 0:
		return InstallationBinding{}, fmt.Errorf("installation resolution: owner %q: %w", owner, ErrNoInstallation)
	case 1:
		return matches[0], nil
	default:
		return InstallationBinding{}, fmt.Errorf("installation resolution: owner %q matched multiple registrations: %w",
			owner, ErrAmbiguousInstallation)
	}
}

func expectedInstallationOwner(app AppCredentials, requested string) string {
	if app.Visibility == AppVisibilityPrivate {
		return app.Owner
	}
	return requested
}

func (r *InstallationResolver) installations(ctx context.Context, app AppCredentials) ([]installationResponse, error) {
	jwt, err := AppJWT(app.Key, app.AppID, r.now())
	if err != nil {
		return nil, fmt.Errorf("installation resolution: registration %d: %w", app.AppID, err)
	}
	var all []installationResponse
	for page := 1; page <= installationMaxPages; page++ {
		path := "/app/installations?per_page=" + strconv.Itoa(installationPageSize) + "&page=" + strconv.Itoa(page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.baseURL+path, nil)
		if err != nil {
			return nil, fmt.Errorf("installation resolution: build request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+jwt.Reveal())
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		resp, err := r.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("installation resolution: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			drainAndClose(resp.Body)
			return nil, fmt.Errorf("installation resolution: %w", &APIError{
				Status:      resp.StatusCode,
				RequestPath: "/app/installations",
			})
		}
		var pageInstallations []installationResponse
		decodeErr := decodeResponse(resp.Body, &pageInstallations)
		drainAndClose(resp.Body)
		if decodeErr != nil {
			return nil, errors.New("installation resolution: decode response")
		}
		if len(pageInstallations) > installationPageSize {
			return nil, errors.New("installation resolution: response page exceeds the requested limit")
		}
		all = append(all, pageInstallations...)
		if len(pageInstallations) < installationPageSize {
			return all, nil
		}
	}
	return nil, errors.New("installation resolution: installation pagination exceeded the safety limit")
}
