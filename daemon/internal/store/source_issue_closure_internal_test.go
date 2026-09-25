package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// closureScaffold seeds the durable rows the closure gate re-derives its
// closable-source fact from: a project, a resolved policy, a run bound to a
// task (with the given source), and the work-unit declaration the opaque
// handle resolves to. It returns the policy and the proposal subject handle.
func closureScaffold(
	t *testing.T, ctx context.Context, st *Store, projectID domain.ProjectID, source *domain.SpecificationSource,
) (domain.ResolvedPolicy, domain.OpaqueSubjectHandle) {
	t.Helper()
	policy, err := domain.NewResolvedPolicy(domain.RunID(string(projectID)+"-run"), []domain.PolicyKey{{
		Key: "paths", Value: "daemon/", Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride,
			Digest: domain.Digest("sha256:" + strings.Repeat("a", 64)),
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	handle := domain.OpaqueSubjectHandle(domain.WorkUnitIDForRun(policy.RunID))
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.RegisterProject(ctx, domain.Project{ID: projectID, Repo: "owner/repo", RepositoryID: 123}); err != nil {
			return err
		}
		// A sourced task must register under its source's canonical intake key,
		// which GetOrCreateTask derives; a sourceless task (the recommended case)
		// takes any non-submission key.
		var task domain.Task
		var err error
		if source != nil {
			task, err = tx.GetOrCreateTask(ctx, projectID, *source)
		} else {
			task, err = tx.getOrCreateTask(ctx, projectID, "closure:"+string(projectID), nil,
				time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC))
		}
		if err != nil {
			return err
		}
		if err := tx.PutRun(ctx, domain.Run{
			ID: policy.RunID, ProjectID: projectID, TaskID: task.ID,
			SpecDigest: "sha256:spec", PolicyDigest: policy.Digest, Stages: []domain.Stage{},
		}); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
			return err
		}
		declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
			CompletionCriterion: domain.CompletionBoundPRMerged,
			DeclaredPaths:       domain.CanonicalDeclaredPaths(policy),
		}, policy.RunID, projectID, time.Date(2026, 8, 11, 11, 0, 0, 0, time.UTC))
		if err != nil {
			return err
		}
		return tx.RecordWorkUnitDeclaration(ctx, declaration)
	}); err != nil {
		t.Fatal(err)
	}
	return policy, handle
}

func allocateClosure(
	t *testing.T, ctx context.Context, st *Store, proposal domain.EffectProposal, event string,
) domain.ProposalInstance {
	t.Helper()
	var instance domain.ProposalInstance
	if err := st.Write(ctx, func(tx *WriteTx) error {
		var err error
		instance, _, err = tx.AllocateProposalInstance(ctx,
			domain.ProposalAdmissionKey{Source: domain.ProposalSourceUpstreamEvent, UpstreamEventID: event},
			"batch-closure", proposal, time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return instance
}

// TestClosureVerifiedSourceMustNameProjectRepository fails a verified closure
// closed when the task's issue-subject source names a repository other than the
// project's own. A daemon-bound subject is verified only within the project
// repository (plan §5.13); a cross-repository source row, corrupted or
// mistakenly created, must not yield a verified close to a foreign repository.
func TestClosureVerifiedSourceMustNameProjectRepository(t *testing.T) {
	ctx := context.Background()
	st := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "store.db"), Options{})

	crossRepoSource := domain.SpecificationSource{
		Kind:         domain.SpecificationSourceIssueSubject,
		IssueSubject: &domain.IssueSubjectRef{Repo: "other/repo", RepositoryID: 999, IssueNumber: 42},
	}
	// closureScaffold registers the project as owner/repo (RepositoryID 123),
	// so the seeded task source names a foreign repository.
	policy, handle := closureScaffold(t, ctx, st, "project-cross-repo", &crossRepoSource)

	proposal, err := domain.NewEffectProposal(domain.EffectSourceIssueClosure, domain.SourceIssueClosureInput{
		SubjectHandle: handle,
		Source: domain.ClosableSource{
			Present: true, Provenance: domain.ClosureProvenanceVerified,
			Repo: "other/repo", RepositoryID: 999, IssueNumber: 42,
		},
		Origin: domain.ClosureFlagOriginProposeSite, Resolves: true,
	}, policy)
	if err != nil {
		t.Fatal(err)
	}

	err = st.Write(ctx, func(tx *WriteTx) error {
		_, _, aerr := tx.AllocateProposalInstance(ctx,
			domain.ProposalAdmissionKey{Source: domain.ProposalSourceUpstreamEvent, UpstreamEventID: "event-cross-repo"},
			"batch-closure", proposal, time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC))
		return aerr
	})
	if !errors.Is(err, errRowInconsistent) {
		t.Fatalf("cross-repository verified closure error = %v, want errRowInconsistent", err)
	}
}

