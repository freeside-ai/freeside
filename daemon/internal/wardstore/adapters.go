// Package wardstore composes ward's persistence ports with the authoritative
// SQLite store. It is deliberately separate from both packages: ward owns the
// runner contract, store owns durable state, and this boundary owns only the
// transaction wrappers and vocabulary mapping between them.
package wardstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// Adapters groups the two production ports backed by one open Store. They are
// separate Go types because AuthStoreLeaser.Get and HandoffJournal.Get
// intentionally use the same verb for different records.
type Adapters struct {
	Journal *Journal
	Leaser  *Leaser
}

// Journal backs ward's journal and atomic leased-open interfaces.
type Journal struct {
	store *store.Store
}

// Leaser backs ward's identity binding and mutation-lease interface.
type Leaser struct {
	store *store.Store
}

// New constructs the production ward persistence adapters.
func New(st *store.Store) (*Adapters, error) {
	if st == nil {
		return nil, errors.New("ward store adapters: nil store")
	}
	return &Adapters{
		Journal: &Journal{store: st},
		Leaser:  &Leaser{store: st},
	}, nil
}

// AuthStoreVolume returns the trusted identity-to-volume binding.
func (a *Leaser) AuthStoreVolume(
	ctx context.Context, id domain.AuthIdentityID,
) (string, error) {
	var volume string
	err := a.store.Read(ctx, func(tx *store.ReadTx) error {
		identity, err := tx.GetAuthIdentity(ctx, id)
		if err != nil {
			return err
		}
		volume = identity.AuthStoreVolume
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("auth-store volume for identity %q: %w", id, err)
	}
	return volume, nil
}

// Acquire opens or converges on one mutation window.
func (a *Leaser) Acquire(
	ctx context.Context,
	id domain.AuthIdentityID,
	holder domain.InvocationID,
	now, expiresAt time.Time,
) (domain.AuthStoreMutationLease, error) {
	var lease domain.AuthStoreMutationLease
	err := a.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		lease, err = tx.AcquireAuthStoreMutationLease(ctx, id, holder, now, expiresAt)
		return err
	})
	if err != nil {
		return domain.AuthStoreMutationLease{}, err
	}
	return lease, nil
}

// Get reconstructs the identity's current mutation-window row.
func (a *Leaser) Get(
	ctx context.Context, id domain.AuthIdentityID,
) (domain.AuthStoreMutationLease, error) {
	var lease domain.AuthStoreMutationLease
	err := a.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		lease, err = tx.GetAuthStoreMutationLease(ctx, id)
		return err
	})
	if err != nil {
		return domain.AuthStoreMutationLease{}, err
	}
	return lease, nil
}

// Release ends the exact held window. Store refusals that mean the recorded
// window already ended map to ward's convergence sentinel.
func (a *Leaser) Release(
	ctx context.Context,
	id domain.AuthIdentityID,
	holder domain.InvocationID,
	fence int64,
	releasedAt time.Time,
) error {
	err := a.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.ReleaseAuthStoreMutationLease(ctx, id, holder, fence, releasedAt)
	})
	if errors.Is(err, store.ErrLeaseNotHeld) || errors.Is(err, store.ErrLeaseWindowRegresses) {
		return fmt.Errorf("%w: %w", ward.ErrLeaseWindowEnded, err)
	}
	return err
}

// Begin opens an unleased journal record.
func (a *Journal) Begin(ctx context.Context, rec ward.HandoffJournalRecord) error {
	if err := rec.Validate(); err != nil {
		return err
	}
	return a.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.BeginHandoffJournal(ctx, toStoreRecord(rec))
	})
}

// BeginLeased atomically acquires the mutation window and opens the journal
// record carrying its exact reference.
func (a *Journal) BeginLeased(
	ctx context.Context,
	rec ward.HandoffJournalRecord,
	claim ward.AuthStoreLeaseClaim,
	now, expiresAt time.Time,
) (domain.AuthStoreMutationLease, error) {
	if err := rec.Validate(); err != nil {
		return domain.AuthStoreMutationLease{}, err
	}
	var lease domain.AuthStoreMutationLease
	err := a.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		lease, err = tx.BeginLeasedHandoffJournal(
			ctx, toStoreRecord(rec), claim.AuthIdentityID, claim.Holder, now, expiresAt,
		)
		return err
	})
	if err != nil {
		return domain.AuthStoreMutationLease{}, err
	}
	return lease, nil
}

