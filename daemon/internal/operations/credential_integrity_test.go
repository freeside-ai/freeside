package operations_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/operations"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
	"github.com/freeside-ai/freeside/daemon/internal/wardstore"
)

const (
	integrityIdentity   = domain.AuthIdentityID("claude-main")
	integrityEnrollment = domain.ClientEnrollmentID("claude-main/claude_code")
	// integrityAccount is the identity's account binding. No item or report
	// may carry it; the label keeps its last four characters only.
	integrityAccount = "operator@example.test"
	integrityLabel   = "****test"
)

var integrityAt = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

// integrityDoctor is a doctor whose other findings are all healthy, over a
// store holding one Claude enrollment at generation one, so every
// system_health item a test sees comes from credential integrity.
type integrityDoctor struct {
	t         *testing.T
	st        *store.Store
	attention *signet.Service
	doctor    operations.Doctor
}

func newIntegrityDoctor(t *testing.T) *integrityDoctor {
	t.Helper()
	ctx := context.Background()
	healthy := &mutableBackupHealth{health: domain.BackupHealth{
		Encryption: domain.BackupHealthHealthy, CheckpointCurrency: domain.BackupHealthHealthy,
		ArtifactClosure: domain.BackupHealthHealthy, RestoreTestAge: domain.BackupHealthHealthy,
	}}
	st, err := store.Open(ctx, t.TempDir()+"/freeside.db", store.Options{BackupHealthSource: healthy})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	const configuration = domain.Digest("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	record, err := domain.NewBackendConformance(domain.BackendConformanceInput{
		Backend: domain.BackendFreshVMReadOnlyVolumeHandoff, Outcome: domain.ConformancePassed,
		ConfigurationDigest: configuration,
		Capabilities: domain.NewCapabilitySnapshot(
			domain.CapDetachableWorkspace, domain.CapPostExitExport, domain.CapReadOnlyRemount,
		),
		ProvedAt: integrityAt,
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
	adapters, err := wardstore.New(st)
	if err != nil {
		t.Fatal(err)
	}
	digest := domain.Digest(contentaddr.Sum([]byte("token")))
	identity := domain.AuthIdentity{
		ID: integrityIdentity, Provider: "claude", AccountBinding: integrityAccount,
		AuthStoreMutationLease: true, MaxParallelExecutions: 1, Enabled: true, CostOwner: "operator",
		Interim: domain.InterimClientFacts{
			AuthStoreVolume: "claude-main-auth", RefreshStrategy: domain.RefreshOnDemand,
		},
	}
	bootstrap := ward.EnrollmentBootstrap{
		Enrollment: domain.ClientEnrollment{
			ID: integrityEnrollment, AuthIdentityID: integrityIdentity,
			HarnessClient: domain.HarnessClientClaudeCode,
			Route:         "anthropic-subscription", AuthMethod: domain.AuthMethodSetupToken,
			CredentialMode:  domain.CredentialSubscriptionContained,
			RefreshStrategy: domain.RefreshExternal, SupportsReadOnlyAuthSnapshot: true,
			AccountBinding: integrityAccount,
		},
		Binding: domain.LeaseGenerationBinding{
			EnrollmentID: integrityEnrollment, AuthStoreVolume: "claude-main-auth",
			StoreManifestDigest: digest,
		},
	}
	lease, err := adapters.Claude.Begin(ctx, identity, bootstrap, "enroll", integrityAt, integrityAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapters.Claude.AppendGeneration(ctx, domain.EnrollmentGeneration{
		EnrollmentID: integrityEnrollment, AuthStoreVolume: "claude-main-auth",
		StoreManifestDigest: digest, LeaseFence: lease.Fence,
		AccountBinding: integrityAccount, RecordedAt: integrityAt,
	}, integrityAt); err != nil {
		t.Fatal(err)
	}
	if err := adapters.Leaser.Release(ctx, integrityIdentity, "enroll", lease.Fence, integrityAt); err != nil {
		t.Fatal(err)
	}
	attention := signet.NewService(st)
	return &integrityDoctor{t: t, st: st, attention: attention, doctor: operations.Doctor{
		Store: st, Attention: attention, ProjectID: "project-system",
		Backend: domain.BackendFreshVMReadOnlyVolumeHandoff, Mode: domain.ModeAttendedDev,
		ConfigurationDigest: configuration,
		Now:                 func() time.Time { return integrityAt.Add(time.Hour) },
		IdentityLabel: func(identity domain.AuthIdentity) string {
			if identity.AccountBinding != integrityAccount {
				t.Errorf("label asked for binding %q", identity.AccountBinding)
			}
			return integrityLabel
		},
	}}
}

func (d *integrityDoctor) run() operations.DoctorReport {
	d.t.Helper()
	report, err := d.doctor.Run(context.Background())
	if err != nil {
		d.t.Fatal(err)
	}
	return report
}

func (d *integrityDoctor) mark(ordinal int, finding domain.CredentialIntegrityFinding) {
	d.t.Helper()
	ctx := context.Background()
	if err := d.st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.RecordGenerationIntegrityMark(ctx, domain.GenerationIntegrityMark{
			EnrollmentID: integrityEnrollment, Ordinal: ordinal,
			Finding: finding, ObservedAt: integrityAt.Add(30 * time.Minute),
		})
		return err
	}); err != nil {
		d.t.Fatal(err)
	}
}

