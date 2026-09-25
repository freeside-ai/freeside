package engine

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

const (
	closureStoreRepo    = "owner/repo"
	closureStoreRepoID  = int64(123)
	closureStoreIssue   = 7
	closureStoreHead    = "head000000000000000000000000000000000000"
	closureStoreBaseRef = "main"
	closureStoreBaseSHA = "base000000000000000000000000000000000000"
	closureStoreProject = domain.ProjectID("proj-1")
)

// seedClosureChain seeds the durable rows a source-issue-closure proposal needs
// for runID: a project, an issue-subject task, a run, a resolved policy, and a
// work-unit declaration. It returns the store, the resolved policy, and the
// task id so a caller can build a productionBinding.
func seedClosureChain(t *testing.T, runID domain.RunID) (*store.Store, domain.ResolvedPolicy, domain.TaskID) {
	source := &domain.SpecificationSource{
		Kind: domain.SpecificationSourceIssueSubject,
		IssueSubject: &domain.IssueSubjectRef{
			Repo: closureStoreRepo, RepositoryID: closureStoreRepoID, IssueNumber: closureStoreIssue,
		},
	}
	return seedClosureChainWithSource(t, runID, source)
}

func seedClosureChainWithSource(t *testing.T, runID domain.RunID, source *domain.SpecificationSource) (*store.Store, domain.ResolvedPolicy, domain.TaskID) {
	t.Helper()
	ctx := context.Background()
	s := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{})
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	policy, err := domain.NewResolvedPolicy(runID, []domain.PolicyKey{{
		Key: "paths", Value: "daemon/",
		Provenance: domain.KeyProvenance{Source: domain.ProvenanceOverride, Digest: domain.Digest("sha256:" + strings.Repeat("a", 64))},
	}})
	if err != nil {
		t.Fatalf("resolved policy: %v", err)
	}
	var taskID domain.TaskID
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.RegisterProject(ctx, domain.Project{ID: closureStoreProject, Repo: closureStoreRepo, RepositoryID: closureStoreRepoID}); err != nil {
			return err
		}
		if source != nil {
			task, err := tx.GetOrCreateTask(ctx, closureStoreProject, *source)
			if err != nil {
				return err
			}
			taskID = task.ID
		}
		if err := tx.PutRun(ctx, domain.Run{
			ID: runID, ProjectID: closureStoreProject, TaskID: taskID,
			SpecDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)), PolicyDigest: policy.Digest, Stages: []domain.Stage{},
		}); err != nil {
			return err
		}
		if taskID == "" {
			run, err := tx.GetRun(ctx, runID)
			if err != nil {
				return err
			}
			taskID = run.TaskID
		}
		if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
			return err
		}
		declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
			CompletionCriterion: domain.CompletionBoundPRMerged,
			DeclaredPaths:       domain.CanonicalDeclaredPaths(policy),
		}, runID, closureStoreProject, at.Add(-time.Hour))
		if err != nil {
			return err
		}
		return tx.RecordWorkUnitDeclaration(ctx, declaration)
	}); err != nil {
		t.Fatalf("seed closure chain: %v", err)
	}
	return s, policy, taskID
}

func closureStoreBinding(policy domain.ResolvedPolicy, taskID domain.TaskID) productionBinding {
	return productionBinding{
		run: domain.Run{ID: policy.RunID, ProjectID: closureStoreProject, TaskID: taskID},
		admission: domain.ExecutionAdmission{Base: domain.BaseRevision{
			Repo: closureStoreRepo, RepositoryID: closureStoreRepoID,
			BaseRef: closureStoreBaseRef, BaseSHA: closureStoreBaseSHA,
		}},
		resolvedPolicy: policy,
	}
}

