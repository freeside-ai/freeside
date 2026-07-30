package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

var admissionEpoch = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// admissionFixture is the run, identity, and admission an execution-record
// test starts from: one run carrying one attempt, and the admission that
// attempt was recorded under.
type admissionFixture struct {
	run       domain.Run
	identity  domain.AuthIdentity
	admission domain.ExecutionAdmission
}

func newAdmissionFixture(t *testing.T, mutate func(*domain.ExecutionAdmissionInput)) admissionFixture {
	t.Helper()
	run := domain.Run{
		ID: "run-1", ProjectID: "proj-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
		Stages: []domain.Stage{{
			ID: "stage-1", RunID: "run-1", Name: "implementation",
			Attempts: []domain.Attempt{{
				ID: "attempt-1", StageID: "stage-1", Number: 1, InvocationID: "inv-1",
			}},
		}},
	}
	identity := domain.AuthIdentity{
		ID: "auth-1", Provider: "claude", AuthStoreMutationLease: true,
		AuthStoreVolume:       "provider-cred",
		MaxParallelExecutions: 1, RefreshStrategy: domain.RefreshOnDemand,
	}
	identityID := identity.ID
	in := domain.ExecutionAdmissionInput{
		InvocationID: "inv-1", RunID: run.ID, StageID: "stage-1", AttemptID: "attempt-1",
		Backend:        "fresh_vm_read_only_volume_handoff",
		Capabilities:   domain.NewCapabilitySnapshot(domain.CapDetachableWorkspace, domain.CapPostExitExport),
		OperatingMode:  domain.ModeAttendedDev,
		CredentialMode: domain.CredentialSubscriptionContained,
		EgressProfile:  domain.EgressProviderOnly,
		ImageRef:       domain.ImageRef("ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("ab", 32)),
		SpecDigest:     run.SpecDigest, PolicyDigest: run.PolicyDigest, InputDigest: "sha256:input",
		Base:           domain.BaseRevision{Repo: "owner/repo", RepositoryID: 424242, BaseRef: "refs/heads/main", BaseSHA: "deadbeef"},
		Workspace:      "ws-1",
		AuthIdentityID: &identityID,
		AdmittedAt:     admissionEpoch,
	}
	if mutate != nil {
		mutate(&in)
	}
	if in.OperatingMode == domain.ModeUnattended &&
		in.BackendConfigurationDigest == "" {
		in.BackendConfigurationDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	}
	admission, err := domain.NewExecutionAdmission(in)
	if err != nil {
		t.Fatalf("NewExecutionAdmission: %v", err)
	}
	return admissionFixture{run: run, identity: identity, admission: admission}
}

// conformantCapabilities is the fresh-vm class ceiling: the widest snapshot a
// backend-conformance record can back, and therefore the widest an unattended
// admission fixture may claim (#320).
func conformantCapabilities(t *testing.T) domain.CapabilitySnapshot {
	t.Helper()
	ceiling, ok := domain.ProvableCapabilities(domain.BackendFreshVMReadOnlyVolumeHandoff)
	if !ok {
		t.Fatal("fresh-vm class has no registered ceiling")
	}
	return ceiling
}

// seedBackendConformance records a passed conformance record at the fresh-vm
// ceiling, so unattended fixtures isolate the gate under test rather than the
// missing-conformance refusal.
func seedBackendConformance(t *testing.T, s *store.Store) {
	t.Helper()
	record, err := domain.NewBackendConformance(domain.BackendConformanceInput{
		Backend:             domain.BackendFreshVMReadOnlyVolumeHandoff,
		Outcome:             domain.ConformancePassed,
		ConfigurationDigest: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		Capabilities:        conformantCapabilities(t),
		ProvedAt:            admissionEpoch,
	})
	if err != nil {
		t.Fatalf("NewBackendConformance: %v", err)
	}
	if err := s.WriteInternal(context.Background(), func(tx *store.InternalTx) error {
		_, err := tx.RecordBackendConformance(context.Background(), record)
		return err
	}); err != nil {
		t.Fatalf("seed backend conformance: %v", err)
	}
}

func attendedFloors() map[domain.OperatingMode]domain.CapabilitySnapshot {
	return map[domain.OperatingMode]domain.CapabilitySnapshot{
		domain.ModeAttendedDev: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
	}
}

func healthyBackupHealth() domain.BackupHealth {
	return domain.BackupHealth{
		Encryption:         domain.BackupHealthHealthy,
		CheckpointCurrency: domain.BackupHealthHealthy,
		ArtifactClosure:    domain.BackupHealthHealthy,
		RestoreTestAge:     domain.BackupHealthHealthy,
	}
}

func healthyBackupHealthSource() store.BackupHealthSource {
	return store.BackupHealthSourceFunc(func(
		context.Context, store.BackupHealthContext,
	) (domain.BackupHealth, error) {
		return healthyBackupHealth(), nil
	})
}

// openWithFixture opens a store under the given policy and seeds the run and
// identity an admission refers to.
func openWithFixture(t *testing.T, f admissionFixture, opts store.Options) *store.Store {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, tempDBPath(t), opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, f.run); err != nil {
			return err
		}
		return tx.RecordAuthIdentity(ctx, f.identity, admissionEpoch)
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	seedBackendConformance(t, s)
	return s
}

