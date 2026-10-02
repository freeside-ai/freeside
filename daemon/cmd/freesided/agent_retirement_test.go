package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// TestAuthAdoptRetiresOnlyTheNamedUnadoptableIdentity covers the retirement
// write. Without the flag an unadoptable identity is untouched (the adoption
// tests assert that). Naming it disables it and changes nothing else about
// it. Naming an identity this run adopts is refused and leaves it enabled,
// so a forgotten argument cannot retire an adoptable identity.
func TestAuthAdoptRetiresOnlyTheNamedUnadoptableIdentity(t *testing.T) {
	f := newAuthAdoptFixture(t)
	f.writeCodexStore(t, "")
	before := f.snapshot(t).identity(t, "codex-review")

	if _, _, err := f.run(t, f.args("-retire-unadoptable", "someone-else")); err == nil {
		t.Fatal("auth adopt accepted a -retire-unadoptable identity it does not adopt")
	}
	report, _, err := f.run(t, f.args("-retire-unadoptable", "claude-main"))
	if err == nil || !strings.Contains(err.Error(), "retires only an unadoptable identity") {
		t.Fatalf("retiring an adopted identity = %v", err)
	}
	if len(report.Identities) == 0 || report.Identities[0].Retired {
		t.Fatalf("report = %+v", report)
	}
	if got := f.snapshot(t); !got.identity(t, "claude-main").Enabled || got.identity(t, "codex-review") != before {
		t.Fatalf("a refused retirement changed an identity: %+v", got)
	}

	report, _, err = f.run(t, f.args("-retire-unadoptable", "codex-review"))
	if err != nil {
		t.Fatalf("auth adopt: %v", err)
	}
	if len(report.Identities) != 2 || report.Identities[0].Retired ||
		report.Identities[1].Status != authAdoptUnadoptable || !report.Identities[1].Retired ||
		len(report.Identities[1].StoppedTasks) != 0 {
		t.Fatalf("report = %+v", report)
	}
	after := f.snapshot(t)
	want := before
	want.Enabled = false
	if got := after.identity(t, "codex-review"); got != want {
		t.Fatalf("retired identity = %+v, want %+v", got, want)
	}
	if !after.identity(t, "claude-main").Enabled || len(after.Enrollments) != 1 {
		t.Fatalf("adopted identity or enrollments changed: %+v", after)
	}
	// A rerun reports the identity unadoptable again and writes nothing.
	report, _, err = f.run(t, f.args("-retire-unadoptable", "codex-review"))
	if err != nil || !report.Identities[1].Retired {
		t.Fatalf("second auth adopt = %+v, %v", report, err)
	}
	if got := f.snapshot(t).identity(t, "codex-review"); got != want {
		t.Fatalf("retired identity after a rerun = %+v", got)
	}
}

// TestAuthAdoptRetiresAnUnadoptableIdentityStoredDisabled names an
// unadoptable identity that was disabled before the run. Its adoption is
// refused inside the first-enrollment transaction, the one that enables an
// adopted identity, so it stays disabled and is reported retired.
func TestAuthAdoptRetiresAnUnadoptableIdentityStoredDisabled(t *testing.T) {
	f := newAuthAdoptFixture(t)
	f.recordIdentity(t, domain.AuthIdentity{
		ID: "codex-other", Provider: "openai", AccountBinding: adoptCodexAccount,
		MaxParallelExecutions: 1, Enabled: true, CostOwner: "operator",
	})
	f.withStore(t, func(st *store.Store) { setIdentityEnabled(t, st, "codex-review", false) })
	before := f.snapshot(t).identity(t, "codex-review")

	report, _, err := f.run(t, f.args("-retire-unadoptable", "codex-review"))
	if err != nil {
		t.Fatalf("auth adopt: %v", err)
	}
	if len(report.Identities) != 2 || report.Identities[1].Status != authAdoptUnadoptable ||
		!report.Identities[1].Retired || report.Identities[1].Enabled {
		t.Fatalf("report = %+v", report)
	}
	if got := f.snapshot(t).identity(t, "codex-review"); got != before || got.Enabled {
		t.Fatalf("retired identity = %+v, want %+v", got, before)
	}
}

