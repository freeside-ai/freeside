package observedb

import (
	"context"
	"database/sql"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/topicstore"
)

// wantSurface is every exported name this package may have. The follow view's
// containment proof (internal/observe/containment_test.go) bounds which
// packages it can name, never which methods of a permitted package it calls,
// so it stops here and rests on this surface staying small. That makes the
// surface the load-bearing claim, and a claim in a comment is one a later
// edit walks past; this pins it.
var wantSurface = map[string]bool{
	"Admission":                                 true,
	"Admission.Base":                            true,
	"Admission.ImageDigest":                     true,
	"Admission.ImageRef":                        true,
	"Admission.InvocationID":                    true,
	"Admission.ReviewConfigurationDigest":       true,
	"Admission.Stage":                           true,
	"Admission.TrustProfileDigest":              true,
	"AttentionItem":                             true,
	"AttentionItem.CreatedAt":                   true,
	"AttentionItem.ID":                          true,
	"AttentionItem.RequestedDecision":           true,
	"AttentionItem.ReviewConfigurationRecovery": true,
	"AttentionItem.Status":                      true,
	"AttentionItem.Type":                        true,
	"Lineage":                                   true,
	"Lineage.ApprovedSpecDigest":                true,
	"Lineage.AttemptNumber":                     true,
	"Lineage.CampaignID":                        true,
	"Lineage.ElaborationRunID":                  true,
	"Lineage.ImplementationRunID":               true,
	"Lineage.Kind":                              true,
	"Lineage.ParentRunID":                       true,
	"Lineage.PublicationDigest":                 true,
	"Lineage.SourceDigest":                      true,
	"Open":                                      true,
	"Snapshot":                                  true,
	"Snapshot.Admissions":                       true,
	"Snapshot.Attempt":                          true,
	"Snapshot.AttentionItems":                   true,
	"Snapshot.ClassifierSamples":                true,
	"Snapshot.Observation":                      true,
	"Snapshot.ShadowReviews":                    true,
	"Snapshot.LastStage":                        true,
	"Snapshot.PublicationInvocationID":          true,
	"Store":                                     true,
	"Store.Close":                               true,
	"Store.ObserveRun":                          true,
	"Store.ObserveSnapshot":                     true,
}

