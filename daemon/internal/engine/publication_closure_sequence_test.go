package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

var (
	closureSequenceBase1 = strings.Repeat("1", 40)
	closureSequenceBase2 = strings.Repeat("2", 40)
	closureSequenceHead1 = strings.Repeat("a", 40)
	closureSequenceHead2 = strings.Repeat("b", 40)
	closureSequenceAt    = time.Date(2026, 10, 21, 3, 0, 0, 0, time.UTC)
)

// closureSequenceFixture is a run under the closure human gate whose first
// publication holds a bare reservation, reviewed clean at closureSequenceBase1
// and audited there, so an unpublished base-advance re-entry can seal.
type closureSequenceFixture struct {
	st      *store.Store
	w       *productionPublicationWorkflow
	policy  domain.ResolvedPolicy
	taskID  domain.TaskID
	prior   domain.ReviewRecord
	opened  map[domain.ProposalInstanceID]domain.ItemID
	ordered []domain.ProposalInstanceID
}

func seedClosureSequence(t *testing.T) *closureSequenceFixture {
	t.Helper()
	ctx := context.Background()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		},
	})
	runID := domain.RunID("run-closure-sequence")
	invocationID := domain.InvocationID("inv-closure-sequence")
	stageID := domain.StageID("stage-closure-sequence")
	attemptID := domain.AttemptID("attempt-closure-sequence")
	provenance := domain.KeyProvenance{Source: domain.ProvenanceOverride, Digest: domain.Digest("sha256:" + strings.Repeat("a", 64))}
	policy, err := domain.NewResolvedPolicy(runID, []domain.PolicyKey{
		{Key: "paths", Value: "daemon/", Provenance: provenance},
		{Key: policySourceIssueClosureGate, Value: "true", Provenance: provenance},
	})
	if err != nil {
		t.Fatal(err)
	}
	source := domain.SpecificationSource{
		Kind: domain.SpecificationSourceIssueSubject,
		IssueSubject: &domain.IssueSubjectRef{
			Repo: closureStoreRepo, RepositoryID: closureStoreRepoID, IssueNumber: closureStoreIssue,
		},
	}
	run := domain.Run{
		ID: runID, ProjectID: closureStoreProject,
		SpecDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)), PolicyDigest: policy.Digest,
		Stages: []domain.Stage{{
			ID: stageID, RunID: runID, Name: "implementation",
			Attempts: []domain.Attempt{{ID: attemptID, StageID: stageID, Number: 1, InvocationID: invocationID}},
		}},
	}
	admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: invocationID, RunID: runID, StageID: stageID, AttemptID: attemptID,
		Backend: "closure-sequence-test", Capabilities: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		OperatingMode: domain.ModeAttendedDev, CredentialMode: domain.CredentialSubscriptionContained,
		EgressProfile: domain.EgressCleanVerification,
		ImageRef:      domain.ImageRef("ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("a", 64)),
		SpecDigest:    run.SpecDigest, PolicyDigest: run.PolicyDigest, InputDigest: "sha256:input",
		Base: domain.BaseRevision{
			Repo: closureStoreRepo, RepositoryID: closureStoreRepoID,
			BaseRef: closureStoreBaseRef, BaseSHA: closureSequenceBase1,
		},
		Workspace: "workspace", AdmittedAt: closureSequenceAt.Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	reviewID := domain.InvocationID("review-closure-sequence")
	prior, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: reviewID, RunID: runID, Round: 1,
		Admission:           fake.RecordAdmission(domain.RoleReviewer, runID, reviewID),
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		InstructionDigest:   domain.Digest("sha256:" + strings.Repeat("d", 64)),
		BaseSHA:             closureSequenceBase1, HeadSHA: closureSequenceHead1,
		CompletedAt:        closureSequenceAt.Add(-time.Minute),
		CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("e", 64)), Outcome: domain.ReviewClean,
	})
	if err != nil {
		t.Fatal(err)
	}
	var taskID domain.TaskID
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.RegisterProject(ctx, domain.Project{
			ID: closureStoreProject, Repo: closureStoreRepo, RepositoryID: closureStoreRepoID,
		}); err != nil {
			return err
		}
		task, err := tx.GetOrCreateTask(ctx, closureStoreProject, source)
		if err != nil {
			return err
		}
		taskID = task.ID
		run.TaskID = task.ID
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
			return err
		}
		if err := tx.RecordExecutionAdmission(ctx, admission); err != nil {
			return err
		}
		declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
			CompletionCriterion: domain.CompletionBoundPRMerged,
			DeclaredPaths:       domain.CanonicalDeclaredPaths(policy),
		}, runID, closureStoreProject, closureSequenceAt.Add(-time.Hour))
		if err != nil {
			return err
		}
		if err := tx.RecordWorkUnitDeclaration(ctx, declaration); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, prior, nil); err != nil {
			return err
		}
		reservation, err := publish.NewReservation(domain.ProductionPublicationInvocationID(runID), runID)
		if err != nil {
			return err
		}
		return publish.ClaimInvocation(ctx, tx, reservation)
	}); err != nil {
		t.Fatalf("seed closure sequence: %v", err)
	}
	blobs, err := signet.NewBlobStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	now := func() time.Time { return closureSequenceAt }
	f := &closureSequenceFixture{
		st: st, policy: policy, taskID: taskID, prior: prior,
		opened: map[domain.ProposalInstanceID]domain.ItemID{},
		w: &productionPublicationWorkflow{
			store: st, attention: signet.NewService(st, signet.WithBlobStore(blobs), signet.WithClock(now)), now: now,
		},
	}
	f.audit(t, closureSequenceBase1)
	return f
}