// TestRetiredOwnershipFollowsTheNewestAdmission: a task whose newest
// admission names another identity is no longer the retired identity's.
func TestRetiredOwnershipFollowsTheNewestAdmission(t *testing.T) {
	ctx := context.Background()
	st, taskID := legacyAdmittedTask(t, "flag-era-auth")
	if owned := legacyOwners(t, st); len(owned) != 1 || len(owned["flag-era-auth"]) != 1 {
		t.Fatalf("owners before the later admission = %v", owned)
	}

	later := domain.AuthIdentity{
		ID: "later-auth", Provider: "claude", AuthStoreMutationLease: true, MaxParallelExecutions: 1, Enabled: true,
		Interim: domain.InterimClientFacts{AuthStoreVolume: "later-volume", RefreshStrategy: domain.RefreshOnDemand},
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		runs, err := tx.TaskRunIDs(ctx, taskID)
		if err != nil {
			return err
		}
		var first domain.ExecutionAdmission
		for _, id := range runs {
			admissions, err := tx.ListRunExecutionAdmissionRecords(ctx, id)
			if err != nil {
				return err
			}
			if len(admissions) != 0 {
				first = admissions[0]
			}
		}
		run, err := tx.GetRun(ctx, first.RunID)
		if err != nil {
			return err
		}
		invocation, err := tx.GetAgentInvocation(ctx, first.InvocationID)
		if err != nil {
			return err
		}
		invocation.ID = "later-invocation"
		if err := tx.PutAgentInvocation(ctx, invocation); err != nil {
			return err
		}
		inputDigest, err := invocation.ComputeInputDigest()
		if err != nil {
			return err
		}
		attempt := domain.Attempt{
			ID: "later-attempt", StageID: first.StageID, Number: 2, InvocationID: invocation.ID,
		}
		run.Stages[0].Attempts = append(run.Stages[0].Attempts, attempt)
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.RecordAuthIdentity(ctx, later, first.AdmittedAt); err != nil {
			return err
		}
		admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
			InvocationID: invocation.ID, RunID: first.RunID, StageID: first.StageID, AttemptID: attempt.ID,
			Backend: first.Backend, BackendConfigurationDigest: first.BackendConfigurationDigest,
			Capabilities: first.Capabilities, OperatingMode: first.OperatingMode,
			CredentialMode: first.CredentialMode, EgressProfile: first.EgressProfile,
			TrustProfileDigest: first.TrustProfileDigest, ImageRef: first.ImageRef,
			SpecDigest: first.SpecDigest, PolicyDigest: first.PolicyDigest, InputDigest: inputDigest,
			Base: first.Base, Workspace: "later-workspace", AuthIdentityID: &later.ID,
			AdmittedAt: first.AdmittedAt.Add(time.Minute),
		})
		if err != nil {
			return err
		}
		return tx.RecordExecutionAdmission(ctx, admission)
	}); err != nil {
		t.Fatalf("record the later admission: %v", err)
	}
	if owned := legacyOwners(t, st); len(owned) != 1 || len(owned["later-auth"]) != 1 {
		t.Fatalf("owners after a later admission under another identity = %v", owned)
	}
	if stopped, err := stopRetiredTasks(ctx, st, "flag-era-auth", time.Now().UTC()); err != nil || len(stopped) != 0 {
		t.Fatalf("stopRetiredTasks = %v, %v", stopped, err)
	}
}

// TestStopRetiredTasksRecordsOneStopPerTask: retirement stops the open task
// the identity owns, a rerun records nothing more, and an identity that owns
// nothing stops nothing.
func TestStopRetiredTasksRecordsOneStopPerTask(t *testing.T) {
	ctx := context.Background()
	st, taskID := legacyAdmittedTask(t, "flag-era-auth")
	now := time.Now().UTC()
	if stopped, err := stopRetiredTasks(ctx, st, "someone-else", now); err != nil || len(stopped) != 0 {
		t.Fatalf("stopRetiredTasks for an identity owning nothing = %v, %v", stopped, err)
	}
	if readTask(t, st, taskID).Cancellation != nil {
		t.Fatal("another identity's retirement stopped the task")
	}
	stopped, err := stopRetiredTasks(ctx, st, "flag-era-auth", now)
	if err != nil || len(stopped) != 1 || stopped[0] != taskID {
		t.Fatalf("stopRetiredTasks = %v, %v", stopped, err)
	}
	cancellation := readTask(t, st, taskID).Cancellation
	if cancellation == nil || cancellation.State != domain.TaskCancellationRequested {
		t.Fatalf("cancellation after retirement = %+v", cancellation)
	}
	if stopped, err := stopRetiredTasks(ctx, st, "flag-era-auth", now.Add(time.Minute)); err != nil || len(stopped) != 1 {
		t.Fatalf("second stopRetiredTasks = %v, %v", stopped, err)
	}
	if again := readTask(t, st, taskID).Cancellation; again.RequestID != cancellation.RequestID {
		t.Fatalf("a rerun recorded another stop: %+v", again)
	}
}

