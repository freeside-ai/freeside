package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

// TestExecutionRecordsMigrationAppliesFromHead is the migration acceptance for
// 0014: the tables land on a database sitting at the real prior head, existing
// rows survive, and nothing is backfilled into them.
func TestExecutionRecordsMigrationAppliesFromHead(t *testing.T) {
	ctx := context.Background()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0014_")

	if _, err := db.ExecContext(ctx,
		`INSERT INTO runs (id, project_id, policy_digest, entity_version, as_of_revision, body)
		 VALUES ('run-1', 'proj-1', 'sha256:policy', 1, 1, '{}')`); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}

	var runs int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE id = 'run-1'`).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 1 {
		t.Errorf("pre-migration run count = %d, want 1", runs)
	}
	for _, table := range []string{"execution_admissions", "execution_exports", "execution_outcomes"} {
		var rows int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&rows); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if rows != 0 {
			t.Errorf("%s = %d rows after migration, want 0 (nothing is backfilled)", table, rows)
		}
	}

	// An admission for a run that does not exist, and an export with no
	// admission, are unrepresentable rather than merely unwritten.
	_, err := db.ExecContext(ctx,
		`INSERT INTO execution_admissions
		   (invocation_id, id, run_id, stage_id, attempt_id, operating_mode, admitted_at, body)
		 VALUES ('inv-1', 'sha256:a', 'run-ghost', 'stage-1', 'attempt-1', 'attended_dev', '2026-01-02T03:04:05Z', '{}')`)
	if err == nil {
		t.Error("an admission naming an unknown run was accepted")
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO execution_exports
		   (invocation_id, admission_id, head_sha, manifest_digest, recorded_at, body)
		 VALUES ('inv-ghost', 'sha256:a', 'cafebabe', 'sha256:m', '2026-01-02T03:04:05Z', '{}')`)
	if err == nil {
		t.Error("an export with no admission was accepted")
	}
}

func TestExecutionAuthorityTriggersRejectOverlap(t *testing.T) {
	ctx := context.Background()
	t.Run("outcome after export", func(t *testing.T) {
		s, admission := seedAdmission(t, nil)
		export, err := domain.NewExecutionExport(domain.ExecutionExportInput{
			InvocationID: admission.InvocationID, AdmissionID: admission.ID,
			ObservedBaseSHA: admission.Base.BaseSHA, HeadSHA: "cafebabe",
			ManifestDigest: "sha256:manifest",
			RecordedAt:     admission.AdmittedAt.Add(time.Hour),
		})
		if err != nil {
			t.Fatalf("NewExecutionExport: %v", err)
		}
		if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
			return tx.RecordExecutionExport(ctx, export)
		}); err != nil {
			t.Fatalf("record export: %v", err)
		}

		_, err = s.db.ExecContext(ctx,
			`INSERT INTO execution_outcomes
			   (invocation_id, admission_id, status, summary, recorded_at, body)
			 VALUES (?, ?, 'failed', 'forged', ?, '{}')`,
			admission.InvocationID, admission.ID, formatTime(admission.AdmittedAt.Add(time.Hour)))
		if err == nil {
			t.Fatal("raw outcome beside an export was accepted")
		}

		seedRawAdmission(t, ctx, s, admission)
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO execution_outcomes
			   (invocation_id, admission_id, status, summary, recorded_at, body)
			 VALUES ('inv-2', 'sha256:other', 'failed', 'forged', ?, '{}')`,
			formatTime(admission.AdmittedAt.Add(time.Hour))); err != nil {
			t.Fatalf("seed raw outcome: %v", err)
		}
		if _, err := s.db.ExecContext(ctx,
			`UPDATE execution_outcomes
			 SET invocation_id = ?, admission_id = ?
			 WHERE invocation_id = 'inv-2'`,
			admission.InvocationID, admission.ID); err == nil {
			t.Fatal("raw outcome moved beside an export was accepted")
		}
	})

	t.Run("export after outcome", func(t *testing.T) {
		s, admission := seedAdmission(t, nil)
		outcome := domain.ExecutionOutcome{
			InvocationID: admission.InvocationID, AdmissionID: admission.ID,
			Status: domain.ExecutionOutcomeFailed, Summary: "failed",
			RecordedAt: admission.AdmittedAt.Add(time.Hour),
		}
		if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
			return tx.RecordExecutionOutcome(ctx, outcome)
		}); err != nil {
			t.Fatalf("record outcome: %v", err)
		}

		_, err := s.db.ExecContext(ctx,
			`INSERT INTO execution_exports
			   (invocation_id, admission_id, observed_base_sha, head_sha, manifest_digest,
			    commit_plan_present, recorded_at, body)
			 VALUES (?, ?, ?, 'cafebabe', 'sha256:manifest', 0, ?, '{}')`,
			admission.InvocationID, admission.ID, admission.Base.BaseSHA,
			formatTime(admission.AdmittedAt.Add(time.Hour)))
		if err == nil {
			t.Fatal("raw export beside an outcome was accepted")
		}

		seedRawAdmission(t, ctx, s, admission)
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO execution_exports
			   (invocation_id, admission_id, observed_base_sha, head_sha, manifest_digest,
			    commit_plan_present, recorded_at, body)
			 VALUES ('inv-2', 'sha256:other', ?, 'cafebabe', 'sha256:manifest', 0, ?, '{}')`,
			admission.Base.BaseSHA, formatTime(admission.AdmittedAt.Add(time.Hour))); err != nil {
			t.Fatalf("seed raw export: %v", err)
		}
		if _, err := s.db.ExecContext(ctx,
			`UPDATE execution_exports
			 SET invocation_id = ?, admission_id = ?
			 WHERE invocation_id = 'inv-2'`,
			admission.InvocationID, admission.ID); err == nil {
			t.Fatal("raw export moved beside an outcome was accepted")
		}
	})
}

