package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func journalRecord(runID string) store.HandoffJournalRecord {
	return store.HandoffJournalRecord{
		RunID:          runID,
		OwnershipToken: "00112233445566778899aabbccddeeff",
		SpecDigest:     strings.Repeat("ab", 32),
		OpenedAt:       leaseEpoch,
	}
}

func TestHandoffJournalDurableLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openWithIdentity(t, testAuthIdentity())
	rec := journalRecord("journal-run")

	var lease domain.AuthStoreMutationLease
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		lease, err = tx.BeginLeasedHandoffJournal(
			ctx, rec, "auth-1", "inv-1", leaseEpoch, leaseEpoch.Add(time.Hour),
		)
		return err
	}); err != nil {
		t.Fatalf("BeginLeasedHandoffJournal: %v", err)
	}
	if lease.AuthIdentityID != "auth-1" || lease.Holder != "inv-1" {
		t.Fatalf("lease = %+v, want auth-1/inv-1", lease)
	}

	observedBase := strings.Repeat("12", 20)
	preDigest := strings.Repeat("cd", 32)
	exportDir := filepath.Join(t.TempDir(), "export")
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.MarkHandoffSeedObserved(ctx, rec.RunID, observedBase); err != nil {
			return err
		}
		// Exact amendment retries converge; a different proof does not.
		if err := tx.MarkHandoffSeedObserved(ctx, rec.RunID, observedBase); err != nil {
			return err
		}
		if err := tx.MarkHandoffCredentialObserved(ctx, rec.RunID, preDigest); err != nil {
			return err
		}
		if err := tx.MarkHandoffWriterComplete(ctx, rec.RunID); err != nil {
			return err
		}
		if err := tx.MarkHandoffExportMaterialized(ctx, rec.RunID, exportDir); err != nil {
			return err
		}
		recoveredExportDir := filepath.Join(t.TempDir(), "recovered-export")
		if err := tx.MarkHandoffExportMaterialized(ctx, rec.RunID, recoveredExportDir); err != nil {
			return err
		}
		exportDir = recoveredExportDir
		return tx.CloseHandoffJournal(ctx, rec.RunID, store.HandoffCompleted)
	}); err != nil {
		t.Fatalf("amend and close: %v", err)
	}

	var got store.HandoffJournalRecord
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		got, err = tx.GetHandoffJournal(ctx, rec.RunID)
		return err
	}); err != nil {
		t.Fatalf("GetHandoffJournal: %v", err)
	}
	if got.Lease == nil || got.Lease.Fence != lease.Fence ||
		got.ObservedBaseSHA != observedBase || got.CredentialPreDigest != preDigest ||
		!got.WriterComplete || got.ExportDir != exportDir ||
		got.Outcome == nil || *got.Outcome != store.HandoffCompleted {
		t.Fatalf("record = %+v, want the fully amended completed record", got)
	}

	err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.CloseHandoffJournal(ctx, rec.RunID, store.HandoffCompleted)
	})
	if !errors.Is(err, store.ErrHandoffJournalClosed) {
		t.Fatalf("second close = %v, want %v", err, store.ErrHandoffJournalClosed)
	}
}

func TestBeginLeasedHandoffRollsBackBothSidesOnJournalConflict(t *testing.T) {
	ctx := context.Background()
	s := openWithIdentity(t, testAuthIdentity())
	rec := journalRecord("conflicting-run")
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.BeginHandoffJournal(ctx, rec)
	}); err != nil {
		t.Fatalf("seed journal: %v", err)
	}

	err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.BeginLeasedHandoffJournal(
			ctx, rec, "auth-1", "inv-1", leaseEpoch, leaseEpoch.Add(time.Hour),
		)
		return err
	})
	if !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("conflicting leased begin = %v, want %v", err, store.ErrImmutableConflict)
	}
	err = s.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetAuthStoreMutationLease(ctx, "auth-1")
		return err
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("lease after rolled-back journal conflict = %v, want not found", err)
	}
}

