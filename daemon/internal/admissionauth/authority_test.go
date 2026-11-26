package admissionauth_test

import (
	"context"
	"errors"
	"fmt"
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

// TestAdmittedPathAllowlistAcceptsAWiderConfiguration pins the start and
// import rule: a run continues while every pattern it recorded is still
// configured, by exact pattern, and keeps its recorded paths.
func TestAdmittedPathAllowlistAcceptsAWiderConfiguration(t *testing.T) {
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
	recorded := newPolicy(t, "paths", "daemon/**, docs/**")
	nested := newPolicy(t, "paths", "daemon/internal/**")
	missingPaths := newPolicy(t, "egress", "provider_only")
	matchEverything := newPolicy(t, "paths", "**/*")

	for _, tc := range []struct {
		name       string
		policy     domain.ResolvedPolicy
		digest     domain.Digest
		configured []string
		want       error
		holds      bool
	}{
		{"equal", recorded, recorded.Digest, []string{"daemon/**", "docs/**"}, nil, false},
		{"reordered", recorded, recorded.Digest, []string{"docs/**", "daemon/**"}, nil, false},
		{"wider", recorded, recorded.Digest, []string{"daemon/**", "scripts/**", "docs/**"}, nil, false},
		{"narrower", recorded, recorded.Digest, []string{"daemon/**"}, domain.ErrPathBoundaryMismatch, true},
		// No glob reasoning: a broader pattern doesn't stand in for a
		// recorded narrower one.
		{"covering glob", nested, nested.Digest, []string{"daemon/**"}, domain.ErrPathBoundaryMismatch, true},
		{"digest substitution", recorded, domain.Digest("sha256:" + strings.Repeat("cd", 32)), []string{"daemon/**", "docs/**"}, domain.ErrParentKeyMismatch, false},
		{"missing paths", missingPaths, missingPaths.Digest, []string{"daemon/**"}, domain.ErrPathBoundaryMismatch, true},
		{"match everything", matchEverything, matchEverything.Digest, []string{"**/*"}, domain.ErrPathBoundaryMismatch, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := admissionauth.AdmittedPathAllowlist(tc.policy, tc.digest, tc.configured)
			if !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
				t.Fatalf("AdmittedPathAllowlist error = %v, want %v", err, tc.want)
			}
			if got := engine.MutableAdmissionPolicyRefusal(err); err != nil && got != tc.holds {
				t.Errorf("engine holds dispatch = %v, want %v", got, tc.holds)
			}
			if err == nil && !slices.Equal(got, []string{"daemon/**", "docs/**"}) {
				t.Fatalf("AdmittedPathAllowlist = %q, want the recorded paths", got)
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
			_, err := tx.RecordBackendConformance(ctx, domain.DaemonHostID, record)
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
	admissionInput := domain.ExecutionAdmissionInput{
		InvocationID: production.InvocationID, RunID: run.ID, StageID: production.StageID, AttemptID: attempt.ID,
		Backend: string(domain.BackendFreshVMReadOnlyVolumeHandoff), BackendConfigurationDigest: oldDigest,
		Capabilities: capabilities, OperatingMode: domain.ModeUnattended, CredentialMode: domain.CredentialSubscriptionContained,
		EgressProfile: domain.EgressProviderOnly, TrustProfileDigest: &profile.ProfileDigest,
		ImageRef:   domain.ImageRef("example/agent@sha256:" + strings.Repeat("a", 64)),
		SpecDigest: run.SpecDigest, PolicyDigest: run.PolicyDigest, InputDigest: inputDigest,
		Base:      domain.BaseRevision{Repo: profile.Repo, RepositoryID: profile.RepositoryID, BaseRef: "main", BaseSHA: strings.Repeat("b", 40)},
		Workspace: "test-workspace", AuthIdentityID: &identity.ID, AdmittedAt: now,
	}
	admission, err := domain.NewExecutionAdmission(admissionInput)
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
	running := oldDigest
	authority := admissionauth.Authority{
		Store: st, Blobs: blobs, Projects: projectPaths(admission.Base, "daemon/**"), CommitAuthors: resolver,
		BackendConfigurationDigestFor: func(class domain.RunnerBackendClass, image string) domain.Digest {
			if class != domain.RunnerBackendClass(admission.Backend) || image != string(admission.ImageRef) {
				return domain.Digest("sha256:" + strings.Repeat("9", 64))
			}
			return running
		},
	}
	spec := exec.StartSpecFromAdmission(admission)
	if err := authority.AuthenticateStart(ctx, production.InvocationID, spec); err != nil {
		t.Fatalf("original start authority: %v", err)
	}
	// A wider allowlist edit lets the run start and import under its own
	// recorded paths. A narrower edit that drops its pattern refuses both
	// until the pattern is restored.
	authority.Projects = projectPaths(admission.Base, "docs/**", "daemon/**")
	if err := authority.AuthenticateStart(ctx, production.InvocationID, spec); err != nil {
		t.Fatalf("start after a wider edit: %v", err)
	}
	if opts, err := authority.ImportOptions(ctx, production.InvocationID, spec, importer.Options{}); err != nil ||
		!slices.Equal(opts.Policy.Allowlist, []string{"daemon/**"}) {
		t.Fatalf("import after a wider edit = %q, %v; want the recorded daemon/**", opts.Policy.Allowlist, err)
	}
	authority.Projects = projectPaths(admission.Base, "docs/**")
	if err := authority.AuthenticateStart(ctx, production.InvocationID, spec); !errors.Is(err, domain.ErrPathBoundaryMismatch) {
		t.Fatalf("start after a narrower edit = %v, want ErrPathBoundaryMismatch", err)
	}
	if _, err := authority.ImportOptions(ctx, production.InvocationID, spec, importer.Options{}); !errors.Is(err, domain.ErrPathBoundaryMismatch) {
		t.Fatalf("import after a narrower edit = %v, want ErrPathBoundaryMismatch", err)
	}
	authority.Projects = projectPaths(admission.Base, "daemon/**")
	if err := authority.AuthenticateStart(ctx, production.InvocationID, spec); err != nil {
		t.Fatalf("start after the pattern is restored: %v", err)
	}
	// The run is checked against its own project's record, never another
	// project's (#3209): a second project that allows the run's pattern
	// doesn't admit it while its own project's record drops it.
	own := engine.ProjectAuthority{
		ProjectID: run.ProjectID, Repo: admission.Base.Repo, RepositoryID: admission.Base.RepositoryID,
		BaseRef: admission.Base.BaseRef, AllowedPaths: []string{"docs/**"},
	}
	other := own
	other.ProjectID, other.AllowedPaths = "project-other", []string{"daemon/**"}
	authority.Projects = projectsByID(own, other)
	if err := authority.AuthenticateStart(ctx, production.InvocationID, spec); !errors.Is(err, domain.ErrPathBoundaryMismatch) {
		t.Fatalf("start under its own project's narrower paths = %v, want ErrPathBoundaryMismatch", err)
	}
	own.AllowedPaths, other.AllowedPaths = []string{"daemon/**"}, []string{"docs/**"}
	authority.Projects = projectsByID(own, other)
	if err := authority.AuthenticateStart(ctx, production.InvocationID, spec); err != nil {
		t.Fatalf("start under its own project's paths beside another's: %v", err)
	}
	// A repository edit to the run's project refuses its start and import
	// with the existing mismatch, and a project whose settings grant
	// nothing refuses both.
	moved := own
	moved.RepositoryID++
	authority.Projects = projectsByID(moved, other)
	if err := authority.AuthenticateStart(ctx, production.InvocationID, spec); !errors.Is(err, domain.ErrRepositoryIdentityMismatch) {
		t.Fatalf("start after a repository edit = %v, want ErrRepositoryIdentityMismatch", err)
	}
	if _, err := authority.ImportOptions(ctx, production.InvocationID, spec, importer.Options{}); !errors.Is(err, domain.ErrRepositoryIdentityMismatch) {
		t.Fatalf("import after a repository edit = %v, want ErrRepositoryIdentityMismatch", err)
	}
	authority.Projects = func(context.Context, *store.ReadTx, domain.ProjectID) (engine.ProjectAuthority, error) {
		return engine.ProjectAuthority{}, errors.Join(engine.ErrProjectSettingsRefused, errors.New("no settings record"))
	}
	if err := authority.AuthenticateStart(ctx, production.InvocationID, spec); !errors.Is(err, engine.ErrProjectSettingsRefused) {
		t.Fatalf("start of a project with no settings = %v, want ErrProjectSettingsRefused", err)
	}
	if _, err := authority.ImportOptions(ctx, production.InvocationID, spec, importer.Options{}); !errors.Is(err, engine.ErrProjectSettingsRefused) {
		t.Fatalf("import of a project with no settings = %v, want ErrProjectSettingsRefused", err)
	}
	authority.Projects = projectPaths(admission.Base, "daemon/**")
	// The writer's list comes from the profile import applies: the widened
	// recipe path is listed and no reviewer-instruction pattern is.
	protected, err := authority.ProtectedPatterns(ctx, production.InvocationID, spec)
	if err != nil {
		t.Fatalf("protected patterns: %v", err)
	}
	if !slices.Contains(protected, ".freeside/recipe.yaml") || slices.Contains(protected, "**/AGENTS.md") {
		t.Fatalf("protected patterns = %q", protected)
	}
	// A newer proof for another configuration, another writer image's, is
	// not a change to this admission's: it still starts (#3211).
	now = now.Add(time.Minute)
	newDigest := domain.Digest("sha256:" + strings.Repeat("2", 64))
	recordProof(newDigest)
	if err := authority.AuthenticateStart(ctx, production.InvocationID, spec); err != nil {
		t.Fatalf("start beside another configuration's newer proof: %v", err)
	}
	// A process that yields another digest for the admission's image, as one
	// restarted with a changed runtime or binary does, refuses to start or
	// recover it even though the admission's own proof still stands.
	running = newDigest
	if err := authority.AuthenticateStart(ctx, production.InvocationID, spec); !errors.Is(err, domain.ErrAdmissionConfigurationMismatch) {
		t.Fatalf("changed backend allowed execution: %v", err)
	}
	withoutDigest := authority
	withoutDigest.BackendConfigurationDigestFor = nil
	if err := withoutDigest.AuthenticateStart(ctx, production.InvocationID, spec); !errors.Is(err, domain.ErrAdmissionConfigurationMismatch) {
		t.Fatalf("authority with no backend configuration allowed execution: %v", err)
	}
	if err := authority.AuthenticateImport(ctx, production.InvocationID, spec); err != nil {
		t.Fatalf("released import after backend change: %v", err)
	}
	opts, err := authority.ImportOptions(ctx, production.InvocationID, spec, importer.Options{})
	if err != nil || !reflect.DeepEqual(opts.Policy.Allowlist, []string{"daemon/**"}) {
		t.Fatalf("current import options = %#v, %v", opts, err)
	}
	if got := opts.Policy.PublishBlockingPatterns(); !slices.Equal(got, protected) {
		t.Fatalf("import policy blocks %q, writer was told %q", got, protected)
	}
	if resolver.RevalidateCalls == 0 {
		t.Fatal("current import omitted live author revalidation")
	}
	authority.Projects = projectPaths(admission.Base, "different/**")
	if err := authority.AuthenticateImport(ctx, production.InvocationID, spec); !errors.Is(err, domain.ErrPathBoundaryMismatch) {
		t.Fatalf("changed current path policy was not refused: %v", err)
	}
	if _, err := authority.ImportOptions(ctx, production.InvocationID, spec, importer.Options{}); !errors.Is(err, domain.ErrPathBoundaryMismatch) {
		t.Fatalf("import options bypassed current paths: %v", err)
	}
	authority.Projects = projectPaths(admission.Base, "daemon/**")
	health.Encryption = domain.BackupHealthUnhealthy
	if err := authority.AuthenticateImport(ctx, production.InvocationID, spec); !errors.Is(err, domain.ErrCheckpointNotEncrypted) {
		t.Fatalf("unhealthy backup was not refused: %v", err)
	}
	// The engine re-admits a never-started attempt under the running
	// configuration (#3433). The superseding admission, bound to the digest
	// this process yields, starts; the gate itself is unchanged.
	health.Encryption = domain.BackupHealthHealthy
	readmittedInput := admissionInput
	readmittedInput.BackendConfigurationDigest = newDigest
	readmittedInput.AdmittedAt = now.Add(time.Minute)
	readmitted, err := domain.NewExecutionAdmission(readmittedInput)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.SupersedeUnstartedExecutionAdmission(ctx, admission, readmitted, nil)
	}); err != nil {
		t.Fatalf("supersede the unstarted admission: %v", err)
	}
	if err := authority.AuthenticateStart(ctx, production.InvocationID, exec.StartSpecFromAdmission(readmitted)); err != nil {
		t.Fatalf("start of the superseding admission: %v", err)
	}
}

// projectsByID answers each project with its own authority, and refuses one
// it doesn't name.
func projectsByID(authorities ...engine.ProjectAuthority) engine.ProjectAuthoritySource {
	return func(_ context.Context, _ *store.ReadTx, project domain.ProjectID) (engine.ProjectAuthority, error) {
		for _, authority := range authorities {
			if authority.ProjectID == project {
				return authority, nil
			}
		}
		return engine.ProjectAuthority{}, errors.Join(engine.ErrProjectSettingsRefused, fmt.Errorf("no project %q", project))
	}
}

// projectPaths stands in for the run's project settings: the admitted
// base's repository with paths as its allowed paths.
func projectPaths(base domain.BaseRevision, paths ...string) engine.ProjectAuthoritySource {
	return engine.FixedProjectAuthority(engine.ProjectAuthority{
		Repo: base.Repo, RepositoryID: base.RepositoryID, BaseRef: base.BaseRef, AllowedPaths: paths,
	})
}
