package observedb

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
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
	"AdjudicationDispatch":                        true,
	"AdjudicationDispatch.AdjudicationConfidence": true,
	"AdjudicationDispatch.AttemptNumber":          true,
	"AdjudicationDispatch.ClassifierConfidence":   true,
	"AdjudicationDispatch.ClassifierMateriality":  true,
	"AdjudicationDispatch.FindingID":              true,
	"AdjudicationDispatch.FindingSeverity":        true,
	"AdjudicationDispatch.InSurface":              true,
	"AdjudicationDispatch.Producer":               true,
	"AdjudicationDispatch.ResolvedPolicyDigest":   true,
	"AdjudicationDispatch.Revision":               true,
	"AdjudicationDispatch.Round":                  true,
	"AdjudicationDispatch.Route":                  true,
	"Admission":                                   true,
	"Admission.Base":                              true,
	"Admission.ImageDigest":                       true,
	"Admission.ImageRef":                          true,
	"Admission.InvocationID":                      true,
	"Admission.ReviewConfigurationDigest":         true,
	"Admission.Stage":                             true,
	"Admission.TrustProfileDigest":                true,
	"AttentionItem":                               true,
	"AttentionItem.CreatedAt":                     true,
	"AttentionItem.ID":                            true,
	"AttentionItem.RequestedDecision":             true,
	"AttentionItem.ReviewConfigurationRecovery":   true,
	"AttentionItem.Status":                        true,
	"AttentionItem.Type":                          true,
	"Lineage":                                     true,
	"Lineage.ApprovedSpecDigest":                  true,
	"Lineage.AttemptNumber":                       true,
	"Lineage.CampaignID":                          true,
	"Lineage.ElaborationRunID":                    true,
	"Lineage.ImplementationRunID":                 true,
	"Lineage.Kind":                                true,
	"Lineage.ParentRunID":                         true,
	"Lineage.PublicationDigest":                   true,
	"Lineage.SourceDigest":                        true,
	"Open":                                        true,
	"Snapshot":                                    true,
	"Snapshot.Adjudications":                      true,
	"Snapshot.Admissions":                         true,
	"Snapshot.AuthenticatedConclusion":            true,
	"Snapshot.Attempt":                            true,
	"Snapshot.AttentionItems":                     true,
	"Snapshot.ClassifierSamples":                  true,
	"Snapshot.Observation":                        true,
	"Snapshot.ProducingInvocationID":              true,
	"Snapshot.PublicationReadyAuthenticated":      true,
	"Snapshot.ShadowReviews":                      true,
	"Snapshot.LastStage":                          true,
	"Snapshot.PublicationInvocationID":            true,
	"Snapshot.ReviewYield":                        true,
	"Store":                                       true,
	"Store.Close":                                 true,
	"Store.ObserveConclusion":                     true,
	"Store.ObserveRun":                            true,
	"Store.ObserveSnapshot":                       true,
	"ReviewYield":                                 true,
	"ReviewYield.AttemptNumber":                   true,
	"ReviewYield.ConfigurationDigest":             true,
	"ReviewYield.DecisionAction":                  true,
	"ReviewYield.Declined":                        true,
	"ReviewYield.Deferred":                        true,
	"ReviewYield.FindingsIngested":                true,
	"ReviewYield.Fixed":                           true,
	"ReviewYield.NewFindings":                     true,
	"ReviewYield.Outcome":                         true,
	"ReviewYield.RecurringFindings":               true,
	"ReviewYield.Round":                           true,
}

