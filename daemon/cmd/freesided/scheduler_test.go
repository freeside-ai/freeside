package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

func schedTestStore(t *testing.T) *store.Store {
	t.Helper()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		},
	})
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return st
}

func schedTestResolvedPolicy(runID domain.RunID) domain.ResolvedPolicy {
	policy, err := domain.NewResolvedPolicy(runID, []domain.PolicyKey{{
		Key: "driver", Value: "claude",
		Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride,
			Digest: domain.Digest("sha256:" + strings.Repeat("ab", 32)),
		},
	}})
	if err != nil {
		panic(err)
	}
	return policy
}

// seedCaptureRun persists the run and resolved policy the declaration
// re-gate re-derives from (an empty declared scope pairs with a policy
// declaring no paths key), so capture fixtures satisfy the reconstruction
// gates the production flow satisfies by construction.
func seedCaptureRun(t *testing.T, st *store.Store, runID domain.RunID) {
	t.Helper()
	ctx := context.Background()
	policy, err := domain.NewResolvedPolicy(runID, []domain.PolicyKey{{
		Key: "driver", Value: "claude",
		Provenance: domain.KeyProvenance{Source: "override", Digest: domain.Digest("sha256:" + strings.Repeat("ab", 32))},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, domain.Run{
			ID: runID, ProjectID: "project-1",
			SpecDigest: "sha256:spec", PolicyDigest: policy.Digest,
		}); err != nil {
			return err
		}
		return tx.PutResolvedPolicy(ctx, policy)
	}); err != nil {
		t.Fatal(err)
	}
}

type readyBindingAuthority struct {
	invocationID            domain.InvocationID
	publicationInvocationID domain.InvocationID
	identity                domain.Digest
}