// Get reconstructs one journal record and re-runs both store and ward gates.
func (a *Journal) Get(
	ctx context.Context, runID string,
) (ward.HandoffJournalRecord, error) {
	var rec store.HandoffJournalRecord
	err := a.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		rec, err = tx.GetHandoffJournal(ctx, runID)
		return err
	})
	if err != nil {
		return ward.HandoffJournalRecord{}, err
	}
	converted := fromStoreRecord(rec)
	if err := converted.Validate(); err != nil {
		return ward.HandoffJournalRecord{}, err
	}
	return converted, nil
}

// MarkSeedObserved commits the pre-writer base proof.
func (a *Journal) MarkSeedObserved(ctx context.Context, runID, observedBaseSHA string) error {
	return a.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.MarkHandoffSeedObserved(ctx, runID, observedBaseSHA)
	})
}

// MarkCredentialObserved commits the pre-writer credential digest.
func (a *Journal) MarkCredentialObserved(ctx context.Context, runID, preDigest string) error {
	return a.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.MarkHandoffCredentialObserved(ctx, runID, preDigest)
	})
}

// MarkWriterComplete commits the writer-complete proof.
func (a *Journal) MarkWriterComplete(ctx context.Context, runID string) error {
	return a.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.MarkHandoffWriterComplete(ctx, runID)
	})
}

// MarkExportMaterialized commits the verified export's host location.
func (a *Journal) MarkExportMaterialized(ctx context.Context, runID, exportDir string) error {
	return a.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.MarkHandoffExportMaterialized(ctx, runID, exportDir)
	})
}

// Close commits the terminal journal outcome.
func (a *Journal) Close(
	ctx context.Context, runID string, outcome ward.HandoffJournalOutcome,
) error {
	return a.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.CloseHandoffJournal(ctx, runID, store.HandoffJournalOutcome(outcome))
	})
}

func toStoreRecord(rec ward.HandoffJournalRecord) store.HandoffJournalRecord {
	converted := store.HandoffJournalRecord{
		RunID:               rec.RunID,
		OwnershipToken:      rec.OwnershipToken,
		SpecDigest:          rec.SpecDigest,
		ObservedBaseSHA:     rec.ObservedBaseSHA,
		CredentialPreDigest: rec.CredentialPreDigest,
		WriterComplete:      rec.WriterComplete,
		ExportDir:           rec.ExportDir,
		OpenedAt:            rec.OpenedAt.UTC(),
	}
	if rec.Lease != nil {
		converted.Lease = &store.HandoffJournalLease{
			AuthIdentityID: rec.Lease.AuthIdentityID,
			Holder:         rec.Lease.Holder,
			Fence:          rec.Lease.Fence,
			AcquiredAt:     rec.Lease.AcquiredAt.UTC(),
			ExpiresAt:      rec.Lease.ExpiresAt.UTC(),
		}
	}
	if rec.Outcome != nil {
		outcome := store.HandoffJournalOutcome(*rec.Outcome)
		converted.Outcome = &outcome
	}
	return converted
}

func fromStoreRecord(rec store.HandoffJournalRecord) ward.HandoffJournalRecord {
	converted := ward.HandoffJournalRecord{
		RunID:               rec.RunID,
		OwnershipToken:      rec.OwnershipToken,
		SpecDigest:          rec.SpecDigest,
		ObservedBaseSHA:     rec.ObservedBaseSHA,
		CredentialPreDigest: rec.CredentialPreDigest,
		WriterComplete:      rec.WriterComplete,
		ExportDir:           rec.ExportDir,
		OpenedAt:            rec.OpenedAt,
	}
	if rec.Lease != nil {
		converted.Lease = &ward.HandoffJournalLease{
			AuthIdentityID: rec.Lease.AuthIdentityID,
			Holder:         rec.Lease.Holder,
			Fence:          rec.Lease.Fence,
			AcquiredAt:     rec.Lease.AcquiredAt,
			ExpiresAt:      rec.Lease.ExpiresAt,
		}
	}
	if rec.Outcome != nil {
		outcome := ward.HandoffJournalOutcome(*rec.Outcome)
		converted.Outcome = &outcome
	}
	return converted
}

var (
	_ ward.AuthStoreLeaser     = (*Leaser)(nil)
	_ ward.HandoffJournal      = (*Journal)(nil)
	_ ward.LeasedHandoffOpener = (*Journal)(nil)
)