func recordAdmission(t *testing.T, s *store.Store, admission domain.ExecutionAdmission) error {
	t.Helper()
	return s.WriteInternal(context.Background(), func(tx *store.InternalTx) error {
		return tx.RecordExecutionAdmission(context.Background(), admission)
	})
}

// TestExecutionAdmissionRoundTrip is the write-once contract: the record comes
// back as it went in, an identical replay converges, and a divergent replay of
// one invocation is an immutable conflict.
func TestExecutionAdmissionRoundTrip(t *testing.T) {
	ctx := context.Background()
	f := newAdmissionFixture(t, nil)
	s := openWithFixture(t, f, store.Options{AdmissionFloors: attendedFloors()})

	if err := recordAdmission(t, s, f.admission); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := recordAdmission(t, s, f.admission); err != nil {
		t.Fatalf("identical replay must converge: %v", err)
	}

	var got domain.ExecutionAdmission
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		got, err = tx.GetExecutionAdmission(ctx, "inv-1")
		return err
	}); err != nil {
		t.Fatalf("GetExecutionAdmission: %v", err)
	}
	if got.ID != f.admission.ID || !got.AdmittedAt.Equal(f.admission.AdmittedAt) ||
		!got.Capabilities.Has(domain.CapDetachableWorkspace) {
		t.Fatalf("round-tripped admission = %+v, want %+v", got, f.admission)
	}

	// A different admission for the same invocation is the "one committed
	// invocation intent" rule at the persistence boundary.
	diverged := newAdmissionFixture(t, func(in *domain.ExecutionAdmissionInput) {
		in.InputDigest = "sha256:other-input"
	})
	if err := recordAdmission(t, s, diverged.admission); !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("divergent replay = %v, want %v", err, store.ErrImmutableConflict)
	}

	var listed []domain.ExecutionAdmission
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		listed, err = tx.ListRunExecutionAdmissions(ctx, "run-1")
		return err
	}); err != nil {
		t.Fatalf("ListRunExecutionAdmissions: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != f.admission.ID {
		t.Fatalf("listed = %+v, want the one recorded admission", listed)
	}
}

// TestExecutionAdmissionRegatedAgainstCurrentFloor is the #52 re-gate: the
// snapshot is frozen at spawn, so what keeps it meaningful is re-checking it
// against the floor policy states now, on both the write and every read.
func TestExecutionAdmissionRegatedAgainstCurrentFloor(t *testing.T) {
	ctx := context.Background()
	f := newAdmissionFixture(t, nil)
	path := tempDBPath(t)

	s, err := store.Open(ctx, path, store.Options{AdmissionFloors: attendedFloors()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, f.run); err != nil {
			return err
		}
		if err := tx.RecordAuthIdentity(ctx, f.identity, admissionEpoch); err != nil {
			return err
		}
		return tx.RecordExecutionAdmission(ctx, f.admission)
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The operator raises the floor and restarts. The recorded class is
	// unchanged and truthful; it is simply no longer admissible.
	raised, err := store.Open(ctx, path, store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: domain.NewCapabilitySnapshot(domain.CapNetworklessExport),
		},
	})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = raised.Close() })

	err = raised.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetExecutionAdmission(ctx, "inv-1")
		return err
	})
	if !errors.Is(err, domain.ErrCapabilityBelowFloor) {
		t.Fatalf("get under a raised floor = %v, want %v", err, domain.ErrCapabilityBelowFloor)
	}
	// The List path shares the reconstruction function, so it refuses too: a
	// gate reachable through only one path is not a gate.
	err = raised.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.ListRunExecutionAdmissions(ctx, "run-1")
		return err
	})
	if !errors.Is(err, domain.ErrCapabilityBelowFloor) {
		t.Fatalf("list under a raised floor = %v, want %v", err, domain.ErrCapabilityBelowFloor)
	}
}

// TestExecutionAdmissionUnconfiguredFloorFailsClosed pins the direction an
// unconfigured policy fails in: nothing is admissible, rather than everything.
func TestExecutionAdmissionUnconfiguredFloorFailsClosed(t *testing.T) {
	f := newAdmissionFixture(t, nil)
	s := openWithFixture(t, f, store.Options{})
	if err := recordAdmission(t, s, f.admission); !errors.Is(err, domain.ErrUnknownAdmissionFloor) {
		t.Fatalf("record with no floors = %v, want %v", err, domain.ErrUnknownAdmissionFloor)
	}
}

func TestExecutionAdmissionRejectsRetiredWaiver(t *testing.T) {
	activeProfile := testTrustProfile(t, "owner/repo", 424242).ProfileDigest
	f := newAdmissionFixture(t, func(in *domain.ExecutionAdmissionInput) {
		in.OperatingMode = domain.ModeUnattended
		in.Capabilities = conformantCapabilities(t)
		in.TrustProfileDigest = &activeProfile
		in.BackupEncryptionWaiver = &domain.BackupEncryptionWaiver{
			RepositoryID: 424242, Reason: "phase 1a.2 supervised runs",
		}
	})
	s := openWithFixture(t, f, store.Options{})
	if err := recordAdmission(t, s, f.admission); !errors.Is(err, domain.ErrBackupEncryptionWaiverUnsupported) {
		t.Fatalf("record waiver-bearing admission = %v, want %v",
			err, domain.ErrBackupEncryptionWaiverUnsupported)
	}
}

