package engine

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/elaborate"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// issueSubjectReservation mirrors what the label-intake admission persists
// before a start: the reserved elaboration run (bare, no markers), its resolved
// policy, the policy artifact, and the daemon-authored coordinates-only
// work-item Specification artifact whose digest is the run's SpecDigest. The
// issue-subject arm of SubmitElaborationRun adopts exactly this state.
type issueSubjectReservation struct {
	store            *store.Store
	spec             ElaborationRunSpec
	elaborationRunID domain.RunID
	workItemArtifact domain.ArtifactID
}

func newIssueSubjectReservation(t *testing.T) issueSubjectReservation {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(root, "state.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	blobs, err := signet.NewBlobStore(filepath.Join(root, "blobs"))
	if err != nil {
		t.Fatal(err)
	}

	const implementationRunID = domain.RunID("run-label-intake-1")
	elaborationRunID, err := ElaborationRunIDForImplementation(implementationRunID)
	if err != nil {
		t.Fatal(err)
	}
	provenance := domain.KeyProvenance{
		Source: domain.ProvenanceOverride,
		Digest: domain.Digest(contentaddr.Sum([]byte("issue-subject-test-policy"))),
	}
	policy, err := domain.NewResolvedPolicy(elaborationRunID, []domain.PolicyKey{
		{Key: elaborate.PolicySpecApproval, Value: "true", Provenance: provenance},
		{Key: elaborate.PolicyMaxIterations, Value: "4", Provenance: provenance},
		{Key: elaborate.PolicyStageActiveTime, Value: "1m", Provenance: provenance},
		{Key: elaborate.PolicyApprovalWait, Value: "1m", Provenance: provenance},
		{Key: elaborate.PolicyResearchAllowlist, Value: "https://api.github.com", Provenance: provenance},
		{Key: elaborate.PolicyResearchMaxBytes, Value: "1024", Provenance: provenance},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The work-item document is daemon-authored coordinates only — no issue
	// content — delivered in the specification role from run.SpecDigest.
	workItemBody := []byte("# Label-intake work item\nrepository_id: 42\nissue: 7\nlabel: freeside\n")
	workItemDigest := domain.Digest(contentaddr.Sum(workItemBody))
	workItem := testElaborationArtifact(t, "work-item-label-intake-1", domain.ArtifactKindSpecification,
		workItemDigest, domain.ProducerDaemon, "intake-work-item")
	policyBody, err := json.Marshal(policy.Keys)
	if err != nil {
		t.Fatal(err)
	}
	policyArt := testElaborationArtifact(t, "policy-label-intake-1", domain.ArtifactKindPolicy,
		policy.Digest, domain.ProducerDaemon, "intake-policy")
	if _, err := blobs.Put(workItemDigest, strings.NewReader(string(workItemBody))); err != nil {
		t.Fatal(err)
	}
	if _, err := blobs.Put(policyArt.Digest, strings.NewReader(string(policyBody))); err != nil {
		t.Fatal(err)
	}
	reservedRun := domain.Run{
		ID: elaborationRunID, ProjectID: "project-1",
		SpecDigest: workItemDigest, PolicyDigest: policy.Digest,
		Stages: []domain.Stage{{
			ID: elaborationStageID(elaborationRunID), RunID: elaborationRunID,
			Name: elaborationStageName, Attempts: []domain.Attempt{},
		}},
	}
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		if err := tx.PutArtifact(t.Context(), workItem); err != nil {
			return err
		}
		if err := tx.PutArtifact(t.Context(), policyArt); err != nil {
			return err
		}
		if err := tx.PutRun(t.Context(), reservedRun); err != nil {
			return err
		}
		return tx.PutResolvedPolicy(t.Context(), policy)
	}); err != nil {
		t.Fatal(err)
	}
	spec := ElaborationRunSpec{
		ElaborationRunID: elaborationRunID, ImplementationRunID: implementationRunID,
		ProjectID: "project-1", SourceArtifactID: workItem.ID,
		PolicyArtifactID: policyArt.ID, ResolvedPolicy: policy,
		Publication: ProductionPublication{
			Title: "Resolve labeled issue #7", Body: "Daemon-composed publication.",
			CommitAuthor: ProductionCommitAuthor{AppSlug: "freeside-bot", BotUserID: 12345},
		},
		Source: domain.ElaborationSource{
			Kind: domain.ElaborationSourceIssueSubject,
			IssueSubject: &domain.IssueSubjectRef{
				Repo: "freeasinbird/freeside", RepositoryID: 42, IssueNumber: 7,
			},
		},
	}
	return issueSubjectReservation{
		store: st, spec: spec, elaborationRunID: elaborationRunID, workItemArtifact: workItem.ID,
	}
}

// TestSubmitIssueSubjectElaborationRunAdoptsReservedRun proves the issue-subject
// arm adopts the reserved run the admission persisted (rather than creating a
// new one), materializing exactly the iteration-1 invocation, dispatch marker,
// dispatched implementation claim, and run-submitted milestone that make the
// elaboration reconciler own it.
func TestSubmitIssueSubjectElaborationRunAdoptsReservedRun(t *testing.T) {
	t.Parallel()
	r := newIssueSubjectReservation(t)

	submitted, err := SubmitElaborationRun(t.Context(), r.store, r.spec)
	if err != nil {
		t.Fatalf("submit issue-subject run: %v", err)
	}
	if submitted.Run.ID != r.elaborationRunID {
		t.Fatalf("adopted run = %q, want %q", submitted.Run.ID, r.elaborationRunID)
	}
	if submitted.ImplementationRunID != r.spec.ImplementationRunID {
		t.Fatalf("implementation run = %q, want %q", submitted.ImplementationRunID, r.spec.ImplementationRunID)
	}
	invocationID := elaborationInvocationID(r.elaborationRunID, 1)

	if err := r.store.Read(t.Context(), func(tx *store.ReadTx) error {
		marker, err := tx.GetOutbox(t.Context(), string(invocationID))
		if err != nil {
			return err
		}
		if marker.Kind != KindElaborationInvocationRequested {
			t.Errorf("marker kind = %q, want %q", marker.Kind, KindElaborationInvocationRequested)
		}
		request, err := decodeElaborationRequest(marker)
		if err != nil {
			return err
		}
		if request.IssueSubject == nil || *request.IssueSubject != *r.spec.Source.IssueSubject {
			t.Errorf("marker issue subject = %+v, want %+v", request.IssueSubject, r.spec.Source.IssueSubject)
		}
		if len(request.InputArtifactIDs) != 1 || request.InputArtifactIDs[0] != r.workItemArtifact {
			t.Errorf("marker inputs = %v, want [%q]", request.InputArtifactIDs, r.workItemArtifact)
		}
		claim, err := tx.GetOutbox(t.Context(), elaborationImplementationClaimKey(r.spec.ImplementationRunID))
		if err != nil {
			return err
		}
		if claim.Kind != KindElaborationImplementationClaim || !claim.Dispatched() {
			t.Errorf("claim kind=%q dispatched=%v, want dispatched %q",
				claim.Kind, claim.Dispatched(), KindElaborationImplementationClaim)
		}
		if _, err := tx.GetAgentInvocation(t.Context(), invocationID); err != nil {
			return err
		}
		milestones, err := tx.ListRunMilestones(t.Context(), r.elaborationRunID)
		if err != nil {
			return err
		}
		submitted := false
		for _, m := range milestones {
			if m.Kind == domain.MilestoneRunSubmitted {
				submitted = true
			}
		}
		if !submitted {
			t.Errorf("adopted run carries no run-submitted milestone")
		}
		return nil
	}); err != nil {
		t.Fatalf("inspect adopted run: %v", err)
	}
}

// TestSubmitIssueSubjectElaborationRunConvergesOnReplay proves a crash-recovery
// replay of the same start converges on the adopted run and creates no second
// invocation, rather than conflicting on the write-once marker.
func TestSubmitIssueSubjectElaborationRunConvergesOnReplay(t *testing.T) {
	t.Parallel()
	r := newIssueSubjectReservation(t)

	first, err := SubmitElaborationRun(t.Context(), r.store, r.spec)
	if err != nil {
		t.Fatalf("first submit: %v", err)
	}
	second, err := SubmitElaborationRun(t.Context(), r.store, r.spec)
	if err != nil {
		t.Fatalf("replayed submit: %v", err)
	}
	if first.Run.ID != second.Run.ID || first.ElaborationInvocationID != second.ElaborationInvocationID {
		t.Fatalf("replay diverged: first=%+v second=%+v", first, second)
	}
}

// TestSubmitIssueSubjectElaborationRunRequiresReservedRun proves the arm adopts
// rather than creates: with no reserved run persisted, the start fails closed
// instead of fabricating one.
func TestSubmitIssueSubjectElaborationRunRequiresReservedRun(t *testing.T) {
	t.Parallel()
	r := newIssueSubjectReservation(t)
	// Point the submission at a run the admission never reserved.
	other, err := ElaborationRunIDForImplementation("run-unreserved")
	if err != nil {
		t.Fatal(err)
	}
	spec := r.spec
	spec.ElaborationRunID = other
	spec.ImplementationRunID = "run-unreserved"
	policy, err := domain.NewResolvedPolicy(other, r.spec.ResolvedPolicy.Keys)
	if err != nil {
		t.Fatal(err)
	}
	spec.ResolvedPolicy = policy

	if _, err := SubmitElaborationRun(t.Context(), r.store, spec); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("submit without a reserved run: err = %v, want ErrNotFound", err)
	}
}

