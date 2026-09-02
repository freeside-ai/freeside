package engine

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

// TestRecordExecutionExportSpecificationArmMintsNoPublicationTask covers the
// latent blocker in issue #768 decision 4: an unattended specification run
// produces an export, but its ownership marker is inv-specify-*, not the
// inv-implement-* marker loadProductionRequest authenticates. Before this arm,
// the unattended branch would fail there and the export-record step would retry
// forever. The arm records the export export-only and mints no
// KindProductionPublicationRequested task, so the specification commit never
// reaches the publication lane.
func TestRecordExecutionExportSpecificationArmMintsNoPublicationTask(t *testing.T) {
	ctx := t.Context()
	const (
		repo   = "owner/repo"
		repoID = int64(424242)
		runID  = domain.RunID("specification-run")
	)
	invocationID := specificationInvocationID(runID, 1)
	stageID := specificationStageID(runID)
	epoch := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"), store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
			domain.ModeUnattended:  domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		},
		ApprovedCredentialModes: []domain.CredentialMode{domain.CredentialSubscriptionContained},
		BackupHealthSource: store.BackupHealthSourceFunc(func(
			context.Context, store.BackupHealthContext,
		) (domain.BackupHealth, error) {
			return domain.BackupHealth{
				Encryption: domain.BackupHealthHealthy, CheckpointCurrency: domain.BackupHealthHealthy,
				ArtifactClosure: domain.BackupHealthHealthy, RestoreTestAge: domain.BackupHealthHealthy,
			}, nil
		}),
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ceiling, ok := domain.ProvableCapabilities(domain.BackendFreshVMReadOnlyVolumeHandoff)
	if !ok {
		t.Fatal("fresh-vm class has no registered ceiling")
	}
	const configDigest = domain.Digest("sha256:1111111111111111111111111111111111111111111111111111111111111111")

	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo: repo, RepositoryID: repoID,
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
	conformance, err := domain.NewBackendConformance(domain.BackendConformanceInput{
		Backend: domain.BackendFreshVMReadOnlyVolumeHandoff, Outcome: domain.ConformancePassed,
		ConfigurationDigest: configDigest, Capabilities: ceiling, ProvedAt: epoch,
	})
	if err != nil {
		t.Fatalf("NewBackendConformance: %v", err)
	}

	identityID := domain.AuthIdentityID("auth-1")
	run := domain.Run{
		ID: runID, ProjectID: "proj-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
		Stages: []domain.Stage{{
			ID: stageID, RunID: runID, Name: "specification",
			Attempts: []domain.Attempt{{
				ID: attemptIDFor(invocationID), StageID: stageID, Number: 1, InvocationID: invocationID,
			}},
		}},
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RecordTrustProfile(ctx, profile, epoch); err != nil {
			return err
		}
		_, err := tx.RecordBackendConformance(ctx, conformance)
		return err
	}); err != nil {
		t.Fatalf("seed trust profile and conformance: %v", err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		return tx.RecordAuthIdentity(ctx, domain.AuthIdentity{
			ID: identityID, Provider: "claude", AuthStoreMutationLease: true, MaxParallelExecutions: 1,
			Interim: domain.InterimClientFacts{AuthStoreVolume: "provider-cred", RefreshStrategy: domain.RefreshOnDemand},
		}, epoch)
	}); err != nil {
		t.Fatalf("seed run and identity: %v", err)
	}

	admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: invocationID, RunID: runID, StageID: stageID, AttemptID: attemptIDFor(invocationID),
		Backend: string(domain.BackendFreshVMReadOnlyVolumeHandoff), BackendConfigurationDigest: configDigest,
		Capabilities: ceiling, OperatingMode: domain.ModeUnattended,
		CredentialMode: domain.CredentialSubscriptionContained, EgressProfile: domain.EgressProviderOnly,
		ImageRef:   domain.ImageRef("ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("ab", 32)),
		SpecDigest: run.SpecDigest, PolicyDigest: run.PolicyDigest, InputDigest: "sha256:input",
		Base:           domain.BaseRevision{Repo: repo, RepositoryID: repoID, BaseRef: "refs/heads/main", BaseSHA: "deadbeef"},
		Workspace:      "ws-1",
		AuthIdentityID: &identityID, TrustProfileDigest: &profile.ProfileDigest,
		AdmittedAt: epoch,
	})
	if err != nil {
		t.Fatalf("NewExecutionAdmission: %v", err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.RecordExecutionAdmission(ctx, admission)
	}); err != nil {
		t.Fatalf("record unattended specification admission: %v", err)
	}

	export, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: invocationID, AdmissionID: admission.ID,
		ObservedBaseSHA: admission.Base.BaseSHA, HeadSHA: "cafebabe",
		ManifestDigest: "sha256:manifest", RecordedAt: epoch.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("NewExecutionExport: %v", err)
	}
	// The specification and attended arms ignore the replay (only the production
	// path consumes it), so the export-only write needs no populated replay.
	if err := RecordExecutionExport(ctx, st, export, ProductionReplay{}); err != nil {
		t.Fatalf("record unattended specification export: %v", err)
	}

	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetExecutionExportRecord(ctx, invocationID); err != nil {
			return err
		}
		// No publication task: the specification commit never enters the lane.
		_, err := tx.GetOutbox(ctx, productionPublicationTaskKey(runID))
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("production publication task = %v, want none", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("verify export-only record: %v", err)
	}
}
