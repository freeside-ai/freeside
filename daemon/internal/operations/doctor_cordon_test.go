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

// TestDoctorReportsEachHostsCordonedProof is #2165's acceptance 6: one
// cordoned_conformance finding per host, each naming the host, its newest
// record's outcome, generation, configuration, and time, and its
// conditions. The daemon's host must hold its pass for the running
// configuration and is left out when the daemon runs no cordon.
func TestDoctorReportsEachHostsCordonedProof(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t, t.TempDir()+"/freeside.db", store.Options{BackupHealthSource: &mutableBackupHealth{health: domain.BackupHealth{
		Encryption: domain.BackupHealthHealthy, CheckpointCurrency: domain.BackupHealthHealthy,
		ArtifactClosure: domain.BackupHealthHealthy, RestoreTestAge: domain.BackupHealthHealthy,
	}}})
	const (
		hostB   = domain.ExecutionHostID("host-b")
		running = domain.Digest("sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
		older   = domain.Digest("sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd")
	)
	at := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	account := "runner"
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.AddExecutionHost(ctx, domain.ExecutionHost{
			ID: hostB, Name: "studio", RunAccount: domain.HostRunAccount{Name: &account},
			Reach:      &domain.HostReach{Address: "studio.local", HostKey: "ssh-ed25519 AAAAstudio"},
			Conditions: []domain.HostObservedCondition{},
		}); err != nil {
			return err
		}
		_, err := tx.SetExecutionHostCondition(ctx, hostB, domain.ExecutionHostProofNotCurrent, at)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	record := func(host domain.ExecutionHostID, outcome domain.ConformanceOutcome, digest domain.Digest) {
		t.Helper()
		in := domain.BackendConformanceInput{
			Backend: domain.BackendCordonedHostProcess, Outcome: outcome, ConfigurationDigest: digest, ProvedAt: at,
		}
		if outcome == domain.ConformancePassed {
			in.Capabilities = domain.CordonedCapabilities()
		}
		r, err := domain.NewBackendConformance(in)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
			_, err := tx.RecordBackendConformance(ctx, host, r)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	record(domain.DaemonHostID, domain.ConformancePassed, older)
	record(hostB, domain.ConformanceFailed, running)

	doctor := operations.Doctor{
		Store: st, Attention: signet.NewService(st), ProjectID: "project-system",
		Backend: domain.BackendFreshVMReadOnlyVolumeHandoff, Mode: domain.ModeAttendedDev,
		CordonConfigurationDigest: running,
	}
	cordoned := func() []operations.DoctorFinding {
		t.Helper()
		report, err := doctor.Run(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var out []operations.DoctorFinding
		for _, f := range report.Findings {
			if f.Code == "cordoned_conformance" {
				out = append(out, f)
			}
		}
		return out
	}
	check := func(f operations.DoctorFinding, healthy bool, parts ...string) {
		t.Helper()
		if f.Healthy != healthy {
			t.Errorf("finding %q healthy = %t, want %t", f.Detail, f.Healthy, healthy)
		}
		for _, part := range parts {
			if !strings.Contains(f.Detail, part) {
				t.Errorf("finding %q doesn't name %q", f.Detail, part)
			}
		}
	}

	findings := cordoned()
	if len(findings) != 2 {
		t.Fatalf("cordoned findings = %+v, want one per host", findings)
	}
	check(findings[0], false, "host daemon:", "generation 1 is passed for configuration "+string(older)+" at 2026-11-26T09:00:00Z",
		"not the running configuration "+string(running), "conditions: none")
	check(findings[1], false, "host host-b:", "generation 2 is failed for configuration "+string(running),
		"conditions: proof_not_current")

	record(domain.DaemonHostID, domain.ConformancePassed, running)
	record(hostB, domain.ConformancePassed, running)
	findings = cordoned()
	check(findings[0], true, "host daemon:", "generation 3 is passed")
	// A pass with the condition still set isn't healthy: the runner clears
	// it when it settles the host.
	check(findings[1], false, "host host-b:", "generation 4 is passed", "conditions: proof_not_current")

	doctor.CordonConfigurationDigest = ""
	findings = cordoned()
	if len(findings) != 1 {
		t.Fatalf("with no cordon on the daemon's host, cordoned findings = %+v, want host B's alone", findings)
	}
	check(findings[0], false, "host host-b:")
}
