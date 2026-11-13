package domain_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

func commentRepository() domain.FollowUpFilingRepository {
	return domain.FollowUpFilingRepository{Repo: "acme/widgets", RepositoryID: 42}
}

func commentInput(t *testing.T) domain.IssueCommentInput {
	t.Helper()
	return domain.IssueCommentInput{
		SubjectHandle: "subject-opaque-1", Repository: commentRepository(),
		Search: duplicateSearchFixture(t, "candidate-1"),
		Body:   screenedIssueText("A review finding matches this issue.\n\nFinding: the config read is not retried."),
	}
}

func commentProposal(t *testing.T, input domain.IssueCommentInput) domain.EffectProposal {
	t.Helper()
	p, err := domain.NewEffectProposal(domain.EffectIssueComment, input, proposalPolicy(t))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestIssueCommentGoldenAndRoundTrip pins the comment proposal's encoding. The
// fixture is built by the validating constructor, so it doubles as a
// validation-positive case; the issue number and source come from the search
// record, never from the caller.
func TestIssueCommentGoldenAndRoundTrip(t *testing.T) {
	input := commentInput(t)
	proposal := commentProposal(t, input)
	body, err := proposal.Encode()
	if err != nil {
		t.Fatal(err)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, body, "", "  "); err != nil {
		t.Fatal(err)
	}
	pretty.WriteByte('\n')
	golden.Assert(t, "effect_proposal_issue_comment", pretty.Bytes())
	decoded, err := domain.DecodeEffectProposal(body)
	if err != nil {
		t.Fatal(err)
	}
	comment := decoded.CommentProposal
	if decoded.Digest != proposal.Digest || comment == nil || decoded.FilingProposal != nil ||
		comment.IssueNumber != 17 || comment.Source.SearchDigest != input.Search.Digest ||
		comment.Source.FindingID != input.Search.FindingID ||
		comment.Source.AdjudicationDigest != input.Search.AdjudicationDigest {
		t.Fatalf("decoded = %#v", decoded)
	}
	if err := domain.GateIssueComment(decoded, commentRepository(), input.Search); err != nil {
		t.Fatalf("gate on the deriving record: %v", err)
	}
}

func TestIssueCommentConstructorRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*testing.T, *domain.IssueCommentInput)
		want   error
	}{
		{"search answered new", func(t *testing.T, in *domain.IssueCommentInput) {
			in.Search = duplicateSearchFixture(t, "")
		}, domain.ErrIssueCommentNotDuplicate},
		{"search for another repository", func(_ *testing.T, in *domain.IssueCommentInput) {
			in.Repository.RepositoryID = 99
		}, domain.ErrIssueCommentTargetMismatch},
		{"search digest tampered", func(_ *testing.T, in *domain.IssueCommentInput) {
			in.Search.DuplicateOf = "candidate-2"
		}, domain.ErrDuplicateSearchDigestMismatch},
		{"body without a record", func(_ *testing.T, in *domain.IssueCommentInput) {
			in.Body = domain.ScreenedIssueText{Text: "A comment"}
		}, domain.ErrFollowUpFilingTextUnscreened},
		{"body verdict rejected", func(_ *testing.T, in *domain.IssueCommentInput) {
			in.Body.Verdict = domain.ScreeningVerdictRejected
		}, domain.ErrFollowUpFilingTextUnscreened},
		{"blank body", func(_ *testing.T, in *domain.IssueCommentInput) {
			in.Body.Text = " \n"
		}, domain.ErrEffectProposalInconsistent},
		{"oversized body", func(_ *testing.T, in *domain.IssueCommentInput) {
			in.Body.Text = strings.Repeat("x", domain.MaxFollowUpFilingBodyBytes+1)
		}, domain.ErrProposalParameterTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := commentInput(t)
			tc.mutate(t, &input)
			_, err := domain.NewEffectProposal(domain.EffectIssueComment, input, proposalPolicy(t))
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
	if _, err := domain.NewEffectProposal(domain.EffectIssueComment,
		domain.FollowUpFilingInput{}, proposalPolicy(t)); !errors.Is(err, domain.ErrEffectProposalInconsistent) {
		t.Fatalf("wrong input type error = %v", err)
	}
}

// TestGateIssueCommentRejectsTamper is the target gate's tamper case: a
// proposal whose issue number is not the record's duplicate, a record that is
// not the one the source names, and a record that answered "new" are refused.
func TestGateIssueCommentRejectsTamper(t *testing.T) {
	search := duplicateSearchFixture(t, "candidate-1")
	proposal := commentProposal(t, commentInput(t))

	otherIssue := proposal
	otherComment := *proposal.CommentProposal
	otherComment.IssueNumber = 9
	otherIssue.CommentProposal = &otherComment
	if err := domain.GateIssueComment(otherIssue, commentRepository(), search); !errors.Is(err, domain.ErrIssueCommentTargetMismatch) {
		t.Fatalf("issue number off the record = %v", err)
	}
	if err := domain.GateIssueComment(proposal, commentRepository(), duplicateSearchFixture(t, "candidate-2")); !errors.Is(err, domain.ErrIssueCommentSourceMismatch) {
		t.Fatalf("record with another digest = %v", err)
	}
	if err := domain.GateIssueComment(proposal, commentRepository(), duplicateSearchFixture(t, "")); !errors.Is(err, domain.ErrIssueCommentSourceMismatch) {
		t.Fatalf("new record with another digest = %v", err)
	}
	// A "new" record the source does name: the source digest is rewritten to
	// match, so only the answer refuses it.
	fresh := duplicateSearchFixture(t, "")
	newSource := proposal
	newComment := *proposal.CommentProposal
	newComment.Source.SearchDigest = fresh.Digest
	newSource.CommentProposal = &newComment
	if err := domain.GateIssueComment(newSource, commentRepository(), fresh); !errors.Is(err, domain.ErrIssueCommentNotDuplicate) {
		t.Fatalf("record answering new = %v", err)
	}
	other := commentRepository()
	other.RepositoryID = 99
	if err := domain.GateIssueComment(proposal, other, search); !errors.Is(err, domain.ErrIssueCommentTargetMismatch) {
		t.Fatalf("another repository = %v", err)
	}
	// The repository id decides (#1537): a rename keeps the comment's target.
	renamed := commentRepository()
	renamed.Repo = "acme/renamed"
	if err := domain.GateIssueComment(proposal, renamed, search); err != nil {
		t.Fatalf("renamed repository: %v", err)
	}
	if err := domain.GateIssueComment(filingProposal(t, filingInput(domain.FollowUpSourceDeferredDisposition)),
		commentRepository(), search); !errors.Is(err, domain.ErrEffectProposalInconsistent) {
		t.Fatalf("filing proposal through the comment gate = %v", err)
	}
}

func TestIssueCommentRejectsMixedKindArms(t *testing.T) {
	comment := commentProposal(t, commentInput(t))
	filing := filingProposal(t, filingInput(domain.FollowUpSourceDeferredDisposition))
	withFiling := comment
	withFiling.FilingProposal = filing.FilingProposal
	filingWithComment := filing
	filingWithComment.CommentProposal = comment.CommentProposal
	empty := comment
	empty.CommentProposal = nil
	for name, proposal := range map[string]domain.EffectProposal{
		"comment with a filing arm": withFiling, "filing with a comment arm": filingWithComment,
		"comment without its arm": empty,
	} {
		if err := proposal.Validate(); !errors.Is(err, domain.ErrEffectProposalInconsistent) {
			t.Errorf("%s: error = %v, want ErrEffectProposalInconsistent", name, err)
		}
	}
}

// TestEffectGateOn: an absent gate is off, a bool reads as itself, and a
// value that does not parse turns the gate on.
func TestEffectGateOn(t *testing.T) {
	t.Parallel()
	provenance := domain.KeyProvenance{
		Source: domain.ProvenanceOverride, Digest: domain.Digest("sha256:" + strings.Repeat("a", 64)),
	}
	for _, tc := range []struct {
		name  string
		value *string
		want  bool
	}{
		{"absent", nil, false},
		{"true", ptr("true"), true},
		{"false", ptr("false"), false},
		{"malformed", ptr("sometimes"), true},
		{"empty", ptr(""), true},
		{"loose false spelling", ptr("0"), true},
		{"capitalized false", ptr("False"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			keys := []domain.PolicyKey{{Key: "paths", Value: "daemon/", Provenance: provenance}}
			if tc.value != nil {
				keys = append(keys, domain.PolicyKey{
					Key: string(domain.WorkflowGateIssueComment), Value: *tc.value, Provenance: provenance,
				})
			}
			policy, err := domain.NewResolvedPolicy("gate-run", keys)
			if err != nil {
				t.Fatal(err)
			}
			if got := domain.EffectGateOn(policy, domain.WorkflowGateIssueComment); got != tc.want {
				t.Fatalf("EffectGateOn(%s) = %v, want %v", tc.name, got, tc.want)
			}
			// Another gate's key never answers for this one.
			if domain.EffectGateOn(policy, domain.WorkflowGateFollowUpFiling) {
				t.Fatal("the follow-up filing gate read the issue comment gate's key")
			}
		})
	}
}