// reEnroll appends the enrollment's next generation.
func (d *integrityDoctor) reEnroll() {
	d.t.Helper()
	ctx := context.Background()
	at := integrityAt.Add(45 * time.Minute)
	if err := d.st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		current, err := tx.CurrentEnrollmentGeneration(ctx, integrityEnrollment)
		if err != nil {
			return err
		}
		lease, err := tx.AcquireAuthStoreMutationLeaseBound(ctx, integrityIdentity, "re-enroll",
			&domain.LeaseGenerationBinding{
				EnrollmentID: integrityEnrollment, AuthStoreVolume: current.AuthStoreVolume,
				StoreManifestDigest: current.StoreManifestDigest, Generation: current.Ordinal,
			}, at, at.Add(time.Minute))
		if err != nil {
			return err
		}
		if _, err := tx.AppendEnrollmentGeneration(ctx, domain.EnrollmentGeneration{
			EnrollmentID: integrityEnrollment, AuthStoreVolume: current.AuthStoreVolume,
			StoreManifestDigest: domain.Digest(contentaddr.Sum([]byte("new token"))),
			LeaseFence:          lease.Fence, AccountBinding: current.AccountBinding, RecordedAt: at,
		}, at); err != nil {
			return err
		}
		return tx.ReleaseAuthStoreMutationLease(ctx, integrityIdentity, "re-enroll", lease.Fence, at)
	}); err != nil {
		d.t.Fatal(err)
	}
}

// healthItems returns every system_health item, in any status.
func (d *integrityDoctor) healthItems() []domain.AttentionItem {
	d.t.Helper()
	snapshots, err := d.attention.ListAttentionItems(context.Background())
	if err != nil {
		d.t.Fatal(err)
	}
	var items []domain.AttentionItem
	for _, snapshot := range snapshots {
		if snapshot.Item.Type == domain.AttentionSystemHealth {
			items = append(items, snapshot.Item)
		}
	}
	return items
}

func (d *integrityDoctor) revision() int64 {
	d.t.Helper()
	ctx := context.Background()
	var revision int64
	if err := d.st.Read(ctx, func(tx *store.ReadTx) error {
		state, err := tx.ServerState(ctx)
		revision = state.Revision
		return err
	}); err != nil {
		d.t.Fatal(err)
	}
	return revision
}

