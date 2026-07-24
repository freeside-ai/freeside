package publish

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// TrustedOwner is one owner account the operator has approved for a public
// registration. Login and ID are both required: neither a renamed account nor
// a reused login may inherit the other coordinate's authority.
type TrustedOwner struct {
	Login string
	ID    int64
}

// TrustedInstallation binds one canonical installation to the exact repository
// IDs it may serve. The source aggregates every trusted repository for an
// installation into one record so reconciliation compares one complete set.
type TrustedInstallation struct {
	RegistrationID int64
	InstallationID int64
	Account        string
	AccountID      int64
	RepositoryIDs  []int64
}

// PendingInstallationEnvelope is the bounded, non-authoritative exception that
// lets a native install or selected-repository expansion finish without racing
// the janitor. A zero InstallationID is allowed only before GitHub assigns one.
// CurrentRepositoryIDs bind an expansion to the trusted base set;
// ExpectedRepositoryIDs are the exact post-change set.
type PendingInstallationEnvelope struct {
	ActiveEpoch            int64
	DurableIntentRevision  int64
	RegistrationID         int64
	ExpectedAccount        string
	ExpectedAccountID      int64
	InstallationID         int64
	CurrentRepositoryIDs   []int64
	ExpectedRepositoryIDs  []int64
	RequiredRepositoryMode string
	ExpiresAt              time.Time
}

// InstallationAuthority is the canonical local snapshot for one registration.
// ActiveEpoch and DurableIntentRevision identify the only pending envelope that
// may receive the temporary reconciliation exception. Trusted installations
// remain authoritative independently of that pending-intent frontier.
type InstallationAuthority struct {
	ActiveEpoch           int64
	DurableIntentRevision int64
	TrustedOwners         []TrustedOwner
	TrustedInstallations  []TrustedInstallation
	Pending               *PendingInstallationEnvelope
}

// InstallationAuthoritySource supplies the canonical installation bindings and
// pending envelope for one stable numeric App registration. Returned values are
// trusted local policy, but still validated before they can drive credentials
// or destructive effects.
type InstallationAuthoritySource interface {
	InstallationAuthority(ctx context.Context, registrationID int64) (InstallationAuthority, error)
}

type trustedOwnerKey struct {
	login string
	id    int64
}

type authorityCandidate struct {
	pending              bool
	installationID       int64
	account              string
	accountID            int64
	repositoryIDs        []int64
	allowedRepositoryIDs []int64
}

type validatedInstallationAuthority struct {
	trustedOwners map[trustedOwnerKey]struct{}
	trusted       map[int64]authorityCandidate
	pending       *authorityCandidate
}

