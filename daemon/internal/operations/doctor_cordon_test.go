package operations_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/operations"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

func cordonedFinding(t *testing.T, report operations.DoctorReport) (operations.DoctorFinding, bool) {
	t.Helper()
	i := slices.IndexFunc(report.Findings, func(f operations.DoctorFinding) bool { return f.Code == "cordoned_conformance" })
	if i < 0 {
		return operations.DoctorFinding{}, false
	}
	return report.Findings[i], true
}

// TestDoctorReportsTheCordonedClassWithoutConverging shows the finding
// follows the newest record for the cordon's configuration and files no
// health item, whatever it says.
func TestDoctorReportsTheCordonedClassWithoutConverging(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t, t.TempDir()+"/freeside.db", store.Options{BackupHealthSource: &mutableBackupHealth{health: domain.BackupHealth{
		Encryption: domain.BackupHealthHealthy, CheckpointCurrency: domain.BackupHealthHealthy,
		ArtifactClosure: domain.BackupHealthHealthy, RestoreTestAge: domain.BackupHealthHealthy,
	}}})
	attention := signet.NewService(st)
	const digest = domain.Digest("sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
	doctor := operations.Doctor{
		Store: st, Attention: attention, ProjectID: "project-system",
		Backend: domain.BackendFreshVMReadOnlyVolumeHandoff, Mode: domain.ModeAttendedDev,
	}
	report, err := doctor.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cordonedFinding(t, report); ok {
		t.Fatal("a doctor with no cordon reports the cordoned class")
	}
	baseline := openHealthItems(t, ctx, attention)

	doctor.CordonConfigurationDigest = digest
	record := func(outcome domain.ConformanceOutcome, at time.Time) {
		t.Helper()
		in := domain.BackendConformanceInput{
			Backend: domain.BackendCordonedHostProcess, Outcome: outcome, ConfigurationDigest: digest, ProvedAt: at,
		}
		if outcome == domain.ConformancePassed {
			in.Capabilities = domain.NewCapabilitySnapshot(domain.CordonedCapabilities()...)
		}
		r, err := domain.NewBackendConformance(in)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
			_, err := tx.RecordBackendConformance(ctx, domain.DaemonHostID, r)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, step := range []struct {
		outcome domain.ConformanceOutcome // "" records nothing
		healthy bool
		detail  string
	}{
		{"", false, "no durable conformance result for configuration " + string(digest)},
		{domain.ConformancePassed, true, "is passed for configuration " + string(digest) + " at 2026-11-22T12:01:00Z"},
		{domain.ConformanceSuperseded, false, "is superseded"},
		{domain.ConformanceFailed, false, "is failed"},
	} {
		if step.outcome != "" {
			record(step.outcome, time.Date(2026, 11, 22, 12, 1, 0, 0, time.UTC))
		}
		report, err := doctor.Run(ctx)
		if err != nil {
			t.Fatal(err)
		}
		finding, ok := cordonedFinding(t, report)
		if !ok || finding.Healthy != step.healthy || !strings.Contains(finding.Detail, step.detail) ||
			!strings.HasPrefix(finding.Detail, string(domain.BackendCordonedHostProcess)+": ") {
			t.Fatalf("after %q the finding is %+v, want healthy %t naming %q", step.outcome, finding, step.healthy, step.detail)
		}
		if got := openHealthItems(t, ctx, attention); got != baseline {
			t.Fatalf("after %q the doctor holds %d health items, want %d: the finding converged", step.outcome, got, baseline)
		}
	}
}

func openHealthItems(t *testing.T, ctx context.Context, attention *signet.Service) int {
	t.Helper()
	items, err := attention.ListAttentionItems(ctx)
	if err != nil {
		t.Fatal(err)
	}
	open := 0
	for _, snapshot := range items {
		if snapshot.Item.Type == domain.AttentionSystemHealth && snapshot.Item.Status == domain.StatusOpen {
			open++
		}
	}
	return open
}
