package publish

import (
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func testDigest(b byte) domain.Digest {
	return domain.Digest("sha256:" + strings.Repeat(string(rune('a'+b%16)), 64))
}

// validClosureProposal builds a valid resolving source-issue-closure proposal and
// a matching policy approval for a given merge, so AuthorizesClose accepts it.
func validClosureProposal(t *testing.T, merge domain.ProspectiveMerge, resolves bool) (domain.EffectProposal, domain.ClosureApproval) {
	t.Helper()
	policy, err := domain.NewResolvedPolicy("run-closure", []domain.PolicyKey{{
		Key: "gates.spec_approval", Value: "false",
		Provenance: domain.KeyProvenance{Source: domain.ProvenancePreset, Digest: testDigest(1)},
	}})
	if err != nil {
		t.Fatalf("resolved policy: %v", err)
	}
	proposal, err := domain.NewEffectProposal(domain.EffectSourceIssueClosure, domain.SourceIssueClosureInput{
		SubjectHandle: "workunit-run-closure",
		Source: domain.ClosableSource{
			Present: true, Provenance: domain.ClosureProvenanceVerified,
			Repo: "owner/name", RepositoryID: 42, IssueNumber: 7,
		},
		Origin: domain.ClosureFlagOriginProposeSite, Resolves: resolves,
	}, policy)
	if err != nil {
		t.Fatalf("new effect proposal: %v", err)
	}
	approval := domain.ClosureApproval{
		ProposalDigest: proposal.Digest, PublicationIdentity: merge.PublicationIdentity,
		CandidateHeadSHA: merge.CandidateHeadSHA, BaseRef: merge.BaseRef, BaseSHA: merge.BaseSHA,
		Actor: domain.ClosureApprovalActorPolicy,
	}
	return proposal, approval
}

func TestParseSourceIssueClosureHumanGate(t *testing.T) {
	t.Parallel()
	key := func(value string) domain.ResolvedPolicy {
		p, err := domain.NewResolvedPolicy("run-x", []domain.PolicyKey{{
			Key: policySourceIssueClosureGate, Value: value,
			Provenance: domain.KeyProvenance{Source: domain.ProvenancePreset, Digest: testDigest(2)},
		}})
		if err != nil {
			t.Fatalf("policy: %v", err)
		}
		return p
	}
	absent, err := domain.NewResolvedPolicy("run-x", []domain.PolicyKey{{
		Key: "gates.spec_approval", Value: "true",
		Provenance: domain.KeyProvenance{Source: domain.ProvenancePreset, Digest: testDigest(3)},
	}})
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	cases := []struct {
		name   string
		policy domain.ResolvedPolicy
		want   bool
	}{
		{"absent defaults to policy approval", absent, false},
		{"true is the human gate", key("true"), true},
		{"false is policy approval", key("false"), false},
		{"malformed fails closed to the human gate", key("maybe"), true},
	}
	for _, tc := range cases {
		if got := parseSourceIssueClosureHumanGate(tc.policy); got != tc.want {
			t.Errorf("%s: got %t, want %t", tc.name, got, tc.want)
		}
	}
}

func TestContainsSourceReferenceMarker(t *testing.T) {
	t.Parallel()
	reject := []string{
		sourceReferenceOpenMarker,
		"prose\n\n## Source issue\n\nCloses #7",
		"<!-- freeside:source-reference -->",
	}
	for _, body := range reject {
		if !containsSourceReferenceMarker(body) {
			t.Errorf("marker not detected in %q", body)
		}
	}
	// A v1 record keeps its prose "Source issue:" line; that must not be
	// mistaken for the publisher-owned heading.
	accept := []string{
		"Source issue: https://github.com/owner/name/issues/7",
		"prose about the source issue and closing it",
		"",
	}
	for _, body := range accept {
		if containsSourceReferenceMarker(body) {
			t.Errorf("false marker detection in %q", body)
		}
	}
}

func TestRenderSourceReference(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		res       closureResolution
		wantEmpty bool
		contains  []string
		absent    []string
	}{
		{
			name:     "closes verified",
			res:      closureResolution{outcome: domain.ClosureOutcome{Reference: domain.ClosureReferenceCloses}, target: 7},
			contains: []string{sourceReferenceOpenMarker, "## Source issue", "Closes #7", sourceReferenceCloseMarker},
			absent:   []string{"recommended"},
		},
		{
			name: "closes recommended carries the confirmation note",
			res: closureResolution{
				outcome: domain.ClosureOutcome{Reference: domain.ClosureReferenceCloses, Recommended: true}, target: 9,
			},
			contains: []string{"Closes #9", "The client recommended this issue; the approver confirmed it."},
		},
		{
			name:     "refs",
			res:      closureResolution{outcome: domain.ClosureOutcome{Reference: domain.ClosureReferenceRefs}, target: 3},
			contains: []string{"Refs #3"},
			absent:   []string{"Closes"},
		},
		{
			name:     "descriptive link names the source url",
			res:      closureResolution{outcome: domain.ClosureOutcome{Reference: domain.ClosureReferenceDescriptiveLink}, sourceURL: "https://github.com/o/r/issues/5"},
			contains: []string{"Source issue: https://github.com/o/r/issues/5"},
			absent:   []string{"Closes", "Refs"},
		},
		{
			name:      "descriptive link with no url renders nothing",
			res:       closureResolution{outcome: domain.ClosureOutcome{Reference: domain.ClosureReferenceDescriptiveLink}},
			wantEmpty: true,
		},
		{
			name:      "zero value renders nothing",
			res:       closureResolution{},
			wantEmpty: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			section, err := renderSourceReference(tc.res)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if tc.wantEmpty {
				if section != "" {
					t.Fatalf("want empty section, got %q", section)
				}
				return
			}
			for _, want := range tc.contains {
				if !strings.Contains(section, want) {
					t.Errorf("section lacks %q:\n%s", want, section)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(section, absent) {
					t.Errorf("section unexpectedly contains %q:\n%s", absent, section)
				}
			}
		})
	}
}