func TestObserveSnapshotProjectsPublicationCompletionIdentities(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "freeside.db")
	runID := domain.RunID("run-snapshot-publication-completion")
	projectID := domain.ProjectID("project-snapshot-publication-completion")
	producingInvocation := domain.InvocationID("inv-implement-" + string(runID))
	publicationInvocation := domain.InvocationID("publish-production-" + string(runID))
	verificationInvocation := domain.InvocationID("verify-production-" + string(runID))
	stageID := domain.StageID("implement-" + string(runID))
	head := strings.Repeat("a", 40)
	base := strings.Repeat("b", 40)
	blob := export.Digest("sha256:" + strings.Repeat("c", 64))
	mode := "0644"
	size := int64(3)
	manifest := export.Manifest{
		Version: export.ManifestVersion,
		Entries: []export.Entry{{
			Path: "README.md", Kind: export.EntryRegular,
			Mode: &mode, Size: &size, Digest: &blob,
		}},
	}
	manifestBytes, err := manifest.Encode()
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest := domain.Digest(fmt.Sprintf("sha256:%x", sha256.Sum256(manifestBytes)))
	taskPayload, err := json.Marshal(struct {
		Version               string                       `json:"version"`
		RunID                 domain.RunID                 `json:"run_id"`
		ProjectID             domain.ProjectID             `json:"project_id"`
		ProducingInvocationID domain.InvocationID          `json:"producing_invocation_id"`
		VerificationID        domain.InvocationID          `json:"verification_invocation_id"`
		PublicationID         domain.InvocationID          `json:"publication_invocation_id"`
		HeadSHA               string                       `json:"head_sha"`
		Replay                engine.ProductionReplay      `json:"replay"`
		Publication           engine.ProductionPublication `json:"publication"`
	}{
		Version:               "freeside.production-publication/v1",
		RunID:                 runID,
		ProjectID:             projectID,
		ProducingInvocationID: producingInvocation,
		VerificationID:        verificationInvocation,
		PublicationID:         publicationInvocation,
		HeadSHA:               head,
		Replay: engine.ProductionReplay{
			InvocationID: producingInvocation, ObservedBaseSHA: base, HeadSHA: head,
			Manifest: manifest, ManifestDigest: manifestDigest,
			ImportOptions: importer.Options{
				BaseSHA:    base,
				CommitDate: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
			},
		},
		Publication: engine.ProductionPublication{
			Title: "Publish the production run", Body: "Produced by a production run.\n",
			CommitAuthor: engine.ProductionCommitAuthor{AppSlug: "freeside", BotUserID: 42},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	terminalPayload, err := json.Marshal(struct {
		InvocationID domain.InvocationID `json:"invocation_id"`
		RunID        domain.RunID        `json:"run_id"`
		StageID      domain.StageID      `json:"stage_id"`
		Status       string              `json:"status"`
		HeadSHA      string              `json:"head_sha"`
	}{
		InvocationID: producingInvocation, RunID: runID, StageID: stageID,
		Status: "completed", HeadSHA: head,
	})
	if err != nil {
		t.Fatal(err)
	}

	st, _, err := topicstore.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		run := domain.Run{
			ID: runID, ProjectID: projectID,
			SpecDigest:   domain.Digest("sha256:" + strings.Repeat("d", 64)),
			PolicyDigest: domain.Digest("sha256:" + strings.Repeat("e", 64)),
			Stages: []domain.Stage{{
				ID: stageID, RunID: runID, Name: "implement", Attempts: []domain.Attempt{},
			}},
		}
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		return tx.AppendRunMilestone(ctx, domain.RunMilestone{
			RunID: runID, Kind: domain.MilestoneRunSubmitted,
			InvocationID: &producingInvocation,
			RecordedAt:   time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		})
	}); err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		key := "production-publication/" + string(runID)
		if _, _, err := tx.EnqueueOutbox(
			ctx, key, engine.KindProductionPublicationRequested, taskPayload,
		); err != nil {
			return err
		}
		if _, _, err := tx.RecordInbox(
			ctx, string(producingInvocation), "production_stage_terminal", terminalPayload,
		); err != nil {
			return err
		}
		return tx.MarkOutboxDispatched(ctx, key)
	}); err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	observed, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = observed.Close() })
	snapshot, err := observed.ObserveSnapshot(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ProducingInvocationID != producingInvocation ||
		snapshot.PublicationInvocationID != publicationInvocation {
		t.Fatalf("publication identities = %q / %q, want %q / %q",
			snapshot.ProducingInvocationID, snapshot.PublicationInvocationID,
			producingInvocation, publicationInvocation)
	}
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
	reviewFinding := domain.Finding{
		ID: "finding-snapshot", RunID: run.ID, Source: "codex_local",
		Location: &domain.FindingLocation{Path: "daemon/a.go", StartLine: 1, EndLine: 1},
		Message:  "snapshot finding", RawText: "snapshot finding", CreatedAt: admittedAt,
	}
	reviewConfigurationDigest := domain.Digest("sha256:" + strings.Repeat("e", 64))
	reviewRecord, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-yield-snapshot", RunID: run.ID, Round: 1,
		Provider: "codex", ModelConfiguration: "test",
		ConfigurationDigest: reviewConfigurationDigest,
		InstructionDigest:   domain.Digest("sha256:" + strings.Repeat("c", 64)),
		CostOwner:           "test", BaseSHA: admission.Base.BaseSHA, HeadSHA: recovery.HeadSHA,
		CompletedAt: admittedAt, CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("d", 64)),
		Outcome: domain.ReviewFindings, FindingIDs: []domain.FindingID{reviewFinding.ID},
	})
	if err != nil {
		t.Fatalf("NewReviewRecord: %v", err)
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
		if err := tx.PutReviewRecord(ctx, reviewRecord, []domain.Finding{reviewFinding}); err != nil {
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
	if snapshot.ProducingInvocationID != "" || snapshot.PublicationInvocationID != "" {
		t.Fatalf("snapshot projected absent publication completion as %q / %q",
			snapshot.ProducingInvocationID, snapshot.PublicationInvocationID)
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
	if len(snapshot.ReviewYield) != 1 || snapshot.ReviewYield[0].AttemptNumber != 2 ||
		snapshot.ReviewYield[0].Round != 1 || snapshot.ReviewYield[0].NewFindings != 1 ||
		snapshot.ReviewYield[0].ConfigurationDigest != reviewConfigurationDigest ||
		snapshot.ReviewYield[0].Outcome != domain.ReviewFindings ||
		snapshot.ReviewYield[0].DecisionAction != nil {
		t.Fatalf("review yield = %+v", snapshot.ReviewYield)
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

// adjDigest builds a 64-hex content-address digest from a single-character seed.
func adjDigest(seed string) domain.Digest {
	return domain.Digest("sha256:" + strings.Repeat(seed, 64))
}

// adjResolvedPolicy builds a run-scoped resolved policy declaring one paths glob,
// so the run's declared surface is reachable to ObserveSnapshot's in-surface join.
func adjResolvedPolicy(t *testing.T, runID domain.RunID, pathsGlob string) domain.ResolvedPolicy {
	t.Helper()
	policy, err := domain.NewResolvedPolicy(runID, []domain.PolicyKey{{
		Key: "paths", Value: pathsGlob,
		Provenance: domain.KeyProvenance{Source: domain.ProvenancePreset, Digest: adjDigest("a")},
	}})
	if err != nil {
		t.Fatalf("NewResolvedPolicy(%q): %v", pathsGlob, err)
	}
	return policy
}

// adjReviewRecord builds the round's review record binding the exact finding set
// the adjudication artifact must match.
func adjReviewRecord(
	t *testing.T, runID domain.RunID, round int, instructionDigest domain.Digest,
	findingIDs []domain.FindingID, at time.Time,
) domain.ReviewRecord {
	t.Helper()
	record, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: domain.InvocationID("review-" + string(runID) + "-1"),
		RunID:        runID, Round: round, Provider: "openai", ModelConfiguration: "gpt-codex/high",
		ConfigurationDigest: adjDigest("c"), InstructionDigest: instructionDigest,
		CostOwner: "owner", BaseSHA: "base", HeadSHA: "head", CompletedAt: at,
		CompletionEvidence: adjDigest("e"), Outcome: domain.ReviewFindings, FindingIDs: findingIDs,
	})
	if err != nil {
		t.Fatalf("NewReviewRecord: %v", err)
	}
	return record
}

func adjFinding(id domain.FindingID, runID domain.RunID, path string, severity domain.FindingSeverity, at time.Time) domain.Finding {
	return domain.Finding{
		ID: id, RunID: runID, Source: "codex_local", Severity: severity,
		Location: &domain.FindingLocation{Path: path, StartLine: 1, EndLine: 1},
		Message:  "finding " + string(id), RawText: "finding " + string(id), CreatedAt: at,
	}
}

func adjClassification(id domain.FindingID, round int, materiality, confidence string) domain.Classification {
	return domain.Classification{
		FindingID: id, Version: round, Materiality: materiality, Confidence: confidence,
		Note: "producer=deterministic/test; observation",
	}
}

func adjEngineEntry(t *testing.T, id domain.FindingID) domain.FindingAdjudicationEntry {
	t.Helper()
	compat := domain.CompatibilityAllowed
	entry, err := domain.NewEngineAdjudicationEntry(
		id, domain.GoalRequired, &compat, domain.RouteRemediate, "in declared scope",
		nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("engine entry %q: %v", id, err)
	}
	return entry
}

func adjEngineModelEntry(t *testing.T, id domain.FindingID, confidence domain.AdjudicationConfidence) domain.FindingAdjudicationEntry {
	t.Helper()
	entry, err := domain.NewEngineModelAdjudicationEntry(
		id, domain.GoalRequired, confidence, "model-required, engine-derived allowed remediation",
		nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("engine-model entry %q: %v", id, err)
	}
	return entry
}

func adjModelSeparateEntry(t *testing.T, id domain.FindingID, confidence domain.AdjudicationConfidence) domain.FindingAdjudicationEntry {
	t.Helper()
	compat := domain.ProposedSeparateWork
	entry, err := domain.NewModelAdjudicationEntry(
		id, domain.GoalRequired, &compat, domain.RouteParkSeparateWork, confidence,
		"remediation belongs in separate work outside the declared surface",
		nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("model entry %q: %v", id, err)
	}
	return entry
}

// seedAdjudicationRun seeds one run with a resolved policy, one review round, an
// adjudication artifact over the given entries, and one classification per
// finding, so ObserveSnapshot can project the dispatch telemetry. The seed keeps
// artifact.ResolvedPolicyDigest == run.PolicyDigest == the resolved policy digest,
// which the store's binding gate requires and which also makes the run's declared
// paths the authentic surface the in-surface join reads.
func seedAdjudicationRun(
	t *testing.T, ctx context.Context, st *store.Store,
	runID domain.RunID, pathsGlob string,
	findings []domain.Finding, classifications []domain.Classification,
	entries []domain.FindingAdjudicationEntry, at time.Time,
) domain.Digest {
	t.Helper()
	policy := adjResolvedPolicy(t, runID, pathsGlob)
	specDigest := adjDigest("f")
	instructionDigest := adjDigest("d")
	findingIDs := make([]domain.FindingID, 0, len(findings))
	for _, f := range findings {
		findingIDs = append(findingIDs, f.ID)
	}
	record := adjReviewRecord(t, runID, 1, instructionDigest, findingIDs, at)
	artifact, err := domain.NewFindingAdjudication(
		runID, 1, specDigest, instructionDigest, policy.Digest, entries, at)
	if err != nil {
		t.Fatalf("NewFindingAdjudication(%q): %v", runID, err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, domain.Run{
			ID: runID, ProjectID: "project-adjudication",
			SpecDigest: specDigest, PolicyDigest: policy.Digest,
		}); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, record, findings); err != nil {
			return err
		}
		for _, classification := range classifications {
			if err := tx.PutClassification(ctx, classification); err != nil {
				return err
			}
		}
		return tx.PutFindingAdjudication(ctx, artifact)
	}); err != nil {
		t.Fatalf("seed adjudication run %q: %v", runID, err)
	}
	return policy.Digest
}