// TestDoctorReportsRecordedCredentialMarks: with no probe wired the finding
// reports what earlier passes recorded. A marked current generation makes
// the report unhealthy and gets one advisory item, never the blocking item
// converge files for the other findings, and a repeat pass changes nothing.
func TestDoctorReportsRecordedCredentialMarks(t *testing.T) {
	d := newIntegrityDoctor(t)

	clean := d.run()
	finding := doctorFinding(t, clean, "credential_integrity")
	if !finding.Healthy || !clean.Healthy || !strings.Contains(finding.Detail, "recorded marks only") {
		t.Fatalf("unmarked report = %+v, want a healthy recorded-marks finding", clean)
	}
	if items := d.healthItems(); len(items) != 0 {
		t.Fatalf("an unmarked store filed items: %+v", items)
	}

	d.mark(1, domain.CredentialIntegrityCorruption)
	marked := d.run()
	finding = doctorFinding(t, marked, "credential_integrity")
	if finding.Healthy || marked.Healthy {
		t.Fatalf("marked report = %+v, want unhealthy", marked)
	}
	if !strings.Contains(finding.Detail, "claude-main/claude_code generation 1 (corruption)") {
		t.Errorf("detail %q does not name the marked generation and its finding", finding.Detail)
	}
	items := d.healthItems()
	if len(items) != 1 {
		t.Fatalf("items = %+v, want exactly one", items)
	}
	item := items[0]
	if item.Status != domain.StatusOpen || item.Posture == nil || *item.Posture != domain.HealthPostureAdvisory {
		t.Errorf("item status %s posture %v, want an open advisory item", item.Status, item.Posture)
	}
	if item.HealthDiagnostic == nil || item.HealthDiagnostic.Code != "credential_integrity" ||
		item.HealthDiagnostic.Impairs != domain.ImpairedCapabilityAgentCredential {
		t.Errorf("diagnostic = %+v, want credential_integrity impairing agent_credential", item.HealthDiagnostic)
	}
	if strings.HasPrefix(string(item.ID), "system-health-doctor-") {
		t.Errorf("item id %q carries the blocking doctor prefix", item.ID)
	}
	for _, want := range []string{"claude-main (" + integrityLabel + ")", string(integrityEnrollment), "generation 1", "corruption"} {
		if !strings.Contains(item.Reason, want) {
			t.Errorf("reason %q does not name %q", item.Reason, want)
		}
	}
	if strings.Contains(item.Reason, integrityAccount) || strings.Contains(finding.Detail, integrityAccount) {
		t.Error("the account binding reached the item or the report")
	}

	before := d.revision()
	d.run()
	if again := d.healthItems(); len(again) != 1 || again[0].ItemVersion != item.ItemVersion {
		t.Errorf("a repeat pass changed the items: %+v", again)
	}
	if after := d.revision(); after != before {
		t.Errorf("a repeat pass with nothing to change advanced the revision from %d to %d", before, after)
	}

	// A second finding on the same generation updates the one item.
	d.mark(1, domain.CredentialIntegrityTruncation)
	d.run()
	updated := d.healthItems()
	if len(updated) != 1 || updated[0].ItemVersion != item.ItemVersion+1 ||
		!strings.Contains(updated[0].Reason, "corruption, truncation") {
		t.Errorf("items after a second finding = %+v, want the one item naming both", updated)
	}
}

// TestDoctorResolvesCredentialItemWhenGenerationMovesOn: re-enrollment
// appends a generation, so the marked one is no longer current. The next
// pass resolves its item and the finding is healthy again.
func TestDoctorResolvesCredentialItemWhenGenerationMovesOn(t *testing.T) {
	d := newIntegrityDoctor(t)
	d.mark(1, domain.CredentialIntegrityTruncation)
	d.run()
	d.reEnroll()

	report := d.run()
	if finding := doctorFinding(t, report, "credential_integrity"); !finding.Healthy || !report.Healthy {
		t.Errorf("report after re-enrollment = %+v, want healthy", report)
	}
	items := d.healthItems()
	if len(items) != 1 || items[0].Status != domain.StatusResolved {
		t.Fatalf("items after re-enrollment = %+v, want the one item resolved", items)
	}

	// A mark on the new generation is a new item, not the old one reopened.
	d.mark(2, domain.CredentialIntegrityCorruption)
	d.run()
	open := 0
	for _, item := range d.healthItems() {
		if item.Status == domain.StatusOpen {
			open++
			if !strings.Contains(item.Reason, "generation 2") {
				t.Errorf("open item reason %q, want generation 2", item.Reason)
			}
		}
	}
	if all := d.healthItems(); len(all) != 2 || open != 1 {
		t.Errorf("items = %+v, want generation 1 resolved and generation 2 open", all)
	}
}

