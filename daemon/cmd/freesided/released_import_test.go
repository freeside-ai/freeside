package main

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

func TestReleasedImportKeepsCurrentPolicyAcrossBackendChange(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	root := t.TempDir()
	work, policyFile, publicationFile := writeSubmissionInputs(t, root)
	cfg := submitCommandConfig{
		DBPath: filepath.Join(root, "freeside.db"), TaskPath: work,
		PolicyPath: policyFile, PublicationPath: publicationFile, ProjectID: "released-import",
	}
	submitted, err := runSubmitCommand(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	health := domain.BackupHealth{
		Encryption: domain.BackupHealthHealthy, CheckpointCurrency: domain.BackupHealthHealthy,
		ArtifactClosure: domain.BackupHealthHealthy, RestoreTestAge: domain.BackupHealthHealthy,
	}
	st := storetest.Open(t, cfg.DBPath, store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeUnattended: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		},
		ApprovedCredentialModes: []domain.CredentialMode{domain.CredentialSubscriptionContained},
		BackupHealthSource: store.BackupHealthSourceFunc(func(context.Context, store.BackupHealthContext) (domain.BackupHealth, error) {
			return health, nil
		}),
	})
	blobs, err := signet.NewBlobStore(cfg.DBPath + ".blobs")
	if err != nil {
		t.Fatal(err)
	}
	var submittedPolicy domain.ResolvedPolicy
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		submittedPolicy, err = tx.GetResolvedPolicy(ctx, submitted.SpecificationRunID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	policy, err := domain.NewResolvedPolicy("released-production", submittedPolicy.Keys)
	if err != nil {
		t.Fatal(err)
	}
	production, err := engine.SubmitProductionRun(ctx, st, engine.ProductionRunSpec{
		RunID: policy.RunID, ProjectID: submitted.ProjectID,
		SpecArtifactID: submitted.SourceArtifactID, PolicyArtifactID: submitted.SpecificationPolicyArtifactID,
		ResolvedPolicy: policy,
		Publication: engine.ProductionPublication{
			Title: "Recover the result", Body: "## Why\n\nRecover completed work.\n",
			CommitAuthor: engine.ProductionCommitAuthor{AppSlug: "freeside-test", BotUserID: 12345},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo: "example/repo", RepositoryID: 44,
		PRExecution: domain.PRExecutionAuditedSameRepo, CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions: domain.TokenPermissionsReadOnly, CommitPlan: domain.CommitPlanSingleCommit,
		MessageRuleset: domain.MessageRulesetGitHub1, WorkflowAuditDigest: "sha256:workflow-audit",
		Review: domain.ReviewSettings{Mode: domain.ReviewFreesideInvoked, ConfigDigest: "sha256:review"},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RecordInactiveTrustProfile(ctx, profile, now); err != nil {
			return err
		}
		return tx.ActivateTrustProfile(ctx, profile.Repo, profile.ProfileDigest, now)
	}); err != nil {
		t.Fatal(err)
	}
	capabilities, ok := domain.ProvableCapabilities(domain.BackendFreshVMReadOnlyVolumeHandoff)
	if !ok {
		t.Fatal("missing backend capabilities")
	}
	oldDigest := domain.Digest("sha256:" + strings.Repeat("1", 64))
	recordProof := func(digest domain.Digest) {
		t.Helper()
		record, err := domain.NewBackendConformance(domain.BackendConformanceInput{
			Backend: domain.BackendFreshVMReadOnlyVolumeHandoff, Outcome: domain.ConformancePassed,
			ConfigurationDigest: digest, Capabilities: capabilities, ProvedAt: now,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := (storeConformanceRecorder{store: st}).RecordBackendConformance(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	recordProof(oldDigest)
	identity := domain.AuthIdentity{
		ID: "released-auth", Provider: "claude", AuthStoreMutationLease: true, MaxParallelExecutions: 1,
		Interim: domain.InterimClientFacts{AuthStoreVolume: "test-volume", RefreshStrategy: domain.RefreshOnDemand},
	}
	run := production.Run
	var inputDigest domain.Digest
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		invocation, err := tx.GetAgentInvocation(ctx, production.InvocationID)
		if err != nil {
			return err
		}
		inputDigest, err = invocation.ComputeInputDigest()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	attempt := domain.Attempt{ID: "released-attempt", StageID: production.StageID, Number: 1, InvocationID: production.InvocationID}
	run.Stages[0].Attempts = []domain.Attempt{attempt}
	admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: production.InvocationID, RunID: run.ID, StageID: production.StageID, AttemptID: attempt.ID,
		Backend: string(domain.BackendFreshVMReadOnlyVolumeHandoff), BackendConfigurationDigest: oldDigest,
		Capabilities: capabilities, OperatingMode: domain.ModeUnattended, CredentialMode: domain.CredentialSubscriptionContained,
		EgressProfile: domain.EgressProviderOnly, TrustProfileDigest: &profile.ProfileDigest,
		ImageRef:   domain.ImageRef("example/agent@sha256:" + strings.Repeat("a", 64)),
		SpecDigest: run.SpecDigest, PolicyDigest: run.PolicyDigest, InputDigest: inputDigest,
		Base:      domain.BaseRevision{Repo: profile.Repo, RepositoryID: profile.RepositoryID, BaseRef: "main", BaseSHA: strings.Repeat("b", 40)},
		Workspace: "test-workspace", AuthIdentityID: &identity.ID, AdmittedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.RecordAuthIdentity(ctx, identity, now); err != nil {
			return err
		}
		return tx.RecordExecutionAdmission(ctx, admission)
	}); err != nil {
		t.Fatal(err)
	}
	resolver := &countingProductionCommitAuthorResolver{identity: publish.AppBotIdentity{AppSlug: "freeside-test", BotUserID: 12345}}
	authority := storeAdmissionAuthority{store: st, blobs: blobs, allowedPaths: []string{"daemon/**"}, commitAuthors: resolver}
	spec := exec.StartSpecFromAdmission(admission)
	if err := authority.AuthenticateStart(ctx, production.InvocationID, spec); err != nil {
		t.Fatalf("original start authority: %v", err)
	}
	now = now.Add(time.Minute)
	recordProof(domain.Digest("sha256:" + strings.Repeat("2", 64)))
	if err := authority.AuthenticateStart(ctx, production.InvocationID, spec); !errors.Is(err, domain.ErrAdmissionConfigurationMismatch) {
		t.Fatalf("changed backend allowed execution: %v", err)
	}
	if err := authority.AuthenticateImport(ctx, production.InvocationID, spec); err != nil {
		t.Fatalf("released import after backend change: %v", err)
	}
	opts, err := authority.ImportOptions(ctx, production.InvocationID, spec, importer.Options{})
	if err != nil || !reflect.DeepEqual(opts.Policy.Allowlist, authority.allowedPaths) {
		t.Fatalf("current import options = %#v, %v", opts, err)
	}
	if resolver.revalidateCalls == 0 {
		t.Fatal("current import omitted live author revalidation")
	}
	authority.allowedPaths = []string{"different/**"}
	if err := authority.AuthenticateImport(ctx, production.InvocationID, spec); !errors.Is(err, domain.ErrPathBoundaryMismatch) {
		t.Fatalf("changed current path policy was not refused: %v", err)
	}
	if _, err := authority.ImportOptions(ctx, production.InvocationID, spec, importer.Options{}); !errors.Is(err, domain.ErrPathBoundaryMismatch) {
		t.Fatalf("import options bypassed current paths: %v", err)
	}
	authority.allowedPaths = []string{"daemon/**"}
	health.Encryption = domain.BackupHealthUnhealthy
	if err := authority.AuthenticateImport(ctx, production.InvocationID, spec); !errors.Is(err, domain.ErrCheckpointNotEncrypted) {
		t.Fatalf("unhealthy backup was not refused: %v", err)
	}
}