// testTrustProfile builds the approved profile a fixture is gated against, so
// a record can name the exact revision the store will compare it with.
func testTrustProfile(t *testing.T, repo string, repositoryID int64) domain.AutomationTrustProfile {
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
			Mode: domain.ReviewAuto, ConfigDigest: "sha256:review-config",
		},
	})
	if err != nil {
		t.Fatalf("NewAutomationTrustProfile: %v", err)
	}
	return profile
}

// seedTrustProfile records an approved trust profile binding repo to the given
// canonical numeric repository id.
func seedTrustProfile(t *testing.T, s *store.Store, repo string, repositoryID int64) {
	t.Helper()
	ctx := context.Background()
	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo: repo, RepositoryID: repositoryID,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        "sha256:workflow-audit",
		Review: domain.ReviewSettings{
			Mode: domain.ReviewAuto, ConfigDigest: "sha256:review-config",
		},
	})
	if err != nil {
		t.Fatalf("NewAutomationTrustProfile: %v", err)
	}
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordTrustProfile(ctx, profile, admissionEpoch)
	}); err != nil {
		t.Fatalf("RecordTrustProfile: %v", err)
	}
}

// TestOptionsAdmissionPolicyIsSnapshotted proves a caller cannot widen the
// boundary policy after Open by mutating the maps and slices it passed in.
func TestOptionsAdmissionPolicyIsSnapshotted(t *testing.T) {
	f := newAdmissionFixture(t, nil)
	floors := attendedFloors()
	waiver := int64(42)
	opts := store.Options{AdmissionFloors: floors, BackupEncryptionWaiverRepositoryID: &waiver}
	s := openWithFixture(t, f, opts)

	delete(floors, domain.ModeAttendedDev)
	floors[domain.ModeUnattended] = nil
	waiver = 99

	if err := recordAdmission(t, s, f.admission); err != nil {
		t.Fatalf("policy followed the caller's map after Open: %v", err)
	}
}

// TestExecutionAdmissionMatchesTheRunsDigests refuses a record whose spec or
// policy digest disagrees with the run it names. Both are fixed at run
// creation and the record is what the driver is later started from, so a
// caller-supplied digest would point execution at a binding the run does not
// have.
func TestExecutionAdmissionMatchesTheRunsDigests(t *testing.T) {
	f := newAdmissionFixture(t, nil)
	s := openWithFixture(t, f, store.Options{AdmissionFloors: attendedFloors()})

	for _, tc := range []struct {
		name   string
		mutate func(*domain.ExecutionAdmissionInput)
	}{
		{"other spec digest", func(in *domain.ExecutionAdmissionInput) { in.SpecDigest = "sha256:other-spec" }},
		{"other policy digest", func(in *domain.ExecutionAdmissionInput) { in.PolicyDigest = "sha256:other-policy" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			substituted := newAdmissionFixture(t, tc.mutate)
			if err := recordAdmission(t, s, substituted.admission); !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("record = %v, want %v", err, domain.ErrParentKeyMismatch)
			}
		})
	}
}

// TestUnattendedAdmissionRequiresATrustedRepository is §5.7's conformance list
// at the persistence boundary: unattended running requires an approved trust
// profile, and the profile is also what stops the record's own repository
// name-and-number pair from being self-asserted.
func TestUnattendedAdmissionRequiresATrustedRepository(t *testing.T) {
	activeProfile := testTrustProfile(t, "owner/repo", 424242).ProfileDigest
	unattended := func(in *domain.ExecutionAdmissionInput) {
		in.OperatingMode = domain.ModeUnattended
		in.Capabilities = conformantCapabilities(t)
		in.TrustProfileDigest = &activeProfile
	}
	f := newAdmissionFixture(t, unattended)
	floors := map[domain.OperatingMode]domain.CapabilitySnapshot{
		domain.ModeUnattended: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
	}
	approved := []domain.CredentialMode{domain.CredentialSubscriptionContained}

	untrusted := openWithFixture(t, f, store.Options{
		AdmissionFloors: floors, ApprovedCredentialModes: approved,
		BackupHealthSource: healthyBackupHealthSource(),
	})
	if err := recordAdmission(t, untrusted, f.admission); !errors.Is(err, store.ErrRepositoryUntrusted) {
		t.Fatalf("unattended admission with no profile = %v, want %v", err, store.ErrRepositoryUntrusted)
	}

	drifted := openWithFixture(t, f, store.Options{
		AdmissionFloors: floors, ApprovedCredentialModes: approved,
		BackupHealthSource: healthyBackupHealthSource(),
	})
	seedTrustProfile(t, drifted, f.admission.Base.Repo, 999)
	if err := recordAdmission(t, drifted, f.admission); !errors.Is(err, domain.ErrRepositoryIdentityMismatch) {
		t.Fatalf("unattended admission against a drifted profile = %v, want %v",
			err, domain.ErrRepositoryIdentityMismatch)
	}

	trusted := openWithFixture(t, f, store.Options{
		AdmissionFloors: floors, ApprovedCredentialModes: approved,
		BackupHealthSource: healthyBackupHealthSource(),
	})
	seedTrustProfile(t, trusted, f.admission.Base.Repo, f.admission.Base.RepositoryID)
	if err := recordAdmission(t, trusted, f.admission); err != nil {
		t.Fatalf("unattended admission against its trusted profile: %v", err)
	}

	// §5.7 also requires an approved credential mode of an unattended run, and
	// the store is where "approved" is configured: a correctly spelled mode
	// nobody approved is refused even with the profile in place.
	unapproved := openWithFixture(t, f, store.Options{
		AdmissionFloors: floors,
	})
	seedTrustProfile(t, unapproved, f.admission.Base.Repo, f.admission.Base.RepositoryID)
	if err := recordAdmission(t, unapproved, f.admission); !errors.Is(err, domain.ErrCredentialModeNotApproved) {
		t.Fatalf("unattended admission under an unapproved mode = %v, want %v",
			err, domain.ErrCredentialModeNotApproved)
	}

	// attended_dev is unchanged: §5.7 allows the weaker class there, so a
	// profile is not part of its conformance.
	attended := newAdmissionFixture(t, nil)
	dev := openWithFixture(t, attended, store.Options{AdmissionFloors: attendedFloors()})
	if err := recordAdmission(t, dev, attended.admission); err != nil {
		t.Fatalf("attended_dev must not require a trust profile: %v", err)
	}
}

