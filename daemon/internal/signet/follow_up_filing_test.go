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

// filingFixture seeds one follow_up_filing proposal instance with the rows its
// gate re-derives from (project, policy naming labels and a milestone, run,
// declaration, and a review round whose adjudication defers the finding) and
// binds an effect_proposal item to it. No signet path opens a filing item
// yet, so the item is written through the store, as its producer will.
type filingFixture struct {
	fixture
	service  *signet.Service
	instance domain.ProposalInstance
	item     domain.AttentionItem
}

func newFilingFixture(t *testing.T) filingFixture {
	t.Helper()
	ctx := context.Background()
	base := newRunFixture(t)
	now := *base.now
	provenance := domain.KeyProvenance{Source: domain.ProvenanceOverride, Digest: testDigest("a")}
	policy, err := domain.NewResolvedPolicy("filing-policy-run", []domain.PolicyKey{
		{Key: "paths", Value: "daemon/", Provenance: provenance},
		{Key: domain.PolicyFollowUpFilingLabels, Value: "lane:spine,deferral", Provenance: provenance},
		{Key: domain.PolicyFollowUpFilingMilestone, Value: "1B", Provenance: provenance},
	})
	if err != nil {
		t.Fatal(err)
	}
	project := domain.Project{ID: "proj-1", Repo: testProjectRepo, RepositoryID: testProjectRepositoryID}
	target, err := domain.DeriveFollowUpFilingTarget(project, policy)
	if err != nil {
		t.Fatal(err)
	}
	finding := domain.Finding{
		ID: "finding-deferred", RunID: policy.RunID, Source: "codex_local",
		Location: &domain.FindingLocation{Path: "daemon/a.go", StartLine: 1, EndLine: 1},
		Message:  "retry budget is unbounded", RawText: "retry budget is unbounded", CreatedAt: now,
	}
	review, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-filing-1", RunID: policy.RunID, Round: 1,
		Provider: "openai", ModelConfiguration: "gpt-codex/high",
		ConfigurationDigest: testDigest("c"), InstructionDigest: testDigest("d"),
		CostOwner: "owner", BaseSHA: "base", HeadSHA: "head-1", CompletedAt: now,
		CompletionEvidence: testDigest("e"),
		Outcome:            domain.ReviewFindings, FindingIDs: []domain.FindingID{finding.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := domain.NewModelAdjudicationEntry(
		finding.ID, domain.GoalAdjacent, nil, domain.RouteDefer, domain.ConfidenceHigh,
		"adjacent to the goal", nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	adjudication, err := domain.NewFindingAdjudication(
		policy.RunID, 1, testDigest("f"), review.InstructionDigest, policy.Digest,
		[]domain.FindingAdjudicationEntry{entry}, "", now)
	if err != nil {
		t.Fatal(err)
	}
	passed := func(text string) domain.ScreenedIssueText {
		return domain.ScreenedIssueText{
			Text: text, Ruleset: domain.IssueTextRulesetGitHubIssue1, Verdict: domain.ScreeningVerdictPassed,
		}
	}
	proposal, err := domain.NewEffectProposal(domain.EffectFollowUpFiling, domain.FollowUpFilingInput{
		SubjectHandle: domain.OpaqueSubjectHandle(domain.WorkUnitIDForRun(policy.RunID)),
		Target:        target,
		Source: domain.FollowUpFilingSource{
			FindingID: finding.ID, AdjudicationDigest: adjudication.Digest,
			Kind: domain.FollowUpSourceDeferredDisposition,
		},
		Title: passed("Bound the retry budget in the sync client"),
		Body:  passed("The sync client retries without a budget.\n\nDeferred from review round 1."),
	}, policy)
	if err != nil {
		t.Fatal(err)
	}

	var instance domain.ProposalInstance
	var item domain.AttentionItem
	if err := base.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.RegisterProject(ctx, project); err != nil {
			return err
		}
		if err := tx.PutRun(ctx, domain.Run{
			ID: policy.RunID, ProjectID: project.ID,
			SpecDigest: testDigest("f"), PolicyDigest: policy.Digest, Stages: []domain.Stage{},
		}); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
			return err
		}
		declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
			CompletionCriterion: domain.CompletionBoundPRMerged,
			DeclaredPaths:       domain.CanonicalDeclaredPaths(policy),
		}, policy.RunID, project.ID, now.Add(-time.Hour))
		if err != nil {
			return err
		}
		if err := tx.RecordWorkUnitDeclaration(ctx, declaration); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, review, []domain.Finding{finding}); err != nil {
			return err
		}
		if err := tx.PutFindingAdjudication(ctx, adjudication); err != nil {
			return err
		}
		if err := tx.PutFindingDisposition(ctx, domain.ReviewDispositionRecord{
			FindingID: finding.ID, RunID: policy.RunID, Round: 1,
			Disposition: domain.ReviewDispositionDeferred, Reason: "tracked as a follow-up",
			AdjudicationDigest: adjudication.Digest, CreatedAt: now,
		}); err != nil {
			return err
		}
		instance, _, err = tx.AllocateProposalInstance(ctx,
			domain.ProposalAdmissionKey{Source: domain.ProposalSourceUpstreamEvent, UpstreamEventID: "filing-event"},
			"batch-filing", proposal, now)
		if err != nil {
			return err
		}
		artifact, err := instance.EvidenceArtifact()
		if err != nil {
			return err
		}
		if err := tx.PutArtifact(ctx, artifact); err != nil {
			return err
		}
		item, err = domain.NewAttentionItem(domain.AttentionItemInput{
			ID: domain.ItemID(string(instance.ID) + "/effect"), ProjectID: project.ID,
			Subject: domain.Subject{Type: domain.SubjectProposalBatch, ID: domain.SubjectID(instance.ProposalBatchID)},
			Type:    domain.AttentionEffectProposal, Priority: domain.PriorityNormal,
			Reason:            "Decide whether to file the follow-up issue",
			RequestedDecision: []domain.Action{domain.ActionApprove, domain.ActionDecline, domain.ActionSnooze},
			EvidenceSnapshot:  []domain.Artifact{artifact}, ItemVersion: 1,
			InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
			CreatedAt: &now,
		}, map[domain.Digest]bool{domain.EffectProposalRecipeDigest: true})
		if err != nil {
			return err
		}
		if err := tx.PutAttentionItem(ctx, item); err != nil {
			return err
		}
		return tx.BindProposalItem(ctx, item.ID, instance.ID, proposal.Digest, nil)
	}); err != nil {
		t.Fatal(err)
	}
	service := signet.NewService(base.store, signet.WithClock(func() time.Time { return *base.now }))
	return filingFixture{fixture: base, service: service, instance: instance, item: item}
}