func seedRawAdmission(
	t *testing.T, ctx context.Context, s *Store, admission domain.ExecutionAdmission,
) {
	t.Helper()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO execution_admissions
		   (invocation_id, id, run_id, stage_id, attempt_id, operating_mode,
		    auth_identity_id, admitted_at, body)
		 SELECT 'inv-2', 'sha256:other', run_id, 'stage-2', 'attempt-2', operating_mode,
		        auth_identity_id, admitted_at, '{}'
		 FROM execution_admissions
		 WHERE invocation_id = ?`,
		admission.InvocationID)
	if err != nil {
		t.Fatalf("seed raw admission: %v", err)
	}
}

// seedAdmission opens a store with a floor, seeds a run and identity, records
// one admission, and returns the store for row-level tampering.
func seedAdmission(t *testing.T, waiver *domain.BackupEncryptionWaiver) (*Store, domain.ExecutionAdmission) {
	t.Helper()
	ctx := context.Background()
	// The configured waiver matches the fixture's canonical repository id, so
	// a waived fixture is admissible and a forged one is not.
	waiverID := int64(424242)
	s, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"), Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: tamperFloor(),
			domain.ModeUnattended:  tamperFloor(),
		},
		ApprovedCredentialModes:            []domain.CredentialMode{domain.CredentialSubscriptionContained},
		BackupEncryptionWaiverRepositoryID: &waiverID,
		BackupHealthSource: BackupHealthSourceFunc(func(
			context.Context, BackupHealthContext,
		) (domain.BackupHealth, error) {
			return domain.BackupHealth{
				Encryption:         domain.BackupHealthHealthy,
				CheckpointCurrency: domain.BackupHealthHealthy,
				ArtifactClosure:    domain.BackupHealthHealthy,
				RestoreTestAge:     domain.BackupHealthHealthy,
			}, nil
		}),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	run := domain.Run{
		ID: "run-1", ProjectID: "proj-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
		Stages: []domain.Stage{{
			ID: "stage-1", RunID: "run-1", Name: "implementation",
			Attempts: []domain.Attempt{{ID: "attempt-1", StageID: "stage-1", Number: 1, InvocationID: "inv-1"}},
		}},
	}
	identityID := domain.AuthIdentityID("auth-1")
	// A waived fixture is necessarily unattended, names the profile revision
	// it is gated against, and clears the fuller capability class that mode
	// requires.
	mode, caps := domain.ModeAttendedDev, domain.NewCapabilitySnapshot(domain.CapPostExitExport)
	var profileDigest *domain.Digest
	if waiver != nil {
		mode = domain.ModeUnattended
		ceiling, ok := domain.ProvableCapabilities(domain.BackendFreshVMReadOnlyVolumeHandoff)
		if !ok {
			t.Fatal("fresh-vm class has no registered ceiling")
		}
		caps = ceiling
		digest := tamperTrustProfile(t, "owner/repo", 424242).ProfileDigest
		profileDigest = &digest
	}
	backendConfigurationDigest := domain.Digest("")
	if mode == domain.ModeUnattended {
		backendConfigurationDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	}
	admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: "inv-1", RunID: run.ID, StageID: "stage-1", AttemptID: "attempt-1",
		Backend:                    "fresh_vm_read_only_volume_handoff",
		BackendConfigurationDigest: backendConfigurationDigest,
		Capabilities:               caps,
		OperatingMode:              mode,
		CredentialMode:             domain.CredentialSubscriptionContained,
		EgressProfile:              domain.EgressProviderOnly,
		ImageRef:                   domain.ImageRef("ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("ab", 32)),
		SpecDigest:                 run.SpecDigest, PolicyDigest: run.PolicyDigest, InputDigest: "sha256:input",
		Base:      domain.BaseRevision{Repo: "owner/repo", RepositoryID: 424242, BaseRef: "refs/heads/main", BaseSHA: "deadbeef"},
		Workspace: "ws-1", AuthIdentityID: &identityID,
		TrustProfileDigest:     profileDigest,
		BackupEncryptionWaiver: waiver,
		AdmittedAt:             time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("NewExecutionAdmission: %v", err)
	}
	if err := s.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if waiver != nil {
			profile := tamperTrustProfile(t, admission.Base.Repo, admission.Base.RepositoryID)
			if err := tx.RecordTrustProfile(ctx, profile, admission.AdmittedAt); err != nil {
				return err
			}
		}
		if err := tx.RecordAuthIdentity(ctx, domain.AuthIdentity{
			ID: identityID, Provider: "claude", AuthStoreMutationLease: true,
			AuthStoreVolume:       "provider-cred",
			MaxParallelExecutions: 1, RefreshStrategy: domain.RefreshOnDemand,
		}, admission.AdmittedAt); err != nil {
			return err
		}
		if admission.OperatingMode == domain.ModeUnattended {
			conformance, err := domain.NewBackendConformance(domain.BackendConformanceInput{
				Backend:             domain.BackendFreshVMReadOnlyVolumeHandoff,
				Outcome:             domain.ConformancePassed,
				ConfigurationDigest: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
				Capabilities:        admission.Capabilities,
				ProvedAt:            admission.AdmittedAt,
			})
			if err != nil {
				return err
			}
			if _, err := tx.RecordBackendConformance(ctx, conformance); err != nil {
				return err
			}
		}
		if waiver != nil {
			// Seed a pre-#305 durable record without exercising the current
			// write boundary, which correctly rejects all new waiver-bearing
			// admissions. Reconstruction is the behavior these fixtures test.
			body, err := encode(admission)
			if err != nil {
				return err
			}
			var identity any
			if admission.AuthIdentityID != nil {
				identity = *admission.AuthIdentityID
			}
			return tx.putImmutable(ctx, recordExecutionAdmissionSQL,
				[]any{
					admission.InvocationID, admission.ID, admission.RunID, admission.StageID,
					admission.AttemptID, admission.OperatingMode, identity,
					formatTime(admission.AdmittedAt), body,
				},
				selectExecutionAdmissionBodySQL, []any{admission.InvocationID}, body)
		}
		return tx.RecordExecutionAdmission(ctx, admission)
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return s, admission
}

// TestLegacyUnboundUnattendedAdmissionReconstructs proves that adding the
// backend-configuration binding does not make pre-binding durable history
// unreadable. The historical record reconstructs under its original content
// address, but the live conformance gate still refuses to dispatch it.
func TestLegacyUnboundUnattendedAdmissionReconstructs(t *testing.T) {
	ctx := context.Background()
	s, admission := seedAdmission(t, &domain.BackupEncryptionWaiver{
		RepositoryID: 424242,
		Reason:       "pre-binding fixture",
	})

	admission.BackendConfigurationDigest = ""
	id, err := admission.ComputeID()
	if err != nil {
		t.Fatalf("compute legacy admission id: %v", err)
	}
	admission.ID = id
	body, err := encode(admission)
	if err != nil {
		t.Fatalf("encode legacy admission: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE execution_admissions SET id = ?, body = ? WHERE invocation_id = ?`,
		admission.ID, body, admission.InvocationID,
	); err != nil {
		t.Fatalf("rewrite as legacy admission: %v", err)
	}
	// The encrypted build cannot configure the retired waiver. Recovery must
	// still reconstruct the old record once the four-part backup gate passes.
	s.admissionPolicy.BackupEncryptionWaiverRepositoryID = nil

	err = s.Read(ctx, func(tx *ReadTx) error {
		got, err := tx.GetExecutionAdmission(ctx, admission.InvocationID)
		if err != nil {
			return err
		}
		if got.BackendConfigurationDigest != "" {
			t.Errorf("legacy backend configuration digest = %q, want empty", got.BackendConfigurationDigest)
		}
		return tx.RequireBackendConformant(ctx, got)
	})
	if !errors.Is(err, domain.ErrAdmissionConfigurationMismatch) {
		t.Fatalf("legacy dispatch gate = %v, want %v", err, domain.ErrAdmissionConfigurationMismatch)
	}
}