// TestDoctorNeverRefilesAHandledCredentialItem: once the operator has
// handled a marked generation's item, later passes leave it alone while the
// finding stays unhealthy.
func TestDoctorNeverRefilesAHandledCredentialItem(t *testing.T) {
	d := newIntegrityDoctor(t)
	d.mark(1, domain.CredentialIntegrityCorruption)
	d.run()
	handled := d.healthItems()[0]
	handled.ItemVersion++
	handled.Status = domain.StatusResolved
	if err := d.attention.PutItem(context.Background(), handled); err != nil {
		t.Fatal(err)
	}

	report := d.run()
	if doctorFinding(t, report, "credential_integrity").Healthy {
		t.Error("handling the item cleared the finding")
	}
	items := d.healthItems()
	if len(items) != 1 || items[0].Status != domain.StatusResolved || items[0].ItemVersion != handled.ItemVersion {
		t.Errorf("items after a later pass = %+v, want the handled item untouched", items)
	}
}

// TestDoctorRunsTheLiveProbeBeforeReadingMarks: a wired probe runs first, so
// a mark it records is reported by the same pass, and its not-checked,
// corruption-unconfirmed, and corruption-not-run outcomes reach the detail as
// ids and codes.
func TestDoctorRunsTheLiveProbeBeforeReadingMarks(t *testing.T) {
	d := newIntegrityDoctor(t)
	d.doctor.CredentialIntegrityProbe = func(context.Context) ([]operations.CredentialIntegrityOutcome, error) {
		d.mark(1, domain.CredentialIntegrityTruncation)
		return []operations.CredentialIntegrityOutcome{
			{EnrollmentID: integrityEnrollment, CorruptionChecked: true},
			{EnrollmentID: "codex-main/codex_cli"},
			{EnrollmentID: "claude-busy/claude_code", NotChecked: "mutation_lease_live"},
			{EnrollmentID: "claude-adopted/claude_code", CorruptionUnconfirmed: true},
		}, nil
	}
	finding := doctorFinding(t, d.run(), "credential_integrity")
	if finding.Healthy {
		t.Fatal("the pass did not report the mark its own probe recorded")
	}
	for _, want := range []string{
		"probed 4 enrollments",
		"marked: claude-main/claude_code generation 1 (truncation)",
		"not checked: claude-busy/claude_code (mutation_lease_live)",
		"corruption finding not reproduced, no mark recorded: claude-adopted/claude_code",
		"corruption check not run on a store the daemon refreshes: codex-main/codex_cli",
	} {
		if !strings.Contains(finding.Detail, want) {
			t.Errorf("detail %q does not carry %q", finding.Detail, want)
		}
	}
	if strings.Contains(finding.Detail, "recorded marks only") {
		t.Errorf("detail %q claims no probe ran", finding.Detail)
	}
}

// TestDoctorFailsWhenTheProbeFails: a probe that cannot run is an error, not
// a clean finding.
func TestDoctorFailsWhenTheProbeFails(t *testing.T) {
	d := newIntegrityDoctor(t)
	cause := errors.New("list identities")
	d.doctor.CredentialIntegrityProbe = func(context.Context) ([]operations.CredentialIntegrityOutcome, error) {
		return nil, cause
	}
	if _, err := d.doctor.Run(context.Background()); !errors.Is(err, cause) {
		t.Errorf("run = %v, want the probe's error", err)
	}
}