// TestUnattendedAdmissionUsesEncryptedBackupHealth pins the ordinary post-waiver
// path: a fully healthy checkpoint admits unattended work without an exception.
func TestUnattendedAdmissionUsesEncryptedBackupHealth(t *testing.T) {
	active := testTrustProfile(t, "owner/repo", 424242).ProfileDigest
	f := newAdmissionFixture(t, func(in *domain.ExecutionAdmissionInput) {
		in.OperatingMode = domain.ModeUnattended
		in.Capabilities = conformantCapabilities(t)
		in.TrustProfileDigest = &active
	})
	s := openWithFixture(t, f, store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeUnattended: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		},
		ApprovedCredentialModes: []domain.CredentialMode{domain.CredentialSubscriptionContained},
		BackupHealthSource:      healthyBackupHealthSource(),
	})
	seedTrustProfile(t, s, f.admission.Base.Repo, f.admission.Base.RepositoryID)

	// Everything else about this run is in order: full capability class, an
	// approved credential mode, and the active trust profile.
	if err := recordAdmission(t, s, f.admission); err != nil {
		t.Fatalf("unattended admission under encrypted backup health: %v", err)
	}

	// attended_dev is unaffected: §5.7 gates backup health on unattended running.
	attended := newAdmissionFixture(t, nil)
	dev := openWithFixture(t, attended, store.Options{AdmissionFloors: attendedFloors()})
	if err := recordAdmission(t, dev, attended.admission); err != nil {
		t.Fatalf("attended_dev must not require backup authorization: %v", err)
	}
}

// TestBackupHealthIsQueryable pins the producer seam independently of
// admission: callers can inspect every dimension, while an unconfigured store
// reports absence instead of synthesizing a healthy default.
func TestBackupHealthIsQueryable(t *testing.T) {
	ctx := context.Background()
	want := healthyBackupHealth()
	configured := openStore(t, store.Options{BackupHealthSource: healthyBackupHealthSource()})
	got, err := configured.BackupHealth(ctx)
	if err != nil {
		t.Fatalf("BackupHealth: %v", err)
	}
	if got != want {
		t.Fatalf("BackupHealth = %+v, want %+v", got, want)
	}

	unconfigured := openStore(t, store.Options{})
	if _, err := unconfigured.BackupHealth(ctx); !errors.Is(err, domain.ErrBackupHealthUnavailable) {
		t.Fatalf("unconfigured BackupHealth = %v, want %v", err, domain.ErrBackupHealthUnavailable)
	}
}

// TestUnattendedAdmissionRegatesEveryBackupHealthDimension covers #317 at
// both trust boundaries. Each failed dimension independently refuses the
// initial write, then the same live source is changed after a healthy write
// and reconstruction refuses the already-recorded admission.
func TestUnattendedAdmissionRegatesEveryBackupHealthDimension(t *testing.T) {
	ctx := context.Background()
	active := testTrustProfile(t, "owner/repo", 424242).ProfileDigest
	f := newAdmissionFixture(t, func(in *domain.ExecutionAdmissionInput) {
		in.OperatingMode = domain.ModeUnattended
		in.Capabilities = conformantCapabilities(t)
		in.TrustProfileDigest = &active
	})

	health := healthyBackupHealth()
	source := store.BackupHealthSourceFunc(func(
		context.Context, store.BackupHealthContext,
	) (domain.BackupHealth, error) {
		return health, nil
	})
	opts := store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeUnattended: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		},
		ApprovedCredentialModes: []domain.CredentialMode{domain.CredentialSubscriptionContained},
		BackupHealthSource:      source,
	}
	s := openWithFixture(t, f, opts)
	seedTrustProfile(t, s, f.admission.Base.Repo, f.admission.Base.RepositoryID)

	cases := []struct {
		name    string
		fail    func(*domain.BackupHealth)
		wantErr error
	}{
		{
			"encryption",
			func(health *domain.BackupHealth) {
				health.Encryption = domain.BackupHealthUnhealthy
			},
			domain.ErrCheckpointNotEncrypted,
		},
		{
			"checkpoint currency",
			func(health *domain.BackupHealth) {
				health.CheckpointCurrency = domain.BackupHealthUnhealthy
			},
			domain.ErrCheckpointNotCurrent,
		},
		{
			"artifact closure",
			func(health *domain.BackupHealth) {
				health.ArtifactClosure = domain.BackupHealthUnhealthy
			},
			domain.ErrArtifactClosureIncomplete,
		},
		{
			"restore-test age",
			func(health *domain.BackupHealth) {
				health.RestoreTestAge = domain.BackupHealthUnhealthy
			},
			domain.ErrRestoreTestStale,
		},
		{
			"missing dimension",
			func(health *domain.BackupHealth) {
				health.RestoreTestAge = ""
			},
			domain.ErrInvalidBackupHealthStatus,
		},
	}
	for _, tc := range cases {
		t.Run("write/"+tc.name, func(t *testing.T) {
			health = healthyBackupHealth()
			tc.fail(&health)
			if err := recordAdmission(t, s, f.admission); !errors.Is(err, tc.wantErr) {
				t.Fatalf("record = %v, want %v", err, tc.wantErr)
			}
		})
	}

	health = healthyBackupHealth()
	if err := recordAdmission(t, s, f.admission); err != nil {
		t.Fatalf("record under healthy backup signal: %v", err)
	}
	for _, tc := range cases {
		t.Run("reconstruction/"+tc.name, func(t *testing.T) {
			health = healthyBackupHealth()
			tc.fail(&health)
			err := s.Read(ctx, func(tx *store.ReadTx) error {
				_, err := tx.GetExecutionAdmission(ctx, f.admission.InvocationID)
				return err
			})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("GetExecutionAdmission = %v, want %v", err, tc.wantErr)
			}
		})
	}
	health = healthyBackupHealth()
}

