package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func recommendationDigest(fill string) domain.Digest {
	return domain.Digest("sha256:" + strings.Repeat(fill, 64))
}

func recommendationItem(t *testing.T) (domain.AttentionItem, domain.DecisionSurface) {
	t.Helper()
	runID := domain.RunID("run-1")
	confidence := domain.ConfidenceHigh
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-recommendation", ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: "run-1", RunID: &runID},
		Type:    domain.AttentionFindingAdjudication, Priority: domain.PriorityHigh,
		Reason: "a routed review batch needs a decision",
		RequestedDecision: []domain.Action{
			domain.ActionAcceptRecommendedRoute, domain.ActionDiscuss, domain.ActionStop,
		},
		FindingAdjudication: &domain.FindingAdjudicationBinding{
			RunID: runID, Round: 2, AdjudicationDigest: recommendationDigest("a"),
			Proposals: []domain.FindingAdjudicationProposal{{
				FindingID: "finding-1", FindingMessage: "the finding",
				Producer:         domain.AdjudicationProducerModel,
				GoalRelationship: domain.GoalContradictory, Route: domain.RouteDecline,
				Rationale: "the finding contradicts the work unit", Evidence: []string{"path is outside scope"},
				Confidence: &confidence,
			}},
		},
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate,
		Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	surface, err := domain.NewDecisionSurface(item)
	if err != nil {
		t.Fatal(err)
	}
	return item, surface
}

