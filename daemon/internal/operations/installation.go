package operations

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

// AuthorityDocumentStore is the operator-authored installation-policy file.
type AuthorityDocumentStore interface {
	UpdateDocument(
		context.Context,
		func(*publish.InstallationAuthorityDocument) error,
	) error
}

// InstallationIntentRequest supplies canonical coordinates already displayed
// by GitHub's native installation flow.
type InstallationIntentRequest struct {
	RegistrationID int64
	Account        string
	AccountID      int64
	InstallationID int64
	RepositoryID   int64
	ExpiresAt      time.Time
}

// BeginInstallation records the bounded selected-repository exception before
// the operator enters GitHub's native flow.
func BeginInstallation(
	ctx context.Context,
	documents AuthorityDocumentStore,
	req InstallationIntentRequest,
	now func() time.Time,
) (publish.PendingEnvelopeRecord, error) {
	if documents == nil || now == nil {
		return publish.PendingEnvelopeRecord{}, errors.New(
			"begin installation: nil authority store or clock")
	}
	if req.RegistrationID <= 0 || req.AccountID <= 0 ||
		req.RepositoryID <= 0 || req.InstallationID < 0 ||
		req.Account == "" || !req.ExpiresAt.After(now().UTC()) {
		return publish.PendingEnvelopeRecord{}, errors.New(
			"begin installation: invalid registration, account, installation, repository, or expiry")
	}
	var pending publish.PendingEnvelopeRecord
	err := documents.UpdateDocument(ctx, func(document *publish.InstallationAuthorityDocument) error {
		currentTime := now().UTC()
		if !req.ExpiresAt.After(currentTime) {
			return errors.New("begin installation: intent expired before it could be recorded")
		}
		entry, err := authorityEntry(*document, req.RegistrationID)
		if err != nil {
			return err
		}
		if entry.Pending != nil && entry.Pending.ExpiresAt.After(currentTime) {
			return errors.New(
				"begin installation: registration already has a pending intent; use --resume")
		}
		current := []int64{}
		for _, binding := range entry.TrustedInstallations {
			if binding.InstallationID == req.InstallationID && req.InstallationID > 0 {
				if binding.AccountID != req.AccountID ||
					!strings.EqualFold(binding.Account, req.Account) {
					return errors.New(
						"begin installation: installation identity differs from trusted binding")
				}
				current = slices.Clone(binding.RepositoryIDs)
			}
			if slices.Contains(binding.RepositoryIDs, req.RepositoryID) {
				return errors.New("begin installation: repository is already trusted")
			}
		}
		expected := append(slices.Clone(current), req.RepositoryID)
		slices.Sort(expected)
		expected = slices.Compact(expected)
		entry.DurableIntentRevision++
		pending = publish.PendingEnvelopeRecord{
			ActiveEpoch:            entry.ActiveEpoch,
			DurableIntentRevision:  entry.DurableIntentRevision,
			ExpectedAccount:        req.Account,
			ExpectedAccountID:      req.AccountID,
			InstallationID:         &req.InstallationID,
			CurrentRepositoryIDs:   current,
			ExpectedRepositoryIDs:  expected,
			RequiredRepositoryMode: "selected",
			ExpiresAt:              req.ExpiresAt.UTC(),
		}
		entry.Pending = &pending
		replaceAuthorityEntry(document, entry)
		return nil
	})
	if err != nil {
		return publish.PendingEnvelopeRecord{}, err
	}
	return pending, nil
}