// TestDecideClosableSourceVerifiesIssueSubject proves the engine mirrors the
// store's verified determination for a same-repository issue-subject task, with
// no proposed target (the daemon uses its bound issue).
func TestDecideClosableSourceVerifiesIssueSubject(t *testing.T) {
	ctx := context.Background()
	s, policy, taskID := seedClosureChain(t, "run-decide")
	w := &productionPublicationWorkflow{store: s}
	task := productionPublicationTask{
		RunID: policy.RunID, PublicationID: "pub-1", HeadSHA: closureStoreHead,
		Publication: ProductionPublication{Recipe: clientPublicationRecipeV2},
	}
	source, target, ok, err := w.decideClosableSource(ctx, task, closureStoreBinding(policy, taskID))
	if err != nil {
		t.Fatalf("decide closable source: %v", err)
	}
	if !ok {
		t.Fatal("issue-subject source not closable; want verified")
	}
	if source.Provenance != domain.ClosureProvenanceVerified || source.IssueNumber != closureStoreIssue {
		t.Errorf("source = %+v, want verified issue %d", source, closureStoreIssue)
	}
	if target != (domain.IssueSubjectRef{}) {
		t.Errorf("verified source carries a proposed target %+v; want none", target)
	}

	// The repository id decides (#1537): after a rename the daemon runs under
	// the new name, the subject bound under the old name stays verified, and the
	// source reports the current name.
	renamed := closureStoreBinding(policy, taskID)
	renamed.admission.Base.Repo = "owner/renamed"
	source, _, ok, err = w.decideClosableSource(ctx, task, renamed)
	if err != nil || !ok {
		t.Fatalf("decide closable source after rename: ok=%t err=%v", ok, err)
	}
	want := domain.ClosableSource{
		Present: true, Provenance: domain.ClosureProvenanceVerified,
		Repo: "owner/renamed", RepositoryID: closureStoreRepoID, IssueNumber: closureStoreIssue,
	}
	if source != want {
		t.Errorf("source after rename = %+v, want %+v", source, want)
	}
	rebound := closureStoreBinding(policy, taskID)
	rebound.admission.Base.RepositoryID++
	if _, _, ok, err := w.decideClosableSource(ctx, task, rebound); err != nil || ok {
		t.Errorf("same-name foreign-id binding: ok=%t err=%v, want no proposal", ok, err)
	}
}

// TestReconcileClosureDefaultPolicyRecordsBindingApproval proves the engine
// emission end to end under the default policy: it decides the verified source,
// admits the proposal (the store re-gate confirms the engine's closable-source
// determination matches its own), and records a policy approval that authorizes
// closing for the bound merge.
func TestReconcileClosureDefaultPolicyRecordsBindingApproval(t *testing.T) {
	ctx := context.Background()
	s, policy, taskID := seedClosureChain(t, "run-emit")
	w := &productionPublicationWorkflow{store: s}
	binding := closureStoreBinding(policy, taskID)
	task := productionPublicationTask{
		RunID: policy.RunID, PublicationID: "pub-1", HeadSHA: closureStoreHead,
		Publication: ProductionPublication{Recipe: clientPublicationRecipeV2},
	}

	source, target, ok, err := w.decideClosableSource(ctx, task, binding)
	if err != nil || !ok {
		t.Fatalf("decide closable source: ok=%t err=%v", ok, err)
	}
	key := productionClosureCheckpointKey(task.RunID, task.PublicationID)
	want := productionClosureCheckpoint{
		Version: productionClosureCheckpointVersion, RunID: task.RunID, PublicationID: task.PublicationID,
	}
	decided, admitted, err := w.admitClosureProposal(ctx, key, want, task, source, target, domain.ClosureFlagOriginProposeSite, true, "")
	if err != nil {
		t.Fatalf("admit closure proposal: %v", err)
	}
	if !admitted || decided.InstanceID == "" {
		t.Fatalf("proposal not admitted (admitted=%t id=%q)", admitted, decided.InstanceID)
	}
	instanceID := decided.InstanceID

	merge := domain.ProspectiveMerge{
		PublicationIdentity: domain.Digest("sha256:" + strings.Repeat("d", 64)),
		CandidateHeadSHA:    closureStoreHead, BaseRef: closureStoreBaseRef, BaseSHA: closureStoreBaseSHA,
	}
	closure := productionClosureCheckpoint{
		Version: productionClosureCheckpointVersion, RunID: policy.RunID, PublicationID: "pub-1",
		HasProposal: true, InstanceID: instanceID, Origin: domain.ClosureFlagOriginProposeSite,
	}
	if err := w.bindClosureMerge(ctx, policy.RunID, binding, closure, merge); err != nil {
		t.Fatalf("bind closure merge: %v", err)
	}

	// The recorded policy approval authorizes closing for the bound merge, and
	// no attention item holds the pull request.
	var approval *domain.ClosureApproval
	var proposal domain.EffectProposal
	var openFound bool
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		instance, err := tx.GetProposalInstance(ctx, instanceID)
		if err != nil {
			return err
		}
		proposal = instance.Proposal
		approval, err = tx.ClosureApprovalForInstance(ctx, instanceID)
		if err != nil {
			return err
		}
		_, _, openFound, err = tx.OpenEffectItemForInstance(ctx, instanceID)
		return err
	}); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if approval == nil {
		t.Fatal("no policy approval recorded")
	}
	if approval.Actor != domain.ClosureApprovalActorPolicy {
		t.Errorf("approval actor = %q, want policy", approval.Actor)
	}
	if !approval.AuthorizesClose(proposal, merge) {
		t.Error("recorded policy approval does not authorize the bound merge")
	}
	if openFound {
		t.Error("default-policy propose_site opened an attention item; want none")
	}
}

