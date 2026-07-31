package operations_test

import (
	"context"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/operations"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

type documentStoreStub struct {
	document publish.InstallationAuthorityDocument
}

func (s *documentStoreStub) UpdateDocument(
	_ context.Context,
	update func(*publish.InstallationAuthorityDocument) error,
) error {
	document := s.document
	if err := update(&document); err != nil {
		return err
	}
	if _, err := document.Encode(); err != nil {
		return err
	}
	s.document = document
	return nil
}

func TestInstallationIntentBindsAndPromotesExactRepositorySet(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	store := &documentStoreStub{document: publish.InstallationAuthorityDocument{
		Version: 1,
		Registrations: []publish.InstallationAuthorityEntry{{
			RegistrationID: 11, ActiveEpoch: 3, DurableIntentRevision: 7,
			TrustedOwners: []publish.TrustedOwnerRecord{{Login: "example", ID: 22}},
			TrustedInstallations: []publish.TrustedInstallationRecord{{
				InstallationID: 33, Account: "example", AccountID: 22,
				RepositoryIDs: []int64{44},
			}},
			Pending: nil,
		}},
	}}
	req := operations.InstallationIntentRequest{
		RegistrationID: 11, Account: "example", AccountID: 22,
		InstallationID: 33, RepositoryID: 55,
		ExpiresAt: now.Add(time.Hour),
	}
	pending, err := operations.BeginInstallation(context.Background(), store, req, clock)
	if err != nil {
		t.Fatal(err)
	}
	if pending.DurableIntentRevision != 8 ||
		len(pending.CurrentRepositoryIDs) != 1 ||
		len(pending.ExpectedRepositoryIDs) != 2 {
		t.Fatalf("pending = %+v", pending)
	}
	if err := operations.PromoteInstallation(
		context.Background(), store,
		approvedPending(req.RegistrationID, pending, req.InstallationID),
		req.RepositoryID, clock,
	); err != nil {
		t.Fatal(err)
	}
	entry := store.document.Registrations[0]
	if entry.Pending != nil || entry.DurableIntentRevision != 9 {
		t.Fatalf("promoted entry = %+v", entry)
	}
	if got := entry.TrustedInstallations[0].RepositoryIDs; len(got) != 2 ||
		got[0] != 44 || got[1] != 55 {
		t.Fatalf("trusted repositories = %v, want [44 55]", got)
	}
}

func TestInstallationIntentRefusesOverlapAndChangedPromotion(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	store := &documentStoreStub{document: publish.InstallationAuthorityDocument{
		Version: 1,
		Registrations: []publish.InstallationAuthorityEntry{{
			RegistrationID: 11, ActiveEpoch: 3, DurableIntentRevision: 7,
			TrustedOwners: []publish.TrustedOwnerRecord{{Login: "example", ID: 22}},
			TrustedInstallations: []publish.TrustedInstallationRecord{{
				InstallationID: 33, Account: "example", AccountID: 22,
				RepositoryIDs: []int64{44},
			}},
			Pending: nil,
		}},
	}}
	alreadyTrusted := operations.InstallationIntentRequest{
		RegistrationID: 11, Account: "example", AccountID: 22,
		InstallationID: 33, RepositoryID: 44,
		ExpiresAt: now.Add(time.Hour),
	}
	if _, err := operations.BeginInstallation(
		context.Background(), store, alreadyTrusted, clock,
	); err == nil {
		t.Fatal("BeginInstallation accepted an already-trusted repository")
	}
	pending := alreadyTrusted
	pending.RepositoryID = 55
	created, err := operations.BeginInstallation(
		context.Background(), store, pending, clock,
	)
	if err != nil {
		t.Fatal(err)
	}
	changed := approvedPending(pending.RegistrationID, created, pending.InstallationID)
	changed.ExpectedRepositoryIDs = append(changed.ExpectedRepositoryIDs, 66)
	if err := operations.PromoteInstallation(
		context.Background(), store, changed, pending.RepositoryID, clock,
	); err == nil {
		t.Fatal("PromoteInstallation accepted changed coordinates")
	}
	store.document.Registrations[0].Pending.ExpiresAt = now.Add(-time.Minute)
	replacement := pending
	replacement.ExpiresAt = now.Add(time.Hour)
	replaced, err := operations.BeginInstallation(
		context.Background(), store, replacement, clock,
	)
	if err != nil {
		t.Fatalf("BeginInstallation did not replace expired intent: %v", err)
	}
	if replaced.DurableIntentRevision != 9 {
		t.Fatalf("replacement revision = %d, want 9", replaced.DurableIntentRevision)
	}
}

func TestPromoteInstallationRechecksCrossRegistrationOwnership(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	pending := publish.PendingEnvelopeRecord{
		ActiveEpoch: 3, DurableIntentRevision: 8,
		ExpectedAccount: "example", ExpectedAccountID: 22,
		InstallationID:         ptr(int64(33)),
		CurrentRepositoryIDs:   []int64{},
		ExpectedRepositoryIDs:  []int64{55},
		RequiredRepositoryMode: "selected",
		ExpiresAt:              now.Add(time.Hour),
	}
	store := &documentStoreStub{document: publish.InstallationAuthorityDocument{
		Version: 1,
		Registrations: []publish.InstallationAuthorityEntry{
			{
				RegistrationID: 11, ActiveEpoch: 3, DurableIntentRevision: 8,
				TrustedOwners:        []publish.TrustedOwnerRecord{{Login: "example", ID: 22}},
				TrustedInstallations: []publish.TrustedInstallationRecord{},
				Pending:              &pending,
			},
			{
				RegistrationID: 12, ActiveEpoch: 1, DurableIntentRevision: 1,
				TrustedOwners: []publish.TrustedOwnerRecord{{Login: "other", ID: 23}},
				TrustedInstallations: []publish.TrustedInstallationRecord{{
					InstallationID: 34, Account: "other", AccountID: 23,
					RepositoryIDs: []int64{55},
				}},
			},
		},
	}}
	err := operations.PromoteInstallation(
		context.Background(), store, approvedPending(11, pending, 33), 55, clock,
	)
	if err == nil {
		t.Fatal("PromoteInstallation accepted repository trusted by another registration")
	}
	if store.document.Registrations[0].Pending == nil ||
		len(store.document.Registrations[0].TrustedInstallations) != 0 {
		t.Fatal("failed cross-registration promotion mutated selected authority")
	}
}

func approvedPending(
	registrationID int64,
	record publish.PendingEnvelopeRecord,
	installationID int64,
) publish.PendingInstallationEnvelope {
	return publish.PendingInstallationEnvelope{
		ActiveEpoch:            record.ActiveEpoch,
		DurableIntentRevision:  record.DurableIntentRevision,
		RegistrationID:         registrationID,
		ExpectedAccount:        record.ExpectedAccount,
		ExpectedAccountID:      record.ExpectedAccountID,
		InstallationID:         installationID,
		CurrentRepositoryIDs:   record.CurrentRepositoryIDs,
		ExpectedRepositoryIDs:  record.ExpectedRepositoryIDs,
		RequiredRepositoryMode: record.RequiredRepositoryMode,
		ExpiresAt:              record.ExpiresAt,
	}
}

func ptr[T any](value T) *T {
	return &value
}
