package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// seedComment extends the filing fixture with a finding whose duplicate
// search named an open issue: the pending deferred finding gets its deferred
// disposition and a search that answered issue 41. It returns that record.
func seedComment(t *testing.T, ctx context.Context, st *store.Store, fx filingFixture) domain.DuplicateSearch {
	t.Helper()
	finding := fx.findings[3].ID
	search := duplicateRecord(t, fx, finding, domain.FollowUpSourceDeferredDisposition)
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutFindingDisposition(ctx, domain.ReviewDispositionRecord{
			FindingID: finding, RunID: fx.runID, Round: 1,
			Disposition: domain.ReviewDispositionDeferred, Reason: "already tracked upstream",
			AdjudicationDigest: fx.adjudication.Digest, CreatedAt: filingAt.Add(2 * time.Minute),
		}); err != nil {
			return err
		}
		return tx.PutDuplicateSearch(ctx, search)
	}); err != nil {
		t.Fatalf("seed comment fixture: %v", err)
	}
	return search
}

func (f filingFixture) commentProposal(t *testing.T, search domain.DuplicateSearch) domain.EffectProposal {
	t.Helper()
	proposal, err := domain.NewEffectProposal(domain.EffectIssueComment, domain.IssueCommentInput{
		SubjectHandle: f.handle,
		Repository:    domain.FollowUpFilingRepository{Repo: f.target.Repo, RepositoryID: f.target.RepositoryID},
		Search:        search,
		Body:          passedIssueText("A review finding matches this issue.\n\nFinding: the config read is not retried."),
	}, f.policy)
	if err != nil {
		t.Fatal(err)
	}
	return proposal
}

// forgeComment applies mutate to a copy of proposal's comment parameters and
// recomputes the content digest, so only the store's gate can refuse it.
func forgeComment(
	t *testing.T, proposal domain.EffectProposal, mutate func(*domain.IssueCommentParameters),
) domain.EffectProposal {
	t.Helper()
	comment := *proposal.CommentProposal
	mutate(&comment)
	forged := proposal
	forged.CommentProposal = &comment
	digest, err := forged.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	forged.Digest = digest
	if err := forged.Validate(); err != nil {
		t.Fatalf("forged proposal must stay structurally valid: %v", err)
	}
	return forged
}

func TestIssueCommentAdmissionRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{})
	fx := seedFiling(t, ctx, st, "project-comment")
	search := seedComment(t, ctx, st, fx)
	proposal := fx.commentProposal(t, search)
	instance, err := allocateFiling(ctx, st, proposal, "event-comment")
	if err != nil {
		t.Fatalf("admit a comment on the searched duplicate: %v", err)
	}
	got, err := getFiling(ctx, st, instance.ID)
	if err != nil {
		t.Fatalf("reconstruct the comment: %v", err)
	}
	comment := got.Proposal.CommentProposal
	if got.Proposal.Digest != proposal.Digest || comment == nil || comment.IssueNumber != 41 ||
		comment.Source.SearchDigest != search.Digest {
		t.Fatalf("reconstructed = %#v", got.Proposal)
	}
}