// TestBindClosureMergeRefusesForeignInstance proves the reconstruction re-gate: a
// checkpoint instance id that belongs to another run cannot bind this run's merge
// to a foreign proposal. The store re-gates closability but not ownership, so a
// restored or corrupted checkpoint naming another run's instance must fail closed
// with ErrParentKeyMismatch before any approval is recorded.
func TestBindClosureMergeRefusesForeignInstance(t *testing.T) {
	ctx := context.Background()
	s, policy, taskID := seedClosureChain(t, "run-own")
	w := &productionPublicationWorkflow{store: s}
	binding := closureStoreBinding(policy, taskID)
	task := productionPublicationTask{
		RunID: policy.RunID, PublicationID: "pub-1", HeadSHA: closureStoreHead,
		Publication: ProductionPublication{Recipe: clientPublicationRecipeV2},
	}

	source, target, ok, err := w.decideClosableSource(ctx, task, binding)
	if err != nil || !ok {
		t.Fatalf("decide closable source: ok=%t err=%v", ok, err)
	}
	key := productionClosureCheckpointKey(task.RunID, task.PublicationID)
	want := productionClosureCheckpoint{
		Version: productionClosureCheckpointVersion, RunID: task.RunID, PublicationID: task.PublicationID,
	}
	decided, admitted, err := w.admitClosureProposal(ctx, key, want, task, source, target, domain.ClosureFlagOriginProposeSite, true, "")
	if err != nil || !admitted {
		t.Fatalf("admit closure proposal: admitted=%t err=%v", admitted, err)
	}

	merge := domain.ProspectiveMerge{
		PublicationIdentity: domain.Digest("sha256:" + strings.Repeat("d", 64)),
		CandidateHeadSHA:    closureStoreHead, BaseRef: closureStoreBaseRef, BaseSHA: closureStoreBaseSHA,
	}
	closure := productionClosureCheckpoint{
		Version: productionClosureCheckpointVersion, RunID: policy.RunID, PublicationID: "pub-1",
		HasProposal: true, InstanceID: decided.InstanceID, Origin: domain.ClosureFlagOriginProposeSite,
	}
	// The instance is owned by "run-own"; a publishing run of "run-foreign" must be
	// refused before any approval is recorded.
	if err := w.bindClosureMerge(ctx, "run-foreign", binding, closure, merge); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("bind foreign instance err = %v, want ErrParentKeyMismatch", err)
	}

	// No policy approval was recorded for the foreign-run bind.
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		approval, err := tx.ClosureApprovalForInstance(ctx, decided.InstanceID)
		if err != nil {
			return err
		}
		if approval != nil {
			t.Error("a policy approval was recorded for a foreign-run bind; want none")
		}
		return nil
	}); err != nil {
		t.Fatalf("read back: %v", err)
	}
}

