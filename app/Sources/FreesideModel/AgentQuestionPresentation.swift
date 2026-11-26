import Foundation
import FreesideAPI

/// The daemon-typed facts of an agent_question card, shaped for display:
/// which role asked, why the implementer stopped, and each decision with its
/// options and the agent's recommendation marked as a claim (plan §9).
public struct AgentQuestionPresentation: Equatable, Sendable {
    public struct Option: Equatable, Sendable {
        public let label: String
        public let tradeoffs: String
        /// True for the option whose label the agent recommended. The
        /// recommendation is an agent claim, never a verified fact.
        public let recommended: Bool
        /// The option's 1-based place in the agent's order. The card draws
        /// the recommended option first but keeps this number, so an answer
        /// typed as "option 2" means the option the agent listed second.
        public let number: Int
    }

    public struct Decision: Equatable, Sendable {
        public let question: String
        public let whyBlocking: String
        /// The recommended option first, then the rest in the agent's order.
        /// Only the card sorts: the record and the wire keep the agent's
        /// order, and an option is named by its label, never its position.
        public let options: [Option]
        public let recommendation: String
        /// The owner's earlier answer to this decision, which the daemon
        /// found on an earlier ask of the same source (#3443), or nil.
        public let previousAnswer: PreviousAnswer?

        /// The choice the earlier answer makes: its option, or "Other
        /// answer" for typed text.
        public var previousChoice: Choice? {
            switch previousAnswer {
            case .option(let label)?: .option(label)
            case .note?: .other
            case nil: nil
            }
        }
    }

    /// An earlier answer to a decision: an option this decision still
    /// offers, named by its exact label, or the text the owner typed.
    public enum PreviousAnswer: Equatable, Sendable {
        case option(String)
        case note(String)

        /// What the card shows after "You answered this before:".
        public var text: String {
            switch self {
            case .option(let label), .note(let label): label
            }
        }
    }

    /// The owner's choice for one decision on the card: an option, named by
    /// its exact label (unique within its decision and kept when a re-asked
    /// question reorders its options), or an answer written in the note.
    public enum Choice: Hashable, Sendable {
        case option(String)
        case other
    }

    /// The choice the "Other answer" row draws as, in the card and the sheet.
    public static let otherAnswerLabel = "Other answer"

    public let stage: Components.Schemas.StageName
    public let kind: Components.Schemas.BlockedKind?
    public let decisions: [Decision]
    public let scopeConflict: Components.Schemas.ScopeConflictFacts?

    public init?(_ item: Components.Schemas.AttentionItem) {
        guard item._type == .agent_question, let facts = item.agent_question?.value1 else {
            return nil
        }
        stage = facts.stage
        kind = facts.kind?.value1
        scopeConflict = facts.scope_conflict?.value1
        let previous = Dictionary(
            (facts.previous_answers ?? []).compactMap { answer -> (Int, PreviousAnswer)? in
                if let label = answer.option_label { return (answer.decision_index, .option(label)) }
                if let note = answer.note { return (answer.decision_index, .note(note)) }
                return nil
            },
            uniquingKeysWith: { first, _ in first })
        decisions = facts.decisions.enumerated().map { index, decision in
            Decision(
                question: decision.question,
                whyBlocking: decision.why_blocking,
                options: Self.recommendedFirst(
                    decision.options.enumerated().map { index, option in
                        Option(
                            label: option.label, tradeoffs: option.tradeoffs,
                            recommended: option.label == decision.recommendation, number: index + 1)
                    }),
                recommendation: decision.recommendation,
                previousAnswer: previous[index])
        }
    }

    /// The choices the card starts with, by decision index: each earlier
    /// answer, as its option or as "Other answer" for typed text. The owner
    /// still confirms, and a pick of their own replaces it.
    public var previousChoices: [Int: Choice] {
        var choices: [Int: Choice] = [:]
        for (index, decision) in decisions.enumerated() {
            choices[index] = decision.previousChoice
        }
        return choices
    }

    /// The owner's picks over the earlier answers: what the card shows as
    /// chosen and what an answer sends.
    public func choices(picked: [Int: Choice]) -> [Int: Choice] {
        previousChoices.merging(picked) { _, picked in picked }
    }

    /// The note the answer sheet starts with: the earlier typed text of each
    /// decision still answered "Other answer", once each, in decision order,
    /// or nil when there is none.
    public func previousNote(_ choices: [Int: Choice]) -> String? {
        var notes: [String] = []
        for (index, decision) in decisions.enumerated() {
            guard choices[index] == .other, case .note(let note)? = decision.previousAnswer,
                !notes.contains(note)
            else { continue }
            notes.append(note)
        }
        return notes.isEmpty ? nil : notes.joined(separator: "\n\n")
    }

    /// The recommended option, then the others in their order. A stable
    /// partition, so options the agent didn't recommend keep its order.
    static func recommendedFirst(_ options: [Option]) -> [Option] {
        options.filter(\.recommended) + options.filter { !$0.recommended }
    }

    /// The answers the choices make, one per decision in decision order, or
    /// nil until every decision has a choice. Choices outside the question
    /// (an index past the decisions, or a label no option carries) count
    /// as no choice, so a stale selection never reaches the daemon.
    public func answers(_ choices: [Int: Choice]) -> [Components.Schemas.QuestionAnswer]? {
        var answers: [Components.Schemas.QuestionAnswer] = []
        for index in decisions.indices {
            guard let answer = answer(at: index, choices) else { return nil }
            answers.append(answer)
        }
        return answers.isEmpty ? nil : answers
    }