func TestReduceClosureApproval(t *testing.T) {
	t.Parallel()
	merge := domain.ProspectiveMerge{
		PublicationIdentity: testDigest(4), CandidateHeadSHA: "head-000", BaseRef: "main", BaseSHA: "base-000",
	}
	other := merge
	other.BaseSHA = "base-999"
	proposal, approval := validClosureProposal(t, merge, true)

	t.Run("policy approval authorizes close", func(t *testing.T) {
		got := reduceClosureApproval(&approval, proposal, merge, false, nil)
		if got != domain.ClosureApprovalStatePolicy {
			t.Fatalf("got %q, want policy", got)
		}
	})
	t.Run("human approval authorizes close", func(t *testing.T) {
		human := approval
		human.Actor = domain.ClosureApprovalActorHuman
		got := reduceClosureApproval(&human, proposal, merge, false, nil)
		if got != domain.ClosureApprovalStateHuman {
			t.Fatalf("got %q, want human", got)
		}
	})
	t.Run("stale approval with an open item is undecided", func(t *testing.T) {
		// AuthorizesClose rejects the approval against the moved merge; the open
		// item bound to the current merge means the decision is still pending.
		got := reduceClosureApproval(&approval, proposal, other, true, &other)
		if got != domain.ClosureApprovalStateNone {
			t.Fatalf("got %q, want none", got)
		}
	})
	t.Run("no approval and no open item is declined", func(t *testing.T) {
		got := reduceClosureApproval(nil, proposal, merge, false, nil)
		if got != domain.ClosureApprovalStateDeclined {
			t.Fatalf("got %q, want declined", got)
		}
	})
	t.Run("open item for a different merge is declined", func(t *testing.T) {
		got := reduceClosureApproval(nil, proposal, merge, true, &other)
		if got != domain.ClosureApprovalStateDeclined {
			t.Fatalf("got %q, want declined", got)
		}
	})
	t.Run("open item for the current merge is undecided", func(t *testing.T) {
		got := reduceClosureApproval(nil, proposal, merge, true, &merge)
		if got != domain.ClosureApprovalStateNone {
			t.Fatalf("got %q, want none", got)
		}
	})
}
