package operations_test

import (
	"context"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/operations"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

type mutableBackupHealth struct{ health domain.BackupHealth }

func (s *mutableBackupHealth) BackupHealth(
	context.Context, store.BackupHealthContext,
) (domain.BackupHealth, error) {
	return s.health, nil
}

func TestDoctorConvergesAndClearsSystemHealthItems(t *testing.T) {
	ctx := context.Background()
	source := &mutableBackupHealth{health: domain.BackupHealth{
		Encryption: domain.BackupHealthUnhealthy, CheckpointCurrency: domain.BackupHealthUnhealthy,
		ArtifactClosure: domain.BackupHealthUnhealthy, RestoreTestAge: domain.BackupHealthUnhealthy,
	}}
	st, err := store.Open(ctx, t.TempDir()+"/freeside.db", store.Options{BackupHealthSource: source})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() //nolint:errcheck // Test cleanup cannot affect the assertion.
	attention := signet.NewService(st)
	doctor := operations.Doctor{
		Store: st, Attention: attention, ProjectID: "project-system",
		Backend: domain.BackendFreshVMReadOnlyVolumeHandoff, Mode: domain.ModeAttendedDev,
		ConfigurationDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	first, err := doctor.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.Healthy {
		t.Fatal("doctor reported unhealthy fixture healthy")
	}
	if _, err := doctor.Run(ctx); err != nil {
		t.Fatal(err)
	}
	assertOpenHealthItems(t, ctx, attention, 6)

	source.health = domain.BackupHealth{
		Encryption: domain.BackupHealthHealthy, CheckpointCurrency: domain.BackupHealthHealthy,
		ArtifactClosure: domain.BackupHealthHealthy, RestoreTestAge: domain.BackupHealthHealthy,
	}
	record, err := domain.NewBackendConformance(domain.BackendConformanceInput{
		Backend: domain.BackendFreshVMReadOnlyVolumeHandoff, Outcome: domain.ConformancePassed,
		ConfigurationDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Capabilities: domain.NewCapabilitySnapshot(
			domain.CapDetachableWorkspace,
			domain.CapPostExitExport,
			domain.CapReadOnlyRemount,
		),
		ProvedAt: time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.RecordBackendConformance(ctx, record)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	cleared, err := doctor.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !cleared.Healthy {
		t.Fatalf("cleared report = %+v", cleared)
	}
	assertOpenHealthItems(t, ctx, attention, 0)
	doctor.Mode = domain.ModeUnattended
	unattended, err := doctor.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if unattended.OperatingMode != domain.ModeUnattended {
		t.Fatalf("reported mode = %q, want unattended", unattended.OperatingMode)
	}
	if unattended.Healthy {
		t.Fatal("unattended doctor accepted the attended capability floor")
	}
	assertOpenHealthItems(t, ctx, attention, 2)
	unattendedRecord, err := domain.NewBackendConformance(domain.BackendConformanceInput{
		Backend: domain.BackendFreshVMReadOnlyVolumeHandoff, Outcome: domain.ConformancePassed,
		ConfigurationDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Capabilities: domain.NewCapabilitySnapshot(
			domain.CapDetachableWorkspace,
			domain.CapPostExitExport,
			domain.CapReadOnlyRemount,
			domain.CapNetworklessExport,
			domain.CapEnforcedProviderEgress,
		),
		ProvedAt: time.Date(2026, 7, 31, 12, 1, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.RecordBackendConformance(ctx, unattendedRecord)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	unattended, err = doctor.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !unattended.Healthy {
		t.Fatalf("unattended report with full capability floor = %+v", unattended)
	}
	assertOpenHealthItems(t, ctx, attention, 0)
	doctor.ConfigurationDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	stale, err := doctor.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stale.Healthy {
		t.Fatal("doctor accepted conformance for a stale backend configuration")
	}
	assertOpenHealthItems(t, ctx, attention, 2)
}

func assertOpenHealthItems(
	t *testing.T, ctx context.Context, attention *signet.Service, want int,
) {
	t.Helper()
	items, err := attention.ListAttentionItems(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := 0
	for _, snapshot := range items {
		if snapshot.Item.Type == domain.AttentionSystemHealth &&
			snapshot.Item.Status == domain.StatusOpen {
			got++
		}
	}
	if got != want {
		t.Fatalf("open system_health items = %d, want %d", got, want)
	}
}
