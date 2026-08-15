package operations_test

import (
	"context"
	"database/sql"
	"strings"
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
		ConfigurationDigest:       "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ReviewConfigurationDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
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

func TestDoctorReportsActivatedReviewConfigurationDrift(t *testing.T) {
	ctx := context.Background()
	source := &mutableBackupHealth{health: domain.BackupHealth{
		Encryption: domain.BackupHealthHealthy, CheckpointCurrency: domain.BackupHealthHealthy,
		ArtifactClosure: domain.BackupHealthHealthy, RestoreTestAge: domain.BackupHealthHealthy,
	}}
	dbPath := t.TempDir() + "/freeside.db"
	st, err := store.Open(ctx, dbPath, store.Options{BackupHealthSource: source})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	attention := signet.NewService(st)
	backendDigest := domain.Digest("sha256:" + strings.Repeat("a", 64))
	effective := domain.Digest("sha256:" + strings.Repeat("b", 64))
	mismatched := domain.Digest("sha256:" + strings.Repeat("c", 64))
	conformance, err := domain.NewBackendConformance(domain.BackendConformanceInput{
		Backend: domain.BackendFreshVMReadOnlyVolumeHandoff, Outcome: domain.ConformancePassed,
		ConfigurationDigest: backendDigest,
		Capabilities: domain.NewCapabilitySnapshot(
			domain.CapDetachableWorkspace,
			domain.CapPostExitExport,
			domain.CapReadOnlyRemount,
			domain.CapNetworklessExport,
			domain.CapEnforcedProviderEgress,
		),
		ProvedAt: time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	matching := doctorTrustProfile(t, "example/matching", 1, effective)
	drifted := doctorTrustProfile(t, "example/drifted", 2, mismatched)
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if _, err := tx.RecordBackendConformance(ctx, conformance); err != nil {
			return err
		}
		if err := tx.RecordTrustProfile(ctx, matching, conformance.ProvedAt); err != nil {
			return err
		}
		return tx.RecordTrustProfile(ctx, drifted, conformance.ProvedAt)
	}); err != nil {
		t.Fatal(err)
	}
	doctor := operations.Doctor{
		Store: st, Attention: attention, ProjectID: "project-system",
		Backend:             domain.BackendFreshVMReadOnlyVolumeHandoff,
		ConfigurationDigest: backendDigest, ReviewConfigurationDigest: effective,
		Mode: domain.ModeUnattended,
	}
	report, err := doctor.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	finding := doctorFinding(t, report, "review_configuration")
	if finding.Healthy || !strings.Contains(finding.Detail, drifted.Repo) ||
		!strings.Contains(finding.Detail, string(mismatched)) ||
		!strings.Contains(finding.Detail, string(effective)) {
		t.Fatalf("review configuration finding = %+v", finding)
	}
	assertOpenHealthItems(t, ctx, attention, 1)
	otherProjectDoctor := doctor
	otherProjectDoctor.ProjectID = "project-other"
	if err := otherProjectDoctor.ConvergeReviewConfiguration(ctx); err != nil {
		t.Fatalf("converge drifted configuration for another project: %v", err)
	}
	assertOpenHealthItems(t, ctx, attention, 2)

	recovered := doctorTrustProfile(t, drifted.Repo, drifted.RepositoryID, effective)
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordTrustProfile(ctx, recovered, conformance.ProvedAt.Add(time.Minute))
	}); err != nil {
		t.Fatal(err)
	}
	if err := doctor.ConvergeReviewConfiguration(ctx); err != nil {
		t.Fatalf("converge recovered review configuration: %v", err)
	}
	assertOpenHealthItems(t, ctx, attention, 0)

	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx,
		`UPDATE trust_profiles SET body = ? WHERE profile_digest = ?`,
		`{}`, recovered.ProfileDigest,
	); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	report, err = doctor.Run(ctx)
	if err != nil {
		t.Fatalf("doctor over unreadable current profile: %v", err)
	}
	finding = doctorFinding(t, report, "review_configuration")
	if finding.Healthy || !strings.Contains(finding.Detail, drifted.Repo) ||
		!strings.Contains(finding.Detail, "unreadable") {
		t.Fatalf("unreadable current profile finding = %+v", finding)
	}
	assertOpenHealthItems(t, ctx, attention, 1)

	reapproved := doctorTrustProfile(t, drifted.Repo, drifted.RepositoryID, effective, "Makefile")
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordTrustProfile(ctx, reapproved, conformance.ProvedAt.Add(2*time.Minute))
	}); err != nil {
		t.Fatal(err)
	}
	report, err = doctor.Run(ctx)
	if err != nil {
		t.Fatalf("doctor after reapproving unreadable current profile: %v", err)
	}
	if finding = doctorFinding(t, report, "review_configuration"); !finding.Healthy {
		t.Fatalf("reapproved unreadable profile finding = %+v", finding)
	}
	assertOpenHealthItems(t, ctx, attention, 0)

	doctor.Mode = domain.ModeAttendedDev
	doctor.ReviewConfigurationDigest = ""
	report, err = doctor.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	finding = doctorFinding(t, report, "review_configuration")
	if !finding.Healthy || !strings.Contains(finding.Detail, "not applicable") {
		t.Fatalf("attended review configuration finding = %+v", finding)
	}
}

func doctorTrustProfile(
	t *testing.T, repo string, repositoryID int64, reviewDigest domain.Digest,
	extraVerificationControlPatterns ...string,
) domain.AutomationTrustProfile {
	t.Helper()
	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo: repo, RepositoryID: repositoryID,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        "sha256:workflow-audit",
		Review: domain.ReviewSettings{
			Mode: domain.ReviewFreesideInvoked, ConfigDigest: reviewDigest,
		},
		ProtectedPaths: domain.ProtectedPathConfig{
			ExtraVerificationControlPatterns: extraVerificationControlPatterns,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func doctorFinding(t *testing.T, report operations.DoctorReport, code string) operations.DoctorFinding {
	t.Helper()
	for _, finding := range report.Findings {
		if finding.Code == code {
			return finding
		}
	}
	t.Fatalf("doctor report has no %q finding: %+v", code, report)
	return operations.DoctorFinding{}
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
			if snapshot.Item.CreatedAt == nil {
				t.Errorf("doctor item %s has nil created_at", snapshot.Item.ID)
			}
			if snapshot.Item.Posture == nil || *snapshot.Item.Posture != domain.HealthPostureBlocking {
				t.Errorf("doctor item %s posture = %v, want blocking",
					snapshot.Item.ID, snapshot.Item.Posture)
			}
			got++
		}
	}
	if got != want {
		t.Fatalf("open system_health items = %d, want %d", got, want)
	}
}