// TestSubmitIssueSubjectElaborationRunRejectsForeignWorkItem proves the arm
// binds the work-item artifact to the reserved run's own SpecDigest: a
// specification artifact whose digest is not the run's is refused.
func TestSubmitIssueSubjectElaborationRunRejectsForeignWorkItem(t *testing.T) {
	t.Parallel()
	r := newIssueSubjectReservation(t)
	foreignBody := []byte("a different specification the reserved run never named")
	foreignDigest := domain.Digest(contentaddr.Sum(foreignBody))
	foreign := testElaborationArtifact(t, "foreign-spec", domain.ArtifactKindSpecification,
		foreignDigest, domain.ProducerDaemon, "foreign")
	if err := r.store.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.PutArtifact(t.Context(), foreign)
	}); err != nil {
		t.Fatal(err)
	}
	spec := r.spec
	spec.SourceArtifactID = foreign.ID

	if _, err := SubmitElaborationRun(t.Context(), r.store, spec); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("submit with a foreign work item: err = %v, want ErrParentKeyMismatch", err)
	}
}

// TestElaborationRequestPinsIssueSubject proves the canonical encode/decode
// round-trips the issue-subject reference and that authenticateElaborationRoot
// rejects a later-iteration request whose subject was swapped: a retargeted
// request cannot adopt a foreign occurrence's issue.
func TestElaborationRequestPinsIssueSubject(t *testing.T) {
	t.Parallel()
	subject := &domain.IssueSubjectRef{Repo: "owner/repo", RepositoryID: 9, IssueNumber: 3}
	base := elaborationRequest{
		Version: elaborationRequestVersion, ElaborationRunID: "run-elaboration-x",
		ImplementationRunID: "run-x", ProjectID: "project-1",
		InvocationID:     elaborationInvocationID("run-elaboration-x", 1),
		Iteration:        1,
		InputArtifactIDs: []domain.ArtifactID{"work-item-x"},
		PolicyArtifactID: "policy-x",
		Publication: ProductionPublication{
			Title: "t", Body: "b",
			CommitAuthor: ProductionCommitAuthor{AppSlug: "freeside-bot", BotUserID: 1},
		},
		IssueSubject: subject,
	}
	payload, err := encodeElaborationRequest(base)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := decodeElaborationPayload(payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.IssueSubject == nil || *decoded.IssueSubject != *subject {
		t.Fatalf("decoded subject = %+v, want %+v", decoded.IssueSubject, subject)
	}
	// A request whose subject was swapped is not the same arm as the root.
	swapped := base
	swapped.IssueSubject = &domain.IssueSubjectRef{Repo: "owner/repo", RepositoryID: 9, IssueNumber: 99}
	if sameIssueSubject(base.IssueSubject, swapped.IssueSubject) {
		t.Fatal("sameIssueSubject accepted a swapped issue number")
	}
	if !sameIssueSubject(nil, nil) {
		t.Fatal("sameIssueSubject rejected the spec-artifact arm (both nil)")
	}
	if sameIssueSubject(subject, nil) {
		t.Fatal("sameIssueSubject accepted a present/absent mismatch")
	}
}
