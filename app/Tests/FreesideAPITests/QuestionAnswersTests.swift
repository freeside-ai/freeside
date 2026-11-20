import Testing

@testable import FreesideAPI

/// The client renders an answer's message exactly as the daemon does
/// (domain.RenderQuestionAnswers), or it would refuse to trust the record.
struct QuestionAnswersTests {
    @Test func rendersLinesInDecisionOrderThenTheNote() {
        let answers: [Components.Schemas.QuestionAnswer] = [
            .init(decision_index: 1, option_label: nil), .init(decision_index: 0, option_label: "SQLite"),
        ]
        #expect(
            Components.Schemas.QuestionAnswer.renderedMessage(answers, note: "Use Redis.")
                == "Decision 1: SQLite\nDecision 2: Other answer, see the note below\n\nUse Redis.")
        #expect(Components.Schemas.QuestionAnswer.renderedMessage(answers, note: " \n") == nil)
        #expect(Components.Schemas.QuestionAnswer.renderedMessage([answers[1]], note: " \n") == "Decision 1: SQLite\n")
    }

    /// The daemon's TestRenderQuestionAnswersFitsTheAnswerBound case: a
    /// multi-byte label over the cap is cut on a character boundary.
    @Test func cutsALongLabelOnACharacterBoundary() throws {
        let label = String(repeating: "\u{e9}", count: 2048)
        let rendered = try #require(
            Components.Schemas.QuestionAnswer.renderedMessage([.init(decision_index: 0, option_label: label)], note: "")
        )
        let line = String(rendered.dropLast())
        #expect(line.hasSuffix("\u{e9}\u{2026}"))
        #expect(line.utf8.count - "Decision 1: ".utf8.count <= 200)
        #expect(line.utf8.count - "Decision 1: ".utf8.count > 196)
    }
}
