import FreesideAPI
import Testing

@testable import FreesideCore

@Suite struct DecisionRecommendationTests {
    private func recommendation(
        action: Components.Schemas.Action = .approve,
        source: Components.Schemas.RecommendationSource,
        provenance: Components.Schemas.RecommendationProvenance,
        confidence: Components.Schemas.AdjudicationConfidence? = nil
    ) -> Components.Schemas.Recommendation {
        .init(
            action: action,
            reason: "The evidence supports this route.",
            source: source,
            provenance: provenance,
            confidence: confidence.map { .init(value1: $0) })
    }

    private var daemonPolicyProvenance: Components.Schemas.RecommendationProvenance {
        .init(
            daemon_policy: .init(
                value1: .init(rule_digest: "sha256:rule", input_digest: "sha256:input")))
    }

    private var agentJudgmentProvenance: Components.Schemas.RecommendationProvenance {
        .init(
            agent_judgment: .init(
                value1: .init(
                    judgment_site: .finding_adjudicator,
                    invocation_id: "adjudicator-1",
                    artifact_digest: "sha256:artifact")))
    }

    private var projectPolicyProvenance: Components.Schemas.RecommendationProvenance {
        .init(
            project_policy: .init(
                value1: .init(
                    policy_key: "review.adjudication.route",
                    resolved_policy_digest: "sha256:policy",
                    application_digest: "sha256:application")))
    }

    @Test func daemonPolicyRendersAsACardFactCitingItsRuleAndInput() throws {
        let presentation = try #require(
            DecisionRecommendationPresentation(
                recommendation(source: .daemon_policy, provenance: daemonPolicyProvenance)))

        #expect(presentation.register == .daemonFact)
        #expect(!presentation.register.isUnverifiedClaim)
        #expect(presentation.title == "Recommended · daemon policy")
        #expect(presentation.label == "Recommended · daemon policy")
        #expect(
            presentation.sourceFacts.map(\.label) == ["Rule digest", "Input digest"])
        #expect(presentation.sourceFacts.allSatisfy { $0.monospaced })
        #expect(presentation.confidence == nil)
    }

    @Test func agentJudgmentRendersAsALabeledUnverifiedProposal() throws {
        let presentation = try #require(
            DecisionRecommendationPresentation(
                recommendation(
                    action: .accept_recommended_route,
                    source: .agent_judgment,
                    provenance: agentJudgmentProvenance,
                    confidence: .high)))

        #expect(presentation.register == .agentClaim)
        #expect(presentation.register.isUnverifiedClaim)
        #expect(presentation.title == "Recommended · agent judgment")
        #expect(presentation.label == "Recommended · agent judgment · High")
        #expect(
            presentation.sourceFacts.map(\.value)
                == ["Finding adjudicator", "adjudicator-1", "sha256:artifact"])
        #expect(presentation.confidence == "High")
        #expect(presentation.action == .accept_recommended_route)
    }

    @Test func projectPolicyCitesItsExactPolicyKeyAndDigest() throws {
        let presentation = try #require(
            DecisionRecommendationPresentation(
                recommendation(source: .project_policy, provenance: projectPolicyProvenance)))

        #expect(presentation.register == .projectPolicy)
        #expect(!presentation.register.isUnverifiedClaim)
        #expect(presentation.title == "Recommended · project policy")
        #expect(
            presentation.sourceFacts.map(\.value)
                == ["review.adjudication.route", "sha256:policy", "sha256:application"])
    }

    /// The label is the block's one register line, so a recommendation the
    /// daemon recorded no confidence for renders the register alone rather
    /// than a dangling separator (#1107).
    @Test func aRecommendationWithoutConfidenceLabelsOnlyItsRegister() throws {
        let presentation = try #require(
            DecisionRecommendationPresentation(
                recommendation(
                    source: .agent_judgment,
                    provenance: agentJudgmentProvenance)))

        #expect(presentation.confidence == nil)
        #expect(presentation.label == "Recommended · agent judgment")
    }

    /// A source the provenance does not authenticate must not pick up the
    /// register that source would imply; the card renders no recommendation
    /// instead (plan §9).
    @Test func aSourceContradictedByItsProvenanceYieldsNoRecommendation() {
        #expect(
            DecisionRecommendationPresentation(
                recommendation(source: .agent_judgment, provenance: daemonPolicyProvenance)) == nil)
        #expect(
            DecisionRecommendationPresentation(
                recommendation(source: .daemon_policy, provenance: projectPolicyProvenance)) == nil)
    }

    @Test func absentOrAmbiguousProvenanceYieldsNoRecommendation() {
        #expect(
            DecisionRecommendationPresentation(
                recommendation(source: .daemon_policy, provenance: .init())) == nil)
        #expect(
            DecisionRecommendationPresentation(
                recommendation(
                    source: .daemon_policy,
                    provenance: .init(
                        daemon_policy: .init(
                            value1: .init(
                                rule_digest: "sha256:rule", input_digest: "sha256:input")),
                        agent_judgment: .init(
                            value1: .init(
                                judgment_site: .finding_adjudicator,
                                invocation_id: "adjudicator-1",
                                artifact_digest: "sha256:artifact"))))) == nil)
    }

    @Test func anItemWithoutARecommendationOffersEquallyWeightedActions() {
        var item = AttentionFixtures.fixture(type: .finding_adjudication).item
        item.recommendation = nil

        #expect(DecisionRecommendationPresentation.of(item) == nil)
        #expect(
            DecisionActionRanking(
                requested: item.requested_decision,
                recommendedAction: DecisionRecommendationPresentation.of(item)?.action
            ).recommended == nil)
    }

    @Test func theItemsOwnRecommendationIsTheOnlySource() throws {
        let item = AttentionFixtures.fixture(type: .finding_adjudication).item
        let presentation = try #require(DecisionRecommendationPresentation.of(item))

        #expect(presentation.action == .accept_recommended_route)
        #expect(item.requested_decision.contains(presentation.action))
        #expect(presentation.register == .agentClaim)
    }
}