func validateInstallationAuthority(
	app AppCredentials,
	snapshot InstallationAuthority,
	now time.Time,
) (validatedInstallationAuthority, error) {
	validated := validatedInstallationAuthority{
		trustedOwners: make(map[trustedOwnerKey]struct{}, len(snapshot.TrustedOwners)),
		trusted:       make(map[int64]authorityCandidate, len(snapshot.TrustedInstallations)),
	}
	seenOwnerIDs := make(map[int64]string, len(snapshot.TrustedOwners))
	for _, owner := range snapshot.TrustedOwners {
		if owner.ID <= 0 || validateOwnerLogin(owner.Login) != nil {
			return validatedInstallationAuthority{}, errors.New("invalid trusted owner")
		}
		login := strings.ToLower(owner.Login)
		if prior, ok := seenOwnerIDs[owner.ID]; ok && prior != login {
			return validatedInstallationAuthority{}, errors.New("conflicting trusted owner identities")
		}
		seenOwnerIDs[owner.ID] = login
		validated.trustedOwners[trustedOwnerKey{login: login, id: owner.ID}] = struct{}{}
	}
	if app.Visibility == AppVisibilityPublic {
		registrationOwner := trustedOwnerKey{login: strings.ToLower(app.Owner), id: app.OwnerID}
		if _, ok := validated.trustedOwners[registrationOwner]; !ok {
			return validatedInstallationAuthority{}, errors.New("registration owner is absent from the trusted owner set")
		}
	}

	seenBoundAccounts := make(map[trustedOwnerKey]int64, len(snapshot.TrustedInstallations))
	seenRepositoryIDs := make(map[int64]int64)
	for _, binding := range snapshot.TrustedInstallations {
		if binding.RegistrationID != app.AppID || binding.InstallationID <= 0 ||
			binding.AccountID <= 0 || validateOwnerLogin(binding.Account) != nil {
			return validatedInstallationAuthority{}, errors.New("invalid trusted installation binding")
		}
		if _, duplicate := validated.trusted[binding.InstallationID]; duplicate {
			return validatedInstallationAuthority{}, errors.New("duplicate trusted installation binding")
		}
		if err := validateBoundAccount(app, validated.trustedOwners, binding.Account, binding.AccountID); err != nil {
			return validatedInstallationAuthority{}, err
		}
		accountKey := trustedOwnerKey{
			login: strings.ToLower(binding.Account),
			id:    binding.AccountID,
		}
		if prior, duplicate := seenBoundAccounts[accountKey]; duplicate && prior != binding.InstallationID {
			return validatedInstallationAuthority{}, errors.New("account is bound to multiple installations")
		}
		seenBoundAccounts[accountKey] = binding.InstallationID
		repositoryIDs, err := canonicalRepositoryIDs(binding.RepositoryIDs, false)
		if err != nil {
			return validatedInstallationAuthority{}, fmt.Errorf("trusted installation binding: %w", err)
		}
		for _, repositoryID := range repositoryIDs {
			if prior, duplicate := seenRepositoryIDs[repositoryID]; duplicate && prior != binding.InstallationID {
				return validatedInstallationAuthority{}, errors.New("repository is bound to multiple installations")
			}
			seenRepositoryIDs[repositoryID] = binding.InstallationID
		}
		validated.trusted[binding.InstallationID] = authorityCandidate{
			installationID:       binding.InstallationID,
			account:              strings.ToLower(binding.Account),
			accountID:            binding.AccountID,
			repositoryIDs:        repositoryIDs,
			allowedRepositoryIDs: repositoryIDs,
		}
	}

	if snapshot.Pending == nil {
		return validated, nil
	}
	pending := *snapshot.Pending
	if pending.RegistrationID != app.AppID || pending.ActiveEpoch <= 0 ||
		pending.DurableIntentRevision <= 0 || pending.ExpectedAccountID <= 0 ||
		validateOwnerLogin(pending.ExpectedAccount) != nil || pending.InstallationID < 0 ||
		pending.RequiredRepositoryMode != "selected" || pending.ExpiresAt.IsZero() {
		return validatedInstallationAuthority{}, errors.New("invalid pending installation envelope")
	}
	if err := validatePendingAccount(app, pending.ExpectedAccount, pending.ExpectedAccountID); err != nil {
		return validatedInstallationAuthority{}, err
	}
	currentIDs, err := canonicalRepositoryIDs(pending.CurrentRepositoryIDs, true)
	if err != nil {
		return validatedInstallationAuthority{}, fmt.Errorf("pending current repository IDs: %w", err)
	}
	expectedIDs, err := canonicalRepositoryIDs(pending.ExpectedRepositoryIDs, false)
	if err != nil {
		return validatedInstallationAuthority{}, fmt.Errorf("pending expected repository IDs: %w", err)
	}
	if !isRepositorySubset(currentIDs, expectedIDs) {
		return validatedInstallationAuthority{}, errors.New("pending current repository IDs are not a subset of the expected set")
	}
	if pending.InstallationID == 0 && len(currentIDs) != 0 {
		return validatedInstallationAuthority{}, errors.New("pending envelope without an installation carries a current repository set")
	}
	if pending.InstallationID == 0 {
		for _, binding := range validated.trusted {
			if binding.account == strings.ToLower(pending.ExpectedAccount) &&
				binding.accountID == pending.ExpectedAccountID {
				return validatedInstallationAuthority{}, errors.New(
					"pending envelope omits the known installation identity",
				)
			}
		}
	}
	for installationID, binding := range validated.trusted {
		if installationID != pending.InstallationID &&
			binding.account == strings.ToLower(pending.ExpectedAccount) &&
			binding.accountID == pending.ExpectedAccountID {
			return validatedInstallationAuthority{}, errors.New(
				"pending envelope names a second installation for a bound account",
			)
		}
	}
	if pending.InstallationID > 0 {
		binding, bound := validated.trusted[pending.InstallationID]
		switch {
		case bound && (binding.account != strings.ToLower(pending.ExpectedAccount) ||
			binding.accountID != pending.ExpectedAccountID ||
			!slices.Equal(binding.repositoryIDs, currentIDs)):
			return validatedInstallationAuthority{}, errors.New("pending envelope does not bind the current trusted installation state")
		case !bound && len(currentIDs) != 0:
			return validatedInstallationAuthority{}, errors.New("pending envelope carries an unbound current repository set")
		}
	}

	// Stale, superseded, or expired envelopes are structurally valid history,
	// but grant no reconciliation exception. Ordinary unknown-installation
	// cleanup therefore resumes without trusting them.
	if pending.ActiveEpoch != snapshot.ActiveEpoch ||
		pending.DurableIntentRevision != snapshot.DurableIntentRevision ||
		!pending.ExpiresAt.After(now) {
		return validated, nil
	}
	validated.pending = &authorityCandidate{
		pending:              true,
		installationID:       pending.InstallationID,
		account:              strings.ToLower(pending.ExpectedAccount),
		accountID:            pending.ExpectedAccountID,
		repositoryIDs:        expectedIDs,
		allowedRepositoryIDs: currentIDs,
	}
	return validated, nil
}