// TestEffectProposalFactsServeTheFilingArm proves the facts read branches on
// the effect kind: a filing item returns its filing facts with the closure arm
// and supersedes null, matched to the item's version tuple, and carries no
// subject handle or policy identity on the wire.
func TestEffectProposalFactsServeTheFilingArm(t *testing.T) {
	f := newFilingFixture(t)
	snapshot, err := f.service.GetAttentionItem(context.Background(), f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := f.service.GetEffectProposalFacts(context.Background(), f.item.ID)
	if err != nil {
		t.Fatalf("GetEffectProposalFacts: %v", err)
	}
	if facts.AsOfRevision != snapshot.AsOfRevision || facts.EntityVersion != snapshot.EntityVersion ||
		facts.ItemVersion != f.item.ItemVersion || facts.ProposalDigest != f.instance.Proposal.Digest ||
		facts.EffectKind != domain.EffectFollowUpFiling {
		t.Fatalf("facts = %#v, want the open item's tuple", facts)
	}
	if facts.SourceIssueClosure != nil || facts.Supersedes != nil {
		t.Fatalf("filing facts carried a closure arm or supersedes: %#v", facts)
	}
	filing := facts.FollowUpFiling
	if filing == nil {
		t.Fatal("facts carried no follow_up_filing arm")
	}
	want := f.instance.Proposal.FilingProposal
	if filing.Repository != want.Repository || !slices.Equal(filing.Labels, []string{"deferral", "lane:spine"}) ||
		filing.Milestone == nil || *filing.Milestone != "1B" ||
		filing.Title != want.Title || filing.Body != want.Body || filing.Source != want.Source {
		t.Fatalf("filing facts = %#v, want %#v", filing, want)
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
	if !bytes.Contains(body, []byte(`"source_issue_closure":null`)) {
		t.Fatalf("facts wire %s lacks an explicit null closure arm", body)
	}
}

// TestFilingInstanceOpensNoClosureCard pins that the closure card opener
// refuses a filing instance: a filing binds no merge, so it must never take
// the card whose approval binds one.
func TestFilingInstanceOpensNoClosureCard(t *testing.T) {
	f := newFilingFixture(t)
	merge := domain.ProspectiveMerge{
		PublicationIdentity: testDigest("b"), CandidateHeadSHA: "head-aaa", BaseRef: "main", BaseSHA: "base-000",
	}
	if _, err := f.service.OpenEffectProposalItem(context.Background(), f.instance.ID, merge); !errors.Is(err, domain.ErrEffectProposalInconsistent) {
		t.Fatalf("OpenEffectProposalItem(filing) = %v, want ErrEffectProposalInconsistent", err)
	}
}
