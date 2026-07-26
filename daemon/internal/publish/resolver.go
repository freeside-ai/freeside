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
	janitor  JanitorStatus
}

type resolverJanitorFaultSource interface {
	RegistrationFaults() []JanitorRegistrationFault
}

// NewInstallationResolver wires owner resolution without janitor coverage.
// It exists for explicit fail-closed construction tests; every registration
// will be refused before GitHub is contacted.
func NewInstallationResolver(ks *Keystore, client *http.Client, baseURL string, now func() time.Time) *InstallationResolver {
	// Trailing slash trimmed: installation paths concatenate a leading-slash
	// path onto baseURL, so a raw trailing slash would double the separator.
	return &InstallationResolver{keystore: ks, client: noRedirect(client), baseURL: strings.TrimRight(baseURL, "/"), now: now}
}

// NewInstallationResolverWithJanitor wires resolution to the always-on
// installation janitor. The status gates resolution twice: nothing reaches
// GitHub while no candidate registration is covered by the janitor's latest
// successful pass, and a binding is only returned for a registration that pass
// covered. Coverage of a registration that produced no match for the requested
// owner is therefore no longer required, since that registration never provides
// the token the gate exists to prove cleanup for; every registration is still
// enumerated and matched, so ambiguity detection is untouched (#291).
//
// This narrows the janitor gate only. Resolution still fails closed for every
// owner when a registration cannot be enumerated or validated at all, because
// an unreadable registration's accounts are unknown and the match set it would
// have contributed to must be complete for ambiguity detection to be sound
// (keystore.go's enumeration rule, #279). A registration whose fault also
// breaks its listing (a revoked key, a forge outage scoped to it) therefore
// still denies unrelated owners.
func NewInstallationResolverWithJanitor(
	ks *Keystore,
	client *http.Client,
	baseURL string,
	now func() time.Time,
	janitor JanitorStatus,
) *InstallationResolver {
	return &InstallationResolver{
		keystore: ks,
		client:   noRedirect(client),
		baseURL:  strings.TrimRight(baseURL, "/"),
		now:      now,
		janitor:  janitor,
	}
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
	return r.resolve(ctx, owner, 0)
}

// ResolveRegistration returns the installation for owner under one explicitly
// selected registration. Onboarding uses this scoped form so another local App
// cannot satisfy the selected registration's prerequisite.
func (r *InstallationResolver) ResolveRegistration(
	ctx context.Context,
	owner string,
	registrationID int64,
) (InstallationBinding, error) {
	if registrationID <= 0 {
		return InstallationBinding{}, fmt.Errorf(
			"installation resolution: registration id %d is invalid",
			registrationID,
		)
	}
	return r.resolve(ctx, owner, registrationID)
}

