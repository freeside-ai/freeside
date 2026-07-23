package publish

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// JanitorStatus is the runtime capability gate public-registration consumers
// check before operating. A registration is active only after the always-on
// janitor has completed a successful, exhaustive pass for it.
type JanitorStatus interface {
	ActiveFor(registrationID int64) bool
}

// TrustedOwner is one owner account the operator has approved for a public
// registration. Login and ID are both required: neither a renamed account nor
// a reused login may inherit the other coordinate's authority.
type TrustedOwner struct {
	Login string
	ID    int64
}

// TrustedOwnerSource supplies the operator-approved owner set for one stable
// numeric App registration.
type TrustedOwnerSource interface {
	TrustedOwners(ctx context.Context, registrationID int64) ([]TrustedOwner, error)
}

// InstallationRemovalReason is the closed audit vocabulary for janitor
// removals.
type InstallationRemovalReason string

const (
	InstallationRemovalUntrustedOwner InstallationRemovalReason = "untrusted_owner"
)

// AllInstallationRemovalReasons is the single registration point for valid
// janitor removal reasons.
var AllInstallationRemovalReasons = []InstallationRemovalReason{
	InstallationRemovalUntrustedOwner,
}

func (r InstallationRemovalReason) valid() bool {
	switch r {
	case InstallationRemovalUntrustedOwner:
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
}

// JanitorRecorder commits the audit barrier before an uninstall request is
// issued. If recording fails, the destructive request is not sent.
type JanitorRecorder interface {
	RecordInstallationRemoval(InstallationRemovalRecord) error
}

// JanitorCycle reports bounded reconciliation work. RemovalLimitReached means
// the cycle stopped after MaxRemovals removals and deliberately did not claim
// complete coverage for the interrupted registration.
type JanitorCycle struct {
	Examined            int
	Removed             int
	RemovalLimitReached bool
}

// InstallationJanitor reconciles every public App registration against the
// operator-approved owner set. It has no signet or AttentionItem dependency:
// unsolicited GitHub state can be removed and audited, but cannot create a
// human-decision path.
type InstallationJanitor struct {
	keystore    *Keystore
	client      *http.Client
	baseURL     string
	owners      TrustedOwnerSource
	recorder    JanitorRecorder
	now         func() time.Time
	maxRemovals int

	cycleMu sync.Mutex
	mu      sync.RWMutex
	running bool
	covered map[int64]struct{}
}

// NewInstallationJanitor constructs a janitor with a hard per-cycle removal
// bound. Enumeration is independently capped by installationMaxPages.
func NewInstallationJanitor(
	ks *Keystore,
	client *http.Client,
	baseURL string,
	owners TrustedOwnerSource,
	recorder JanitorRecorder,
	now func() time.Time,
	maxRemovals int,
) (*InstallationJanitor, error) {
	if ks == nil || client == nil || owners == nil || recorder == nil || now == nil {
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
		owners:      owners,
		recorder:    recorder,
		now:         now,
		maxRemovals: maxRemovals,
		covered:     map[int64]struct{}{},
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

// Run keeps reconciliation active until ctx is canceled. Coverage is published
// only after each successful cycle and cleared before Run returns; an API,
// trust-source, or audit failure therefore closes public-registration
// operation immediately.
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

func (j *InstallationJanitor) runCycle(ctx context.Context) (JanitorCycle, []int64, error) {
	if j == nil || j.keystore == nil || j.client == nil || j.owners == nil ||
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
	var covered []int64
	for _, app := range apps {
		if app.Visibility != AppVisibilityPublic {
			continue
		}
		trusted, err := j.trustedOwnerSet(ctx, app)
		if err != nil {
			return cycle, covered, err
		}
		complete, err := j.reconcileRegistration(ctx, app, trusted, &cycle)
		if err != nil {
			return cycle, covered, err
		}
		if !complete {
			return cycle, covered, nil
		}
		covered = append(covered, app.AppID)
	}
	return cycle, covered, nil
}

type trustedOwnerKey struct {
	login string
	id    int64
}

func (j *InstallationJanitor) trustedOwnerSet(
	ctx context.Context,
	app AppCredentials,
) (map[trustedOwnerKey]struct{}, error) {
	owners, err := j.owners.TrustedOwners(ctx, app.AppID)
	if err != nil {
		return nil, fmt.Errorf("installation janitor: registration %d trusted owners: %w", app.AppID, err)
	}
	trusted := make(map[trustedOwnerKey]struct{}, len(owners))
	seenIDs := make(map[int64]string, len(owners))
	for _, owner := range owners {
		if owner.ID <= 0 || validateOwnerLogin(owner.Login) != nil {
			return nil, fmt.Errorf("installation janitor: registration %d has invalid trusted owner", app.AppID)
		}
		login := strings.ToLower(owner.Login)
		if prior, ok := seenIDs[owner.ID]; ok && prior != login {
			return nil, fmt.Errorf("installation janitor: registration %d has conflicting trusted owner identities", app.AppID)
		}
		seenIDs[owner.ID] = login
		trusted[trustedOwnerKey{login: login, id: owner.ID}] = struct{}{}
	}
	registrationOwner := trustedOwnerKey{login: strings.ToLower(app.Owner), id: app.OwnerID}
	if _, ok := trusted[registrationOwner]; !ok {
		return nil, fmt.Errorf(
			"installation janitor: registration %d owner is absent from the trusted owner set",
			app.AppID,
		)
	}
	return trusted, nil
}

func (j *InstallationJanitor) reconcileRegistration(
	ctx context.Context,
	app AppCredentials,
	trusted map[trustedOwnerKey]struct{},
	cycle *JanitorCycle,
) (bool, error) {
	jwt, err := AppJWT(app.Key, app.AppID, j.now())
	if err != nil {
		return false, fmt.Errorf("installation janitor: registration %d: %w", app.AppID, err)
	}
	seen := map[int64]struct{}{}
	var unknown []installationRemoval
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
			return false, fmt.Errorf("installation janitor: registration %d: %w", app.AppID, err)
		}
		for _, installation := range installations {
			if installation.ID <= 0 || installation.AppID != app.AppID ||
				installation.Account.ID <= 0 || installation.TargetID != installation.Account.ID ||
				validateOwnerLogin(installation.Account.Login) != nil {
				return false, fmt.Errorf(
					"installation janitor: registration %d: %w",
					app.AppID,
					ErrInstallationResolution,
				)
			}
			if _, duplicate := seen[installation.ID]; duplicate {
				return false, fmt.Errorf(
					"installation janitor: registration %d: duplicate installation identity: %w",
					app.AppID,
					ErrInstallationResolution,
				)
			}
			seen[installation.ID] = struct{}{}
			cycle.Examined++

			key := trustedOwnerKey{
				login: strings.ToLower(installation.Account.Login),
				id:    installation.Account.ID,
			}
			if _, ok := trusted[key]; ok {
				continue
			}
			unknown = append(unknown, installationRemoval{
				installationID: installation.ID,
				accountID:      installation.Account.ID,
			})
		}
		if len(installations) < installationPageSize {
			break
		}
		if page == installationMaxPages {
			return false, errors.New("installation janitor: installation pagination exceeded the safety limit")
		}
	}

	if len(unknown) == 0 {
		return true, nil
	}
	for _, installation := range unknown {
		if cycle.Removed >= j.maxRemovals {
			cycle.RemovalLimitReached = true
			return false, nil
		}
		record := InstallationRemovalRecord{
			RequestedAt:    j.now().UTC(),
			RegistrationID: app.AppID,
			InstallationID: installation.installationID,
			AccountID:      installation.accountID,
			Reason:         InstallationRemovalUntrustedOwner,
		}
		if !record.Reason.valid() {
			return false, fmt.Errorf("installation janitor: registration %d has invalid removal reason", app.AppID)
		}
		if err := j.recorder.RecordInstallationRemoval(record); err != nil {
			return false, fmt.Errorf(
				"installation janitor: registration %d audit removal: %w",
				app.AppID,
				err,
			)
		}
		if err := j.deleteInstallation(ctx, jwt, installation.installationID); err != nil {
			return false, fmt.Errorf("installation janitor: registration %d: %w", app.AppID, err)
		}
		cycle.Removed++
	}

	// The snapshot was exhaustive, but a destructive pass changed the
	// collection it described. Only a later clean pass may publish coverage.
	return false, nil
}

type installationRemoval struct {
	installationID int64
	accountID      int64
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

func (j *InstallationJanitor) setCovered(registrationIDs []int64) {
	covered := make(map[int64]struct{}, len(registrationIDs))
	for _, registrationID := range registrationIDs {
		covered[registrationID] = struct{}{}
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
	j.covered = map[int64]struct{}{}
	j.mu.Unlock()
}