// tamperTrustProfile is the approved profile a waived fixture is gated
// against; the fixture names its digest so the record matches the activation.
func tamperTrustProfile(t *testing.T, repo string, repositoryID int64) domain.AutomationTrustProfile {
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
			Mode: domain.ReviewFreesideInvoked, ConfigDigest: "sha256:review-config",
		},
	})
	if err != nil {
		t.Fatalf("NewAutomationTrustProfile: %v", err)
	}
	return profile
}

// tamperFloor is the floor the tampering tests admit under.
func tamperFloor() domain.CapabilitySnapshot {
	return domain.NewCapabilitySnapshot(domain.CapPostExitExport)
}

// TestExecutionAdmissionTamperedRowFailsClosed pins the self-certifying half:
// a body edited in place no longer resolves to its content address, so the
// row is refused rather than read as an admission nobody granted. It catches
// partial corruption and any edit that did not recompute the digest.
func TestExecutionAdmissionTamperedRowFailsClosed(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		body string
	}{
		{
			"widened capability class",
			`json_replace(body, '$.capabilities', json_array('supports_networkless_export', 'supports_post_exit_export'))`,
		},
		{"retargeted base", `json_set(body, '$.base.base_sha', 'f0rged')`},
		{"swapped image", `json_set(body, '$.image_ref', 'evil/agent@sha256:` + strings.Repeat("ef", 32) + `')`},
		{"widened egress", `json_set(body, '$.egress_profile', 'provider_web_read')`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := seedAdmission(t, nil)
			// The tampering expressions are literals from this test's own
			// table; there is no external input in the statement.
			stmt := `UPDATE execution_admissions SET body = ` + tc.body + ` WHERE invocation_id = 'inv-1'` //nolint:gosec // G202: fixed test literals
			if _, err := s.db.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("tamper: %v", err)
			}
			err := s.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.GetExecutionAdmission(ctx, "inv-1")
				return err
			})
			if !errors.Is(err, domain.ErrAdmissionInconsistent) {
				t.Fatalf("read of a tampered row = %v, want %v", err, domain.ErrAdmissionInconsistent)
			}
		})
	}
}

