package publish

import (
	"bytes"
	"cmp"
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
//
// Removed counts completed removals, so a pass that fails partway can report
// fewer than it attempted; attempted is what the bound is spent against, since
// a destructive request that fails is still a destructive request.
type JanitorCycle struct {
	Examined            int
	Removed             int
	RemovalLimitReached bool

	attempted int
}

// errJanitorUnsafe marks a failure of the daemon's own safety machinery rather
// than of one registration's remote or authored state: the durable audit
// barrier, or a credential the janitor minted that it cannot account for,
// whether revocation failed or the mint's outcome is simply unknown. Neither
// belongs to the registration that happened to reach it first, and continuing
// the pass would keep acting destructively without a barrier, or keep minting
// credentials the daemon cannot take back. Such a failure stops the pass, as
// every failure did before #281.
var errJanitorUnsafe = errors.New("installation janitor: unsafe to continue")

// JanitorRegistrationFault names one registration the latest pass could not
// complete, with the error that denied it. A faulted registration is absent
// from coverage, so its runtime gate is already shut; the fault is what tells
// an operator why, instead of leaving an unexplained denial behind.
//
// Err is the janitor's own wrapped error. It may quote a remote or
// operator-authored value, so it belongs in an operator diagnostic, not in a
// durable audit record: InstallationRemovalRecord and CredentialFinding carry
// safe coordinates by design.
type JanitorRegistrationFault struct {
	RegistrationID int64
	Err            error
}

// JanitorRegistrationChurn identifies a registration whose latest completed
// pass removed installations without reaching a clean pass. ConsecutivePasses
// counts how many completed passes in a row have done so, distinguishing
// repeated removal churn from a registration that has not been visited yet.
type JanitorRegistrationChurn struct {
	RegistrationID    int64
	ConsecutivePasses int
}

// InstallationJanitor reconciles every App registration against canonical
// installation-to-repository bindings and the current pending envelope. It has
// no signet or AttentionItem dependency: unsolicited GitHub state can be
// removed and audited, but cannot create a human-decision path.
type InstallationJanitor struct {
	keystore     *Keystore
	client       *http.Client
	baseURL      string
	authority    InstallationAuthoritySource
	recorder     JanitorRecorder
	mintRecorder InstallationMintRecorder
	now          func() time.Time
	maxRemovals  int

	cycleMu sync.Mutex
	mu      sync.RWMutex
	running bool
	covered map[int64]registrationCoverage
	faults  []JanitorRegistrationFault
	churn   []JanitorRegistrationChurn
}

// NewInstallationJanitor constructs a janitor with a hard per-cycle removal
// bound. Enumeration is independently capped by installationMaxPages.
//
// It takes two audit ports because it records two different events to two
// durable surfaces: recorder commits the destructive-action barrier (the file
// journal), and mintRecorder commits each grant-read mint (the SQLite audit
// ledger, via the store-backed recorder).
func NewInstallationJanitor(
	ks *Keystore,
	client *http.Client,
	baseURL string,
	authority InstallationAuthoritySource,
	recorder JanitorRecorder,
	mintRecorder InstallationMintRecorder,
	now func() time.Time,
	maxRemovals int,
) (*InstallationJanitor, error) {
	if ks == nil || client == nil || authority == nil || recorder == nil ||
		mintRecorder == nil || now == nil {
		return nil, errors.New("installation janitor: nil dependency")
	}
	if baseURL == "" {
		return nil, errors.New("installation janitor: empty API base URL")
	}
	if maxRemovals <= 0 {
		return nil, errors.New("installation janitor: removal bound must be positive")
	}
	return &InstallationJanitor{
		keystore:     ks,
		client:       noRedirect(client),
		baseURL:      baseURL,
		authority:    authority,
		recorder:     recorder,
		mintRecorder: mintRecorder,
		now:          now,
		maxRemovals:  maxRemovals,
		covered:      map[int64]registrationCoverage{},
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

// AwaitAllowsRepository returns the latest completed pass's exact trusted
// grant observation. If a pass is in progress, it waits for that pass to
// publish or withdraw coverage rather than exposing the deliberate transient
// withdrawal between those two events. Onboarding uses this coordinated view;
// ordinary runtime gates retain the immediate fail-closed view above.
func (j *InstallationJanitor) AwaitAllowsRepository(
	registrationID, installationID, repositoryID int64,
) bool {
	if j == nil {
		return false
	}
	j.cycleMu.Lock()
	defer j.cycleMu.Unlock()
	return j.AllowsRepository(registrationID, installationID, repositoryID)
}

// WithStableCoverage runs fn while no reconciliation pass can withdraw or
// replace the latest complete coverage snapshot. It is reserved for bounded
// operational probes, such as scheduled conformance, whose own authenticated
// reads must not fail merely because the janitor starts a concurrent pass.
// Ordinary runtime gates continue to use the immediate fail-closed view.
func (j *InstallationJanitor) WithStableCoverage(fn func() error) error {
	if j == nil || fn == nil {
		return errors.New("installation janitor: nil stable-coverage dependency")
	}
	j.cycleMu.Lock()
	defer j.cycleMu.Unlock()
	return fn()
}

// PendingReady reports whether the latest complete pass observed the exact
// selected-repository grant set named by the current pending envelope. It is
// an onboarding transition signal, never runtime authority: pending
// repositories remain absent from AllowsRepository until the operator-authored
// document is promoted and a later pass covers the trusted binding.
func (j *InstallationJanitor) PendingReady(
	envelope PendingInstallationEnvelope,
) (int64, bool) {
	if j == nil || envelope.RegistrationID <= 0 || envelope.InstallationID < 0 ||
		envelope.ActiveEpoch <= 0 || envelope.DurableIntentRevision <= 0 ||
		len(envelope.ExpectedRepositoryIDs) == 0 {
		return 0, false
	}
	j.mu.RLock()
	defer j.mu.RUnlock()
	coverage, ok := j.covered[envelope.RegistrationID]
	if !ok || coverage.pendingReady == nil ||
		(envelope.InstallationID > 0 &&
			coverage.pendingReady.installationID != envelope.InstallationID) ||
		coverage.pendingReady.activeEpoch != envelope.ActiveEpoch ||
		coverage.pendingReady.durableIntentRevision != envelope.DurableIntentRevision ||
		len(coverage.pendingReady.repositories) != len(envelope.ExpectedRepositoryIDs) {
		return 0, false
	}
	for _, repositoryID := range envelope.ExpectedRepositoryIDs {
		if _, ok := coverage.pendingReady.repositories[repositoryID]; !ok {
			return 0, false
		}
	}
	return coverage.pendingReady.installationID, true
}

// AwaitPendingReady is the pending-envelope counterpart to
// AwaitAllowsRepository: it linearizes the onboarding transition gate after
// any reconciliation pass already in progress.
func (j *InstallationJanitor) AwaitPendingReady(
	envelope PendingInstallationEnvelope,
) (int64, bool) {
	if j == nil {
		return 0, false
	}
	j.cycleMu.Lock()
	defer j.cycleMu.Unlock()
	return j.PendingReady(envelope)
}

// RegistrationFaults reports the registrations the most recently completed
// pass could not complete, ordered by registration ID. Faults outlive the gate
// they explain: they stay published while the next pass runs, and are replaced
// only when that pass finishes. They are empty before the first pass and after
// Run returns. A one-off RunCycle does not publish here; it returns its faults
// as an error.
func (j *InstallationJanitor) RegistrationFaults() []JanitorRegistrationFault {
	if j == nil {
		return nil
	}
	j.mu.RLock()
	defer j.mu.RUnlock()
	return slices.Clone(j.faults)
}

// ChurningRegistrations reports registrations whose most recently completed
// pass removed installations without reaching a clean pass, ordered by
// registration ID. Like faults, churn stays published while the next pass runs
// and is cleared after a clean or failed pass and when Run returns.
func (j *InstallationJanitor) ChurningRegistrations() []JanitorRegistrationChurn {
	if j == nil {
		return nil
	}
	j.mu.RLock()
	defer j.mu.RUnlock()
	return slices.Clone(j.churn)
}

// Run keeps reconciliation active until ctx is canceled. Coverage is published
// only after each cycle and cleared before Run returns, so a registration is
// active only while its own latest pass covered it.
//
// A failure attributable to one registration denies that registration and is
// reported by RegistrationFaults; the loop keeps running and the remaining
// registrations keep their coverage. Only a failure of the whole pass stops
// the loop, because a stopped loop denies every registration until a human
// restarts the daemon, and an authority source may legitimately have no entry
// for a registration onboarding has just written (#281).
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
		// coverage looking current. Its diagnostics stay published: they are
		// not grants, and clearing them would leave a persistently failing or
		// churning registration reporting as merely unvisited for as long as
		// each pass takes.
		j.cycleMu.Lock()
		j.withdrawCoverage()
		_, pass, err := j.runCycle(ctx)
		if err != nil {
			j.cycleMu.Unlock()
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		j.publishPass(pass)
		j.cycleMu.Unlock()

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

// RunScheduledPass performs one reconciliation pass with the always-on
// loop's full coverage lifecycle — withdraw, reconcile, publish, under the
// cycle mutex — for a caller that owns the cadence (the §5.16 durable
// scheduler's janitor kind). The janitor keeps ownership of what a pass
// means and what coverage it publishes; only the ticker moved out. Coverage
// persists between scheduled passes exactly as it does between always-on
// loop iterations, and a pass-wide failure is returned for the caller to
// treat as fatally as the loop's own exit. It refuses to run while the
// always-on loop is active, which would double-drive the lifecycle.
func (j *InstallationJanitor) RunScheduledPass(ctx context.Context) error {
	if j == nil {
		return errors.New("installation janitor: nil janitor")
	}
	j.cycleMu.Lock()
	defer j.cycleMu.Unlock()
	j.mu.RLock()
	running := j.running
	j.mu.RUnlock()
	if running {
		return errors.New("installation janitor: always-on loop is already running")
	}
	j.withdrawCoverage()
	_, pass, err := j.runCycle(ctx)
	if err != nil {
		return err
	}
	j.publishPass(pass)
	return nil
}

// RunCycle performs one bounded pass without activating the runtime gate or
// publishing diagnostics. A per-registration failure is an error here rather
// than a recorded fault: the one-off form exists for operator diagnostics,
// which have no later pass to report through.
func (j *InstallationJanitor) RunCycle(ctx context.Context) (JanitorCycle, error) {
	if j == nil {
		return JanitorCycle{}, errors.New("installation janitor: nil janitor")
	}
	j.cycleMu.Lock()
	defer j.cycleMu.Unlock()
	j.mu.RLock()
	running := j.running
	j.mu.RUnlock()
	if running {
		return JanitorCycle{}, errors.New("installation janitor: always-on loop is already running")
	}
	cycle, pass, err := j.runCycle(ctx)
	if err != nil {
		return cycle, err
	}
	faulted := make([]error, 0, len(pass.faults))
	for _, fault := range pass.faults {
		faulted = append(faulted, fault.Err)
	}
	return cycle, errors.Join(faulted...)
}

type janitorPass struct {
	registrations []int64
	covered       []registrationCoverage
	faults        []JanitorRegistrationFault
	churn         []int64
	incomplete    []int64
}

// runCycle reconciles every registration in the keystore. A failure it can
// attribute to one registration becomes a fault: that registration is left out
// of coverage, which shuts its gate, and the pass continues.
//
// Three classes are not attributable and are returned as an error, stopping
// the pass: enumerating the keystore, a canceled context, and errJanitorUnsafe
// (the shared audit barrier, or a credential the janitor could not revoke).
func (j *InstallationJanitor) runCycle(
	ctx context.Context,
) (JanitorCycle, janitorPass, error) {
	if j == nil || j.keystore == nil || j.client == nil || j.authority == nil ||
		j.recorder == nil || j.now == nil || j.maxRemovals <= 0 {
		return JanitorCycle{}, janitorPass{}, errors.New("installation janitor: nil or invalid dependency")
	}
	apps, err := j.keystore.ListApps()
	if err != nil {
		return JanitorCycle{}, janitorPass{}, fmt.Errorf("installation janitor: %w", err)
	}

	var cycle JanitorCycle
	pass := janitorPass{registrations: make([]int64, 0, len(apps))}
	requiredRecords := make(map[int64]int, len(apps))
	for _, app := range apps {
		pass.registrations = append(pass.registrations, app.AppID)
		requiredRecords[app.AppID]++
	}
	slices.Sort(pass.registrations)
	pass.registrations = slices.Compact(pass.registrations)
	cleanRecords := make(map[int64]int, len(pass.registrations))
	for _, app := range apps {
		removedBefore := cycle.Removed
		complete, registrationCoverage, err := j.reconcileApp(ctx, app, &cycle)
		if err != nil {
			if errors.Is(err, errJanitorUnsafe) {
				return cycle, pass, err
			}
			// Shutdown is not a registration's fault, and recording it as one
			// would bury the pass's real faults under every remaining app.
			// What failed first need not be the cancellation itself, so the
			// cause rides along with it rather than replacing it.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return cycle, pass, fmt.Errorf("%w: %w", ctxErr, err)
			}
			pass.faults = append(
				pass.faults,
				JanitorRegistrationFault{RegistrationID: app.AppID, Err: err},
			)
			if cycle.RemovalLimitReached {
				break
			}
			continue
		}
		if !complete {
			if cycle.Removed > removedBefore {
				pass.churn = append(pass.churn, app.AppID)
			}
			// The removal bound is the whole pass's budget, so a later
			// registration could not be reconciled within it either.
			if cycle.RemovalLimitReached {
				break
			}
			continue
		}
		pass.covered = append(pass.covered, registrationCoverage)
		cleanRecords[app.AppID]++
	}
	slices.SortFunc(pass.faults, func(a, b JanitorRegistrationFault) int {
		return cmp.Compare(a.RegistrationID, b.RegistrationID)
	})
	slices.Sort(pass.churn)
	pass.churn = slices.Compact(pass.churn)
	if len(pass.faults) > 0 && len(pass.churn) > 0 {
		faulted := make(map[int64]struct{}, len(pass.faults))
		for _, fault := range pass.faults {
			faulted[fault.RegistrationID] = struct{}{}
		}
		pass.churn = slices.DeleteFunc(pass.churn, func(registrationID int64) bool {
			_, ok := faulted[registrationID]
			return ok
		})
	}
	for registrationID, required := range requiredRecords {
		if cleanRecords[registrationID] < required {
			pass.incomplete = append(pass.incomplete, registrationID)
		}
	}
	slices.Sort(pass.incomplete)
	pass.covered = withdrawUnreconciled(
		pass.covered,
		pass.faults,
		pass.churn,
		pass.incomplete,
	)
	return cycle, pass, nil
}

// withdrawUnreconciled drops coverage for any registration ID that faulted,
// removed installations, or whose owner-keyed records did not all finish clean
// in the same pass. Two keystore records can carry one registration ID (which
// is what ErrAmbiguousAppRegistration exists for); without this, a record that
// reconciled would open the gate for an ID whose sibling record was not clean,
// was skipped, or reached drift only after the pass-wide bound was spent.
func withdrawUnreconciled(
	covered []registrationCoverage,
	faults []JanitorRegistrationFault,
	churn []int64,
	incomplete []int64,
) []registrationCoverage {
	if len(faults) == 0 && len(churn) == 0 && len(incomplete) == 0 {
		return covered
	}
	unreconciled := make(map[int64]struct{}, len(faults)+len(churn)+len(incomplete))
	for _, fault := range faults {
		unreconciled[fault.RegistrationID] = struct{}{}
	}
	for _, registrationID := range churn {
		unreconciled[registrationID] = struct{}{}
	}
	for _, registrationID := range incomplete {
		unreconciled[registrationID] = struct{}{}
	}
	return slices.DeleteFunc(covered, func(coverage registrationCoverage) bool {
		_, ok := unreconciled[coverage.registrationID]
		return ok
	})
}

// reconcileApp resolves one registration's authority and reconciles it. Its
// errors are attributable to that registration alone except for
// errJanitorUnsafe, which reconcileRegistration raises for the shared audit
// barrier and for a credential it could not revoke; runCycle separates them.
func (j *InstallationJanitor) reconcileApp(
	ctx context.Context,
	app AppCredentials,
	cycle *JanitorCycle,
) (bool, registrationCoverage, error) {
	snapshot, err := j.authority.InstallationAuthority(ctx, app.AppID)
	if err != nil {
		return false, registrationCoverage{}, fmt.Errorf(
			"installation janitor: registration %d authority: %w",
			app.AppID,
			err,
		)
	}
	authority, err := validateInstallationAuthority(app, snapshot, j.now())
	if err != nil {
		return false, registrationCoverage{}, fmt.Errorf(
			"installation janitor: registration %d authority: %w",
			app.AppID,
			err,
		)
	}
	return j.reconcileRegistration(ctx, app, authority, cycle)
}

type registrationCoverage struct {
	registrationID int64
	repositories   map[int64]map[int64]struct{}
	pendingReady   *pendingCoverage
}

type pendingCoverage struct {
	activeEpoch           int64
	durableIntentRevision int64
	installationID        int64
	repositories          map[int64]struct{}
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
			repositoryIDs, err := j.enumerateRepositoryGrants(ctx, jwt, app.AppID, installation.ID)
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
			if candidate.pending && matchesExpected {
				repositories := make(map[int64]struct{}, len(candidate.repositoryIDs))
				for _, repositoryID := range candidate.repositoryIDs {
					repositories[repositoryID] = struct{}{}
				}
				coverage.pendingReady = &pendingCoverage{
					activeEpoch:           candidate.activeEpoch,
					durableIntentRevision: candidate.durableIntentRevision,
					installationID:        installation.ID,
					repositories:          repositories,
				}
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
	var removalErrs []error
	for _, installation := range actions {
		// The bound is spent on attempts, not successes. A failed suspend or
		// delete used to end the pass, so counting only what completed was
		// safe; now that the pass continues, counting completions would let
		// every registration spend one more destructive request than the
		// operator's bound allows.
		if cycle.attempted >= j.maxRemovals {
			cycle.RemovalLimitReached = true
			return false, registrationCoverage{}, errors.Join(removalErrs...)
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
			// The journal is one shared file, so its failure is the host's,
			// not this registration's.
			unsafeErr := fmt.Errorf(
				"installation janitor: registration %d audit removal: %w: %w",
				app.AppID,
				errJanitorUnsafe,
				recordErr,
			)
			return false, registrationCoverage{}, errors.Join(append(removalErrs, unsafeErr)...)
		}
		cycle.attempted++
		if installation.quarantine {
			if err := j.suspendInstallation(ctx, jwt, installation.installationID); err != nil {
				removalErrs = append(removalErrs, fmt.Errorf(
					"installation janitor: registration %d installation %d: %w",
					app.AppID,
					installation.installationID,
					err,
				))
				if ctx.Err() != nil {
					return false, registrationCoverage{}, errors.Join(removalErrs...)
				}
				continue
			}
		}
		if err := j.deleteInstallation(ctx, jwt, installation.installationID); err != nil {
			removalErrs = append(removalErrs, fmt.Errorf(
				"installation janitor: registration %d installation %d: %w",
				app.AppID,
				installation.installationID,
				err,
			))
			if ctx.Err() != nil {
				return false, registrationCoverage{}, errors.Join(removalErrs...)
			}
			continue
		}
		cycle.Removed++
	}

	if len(removalErrs) > 0 {
		return false, registrationCoverage{}, errors.Join(removalErrs...)
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

// grantReadPermissions is the typed form of the grant-read request, recorded in
// the mint audit as both requested and granted: the validation against
// grantReadPermissionScopes proves the returned grant identical to it, so no
// other grant reaches the audit on the validated-clean path.
var grantReadPermissions = Permissions{Metadata: "read"}

type grantReadMintRequest struct {
	Permissions Permissions `json:"permissions"`
}

// grantReadMintResponse decodes the janitor's own 201. It decodes
// expires_at for the same reason the worker-bound mint does: the
// enumeration credential's lifetime is returned data, and an
// unverified one cannot be reasoned about when revocation fails.
type grantReadMintResponse struct {
	Token               Secret            `json:"token"`
	ExpiresAt           Secret            `json:"expires_at"`
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
	registrationID int64,
	installationID int64,
) ([]int64, error) {
	token, mintErr := j.mintGrantReadToken(ctx, jwt, registrationID, installationID)
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
	if revokeErr != nil {
		// A token the janitor minted and could not revoke outlives the pass.
		// Its lifetime is bounded only when the mint's expiry check passed,
		// and unknown when the response never got that far, which is why an
		// unrevoked token is never merely waited out. Faulting the
		// registration would re-mint one every pass, so an unrevoked token
		// stops the pass instead: the daemon must not keep issuing
		// credentials it has just proven it cannot take back.
		revokeErr = fmt.Errorf("%w: %w", errJanitorUnsafe, revokeErr)
	}
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
	registrationID int64,
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
	// Minting is not idempotent, so from here on an error that leaves the
	// outcome unknown must not be retried: the token GitHub may have created
	// is live for GitHub's declared lifetime and this daemon never learned
	// its value, so it can never be revoked. Only a refusal proves nothing
	// was created.
	resp, err := j.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("mint grant-read token: %w: %w", errJanitorUnsafe, err)
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		mintErr := fmt.Errorf("mint grant-read token: %w", &APIError{
			Status:      resp.StatusCode,
			RequestPath: "/app/installations/{installation_id}/access_tokens",
		})
		if resp.StatusCode < http.StatusBadRequest || resp.StatusCode >= http.StatusInternalServerError {
			return "", fmt.Errorf("%w: %w", errJanitorUnsafe, mintErr)
		}
		// GitHub refused the request, which is this registration's own state:
		// a wrong key, a withdrawn installation.
		return "", mintErr
	}
	// GitHub created a token. Every path below either carries its value out
	// for the caller to revoke, or has lost it for good.
	var minted grantReadMintResponse
	decodeErr := decodeResponse(resp.Body, &minted)
	if minted.Token.Reveal() == "" {
		// No token was recovered, so GitHub created nothing this daemon must
		// account for: there is nothing to audit or revoke.
		if decodeErr != nil {
			return "", fmt.Errorf("mint grant-read token: %w: decode response", errJanitorUnsafe)
		}
		return "", fmt.Errorf("mint grant-read token: %w: response carries no token", errJanitorUnsafe)
	}
	// A token now provably exists. Every return below carries it out for the
	// caller to revoke, and it MUST be audited first: a token whose revoke also
	// fails is the live, unrevocable credential the whole audit exists to make
	// operator-findable, so it cannot depend on the grant being well-formed.
	// The clock is read once so the audit row and the expiry check share one
	// instant. classifyGrantReadMint returns the outcome, the granted scopes to
	// record (the fixed grant only when validated), the validated expiry (nil
	// otherwise), and the caller error that preserves each path's prior
	// meaning.
	mintedAt := j.now()
	outcome, granted, expiresAt, mintErr := classifyGrantReadMint(&minted, decodeErr, mintedAt)
	// A record that fails to commit fails the mint (mirroring the worker mint,
	// #545/#80): the token still travels out so the caller revokes it, and on
	// this failure it is never used. This is the barrier, so its error subsumes
	// any validation error the mint would otherwise have returned.
	if err := j.mintRecorder.RecordInstallationMint(InstallationMintRecord{
		MintedAt:       mintedAt.UTC(),
		RegistrationID: registrationID,
		InstallationID: installationID,
		Outcome:        outcome,
		Requested:      grantReadPermissions,
		Granted:        granted,
		ExpiresAt:      expiresAt,
	}); err != nil {
		return minted.Token, fmt.Errorf("mint grant-read token: %w: %w", errJanitorUnsafe, err)
	}
	return minted.Token, mintErr
}

// classifyGrantReadMint judges a grant-read 201 whose token is known to exist,
// and returns the audit outcome, the granted scopes to record (the fixed grant
// only when the scope comparison passed, else the zero Permissions, since the
// daemon does not vouch for a grant it rejected), the validated expiry (nil
// unless the whole grant is clean), and the caller error each path returns.
// decodeErr, when non-nil, means the body could not be decoded even though a
// token was recovered from it. The error strings preserve the meanings the
// inlined validation carried before the audit was added: an undecodable body
// and an expiry failure stay attributable to the registration (revoked, pass
// continues), while a returned-grant mismatch stays an ErrGrantMismatch.
func classifyGrantReadMint(
	minted *grantReadMintResponse,
	decodeErr error,
	now time.Time,
) (InstallationMintOutcome, Permissions, *time.Time, error) {
	if decodeErr != nil {
		return InstallationMintUndecodable, Permissions{}, nil,
			errors.New("mint grant-read token: decode response")
	}
	if !maps.Equal(minted.Permissions, grantReadPermissionScopes) ||
		minted.RepositorySelection != "selected" {
		return InstallationMintGrantRejected, Permissions{}, nil,
			fmt.Errorf("mint grant-read token: returned grant differs from request: %w", ErrGrantMismatch)
	}
	// The scope comparison passed, so the granted scope is the fixed request.
	// Bound the expiry before the token is used: missing, garbled, lapsed, or
	// over-long is rejected here, after the scope comparison so an over-broad
	// grant keeps its own error.
	expiresAt, err := checkInstallationTokenExpiry(minted.ExpiresAt, now)
	if err != nil {
		return InstallationMintExpiryRejected, grantReadPermissions, nil,
			fmt.Errorf("mint grant-read token: %w", err)
	}
	return InstallationMintValidated, grantReadPermissions, &expiresAt, nil
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

// withdrawCoverage shuts every gate while leaving the last pass's diagnostics
// published, so a registration that is failing or repeatedly removing does not
// read as one that has simply not been visited yet.
func (j *InstallationJanitor) withdrawCoverage() {
	j.mu.Lock()
	j.covered = map[int64]registrationCoverage{}
	j.mu.Unlock()
}

// publishPass replaces the gate's whole view of the latest pass. Coverage,
// faults, and churn are written together. Churn persists without incrementing
// when the pass-wide bound skips a known registration, and clears only after a
// clean or failed reconciliation or when the registration leaves the keystore.
// A reader that takes diagnostics in separate calls can still straddle a pass
// boundary, which may report a less specific inactive state but never grants
// coverage.
func (j *InstallationJanitor) publishPass(pass janitorPass) {
	covered := make(map[int64]registrationCoverage, len(pass.covered))
	for _, registration := range pass.covered {
		covered[registration.registrationID] = registration
	}
	j.mu.Lock()
	known := make(map[int64]struct{}, len(pass.registrations))
	for _, registrationID := range pass.registrations {
		known[registrationID] = struct{}{}
	}
	resolved := make(map[int64]struct{}, len(pass.covered)+len(pass.faults)+len(pass.churn))
	for _, registration := range pass.covered {
		resolved[registration.registrationID] = struct{}{}
	}
	for _, fault := range pass.faults {
		resolved[fault.RegistrationID] = struct{}{}
	}
	for _, registrationID := range pass.churn {
		resolved[registrationID] = struct{}{}
	}
	previousChurn := make(map[int64]int, len(j.churn))
	for _, registration := range j.churn {
		previousChurn[registration.RegistrationID] = registration.ConsecutivePasses
	}
	nextChurn := make([]JanitorRegistrationChurn, 0, len(pass.churn)+len(j.churn))
	for _, registrationID := range pass.churn {
		nextChurn = append(nextChurn, JanitorRegistrationChurn{
			RegistrationID:    registrationID,
			ConsecutivePasses: previousChurn[registrationID] + 1,
		})
	}
	for _, registration := range j.churn {
		_, stillKnown := known[registration.RegistrationID]
		_, reachedOutcome := resolved[registration.RegistrationID]
		if stillKnown && !reachedOutcome {
			nextChurn = append(nextChurn, registration)
		}
	}
	slices.SortFunc(nextChurn, func(a, b JanitorRegistrationChurn) int {
		return cmp.Compare(a.RegistrationID, b.RegistrationID)
	})
	j.covered = covered
	j.faults = slices.Clone(pass.faults)
	j.churn = nextChurn
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
	j.faults = nil
	j.churn = nil
	j.mu.Unlock()
}
