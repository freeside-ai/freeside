package store

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// openClosureCard opens an effect_proposal card for a closure instance and
// binds it to merge, as the attention service does for a gate card.
func openClosureCard(
	t *testing.T, ctx context.Context, st *Store, projectID domain.ProjectID,
	instance domain.ProposalInstance, merge domain.ProspectiveMerge,
) domain.AttentionItem {
	t.Helper()
	var item domain.AttentionItem
	if err := st.Write(ctx, func(tx *WriteTx) error {
		artifact, err := instance.EvidenceArtifact()
		if err != nil {
			return err
		}
		if err := tx.PutArtifact(ctx, artifact); err != nil {
			return err
		}
		item, err = domain.NewAttentionItem(domain.AttentionItemInput{
			ID: domain.ItemID(string(instance.ID) + "/effect"), ProjectID: projectID,
			Subject: domain.Subject{Type: domain.SubjectProposalBatch, ID: domain.SubjectID(instance.ProposalBatchID)},
			Type:    domain.AttentionEffectProposal, Priority: domain.PriorityNormal,
			Reason:            "Decide the proposed effect on the source issue",
			RequestedDecision: []domain.Action{domain.ActionApprove, domain.ActionDecline, domain.ActionSnooze},
			EvidenceSnapshot:  []domain.Artifact{artifact}, ItemVersion: 1,
			InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
			PRHeadSHA: merge.CandidateHeadSHA,
		}, map[domain.Digest]bool{domain.EffectProposalRecipeDigest: true})
		if err != nil {
			return err
		}
		if err := tx.PutAttentionItem(ctx, item); err != nil {
			return err
		}
		return tx.BindProposalItem(ctx, item.ID, instance.ID, instance.Proposal.Digest, &merge)
	}); err != nil {
		t.Fatal(err)
	}
	return item
}

