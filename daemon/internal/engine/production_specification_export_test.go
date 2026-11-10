package engine

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	specifyfake "github.com/freeside-ai/freeside/daemon/internal/specify/fake"
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

// enqueueSpecificationDiscussion drives f's attended engine until a discuss
// command on the specification card has enqueued its discussion marker, and
// returns the discussion invocation. The marker stays undispatched, so a
// test can admit the invocation itself.
func enqueueSpecificationDiscussion(t *testing.T, f specificationFixture) domain.InvocationID {
	t.Helper()
	driver := f.newDriver(t)
	if err := specifyfake.Script(driver, specificationInvocationID("specification-run", 1), 0, 0, specify.Output{
		Specification: &specify.Specification{
			Summary:    "The bounded implementation plan is ready.",
			Body:       "# Approved Specification\n\nImplement the bounded workflow.",
			Addressals: []specify.Addressal{},
		},
	}); err != nil {
		t.Fatal(err)
	}
	f.submit(t)
	engine := f.newEngine(t, driver)
	itemID := domain.ItemID("spec-approval-implementation-run-1")
	for pass := 1; pass <= 3; pass++ {
		if _, err := engine.Reconcile(t.Context()); err != nil {
			t.Fatalf("prepare specification pass %d: %v", pass, err)
		}
		if _, err := f.signet.GetAttentionItem(t.Context(), itemID); err == nil {
			break
		}
	}
	item, snapshot := f.item(t, itemID)
	if _, err := f.signet.Submit(t.Context(), signet.ClientCommand{
		CommandID: "explain-specification", DeviceID: "device-1",
		ExpectedEntityVersion: snapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: item.ID, Action: domain.ActionDiscuss, ItemVersion: item.ItemVersion,
			ArtifactDigests: item.ArtifactDigests,
			Message:         "Why does the specification keep the workflow bounded?",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Reconcile(t.Context()); err != nil {
		t.Fatalf("enqueue discussion: %v", err)
	}
	discussionID := specDiscussionInvocationID("explain-specification")
	var marker store.QueueEntry
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		marker, err = tx.GetOutbox(t.Context(), string(discussionID))
		return err
	}); err != nil || marker.Kind != KindSpecificationDiscussionRequested || marker.Dispatched() {
		t.Fatalf("discussion marker = %+v, error = %v", marker, err)
	}
	return discussionID
}

// specificationAdmissionBase is the base the fixture's specification
// invocation was admitted at, which binds the project's repository.
func specificationAdmissionBase(t *testing.T, st *store.Store) domain.BaseRevision {
	t.Helper()
	var admission domain.ExecutionAdmission
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		admission, err = tx.GetExecutionAdmissionRecord(t.Context(), specificationInvocationID("specification-run", 1))
		return err
	}); err != nil {
		t.Fatalf("specification admission: %v", err)
	}
	return admission.Base
}

// discussionInputDigest is the input digest the discussion invocation's
// durable record binds, which its admission must name.
func discussionInputDigest(t *testing.T, st *store.Store, invocationID domain.InvocationID) domain.Digest {
	t.Helper()
	var digest domain.Digest
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		invocation, err := tx.GetAgentInvocation(t.Context(), invocationID)
		if err != nil {
			return err
		}
		digest, err = invocation.ComputeInputDigest()
		return err
	}); err != nil {
		t.Fatalf("discussion input digest: %v", err)
	}
	return digest
}

// appendAttempt adds invocationID's attempt to run's stageID and stores the
// run, as the engine does when it starts an invocation.
func appendAttempt(
	t *testing.T, st *store.Store, run domain.Run, stageID domain.StageID, invocationID domain.InvocationID,
) domain.Run {
	t.Helper()
	for i, stage := range run.Stages {
		if stage.ID != stageID {
			continue
		}
		stage.Attempts = append(stage.Attempts, domain.Attempt{
			ID: attemptIDFor(invocationID), StageID: stageID,
			Number: len(stage.Attempts) + 1, InvocationID: invocationID,
		})
		run.Stages[i] = stage
		if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
			return tx.PutRun(t.Context(), run)
		}); err != nil {
			t.Fatalf("append attempt: %v", err)
		}
		return run
	}
	t.Fatalf("run %q has no stage %q", run.ID, stageID)
	return domain.Run{}
}

