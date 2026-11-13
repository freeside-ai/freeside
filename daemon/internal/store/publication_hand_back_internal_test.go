package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publicationrecord"
)

// handBackHead is the head the lander pushed before it handed the pull
// request back: no Freeside invocation produced it.
var handBackHead = strings.Repeat("c", 40)

// handBackFixture is a published ready item whose pull request the lander
// handed back on a head it pushed, with an admitted reviewer's conversation
// comment on that head, and the hand_back authority that state proves.
type handBackFixture struct {
	externalReviewFixture
	comment domain.Finding
}

type handBackOptions struct {
	// leaveOpen skips superseding the predecessor.
	leaveOpen bool
	// pull edits the recorded pull fact before it is appended.
	pull func(*domain.PullMergeFact)
}

func conversationFinding(t *testing.T, runID domain.RunID, head string, comment int, raw string) domain.Finding {
	t.Helper()
	finding, err := domain.NewExternalFinding(domain.ExternalFindingInput{
		RunID: runID, Forge: domain.ExternalReviewForgeGitHub,
		ReviewerAccountID: externalReviewerID, ReviewerLogin: externalReviewerLogin,
		ThreadID: "issue_comment/" + strings.Repeat("7", comment), HeadSHA: head,
		Message: "needs rework", RawText: raw, CreatedAt: reentryAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	return finding
}

func appendPullFact(t *testing.T, st *Store, fact domain.PullMergeFact) {
	t.Helper()
	if err := st.WriteInternal(context.Background(), func(tx *InternalTx) error {
		_, err := tx.AppendPullMergeFact(context.Background(), fact)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func (f handBackFixture) openPullFact(head string, minute int) domain.PullMergeFact {
	return domain.PullMergeFact{
		Repo: f.binding.Repo, RepositoryID: f.binding.RepositoryID, PRNumber: f.binding.PRNumber,
		State: domain.PullRequestOpen, BaseRef: f.binding.BaseRef, HeadSHA: head,
		ObservedAt: reentryAt.Add(time.Duration(minute) * time.Minute),
	}
}

func handBackAuthority(
	runID domain.RunID, predecessor domain.ItemID, prior domain.ReviewRecord,
	finding domain.Finding, profile domain.AutomationTrustProfile,
) domain.PublicationSuccessor {
	return domain.PublicationSuccessor{
		Version: domain.PublicationHandBackVersion, Origin: domain.PublicationSuccessorHandBack,
		RunID: runID, PredecessorItemID: predecessor,
		PriorReviewInvocationID: prior.InvocationID, ReviewRound: prior.Round + 1,
		Reentry:           &domain.PublicationSuccessorReentry{BaseSHA: prior.BaseSHA, HeadSHA: finding.External.HeadSHA},
		ExternalFindingID: finding.ID, AdmittingProfileDigest: profile.ProfileDigest,
	}
}

func seedHandBack(t *testing.T, opts handBackOptions) handBackFixture {
	t.Helper()
	f := handBackFixture{externalReviewFixture: seedExternalReview(t, externalReviewOptions{leaveOpen: opts.leaveOpen})}
	pull := f.openPullFact(handBackHead, 10)
	if opts.pull != nil {
		opts.pull(&pull)
	}
	appendPullFact(t, f.st, pull)
	f.comment = conversationFinding(t, f.run.ID, handBackHead, 1, "Handed back: rework on the current tip")
	putExternalFinding(t, f.st, f.comment)
	f.authority = domain.PublicationSuccessor{
		Version: domain.PublicationHandBackVersion, Origin: domain.PublicationSuccessorHandBack,
		RunID: f.run.ID, PredecessorItemID: f.item.ID,
		PriorReviewInvocationID: f.authority.PriorReviewInvocationID, ReviewRound: f.authority.ReviewRound,
		Reentry:           &domain.PublicationSuccessorReentry{BaseSHA: reentryBase1, HeadSHA: handBackHead},
		ExternalFindingID: f.comment.ID, AdmittingProfileDigest: f.admitting.ProfileDigest,
	}
	return f
}

// TestHandBackAuthorityAccepted: an admitted account's conversation comment on
// the head the lander pushed seals a cycle that adopts that head. Its base is
// the head's merge base with the target, which need not be the base the last
// review used: the lander rebased the head, and the cycle proves the base with
// git, not the store. The pull request carries the adopted head, so that is
// the head its push leases against, and the authority keeps reading after the
// cycle's own push moves the latest recorded head.
func TestHandBackAuthorityAccepted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := seedHandBack(t, handBackOptions{})
	authority := f.authority
	reentry := *authority.Reentry
	reentry.BaseSHA = reentryBase2
	authority.Reentry = &reentry
	if err := f.record(t, authority); err != nil {
		t.Fatal(err)
	}
	if err := f.record(t, authority); err != nil {
		t.Fatalf("replay: %v", err)
	}
	var target publicationrecord.SuccessorTarget
	if err := f.st.Read(ctx, func(tx *ReadTx) (err error) {
		target, err = tx.PublicationSuccessorTarget(ctx, f.run.ID, authority.PublicationID())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if target.HeadSHA != handBackHead || target.ItemID != f.item.ID || target.PRNumber != f.binding.PRNumber {
		t.Fatalf("hand-back target = %#v", target)
	}
	appendPullFact(t, f.st, f.openPullFact(reentryHead2, 20))
	if err := f.read(t, authority); err != nil {
		t.Fatalf("read after the cycle's push: %v", err)
	}
}

// TestHandBackAuthorityRejectsEachMismatch offers a record that disagrees
// with current state on one fact at a time. The gate itself refuses each, so
// the same record is refused on every read.
func TestHandBackAuthorityRejectsEachMismatch(t *testing.T) {
	t.Parallel()
	mismatch, notAdmitted := domain.ErrParentKeyMismatch, domain.ErrExternalReviewNotAdmitted
	for _, tc := range []struct {
		name   string
		opts   handBackOptions
		want   error
		mutate func(handBackFixture, *domain.PublicationSuccessor)
	}{
		{name: "head the pull request never had", want: mismatch, mutate: func(f handBackFixture, s *domain.PublicationSuccessor) {
			other := conversationFinding(t, f.run.ID, reentryHead2, 2, "Handed back on a head nobody recorded")
			putExternalFinding(t, f.st, other)
			s.Reentry.HeadSHA, s.ExternalFindingID = reentryHead2, other.ID
		}},
		{name: "merged pull request", want: mismatch, opts: handBackOptions{pull: func(p *domain.PullMergeFact) {
			p.State, p.Merged, p.MergeCommitSHA = domain.PullRequestClosed, true, strings.Repeat("d", 40)
		}}},
		{name: "closed pull request", want: mismatch, opts: handBackOptions{pull: func(p *domain.PullMergeFact) {
			p.State = domain.PullRequestClosed
		}}},
		{name: "retargeted pull request", want: mismatch, opts: handBackOptions{pull: func(p *domain.PullMergeFact) {
			p.BaseRef = "release"
		}}},
		{name: "comment on another head", want: mismatch, mutate: func(f handBackFixture, s *domain.PublicationSuccessor) {
			other := conversationFinding(t, f.run.ID, reentryHead1, 2, "Handed back on the published head")
			putExternalFinding(t, f.st, other)
			s.ExternalFindingID = other.ID
		}},
		{name: "review thread instead of a conversation comment", want: mismatch, mutate: func(f handBackFixture, s *domain.PublicationSuccessor) {
			other := externalFindingWithText(t, f.run.ID, handBackHead, "P1: an inline finding")
			putExternalFinding(t, f.st, other)
			s.ExternalFindingID = other.ID
		}},
		{name: "unlisted author", want: notAdmitted, mutate: func(f handBackFixture, s *domain.PublicationSuccessor) {
			s.AdmittingProfileDigest = f.unlisted.ProfileDigest
		}},
		{name: "round not one above prior", want: mismatch, mutate: func(_ handBackFixture, s *domain.PublicationSuccessor) {
			s.ReviewRound = 3
		}},
		{name: "unknown predecessor", want: mismatch, mutate: func(_ handBackFixture, s *domain.PublicationSuccessor) {
			s.PredecessorItemID = "production-ready-reentry-unknown"
		}},
		{name: "predecessor still open", want: mismatch, opts: handBackOptions{leaveOpen: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := seedHandBack(t, tc.opts)
			forged := f.authority
			reentry := *f.authority.Reentry
			forged.Reentry = &reentry
			if tc.mutate != nil {
				tc.mutate(f, &forged)
			}
			if err := forged.Validate(); err != nil {
				t.Fatalf("mutation is not a store-gate case: %v", err)
			}
			if err := f.gate(forged); !errors.Is(err, tc.want) {
				t.Fatalf("gate error = %v, want %v", err, tc.want)
			}
			if err := f.record(t, forged); err == nil {
				t.Fatal("forged authority sealed")
			}
		})
	}
}

// TestHandBackSealOnlyChecks: what holds only when a cycle starts. The gate
// alone admits each record below, so each refusal is the seal's.
func TestHandBackSealOnlyChecks(t *testing.T) {
	t.Parallel()
	t.Run("profile no longer active", func(t *testing.T) {
		f := seedHandBack(t, handBackOptions{})
		activateProfile(t, f.st, f.binding.Repo, f.binding.RepositoryID, reentryAt.Add(time.Hour),
			reviewerEntry(externalReviewerID, externalReviewerLogin), reviewerEntry(900, "maintainer"))
		if err := f.gate(f.authority); err != nil {
			t.Fatal(err)
		}
		if err := f.record(t, f.authority); !errors.Is(err, domain.ErrExternalReviewNotAdmitted) {
			t.Fatalf("seal under an inactive profile: %v, want ErrExternalReviewNotAdmitted", err)
		}
	})
	t.Run("pull request moved since", func(t *testing.T) {
		f := seedHandBack(t, handBackOptions{})
		appendPullFact(t, f.st, f.openPullFact(reentryHead2, 20))
		if err := f.gate(f.authority); err != nil {
			t.Fatal(err)
		}
		if err := f.record(t, f.authority); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("seal on a head the pull request left: %v, want ErrParentKeyMismatch", err)
		}
	})
	// An item that is no longer current already has the successor that
	// replaced it, and one item admits one commandless successor.
	t.Run("predecessor no longer current", func(t *testing.T) {
		f := seedHandBack(t, handBackOptions{})
		if err := f.record(t, f.authority); err != nil {
			t.Fatal(err)
		}
		second := conversationFinding(t, f.run.ID, handBackHead, 2, "Handed back again")
		putExternalFinding(t, f.st, second)
		stale := f.authority
		stale.ExternalFindingID = second.ID
		if err := f.gate(stale); err != nil {
			t.Fatal(err)
		}
		if err := f.record(t, stale); !errors.Is(err, domain.ErrImmutableTransition) {
			t.Fatalf("second cycle from a replaced item: %v, want ErrImmutableTransition", err)
		}
	})
}

// earlierHandBack seals one hand-back cycle, lets it re-earn readiness in
// place on the adopted head, and returns the review a second cycle would
// follow, with that ready item superseded.
func earlierHandBack(t *testing.T, f handBackFixture) (domain.AttentionItem, domain.ReviewRecord) {
	t.Helper()
	if err := f.record(t, f.authority); err != nil {
		t.Fatal(err)
	}
	item, binding := f.reenteredItemAndBinding(t, f.authority, f.binding)
	f.putItem(t, item)
	if err := f.recordBinding(binding); err != nil {
		t.Fatal(err)
	}
	prior := putReentryReview(t, f.st, f.run.ID, 2, reentryBase1, handBackHead)
	supersedeReadyItem(t, f.st, item.ID)
	return item, prior
}

// TestHandBackOneCyclePerComment: a cycle that re-earned readiness on the
// adopted head leaves its comment there, where the gate alone would admit it
// again. Sealing refuses a comment an earlier cycle named.
func TestHandBackOneCyclePerComment(t *testing.T) {
	t.Parallel()
	f := seedHandBack(t, handBackOptions{})
	item, prior := earlierHandBack(t, f)
	again := handBackAuthority(f.run.ID, item.ID, prior, f.comment, f.admitting)
	if err := f.gate(again); err != nil {
		t.Fatalf("the gate alone refuses the reused comment, so this test proves nothing: %v", err)
	}
	if err := f.record(t, again); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("second cycle from one comment: %v, want ErrParentKeyMismatch", err)
	}
	// A new comment on the same head starts the next cycle.
	next := conversationFinding(t, f.run.ID, handBackHead, 2, "Handed back again")
	putExternalFinding(t, f.st, next)
	if err := f.record(t, handBackAuthority(f.run.ID, item.ID, prior, next, f.admitting)); err != nil {
		t.Fatalf("new comment refused: %v", err)
	}
}

// TestHandBackRefusedOverAnOpenEnding: an earlier hand-back cycle that ended
// on a person leaves that person's item open; a new comment does not start
// another cycle over it.
func TestHandBackRefusedOverAnOpenEnding(t *testing.T) {
	t.Parallel()
	f := seedHandBack(t, handBackOptions{})
	item, prior := earlierHandBack(t, f)
	putHeldItem(t, f.st, f.run, f.authority.BlockedItemID(), handBackHead, nil)
	next := conversationFinding(t, f.run.ID, handBackHead, 2, "Handed back again")
	putExternalFinding(t, f.st, next)
	authority := handBackAuthority(f.run.ID, item.ID, prior, next, f.admitting)
	if err := f.gate(authority); err != nil {
		t.Fatal(err)
	}
	if err := f.record(t, authority); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("hand-back over an open ending: %v, want ErrParentKeyMismatch", err)
	}
}

// TestHandBackHeadIsAdopted: an external review of the head a hand-back
// cycle re-earned readiness on is admitted, though no Freeside invocation
// produced that head: the hand-back authority adopted it.
func TestHandBackHeadIsAdopted(t *testing.T) {
	t.Parallel()
	f := seedHandBack(t, handBackOptions{})
	item, prior := earlierHandBack(t, f)
	var foreign bool
	if err := f.st.Read(context.Background(), func(tx *ReadTx) (err error) {
		foreign, err = tx.ReadyHeadIsForeign(context.Background(), item.ID)
		return err
	}); err != nil || foreign {
		t.Fatalf("ReadyHeadIsForeign = %t, %v; want false", foreign, err)
	}
	finding := externalFindingWithText(t, f.run.ID, handBackHead, "P1: still wrong")
	putExternalFinding(t, f.st, finding)
	if err := f.record(t, externalReviewAuthority(f.run.ID, item.ID, prior, finding, f.admitting)); err != nil {
		t.Fatalf("external review of the adopted head: %v", err)
	}
}

// TestHandoffConflictOnAHandedBackHead: a hand-back cycle hands its adopted
// head off under its own key before it re-earns readiness. A conflict there
// seals a handoff conflict authority over that check, and the remediated
// head's push leases against the adopted head.
func TestHandoffConflictOnAHandedBackHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := seedHandBack(t, handBackOptions{})
	if err := f.record(t, f.authority); err != nil {
		t.Fatal(err)
	}
	sealCheck := func(publication domain.InvocationID) {
		t.Helper()
		check := domain.HandoffCheck{
			RunID: f.run.ID, PublicationID: publication,
			HeadSHA: handBackHead, EvidenceBaseSHA: reentryBase1, TipSHA: reentryBase3,
			WorkflowAuditID: recordHandoffAudit(t, f.st, f.admission.Base.Repo, f.admission.Base.BaseRef, reentryBase3),
		}
		if err := f.st.WriteInternal(ctx, func(tx *InternalTx) error {
			return tx.SealHandoffTip(ctx, check, handoffConflictAt)
		}); err != nil {
			t.Fatal(err)
		}
	}
	prior := putReentryReview(t, f.st, f.run.ID, 2, reentryBase1, handBackHead)
	conflict := domain.PublicationSuccessor{
		Version: domain.PublicationHandoffConflictVersion, Origin: domain.PublicationSuccessorHandoffConflict,
		RunID: f.run.ID, PredecessorItemID: f.authority.ReadyItemID(),
		PriorReviewInvocationID: prior.InvocationID, ReviewRound: 3,
		Reentry:              &domain.PublicationSuccessorReentry{BaseSHA: reentryBase3, HeadSHA: handBackHead},
		HandoffConflictCause: domain.HandoffConflictTextual,
	}
	// A check under the cycle's publication ID is a fix's handoff, not the
	// adopted head's.
	sealCheck(f.authority.PublicationID())
	if err := f.gate(conflict); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("conflict over the publication's check: %v, want ErrParentKeyMismatch", err)
	}
	sealCheck(f.authority.InPlaceHandoffID())
	if err := f.record(t, conflict); err != nil {
		t.Fatalf("conflict over the in-place handoff: %v", err)
	}
	// The pull request still carries the adopted head, so the remediated
	// head's push leases against it, not against the published ancestor's.
	var target publicationrecord.SuccessorTarget
	if err := f.st.Read(ctx, func(tx *ReadTx) (err error) {
		target, err = tx.PublicationSuccessorTarget(ctx, f.run.ID, conflict.PublicationID())
		return err
	}); err != nil || target.HeadSHA != handBackHead || target.ItemID != f.item.ID {
		t.Fatalf("conflict target = %#v, %v; want head %s on %s", target, err, handBackHead, f.item.ID)
	}
}

// TestHandBackReadyBinding: the label remover's work list. The root ready
// item is never due; the ready item a hand-back cycle earned is due while it
// is open, and is not once it is superseded. A later cycle's ready item
// whose predecessor already published is not due: that predecessor's own
// readiness took the label off.
func TestHandBackReadyBinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := seedHandBack(t, handBackOptions{})
	due := func() (domain.ReadyItemPRBinding, bool) {
		t.Helper()
		var (
			ready domain.ReadyItemPRBinding
			ok    bool
		)
		if err := f.st.Read(ctx, func(tx *ReadTx) (err error) {
			ready, ok, err = tx.HandBackReadyBinding(ctx, f.run.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return ready, ok
	}
	if _, ok := due(); ok {
		t.Fatal("the root ready item is due")
	}
	if err := f.record(t, f.authority); err != nil {
		t.Fatal(err)
	}
	item, binding := f.reenteredItemAndBinding(t, f.authority, f.binding)
	f.putItem(t, item)
	if err := f.recordBinding(binding); err != nil {
		t.Fatal(err)
	}
	if ready, ok := due(); !ok || ready.ItemID != item.ID || ready.PRNumber != f.binding.PRNumber {
		t.Fatalf("hand-back ready item: %#v, %t", ready, ok)
	}
	prior := putReentryReview(t, f.st, f.run.ID, 2, reentryBase1, handBackHead)
	supersedeReadyItem(t, f.st, item.ID)
	if _, ok := due(); ok {
		t.Fatal("a superseded hand-back ready item is due")
	}
	finding := externalFindingWithText(t, f.run.ID, handBackHead, "P1: still wrong")
	putExternalFinding(t, f.st, finding)
	later := externalReviewAuthority(f.run.ID, item.ID, prior, finding, f.admitting)
	if err := f.record(t, later); err != nil {
		t.Fatal(err)
	}
	next, nextBinding := f.reenteredItemAndBinding(t, later, binding)
	f.putItem(t, next)
	if err := f.recordBinding(nextBinding); err != nil {
		t.Fatal(err)
	}
	if ready, ok := due(); ok {
		t.Fatalf("an external review's ready item after a published hand-back is due: %#v", ready)
	}
}