func validateBoundAccount(
	app AppCredentials,
	trustedOwners map[trustedOwnerKey]struct{},
	account string,
	accountID int64,
) error {
	if app.Visibility == AppVisibilityPrivate {
		if !strings.EqualFold(account, app.Owner) || accountID != app.OwnerID {
			return errors.New("private trusted installation does not match the registration owner")
		}
		return nil
	}
	if _, ok := trustedOwners[trustedOwnerKey{login: strings.ToLower(account), id: accountID}]; !ok {
		return errors.New("public trusted installation owner is not trusted")
	}
	return nil
}

func validatePendingAccount(app AppCredentials, account string, accountID int64) error {
	if app.Visibility == AppVisibilityPrivate &&
		(!strings.EqualFold(account, app.Owner) || accountID != app.OwnerID) {
		return errors.New("private pending installation does not match the registration owner")
	}
	return nil
}

func canonicalRepositoryIDs(ids []int64, emptyAllowed bool) ([]int64, error) {
	if len(ids) == 0 {
		if emptyAllowed {
			return []int64{}, nil
		}
		return nil, errors.New("empty repository ID set")
	}
	canonical := append([]int64(nil), ids...)
	slices.Sort(canonical)
	for index, id := range canonical {
		if id <= 0 {
			return nil, errors.New("non-positive repository ID")
		}
		if index > 0 && id == canonical[index-1] {
			return nil, errors.New("duplicate repository ID")
		}
	}
	return canonical, nil
}

func isRepositorySubset(subset, set []int64) bool {
	for _, id := range subset {
		if _, found := slices.BinarySearch(set, id); !found {
			return false
		}
	}
	return true
}

func (a validatedInstallationAuthority) candidate(
	installation installationResponse,
) (authorityCandidate, bool, bool) {
	if a.pending != nil &&
		(a.pending.installationID == 0 || a.pending.installationID == installation.ID) {
		if a.pending.account == strings.ToLower(installation.Account.Login) &&
			a.pending.accountID == installation.Account.ID {
			pending := *a.pending
			pending.installationID = installation.ID
			return pending, true, false
		}
		if a.pending.installationID == installation.ID {
			return authorityCandidate{}, false, true
		}
	}
	if trusted, ok := a.trusted[installation.ID]; ok {
		if trusted.account == strings.ToLower(installation.Account.Login) &&
			trusted.accountID == installation.Account.ID {
			return trusted, true, false
		}
		return authorityCandidate{}, false, true
	}
	return authorityCandidate{}, false, false
}