func TestBeginLeasedHandoffRefusesConvergedSameHolderLease(t *testing.T) {
	ctx := context.Background()
	s := openWithIdentity(t, testAuthIdentity())
	oldAcquiredAt := leaseEpoch
	oldExpiresAt := leaseEpoch.Add(time.Hour)
	var oldLease domain.AuthStoreMutationLease
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		oldLease, err = tx.AcquireAuthStoreMutationLease(
			ctx, "auth-1", "inv-1", oldAcquiredAt, oldExpiresAt,
		)
		return err
	}); err != nil {
		t.Fatalf("seed lease: %v", err)
	}

	rec := journalRecord("converged-lease-run")
	newAcquiredAt := oldAcquiredAt.Add(time.Minute)
	err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.BeginLeasedHandoffJournal(
			ctx, rec, "auth-1", "inv-1", newAcquiredAt, newAcquiredAt.Add(time.Hour),
		)
		return err
	})
	if err == nil {
		t.Fatal("BeginLeasedHandoffJournal accepted a converged same-holder lease")
	}
	err = s.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetHandoffJournal(ctx, rec.RunID)
		return err
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("journal after converged lease = %v, want not found", err)
	}
	var gotLease domain.AuthStoreMutationLease
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		gotLease, err = tx.GetAuthStoreMutationLease(ctx, "auth-1")
		return err
	}); err != nil {
		t.Fatalf("read original lease: %v", err)
	}
	if gotLease != oldLease {
		t.Fatalf("lease after refused convergence = %+v, want unchanged %+v", gotLease, oldLease)
	}
}

func TestUnleasedJournalBeginCannotForgeALeaseReference(t *testing.T) {
	ctx := context.Background()
	s := openWithIdentity(t, testAuthIdentity())
	rec := journalRecord("forged-lease-run")
	rec.Lease = &store.HandoffJournalLease{
		AuthIdentityID: "auth-1",
		Holder:         "inv-1",
		Fence:          1,
		AcquiredAt:     leaseEpoch,
		ExpiresAt:      leaseEpoch.Add(time.Hour),
	}
	err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.BeginHandoffJournal(ctx, rec)
	})
	if err == nil {
		t.Fatal("BeginHandoffJournal accepted a caller-supplied lease reference")
	}
	err = s.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetHandoffJournal(ctx, rec.RunID)
		return err
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("journal after forged leased begin = %v, want not found", err)
	}
}

func TestJournalBeginRejectsCallerSuppliedProgressBeforeLeaseAcquisition(t *testing.T) {
	ctx := context.Background()
	s := openWithIdentity(t, testAuthIdentity())
	rec := journalRecord("forged-progress-run")
	rec.WriterComplete = true

	err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.BeginLeasedHandoffJournal(
			ctx, rec, "auth-1", "inv-1", leaseEpoch, leaseEpoch.Add(time.Hour),
		)
		return err
	})
	if err == nil {
		t.Fatal("BeginLeasedHandoffJournal accepted caller-supplied progress")
	}
	err = s.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetHandoffJournal(ctx, rec.RunID)
		return err
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("journal after forged progress = %v, want not found", err)
	}
	err = s.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetAuthStoreMutationLease(ctx, "auth-1")
		return err
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("lease after forged progress = %v, want not found", err)
	}
}

func TestHandoffJournalProofAmendmentsAreImmutable(t *testing.T) {
	ctx := context.Background()
	s := openWithIdentity(t, testAuthIdentity())
	rec := journalRecord("proof-run")
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.BeginHandoffJournal(ctx, rec)
	}); err != nil {
		t.Fatalf("BeginHandoffJournal: %v", err)
	}
	err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.MarkHandoffSeedObserved(ctx, rec.RunID, strings.Repeat("12", 20)); err != nil {
			return err
		}
		return tx.MarkHandoffSeedObserved(ctx, rec.RunID, strings.Repeat("34", 20))
	})
	if !errors.Is(err, store.ErrHandoffJournalProofConflict) {
		t.Fatalf("rewritten proof = %v, want %v", err, store.ErrHandoffJournalProofConflict)
	}
}
