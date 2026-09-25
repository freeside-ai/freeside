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

const closureRepo = "octo/repo"

const closureRepositoryID = 42

func verifiedClosureSource() domain.ClosableSource {
	return domain.ClosableSource{
		Present: true, Provenance: domain.ClosureProvenanceVerified,
		Repo: closureRepo, RepositoryID: closureRepositoryID, IssueNumber: 7,
	}
}

func recommendedClosureSource() domain.ClosableSource {
	return domain.ClosableSource{
		Present: true, Provenance: domain.ClosureProvenanceRecommended,
		Repo: closureRepo, RepositoryID: closureRepositoryID,
	}
}

func verifiedClosureProposal(t *testing.T) domain.EffectProposal {
	t.Helper()
	p, err := domain.NewEffectProposal(domain.EffectSourceIssueClosure, domain.SourceIssueClosureInput{
		SubjectHandle: "subject-opaque-1", Source: verifiedClosureSource(),
		Origin: domain.ClosureFlagOriginProposeSite, Resolves: true,
	}, proposalPolicy(t))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func recommendedClosureProposal(t *testing.T) domain.EffectProposal {
	t.Helper()
	p, err := domain.NewEffectProposal(domain.EffectSourceIssueClosure, domain.SourceIssueClosureInput{
		SubjectHandle: "subject-opaque-1", Source: recommendedClosureSource(),
		ProposedTarget: domain.IssueSubjectRef{Repo: closureRepo, RepositoryID: closureRepositoryID, IssueNumber: 9},
		Origin:         domain.ClosureFlagOriginProposeSite, Resolves: true,
	}, proposalPolicy(t))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func fallbackClosureProposal(t *testing.T) domain.EffectProposal {
	t.Helper()
	// A daemon_fallback constructs with resolves forced false even though the
	// caller requested true.
	p, err := domain.NewEffectProposal(domain.EffectSourceIssueClosure, domain.SourceIssueClosureInput{
		SubjectHandle: "subject-opaque-1", Source: verifiedClosureSource(),
		Origin: domain.ClosureFlagOriginDaemonFallback, Resolves: true,
	}, proposalPolicy(t))
	if err != nil {
		t.Fatal(err)
	}
	if p.ClosureProposal.Resolves {
		t.Fatalf("daemon_fallback resolves = true, want forced false")
	}
	return p
}

func TestSourceIssueClosureGoldensAndRoundTrip(t *testing.T) {
	cases := []struct {
		name     string
		proposal domain.EffectProposal
	}{
		{"effect_proposal_closure_verified", verifiedClosureProposal(t)},
		{"effect_proposal_closure_recommended", recommendedClosureProposal(t)},
		{"effect_proposal_closure_fallback", fallbackClosureProposal(t)},
	}
	seen := map[domain.Digest]bool{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := tc.proposal.Encode()
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
			if decoded.Digest != tc.proposal.Digest || decoded.ClosureProposal == nil ||
				decoded.ClosureProposal.SubjectHandle != "subject-opaque-1" {
				t.Fatalf("decoded = %#v", decoded)
			}
		})
		seen[tc.proposal.Digest] = true
	}
	// verified and recommended differ visibly in their encoded form, so their
	// digests differ; that difference is what #1443 projects to the card.
	if len(seen) != 3 {
		t.Fatalf("closure proposals collided: %d distinct digests", len(seen))
	}
}

// TestRunProposalDigestStableUnderClosureField pins that adding the closure
// parameter pointer did not change the canonical encoding of an existing
// run_proposal proposal: every stored proposal must still revalidate after the
// upgrade (#1417).
func TestRunProposalDigestStableUnderClosureField(t *testing.T) {
	const pinned = domain.Digest("sha256:1b35b963f1a65b54e12dde2470d97d326371d8dd6d8c1ec7f77362204f3c22d3")
	if got := validEffectProposal(t).Digest; got != pinned {
		t.Fatalf("run_proposal digest = %q, want %q", got, pinned)
	}
}