// TestAuthorSourceIssueSelection proves both author sites use the bound issue
// subject for intake and read client source text only from the target repo.
func TestAuthorSourceIssueSelection(t *testing.T) {
	s, policy, taskID := seedClosureChain(t, "run-author-ref")
	w := &productionPublicationWorkflow{store: s}
	task := productionPublicationTask{
		RunID: policy.RunID, PublicationID: "pub-1", HeadSHA: closureStoreHead,
		Publication: ProductionPublication{Recipe: IntakePublicationRecipe, Title: "Resolve owner/repo#7", Body: "literal"},
	}
	ref, number, sameRepo, err := w.authorSourceIssue(context.Background(), task, closureStoreBinding(policy, taskID))
	if err != nil {
		t.Fatalf("author source issue: %v", err)
	}
	if want := "https://github.com/owner/repo/issues/7"; ref != want || number != 7 || !sameRepo {
		t.Errorf("intake source = (%q, %d, %t), want (%q, 7, true)", ref, number, sameRepo, want)
	}
	// After a rename (#1537) the subject bound under the old name is still the
	// target repository's, and the reference carries the current name.
	renamed := closureStoreBinding(policy, taskID)
	renamed.admission.Base.Repo = "owner/renamed"
	ref, number, sameRepo, err = w.authorSourceIssue(context.Background(), task, renamed)
	if err != nil {
		t.Fatalf("author renamed source issue: %v", err)
	}
	if want := "https://github.com/owner/renamed/issues/7"; ref != want || number != 7 || !sameRepo {
		t.Errorf("renamed intake source = (%q, %d, %t), want (%q, 7, true)", ref, number, sameRepo, want)
	}
	rebound := closureStoreBinding(policy, taskID)
	rebound.admission.Base.RepositoryID++
	ref, number, sameRepo, err = w.authorSourceIssue(context.Background(), task, rebound)
	if err != nil {
		t.Fatalf("author rebound source issue: %v", err)
	}
	if ref != "https://github.com/owner/repo/issues/7" || number != 0 || sameRepo {
		t.Errorf("rebound intake source = (%q, %d, %t), want reference only", ref, number, sameRepo)
	}

	s, policy, taskID = seedClosureChainWithSource(t, "run-client-ref", nil)
	w = &productionPublicationWorkflow{store: s}
	task.RunID = policy.RunID
	task.Publication = ProductionPublication{Recipe: clientPublicationRecipeV2, SourceIssue: "https://github.com/owner/repo/issues/9"}
	binding := closureStoreBinding(policy, taskID)
	ref, number, sameRepo, err = w.authorSourceIssue(context.Background(), task, binding)
	if err != nil {
		t.Fatalf("author client source issue: %v", err)
	}
	if ref != task.Publication.SourceIssue || number != 9 || !sameRepo {
		t.Errorf("same-repo client source = (%q, %d, %t), want (%q, 9, true)", ref, number, sameRepo, task.Publication.SourceIssue)
	}

	task.Publication.SourceIssue = "https://github.com/other/repo/issues/9"
	ref, number, sameRepo, err = w.authorSourceIssue(context.Background(), task, binding)
	if err != nil {
		t.Fatalf("author cross-repo source issue: %v", err)
	}
	if ref != task.Publication.SourceIssue || number != 0 || sameRepo {
		t.Errorf("cross-repo client source = (%q, %d, %t), want (%q, 0, false)", ref, number, sameRepo, task.Publication.SourceIssue)
	}
}
