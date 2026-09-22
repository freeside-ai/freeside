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

// closurePolicyFixture is the closable-source graph a policy approval binds to:
// a store at head, one source_issue_closure instance, and a valid prospective
// merge. No attention item is created; a policy approval needs none.
type closurePolicyFixture struct {
	st       *Store
	instance domain.ProposalInstance
	proposal domain.EffectProposal
	merge    domain.ProspectiveMerge
	at       time.Time
}

const (
	closurePolicyRepo   = "owner/repo"
	closurePolicyRepoID = int64(123)
	closurePolicyIssue  = 7
)

func seedClosurePolicyFixture(
	t *testing.T, ctx context.Context, origin domain.ClosureFlagOrigin, resolves bool,
) closurePolicyFixture {
	t.Helper()
	st := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "store.db"), Options{})

	policy, err := domain.NewResolvedPolicy("closure-policy-run", []domain.PolicyKey{{
		Key: "paths", Value: "daemon/", Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride, Digest: domain.Digest("sha256:" + strings.Repeat("a", 64)),
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	handle := domain.OpaqueSubjectHandle(domain.WorkUnitIDForRun(policy.RunID))
	target := domain.IssueSubjectRef{Repo: closurePolicyRepo, RepositoryID: closurePolicyRepoID, IssueNumber: closurePolicyIssue}
	source := domain.SpecificationSource{Kind: domain.SpecificationSourceIssueSubject, IssueSubject: &target}
	proposal, err := domain.NewEffectProposal(domain.EffectSourceIssueClosure, domain.SourceIssueClosureInput{
		SubjectHandle:  handle,
		Source:         domain.ClosableSource{Present: true, Provenance: domain.ClosureProvenanceVerified, Repo: closurePolicyRepo, RepositoryID: closurePolicyRepoID, IssueNumber: closurePolicyIssue},
		ProposedTarget: target,
		Origin:         origin,
		Resolves:       resolves,
	}, policy)
	if err != nil {
		t.Fatal(err)
	}

	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	var instance domain.ProposalInstance
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.RegisterProject(ctx, domain.Project{ID: "proj-1", Repo: closurePolicyRepo, RepositoryID: closurePolicyRepoID}); err != nil {
			return err
		}
		task, err := tx.GetOrCreateTask(ctx, "proj-1", source)
		if err != nil {
			return err
		}
		if err := tx.PutRun(ctx, domain.Run{
			ID: policy.RunID, ProjectID: "proj-1", TaskID: task.ID,
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
		}, policy.RunID, "proj-1", at.Add(-time.Hour))
		if err != nil {
			return err
		}
		if err := tx.RecordWorkUnitDeclaration(ctx, declaration); err != nil {
			return err
		}
		instance, _, err = tx.AllocateProposalInstance(ctx,
			domain.ProposalAdmissionKey{Source: domain.ProposalSourceUpstreamEvent, UpstreamEventID: "closure-event"},
			"batch-closure", proposal, at)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	merge := domain.ProspectiveMerge{
		PublicationIdentity: domain.Digest("sha256:" + strings.Repeat("b", 64)),
		CandidateHeadSHA:    "head-aaa",
		BaseRef:             "main",
		BaseSHA:             "base-000",
	}
	return closurePolicyFixture{st: st, instance: instance, proposal: proposal, merge: merge, at: at}
}

// recordPolicyApproval runs RecordPolicyClosureApproval in a write transaction.
func (f closurePolicyFixture) recordPolicyApproval(t *testing.T, ctx context.Context, merge domain.ProspectiveMerge) (domain.ClosureApproval, error) {
	t.Helper()
	var approval domain.ClosureApproval
	err := f.st.Write(ctx, func(tx *WriteTx) error {
		var writeErr error
		approval, writeErr = tx.RecordPolicyClosureApproval(ctx, f.instance.ID, merge, f.at)
		return writeErr
	})
	return approval, err
}

// seedDeclineCommand binds an effect_proposal item to the instance and stores a
// decline command against it, without recording the decision. The caller then
// records the decline itself, so a test can observe the second-actor guard on
// the RecordProposalDecision call.
func (f closurePolicyFixture) seedDeclineCommand(t *testing.T, ctx context.Context) string {
	t.Helper()
	if err := f.st.Write(ctx, func(tx *WriteTx) error {
		artifact, err := f.instance.EvidenceArtifact()
		if err != nil {
			return err
		}
		if err := tx.PutArtifact(ctx, artifact); err != nil {
			return err
		}
		item, err := domain.NewAttentionItem(domain.AttentionItemInput{
			ID:        domain.ItemID(string(f.instance.ID) + "/effect"),
			ProjectID: "proj-1",
			Subject:   domain.Subject{Type: domain.SubjectProposalBatch, ID: domain.SubjectID(f.instance.ProposalBatchID)},
			Type:      domain.AttentionEffectProposal, Priority: domain.PriorityNormal,
			Reason:            "Decide the proposed effect on the source issue",
			RequestedDecision: []domain.Action{domain.ActionApprove, domain.ActionDecline, domain.ActionSnooze},
			EvidenceSnapshot:  []domain.Artifact{artifact}, ItemVersion: 1,
			InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
			PRHeadSHA: f.merge.CandidateHeadSHA,
		}, map[domain.Digest]bool{domain.EffectProposalRecipeDigest: true})
		if err != nil {
			return err
		}
		if err := tx.PutAttentionItem(ctx, item); err != nil {
			return err
		}
		if err := tx.BindProposalItem(ctx, item.ID, f.instance.ID, f.proposal.Digest, &f.merge); err != nil {
			return err
		}
		command, err := domain.NewCommand(domain.CommandInput{
			CommandID: "command-decline", DeviceID: "device-1", ItemID: item.ID,
			ItemVersion: item.ItemVersion, PRHeadSHA: item.PRHeadSHA,
			ArtifactDigests: item.ArtifactDigests, Action: domain.ActionDecline,
		})
		if err != nil {
			return err
		}
		return tx.PutCommand(ctx, command)
	}); err != nil {
		t.Fatal(err)
	}
	return "command-decline"
}

func (f closurePolicyFixture) reconstruct(t *testing.T, ctx context.Context) (*domain.ClosureApproval, error) {
	t.Helper()
	var approval *domain.ClosureApproval
	err := f.st.Read(ctx, func(tx *ReadTx) error {
		var readErr error
		approval, readErr = tx.ClosureApprovalForInstance(ctx, f.instance.ID)
		return readErr
	})
	return approval, err
}

// TestRecordPolicyClosureApprovalReconstructs proves the policy write records a
// binding that reconstructs with the policy actor and the instance's current
// proposal digest, and authorizes the close for the merge it was recorded
// against.
func TestRecordPolicyClosureApprovalReconstructs(t *testing.T) {
	ctx := context.Background()
	f := seedClosurePolicyFixture(t, ctx, domain.ClosureFlagOriginProposeSite, true)

	recorded, err := f.recordPolicyApproval(t, ctx, f.merge)
	if err != nil {
		t.Fatalf("RecordPolicyClosureApproval: %v", err)
	}
	if recorded.Actor != domain.ClosureApprovalActorPolicy || recorded.ProposalDigest != f.instance.Proposal.Digest {
		t.Fatalf("recorded approval = %+v, want policy actor and current digest", recorded)
	}

	approval, err := f.reconstruct(t, ctx)
	if err != nil {
		t.Fatalf("reconstruct: %v", err)
	}
	if approval == nil {
		t.Fatal("policy approval reconstructed as nil")
	}
	if approval.Actor != domain.ClosureApprovalActorPolicy ||
		approval.ProposalDigest != f.instance.Proposal.Digest ||
		approval.PublicationIdentity != f.merge.PublicationIdentity ||
		approval.CandidateHeadSHA != f.merge.CandidateHeadSHA ||
		approval.BaseRef != f.merge.BaseRef || approval.BaseSHA != f.merge.BaseSHA {
		t.Fatalf("reconstructed approval = %+v, want the recorded merge with policy actor", approval)
	}
	if !approval.AuthorizesClose(f.instance.Proposal, f.merge) {
		t.Fatal("policy approval did not authorize the merge it was recorded against")
	}
}

// TestRecordPolicyClosureApprovalReplacesMovedMerge proves a second write for a
// moved merge upserts one row: the reconstruction binds the new merge, and
// neither binding authorizes the old merge.
func TestRecordPolicyClosureApprovalReplacesMovedMerge(t *testing.T) {
	ctx := context.Background()
	f := seedClosurePolicyFixture(t, ctx, domain.ClosureFlagOriginProposeSite, true)

	if _, err := f.recordPolicyApproval(t, ctx, f.merge); err != nil {
		t.Fatalf("first policy approval: %v", err)
	}
	moved := f.merge
	moved.CandidateHeadSHA = "head-bbb"
	moved.BaseSHA = "base-999"
	if _, err := f.recordPolicyApproval(t, ctx, moved); err != nil {
		t.Fatalf("moved policy approval: %v", err)
	}

	approval, err := f.reconstruct(t, ctx)
	if err != nil {
		t.Fatalf("reconstruct: %v", err)
	}
	if approval == nil || approval.CandidateHeadSHA != "head-bbb" || approval.BaseSHA != "base-999" {
		t.Fatalf("reconstructed approval = %+v, want the moved merge", approval)
	}
	if !approval.AuthorizesClose(f.instance.Proposal, moved) {
		t.Fatal("moved policy approval did not authorize the moved merge")
	}
	if approval.AuthorizesClose(f.instance.Proposal, f.merge) {
		t.Fatal("moved policy approval still authorizes the superseded merge")
	}
}

// TestRecordPolicyClosureApprovalRefusesFallback proves a daemon_fallback
// closure is refused: that path gets the notice item, never an approval.
func TestRecordPolicyClosureApprovalRefusesFallback(t *testing.T) {
	ctx := context.Background()
	f := seedClosurePolicyFixture(t, ctx, domain.ClosureFlagOriginDaemonFallback, false)

	if _, err := f.recordPolicyApproval(t, ctx, f.merge); !errors.Is(err, domain.ErrEffectProposalInconsistent) {
		t.Fatalf("fallback policy approval error = %v, want ErrEffectProposalInconsistent", err)
	}
}

// TestRecordPolicyClosureApprovalReGatesUnclosableSource proves the write and the
// read both fail closed when the closable source can no longer be confirmed: the
// project's lookup column is tampered so the re-gate refuses it.
func TestRecordPolicyClosureApprovalReGatesUnclosableSource(t *testing.T) {
	ctx := context.Background()

	// Write path: an unconfirmable source admits no approval.
	fw := seedClosurePolicyFixture(t, ctx, domain.ClosureFlagOriginProposeSite, true)
	if _, err := fw.st.db.ExecContext(ctx,
		`UPDATE projects SET repository_id = repository_id + 1 WHERE project_id = 'proj-1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := fw.recordPolicyApproval(t, ctx, fw.merge); !errors.Is(err, errRowInconsistent) {
		t.Fatalf("write on unclosable source error = %v, want row inconsistency", err)
	}

	// Read path: a source that becomes unconfirmable after the approval was
	// recorded fails closed on reconstruction too.
	fr := seedClosurePolicyFixture(t, ctx, domain.ClosureFlagOriginProposeSite, true)
	if _, err := fr.recordPolicyApproval(t, ctx, fr.merge); err != nil {
		t.Fatalf("record before tamper: %v", err)
	}
	if _, err := fr.st.db.ExecContext(ctx,
		`UPDATE projects SET repository_id = repository_id + 1 WHERE project_id = 'proj-1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := fr.reconstruct(t, ctx); !errors.Is(err, errRowInconsistent) {
		t.Fatalf("read on unclosable source error = %v, want row inconsistency", err)
	}
}

// TestClosureApprovalActorConflict proves the two recorders never coexist: each
// write refuses once the other actor recorded, and a reconstruction with both
// rows present fails closed.
func TestClosureApprovalActorConflict(t *testing.T) {
	ctx := context.Background()

	t.Run("policy write refuses after a human decision", func(t *testing.T) {
		f := seedClosurePolicyFixture(t, ctx, domain.ClosureFlagOriginProposeSite, true)
		commandID := f.seedDeclineCommand(t, ctx)
		if err := f.st.Write(ctx, func(tx *WriteTx) error {
			return tx.RecordProposalDecision(ctx, f.instance.ID, commandID, domain.ActionDecline, nil, f.at)
		}); err != nil {
			t.Fatalf("record decline: %v", err)
		}
		if _, err := f.recordPolicyApproval(t, ctx, f.merge); !errors.Is(err, ErrClosureApprovalActorConflict) {
			t.Fatalf("policy write after decision error = %v, want ErrClosureApprovalActorConflict", err)
		}
	})

	t.Run("human decision refuses after a policy approval", func(t *testing.T) {
		f := seedClosurePolicyFixture(t, ctx, domain.ClosureFlagOriginProposeSite, true)
		commandID := f.seedDeclineCommand(t, ctx)
		if _, err := f.recordPolicyApproval(t, ctx, f.merge); err != nil {
			t.Fatalf("record policy approval: %v", err)
		}
		if err := f.st.Write(ctx, func(tx *WriteTx) error {
			return tx.RecordProposalDecision(ctx, f.instance.ID, commandID, domain.ActionDecline, nil, f.at)
		}); !errors.Is(err, ErrClosureApprovalActorConflict) {
			t.Fatalf("decision after policy approval error = %v, want ErrClosureApprovalActorConflict", err)
		}
	})

	t.Run("reconstruction with both rows fails closed", func(t *testing.T) {
		f := seedClosurePolicyFixture(t, ctx, domain.ClosureFlagOriginProposeSite, true)
		commandID := f.seedDeclineCommand(t, ctx)
		if _, err := f.recordPolicyApproval(t, ctx, f.merge); err != nil {
			t.Fatalf("record policy approval: %v", err)
		}
		// Force a decision row in beside the policy row, bypassing the write guard
		// (its foreign keys are satisfied by the real command seeded above), to
		// prove the read fails closed on the corrupt both-actors state.
		if _, err := f.st.db.ExecContext(ctx,
			`INSERT INTO effect_proposal_decisions (instance_id, command_id, action, selected_digest, decided_at)
			 VALUES (?, ?, 'decline', NULL, '2026-09-22T12:00:00Z')`, f.instance.ID, commandID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.reconstruct(t, ctx); !errors.Is(err, errRowInconsistent) {
			t.Fatalf("both-actors reconstruction error = %v, want row inconsistency", err)
		}
	})
}

// TestPolicyClosureApprovalReconstructionFailsClosed proves a policy row whose
// digest disagrees with the current proposal, or whose merge column is
// malformed, is refused rather than reconstructed into a weakened approval.
func TestPolicyClosureApprovalReconstructionFailsClosed(t *testing.T) {
	ctx := context.Background()

	t.Run("digest disagrees with current proposal", func(t *testing.T) {
		f := seedClosurePolicyFixture(t, ctx, domain.ClosureFlagOriginProposeSite, true)
		if _, err := f.recordPolicyApproval(t, ctx, f.merge); err != nil {
			t.Fatalf("record: %v", err)
		}
		if _, err := f.st.db.ExecContext(ctx,
			`UPDATE effect_proposal_policy_approvals SET proposal_digest = ? WHERE instance_id = ?`,
			"sha256:"+strings.Repeat("c", 64), f.instance.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.reconstruct(t, ctx); !errors.Is(err, errRowInconsistent) {
			t.Fatalf("wrong-digest reconstruction error = %v, want row inconsistency", err)
		}
	})

	t.Run("malformed merge column", func(t *testing.T) {
		f := seedClosurePolicyFixture(t, ctx, domain.ClosureFlagOriginProposeSite, true)
		if _, err := f.recordPolicyApproval(t, ctx, f.merge); err != nil {
			t.Fatalf("record: %v", err)
		}
		// A non-empty but non-content-addressable publication_identity passes the
		// column CHECK yet fails ClosureApproval.Validate on read.
		if _, err := f.st.db.ExecContext(ctx,
			`UPDATE effect_proposal_policy_approvals SET publication_identity = 'not-a-digest' WHERE instance_id = ?`,
			f.instance.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.reconstruct(t, ctx); !errors.Is(err, errRowInconsistent) {
			t.Fatalf("malformed-column reconstruction error = %v, want row inconsistency", err)
		}
	})
}
