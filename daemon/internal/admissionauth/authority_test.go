package admissionauth_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/admissionauth"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestResolvedPathAllowlistBindsTheAdmittedPolicy(t *testing.T) {
	t.Parallel()
	newPolicy := func(t *testing.T, key, value string) domain.ResolvedPolicy {
		t.Helper()
		policy, err := domain.NewResolvedPolicy("run-policy", []domain.PolicyKey{{
			Key: key, Value: value,
			Provenance: domain.KeyProvenance{
				Source: domain.ProvenanceOverride,
				Digest: domain.Digest("sha256:" + strings.Repeat("ab", 32)),
			},
		}})
		if err != nil {
			t.Fatalf("NewResolvedPolicy: %v", err)
		}
		return policy
	}

	canonical := newPolicy(t, "paths", "daemon/**, docs/**")
	got, err := admissionauth.ResolvedPathAllowlist(
		canonical, canonical.Digest, []string{"daemon/**", "docs/**"},
	)
	if err != nil {
		t.Fatalf("canonical allowlist: %v", err)
	}
	if !slices.Equal(got, []string{"daemon/**", "docs/**"}) {
		t.Fatalf("canonical allowlist = %q", got)
	}
	missingPaths := newPolicy(t, "egress", "provider_only")
	matchEverything := newPolicy(t, "paths", "**/*")

	// A digest substitution is record corruption and must stay fatal. Every
	// other refusal compares the durable policy against this daemon's own
	// -allowed-paths, so it must be a mutable policy verdict the engine holds
	// on: dispatch returns the error up to Engine.Run, and a fatal one there
	// ends the reconcile loop, so a single mismatched task would exit
	// the daemon on every restart with no way to reconfigure out of it.
	tests := []struct {
		name       string
		policy     domain.ResolvedPolicy
		digest     domain.Digest
		configured []string
		want       error
		holds      bool
	}{
		{"digest substitution", canonical, domain.Digest("sha256:" + strings.Repeat("cd", 32)), []string{"daemon/**", "docs/**"}, domain.ErrParentKeyMismatch, false},
		{"broader daemon configuration", canonical, canonical.Digest, []string{"daemon/**", "docs/**", "scripts/**"}, domain.ErrPathBoundaryMismatch, true},
		{"different run policy", canonical, canonical.Digest, []string{"daemon/**"}, domain.ErrPathBoundaryMismatch, true},
		{"reordered run policy", canonical, canonical.Digest, []string{"docs/**", "daemon/**"}, domain.ErrPathBoundaryMismatch, true},
		{"missing paths", missingPaths, missingPaths.Digest, []string{"daemon/**", "docs/**"}, domain.ErrPathBoundaryMismatch, true},
		{"match everything", matchEverything, matchEverything.Digest, []string{"**/*"}, domain.ErrPathBoundaryMismatch, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := admissionauth.ResolvedPathAllowlist(tc.policy, tc.digest, tc.configured)
			if !errors.Is(err, tc.want) {
				t.Fatalf("ResolvedPathAllowlist error = %v, want %v", err, tc.want)
			}
			if got := engine.MutableAdmissionPolicyRefusal(err); got != tc.holds {
				t.Errorf("engine holds dispatch = %v, want %v (fatal here exits the daemon)", got, tc.holds)
			}
		})
	}
}

func TestReleasedImportKeepsCurrentPolicyAcrossBackendChange(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	health := domain.BackupHealth{
		Encryption: domain.BackupHealthHealthy, CheckpointCurrency: domain.BackupHealthHealthy,
		ArtifactClosure: domain.BackupHealthHealthy, RestoreTestAge: domain.BackupHealthHealthy,
	}
	fixture := admissionauth.NewSubmissionFixture(t, store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeUnattended: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		},
		ApprovedCredentialModes: []domain.CredentialMode{domain.CredentialSubscriptionContained},
		BackupHealthSource: store.BackupHealthSourceFunc(func(context.Context, store.BackupHealthContext) (domain.BackupHealth, error) {
			return health, nil
		}),
	})
	st, blobs := fixture.Store, fixture.Blobs
	production := fixture.SubmitProduction(t, "released-production", engine.ProductionPublication{
		Title: "Recover the result", Body: "## Why\n\nRecover completed work.\n",
		CommitAuthor: engine.ProductionCommitAuthor{AppSlug: "freeside-test", BotUserID: 12345},
	})
	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo: "example/repo", RepositoryID: 44,
		PRExecution: domain.PRExecutionAuditedSameRepo, CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions: domain.TokenPermissionsReadOnly, CommitPlan: domain.CommitPlanSingleCommit,
		MessageRuleset: domain.MessageRulesetGitHub1, WorkflowAuditDigest: "sha256:workflow-audit",
		Review: domain.ReviewSettings{Mode: domain.ReviewFreesideInvoked, ConfigDigest: "sha256:review"},
		ProtectedPaths: domain.ProtectedPathConfig{
			ExtraVerificationControlPatterns: []string{".freeside/recipe.yaml"},
		},
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
		if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
			_, err := tx.RecordBackendConformance(ctx, record)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	recordProof(oldDigest)
	identity := domain.AuthIdentity{
		ID: "released-auth", Provider: "claude", AuthStoreMutationLease: true,
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
	resolver := &admissionauth.CountingCommitAuthorResolver{Identity: publish.AppBotIdentity{AppSlug: "freeside-test", BotUserID: 12345}}
	authority := admissionauth.Authority{Store: st, Blobs: blobs, AllowedPaths: []string{"daemon/**"}, CommitAuthors: resolver}
	spec := exec.StartSpecFromAdmission(admission)
	if err := authority.AuthenticateStart(ctx, production.InvocationID, spec); err != nil {
		t.Fatalf("original start authority: %v", err)
	}
	// The writer's list comes from the profile import applies: the widened
	// recipe path is listed and no reviewer-instruction pattern is.
	protected, err := authority.ProtectedPatterns(ctx, production.InvocationID, spec)
	if err != nil {
		t.Fatalf("protected patterns: %v", err)
	}
	if !slices.Contains(protected, ".freeside/recipe.yaml") || slices.Contains(protected, "**/AGENTS.md") {
		t.Fatalf("protected patterns = %q", protected)
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
	if err != nil || !reflect.DeepEqual(opts.Policy.Allowlist, authority.AllowedPaths) {
		t.Fatalf("current import options = %#v, %v", opts, err)
	}
	if got := opts.Policy.PublishBlockingPatterns(); !slices.Equal(got, protected) {
		t.Fatalf("import policy blocks %q, writer was told %q", got, protected)
	}
	if resolver.RevalidateCalls == 0 {
		t.Fatal("current import omitted live author revalidation")
	}
	authority.AllowedPaths = []string{"different/**"}
	if err := authority.AuthenticateImport(ctx, production.InvocationID, spec); !errors.Is(err, domain.ErrPathBoundaryMismatch) {
		t.Fatalf("changed current path policy was not refused: %v", err)
	}
	if _, err := authority.ImportOptions(ctx, production.InvocationID, spec, importer.Options{}); !errors.Is(err, domain.ErrPathBoundaryMismatch) {
		t.Fatalf("import options bypassed current paths: %v", err)
	}
	authority.AllowedPaths = []string{"daemon/**"}
	health.Encryption = domain.BackupHealthUnhealthy
	if err := authority.AuthenticateImport(ctx, production.InvocationID, spec); !errors.Is(err, domain.ErrCheckpointNotEncrypted) {
		t.Fatalf("unhealthy backup was not refused: %v", err)
	}
}
