package wardstore_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
	"github.com/freeside-ai/freeside/daemon/internal/wardstore"
)

func TestAdaptersRoundTripAcrossStoreReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "freeside.db")
	at := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	st, err := store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	identity := domain.AuthIdentity{
		ID: "auth-1", Provider: "claude", AuthStoreMutationLease: true,
		AuthStoreVolume:       "provider-cred",
		MaxParallelExecutions: 1, RefreshStrategy: domain.RefreshOnDemand,
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordAuthIdentity(ctx, identity, at)
	}); err != nil {
		t.Fatalf("RecordAuthIdentity: %v", err)
	}
	adapters, err := wardstore.New(st)
	if err != nil {
		t.Fatalf("wardstore.New: %v", err)
	}
	rec := ward.HandoffJournalRecord{
		RunID:          "adapter-run",
		OwnershipToken: "00112233445566778899aabbccddeeff",
		SpecDigest:     strings.Repeat("ab", 32),
		OpenedAt:       at,
	}
	lease, err := adapters.Journal.BeginLeased(
		ctx, rec,
		ward.AuthStoreLeaseClaim{AuthIdentityID: identity.ID, Holder: "inv-1"},
		at, at.Add(time.Hour),
	)
	if err != nil {
		t.Fatalf("BeginLeased: %v", err)
	}
	if lease.Fence != 1 {
		t.Fatalf("lease fence = %d, want 1", lease.Fence)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	reopened, err := store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	adapters, err = wardstore.New(reopened)
	if err != nil {
		t.Fatalf("wardstore.New(reopened): %v", err)
	}
	if volume, err := adapters.Leaser.AuthStoreVolume(ctx, identity.ID); err != nil || volume != identity.AuthStoreVolume {
		t.Fatalf("AuthStoreVolume = %q, %v; want %q", volume, err, identity.AuthStoreVolume)
	}
	got, err := adapters.Journal.Get(ctx, rec.RunID)
	if err != nil {
		t.Fatalf("Journal.Get after reopen: %v", err)
	}
	if got.Lease == nil || got.Lease.Fence != lease.Fence ||
		got.Lease.AuthIdentityID != identity.ID || got.Outcome != nil {
		t.Fatalf("reopened record = %+v, want the same open leased record", got)
	}
	current, err := adapters.Leaser.Get(ctx, identity.ID)
	if err != nil {
		t.Fatalf("Leaser.Get after reopen: %v", err)
	}
	if current != lease {
		t.Fatalf("reopened lease = %+v, want %+v", current, lease)
	}

	base := strings.Repeat("12", 20)
	pre := strings.Repeat("cd", 32)
	exportDir := filepath.Join(t.TempDir(), "export")
	if err := adapters.Journal.MarkSeedObserved(ctx, rec.RunID, base); err != nil {
		t.Fatalf("MarkSeedObserved: %v", err)
	}
	if err := adapters.Journal.MarkCredentialObserved(ctx, rec.RunID, pre); err != nil {
		t.Fatalf("MarkCredentialObserved: %v", err)
	}
	if err := adapters.Journal.MarkWriterComplete(ctx, rec.RunID); err != nil {
		t.Fatalf("MarkWriterComplete: %v", err)
	}
	if err := adapters.Journal.MarkExportMaterialized(ctx, rec.RunID, exportDir); err != nil {
		t.Fatalf("MarkExportMaterialized: %v", err)
	}
	if err := adapters.Journal.Close(ctx, rec.RunID, ward.HandoffCompleted); err != nil {
		t.Fatalf("Close: %v", err)
	}
	closed, err := adapters.Journal.Get(ctx, rec.RunID)
	if err != nil {
		t.Fatalf("Get closed: %v", err)
	}
	if closed.Outcome == nil || *closed.Outcome != ward.HandoffCompleted {
		t.Fatalf("closed outcome = %v, want completed", closed.Outcome)
	}
}

func TestJournalMapsMissingRecordToWardSentinel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	adapters, err := wardstore.New(st)
	if err != nil {
		t.Fatalf("wardstore.New: %v", err)
	}
	_, err = adapters.Journal.Get(ctx, "missing-run")
	if !errors.Is(err, ward.ErrJournalRecordNotFound) {
		t.Fatalf("Journal.Get missing = %v, want ErrJournalRecordNotFound", err)
	}
}

func TestLeaserMapsEndedWindowForWardConvergence(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	at := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	identity := domain.AuthIdentity{
		ID: "auth-1", Provider: "claude", AuthStoreMutationLease: true,
		AuthStoreVolume:       "provider-cred",
		MaxParallelExecutions: 1, RefreshStrategy: domain.RefreshOnDemand,
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordAuthIdentity(ctx, identity, at)
	}); err != nil {
		t.Fatalf("RecordAuthIdentity: %v", err)
	}
	adapters, err := wardstore.New(st)
	if err != nil {
		t.Fatalf("wardstore.New: %v", err)
	}
	lease, err := adapters.Leaser.Acquire(ctx, identity.ID, "inv-1", at, at.Add(time.Minute))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	err = adapters.Leaser.Release(
		ctx, identity.ID, lease.Holder, lease.Fence, lease.ExpiresAt.Add(time.Second),
	)
	if !errors.Is(err, ward.ErrLeaseWindowEnded) {
		t.Fatalf("late release = %v, want %v", err, ward.ErrLeaseWindowEnded)
	}
}

func TestNewRejectsNilStore(t *testing.T) {
	if _, err := wardstore.New(nil); err == nil {
		t.Fatal("wardstore.New(nil) succeeded")
	}
}