// TestUnattendedAdmissionWithNoBackupHealthSourceFailsClosed distinguishes a
// missing producer from an explicitly unhealthy signal. Neither is an
// implicit pass.
func TestUnattendedAdmissionWithNoBackupHealthSourceFailsClosed(t *testing.T) {
	active := testTrustProfile(t, "owner/repo", 424242).ProfileDigest
	f := newAdmissionFixture(t, func(in *domain.ExecutionAdmissionInput) {
		in.OperatingMode = domain.ModeUnattended
		in.Capabilities = conformantCapabilities(t)
		in.TrustProfileDigest = &active
	})
	s := openWithFixture(t, f, store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeUnattended: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		},
		ApprovedCredentialModes: []domain.CredentialMode{domain.CredentialSubscriptionContained},
	})
	seedTrustProfile(t, s, f.admission.Base.Repo, f.admission.Base.RepositoryID)
	if err := recordAdmission(t, s, f.admission); !errors.Is(err, domain.ErrBackupHealthUnavailable) {
		t.Fatalf("record with no backup-health source = %v, want %v",
			err, domain.ErrBackupHealthUnavailable)
	}
}

func TestListUnattendedAdmissionsEvaluatesBackupHealthOnce(t *testing.T) {
	ctx := context.Background()
	active := testTrustProfile(t, "owner/repo", 424242).ProfileDigest
	unattended := func(in *domain.ExecutionAdmissionInput) {
		in.OperatingMode = domain.ModeUnattended
		in.Capabilities = conformantCapabilities(t)
		in.TrustProfileDigest = &active
	}
	first := newAdmissionFixture(t, unattended)
	second := newAdmissionFixture(t, func(in *domain.ExecutionAdmissionInput) {
		unattended(in)
		in.InvocationID = "inv-2"
		in.AttemptID = "attempt-2"
		in.InputDigest = "sha256:input-2"
		in.Workspace = "ws-2"
	})
	first.run.Stages[0].Attempts = append(first.run.Stages[0].Attempts, domain.Attempt{
		ID: "attempt-2", StageID: "stage-1", Number: 2, InvocationID: "inv-2",
	})

	sourceCalls := 0
	s := openWithFixture(t, first, store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeUnattended: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		},
		ApprovedCredentialModes: []domain.CredentialMode{domain.CredentialSubscriptionContained},
		BackupHealthSource: store.BackupHealthSourceFunc(func(
			context.Context, store.BackupHealthContext,
		) (domain.BackupHealth, error) {
			sourceCalls++
			return healthyBackupHealth(), nil
		}),
	})
	seedTrustProfile(t, s, first.admission.Base.Repo, first.admission.Base.RepositoryID)
	if err := recordAdmission(t, s, first.admission); err != nil {
		t.Fatalf("record first admission: %v", err)
	}
	if err := recordAdmission(t, s, second.admission); err != nil {
		t.Fatalf("record second admission: %v", err)
	}

	sourceCalls = 0
	var admissions []domain.ExecutionAdmission
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		admissions, err = tx.ListRunExecutionAdmissions(ctx, first.run.ID)
		return err
	}); err != nil {
		t.Fatalf("ListRunExecutionAdmissions: %v", err)
	}
	if len(admissions) != 2 {
		t.Fatalf("listed %d admissions, want 2", len(admissions))
	}
	if sourceCalls != 1 {
		t.Fatalf("backup-health source calls = %d, want 1 per transaction", sourceCalls)
	}
}