func agentRecommendationRecord(
	t *testing.T, item domain.AttentionItem, surface domain.DecisionSurface,
) domain.RecommendationSourceRecord {
	t.Helper()
	record, err := domain.NewRecommendationSourceRecord(domain.RecommendationSourceRecord{
		ItemID: item.ID, Source: domain.RecommendationAgentJudgment,
		Provenance: domain.RecommendationProvenance{
			AgentJudgment: &domain.AgentJudgmentRecommendationProvenance{
				JudgmentSite:   domain.JudgmentSiteFindingAdjudicator,
				InvocationID:   "review-invocation-2",
				ArtifactDigest: item.FindingAdjudication.AdjudicationDigest,
			},
		},
		Action:                domain.ActionAcceptRecommendedRoute,
		Reason:                domain.FindingAdjudicatorRecommendationReason,
		DecisionSurfaceDigest: surface.Digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

type fakeRecommendationAuthority struct {
	agent           domain.AgentJudgmentRecommendation
	agentInvocation domain.InvocationID
	rules           map[domain.Digest]domain.DaemonPolicyRule
	policy          domain.Digest
}

func (a fakeRecommendationAuthority) ResolveAgentJudgment(
	_ domain.JudgmentSite, invocationID domain.InvocationID, _ domain.Digest,
) (domain.AgentJudgmentRecommendation, error) {
	if a.agentInvocation != "" && invocationID != a.agentInvocation {
		return domain.AgentJudgmentRecommendation{}, domain.ErrParentKeyMismatch
	}
	return a.agent, nil
}

func (a fakeRecommendationAuthority) DaemonPolicyRule(
	digest domain.Digest,
) (domain.DaemonPolicyRule, bool) {
	rule, ok := a.rules[digest]
	return rule, ok
}

func (a fakeRecommendationAuthority) CurrentResolvedPolicyDigest(domain.RunID) (domain.Digest, error) {
	return a.policy, nil
}

type fakeRecommendationRule struct {
	projection domain.RecommendationProjection
	input      domain.Digest
	applicable bool
}

func (r fakeRecommendationRule) EvaluateRecommendation(
	domain.AttentionItem, domain.DecisionSurface,
) (domain.RecommendationProjection, domain.Digest, bool, error) {
	return r.projection, r.input, r.applicable, nil
}

func TestRecommendationDerivationUniqueOrNone(t *testing.T) {
	t.Parallel()
	item, surface := recommendationItem(t)
	agent := agentRecommendationRecord(t, item, surface)
	authority := fakeRecommendationAuthority{agent: domain.AgentJudgmentRecommendation{
		RunID: "run-1", Round: 2,
		Projection: domain.RecommendationProjection{
			Action: domain.ActionAcceptRecommendedRoute,
			Reason: domain.FindingAdjudicatorRecommendationReason,
		},
	}, rules: map[domain.Digest]domain.DaemonPolicyRule{}}

	got, err := domain.DeriveRecommendation(item, surface, nil, authority)
	if err != nil || got != nil {
		t.Fatalf("zero records = %#v, %v; want absent", got, err)
	}
	got, err = domain.DeriveRecommendation(item, surface, []domain.RecommendationSourceRecord{agent}, authority)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Action != domain.ActionAcceptRecommendedRoute ||
		got.Source != domain.RecommendationAgentJudgment ||
		got.Reason != domain.FindingAdjudicatorRecommendationReason {
		t.Fatalf("unique agent record = %#v", got)
	}
	second, err := domain.NewRecommendationSourceRecord(domain.RecommendationSourceRecord{
		ItemID: item.ID, Source: agent.Source,
		Provenance: domain.RecommendationProvenance{AgentJudgment: &domain.AgentJudgmentRecommendationProvenance{
			JudgmentSite: domain.JudgmentSiteFindingAdjudicator,
			InvocationID: "review-invocation-other", ArtifactDigest: item.FindingAdjudication.AdjudicationDigest,
		}},
		Action: agent.Action, Reason: agent.Reason, DecisionSurfaceDigest: surface.Digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err = domain.DeriveRecommendation(item, surface, []domain.RecommendationSourceRecord{agent, second}, authority)
	if err != nil || got != nil {
		t.Fatalf("two same-class records = %#v, %v; want absent", got, err)
	}

	ruleDigest := recommendationDigest("b")
	inputDigest := recommendationDigest("c")
	ruleProjection := domain.RecommendationProjection{
		Action: domain.ActionDiscuss, Reason: "Discuss the deterministic policy conflict.",
	}
	rule := fakeRecommendationRule{projection: ruleProjection, input: inputDigest, applicable: true}
	authority.rules[ruleDigest] = rule
	daemonRecord, err := domain.NewRecommendationSourceRecord(domain.RecommendationSourceRecord{
		ItemID: item.ID, Source: domain.RecommendationDaemonPolicy,
		Provenance: domain.RecommendationProvenance{DaemonPolicy: &domain.DaemonPolicyRecommendationProvenance{
			RuleDigest: ruleDigest, InputDigest: inputDigest,
		}},
		Action: ruleProjection.Action, Reason: ruleProjection.Reason,
		DecisionSurfaceDigest: surface.Digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err = domain.DeriveRecommendation(item, surface, []domain.RecommendationSourceRecord{agent, daemonRecord}, authority)
	if err != nil || got != nil {
		t.Fatalf("cross-class collision = %#v, %v; want absent", got, err)
	}
}

func TestRecommendationDerivationRejectsSubstitutionAndStaleCommitment(t *testing.T) {
	t.Parallel()
	item, surface := recommendationItem(t)
	record := agentRecommendationRecord(t, item, surface)
	authority := fakeRecommendationAuthority{agent: domain.AgentJudgmentRecommendation{
		RunID: "run-1", Round: 2,
		Projection: domain.RecommendationProjection{
			Action: domain.ActionAcceptRecommendedRoute,
			Reason: domain.FindingAdjudicatorRecommendationReason,
		},
	}}

	stale := record
	stale.DecisionSurfaceDigest = recommendationDigest("f")
	stale, err := domain.NewRecommendationSourceRecord(stale)
	if err != nil {
		t.Fatal(err)
	}
	got, err := domain.DeriveRecommendation(item, surface, []domain.RecommendationSourceRecord{stale}, authority)
	if err != nil || got != nil {
		t.Fatalf("stale commitment = %#v, %v; want absent", got, err)
	}

	substituted := record
	substituted.Action = domain.ActionDiscuss
	substituted.Reason = "caller replacement"
	substituted, err = domain.NewRecommendationSourceRecord(substituted)
	if err != nil {
		t.Fatal(err)
	}
	got, err = domain.DeriveRecommendation(item, surface, []domain.RecommendationSourceRecord{substituted}, authority)
	if err != nil || got != nil {
		t.Fatalf("substituted payload = %#v, %v; want absent", got, err)
	}
}

func TestRecommendationShapesRejectMalformedAuthority(t *testing.T) {
	t.Parallel()
	item, surface := recommendationItem(t)
	record := agentRecommendationRecord(t, item, surface)
	if err := record.Validate(); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		mutate func(*domain.RecommendationSourceRecord)
		want   error
	}{
		{"source mismatch", func(r *domain.RecommendationSourceRecord) { r.Source = domain.RecommendationDaemonPolicy }, domain.ErrRecommendationProvenanceInconsistent},
		{"two variants", func(r *domain.RecommendationSourceRecord) {
			r.Provenance.DaemonPolicy = &domain.DaemonPolicyRecommendationProvenance{RuleDigest: recommendationDigest("b"), InputDigest: recommendationDigest("c")}
		}, domain.ErrRecommendationProvenanceInconsistent},
		{"unknown site", func(r *domain.RecommendationSourceRecord) { r.Provenance.AgentJudgment.JudgmentSite = "other" }, domain.ErrInvalidJudgmentSite},
		{"malformed artifact", func(r *domain.RecommendationSourceRecord) { r.Provenance.AgentJudgment.ArtifactDigest = "artifact" }, domain.ErrInvalidDigest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bad := record
			bad.Provenance = domain.RecommendationProvenance{AgentJudgment: &domain.AgentJudgmentRecommendationProvenance{
				JudgmentSite:   record.Provenance.AgentJudgment.JudgmentSite,
				InvocationID:   record.Provenance.AgentJudgment.InvocationID,
				ArtifactDigest: record.Provenance.AgentJudgment.ArtifactDigest,
			}}
			tc.mutate(&bad)
			if err := bad.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Validate = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestAttentionItemRejectsRecommendationOutsideRequestedDecision(t *testing.T) {
	t.Parallel()
	item, surface := recommendationItem(t)
	record := agentRecommendationRecord(t, item, surface)
	item.Recommendation = &domain.Recommendation{
		Action: domain.ActionDismiss, Reason: record.Reason, Source: record.Source,
		Provenance: record.Provenance,
	}
	if err := item.Validate(); !errors.Is(err, domain.ErrInvalidAction) {
		t.Fatalf("Validate = %v, want ErrInvalidAction", err)
	}
}

func TestAttentionItemRejectsUnboundRecommendationArtifact(t *testing.T) {
	t.Parallel()
	item, surface := recommendationItem(t)
	record := agentRecommendationRecord(t, item, surface)
	provenance := record.Provenance
	provenance.AgentJudgment = &domain.AgentJudgmentRecommendationProvenance{
		JudgmentSite:   provenance.AgentJudgment.JudgmentSite,
		InvocationID:   provenance.AgentJudgment.InvocationID,
		ArtifactDigest: recommendationDigest("f"),
	}
	item.Recommendation = &domain.Recommendation{
		Action: record.Action, Reason: record.Reason, Source: record.Source,
		Provenance: provenance,
	}
	if err := item.Validate(); !errors.Is(err, domain.ErrBindingMismatch) {
		t.Fatalf("Validate = %v, want ErrBindingMismatch", err)
	}
}

func TestRecommendationDerivationRejectsAuthorityMismatches(t *testing.T) {
	t.Parallel()
	item, surface := recommendationItem(t)
	agent := agentRecommendationRecord(t, item, surface)
	agentProjection := domain.AgentJudgmentRecommendation{
		RunID: "run-1", Round: 2,
		Projection: domain.RecommendationProjection{
			Action: domain.ActionAcceptRecommendedRoute,
			Reason: domain.FindingAdjudicatorRecommendationReason,
		},
	}

	ruleDigest := recommendationDigest("b")
	inputDigest := recommendationDigest("c")
	ruleProjection := domain.RecommendationProjection{Action: domain.ActionDiscuss, Reason: "Discuss the policy exception."}
	daemonRecord, err := domain.NewRecommendationSourceRecord(domain.RecommendationSourceRecord{
		ItemID: item.ID, Source: domain.RecommendationDaemonPolicy,
		Provenance: domain.RecommendationProvenance{DaemonPolicy: &domain.DaemonPolicyRecommendationProvenance{
			RuleDigest: ruleDigest, InputDigest: inputDigest,
		}},
		Action: ruleProjection.Action, Reason: ruleProjection.Reason,
		DecisionSurfaceDigest: surface.Digest,
	})
	if err != nil {
		t.Fatal(err)
	}

	policyDigest := recommendationDigest("d")
	applicationDigest, err := domain.ComputeProjectPolicyRecommendationApplicationDigest(
		"review.dispute", policyDigest, surface.Digest, domain.ActionDiscuss, "Discuss the policy exception.",
	)
	if err != nil {
		t.Fatal(err)
	}
	projectRecord, err := domain.NewRecommendationSourceRecord(domain.RecommendationSourceRecord{
		ItemID: item.ID, Source: domain.RecommendationProjectPolicy,
		Provenance: domain.RecommendationProvenance{ProjectPolicy: &domain.ProjectPolicyRecommendationProvenance{
			PolicyKey: "review.dispute", ResolvedPolicyDigest: policyDigest, ApplicationDigest: applicationDigest,
		}},
		Action: domain.ActionDiscuss, Reason: "Discuss the policy exception.",
		DecisionSurfaceDigest: surface.Digest,
	})
	if err != nil {
		t.Fatal(err)
	}

	wrongArtifact := agent
	wrongArtifact.Provenance = domain.RecommendationProvenance{AgentJudgment: &domain.AgentJudgmentRecommendationProvenance{
		JudgmentSite:   agent.Provenance.AgentJudgment.JudgmentSite,
		InvocationID:   agent.Provenance.AgentJudgment.InvocationID,
		ArtifactDigest: recommendationDigest("e"),
	}}
	wrongArtifact, err = domain.NewRecommendationSourceRecord(wrongArtifact)
	if err != nil {
		t.Fatal(err)
	}
	badApplication := projectRecord
	badApplication.Provenance = domain.RecommendationProvenance{ProjectPolicy: &domain.ProjectPolicyRecommendationProvenance{
		PolicyKey:            projectRecord.Provenance.ProjectPolicy.PolicyKey,
		ResolvedPolicyDigest: projectRecord.Provenance.ProjectPolicy.ResolvedPolicyDigest,
		ApplicationDigest:    recommendationDigest("f"),
	}}
	badApplication, err = domain.NewRecommendationSourceRecord(badApplication)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		record    domain.RecommendationSourceRecord
		authority fakeRecommendationAuthority
	}{
		{"unbound agent artifact", wrongArtifact, fakeRecommendationAuthority{agent: agentProjection}},
		{"wrong review invocation", agent, fakeRecommendationAuthority{agent: agentProjection, agentInvocation: "other-invocation"}},
		{"unregistered daemon rule", daemonRecord, fakeRecommendationAuthority{rules: map[domain.Digest]domain.DaemonPolicyRule{}}},
		{"changed daemon input", daemonRecord, fakeRecommendationAuthority{rules: map[domain.Digest]domain.DaemonPolicyRule{
			ruleDigest: fakeRecommendationRule{projection: ruleProjection, input: recommendationDigest("e"), applicable: true},
		}}},
		{"stale resolved policy", projectRecord, fakeRecommendationAuthority{policy: recommendationDigest("e")}},
		{"changed policy application", badApplication, fakeRecommendationAuthority{policy: policyDigest}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := domain.DeriveRecommendation(item, surface, []domain.RecommendationSourceRecord{tc.record}, tc.authority)
			if err != nil || got != nil {
				t.Fatalf("DeriveRecommendation = %#v, %v; want absent", got, err)
			}
		})
	}
}

func TestRecommendationEnumRegistries(t *testing.T) {
	t.Parallel()
	if len(domain.AllRecommendationSources) != 3 || len(domain.AllJudgmentSites) != 1 {
		t.Fatalf("registries = %v / %v", domain.AllRecommendationSources, domain.AllJudgmentSites)
	}
}