// TestExecutionAdmissionTamperedColumnFailsClosed covers the other half of the
// cross-check: an edit to an extracted column that leaves the body alone.
func TestExecutionAdmissionTamperedColumnFailsClosed(t *testing.T) {
	ctx := context.Background()
	s, _ := seedAdmission(t, nil)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE execution_admissions SET operating_mode = 'unattended' WHERE invocation_id = 'inv-1'`); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	err := s.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetExecutionAdmission(ctx, "inv-1")
		return err
	})
	if !errors.Is(err, errRowInconsistent) {
		t.Fatalf("read of a row whose column disagrees with its body = %v, want %v", err, errRowInconsistent)
	}
}

// TestForgedWaiverRowFailsClosed is the re-gate against the operator's
// configuration: a waiver written straight into the database, with its digest
// recomputed so the record certifies itself, still buys nothing unless the
// operator holds that exact waiver.
func TestForgedWaiverRowFailsClosed(t *testing.T) {
	ctx := context.Background()
	// The forged record is built the honest way and then written under a
	// daemon whose operator configured a waiver for a different repository:
	// this is the strongest form of the attack, since the digest matches.
	forged := &domain.BackupEncryptionWaiver{RepositoryID: 7, Reason: "forged"}
	s, admission := seedAdmission(t, &domain.BackupEncryptionWaiver{
		RepositoryID: 424242, Reason: "phase 1a.2",
	})
	other, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: admission.InvocationID, RunID: admission.RunID, StageID: admission.StageID,
		AttemptID: admission.AttemptID, Backend: admission.Backend,
		BackendConfigurationDigest: admission.BackendConfigurationDigest,
		Capabilities:               admission.Capabilities, OperatingMode: admission.OperatingMode,
		CredentialMode: admission.CredentialMode, EgressProfile: admission.EgressProfile,
		ImageRef: admission.ImageRef, SpecDigest: admission.SpecDigest,
		PolicyDigest: admission.PolicyDigest, InputDigest: admission.InputDigest,
		Base: admission.Base, Workspace: admission.Workspace,
		AuthIdentityID:         admission.AuthIdentityID,
		TrustProfileDigest:     admission.TrustProfileDigest,
		BackupEncryptionWaiver: forged,
		AdmittedAt:             admission.AdmittedAt,
	})
	if err != nil {
		t.Fatalf("NewExecutionAdmission: %v", err)
	}
	body, err := encode(other)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE execution_admissions SET id = ?, body = ? WHERE invocation_id = 'inv-1'`,
		other.ID, body); err != nil {
		t.Fatalf("write the forged row: %v", err)
	}

	err = s.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetExecutionAdmission(ctx, "inv-1")
		return err
	})
	if !errors.Is(err, domain.ErrWaiverRepositoryMismatch) {
		t.Fatalf("read of a self-consistent forged waiver = %v, want %v",
			err, domain.ErrWaiverRepositoryMismatch)
	}
}

