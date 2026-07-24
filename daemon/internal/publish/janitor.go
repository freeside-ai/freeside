package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// JanitorStatus is the runtime capability gate registration consumers check
// before operating. A registration is active only after the always-on janitor
// has completed a successful, exhaustive pass for it.
type JanitorStatus interface {
	ActiveFor(registrationID int64) bool
	AllowsRepository(registrationID, installationID, repositoryID int64) bool
}

// InstallationRemovalReason is the closed audit vocabulary for janitor
// removals.
type InstallationRemovalReason string

const (
	InstallationRemovalUntrustedOwner InstallationRemovalReason = "untrusted_owner"
	InstallationRemovalUnbound        InstallationRemovalReason = "unbound_installation"
	InstallationRemovalIdentityDrift  InstallationRemovalReason = "installation_identity_drift"
	InstallationRemovalSelectionDrift InstallationRemovalReason = "repository_selection_drift"
	InstallationRemovalGrantDrift     InstallationRemovalReason = "repository_grant_drift"
)

// AllInstallationRemovalReasons is the single registration point for valid
// janitor removal reasons.
var AllInstallationRemovalReasons = []InstallationRemovalReason{
	InstallationRemovalUntrustedOwner,
	InstallationRemovalUnbound,
	InstallationRemovalIdentityDrift,
	InstallationRemovalSelectionDrift,
	InstallationRemovalGrantDrift,
}

func (r InstallationRemovalReason) valid() bool {
	switch r {
	case InstallationRemovalUntrustedOwner,
		InstallationRemovalUnbound,
		InstallationRemovalIdentityDrift,
		InstallationRemovalSelectionDrift,
		InstallationRemovalGrantDrift:
		return true
	default:
		return false
	}
}

// InstallationRemovalRecord is the durable pre-effect audit record for an
// uninstall request. It intentionally omits the returned account login, which
// is untrusted text and may contain credential-shaped content.
type InstallationRemovalRecord struct {
	RequestedAt    time.Time
	RegistrationID int64
	InstallationID int64
	AccountID      int64
	Reason         InstallationRemovalReason
	// ObservedRepositoryIDs is populated only after complete, validated grant
	// enumeration. Selection drift and metadata-only removals leave it nil.
	ObservedRepositoryIDs []int64
}

// JanitorRecorder commits an audit barrier before a destructive request.
// RecordInstallationQuarantine additionally invalidates every trusted binding
// and pending envelope for the installation in the same durable transaction.
// That local fail-closed step happens before suspension or deletion.
type JanitorRecorder interface {
	RecordInstallationRemoval(InstallationRemovalRecord) error
	RecordInstallationQuarantine(InstallationRemovalRecord) error
}

// JanitorCycle reports bounded reconciliation work. RemovalLimitReached means
// the cycle stopped after MaxRemovals removals and deliberately did not claim
// complete coverage for the interrupted registration.
type JanitorCycle struct {
	Examined            int
	Removed             int
	RemovalLimitReached bool
}

// InstallationJanitor reconciles every App registration against canonical
// installation-to-repository bindings and the current pending envelope. It has
// no signet or AttentionItem dependency: unsolicited GitHub state can be
// removed and audited, but cannot create a human-decision path.
type InstallationJanitor struct {
	keystore    *Keystore
	client      *http.Client
	baseURL     string
	authority   InstallationAuthoritySource
	recorder    JanitorRecorder
	now         func() time.Time
	maxRemovals int

	cycleMu sync.Mutex
	mu      sync.RWMutex
	running bool
	covered map[int64]registrationCoverage
}

// NewInstallationJanitor constructs a janitor with a hard per-cycle removal
// bound. Enumeration is independently capped by installationMaxPages.
func NewInstallationJanitor(
	ks *Keystore,
	client *http.Client,
	baseURL string,
	authority InstallationAuthoritySource,
	recorder JanitorRecorder,
	now func() time.Time,
	maxRemovals int,
) (*InstallationJanitor, error) {
	if ks == nil || client == nil || authority == nil || recorder == nil || now == nil {
		return nil, errors.New("installation janitor: nil dependency")
	}
	if baseURL == "" {
		return nil, errors.New("installation janitor: empty API base URL")
	}
	if maxRemovals <= 0 {
		return nil, errors.New("installation janitor: removal bound must be positive")
	}
	return &InstallationJanitor{
		keystore:    ks,
		client:      noRedirect(client),
		baseURL:     baseURL,
		authority:   authority,
		recorder:    recorder,
		now:         now,
		maxRemovals: maxRemovals,
		covered:     map[int64]registrationCoverage{},
	}, nil
}