// TestAdmissionBoundToTheActiveTrustProfileRevision is the revision half of
// the trust binding: the repository id survives a revision, so it cannot say
// whether a record was admitted under the profile that is approved now. An
// operator who activates a revised profile expects it to bind work already in
// flight, not merely the next run.
func TestAdmissionBoundToTheActiveTrustProfileRevision(t *testing.T) {
	ctx := context.Background()
	active := testTrustProfile(t, "owner/repo", 424242).ProfileDigest
	f := newAdmissionFixture(t, func(in *domain.ExecutionAdmissionInput) {
		in.OperatingMode = domain.ModeUnattended
		in.Capabilities = conformantCapabilities(t)
		in.TrustProfileDigest = &active
	})
	floors := map[domain.OperatingMode]domain.CapabilitySnapshot{
		domain.ModeUnattended: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
	}
	s := openWithFixture(t, f, store.Options{
		AdmissionFloors:         floors,
		ApprovedCredentialModes: []domain.CredentialMode{domain.CredentialSubscriptionContained},
		BackupHealthSource:      healthyBackupHealthSource(),
	})
	seedTrustProfile(t, s, f.admission.Base.Repo, f.admission.Base.RepositoryID)
	if err := recordAdmission(t, s, f.admission); err != nil {
		t.Fatalf("record under the active revision: %v", err)
	}
	// The operator approves and activates a revised profile for the same
	// repository: same numeric id, different content, different digest.
	revised, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo: f.admission.Base.Repo, RepositoryID: f.admission.Base.RepositoryID,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        "sha256:workflow-audit-v2",
		Review: domain.ReviewSettings{
			Mode: domain.ReviewAuto, ConfigDigest: "sha256:review-config",
		},
	})
	if err != nil {
		t.Fatalf("NewAutomationTrustProfile: %v", err)
	}
	if revised.ProfileDigest == active {
		t.Fatal("the revised profile must differ from the one admitted under")
	}
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordTrustProfile(ctx, revised, admissionEpoch.Add(time.Hour))
	}); err != nil {
		t.Fatalf("activate the revision: %v", err)
	}
	outcome := domain.ExecutionOutcome{
		InvocationID: f.admission.InvocationID, AdmissionID: f.admission.ID,
		Status: domain.ExecutionOutcomeLost, RecordedAt: admissionEpoch.Add(2 * time.Hour),
	}
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordExecutionOutcome(ctx, outcome)
	}); err != nil {
		t.Fatalf("record terminal outcome after policy revision: %v", err)
	}

	err = s.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetExecutionAdmission(ctx, f.admission.InvocationID)
		return err
	})
	if !errors.Is(err, domain.ErrTrustProfileSuperseded) {
		t.Fatalf("read under a revised profile = %v, want %v", err, domain.ErrTrustProfileSuperseded)
	}
	err = s.Read(ctx, func(tx *store.ReadTx) error {
		admission, err := tx.GetExecutionAdmissionRecord(ctx, f.admission.InvocationID)
		if err != nil {
			return err
		}
		if admission.ID != f.admission.ID {
			t.Errorf("historical admission id = %q, want %q", admission.ID, f.admission.ID)
		}
		got, err := tx.GetExecutionOutcomeRecord(ctx, f.admission.InvocationID)
		if err != nil {
			return err
		}
		if got.Status != domain.ExecutionOutcomeLost || got.AdmissionID != f.admission.ID {
			t.Errorf("historical outcome = %#v, want lost under admission %s", got, f.admission.ID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read immutable terminal history under a revised profile: %v", err)
	}
}

// TestExecutionAdmissionMatchesTheInvocationsInputs refuses a record that
// claims the stage ran against inputs the invocation does not bind. The agent
// invocation record is the durable statement of what a turn was given, so the
// digest is recomputed from it rather than trusted from the caller.
func TestExecutionAdmissionMatchesTheInvocationsInputs(t *testing.T) {
	ctx := context.Background()
	f := newAdmissionFixture(t, nil)
	s := openWithFixture(t, f, store.Options{AdmissionFloors: attendedFloors()})

	conversation := domain.ConversationID("conv-1")
	invocation, err := domain.NewAgentInvocation("inv-1", nil, &conversation, 1)
	if err != nil {
		t.Fatalf("NewAgentInvocation: %v", err)
	}
	bound, err := invocation.ComputeInputDigest()
	if err != nil {
		t.Fatalf("ComputeInputDigest: %v", err)
	}
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutConversation(ctx, domain.Conversation{
			ID: conversation, Status: domain.ConversationAwaitingAgent,
			Messages: []domain.Message{{
				ID: "msg-1", ConversationID: conversation, Sequence: 1,
				Author: domain.AuthorUser, Body: "go", CreatedAt: admissionEpoch,
			}},
		}); err != nil {
			return err
		}
		return tx.PutAgentInvocation(ctx, invocation)
	}); err != nil {
		t.Fatalf("seed invocation: %v", err)
	}

	// The fixture's placeholder digest is not the invocation's binding.
	if err := recordAdmission(t, s, f.admission); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("substituted input digest = %v, want %v", err, domain.ErrParentKeyMismatch)
	}

	matching := newAdmissionFixture(t, func(in *domain.ExecutionAdmissionInput) {
		in.InputDigest = bound
	})
	if err := recordAdmission(t, s, matching.admission); err != nil {
		t.Fatalf("admission naming the invocation's own inputs: %v", err)
	}
}

