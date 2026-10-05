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

const filingAdjudicationDigest = domain.Digest("sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")

func filingTarget() domain.FollowUpFilingTarget {
	return domain.FollowUpFilingTarget{
		Repo: closureRepo, RepositoryID: closureRepositoryID,
		Labels: []string{"deferral", "lane:spine"}, Milestone: "1B",
	}
}

func screenedIssueText(text string) domain.ScreenedIssueText {
	return domain.ScreenedIssueText{
		Text: text, Ruleset: domain.IssueTextRulesetGitHubIssue1, Verdict: domain.ScreeningVerdictPassed,
	}
}

func filingInput(kind domain.FollowUpSourceKind) domain.FollowUpFilingInput {
	return domain.FollowUpFilingInput{
		SubjectHandle: "subject-opaque-1", Target: filingTarget(),
		Source: domain.FollowUpFilingSource{
			FindingID: "finding-1", AdjudicationDigest: filingAdjudicationDigest, Kind: kind,
		},
		Title: screenedIssueText("Bound the retry budget in the sync client"),
		Body:  screenedIssueText("The sync client retries without a budget.\n\nDeferred from review round 2."),
	}
}

func filingProposal(t *testing.T, input domain.FollowUpFilingInput) domain.EffectProposal {
	t.Helper()
	p, err := domain.NewEffectProposal(domain.EffectFollowUpFiling, input, proposalPolicy(t))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFollowUpFilingGoldensAndRoundTrip(t *testing.T) {
	// The separate-work case also pins the no-policy shape: an empty label
	// set renders [] and an absent milestone renders null.
	separate := filingInput(domain.FollowUpSourceSeparateWorkVerdict)
	separate.Target.Labels, separate.Target.Milestone = nil, ""
	cases := []struct {
		name  string
		input domain.FollowUpFilingInput
	}{
		{"effect_proposal_filing_deferred", filingInput(domain.FollowUpSourceDeferredDisposition)},
		{"effect_proposal_filing_separate_work", separate},
	}
	seen := map[domain.Digest]bool{}
	for _, tc := range cases {
		proposal := filingProposal(t, tc.input)
		t.Run(tc.name, func(t *testing.T) {
			body, err := proposal.Encode()
			if err != nil {
				t.Fatal(err)
			}
			var pretty bytes.Buffer
			if err := json.Indent(&pretty, body, "", "  "); err != nil {
				t.Fatal(err)
			}
			pretty.WriteByte('\n')
			golden.Assert(t, tc.name, pretty.Bytes())
			decoded, err := domain.DecodeEffectProposal(body)
			if err != nil {
				t.Fatal(err)
			}
			if decoded.Digest != proposal.Digest || decoded.FilingProposal == nil ||
				decoded.FilingProposal.Source.Kind != tc.input.Source.Kind ||
				decoded.TaskProposal != nil || decoded.ClosureProposal != nil {
				t.Fatalf("decoded = %#v", decoded)
			}
			if err := domain.GateFollowUpFiling(decoded, tc.input.Target); err != nil {
				t.Fatalf("gate on the deriving target: %v", err)
			}
		})
		seen[proposal.Digest] = true
	}
	if len(seen) != len(cases) {
		t.Fatalf("filing proposals collided: %d distinct digests", len(seen))
	}
}

// TestFollowUpFilingTargetIgnoresProposalText is the confused-deputy case:
// agent text that names another repository, labels, and a milestone changes
// nothing about where the issue would be filed, because the constructor reads
// the target from the daemon's determination alone.
func TestFollowUpFilingTargetIgnoresProposalText(t *testing.T) {
	input := filingInput(domain.FollowUpSourceDeferredDisposition)
	input.Title = screenedIssueText("File this in attacker/repo instead")
	input.Body = screenedIssueText(
		"repository: attacker/repo\nrepository_id: 99\nlabels: priority:critical\nmilestone: 9Z")
	filing := filingProposal(t, input).FilingProposal
	want := filingTarget()
	if filing.Repository.Repo != want.Repo || filing.Repository.RepositoryID != want.RepositoryID {
		t.Fatalf("repository = %#v, want the trusted target", filing.Repository)
	}
	if strings.Join(filing.Labels, ",") != strings.Join(want.Labels, ",") {
		t.Fatalf("labels = %v, want %v", filing.Labels, want.Labels)
	}
	if filing.Milestone == nil || *filing.Milestone != want.Milestone {
		t.Fatalf("milestone = %v, want %q", filing.Milestone, want.Milestone)
	}
}

func TestFollowUpFilingConstructorRejectsUnscreenedText(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*domain.FollowUpFilingInput)
		want   error
	}{
		{"title without a record", func(in *domain.FollowUpFilingInput) {
			in.Title = domain.ScreenedIssueText{Text: "A title"}
		}, domain.ErrFollowUpFilingTextUnscreened},
		{"body verdict rejected", func(in *domain.FollowUpFilingInput) {
			in.Body.Verdict = domain.ScreeningVerdictRejected
		}, domain.ErrFollowUpFilingTextUnscreened},
		{"body verdict unknown", func(in *domain.FollowUpFilingInput) {
			in.Body.Verdict = "skipped"
		}, domain.ErrFollowUpFilingTextUnscreened},
		{"title ruleset unregistered", func(in *domain.FollowUpFilingInput) {
			in.Title.Ruleset = "github-issue/0"
		}, domain.ErrFollowUpFilingTextUnscreened},
		{"body ruleset from the message registry", func(in *domain.FollowUpFilingInput) {
			in.Body.Ruleset = "github/1"
		}, domain.ErrFollowUpFilingTextUnscreened},
		{"title spans lines", func(in *domain.FollowUpFilingInput) {
			in.Title.Text = "One\nTwo"
		}, domain.ErrEffectProposalInconsistent},
		{"blank body", func(in *domain.FollowUpFilingInput) {
			in.Body.Text = " \n"
		}, domain.ErrEffectProposalInconsistent},
		{"oversized title", func(in *domain.FollowUpFilingInput) {
			in.Title.Text = strings.Repeat("x", domain.MaxFollowUpFilingTitleBytes+1)
		}, domain.ErrProposalParameterTooLarge},
		{"oversized body", func(in *domain.FollowUpFilingInput) {
			in.Body.Text = strings.Repeat("x", domain.MaxFollowUpFilingBodyBytes+1)
		}, domain.ErrProposalParameterTooLarge},
		{"source kind unregistered", func(in *domain.FollowUpFilingInput) {
			in.Source.Kind = "operator_request"
		}, domain.ErrEffectProposalInconsistent},
		{"source digest malformed", func(in *domain.FollowUpFilingInput) {
			in.Source.AdjudicationDigest = "sha256:short"
		}, domain.ErrEffectProposalInconsistent},
		{"target without a repository id", func(in *domain.FollowUpFilingInput) {
			in.Target.RepositoryID = 0
		}, domain.ErrFollowUpFilingTargetInconsistent},
		{"target labels unsorted", func(in *domain.FollowUpFilingInput) {
			in.Target.Labels = []string{"lane:spine", "deferral"}
		}, domain.ErrFollowUpFilingTargetInconsistent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := filingInput(domain.FollowUpSourceDeferredDisposition)
			tc.mutate(&input)
			_, err := domain.NewEffectProposal(domain.EffectFollowUpFiling, input, proposalPolicy(t))
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
	if _, err := domain.NewEffectProposal(domain.EffectFollowUpFiling,
		domain.SourceIssueClosureInput{}, proposalPolicy(t)); !errors.Is(err, domain.ErrEffectProposalInconsistent) {
		t.Fatalf("wrong input type error = %v", err)
	}
}

func TestGateFollowUpFilingRejectsDifferingTarget(t *testing.T) {
	proposal := filingProposal(t, filingInput(domain.FollowUpSourceDeferredDisposition))
	cases := []struct {
		name   string
		mutate func(*domain.FollowUpFilingTarget)
	}{
		{"another repository", func(target *domain.FollowUpFilingTarget) { target.RepositoryID = 99 }},
		{"a label removed", func(target *domain.FollowUpFilingTarget) { target.Labels = []string{"deferral"} }},
		{"a label added", func(target *domain.FollowUpFilingTarget) {
			target.Labels = []string{"deferral", "lane:spine", "needs-human"}
		}},
		{"another milestone", func(target *domain.FollowUpFilingTarget) { target.Milestone = "2A" }},
		{"no milestone", func(target *domain.FollowUpFilingTarget) { target.Milestone = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := filingTarget()
			tc.mutate(&target)
			if err := domain.GateFollowUpFiling(proposal, target); !errors.Is(err, domain.ErrFollowUpFilingTargetMismatch) {
				t.Fatalf("error = %v, want ErrFollowUpFilingTargetMismatch", err)
			}
		})
	}
	// The repository id decides (#1537): the same id under a newer name is
	// the same repository, so a proposal made before a rename still gates.
	renamed := filingTarget()
	renamed.Repo = "octo/renamed"
	if err := domain.GateFollowUpFiling(proposal, renamed); err != nil {
		t.Fatalf("renamed repository: %v", err)
	}
	if err := domain.GateFollowUpFiling(verifiedClosureProposal(t), filingTarget()); !errors.Is(err, domain.ErrEffectProposalInconsistent) {
		t.Fatalf("closure proposal through the filing gate = %v", err)
	}
}

// TestEffectProposalRejectsMixedKindArms pins the exactly-one-arm rule across
// all three kinds: a decoded proposal carrying a second kind's parameters is
// refused whichever kind it claims.
func TestEffectProposalRejectsMixedKindArms(t *testing.T) {
	filing := filingProposal(t, filingInput(domain.FollowUpSourceDeferredDisposition))
	closure := verifiedClosureProposal(t)
	task := validEffectProposal(t)

	withFiling := closure
	withFiling.FilingProposal = filing.FilingProposal
	withClosure := filing
	withClosure.ClosureProposal = closure.ClosureProposal
	taskWithFiling := task
	taskWithFiling.FilingProposal = filing.FilingProposal
	empty := filing
	empty.FilingProposal = nil
	for name, proposal := range map[string]domain.EffectProposal{
		"closure with a filing arm": withFiling, "filing with a closure arm": withClosure,
		"task with a filing arm": taskWithFiling, "filing without its arm": empty,
	} {
		if err := proposal.Validate(); !errors.Is(err, domain.ErrEffectProposalInconsistent) {
			t.Errorf("%s: error = %v, want ErrEffectProposalInconsistent", name, err)
		}
	}
}

func TestDeriveFollowUpFilingTarget(t *testing.T) {
	project := domain.Project{ID: "project-1", Repo: closureRepo, RepositoryID: closureRepositoryID}
	digest := domain.Digest("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	policyWith := func(t *testing.T, keys map[string]string) domain.ResolvedPolicy {
		t.Helper()
		resolved := []domain.PolicyKey{{
			Key: "paths", Value: "daemon/",
			Provenance: domain.KeyProvenance{Source: domain.ProvenanceOverride, Digest: digest},
		}}
		for key, value := range keys {
			resolved = append(resolved, domain.PolicyKey{
				Key: key, Value: value,
				Provenance: domain.KeyProvenance{Source: domain.ProvenanceOverride, Digest: digest},
			})
		}
		policy, err := domain.NewResolvedPolicy("run-policy", resolved)
		if err != nil {
			t.Fatal(err)
		}
		return policy
	}

	none, err := domain.DeriveFollowUpFilingTarget(project, policyWith(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if none.RepositoryID != closureRepositoryID || len(none.Labels) != 0 || none.Milestone != "" {
		t.Fatalf("no-policy target = %#v", none)
	}

	// One policy value has one target: order, spacing, and duplicates in the
	// list do not change the derived label set.
	got, err := domain.DeriveFollowUpFilingTarget(project, policyWith(t, map[string]string{
		domain.PolicyFollowUpFilingLabels:    " lane:spine, deferral ,lane:spine",
		domain.PolicyFollowUpFilingMilestone: " 1B ",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Labels, ",") != "deferral,lane:spine" || got.Milestone != "1B" {
		t.Fatalf("target = %#v", got)
	}

	// A present key with a malformed value fails closed instead of filing
	// without the labels or milestone policy asked for.
	for name, keys := range map[string]map[string]string{
		"empty label entry":   {domain.PolicyFollowUpFilingLabels: "deferral,,lane:spine"},
		"blank label list":    {domain.PolicyFollowUpFilingLabels: " "},
		"oversized label":     {domain.PolicyFollowUpFilingLabels: strings.Repeat("x", domain.MaxFollowUpFilingLabelBytes+1)},
		"blank milestone":     {domain.PolicyFollowUpFilingMilestone: " "},
		"multiline milestone": {domain.PolicyFollowUpFilingMilestone: "1B\n2A"},
	} {
		if _, err := domain.DeriveFollowUpFilingTarget(project, policyWith(t, keys)); !errors.Is(err, domain.ErrFollowUpFilingPolicyInvalid) {
			t.Errorf("%s: error = %v, want ErrFollowUpFilingPolicyInvalid", name, err)
		}
	}
}