func seedReadyBindingAuthority(
	t *testing.T, st *store.Store, runID domain.RunID, headSHA string,
) readyBindingAuthority {
	t.Helper()
	ctx := context.Background()
	var run domain.Run
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		run, err = tx.GetRun(ctx, runID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	invocationID := domain.InvocationID("inv-ready-" + string(runID))
	stageID := domain.StageID("stage-ready-" + string(runID))
	attemptID := domain.AttemptID("attempt-ready-" + string(runID))
	run.Stages = append(run.Stages, domain.Stage{
		ID: stageID, RunID: runID, Name: "ready binding authority",
		Attempts: []domain.Attempt{{
			ID: attemptID, StageID: stageID, Number: 1, InvocationID: invocationID,
		}},
	})
	if err := st.Write(ctx, func(tx *store.WriteTx) error { return tx.PutRun(ctx, run) }); err != nil {
		t.Fatal(err)
	}
	admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: invocationID, RunID: runID, StageID: stageID, AttemptID: attemptID,
		Backend: "ready-binding-test", Capabilities: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		OperatingMode: domain.ModeAttendedDev, CredentialMode: domain.CredentialSubscriptionContained,
		EgressProfile: domain.EgressCleanVerification,
		ImageRef:      domain.ImageRef("ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("ab", 32)),
		SpecDigest:    run.SpecDigest, PolicyDigest: run.PolicyDigest, InputDigest: "sha256:ready-input",
		Base: domain.BaseRevision{
			Repo: "owner/repo", RepositoryID: 424242, BaseRef: "main", BaseSHA: "deadbeef",
		},
		Workspace: "ready-binding-workspace", AdmittedAt: activeResourceTestTime.Add(-3 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	export, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: invocationID, AdmissionID: admission.ID,
		ObservedBaseSHA: admission.Base.BaseSHA, HeadSHA: headSHA,
		ManifestDigest: "sha256:ready-manifest", RecordedAt: activeResourceTestTime.Add(-2 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := publish.DeriveIdentity(publish.IdentityInput{
		Repo: "owner/repo", BaseRef: "main", SourceHeadSHA: headSHA,
		ArtifactDigests: []domain.Digest{"sha256:ready-artifact"},
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome := publish.Outcome{
		Identity: identity.Digest(), Repo: "owner/repo", BaseRef: "main", HeadSHA: headSHA,
		Branch: identity.BranchName(), PRNumber: 450, EvidenceEligible: true,
	}
	payload, err := outcome.Encode()
	if err != nil {
		t.Fatal(err)
	}
	publicationInvocationID := domain.InvocationID("publish-production-" + string(runID))
	intentPayload, err := (publish.Intent{
		FormatVersion: publish.IntentFormatCurrent,
		Identity:      identity.Digest(), InvocationID: publicationInvocationID,
		Branch: identity.BranchName(),
		Repo:   "owner/repo", BaseRef: "main", SourceHeadSHA: headSHA,
		AuthorizationID:       domain.Digest("sha256:" + strings.Repeat("cd", 32)),
		ProducingInvocationID: invocationID, ReservationRunID: runID,
	}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	intentKey, err := publish.IntentKey(publicationInvocationID, publish.IntentKindPublication)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.RecordExecutionAdmission(ctx, admission); err != nil {
			return err
		}
		if err := tx.RecordExecutionExport(ctx, export); err != nil {
			return err
		}
		if _, _, err := tx.EnqueueOutbox(ctx, intentKey, publish.IntentKindPublication, intentPayload); err != nil {
			return err
		}
		if err := tx.MarkOutboxDispatched(ctx, intentKey); err != nil {
			return err
		}
		_, _, err := tx.RecordInbox(ctx, publish.OutcomeKey(identity), publish.IntentKindOutcome, payload)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return readyBindingAuthority{
		invocationID: invocationID, publicationInvocationID: publicationInvocationID,
		identity: identity.Digest(),
	}
}

// capturedRun seeds the store with a declared, PR-bound work unit and
// returns the ready item whose subject names its run, so the
// active-resource reconciler's §5.18 completion pass has a unit to settle.
func capturedRun(t *testing.T, st *store.Store) domain.AttentionItem {
	return capturedRunNamed(t, st, "run-cap", "item-ready-cap")
}

func capturedRunNamed(
	t *testing.T, st *store.Store, runID domain.RunID, itemID domain.ItemID,
) domain.AttentionItem {
	boundIssue := 443
	return capturedRunWithCriterion(
		t, st, runID, itemID, domain.CompletionBoundIssueClosedByMergedPR, &boundIssue,
	)
}

func capturedRunWithCriterion(
	t *testing.T, st *store.Store, runID domain.RunID, itemID domain.ItemID,
	criterion domain.CompletionCriterionKind, boundIssue *int,
) domain.AttentionItem {
	t.Helper()
	return capturedRunAtHead(t, st, runID, itemID, criterion, boundIssue, "cafed00d")
}

// capturedRunAtHead is capturedRunWithCriterion publishing head, for a test
// whose coordinates must validate as full SHAs.
func capturedRunAtHead(
	t *testing.T, st *store.Store, runID domain.RunID, itemID domain.ItemID,
	criterion domain.CompletionCriterionKind, boundIssue *int, head string,
) domain.AttentionItem {
	t.Helper()
	ctx := context.Background()
	seedCaptureRun(t, st, runID)
	declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
		CompletionCriterion: criterion,
		BoundIssue:          boundIssue,
	}, runID, "project-1", time.Date(2026, 2, 3, 3, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	binding := domain.WorkUnitPRBinding{
		UnitID: declaration.ID, Repo: "owner/repo", RepositoryID: 424242,
		PRNumber: 450, BaseRef: "main", HeadSHA: head,
		RecordedAt: time.Date(2026, 2, 3, 3, 30, 0, 0, time.UTC),
	}
	authority := seedReadyBindingAuthority(t, st, runID, binding.HeadSHA)
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RecordWorkUnitDeclaration(ctx, declaration); err != nil {
			return err
		}
		return tx.RecordWorkUnitPRBinding(ctx, binding)
	}); err != nil {
		t.Fatal(err)
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: itemID, ProjectID: "project-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason:            "published and verified",
		RequestedDecision: []domain.Action{domain.ActionOpenPR, domain.ActionMarkSeen},
		// PRHeadSHA is the head anchor the capture pass verifies the
		// binding against.
		PRHeadSHA:   head,
		PRReference: &domain.PRReference{Repo: "owner/repo", Number: 450},
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate,
		Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordReadyItemPRBinding(ctx, domain.ReadyItemPRBinding{
			ItemID: item.ID, RunID: runID,
			ProducingInvocationID: authority.invocationID, PublicationIdentity: authority.identity,
			PublicationInvocationID: authority.publicationInvocationID,
			Repo:                    binding.Repo,
			RepositoryID:            binding.RepositoryID, PRNumber: binding.PRNumber,
			BaseRef: binding.BaseRef, HeadSHA: binding.HeadSHA,
			RecordedAt: binding.RecordedAt,
		})
	}); err != nil {
		t.Fatal(err)
	}
	// Publication convergence records the ready milestone alongside the ready
	// item; the completion mirror is written only over a standing ready.
	publicationInvocation := authority.publicationInvocationID
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.AppendRunMilestone(ctx, domain.RunMilestone{
			RunID: runID, Kind: domain.MilestonePublicationReady,
			InvocationID: &publicationInvocation, RecordedAt: binding.RecordedAt,
		})
	}); err != nil {
		t.Fatal(err)
	}
	return item
}