// PromoteInstallation converts exactly the still-current pending envelope to
// a trusted binding after the janitor's canonical pass reports PendingReady.
func PromoteInstallation(
	ctx context.Context,
	documents AuthorityDocumentStore,
	approved publish.PendingInstallationEnvelope,
	repositoryID int64,
	now func() time.Time,
) error {
	if documents == nil || now == nil {
		return errors.New("promote installation: nil authority store or clock")
	}
	if approved.RegistrationID <= 0 || approved.InstallationID <= 0 ||
		approved.ActiveEpoch <= 0 || approved.DurableIntentRevision <= 0 ||
		approved.ExpectedAccountID <= 0 || approved.ExpectedAccount == "" ||
		approved.RequiredRepositoryMode != "selected" ||
		!approved.ExpiresAt.After(now().UTC()) {
		return errors.New("promote installation: invalid approved pending envelope")
	}
	return documents.UpdateDocument(ctx, func(document *publish.InstallationAuthorityDocument) error {
		currentTime := now().UTC()
		entry, err := authorityEntry(*document, approved.RegistrationID)
		if err != nil {
			return err
		}
		pending := entry.Pending
		if pending == nil || pending.InstallationID == nil ||
			pending.ActiveEpoch != approved.ActiveEpoch ||
			pending.DurableIntentRevision != approved.DurableIntentRevision ||
			(*pending.InstallationID != 0 &&
				*pending.InstallationID != approved.InstallationID) ||
			pending.ExpectedAccountID != approved.ExpectedAccountID ||
			!strings.EqualFold(pending.ExpectedAccount, approved.ExpectedAccount) ||
			pending.RequiredRepositoryMode != approved.RequiredRepositoryMode ||
			!pending.ExpiresAt.After(currentTime) ||
			!pending.ExpiresAt.Equal(approved.ExpiresAt) ||
			!slices.Equal(pending.CurrentRepositoryIDs, approved.CurrentRepositoryIDs) ||
			!slices.Equal(pending.ExpectedRepositoryIDs, approved.ExpectedRepositoryIDs) {
			return errors.New("promote installation: pending intent is absent, changed, or expired")
		}
		var added []int64
		for _, repositoryID := range pending.ExpectedRepositoryIDs {
			if !slices.Contains(pending.CurrentRepositoryIDs, repositoryID) {
				added = append(added, repositoryID)
			}
		}
		if len(added) != 1 || added[0] != repositoryID {
			return errors.New(
				"promote installation: pending envelope does not add the approved repository")
		}
		for _, candidate := range document.Registrations {
			for _, binding := range candidate.TrustedInstallations {
				if slices.Contains(binding.RepositoryIDs, repositoryID) {
					return fmt.Errorf(
						"promote installation: repository %d is already trusted by registration %d",
						repositoryID, candidate.RegistrationID,
					)
				}
			}
		}
		found := false
		for i := range entry.TrustedInstallations {
			binding := &entry.TrustedInstallations[i]
			if binding.InstallationID != approved.InstallationID {
				continue
			}
			if binding.AccountID != approved.ExpectedAccountID ||
				!strings.EqualFold(binding.Account, approved.ExpectedAccount) {
				return errors.New("promote installation: trusted installation identity differs")
			}
			binding.RepositoryIDs = slices.Clone(pending.ExpectedRepositoryIDs)
			found = true
		}
		if !found {
			entry.TrustedInstallations = append(
				entry.TrustedInstallations,
				publish.TrustedInstallationRecord{
					InstallationID: approved.InstallationID,
					Account:        approved.ExpectedAccount,
					AccountID:      approved.ExpectedAccountID,
					RepositoryIDs:  slices.Clone(pending.ExpectedRepositoryIDs),
				},
			)
		}
		slices.SortFunc(entry.TrustedInstallations, func(a, b publish.TrustedInstallationRecord) int {
			return cmp.Compare(a.InstallationID, b.InstallationID)
		})
		ownerFound := false
		for _, owner := range entry.TrustedOwners {
			if owner.ID == approved.ExpectedAccountID &&
				strings.EqualFold(owner.Login, approved.ExpectedAccount) {
				ownerFound = true
			}
		}
		if !ownerFound {
			entry.TrustedOwners = append(entry.TrustedOwners, publish.TrustedOwnerRecord{
				Login: approved.ExpectedAccount, ID: approved.ExpectedAccountID,
			})
			slices.SortFunc(entry.TrustedOwners, func(a, b publish.TrustedOwnerRecord) int {
				return cmp.Compare(a.ID, b.ID)
			})
		}
		entry.DurableIntentRevision++
		entry.Pending = nil
		replaceAuthorityEntry(document, entry)
		return nil
	})
}

func authorityEntry(
	document publish.InstallationAuthorityDocument,
	registrationID int64,
) (publish.InstallationAuthorityEntry, error) {
	for _, entry := range document.Registrations {
		if entry.RegistrationID == registrationID {
			return entry, nil
		}
	}
	return publish.InstallationAuthorityEntry{}, fmt.Errorf(
		"installation authority has no registration %d", registrationID)
}

func replaceAuthorityEntry(
	document *publish.InstallationAuthorityDocument,
	replacement publish.InstallationAuthorityEntry,
) {
	for i := range document.Registrations {
		if document.Registrations[i].RegistrationID == replacement.RegistrationID {
			document.Registrations[i] = replacement
			return
		}
	}
}
