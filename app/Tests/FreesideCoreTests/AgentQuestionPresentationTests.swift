import FreesideAPI
import Testing

@testable import FreesideCore

struct AgentQuestionPresentationTests {
    @Test func presentsTypedDecisionsWithTheRecommendationMarked() throws {
        let item = AttentionFixtures.fixture(type: .agent_question).item
        let presentation = try #require(AgentQuestionPresentation(item))

        #expect(presentation.stage == .implementation)
        #expect(presentation.kindLabel == "Owner decision")
        #expect(presentation.decisions.count == AttentionFixtures.agentQuestionDecisions.count)
        let decision = try #require(presentation.decisions.first)
        #expect(decision.question == "Which order should the migration run in?")
        #expect(decision.options.map(\.recommended) == [true, false])
        #expect(decision.options.map(\.label) == ["Store first, then API", "API first, then store"])
        #expect(AgentQuestionPresentation.answerRoute(for: item) == .retry_implementation)
    }

    @Test func specificationStageCarriesNoKindAndNoRoute() throws {
        var item = AttentionFixtures.fixture(type: .agent_question).item
        item.agent_question = .init(
            value1: .init(
                stage: .specification, invocation_id: "inv-specify-1", kind: nil,
                decisions: AttentionFixtures.agentQuestionDecisions))
        let presentation = try #require(AgentQuestionPresentation(item))

        #expect(presentation.stage == .specification)
        #expect(presentation.kindLabel == nil)
        #expect(AgentQuestionPresentation.answerRoute(for: item) == nil)
    }

    @Test func otherTypesAndMissingFactsPresentNothing() {
        let approval = AttentionFixtures.fixture(type: .spec_approval).item
        #expect(AgentQuestionPresentation(approval) == nil)
        #expect(AgentQuestionPresentation.answerRoute(for: approval) == nil)
        #expect(AgentQuestionPresentation.answerRoute(for: nil) == nil)
    }
}