// TestRecordExecutionExportSpecificationDiscussionArmMintsNoPublicationTask
// covers issue #2991: a discussion on a specification card runs under the
// specification run, but its ID (specification-discussion-<command>) is not
// the inv-specify-<run>-<iter> identity the #768 arm recognizes. Before the
// discussion arm, the unattended branch fell through to loadProductionRequest,
// which found no production marker for the specification run, and the
// export-record step retried forever: the reply never posted and the task
// could not be stopped. The arm authenticates the discussion marker and
// records the export export-only, minting no publication task.
func TestRecordExecutionExportSpecificationDiscussionArmMintsNoPublicationTask(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newSpecificationFixture(t, true, 4)
	discussionID := enqueueSpecificationDiscussion(t, f)
	base := specificationAdmissionBase(t, f.store)
	profileDigest := seedUnattendedAdmissionTrust(t, f.store, base)

	const runID = domain.RunID("specification-run")
	stageID := specificationStageID(runID)
	run, err := f.run(runID)
	if err != nil {
		t.Fatal(err)
	}
	run = appendAttempt(t, f.store, run, stageID, discussionID)
	admission := recordUnattendedAdmission(t, f.store, run, stageID, base, discussionID,
		discussionInputDigest(t, f.store, discussionID), "auth-1", profileDigest)

	if err := RecordExecutionExport(ctx, f.store, unattendedExecutionExport(t, admission), ProductionReplay{}, nil, nil); err != nil {
		t.Fatalf("record unattended specification-discussion export: %v", err)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetExecutionExportRecord(ctx, discussionID); err != nil {
			return err
		}
		return requireNoPublicationTask(ctx, t, tx)
	}); err != nil {
		t.Fatalf("verify export-only record: %v", err)
	}
}

// TestRecordExecutionExportSpecificationDiscussionRefusesForeignRun pins the
// discussion arm's gate: a discussion marker admitted under a run that is not
// its specification run fails authentication, so the export records nothing
// rather than falling through to another arm.
func TestRecordExecutionExportSpecificationDiscussionRefusesForeignRun(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newSpecificationFixture(t, true, 4)
	discussionID := enqueueSpecificationDiscussion(t, f)
	base := specificationAdmissionBase(t, f.store)
	profileDigest := seedUnattendedAdmissionTrust(t, f.store, base)

	specificationRun, err := f.run("specification-run")
	if err != nil {
		t.Fatal(err)
	}
	const foreignRunID = domain.RunID("foreign-run")
	foreignStageID := specificationStageID(foreignRunID)
	foreign := domain.Run{
		ID: foreignRunID, ProjectID: specificationRun.ProjectID,
		SpecDigest: specificationRun.SpecDigest, PolicyDigest: specificationRun.PolicyDigest,
		Stages: []domain.Stage{{ID: foreignStageID, RunID: foreignRunID, Name: "specification"}},
	}
	foreign = appendAttempt(t, f.store, foreign, foreignStageID, discussionID)
	admission := recordUnattendedAdmission(t, f.store, foreign, foreignStageID, base, discussionID,
		discussionInputDigest(t, f.store, discussionID), "auth-1", profileDigest)

	err = RecordExecutionExport(ctx, f.store, unattendedExecutionExport(t, admission), ProductionReplay{}, nil, nil)
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("record foreign-run discussion export = %v, want %v", err, domain.ErrParentKeyMismatch)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetExecutionExportRecord(ctx, discussionID)
		return err
	}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign-run discussion export record = %v, want none", err)
	}
}
