import Foundation

/// The daemon's question_answers rules (daemon/internal/domain/question_answer.go),
/// shared by the mock server, the client's result-trust check, and the card,
/// so all three render and refuse an answer exactly as the daemon does.
extension Components.Schemas.QuestionAnswer {
    /// domain.MaxDecisionsPerResult.
    public static let maxAnswers = 8
    /// domain.MaxQuestionAnswerNoteBytes: the note's limit when answers ride along.
    public static let maxNoteBytes = 6144
    /// domain.MaxQuestionAnswerLabelBytes: a rendered label's cap.
    static let maxRenderedLabelBytes = 200
    static let ellipsis = "\u{2026}"
    static let otherText = "Other answer, see the note below"

    /// Why the answers are malformed without the question in view, or nil:
    /// one to eight answers, each decision index in that range and named
    /// once, and no empty label (domain.validateQuestionAnswerShape).
    public static func shapeFault(_ answers: [Self]) -> String? {
        guard (1...maxAnswers).contains(answers.count) else { return "question_answers must hold 1 to 8 answers" }
        var seen = Set<Int>()
        for answer in answers {
            guard (0..<maxAnswers).contains(answer.decision_index) else {
                return "question_answers decision_index is out of range"
            }
            guard seen.insert(answer.decision_index).inserted else {
                return "question_answers names a decision twice"
            }
            if answer.option_label?.isEmpty == true { return "question_answers option_label is empty" }
        }
        return nil
    }

    /// Why the answers don't answer these decisions exactly, or nil: one
    /// answer per decision, each chosen label one of its decision's labels
    /// (domain.ValidateQuestionAnswers).
    public static func questionFault(_ answers: [Self], decisions: [Components.Schemas.Decision]) -> String? {
        if let fault = shapeFault(answers) { return fault }
        guard answers.count == decisions.count else { return "question_answers must answer every decision once" }
        for answer in answers {
            guard answer.decision_index < decisions.count else {
                return "question_answers decision_index is out of range"
            }
            if let label = answer.option_label,
                !decisions[answer.decision_index].options.contains(where: { $0.label == label })
            {
                return "question_answers names no option of its decision"
            }
        }
        return nil
    }

    /// The durable message the daemon renders from the answers and the note
    /// (domain.RenderQuestionAnswers), or nil where it refuses them: a
    /// malformed set, a note over `maxNoteBytes`, or a decision answered in
    /// a blank note. One `Decision N: <label>` line per answer in decision
    /// order, then a blank line and the note when there is one.
    public static func renderedMessage(_ answers: [Self], note: String) -> String? {
        guard shapeFault(answers) == nil, note.utf8.count <= maxNoteBytes else { return nil }
        let noteBlank = note.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
        var lines = ""
        for answer in answers.sorted(by: { $0.decision_index < $1.decision_index }) {
            let choice: String
            if let label = answer.option_label {
                choice = renderedLabel(label)
            } else if noteBlank {
                return nil
            } else {
                choice = otherText
            }
            lines += "Decision \(answer.decision_index + 1): \(choice)\n"
        }
        return noteBlank ? lines : lines + "\n" + note
    }

    /// The label within `maxRenderedLabelBytes`, cut on a UTF-8 boundary and
    /// marked with an ellipsis (domain.cutQuestionAnswerLabel).
    static func renderedLabel(_ label: String) -> String {
        let bytes = Array(label.utf8)
        guard bytes.count > maxRenderedLabelBytes else { return label }
        var cut = maxRenderedLabelBytes - ellipsis.utf8.count
        // A continuation byte is 0b10xxxxxx; back up to the start of its rune.
        while cut > 0 && bytes[cut] & 0xC0 == 0x80 { cut -= 1 }
        return String(decoding: bytes[..<cut], as: UTF8.self) + ellipsis
    }
}
