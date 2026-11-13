package publish_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/domain/domaintest"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestRunOutcomesReadTheDispatchersCappedNotices drives the real filer and
// comment dispatcher to a filed issue, a posted comment, and a cap of each,
// and shows the run outcome list reads every one, with a declined finding
// beside them. signet repeats the notice IDs and codes these components
// write, so this test fails if the two drift apart.
func TestRunOutcomesReadTheDispatchersCappedNotices(t *testing.T) {
	t.Parallel()
	h := newFilingHarness(t)
	h.trustComments()
	run := h.seedRun("run-outcomes", map[string]string{"follow_up_filing.max_per_run": "2"}, nil)

	filing := h.approve(run, 0)
	comment := h.proposeComment(run, 1, 41, domain.ActionApprove)
	h.mustPass()
	h.mustCommentPass()
	cappedFiling := h.approve(run, 2)
	cappedComment := h.proposeComment(run, 3, 42, domain.ActionApprove)
	h.mustPass()
	h.mustCommentPass()
	waiting := h.propose(run, 4, "")
	declined := h.propose(run, 5, domain.ActionDecline)
	declinedFinding := h.declineInRoundTwo(run)

	filedNumber := h.forgeIssues()[0].Number
	commentID := h.issueCommentsOn(41)[0].ID
	h.assertLedgered(filing.ID, filedNumber)
	h.assertPosted(comment.ID, 41, commentID)
	filingCap := h.assertTerminal(cappedFiling.ID, domain.FollowUpFilingRefused,
		domain.FollowUpFilingRefusalPreconditionFailed, "follow_up_filing_capped")
	commentCap := h.assertCommentTerminal(cappedComment.ID, domain.FollowUpFilingRefused,
		domain.FollowUpFilingRefusalPreconditionFailed, "issue_comment_capped")

	outcomes := h.runOutcomes(run.id)
	if !outcomes.Complete {
		t.Fatal("outcome list incomplete")
	}
	posts := postsByInstance(t, outcomes, filing, comment, cappedFiling, cappedComment, waiting, declined)
	assertPost(t, posts[filing.ID], domain.EffectFollowUpFiling, signet.RunOutcomeCompleted)
	if got := posts[filing.ID]; got.IssueNumber == nil || *got.IssueNumber != filedNumber || got.CommentID != nil {
		t.Fatalf("filed issue = %+v, want issue %d", got, filedNumber)
	}
	assertPost(t, posts[comment.ID], domain.EffectIssueComment, signet.RunOutcomeCompleted)
	if got := posts[comment.ID]; got.IssueNumber == nil || *got.IssueNumber != 41 ||
		got.CommentID == nil || *got.CommentID != commentID {
		t.Fatalf("posted comment = %+v, want comment %d on issue 41", got, commentID)
	}
	for id, notice := range map[domain.ProposalInstanceID]domain.AttentionItem{
		cappedFiling.ID: filingCap, cappedComment.ID: commentCap,
	} {
		got := posts[id]
		if got.State != signet.RunOutcomeCapped || got.RefusalReason == nil ||
			*got.RefusalReason != domain.FollowUpFilingRefusalPreconditionFailed || got.Notice == nil ||
			got.Notice.Code != notice.HealthDiagnostic.Code || got.Notice.Reason != notice.Reason {
			t.Fatalf("capped %q = %+v, want capped with notice %q", id, got, notice.Reason)
		}
	}
	assertPost(t, posts[waiting.ID], domain.EffectFollowUpFiling, signet.RunOutcomePending)
	assertPost(t, posts[declined.ID], domain.EffectFollowUpFiling, signet.RunOutcomeCardDeclined)

	var listed *signet.RunOutcomeFinding
	for index, finding := range outcomes.Findings {
		if finding.FindingID == declinedFinding.ID {
			listed = &outcomes.Findings[index]
		}
	}
	if listed == nil || listed.Disposition != domain.ReviewDispositionDeclined || listed.Round != 2 ||
		listed.External || listed.Message != declinedFinding.Message || listed.Reason != "The caller bounds it." {
		t.Fatalf("declined finding = %+v in %+v", listed, outcomes.Findings)
	}
	// Round 2 lists only the declined finding, so the round 1 deferrals
	// stand as the latest disposition of the others.
	if len(outcomes.Findings) != filingFindingsPerRun+1 {
		t.Fatalf("findings = %d, want %d: %+v", len(outcomes.Findings), filingFindingsPerRun+1, outcomes.Findings)
	}
}