func legacyOwners(t *testing.T, st *store.Store) map[domain.AuthIdentityID][]domain.Task {
	t.Helper()
	var owned map[domain.AuthIdentityID][]domain.Task
	if err := st.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		owned, err = legacyOpenTasks(context.Background(), tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return owned
}

func readTask(t *testing.T, st *store.Store, id domain.TaskID) domain.Task {
	t.Helper()
	var task domain.Task
	if err := st.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		task, err = tx.GetTask(context.Background(), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return task
}

// legacyAdmittedTask is a store holding one open task whose run was admitted
// the flag-era way: under the named identity, with no agent binding. The
// identity is enabled and holds no enrollment.
func legacyAdmittedTask(t *testing.T, identityID domain.AuthIdentityID) (*store.Store, domain.TaskID) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	work, policyFile, publicationFile := writeSubmissionInputs(t, root)
	cfg := submitCommandConfig{
		DBPath: filepath.Join(root, "freeside.db"), TaskPath: work,
		PolicyPath: policyFile, PublicationPath: publicationFile, ProjectID: "agent-cutover",
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
	var submittedPolicy domain.ResolvedPolicy
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		submittedPolicy, err = tx.GetResolvedPolicy(ctx, submitted.SpecificationRunID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	policy, err := domain.NewResolvedPolicy("cutover-production", submittedPolicy.Keys)
	if err != nil {
		t.Fatal(err)
	}
	production, err := engine.SubmitProductionRun(ctx, st, engine.ProductionRunSpec{
		RunID: policy.RunID, ProjectID: submitted.ProjectID,
		SpecArtifactID: submitted.SourceArtifactID, PolicyArtifactID: submitted.SpecificationPolicyArtifactID,
		ResolvedPolicy: policy,
		Publication: engine.ProductionPublication{
			Title: "Finish under the old identity", Body: "## Why\n\nWork admitted before the cutover.\n",
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
	backendDigest := domain.Digest("sha256:" + strings.Repeat("1", 64))
	record, err := domain.NewBackendConformance(domain.BackendConformanceInput{
		Backend: domain.BackendFreshVMReadOnlyVolumeHandoff, Outcome: domain.ConformancePassed,
		ConfigurationDigest: backendDigest, Capabilities: capabilities, ProvedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := (storeConformanceRecorder{store: st}).RecordBackendConformance(ctx, record); err != nil {
		t.Fatal(err)
	}
	identity := domain.AuthIdentity{
		ID: identityID, Provider: "claude", AuthStoreMutationLease: true, MaxParallelExecutions: 1, Enabled: true,
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
	attempt := domain.Attempt{ID: "flag-era-attempt", StageID: production.StageID, Number: 1, InvocationID: production.InvocationID}
	run.Stages[0].Attempts = []domain.Attempt{attempt}
	admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: production.InvocationID, RunID: run.ID, StageID: production.StageID, AttemptID: attempt.ID,
		Backend: string(domain.BackendFreshVMReadOnlyVolumeHandoff), BackendConfigurationDigest: backendDigest,
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
	if admission.AgentBinding != nil {
		t.Fatal("the fixture admission carries an agent binding")
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
	var taskID domain.TaskID
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		stored, err := tx.GetRun(ctx, run.ID)
		taskID = stored.TaskID
		return err
	}); err != nil || taskID == "" {
		t.Fatalf("the fixture run has no task: %q, %v", taskID, err)
	}
	return st, taskID
}
