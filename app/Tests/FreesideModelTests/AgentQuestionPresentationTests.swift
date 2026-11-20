import FreesideAPI
import Testing

@testable import FreesideModel

struct AgentQuestionPresentationTests {
    @Test func scopeConflictKeepsFactsAndOmitsRetryRoute() throws {
        let item = AttentionFixtures.scopeConflictQuestion().item
        let presentation = try #require(AgentQuestionPresentation(item))
        #expect(presentation.scopeConflict?.paths == ["AGENTS.md"])
        #expect(presentation.scopeConflict?.declared_paths == ["devlog/*.md", "src/*.ts"])
        #expect(AgentQuestionPresentation.answerRoute(for: item) == nil)
        #expect(AgentQuestionPresentation.answerRoutes(for: item) == [])
        #expect(item.requested_decision == [.answer_without_retry, .stop])
        #expect(AttentionFixtures.scopeKeptReady().item.scope_decision?.value1.paths == ["AGENTS.md"])
    }
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
        // An implementation-stage question offers both routes, retry first (#1083).
        #expect(
            AgentQuestionPresentation.answerRoutes(for: item)
                == [.retry_implementation, .revise_specification])
        #expect(AgentQuestionPresentation.answerRouteLabel(.retry_implementation) == "Retry the implementer")
        #expect(AgentQuestionPresentation.answerRouteLabel(.revise_specification) == "Revise the specification")
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
        #expect(AgentQuestionPresentation.answerRoutes(for: item) == [])
    }

    @Test func otherTypesAndMissingFactsPresentNothing() {
        let approval = AttentionFixtures.fixture(type: .spec_approval).item
        #expect(AgentQuestionPresentation(approval) == nil)
        #expect(AgentQuestionPresentation.answerRoute(for: approval) == nil)
        #expect(AgentQuestionPresentation.answerRoute(for: nil) == nil)
        #expect(AgentQuestionPresentation.answerRoutes(for: approval) == [])
        #expect(AgentQuestionPresentation.answerRoutes(for: nil) == [])
    }
}

extension AgentQuestionPresentationTests {
    @Test func drawsTheRecommendedOptionFirstAndKeepsTheRestInOrder() throws {
        let item = AttentionFixtures.longQuestion().item
        let presentation = try #require(AgentQuestionPresentation(item))
        #expect(
            presentation.decisions.map { $0.options.map(\.label) } == [
                ["Copy to a new table, then switch readers", "Rewrite rows in place"],
                ["Translate for one release", "Reject the old shape", "Translate indefinitely"],
            ])
        // Each option keeps the agent's number, so a typed "option 2" still
        // means the agent's second option.
        #expect(presentation.decisions.map { $0.options.map(\.number) } == [[2, 1], [2, 1, 3]])
        // The item keeps the agent's order; only the card sorts.
        #expect(item.agent_question?.value1.decisions[0].options[0].label == "Rewrite rows in place")
    }

    @Test func answersNeedAValidChoiceForEveryDecision() throws {
        let presentation = try #require(AgentQuestionPresentation(AttentionFixtures.longQuestion().item))
        let first = AgentQuestionPresentation.Choice.option("Rewrite rows in place")
        #expect(presentation.answers([:]) == nil)
        #expect(presentation.answers([0: first]) == nil)
        // A label from another decision, or an index past the decisions, is no choice.
        #expect(presentation.answers([0: first, 1: .option("Rewrite rows in place")]) == nil)
        #expect(presentation.answers([0: first, 2: .other]) == nil)

        let answers = try #require(presentation.answers([1: .other, 0: first]))
        #expect(
            answers == [
                .init(decision_index: 0, option_label: "Rewrite rows in place"),
                .init(decision_index: 1, option_label: nil),
            ])
        #expect(AgentQuestionPresentation.needsNote(answers))
        #expect(!AgentQuestionPresentation.needsNote([answers[0]]))
        #expect(
            AgentQuestionPresentation.choiceLines(answers)
                == ["Decision 1: Rewrite rows in place", "Decision 2: Other answer"])
    }

    @Test func confirmLabelNamesTheOneChoiceOrCountsThem() {
        let one: [Components.Schemas.QuestionAnswer] = [.init(decision_index: 0, option_label: "Store first, then API")]
        #expect(
            AgentQuestionPresentation.confirmLabel(.answer_and_retry, answers: one)
                == "Answer and Retry: Store first, then API")
        #expect(
            AgentQuestionPresentation.confirmLabel(
                .answer_without_retry, answers: [.init(decision_index: 0, option_label: nil)])
                == "Record: Other answer")
        let long = String(repeating: "x", count: 41)
        #expect(
            AgentQuestionPresentation.confirmLabel(
                .answer_and_retry, answers: [.init(decision_index: 0, option_label: long)])
                == "Answer and Retry: \(String(repeating: "x", count: 39))\u{2026}")
        #expect(
            AgentQuestionPresentation.confirmLabel(
                .answer_without_retry, answers: one + [.init(decision_index: 1, option_label: nil)])
                == "Record with 2 Answers")
    }
}

extension AgentQuestionPresentationTests {
    @Test func previousAnswersStartAsTheChoicesAndAPickReplacesThem() throws {
        let item = AttentionFixtures.previouslyAnsweredQuestion().item
        let presentation = try #require(AgentQuestionPresentation(item))
        #expect(
            presentation.decisions.map(\.previousAnswer) == [
                .option("Rewrite rows in place"), .note(AttentionFixtures.previousTypedAnswer),
            ])
        // An earlier option answer starts chosen; typed text starts as
        // "Other answer", and the sheet opens with that text.
        #expect(presentation.previousChoices == [0: .option("Rewrite rows in place"), 1: .other])
        #expect(presentation.previousNote(presentation.previousChoices) == AttentionFixtures.previousTypedAnswer)
        let answers = try #require(presentation.answers(presentation.choices(picked: [:])))
        #expect(
            answers == [
                .init(decision_index: 0, option_label: "Rewrite rows in place"),
                .init(decision_index: 1, option_label: nil),
            ])

        // The owner's own pick wins, and once no decision is left at the
        // earlier typed answer the sheet starts empty.
        let picked = presentation.choices(picked: [1: .option("Reject the old shape")])
        #expect(picked == [0: .option("Rewrite rows in place"), 1: .option("Reject the old shape")])
        #expect(presentation.previousNote(picked) == nil)
    }

    @Test func aQuestionWithoutPreviousAnswersStartsUnchosen() throws {
        let presentation = try #require(AgentQuestionPresentation(AttentionFixtures.longQuestion().item))
        #expect(presentation.decisions.allSatisfy { $0.previousAnswer == nil })
        #expect(presentation.previousChoices.isEmpty)
        #expect(presentation.previousNote(presentation.choices(picked: [0: .other, 1: .other])) == nil)
    }
}