// TestRunOutcomesReadEachRefusedAndAmbiguousNotice drives the filer and the
// comment dispatcher to a refusal and an ambiguous create of each, and shows
// the list carries each notice. With the capped test, it pins every notice
// code signet repeats.
func TestRunOutcomesReadEachRefusedAndAmbiguousNotice(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		kind  domain.EffectKind
		state signet.RunOutcomePostState
		code  string
		drive func(h *filingHarness, run filingRun) domain.ProposalInstance
	}{
		"filing refused": {
			kind: domain.EffectFollowUpFiling, state: signet.RunOutcomeRefused, code: "follow_up_filing_refused",
			drive: func(h *filingHarness, run filingRun) domain.ProposalInstance {
				instance := h.approve(run, 0)
				h.respond(fakeCreateResponse{Status: http.StatusUnprocessableEntity})
				h.mustPass()
				return instance
			},
		},
		"filing ambiguous": {
			kind: domain.EffectFollowUpFiling, state: signet.RunOutcomeAmbiguous, code: "follow_up_filing_ambiguous",
			drive: func(h *filingHarness, run filingRun) domain.ProposalInstance {
				instance := h.approve(run, 0)
				h.respond(unprovenLost)
				h.mustPass()
				h.advance(pastSettle)
				h.mustPass()
				return instance
			},
		},
		"comment refused": {
			// No trust profile reads, so the dispatcher refuses the post.
			kind: domain.EffectIssueComment, state: signet.RunOutcomeRefused, code: "issue_comment_refused",
			drive: func(h *filingHarness, run filingRun) domain.ProposalInstance {
				instance := h.proposeComment(run, 0, 41, domain.ActionApprove)
				h.mustCommentPass()
				return instance
			},
		},
		"comment ambiguous": {
			kind: domain.EffectIssueComment, state: signet.RunOutcomeAmbiguous, code: "issue_comment_ambiguous",
			drive: func(h *filingHarness, run filingRun) domain.ProposalInstance {
				h.trustComments()
				h.plantComment(41, filingBotID)
				instance := h.proposeComment(run, 0, 41, domain.ActionApprove)
				h.issueComments.responses = []fakeCreateResponse{{Status: http.StatusInternalServerError}}
				_ = h.commentPass()
				h.advance(pastSettle)
				for range 5 {
					if intent := h.commentIntent(instance.ID); intent != nil && intent.Terminal != nil {
						break
					}
					_ = h.commentPass()
				}
				return instance
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newFilingHarness(t)
			run := h.seedRun("run-outcomes", nil, nil)
			instance := tc.drive(h, run)
			outcome := domain.FollowUpFilingRefused
			if tc.state == signet.RunOutcomeAmbiguous {
				outcome = domain.FollowUpFilingAmbiguous
			}
			var notice domain.AttentionItem
			if tc.kind == domain.EffectIssueComment {
				notice = h.assertCommentTerminal(instance.ID, outcome, refusalReasonFor(h.commentIntent(instance.ID)), tc.code)
			} else {
				notice = h.assertTerminal(instance.ID, outcome, refusalReasonFor(h.intent(instance.ID)), tc.code)
			}
			outcomes := h.runOutcomes(run.id)
			got := postsByInstance(t, outcomes, instance)[instance.ID]
			assertPost(t, got, tc.kind, tc.state)
			if !outcomes.Complete || got.Notice == nil || got.Notice.Code != tc.code || got.Notice.Reason != notice.Reason {
				t.Fatalf("post = %+v, want notice %q: %q", got, tc.code, notice.Reason)
			}
			if (got.RefusalReason != nil) != (tc.state == signet.RunOutcomeRefused) {
				t.Fatalf("refusal reason = %v on a %s post", got.RefusalReason, tc.state)
			}
		})
	}
}