func TestClosureProposalStoreRoundTripAndReGate(t *testing.T) {
	ctx := context.Background()
	st := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "store.db"), Options{})

	issueSource := domain.SpecificationSource{
		Kind:         domain.SpecificationSourceIssueSubject,
		IssueSubject: &domain.IssueSubjectRef{Repo: "owner/repo", RepositoryID: 123, IssueNumber: 42},
	}
	verifiedPolicy, verifiedHandle := closureScaffold(t, ctx, st, "project-verified", &issueSource)
	recommendedPolicy, recommendedHandle := closureScaffold(t, ctx, st, "project-recommended", nil)

	verified, err := domain.NewEffectProposal(domain.EffectSourceIssueClosure, domain.SourceIssueClosureInput{
		SubjectHandle: verifiedHandle,
		Source: domain.ClosableSource{
			Present: true, Provenance: domain.ClosureProvenanceVerified, Repo: "owner/repo", RepositoryID: 123, IssueNumber: 42,
		},
		Origin: domain.ClosureFlagOriginProposeSite, Resolves: true,
	}, verifiedPolicy)
	if err != nil {
		t.Fatal(err)
	}
	recommended, err := domain.NewEffectProposal(domain.EffectSourceIssueClosure, domain.SourceIssueClosureInput{
		SubjectHandle: recommendedHandle,
		Source: domain.ClosableSource{
			Present: true, Provenance: domain.ClosureProvenanceRecommended, Repo: "owner/repo", RepositoryID: 123,
		},
		ProposedTarget: domain.IssueSubjectRef{Repo: "owner/repo", RepositoryID: 123, IssueNumber: 9},
		Origin:         domain.ClosureFlagOriginProposeSite, Resolves: true,
	}, recommendedPolicy)
	if err != nil {
		t.Fatal(err)
	}
	fallback, err := domain.NewEffectProposal(domain.EffectSourceIssueClosure, domain.SourceIssueClosureInput{
		SubjectHandle: verifiedHandle,
		Source: domain.ClosableSource{
			Present: true, Provenance: domain.ClosureProvenanceVerified, Repo: "owner/repo", RepositoryID: 123, IssueNumber: 42,
		},
		Origin: domain.ClosureFlagOriginDaemonFallback, Resolves: true,
	}, verifiedPolicy)
	if err != nil {
		t.Fatal(err)
	}

	verifiedInstance := allocateClosure(t, ctx, st, verified, "event-verified")
	recommendedInstance := allocateClosure(t, ctx, st, recommended, "event-recommended")
	fallbackInstance := allocateClosure(t, ctx, st, fallback, "event-fallback")

	// Every kind round-trips through the kind-aware reconstruction and re-gate.
	for _, tc := range []struct {
		name       string
		instance   domain.ProposalInstance
		provenance domain.ClosureProvenance
		resolves   bool
	}{
		{"verified", verifiedInstance, domain.ClosureProvenanceVerified, true},
		{"recommended", recommendedInstance, domain.ClosureProvenanceRecommended, true},
		{"fallback", fallbackInstance, domain.ClosureProvenanceVerified, false},
	} {
		t.Run("round trip "+tc.name, func(t *testing.T) {
			var got domain.ProposalInstance
			if err := st.Read(ctx, func(tx *ReadTx) error {
				var err error
				got, err = tx.GetProposalInstance(ctx, tc.instance.ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if got.Proposal.Kind != domain.EffectSourceIssueClosure || got.Proposal.ClosureProposal == nil {
				t.Fatalf("reconstructed = %#v", got.Proposal)
			}
			if got.Proposal.ClosureProposal.Provenance != tc.provenance || got.Proposal.ClosureProposal.Resolves != tc.resolves {
				t.Fatalf("closure params = %#v", got.Proposal.ClosureProposal)
			}
		})
	}

	// Fail-closed: a self-consistent stored proposal (valid digest) whose target
	// or provenance current state does not grant is rejected on reconstruction.
	// Build alternatives against the same policy and handle, then swap the row's
	// content_digest and body together, mirroring a coordinated tamper.
	verifiedWrongIssue, err := domain.NewEffectProposal(domain.EffectSourceIssueClosure, domain.SourceIssueClosureInput{
		SubjectHandle: verifiedHandle,
		Source: domain.ClosableSource{
			Present: true, Provenance: domain.ClosureProvenanceVerified, Repo: "owner/repo", RepositoryID: 123, IssueNumber: 99,
		},
		Origin: domain.ClosureFlagOriginProposeSite, Resolves: true,
	}, verifiedPolicy)
	if err != nil {
		t.Fatal(err)
	}
	recommendedOnVerified, err := domain.NewEffectProposal(domain.EffectSourceIssueClosure, domain.SourceIssueClosureInput{
		SubjectHandle: verifiedHandle,
		Source: domain.ClosableSource{
			Present: true, Provenance: domain.ClosureProvenanceRecommended, Repo: "owner/repo", RepositoryID: 123,
		},
		ProposedTarget: domain.IssueSubjectRef{Repo: "owner/repo", RepositoryID: 123, IssueNumber: 42},
		Origin:         domain.ClosureFlagOriginProposeSite, Resolves: true,
	}, verifiedPolicy)
	if err != nil {
		t.Fatal(err)
	}
	// A stored daemon_fallback whose resolves flag was flipped true is caught by
	// the structural invariant even though its content address is self-consistent.
	forgedFallback := domain.EffectProposal{
		EncodingVersion: 1, Kind: domain.EffectSourceIssueClosure,
		ResolvedPolicyRunID: verifiedPolicy.RunID, ResolvedPolicyDigest: verifiedPolicy.Digest,
		ClosureProposal: &domain.SourceIssueClosureParameters{
			SubjectHandle: verifiedHandle,
			Target:        domain.IssueSubjectRef{Repo: "owner/repo", RepositoryID: 123, IssueNumber: 42},
			Resolves:      true, Provenance: domain.ClosureProvenanceVerified, Origin: domain.ClosureFlagOriginDaemonFallback,
		},
	}
	forgedFallback.Digest, err = forgedFallback.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		alt  domain.EffectProposal
		want error
	}{
		{"verified target mismatch", verifiedWrongIssue, domain.ErrClosureTargetMismatch},
		{"provenance mismatch", recommendedOnVerified, domain.ErrClosureProvenanceMismatch},
		{"forged fallback resolves", forgedFallback, domain.ErrEffectProposalInconsistent},
	} {
		t.Run("fail closed "+tc.name, func(t *testing.T) {
			tampered := verifiedInstance
			tampered.Proposal = tc.alt
			body, err := marshalProposalInstance(tampered)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.db.ExecContext(ctx, `UPDATE effect_proposal_instances SET
				content_digest = ?, body = ? WHERE instance_id = ?`, tc.alt.Digest, string(body), verifiedInstance.ID); err != nil {
				t.Fatal(err)
			}
			err = st.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.GetProposalInstance(ctx, verifiedInstance.ID)
				return err
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("reconstruction error = %v, want %v", err, tc.want)
			}
			// Restore the canonical row for the next case.
			restore, err := marshalProposalInstance(verifiedInstance)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.db.ExecContext(ctx, `UPDATE effect_proposal_instances SET
				content_digest = ?, body = ? WHERE instance_id = ?`, verifiedInstance.Proposal.Digest, string(restore), verifiedInstance.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestClosureFollowsRepositoryRename pins the #1537 rule on the closure path:
// the repository id decides, so after a rename (same id, new name) a task
// bound under the old name still earns a verified closure, proposals admitted
// under the new name pass the store re-gate, and proposals admitted before the
// rename still reconstruct.
func TestClosureFollowsRepositoryRename(t *testing.T) {
	ctx := context.Background()
	st := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "store.db"), Options{})

	oldNameSource := domain.SpecificationSource{
		Kind:         domain.SpecificationSourceIssueSubject,
		IssueSubject: &domain.IssueSubjectRef{Repo: "owner/repo", RepositoryID: 123, IssueNumber: 42},
	}
	verifiedPolicy, verifiedHandle := closureScaffold(t, ctx, st, "project-verified", &oldNameSource)
	recommendedPolicy, recommendedHandle := closureScaffold(t, ctx, st, "project-recommended", nil)

	proposals := func(repo string) (verified, recommended domain.EffectProposal) {
		t.Helper()
		verified, err := domain.NewEffectProposal(domain.EffectSourceIssueClosure, domain.SourceIssueClosureInput{
			SubjectHandle: verifiedHandle,
			Source: domain.ClosableSource{
				Present: true, Provenance: domain.ClosureProvenanceVerified, Repo: repo, RepositoryID: 123, IssueNumber: 42,
			},
			Origin: domain.ClosureFlagOriginProposeSite, Resolves: true,
		}, verifiedPolicy)
		if err != nil {
			t.Fatal(err)
		}
		recommended, err = domain.NewEffectProposal(domain.EffectSourceIssueClosure, domain.SourceIssueClosureInput{
			SubjectHandle: recommendedHandle,
			Source: domain.ClosableSource{
				Present: true, Provenance: domain.ClosureProvenanceRecommended, Repo: repo, RepositoryID: 123,
			},
			ProposedTarget: domain.IssueSubjectRef{Repo: repo, RepositoryID: 123, IssueNumber: 9},
			Origin:         domain.ClosureFlagOriginProposeSite, Resolves: true,
		}, recommendedPolicy)
		if err != nil {
			t.Fatal(err)
		}
		return verified, recommended
	}

	oldVerified, oldRecommended := proposals("owner/repo")
	instances := []domain.ProposalInstance{
		allocateClosure(t, ctx, st, oldVerified, "event-old-verified"),
		allocateClosure(t, ctx, st, oldRecommended, "event-old-recommended"),
	}

	if err := st.Write(ctx, func(tx *WriteTx) error {
		for _, id := range []domain.ProjectID{"project-verified", "project-recommended"} {
			if err := tx.RegisterProject(ctx, domain.Project{ID: id, Repo: "owner/renamed", RepositoryID: 123}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("rename projects: %v", err)
	}

	var closable domain.ClosableSource
	if err := st.Read(ctx, func(tx *ReadTx) error {
		declaration, err := tx.GetWorkUnitDeclarationByRun(ctx, verifiedPolicy.RunID)
		if err != nil {
			return err
		}
		closable, err = tx.closableSource(ctx, declaration)
		return err
	}); err != nil {
		t.Fatalf("closable source after rename: %v", err)
	}
	want := domain.ClosableSource{
		Present: true, Provenance: domain.ClosureProvenanceVerified, Repo: "owner/renamed", RepositoryID: 123, IssueNumber: 42,
	}
	if closable != want {
		t.Fatalf("closable source after rename = %+v, want %+v", closable, want)
	}

	newVerified, newRecommended := proposals("owner/renamed")
	instances = append(instances,
		allocateClosure(t, ctx, st, newVerified, "event-new-verified"),
		allocateClosure(t, ctx, st, newRecommended, "event-new-recommended"),
	)
	for _, instance := range instances {
		if err := st.Read(ctx, func(tx *ReadTx) error {
			_, err := tx.GetProposalInstance(ctx, instance.ID)
			return err
		}); err != nil {
			t.Fatalf("reconstruct %s after rename: %v", instance.ID, err)
		}
	}
}