// ActiveFor reports whether the current always-on loop has completed a
// successful exhaustive pass for registrationID. A one-off RunCycle is useful
// for tests and operator diagnostics, but deliberately does not activate the
// runtime gate.
func (j *InstallationJanitor) ActiveFor(registrationID int64) bool {
	if j == nil || registrationID <= 0 {
		return false
	}
	j.mu.RLock()
	defer j.mu.RUnlock()
	_, ok := j.covered[registrationID]
	return ok
}

// AllowsRepository reports whether the latest complete pass matched a trusted
// binding for the exact registration, installation, and repository. Pending
// envelopes never enter this allow-set.
func (j *InstallationJanitor) AllowsRepository(registrationID, installationID, repositoryID int64) bool {
	if j == nil || registrationID <= 0 || installationID <= 0 || repositoryID <= 0 {
		return false
	}
	j.mu.RLock()
	defer j.mu.RUnlock()
	coverage, ok := j.covered[registrationID]
	if !ok {
		return false
	}
	repositories, ok := coverage.repositories[installationID]
	if !ok {
		return false
	}
	_, ok = repositories[repositoryID]
	return ok
}

// Run keeps reconciliation active until ctx is canceled. Coverage is published
// only after each successful cycle and cleared before Run returns; an API,
// authority-source, or audit failure therefore closes registration operation
// immediately.
func (j *InstallationJanitor) Run(ctx context.Context, interval time.Duration) error {
	if j == nil {
		return errors.New("installation janitor: nil janitor")
	}
	if interval <= 0 {
		return errors.New("installation janitor: interval must be positive")
	}
	if !j.beginRun() {
		return errors.New("installation janitor: already running")
	}
	defer j.finishRun()

	for {
		// A pass that stalls or fails must not leave the previous pass's
		// coverage looking current.
		j.setCovered(nil)
		_, covered, err := j.runCycle(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		j.setCovered(covered)

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

// RunCycle performs one bounded pass without activating the runtime gate.
func (j *InstallationJanitor) RunCycle(ctx context.Context) (JanitorCycle, error) {
	if j == nil {
		return JanitorCycle{}, errors.New("installation janitor: nil janitor")
	}
	j.mu.RLock()
	running := j.running
	j.mu.RUnlock()
	if running {
		return JanitorCycle{}, errors.New("installation janitor: always-on loop is already running")
	}
	cycle, _, err := j.runCycle(ctx)
	return cycle, err
}

func (j *InstallationJanitor) runCycle(ctx context.Context) (JanitorCycle, []registrationCoverage, error) {
	if j == nil || j.keystore == nil || j.client == nil || j.authority == nil ||
		j.recorder == nil || j.now == nil || j.maxRemovals <= 0 {
		return JanitorCycle{}, nil, errors.New("installation janitor: nil or invalid dependency")
	}
	j.cycleMu.Lock()
	defer j.cycleMu.Unlock()

	apps, err := j.keystore.ListApps()
	if err != nil {
		return JanitorCycle{}, nil, fmt.Errorf("installation janitor: %w", err)
	}

	var cycle JanitorCycle
	var covered []registrationCoverage
	for _, app := range apps {
		snapshot, err := j.authority.InstallationAuthority(ctx, app.AppID)
		if err != nil {
			return cycle, covered, fmt.Errorf(
				"installation janitor: registration %d authority: %w",
				app.AppID,
				err,
			)
		}
		authority, err := validateInstallationAuthority(app, snapshot, j.now())
		if err != nil {
			return cycle, covered, fmt.Errorf(
				"installation janitor: registration %d authority: %w",
				app.AppID,
				err,
			)
		}
		complete, registrationCoverage, err := j.reconcileRegistration(ctx, app, authority, &cycle)
		if err != nil {
			return cycle, covered, err
		}
		if !complete {
			return cycle, covered, nil
		}
		covered = append(covered, registrationCoverage)
	}
	return cycle, covered, nil
}

type registrationCoverage struct {
	registrationID int64
	repositories   map[int64]map[int64]struct{}
}

func (j *InstallationJanitor) reconcileRegistration(
	ctx context.Context,
	app AppCredentials,
	authority validatedInstallationAuthority,
	cycle *JanitorCycle,
) (bool, registrationCoverage, error) {
	jwt, err := AppJWT(app.Key, app.AppID, j.now())
	if err != nil {
		return false, registrationCoverage{}, fmt.Errorf("installation janitor: registration %d: %w", app.AppID, err)
	}
	seen := map[int64]struct{}{}
	seenPending := false
	var actions []installationRemoval
	coverage := registrationCoverage{
		registrationID: app.AppID,
		repositories:   make(map[int64]map[int64]struct{}),
	}
	for page := 1; page <= installationMaxPages; page++ {
		installations, err := installationPage(
			ctx,
			j.client,
			j.baseURL,
			jwt,
			page,
			installationPageSize,
		)
		if err != nil {
			return false, registrationCoverage{}, fmt.Errorf("installation janitor: registration %d: %w", app.AppID, err)
		}
		for _, installation := range installations {
			if installation.ID <= 0 || installation.AppID != app.AppID ||
				installation.Account.ID <= 0 || installation.TargetID != installation.Account.ID ||
				validateOwnerLogin(installation.Account.Login) != nil {
				return false, registrationCoverage{}, fmt.Errorf(
					"installation janitor: registration %d: %w",
					app.AppID,
					ErrInstallationResolution,
				)
			}
			if _, duplicate := seen[installation.ID]; duplicate {
				return false, registrationCoverage{}, fmt.Errorf(
					"installation janitor: registration %d: duplicate installation identity: %w",
					app.AppID,
					ErrInstallationResolution,
				)
			}
			seen[installation.ID] = struct{}{}
			cycle.Examined++

			candidate, ok, identityDrift := authority.candidate(installation)
			if identityDrift {
				actions = append(actions, installationRemoval{
					installationID: installation.ID,
					accountID:      installation.Account.ID,
					reason:         InstallationRemovalIdentityDrift,
					quarantine:     true,
				})
				continue
			}
			if !ok {
				reason := InstallationRemovalUnbound
				_, trustedOwner := authority.trustedOwners[trustedOwnerKey{
					login: strings.ToLower(installation.Account.Login),
					id:    installation.Account.ID,
				}]
				if app.Visibility == AppVisibilityPrivate &&
					strings.EqualFold(installation.Account.Login, app.Owner) &&
					installation.Account.ID == app.OwnerID {
					trustedOwner = true
				}
				if !trustedOwner {
					reason = InstallationRemovalUntrustedOwner
				}
				actions = append(actions, installationRemoval{
					installationID: installation.ID,
					accountID:      installation.Account.ID,
					reason:         reason,
				})
				continue
			}
			if candidate.pending {
				if seenPending {
					return false, registrationCoverage{}, errors.New(
						"installation janitor: pending envelope matched multiple installations",
					)
				}
				seenPending = true
			}
			if installation.RepositorySelection != "selected" {
				actions = append(actions, installationRemoval{
					installationID: installation.ID,
					accountID:      installation.Account.ID,
					reason:         InstallationRemovalSelectionDrift,
					quarantine:     true,
				})
				continue
			}
			repositoryIDs, err := j.enumerateRepositoryGrants(ctx, jwt, installation.ID)
			if err != nil {
				return false, registrationCoverage{}, fmt.Errorf(
					"installation janitor: registration %d installation %d grants: %w",
					app.AppID,
					installation.ID,
					err,
				)
			}
			matchesExpected := slices.Equal(repositoryIDs, candidate.repositoryIDs)
			matchesPendingBase := candidate.pending &&
				len(candidate.allowedRepositoryIDs) > 0 &&
				slices.Equal(repositoryIDs, candidate.allowedRepositoryIDs)
			if !matchesExpected && !matchesPendingBase {
				actions = append(actions, installationRemoval{
					installationID:        installation.ID,
					accountID:             installation.Account.ID,
					reason:                InstallationRemovalGrantDrift,
					observedRepositoryIDs: repositoryIDs,
					quarantine:            true,
				})
				continue
			}
			if len(candidate.allowedRepositoryIDs) > 0 {
				repositories := make(map[int64]struct{}, len(candidate.allowedRepositoryIDs))
				for _, repositoryID := range candidate.allowedRepositoryIDs {
					repositories[repositoryID] = struct{}{}
				}
				coverage.repositories[installation.ID] = repositories
			}
		}
		if len(installations) < installationPageSize {
			break
		}
		if page == installationMaxPages {
			return false, registrationCoverage{}, errors.New("installation janitor: installation pagination exceeded the safety limit")
		}
	}

	for installationID := range authority.trusted {
		if _, ok := seen[installationID]; !ok {
			return false, registrationCoverage{}, fmt.Errorf(
				"installation janitor: registration %d trusted installation %d is absent",
				app.AppID,
				installationID,
			)
		}
	}

	if len(actions) == 0 {
		return true, coverage, nil
	}
	for _, installation := range actions {
		if cycle.Removed >= j.maxRemovals {
			cycle.RemovalLimitReached = true
			return false, registrationCoverage{}, nil
		}
		record := InstallationRemovalRecord{
			RequestedAt:    j.now().UTC(),
			RegistrationID: app.AppID,
			InstallationID: installation.installationID,
			AccountID:      installation.accountID,
			Reason:         installation.reason,
			ObservedRepositoryIDs: append(
				[]int64(nil),
				installation.observedRepositoryIDs...,
			),
		}
		if !record.Reason.valid() {
			return false, registrationCoverage{}, fmt.Errorf("installation janitor: registration %d has invalid removal reason", app.AppID)
		}
		var recordErr error
		if installation.quarantine {
			recordErr = j.recorder.RecordInstallationQuarantine(record)
		} else {
			recordErr = j.recorder.RecordInstallationRemoval(record)
		}
		if recordErr != nil {
			return false, registrationCoverage{}, fmt.Errorf(
				"installation janitor: registration %d audit removal: %w",
				app.AppID,
				recordErr,
			)
		}
		if installation.quarantine {
			if err := j.suspendInstallation(ctx, jwt, installation.installationID); err != nil {
				return false, registrationCoverage{}, fmt.Errorf("installation janitor: registration %d: %w", app.AppID, err)
			}
		}
		if err := j.deleteInstallation(ctx, jwt, installation.installationID); err != nil {
			return false, registrationCoverage{}, fmt.Errorf("installation janitor: registration %d: %w", app.AppID, err)
		}
		cycle.Removed++
	}

	// The snapshot was exhaustive, but a destructive pass changed the
	// collection it described. Only a later clean pass may publish coverage.
	return false, registrationCoverage{}, nil
}

type installationRemoval struct {
	installationID        int64
	accountID             int64
	reason                InstallationRemovalReason
	observedRepositoryIDs []int64
	quarantine            bool
}

var grantReadPermissionScopes = map[string]string{"metadata": "read"}

type grantReadMintRequest struct {
	Permissions Permissions `json:"permissions"`
}

type grantReadMintResponse struct {
	Token               Secret            `json:"token"`
	Permissions         map[string]string `json:"permissions"`
	RepositorySelection string            `json:"repository_selection"`
}

type installationRepositoriesPage struct {
	TotalCount   int64 `json:"total_count"`
	Repositories []struct {
		ID int64 `json:"id"`
	} `json:"repositories"`
}

func (j *InstallationJanitor) enumerateRepositoryGrants(
	ctx context.Context,
	jwt Secret,
	installationID int64,
) ([]int64, error) {
	token, mintErr := j.mintGrantReadToken(ctx, jwt, installationID)
	if token.Reveal() == "" {
		return nil, mintErr
	}
	var repositoryIDs []int64
	if mintErr == nil {
		repositoryIDs, mintErr = j.listInstallationRepositories(ctx, token)
	}

	revokeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	revokeErr := j.revokeInstallationToken(revokeCtx, token)
	if mintErr != nil {
		if revokeErr != nil {
			return nil, errors.Join(mintErr, revokeErr)
		}
		return nil, mintErr
	}
	if revokeErr != nil {
		return nil, revokeErr
	}
	return repositoryIDs, nil
}

func (j *InstallationJanitor) mintGrantReadToken(
	ctx context.Context,
	jwt Secret,
	installationID int64,
) (Secret, error) {
	body, err := json.Marshal(grantReadMintRequest{
		Permissions: Permissions{Metadata: "read"},
	})
	if err != nil {
		return "", fmt.Errorf("mint grant-read token: encode request: %w", err)
	}
	path := "/app/installations/" + strconv.FormatInt(installationID, 10) + "/access_tokens"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("mint grant-read token: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt.Reveal())
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")
	resp, err := j.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("mint grant-read token: %w", err)
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("mint grant-read token: %w", &APIError{
			Status:      resp.StatusCode,
			RequestPath: "/app/installations/{installation_id}/access_tokens",
		})
	}
	var minted grantReadMintResponse
	if err := decodeResponse(resp.Body, &minted); err != nil {
		return minted.Token, errors.New("mint grant-read token: decode response")
	}
	if minted.Token.Reveal() == "" {
		return "", errors.New("mint grant-read token: response carries no token")
	}
	if !maps.Equal(minted.Permissions, grantReadPermissionScopes) ||
		minted.RepositorySelection != "selected" {
		return minted.Token, fmt.Errorf("mint grant-read token: returned grant differs from request: %w", ErrGrantMismatch)
	}
	return minted.Token, nil
}

func (j *InstallationJanitor) listInstallationRepositories(
	ctx context.Context,
	token Secret,
) ([]int64, error) {
	seen := make(map[int64]struct{})
	var (
		repositoryIDs []int64
		totalCount    int64 = -1
	)
	for page := 1; page <= installationMaxPages; page++ {
		path := "/installation/repositories?per_page=" +
			strconv.Itoa(installationPageSize) + "&page=" + strconv.Itoa(page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.baseURL+path, nil)
		if err != nil {
			return nil, fmt.Errorf("list installation repositories: build request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token.Reveal())
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		resp, err := j.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("list installation repositories: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			drainAndClose(resp.Body)
			return nil, fmt.Errorf("list installation repositories: %w", &APIError{
				Status:      resp.StatusCode,
				RequestPath: "/installation/repositories",
			})
		}
		var response installationRepositoriesPage
		decodeErr := decodeResponse(resp.Body, &response)
		drainAndClose(resp.Body)
		if decodeErr != nil || response.Repositories == nil || response.TotalCount < 0 ||
			len(response.Repositories) > installationPageSize {
			return nil, errors.New("list installation repositories: invalid response page")
		}
		if totalCount < 0 {
			totalCount = response.TotalCount
			if totalCount > int64(installationPageSize*installationMaxPages) {
				return nil, errors.New("list installation repositories: repository count exceeds the safety limit")
			}
		} else if response.TotalCount != totalCount {
			return nil, errors.New("list installation repositories: total count changed during pagination")
		}
		for _, repository := range response.Repositories {
			if repository.ID <= 0 {
				return nil, errors.New("list installation repositories: non-positive repository ID")
			}
			if _, duplicate := seen[repository.ID]; duplicate {
				return nil, errors.New("list installation repositories: duplicate repository ID")
			}
			seen[repository.ID] = struct{}{}
			repositoryIDs = append(repositoryIDs, repository.ID)
		}
		switch {
		case int64(len(repositoryIDs)) == totalCount:
			slices.Sort(repositoryIDs)
			return repositoryIDs, nil
		case int64(len(repositoryIDs)) > totalCount:
			return nil, errors.New("list installation repositories: page exceeds total count")
		case len(response.Repositories) < installationPageSize:
			return nil, errors.New("list installation repositories: pagination ended before total count")
		}
	}
	return nil, errors.New("list installation repositories: pagination exceeded the safety limit")
}

func (j *InstallationJanitor) revokeInstallationToken(ctx context.Context, token Secret) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, j.baseURL+"/installation/token", nil)
	if err != nil {
		return fmt.Errorf("revoke installation token: build request: %w", err)
	}
	req.Header.Set("Authorization", "token "+token.Reveal())
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := j.client.Do(req)
	if err != nil {
		return fmt.Errorf("revoke installation token: %w", err)
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("revoke installation token: %w", &APIError{
			Status:      resp.StatusCode,
			RequestPath: "/installation/token",
		})
	}
	return nil
}

func (j *InstallationJanitor) suspendInstallation(ctx context.Context, jwt Secret, installationID int64) error {
	path := "/app/installations/" + strconv.FormatInt(installationID, 10) + "/suspended"
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, j.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("suspend installation: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt.Reveal())
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := j.client.Do(req)
	if err != nil {
		return fmt.Errorf("suspend installation: %w", err)
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("suspend installation: %w", &APIError{
			Status:      resp.StatusCode,
			RequestPath: "/app/installations/{installation_id}/suspended",
		})
	}
	return nil
}

func (j *InstallationJanitor) deleteInstallation(ctx context.Context, jwt Secret, installationID int64) error {
	path := "/app/installations/" + strconv.FormatInt(installationID, 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, j.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("delete installation: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwt.Reveal())
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := j.client.Do(req)
	if err != nil {
		return fmt.Errorf("delete installation: %w", err)
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("delete installation: %w", &APIError{
			Status:      resp.StatusCode,
			RequestPath: "/app/installations/{installation_id}",
		})
	}
	return nil
}

func (j *InstallationJanitor) setCovered(registrations []registrationCoverage) {
	covered := make(map[int64]registrationCoverage, len(registrations))
	for _, registration := range registrations {
		covered[registration.registrationID] = registration
	}
	j.mu.Lock()
	j.covered = covered
	j.mu.Unlock()
}

func (j *InstallationJanitor) beginRun() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.running {
		return false
	}
	j.running = true
	return true
}

func (j *InstallationJanitor) finishRun() {
	j.mu.Lock()
	j.running = false
	j.covered = map[int64]registrationCoverage{}
	j.mu.Unlock()
}