// TestRunOutcomesReadUnfinishedAndClosedPosts shows a post with nothing
// final reads pending, whether approved and not yet dispatched or sent and
// waiting on a retry, and that once the run's workflow closes, a proposal
// with no intent reads withdrawn, approved or not.
func TestRunOutcomesReadUnfinishedAndClosedPosts(t *testing.T) {
	t.Parallel()
	h := newFilingHarness(t)
	run := h.seedRun("run-outcomes", nil, nil)
	retrying := h.approve(run, 0)
	h.respond(fakeCreateResponse{Status: http.StatusTooManyRequests})
	h.mustPass()
	if intent := h.intent(retrying.ID); intent == nil || intent.Terminal != nil {
		t.Fatalf("retrying intent = %+v, want one open with no terminal", intent)
	}
	approved := h.approve(run, 1)
	undecided := h.propose(run, 2, "")
	posts := postsByInstance(t, h.runOutcomes(run.id), retrying, approved, undecided)
	for _, id := range []domain.ProposalInstanceID{retrying.ID, approved.ID, undecided.ID} {
		assertPost(t, posts[id], domain.EffectFollowUpFiling, signet.RunOutcomePending)
	}

	closed := h.seedRun("run-closed", nil, nil)
	closedApproved := h.approve(closed, 0)
	closedUndecided := h.propose(closed, 1, "")
	closedDeclined := h.propose(closed, 2, domain.ActionDecline)
	publish.CloseWorkflowForTest(t, h.store, closed.id, h.now)
	outcomes := h.runOutcomes(closed.id)
	posts = postsByInstance(t, outcomes, closedApproved, closedUndecided, closedDeclined)
	assertPost(t, posts[closedApproved.ID], domain.EffectFollowUpFiling, signet.RunOutcomeWithdrawn)
	assertPost(t, posts[closedUndecided.ID], domain.EffectFollowUpFiling, signet.RunOutcomeWithdrawn)
	assertPost(t, posts[closedDeclined.ID], domain.EffectFollowUpFiling, signet.RunOutcomeCardDeclined)
	if !outcomes.Complete {
		t.Fatal("outcome list incomplete")
	}
}

// TestRunOutcomesReadPolicyApprovedComments shows a comment approved by
// policy, which has neither a card nor a decision, reads pending until its
// dispatcher posts it and completed after, and withdrawn when the run's
// workflow closes before any intent opens.
func TestRunOutcomesReadPolicyApprovedComments(t *testing.T) {
	t.Parallel()
	h := newFilingHarness(t)
	h.trustComments()
	run := h.seedRun("run-outcomes", nil, nil)
	comment := h.proposePolicyComment(run, 0, 41)
	assertPost(t, postsByInstance(t, h.runOutcomes(run.id), comment)[comment.ID],
		domain.EffectIssueComment, signet.RunOutcomePending)
	h.mustCommentPass()
	outcomes := h.runOutcomes(run.id)
	got := postsByInstance(t, outcomes, comment)[comment.ID]
	assertPost(t, got, domain.EffectIssueComment, signet.RunOutcomeCompleted)
	if !outcomes.Complete || got.Notice != nil || got.CommentID == nil {
		t.Fatalf("posted comment = %+v, complete %v; want a comment ID and no notice", got, outcomes.Complete)
	}

	closed := h.seedRun("run-closed", nil, nil)
	unsent := h.proposePolicyComment(closed, 0, 42)
	publish.CloseWorkflowForTest(t, h.store, closed.id, h.now)
	assertPost(t, postsByInstance(t, h.runOutcomes(closed.id), unsent)[unsent.ID],
		domain.EffectIssueComment, signet.RunOutcomeWithdrawn)
}

// refusalReasonFor is the terminal's refusal reason, or "" for none.
func refusalReasonFor[I interface {
	*domain.FollowUpFilingIntent | *domain.IssueCommentIntent
}](intent I) domain.FollowUpFilingRefusalReason {
	var terminal *domain.FollowUpFilingTerminal
	switch intent := any(intent).(type) {
	case *domain.FollowUpFilingIntent:
		if intent != nil {
			terminal = intent.Terminal
		}
	case *domain.IssueCommentIntent:
		if intent != nil {
			terminal = intent.Terminal
		}
	}
	if terminal == nil || terminal.Reason == nil {
		return ""
	}
	return *terminal.Reason
}