func TestSourceIssueClosureConstructorRejectsForeignInput(t *testing.T) {
	policy := proposalPolicy(t)
	// Cross-repository target: a recommended source whose client-chosen target
	// names a different repository yields no proposal.
	_, err := domain.NewEffectProposal(domain.EffectSourceIssueClosure, domain.SourceIssueClosureInput{
		SubjectHandle: "subject-opaque-1", Source: recommendedClosureSource(),
		ProposedTarget: domain.IssueSubjectRef{Repo: "attacker/repo", RepositoryID: 99, IssueNumber: 9},
		Origin:         domain.ClosureFlagOriginProposeSite, Resolves: true,
	}, policy)
	if !errors.Is(err, domain.ErrClosureTargetMismatch) {
		t.Fatalf("cross-repository target error = %v, want ErrClosureTargetMismatch", err)
	}
	// The repository id decides (#1537): a target under the source's name but
	// another id is cross-repository, and one under an older name of the same
	// id is not.
	_, err = domain.NewEffectProposal(domain.EffectSourceIssueClosure, domain.SourceIssueClosureInput{
		SubjectHandle: "subject-opaque-1", Source: recommendedClosureSource(),
		ProposedTarget: domain.IssueSubjectRef{Repo: closureRepo, RepositoryID: 99, IssueNumber: 9},
		Origin:         domain.ClosureFlagOriginProposeSite, Resolves: true,
	}, policy)
	if !errors.Is(err, domain.ErrClosureTargetMismatch) {
		t.Fatalf("same-name foreign-id target error = %v, want ErrClosureTargetMismatch", err)
	}
	if _, err := domain.NewEffectProposal(domain.EffectSourceIssueClosure, domain.SourceIssueClosureInput{
		SubjectHandle: "subject-opaque-1", Source: recommendedClosureSource(),
		ProposedTarget: domain.IssueSubjectRef{Repo: "octo/old-name", RepositoryID: closureRepositoryID, IssueNumber: 9},
		Origin:         domain.ClosureFlagOriginProposeSite, Resolves: true,
	}, policy); err != nil {
		t.Fatalf("old-name same-id target error = %v, want nil", err)
	}
	// Absent closable source: no proposal.
	_, err = domain.NewEffectProposal(domain.EffectSourceIssueClosure, domain.SourceIssueClosureInput{
		SubjectHandle: "subject-opaque-1", Source: domain.ClosableSource{},
		Origin: domain.ClosureFlagOriginProposeSite,
	}, policy)
	if !errors.Is(err, domain.ErrClosableSourceAbsent) {
		t.Fatalf("absent source error = %v, want ErrClosableSourceAbsent", err)
	}
	// Wrong parameter type.
	if _, err := domain.NewEffectProposal(domain.EffectSourceIssueClosure, "wrong", policy); !errors.Is(err, domain.ErrEffectProposalInconsistent) {
		t.Fatalf("wrong type error = %v, want ErrEffectProposalInconsistent", err)
	}
}