    /// The answer one decision's choice makes, or nil when it has none: no
    /// choice, a label the decision no longer offers, or an index past the
    /// decisions. `answers` and `isAnswered` both read it, so the card never
    /// calls a decision answered that an answer would not send.
    func answer(at index: Int, _ choices: [Int: Choice]) -> Components.Schemas.QuestionAnswer? {
        guard decisions.indices.contains(index) else { return nil }
        switch choices[index] {
        case .option(let label)?:
            guard decisions[index].options.contains(where: { $0.label == label }) else { return nil }
            return .init(decision_index: index, option_label: label)
        case .other?:
            return .init(decision_index: index, option_label: nil)
        case nil:
            return nil
        }
    }

    /// Whether the choices answer the decision at `index`, by the rule
    /// `answers` applies.
    public func isAnswered(_ index: Int, _ choices: [Int: Choice]) -> Bool {
        answer(at: index, choices) != nil
    }

    /// The keyword over one decision of a question that asks several:
    /// `Agent question 2 of 3`. A one-decision question keeps its plain
    /// keyword, so this is nil there.
    public func questionKeyword(at index: Int) -> String? {
        guard decisions.count > 1 else { return nil }
        return "Agent question \(index + 1) of \(decisions.count)"
    }

    /// The state a decision's keyword line trails where the card lets the
    /// operator pick.
    public static let answeredLabel = "Answered"
    public static let unansweredLabel = "Needs an answer"

    /// What a question of several decisions still needs before an answer can
    /// go: a count and a sentence naming the first unanswered decision.
    public struct PickProgress: Equatable, Sendable {
        /// `1 of 2 choices selected`.
        public let count: String
        /// `Choose an answer to question 2 to continue.`
        public let prompt: String
    }

    /// The progress line above the answer actions, or nil when there is
    /// nothing to explain: a one-decision question, whose disabled answer
    /// has one obvious cause, or every decision answered.
    public func pickProgress(_ choices: [Int: Choice]) -> PickProgress? {
        guard decisions.count > 1,
            let firstUnanswered = decisions.indices.first(where: { !isAnswered($0, choices) })
        else { return nil }
        let answered = decisions.indices.filter { isAnswered($0, choices) }.count
        return PickProgress(
            count: "\(answered) of \(decisions.count) choices selected",
            prompt: "Choose an answer to question \(firstUnanswered + 1) to continue.")
    }

    /// Whether the answers leave a decision to the note, which the daemon
    /// then requires.
    public static func needsNote(_ answers: [Components.Schemas.QuestionAnswer]) -> Bool {
        answers.contains { $0.option_label == nil }
    }

    /// The choices as the answer sheet lists them above its note, one line
    /// per decision: `Decision N: <label>`.
    public static func choiceLines(_ answers: [Components.Schemas.QuestionAnswer]) -> [String] {
        answers.map { "Decision \($0.decision_index + 1): \($0.option_label ?? otherAnswerLabel)" }
    }

    /// The answer sheet's confirm label: the chosen option for a one-decision
    /// question, else the number of answers, which the sheet lists above it.
    /// A long label is shortened; the sheet's list shows it whole.
    public static func confirmLabel(
        _ action: Components.Schemas.Action, answers: [Components.Schemas.QuestionAnswer]
    ) -> String {
        let verb = action == .answer_without_retry ? "Record" : "Answer and Retry"
        guard answers.count == 1, let answer = answers.first else {
            return "\(verb) with \(answers.count) Answers"
        }
        let choice = answer.option_label ?? otherAnswerLabel
        let shown = choice.count > 40 ? choice.prefix(39) + "\u{2026}" : Substring(choice)
        return "\(verb): \(shown)"
    }

    /// The blocker kind in operator words; nil on the specification stage,
    /// which has no blocker taxonomy.
    public var kindLabel: String? {
        kind.map(Self.kindLabel)
    }

    static func kindLabel(_ kind: Components.Schemas.BlockedKind) -> String {
        switch kind {
        case .specification_contradiction: return "Specification contradiction"
        case .owner_decision: return "Owner decision"
        case .scope_expansion: return "Scope expansion"
        case .capability_unavailable: return "Capability unavailable"
        case .commit_plan_collision: return "Commit plan collision"
        case .unknown: return "Unrecognized blocker"
        }
    }

    /// The answer_route choices an answer_and_retry on this item offers: an
    /// implementation-stage question without a scope conflict lets the operator
    /// retry the implementer or revise the specification (#1083); every other
    /// item offers none, so its answer carries no route. The order is the
    /// display order, and the first entry is the default.
    public static func answerRoutes(
        for item: Components.Schemas.AttentionItem?
    ) -> [Components.Schemas.AnswerRoute] {
        guard let item, let presentation = AgentQuestionPresentation(item),
            presentation.stage == .implementation, presentation.scopeConflict == nil
        else { return [] }
        return [.retry_implementation, .revise_specification]
    }

    /// The default route an answer_and_retry on this item carries when the
    /// operator does not choose otherwise, or nil when the item offers none.
    public static func answerRoute(
        for item: Components.Schemas.AttentionItem?
    ) -> Components.Schemas.AnswerRoute? {
        answerRoutes(for: item).first
    }

    /// The operator-facing label for an answer route.
    public static func answerRouteLabel(_ route: Components.Schemas.AnswerRoute) -> String {
        switch route {
        case .retry_implementation: return "Retry the implementer"
        case .revise_specification: return "Revise the specification"
        case .unknown: return "Unrecognized route"
        }
    }
}