func (r *InstallationResolver) resolve(
	ctx context.Context,
	owner string,
	registrationID int64,
) (InstallationBinding, error) {
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
	if registrationID > 0 {
		selected := make([]AppCredentials, 0, 1)
		for _, app := range apps {
			if app.AppID == registrationID {
				selected = append(selected, app)
			}
		}
		if len(selected) == 0 {
			return InstallationBinding{}, fmt.Errorf(
				"installation resolution: registration %d: %w",
				registrationID,
				ErrNoAppRegistration,
			)
		}
		if len(selected) > 1 {
			return InstallationBinding{}, fmt.Errorf(
				"installation resolution: registration %d is duplicated: %w",
				registrationID,
				ErrAmbiguousAppRegistration,
			)
		}
		apps = selected
	}
	// A candidate set with no covered registration can never produce a usable
	// binding, so refusing it here spares GitHub every App key while the
	// janitor is absent, starting, or stopped. The scoped form's candidate set
	// is one registration, so its gate is unchanged (#291).
	//
	// This is a guard, not an invariant, and no snapshot of coverage could make
	// it one: a pass may begin after the count and before the requests go out,
	// so a listing can always reach GitHub for a registration that is uncovered
	// by the time it arrives. Coverage is empty for the duration of every pass
	// anyway (`janitor.go`'s Run withdraws it before each one) while the
	// janitor itself contacts every registration. The invariant is the token
	// gate below, which is re-read after matching and re-checked at the mint.
	covered := 0
	for _, app := range apps {
		if r.janitor != nil && r.janitor.ActiveFor(app.AppID) {
			covered++
		}
	}
	if covered == 0 {
		registrationIDs := make([]int64, len(apps))
		for i, app := range apps {
			registrationIDs[i] = app.AppID
		}
		return InstallationBinding{}, janitorInactiveError(r.janitor, registrationIDs...)
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

	// Every registration was enumerated and matched above, so the match set is
	// still computed over the complete candidate set and an uncovered
	// registration that does match still denies: a narrowing can never launder
	// an ambiguity into a confident single match. What no longer denies is a
	// registration that produced no match, because it never provides the token
	// the gate exists to prove cleanup for (#291).
	//
	// The floor above returns unless some registration is covered, so a janitor
	// is present by the time any match exists.
	for _, match := range matches {
		if !r.janitor.ActiveFor(match.RegistrationID) {
			return InstallationBinding{}, janitorInactiveError(r.janitor, match.RegistrationID)
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

func janitorInactiveError(status JanitorStatus, registrationIDs ...int64) error {
	if source, ok := status.(resolverJanitorFaultSource); ok {
		requested := make(map[int64]struct{}, len(registrationIDs))
		for _, registrationID := range registrationIDs {
			requested[registrationID] = struct{}{}
		}
		faultByRegistration := make(map[int64]error, len(requested))
		for _, fault := range source.RegistrationFaults() {
			if _, ok := requested[fault.RegistrationID]; !ok || fault.Err == nil {
				continue
			}
			faultByRegistration[fault.RegistrationID] = fault.Err
		}
		// Before repository matching, any unfaulted candidate may become
		// covered when the active janitor pass finishes. Only a known single
		// registration, or a candidate set faulted in full, is definitive.
		if len(faultByRegistration) == len(requested) {
			faults := make([]error, 0, len(requested))
			seen := make(map[int64]struct{}, len(requested))
			for _, registrationID := range registrationIDs {
				if _, ok := seen[registrationID]; ok {
					continue
				}
				seen[registrationID] = struct{}{}
				faults = append(faults, fmt.Errorf(
					"registration %d janitor fault: %w",
					registrationID,
					faultByRegistration[registrationID],
				))
			}
			return fmt.Errorf("installation resolution: %w", errors.Join(faults...))
		}
	}
	return fmt.Errorf(
		"installation resolution: registration %d: %w",
		registrationIDs[0],
		ErrJanitorInactive,
	)
}

func (r *InstallationResolver) allowsRepository(
	registrationID, installationID, repositoryID int64,
) bool {
	return r != nil && r.janitor != nil &&
		r.janitor.AllowsRepository(registrationID, installationID, repositoryID)
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
		pageInstallations, err := installationPage(
			ctx,
			r.client,
			r.baseURL,
			jwt,
			page,
			installationPageSize,
		)
		if err != nil {
			return nil, fmt.Errorf("installation resolution: %w", err)
		}
		all = append(all, pageInstallations...)
		if len(pageInstallations) < installationPageSize {
			return all, nil
		}
	}
	return nil, errors.New("installation resolution: installation pagination exceeded the safety limit")
}

func installationPage(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	jwt Secret,
	page int,
	pageSize int,
) ([]installationResponse, error) {
	path := "/app/installations?per_page=" + strconv.Itoa(pageSize) + "&page=" + strconv.Itoa(page)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt.Reveal())
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		drainAndClose(resp.Body)
		return nil, &APIError{
			Status:      resp.StatusCode,
			RequestPath: "/app/installations",
		}
	}
	var installations []installationResponse
	decodeErr := decodeResponse(resp.Body, &installations)
	drainAndClose(resp.Body)
	if decodeErr != nil || installations == nil {
		return nil, errors.New("decode response")
	}
	if len(installations) > pageSize {
		return nil, errors.New("response page exceeds the requested limit")
	}
	return installations, nil
}