// TestRetireOpenClosureProposals proves the helper concludes every open,
// undecided closure card of the named run and nothing else: a decided closure
// card, the run's task-proposal card, and another run's closure card keep
// their state, and a second call retires nothing.
func TestRetireOpenClosureProposals(t *testing.T) {
	ctx := context.Background()
	st := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "store.db"), Options{})
	closureFor := func(projectID domain.ProjectID) (domain.ResolvedPolicy, domain.EffectProposal) {
		source := domain.SpecificationSource{
			Kind:         domain.SpecificationSourceIssueSubject,
			IssueSubject: &domain.IssueSubjectRef{Repo: "owner/repo", RepositoryID: 123, IssueNumber: 42},
		}
		policy, handle := closureScaffold(t, ctx, st, projectID, &source)
		proposal, err := domain.NewEffectProposal(domain.EffectSourceIssueClosure, domain.SourceIssueClosureInput{
			SubjectHandle: handle,
			Source: domain.ClosableSource{
				Present: true, Provenance: domain.ClosureProvenanceVerified, Repo: "owner/repo", RepositoryID: 123, IssueNumber: 42,
			},
			Origin: domain.ClosureFlagOriginProposeSite, Resolves: true,
		}, policy)
		if err != nil {
			t.Fatal(err)
		}
		return policy, proposal
	}
	merge := func(head string) domain.ProspectiveMerge {
		return domain.ProspectiveMerge{
			PublicationIdentity: domain.Digest("sha256:" + strings.Repeat("9", 64)),
			CandidateHeadSHA:    strings.Repeat(head, 40), BaseRef: "main", BaseSHA: strings.Repeat("b", 40),
		}
	}

	policy, proposal := closureFor("project-a")
	first := openClosureCard(t, ctx, st, "project-a", allocateClosure(t, ctx, st, proposal, "event-a1"), merge("1"))
	second := openClosureCard(t, ctx, st, "project-a", allocateClosure(t, ctx, st, proposal, "event-a2"), merge("2"))
	decided := openClosureCard(t, ctx, st, "project-a", allocateClosure(t, ctx, st, proposal, "event-a3"), merge("3"))
	decidedAt := time.Date(2026, 8, 11, 13, 0, 0, 0, time.UTC)
	if err := st.Write(ctx, func(tx *WriteTx) error {
		closed, err := decided.ConcludeByCommand(domain.StatusDismissed, decidedAt, domain.ConclusionDecision{
			CommandID: "command-decline", Action: domain.ActionDecline, DeviceID: "device-1", DeviceDisplayName: "Phone",
		})
		if err != nil {
			return err
		}
		closed.ItemVersion = decided.ItemVersion + 1
		return tx.PutAttentionItem(ctx, closed)
	}); err != nil {
		t.Fatal(err)
	}

	// The run's task proposal shares the closure's subject handle, so only the
	// effect-kind filter keeps its card out.
	taskProposal, err := domain.NewEffectProposal(domain.EffectTaskProposal, domain.TaskProposalParameters{
		SubjectHandle: domain.OpaqueSubjectHandle(domain.WorkUnitIDForRun(policy.RunID)),
		Intent:        domain.TaskProposalIntentImplement, ExpectedCostUnits: 10,
		Scope: domain.TaskProposalScope{ComponentCount: 1, DeclaredPathCount: 1},
	}, policy)
	if err != nil {
		t.Fatal(err)
	}
	var taskCard domain.AttentionItem
	if err := st.Write(ctx, func(tx *WriteTx) error {
		instance, _, err := tx.AllocateProposalInstance(ctx,
			domain.ProposalAdmissionKey{Source: domain.ProposalSourceClientCommand, SubmissionCommandID: "command-task"},
			"batch-task", taskProposal, time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC))
		if err != nil {
			return err
		}
		artifact, err := instance.EvidenceArtifact()
		if err != nil {
			return err
		}
		if err := tx.PutArtifact(ctx, artifact); err != nil {
			return err
		}
		taskCard, err = domain.NewAttentionItem(domain.AttentionItemInput{
			ID: domain.ItemID(string(instance.ID) + "/task"), ProjectID: "project-a",
			Subject: domain.Subject{Type: domain.SubjectProposalBatch, ID: "batch-task"},
			Type:    domain.AttentionTaskProposal, Priority: domain.PriorityNormal,
			Reason:            "Start the accepted work",
			RequestedDecision: []domain.Action{domain.ActionStart, domain.ActionDecline, domain.ActionSnooze},
			EvidenceSnapshot:  []domain.Artifact{artifact}, ItemVersion: 1,
			InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
		}, map[domain.Digest]bool{domain.EffectProposalRecipeDigest: true})
		if err != nil {
			return err
		}
		if err := tx.PutAttentionItem(ctx, taskCard); err != nil {
			return err
		}
		return tx.BindProposalItem(ctx, taskCard.ID, instance.ID, taskProposal.Digest, nil)
	}); err != nil {
		t.Fatal(err)
	}

	_, otherProposal := closureFor("project-b")
	other := openClosureCard(t, ctx, st, "project-b", allocateClosure(t, ctx, st, otherProposal, "event-b1"), merge("4"))

	const reason = "Superseded because the publication was refused; a rerun that publishes proposes closure again."
	at := time.Date(2026, 8, 11, 14, 0, 0, 0, time.UTC)
	retire := func() []domain.ItemID {
		t.Helper()
		var retired []domain.ItemID
		if err := st.Write(ctx, func(tx *WriteTx) error {
			var err error
			retired, err = tx.RetireOpenClosureProposals(ctx, policy.RunID, reason, at)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return retired
	}
	want := []domain.ItemID{first.ID, second.ID}
	slices.Sort(want)
	if got := retire(); !slices.Equal(got, want) {
		t.Fatalf("retired = %v, want %v", got, want)
	}
	if got := retire(); len(got) != 0 {
		t.Fatalf("second call retired %v, want nothing", got)
	}

	read := func(id domain.ItemID) domain.AttentionItem {
		t.Helper()
		var item domain.AttentionItem
		if err := st.Read(ctx, func(tx *ReadTx) error {
			var err error
			item, err = tx.GetAttentionItem(ctx, id)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return item
	}
	for _, id := range want {
		item := read(id)
		if item.Status != domain.StatusSuperseded || item.ItemVersion != 2 || item.Conclusion == nil ||
			item.Conclusion.Cause != domain.ConcludedBySupersession || item.Conclusion.Reason != reason ||
			!item.Conclusion.ConcludedAt.Equal(at) || item.Conclusion.SuccessorItemID != nil {
			t.Fatalf("retired card %s = status %q version %d conclusion %+v", id, item.Status, item.ItemVersion, item.Conclusion)
		}
	}
	if item := read(decided.ID); item.Status != domain.StatusDismissed || item.Conclusion.Cause != domain.ConcludedByDecision {
		t.Fatalf("decided card = status %q conclusion %+v, want dismissed by decision", item.Status, item.Conclusion)
	}
	for _, id := range []domain.ItemID{taskCard.ID, other.ID} {
		if item := read(id); item.Status != domain.StatusOpen || item.ItemVersion != 1 {
			t.Fatalf("card %s = status %q version %d, want open at version 1", id, item.Status, item.ItemVersion)
		}
	}
}

// TestRecordPublicationSuccessorRetiresClosureCardOnFirstSeal proves the seal
// retires the replaced publication's open closure card with the origin's
// sentence, and that a replayed seal retires nothing further.
func TestRecordPublicationSuccessorRetiresClosureCardOnFirstSeal(t *testing.T) {
	ctx := context.Background()
	f := seedUnpublishedReentry(t, publicationReservationKind)
	var proposal domain.EffectProposal
	if err := f.st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.RegisterProject(ctx, domain.Project{ID: "project-1", Repo: "owner/repo", RepositoryID: 424242}); err != nil {
			return err
		}
		policy, err := tx.GetResolvedPolicy(ctx, f.runID)
		if err != nil {
			return err
		}
		declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
			CompletionCriterion: domain.CompletionBoundPRMerged,
		}, f.runID, "project-1", reentryAt.Add(-time.Hour))
		if err != nil {
			return err
		}
		if err := tx.RecordWorkUnitDeclaration(ctx, declaration); err != nil {
			return err
		}
		proposal, err = domain.NewEffectProposal(domain.EffectSourceIssueClosure, domain.SourceIssueClosureInput{
			SubjectHandle: domain.OpaqueSubjectHandle(domain.WorkUnitIDForRun(f.runID)),
			Source: domain.ClosableSource{
				Present: true, Provenance: domain.ClosureProvenanceRecommended, Repo: "owner/repo", RepositoryID: 424242,
			},
			ProposedTarget: domain.IssueSubjectRef{Repo: "owner/repo", RepositoryID: 424242, IssueNumber: 9},
			Origin:         domain.ClosureFlagOriginProposeSite, Resolves: true,
		}, policy)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	merge := domain.ProspectiveMerge{
		PublicationIdentity: domain.Digest("sha256:" + strings.Repeat("9", 64)),
		CandidateHeadSHA:    reentryHead1, BaseRef: "main", BaseSHA: reentryBase1,
	}
	replaced := openClosureCard(t, ctx, f.st, "project-1", allocateClosure(t, ctx, f.st, proposal, "event-first"), merge)

	f.audit(t, "owner/repo", reentryBase2)
	authority := f.authority(domain.ProductionReadyItemID(f.runID), f.prior, reentryBase2)
	if err := f.record(authority); err != nil {
		t.Fatalf("seal the re-entry: %v", err)
	}
	read := func(id domain.ItemID) domain.AttentionItem {
		t.Helper()
		var item domain.AttentionItem
		if err := f.st.Read(ctx, func(tx *ReadTx) error {
			var err error
			item, err = tx.GetAttentionItem(ctx, id)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return item
	}
	want, _ := closureRetirementReason(domain.PublicationSuccessorUnpublishedBaseAdvance)
	if item := read(replaced.ID); item.Status != domain.StatusSuperseded || item.Conclusion == nil ||
		item.Conclusion.Reason != want {
		t.Fatalf("replaced card = status %q conclusion %+v, want superseded with %q", item.Status, item.Conclusion, want)
	}

	// A card opened after the seal belongs to the successor's publication; a
	// replayed seal must leave it open.
	merge.BaseSHA = reentryBase2
	current := openClosureCard(t, ctx, f.st, "project-1", allocateClosure(t, ctx, f.st, proposal, "event-second"), merge)
	if err := f.record(authority); err != nil {
		t.Fatalf("replay the seal: %v", err)
	}
	if item := read(current.ID); item.Status != domain.StatusOpen || item.ItemVersion != 1 {
		t.Fatalf("current card after a replayed seal = status %q version %d, want open at version 1", item.Status, item.ItemVersion)
	}
}

// TestClosureRetirementReasonByOrigin pins which successor origins retire the
// replaced closure card: every origin but an external review, whose in-place
// cycle keeps the merge the card binds.
func TestClosureRetirementReasonByOrigin(t *testing.T) {
	for _, origin := range domain.AllPublicationSuccessorOrigins {
		reason, retires := closureRetirementReason(origin)
		if want := origin != domain.PublicationSuccessorExternalReview; retires != want {
			t.Errorf("%s: retires = %v, want %v", origin, retires, want)
		}
		if retires && reason == "" {
			t.Errorf("%s: retiring origin has no reason", origin)
		}
	}
}
