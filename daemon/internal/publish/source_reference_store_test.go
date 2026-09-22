package publish

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// closureFixture seeds the durable rows a source-issue-closure proposal needs
// (project, issue-subject task, run, resolved policy, work-unit declaration) and
// allocates the proposal instance, then builds the candidate and merge the
// publisher will observe. gateValue selects the closure human-gate policy switch.
type closureFixture struct {
	store      *store.Store
	publisher  *Publisher
	instanceID domain.ProposalInstanceID
	issue      int
	identity   Identity
	merge      domain.ProspectiveMerge
	candidate  Candidate
	at         time.Time
}

func seedClosureFixture(t *testing.T, runID domain.RunID, gateValue string) closureFixture {
	t.Helper()
	ctx := context.Background()
	s := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{})

	const (
		repo    = "owner/repo"
		repoID  = int64(123)
		issue   = 7
		headSHA = "head000000000000000000000000000000000000"
		baseRef = "main"
		baseSHA = "base000000000000000000000000000000000000"
		project = domain.ProjectID("proj-1")
	)
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

	keys := []domain.PolicyKey{{
		Key: "paths", Value: "daemon/",
		Provenance: domain.KeyProvenance{Source: domain.ProvenanceOverride, Digest: testDigest(1)},
	}}
	if gateValue != "" {
		keys = append(keys, domain.PolicyKey{
			Key: policySourceIssueClosureGate, Value: gateValue,
			Provenance: domain.KeyProvenance{Source: domain.ProvenanceOverride, Digest: testDigest(2)},
		})
	}
	policy, err := domain.NewResolvedPolicy(runID, keys)
	if err != nil {
		t.Fatalf("resolved policy: %v", err)
	}
	handle := domain.OpaqueSubjectHandle(domain.WorkUnitIDForRun(runID))
	proposedTarget := domain.IssueSubjectRef{Repo: repo, RepositoryID: repoID, IssueNumber: issue}
	source := domain.SpecificationSource{Kind: domain.SpecificationSourceIssueSubject, IssueSubject: &proposedTarget}
	proposal, err := domain.NewEffectProposal(domain.EffectSourceIssueClosure, domain.SourceIssueClosureInput{
		SubjectHandle: handle,
		Source: domain.ClosableSource{
			Present: true, Provenance: domain.ClosureProvenanceVerified,
			Repo: repo, RepositoryID: repoID, IssueNumber: issue,
		},
		ProposedTarget: proposedTarget,
		Origin:         domain.ClosureFlagOriginProposeSite,
		Resolves:       true,
	}, policy)
	if err != nil {
		t.Fatalf("new effect proposal: %v", err)
	}

	recipe := testDigest(5)

	var instanceID domain.ProposalInstanceID
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.RegisterProject(ctx, domain.Project{ID: project, Repo: repo, RepositoryID: repoID}); err != nil {
			return err
		}
		task, err := tx.GetOrCreateTask(ctx, project, source)
		if err != nil {
			return err
		}
		if err := tx.PutRun(ctx, domain.Run{
			ID: runID, ProjectID: project, TaskID: task.ID,
			SpecDigest: testDigest(9), PolicyDigest: policy.Digest, Stages: []domain.Stage{},
		}); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
			return err
		}
		declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
			CompletionCriterion: domain.CompletionBoundPRMerged,
			DeclaredPaths:       domain.CanonicalDeclaredPaths(policy),
		}, runID, project, at.Add(-time.Hour))
		if err != nil {
			return err
		}
		if err := tx.RecordWorkUnitDeclaration(ctx, declaration); err != nil {
			return err
		}
		instance, _, err := tx.AllocateProposalInstance(ctx,
			domain.ProposalAdmissionKey{Source: domain.ProposalSourceUpstreamEvent, UpstreamEventID: "closure-event-" + string(runID)},
			"batch-closure-"+domain.ProposalBatchID(runID), proposal, at)
		if err != nil {
			return err
		}
		instanceID = instance.ID
		return nil
	}); err != nil {
		t.Fatalf("seed closure fixture: %v", err)
	}

	identity, err := DeriveIdentity(IdentityInput{
		Repo: repo, BaseRef: baseRef, SourceHeadSHA: headSHA, RecipeDigest: &recipe,
		ArtifactDigests: []domain.Digest{testDigest(4)},
	})
	if err != nil {
		t.Fatalf("derive identity: %v", err)
	}
	merge := domain.ProspectiveMerge{
		PublicationIdentity: identity.Digest(), CandidateHeadSHA: headSHA, BaseRef: baseRef, BaseSHA: baseSHA,
	}
	candidate := Candidate{
		Repo: repo, BaseRef: baseRef, BaseSHA: baseSHA, HeadSHA: headSHA, RunID: runID,
		RecipeDigest: &recipe, ClosureInstanceID: instanceID,
	}
	return closureFixture{
		store: s, publisher: &Publisher{storeDecision: &storePublicationDecision{store: s}},
		instanceID: instanceID, issue: issue, identity: identity, merge: merge, candidate: candidate, at: at,
	}
}