func TestObserveSnapshotProjectsLineageAdmissionAndActionableAttention(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "freeside.db")
	rootImplementationRunID := domain.RunID("run-snapshot-root")
	campaignID, err := engine.ProductionCampaignIDForImplementation(rootImplementationRunID)
	if err != nil {
		t.Fatalf("ProductionCampaignIDForImplementation: %v", err)
	}
	implementationRunID, err := engine.ProductionAttemptRunID(campaignID, 2)
	if err != nil {
		t.Fatalf("ProductionAttemptRunID: %v", err)
	}
	elaborationRunID, err := engine.ElaborationRunIDForImplementation(rootImplementationRunID)
	if err != nil {
		t.Fatalf("ElaborationRunIDForImplementation: %v", err)
	}
	initialAttempt := domain.ProductionAttempt{
		CampaignID: campaignID, AttemptNumber: 1, Kind: domain.ProductionAttemptInitial,
		SourceDigest: "sha256:source", PublicationDigest: "sha256:publication",
		ElaborationRunID: elaborationRunID, ImplementationRunID: rootImplementationRunID,
	}
	productionAttempt := domain.ProductionAttempt{
		CampaignID: campaignID, AttemptNumber: 2, Kind: domain.ProductionAttemptRetry,
		Reason: "retry after rig repair", ParentRunID: rootImplementationRunID,
		SourceDigest: initialAttempt.SourceDigest, PublicationDigest: initialAttempt.PublicationDigest,
		ApprovedSpecDigest: "sha256:approved-spec",
		ElaborationRunID:   elaborationRunID, ImplementationRunID: implementationRunID,
	}
	invocationID := domain.InvocationID("inv-snapshot")
	run := domain.Run{
		ID: implementationRunID, ProjectID: "project-snapshot",
		SpecDigest: "sha256:approved-spec", PolicyDigest: "sha256:policy",
		Stages: []domain.Stage{{
			ID: "stage-snapshot", RunID: implementationRunID, Name: string(domain.StageNameImplementation),
			Attempts: []domain.Attempt{{
				ID: "attempt-snapshot", StageID: "stage-snapshot", Number: 1, InvocationID: invocationID,
			}},
		}},
	}
	elaborationRun := domain.Run{
		ID: elaborationRunID, ProjectID: run.ProjectID,
		SpecDigest: initialAttempt.SourceDigest, PolicyDigest: run.PolicyDigest,
		CampaignID: campaignID, AttemptNumber: 1,
		Stages: []domain.Stage{{
			ID: "stage-elaboration", RunID: elaborationRunID, Name: string(domain.StageNameElaboration),
		}},
	}
	foreignRun := domain.Run{
		ID: "run-snapshot-foreign", ProjectID: run.ProjectID,
		SpecDigest: run.SpecDigest, PolicyDigest: run.PolicyDigest,
		Stages: []domain.Stage{{
			ID: "stage-snapshot-foreign", RunID: "run-snapshot-foreign",
			Name: string(domain.StageNameImplementation),
			Attempts: []domain.Attempt{{
				ID: "attempt-snapshot-foreign", StageID: "stage-snapshot-foreign",
				Number: 1, InvocationID: invocationID,
			}},
		}},
	}
	staleRun := domain.Run{
		ID: "run-snapshot-stale-evidence", ProjectID: run.ProjectID,
		SpecDigest: run.SpecDigest, PolicyDigest: run.PolicyDigest,
		Stages: []domain.Stage{},
	}
	malformedRun := domain.Run{
		ID: "run-snapshot-malformed-attention", ProjectID: run.ProjectID,
		SpecDigest: run.SpecDigest, PolicyDigest: run.PolicyDigest,
		Stages: []domain.Stage{},
	}
	retargetedRun := domain.Run{
		ID: "run-snapshot-retargeted-attention", ProjectID: run.ProjectID,
		SpecDigest: run.SpecDigest, PolicyDigest: run.PolicyDigest,
		Stages: []domain.Stage{},
	}
	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo: "owner/repo", RepositoryID: 42,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit, MessageRuleset: domain.MessageRulesetGitHub1,
		WorkflowAuditDigest: "sha256:workflow-audit",
		Review: domain.ReviewSettings{
			Mode: domain.ReviewFreesideInvoked, ConfigDigest: "sha256:review-config",
		},
	})
	if err != nil {
		t.Fatalf("NewAutomationTrustProfile: %v", err)
	}
	identity := domain.AuthIdentity{
		ID: "auth-snapshot", Provider: "claude", AuthStoreMutationLease: true, MaxParallelExecutions: 1,
		Interim: domain.InterimClientFacts{AuthStoreVolume: "provider-cred", RefreshStrategy: domain.RefreshOnDemand},
	}
	identityID := identity.ID
	admittedAt := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	staleRecipe := domain.Digest("sha256:stale-recipe")
	staleRecipes := map[domain.Digest]bool{staleRecipe: true}
	staleArtifact, err := domain.NewArtifact(domain.ArtifactInput{
		ID: "artifact-stale-evidence", Type: domain.ArtifactKindVerifyLog,
		Digest: "sha256:stale-evidence",
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerVerifier, ProducerInvocationID: "inv-stale-evidence",
			HeadBinding: domain.HeadIndependent, VerificationRecipeDigest: &staleRecipe,
			SensitivityClass: domain.SensitivityNormal,
		},
	}, staleRecipes)
	if err != nil {
		t.Fatalf("NewArtifact(stale evidence): %v", err)
	}
	staleRunID := staleRun.ID
	staleAttention, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "execution-failure-stale-evidence", ProjectID: staleRun.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(staleRunID), RunID: &staleRunID},
		Type:    domain.AttentionExecutionFailure, Priority: domain.PriorityHigh,
		Reason: "old run evidence", RequestedDecision: []domain.Action{domain.ActionRetry, domain.ActionStop},
		EvidenceSnapshot: []domain.Artifact{staleArtifact}, ItemVersion: 1,
		InterruptionClass: domain.InterruptionExceptional, Status: domain.StatusOpen,
	}, staleRecipes)
	if err != nil {
		t.Fatalf("NewAttentionItem(stale evidence): %v", err)
	}
	readyRunID := run.ID
	historicalReady, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "ready-historical-recipe", ProjectID: run.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(readyRunID), RunID: &readyRunID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason:            "historical ready evidence must not hide publication",
		RequestedDecision: []domain.Action{domain.ActionOpenPR, domain.ActionReturnToAgent, domain.ActionDismiss},
		EvidenceSnapshot:  []domain.Artifact{staleArtifact}, PRHeadSHA: "head-ready",
		PRReference: &domain.PRReference{Repo: "owner/repo", Number: 821},
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
	}, staleRecipes)
	if err != nil {
		t.Fatalf("NewAttentionItem(historical ready): %v", err)
	}
	attentionForRun := func(id domain.ItemID, selected domain.Run) domain.AttentionItem {
		t.Helper()
		selectedRunID := selected.ID
		item, err := domain.NewAttentionItem(domain.AttentionItemInput{
			ID: id, ProjectID: selected.ProjectID,
			Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(selectedRunID), RunID: &selectedRunID},
			Type:    domain.AttentionExecutionFailure, Priority: domain.PriorityHigh,
			Reason: "run-scoped corruption fixture", RequestedDecision: []domain.Action{domain.ActionRetry, domain.ActionStop},
			ItemVersion: 1, InterruptionClass: domain.InterruptionExceptional, Status: domain.StatusOpen,
		}, nil)
		if err != nil {
			t.Fatalf("NewAttentionItem(%s): %v", id, err)
		}
		return item
	}
	malformedAttention := attentionForRun("execution-failure-malformed", malformedRun)
	retargetedAttention := attentionForRun("execution-failure-retargeted", retargetedRun)
	capabilities, ok := domain.ProvableCapabilities(domain.BackendFreshVMReadOnlyVolumeHandoff)
	if !ok {
		t.Fatal("fresh VM backend has no provable capability ceiling")
	}
	backendConfigurationDigest := domain.Digest("sha256:" + strings.Repeat("b", 64))
	admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: invocationID, RunID: run.ID, StageID: "stage-snapshot", AttemptID: "attempt-snapshot",
		Backend: string(domain.BackendFreshVMReadOnlyVolumeHandoff), Capabilities: capabilities,
		BackendConfigurationDigest: backendConfigurationDigest,
		OperatingMode:              domain.ModeUnattended, CredentialMode: domain.CredentialSubscriptionContained,
		EgressProfile: domain.EgressProviderOnly,
		ImageRef:      domain.ImageRef("registry/agent@sha256:" + strings.Repeat("a", 64)),
		SpecDigest:    run.SpecDigest, PolicyDigest: run.PolicyDigest, InputDigest: "sha256:input",
		Base:      domain.BaseRevision{Repo: profile.Repo, RepositoryID: profile.RepositoryID, BaseRef: "main", BaseSHA: "base-sha"},
		Workspace: "workspace-snapshot", AuthIdentityID: &identityID,
		TrustProfileDigest: &profile.ProfileDigest, AdmittedAt: admittedAt,
	})
	if err != nil {
		t.Fatalf("NewExecutionAdmission: %v", err)
	}
	recovery := domain.ReviewConfigurationRecoveryBinding{
		RunID: run.ID, InvocationID: "review-snapshot", Round: 1,
		BaseSHA: admission.Base.BaseSHA, HeadSHA: "head-sha", FailureDigest: "sha256:failure",
		Repo: profile.Repo, RepositoryID: profile.RepositoryID,
		SupersededProfileDigest: profile.ProfileDigest,
	}
	runID := run.ID
	attention, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "review-snapshot-1", ProjectID: run.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(run.ID), RunID: &runID},
		Type:    domain.AttentionReviewConfiguration, Priority: domain.PriorityHigh,
		Reason:            "this arbitrary reason must not be projected",
		RequestedDecision: []domain.Action{domain.ActionAdoptReviewConfiguration},
		PRHeadSHA:         recovery.HeadSHA, ReviewConfigurationRecovery: &recovery,
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate,
		CreatedAt: &admittedAt, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}

	st, _, err := topicstore.Open(ctx, path, store.Options{
		ApprovedRecipes: staleRecipes,
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeUnattended: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
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
		t.Fatalf("open seed store: %v", err)
	}
	conformance, err := domain.NewBackendConformance(domain.BackendConformanceInput{
		Backend: domain.BackendFreshVMReadOnlyVolumeHandoff, Outcome: domain.ConformancePassed,
		ConfigurationDigest: backendConfigurationDigest, Capabilities: capabilities,
		ProvedAt: admittedAt,
	})
	if err != nil {
		t.Fatalf("NewBackendConformance: %v", err)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.RecordBackendConformance(ctx, conformance)
		return err
	}); err != nil {
		t.Fatalf("RecordBackendConformance: %v", err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutProductionAttempt(ctx, initialAttempt); err != nil {
			return err
		}
		if _, err := tx.ApproveProductionAttempt(ctx, campaignID, 1, run.SpecDigest); err != nil {
			return err
		}
		if err := tx.PutProductionAttempt(ctx, productionAttempt); err != nil {
			return err
		}
		if err := tx.PutRun(ctx, elaborationRun); err != nil {
			return err
		}
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutRun(ctx, foreignRun); err != nil {
			return err
		}
		if err := tx.PutRun(ctx, staleRun); err != nil {
			return err
		}
		if err := tx.PutRun(ctx, malformedRun); err != nil {
			return err
		}
		if err := tx.PutRun(ctx, retargetedRun); err != nil {
			return err
		}
		if err := tx.RecordTrustProfile(ctx, profile, admittedAt); err != nil {
			return err
		}
		if err := tx.RecordAuthIdentity(ctx, identity, admittedAt); err != nil {
			return err
		}
		if err := tx.RecordExecutionAdmission(ctx, admission); err != nil {
			return err
		}
		if err := tx.PutAttentionItem(ctx, attention); err != nil {
			return err
		}
		if err := tx.PutAttentionItem(ctx, staleAttention); err != nil {
			return err
		}
		if err := tx.PutAttentionItem(ctx, historicalReady); err != nil {
			return err
		}
		if err := tx.PutAttentionItem(ctx, malformedAttention); err != nil {
			return err
		}
		if err := tx.PutAttentionItem(ctx, retargetedAttention); err != nil {
			return err
		}
		if err := tx.AppendRunMilestone(ctx, domain.RunMilestone{
			RunID: run.ID, Kind: domain.MilestoneInvocationAdmitted,
			InvocationID: &invocationID, RecordedAt: admittedAt,
		}); err != nil {
			return err
		}
		if err := tx.AppendRunMilestone(ctx, domain.RunMilestone{
			RunID: run.ID, Kind: domain.MilestonePublicationReady,
			InvocationID: &invocationID, RecordedAt: admittedAt.Add(time.Minute),
		}); err != nil {
			return err
		}
		completed := domain.ObservedStatusCompleted
		return tx.AppendRunMilestone(ctx, domain.RunMilestone{
			RunID: run.ID, Kind: domain.MilestoneTerminalRecorded,
			InvocationID: &invocationID, Terminal: &completed,
			RecordedAt: admittedAt.Add(2 * time.Minute),
		})
	}); err != nil {
		_ = st.Close()
		t.Fatalf("seed store: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw store: %v", err)
	}
	if _, err := raw.ExecContext(ctx,
		`UPDATE attention_items SET body = '{' WHERE id = ?`, malformedAttention.ID); err != nil {
		_ = raw.Close()
		t.Fatalf("malform unrelated attention body: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `UPDATE attention_items
		SET body = json_set(body, '$.subject.subject_id', 'run-other', '$.subject.run_id', 'run-other')
		WHERE id = ?`, retargetedAttention.ID); err != nil {
		_ = raw.Close()
		t.Fatalf("retarget attention body: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw store: %v", err)
	}

	observed, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = observed.Close() })
	snapshot, err := observed.ObserveSnapshot(ctx, run.ID)
	if err != nil {
		t.Fatalf("ObserveSnapshot: %v", err)
	}
	if conclusion := domain.ConcludeRun(snapshot.Observation); !conclusion.Final ||
		conclusion.Outcome != domain.RunOutcomePublished {
		t.Fatalf("published observation conclusion = %+v", conclusion)
	}
	if snapshot.PublicationInvocationID != "" {
		t.Fatalf("snapshot projected absent publication completion as %q", snapshot.PublicationInvocationID)
	}
	if len(snapshot.Admissions) != 1 || snapshot.Admissions[0].ImageDigest != "sha256:"+domain.Digest(strings.Repeat("a", 64)) ||
		snapshot.Admissions[0].ReviewConfigurationDigest == nil ||
		*snapshot.Admissions[0].ReviewConfigurationDigest != profile.Review.ConfigDigest {
		t.Fatalf("admissions = %+v", snapshot.Admissions)
	}
	if len(snapshot.AttentionItems) != 1 || snapshot.AttentionItems[0].ID != attention.ID ||
		snapshot.AttentionItems[0].ReviewConfigurationRecovery == nil {
		t.Fatalf("attention = %+v", snapshot.AttentionItems)
	}
	elaborationSnapshot, err := observed.ObserveSnapshot(ctx, elaborationRun.ID)
	if err != nil {
		t.Fatalf("ObserveSnapshot(elaboration): %v", err)
	}
	if elaborationSnapshot.Attempt == nil ||
		elaborationSnapshot.Attempt.ElaborationRunID != elaborationRun.ID ||
		elaborationSnapshot.Attempt.ImplementationRunID != rootImplementationRunID ||
		elaborationSnapshot.Attempt.ApprovedSpecDigest != run.SpecDigest {
		t.Fatalf("elaboration lineage = %+v", elaborationSnapshot.Attempt)
	}
	if _, err := observed.ObserveSnapshot(ctx, foreignRun.ID); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("ObserveSnapshot(foreign admission) error = %v, want ErrParentKeyMismatch", err)
	}
	if _, err := observed.ObserveSnapshot(ctx, staleRun.ID); !errors.Is(err, domain.ErrUnapprovedRecipe) {
		t.Fatalf("ObserveSnapshot(stale run evidence) error = %v, want ErrUnapprovedRecipe", err)
	}
	for name, selectedRunID := range map[string]domain.RunID{
		"malformed body":  malformedRun.ID,
		"retargeted body": retargetedRun.ID,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := observed.ObserveSnapshot(ctx, selectedRunID); err == nil ||
				!strings.Contains(err.Error(), "stored row body inconsistent") {
				t.Fatalf("ObserveSnapshot(%s) error = %v, want fail-closed row inconsistency", selectedRunID, err)
			}
		})
	}
}

func TestObserveSnapshotProjectsShadowEvidence(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "freeside.db")
	st, err := store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Date(2026, 8, 24, 15, 0, 0, 0, time.UTC)
	run := domain.Run{
		ID: "run-shadow-observe", ProjectID: "project-shadow",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	routed, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-routed", RunID: run.ID, Round: 1,
		Provider: "openai", ModelConfiguration: "codex/test",
		ConfigurationDigest: observedShadowDigest("a"), InstructionDigest: observedShadowDigest("b"),
		CostOwner: "routed", BaseSHA: strings.Repeat("1", 40), HeadSHA: strings.Repeat("2", 40),
		CompletedAt: now, CompletionEvidence: observedShadowDigest("c"), Outcome: domain.ReviewClean,
	})
	if err != nil {
		t.Fatal(err)
	}
	finding := domain.Finding{
		ID: "shadow-observed-finding", RunID: run.ID, Source: string(domain.ShadowReviewClaudeLocal),
		Severity: domain.FindingSeverityP2,
		Location: &domain.FindingLocation{Path: "daemon/a.go", StartLine: 1, EndLine: 1},
		Message:  "shadow observation", RawText: "shadow observation", CreatedAt: now,
	}
	shadow, err := domain.NewShadowReviewRecord(domain.ShadowReviewRecord{
		InvocationID: "shadow-review-observed", RunID: run.ID, ShadowedRound: 1,
		Source: domain.ShadowReviewClaudeLocal, Provider: "anthropic",
		ModelConfiguration: "claude/test", ConfigurationDigest: observedShadowDigest("d"),
		InstructionDigest: routed.InstructionDigest, CostOwner: "shadow",
		BaseSHA: routed.BaseSHA, HeadSHA: routed.HeadSHA, CompletedAt: now,
		CompletionEvidence: observedShadowDigest("e"), Outcome: domain.ReviewFindings,
		FindingIDs: []domain.FindingID{finding.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	classification := domain.Classification{
		FindingID: finding.ID, Version: 1,
		Materiality: "high", Confidence: "low", Note: "producer=deterministic/test; observation",
	}
	sample := domain.ClassifierAccuracySample{
		RunID: run.ID, FindingID: finding.ID, ClassificationVersion: 1,
		ShadowInvocationID: shadow.InvocationID,
		Assessment:         domain.ClassifierAssessmentIndeterminate, RecordedAt: now,
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, routed, nil); err != nil {
			return err
		}
		if err := tx.PutShadowReviewRecord(ctx, shadow, []domain.Finding{finding}); err != nil {
			return err
		}
		if err := tx.PutClassification(ctx, classification); err != nil {
			return err
		}
		return tx.PutClassifierAccuracySample(ctx, sample)
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := (&Store{store: st}).ObserveSnapshot(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.ShadowReviews) != 1 || snapshot.ShadowReviews[0].InvocationID != shadow.InvocationID ||
		len(snapshot.ClassifierSamples) != 1 || snapshot.ClassifierSamples[0] != sample {
		t.Fatalf("shadow snapshot = %+v", snapshot)
	}
}

func observedShadowDigest(seed string) domain.Digest {
	return domain.Digest("sha256:" + strings.Repeat(seed, 64))
}

// TestExportedSurfaceStaysNarrow fails when this package grows an exported
// name. Adding one is not forbidden, but it widens what the follow path can
// reach, so it is a deliberate edit to wantSurface with the containment
// reasoning in view, never a silent addition.
func TestExportedSurfaceStaysNarrow(t *testing.T) {
	got := map[string]bool{}
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	files := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files++
		collectExported(t, fset, name, nil, got)
	}
	if files == 0 {
		t.Fatal("no non-test Go files found; the surface assertion would pass vacuously")
	}
	for name := range got {
		if !wantSurface[name] {
			t.Errorf("package exports %s, which widens what the follow path can reach; "+
				"add it to wantSurface deliberately or keep it unexported", name)
		}
	}
	for name := range wantSurface {
		if !got[name] {
			t.Errorf("expected export %s is gone; wantSurface is stale", name)
		}
	}
}

// collectExported records path's exported surface into `into`. src is nil for
// a real file and non-nil only for the synthetic sources that prove this
// collector sees what it claims to.
func collectExported(
	t *testing.T, fset *token.FileSet, path string, src any, into map[string]bool,
) {
	t.Helper()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if !d.Name.IsExported() {
				continue
			}
			if d.Recv == nil {
				into[d.Name.Name] = true
				continue
			}
			into[receiverName(d.Recv)+"."+d.Name.Name] = true
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				collectSpec(spec, into)
			}
		}
	}
}

// collectSpec records an exported type, var, or const, and then everything
// that type itself exposes. A name alone is not the surface: an exported
// field (`Raw *store.Store`) hands a caller the wrapped API directly, an
// embedded type promotes its whole method set onto this one, and an exported
// interface's methods are callable the same way. Each is a route to the
// store's write, checkpoint, and restore methods that changes no import, so
// each is enumerated rather than assumed away.
func collectSpec(spec ast.Spec, into map[string]bool) {
	for _, name := range specNames(spec) {
		if name.IsExported() {
			into[name.Name] = true
		}
	}
	typeSpec, ok := spec.(*ast.TypeSpec)
	if !ok || !typeSpec.Name.IsExported() {
		return
	}
	switch t := typeSpec.Type.(type) {
	case *ast.StructType:
		collectFields(typeSpec.Name.Name, t.Fields, into)
	case *ast.InterfaceType:
		collectFields(typeSpec.Name.Name, t.Methods, into)
	}
}

// collectFields records the exported members a type exposes. An anonymous
// member is an embedding, so it is recorded under the embedded type's name
// with an explicit marker: its promoted methods are reachable without ever
// naming them here, which is exactly the case a name-only pin would miss.
func collectFields(owner string, fields *ast.FieldList, into map[string]bool) {
	if fields == nil {
		return
	}
	for _, field := range fields.List {
		if len(field.Names) == 0 {
			into[owner+" embeds "+typeName(field.Type)] = true
			continue
		}
		for _, name := range field.Names {
			if name.IsExported() {
				into[owner+"."+name.Name] = true
			}
		}
	}
}

// typeName renders an embedded type's name for the surface record, following
// pointers and qualified names so `*store.Store` reads as store.Store.
func typeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return typeName(t.X)
	case *ast.SelectorExpr:
		return typeName(t.X) + "." + t.Sel.Name
	case *ast.Ident:
		return t.Name
	default:
		return "unknown"
	}
}

func receiverName(recv *ast.FieldList) string {
	if len(recv.List) == 0 {
		return ""
	}
	expr := recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

func specNames(spec ast.Spec) []*ast.Ident {
	switch s := spec.(type) {
	case *ast.TypeSpec:
		return []*ast.Ident{s.Name}
	case *ast.ValueSpec:
		return s.Names
	default:
		return nil
	}
}

// TestSurfaceCollectorSeesFieldsAndEmbedding proves the collector above
// actually catches the routes it claims to. Without this, the surface pin
// would be another unverified assertion, which is the exact mistake that made
// three review rounds necessary: a check that looks like a boundary and holds
// nothing. Each case is a way to hand a caller the wrapped store's write,
// checkpoint, and restore methods while changing no import.
func TestSurfaceCollectorSeesFieldsAndEmbedding(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want string
	}{
		{
			name: "exported field",
			src:  "package p\ntype Store struct {\n\tRaw *store.Store\n}\n",
			want: "Store.Raw",
		},
		{
			name: "embedded pointer",
			src:  "package p\ntype Store struct {\n\t*store.Store\n}\n",
			want: "Store embeds store.Store",
		},
		{
			name: "embedded value",
			src:  "package p\ntype Store struct {\n\tstore.Store\n}\n",
			want: "Store embeds store.Store",
		},
		{
			name: "interface method",
			src:  "package p\ntype Reader interface {\n\tWriteInternal() error\n}\n",
			want: "Reader.WriteInternal",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := map[string]bool{}
			collectExported(t, token.NewFileSet(), "synthetic.go", tc.src, got)
			if !got[tc.want] {
				t.Fatalf("collector missed %q; it recorded %v", tc.want, keys(got))
			}
			if wantSurface[tc.want] {
				t.Fatalf("%q is in wantSurface, so this case proves nothing", tc.want)
			}
		})
	}

	// An unexported field is not a route: a caller outside this package
	// cannot name it, so recording one would only make the pin noisy.
	got := map[string]bool{}
	collectExported(t, token.NewFileSet(), "synthetic.go",
		"package p\ntype Store struct {\n\traw *store.Store\n}\n", got)
	if got["Store.raw"] {
		t.Error("collector recorded an unexported field")
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