func TestSourceIssueClosureParametersValidate(t *testing.T) {
	valid := verifiedClosureProposal(t).ClosureProposal
	cases := []struct {
		name   string
		mutate func(*domain.SourceIssueClosureParameters)
		want   error
	}{
		{"empty subject", func(p *domain.SourceIssueClosureParameters) { p.SubjectHandle = "" }, domain.ErrEmptyID},
		{"bad target", func(p *domain.SourceIssueClosureParameters) { p.Target = domain.IssueSubjectRef{} }, domain.ErrEmptyField},
		{"invalid provenance", func(p *domain.SourceIssueClosureParameters) { p.Provenance = "bogus" }, domain.ErrEffectProposalInconsistent},
		{"invalid origin", func(p *domain.SourceIssueClosureParameters) { p.Origin = "bogus" }, domain.ErrEffectProposalInconsistent},
		{"fallback resolves true", func(p *domain.SourceIssueClosureParameters) {
			p.Origin = domain.ClosureFlagOriginDaemonFallback
			p.Resolves = true
		}, domain.ErrEffectProposalInconsistent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := *valid
			tc.mutate(&p)
			if err := p.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("validate error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestGateSourceIssueClosure(t *testing.T) {
	verified := verifiedClosureProposal(t)
	recommended := recommendedClosureProposal(t)

	if err := domain.GateSourceIssueClosure(verified, verifiedClosureSource()); err != nil {
		t.Fatalf("verified gate = %v, want nil", err)
	}
	if err := domain.GateSourceIssueClosure(recommended, recommendedClosureSource()); err != nil {
		t.Fatalf("recommended gate = %v, want nil", err)
	}
	// The repository id is the identity (#1537): after a rename the current
	// source carries the new name, and a proposal naming the old one still
	// passes.
	renamedVerified := verifiedClosureSource()
	renamedVerified.Repo = "owner/renamed"
	if err := domain.GateSourceIssueClosure(verified, renamedVerified); err != nil {
		t.Fatalf("verified gate after rename = %v, want nil", err)
	}
	renamedRecommended := recommendedClosureSource()
	renamedRecommended.Repo = "owner/renamed"
	if err := domain.GateSourceIssueClosure(recommended, renamedRecommended); err != nil {
		t.Fatalf("recommended gate after rename = %v, want nil", err)
	}

	cases := []struct {
		name     string
		proposal domain.EffectProposal
		closable domain.ClosableSource
		want     error
	}{
		{"absent source", verified, domain.ClosableSource{}, domain.ErrClosableSourceAbsent},
		{"verified claim on recommended source", verified, recommendedClosureSource(), domain.ErrClosureProvenanceMismatch},
		{"recommended claim on verified source", recommended, verifiedClosureSource(), domain.ErrClosureProvenanceMismatch},
		{"verified target issue mismatch", verified, domain.ClosableSource{
			Present: true, Provenance: domain.ClosureProvenanceVerified,
			Repo: closureRepo, RepositoryID: closureRepositoryID, IssueNumber: 8,
		}, domain.ErrClosureTargetMismatch},
		{"recommended repository mismatch", recommended, domain.ClosableSource{
			Present: true, Provenance: domain.ClosureProvenanceRecommended,
			Repo: "other/repo", RepositoryID: 99,
		}, domain.ErrClosureTargetMismatch},
		{"verified repository id mismatch under the same name", verified, domain.ClosableSource{
			Present: true, Provenance: domain.ClosureProvenanceVerified,
			Repo: closureRepo, RepositoryID: 99, IssueNumber: 7,
		}, domain.ErrClosureTargetMismatch},
		{"recommended repository id mismatch under the same name", recommended, domain.ClosableSource{
			Present: true, Provenance: domain.ClosureProvenanceRecommended,
			Repo: closureRepo, RepositoryID: 99,
		}, domain.ErrClosureTargetMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := domain.GateSourceIssueClosure(tc.proposal, tc.closable); !errors.Is(err, tc.want) {
				t.Fatalf("gate error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestClosureApprovalGoldenAndSupersession(t *testing.T) {
	proposal := verifiedClosureProposal(t)
	identity := domain.Digest("sha256:" + strings.Repeat("b", 64))
	merge := domain.ProspectiveMerge{
		PublicationIdentity: identity, CandidateHeadSHA: "cafebabe",
		BaseRef: "refs/heads/main", BaseSHA: "deadbeef",
	}
	approval := domain.ClosureApproval{
		ProposalDigest: proposal.Digest, PublicationIdentity: identity,
		CandidateHeadSHA: "cafebabe", BaseRef: "refs/heads/main", BaseSHA: "deadbeef",
		Actor: domain.ClosureApprovalActorHuman,
	}
	// The policy actor binds the same five values; only the recorder differs, so
	// a second golden pins the policy rendering beside the human one.
	policyApproval := approval
	policyApproval.Actor = domain.ClosureApprovalActorPolicy

	for name, value := range map[string]any{
		"closure_approval":        approval,
		"closure_approval_policy": policyApproval,
		"prospective_merge":       merge,
	} {
		body, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		golden.Assert(t, name, append(body, '\n'))
	}

	if !approval.AuthorizesClose(proposal, merge) {
		t.Fatal("matching human approval does not authorize close")
	}
	if !policyApproval.AuthorizesClose(proposal, merge) {
		t.Fatal("matching policy approval does not authorize close")
	}

	// no approval never authorizes: an ill-formed (zero) approval is fail-closed,
	// so admission alone cannot authorize a close.
	if (domain.ClosureApproval{}).AuthorizesClose(proposal, merge) {
		t.Fatal("empty approval authorized close")
	}

	// A binding with no actor is invalid, and AuthorizesClose (which validates
	// first) refuses it: an approval reconstructed without a recorder fails closed.
	noActor := approval
	noActor.Actor = ""
	if err := noActor.Validate(); err == nil {
		t.Fatal("zero-actor approval validated")
	}
	if noActor.AuthorizesClose(proposal, merge) {
		t.Fatal("zero-actor approval authorized close")
	}

	newHead := merge
	newHead.CandidateHeadSHA = "feedface"
	if approval.AuthorizesClose(proposal, newHead) {
		t.Fatal("new head did not supersede approval")
	}

	baseAdvance := merge
	baseAdvance.BaseSHA = "0badf00d"
	if approval.AuthorizesClose(proposal, baseAdvance) {
		t.Fatal("base advance (same ref, new sha) did not supersede approval")
	}

	newIdentity := merge
	newIdentity.PublicationIdentity = domain.Digest("sha256:" + strings.Repeat("c", 64))
	if approval.AuthorizesClose(proposal, newIdentity) {
		t.Fatal("changed publication identity did not supersede approval")
	}

	// An approved proposal whose flag is unset never authorizes a close.
	unset, err := domain.NewEffectProposal(domain.EffectSourceIssueClosure, domain.SourceIssueClosureInput{
		SubjectHandle: "subject-opaque-1", Source: verifiedClosureSource(),
		Origin: domain.ClosureFlagOriginDaemonFallback, Resolves: false,
	}, proposalPolicy(t))
	if err != nil {
		t.Fatal(err)
	}
	unsetApproval := approval
	unsetApproval.ProposalDigest = unset.Digest
	if unsetApproval.AuthorizesClose(unset, merge) {
		t.Fatal("unset resolves flag authorized close")
	}
}

// TestClosureApprovalActorRegistered proves every registered actor is a valid
// binding recorder and an unregistered actor is rejected, so the actor is a
// closed enum on the approval like every other domain vocabulary.
func TestClosureApprovalActorRegistered(t *testing.T) {
	proposal := verifiedClosureProposal(t)
	base := domain.ClosureApproval{
		ProposalDigest: proposal.Digest, PublicationIdentity: domain.Digest("sha256:" + strings.Repeat("b", 64)),
		CandidateHeadSHA: "cafebabe", BaseRef: "refs/heads/main", BaseSHA: "deadbeef",
	}
	for _, actor := range domain.AllClosureApprovalActors {
		approval := base
		approval.Actor = actor
		if err := approval.Validate(); err != nil {
			t.Fatalf("actor %q: Validate = %v, want nil", actor, err)
		}
	}
	bogus := base
	bogus.Actor = "operator"
	if err := bogus.Validate(); !errors.Is(err, domain.ErrClosureApprovalInconsistent) {
		t.Fatalf("bogus actor Validate = %v, want ErrClosureApprovalInconsistent", err)
	}
}