// audit records a workflow audit of the run's repository that observed tip.
func (f *closureSequenceFixture) audit(t *testing.T, tip string) {
	t.Helper()
	evidence, err := domain.NewWorkflowAuditEvidence([]byte(
		`{"version":"freeside-workflow-audit/v2","repo":"` + closureStoreRepo + `","workflows":[]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.WriteInternal(context.Background(), func(tx *store.InternalTx) error {
		_, err := tx.RecordWorkflowAudit(context.Background(), domain.WorkflowAudit{
			Repo: closureStoreRepo, AuditedCommitSHA: tip, AuditedAt: closureSequenceAt,
			WorkflowAuditDigest: evidence.Digest(), Evidence: &evidence,
			EffectiveTokenPerms: domain.TokenPermissionsReadOnly,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// publish runs the closure step of one publication the way the workflow does:
// admit the publication's own proposal, then bind its prospective merge, which
// under the human gate opens the gate card.
func (f *closureSequenceFixture) publish(
	t *testing.T, publicationID domain.InvocationID, head, base string,
) domain.ProspectiveMerge {
	t.Helper()
	ctx := context.Background()
	task := productionPublicationTask{
		RunID: f.policy.RunID, PublicationID: publicationID, HeadSHA: head,
		Publication: ProductionPublication{Recipe: clientPublicationRecipeV2},
	}
	binding := closureStoreBinding(f.policy, f.taskID)
	binding.admission.Base.BaseSHA = base
	source, target, ok, err := f.w.decideClosableSource(ctx, task, binding)
	if err != nil || !ok {
		t.Fatalf("decide closable source for %s: ok=%t err=%v", publicationID, ok, err)
	}
	want := productionClosureCheckpoint{
		Version: productionClosureCheckpointVersion, RunID: task.RunID, PublicationID: publicationID,
	}
	closure, admitted, err := f.w.admitClosureProposal(ctx,
		productionClosureCheckpointKey(task.RunID, publicationID), want, task, source, target,
		domain.ClosureFlagOriginProposeSite, true, "")
	if err != nil || !admitted {
		t.Fatalf("admit closure proposal for %s: admitted=%t err=%v", publicationID, admitted, err)
	}
	merge := domain.ProspectiveMerge{
		PublicationIdentity: domain.Digest("sha256:" + strings.Repeat(string(publicationID[len(publicationID)-1]), 64)),
		CandidateHeadSHA:    head, BaseRef: closureStoreBaseRef, BaseSHA: base,
	}
	if err := f.w.bindClosureMerge(ctx, task.RunID, binding, closure, merge); err != nil {
		t.Fatalf("bind closure merge for %s: %v", publicationID, err)
	}
	if err := f.st.Read(ctx, func(tx *store.ReadTx) error {
		item, _, found, err := tx.OpenEffectItemForInstance(ctx, closure.InstanceID)
		if err == nil && !found {
			t.Fatalf("publication %s opened no closure card", publicationID)
		}
		f.opened[closure.InstanceID] = item.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.ordered = append(f.ordered, closure.InstanceID)
	return merge
}

// requireOpen asserts the run has at most one open closure card: none when
// want is nil, otherwise one whose bound merge is want.
func (f *closureSequenceFixture) requireOpen(t *testing.T, step string, want *domain.ProspectiveMerge) {
	t.Helper()
	ctx := context.Background()
	var open []domain.ProspectiveMerge
	if err := f.st.Read(ctx, func(tx *store.ReadTx) error {
		for _, instanceID := range f.ordered {
			_, merge, found, err := tx.OpenEffectItemForInstance(ctx, instanceID)
			if err != nil {
				return err
			}
			if found {
				open = append(open, *merge)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	switch {
	case want == nil && len(open) != 0:
		t.Fatalf("%s: open closure cards = %+v, want none", step, open)
	case want != nil && (len(open) != 1 || open[0] != *want):
		t.Fatalf("%s: open closure cards = %+v, want one at %+v", step, open, *want)
	}
}

// requireRetired asserts the card a publication opened was superseded by the
// daemon with reason.
func (f *closureSequenceFixture) requireRetired(t *testing.T, step string, publication int, reason string) {
	t.Helper()
	var item domain.AttentionItem
	if err := f.st.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		item, err = tx.GetAttentionItem(context.Background(), f.opened[f.ordered[publication]])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if item.Status != domain.StatusSuperseded || item.Conclusion == nil ||
		item.Conclusion.Cause != domain.ConcludedBySupersession || item.Conclusion.Reason != reason {
		t.Fatalf("%s: card %s = status %q conclusion %+v, want superseded with %q",
			step, item.ID, item.Status, item.Conclusion, reason)
	}
}

// TestClosureCardsRetireAcrossTheExitRunSequence drives the Wave 8 exit-run
// shape (#2112): each publication admits its own closure proposal, and every
// event that replaces or refuses a publication retires its open card, so at
// most one card is open and it names the current publication's merge.
func TestClosureCardsRetireAcrossTheExitRunSequence(t *testing.T) {
	ctx := context.Background()
	f := seedClosureSequence(t)
	runID := f.policy.RunID

	first := f.publish(t, domain.ProductionPublicationInvocationID(runID), closureSequenceHead1, closureSequenceBase1)
	f.requireOpen(t, "first publication", &first)

	// The target base advances before the first publication commits an
	// intent; the re-entry seals through the store's real gates.
	f.audit(t, closureSequenceBase2)
	reentry := domain.PublicationSuccessor{
		Version: domain.PublicationUnpublishedReentryVersion, Origin: domain.PublicationSuccessorUnpublishedBaseAdvance,
		RunID: runID, PredecessorItemID: domain.ProductionReadyItemID(runID),
		PriorReviewInvocationID: f.prior.InvocationID, ReviewRound: f.prior.Round + 1,
		Reentry: &domain.PublicationSuccessorReentry{
			Reason: domain.ReadinessInvalidationBaseAdvanced, BaseSHA: closureSequenceBase2, HeadSHA: closureSequenceHead1,
		},
	}
	if err := f.st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.RecordPublicationSuccessor(ctx, reentry)
	}); err != nil {
		t.Fatalf("seal the unpublished re-entry: %v", err)
	}
	f.requireOpen(t, "unpublished re-entry", nil)
	f.requireRetired(t, "unpublished re-entry", 0,
		"Superseded because the target base advanced; the re-entered publication proposes closure again.")

	second := f.publish(t, reentry.PublicationID(), closureSequenceHead1, closureSequenceBase2)
	f.requireOpen(t, "re-entered publication", &second)

	// Return to Agent retires the card in the feedback request's transaction
	// (TestReturnToAgentRecordsFeedbackAndCandidatePatchForResumedWork covers
	// that call site); this step applies the same write.
	if err := f.st.Write(ctx, func(tx *store.WriteTx) error {
		_, err := tx.RetireOpenClosureProposals(ctx, runID, closureReturnedToAgentReason, closureSequenceAt)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.requireOpen(t, "return to agent", nil)
	f.requireRetired(t, "return to agent", 1, closureReturnedToAgentReason)

	third := f.publish(t, "publication-feedback-successor-3", closureSequenceHead2, closureSequenceBase2)
	f.requireOpen(t, "successor publication", &third)

	// A target_base_advanced refusal that completes blocked retires the card
	// in completeBlockedTask (TestBlockedPublicationRetiresItsClosureCard
	// drives that path); this step applies the same write.
	if err := f.w.retireBlockedClosureProposals(ctx, runID); err != nil {
		t.Fatal(err)
	}
	f.requireOpen(t, "refused publication", nil)
	f.requireRetired(t, "refused publication", 2, closureBlockedReason)
}