// seedForeignRunPolicy seeds a distinct project, task, run, and resolved policy
// for runID in an existing store, so resolveClosure can read that run's policy
// while the closure instance stays owned by a different run.
func seedForeignRunPolicy(t *testing.T, s *store.Store, runID domain.RunID) {
	t.Helper()
	ctx := context.Background()
	policy, err := domain.NewResolvedPolicy(runID, []domain.PolicyKey{{
		Key: "paths", Value: "daemon/",
		Provenance: domain.KeyProvenance{Source: domain.ProvenanceOverride, Digest: testDigest(6)},
	}})
	if err != nil {
		t.Fatalf("foreign policy: %v", err)
	}
	project := domain.ProjectID("proj-" + string(runID))
	source := domain.SpecificationSource{
		Kind:         domain.SpecificationSourceIssueSubject,
		IssueSubject: &domain.IssueSubjectRef{Repo: "other/repo", RepositoryID: 999, IssueNumber: 3},
	}
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.RegisterProject(ctx, domain.Project{ID: project, Repo: "other/repo", RepositoryID: 999}); err != nil {
			return err
		}
		task, err := tx.GetOrCreateTask(ctx, project, source)
		if err != nil {
			return err
		}
		if err := tx.PutRun(ctx, domain.Run{
			ID: runID, ProjectID: project, TaskID: task.ID,
			SpecDigest: testDigest(9), PolicyDigest: policy.Digest, Stages: []domain.Stage{},
		}); err != nil {
			return err
		}
		return tx.PutResolvedPolicy(ctx, policy)
	}); err != nil {
		t.Fatalf("seed foreign run: %v", err)
	}
}

// TestResolveClosureForeignInstanceFailsClosed proves the publisher re-binds the
// candidate's instance id to its run: an instance owned by another work unit
// cannot authorize a close, even with a policy approval recorded against the
// candidate's own merge. This closes the reconstruction/exported-struct gap where
// a spoofed ClosureInstanceID would emit Closes for the other run's issue.
func TestResolveClosureForeignInstanceFailsClosed(t *testing.T) {
	ctx := context.Background()
	f := seedClosureFixture(t, "run-owner", "")
	// Record a policy approval for the owner run's instance against the merge the
	// publisher observes, so only the ownership re-bind stands between the foreign
	// candidate and a wrong Closes.
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		_, err := tx.RecordPolicyClosureApproval(ctx, f.instanceID, f.merge, f.at)
		return err
	}); err != nil {
		t.Fatalf("record policy approval: %v", err)
	}
	seedForeignRunPolicy(t, f.store, "run-foreign")
	foreign := f.candidate
	foreign.RunID = "run-foreign"
	if _, err := f.publisher.resolveClosure(ctx, foreign, f.identity); !errors.Is(err, ErrUnauthorizedPublication) {
		t.Fatalf("resolve foreign instance err = %v, want ErrUnauthorizedPublication", err)
	}
}