// TestExecutionAdmissionRequiresItsAttempt refuses a claim about an attempt
// the run does not carry: the admission asserts a binding, and an unbound
// assertion is what a confused writer produces.
func TestExecutionAdmissionRequiresItsAttempt(t *testing.T) {
	f := newAdmissionFixture(t, nil)
	s := openWithFixture(t, f, store.Options{AdmissionFloors: attendedFloors()})

	unknownAttempt := newAdmissionFixture(t, func(in *domain.ExecutionAdmissionInput) {
		in.InvocationID = "inv-unknown"
	})
	if err := recordAdmission(t, s, unknownAttempt.admission); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("admission for an unrecorded attempt = %v, want %v", err, domain.ErrParentKeyMismatch)
	}

	retargeted := newAdmissionFixture(t, func(in *domain.ExecutionAdmissionInput) {
		in.AttemptID = "attempt-other"
	})
	if err := recordAdmission(t, s, retargeted.admission); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("admission retargeting the attempt = %v, want %v", err, domain.ErrParentKeyMismatch)
	}

	unknownRun := newAdmissionFixture(t, func(in *domain.ExecutionAdmissionInput) {
		in.RunID = "run-unknown"
	})
	if err := recordAdmission(t, s, unknownRun.admission); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("admission for an unknown run = %v, want %v", err, store.ErrNotFound)
	}
}

// TestExecutionExportBinding covers the link the publication chain joins on: an
// export belongs to its admission, at the base that admission was granted for.
func TestExecutionExportBinding(t *testing.T) {
	ctx := context.Background()
	f := newAdmissionFixture(t, nil)
	s := openWithFixture(t, f, store.Options{AdmissionFloors: attendedFloors()})
	if err := recordAdmission(t, s, f.admission); err != nil {
		t.Fatalf("record admission: %v", err)
	}

	evidence := domain.Digest("sha256:evidence")
	export, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: "inv-1", AdmissionID: f.admission.ID,
		ObservedBaseSHA: f.admission.Base.BaseSHA, HeadSHA: "cafebabe",
		ManifestDigest: "sha256:manifest", EvidenceManifestDigest: &evidence,
		CommitPlanPresent: true, RecordedAt: admissionEpoch.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("NewExecutionExport: %v", err)
	}
	record := func(x domain.ExecutionExport) error {
		return s.WriteInternal(ctx, func(tx *store.InternalTx) error {
			return tx.RecordExecutionExport(ctx, x)
		})
	}
	if err := record(export); err != nil {
		t.Fatalf("record export: %v", err)
	}
	if err := record(export); err != nil {
		t.Fatalf("identical replay must converge: %v", err)
	}

	var got domain.ExecutionExport
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		got, err = tx.GetExecutionExport(ctx, "inv-1")
		return err
	}); err != nil {
		t.Fatalf("GetExecutionExport: %v", err)
	}
	if got.HeadSHA != "cafebabe" || got.EvidenceManifestDigest == nil || *got.EvidenceManifestDigest != evidence {
		t.Fatalf("round-tripped export = %+v", got)
	}

	// A handoff that observed a base other than the admitted one is not this
	// attempt's export.
	drifted, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: "inv-2", AdmissionID: f.admission.ID,
		ObservedBaseSHA: "0ther", HeadSHA: "cafebabe",
		ManifestDigest: "sha256:manifest", RecordedAt: admissionEpoch.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("NewExecutionExport: %v", err)
	}
	if err := record(drifted); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("export for an unadmitted invocation = %v, want %v", err, store.ErrNotFound)
	}

	baseDrift, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: "inv-1", AdmissionID: f.admission.ID,
		ObservedBaseSHA: "0ther", HeadSHA: "cafebabe",
		ManifestDigest: "sha256:manifest", RecordedAt: admissionEpoch.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("NewExecutionExport: %v", err)
	}
	if err := record(baseDrift); !errors.Is(err, domain.ErrExportBaseMismatch) {
		t.Fatalf("export at another base = %v, want %v", err, domain.ErrExportBaseMismatch)
	}
}

func TestExecutionOutcomeBinding(t *testing.T) {
	ctx := context.Background()
	f := newAdmissionFixture(t, nil)
	s := openWithFixture(t, f, store.Options{AdmissionFloors: attendedFloors()})
	if err := recordAdmission(t, s, f.admission); err != nil {
		t.Fatalf("record admission: %v", err)
	}
	outcome := domain.ExecutionOutcome{
		InvocationID: "inv-1",
		AdmissionID:  f.admission.ID,
		Status:       domain.ExecutionOutcomeFailed,
		Summary:      "writer failed",
		RecordedAt:   admissionEpoch.Add(time.Hour),
	}
	record := func(x domain.ExecutionOutcome) error {
		return s.WriteInternal(ctx, func(tx *store.InternalTx) error {
			return tx.RecordExecutionOutcome(ctx, x)
		})
	}
	if err := record(outcome); err != nil {
		t.Fatalf("record outcome: %v", err)
	}
	if err := record(outcome); err != nil {
		t.Fatalf("identical replay must converge: %v", err)
	}
	var got domain.ExecutionOutcome
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		got, err = tx.GetExecutionOutcome(ctx, "inv-1")
		return err
	}); err != nil {
		t.Fatalf("GetExecutionOutcome: %v", err)
	}
	if got.Status != domain.ExecutionOutcomeFailed || got.Summary != "writer failed" {
		t.Fatalf("round-tripped outcome = %+v", got)
	}
	changed := outcome
	changed.Status = domain.ExecutionOutcomeCanceled
	if err := record(changed); !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("changed outcome = %v, want immutable conflict", err)
	}
}

