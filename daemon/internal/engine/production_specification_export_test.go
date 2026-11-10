package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// unattendedExportStoreOptions lets a test store record an unattended
// admission next to the attended ones its engine writes, so a test can drive
// RecordExecutionExport's unattended arm without an unattended engine.
func unattendedExportStoreOptions() store.Options {
	return store.Options{
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
	}
}

const unattendedConfigDigest = domain.Digest("sha256:1111111111111111111111111111111111111111111111111111111111111111")

var unattendedExportBase = domain.BaseRevision{
	Repo: "owner/repo", RepositoryID: 424242, BaseRef: "refs/heads/main", BaseSHA: "deadbeef",
}

var unattendedExportEpoch = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// seedUnattendedAdmissionTrust records base's repository trust profile and the
// backend conformance an unattended admission cites, and returns the profile
// digest.
func seedUnattendedAdmissionTrust(t *testing.T, st *store.Store, base domain.BaseRevision) domain.Digest {
	t.Helper()
	ceiling, ok := domain.ProvableCapabilities(domain.BackendFreshVMReadOnlyVolumeHandoff)
	if !ok {
		t.Fatal("fresh-vm class has no registered ceiling")
	}
	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo: base.Repo, RepositoryID: base.RepositoryID,
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
		ConfigurationDigest: unattendedConfigDigest, Capabilities: ceiling, ProvedAt: unattendedExportEpoch,
	})
	if err != nil {
		t.Fatalf("NewBackendConformance: %v", err)
	}
	if err := st.WriteInternal(t.Context(), func(tx *store.InternalTx) error {
		if err := tx.RecordTrustProfile(t.Context(), profile, unattendedExportEpoch); err != nil {
			return err
		}
		_, err := tx.RecordBackendConformance(t.Context(), conformance)
		return err
	}); err != nil {
		t.Fatalf("seed trust profile and conformance: %v", err)
	}
	return profile.ProfileDigest
}

// recordUnattendedAdmission records an unattended admission of invocationID
// under run's stageID at base; the run must already hold its attempt, and
// inputDigest must match the invocation's input binding when it has one.
func recordUnattendedAdmission(
	t *testing.T, st *store.Store, run domain.Run, stageID domain.StageID, base domain.BaseRevision,
	invocationID domain.InvocationID, inputDigest domain.Digest,
	identityID domain.AuthIdentityID, profileDigest domain.Digest,
) domain.ExecutionAdmission {
	t.Helper()
	ceiling, _ := domain.ProvableCapabilities(domain.BackendFreshVMReadOnlyVolumeHandoff)
	admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: invocationID, RunID: run.ID, StageID: stageID, AttemptID: attemptIDFor(invocationID),
		Backend: string(domain.BackendFreshVMReadOnlyVolumeHandoff), BackendConfigurationDigest: unattendedConfigDigest,
		Capabilities: ceiling, OperatingMode: domain.ModeUnattended,
		CredentialMode: domain.CredentialSubscriptionContained, EgressProfile: domain.EgressProviderOnly,
		ImageRef:   domain.ImageRef("ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("ab", 32)),
		SpecDigest: run.SpecDigest, PolicyDigest: run.PolicyDigest, InputDigest: inputDigest,
		Base:           base,
		Workspace:      "ws-1",
		AuthIdentityID: &identityID, TrustProfileDigest: &profileDigest,
		AdmittedAt: unattendedExportEpoch,
	})
	if err != nil {
		t.Fatalf("NewExecutionAdmission: %v", err)
	}
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.RecordExecutionAdmission(t.Context(), admission)
	}); err != nil {
		t.Fatalf("record unattended admission: %v", err)
	}
	return admission
}

func unattendedExecutionExport(t *testing.T, admission domain.ExecutionAdmission) domain.ExecutionExport {
	t.Helper()
	export, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: admission.InvocationID, AdmissionID: admission.ID,
		ObservedBaseSHA: admission.Base.BaseSHA, HeadSHA: "cafebabe",
		ManifestDigest: "sha256:manifest", RecordedAt: unattendedExportEpoch.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("NewExecutionExport: %v", err)
	}
	return export
}

// requireNoPublicationTask fails t when any publication task is pending,
// whatever run its key names, so an export-only arm can't pass by minting a
// task under a key the test didn't look up.
func requireNoPublicationTask(ctx context.Context, t *testing.T, tx *store.ReadTx) error {
	t.Helper()
	tasks, err := tx.ListPendingOutbox(ctx, KindProductionPublicationRequested)
	if err != nil {
		return err
	}
	if len(tasks) != 0 {
		t.Fatalf("production publication tasks = %+v, want none", tasks)
	}
	return nil
}

// TestRecordExecutionExportSpecificationArmMintsNoPublicationTask covers the
// latent blocker in issue #768 decision 4: an unattended specification run
// produces an export, but its ownership marker is inv-specify-*, not the
// inv-implement-* marker loadProductionRequest authenticates. Before this arm,
// the unattended branch would fail there and the export-record step would retry
// forever. The arm records the export export-only and mints no
// KindProductionPublicationRequested task, so the specification commit never
// reaches the publication lane.
func TestRecordExecutionExportSpecificationArmMintsNoPublicationTask(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	const runID = domain.RunID("specification-run")
	invocationID := specificationInvocationID(runID, 1)
	stageID := specificationStageID(runID)

	st := storetest.Open(t, filepath.Join(t.TempDir(), "state.db"), unattendedExportStoreOptions())
	profileDigest := seedUnattendedAdmissionTrust(t, st, unattendedExportBase)

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
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		return tx.RecordAuthIdentity(ctx, domain.AuthIdentity{
			ID: identityID, Provider: "claude", AuthStoreMutationLease: true,
			Interim: domain.InterimClientFacts{AuthStoreVolume: "provider-cred", RefreshStrategy: domain.RefreshOnDemand},
		}, unattendedExportEpoch)
	}); err != nil {
		t.Fatalf("seed run and identity: %v", err)
	}
	admission := recordUnattendedAdmission(t, st, run, stageID, unattendedExportBase, invocationID, "sha256:input", identityID, profileDigest)

	// The specification and attended arms read only the replay's usage (the
	// production path consumes the rest), so the export-only write needs no
	// populated replay.
	if err := RecordExecutionExport(ctx, st, unattendedExecutionExport(t, admission), ProductionReplay{}, nil, nil); err != nil {
		t.Fatalf("record unattended specification export: %v", err)
	}

	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetExecutionExportRecord(ctx, invocationID); err != nil {
			return err
		}
		// No publication task: the specification commit never enters the lane.
		return requireNoPublicationTask(ctx, t, tx)
	}); err != nil {
		t.Fatalf("verify export-only record: %v", err)
	}
}
