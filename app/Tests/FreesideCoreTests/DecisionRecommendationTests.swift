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
        #expect(presentation.actor == "Daemon policy")
        #expect(presentation.sentence(for: nil) == "Daemon policy recommends Approve.")
        #expect(
            presentation.sourceFacts.map(\.label) == ["Rule Digest", "Input Digest"])
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
        #expect(presentation.actor == "The agent")
        #expect(
            presentation.sentence(for: nil)
                == "The agent recommends Accept Recommended Route, with high confidence.")
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
        #expect(presentation.actor == "Project policy")
        #expect(presentation.sentence(for: nil) == "Project policy recommends Approve.")
        #expect(
            presentation.sourceFacts.map(\.value)
                == ["review.adjudication.route", "sha256:policy", "sha256:application"])
    }

    /// The sentence carries the confidence, so a recommendation the daemon
    /// recorded no confidence for ends at the action rather than on a
    /// dangling clause (#1107).
    @Test func aRecommendationWithoutConfidenceNamesOnlyItsActorAndAction() throws {
        let presentation = try #require(
            DecisionRecommendationPresentation(
                recommendation(
                    source: .agent_judgment,
                    provenance: agentJudgmentProvenance)))

        #expect(presentation.confidence == nil)
        #expect(presentation.sentence(for: nil) == "The agent recommends Approve.")
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

    /// Accepting a finding item's routes is one command over every finding
    /// it binds, so the sentence says what that covers (R20, frame 7.1); any
    /// other action is named by its own label.
    @Test func theSentenceSaysWhatAcceptingAFindingItemCovers() throws {
        let batch = AttentionFixtures.fixture(type: .finding_adjudication).item
        let single = AttentionFixtures.findingAdjudicationFixture(route: .remediate).item
        let presentation = try #require(DecisionRecommendationPresentation.of(batch))
        let confidence = try #require(presentation.confidence).lowercased()

        #expect(
            presentation.sentence(for: batch)
                == "The agent recommends accepting the proposed route for each of the 2 findings above, "
                + "with \(confidence) confidence.")
        #expect(
            DecisionRecommendationPresentation.actionPhrase(.accept_recommended_route, for: single)
                == "accepting the proposed route for the one finding above")
        #expect(
            DecisionRecommendationPresentation.actionPhrase(.stop, for: batch)
                == AttentionDisplay.label(.stop))
    }

    @Test func theItemsOwnRecommendationIsTheOnlySource() throws {
        let item = AttentionFixtures.fixture(type: .finding_adjudication).item
        let presentation = try #require(DecisionRecommendationPresentation.of(item))

        #expect(presentation.action == .accept_recommended_route)
        #expect(item.requested_decision.contains(presentation.action))
        #expect(presentation.register == .agentClaim)
    }
}