func TestExecutionExportAndOutcomeAreMutuallyExclusive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tc := range []struct {
		name        string
		recordFirst func(
			context.Context, *store.InternalTx,
			domain.ExecutionExport, domain.ExecutionOutcome,
		) error
		recordSecond func(
			context.Context, *store.InternalTx,
			domain.ExecutionExport, domain.ExecutionOutcome,
		) error
	}{
		{
			name: "export then outcome",
			recordFirst: func(
				ctx context.Context, tx *store.InternalTx,
				export domain.ExecutionExport, _ domain.ExecutionOutcome,
			) error {
				return tx.RecordExecutionExport(ctx, export)
			},
			recordSecond: func(
				ctx context.Context, tx *store.InternalTx,
				_ domain.ExecutionExport, outcome domain.ExecutionOutcome,
			) error {
				return tx.RecordExecutionOutcome(ctx, outcome)
			},
		},
		{
			name: "outcome then export",
			recordFirst: func(
				ctx context.Context, tx *store.InternalTx,
				_ domain.ExecutionExport, outcome domain.ExecutionOutcome,
			) error {
				return tx.RecordExecutionOutcome(ctx, outcome)
			},
			recordSecond: func(
				ctx context.Context, tx *store.InternalTx,
				export domain.ExecutionExport, _ domain.ExecutionOutcome,
			) error {
				return tx.RecordExecutionExport(ctx, export)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newAdmissionFixture(t, nil)
			s := openWithFixture(t, f, store.Options{AdmissionFloors: attendedFloors()})
			if err := recordAdmission(t, s, f.admission); err != nil {
				t.Fatalf("record admission: %v", err)
			}
			export, err := domain.NewExecutionExport(domain.ExecutionExportInput{
				InvocationID: "inv-1", AdmissionID: f.admission.ID,
				ObservedBaseSHA: f.admission.Base.BaseSHA, HeadSHA: "cafebabe",
				ManifestDigest: "sha256:manifest",
				RecordedAt:     admissionEpoch.Add(time.Hour),
			})
			if err != nil {
				t.Fatalf("NewExecutionExport: %v", err)
			}
			outcome := domain.ExecutionOutcome{
				InvocationID: "inv-1", AdmissionID: f.admission.ID,
				Status: domain.ExecutionOutcomeFailed, Summary: "failed",
				RecordedAt: admissionEpoch.Add(time.Hour),
			}
			if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
				return tc.recordFirst(ctx, tx, export, outcome)
			}); err != nil {
				t.Fatalf("record first authority: %v", err)
			}
			if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
				return tc.recordSecond(ctx, tx, export, outcome)
			}); !errors.Is(err, store.ErrImmutableConflict) {
				t.Fatalf("record contradictory authority = %v, want immutable conflict", err)
			}
		})
	}
}

// TestRunReachesItsAdmissionAndExport walks the durable half of the identity
// chain: from a run, to the attempt that carries the invocation, to the
// admission that granted it, to the export that attempt produced — each hop
// reconstructed through the store, and the export's own bindings checked
// against the admission it names.
//
// What this deliberately does not assert is the publication hop. A publication
// intent's SourceHeadSHA should be held against this record's head, and no
// production writer does that comparison today: nothing outside these APIs
// writes an ExecutionExport yet (#302/#303/#237 supply the producer), so the
// check has nothing to run against and is filed as #318. An earlier version of
// this test "asserted" that hop by comparing a literal to the same literal,
// which proved nothing; better to state the gap than to simulate coverage.
func TestRunReachesItsAdmissionAndExport(t *testing.T) {
	ctx := context.Background()
	f := newAdmissionFixture(t, nil)
	s := openWithFixture(t, f, store.Options{AdmissionFloors: attendedFloors()})
	if err := recordAdmission(t, s, f.admission); err != nil {
		t.Fatalf("record admission: %v", err)
	}
	export, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: "inv-1", AdmissionID: f.admission.ID,
		ObservedBaseSHA: f.admission.Base.BaseSHA, HeadSHA: "cafebabe",
		ManifestDigest: "sha256:manifest", RecordedAt: admissionEpoch.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("NewExecutionExport: %v", err)
	}
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordExecutionExport(ctx, export)
	}); err != nil {
		t.Fatalf("record export: %v", err)
	}

	var (
		admission domain.ExecutionAdmission
		stored    domain.ExecutionExport
		attempt   domain.Attempt
	)
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		// #308's reservation binds a publication invocation to a run id; the
		// walk starts from the run that binding names.
		run, err := tx.GetRun(ctx, f.run.ID)
		if err != nil {
			return err
		}
		attempt = run.Stages[0].Attempts[0]
		admission, err = tx.GetExecutionAdmission(ctx, attempt.InvocationID)
		if err != nil {
			return err
		}
		stored, err = tx.GetExecutionExport(ctx, attempt.InvocationID)
		return err
	}); err != nil {
		t.Fatalf("walk the chain: %v", err)
	}

	if admission.AttemptID != attempt.ID || admission.InvocationID != attempt.InvocationID {
		t.Fatalf("admission %+v does not name the attempt the run carries (%+v)", admission, attempt)
	}
	if stored.AdmissionID != admission.ID {
		t.Fatalf("export names admission %s, the run's admission is %s", stored.AdmissionID, admission.ID)
	}
	if stored.ObservedBaseSHA != admission.Base.BaseSHA {
		t.Fatalf("export base %q, admitted base %q", stored.ObservedBaseSHA, admission.Base.BaseSHA)
	}
	if stored.HeadSHA != export.HeadSHA {
		t.Fatalf("export head %q did not round-trip (wrote %q)", stored.HeadSHA, export.HeadSHA)
	}
}