// TestObserveSnapshotProjectsAdjudicationDispatch proves the bounded dispatch
// projection carries the authenticated axes that answer the revision-31
// calibration question — how often critical/high severity (P0/P1), material, in-surface
// findings reach deterministic engine dispatch versus model residue — across the
// five fixtures the unit specifies: fast-path (engine), model-residue
// (engine_model), low-confidence, out-of-surface, and configuration-change. The
// computed calibration numerator and denominator are asserted from the projection
// alone, with no raw SQLite access.
func TestObserveSnapshotProjectsAdjudicationDispatch(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "freeside.db")
	st, err := store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	at := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	// Run 1 carries the fast-path, model-residue, low-confidence, and
	// out-of-surface fixtures in one round. Declared scope is daemon/**, so a
	// daemon/ location is in-surface and an app/ location is not.
	const run1 = domain.RunID("run-adjudication-calibration")
	fastID := domain.FindingID("finding-fastpath")
	residueID := domain.FindingID("finding-residue")
	lowConfID := domain.FindingID("finding-lowconf")
	outSurfaceID := domain.FindingID("finding-outsurface")
	run1Findings := []domain.Finding{
		adjFinding(fastID, run1, "daemon/fast.go", domain.FindingSeverityP1, at),
		adjFinding(residueID, run1, "daemon/residue.go", domain.FindingSeverityP0, at),
		adjFinding(lowConfID, run1, "daemon/lowconf.go", domain.FindingSeverityP1, at),
		adjFinding(outSurfaceID, run1, "app/outside.swift", domain.FindingSeverityP1, at),
	}
	run1Classifications := []domain.Classification{
		adjClassification(fastID, 1, "high", "high"),
		adjClassification(residueID, 1, "high", "high"),
		// The classifier's confidence (medium) differs from the adjudication
		// proposal's confidence (low), proving the two axes are never conflated.
		adjClassification(lowConfID, 1, "high", "medium"),
		adjClassification(outSurfaceID, 1, "high", "high"),
	}
	run1Entries := []domain.FindingAdjudicationEntry{
		adjEngineEntry(t, fastID),
		adjEngineModelEntry(t, residueID, domain.ConfidenceHigh),
		adjEngineModelEntry(t, lowConfID, domain.ConfidenceLow),
		adjModelSeparateEntry(t, outSurfaceID, domain.ConfidenceHigh),
	}
	run1PolicyDigest := seedAdjudicationRun(t, ctx, st, run1, "daemon/**", run1Findings, run1Classifications, run1Entries, at)

	// Run 2 is the configuration-change fixture: a different declared scope
	// (api/**) resolves to a different policy digest, which the projection
	// surfaces so a consumer detects the change without raw SQLite.
	const run2 = domain.RunID("run-adjudication-configchange")
	configID := domain.FindingID("finding-config")
	run2PolicyDigest := seedAdjudicationRun(t, ctx, st, run2, "api/**",
		[]domain.Finding{adjFinding(configID, run2, "api/thing.go", domain.FindingSeverityP1, at)},
		[]domain.Classification{adjClassification(configID, 1, "high", "high")},
		[]domain.FindingAdjudicationEntry{adjEngineEntry(t, configID)}, at)
	if run1PolicyDigest == run2PolicyDigest {
		t.Fatalf("configuration-change fixture did not vary the resolved policy digest: %q", run1PolicyDigest)
	}

	observed := &Store{store: st}
	snapshot, err := observed.ObserveSnapshot(ctx, run1)
	if err != nil {
		t.Fatalf("ObserveSnapshot(run1): %v", err)
	}
	byFinding := map[domain.FindingID]AdjudicationDispatch{}
	for _, dispatch := range snapshot.Adjudications {
		if _, dup := byFinding[dispatch.FindingID]; dup {
			t.Fatalf("duplicate projection for finding %q", dispatch.FindingID)
		}
		byFinding[dispatch.FindingID] = dispatch
		if dispatch.Round != 1 || dispatch.Revision != 1 || dispatch.ResolvedPolicyDigest != run1PolicyDigest {
			t.Fatalf("dispatch %q round/revision/policy = %+v", dispatch.FindingID, dispatch)
		}
	}
	if len(byFinding) != 4 {
		t.Fatalf("run1 adjudications = %d, want 4: %+v", len(byFinding), snapshot.Adjudications)
	}

	fast := byFinding[fastID]
	if fast.Producer != domain.AdjudicationProducerEngine || fast.Route != domain.RouteRemediate ||
		fast.AdjudicationConfidence != nil || fast.FindingSeverity != domain.FindingSeverityP1 ||
		fast.ClassifierMateriality != "high" || fast.ClassifierConfidence != "high" || !fast.InSurface {
		t.Fatalf("fast-path dispatch = %+v", fast)
	}
	residue := byFinding[residueID]
	if residue.Producer != domain.AdjudicationProducerEngineModel || residue.AdjudicationConfidence == nil ||
		*residue.AdjudicationConfidence != domain.ConfidenceHigh || residue.FindingSeverity != domain.FindingSeverityP0 ||
		!residue.InSurface {
		t.Fatalf("model-residue dispatch = %+v", residue)
	}
	lowConf := byFinding[lowConfID]
	if lowConf.Producer != domain.AdjudicationProducerEngineModel || lowConf.AdjudicationConfidence == nil ||
		*lowConf.AdjudicationConfidence != domain.ConfidenceLow ||
		lowConf.ClassifierConfidence != "medium" || !lowConf.InSurface {
		t.Fatalf("low-confidence dispatch = %+v", lowConf)
	}
	outSurface := byFinding[outSurfaceID]
	if outSurface.Producer != domain.AdjudicationProducerModel || outSurface.InSurface ||
		outSurface.Route != domain.RouteParkSeparateWork {
		t.Fatalf("out-of-surface dispatch = %+v", outSurface)
	}

	// Calibration: among critical/high severity (P0/P1), material, in-surface findings, the
	// numerator reached deterministic engine dispatch; the denominator reached
	// engine or engine_model. Both are computed from the projection alone.
	numerator, denominator := 0, 0
	for _, dispatch := range snapshot.Adjudications {
		criticalHigh := dispatch.FindingSeverity == domain.FindingSeverityP0 || dispatch.FindingSeverity == domain.FindingSeverityP1
		material := dispatch.ClassifierMateriality == "high" || dispatch.ClassifierMateriality == "medium"
		if !criticalHigh || !material || !dispatch.InSurface {
			continue
		}
		switch dispatch.Producer {
		case domain.AdjudicationProducerEngine:
			numerator++
			denominator++
		case domain.AdjudicationProducerEngineModel:
			denominator++
		case domain.AdjudicationProducerModel:
			// Pure model residue is visible in the projection but sits outside
			// this ratio's engine-versus-engine_model contrast.
		}
	}
	if numerator != 1 || denominator != 3 {
		t.Fatalf("calibration numerator/denominator = %d/%d, want 1/3", numerator, denominator)
	}

	configSnapshot, err := observed.ObserveSnapshot(ctx, run2)
	if err != nil {
		t.Fatalf("ObserveSnapshot(run2): %v", err)
	}
	if len(configSnapshot.Adjudications) != 1 ||
		configSnapshot.Adjudications[0].ResolvedPolicyDigest != run2PolicyDigest ||
		configSnapshot.Adjudications[0].ResolvedPolicyDigest == run1PolicyDigest ||
		!configSnapshot.Adjudications[0].InSurface {
		t.Fatalf("configuration-change dispatch = %+v", configSnapshot.Adjudications)
	}
}