// TestExecutionAdmissionAuthIdentityColumnCrossChecked pins the other half of
// the foreign key: the column is what the FK constrains, so reconstruction has
// to compare it with the body rather than taking the body's word for an
// identity binding no trusted row backs.
func TestExecutionAdmissionAuthIdentityColumnCrossChecked(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		stmt string
	}{
		{"column cleared", `UPDATE execution_admissions SET auth_identity_id = NULL WHERE invocation_id = 'inv-1'`},
		{"column retargeted", `UPDATE execution_admissions SET auth_identity_id = 'auth-2' WHERE invocation_id = 'inv-1'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := seedAdmission(t, nil)
			if _, err := s.db.ExecContext(ctx,
				`INSERT INTO auth_identities
				   (id, provider, auth_store_mutation_lease, max_parallel_executions,
				    refresh_strategy, supports_read_only_auth_snapshot, recorded_at, body)
				 VALUES ('auth-2', 'claude', 1, 1, 'refresh_on_demand', 0, '2026-01-02T03:04:05Z', '{}')`); err != nil {
				t.Fatalf("seed second identity: %v", err)
			}
			if _, err := s.db.ExecContext(ctx, tc.stmt); err != nil {
				t.Fatalf("tamper: %v", err)
			}
			err := s.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.GetExecutionAdmission(ctx, "inv-1")
				return err
			})
			if !errors.Is(err, errRowInconsistent) {
				t.Fatalf("read of a retargeted identity column = %v, want %v", err, errRowInconsistent)
			}
		})
	}
}

// TestLookupSeparatesAbsenceFromRefusal is the distinction the engine's
// acceptance path depends on. A waived admission whose trusted profile is gone
// fails its gate with a not-found of the gate's own, and a caller asking "is
// there a record?" must not read that as "there is none": doing so accepts
// output whose reconstruction explicitly failed closed.
func TestLookupSeparatesAbsenceFromRefusal(t *testing.T) {
	ctx := context.Background()
	waiver := &domain.BackupEncryptionWaiver{RepositoryID: 424242, Reason: "phase 1a.2"}
	s, _ := seedAdmission(t, waiver)

	// While the profile stands, the record reconstructs and is present.
	if err := s.Read(ctx, func(tx *ReadTx) error {
		_, found, err := tx.LookupExecutionAdmission(ctx, "inv-1")
		if err != nil {
			return err
		}
		if !found {
			t.Error("a recorded admission reported itself absent")
		}
		return nil
	}); err != nil {
		t.Fatalf("lookup with the profile in place: %v", err)
	}

	// The approved profile is what binds the repository's name to the waived
	// numeric id. Without it the pair is self-asserted again, so the gate
	// refuses.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM trust_profile_activations`); err != nil {
		t.Fatalf("drop activations: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM trust_profiles`); err != nil {
		t.Fatalf("drop profiles: %v", err)
	}

	var (
		found     bool
		lookupErr error
	)
	_ = s.Read(ctx, func(tx *ReadTx) error {
		_, found, lookupErr = tx.LookupExecutionAdmission(ctx, "inv-1")
		return nil
	})
	if lookupErr == nil {
		t.Fatal("a waived admission with no trusted profile must not reconstruct")
	}
	if found {
		t.Error("a refused admission reported itself as present")
	}
	if errors.Is(lookupErr, ErrNotFound) {
		t.Errorf("refusal %v reads as absence; acceptance would treat it as a legacy attempt", lookupErr)
	}
	if !errors.Is(lookupErr, ErrRepositoryUntrusted) {
		t.Errorf("refusal = %v, want %v", lookupErr, ErrRepositoryUntrusted)
	}
}

// TestExecutionExportAuditFieldsCrossChecked pins that every fact the export
// asserts is extracted and compared, not carried by the body alone. The export
// has no content address, so a column is what makes a partially edited row
// fail closed instead of reading back as evidence about the handoff.
func TestExecutionExportAuditFieldsCrossChecked(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		stmt string
	}{
		{
			"evidence claimed in the body alone",
			`UPDATE execution_exports
			 SET body = json_set(body, '$.evidence_manifest_digest', 'sha256:forged')
			 WHERE invocation_id = 'inv-1'`,
		},
		{
			"commit plan claimed in the body alone",
			`UPDATE execution_exports
			 SET body = json_set(body, '$.commit_plan_present', json('true'))
			 WHERE invocation_id = 'inv-1'`,
		},
		{
			"observed base rewritten in the body alone",
			`UPDATE execution_exports
			 SET body = json_set(body, '$.observed_base_sha', 'f0rged')
			 WHERE invocation_id = 'inv-1'`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, admission := seedAdmission(t, nil)
			export, err := domain.NewExecutionExport(domain.ExecutionExportInput{
				InvocationID: admission.InvocationID, AdmissionID: admission.ID,
				ObservedBaseSHA: admission.Base.BaseSHA, HeadSHA: "cafebabe",
				ManifestDigest: "sha256:manifest",
				RecordedAt:     admission.AdmittedAt.Add(time.Hour),
			})
			if err != nil {
				t.Fatalf("NewExecutionExport: %v", err)
			}
			if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
				return tx.RecordExecutionExport(ctx, export)
			}); err != nil {
				t.Fatalf("record export: %v", err)
			}
			if _, err := s.db.ExecContext(ctx, tc.stmt); err != nil {
				t.Fatalf("tamper: %v", err)
			}
			err = s.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.GetExecutionExport(ctx, "inv-1")
				return err
			})
			if !errors.Is(err, errRowInconsistent) {
				t.Fatalf("read of a partially edited export = %v, want %v", err, errRowInconsistent)
			}
		})
	}
}

// TestAdmissionFailsClosedOnAnUnreadableIdentity covers what the foreign key
// cannot: it proves a row exists, not that the declaration in it reconstructs.
// An admission naming an identity whose body is malformed must fail closed
// rather than let a replay dispatch under credential state nobody can read.
func TestAdmissionFailsClosedOnAnUnreadableIdentity(t *testing.T) {
	ctx := context.Background()
	s, _ := seedAdmission(t, nil)

	// The row keeps its key and its columns; only the declaration is ruined.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE auth_identities SET body = '{"identity":{},"recorded_at":"2026-01-02T03:04:05Z"}'
		 WHERE id = 'auth-1'`); err != nil {
		t.Fatalf("corrupt the identity: %v", err)
	}

	err := s.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetExecutionAdmission(ctx, "inv-1")
		return err
	})
	if err == nil {
		t.Fatal("an admission naming an unreadable identity reconstructed")
	}
	if !errors.Is(err, domain.ErrEmptyID) {
		t.Fatalf("read = %v, want the identity's own validation failure", err)
	}
}