// TestIssueCommentGateRefusesTamper is the comment's Tier A tamper case: a
// proposal whose issue is not the duplicate its search record names, whose
// source names another record digest, whose finding's search answered "new",
// or whose body marked passed carries a mention is refused at admission and,
// written past admission into the row, at reconstruction.
func TestIssueCommentGateRefusesTamper(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cases := []struct {
		name string
		// mutate receives the "new" search seeded for findings[0].
		mutate func(filingFixture, domain.DuplicateSearch, *domain.IssueCommentParameters)
		want   error
	}{
		{"issue off the record", func(_ filingFixture, _ domain.DuplicateSearch, c *domain.IssueCommentParameters) {
			c.IssueNumber = 7 // the record's other candidate, not its answer
		}, domain.ErrIssueCommentTargetMismatch},
		{"another record digest", func(fx filingFixture, _ domain.DuplicateSearch, c *domain.IssueCommentParameters) {
			c.Source.SearchDigest = fx.adjudication.Digest
		}, domain.ErrIssueCommentSourceMismatch},
		{"search answered new", func(fx filingFixture, fresh domain.DuplicateSearch, c *domain.IssueCommentParameters) {
			// findings[0]'s seeded search answered "new"; point the source at
			// that record with its own digest so only the answer refuses.
			c.Source.FindingID = fx.findings[0].ID
			c.Source.SearchDigest = fresh.Digest
		}, domain.ErrIssueCommentNotDuplicate},
		{"no search for the finding", func(fx filingFixture, _ domain.DuplicateSearch, c *domain.IssueCommentParameters) {
			c.Source.FindingID = fx.findings[2].ID
		}, domain.ErrIssueCommentSourceMismatch},
		{"another repository", func(_ filingFixture, _ domain.DuplicateSearch, c *domain.IssueCommentParameters) {
			c.Repository = domain.FollowUpFilingRepository{Repo: "attacker/repo", RepositoryID: 999}
		}, domain.ErrIssueCommentTargetMismatch},
		{"passed body with a mention", func(_ filingFixture, _ domain.DuplicateSearch, c *domain.IssueCommentParameters) {
			c.Body = passedIssueText("Same as this issue. Ask @octocat.")
		}, domain.ErrIssueCommentTextRejected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "store.db")
			st := storetest.Open(t, path, store.Options{})
			fx := seedFiling(t, ctx, st, "project-comment")
			search := seedComment(t, ctx, st, fx)
			newSearch, err := getDuplicateSearch(ctx, st, fx.adjudication.Digest, fx.findings[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			proposal := fx.commentProposal(t, search)
			forged := forgeComment(t, proposal, func(c *domain.IssueCommentParameters) { tc.mutate(fx, newSearch, c) })

			if _, err := allocateFiling(ctx, st, forged, "event-forged"); !errors.Is(err, tc.want) {
				t.Fatalf("admission error = %v, want %v", err, tc.want)
			}

			instance, err := allocateFiling(ctx, st, proposal, "event-comment")
			if err != nil {
				t.Fatal(err)
			}
			stored := instance
			stored.Proposal = forged
			body, err := json.Marshal(stored)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			result, err := raw.Exec(`UPDATE effect_proposal_instances SET content_digest = ?, body = ?
				WHERE instance_id = ?`, forged.Digest, string(body), instance.ID)
			if err != nil {
				t.Fatalf("tamper: %v", err)
			}
			if changed, err := result.RowsAffected(); err != nil || changed != 1 {
				t.Fatalf("tamper changed %d rows, %v", changed, err)
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := storetest.Open(t, path, store.Options{})
			if _, err := getFiling(ctx, reopened, instance.ID); !errors.Is(err, tc.want) {
				t.Fatalf("reconstruction error = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestIssueCommentDecisions shows approve and decline decide a comment card,
// a revision command never does, and an approval re-checks that the source
// still backs the comment: once a later round fixes the finding, the card
// stays readable and declinable but can no longer be approved, and a new
// comment is refused.
func TestIssueCommentDecisions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{})
	fx := seedFiling(t, ctx, st, "project-comment")
	proposal := fx.commentProposal(t, seedComment(t, ctx, st, fx))
	approved, approveCommand := openFilingCard(t, ctx, st, fx, proposal, "event-approve", domain.ActionApprove)
	if err := decideFiling(ctx, st, approved, approveCommand); err != nil {
		t.Fatalf("approve a current comment: %v", err)
	}
	revised, reviseCommand := openFilingCard(
		t, ctx, st, fx, proposal, "event-revise", domain.ActionApproveWithChanges)
	if err := decideFiling(ctx, st, revised, reviseCommand); !errors.Is(err, domain.ErrTransitionCommandMismatch) {
		t.Fatalf("approve_with_changes on a comment = %v, want ErrTransitionCommandMismatch", err)
	}
	stale, staleCommand := openFilingCard(t, ctx, st, fx, proposal, "event-stale", domain.ActionApprove)
	decline, declineCommand := openFilingCard(t, ctx, st, fx, proposal, "event-decline", domain.ActionDecline)

	finding := fx.findings[3]
	secondReview := adjudicationReviewRecord(
		t, fx.runID, 2, []domain.FindingID{finding.ID}, filingAt.Add(3*time.Hour))
	remediation := dispositionReviewRecord(t, fx.runID, 3, nil, filingAt.Add(4*time.Hour))
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutReviewRecord(ctx, secondReview, []domain.Finding{finding}); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, remediation, nil); err != nil {
			return err
		}
		return tx.PutFindingDisposition(ctx, domain.ReviewDispositionRecord{
			FindingID: finding.ID, RunID: fx.runID, Round: 2,
			Disposition: domain.ReviewDispositionFixed, Reason: "fixed in the remediation head",
			RemediationInvocationID: remediation.InvocationID, CreatedAt: filingAt.Add(5 * time.Hour),
		})
	}); err != nil {
		t.Fatalf("record the later fix: %v", err)
	}

	if _, err := getFiling(ctx, st, stale.ID); err != nil {
		t.Fatalf("read a comment whose source went stale: %v", err)
	}
	if err := decideFiling(ctx, st, stale, staleCommand); !errors.Is(err, domain.ErrFollowUpFilingSourceStale) {
		t.Fatalf("approve after the finding was fixed = %v, want ErrFollowUpFilingSourceStale", err)
	}
	if err := decideFiling(ctx, st, decline, declineCommand); err != nil {
		t.Fatalf("decline a stale comment: %v", err)
	}
	if _, err := allocateFiling(ctx, st, proposal, "event-after-fix"); !errors.Is(err, domain.ErrFollowUpFilingSourceStale) {
		t.Fatalf("admission after the finding was fixed = %v, want ErrFollowUpFilingSourceStale", err)
	}
}