// TestRunOutcomesReadACapWithNoNoticeAsRefused shows a cap whose notice is
// missing, as when the proposal's workflow closed before the filer raised
// it, reads as refused with the ledger's reason: only the notice says
// capped.
func TestRunOutcomesReadACapWithNoNoticeAsRefused(t *testing.T) {
	t.Parallel()
	h := newFilingHarness(t)
	run := h.seedRun("run-outcomes", map[string]string{"follow_up_filing.max_per_run": "1"}, nil)
	first := h.approve(run, 0)
	h.mustPass()
	capped := h.approve(run, 1)
	h.mustPass()
	h.assertTerminal(capped.ID, domain.FollowUpFilingRefused,
		domain.FollowUpFilingRefusalPreconditionFailed, "follow_up_filing_capped")
	h.tamper(`DELETE FROM attention_items WHERE id = ?`, "follow-up-filing/"+string(capped.ID)+"/follow_up_filing_capped")

	outcomes := h.runOutcomes(run.id)
	got := postsByInstance(t, outcomes, first, capped)[capped.ID]
	if !outcomes.Complete || got.State != signet.RunOutcomeRefused || got.Notice != nil ||
		got.RefusalReason == nil || *got.RefusalReason != domain.FollowUpFilingRefusalPreconditionFailed {
		t.Fatalf("cap with no notice = %+v, want refused with no notice", got)
	}
}

// TestRunOutcomesListAnUnreadableInstance shows an instance whose rows no
// longer pass the store's checks reads as unreadable, beside the instances
// that still read, and marks the list incomplete.
func TestRunOutcomesListAnUnreadableInstance(t *testing.T) {
	t.Parallel()
	h := newFilingHarness(t)
	run := h.seedRun("run-outcomes", nil, nil)
	broken := h.propose(run, 0, "")
	fine := h.propose(run, 1, "")
	withdrawn := h.propose(run, 2, "")
	h.supersedeCard(withdrawn.ID)
	h.rewriteBody(broken, run, 0, "@someone please look")

	outcomes := h.runOutcomes(run.id)
	if outcomes.Complete || len(outcomes.Posts) != 3 {
		t.Fatalf("outcomes = %+v, want three posts and an incomplete list", outcomes)
	}
	states := map[signet.RunOutcomePostState]int{}
	for _, post := range outcomes.Posts {
		states[post.State]++
		if post.State == signet.RunOutcomeUnreadable &&
			(post.EffectKind != domain.EffectFollowUpFiling || post.FindingID != nil || post.Repository != nil) {
			t.Fatalf("unreadable post = %+v, want only its kind", post)
		}
	}
	want := map[signet.RunOutcomePostState]int{
		signet.RunOutcomeUnreadable: 1, signet.RunOutcomePending: 1, signet.RunOutcomeWithdrawn: 1,
	}
	if len(states) != len(want) || states[signet.RunOutcomeUnreadable] != 1 ||
		states[signet.RunOutcomePending] != 1 || states[signet.RunOutcomeWithdrawn] != 1 {
		t.Fatalf("states = %v, want %v (fine %q, withdrawn %q)", states, want, fine.ID, withdrawn.ID)
	}
}

func (h *filingHarness) runOutcomes(runID domain.RunID) signet.RunOutcomes {
	h.t.Helper()
	var outcomes signet.RunOutcomes
	if err := h.store.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		outcomes, _, err = signet.ReadRunOutcomes(context.Background(), tx, runID)
		return err
	}); err != nil {
		h.t.Fatalf("read run outcomes: %v", err)
	}
	return outcomes
}

// postsByInstance keys the list's posts by the instance whose finding each
// names, failing on a post that names no listed instance's finding.
func postsByInstance(
	t *testing.T, outcomes signet.RunOutcomes, instances ...domain.ProposalInstance,
) map[domain.ProposalInstanceID]signet.RunOutcomePost {
	t.Helper()
	byFinding := map[domain.FindingID]domain.ProposalInstanceID{}
	for _, instance := range instances {
		if filing := instance.Proposal.FilingProposal; filing != nil {
			byFinding[filing.Source.FindingID] = instance.ID
		} else {
			byFinding[instance.Proposal.CommentProposal.Source.FindingID] = instance.ID
		}
	}
	out := map[domain.ProposalInstanceID]signet.RunOutcomePost{}
	for _, post := range outcomes.Posts {
		if post.FindingID == nil {
			t.Fatalf("post %+v names no finding", post)
		}
		id, ok := byFinding[*post.FindingID]
		if !ok {
			t.Fatalf("post %+v names an unexpected finding", post)
		}
		if post.Repository == nil || post.Repository.Repo != filingRepo || post.Repository.RepositoryID != testRepoID {
			t.Fatalf("post %+v repository, want %s (%d)", post, filingRepo, testRepoID)
		}
		out[id] = post
	}
	if len(out) != len(instances) {
		t.Fatalf("posts = %+v, want one for each of %d instances", outcomes.Posts, len(instances))
	}
	return out
}

