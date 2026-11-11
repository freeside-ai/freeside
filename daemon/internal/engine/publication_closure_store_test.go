package engine

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
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
// task id so a caller can build a productionBinding. keys join the policy's
// paths key, so a caller can turn the closure human gate on.
func seedClosureChain(t *testing.T, runID domain.RunID, keys ...domain.PolicyKey) (*store.Store, domain.ResolvedPolicy, domain.TaskID) {
	source := &domain.SpecificationSource{
		Kind: domain.SpecificationSourceIssueSubject,
		IssueSubject: &domain.IssueSubjectRef{
			Repo: closureStoreRepo, RepositoryID: closureStoreRepoID, IssueNumber: closureStoreIssue,
		},
	}
	return seedClosureChainWithSource(t, runID, source, keys...)
}

// closureHumanGateKey is the policy key that turns the closure human gate on.
func closureHumanGateKey() domain.PolicyKey {
	return domain.PolicyKey{
		Key: policySourceIssueClosureGate, Value: "true",
		Provenance: domain.KeyProvenance{Source: domain.ProvenanceOverride, Digest: domain.Digest("sha256:" + strings.Repeat("b", 64))},
	}
}

func seedClosureChainWithSource(
	t *testing.T, runID domain.RunID, source *domain.SpecificationSource, keys ...domain.PolicyKey,
) (*store.Store, domain.ResolvedPolicy, domain.TaskID) {
	t.Helper()
	s := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{})
	policy, taskID := seedClosureRows(t, s, runID, source, keys...)
	return s, policy, taskID
}

// seedClosureRows writes seedClosureChainWithSource's rows into a store the
// caller opened, so a test can choose the store's options.
func seedClosureRows(
	t *testing.T, s *store.Store, runID domain.RunID, source *domain.SpecificationSource, keys ...domain.PolicyKey,
) (domain.ResolvedPolicy, domain.TaskID) {
	t.Helper()
	ctx := context.Background()
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	policy, err := domain.NewResolvedPolicy(runID, append([]domain.PolicyKey{{
		Key: "paths", Value: "daemon/",
		Provenance: domain.KeyProvenance{Source: domain.ProvenanceOverride, Digest: domain.Digest("sha256:" + strings.Repeat("a", 64))},
	}}, keys...))
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
	return policy, taskID
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
	t.Parallel()
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
// admits a resolving policy_default proposal without asking the propose site
// (the workflow has no inference client at all), and records a policy approval
// that authorizes closing for the bound merge, with no attention item (#3023).
// It runs for a daemon-bound issue subject (verified) and a same-repository
// client source URL (recommended). A propose_site checkpoint stored before
// #3023 under the default policy still binds its policy approval, so a run in
// flight across the upgrade keeps its close.
func TestReconcileClosureDefaultPolicyRecordsBindingApproval(t *testing.T) {
	t.Parallel()
	for _, origin := range []domain.ClosureFlagOrigin{
		domain.ClosureFlagOriginPolicyDefault, domain.ClosureFlagOriginProposeSite,
	} {
		for _, provenance := range domain.AllClosureProvenances {
			t.Run(string(origin)+"/"+string(provenance), func(t *testing.T) {
				t.Parallel()
				testReconcileClosureDefaultPolicy(t, origin, provenance)
			})
		}
	}
}

func testReconcileClosureDefaultPolicy(t *testing.T, origin domain.ClosureFlagOrigin, provenance domain.ClosureProvenance) {
	ctx := context.Background()
	task := productionPublicationTask{
		PublicationID: "pub-1", HeadSHA: closureStoreHead,
		Publication: ProductionPublication{Recipe: clientPublicationRecipeV2},
	}
	var s *store.Store
	var policy domain.ResolvedPolicy
	var taskID domain.TaskID
	switch provenance {
	case domain.ClosureProvenanceVerified:
		s, policy, taskID = seedClosureChain(t, "run-emit")
	case domain.ClosureProvenanceRecommended:
		s, policy, taskID = seedClosureChainWithSource(t, "run-emit", nil)
		task.Publication.SourceIssue = "https://github.com/" + closureStoreRepo + "/issues/9"
	}
	task.RunID = policy.RunID
	w := &productionPublicationWorkflow{store: s}
	binding := closureStoreBinding(policy, taskID)

	var closure productionClosureCheckpoint
	switch origin {
	case domain.ClosureFlagOriginPolicyDefault:
		decided, err := w.reconcileClosureDecision(
			ctx, task, binding, productionVerificationCheckpoint{}, exec.ReviewInstructionBinding{}, "", nil)
		if err != nil {
			t.Fatalf("reconcile closure decision: %v", err)
		}
		closure = decided
	case domain.ClosureFlagOriginProposeSite:
		source, target, ok, err := w.decideClosableSource(ctx, task, binding)
		if err != nil || !ok {
			t.Fatalf("decide closable source: ok=%t err=%v", ok, err)
		}
		key := productionClosureCheckpointKey(task.RunID, task.PublicationID)
		want := productionClosureCheckpoint{
			Version: productionClosureCheckpointVersion, RunID: task.RunID, PublicationID: task.PublicationID,
		}
		decided, admitted, err := w.admitClosureProposal(ctx, key, want, task, source, target, origin, true, "")
		if err != nil || !admitted {
			t.Fatalf("admit closure proposal: admitted=%t err=%v", admitted, err)
		}
		closure = decided
	case domain.ClosureFlagOriginDaemonFallback:
		t.Fatalf("origin %q records no policy approval", origin)
	}
	if !closure.HasProposal || closure.InstanceID == "" || closure.Origin != origin || closure.Reason != "" {
		t.Fatalf("closure checkpoint = %+v, want an admitted %q proposal with no cause", closure, origin)
	}
	instanceID := closure.InstanceID

	merge := domain.ProspectiveMerge{
		PublicationIdentity: domain.Digest("sha256:" + strings.Repeat("d", 64)),
		CandidateHeadSHA:    closureStoreHead, BaseRef: closureStoreBaseRef, BaseSHA: closureStoreBaseSHA,
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
	if proposal.ClosureProposal == nil || !proposal.ClosureProposal.Resolves ||
		proposal.ClosureProposal.Origin != origin || proposal.ClosureProposal.Provenance != provenance {
		t.Fatalf("stored proposal = %+v, want a resolving %q %q proposal", proposal.ClosureProposal, provenance, origin)
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
		t.Errorf("default-policy %s opened an attention item; want none", origin)
	}
}

// TestBindClosureMergeRefusesForeignInstance proves the reconstruction re-gate: a
// checkpoint instance id that belongs to another run cannot bind this run's merge
// to a foreign proposal. The store re-gates closability but not ownership, so a
// restored or corrupted checkpoint naming another run's instance must fail closed
// with ErrParentKeyMismatch before any approval is recorded.
func TestBindClosureMergeRefusesForeignInstance(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	s, policy, taskID := seedClosureChain(t, "run-author-ref")
	w := &productionPublicationWorkflow{store: s}
	task := productionPublicationTask{
		RunID: policy.RunID, PublicationID: "pub-1", HeadSHA: closureStoreHead,
		Publication: ProductionPublication{Recipe: IntakePublicationRecipeV2, Title: "Resolve owner/repo#7", Body: "literal"},
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