// TestResolveClosureDefaultPolicyWritesClosesFromRecordedApproval proves the
// engine/publisher merge-match: a policy approval recorded at publication binds
// the merge, and the publisher, building the same merge from its own identity,
// head, and authorization base SHA, asks the matrix and writes Closes. It also
// pins the default-policy invariant that the pull request is never managed as a
// draft.
func TestResolveClosureDefaultPolicyWritesClosesFromRecordedApproval(t *testing.T) {
	ctx := context.Background()
	f := seedClosureFixture(t, "run-closes", "")
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		_, err := tx.RecordPolicyClosureApproval(ctx, f.instanceID, f.merge, f.at)
		return err
	}); err != nil {
		t.Fatalf("record policy approval: %v", err)
	}
	res, err := f.publisher.resolveClosure(ctx, f.candidate, f.identity)
	if err != nil {
		t.Fatalf("resolve closure: %v", err)
	}
	if res.outcome.Reference != domain.ClosureReferenceCloses {
		t.Fatalf("reference = %q, want closes", res.outcome.Reference)
	}
	if res.target != f.issue {
		t.Errorf("target = %d, want %d", res.target, f.issue)
	}
	if res.managed {
		t.Error("default-policy closure is managed as a draft; want unmanaged")
	}
	section, err := renderSourceReference(res)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if want := "Closes #7"; !strings.Contains(section, want) {
		t.Errorf("section lacks %q:\n%s", want, section)
	}
}

// TestResolveClosureWithoutApprovalWritesRefs proves the publisher never reads a
// close from the candidate: with no recorded approval the matrix yields Refs.
func TestResolveClosureWithoutApprovalWritesRefs(t *testing.T) {
	ctx := context.Background()
	f := seedClosureFixture(t, "run-refs", "")
	res, err := f.publisher.resolveClosure(ctx, f.candidate, f.identity)
	if err != nil {
		t.Fatalf("resolve closure: %v", err)
	}
	if res.outcome.Reference != domain.ClosureReferenceRefs {
		t.Fatalf("reference = %q, want refs", res.outcome.Reference)
	}
}

// TestResolveClosureStaleApprovalWritesRefs proves a base advance voids an
// earlier approval: an approval bound to a different base SHA does not authorize
// a close for the merge the publisher observes now.
func TestResolveClosureStaleApprovalWritesRefs(t *testing.T) {
	ctx := context.Background()
	f := seedClosureFixture(t, "run-stale", "")
	stale := f.merge
	stale.BaseSHA = "advanced0000000000000000000000000000000000"
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		_, err := tx.RecordPolicyClosureApproval(ctx, f.instanceID, stale, f.at)
		return err
	}); err != nil {
		t.Fatalf("record stale approval: %v", err)
	}
	res, err := f.publisher.resolveClosure(ctx, f.candidate, f.identity)
	if err != nil {
		t.Fatalf("resolve closure: %v", err)
	}
	// A base-advance stale approval must not close the issue.
	if res.outcome.Reference == domain.ClosureReferenceCloses {
		t.Fatalf("stale approval closed the issue; want refs")
	}
}

// TestResolveClosureHumanGateHoldsDraftUntilDecided proves the human-gate row of
// the matrix: with the gate on and an open effect_proposal card bound to the
// current merge, the publisher manages the draft state and holds the pull
// request draft, referencing the issue while it is undecided.
func TestResolveClosureHumanGateHoldsDraftUntilDecided(t *testing.T) {
	ctx := context.Background()
	f := seedClosureFixture(t, "run-gate", "true")
	attention := signet.NewService(f.store)
	if _, err := attention.OpenEffectProposalItem(ctx, f.instanceID, f.merge); err != nil {
		t.Fatalf("open effect proposal item: %v", err)
	}
	res, err := f.publisher.resolveClosure(ctx, f.candidate, f.identity)
	if err != nil {
		t.Fatalf("resolve closure: %v", err)
	}
	if !res.managed {
		t.Error("human-gate closure is unmanaged; want managed draft state")
	}
	if res.outcome.Hold != domain.ClosureHoldDraft {
		t.Errorf("hold = %q, want draft", res.outcome.Hold)
	}
	if res.outcome.Reference != domain.ClosureReferenceRefs {
		t.Errorf("reference = %q, want refs while undecided", res.outcome.Reference)
	}
	if got := desiredDraftState(res); got == nil || !*got {
		t.Errorf("desiredDraftState = %v, want *true (held draft)", got)
	}
}
