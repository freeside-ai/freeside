package signet_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestOpenIssueCommentItemOpensTheCommentCard pins the item the comment opener
// writes: the filing card's shape with the comment's fixed reason.
func TestOpenIssueCommentItemOpensTheCommentCard(t *testing.T) {
	t.Parallel()
	f := newCommentFixture(t)
	item := f.item
	wantActions := []domain.Action{domain.ActionApprove, domain.ActionDecline, domain.ActionSnooze}
	if item.ID != domain.ItemID(string(f.instance.ID)+"/effect") ||
		item.Type != domain.AttentionEffectProposal || item.Status != domain.StatusOpen ||
		item.InterruptionClass != domain.InterruptionPlannedGate ||
		item.Subject.Type != domain.SubjectProposalBatch ||
		item.Subject.ID != domain.SubjectID(f.instance.ProposalBatchID) ||
		item.Reason != "Decide whether to comment on the matching issue" || item.PRHeadSHA != "" ||
		!slices.Equal(item.RequestedDecision, wantActions) ||
		!slices.Equal(item.ArtifactDigests, []domain.Digest{f.instance.Proposal.Digest}) {
		t.Fatalf("comment item = %#v", item)
	}
}

// TestProposalCardOpenersRefuseTheOtherKind pins the kind check on both
// openers, so a comment never takes the filing card or the reverse.
func TestProposalCardOpenersRefuseTheOtherKind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	comment, filing := newCommentFixture(t), newFilingFixture(t)
	cases := map[string]func() error{
		"filing opener on a comment": func() error {
			return comment.store.Write(ctx, func(tx *store.WriteTx) error {
				_, err := signet.OpenFollowUpFilingItem(ctx, tx, comment.blobs, comment.instance.ID, *comment.now)
				return err
			})
		},
		"comment opener on a filing": func() error {
			return filing.store.Write(ctx, func(tx *store.WriteTx) error {
				_, err := signet.OpenIssueCommentItem(ctx, tx, filing.blobs, filing.instance.ID, *filing.now)
				return err
			})
		},
	}
	for name, open := range cases {
		if err := open(); !errors.Is(err, domain.ErrEffectProposalInconsistent) {
			t.Errorf("%s = %v, want ErrEffectProposalInconsistent", name, err)
		}
	}
}

// TestEffectProposalFactsServeTheCommentArm proves the facts read serves the
// comment arm: the project's repository, the target issue the search
// answered, the screened body, and the source link, with the other arms null
// and no subject handle or policy identity on the wire.
func TestEffectProposalFactsServeTheCommentArm(t *testing.T) {
	t.Parallel()
	f := newCommentFixture(t)
	facts, err := f.service.GetEffectProposalFacts(context.Background(), f.item.ID)
	if err != nil {
		t.Fatalf("GetEffectProposalFacts: %v", err)
	}
	if facts.EffectKind != domain.EffectIssueComment || facts.ProposalDigest != f.instance.Proposal.Digest ||
		facts.FollowUpFiling != nil || facts.SourceIssueClosure != nil || facts.Supersedes != nil {
		t.Fatalf("facts = %#v, want only the comment arm", facts)
	}
	comment := facts.IssueComment
	want := f.instance.Proposal.CommentProposal
	if comment == nil || comment.Repository != want.Repository || comment.IssueNumber != commentFixtureIssue ||
		comment.Body != want.Body || comment.Source != want.Source {
		t.Fatalf("comment facts = %#v, want %#v on issue %d", comment, want, commentFixtureIssue)
	}
	body, err := json.Marshal(facts)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"subject_handle", "resolved_policy", "policy_run"} {
		if bytes.Contains(body, []byte(forbidden)) {
			t.Fatalf("facts wire %s carries %q", body, forbidden)
		}
	}
	if !bytes.Contains(body, []byte(`"follow_up_filing":null`)) {
		t.Fatalf("facts wire %s lacks an explicit null filing arm", body)
	}
}

// TestApprovingACommentWakesTheFiler pins that a committed comment approve
// wakes the follow-up posters, as a filing approve does, and records the
// approval the dispatcher reads.
func TestApprovingACommentWakesTheFiler(t *testing.T) {
	t.Parallel()
	f := newCommentFixture(t)
	ctx := context.Background()
	filer := &wakeCounter{}
	service := signet.NewService(f.store, signet.WithBlobStore(f.blobs),
		signet.WithClock(func() time.Time { return *f.now }),
		signet.WithFollowUpFiler(func() signet.FollowUpFiler { return filer }))
	if _, err := service.Submit(ctx, filingDecision(f, "approve-comment", domain.ActionApprove)); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if filer.wakes != 1 {
		t.Fatalf("wakes after the approve = %d, want 1", filer.wakes)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		approved, err := tx.ApprovedIssueComment(ctx, f.instance.ID)
		if err != nil {
			return err
		}
		if approved.IssueNumber != commentFixtureIssue || approved.RepositoryID != testProjectRepositoryID {
			t.Fatalf("approved comment = %#v", approved)
		}
		return nil
	}); err != nil {
		t.Fatalf("ApprovedIssueComment: %v", err)
	}
}