// TestFindingInSurfaceRejectsNonCanonicalPaths pins the declared-scope
// containment half of the engine's allowed-compatibility check: the telemetry
// axis applies the same canonical-repository-path gate EngineCompatibility runs
// before containment, so a boundary-exiting path (traversal, absolute,
// backslash, control character, or a `.`/empty segment) never reads in-surface
// even against a match-all glob, while a canonical in-scope path still does. It
// does not re-derive tree existence: a canonical in-scope path reads in-surface
// whether or not it exists in a tree.
func TestFindingInSurfaceRejectsNonCanonicalPaths(t *testing.T) {
	matchAll := []string{"**"}
	daemonScope := []string{"daemon/**"}
	tests := []struct {
		name          string
		location      *domain.FindingLocation
		declaredPaths []string
		want          bool
	}{
		{"nil location", nil, matchAll, false},
		{"empty path", &domain.FindingLocation{Path: ""}, matchAll, false},
		{"canonical in scope", &domain.FindingLocation{Path: "daemon/observe/x.go"}, daemonScope, true},
		{"canonical out of scope", &domain.FindingLocation{Path: "app/x.swift"}, daemonScope, false},
		{"canonical matches match-all", &domain.FindingLocation{Path: "daemon/x.go"}, matchAll, true},
		{"leading slash", &domain.FindingLocation{Path: "/etc/passwd"}, matchAll, false},
		{"parent traversal", &domain.FindingLocation{Path: "../secret"}, matchAll, false},
		{"dotdot segment", &domain.FindingLocation{Path: "daemon/../etc"}, matchAll, false},
		{"dot segment", &domain.FindingLocation{Path: "daemon/./x"}, matchAll, false},
		{"backslash", &domain.FindingLocation{Path: `daemon\x`}, matchAll, false},
		{"double slash", &domain.FindingLocation{Path: "daemon//x"}, matchAll, false},
		{"trailing slash", &domain.FindingLocation{Path: "daemon/"}, matchAll, false},
		{"control character", &domain.FindingLocation{Path: "daemon/\x01x"}, matchAll, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := findingInSurface(tc.location, tc.declaredPaths); got != tc.want {
				t.Fatalf("findingInSurface(%+v, %v) = %v, want %v",
					tc.location, tc.declaredPaths, got, tc.want)
			}
		})
	}
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