func assertPost(t *testing.T, got signet.RunOutcomePost, kind domain.EffectKind, state signet.RunOutcomePostState) {
	t.Helper()
	if got.EffectKind != kind || got.State != state {
		t.Fatalf("post = %+v, want a %s in state %s", got, kind, state)
	}
}

// declineInRoundTwo records a second review round that lists one new finding
// and an adjudication that declines it, with its declined disposition.
func (h *filingHarness) declineInRoundTwo(run filingRun) domain.Finding {
	h.t.Helper()
	ctx := context.Background()
	finding := domain.Finding{
		ID: domain.FindingID("finding-" + string(run.id) + "-declined"), RunID: run.id, Source: "codex_local",
		Location: &domain.FindingLocation{Path: "daemon/declined.go", StartLine: 1, EndLine: 1},
		Message:  "The retry loop has no upper bound.", RawText: "The retry loop has no upper bound.",
		CreatedAt: filingAt.Add(time.Hour),
	}
	reviewID := domain.InvocationID("review-" + string(run.id) + "-2")
	review, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: reviewID, RunID: run.id, Round: 2,
		Admission:           fake.RecordAdmission(domain.RoleReviewer, run.id, reviewID),
		ConfigurationDigest: filingDigest("c"), InstructionDigest: filingDigest("d"),
		BaseSHA: "base", HeadSHA: "head-2", CompletedAt: filingAt.Add(time.Hour),
		CompletionEvidence: filingDigest("e"), Outcome: domain.ReviewFindings,
		FindingIDs: []domain.FindingID{finding.ID},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	entry, err := domain.NewModelAdjudicationEntry(
		finding.ID, domain.GoalContradictory, nil, domain.RouteDecline, domain.ConfidenceHigh,
		"contradicts the goal", nil, nil, nil, nil, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	adjudication, err := domain.NewFindingAdjudication(
		run.id, 2, filingDigest("f"), review.InstructionDigest, run.policy.Digest,
		[]domain.FindingAdjudicationEntry{entry}, "", filingAt.Add(time.Hour+time.Minute))
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutReviewRecord(ctx, review, []domain.Finding{finding}); err != nil {
			return err
		}
		if err := tx.PutFindingAdjudication(ctx, adjudication); err != nil {
			return err
		}
		return tx.PutFindingDisposition(ctx, domain.ReviewDispositionRecord{
			FindingID: finding.ID, RunID: run.id, Round: 2,
			Disposition: domain.ReviewDispositionDeclined, Reason: "The caller bounds it.",
			AdjudicationDigest: adjudication.Digest, CreatedAt: filingAt.Add(time.Hour + 2*time.Minute),
		})
	}); err != nil {
		h.t.Fatalf("decline in round 2: %v", err)
	}
	return finding
}

// supersedeCard closes a proposal's open card with no decision, as a newer
// proposal for the same finding would.
func (h *filingHarness) supersedeCard(id domain.ProposalInstanceID) {
	h.t.Helper()
	ctx := context.Background()
	if err := h.store.Write(ctx, func(tx *store.WriteTx) error {
		item, _, open, err := tx.OpenEffectItemForInstance(ctx, id)
		if err != nil {
			return err
		}
		if !open {
			return fmt.Errorf("no open card")
		}
		item.ItemVersion++
		item.Status = domain.StatusSuperseded
		return tx.PutAttentionItem(ctx, domaintest.MustConclude(item))
	}); err != nil {
		h.t.Fatalf("supersede card for %q: %v", id, err)
	}
}
